package tracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Only a synthetic key and local TLS servers are used; no request leaves the
// machine. The records are the ones a Redmine 6.0.11 answered with
// (testdata/redmine, written by the live check's setup with the key removed).
const redmineTestKey = "redmine-test-key"

func redmineTestdata(t *testing.T, name string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "redmine", name))
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	return record
}

// redmineRecord is issue 5 as Redmine sent it, without its journals.
func redmineRecord(t *testing.T) map[string]any {
	t.Helper()
	return redmineTestdata(t, "issue.json")["issue"].(map[string]any)
}

// fakeRedmine keeps one issue and its journals the way Redmine does: a note
// or a change answers 204 without a body, a status or an assignee Redmine
// would not set is left as it was without an error, and a journal is edited
// or emptied by its own id.
type fakeRedmine struct {
	t        *testing.T
	mu       sync.Mutex
	issue    map[string]any
	journals []map[string]any
	next     int64
	calls    []string
	// ignore names the fields Redmine leaves as they were.
	ignore map[string]bool
	// refuse answers a method and path with a status and no change.
	refuse map[string]int
	// lose keeps a note from being stored, as a note lost on the way.
	lose bool
	// users are the accounts GET /users/<id>.json finds, by login; any other
	// id is answered 404, as Redmine answers for its anonymous user.
	users map[int64]string
	// before runs ahead of the fake's own handling; true when it answered.
	before func(w http.ResponseWriter, r *http.Request) bool
}

func redmineFixture(t *testing.T) (*fakeRedmine, RedmineProject) {
	t.Helper()
	t.Setenv("REDMINE_TEST_KEY", redmineTestKey)
	fake := &fakeRedmine{t: t, issue: redmineRecord(t), next: 900, ignore: map[string]bool{}, refuse: map[string]int{},
		users: map[int64]string{5: "engine-bot", 6: "requester", 7: "operator"}}
	for _, raw := range redmineTestdata(t, "issue-journals.json")["issue"].(map[string]any)["journals"].([]any) {
		fake.journals = append(fake.journals, raw.(map[string]any))
	}
	server := httptest.NewTLSServer(http.HandlerFunc(fake.serve))
	// The process keeps what it learned about an address, and a later server
	// can be given this one's port: it starts with none and leaves none.
	forgetRedmine(server.URL)
	t.Cleanup(func() {
		server.Close()
		forgetRedmine(server.URL)
	})
	return fake, RedmineProject{Client: Redmine{BaseURL: server.URL, KeyEnv: "REDMINE_TEST_KEY", Client: server.Client()}, ProjectID: 1}
}

// forgetRedmine drops what the process keeps for the Redmine at base.
func forgetRedmine(base string) {
	redmineKnown.Lock()
	defer redmineKnown.Unlock()
	for place := range redmineKnown.me {
		if strings.HasPrefix(place, base+"\x00") {
			delete(redmineKnown.me, place)
		}
	}
	for place := range redmineKnown.persons {
		if strings.HasPrefix(place, base+"\x00") {
			delete(redmineKnown.persons, place)
		}
	}
}

var redmineUserPath = regexp.MustCompile(`^/users/([0-9]+)\.json$`)

func (f *fakeRedmine) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("X-Redmine-API-Key") != redmineTestKey {
		f.t.Errorf("request without the key header: %v", r.Header)
	}
	if strings.Contains(r.URL.String(), redmineTestKey) {
		f.t.Error("the key is in the address")
	}
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	if r.Method == http.MethodGet && redmineUserPath.MatchString(r.URL.Path) {
		if status := f.refuse[r.Method+" "+r.URL.Path]; status != 0 {
			w.WriteHeader(status)
			return
		}
		id, _ := strconv.ParseInt(redmineUserPath.FindStringSubmatch(r.URL.Path)[1], 10, 64)
		login, found := f.users[id]
		if !found {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"user": map[string]any{"id": id, "login": login, "firstname": login, "lastname": "Check"}})
		return
	}
	if f.before != nil && f.before(w, r) {
		return
	}
	if status := f.refuse[r.Method+" "+r.URL.Path]; status != 0 {
		w.WriteHeader(status)
		return
	}
	id := strconv.FormatFloat(f.issue["id"].(float64), 'f', -1, 64)
	var body map[string]map[string]any
	if r.Method == http.MethodPut {
		if r.Header.Get("Content-Type") != "application/json" || json.NewDecoder(r.Body).Decode(&body) != nil {
			f.t.Errorf("a change that is not JSON: %s", r.URL)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/users/current.json":
		json.NewEncoder(w).Encode(redmineTestdata(f.t, "users-current.json"))
	case r.Method == http.MethodGet && r.URL.Path == "/issues/"+id+".json":
		record := map[string]any{}
		for k, v := range f.issue {
			record[k] = v
		}
		if r.URL.Query().Get("include") == "journals" {
			record["journals"] = f.journals
		}
		json.NewEncoder(w).Encode(map[string]any{"issue": record})
	case r.Method == http.MethodPut && r.URL.Path == "/issues/"+id+".json":
		fields := body["issue"]
		if notes, ok := fields["notes"].(string); ok && strings.TrimSpace(notes) != "" && !f.lose {
			f.next++
			f.journals = append(f.journals, map[string]any{"id": f.next, "user": map[string]any{"id": 5, "name": "Engine Check"},
				"notes": notes, "created_on": "2026-10-20T05:00:00Z", "private_notes": false, "details": []any{}})
		}
		if status, ok := fields["status_id"].(float64); ok && !f.ignore["status_id"] {
			f.issue["status"] = map[string]any{"id": status, "name": "Configured"}
		}
		if assignee, ok := fields["assigned_to_id"].(float64); ok {
			if assignee == 999999 {
				w.WriteHeader(http.StatusUnprocessableEntity)
				fmt.Fprint(w, `{"errors":["Assignee is invalid"]}`)
				return
			}
			if !f.ignore["assigned_to_id"] {
				f.issue["assigned_to"] = map[string]any{"id": assignee, "name": "Someone"}
			}
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/journals/"):
		for i, journal := range f.journals {
			if fmt.Sprint(journal["id"]) != strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/journals/"), ".json") {
				continue
			}
			notes, _ := body["journal"]["notes"].(string)
			if details, _ := journal["details"].([]any); notes == "" && len(details) == 0 {
				f.journals = append(f.journals[:i:i], f.journals[i+1:]...)
			} else {
				journal["notes"] = notes
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	default:
		f.t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusBadRequest)
	}
}

// called lists the requests made, leaving out the look-ups of authors, which
// the process keeps and so asks once.
func (f *fakeRedmine) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	calls := []string{}
	for _, call := range f.calls {
		if !redmineUserPath.MatchString(strings.TrimPrefix(call, "GET ")) {
			calls = append(calls, call)
		}
	}
	return calls
}

func TestRedmineAsksWithItsKeyHeaderAndKeepsTheKeyOutOfErrors(t *testing.T) {
	fake, redmine := redmineFixture(t)
	ctx := context.Background()
	if me, err := redmine.Myself(ctx); err != nil || me != (Account{ID: 5, Login: "engine-bot"}) {
		t.Fatalf("myself: %+v (%v)", me, err)
	}
	if text, err := redmine.Request(ctx, "5"); err != nil || text != "Original issue: 5\nTitle: Live marked\n\nLive check body\r\nsecond line 日本語" {
		t.Fatalf("request: %q (%v)", text, err)
	}
	fake.before = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/issues/8.json" {
			return false
		}
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "upstream detail "+redmineTestKey)
		return true
	}
	_, err := redmine.Request(ctx, "8")
	if err == nil || !strings.Contains(err.Error(), "500: upstream detail [credential]") || strings.Contains(err.Error(), redmineTestKey) {
		t.Fatalf("the reason was lost or the key leaked: %v", err)
	}
	for _, key := range []string{"", "0", "-1", "07", "EXAMPLE-1", "1/../2", "5.json"} {
		if _, err := redmine.Request(ctx, key); err == nil {
			t.Errorf("%q was read as an issue id", key)
		}
	}
	if calls := fake.called(); len(calls) != 3 {
		t.Fatalf("calls: %v", calls)
	}
	// An issue the answer does not name as the one asked for is refused.
	fake.before = func(w http.ResponseWriter, r *http.Request) bool {
		json.NewEncoder(w).Encode(map[string]any{"issue": map[string]any{"id": 6, "subject": "another"}})
		return true
	}
	if text, err := redmine.Request(ctx, "5"); err == nil {
		t.Fatalf("another issue's record was rendered: %q", text)
	}
}

func TestRedmineRefusesAnAddressThatIsNotItsOwnHTTPSOne(t *testing.T) {
	t.Setenv("REDMINE_TEST_KEY", redmineTestKey)
	for _, base := range []string{"http://redmine.example", "https:///redmine", "https://someone:secret@redmine.example",
		"https://redmine.example/?query", "https://redmine.example/#part"} {
		r := RedmineProject{Client: Redmine{BaseURL: base, KeyEnv: "REDMINE_TEST_KEY"}, ProjectID: 1}
		if _, err := r.Myself(context.Background()); err == nil || !strings.Contains(err.Error(), "HTTPS URL") {
			t.Errorf("%s: %v", base, err)
		}
	}
	t.Setenv("REDMINE_TEST_KEY", "")
	r := RedmineProject{Client: Redmine{BaseURL: "https://redmine.example", KeyEnv: "REDMINE_TEST_KEY"}}
	if _, err := r.Myself(context.Background()); err == nil || !strings.Contains(err.Error(), "credential is unavailable") {
		t.Fatalf("no key: %v", err)
	}
}

// servedIssues answers a list of the issues 1 to n of project 1.
func servedIssues(t *testing.T, n int, w http.ResponseWriter, r *http.Request, change func(int, map[string]any)) {
	ids := make([]int, n)
	for i := range ids {
		ids[i] = i + 1
	}
	serveIssueList(t, ids, w, r, change)
}

// serveIssueList answers a list of issues of project 1 as Redmine does: in
// ascending id, narrowed by issue_id=>=N when asked, then by offset and
// limit, with the count of all that the narrowing leaves.
func serveIssueList(t *testing.T, ids []int, w http.ResponseWriter, r *http.Request, change func(int, map[string]any)) {
	q := r.URL.Query()
	from := 0
	if cursor, found := strings.CutPrefix(q.Get("issue_id"), ">="); found {
		from, _ = strconv.Atoi(cursor)
	}
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	listed := []int{}
	for _, id := range ids {
		if id >= from {
			listed = append(listed, id)
		}
	}
	page := []any{}
	for _, id := range listed[min(offset, len(listed)):min(offset+limit, len(listed))] {
		record := redmineRecord(t)
		record["id"] = id
		if change != nil {
			change(id, record)
		}
		page = append(page, record)
	}
	json.NewEncoder(w).Encode(map[string]any{"issues": page, "total_count": len(listed), "offset": offset, "limit": limit})
}

func TestRedmineReadsEveryPageOfTheProjectsOpenIssues(t *testing.T) {
	fake, redmine := redmineFixture(t)
	redmine.Categories = []int64{1, 4}
	redmine.Since = time.Date(2026, 10, 20, 4, 0, 0, 500, time.FixedZone("JST", 9*3600))
	pages := 0
	fake.before = func(w http.ResponseWriter, r *http.Request) bool {
		q := r.URL.Query()
		cursor := ""
		if pages > 0 {
			cursor = ">=" + strconv.Itoa(pages*100+1)
		}
		if r.URL.Path != "/issues.json" || q.Get("project_id") != "1" || q.Get("subproject_id") != "!*" || q.Get("status_id") != "open" ||
			q.Get("sort") != "id" || q.Get("limit") != "100" || q.Get("issue_id") != cursor || q.Has("offset") ||
			q.Get("created_on") != ">=2026-10-19T19:00:00Z" || q.Get("category_id") != "1|4" {
			t.Errorf("a page was asked as %s", r.URL)
		}
		pages++
		servedIssues(t, 205, w, r, nil)
		return true
	}
	rows, err := redmine.Issues(context.Background())
	if err != nil || len(rows) != 205 || pages != 3 {
		t.Fatalf("issues=%d pages=%d error=%v", len(rows), pages, err)
	}
	for i, raw := range rows {
		issue, err := redmine.ReadIssue(raw)
		if err != nil || issue.ID != int64(i+1) || !redmine.Marked(issue) {
			t.Fatalf("an unreadable or an unmarked record was listed: %s (%v)", raw, err)
		}
	}
	// Without categories or a start, Redmine is asked for neither.
	redmine.Categories, redmine.Since, pages = nil, time.Time{}, 0
	fake.before = func(w http.ResponseWriter, r *http.Request) bool {
		if q := r.URL.Query(); q.Has("category_id") || q.Has("created_on") {
			t.Errorf("asked as %s", r.URL)
		}
		pages++
		servedIssues(t, 100, w, r, nil)
		return true
	}
	// A full page may be the last; the page after it, empty, says so.
	if rows, err := redmine.Issues(context.Background()); err != nil || len(rows) != 100 || pages != 2 {
		t.Fatalf("a full last page was lost: issues=%d pages=%d error=%v", len(rows), pages, err)
	}
	redmine.ProjectID = 0
	if _, err := redmine.Issues(context.Background()); err == nil {
		t.Fatal("issues were listed without a project")
	}
}

func TestRedmineReturnsNoListThatIsPartialOrNotTheProjects(t *testing.T) {
	for _, failure := range []string{"another project", "cursor ignored", "repeated issue", "second page outage", "moved", "not a list",
		"oversized page", "over a hundred"} {
		t.Run(failure, func(t *testing.T) {
			fake, redmine := redmineFixture(t)
			pages := 0
			fake.before = func(w http.ResponseWriter, r *http.Request) bool {
				pages++
				switch failure {
				case "another project":
					servedIssues(t, 101, w, r, func(id int, record map[string]any) {
						if id == 101 {
							record["project"] = map[string]any{"id": 2, "name": "Intake sub"}
						}
					})
				case "cursor ignored":
					// A server that does not narrow by issue_id gives the first
					// page again.
					r.URL.RawQuery = strings.ReplaceAll(r.URL.RawQuery, "issue_id=", "ignored=")
					servedIssues(t, 101, w, r, nil)
				case "repeated issue":
					serveIssueList(t, []int{1, 2, 2, 3}, w, r, nil)
				case "second page outage":
					if pages == 2 {
						w.WriteHeader(http.StatusServiceUnavailable)
						fmt.Fprint(w, "second page unavailable")
						return true
					}
					servedIssues(t, 101, w, r, nil)
				case "moved":
					w.Header().Set("Location", "https://redmine.example/login?back_url="+redmineTestKey)
					w.WriteHeader(http.StatusFound)
				case "not a list":
					fmt.Fprint(w, `{"issue":{"id":1}}`)
				case "oversized page":
					fmt.Fprint(w, `{"issues":[],"total_count":0}`+strings.Repeat(" ", 32<<20))
				case "over a hundred":
					page := []any{}
					for id := 1; id <= 101; id++ {
						record := redmineRecord(t)
						record["id"] = id
						page = append(page, record)
					}
					json.NewEncoder(w).Encode(map[string]any{"issues": page, "total_count": 300})
				}
				return true
			}
			rows, err := redmine.Issues(context.Background())
			want := map[string]string{"another project": "outside the configured project", "cursor ignored": "ascending id after the last one read",
				"repeated issue": "ascending id after the last one read", "second page outage": "503: second page unavailable",
				"moved": "HTTP 302, a redirect to", "not a list": "not a bounded issue list", "oversized page": "exceeds 32 MiB",
				"over a hundred": "not a bounded issue list"}[failure]
			if err == nil || rows != nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), redmineTestKey) {
				t.Fatalf("a list was returned, or for another reason: rows=%d pages=%d error=%v", len(rows), pages, err)
			}
			if failure == "moved" && !strings.Contains(err.Error(), "check redmine.base_url") {
				t.Fatalf("the operator is not told what to check: %v", err)
			}
		})
	}
}

// A record that names its place but cannot be read for the rest is listed,
// for the intake to leave aside and say so, as the other trackers' are.
func TestRedmineListsARecordItCannotReadForTheIntakeToLeaveAside(t *testing.T) {
	fake, redmine := redmineFixture(t)
	fake.before = func(w http.ResponseWriter, r *http.Request) bool {
		servedIssues(t, 2, w, r, func(id int, record map[string]any) {
			if id == 2 {
				record["created_on"] = "yesterday"
			}
		})
		return true
	}
	rows, err := redmine.Issues(context.Background())
	if err != nil || len(rows) != 2 {
		t.Fatalf("issues=%d error=%v", len(rows), err)
	}
	if issue, err := redmine.ReadIssue(rows[1]); err == nil {
		t.Fatalf("a record with an unreadable time was read as %+v", issue)
	}
}

func TestRedmineReadsAnIssueRecordAsTheEngineKeepsIt(t *testing.T) {
	redmine := RedmineProject{Client: Redmine{BaseURL: "https://redmine.example/tracker/", KeyEnv: "REDMINE_TEST_KEY"}, ProjectID: 1}
	if got := redmine.Identity(); got != "Issue intake: https://redmine.example/tracker\nRedmine project: 1" {
		t.Fatalf("identity: %q", got)
	}
	backlog := BacklogProject{Client: Backlog{BaseURL: "https://redmine.example/tracker"}, ProjectID: 1}
	if backlog.Identity() == redmine.Identity() {
		t.Fatal("a Redmine queue has a Backlog queue's identity")
	}
	raw, _ := json.Marshal(redmineRecord(t))
	issue, err := redmine.ReadIssue(raw)
	if err != nil || issue.ID != 5 || issue.Key != "5" || issue.Creator != (Account{ID: 6}) ||
		!issue.Created.Equal(time.Date(2026, 10, 20, 4, 56, 8, 0, time.UTC)) || !bytes.Equal(issue.Raw, raw) {
		t.Fatalf("issue read as %+v (%v)", issue, err)
	}
	if text, err := redmine.RequestText(raw); err != nil || text != "Original issue: 5\nTitle: Live marked\n\nLive check body\r\nsecond line 日本語" {
		t.Fatalf("request text: %q (%v)", text, err)
	}
	empty := redmineRecord(t)
	empty["description"] = nil
	raw, _ = json.Marshal(empty)
	if text, err := redmine.RequestText(raw); err != nil || text != "Original issue: 5\nTitle: Live marked\n\n" {
		t.Fatalf("an issue without a description: %q (%v)", text, err)
	}
	for _, categories := range [][]int64{nil, {1}, {3, 1}} {
		redmine.Categories = categories
		if !redmine.Marked(issue) {
			t.Errorf("categories %v: the issue was not taken up", categories)
		}
	}
	redmine.Categories = []int64{2}
	if redmine.Marked(issue) {
		t.Error("an issue without the intake category was taken up")
	}
	uncategorized := redmineRecord(t)
	delete(uncategorized, "category")
	raw, _ = json.Marshal(uncategorized)
	if issue, _ := redmine.ReadIssue(raw); redmine.Marked(issue) {
		t.Error("an issue without a category was taken up")
	}
	for name, change := range map[string]func(map[string]any){
		"another project":      func(r map[string]any) { r["project"] = map[string]any{"id": 2} },
		"no project":           func(r map[string]any) { delete(r, "project") },
		"no id":                func(r map[string]any) { delete(r, "id") },
		"unreadable time":      func(r map[string]any) { r["created_on"] = "yesterday" },
		"unreadable requester": func(r map[string]any) { r["author"] = "someone" },
	} {
		record := redmineRecord(t)
		change(record)
		raw, _ := json.Marshal(record)
		if issue, err := redmine.ReadIssue(raw); err == nil {
			t.Errorf("%s: read as %+v", name, issue)
		}
	}
	for _, raw := range []string{`{"subject":"no id"}`, `not JSON`} {
		if text, err := redmine.RequestText(json.RawMessage(raw)); err == nil {
			t.Errorf("%s: rendered as %q", raw, text)
		}
	}
}

func TestRedmineReadsJournalsInOrderAndPlacesThemByTheirIssue(t *testing.T) {
	fake, redmine := redmineFixture(t)
	issue := Issue{ID: 5, Key: "5"}
	rows, err := redmine.Comments(context.Background(), issue)
	if err != nil || len(rows) != 10 {
		t.Fatalf("journals=%d error=%v", len(rows), err)
	}
	// Each record is Redmine's own, with the issue it came from in front.
	original, _ := json.Marshal(fake.journals[1])
	if !bytes.Equal(rows[1], append([]byte(`{"issue":{"id":5},`), original[1:]...)) {
		t.Fatalf("the record was changed beyond its place: %s", rows[1])
	}
	comment, err := redmine.ReadComment(rows[1], issue)
	if err != nil || comment != (Comment{ID: 275, Body: "(a) でお願いします。", Author: Account{ID: 6}, OnIssue: true}) {
		t.Fatalf("a note read as %+v (%v)", comment, err)
	}
	// A change without words is a comment without words, as Backlog's is.
	if change, err := redmine.ReadComment(rows[3], issue); err != nil || change.Body != "" || change.Author.ID != 5 || !change.OnIssue {
		t.Fatalf("a change read as %+v (%v)", change, err)
	}
	if elsewhere, _ := redmine.ReadComment(rows[1], Issue{ID: 6, Key: "6"}); elsewhere.OnIssue {
		t.Fatal("a journal was placed on another issue")
	}
	for raw, want := range map[string]Comment{
		`{"id":7,"notes":null,"user":null,"issue":{"id":5}}`:                     {ID: 7, OnIssue: true},
		`{"id":7,"notes":"x","user":{"id":6,"name":"R"},"issue":{"id":5}}`:       {ID: 7, Body: "x", Author: Account{ID: 6}, OnIssue: true},
		`{"id":7,"notes":"x","issue":{"id":6}}`:                                  {ID: 7, Body: "x"},
		`{"id":7,"notes":"x"}`:                                                   {ID: 7, Body: "x"},
		`{"id":7,"notes":"x","issue":null}`:                                      {ID: 7, Body: "x"},
		`{"id":7,"notes":"x","user":{"id":6},"issue":{"id":5,"subject":"kept"}}`: {ID: 7, Body: "x", Author: Account{ID: 6}, OnIssue: true},
	} {
		if comment, err := redmine.ReadComment(json.RawMessage(raw), issue); err != nil || comment != want {
			t.Errorf("%s: read as %+v (%v)", raw, comment, err)
		}
	}
	for _, raw := range []string{`{"id":0}`, `{"notes":"x"}`, `{"id":"7"}`, `{"id":7,"notes":42}`, `not JSON`} {
		if comment, err := redmine.ReadComment(json.RawMessage(raw), issue); err == nil {
			t.Errorf("%s: read as %+v", raw, comment)
		}
	}
	if id, words, err := redmine.CommentText(json.RawMessage(`{"id":7,"notes":"words","user":"odd","issue":42}`)); err != nil || id != 7 || words != "words" {
		t.Fatalf("id and words: %d %q (%v)", id, words, err)
	}
	// A journal that names an issue of its own keeps it, and one that is
	// empty still gets its place.
	fake.journals = []map[string]any{{"id": 1, "user": map[string]any{"id": 6}}, {"id": 2, "issue": map[string]any{"id": 9}, "user": map[string]any{"id": 6}}}
	rows, err = redmine.Comments(context.Background(), issue)
	if err != nil || string(rows[0]) != `{"issue":{"id":5},"id":1,"user":{"id":6}}` || string(rows[1]) != `{"id":2,"issue":{"id":9},"user":{"id":6}}` {
		t.Fatalf("placed as %s (%v)", rows, err)
	}
	for name, journals := range map[string][]map[string]any{
		"repeated": {{"id": 2}, {"id": 2}}, "no id": {{"notes": "x"}},
	} {
		fake.journals = journals
		if rows, err := redmine.Comments(context.Background(), issue); err == nil || rows != nil {
			t.Errorf("%s: an unreadable list was returned: %s", name, rows)
		}
	}
	fake.before = func(w http.ResponseWriter, r *http.Request) bool {
		json.NewEncoder(w).Encode(map[string]any{"issue": redmineRecord(t)})
		return true
	}
	if rows, err := redmine.Comments(context.Background(), issue); err == nil {
		t.Fatalf("an issue read without its journals was taken for one without notes: %s", rows)
	}
}

func TestRedmineRefusesAnOversizedRecordWithoutCuttingIt(t *testing.T) {
	fake, redmine := redmineFixture(t)
	fake.before = func(w http.ResponseWriter, r *http.Request) bool {
		record, _ := json.Marshal(map[string]any{"issue": redmineRecord(t)})
		// Valid JSON before the padding would still read if it were cut.
		w.Write(append(record, bytes.Repeat([]byte(" "), 4<<20)...))
		return true
	}
	if text, err := redmine.Request(context.Background(), "5"); err == nil || !strings.Contains(err.Error(), "exceeds 4 MiB") {
		t.Fatalf("an oversized record was read: %d bytes (%v)", len(text), err)
	}
	// An issue with every note on it is read up to the list's bound.
	fake.before = func(w http.ResponseWriter, r *http.Request) bool {
		record, _ := json.Marshal(map[string]any{"issue": map[string]any{"id": 5, "project": map[string]any{"id": 1},
			"journals": []any{map[string]any{"id": 1, "user": map[string]any{"id": 6}, "notes": strings.Repeat("x", 5<<20)}}}})
		w.Write(record)
		return true
	}
	if rows, err := redmine.Comments(context.Background(), Issue{ID: 5, Key: "5"}); err != nil || len(rows) != 1 {
		t.Fatalf("a long journal list was refused: %v", err)
	}
	fake.before = func(w http.ResponseWriter, r *http.Request) bool {
		record, _ := json.Marshal(map[string]any{"issue": map[string]any{"id": 5, "journals": []any{}}})
		w.Write(append(record, bytes.Repeat([]byte(" "), 32<<20)...))
		return true
	}
	if _, err := redmine.Comments(context.Background(), Issue{ID: 5, Key: "5"}); err == nil || !strings.Contains(err.Error(), "exceeds 32 MiB") {
		t.Fatalf("an oversized journal list was read: %v", err)
	}
}

// Redmine lists journals by their time and then their id. A clock set back
// lists a later journal first; the engine reads them in the order Redmine
// stored them, by id, and a stop among them is read. (Fails at b997c63,
// which refused such a list for good.)
func TestRedmineOrdersJournalsByIDWhateverTheirTimes(t *testing.T) {
	fake, redmine := redmineFixture(t)
	fake.journals = []map[string]any{
		{"id": 276, "user": map[string]any{"id": 6}, "notes": "停止", "created_on": "2026-10-20T04:00:00Z"},
		{"id": 274, "user": map[string]any{"id": 5}, "notes": "question", "created_on": "2026-10-20T04:01:00Z"},
		{"id": 275, "user": map[string]any{"id": 6}, "notes": "answer", "created_on": "2026-10-20T04:02:00Z"},
	}
	issue := Issue{ID: 5, Key: "5"}
	rows, err := redmine.Comments(context.Background(), issue)
	if err != nil || len(rows) != 3 {
		t.Fatalf("journals=%d error=%v", len(rows), err)
	}
	var order []int64
	for _, raw := range rows {
		comment, err := redmine.ReadComment(raw, issue)
		if err != nil || !comment.OnIssue {
			t.Fatalf("%s: %+v %v", raw, comment, err)
		}
		order = append(order, comment.ID)
	}
	if fmt.Sprint(order) != "[274 275 276]" {
		t.Fatalf("read in the order %v", order)
	}
}

// An issue closed between two pages does not shift another one out of the
// list: each page asks for the issues after the last one read. (Fails at
// b997c63, which paged by position and lost issue 101 here.)
func TestRedmineListsEveryIssueWhenOneLeavesTheListBetweenPages(t *testing.T) {
	fake, redmine := redmineFixture(t)
	ids := []int{}
	for id := 1; id <= 205; id++ {
		ids = append(ids, id)
	}
	pages := 0
	fake.before = func(w http.ResponseWriter, r *http.Request) bool {
		serveIssueList(t, ids, w, r, nil)
		if pages++; pages == 1 {
			ids = slices.DeleteFunc(ids, func(id int) bool { return id == 50 }) // closed meanwhile
		}
		return true
	}
	rows, err := redmine.Issues(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	listed := map[int64]bool{}
	for _, raw := range rows {
		issue, _ := redmine.ReadIssue(raw)
		listed[issue.ID] = true
	}
	if len(rows) != 205 || !listed[101] || !listed[205] {
		t.Fatalf("issues=%d, 101 listed: %v, pages=%d", len(rows), listed[101], pages)
	}
}

// A request moved to another project stops there: nothing is read from the
// issue and nothing is written to it. (Fails at b997c63, which checked the
// project only when the intake listed the issue.)
func TestRedmineStopsAtAnIssueMovedToAnotherProject(t *testing.T) {
	fake, redmine := redmineFixture(t)
	redmine.Statuses = map[string]int64{Processing: 2}
	fake.issue["project"] = map[string]any{"id": 2, "name": "Elsewhere"}
	issue := Issue{ID: 5, Key: "5"}
	ctx := context.Background()
	_, request := redmine.Request(ctx, "5")
	_, comments := redmine.Comments(ctx, issue)
	_, post := redmine.AddComment(ctx, issue, "a notice")
	edit := redmine.EditComment(ctx, issue, 274, "new words")
	for name, err := range map[string]error{"request": request, "comments": comments, "post": post, "edit": edit,
		"move": redmine.Move(ctx, issue, Processing), "assign": redmine.Assign(ctx, issue, Account{ID: 6})} {
		if err == nil || !strings.Contains(err.Error(), "no longer in the configured project 1") {
			t.Errorf("%s went on in another project: %v", name, err)
		}
	}
	for _, call := range fake.called() {
		if !strings.HasPrefix(call, "GET ") {
			t.Fatalf("a change was sent to an issue in another project: %v", fake.called())
		}
	}
}
