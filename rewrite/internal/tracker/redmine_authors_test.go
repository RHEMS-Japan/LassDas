package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Everyone who writes without an account writes as Redmine's anonymous user,
// whose id and translated name differ from one Redmine to the next. The
// engine looks each author up: Redmine answers 404 for the anonymous user to
// a key that is not an administrator's, and gives an administrator's an empty
// login. An issue it filed is not listed, and its notes are nobody's. (Both
// fail at b997c63, which took the anonymous author for the requester.)
func TestRedmineTakesNothingFromRedminesAnonymousUser(t *testing.T) {
	for name, users := range map[string]map[int64]string{
		"not an administrator's key": {5: "engine-bot", 6: "requester"},
		"an administrator's key":     {5: "engine-bot", 6: "requester", 4: ""},
	} {
		t.Run(name, func(t *testing.T) {
			fake, redmine := redmineFixture(t)
			fake.users = users
			fake.before = func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != "/issues.json" {
					return false
				}
				servedIssues(t, 3, w, r, func(id int, record map[string]any) {
					if id == 2 {
						record["author"] = map[string]any{"id": 4, "name": "匿名ユーザー"}
					}
				})
				return true
			}
			rows, err := redmine.Issues(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			var listed []int64
			for _, raw := range rows {
				issue, _ := redmine.ReadIssue(raw)
				listed = append(listed, issue.ID)
			}
			if len(listed) != 2 || listed[0] != 1 || listed[1] != 3 {
				t.Fatalf("listed %v; the anonymous user's issue 2 must be left out", listed)
			}
			fake.journals = append(fake.journals, map[string]any{"id": 900, "user": map[string]any{"id": 4, "name": "Anonymous"}, "notes": "停止"})
			journals, err := redmine.Comments(context.Background(), Issue{ID: 5, Key: "5"})
			if err != nil {
				t.Fatal(err)
			}
			comment, err := redmine.ReadComment(journals[len(journals)-1], Issue{ID: 5, Key: "5"})
			if err != nil || comment.ID != 900 || comment.Author.ID != 0 || !comment.OnIssue {
				t.Fatalf("the anonymous note read as %+v (%v)", comment, err)
			}
			if requester, _ := redmine.ReadComment(journals[1], Issue{ID: 5, Key: "5"}); requester.Author.ID != 6 {
				t.Fatalf("the requester's note lost its author: %+v", requester)
			}
		})
	}
}

// A look-up that fails for another reason than not found fails a read of the
// issue's notes, rather than leaving an author out by mistake. In the intake
// it holds only the issues of that author, poll by poll, and says so in the
// engine's log when the hold begins, again every ten minutes while it lasts,
// with how many polls it has been held, and when it ends. The other issues
// are listed. (Fails at b84f97f, which said the hold once and never again.)
func TestRedmineHoldsOnlyTheIssueWhoseAuthorCannotBeLookedUpAndSaysSoWhileHeld(t *testing.T) {
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	previous := redmineNow
	redmineNow = func() time.Time { return now }
	t.Cleanup(func() { redmineNow = previous })
	fake, redmine := redmineFixture(t)
	fake.users[8] = "another-requester"
	fake.refuse["GET /users/6.json"] = http.StatusForbidden
	if _, err := redmine.Comments(context.Background(), Issue{ID: 5, Key: "5"}); err == nil || !strings.Contains(err.Error(), "author 6") {
		t.Fatalf("a read went on without its author: %v", err)
	}
	fake.before = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/issues.json" {
			return false
		}
		servedIssues(t, 3, w, r, func(id int, record map[string]any) {
			if id != 2 {
				record["author"] = map[string]any{"id": 8, "name": "Another"}
			}
		})
		return true
	}
	var said []string
	observed := redmine.ObservedBy(func(line string) { said = append(said, line) })
	poll := func(after time.Duration, want string) {
		t.Helper()
		now = now.Add(after)
		rows, err := observed.Issues(context.Background())
		var listed []int64
		for _, raw := range rows {
			issue, _ := redmine.ReadIssue(raw)
			listed = append(listed, issue.ID)
		}
		if err != nil || fmt.Sprint(listed) != want {
			t.Fatalf("listed %v (%v), want %s", listed, err, want)
		}
	}
	poll(0, "[1 3]")
	poll(time.Minute, "[1 3]")
	poll(5*time.Minute, "[1 3]")
	poll(5*time.Minute, "[1 3]") // ten minutes after the first line
	delete(fake.refuse, "GET /users/6.json")
	poll(time.Minute, "[1 2 3]")
	if len(said) != 3 || !strings.Contains(said[0], "Redmine issue 2 is held from the intake (1 polls so far") || !strings.Contains(said[0], "403") ||
		!strings.Contains(said[1], "Redmine issue 2 is held from the intake (4 polls so far") ||
		!strings.Contains(said[2], "Redmine issue 2 is no longer held from the intake, after 4 polls") {
		t.Fatalf("the engine's log said %q", said)
	}
}

// An issue left out because its author is no person's account is said once in
// the engine's log, with the reason. (Fails at 3adf54a, which said nothing.)
func TestRedmineSaysOnceWhyItLeftAnIssueOut(t *testing.T) {
	fake, redmine := redmineFixture(t)
	fake.before = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/issues.json" {
			return false
		}
		servedIssues(t, 2, w, r, func(id int, record map[string]any) {
			if id == 2 {
				record["author"] = map[string]any{"id": 4, "name": "Anonymous"}
			}
		})
		return true
	}
	var said []string
	observed := redmine.ObservedBy(func(line string) { said = append(said, line) })
	for range 3 {
		if rows, err := observed.Issues(context.Background()); err != nil || len(rows) != 1 {
			t.Fatalf("issues=%d error=%v", len(rows), err)
		}
	}
	if len(said) != 1 || !strings.Contains(said[0], "Redmine issue 2 is left out of the intake: its author 4 is not an account") {
		t.Fatalf("the engine's log said %q", said)
	}
}

// Each author is looked up once, not on every read; that an id is no person's
// account is kept for ten minutes only, then asked again. (The first fails
// whenever the look-ups are not kept; the second fails at 3adf54a, which kept
// "no person" until the engine restarted.)
func TestRedmineKeepsWhatItLearnedOfAnAuthorForAWhile(t *testing.T) {
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	previous := redmineNow
	redmineNow = func() time.Time { return now }
	t.Cleanup(func() { redmineNow = previous })
	fake, redmine := redmineFixture(t)
	fake.journals = append(fake.journals, map[string]any{"id": 900, "user": map[string]any{"id": 4}, "notes": "a note without an account"})
	issue := Issue{ID: 5, Key: "5"}
	for range 3 {
		if _, err := redmine.Comments(context.Background(), issue); err != nil {
			t.Fatal(err)
		}
	}
	if got := fmt.Sprint(fake.lookups()); got != "[GET /users/5.json GET /users/6.json GET /users/4.json]" {
		t.Fatalf("authors looked up: %s; each is to be looked up once", got)
	}
	// The account becomes visible; within ten minutes it is still nobody.
	fake.users[4] = "now-visible"
	now = now.Add(9 * time.Minute)
	rows, _ := redmine.Comments(context.Background(), issue)
	if comment, _ := redmine.ReadComment(rows[len(rows)-1], issue); comment.Author.ID != 0 {
		t.Fatalf("read as %+v within the ten minutes", comment)
	}
	now = now.Add(2 * time.Minute)
	rows, _ = redmine.Comments(context.Background(), issue)
	if comment, _ := redmine.ReadComment(rows[len(rows)-1], issue); comment.Author.ID != 4 {
		t.Fatalf("read as %+v after the ten minutes", comment)
	}
	if n := len(fake.lookups()); n != 4 {
		t.Fatalf("%d look-ups; only the one taken for no person is asked again", n)
	}
}

// Only an empty login (Redmine's anonymous user, as an administrator's key
// sees it) or a 404 says an id is no person's. An answer without the field at
// all does not shut everyone out. (Fails at 3adf54a, which read a missing
// login as an empty one.)
func TestRedmineTakesAnAccountWithoutALoginFieldForAPerson(t *testing.T) {
	fake, redmine := redmineFixture(t)
	fake.users[6] = redmineNoLogin
	fake.before = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/issues.json" {
			return false
		}
		servedIssues(t, 1, w, r, nil)
		return true
	}
	if rows, err := redmine.Issues(context.Background()); err != nil || len(rows) != 1 {
		t.Fatalf("an issue by an account without a login field was left out: %d %v", len(rows), err)
	}
	rows, err := redmine.Comments(context.Background(), Issue{ID: 5, Key: "5"})
	if err != nil {
		t.Fatal(err)
	}
	if comment, _ := redmine.ReadComment(rows[1], Issue{ID: 5, Key: "5"}); comment.Author.ID != 6 {
		t.Fatalf("a note by an account without a login field read as %+v", comment)
	}
}

// Redmine 5.0 and later names under updated_by the account that edited a
// note. Words someone else edited are the editor's, not the first author's
// answer or stop. (Fails at b997c63, which credited them to the first author.)
func TestRedmineCreditsAnEditedNoteToItsEditor(t *testing.T) {
	redmine := RedmineProject{ProjectID: 1}
	issue := Issue{ID: 5, Key: "5", Creator: Account{ID: 6}}
	for raw, want := range map[string]int64{
		`{"issue":{"id":5},"id":7,"notes":"停止","user":{"id":6},"updated_by":{"id":3}}`: 3,
		`{"issue":{"id":5},"id":7,"notes":"停止","user":{"id":6},"updated_by":{"id":6}}`: 6,
		`{"issue":{"id":5},"id":7,"notes":"停止","user":{"id":6}}`:                       6,
		`{"issue":{"id":5},"id":7,"notes":"停止","user":{"id":6},"updated_by":null}`:     6,
	} {
		comment, err := redmine.ReadComment(json.RawMessage(raw), issue)
		if err != nil || comment.Author.ID != want {
			t.Errorf("%s: author %d, want %d (%v)", raw, comment.Author.ID, want, err)
		}
	}
}
