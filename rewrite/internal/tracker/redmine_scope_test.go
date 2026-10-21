package tracker

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// redmineScopeFixture puts the issue scope for issue 5 in front of the fake
// Redmine, and returns the client a role uses through it.
func redmineScopeFixture(t *testing.T, post bool, options ...func(*IssueScope)) (*fakeRedmine, RedmineProject, Backlog, *IssueScope) {
	t.Helper()
	fake, redmine := redmineFixture(t)
	scope, err := NewIssueScope(redmine, "5", post, options...)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(scope)
	t.Cleanup(server.Close)
	t.Setenv("SCOPE_TEST_WORKER", scope.Key())
	return fake, redmine, Backlog{BaseURL: server.URL, KeyEnv: "SCOPE_TEST_WORKER", Client: server.Client()}, scope
}

func TestRedmineScopeReadsPostsAndReadsBackUnmodifiedProse(t *testing.T) {
	const prose = "\r\n日本語 😀 � @example #17\r\n{\"unknown\":null} $(literal) `text` & + %\n"
	fake, _, worker, _ := redmineScopeFixture(t, true)
	fake.issue["description"] = prose
	ctx := context.Background()
	text, err := worker.Request(ctx, "5")
	if err != nil || text != "Original issue: 5\nTitle: Live marked\n\n"+prose {
		t.Fatalf("read: %q %v", text, err)
	}
	posted, err := worker.AddComment(ctx, "5", prose)
	if err != nil {
		t.Fatal(err)
	}
	read, err := worker.Comment(ctx, "5", 901)
	var normalized struct {
		ID          int64
		Content     string
		Notes       string
		Created     string
		CreatedUser struct {
			ID   int64
			Name string
		}
		Issue struct{ ID int64 }
	}
	if err != nil || !bytes.Equal(posted, read) || json.Unmarshal(read, &normalized) != nil || normalized.ID != 901 || normalized.Content != prose ||
		normalized.Notes != prose || normalized.Created != "2026-10-20T05:00:00Z" || normalized.CreatedUser.ID != 5 || normalized.CreatedUser.Name != "Engine Check" ||
		normalized.Issue.ID != 5 {
		t.Fatalf("readback: %s (%v)", read, err)
	}
	var issue struct {
		ID          int64
		IssueKey    string
		Summary     string
		Description string
		Created     string
		CreatedUser struct {
			ID   int64
			Name string
		}
		Subject string
	}
	record, err := worker.Comment(ctx, "5", 275)
	if err != nil || !bytes.Contains(record, []byte(`"content":"(a) でお願いします。"`)) {
		t.Fatalf("the requester's note: %s %v", record, err)
	}
	raw, err := (Backlog{BaseURL: worker.BaseURL, KeyEnv: worker.KeyEnv, Client: worker.Client}).call(ctx, http.MethodGet, "/issues/5", nil, nil, http.StatusOK)
	if err != nil || json.Unmarshal(raw, &issue) != nil || issue.ID != 5 || issue.IssueKey != "5" || issue.Summary != "Live marked" || issue.Subject != "Live marked" ||
		issue.Description != prose || issue.Created != "2026-10-20T04:56:08Z" || issue.CreatedUser.ID != 6 || issue.CreatedUser.Name != "Requester Check" {
		t.Fatalf("the issue for a role: %s %v", raw, err)
	}
}

func TestRedmineScopePagesTheIssuesJournalsByID(t *testing.T) {
	_, _, worker, _ := redmineScopeFixture(t, false)
	ctx := context.Background()
	ids := func(after int64) []int64 {
		t.Helper()
		rows, err := worker.Comments(ctx, "5", after)
		if err != nil {
			t.Fatal(err)
		}
		var got []int64
		for _, raw := range rows {
			var comment struct {
				ID      int64
				Content *string
			}
			if json.Unmarshal(raw, &comment) != nil || comment.Content == nil {
				t.Fatalf("a journal without content for a role: %s", raw)
			}
			got = append(got, comment.ID)
		}
		return got
	}
	if got := ids(0); !slices.Equal(got, []int64{274, 275, 276, 277, 278, 279, 280, 281, 282, 283}) {
		t.Fatalf("every journal: %v", got)
	}
	if got := ids(280); !slices.Equal(got, []int64{281, 282, 283}) {
		t.Fatalf("after 280: %v", got)
	}
	page, err := worker.call(ctx, http.MethodGet, "/issues/5/comments", url.Values{"order": {"asc"}, "count": {"2"}, "minId": {"275"}}, nil, http.StatusOK)
	if err != nil || strings.Count(string(page), `"content"`) != 2 || !strings.Contains(string(page), `"id":276`) || strings.Contains(string(page), `"id":275,`) {
		t.Fatalf("a counted page: %s %v", page, err)
	}
}

func TestRedmineScopeRefusesWhatItDoesNotTranslate(t *testing.T) {
	fake, redmine := redmineFixture(t)
	ctx := context.Background()
	for _, request := range []struct {
		method, path string
		query, form  url.Values
		expected     int
	}{
		{http.MethodGet, "/projects/1", nil, nil, http.StatusOK},
		{http.MethodGet, "/issues", nil, nil, http.StatusOK},
		{http.MethodGet, "/issues/EXAMPLE-5", nil, nil, http.StatusOK},
		{http.MethodGet, "/issues/05", nil, nil, http.StatusOK},
		{http.MethodGet, "/issues/5", url.Values{"include": {"journals"}}, nil, http.StatusOK},
		{http.MethodGet, "/issues/5/comments", url.Values{"count": {"101"}}, nil, http.StatusOK},
		{http.MethodGet, "/issues/5/comments", url.Values{"order": {"desc"}}, nil, http.StatusOK},
		{http.MethodGet, "/issues/5/comments", url.Values{"minId": {"1", "2"}}, nil, http.StatusOK},
		{http.MethodGet, "/issues/5/comments/0", nil, nil, http.StatusOK},
		{http.MethodGet, "/issues/5/comments/0274", nil, nil, http.StatusOK},
		{http.MethodPost, "/issues/5/comments", nil, url.Values{"content": {"x"}, "notifiedUserId[]": {"6"}}, http.StatusCreated},
		{http.MethodPost, "/issues/5", nil, url.Values{"content": {"x"}}, http.StatusCreated},
		{http.MethodPatch, "/issues/5", nil, url.Values{"statusId": {"2"}}, http.StatusOK},
		{http.MethodPut, "/issues/5/comments/274", nil, url.Values{"content": {"x"}}, http.StatusOK},
		{http.MethodDelete, "/issues/5", nil, nil, http.StatusOK},
	} {
		if data, err := redmine.Forward(ctx, request.method, request.path, request.query, request.form, request.expected); err == nil {
			t.Errorf("%s %s %v %v was translated: %s", request.method, request.path, request.query, request.form, data)
		}
	}
	if calls := fake.called(); len(calls) != 0 {
		t.Fatalf("refused operations reached Redmine: %v", calls)
	}
	// A journal of another issue is not this issue's to read or to empty.
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		_, err := redmine.Forward(ctx, method, "/issues/5/comments/7001", nil, nil, http.StatusOK)
		if refusal, ok := err.(*trackerError); !ok || refusal.Status != http.StatusNotFound {
			t.Fatalf("%s of another issue's journal: %v", method, err)
		}
	}
	if slices.Contains(fake.called(), "PUT /journals/7001.json") {
		t.Fatal("another issue's journal was emptied")
	}
}

// A role with "comment" access leaves one comment per launch: the earlier
// ones are emptied, which Redmine 5.0 and later takes as removing them. One
// that cannot be emptied (an older Redmine, no permission) stays.
func TestRedmineScopeLeavesOnlyTheLaunchsLastPost(t *testing.T) {
	for _, refused := range []bool{false, true} {
		fake, redmine, worker, scope := redmineScopeFixture(t, true, KeepLatestPost)
		if refused {
			fake.refuse["PUT /journals/901.json"] = http.StatusNotFound
		}
		ctx := context.Background()
		for _, words := range []string{"trial line", "what the role means"} {
			if _, err := worker.AddComment(ctx, "5", words); err != nil {
				t.Fatal(err)
			}
		}
		removed := scope.RemoveEarlierPosts(ctx)
		rows, err := redmine.Comments(ctx, Issue{ID: 5, Key: "5"})
		if err != nil {
			t.Fatal(err)
		}
		var words []string
		for _, raw := range rows[10:] {
			_, text, _ := redmine.CommentText(raw)
			words = append(words, text)
		}
		want, wantRemoved := []string{"what the role means"}, 1
		if refused {
			want, wantRemoved = []string{"trial line", "what the role means"}, 0
		}
		if removed != wantRemoved || !slices.Equal(words, want) {
			t.Fatalf("refused=%v: removed %d, left %q", refused, removed, words)
		}
		emptied := 0
		for _, call := range fake.called() {
			if strings.HasPrefix(call, "PUT /journals/") {
				emptied++
				if call != "PUT /journals/901.json" {
					t.Fatalf("emptied %s", call)
				}
			}
		}
		if emptied != 1 {
			t.Fatalf("emptied %d journals", emptied)
		}
	}
}

// A private note never reaches a role, whatever the engine's account may see:
// the role's own comments are public, and it could quote one into them.
// (Fails at b997c63, which gave roles every note the account could see.)
func TestRedmineScopeKeepsPrivateNotesFromRoles(t *testing.T) {
	fake, redmine, worker, _ := redmineScopeFixture(t, true)
	fake.journals = append(fake.journals, map[string]any{"id": 290, "user": map[string]any{"id": 7}, "notes": "an operator's private note", "private_notes": true})
	ctx := context.Background()
	rows, err := worker.Comments(ctx, "5", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range rows {
		if bytes.Contains(raw, []byte("private note")) {
			t.Fatalf("a role read a private note: %s", raw)
		}
	}
	if len(rows) != 10 {
		t.Fatalf("the role read %d notes, want the 10 public ones", len(rows))
	}
	if raw, err := worker.Comment(ctx, "5", 290); err == nil {
		t.Fatalf("a role read a private note by its id: %s", raw)
	}
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		_, err := redmine.Forward(ctx, method, "/issues/5/comments/290", nil, nil, http.StatusOK)
		if refusal, ok := err.(*trackerError); !ok || refusal.Status != http.StatusNotFound {
			t.Fatalf("%s of a private note: %v", method, err)
		}
	}
	if slices.Contains(fake.called(), "PUT /journals/290.json") {
		t.Fatal("a private note was emptied through the scope")
	}
	// The engine's own reads still see it, for an answer or a stop.
	all, err := redmine.Comments(ctx, Issue{ID: 5, Key: "5"})
	if err != nil || len(all) != 11 {
		t.Fatalf("the engine read %d notes (%v)", len(all), err)
	}
}
