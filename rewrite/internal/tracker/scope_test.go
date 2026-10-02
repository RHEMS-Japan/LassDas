package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type unreadableScopeBody struct{}

func (unreadableScopeBody) Read([]byte) (int, error) {
	return 0, errors.New("synthetic upload disconnected")
}

func TestIssueScopePreservesUploadFailureReason(t *testing.T) {
	var calls atomic.Int32
	_, scope := scopedFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }), true)
	r := httptest.NewRequest("POST", "/issues/EXAMPLE-1/comments?apiKey="+scope.Key(), unreadableScopeBody{})
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	scope.ServeHTTP(w, r)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "synthetic upload disconnected") || calls.Load() != 0 {
		t.Fatalf("upload reason replaced: %d %s calls=%d", w.Code, w.Body.String(), calls.Load())
	}
}

// Only synthetic credentials and local TLS services are used in these tests.
func scopedFixture(t *testing.T, handler http.Handler, post bool, options ...func(*IssueScope)) (Backlog, *IssueScope) {
	t.Helper()
	t.Setenv("SCOPE_TEST_ACCOUNT", "synthetic-account+/=key")
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("apiKey") != "synthetic-account+/=key" {
			t.Error("upstream must receive the controller credential, not worker access")
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(upstream.Close)
	scope, err := NewIssueScope(Backlog{BaseURL: upstream.URL + "/api/v2", KeyEnv: "SCOPE_TEST_ACCOUNT", Client: upstream.Client()}, "EXAMPLE-1", post, options...)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(scope)
	t.Cleanup(server.Close)
	t.Setenv("SCOPE_TEST_WORKER", scope.Key())
	if scope.Key() == "synthetic-account+/=key" || len(scope.Key()) < 40 {
		t.Fatal("access is not independent")
	}
	return Backlog{BaseURL: server.URL, KeyEnv: "SCOPE_TEST_WORKER", Client: server.Client()}, scope
}

func TestIssueScopeReadsPostsAndReadsBackUnmodifiedProse(t *testing.T) {
	const prose = "\nordinary 日本語 report\n{\"unknown\":true} $(literal) `text` & + %\n"
	var posts atomic.Int32
	b, _ := scopedFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v2/issues/EXAMPLE-1":
			json.NewEncoder(w).Encode(map[string]any{"issueKey": "EXAMPLE-1", "summary": "Task", "description": prose, "future": true})
		case "POST /api/v2/issues/EXAMPLE-1/comments":
			posts.Add(1)
			if err := r.ParseForm(); err != nil || len(r.PostForm) != 1 || r.PostForm.Get("content") != prose {
				t.Errorf("prose changed: %v %v", r.PostForm, err)
			}
			w.WriteHeader(201)
			fallthrough
		case "GET /api/v2/issues/EXAMPLE-1/comments/7":
			json.NewEncoder(w).Encode(map[string]any{"id": 7, "content": prose, "future": true})
		default:
			t.Errorf("unexpected operation: %s %s", r.Method, r.URL.Path)
		}
	}), true)
	ctx := context.Background()
	text, err := b.Request(ctx, "EXAMPLE-1")
	if err != nil || !strings.HasSuffix(text, prose) {
		t.Fatalf("read: %q %v", text, err)
	}
	posted, err := b.AddComment(ctx, "EXAMPLE-1", prose)
	if err != nil {
		t.Fatal(err)
	}
	read, err := b.Comment(ctx, "EXAMPLE-1", 7)
	if err != nil || string(read) != string(posted) || posts.Load() != 1 || !strings.Contains(string(read), `"future":true`) {
		t.Fatalf("readback: %s %v posts=%d", read, err, posts.Load())
	}
}

func TestIssueScopeCannotExpandItsAuthority(t *testing.T) {
	var calls atomic.Int32
	b, scope := scopedFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); fmt.Fprint(w, `{}`) }), true)
	other, err := NewIssueScope(scope.source, "EXAMPLE-2", true)
	if err != nil {
		t.Fatal(err)
	}
	readOnly, err := NewIssueScope(scope.source, "EXAMPLE-1", false)
	if err != nil {
		t.Fatal(err)
	}
	readServer := httptest.NewTLSServer(readOnly)
	defer readServer.Close()
	tests := []struct {
		name, method, path, query, body, key string
		readOnly                             bool
	}{
		{name: "missing key", method: "GET", path: "/issues/EXAMPLE-1"},
		{name: "other scope key", method: "GET", path: "/issues/EXAMPLE-1", key: other.Key()},
		{name: "other issue", method: "GET", path: "/issues/EXAMPLE-2", key: scope.Key()},
		{name: "other issue post", method: "POST", path: "/issues/EXAMPLE-2/comments", body: "content=hello", key: scope.Key()},
		{name: "prefix", method: "GET", path: "/issues/EXAMPLE-10", key: scope.Key()},
		{name: "list", method: "GET", path: "/issues", key: scope.Key()},
		{name: "delete", method: "DELETE", path: "/issues/EXAMPLE-1", key: scope.Key()},
		{name: "update", method: "PATCH", path: "/issues/EXAMPLE-1", key: scope.Key()},
		{name: "traversal", method: "GET", path: "/issues/EXAMPLE-1/../EXAMPLE-2", key: scope.Key()},
		{name: "encoded traversal", method: "GET", path: "/issues/EXAMPLE-1/%2e%2e/EXAMPLE-2", key: scope.Key()},
		{name: "encoded slash", method: "GET", path: "/issues/EXAMPLE-1%2f..%2fEXAMPLE-2", key: scope.Key()},
		{name: "double slash", method: "GET", path: "//issues/EXAMPLE-1", key: scope.Key()},
		{name: "comment traversal", method: "GET", path: "/issues/EXAMPLE-1/comments/1/../../EXAMPLE-2", key: scope.Key()},
		{name: "comment alias", method: "GET", path: "/issues/EXAMPLE-1/comments/01", key: scope.Key()},
		{name: "query issue", method: "GET", path: "/issues/EXAMPLE-1", query: "issueId=2", key: scope.Key()},
		{name: "duplicate key", method: "GET", path: "/issues/EXAMPLE-1", query: "apiKey=" + scope.Key(), key: scope.Key()},
		{name: "malformed query", method: "GET", path: "/issues/EXAMPLE-1", query: "bad=%zz", key: scope.Key()},
		{name: "duplicate page", method: "GET", path: "/issues/EXAMPLE-1/comments", query: "count=1&count=2", key: scope.Key()},
		{name: "huge page", method: "GET", path: "/issues/EXAMPLE-1/comments", query: "count=101", key: scope.Key()},
		{name: "bad cursor", method: "GET", path: "/issues/EXAMPLE-1/comments", query: "minId=-1", key: scope.Key()},
		{name: "additional form", method: "POST", path: "/issues/EXAMPLE-1/comments", body: "content=hello&statusId=4", key: scope.Key()},
		{name: "notification", method: "POST", path: "/issues/EXAMPLE-1/comments", body: "content=hello&notifiedUserId[]=12", key: scope.Key()},
		{name: "duplicate content", method: "POST", path: "/issues/EXAMPLE-1/comments", body: "content=hello&content=bye", key: scope.Key()},
		{name: "missing content", method: "POST", path: "/issues/EXAMPLE-1/comments", body: "statusId=4", key: scope.Key()},
		{name: "read only", method: "POST", path: "/issues/EXAMPLE-1/comments", body: "content=hello", key: readOnly.Key(), readOnly: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			address, client := b.BaseURL, b.Client
			if tt.readOnly {
				address, client = readServer.URL, readServer.Client()
			}
			query := "apiKey=" + url.QueryEscape(tt.key)
			if tt.query != "" {
				query += "&" + tt.query
			}
			req, err := http.NewRequest(tt.method, address+tt.path+"?"+query, strings.NewReader(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			response, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, _ := io.ReadAll(response.Body)
			if response.StatusCode < 400 || response.StatusCode >= 500 || calls.Load() != 0 {
				t.Fatalf("scope bypass status=%d calls=%d body=%s", response.StatusCode, calls.Load(), body)
			}
			if strings.Contains(string(body), scope.Key()) {
				t.Fatal("access key reflected")
			}
		})
	}
}

func TestIssueScopePreservesPagination(t *testing.T) {
	var calls atomic.Int32
	b, _ := scopedFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		q := r.URL.Query()
		if r.Method != "GET" || r.URL.Path != "/api/v2/issues/EXAMPLE-1/comments" || q.Get("count") != "100" || q.Get("order") != "asc" {
			t.Error("pagination changed")
		}
		start, _ := strconv.Atoi(q.Get("minId"))
		if start == 0 {
			start = 1
		}
		page := []map[string]any{}
		for id := start; id <= 205 && len(page) < 100; id++ {
			page = append(page, map[string]any{"id": id, "content": fmt.Sprint(id), "future": true})
		}
		json.NewEncoder(w).Encode(page)
	}), false)
	comments, err := b.Comments(context.Background(), "EXAMPLE-1", 0)
	if err != nil || len(comments) != 205 || calls.Load() != 3 {
		t.Fatalf("partial comments: %d %v calls=%d", len(comments), err, calls.Load())
	}
	for i, entry := range comments {
		var row struct {
			ID     int
			Future bool
		}
		if err := json.Unmarshal(entry, &row); err != nil || row.ID != i+1 || !row.Future {
			t.Fatalf("changed record: %s %v", entry, err)
		}
	}
}

func TestIssueScopeAmbiguousPostDoesNotResubmit(t *testing.T) {
	for _, failure := range []string{"disconnect", "503", "redirect"} {
		t.Run(failure, func(t *testing.T) {
			var calls, posts atomic.Int32
			b, _ := scopedFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method == "GET" {
					fmt.Fprint(w, `{"id":7,"content":"stored"}`)
					return
				}
				posts.Add(1)
				switch failure {
				case "disconnect":
					conn, _, _ := w.(http.Hijacker).Hijack()
					conn.Close()
				case "503":
					w.WriteHeader(503)
					fmt.Fprint(w, "stored but receipt lost synthetic-account+/=key synthetic-account%2B%2F%3Dkey")
				case "redirect":
					w.Header().Set("Location", "/elsewhere")
					w.WriteHeader(307)
				}
			}), true)
			_, err := b.AddComment(context.Background(), "EXAMPLE-1", "stored")
			if err == nil || !strings.Contains(err.Error(), "inspect comments") || strings.Contains(err.Error(), "synthetic-account") || posts.Load() != 1 || calls.Load() != 1 {
				t.Fatalf("uncertain post: %v calls=%d posts=%d", err, calls.Load(), posts.Load())
			}
			if failure == "503" && !strings.Contains(err.Error(), "503: stored but receipt lost") {
				t.Fatalf("reason hidden: %v", err)
			}
			read, err := b.Comment(context.Background(), "EXAMPLE-1", 7)
			if err != nil || !strings.Contains(string(read), "stored") || calls.Load() != 2 || posts.Load() != 1 {
				t.Fatalf("cannot inspect: %s %v", read, err)
			}
		})
	}
}

func TestIssueScopeRejectsBadAssignmentAndOversizeTransport(t *testing.T) {
	for _, issue := range []string{"", ".", "..", "EXAMPLE-1/comments", "EXAMPLE-1?x", "EXAMPLE-1#x", "EXAMPLE-1\n", "EXAMPLE-1\\x"} {
		if _, err := NewIssueScope(Backlog{}, issue, true); err == nil {
			t.Fatalf("bad assigned issue: %q", issue)
		}
	}
	var calls atomic.Int32
	b, _ := scopedFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }), true)
	_, err := b.AddComment(context.Background(), "EXAMPLE-1", strings.Repeat("x", 4<<20))
	if err == nil || !strings.Contains(err.Error(), "413") || calls.Load() != 0 {
		t.Fatalf("unbounded post: %v calls=%d", err, calls.Load())
	}
	r := httptest.NewRecorder()
	(&IssueScope{}).ServeHTTP(r, httptest.NewRequest("GET", "/issues/EXAMPLE-1?apiKey=", nil))
	if r.Code != 401 || !strings.Contains(r.Body.String(), "issue-scoped access is unavailable") {
		t.Fatalf("zero handler: %d %s", r.Code, r.Body.String())
	}
}

func TestIssueAccessTLSAndCancellation(t *testing.T) {
	t.Setenv("SCOPE_TEST_ACCOUNT", "synthetic-account-only")
	entered, cancelled := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		close(entered)
		select {
		case <-r.Context().Done():
			close(cancelled)
		case <-time.After(5 * time.Second):
			t.Error("upstream handler did not see cancellation")
		}
	}))
	defer upstream.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	source := Backlog{BaseURL: upstream.URL, KeyEnv: "SCOPE_TEST_ACCOUNT", Client: upstream.Client()}
	access, err := ServeIssue(ctx, source, "EXAMPLE-1", true)
	if err != nil {
		t.Fatal(err)
	}
	defer access.Close()
	other, err := ServeIssue(ctx, source, "EXAMPLE-1", true)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if access.Key == other.Key || access.Certificate == other.Certificate {
		t.Fatal("launches shared access")
	}
	wrong, err := CertificateClient(other.Certificate)
	if err != nil {
		t.Fatal(err)
	}
	defer wrong.CloseIdleConnections()
	if response, err := wrong.Get(access.URL + "/issues/EXAMPLE-1?apiKey=" + access.Key); err == nil {
		response.Body.Close()
		t.Fatal("different certificate trusted")
	}
	for _, invalid := range []string{"", "not a certificate"} {
		if _, err := CertificateClient(invalid); err == nil {
			t.Fatal("missing trust silently ignored")
		}
	}
	client, err := CertificateClient(access.Certificate)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	if client.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("scoped key may be routed through ambient proxy")
	}
	t.Setenv("SCOPE_TEST_WORKER", access.Key)
	b := Backlog{BaseURL: access.URL, KeyEnv: "SCOPE_TEST_WORKER", Client: client}
	finished := make(chan error, 1)
	go func() { _, err := b.AddComment(context.Background(), "EXAMPLE-1", "ordinary prose"); finished <- err }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream was not reached")
	}
	cancel()
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("cancelled write reported success")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker request did not stop")
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request remained active")
	}
	access.Close() // safe after context cancellation and repeated by defer
	address, _ := url.Parse(access.URL)
	conn, err := net.DialTimeout("tcp", address.Host, 100*time.Millisecond)
	if err == nil {
		conn.Close()
		t.Fatal("cancelled access still listening")
	}
}

// A launch that posts a trial line before what it means to say leaves only
// the latter: once the launch is over, the scope removes what it stored
// before its last comment. Which is which is never read; the one the tracker
// stored last stays. No removal runs while a post is being answered.
func TestIssueScopeKeepsOnlyTheLatestPostOfALaunch(t *testing.T) {
	for _, test := range []struct {
		name            string
		options         []func(*IssueScope)
		ids             []int
		removalStatus   int
		secondPostFails bool
		removals        []string
		removed         int
	}{
		{name: "the earlier post is removed when the launch is over", options: []func(*IssueScope){KeepLatestPost}, ids: []int{41, 42},
			removalStatus: 200, removals: []string{"DELETE /api/v2/issues/EXAMPLE-1/comments/41"}, removed: 1},
		{name: "the comment stored last stays whatever the order of the answers", options: []func(*IssueScope){KeepLatestPost}, ids: []int{42, 41},
			removalStatus: 200, removals: []string{"DELETE /api/v2/issues/EXAMPLE-1/comments/41"}, removed: 1},
		{name: "every post stays without the option", ids: []int{41, 42}, removalStatus: 200},
		{name: "a refused removal leaves both", options: []func(*IssueScope){KeepLatestPost}, ids: []int{41, 42},
			removalStatus: 403, removals: []string{"DELETE /api/v2/issues/EXAMPLE-1/comments/41"}},
		{name: "a post that fails leaves the earlier comment", options: []func(*IssueScope){KeepLatestPost}, ids: []int{41, 42},
			removalStatus: 200, secondPostFails: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var mu sync.Mutex
			var operations []string
			posts := 0
			b, scope := scopedFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch {
				case r.Method == "POST" && r.URL.Path == "/api/v2/issues/EXAMPLE-1/comments":
					operations = append(operations, "POST")
					posts++
					if test.secondPostFails && posts == 2 {
						w.WriteHeader(500)
						return
					}
					w.WriteHeader(201)
					json.NewEncoder(w).Encode(map[string]any{"id": test.ids[posts-1], "content": r.PostFormValue("content")})
				case r.Method == "DELETE":
					operations = append(operations, "DELETE "+r.URL.Path)
					w.WriteHeader(test.removalStatus)
					json.NewEncoder(w).Encode(map[string]any{})
				default:
					t.Errorf("unexpected operation: %s %s", r.Method, r.URL.Path)
				}
			}), true, test.options...)
			ctx := context.Background()
			if _, err := b.AddComment(ctx, "EXAMPLE-1", "test comment"); err != nil {
				t.Fatal(err)
			}
			_, err := b.AddComment(ctx, "EXAMPLE-1", "the question itself")
			if (err != nil) != test.secondPostFails {
				t.Fatalf("second post: %v", err)
			}
			mu.Lock()
			during := strings.Join(operations, ", ")
			mu.Unlock()
			if during != "POST, POST" {
				t.Fatalf("something other than the posts ran while the launch was posting: %s", during)
			}
			if removed := scope.RemoveEarlierPosts(ctx); removed != test.removed {
				t.Fatalf("removed %d comments, want %d", removed, test.removed)
			}
			// Settling twice removes nothing more.
			if again := scope.RemoveEarlierPosts(ctx); again != 0 && test.removalStatus == 200 {
				t.Fatalf("a second settlement removed %d more", again)
			}
			mu.Lock()
			defer mu.Unlock()
			after := operations[2:]
			if test.removalStatus != 200 && len(after) > len(test.removals) {
				after = after[:len(test.removals)] // a refused removal may be tried again by the second settlement
			}
			if strings.Join(after, ", ") != strings.Join(test.removals, ", ") {
				t.Fatalf("removals %v, want %v", after, test.removals)
			}
		})
	}
}

// Closing a launch's access is what settles its comments, after the endpoint
// has stopped accepting posts.
func TestIssueAccessSettlesTheLaunchCommentsWhenItCloses(t *testing.T) {
	t.Setenv("SCOPE_TEST_ACCOUNT", "synthetic-account+/=key")
	var mu sync.Mutex
	var operations []string
	next := 40
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case "POST":
			next++
			operations = append(operations, "POST")
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]any{"id": next})
		case "DELETE":
			operations = append(operations, "DELETE "+r.URL.Path)
			json.NewEncoder(w).Encode(map[string]any{})
		}
	}))
	defer upstream.Close()
	source := Backlog{BaseURL: upstream.URL + "/api/v2", KeyEnv: "SCOPE_TEST_ACCOUNT", Client: upstream.Client()}
	access, err := ServeIssue(context.Background(), source, "EXAMPLE-1", true, KeepLatestPost)
	if err != nil {
		t.Fatal(err)
	}
	client, err := CertificateClient(access.Certificate)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	t.Setenv("SCOPE_TEST_WORKER", access.Key)
	b := Backlog{BaseURL: access.URL, KeyEnv: "SCOPE_TEST_WORKER", Client: client}
	for _, text := range []string{"test comment", "the question itself"} {
		if _, err := b.AddComment(context.Background(), "EXAMPLE-1", text); err != nil {
			t.Fatal(err)
		}
	}
	access.Close()
	access.Close() // repeated by a deferred release; settles once
	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(operations, ", "); got != "POST, POST, DELETE /api/v2/issues/EXAMPLE-1/comments/41" {
		t.Fatalf("upstream saw %s", got)
	}
}

// The removal is the scope's own, made with the controller's account for a
// comment the scope itself stored. A worker cannot ask for one.
func TestIssueScopeDoesNotLetAWorkerRemoveComments(t *testing.T) {
	b, scope := scopedFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a removal reached the tracker: %s %s", r.Method, r.URL.Path)
	}), true, KeepLatestPost)
	request, err := http.NewRequest("DELETE", b.BaseURL+"/issues/EXAMPLE-1/comments/41?apiKey="+scope.Key(), nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := b.Client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("a worker's removal was answered %d", response.StatusCode)
	}
}
