package tracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Only synthetic tokens and local TLS servers are used; no request leaves the
// machine.
const githubTestToken = "synthetic-github-token"

func githubRecord(base string, number int) map[string]any {
	return map[string]any{"number": number, "id": 9000 + number, "title": "Original title",
		"body": "Keep all conditions.\n{\"unknown\":null} `code` 日本語\n", "created_at": "2026-01-03T00:00:00Z",
		"user": map[string]any{"id": 55, "login": "requester"}, "state": "open",
		"repository_url": base + "/repos/octo-org/widgets", "html_url": "https://github.example/octo-org/widgets/issues/" + strconv.Itoa(number),
		"labels": []any{map[string]any{"name": "Engine"}}, "futureField": map[string]any{"kept": true}}
}

// githubFixture serves handler on a local TLS server and returns the tracker
// pointed at it, with the headers of every request checked.
func githubFixture(t *testing.T, handler func(base string, call int32, w http.ResponseWriter, r *http.Request)) (GitHub, *atomic.Int32) {
	t.Helper()
	t.Setenv("GITHUB_TEST_TOKEN", githubTestToken)
	var calls atomic.Int32
	var base string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+githubTestToken || r.Header.Get("Accept") != "application/vnd.github+json" ||
			r.Header.Get("X-GitHub-Api-Version") != "2022-11-28" || r.Header.Get("User-Agent") == "" {
			t.Errorf("request without the four headers: %v", r.Header)
		}
		if strings.Contains(r.URL.String(), githubTestToken) {
			t.Error("the token is in the address")
		}
		handler(base, call, w, r)
	}))
	t.Cleanup(server.Close)
	base = server.URL
	return GitHub{APIURL: server.URL, Repository: "octo-org/widgets", KeyEnv: "GITHUB_TEST_TOKEN", Client: server.Client()}, &calls
}

// servePages answers a list of records a hundred at a time, naming the next
// page as GitHub does, or not at all when link is false.
func servePages(base string, w http.ResponseWriter, r *http.Request, records []any, link bool) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = max(page, 1)
	from, to := min((page-1)*100, len(records)), min(page*100, len(records))
	if link && to < len(records) {
		w.Header().Set("Link", fmt.Sprintf(`<%s/repositories/1/items?per_page=100&page=%d>; rel="next", <%s/repositories/1/items?per_page=100&page=9>; rel="last"`, base, page+1, base))
	}
	json.NewEncoder(w).Encode(records[from:to])
}

func TestGitHubAsksWithItsHeadersAndKeepsTheTokenOutOfErrors(t *testing.T) {
	github, calls := githubFixture(t, func(base string, call int32, w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			fmt.Fprint(w, `{"id":900,"login":"engine-bot","type":"User"}`)
		case "/repos/octo-org/widgets/issues/7":
			json.NewEncoder(w).Encode(githubRecord(base, 7))
		default:
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, "upstream detail "+githubTestToken)
		}
	})
	ctx := context.Background()
	if me, err := github.Myself(ctx); err != nil || me != (Account{ID: 900, Login: "engine-bot"}) {
		t.Fatalf("myself: %+v (%v)", me, err)
	}
	if text, err := github.Request(ctx, "7"); err != nil || text != "Original issue: 7\nTitle: Original title\n\nKeep all conditions.\n{\"unknown\":null} `code` 日本語\n" {
		t.Fatalf("request: %q (%v)", text, err)
	}
	_, err := github.Request(ctx, "8")
	if err == nil || !strings.Contains(err.Error(), "500: upstream detail [credential]") || strings.Contains(err.Error(), githubTestToken) {
		t.Fatalf("the reason was lost or the token leaked: %v", err)
	}
	for _, key := range []string{"", "0", "-1", "07", "EXAMPLE-1", "1/../2"} {
		if _, err := github.Request(ctx, key); err == nil {
			t.Errorf("%q was read as an issue number", key)
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("calls: %d", calls.Load())
	}
}

func TestGitHubReadsEveryPageOfOpenIssuesAndLeavesPullRequestsOut(t *testing.T) {
	for _, link := range []bool{true, false} {
		t.Run(fmt.Sprintf("link=%v", link), func(t *testing.T) {
			total := 205
			if !link {
				// Without a next page named, a full page is followed by the
				// one after it, which here is empty.
				total = 200
			}
			var records []any
			github, calls := githubFixture(t, func(base string, call int32, w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				if call == 1 && (r.URL.Path != "/repos/octo-org/widgets/issues" || q.Get("state") != "open" || q.Get("sort") != "created" ||
					q.Get("direction") != "asc" || q.Get("per_page") != "100" || q.Get("labels") != "engine") {
					t.Errorf("the first page was asked as %s", r.URL)
				}
				if records == nil {
					for number := 1; number <= total; number++ {
						record := githubRecord(base, number)
						if number%50 == 0 {
							record["pull_request"] = map[string]any{"url": base + "/repos/octo-org/widgets/pulls/" + strconv.Itoa(number)}
						}
						records = append(records, record)
					}
				}
				servePages(base, w, r, records, link)
			})
			github.IntakeLabel = "engine"
			rows, err := github.Issues(context.Background())
			if err != nil || len(rows) != total-total/50 || calls.Load() != 3 {
				t.Fatalf("issues=%d calls=%d error=%v", len(rows), calls.Load(), err)
			}
			for _, raw := range rows {
				issue, err := github.ReadIssue(raw)
				if err != nil || issue.ID%50 == 0 || !github.Marked(issue) {
					t.Fatalf("a pull request, an unreadable or an unmarked record was listed: %s (%v)", raw, err)
				}
			}
		})
	}
}

func TestGitHubReturnsNoListThatIsPartialOrNotTheRepositorys(t *testing.T) {
	// A next page named anywhere the token is not to go fails the list, and so
	// does one the list already gave.
	elsewhere := map[string]func(base string) string{
		"next page on another host": func(string) string { return "https://127.0.0.1:1/repositories/1/issues?page=2" },
		"next page over http": func(base string) string {
			return strings.Replace(base, "https://", "http://", 1) + "/repositories/1/issues?page=2"
		},
		"next page with a user": func(base string) string {
			return strings.Replace(base, "https://", "https://someone:secret@", 1) + "/repositories/1/issues?page=2"
		},
		"next page through dot segments": func(base string) string { return base + "/repos/octo-org/widgets/../../admin?page=2" },
		"next page through encoded dots": func(base string) string { return base + "/repositories/1/%2e%2e/%2E%2E/admin?page=2" },
		"next page already given": func(base string) string {
			return base + "/repos/octo-org/widgets/issues?sort=created&per_page=100&state=open&direction=asc"
		},
	}
	failures := []string{"another repository", "repeated issue", "second page outage", "moved", "not an array", "oversized page"}
	for failure := range elsewhere {
		failures = append(failures, failure)
	}
	for _, failure := range failures {
		t.Run(failure, func(t *testing.T) {
			github, calls := githubFixture(t, func(base string, call int32, w http.ResponseWriter, r *http.Request) {
				records := []any{}
				for number := 1; number <= 101; number++ {
					records = append(records, githubRecord(base, number))
				}
				switch failure {
				case "another repository":
					records[100].(map[string]any)["repository_url"] = base + "/repos/octo-org/gadgets"
				case "repeated issue":
					records[100] = githubRecord(base, 3)
				case "second page outage":
					if call == 2 {
						w.WriteHeader(http.StatusServiceUnavailable)
						fmt.Fprint(w, "second page unavailable")
						return
					}
				case "moved":
					w.Header().Set("Location", base+"/repositories/1/issues")
					w.WriteHeader(http.StatusMovedPermanently)
					return
				case "not an array":
					fmt.Fprint(w, `{"message":"not a list"}`)
					return
				case "oversized page":
					fmt.Fprint(w, "[]"+strings.Repeat(" ", 32<<20))
					return
				}
				if next, found := elsewhere[failure]; found {
					w.Header().Set("Link", "<"+next(base)+`>; rel="next"`)
					json.NewEncoder(w).Encode(records[:100])
					return
				}
				servePages(base, w, r, records, true)
			})
			rows, err := github.Issues(context.Background())
			want := map[string]string{"another repository": "outside the configured repository", "repeated issue": "repeated an issue",
				"second page outage": "503: second page unavailable", "moved": "HTTP 301, a redirect to", "next page already given": "already given",
				"not an array": "not a bounded array", "oversized page": "exceeds 32 MiB"}[failure]
			if want == "" {
				want = "outside its API"
			}
			if failure == "moved" && (err == nil || !strings.Contains(err.Error(), "may have been moved or renamed; check the configured repository")) {
				t.Fatalf("the operator is not told what a redirect likely means: %v", err)
			}
			wantCalls := map[string]int32{"second page outage": 2, "another repository": 2, "repeated issue": 2}[failure]
			if wantCalls == 0 {
				wantCalls = 1
			}
			if err == nil || rows != nil || !strings.Contains(err.Error(), want) || calls.Load() != wantCalls {
				t.Fatalf("a list was returned, or for another reason: rows=%d calls=%d error=%v", len(rows), calls.Load(), err)
			}
		})
	}
}

// A record that names its place but cannot be read for the rest is listed,
// for the intake to leave aside and say so, as a Backlog record is.
func TestGitHubListsARecordItCannotReadForTheIntakeToLeaveAside(t *testing.T) {
	github, _ := githubFixture(t, func(base string, call int32, w http.ResponseWriter, r *http.Request) {
		odd := githubRecord(base, 2)
		odd["created_at"] = "yesterday"
		json.NewEncoder(w).Encode([]any{githubRecord(base, 1), odd})
	})
	rows, err := github.Issues(context.Background())
	if err != nil || len(rows) != 2 {
		t.Fatalf("issues=%d error=%v", len(rows), err)
	}
	if issue, err := github.ReadIssue(rows[1]); err == nil {
		t.Fatalf("a record with an unreadable time was read as %+v", issue)
	}
}

func TestGitHubReadsAnIssueRecordAsTheEngineKeepsIt(t *testing.T) {
	github := GitHub{APIURL: "https://api.github.example/api/v3/", Repository: "Octo-Org/Widgets", KeyEnv: "GITHUB_TEST_TOKEN"}
	if got := github.Identity(); got != "Issue intake: https://api.github.example/api/v3\nRepository: octo-org/widgets" {
		t.Fatalf("identity: %q", got)
	}
	if (GitHub{Repository: "octo-org/widgets"}).Identity() != "Issue intake: https://api.github.com\nRepository: octo-org/widgets" {
		t.Fatal("the default API is not named")
	}
	base := "https://api.github.example/api/v3"
	raw, _ := json.Marshal(githubRecord(base, 12))
	issue, err := github.ReadIssue(raw)
	if err != nil || issue.ID != 12 || issue.Key != "12" || issue.Creator != (Account{ID: 55, Login: "requester"}) ||
		!issue.Created.Equal(time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)) || !bytes.Equal(issue.Raw, raw) {
		t.Fatalf("issue read as %+v (%v)", issue, err)
	}
	if text, err := github.RequestText(raw); err != nil || text != "Original issue: 12\nTitle: Original title\n\nKeep all conditions.\n{\"unknown\":null} `code` 日本語\n" {
		t.Fatalf("request text: %q (%v)", text, err)
	}
	empty := githubRecord(base, 13)
	empty["body"] = nil
	raw, _ = json.Marshal(empty)
	if text, err := github.RequestText(raw); err != nil || text != "Original issue: 13\nTitle: Original title\n\n" {
		t.Fatalf("an issue without a body: %q (%v)", text, err)
	}
	for _, label := range []string{"", "engine", "ENGINE"} {
		github.IntakeLabel = label
		if !github.Marked(issue) {
			t.Errorf("label %q: the issue was not taken up", label)
		}
	}
	github.IntakeLabel = "later"
	if github.Marked(issue) {
		t.Error("an issue without the intake label was taken up")
	}
	for name, change := range map[string]func(map[string]any){
		"a pull request":       func(r map[string]any) { r["pull_request"] = map[string]any{"url": "x"} },
		"another repository":   func(r map[string]any) { r["repository_url"] = base + "/repos/octo-org/gadgets" },
		"no repository":        func(r map[string]any) { delete(r, "repository_url") },
		"no number":            func(r map[string]any) { delete(r, "number") },
		"unreadable time":      func(r map[string]any) { r["created_at"] = "yesterday" },
		"unreadable requester": func(r map[string]any) { r["user"] = "someone" },
	} {
		record := githubRecord(base, 12)
		change(record)
		raw, _ := json.Marshal(record)
		if issue, err := github.ReadIssue(raw); err == nil {
			t.Errorf("%s: read as %+v", name, issue)
		}
	}
	for _, raw := range []string{`{"title":"no number"}`, `not JSON`} {
		if text, err := github.RequestText(json.RawMessage(raw)); err == nil {
			t.Errorf("%s: rendered as %q", raw, text)
		}
	}
}

func TestGitHubReadsCommentsInOrderAndPlacesThemByTheirIssue(t *testing.T) {
	order := "ascending"
	github, calls := githubFixture(t, func(base string, call int32, w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/octo-org/widgets/issues/12/comments" && r.URL.Query().Get("per_page") != "100" {
			t.Errorf("comments asked as %s", r.URL)
		}
		records := []any{}
		for id := 1; id <= 205; id++ {
			records = append(records, map[string]any{"id": id, "body": fmt.Sprintf("comment %d", id), "user": map[string]any{"id": 55, "login": "requester"},
				"issue_url": base + "/repos/octo-org/widgets/issues/12"})
		}
		if order == "descending at a page boundary" {
			records[100], records[99] = records[99], records[100]
		}
		servePages(base, w, r, records, true)
	})
	issue := Issue{ID: 12, Key: "12"}
	rows, err := github.Comments(context.Background(), issue)
	if err != nil || len(rows) != 205 || calls.Load() != 3 {
		t.Fatalf("comments=%d calls=%d error=%v", len(rows), calls.Load(), err)
	}
	comment, err := github.ReadComment(rows[204], issue)
	if err != nil || comment != (Comment{ID: 205, Body: "comment 205", Author: Account{ID: 55, Login: "requester"}, OnIssue: true}) {
		t.Fatalf("comment read as %+v (%v)", comment, err)
	}
	order = "descending at a page boundary"
	if rows, err := github.Comments(context.Background(), issue); err == nil || rows != nil {
		t.Fatalf("an out-of-order list was returned: %d", len(rows))
	}
	base := github.APIURL
	for raw, want := range map[string]Comment{
		`{"id":7,"body":null,"user":null,"issue_url":"` + base + `/repos/octo-org/widgets/issues/12"}`:                         {ID: 7, OnIssue: true},
		`{"id":7,"body":"x","user":{"id":55,"login":"requester"},"issue_url":"` + base + `/repos/OCTO-ORG/widgets/issues/12"}`: {ID: 7, Body: "x", Author: Account{ID: 55, Login: "requester"}, OnIssue: true},
		`{"id":7,"body":"x","issue_url":"` + base + `/repos/octo-org/widgets/issues/13"}`:                                      {ID: 7, Body: "x"},
		`{"id":7,"body":"x","issue_url":"` + base + `/repos/octo-org/gadgets/issues/12"}`:                                      {ID: 7, Body: "x"},
		`{"id":7,"body":"x"}`: {ID: 7, Body: "x"},
	} {
		if comment, err := github.ReadComment(json.RawMessage(raw), issue); err != nil || comment != want {
			t.Errorf("%s: read as %+v (%v)", raw, comment, err)
		}
	}
	for _, raw := range []string{`{"id":0}`, `{"body":"x"}`, `{"id":"7"}`, `{"id":7,"body":42}`, `not JSON`} {
		if comment, err := github.ReadComment(json.RawMessage(raw), issue); err == nil {
			t.Errorf("%s: read as %+v", raw, comment)
		}
	}
	if id, words, err := github.CommentText(json.RawMessage(`{"id":7,"body":"words","user":"odd","issue_url":42}`)); err != nil || id != 7 || words != "words" {
		t.Fatalf("id and words: %d %q (%v)", id, words, err)
	}
}

func TestGitHubRefusesAnOversizedRecordWithoutCuttingIt(t *testing.T) {
	github, _ := githubFixture(t, func(base string, call int32, w http.ResponseWriter, r *http.Request) {
		record, _ := json.Marshal(githubRecord(base, 7))
		// Valid JSON before the padding would still read if it were cut.
		w.Write(append(record, bytes.Repeat([]byte(" "), 4<<20)...))
	})
	if text, err := github.Request(context.Background(), "7"); err == nil || !strings.Contains(err.Error(), "exceeds 4 MiB") {
		t.Fatalf("an oversized record was read: %d bytes (%v)", len(text), err)
	}
}
