package tracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The live check runs only when it is pointed at a Redmine set up for it:
// REDMINE_LIVE_URL is its address (a local container answers over plain
// HTTP, so the adapter's HTTPS address is carried there by the test's own
// transport), and REDMINE_LIVE_KEYS a JSON file with the accounts' ids and
// API keys, the project and its categories: the engine's account (a member
// whose role may view issues, add and edit notes, edit its own notes and
// edit issues, with a workflow from New to In Progress, Feedback, Resolved
// and Closed), a requester's and an administrator's. Nothing else runs it.
type redmineLive struct {
	t      *testing.T
	target *url.URL
	keys   map[string]json.RawMessage
}

const redmineLiveAddress = "https://redmine-live.test"

func redmineLiveSetup(t *testing.T) (*redmineLive, RedmineProject) {
	t.Helper()
	address, keys := os.Getenv("REDMINE_LIVE_URL"), os.Getenv("REDMINE_LIVE_KEYS")
	if address == "" || keys == "" {
		t.Skip("set REDMINE_LIVE_URL and REDMINE_LIVE_KEYS to check against a real Redmine")
	}
	target, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(keys)
	if err != nil {
		t.Fatal(err)
	}
	live := &redmineLive{t: t, target: target}
	if err := json.Unmarshal(data, &live.keys); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REDMINE_LIVE_ENGINE_KEY", live.key("engine-bot"))
	project := RedmineProject{Client: Redmine{BaseURL: redmineLiveAddress, KeyEnv: "REDMINE_LIVE_ENGINE_KEY",
		Client: &http.Client{Transport: live}}, ProjectID: live.number("project", "id")}
	return live, project
}

// RoundTrip carries the adapter's HTTPS requests to the live Redmine.
func (l *redmineLive) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "https" || r.URL.Host != "redmine-live.test" {
		return nil, fmt.Errorf("a request left the live Redmine: %s", r.URL)
	}
	if strings.Contains(r.URL.String(), l.key("engine-bot")) {
		l.t.Error("the key is in the address")
	}
	moved := r.Clone(r.Context())
	moved.URL.Scheme, moved.URL.Host, moved.Host = l.target.Scheme, l.target.Host, l.target.Host
	return http.DefaultTransport.RoundTrip(moved)
}

func (l *redmineLive) key(account string) string {
	var entry struct{ Key string }
	json.Unmarshal(l.keys[account], &entry)
	return entry.Key
}

func (l *redmineLive) number(entry, field string) int64 {
	var fields map[string]int64
	json.Unmarshal(l.keys[entry], &fields)
	return fields[field]
}

// as calls the live Redmine directly as another account, the way a person's
// browser or script would.
func (l *redmineLive) as(account, method, path string, body any) (int, []byte) {
	l.t.Helper()
	var reader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reader = bytes.NewReader(data)
	}
	request, err := http.NewRequest(method, l.target.String()+path, reader)
	if err != nil {
		l.t.Fatal(err)
	}
	request.Header.Set("X-Redmine-API-Key", l.key(account))
	request.Header.Set("Content-Type", "application/json")
	// A Redmine that stops answering fails the check rather than holding it.
	response, err := (&http.Client{Transport: http.DefaultTransport, Timeout: 30 * time.Second}).Do(request)
	if err != nil {
		l.t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	return response.StatusCode, data
}

func (l *redmineLive) file(account string, project int64, category int64, subject string) int64 {
	l.t.Helper()
	fields := map[string]any{"project_id": project, "subject": subject, "description": "Live check body\nsecond line 日本語"}
	if category > 0 {
		fields["category_id"] = category
	}
	status, data := l.as(account, http.MethodPost, "/issues.json", map[string]any{"issue": fields})
	var created struct{ Issue struct{ ID int64 } }
	if status != http.StatusCreated || json.Unmarshal(data, &created) != nil || created.Issue.ID <= 0 {
		l.t.Fatalf("filing an issue answered %d: %.300s", status, data)
	}
	return created.Issue.ID
}

func (l *redmineLive) note(account string, issue int64, notes string, private bool) {
	l.t.Helper()
	status, data := l.as(account, http.MethodPut, "/issues/"+strconv.FormatInt(issue, 10)+".json",
		map[string]any{"issue": map[string]any{"notes": notes, "private_notes": private}})
	if status != http.StatusNoContent {
		l.t.Fatalf("a note answered %d: %.300s", status, data)
	}
}

func TestRedmineLiveAdapter(t *testing.T) {
	live, project := redmineLiveSetup(t)
	ctx := context.Background()
	me, err := project.Myself(ctx)
	if err != nil || me.ID != live.number("engine-bot", "id") || me.Login != "engine-bot" {
		t.Fatalf("myself: %+v %v", me, err)
	}
	requester := live.number("requester", "id")
	engineCategory := live.number("categories", "engine")
	otherCategory := live.number("categories", "other")
	// Redmine keeps whole seconds; the start is the second before the first
	// issue of this run.
	project.Since = time.Now().UTC().Truncate(time.Second).Add(-time.Second)
	marked := live.file("requester", project.ProjectID, engineCategory, "Live marked")
	other := live.file("requester", project.ProjectID, otherCategory, "Live other category")
	plain := live.file("requester", project.ProjectID, 0, "Live without category")
	sub := live.file("requester", live.number("project", "sub_id"), 0, "Live in a subproject")
	t.Logf("filed %d %d %d %d", marked, other, plain, sub)

	listed := func(p RedmineProject) []int64 {
		t.Helper()
		rows, err := p.Issues(ctx)
		if err != nil {
			t.Fatal(err)
		}
		ids := []int64{}
		for _, raw := range rows {
			issue, err := p.ReadIssue(raw)
			if err != nil || issue.Created.Before(p.Since) {
				t.Fatalf("listed a record it cannot read or one before the start: %s %v", raw, err)
			}
			if p.Marked(issue) {
				ids = append(ids, issue.ID)
			}
		}
		return ids
	}
	if got := fmt.Sprint(listed(project)); got != fmt.Sprint([]int64{marked, other, plain}) {
		t.Fatalf("the project's open issues since the start, without the subproject's: %s", got)
	}
	narrowed := project
	narrowed.Categories = []int64{engineCategory}
	if got := fmt.Sprint(listed(narrowed)); got != fmt.Sprint([]int64{marked}) {
		t.Fatalf("the category did not narrow the intake: %s", got)
	}
	later := project
	later.Since = time.Now().UTC().Add(time.Hour)
	if got := listed(later); len(got) != 0 {
		t.Fatalf("issues created before the start were listed: %v", got)
	}

	issue := Issue{ID: marked, Key: strconv.FormatInt(marked, 10)}
	text, err := project.Request(ctx, issue.Key)
	if err != nil || text != "Original issue: "+issue.Key+"\nTitle: Live marked\n\nLive check body\r\nsecond line 日本語" {
		t.Fatalf("request text: %q %v", text, err)
	}

	question := "どちらにしますか？ (a) この範囲 (b) 別の範囲\n@requester へ"
	posted, err := project.AddComment(ctx, issue, question)
	if err != nil || posted <= 0 {
		t.Fatalf("question: %d %v", posted, err)
	}
	live.note("requester", marked, "(a) でお願いします。", false)
	live.note("requester", marked, "停止\n理由はあとで", false)
	rows, err := project.Comments(ctx, issue)
	if err != nil || len(rows) != 3 {
		t.Fatalf("comments: %d %v", len(rows), err)
	}
	want := []Comment{{ID: posted, Body: question, Author: Account{ID: me.ID}, OnIssue: true}}
	for i, raw := range rows {
		comment, err := project.ReadComment(raw, issue)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 && comment != want[0] {
			t.Fatalf("the engine's note read back as %+v", comment)
		}
		if i > 0 && (comment.Author.ID != requester || !comment.OnIssue || comment.ID <= posted) {
			t.Fatalf("the requester's note read back as %+v", comment)
		}
		if elsewhere, _ := project.ReadComment(raw, Issue{ID: other, Key: strconv.FormatInt(other, 10)}); elsewhere.OnIssue {
			t.Fatal("a note was placed on another issue")
		}
	}

	project.Statuses = map[string]int64{Processing: 2, AwaitingRequester: 4, Delivered: 3, Stopped: 5}
	for _, turn := range []string{Processing, AwaitingRequester, Processing, Delivered, Stopped} {
		if err := project.Move(ctx, issue, turn); err != nil {
			t.Fatalf("%s: %v", turn, err)
		}
	}
	// The engine's role has no move out of Closed: Redmine answers 204 and
	// leaves the status.
	if err := project.Move(ctx, issue, Processing); err == nil || !strings.Contains(err.Error(), "still in status 5") {
		t.Fatalf("an ignored move was taken for done: %v", err)
	}
	for _, to := range []Account{me, {ID: requester}} {
		if err := project.Assign(ctx, issue, to); err != nil {
			t.Fatalf("assign %d: %v", to.ID, err)
		}
	}
	if err := project.Assign(ctx, issue, Account{ID: 999999}); err == nil {
		t.Fatal("an account Redmine cannot assign was taken for assigned")
	}
	if err := project.RecordHours(ctx, issue, 1.5); err != nil {
		t.Fatal(err)
	}

	if err := project.EditComment(ctx, issue, posted, question+"（追記）"); err != nil {
		t.Fatalf("editing the engine's own note: %v", err)
	}
	rows, _ = project.Comments(ctx, issue)
	if id, words, _ := project.CommentText(rows[0]); id != posted || words != question+"（追記）" {
		t.Fatalf("edited note reads %d %q", id, words)
	}
	theirs, words, _ := project.CommentText(rows[1])
	if err := project.EditComment(ctx, issue, theirs, "not mine"); err == nil {
		t.Fatal("the requester's note was edited")
	}
	rows, _ = project.Comments(ctx, issue)
	if _, now, _ := project.CommentText(rows[1]); now != words {
		t.Fatal("the requester's note changed")
	}
	if err := project.EditComment(ctx, Issue{ID: other, Key: strconv.FormatInt(other, 10)}, posted, "elsewhere"); err == nil {
		t.Fatal("a note was edited through another issue")
	}

	// An administrator rewrites the requester's note: the words are the
	// administrator's now, not the requester's.
	status, data := live.as("admin", http.MethodPut, "/journals/"+strconv.FormatInt(theirs, 10)+".json", map[string]any{"journal": map[string]any{"notes": "停止"}})
	if status != http.StatusNoContent {
		t.Fatalf("the administrator's edit answered %d: %s", status, data)
	}
	rows, _ = project.Comments(ctx, issue)
	for _, raw := range rows {
		comment, _ := project.ReadComment(raw, issue)
		if comment.ID != theirs {
			continue
		}
		var fields map[string]json.RawMessage
		json.Unmarshal(raw, &fields)
		if _, named := fields["updated_by"]; !named {
			// Redmine 5.x does not say in its API that a note was edited, or
			// by whom (6.0 does): the limit SETUP.md states.
			t.Logf("this Redmine names no editor; the edited note stays its first author's: %+v", comment)
			if comment.Author.ID != requester {
				t.Fatalf("an edited note read as %+v", comment)
			}
			continue
		}
		if comment.Author.ID == requester {
			t.Fatalf("an edited note is still credited to its first author: %+v", comment)
		}
	}

	// The issue is moved to the subproject: the engine stops there.
	status, data = live.as("admin", http.MethodPut, "/issues/"+issue.Key+".json", map[string]any{"issue": map[string]any{"project_id": live.number("project", "sub_id")}})
	if status != http.StatusNoContent {
		t.Fatalf("moving the issue answered %d: %s", status, data)
	}
	if _, err := project.Comments(ctx, issue); err == nil || !strings.Contains(err.Error(), "no longer in the configured project") {
		t.Fatalf("the engine read an issue moved to another project: %v", err)
	}
	if _, err := project.AddComment(ctx, issue, "after the move"); err == nil {
		t.Fatal("the engine wrote to an issue moved to another project")
	}
}

// The list goes past a page of a hundred by asking for the issues after the
// last one read.
func TestRedmineLivePagesPastAHundred(t *testing.T) {
	live, project := redmineLiveSetup(t)
	// The start is the first issue's own creation time, by Redmine's clock,
	// which need not agree with this machine's; the issues other tests filed
	// are seconds older.
	time.Sleep(2 * time.Second)
	project.Categories = []int64{live.number("categories", "other")}
	var filed []int64
	for n := range 105 {
		filed = append(filed, live.file("requester", project.ProjectID, project.Categories[0], fmt.Sprintf("Live page %d", n)))
	}
	var first struct {
		Issue struct {
			Created time.Time `json:"created_on"`
		}
	}
	if _, data := live.as("admin", http.MethodGet, "/issues/"+strconv.FormatInt(filed[0], 10)+".json", nil); json.Unmarshal(data, &first) != nil {
		t.Fatal("the first issue could not be read")
	}
	project.Since = first.Issue.Created
	rows, err := project.Issues(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var listed []int64
	for _, raw := range rows {
		issue, _ := project.ReadIssue(raw)
		listed = append(listed, issue.ID)
		if !slices.Contains(filed, issue.ID) {
			t.Logf("listed besides those filed: %.200s", raw)
		}
	}
	if fmt.Sprint(listed) != fmt.Sprint(filed) {
		t.Fatalf("listed %d of the %d filed", len(listed), len(filed))
	}
}

// An issue filed without an account is not taken, and a note written
// without one is nobody's. It runs where the Redmine has a public project
// "public-check" in which the Anonymous role may add issues and notes, and
// the engine's account is a member.
func TestRedmineLiveAnonymousAuthor(t *testing.T) {
	live, project := redmineLiveSetup(t)
	status, data := live.as("engine-bot", http.MethodGet, "/projects/public-check.json", nil)
	var found struct{ Project struct{ ID int64 } }
	if status != http.StatusOK || json.Unmarshal(data, &found) != nil {
		t.Skip("the Redmine has no public project public-check")
	}
	project.ProjectID = found.Project.ID
	project.Since = time.Now().UTC().Truncate(time.Second).Add(-time.Second)
	anonymous := func(method, path string, body any) []byte {
		encoded, _ := json.Marshal(body)
		request, _ := http.NewRequest(method, live.target.String()+path, bytes.NewReader(encoded))
		request.Header.Set("Content-Type", "application/json")
		response, err := (&http.Client{Timeout: 30 * time.Second}).Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		answer, _ := io.ReadAll(response.Body)
		if response.StatusCode >= 300 {
			t.Fatalf("%s %s without an account answered %d: %s", method, path, response.StatusCode, answer)
		}
		return answer
	}
	var created struct{ Issue struct{ ID int64 } }
	json.Unmarshal(anonymous(http.MethodPost, "/issues.json", map[string]any{"issue": map[string]any{"project_id": project.ProjectID,
		"subject": "Filed without an account", "description": "x"}}), &created)
	byRequester := live.file("requester", project.ProjectID, 0, "Filed by the requester")
	rows, err := project.Issues(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var listed []int64
	for _, raw := range rows {
		issue, _ := project.ReadIssue(raw)
		listed = append(listed, issue.ID)
	}
	if fmt.Sprint(listed) != fmt.Sprint([]int64{byRequester}) {
		t.Fatalf("listed %v; only %d (not the anonymous %d) must be", listed, byRequester, created.Issue.ID)
	}
	key := strconv.FormatInt(byRequester, 10)
	anonymous(http.MethodPut, "/issues/"+key+".json", map[string]any{"issue": map[string]any{"notes": "停止"}})
	journals, err := project.Comments(context.Background(), Issue{ID: byRequester, Key: key})
	if err != nil || len(journals) != 1 {
		t.Fatalf("journals %d (%v)", len(journals), err)
	}
	if comment, _ := project.ReadComment(journals[0], Issue{ID: byRequester, Key: key}); comment.Author.ID != 0 {
		t.Fatalf("a note written without an account is credited to %d", comment.Author.ID)
	}
}

func TestRedmineLiveScopeForRoles(t *testing.T) {
	live, project := redmineLiveSetup(t)
	ctx := context.Background()
	project.Since = time.Now().UTC().Add(-time.Minute)
	id := live.file("requester", project.ProjectID, 0, "Live scope")
	key := strconv.FormatInt(id, 10)
	live.note("requester", id, "Requester context.", false)
	live.note("operator", id, "Operator private note.", true)

	access, err := ServeIssue(ctx, project, key, true, KeepLatestPost)
	if err != nil {
		t.Fatal(err)
	}
	client, err := CertificateClient(access.Certificate)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("REDMINE_LIVE_SCOPE_KEY", access.Key)
	role := Backlog{BaseURL: access.URL, KeyEnv: "REDMINE_LIVE_SCOPE_KEY", Client: client}
	text, err := role.Request(ctx, key)
	if err != nil || !strings.HasPrefix(text, "Original issue: "+key+"\nTitle: Live scope\n\n") {
		t.Fatalf("role read: %q %v", text, err)
	}
	var stored []int64
	for _, words := range []string{"trial line", "the role's question"} {
		receipt, err := role.AddComment(ctx, key, words)
		if err != nil {
			t.Fatal(err)
		}
		var posted struct {
			ID      int64
			Content string
		}
		if json.Unmarshal(receipt, &posted) != nil || posted.ID <= 0 || posted.Content != words {
			t.Fatalf("receipt: %s", receipt)
		}
		stored = append(stored, posted.ID)
	}
	comments, err := role.Comments(ctx, key, 0)
	if err != nil {
		t.Fatal(err)
	}
	var contents []string
	for _, raw := range comments {
		var comment struct{ Content string }
		json.Unmarshal(raw, &comment)
		contents = append(contents, comment.Content)
	}
	// The engine's account may see private notes here; its roles never do.
	if got := strings.Join(contents, "|"); got != "Requester context.|trial line|the role's question" {
		t.Fatalf("the role read %q", got)
	}
	after, err := role.Comments(ctx, key, stored[0])
	if err != nil || len(after) != 1 {
		t.Fatalf("comments after the first post: %d %v", len(after), err)
	}
	if readback, err := role.Comment(ctx, key, stored[1]); err != nil || !bytes.Contains(readback, []byte(`"the role's question"`)) {
		t.Fatalf("read back: %s %v", readback, err)
	}
	if _, err := role.Comment(ctx, key, stored[1]+100000); err == nil {
		t.Fatal("a comment that is not on the issue was read")
	}
	access.Close()
	rows, err := project.Comments(ctx, Issue{ID: id, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	contents = nil
	for _, raw := range rows {
		_, words, _ := project.CommentText(raw)
		contents = append(contents, words)
	}
	if got := strings.Join(contents, "|"); got != "Requester context.|Operator private note.|the role's question" {
		t.Fatalf("the launch's earlier post was not removed: %q", got)
	}
}
