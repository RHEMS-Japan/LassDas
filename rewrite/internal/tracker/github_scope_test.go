package tracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func githubScopeFixture(t *testing.T, handler func(string, int32, http.ResponseWriter, *http.Request), post bool, options ...func(*IssueScope)) (GitHub, Backlog, *IssueScope, *atomic.Int32) {
	t.Helper()
	g, calls := githubFixture(t, handler)
	scope, err := NewIssueScope(g, "7", post, options...)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(scope)
	t.Cleanup(server.Close)
	t.Setenv("SCOPE_TEST_WORKER", scope.Key())
	return g, Backlog{BaseURL: server.URL, KeyEnv: "SCOPE_TEST_WORKER", Client: server.Client()}, scope, calls
}

func githubScopeComment(base string, id int64, body string) map[string]any {
	return map[string]any{"id": id, "body": body, "issue_url": base + "/repos/octo-org/widgets/issues/7",
		"user": map[string]any{"id": 55, "login": "requester"}, "created_at": "2026-01-03T00:00:00Z", "future": true}
}

func TestGitHubScopeReadsPostsAndReadsBackUnmodifiedProse(t *testing.T) {
	const prose = "\r\n日本語 😀 @example #17\r\n{\"unknown\":null} $(literal) `text` & + %\n"
	var posts atomic.Int32
	_, worker, _, _ := githubScopeFixture(t, func(base string, _ int32, w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /repos/octo-org/widgets/issues/7":
			record := githubRecord(base, 7)
			record["body"] = prose
			json.NewEncoder(w).Encode(record)
		case "POST /repos/octo-org/widgets/issues/7/comments":
			posts.Add(1)
			var form map[string]string
			if json.NewDecoder(r.Body).Decode(&form) != nil || len(form) != 1 || form["body"] != prose {
				t.Error("the comment text or authority changed")
			}
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(githubScopeComment(base, 41, prose))
		case "GET /repos/octo-org/widgets/issues/comments/41":
			json.NewEncoder(w).Encode(githubScopeComment(base, 41, prose))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			w.WriteHeader(400)
		}
	}, true)
	ctx := context.Background()
	text, err := worker.Request(ctx, "7")
	if err != nil || text != "Original issue: 7\nTitle: Original title\n\n"+prose {
		t.Fatalf("read: %q %v", text, err)
	}
	posted, err := worker.AddComment(ctx, "7", prose)
	if err != nil {
		t.Fatal(err)
	}
	read, err := worker.Comment(ctx, "7", 41)
	var normalized struct {
		ID          int64
		Content     string
		Created     string
		CreatedUser struct {
			ID           int64
			UserID, Name string
		}
		Future bool
	}
	if err != nil || !bytes.Equal(posted, read) || json.Unmarshal(read, &normalized) != nil || normalized.ID != 41 || normalized.Content != prose ||
		normalized.Created != "2026-01-03T00:00:00Z" || normalized.CreatedUser.ID != 55 || normalized.CreatedUser.UserID != "requester" || normalized.CreatedUser.Name != "requester" || !normalized.Future || posts.Load() != 1 {
		t.Fatalf("readback: %s (%v), posts=%d", read, err, posts.Load())
	}
}

func TestGitHubScopeRejectsOperationsBeforeSendingAnything(t *testing.T) {
	_, _, scope, calls := githubScopeFixture(t, func(_ string, _ int32, w http.ResponseWriter, r *http.Request) {
		t.Errorf("outside authority reached GitHub: %s %s", r.Method, r.URL)
	}, true, KeepLatestPost)
	for _, test := range []struct{ method, path, query, body string }{
		{"GET", "/issues/8", "", ""}, {"POST", "/issues/8/comments", "", "content=hello"},
		{"GET", "/issues", "", ""}, {"GET", "/repos/another/project/issues/7", "", ""},
		{"GET", "/issues/70", "", ""}, {"GET", "/issues/7/../8", "", ""},
		{"GET", "/issues/7/%2e%2e/8", "", ""}, {"GET", "/issues/7%2f..%2f8", "", ""},
		{"GET", "/issues/7/comments/01", "", ""}, {"GET", "/issues/7/comments/41/../../8", "", ""},
		{"GET", "/issues/7", "issueId=8", ""}, {"GET", "/issues/7", "apiKey=other", ""},
		{"GET", "/issues/7/comments", "count=101", ""}, {"GET", "/issues/7/comments", "count=1&count=2", ""},
		{"GET", "/issues/7/comments", "minId=-1", ""}, {"GET", "/issues/7/comments", "order=desc", ""},
		{"GET", "/issues/7/comments", "page=2", ""}, {"GET", "/issues/7", "bad=%zz", ""},
		{"PATCH", "/issues/7", "", "statusId=4"}, {"DELETE", "/issues/7/comments/41", "", ""},
		{"POST", "/issues/7/comments", "", "content=hello&assigneeId=2"},
		{"POST", "/issues/7/comments", "", "content=hello&notifiedUserId[]=2"},
		{"POST", "/issues/7/comments", "", "content=hello&content=again"},
		{"POST", "/issues/7/comments", "", "state=closed"},
	} {
		t.Run(test.method+test.path+test.query+test.body, func(t *testing.T) {
			r := httptest.NewRequest(test.method, test.path+"?apiKey="+scope.Key()+"&"+test.query, strings.NewReader(test.body))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w := httptest.NewRecorder()
			scope.ServeHTTP(w, r)
			if w.Code < 400 || w.Code >= 500 || calls.Load() != 0 {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, calls.Load(), w.Body)
			}
		})
	}
	readOnly, err := NewIssueScope(scope.source, "7", false)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/issues/7/comments?apiKey="+readOnly.Key(), strings.NewReader("content=hello"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	readOnly.ServeHTTP(w, r)
	if w.Code != 403 || calls.Load() != 0 {
		t.Fatalf("read-only scope wrote: status=%d calls=%d", w.Code, calls.Load())
	}
}

func TestGitHubScopeDoesNotReadOrRemoveAnotherIssuesComment(t *testing.T) {
	for _, mismatch := range []string{"issue", "repository", "host", "missing association", "id"} {
		t.Run(mismatch, func(t *testing.T) {
			g, worker, _, calls := githubScopeFixture(t, func(base string, _ int32, w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/repos/octo-org/widgets/issues/comments/41" {
					t.Errorf("unsafe request: %s %s", r.Method, r.URL)
				}
				comment := githubScopeComment(base, 41, "private to another issue")
				switch mismatch {
				case "issue":
					comment["issue_url"] = base + "/repos/octo-org/widgets/issues/8"
				case "repository":
					comment["issue_url"] = base + "/repos/another/project/issues/7"
				case "host":
					comment["issue_url"] = "https://tracker.invalid/repos/octo-org/widgets/issues/7"
				case "missing association":
					delete(comment, "issue_url")
				case "id":
					comment["id"] = 42
				}
				json.NewEncoder(w).Encode(comment)
			}, true)
			data, err := worker.Comment(context.Background(), "7", 41)
			var missing *trackerError
			if !errors.As(err, &missing) || missing.Status != 404 || len(data) != 0 || strings.Contains(err.Error(), "private to another issue") {
				t.Fatalf("a foreign comment was exposed: %s %v", data, err)
			}
			if _, err := g.Forward(context.Background(), "DELETE", "/issues/7/comments/41", nil, nil, 200); err == nil || calls.Load() != 2 {
				t.Fatalf("cleanup did not recheck the association: %v calls=%d", err, calls.Load())
			}
		})
	}
}

func TestGitHubScopeOnlyRemovesEarlierConfirmedPosts(t *testing.T) {
	for _, failure := range []string{"", "post", "delete", "association", "no cleanup"} {
		t.Run(failure, func(t *testing.T) {
			githubClock(t)
			var mu sync.Mutex
			var operations []string
			observed := func() string {
				mu.Lock()
				defer mu.Unlock()
				return strings.Join(operations, ",")
			}
			options := []func(*IssueScope){KeepLatestPost}
			if failure == "no cleanup" {
				options = nil
			}
			posts := int64(40)
			_, worker, scope, _ := githubScopeFixture(t, func(base string, _ int32, w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				operations = append(operations, r.Method+" "+r.URL.Path)
				switch r.Method {
				case "POST":
					posts++
					if failure == "post" && posts == 42 {
						w.WriteHeader(500)
						return
					}
					w.WriteHeader(201)
					json.NewEncoder(w).Encode(githubScopeComment(base, posts, "question"))
				case "GET":
					comment := githubScopeComment(base, 41, "earlier question")
					if failure == "association" {
						comment["issue_url"] = base + "/repos/octo-org/widgets/issues/8"
					}
					json.NewEncoder(w).Encode(comment)
				case "DELETE":
					if failure == "delete" {
						w.WriteHeader(403)
					} else {
						w.WriteHeader(204)
					}
				}
			}, true, options...)
			if _, err := worker.AddComment(context.Background(), "7", "trial"); err != nil {
				t.Fatal(err)
			}
			_, err := worker.AddComment(context.Background(), "7", "the question")
			if (err != nil) != (failure == "post") {
				t.Fatal(err)
			}
			if strings.Count(observed(), ",") != 1 {
				t.Fatalf("cleanup ran during posting: %s", observed())
			}
			removed := scope.RemoveEarlierPosts(context.Background())
			wantRemoved := 0
			if failure == "" {
				wantRemoved = 1
			}
			if removed != wantRemoved {
				t.Fatalf("removed=%d want=%d", removed, wantRemoved)
			}
			want := "POST /repos/octo-org/widgets/issues/7/comments,POST /repos/octo-org/widgets/issues/7/comments"
			if failure != "post" && failure != "no cleanup" {
				want += ",GET /repos/octo-org/widgets/issues/comments/41"
			}
			if failure == "" || failure == "delete" {
				want += ",DELETE /repos/octo-org/widgets/issues/comments/41"
			}
			if scope.RemoveEarlierPosts(context.Background()) != 0 || observed() != want {
				t.Fatalf("unexpected cleanup: %s", observed())
			}
		})
	}
}

func TestGitHubScopeSendsNothingDuringTheRequestedWait(t *testing.T) {
	_, _, advance := githubClock(t)
	_, worker, _, calls := githubScopeFixture(t, func(_ string, _ int32, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(429)
		fmt.Fprint(w, "please wait")
	}, true)
	ctx := context.Background()
	if _, err := worker.Request(ctx, "7"); err == nil {
		t.Fatal("rate limit hidden")
	}
	for _, read := range []func() error{
		func() error { _, err := worker.Request(ctx, "7"); return err },
		func() error { _, err := worker.Comments(ctx, "7", 0); return err },
		func() error { _, err := worker.Comment(ctx, "7", 41); return err },
		func() error { _, err := worker.AddComment(ctx, "7", "report"); return err },
	} {
		if err := read(); err == nil || !strings.Contains(err.Error(), "nothing was sent") || calls.Load() != 1 {
			t.Fatalf("wait bypassed: %v calls=%d", err, calls.Load())
		}
	}
	advance(121 * time.Second)
	worker.Request(ctx, "7")
	if calls.Load() != 2 {
		t.Fatal("the expired wait still blocked requests")
	}
}

func TestGitHubScopeCLIReadsAllCommentsAndNoPartialList(t *testing.T) {
	// Build the real CLI. Its process receives only this launch's access, not
	// the synthetic upstream account credential held by the test controller.
	binary := filepath.Join(t.TempDir(), "tracker")
	build := exec.Command("go", "build", "-p", "1", "-o", binary, "../../cmd/tracker")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %s %v", out, err)
	}
	var failLater atomic.Bool
	g, _ := githubFixture(t, func(base string, _ int32, w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/repos/octo-org/widgets/issues/7/comments" || r.URL.Query().Get("per_page") != "100" {
			t.Errorf("unexpected CLI request: %s %s", r.Method, r.URL)
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		page = max(page, 1)
		if failLater.Load() && page == 2 {
			w.WriteHeader(503)
			fmt.Fprint(w, "later page unavailable")
			return
		}
		rows := []any{}
		for id := (page-1)*100 + 1; id <= min(page*100, 205); id++ {
			rows = append(rows, githubScopeComment(base, int64(id), fmt.Sprintf("日本語 %d\r\n", id)))
		}
		if page < 3 {
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/octo-org/widgets/issues/7/comments?per_page=100&page=%d>; rel="next"`, base, page+1))
		}
		json.NewEncoder(w).Encode(rows)
	})
	access, err := ServeIssue(context.Background(), g, "7", false)
	if err != nil {
		t.Fatal(err)
	}
	defer access.Close()
	for _, fail := range []bool{false, true} {
		failLater.Store(fail)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		command := exec.CommandContext(ctx, binary, "--base-url", access.URL, "--key-env", "SCOPE_CLI_KEY", "--cert-env", "SCOPE_CLI_CERT", "--issue", "7", "comments")
		command.Env = []string{"SCOPE_CLI_KEY=" + access.Key, "SCOPE_CLI_CERT=" + access.Certificate}
		var out, log bytes.Buffer
		command.Stdout, command.Stderr = &out, &log
		err := command.Run()
		cancel()
		if fail {
			if err == nil || out.Len() != 0 || !strings.Contains(log.String(), "later page unavailable") {
				t.Fatalf("partial list: %v stdout=%s stderr=%s", err, &out, &log)
			}
			continue
		}
		var comments []struct {
			ID      int64
			Content string
		}
		if err != nil || log.Len() != 0 || json.Unmarshal(out.Bytes(), &comments) != nil || len(comments) != 205 {
			t.Fatalf("CLI comments: %v stdout=%s stderr=%s", err, &out, &log)
		}
		for index, comment := range comments {
			if comment.ID != int64(index+1) || comment.Content != fmt.Sprintf("日本語 %d\r\n", index+1) {
				t.Fatalf("changed comment: %+v", comment)
			}
		}
	}
}

func TestGitHubForwardRejectsUnsupportedRequestsWithoutSending(t *testing.T) {
	g, calls := githubFixture(t, func(_ string, _ int32, w http.ResponseWriter, r *http.Request) { t.Error("unsupported request sent") })
	for _, path := range []string{"/issues", "/users/myself", "/repos/another/project/issues/7", "/issues/07", "/issues/7/../8", "/issues/7/comments/01"} {
		if _, err := g.Forward(context.Background(), "GET", path, nil, nil, 200); err == nil {
			t.Errorf("accepted %s", path)
		}
	}
	if _, err := g.Forward(context.Background(), "POST", "/issues/7/comments", nil, url.Values{"content": {"text"}, "assignee": {"other"}}, 201); err == nil {
		t.Error("additional form accepted")
	}
	if calls.Load() != 0 {
		t.Fatal("requests were sent")
	}
}

func TestGitHubScopeDoesNotRetryAnUnconfirmedPost(t *testing.T) {
	_, worker, _, calls := githubScopeFixture(t, func(_ string, _ int32, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		io.WriteString(w, "unreadable receipt")
	}, true)
	if _, err := worker.AddComment(context.Background(), "7", "report"); err == nil || !strings.Contains(err.Error(), "may already be posted") || calls.Load() != 1 {
		t.Fatalf("ambiguous submission was hidden or retried: %v calls=%d", err, calls.Load())
	}
}

func TestGitHubScopeRejectsAnIssueFromElsewhereOrAPullRequest(t *testing.T) {
	for _, mismatch := range []string{"number", "repository", "pull request"} {
		t.Run(mismatch, func(t *testing.T) {
			_, worker, _, _ := githubScopeFixture(t, func(base string, _ int32, w http.ResponseWriter, r *http.Request) {
				record := githubRecord(base, 7)
				switch mismatch {
				case "number":
					record["number"] = 8
				case "repository":
					record["repository_url"] = base + "/repos/another/project"
				case "pull request":
					record["pull_request"] = map[string]string{"url": base + "/repos/octo-org/widgets/pulls/7"}
				}
				json.NewEncoder(w).Encode(record)
			}, false)
			if text, err := worker.Request(context.Background(), "7"); err == nil || text != "" {
				t.Fatalf("unassigned request exposed: %q %v", text, err)
			}
		})
	}
}

func TestGitHubScopeChecksEveryListedCommentsIssue(t *testing.T) {
	_, worker, _, _ := githubScopeFixture(t, func(base string, _ int32, w http.ResponseWriter, r *http.Request) {
		comment := githubScopeComment(base, 41, "private to another issue")
		comment["issue_url"] = base + "/repos/octo-org/widgets/issues/8"
		json.NewEncoder(w).Encode([]any{comment})
	}, false)
	for _, after := range []int64{0, 41} {
		if rows, err := worker.Comments(context.Background(), "7", after); err == nil || len(rows) != 0 || strings.Contains(err.Error(), "private to another issue") {
			t.Fatalf("foreign comment accepted after %d: %s %v", after, rows, err)
		}
	}
}
