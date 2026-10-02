package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// GitHub answers everything the engine asks of its tracker but Forward, which
// waits for how a role is to see a GitHub issue.
var _ Tracker = struct {
	GitHub
	Upstream
}{}

func TestGitHubPostsACommentOnceAndNeverAgain(t *testing.T) {
	githubClock(t)
	const text = "\n受け付けました。\nUnknown fields: {\"anything\":null}\nLiteral $(not-a-command), `code`, & + % @someone #12\n"
	for _, outcome := range []string{"stored", "disconnect", "unreadable", "missing-id", "redirect", "rate-limit"} {
		t.Run(outcome, func(t *testing.T) {
			github, calls := githubFixture(t, func(base string, call int32, w http.ResponseWriter, r *http.Request) {
				var sent map[string]string
				if r.Method != http.MethodPost || r.URL.Path != "/repos/octo-org/widgets/issues/12/comments" || r.Header.Get("Content-Type") != "application/json" ||
					json.NewDecoder(r.Body).Decode(&sent) != nil || len(sent) != 1 || sent["body"] != text {
					t.Errorf("the comment was asked as %s %s %q", r.Method, r.URL.Path, sent)
				}
				switch outcome {
				case "stored":
					w.WriteHeader(http.StatusCreated)
					fmt.Fprint(w, `{"id":77,"body":"stored"}`)
				case "disconnect":
					connection, _, _ := w.(http.Hijacker).Hijack()
					connection.Close()
				case "unreadable":
					w.WriteHeader(http.StatusCreated)
					fmt.Fprint(w, "not JSON")
				case "missing-id":
					w.WriteHeader(http.StatusCreated)
					fmt.Fprint(w, `{"body":"stored"}`)
				case "redirect":
					w.Header().Set("Location", "/elsewhere")
					w.WriteHeader(http.StatusTemporaryRedirect)
				case "rate-limit":
					w.WriteHeader(http.StatusTooManyRequests)
					fmt.Fprint(w, "try later "+githubTestToken)
				}
			})
			id, err := github.AddComment(context.Background(), Issue{ID: 12, Key: "12"}, text)
			if outcome == "stored" {
				if err != nil || id != 77 || calls.Load() != 1 {
					t.Fatalf("id=%d calls=%d (%v)", id, calls.Load(), err)
				}
				return
			}
			if err == nil || id != 0 || calls.Load() != 1 || !strings.Contains(err.Error(), "inspect comments") || strings.Contains(err.Error(), githubTestToken) {
				t.Fatalf("an uncertain post was repeated, hidden or leaked the token: calls=%d (%v)", calls.Load(), err)
			}
			if outcome == "rate-limit" && !strings.Contains(err.Error(), "429: try later") {
				t.Fatalf("the reason was lost: %v", err)
			}
		})
	}
}

func TestGitHubMovesTheIssueByLabelsAndLeavesPeoplesOwn(t *testing.T) {
	githubClock(t)
	carried := []string{"bug", "engine:accepted", "engine/question?"}
	gone := false
	var asked []string
	github, _ := githubFixture(t, func(base string, call int32, w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		asked = append(asked, strings.TrimSpace(r.Method+" "+r.URL.EscapedPath()+" "+string(body)))
		switch r.Method {
		case http.MethodPost:
			var added struct{ Labels []string }
			json.Unmarshal(body, &added)
			for _, name := range added.Labels {
				if !strings.Contains(strings.Join(carried, "\n"), name) && name != "never confirmed" {
					carried = append(carried, name)
				}
			}
		case http.MethodDelete:
			name := strings.TrimPrefix(r.URL.Path, "/repos/octo-org/widgets/issues/12/labels/")
			kept := []string{}
			for _, label := range carried {
				if label != name {
					kept = append(kept, label)
				}
			}
			if len(kept) == len(carried) || gone {
				// Someone took it off a moment before.
				carried = kept
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"message":"Label does not exist"}`)
				return
			}
			carried = kept
		}
		labels := []map[string]string{}
		for _, name := range carried {
			labels = append(labels, map[string]string{"name": name})
		}
		json.NewEncoder(w).Encode(labels)
	})
	github.Labels = GitHubLabels{Accepted: "engine:accepted", Processing: "engine: working", AwaitingRequester: "engine/question?", Delivered: "engine:delivered"}
	if string(github.Target(Processing)) != `"engine: working"` || github.Target(Stopped) != nil || (GitHub{}).Target(Accepted) != nil {
		t.Fatal("a turn's target is not its label, or a turn without one has one")
	}
	ctx := context.Background()
	issue := Issue{ID: 12, Key: "12"}
	for _, turn := range []string{Accepted, Processing, AwaitingRequester} {
		if err := github.Move(ctx, issue, turn); err != nil {
			t.Fatalf("%s: %v", turn, err)
		}
	}
	// A working label someone took off meanwhile counts as taken off once the
	// issue's labels, read again, no longer hold it.
	gone = true
	if err := github.Move(ctx, issue, Delivered); err != nil {
		t.Fatalf("delivered: %v", err)
	}
	if err := github.Move(ctx, issue, Stopped); err == nil {
		t.Fatal("a turn without a label moved the issue")
	}
	github.Labels.Processing = "never confirmed"
	if err := github.Move(ctx, issue, Processing); err == nil || !strings.Contains(err.Error(), "did not confirm the label") {
		t.Fatalf("an unconfirmed label was taken as given: %v", err)
	}
	want := []string{
		`POST /repos/octo-org/widgets/issues/12/labels {"labels":["engine:accepted"]}`,
		`POST /repos/octo-org/widgets/issues/12/labels {"labels":["engine: working"]}`,
		`DELETE /repos/octo-org/widgets/issues/12/labels/engine%2Fquestion%3F`,
		`POST /repos/octo-org/widgets/issues/12/labels {"labels":["engine/question?"]}`,
		`DELETE /repos/octo-org/widgets/issues/12/labels/engine:%20working`,
		`POST /repos/octo-org/widgets/issues/12/labels {"labels":["engine:delivered"]}`,
		`DELETE /repos/octo-org/widgets/issues/12/labels/engine%2Fquestion%3F`,
		`GET /repos/octo-org/widgets/issues/12/labels`,
		`POST /repos/octo-org/widgets/issues/12/labels {"labels":["never confirmed"]}`,
	}
	if strings.Join(asked, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the tracker was asked:\n%s", strings.Join(asked, "\n"))
	}
	if strings.Join(carried, ",") != "bug,engine:accepted,engine:delivered" {
		t.Fatalf("the issue carries %q", carried)
	}
}

// A label is taken off under the name the issue carries it by, a later move
// takes off the label of a stop too, and a removal GitHub refuses, or answers
// as not found while the issue still carries the label, fails the move.
func TestGitHubTakesALabelOffOnlyWhenItIsGone(t *testing.T) {
	for _, answer := range []string{"removed", "not found but still there", "refused"} {
		t.Run(answer, func(t *testing.T) {
			githubClock(t)
			carried := []string{"bug", "Engine:Working", "engine:stopped"}
			var asked []string
			github, _ := githubFixture(t, func(base string, call int32, w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				asked = append(asked, strings.TrimSpace(r.Method+" "+r.URL.EscapedPath()+" "+string(body)))
				if r.Method == http.MethodPost {
					carried = append(carried, "engine:delivered")
				}
				if r.Method == http.MethodDelete {
					name := strings.TrimPrefix(r.URL.Path, "/repos/octo-org/widgets/issues/12/labels/")
					switch answer {
					case "not found but still there":
						w.WriteHeader(http.StatusNotFound)
						fmt.Fprint(w, `{"message":"Label does not exist"}`)
						return
					case "refused":
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					kept := []string{}
					for _, label := range carried {
						if label != name {
							kept = append(kept, label)
						}
					}
					carried = kept
				}
				labels := []map[string]string{}
				for _, name := range carried {
					labels = append(labels, map[string]string{"name": name})
				}
				json.NewEncoder(w).Encode(labels)
			})
			github.Labels = GitHubLabels{Processing: "engine:working", Delivered: "engine:delivered", Stopped: "engine:stopped"}
			err := github.Move(context.Background(), Issue{ID: 12, Key: "12"}, Delivered)
			switch answer {
			case "removed":
				if err != nil || strings.Join(carried, ",") != "bug,engine:delivered" ||
					!strings.Contains(strings.Join(asked, "\n"), "DELETE /repos/octo-org/widgets/issues/12/labels/Engine:Working") {
					t.Fatalf("carried %q after %q (%v)", carried, asked, err)
				}
			case "not found but still there":
				if err == nil || !strings.Contains(err.Error(), `"Engine:Working" could not be taken off`) {
					t.Fatalf("a label still on the issue was taken as taken off: %v", err)
				}
			case "refused":
				if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
					t.Fatalf("a refused removal was taken as made: %v", err)
				}
			}
		})
	}
}

func TestGitHubHandsTheIssueOverByLoginAndTakesItFromTheOtherSideOnly(t *testing.T) {
	githubClock(t)
	assigned := []string{"someone-else", "requester"}
	ignore := ""
	var asked []string
	github, _ := githubFixture(t, func(base string, call int32, w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path == "/user" {
			fmt.Fprint(w, `{"id":900,"login":"engine-bot"}`)
			return
		}
		asked = append(asked, strings.TrimSpace(r.Method+" "+r.URL.Path+" "+string(body)))
		var change struct{ Assignees []string }
		json.Unmarshal(body, &change)
		for _, login := range change.Assignees {
			if login == ignore {
				continue
			}
			kept := []string{}
			for _, current := range assigned {
				if current != login {
					kept = append(kept, current)
				}
			}
			if assigned = kept; r.Method == http.MethodPost {
				assigned = append(assigned, login)
			}
		}
		users := []map[string]any{}
		for _, login := range assigned {
			users = append(users, map[string]any{"login": login})
		}
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
		}
		json.NewEncoder(w).Encode(map[string]any{"number": 12, "assignees": users})
	})
	ctx := context.Background()
	issue := Issue{ID: 12, Key: "12", Creator: Account{ID: 55, Login: "requester"}}
	runtime := Account{ID: 900, Login: "engine-bot"}
	for _, to := range []Account{runtime, issue.Creator} {
		if err := github.Assign(ctx, issue, to); err != nil {
			t.Fatalf("to %s: %v", to.Login, err)
		}
	}
	if strings.Join(assigned, ",") != "someone-else,requester" {
		t.Fatalf("assigned: %q", assigned)
	}
	// GitHub ignores without a word an account it will not assign.
	ignore = "engine-bot"
	if err := github.Assign(ctx, issue, runtime); err == nil {
		t.Fatal("an assignment GitHub ignored was taken as made")
	}
	if err := github.Assign(ctx, issue, Account{ID: 55}); err == nil {
		t.Fatal("an account without a login was assigned")
	}
	// The same goes for taking an account off.
	ignore = "requester"
	if err := github.Assign(ctx, issue, runtime); err == nil {
		t.Fatal("a removal GitHub ignored was taken as made")
	}
	want := []string{
		`POST /repos/octo-org/widgets/issues/12/assignees {"assignees":["engine-bot"]}`,
		`DELETE /repos/octo-org/widgets/issues/12/assignees {"assignees":["requester"]}`,
		`POST /repos/octo-org/widgets/issues/12/assignees {"assignees":["requester"]}`,
		`DELETE /repos/octo-org/widgets/issues/12/assignees {"assignees":["engine-bot"]}`,
		`POST /repos/octo-org/widgets/issues/12/assignees {"assignees":["engine-bot"]}`,
		`POST /repos/octo-org/widgets/issues/12/assignees {"assignees":["engine-bot"]}`,
		`DELETE /repos/octo-org/widgets/issues/12/assignees {"assignees":["requester"]}`,
	}
	if strings.Join(asked, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the tracker was asked:\n%s", strings.Join(asked, "\n"))
	}
}

func TestGitHubRecordsNoHoursAndAsksNothing(t *testing.T) {
	github, calls := githubFixture(t, func(base string, call int32, w http.ResponseWriter, r *http.Request) {})
	if err := github.RecordHours(context.Background(), Issue{ID: 12, Key: "12"}, 0.25); err != nil || calls.Load() != 0 {
		t.Fatalf("calls=%d (%v)", calls.Load(), err)
	}
}

// On an issue the engine's own account opened, handing it over either way
// leaves the engine's account assigned: it is never taken off as the other
// side. Logins are matched without regard to case, as GitHub matches them,
// and a refused assignment names the login and why GitHub may refuse it.
func TestGitHubKeepsTheEngineOnAnIssueItOpenedAndMatchesLoginsWithoutCase(t *testing.T) {
	githubClock(t)
	// GitHub answers with an account's login as the account spells it.
	assigned := []string{"Engine-Bot"}
	spelled := func(login string) string {
		if strings.EqualFold(login, "engine-bot") {
			return "Engine-Bot"
		}
		return login
	}
	refuse := false
	var asked []string
	github, _ := githubFixture(t, func(base string, call int32, w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path == "/user" {
			fmt.Fprint(w, `{"id":900,"login":"Engine-Bot"}`)
			return
		}
		asked = append(asked, strings.TrimSpace(r.Method+" "+string(body)))
		var change struct{ Assignees []string }
		json.Unmarshal(body, &change)
		for _, login := range change.Assignees {
			kept := []string{}
			for _, current := range assigned {
				if !strings.EqualFold(current, login) {
					kept = append(kept, current)
				}
			}
			if r.Method == http.MethodDelete || !refuse {
				assigned = kept
				if r.Method == http.MethodPost {
					assigned = append(assigned, spelled(login))
				}
			}
		}
		users := []map[string]any{}
		for _, login := range assigned {
			users = append(users, map[string]any{"login": login})
		}
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
		}
		json.NewEncoder(w).Encode(map[string]any{"number": 12, "assignees": users})
	})
	ctx := context.Background()
	issue := Issue{ID: 12, Key: "12", Creator: Account{ID: 900, Login: "engine-bot"}}
	for _, to := range []Account{{ID: 900, Login: "engine-bot"}, issue.Creator} {
		if err := github.Assign(ctx, issue, to); err != nil {
			t.Fatalf("to %s: %v", to.Login, err)
		}
	}
	if strings.Join(assigned, ",") != "Engine-Bot" || strings.Contains(strings.Join(asked, "\n"), "DELETE") {
		t.Fatalf("the engine's own issue was left with %q after %q", assigned, asked)
	}
	refuse = true
	err := github.Assign(ctx, Issue{ID: 12, Key: "12", Creator: Account{ID: 55, Login: "requester"}}, Account{ID: 55, Login: "requester"})
	if err == nil || !strings.Contains(err.Error(), `GitHub did not assign "requester"`) || !strings.Contains(err.Error(), "commented on the issue") || !strings.Contains(err.Error(), "no more than ten") {
		t.Fatalf("a refused assignment does not say whom or why: %v", err)
	}
}
