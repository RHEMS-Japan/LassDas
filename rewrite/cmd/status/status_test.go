package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ticket-runner/internal/chain"
)

// fixtureQueue lays out one accepted request the way the watch mode does:
// the issue, the request text, the run record, a consumed answer, a notice,
// a review command's findings, a report and one process running right now.
func fixtureQueue(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	job := filepath.Join(root, "jobs", "7")
	for _, dir := range []string{filepath.Join(job, "run"), filepath.Join(job, "live"),
		filepath.Join(job, "homes", "1-0"), filepath.Join(job, "workspace", "report")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(name, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(job, name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("issue.json", `{"id":7,"issueKey":"EXAMPLE-7","summary":"Add a docstring","created":"2026-01-02T00:00:00Z","createdUser":{"id":11,"name":"fixture requester"}}`)
	write("request.txt", "Original issue: EXAMPLE-7\nREQUEST-TEXT")
	write("engine.json", `{"instructions":"CONFIG-TEXT"}`)
	write("answer-3.json", `{"after":"2"}`)
	write("notices.json", `{"notices":[{"kind":"resume","text":"NOTICE-TEXT","written_at":"2026-01-02T01:00:00Z"}]}`)
	write(filepath.Join("homes", "1-0", "review.md"), "REVIEW-FINDING")
	write(filepath.Join("workspace", "report", "result.md"), "REPORT-TEXT")
	started := time.Date(2026, 1, 2, 0, 10, 0, 0, time.UTC)
	state := chain.State{Request: "REQUEST-TEXT", Step: "implement", Pending: &chain.Assignment{Role: "implement"},
		Workflow: &chain.Workflow{Stages: []chain.Stage{{Name: "elicit"}, {Name: "implement"}, {Name: "verify"}}},
		History: []chain.Result{
			{Role: "elicit", Speaker: "elicit-process", Model: "vendor/model", ModelPrefix: "gateway/", Output: "OUTPUT-ELICIT",
				Instruction: "INSTRUCTION-ELICIT", StartedAt: started, FinishedAt: started.Add(time.Minute)},
			{Role: "elicit", Speaker: "runtime", Output: "RUNTIME-NOTE", StartedAt: started.Add(time.Minute), FinishedAt: started.Add(time.Minute)},
			{Role: "ask_requester", Speaker: "requester", Output: "ANSWER-TEXT", StartedAt: started.Add(2 * time.Minute), FinishedAt: started.Add(2 * time.Minute)},
			{Role: "verify", Speaker: "verify-process", Error: "exit status 1", Diagnostics: "DIAG-TEXT",
				StartedAt: started.Add(3 * time.Minute), FinishedAt: started.Add(4 * time.Minute)},
		}}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	write(filepath.Join("run", "history.json"), string(raw))
	write(filepath.Join("live", "implement-implement-process.json"), `{"role":"implement","speaker":"implement-process","model":"vendor/worker","instruction":"LIVE-INSTRUCTION","started_at":"2026-01-02T00:15:00Z"}`)
	write(filepath.Join("live", "implement-implement-process.stdout"), "LIVE-OUT-SO-FAR")
	write(filepath.Join("live", "implement-implement-process.stderr"), "LIVE-ERR-SO-FAR")
	if err := os.WriteFile(filepath.Join(root, "engine.log"), []byte("LOG-LINE-1\nLOG-LINE-2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return root
}

func fixtureConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "operator.json")
	text := `{"backlog":{"base_url":"https://space.example/api/v2"},"intake":{"project_id":17,"issue_ids":[7]},` +
		`"router":{"mode":"stages"},"workflow":{"stages":[{"name":"elicit"},{"name":"implement"},{"name":"verify"}]}}`
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func serve(t *testing.T, root, config, userEnv, passwordEnv string) *httptest.Server {
	t.Helper()
	s, err := newServer(root, config, userEnv, passwordEnv)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.handler())
	t.Cleanup(ts.Close)
	return ts
}

func get(t *testing.T, ts *httptest.Server, path string, credentials ...string) (*http.Response, string) {
	t.Helper()
	request, err := http.NewRequest("GET", ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(credentials) == 2 {
		request.SetBasicAuth(credentials[0], credentials[1])
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response, string(body)
}

func expectAll(t *testing.T, body string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
}

func TestOverviewListsEveryRequestWithItsPosition(t *testing.T) {
	ts := serve(t, fixtureQueue(t), fixtureConfig(t), "", "")
	response, body := get(t, ts, "/")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("%d: %s", response.StatusCode, body)
	}
	expectAll(t, body, "EXAMPLE-7", "Add a docstring", "running implement", "step 2 of 3: implement",
		"LOG-LINE-2", "issue_ids", "project_id", "elicit &rarr; implement &rarr; verify", "&#34;mode&#34;: &#34;stages&#34;",
		`<div class="lane running"><h2>Running (1)</h2>`, `<h2>Awaiting answer (0)</h2>`, `<h2>Needs attention (0)</h2>`, `<h2>Delivered (0)</h2>`,
		`<span class="passed">elicit</span> &rarr; <span class="current">implement</span> &rarr; <span class="ahead">verify</span>`, "fixture requester")
	if strings.Contains(body, "engine.log is not present") {
		t.Error("the log tail was present but reported missing")
	}
}

func TestRequestPageShowsEverythingOnDisk(t *testing.T) {
	ts := serve(t, fixtureQueue(t), fixtureConfig(t), "", "")
	response, body := get(t, ts, "/jobs/7")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("%d: %s", response.StatusCode, body)
	}
	expectAll(t, body, "REQUEST-TEXT", "OUTPUT-ELICIT", "INSTRUCTION-ELICIT", "RUNTIME-NOTE", "ANSWER-TEXT",
		"exit status 1", "DIAG-TEXT", "gateway/vendor/model", "Running now: implement", "vendor/worker",
		"LIVE-INSTRUCTION", "LIVE-OUT-SO-FAR", "LIVE-ERR-SO-FAR", "NOTICE-TEXT", "REVIEW-FINDING", "REPORT-TEXT",
		"answer-3.json", "https://space.example/view/EXAMPLE-7", "fixture requester", "<b>implement</b>",
		"/jobs/7/raw/history.json", "no checkout yet")
}

func TestRawFilesAreServedAndNothingElse(t *testing.T) {
	ts := serve(t, fixtureQueue(t), "", "", "")
	for name, want := range map[string]string{"history.json": "OUTPUT-ELICIT", "engine.json": "CONFIG-TEXT",
		"issue.json": "EXAMPLE-7", "request.txt": "REQUEST-TEXT", "answer-3.json": `"after"`, "notices.json": "NOTICE-TEXT"} {
		response, body := get(t, ts, "/jobs/7/raw/"+name)
		if response.StatusCode != http.StatusOK || !strings.Contains(body, want) {
			t.Errorf("%s: %d %q", name, response.StatusCode, body)
		}
		if !strings.HasPrefix(response.Header.Get("Content-Type"), "text/plain") {
			t.Errorf("%s served as %s", name, response.Header.Get("Content-Type"))
		}
	}
	for _, path := range []string{"/jobs/7/raw/passwd", "/jobs/7/raw/answer-x.json", "/jobs/7/raw/..%2Fissue.json",
		"/jobs/abc", "/jobs/8", "/jobs/7/raw/history.json/..", "/jobs/../etc"} {
		if response, body := get(t, ts, path); response.StatusCode == http.StatusOK {
			t.Errorf("%s answered 200: %s", path, body)
		}
	}
}

func TestDamagedRecordsAreReportedNotHidden(t *testing.T) {
	root := fixtureQueue(t)
	if err := os.WriteFile(filepath.Join(root, "jobs", "7", "run", "history.json"), []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	ts := serve(t, root, "", "", "")
	response, body := get(t, ts, "/jobs/7")
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "history.json could not be decoded") {
		t.Fatalf("%d: %s", response.StatusCode, body)
	}
	expectAll(t, body, "REQUEST-TEXT", "LIVE-OUT-SO-FAR")
	response, body = get(t, ts, "/")
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "no run record yet") {
		t.Fatalf("%d: %s", response.StatusCode, body)
	}
	expectAll(t, body, `<h2>Needs attention (1)</h2>`, "history.json could not be decoded")
}

func TestTheBoardPutsEachRequestInItsLane(t *testing.T) {
	root := fixtureQueue(t)
	started := time.Date(2026, 1, 2, 0, 10, 0, 0, time.UTC)
	add := func(id string, state chain.State, notices string) {
		t.Helper()
		dir := filepath.Join(root, "jobs", id)
		if err := os.MkdirAll(filepath.Join(dir, "run"), 0700); err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		for name, text := range map[string]string{"issue.json": `{"id":` + id + `,"issueKey":"EXAMPLE-` + id + `","summary":"request ` + id + `"}`,
			filepath.Join("run", "history.json"): string(raw), "notices.json": notices} {
			if text == "" {
				continue
			}
			if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	add("8", chain.State{Done: true, Step: "confirm_report", Workflow: &chain.Workflow{Stages: []chain.Stage{{Name: "elicit"}, {Name: "confirm_report"}}},
		History: []chain.Result{{Role: "confirm_report", Speaker: "confirm-process", StartedAt: started, FinishedAt: started.Add(time.Minute)}}}, "")
	add("9", chain.State{Waiting: true, History: []chain.Result{{Role: "ask_requester", Speaker: "ask-process", StartedAt: started, FinishedAt: started.Add(time.Minute)}}}, "")
	add("10", chain.State{History: []chain.Result{{Role: "router", Speaker: "runtime", Error: "routing unavailable: ROUTER-ERROR", StartedAt: started, FinishedAt: started.Add(time.Minute)}}}, "")
	add("11", chain.State{History: []chain.Result{{Role: "implement", Speaker: "implement-process", StartedAt: started, FinishedAt: started.Add(time.Minute)}}},
		`{"notices":[{"kind":"budget-paused","text":"BUDGET-PAUSED-TEXT","written_at":"2026-01-02T00:20:00Z"}]}`)
	add("12", chain.State{History: []chain.Result{{Role: "implement", Speaker: "implement-process", StartedAt: started, FinishedAt: started.Add(time.Minute)}}},
		`{"notices":[{"kind":"resume","text":"RESUMED","written_at":"2026-01-02T00:20:00Z"}]}`)
	ts := serve(t, root, "", "", "")
	_, body := get(t, ts, "/")
	expectAll(t, body, `<h2>Running (2)</h2>`, `<h2>Awaiting answer (1)</h2>`, `<h2>Needs attention (2)</h2>`, `<h2>Delivered (1)</h2>`,
		"ROUTER-ERROR", "BUDGET-PAUSED-TEXT", `<span class="passed">elicit</span> &rarr; <span class="passed">confirm_report</span>`)
	for _, section := range []struct{ lane, key string }{{"delivered", "EXAMPLE-8"}, {"awaiting", "EXAMPLE-9"}, {"attention", "EXAMPLE-10"}, {"attention", "EXAMPLE-11"}, {"running", "EXAMPLE-12"}, {"running", "EXAMPLE-7"}} {
		start := strings.Index(body, `<div class="lane `+section.lane+`">`)
		end := strings.Index(body[start+1:], `<div class="lane `)
		if end < 0 {
			end = len(body) - start - 1
		}
		if !strings.Contains(body[start:start+1+end], section.key) {
			t.Errorf("%s is not in the %s lane", section.key, section.lane)
		}
	}
}

func TestBasicAuthenticationGuardsEveryPageButTheHealthCheck(t *testing.T) {
	t.Setenv("STATUS_USER", "operator")
	t.Setenv("STATUS_PASSWORD", "a-long-password")
	ts := serve(t, fixtureQueue(t), "", "STATUS_USER", "STATUS_PASSWORD")
	for _, path := range []string{"/", "/jobs/7", "/jobs/7/raw/history.json", "/jobs/7/workspace", "/config", "/log"} {
		response, _ := get(t, ts, path)
		if response.StatusCode != http.StatusUnauthorized || response.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("%s without credentials: %d", path, response.StatusCode)
		}
		if response, _ := get(t, ts, path, "operator", "wrong"); response.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s with a wrong password: %d", path, response.StatusCode)
		}
		if response, _ := get(t, ts, path, "operator", "a-long-password"); response.StatusCode != http.StatusOK {
			t.Errorf("%s with the right credentials: %d", path, response.StatusCode)
		}
	}
	if response, _ := get(t, ts, "/healthz"); response.StatusCode != http.StatusOK {
		t.Errorf("health check needs credentials: %d", response.StatusCode)
	}
	if _, err := newServer(t.TempDir(), "", "STATUS_MISSING_USER", "STATUS_PASSWORD"); err == nil {
		t.Error("an empty credential variable started the server without authentication")
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.invalid"}, args...)...)
	command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull)
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

func TestWorkspaceChangesAreShownWithoutWritingToTheCheckout(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := fixtureQueue(t)
	workspace := filepath.Join(root, "jobs", "7", "workspace")
	git(t, workspace, "init", "-q")
	if err := os.WriteFile(filepath.Join(workspace, "tracked.txt"), []byte("old line\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git(t, workspace, "add", "tracked.txt")
	git(t, workspace, "commit", "-q", "-m", "base")
	if err := os.WriteFile(filepath.Join(workspace, "tracked.txt"), []byte("new line\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "added.txt"), []byte("NEW-CONTENT\n"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(filepath.Join(workspace, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	ts := serve(t, root, "", "", "")
	response, body := get(t, ts, "/jobs/7/workspace")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("%d: %s", response.StatusCode, body)
	}
	expectAll(t, body, " M tracked.txt", "?? added.txt", "-old line", "+new line", "== untracked: added.txt", "NEW-CONTENT")
	_, body = get(t, ts, "/jobs/7")
	expectAll(t, body, "new file: added.txt", "NEW-CONTENT", "&#43;new line") // html/template writes + as &#43;
	after, err := os.Stat(filepath.Join(workspace, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("reading the workspace rewrote the checkout's index")
	}
	if _, err := os.Stat(filepath.Join(workspace, ".git", "index.lock")); err == nil {
		t.Error("reading the workspace left an index lock behind")
	}
}

func TestIssueLinksComeFromTheTrackerBase(t *testing.T) {
	for base, want := range map[string]string{"https://space.example/api/v2": "https://space.example/view/",
		"https://space.example/api/v2/": "https://space.example/view/", "": ""} {
		if got := issueLinkBase(base); got != want {
			t.Errorf("%q -> %q, want %q", base, got, want)
		}
	}
}

func TestEveryFileOfTheQueueIsReachableAndNothingOutsideIt(t *testing.T) {
	root := fixtureQueue(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("OUTSIDE-SECRET"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "jobs", "7", "escape")); err != nil {
		t.Fatal(err)
	}
	ts := serve(t, root, "", "", "")
	response, body := get(t, ts, "/files/")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("%d: %s", response.StatusCode, body)
	}
	expectAll(t, body, `href="/files/jobs/"`, "engine.log")
	_, body = get(t, ts, "/files/jobs/7/")
	expectAll(t, body, `href="/files/jobs/7/issue.json"`, `href="/files/jobs/7/run/"`, `href="/files/jobs/7/homes/"`, `href="/files/jobs/7/../"`)
	response, body = get(t, ts, "/files/jobs/7/run/history.json")
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "OUTPUT-ELICIT") ||
		!strings.HasPrefix(response.Header.Get("Content-Type"), "text/plain") {
		t.Fatalf("%d %s: %s", response.StatusCode, response.Header.Get("Content-Type"), body)
	}
	if _, body = get(t, ts, "/files/jobs/7/homes/1-0/review.md"); !strings.Contains(body, "REVIEW-FINDING") {
		t.Fatalf("home file: %s", body)
	}
	if _, body = get(t, ts, "/files"); !strings.Contains(body, `href="/files/jobs/"`) {
		t.Fatalf("/files did not lead to the listing: %s", body)
	}
	for _, path := range []string{"/files/jobs/7/escape/secret.txt", "/files/jobs/7/escape/", "/files/../etc/passwd", "/files/..%2F..%2Fetc%2Fpasswd"} {
		response, body := get(t, ts, path)
		if response.StatusCode == http.StatusOK || strings.Contains(body, "OUTSIDE-SECRET") || strings.Contains(body, "root:") {
			t.Errorf("%s answered %d: %.80s", path, response.StatusCode, body)
		}
	}
}

func TestReviewViewsShowTimeByStageGapsUsageAndTranscript(t *testing.T) {
	root := fixtureQueue(t)
	job := filepath.Join(root, "jobs", "7")
	raw, err := os.ReadFile(filepath.Join(job, "run", "history.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state chain.State
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	state.Pending.Instruction = "PENDING-INSTRUCTION"
	if raw, err = json.Marshal(state); err != nil {
		t.Fatal(err)
	}
	write := func(name, text string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(job, name)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(job, name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join("run", "history.json"), string(raw))
	write(filepath.Join("homes", "1-0", "logs", "agent.log"),
		"2026-01-02 00:15:01 INFO agent.conversation_loop: API call #1: model=x provider=y in=100 out=20 total=120 latency=1.0s\n"+
			"2026-01-02 00:15:05 INFO agent.tool_executor: tool terminal completed (0.1s, 50 chars)\n"+
			"2026-01-02 00:15:09 INFO agent.conversation_loop: API call #2: model=x provider=y in=250 out=30 total=280 latency=2.0s\n")
	write(filepath.Join("homes", "1-0", "transcript.json"), `[{"role":"assistant","content":"TRANSCRIPT-TEXT"}]`)
	write(filepath.Join("workspace", ".git", "ticket-engine", "delivery.json"), `{"pull_request": 41, "merge_sha": "RECEIPT-SHA"}`)
	write(filepath.Join("live", "implement-implement-process.json"),
		`{"role":"implement","speaker":"implement-process","model":"vendor/worker","instruction":"LIVE-INSTRUCTION","started_at":"2026-01-02T00:15:00Z","home":"`+filepath.Join(job, "homes", "1-0")+`"}`)
	big := strings.Repeat("LOG-LINE\n", 40000)
	if err := os.WriteFile(filepath.Join(root, "engine.log"), []byte(big), 0600); err != nil {
		t.Fatal(err)
	}
	ts := serve(t, root, "", "", "")
	response, body := get(t, ts, "/jobs/7")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("%d: %s", response.StatusCode, body)
	}
	expectAll(t, body, "PENDING-INSTRUCTION", "Time by stage",
		"<td>elicit</td><td>1</td><td>0</td><td>1m00s</td>", "<td>verify</td><td>1</td><td>1</td><td>1m00s</td>", "<td>implement</td><td>0</td>",
		"1m00s after the previous record", "2 model calls, 350 input tokens, 50 output tokens", "TRANSCRIPT-TEXT",
		"RECEIPT-SHA", "tool terminal completed", `content="10"`, "/files/jobs/7/homes/1-0/logs/agent.log", "the native agent&#39;s own log so far")
	response, body = get(t, ts, "/log")
	if response.StatusCode != http.StatusOK || len(body) != len(big) || strings.Contains(body, "not shown") {
		t.Fatalf("/log: %d, %d of %d bytes", response.StatusCode, len(body), len(big))
	}
	if _, body = get(t, ts, "/"); !strings.Contains(body, `content="30"`) {
		t.Error("the overview should reload every 30 seconds")
	}
}

func TestARolesOwnDirectoryCannotLeadThePageOutsideTheQueue(t *testing.T) {
	root := fixtureQueue(t)
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "logs"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{filepath.Join("logs", "agent.log"): "OUTSIDE-AGENT-LOG", "secret.txt": "OUTSIDE-SECRET"} {
		if err := os.WriteFile(filepath.Join(outside, name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	home := filepath.Join(root, "jobs", "7", "homes", "2-0")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "logs"), filepath.Join(home, "logs")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(home, "transcript.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "jobs", "7", "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "jobs", "7", "live", "implement-implement-process.json"),
		[]byte(`{"role":"implement","speaker":"implement-process","started_at":"2026-01-02T00:15:00Z","home":"`+filepath.Join(root, "jobs", "7", "escape")+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	ts := serve(t, root, "", "", "")
	response, body := get(t, ts, "/jobs/7")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("%d: %s", response.StatusCode, body)
	}
	for _, leaked := range []string{"OUTSIDE-AGENT-LOG", "OUTSIDE-SECRET"} {
		if strings.Contains(body, leaked) {
			t.Errorf("the page showed %s from outside the queue", leaked)
		}
	}
	_, listing := get(t, ts, "/files/jobs/7/")
	if !strings.Contains(listing, "symbolic link; not followed") || strings.Contains(listing, `href="/files/jobs/7/escape/"`) {
		t.Errorf("a symbolic link should be named and not linked: %s", listing)
	}
	if response, body := get(t, ts, "/files/jobs/7/homes/2-0/transcript.json"); response.StatusCode == http.StatusOK || strings.Contains(body, "OUTSIDE") {
		t.Errorf("a file symbolic link was followed: %d %s", response.StatusCode, body)
	}
	if strings.Contains(body, `href="/files/jobs/7/homes/2-0/transcript.json"`) || strings.Contains(body, `href="/files/jobs/7/homes/2-0/logs`) {
		t.Error("a symbolic link inside a home was offered as a link")
	}
	if !strings.Contains(body, "transcript.json <span class=\"meta\">(symbolic link") {
		t.Error("a symbolic link inside a home should be named as one")
	}
}

func TestListingLinksSurviveAwkwardFileNames(t *testing.T) {
	root := fixtureQueue(t)
	for name, text := range map[string]string{"a#b.txt": "HASH-NAME", "pct%41.txt": "PERCENT-NAME", "<x>&y.txt": "ANGLE-NAME"} {
		if err := os.WriteFile(filepath.Join(root, "jobs", "7", name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ts := serve(t, root, "", "", "")
	_, listing := get(t, ts, "/files/jobs/7/")
	for _, href := range []string{`href="/files/jobs/7/a%23b.txt"`, `href="/files/jobs/7/pct%2541.txt"`, `href="/files/jobs/7/%3Cx%3E&amp;y.txt"`} {
		if !strings.Contains(listing, href) {
			t.Errorf("listing lacks %s", href)
		}
	}
	if strings.Contains(listing, "<x>&y.txt") {
		t.Error("a file name reached the page unescaped")
	}
	for path, want := range map[string]string{"/files/jobs/7/a%23b.txt": "HASH-NAME", "/files/jobs/7/pct%2541.txt": "PERCENT-NAME", "/files/jobs/7/%3Cx%3E&y.txt": "ANGLE-NAME"} {
		if response, body := get(t, ts, path); response.StatusCode != http.StatusOK || body != want {
			t.Errorf("%s: %d %q", path, response.StatusCode, body)
		}
	}
}

func TestUsageCountsNeedMatchingLines(t *testing.T) {
	if calls, in, out := agentUsage("nothing about calls here\n"); calls != 0 || in != 0 || out != 0 {
		t.Fatalf("%d %d %d", calls, in, out)
	}
	if calls, in, out := agentUsage("x API call #3: model=m in=5 out=7 total=12\nx API call #4: model=m in=1 out=1 total=2\n"); calls != 2 || in != 6 || out != 8 {
		t.Fatalf("%d %d %d", calls, in, out)
	}
}

func TestLabelsSwitchToJapaneseAndBack(t *testing.T) {
	ts := serve(t, fixtureQueue(t), fixtureConfig(t), "", "")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, _ := http.NewRequest("GET", ts.URL+"/lang/ja", nil)
	request.Header.Set("Referer", ts.URL+"/jobs/7")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "/jobs/7" {
		t.Fatalf("%d -> %q", response.StatusCode, response.Header.Get("Location"))
	}
	var cookie *http.Cookie
	for _, c := range response.Cookies() {
		if c.Name == "lang" && c.Value == "ja" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no lang cookie was set")
	}
	fetch := func(path string) string {
		request, _ := http.NewRequest("GET", ts.URL+path, nil)
		request.AddCookie(cookie)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		return string(body)
	}
	expectAll(t, fetch("/"), "<h1>依頼</h1>", "<h2>実行中 (1)</h2>", "<h2>返事待ち (0)</h2>", "<h2>要対応 (0)</h2>", "<h2>納品済み (0)</h2>",
		"実行中: implement", "工程 2/3: implement", "受付の設定", `<a href="/lang/en">English</a>`, " 前")
	expectAll(t, fetch("/jobs/7"), "工程別の時間", "依頼の原文", "記録 (4 件)", "実行中: implement", "渡した指示", "<title>EXAMPLE-7 状態</title>")
	expectAll(t, fetch("/files/jobs/7/"), "<th>名前</th>")
	if body := fetch("/jobs/7"); strings.Contains(body, "OUTPUT-ELICIT") == false || strings.Contains(body, "REQUEST-TEXT") == false {
		t.Error("the content must stay as it is in Japanese")
	}
	request, _ = http.NewRequest("GET", ts.URL+"/lang/en", nil)
	request.Header.Set("Referer", "https://evil.example/somewhere")
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.Header.Get("Location") != "/somewhere" && response.Header.Get("Location") != "/" {
		t.Fatalf("a foreign referer must not become an open redirect: %q", response.Header.Get("Location"))
	}
	for _, referer := range []string{`/\\evil.example`, `/\\/evil.example`, `https://evil.example/\\@evil`, "//evil.example", "https://evil.example//foo"} {
		request, _ = http.NewRequest("GET", ts.URL+"/lang/en", nil)
		request.Header.Set("Referer", referer)
		response, err = client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.Header.Get("Location") != "/" {
			t.Errorf("referer %q led to %q", referer, response.Header.Get("Location"))
		}
	}
	if response, _ := get(t, ts, "/lang/xx"); response.StatusCode != http.StatusNotFound {
		t.Errorf("an unknown language answered %d", response.StatusCode)
	}
	if body := fetch("/jobs/7"); !strings.Contains(body, "工程別の時間") {
		t.Error("the cookie must keep the choice")
	}
	if _, body := get(t, ts, "/"); !strings.Contains(body, "<h1>Requests</h1>") || !strings.Contains(body, `<a href="/lang/ja">日本語</a>`) {
		t.Error("without the cookie the labels are English with a link to Japanese")
	}
}

func TestTheWorkspaceViewRunsNothingFromTheCheckoutAndStaysInsideTheQueue(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("STATUS_USER", "operator")
	t.Setenv("STATUS_PASSWORD", "the-status-password")
	root := fixtureQueue(t)
	workspace := filepath.Join(root, "jobs", "7", "workspace")
	git(t, workspace, "init", "-q")
	if err := os.WriteFile(filepath.Join(workspace, "tracked.txt"), []byte("old line\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git(t, workspace, "add", "tracked.txt")
	git(t, workspace, "commit", "-q", "-m", "base")
	if err := os.WriteFile(filepath.Join(workspace, "tracked.txt"), []byte("new line\n"), 0600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "executed")
	script := filepath.Join(t.TempDir(), "driver.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'EXECUTED %s' \"$STATUS_PASSWORD\" > "+marker+"\ncat \"$2\" 2>/dev/null; exit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(workspace, ".git", "config")
	extra := "\n[diff]\n\texternal = " + script + "\n[diff \"marked\"]\n\ttextconv = " + script + "\n"
	raw, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, append(raw, extra...), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".gitattributes"), []byte("*.txt diff=marked\n"), 0600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("OUTSIDE-SECRET"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "escape.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "staged.txt"), []byte("STAGED-CONTENT\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git(t, workspace, "add", "staged.txt")
	if err := os.WriteFile(filepath.Join(workspace, "caf\u00e9.txt"), []byte("ACCENT-CONTENT\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 60; i++ {
		if err := os.WriteFile(filepath.Join(workspace, fmt.Sprintf("many-%02d.txt", i)), []byte("MANY-CONTENT\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ts := serve(t, root, "", "STATUS_USER", "STATUS_PASSWORD")
	for _, path := range []string{"/jobs/7/workspace", "/jobs/7"} {
		response, body := get(t, ts, path, "operator", "the-status-password")
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d", path, response.StatusCode)
		}
		if response.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s served without nosniff", path)
		}
		if strings.Contains(body, "OUTSIDE-SECRET") {
			t.Errorf("%s followed a symbolic link out of the queue", path)
		}
		if strings.Contains(body, "the-status-password") {
			t.Errorf("%s leaked the page's password", path)
		}
		expectAll(t, body, "STAGED-CONTENT", "ACCENT-CONTENT", "escape.txt", "symbolic link")
		if strings.Count(body, "MANY-CONTENT") > 50 {
			t.Errorf("%s showed %d new files inline; at most 50", path, strings.Count(body, "MANY-CONTENT"))
		}
		if !strings.Contains(body, "more new files are not shown") {
			t.Errorf("%s did not say how many new files were left out", path)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a program named by the checkout's configuration ran")
	}
	big := make([]byte, 3<<20)
	copy(big, "HEAD-OF-BIG-FILE ")
	copy(big[len(big)-16:], "TAIL-OF-BIG-FILE")
	if err := os.WriteFile(filepath.Join(workspace, "big.bin"), big, 0600); err != nil {
		t.Fatal(err)
	}
	_, text := get(t, ts, "/jobs/7/workspace", "operator", "the-status-password")
	if !strings.Contains(text, "HEAD-OF-BIG-FILE") || strings.Contains(text, "TAIL-OF-BIG-FILE") || !strings.Contains(text, fmt.Sprintf("[cut here: %d more bytes on disk]", 3<<20-untrackedLimit)) {
		t.Error("a large new file must show its head, be cut at the limit and say how much lies beyond")
	}
	_, body := get(t, ts, "/jobs/7", "operator", "the-status-password")
	if !strings.Contains(body, "-old line") || !strings.Contains(body, "&#43;new line") {
		t.Error("the tracked change is missing from the diff")
	}
}

func TestARequesterRecordIsStyledAsAPerson(t *testing.T) {
	ts := serve(t, fixtureQueue(t), "", "", "")
	_, body := get(t, ts, "/jobs/7")
	if !strings.Contains(body, `class="rec person"`) {
		t.Error("the requester's answer is not styled as a person's record")
	}
}

func TestAResumeAfterARestartIsRunningNotAttentionAndKeepsItsStart(t *testing.T) {
	root := fixtureQueue(t)
	started := time.Date(2026, 1, 2, 0, 10, 0, 0, time.UTC)
	state := chain.State{Step: "implement", Recovering: true, Pending: &chain.Assignment{Role: "implement"},
		Workflow: &chain.Workflow{Stages: []chain.Stage{{Name: "elicit"}, {Name: "implement"}}},
		History: []chain.Result{
			{Role: "elicit", Speaker: "elicit-process", StartedAt: started, FinishedAt: started.Add(2 * time.Minute)},
			{Role: "implement", Speaker: "implement-process", Error: "context canceled", StartedAt: started.Add(2 * time.Minute), FinishedAt: started.Add(5 * time.Minute)},
			{Role: "implement", Speaker: "runtime", Error: "The process stopped while this action was pending. Available reports may be partial.", FinishedAt: started.Add(5 * time.Minute)},
		}}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "jobs", "7")
	if err := os.WriteFile(filepath.Join(dir, "run", "history.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(dir, "live")); err != nil {
		t.Fatal(err)
	}
	ts := serve(t, root, "", "", "")
	_, body := get(t, ts, "/")
	expectAll(t, body, `<h2>Running (1)</h2>`, `<h2>Needs attention (0)</h2>`, "recovering after a restart; assigned to implement")
	if strings.Contains(body, "elapsed 0s") || !strings.Contains(body, "elapsed ") {
		t.Errorf("the elapsed time must count from the first record: %s", body[strings.Index(body, "elapsed"):][:40])
	}
	_, page := get(t, ts, "/jobs/7")
	if want := started.In(time.Local).Format("2006-01-02 15:04:05"); !strings.Contains(page, want) {
		t.Errorf("the request page must show the first record's start (%s) as the start", want)
	}
	expectAll(t, body, `<span class="passed">elicit</span> &rarr; <span class="current">implement</span>`)
}

func TestARolesFileListIsCapped(t *testing.T) {
	root := fixtureQueue(t)
	cache := filepath.Join(root, "jobs", "7", "homes", "1-0", "cache")
	if err := os.MkdirAll(cache, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 250; i++ {
		if err := os.WriteFile(filepath.Join(cache, fmt.Sprintf("entry-%03d", i)), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ts := serve(t, root, "", "", "")
	_, body := get(t, ts, "/jobs/7")
	if strings.Count(body, "/files/jobs/7/homes/1-0/cache/entry-") > homeFileLimit {
		t.Errorf("%d file links; at most %d", strings.Count(body, "/files/jobs/7/homes/1-0/cache/entry-"), homeFileLimit)
	}
	if !strings.Contains(body, "more under files") {
		t.Error("the page must say that more files are under the browser")
	}
}
