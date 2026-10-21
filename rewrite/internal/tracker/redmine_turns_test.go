package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
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

// A refusal says the note was not saved; a failure that leaves it unknown
// says it may already be posted. (The 4xx case fails at b997c63, which said
// "may already be posted" for every failure.)
func TestRedmineCommentThatCannotBeConfirmedSaysWhetherItMayBePosted(t *testing.T) {
	for _, failure := range []string{"refused", "server error", "unreadable after", "nothing new"} {
		t.Run(failure, func(t *testing.T) {
			fake, redmine := redmineFixture(t)
			reads := 0
			switch failure {
			case "refused":
				fake.before = func(w http.ResponseWriter, r *http.Request) bool {
					if r.Method != http.MethodPut {
						return false
					}
					w.WriteHeader(http.StatusUnprocessableEntity)
					fmt.Fprint(w, `{"errors":["Due date cannot be blank"]}`)
					return true
				}
			case "server error":
				fake.refuse["PUT /issues/5.json"] = http.StatusBadGateway
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
			case "nothing new":
				// Redmine answers 204 and stores nothing when the role lacks
				// Add notes. The same words posted earlier by the same
				// account are not this post.
				fake.lose = true
				fake.journals = append(fake.journals, map[string]any{"id": 284, "user": map[string]any{"id": 5}, "notes": "a question"})
			}
			_, err := redmine.AddComment(context.Background(), Issue{ID: 5, Key: "5"}, "a question")
			saved := map[string]bool{"refused": false, "server error": true, "unreadable after": true, "nothing new": false}[failure]
			if err == nil || saved != strings.Contains(err.Error(), "may already be posted; inspect comments before retrying") ||
				!saved && !strings.Contains(err.Error(), "comment was not saved") {
				t.Fatalf("the failure misstates whether the note may be posted: %v", err)
			}
			if failure == "refused" && !strings.Contains(err.Error(), "Due date cannot be blank") {
				t.Fatalf("Redmine's reason was lost: %v", err)
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

// The account the key belongs to does not change while the engine runs: it is
// asked once, not before every note. (Fails at b997c63, which asked each time.)
func TestRedmineAsksForItsOwnAccountOnce(t *testing.T) {
	fake, redmine := redmineFixture(t)
	for _, words := range []string{"first", "second"} {
		if _, err := redmine.AddComment(context.Background(), Issue{ID: 5, Key: "5"}, words); err != nil {
			t.Fatal(err)
		}
	}
	asked := 0
	for _, call := range fake.called() {
		if call == "GET /users/current.json" {
			asked++
		}
	}
	if asked != 1 {
		t.Fatalf("asked for its own account %d times: %v", asked, fake.called())
	}
}

// Redmine keeps a note in other words than sent (a database that cannot hold
// every character, a length limit, a plugin). The only new note by the
// engine's account is that note; two new ones that cannot be told apart leave
// it saved without an id, which is not to be posted again. (The first fails
// at b997c63, which found no note and said it may be posted; the second too.)
func TestRedmineFindsANoteRedmineKeptInOtherWords(t *testing.T) {
	for _, others := range []int{0, 1} {
		fake, redmine := redmineFixture(t)
		fake.before = func(w http.ResponseWriter, r *http.Request) bool {
			if r.Method != http.MethodPut {
				return false
			}
			for range others {
				fake.next++
				fake.journals = append(fake.journals, map[string]any{"id": fake.next, "user": map[string]any{"id": 5}, "notes": "a role's note"})
			}
			fake.next++
			fake.journals = append(fake.journals, map[string]any{"id": fake.next, "user": map[string]any{"id": 5}, "notes": "a notice ????"})
			w.WriteHeader(http.StatusNoContent)
			return true
		}
		id, err := redmine.AddComment(context.Background(), Issue{ID: 5, Key: "5"}, "a notice 😀😀😀😀")
		want := map[int]int64{0: 901, 1: 0}[others]
		if err != nil || id != want {
			t.Fatalf("with %d other new notes: id=%d (%v), want %d", others, id, err, want)
		}
	}
}

// A role's client gives up after 30 seconds; a note it sent is still read
// back and its id recorded for the launch, so the launch's cleanup knows it
// and the role is not left to post it again unseen. (Fails at b997c63, whose
// read back stopped with the role's request and recorded nothing.)
func TestRedmineRecordsAPostWhoseRoleStoppedWaiting(t *testing.T) {
	fake, redmine := redmineFixture(t)
	var recorded []int64
	scope, err := NewIssueScope(redmine, "5", true, ObserveStoredPost(func(id int64) error {
		recorded = append(recorded, id)
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sent := false
	fake.before = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPut {
			sent = true
		} else if sent && r.Method == http.MethodGet && r.URL.Path == "/issues/5.json" {
			cancel() // the role's client gives up while the note is read back
			time.Sleep(50 * time.Millisecond)
		}
		return false
	}
	form := url.Values{"content": {"the role's question"}}.Encode()
	request := httptest.NewRequest(http.MethodPost, "/issues/5/comments?apiKey="+scope.Key(), strings.NewReader(form)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	scope.ServeHTTP(httptest.NewRecorder(), request)
	if len(recorded) != 1 || recorded[0] != 901 {
		t.Fatalf("recorded %v; the note's id 901 was lost with the role's request", recorded)
	}
	notes := 0
	for _, journal := range fake.journals {
		if journal["notes"] == "the role's question" {
			notes++
		}
	}
	if notes != 1 {
		t.Fatalf("the note was stored %d times", notes)
	}
}
