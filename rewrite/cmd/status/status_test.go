package main

import (
	"encoding/json"
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
		`<span class="dots">●◉○</span>`, "fixture requester")
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
	add("8", chain.State{Done: true, History: []chain.Result{{Role: "confirm_report", Speaker: "confirm-process", StartedAt: started, FinishedAt: started.Add(time.Minute)}}}, "")
	add("9", chain.State{Waiting: true, History: []chain.Result{{Role: "ask_requester", Speaker: "ask-process", StartedAt: started, FinishedAt: started.Add(time.Minute)}}}, "")
	add("10", chain.State{History: []chain.Result{{Role: "router", Speaker: "runtime", Error: "routing unavailable: ROUTER-ERROR", StartedAt: started, FinishedAt: started.Add(time.Minute)}}}, "")
	add("11", chain.State{History: []chain.Result{{Role: "implement", Speaker: "implement-process", StartedAt: started, FinishedAt: started.Add(time.Minute)}}},
		`{"notices":[{"kind":"budget-paused","text":"BUDGET-PAUSED-TEXT","written_at":"2026-01-02T00:20:00Z"}]}`)
	add("12", chain.State{History: []chain.Result{{Role: "implement", Speaker: "implement-process", StartedAt: started, FinishedAt: started.Add(time.Minute)}}},
		`{"notices":[{"kind":"resume","text":"RESUMED","written_at":"2026-01-02T00:20:00Z"}]}`)
	ts := serve(t, root, "", "", "")
	_, body := get(t, ts, "/")
	expectAll(t, body, `<h2>Running (2)</h2>`, `<h2>Awaiting answer (1)</h2>`, `<h2>Needs attention (2)</h2>`, `<h2>Delivered (1)</h2>`,
		"ROUTER-ERROR", "BUDGET-PAUSED-TEXT")
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
		"RECEIPT-SHA", "tool terminal completed", `content="10"`, "/files/jobs/7/homes/1-0/logs/agent.log", "the native agent's own log so far")
	response, body = get(t, ts, "/log")
	if response.StatusCode != http.StatusOK || len(body) != len(big) || strings.Contains(body, "not shown") {
		t.Fatalf("/log: %d, %d of %d bytes", response.StatusCode, len(body), len(big))
	}
	if _, body = get(t, ts, "/"); !strings.Contains(body, `content="30"`) {
		t.Error("the overview should reload every 30 seconds")
	}
}
