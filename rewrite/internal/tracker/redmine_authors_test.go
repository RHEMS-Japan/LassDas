package tracker

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
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

// A look-up that fails for another reason than not found fails the list and
// the read, rather than leaving an author out by mistake.
func TestRedmineFailsAReadWhoseAuthorCannotBeLookedUp(t *testing.T) {
	fake, redmine := redmineFixture(t)
	fake.refuse["GET /users/6.json"] = http.StatusForbidden
	if _, err := redmine.Comments(context.Background(), Issue{ID: 5, Key: "5"}); err == nil || !strings.Contains(err.Error(), "author 6") {
		t.Fatalf("a read went on without its author: %v", err)
	}
	fake.before = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/issues.json" {
			return false
		}
		servedIssues(t, 1, w, r, nil)
		return true
	}
	if rows, err := redmine.Issues(context.Background()); err == nil || rows != nil {
		t.Fatalf("a list went on without its author: %d %v", len(rows), err)
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
