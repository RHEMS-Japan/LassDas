package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func TestRedmineFindsItsNewNoteByReadingTheJournalsAgain(t *testing.T) {
	const text = "どちらにしますか？\r\n日本語 😀 `code` @example {\"future\":null}\n\n"
	fake, redmine := redmineFixture(t)
	issue := Issue{ID: 5, Key: "5"}
	fake.before = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPut {
			return false
		}
		var body map[string]map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if len(body) != 1 || len(body["issue"]) != 1 || body["issue"]["notes"] != text {
			t.Errorf("the note's words or fields changed: %v", body)
		}
		// Others post the same words at the same time; only the key's own
		// account's note after the newest one read before is this one.
		fake.journals = append(fake.journals,
			map[string]any{"id": 901, "user": map[string]any{"id": 5}, "notes": text},
			map[string]any{"id": 902, "user": map[string]any{"id": 6}, "notes": text},
			map[string]any{"id": 903, "user": map[string]any{"id": 5}, "notes": "other words"})
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	fake.journals = append(fake.journals, map[string]any{"id": 900, "user": map[string]any{"id": 5}, "notes": text})
	id, err := redmine.AddComment(context.Background(), issue, text)
	if err != nil || id != 901 {
		t.Fatalf("the note was taken as %d (%v)", id, err)
	}
	if calls := fake.called(); !slices.Equal(calls, []string{"GET /users/current.json", "GET /issues/5.json", "PUT /issues/5.json", "GET /issues/5.json"}) {
		t.Fatalf("calls: %v", calls)
	}
}

func TestRedmineCommentThatCannotBeConfirmedSaysItMayBePosted(t *testing.T) {
	for _, failure := range []string{"refused", "lost", "unreadable after"} {
		t.Run(failure, func(t *testing.T) {
			fake, redmine := redmineFixture(t)
			reads := 0
			switch failure {
			case "refused":
				fake.refuse["PUT /issues/5.json"] = http.StatusUnprocessableEntity
			case "lost":
				// The same words posted earlier by the same account are not
				// this post.
				fake.lose = true
				fake.journals = append(fake.journals, map[string]any{"id": 284, "user": map[string]any{"id": 5}, "notes": "a question"})
			case "unreadable after":
				fake.before = func(w http.ResponseWriter, r *http.Request) bool {
					if r.URL.Path == "/issues/5.json" && r.Method == http.MethodGet {
						if reads++; reads == 2 {
							w.WriteHeader(http.StatusBadGateway)
							return true
						}
					}
					return false
				}
			}
			_, err := redmine.AddComment(context.Background(), Issue{ID: 5, Key: "5"}, "a question")
			if err == nil || !strings.Contains(err.Error(), "may already be posted; inspect comments before retrying") {
				t.Fatalf("an unconfirmed note was not said to be possibly posted: %v", err)
			}
			if puts := slices.Index(fake.called(), "PUT /issues/5.json"); puts < 0 || slices.Contains(fake.called()[puts+1:], "PUT /issues/5.json") {
				t.Fatalf("the note was not sent once: %v", fake.called())
			}
		})
	}
}

func TestRedmineCommentsWithoutWordsOrValidUTF8AreNotSent(t *testing.T) {
	for _, text := range []string{"", " \r\n\t　", "fixture" + string([]byte{0xff})} {
		fake, redmine := redmineFixture(t)
		_, err := redmine.AddComment(context.Background(), Issue{ID: 5, Key: "5"}, text)
		if err == nil || !strings.Contains(err.Error(), "not sent") || strings.Contains(err.Error(), "may already be posted") || len(fake.called()) != 0 {
			t.Fatalf("%q: %v calls=%v", text, err, fake.called())
		}
		if err := redmine.EditComment(context.Background(), Issue{ID: 5, Key: "5"}, 274, text); err == nil || len(fake.called()) != 0 {
			t.Fatalf("%q: an edit to it was sent: %v", text, err)
		}
	}
}

func TestRedmineEditsOnlyAJournalOfTheIssueAndReadsItBack(t *testing.T) {
	ctx := context.Background()
	issue := Issue{ID: 5, Key: "5"}
	fake, redmine := redmineFixture(t)
	if err := redmine.EditComment(ctx, issue, 274, "new words"); err != nil {
		t.Fatal(err)
	}
	if fake.journals[0]["notes"] != "new words" || !slices.Equal(fake.called(), []string{"GET /issues/5.json", "PUT /journals/274.json", "GET /issues/5.json"}) {
		t.Fatalf("edit: %v %v", fake.journals[0]["notes"], fake.called())
	}
	// A journal id names no issue: one that is not this issue's is not sent.
	fake, redmine = redmineFixture(t)
	err := redmine.EditComment(ctx, issue, 7001, "new words")
	var refusal *trackerError
	if err == nil || !errors.As(err, &refusal) || refusal.Status != http.StatusNotFound || slices.Contains(fake.called(), "PUT /journals/7001.json") {
		t.Fatalf("another issue's journal: %v %v", err, fake.called())
	}
	// Before Redmine 5.0, or without the permission to edit one's own notes,
	// the edit is refused, and the engine posts a new comment instead.
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusNotAcceptable} {
		fake, redmine = redmineFixture(t)
		fake.refuse["PUT /journals/274.json"] = status
		if err := redmine.EditComment(ctx, issue, 274, "new words"); err == nil || !strings.Contains(err.Error(), fmt.Sprint(status)) {
			t.Fatalf("%d: %v", status, err)
		}
	}
	// An answer of success that did not change the words is not one.
	fake, redmine = redmineFixture(t)
	fake.before = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPut {
			w.WriteHeader(http.StatusNoContent)
			return true
		}
		return false
	}
	if err := redmine.EditComment(ctx, issue, 274, "new words"); err == nil || !strings.Contains(err.Error(), "did not confirm") {
		t.Fatalf("an unchanged note was taken for edited: %v", err)
	}
	if err := redmine.EditComment(ctx, issue, 0, "new words"); err == nil {
		t.Fatal("no id was edited")
	}
}

func TestRedmineTurnsSetStatusesAndAssigneesOnlyAsTheIssueReadsBack(t *testing.T) {
	ctx := context.Background()
	issue := Issue{ID: 5, Key: "5"}
	fake, redmine := redmineFixture(t)
	redmine.Statuses = map[string]int64{Processing: 2, AwaitingRequester: 4, Delivered: 3}
	for turn, want := range map[string]string{Accepted: "", Processing: "2", AwaitingRequester: "4", Delivered: "3", Stopped: ""} {
		if got := string(redmine.Target(turn)); got != want {
			t.Errorf("%s targets %q", turn, got)
		}
	}
	for _, turn := range []string{Processing, AwaitingRequester, Delivered} {
		if err := redmine.Move(ctx, issue, turn); err != nil {
			t.Fatalf("%s: %v", turn, err)
		}
		if got := fake.issue["status"].(map[string]any)["id"]; got != float64(redmine.Statuses[turn]) {
			t.Fatalf("%s: status %v", turn, got)
		}
	}
	for _, turn := range []string{Accepted, Stopped} {
		if err := redmine.Move(ctx, issue, turn); err == nil {
			t.Fatalf("%s: moved without a configured status", turn)
		}
	}
	// Redmine answers 204 for a status the role's workflow does not allow and
	// leaves it as it was.
	fake.ignore["status_id"] = true
	if err := redmine.Move(ctx, issue, Processing); err == nil || !strings.Contains(err.Error(), "still in status 3") || !strings.Contains(err.Error(), "workflow") {
		t.Fatalf("an ignored move was taken for done: %v", err)
	}
	for _, to := range []Account{{ID: 5}, {ID: 6, Login: "requester"}} {
		if err := redmine.Assign(ctx, issue, to); err != nil {
			t.Fatalf("assign %d: %v", to.ID, err)
		}
	}
	if err := redmine.Assign(ctx, issue, Account{ID: 999999}); err == nil || !strings.Contains(err.Error(), "Assignee is invalid") {
		t.Fatalf("a refused assignee: %v", err)
	}
	fake.ignore["assigned_to_id"] = true
	if err := redmine.Assign(ctx, issue, Account{ID: 5}); err == nil || !strings.Contains(err.Error(), "Edit issues") {
		t.Fatalf("an ignored assignee was taken for done: %v", err)
	}
	if err := redmine.Assign(ctx, issue, Account{Login: "no id"}); err == nil {
		t.Fatal("an account without an id was assigned")
	}
	before := len(fake.called())
	if err := redmine.RecordHours(ctx, issue, 1.5); err != nil || len(fake.called()) != before {
		t.Fatalf("hours were sent: %v %v", err, fake.called()[before:])
	}
	for _, call := range fake.called() {
		if call == "PUT /issues/5.json" {
			continue
		}
		if strings.HasPrefix(call, "PUT") || strings.HasPrefix(call, "POST") || strings.HasPrefix(call, "DELETE") {
			t.Fatalf("a change other than the issue's fields was sent: %s", call)
		}
	}
}
