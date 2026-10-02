package tracker

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// recordingUpstream stands behind a scope in place of a tracker and keeps
// every request that reaches it.
type recordingUpstream struct {
	mu       sync.Mutex
	requests []string
	posts    int
}

func (u *recordingUpstream) Forward(_ context.Context, method, path string, query, form url.Values, expected int) ([]byte, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.requests = append(u.requests, fmt.Sprintf("%s %s %s %s %d", method, path, query.Encode(), form.Encode(), expected))
	if method == http.MethodPost {
		u.posts++
		return []byte(fmt.Sprintf(`{"id":%d}`, 40+u.posts)), nil
	}
	return []byte(`{}`), nil
}

func (u *recordingUpstream) seen() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.requests...)
}

// Whatever stands behind a scope, a request outside the issue's granted
// operations never reaches it, and a granted one reaches it as it was asked.
func TestAScopeForwardsToAnyUpstreamOnlyWhatItGrants(t *testing.T) {
	upstream := &recordingUpstream{}
	scope, err := NewIssueScope(upstream, "EXAMPLE-1", true, KeepLatestPost)
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewIssueScope(upstream, "EXAMPLE-2", true)
	if err != nil {
		t.Fatal(err)
	}
	readOnly, err := NewIssueScope(upstream, "EXAMPLE-1", false)
	if err != nil {
		t.Fatal(err)
	}
	ask := func(target *IssueScope, method, path, query, body, key string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path+"?apiKey="+url.QueryEscape(key)+query, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		target.ServeHTTP(w, r)
		return w
	}
	// The requests TestIssueScopeCannotExpandItsAuthority and
	// TestIssueScopeDoesNotLetAWorkerRemoveComments refuse.
	for _, refused := range []struct {
		name, method, path, query, body, key string
		target                               *IssueScope
	}{
		{"missing key", "GET", "/issues/EXAMPLE-1", "", "", "", scope},
		{"other scope key", "GET", "/issues/EXAMPLE-1", "", "", other.Key(), scope},
		{"other issue", "GET", "/issues/EXAMPLE-2", "", "", scope.Key(), scope},
		{"other issue post", "POST", "/issues/EXAMPLE-2/comments", "", "content=hello", scope.Key(), scope},
		{"prefix", "GET", "/issues/EXAMPLE-10", "", "", scope.Key(), scope},
		{"list", "GET", "/issues", "", "", scope.Key(), scope},
		{"delete", "DELETE", "/issues/EXAMPLE-1", "", "", scope.Key(), scope},
		{"comment delete", "DELETE", "/issues/EXAMPLE-1/comments/41", "", "", scope.Key(), scope},
		{"update", "PATCH", "/issues/EXAMPLE-1", "", "", scope.Key(), scope},
		{"traversal", "GET", "/issues/EXAMPLE-1/../EXAMPLE-2", "", "", scope.Key(), scope},
		{"encoded traversal", "GET", "/issues/EXAMPLE-1/%2e%2e/EXAMPLE-2", "", "", scope.Key(), scope},
		{"encoded slash", "GET", "/issues/EXAMPLE-1%2f..%2fEXAMPLE-2", "", "", scope.Key(), scope},
		{"double slash", "GET", "//issues/EXAMPLE-1", "", "", scope.Key(), scope},
		{"comment traversal", "GET", "/issues/EXAMPLE-1/comments/1/../../EXAMPLE-2", "", "", scope.Key(), scope},
		{"comment alias", "GET", "/issues/EXAMPLE-1/comments/01", "", "", scope.Key(), scope},
		{"query issue", "GET", "/issues/EXAMPLE-1", "&issueId=2", "", scope.Key(), scope},
		{"duplicate key", "GET", "/issues/EXAMPLE-1", "&apiKey=" + scope.Key(), "", scope.Key(), scope},
		{"malformed query", "GET", "/issues/EXAMPLE-1", "&bad=%zz", "", scope.Key(), scope},
		{"duplicate page", "GET", "/issues/EXAMPLE-1/comments", "&count=1&count=2", "", scope.Key(), scope},
		{"huge page", "GET", "/issues/EXAMPLE-1/comments", "&count=101", "", scope.Key(), scope},
		{"bad cursor", "GET", "/issues/EXAMPLE-1/comments", "&minId=-1", "", scope.Key(), scope},
		{"additional form", "POST", "/issues/EXAMPLE-1/comments", "", "content=hello&statusId=4", scope.Key(), scope},
		{"notification", "POST", "/issues/EXAMPLE-1/comments", "", "content=hello&notifiedUserId[]=12", scope.Key(), scope},
		{"duplicate content", "POST", "/issues/EXAMPLE-1/comments", "", "content=hello&content=bye", scope.Key(), scope},
		{"missing content", "POST", "/issues/EXAMPLE-1/comments", "", "statusId=4", scope.Key(), scope},
		{"read only", "POST", "/issues/EXAMPLE-1/comments", "", "content=hello", readOnly.Key(), readOnly},
	} {
		if w := ask(refused.target, refused.method, refused.path, refused.query, refused.body, refused.key); w.Code < 400 || w.Code >= 500 {
			t.Errorf("%s: answered %d %s", refused.name, w.Code, w.Body.String())
		}
	}
	if seen := upstream.seen(); len(seen) != 0 {
		t.Fatalf("refused requests reached the upstream: %q", seen)
	}
	for _, granted := range []struct {
		method, path, query, body string
		status                    int
	}{
		{"GET", "/issues/EXAMPLE-1", "", "", http.StatusOK},
		{"GET", "/issues/EXAMPLE-1/comments", "&order=asc&count=100&minId=0", "", http.StatusOK},
		{"GET", "/issues/EXAMPLE-1/comments/7", "", "", http.StatusOK},
		{"POST", "/issues/EXAMPLE-1/comments", "", "content=hello", http.StatusCreated},
		{"POST", "/issues/EXAMPLE-1/comments", "", "content=the+question", http.StatusCreated},
	} {
		if w := ask(scope, granted.method, granted.path, granted.query, granted.body, scope.Key()); w.Code != granted.status {
			t.Fatalf("%s %s: answered %d %s", granted.method, granted.path, w.Code, w.Body.String())
		}
	}
	// The scope's own removal of the launch's earlier post goes the same way.
	if removed := scope.RemoveEarlierPosts(context.Background()); removed != 1 {
		t.Fatalf("removed %d earlier posts", removed)
	}
	want := []string{
		"GET /issues/EXAMPLE-1   200",
		"GET /issues/EXAMPLE-1/comments count=100&minId=0&order=asc  200",
		"GET /issues/EXAMPLE-1/comments/7   200",
		"POST /issues/EXAMPLE-1/comments  content=hello 201",
		"POST /issues/EXAMPLE-1/comments  content=the+question 201",
		"DELETE /issues/EXAMPLE-1/comments/41   200",
	}
	if seen := upstream.seen(); strings.Join(seen, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the upstream was asked:\n%s", strings.Join(seen, "\n"))
	}
}
