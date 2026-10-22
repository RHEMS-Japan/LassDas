package tracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// backlogRecord has the shape of the issue records the engine saves.
func backlogRecord(id int) map[string]any {
	return map[string]any{"id": id, "projectId": 17, "issueKey": fmt.Sprintf("EXAMPLE-%d", id),
		"summary": "Original title", "description": "Keep all conditions.\n{\"unknown\":null} `code` 日本語\n",
		"created": "2026-01-03T00:00:00Z", "createdUser": map[string]any{"id": 55},
		"category": []any{map[string]any{"id": 7}}, "futureField": map[string]any{"kept": true}}
}

func backlogComment(id, issue, project, user int64, body string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"id": id, "issueId": issue, "projectId": project,
		"createdUser": map[string]any{"id": user}, "content": body, "unknown": "keep this native metadata"})
	return raw
}

func TestABacklogProjectReadsAnIssueRecordAsTheEngineKeepsIt(t *testing.T) {
	project := BacklogProject{Client: Backlog{BaseURL: "https://tracker.example/api/v2", KeyEnv: "TRACKER_TEST_KEY"}, ProjectID: 17}
	// The queue's identity, as every queue so far was written with it.
	if got := project.Identity(); got != "Issue intake: https://tracker.example/api/v2\nProject: 17" {
		t.Fatalf("identity: %q", got)
	}
	if project.CredentialEnv() != "TRACKER_TEST_KEY" {
		t.Fatalf("credential: %q", project.CredentialEnv())
	}
	raw, _ := json.Marshal(backlogRecord(51))
	issue, err := project.ReadIssue(raw)
	if err != nil || issue.ID != 51 || issue.Key != "EXAMPLE-51" || issue.Creator.ID != 55 ||
		!issue.Created.Equal(time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)) || !bytes.Equal(issue.Raw, raw) {
		t.Fatalf("issue read as %+v (%v)", issue, err)
	}
	// Each request's history holds this text, so it may not change by a byte.
	const want = "Original issue: EXAMPLE-51\nTitle: Original title\n\nKeep all conditions.\n{\"unknown\":null} `code` 日本語\n"
	before, beforeErr := RequestText(raw)
	if text, err := project.RequestText(raw); err != nil || beforeErr != nil || text != want || before != want {
		t.Fatalf("request text %q (%v), before %q (%v)", text, err, before, beforeErr)
	}
	// A record that does not say when it was created is read; the intake
	// decides what to do with it.
	undated := backlogRecord(52)
	delete(undated, "created")
	raw, _ = json.Marshal(undated)
	if issue, err := project.ReadIssue(raw); err != nil || !issue.Created.IsZero() {
		t.Fatalf("undated record read as %+v (%v)", issue, err)
	}
	for name, change := range map[string]func(map[string]any){
		"another project":          func(r map[string]any) { r["projectId"] = 18 },
		"no project":               func(r map[string]any) { delete(r, "projectId") },
		"no id":                    func(r map[string]any) { delete(r, "id") },
		"no key":                   func(r map[string]any) { delete(r, "issueKey") },
		"unreadable creation time": func(r map[string]any) { r["created"] = "yesterday" },
		"unreadable requester":     func(r map[string]any) { r["createdUser"] = "someone" },
		"unreadable categories":    func(r map[string]any) { r["category"] = "seven" },
	} {
		record := backlogRecord(51)
		change(record)
		raw, _ := json.Marshal(record)
		if issue, err := project.ReadIssue(raw); err == nil {
			t.Errorf("%s: read as %+v", name, issue)
		}
	}
}

func TestABacklogProjectTakesUpOnlyIssuesCarryingAConfiguredCategory(t *testing.T) {
	project := BacklogProject{ProjectID: 17, Categories: []int64{77, 78}}
	carrying := func(categories any) Issue {
		record := backlogRecord(51)
		record["category"] = categories
		raw, _ := json.Marshal(record)
		issue, err := project.ReadIssue(raw)
		if err != nil {
			t.Fatal(err)
		}
		return issue
	}
	if !project.Marked(carrying([]any{map[string]any{"id": 5}, map[string]any{"id": 78}})) {
		t.Fatal("an issue carrying a configured category was left")
	}
	for _, categories := range []any{[]any{map[string]any{"id": 5}}, []any{}, nil} {
		if project.Marked(carrying(categories)) {
			t.Fatalf("an issue carrying %v was taken up", categories)
		}
	}
	if !(BacklogProject{ProjectID: 17}).Marked(carrying(nil)) {
		t.Fatal("without configured categories an issue was left")
	}
}

func TestABacklogProjectPlacesACommentByTheIssueAndProjectItNames(t *testing.T) {
	project := BacklogProject{ProjectID: 17}
	issue := Issue{ID: 51, Key: "EXAMPLE-51"}
	comment, err := project.ReadComment(backlogComment(701, 51, 17, 55, "停止\nReason in ordinary prose"), issue)
	if err != nil || comment != (Comment{ID: 701, Body: "停止\nReason in ordinary prose", Author: Account{ID: 55}, OnIssue: true}) {
		t.Fatalf("comment read as %+v (%v)", comment, err)
	}
	for name, raw := range map[string]json.RawMessage{
		"another issue":   backlogComment(701, 52, 17, 55, "停止"),
		"another project": backlogComment(701, 51, 99, 55, "停止"),
		"no place named":  json.RawMessage(`{"id":701,"content":"停止","createdUser":{"id":55}}`),
	} {
		if comment, err := project.ReadComment(raw, issue); err != nil || comment.OnIssue {
			t.Errorf("%s: read as %+v (%v)", name, comment, err)
		}
	}
	for _, raw := range []string{`{"id":"broken"}`, `{"id":0,"issueId":51,"projectId":17}`, `{"issueId":51,"projectId":17}`,
		`{"id":701,"issueId":51,"projectId":17,"content":42}`, `{"id":701,"issueId":51,"projectId":17,"createdUser":"someone"}`, `not JSON`} {
		if comment, err := project.ReadComment(json.RawMessage(raw), issue); err == nil {
			t.Errorf("%s: read as %+v", raw, comment)
		}
	}
}

// The engine finds a notice of its own by the comment's id and words alone,
// so nothing else a record holds, in whatever shape, keeps it from reading
// those two.
func TestABacklogProjectReadsACommentsIdAndWordsAlone(t *testing.T) {
	project := BacklogProject{ProjectID: 17}
	for raw, want := range map[string]string{
		`{"id":701,"issueId":"51","projectId":[17],"createdUser":"someone","content":"words"}`: "words",
		`{"id":702}`: "",
	} {
		id, words, err := project.CommentText(json.RawMessage(raw))
		if err != nil || id <= 0 || words != want {
			t.Errorf("%s: read as %d %q (%v)", raw, id, words, err)
		}
	}
	for _, raw := range []string{`{"id":"broken"}`, `{"id":0,"content":"words"}`, `{"content":"words"}`, `{"id":701,"content":42}`, `not JSON`} {
		if id, words, err := project.CommentText(json.RawMessage(raw)); err == nil {
			t.Errorf("%s: read as %d %q", raw, id, words)
		}
	}
}

// Each operation sends what the engine sent when it called the client itself.
func TestABacklogProjectAsksTheTrackerWhatTheEngineAskedBefore(t *testing.T) {
	t.Setenv("TRACKER_TEST_KEY", "synthetic-token")
	var seen []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("apiKey") != "synthetic-token" {
			t.Error("missing credential")
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		query := r.URL.Query()
		query.Del("apiKey")
		seen = append(seen, strings.TrimSpace(r.Method+" "+r.URL.Path+" "+query.Encode()+" "+r.PostForm.Encode()))
		switch {
		case r.URL.Path == "/api/v2/users/myself":
			json.NewEncoder(w).Encode(map[string]any{"id": 900})
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"id": 702, "content": r.PostForm.Get("content")})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			json.NewEncoder(w).Encode([]json.RawMessage{backlogComment(701, 51, 17, 55, "停止")})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/issues":
			json.NewEncoder(w).Encode([]any{backlogRecord(51)})
		case r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(backlogRecord(51))
		default:
			// Confirm whatever the change asked for.
			reply := map[string]any{"id": 51}
			if id, err := strconv.Atoi(r.PostForm.Get("statusId")); err == nil {
				reply["status"] = map[string]any{"id": id}
			}
			if id, err := strconv.Atoi(r.PostForm.Get("assigneeId")); err == nil {
				reply["assignee"] = map[string]any{"id": id}
			}
			if hours := r.PostForm.Get("actualHours"); hours != "" {
				reply["actualHours"] = json.Number(hours)
			}
			categories := []any{}
			for _, value := range r.PostForm["categoryId[]"] {
				id, _ := strconv.Atoi(value)
				categories = append(categories, map[string]any{"id": id})
			}
			reply["category"] = categories
			json.NewEncoder(w).Encode(reply)
		}
	}))
	defer server.Close()
	project := BacklogProject{Client: Backlog{BaseURL: server.URL + "/api/v2", KeyEnv: "TRACKER_TEST_KEY", Client: server.Client()},
		ProjectID: 17, OnAccept: 2001, Statuses: map[string]int64{Processing: 1001, Delivered: 3}}
	if string(project.Target(Accepted)) != "2001" || string(project.Target(Processing)) != "1001" || string(project.Target(Delivered)) != "3" ||
		project.Target(AwaitingRequester) != nil || project.Target(Stopped) != nil || (BacklogProject{}).Target(Accepted) != nil {
		t.Fatal("a turn's target is not the configured id, or an unconfigured turn has one")
	}
	ctx := context.Background()
	rows, err := project.Issues(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("issues: %s (%v)", rows, err)
	}
	issue, err := project.ReadIssue(rows[0])
	if err != nil {
		t.Fatal(err)
	}
	if text, err := project.Request(ctx, "EXAMPLE-51"); err != nil || !strings.HasPrefix(text, "Original issue: EXAMPLE-51\nTitle: Original title\n\n") {
		t.Fatalf("request: %q (%v)", text, err)
	}
	if comments, err := project.Comments(ctx, issue); err != nil || len(comments) != 1 || !bytes.Equal(comments[0], backlogComment(701, 51, 17, 55, "停止")) {
		t.Fatalf("comments: %s (%v)", comments, err)
	}
	if id, err := project.AddComment(ctx, issue, "受け付けました。"); err != nil || id != 702 {
		t.Fatalf("comment: %d (%v)", id, err)
	}
	me, err := project.Myself(ctx)
	if err != nil || me.ID != 900 {
		t.Fatalf("myself: %+v (%v)", me, err)
	}
	// The accepted issue keeps its category 7 and gains 2001; one built
	// without a record has none to keep; a category without an id and the one
	// it already carries are not sent again.
	record := backlogRecord(51)
	record["category"] = []any{map[string]any{"id": 0}, map[string]any{"id": 7}, map[string]any{"id": 2001}}
	raw, _ := json.Marshal(record)
	carrying, err := project.ReadIssue(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []error{
		project.Move(ctx, issue, Accepted),
		project.Move(ctx, Issue{ID: 51, Key: "EXAMPLE-51"}, Accepted),
		project.Move(ctx, carrying, Accepted),
		project.Move(ctx, issue, Processing),
		project.Assign(ctx, issue, me),
		project.Move(ctx, issue, Delivered),
		project.Assign(ctx, issue, issue.Creator),
		project.RecordHours(ctx, issue, 0.25),
	} {
		if step != nil {
			t.Fatal(step)
		}
	}
	if err := project.Move(ctx, issue, AwaitingRequester); err == nil {
		t.Fatal("a turn without a status moved the issue")
	}
	if data, err := project.Forward(ctx, http.MethodGet, "/issues/EXAMPLE-51/comments", url.Values{"order": {"asc"}}, nil, http.StatusOK); err != nil || !strings.Contains(string(data), "停止") {
		t.Fatalf("forward: %s (%v)", data, err)
	}
	want := []string{
		"GET /api/v2/issues count=100&offset=0&order=asc&projectId%5B%5D=17&sort=created",
		"GET /api/v2/issues/EXAMPLE-51",
		"GET /api/v2/issues/EXAMPLE-51/comments count=100&minId=0&order=asc",
		"POST /api/v2/issues/EXAMPLE-51/comments  " + url.Values{"content": {"受け付けました。"}}.Encode(),
		"GET /api/v2/users/myself",
		"PATCH /api/v2/issues/EXAMPLE-51  categoryId%5B%5D=7&categoryId%5B%5D=2001",
		"PATCH /api/v2/issues/EXAMPLE-51  categoryId%5B%5D=2001",
		"PATCH /api/v2/issues/EXAMPLE-51  categoryId%5B%5D=7&categoryId%5B%5D=2001",
		"PATCH /api/v2/issues/EXAMPLE-51  statusId=1001",
		"PATCH /api/v2/issues/EXAMPLE-51  assigneeId=900",
		"PATCH /api/v2/issues/EXAMPLE-51  statusId=3",
		"PATCH /api/v2/issues/EXAMPLE-51  assigneeId=55",
		"PATCH /api/v2/issues/EXAMPLE-51  actualHours=0.25",
		"GET /api/v2/issues/EXAMPLE-51/comments order=asc",
	}
	if strings.Join(seen, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests:\n%s", strings.Join(seen, "\n"))
	}
}

// Synthetic fixtures use the public API response shape, never live records:
// https://developer.nulab.com/docs/backlog/api/2/get-comment-list/
// https://developer.nulab.com/docs/backlog/api/2/get-comment/
func TestBacklogCommentMetadata(t *testing.T) {
	for _, test := range []struct {
		name, metadata    string
		title, attachment bool
		created, updated  bool
	}{
		{"summary", `"created":"2026-01-03T00:00:00Z","updated":"2026-01-03T01:00:00Z","changeLog":[{"field":"summary","newValue":"New title","originalValue":"Old title","attachmentInfo":null,"attributeInfo":null,"notificationInfo":null}]`, true, false, true, true},
		{"description", `"created":"2026-01-03T00:00:00Z","updated":"2026-01-03T01:00:00Z","changeLog":[{"field":"description","newValue":"New text","originalValue":"Old text","attachmentInfo":null,"attributeInfo":null,"notificationInfo":null}]`, true, false, true, true},
		{"attachment", `"created":"2026-01-03T00:00:00Z","updated":"2026-01-03T01:00:00Z","changeLog":[{"field":"attachment","newValue":null,"originalValue":null,"attachmentInfo":{"id":81,"name":"example.txt"},"attributeInfo":null,"notificationInfo":null}]`, false, true, true, true},
		{"assigner", `"created":"2026-01-03T00:00:00Z","updated":"2026-01-03T01:00:00Z","changeLog":[{"field":"assigner","newValue":"56","originalValue":"55","attachmentInfo":null,"attributeInfo":null,"notificationInfo":null}]`, false, false, true, true},
		{"missing", `"stars":[]`, false, false, false, false},
		{"null", `"created":null,"updated":null,"changeLog":null`, false, false, false, false},
		{"unreadable", `"created":42,"updated":"not a time","changeLog":{}`, false, false, false, false},
		{"independent fields", `"created":"not a time","updated":"2026-01-03T01:00:00Z","changeLog":[{"field":42},{"field":"summary"},{"field":"attachment"}]`, true, true, false, true},
		{"independent updated", `"created":"2026-01-03T00:00:00Z","updated":[],"changeLog":[{"field":"assigner"}]`, false, false, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := json.RawMessage(`{"id":701,"projectId":17,"issueId":51,"content":"Example comment","createdUser":{"id":55,"userId":"example","name":"Example","roleType":2,"lang":"en","mailAddress":"example@example.invalid"},"notifications":[],` + test.metadata + `}`)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("method: %s", r.Method)
				}
				switch r.URL.Path {
				case "/api/v2/issues/EXAMPLE-51/comments":
					fmt.Fprintf(w, "[%s]", raw)
				case "/api/v2/issues/EXAMPLE-51/comments/701":
					w.Write(raw)
				default:
					t.Errorf("unexpected path: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			t.Setenv("COMMENT_METADATA_TEST_KEY", "fixture")
			project := BacklogProject{ProjectID: 17, Client: Backlog{BaseURL: server.URL + "/api/v2", KeyEnv: "COMMENT_METADATA_TEST_KEY", Client: server.Client()}}
			issue := Issue{ID: 51, Key: "EXAMPLE-51"}
			rows, err := project.Comments(context.Background(), issue)
			if err != nil || len(rows) != 1 {
				t.Fatalf("list: %d rows (%v)", len(rows), err)
			}
			single, err := project.Client.Comment(context.Background(), issue.Key, 701)
			if err != nil {
				t.Fatal(err)
			}
			for _, record := range append(rows, single) {
				got, err := project.ReadComment(record, issue)
				want := Comment{ID: 701, Body: "Example comment", Author: Account{ID: 55}, OnIssue: true,
					ChangedTitleOrDescription: test.title, HasAttachment: test.attachment}
				if test.created {
					want.Created = time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)
				}
				if test.updated {
					want.Updated = time.Date(2026, 1, 3, 1, 0, 0, 0, time.UTC)
				}
				if err != nil || got != want {
					t.Fatalf("got %+v (%v), want %+v", got, err, want)
				}
			}
		})
	}
}

func TestTrackerTextFormattingRule(t *testing.T) {
	for _, rule := range []string{"markdown", "backlog"} {
		t.Run(rule, func(t *testing.T) {
			// https://developer.nulab.com/docs/backlog/api/2/get-project/
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/api/v2/projects/17" {
					t.Errorf("request: %s %s", r.Method, r.URL.Path)
				}
				fmt.Fprintf(w, `{"id":17,"projectKey":"EXAMPLE","name":"Example","textFormattingRule":%q,"archived":false}`, rule)
			}))
			defer server.Close()
			t.Setenv("FORMATTING_TEST_KEY", "fixture")
			project := BacklogProject{ProjectID: 17, Client: Backlog{BaseURL: server.URL + "/api/v2", KeyEnv: "FORMATTING_TEST_KEY", Client: server.Client()}}
			if got, err := project.TextFormattingRule(context.Background()); err != nil || got != rule {
				t.Fatalf("format: %q (%v)", got, err)
			}
		})
	}
	for _, source := range []Tracker{GitHub{}, RedmineProject{}} {
		if got, err := source.TextFormattingRule(context.Background()); err != nil || got != "" {
			t.Fatalf("%T format: %q (%v)", source, got, err)
		}
	}
}
