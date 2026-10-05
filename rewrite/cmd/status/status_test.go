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

// inColumn returns the board column of one stage.
func inColumn(body, stage string) string {
	start := strings.Index(body, `<section class="col`)
	for start >= 0 {
		end := strings.Index(body[start+1:], `<section class="col`)
		var section string
		if end < 0 {
			section = body[start:]
		} else {
			section = body[start : start+1+end]
		}
		if strings.Contains(section, `data-stage="`+stage+`"`) {
			return section
		}
		if end < 0 {
			break
		}
		start = start + 1 + end
	}
	return ""
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
		"LOG-LINE-2", "issue_ids", "project_id", `<span class="chip">elicit<small>elicit</small></span>`, "&#34;mode&#34;: &#34;stages&#34;",
		`Running <b>1</b>`, `Awaiting answer <b>0</b>`, `Needs attention <b>0</b>`, `Delivered <b>0</b>`, "fixture requester")
	if !strings.Contains(inColumn(body, "implement"), `data-key="EXAMPLE-7"`) {
		t.Error("the running request must sit in the column of its stage")
	}
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
		"answer-3.json", "https://space.example/view/EXAMPLE-7", "fixture requester", `<span class="chip current">implement<small>implement</small></span>`,
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
	expectAll(t, body, `Needs attention <b>1</b>`, "history.json could not be decoded")
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
	// A pause the runtime has since lifted is over; a pause after that holds again; a no-progress line outlives the budget's return.
	add("13", chain.State{History: []chain.Result{{Role: "implement", Speaker: "implement-process", StartedAt: started, FinishedAt: started.Add(time.Minute)}}},
		`{"notices":[{"kind":"budget-paused","text":"OLD-PAUSE-TEXT","written_at":"2026-01-02T00:20:00Z"},{"kind":"budget-restored","text":"BUDGET-BACK-TEXT","written_at":"2026-01-02T00:25:00Z"}]}`)
	add("14", chain.State{History: []chain.Result{{Role: "implement", Speaker: "implement-process", StartedAt: started, FinishedAt: started.Add(time.Minute)}}},
		`{"notices":[{"kind":"budget-paused","text":"FIRST-PAUSE","written_at":"2026-01-02T00:20:00Z"},{"kind":"budget-restored","text":"BACK","written_at":"2026-01-02T00:25:00Z"},{"kind":"budget-paused","text":"SECOND-PAUSE-TEXT","written_at":"2026-01-02T00:30:00Z"}]}`)
	add("15", chain.State{History: []chain.Result{{Role: "implement", Speaker: "implement-process", StartedAt: started, FinishedAt: started.Add(time.Minute)}}},
		`{"notices":[{"kind":"stall","text":"STALL-TEXT","written_at":"2026-01-02T00:20:00Z"},{"kind":"budget-restored","text":"BACK","written_at":"2026-01-02T00:25:00Z"}]}`)
	// A question to the requester is a side step of the stage that asked it: the card keeps that stage's column and place.
	add("16", chain.State{Waiting: true, Step: "ask_requester", Workflow: &chain.Workflow{Stages: []chain.Stage{{Name: "elicit"}, {Name: "implement"}}},
		History: []chain.Result{{Role: "elicit", Speaker: "elicit-process", StartedAt: started, FinishedAt: started.Add(time.Minute)},
			{Role: "ask_requester", Speaker: "ask-process", StartedAt: started.Add(time.Minute), FinishedAt: started.Add(2 * time.Minute)}}}, "")
	ts := serve(t, root, "", "", "")
	_, body := get(t, ts, "/")
	expectAll(t, body, `Running <b>3</b>`, `Awaiting answer <b>2</b>`, `Needs attention <b>4</b>`, `Delivered <b>1</b>`,
		"ROUTER-ERROR", "BUDGET-PAUSED-TEXT", "SECOND-PAUSE-TEXT", "STALL-TEXT")
	if column := inColumn(body, "elicit"); !strings.Contains(column, `data-key="EXAMPLE-16"`) || !strings.Contains(column, "step 1 of 2: elicit") {
		t.Error("a request waiting on a question is not placed at the stage that asked it")
	}
	if _, page := get(t, ts, "/jobs/16"); strings.Count(page, `class="badge awaiting"`) != 1 {
		t.Errorf("the waiting badge is shown %d times on the job page", strings.Count(page, `class="badge awaiting"`))
	}
	if strings.Contains(body, "OLD-PAUSE-TEXT") {
		t.Error("a pause the runtime has lifted is still shown on the card")
	}
	for _, want := range []struct{ lane, key string }{{"delivered", "EXAMPLE-8"}, {"awaiting", "EXAMPLE-9"}, {"attention", "EXAMPLE-10"}, {"attention", "EXAMPLE-11"}, {"running", "EXAMPLE-12"}, {"running", "EXAMPLE-7"},
		{"running", "EXAMPLE-13"}, {"attention", "EXAMPLE-14"}, {"attention", "EXAMPLE-15"}, {"awaiting", "EXAMPLE-16"}} {
		if !strings.Contains(body, `<article class="card `+want.lane+`" data-key="`+want.key+`">`) {
			t.Errorf("%s is not a %s card", want.key, want.lane)
		}
	}
	if !strings.Contains(inColumn(body, "done"), `data-key="EXAMPLE-8"`) {
		t.Error("a delivered request must sit in the last column")
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
		`<td>elicit <small class="meta">elicit</small></td><td>1</td><td>0</td><td>1m00s</td>`, `<td>verify <small class="meta">verify</small></td><td>1</td><td>1</td><td>1m00s</td>`, `<td>implement <small class="meta">implement</small></td><td>0</td>`,
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
	expectAll(t, fetch("/"), "<h1>依頼</h1>", "実行中 <b>1</b>", "返事待ち <b>0</b>", "要対応 <b>0</b>", "納品済み <b>0</b>",
		"実行中: 実装", "工程 2/3: 実装", "受付の設定", `<a href="/lang/en">English</a>`, " 前", "要件確定<small>elicit</small>")
	expectAll(t, fetch("/jobs/7"), "工程別の時間", "依頼の原文", "記録 (4 件)", "実行中: 実装", "渡した指示", "<title>EXAMPLE-7 状態</title>", "<b>検証</b>")
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
	expectAll(t, body, `Running <b>1</b>`, `Needs attention <b>0</b>`, "taking up an interrupted step; assigned to implement")
	if strings.Contains(body, "last failure") {
		t.Error("an interrupted step is shown as a failure being retried")
	}
	if strings.Contains(body, "elapsed 0s") || !strings.Contains(body, "elapsed ") {
		t.Errorf("the elapsed time must count from the first record: %s", body[strings.Index(body, "elapsed"):][:40])
	}
	_, page := get(t, ts, "/jobs/7")
	if want := started.In(time.Local).Format("2006-01-02 15:04:05"); !strings.Contains(page, want) {
		t.Errorf("the request page must show the first record's start (%s) as the start", want)
	}
	expectAll(t, page, `<span class="chip passed">elicit<small>elicit</small></span>`, `<span class="chip current">implement<small>implement</small></span>`)
}

func TestTheCardNamesTheStageRunningNowNotTheLastRecorded(t *testing.T) {
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
	state.Step = "elicit" // the record's step moves only after the launch returns
	if raw, err = json.Marshal(state); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(job, "run", "history.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	ts := serve(t, root, "", "", "")
	_, body := get(t, ts, "/")
	expectAll(t, body, "step 2 of 3: implement")
	if strings.Contains(body, "step 1 of 3") {
		t.Error("the card named the stage before the one running")
	}
	if !strings.Contains(inColumn(body, "implement"), `data-key="EXAMPLE-7"`) || strings.Contains(inColumn(body, "elicit"), `data-key="EXAMPLE-7"`) {
		t.Error("the card must sit in the column of the stage running now")
	}
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

// writeJob lays down one accepted request with its saved history, the way
// the runtime leaves it.
func writeJob(t *testing.T, root, id string, state chain.State) {
	t.Helper()
	dir := filepath.Join(root, "jobs", id)
	if err := os.MkdirAll(filepath.Join(dir, "run"), 0700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"issue.json": `{"id":` + id + `,"issueKey":"EXAMPLE-` + id + `","summary":"request ` + id + `"}`, filepath.Join("run", "history.json"): string(raw)} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

// A request that ended with no change delivered nothing, and the page says
// so: the delivery's own receipt is what tells it apart, not anything a role
// wrote. A delivered request beside it is still shown as delivered.
func TestARequestThatEndedWithoutAChangeIsNotShownAsDelivered(t *testing.T) {
	root := fixtureQueue(t)
	started := time.Date(2026, 1, 2, 0, 10, 0, 0, time.UTC)
	done := chain.State{Done: true, Step: "confirm_report", Workflow: &chain.Workflow{Stages: []chain.Stage{{Name: "elicit"}, {Name: "confirm_report"}}},
		History: []chain.Result{{Role: "confirm_report", Speaker: "confirm-process", Output: "Delivered everything.", StartedAt: started, FinishedAt: started.Add(time.Minute)}}}
	for id, receipt := range map[string]string{
		"23": `{"unchanged": true, "issue": "EXAMPLE-23", "base_sha": "UNCHANGED-BASE"}`,
		"24": `{"pull_request": 41, "merge_sha": "RECEIPT-SHA"}`,
	} {
		writeJob(t, root, id, done)
		path := filepath.Join(root, "jobs", id, "workspace", ".git", "ticket-engine", "delivery.json")
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(receipt), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ts := serve(t, root, "", "", "")
	_, body := get(t, ts, "/")
	expectAll(t, body, `<article class="card unchanged" data-key="EXAMPLE-23">`, `<article class="card delivered" data-key="EXAMPLE-24">`,
		`Done without a change <b>1</b>`, `Delivered <b>1</b>`, "done without a change; nothing was delivered")
	if !strings.Contains(inColumn(body, "done"), `data-key="EXAMPLE-23"`) {
		t.Error("a request that ended without a change is not with the finished ones")
	}
	request, _ := http.NewRequest("GET", ts.URL+"/", nil)
	request.AddCookie(&http.Cookie{Name: "lang", Value: "ja"})
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	japanese, _ := io.ReadAll(response.Body)
	response.Body.Close()
	expectAll(t, string(japanese), "変更なしで完了 <b>1</b>", "変更なしで完了 (何も納品していません)", "納品済み <b>1</b>")
}

// A delivery that ended at the open pull request merged nothing, and the page
// says so until a later delivery records that a person merged it.
func TestARequestEndedAtAnOpenPullRequestIsNotShownAsDelivered(t *testing.T) {
	root := fixtureQueue(t)
	started := time.Date(2026, 1, 2, 0, 10, 0, 0, time.UTC)
	done := chain.State{Done: true, Step: "confirm_report", Workflow: &chain.Workflow{Stages: []chain.Stage{{Name: "elicit"}, {Name: "confirm_report"}}},
		History: []chain.Result{{Role: "confirm_report", Speaker: "confirm-process", StartedAt: started, FinishedAt: started.Add(time.Minute)}}}
	for id, receipt := range map[string]string{
		"25": `{"merge_left_to_person": true, "merge_method": "none", "pull_request": 5, "pull_request_url": "http://service.invalid/pulls/5"}`,
		"26": `{"merge_left_to_person": true, "merge_method": "none", "pull_request": 6, "merge_sha": "MERGED-BY-A-PERSON"}`,
		"27": `{"merge_left_to_person": true, "merge_method": "none", "pull_request": 7, "closed_unmerged": true}`,
	} {
		writeJob(t, root, id, done)
		path := filepath.Join(root, "jobs", id, "workspace", ".git", "ticket-engine", "delivery.json")
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(receipt), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ts := serve(t, root, "", "", "")
	_, body := get(t, ts, "/")
	expectAll(t, body, `<article class="card unmerged" data-key="EXAMPLE-25">`, `<article class="card delivered" data-key="EXAMPLE-26">`,
		`<article class="card closed" data-key="EXAMPLE-27">`, `Done with the pull request open <b>1</b>`, `Delivered <b>1</b>`,
		`Done, the pull request closed unmerged <b>1</b>`, "done; the pull request is open, its merge left to a person",
		"done; the pull request was closed without being merged, so nothing was delivered")
	request, _ := http.NewRequest("GET", ts.URL+"/", nil)
	request.AddCookie(&http.Cookie{Name: "lang", Value: "ja"})
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	japanese, _ := io.ReadAll(response.Body)
	response.Body.Close()
	expectAll(t, string(japanese), "PR を開いて完了 <b>1</b>", "完了 (PR は開いたまま。merge は人に任せています)",
		"PR が閉じられて終了 <b>1</b>", "完了 (PR はマージされずに閉じられたので、何も納品していません)")
}

func TestAPersonChangedAndMergedBranchIsNotAnOpenPullRequest(t *testing.T) {
	job := job{Receipt: `{"merge_left_to_person":true,"changed_by_person":true,"merge_sha":"PERSONS-MERGE"}`}
	if job.endedUnmerged() {
		t.Fatal("a recorded human merge was displayed as an open pull request")
	}
}

// A person merged an earlier round of the request and closed the pull request
// of a later one: something was delivered, and the page does not say otherwise.
func TestAClosedPullRequestAfterAMergedRoundIsNotShownAsNothingDelivered(t *testing.T) {
	root := fixtureQueue(t)
	started := time.Date(2026, 1, 2, 0, 10, 0, 0, time.UTC)
	writeJob(t, root, "28", chain.State{Done: true, Step: "confirm_report", Workflow: &chain.Workflow{Stages: []chain.Stage{{Name: "elicit"}, {Name: "confirm_report"}}},
		History: []chain.Result{{Role: "confirm_report", Speaker: "confirm-process", StartedAt: started, FinishedAt: started.Add(time.Minute)}}})
	path := filepath.Join(root, "jobs", "28", "workspace", ".git", "ticket-engine", "delivery.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	receipt := `{"merge_left_to_person": true, "merge_method": "none", "pull_request": 8, "closed_unmerged": true, "previous": [{"pull_request": 7, "merge_sha": "MERGED-BY-A-PERSON"}]}`
	if err := os.WriteFile(path, []byte(receipt), 0600); err != nil {
		t.Fatal(err)
	}
	ts := serve(t, root, "", "", "")
	_, body := get(t, ts, "/")
	expectAll(t, body, `<article class="card closed" data-key="EXAMPLE-28">`,
		"done; an earlier round was merged, and the last pull request was closed without being merged")
	if strings.Contains(body, "nothing was delivered") {
		t.Error("a request with a merged round is said to have delivered nothing")
	}
	request, _ := http.NewRequest("GET", ts.URL+"/", nil)
	request.AddCookie(&http.Cookie{Name: "lang", Value: "ja"})
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	japanese, _ := io.ReadAll(response.Body)
	response.Body.Close()
	expectAll(t, string(japanese), "完了 (前の回はマージ済み。最後の PR はマージされずに閉じられました)")
	if strings.Contains(string(japanese), "何も納品していません") {
		t.Error("a request with a merged round is said to have delivered nothing, in Japanese")
	}
}

func TestASideRoleWithNoStageBehindItStaysInTheOtherColumn(t *testing.T) {
	root := fixtureQueue(t)
	started := time.Date(2026, 1, 2, 0, 10, 0, 0, time.UTC)
	flow := &chain.Workflow{Stages: []chain.Stage{{Name: "elicit"}, {Name: "implement"}, {Name: "verify"}}}
	writeJob(t, root, "20", chain.State{Waiting: true, Step: "ask_requester", Workflow: flow,
		History: []chain.Result{{Role: "ask_requester", Speaker: "ask-process", StartedAt: started, FinishedAt: started.Add(time.Minute)}}})
	writeJob(t, root, "21", chain.State{Step: "stop_report", Workflow: flow})
	ts := serve(t, root, "", "", "")
	response, body := get(t, ts, "/")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("the board answered HTTP %d", response.StatusCode)
	}
	other := inColumn(body, "other")
	for _, key := range []string{"EXAMPLE-20", "EXAMPLE-21"} {
		if !strings.Contains(other, `data-key="`+key+`"`) {
			t.Errorf("%s is not in the other column", key)
		}
		for _, stage := range []string{"elicit", "implement", "verify"} {
			if strings.Contains(inColumn(body, stage), `data-key="`+key+`"`) {
				t.Errorf("%s was placed at stage %s with no stage in its history", key, stage)
			}
		}
	}
	for _, id := range []string{"20", "21"} {
		if response, _ := get(t, ts, "/jobs/"+id); response.StatusCode != http.StatusOK {
			t.Errorf("the job page for %s answered HTTP %d", id, response.StatusCode)
		}
	}
}

func TestADeliveredRequestIsListedOnlyInTheLastColumn(t *testing.T) {
	root := fixtureQueue(t)
	started := time.Date(2026, 1, 2, 0, 10, 0, 0, time.UTC)
	writeJob(t, root, "22", chain.State{Done: true, Step: "implement", Workflow: &chain.Workflow{Stages: []chain.Stage{{Name: "elicit"}, {Name: "implement"}, {Name: "verify"}}},
		History: []chain.Result{{Role: "implement", Speaker: "implement-process", StartedAt: started, FinishedAt: started.Add(time.Minute)}}})
	ts := serve(t, root, "", "", "")
	_, body := get(t, ts, "/")
	if !strings.Contains(inColumn(body, "done"), `data-key="EXAMPLE-22"`) {
		t.Error("a delivered request is not in the last column")
	}
	if got := strings.Count(body, `data-key="EXAMPLE-22"`); got != 1 {
		t.Errorf("a delivered request appears %d times on the board", got)
	}
}

func TestAStepRetriedAfterAFailureSaysSoAndNamesTheFailure(t *testing.T) {
	root := fixtureQueue(t)
	started := time.Date(2026, 1, 2, 0, 10, 0, 0, time.UTC)
	writeJob(t, root, "30", chain.State{Step: "elicit", Recovering: true, Pending: &chain.Assignment{Role: "elicit"},
		Workflow: &chain.Workflow{Stages: []chain.Stage{{Name: "elicit"}, {Name: "implement"}}},
		History: []chain.Result{
			{Role: "elicit", Speaker: "elicit-process", Error: "fork/exec /usr/bin/elicit: no such file or directory\nmore detail", StartedAt: started, FinishedAt: started},
			{Role: "elicit", Speaker: "runtime", Output: "Process elicit-process did not exit 0.", StartedAt: started, FinishedAt: started},
		}})
	ts := serve(t, root, "", "", "")
	_, body := get(t, ts, "/")
	expectAll(t, body, "retrying after a failure; assigned to elicit, no process output yet", "last failure: fork/exec /usr/bin/elicit: no such file or directory")
	if strings.Contains(body, "more detail") || strings.Contains(body, "recovering after a restart") {
		t.Error("the card shows more than the failure's first line, or the old wording")
	}
	if !strings.Contains(body, `<article class="card running" data-key="EXAMPLE-30">`) {
		t.Error("a step retrying by itself is not a running card")
	}
	_, page := get(t, ts, "/jobs/30")
	if strings.Count(page, "retrying after a failure") != 1 || !strings.Contains(page, "last failure: fork/exec /usr/bin/elicit: no such file or directory") || strings.Contains(page, `<span class="badge">recovering</span>`) {
		t.Error("the request page does not say the same as the card, or still shows the generic recovering badge")
	}
	// In Japanese, chosen by the cookie the language page sets, the request
	// page says the same and the old wording is gone.
	request, _ := http.NewRequest("GET", ts.URL+"/jobs/30", nil)
	request.AddCookie(&http.Cookie{Name: "lang", Value: "ja"})
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	japanese, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if !strings.Contains(string(japanese), "失敗後の再試行中。割り当て済み (出力はまだ): 要件確定") || !strings.Contains(string(japanese), "直近の失敗: fork/exec /usr/bin/elicit") || strings.Contains(string(japanese), "再起動後の復帰中") {
		t.Error("the Japanese request page does not say the same as the card, or still says 再起動後の復帰中")
	}
	// A role with two processes: the one that failed may be recorded before
	// the one that returned, and it is still the failure named.
	writeJob(t, root, "31", chain.State{Step: "verify", Recovering: true, Pending: &chain.Assignment{Role: "verify"},
		Workflow: &chain.Workflow{Stages: []chain.Stage{{Name: "implement"}, {Name: "verify"}}},
		History: []chain.Result{
			{Role: "implement", Speaker: "implement-process", Output: "done", StartedAt: started, FinishedAt: started},
			{Role: "implement", Speaker: "runtime", Output: "Process implement-process exited 0.", StartedAt: started, FinishedAt: started},
			{Role: "verify", Speaker: "project-tests", Error: "exit status 1: TestX failed", StartedAt: started, FinishedAt: started},
			{Role: "verify", Speaker: "project-build", Output: "ok", StartedAt: started, FinishedAt: started},
			{Role: "verify", Speaker: "runtime", Output: "Process project-tests did not exit 0.", StartedAt: started, FinishedAt: started},
		}})
	_, again := get(t, ts, "/")
	if !strings.Contains(again, "last failure: exit status 1: TestX failed") {
		t.Error("the failed process of a two-process launch is not named when its sibling returned after it")
	}
	if got := localize("ja", "retrying after a failure; assigned to elicit, no process output yet"); got != "失敗後の再試行中。割り当て済み (出力はまだ): 要件確定" {
		t.Errorf("localized status: %q", got)
	}
	if got := localize("ja", "taking up an interrupted step; between steps"); got != "中断した工程の再開中。工程の切れ目" {
		t.Errorf("localized interrupted status: %q", got)
	}
}

func TestAStoppedRequestSaysSoAndWhetherItsReportWentOut(t *testing.T) {
	root := fixtureQueue(t)
	started := time.Date(2026, 1, 2, 0, 10, 0, 0, time.UTC)
	flow := &chain.Workflow{Stages: []chain.Stage{{Name: "elicit"}, {Name: "implement"}}}
	failing := []chain.Result{
		{Role: "elicit", Speaker: "elicit-process", Error: "fork/exec /usr/bin/elicit: no such file or directory", StartedAt: started, FinishedAt: started},
		{Role: "elicit", Speaker: "runtime", Output: "Process elicit-process did not exit 0.", StartedAt: started, FinishedAt: started},
	}
	for _, id := range []string{"40", "41"} {
		writeJob(t, root, id, chain.State{Step: "elicit", Recovering: true, Pending: &chain.Assignment{Role: "elicit"}, Workflow: flow, History: failing})
		if err := os.WriteFile(filepath.Join(root, "jobs", id, "stop-request.json"), []byte(`{"id":55,"content":"停止"}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "jobs", "40", "stop-report"), 0700); err != nil {
		t.Fatal(err)
	}
	report, _ := json.Marshal(chain.State{Request: "report", Done: true, History: []chain.Result{{Role: "stop_report", Speaker: "reporter", Output: "posted"}}})
	if err := os.WriteFile(filepath.Join(root, "jobs", "40", "stop-report", "history.json"), report, 0600); err != nil {
		t.Fatal(err)
	}
	ts := serve(t, root, "", "", "")
	_, body := get(t, ts, "/")
	expectAll(t, body, `Stopped <b>2</b>`, `Running <b>1</b>`, `Needs attention <b>0</b>`,
		`<article class="card stopped" data-key="EXAMPLE-40">`, `<article class="card stopped" data-key="EXAMPLE-41">`,
		"stopped by the requester; report posted", "stopped by the requester; report pending")
	if strings.Contains(body, "retrying after a failure") {
		t.Error("a stopped request is still shown as retrying")
	}
	if !strings.Contains(inColumn(body, "elicit"), `data-key="EXAMPLE-40"`) {
		t.Error("a stopped request left the stage it stopped at")
	}
	request, _ := http.NewRequest("GET", ts.URL+"/jobs/40", nil)
	request.AddCookie(&http.Cookie{Name: "lang", Value: "ja"})
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	japanese, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if !strings.Contains(string(japanese), "依頼者が停止。報告済み") {
		t.Error("the Japanese request page does not say the request was stopped and reported")
	}
	// Stopped while waiting on a question: the page does not also say it
	// waits. Stopped after delivery: delivered stands. A stop record the
	// runtime cannot read holds the work and needs a person.
	writeJob(t, root, "42", chain.State{Waiting: true, Step: "ask_requester", Workflow: flow,
		History: []chain.Result{{Role: "elicit", Speaker: "elicit-process", StartedAt: started, FinishedAt: started}, {Role: "ask_requester", Speaker: "ask-process", StartedAt: started, FinishedAt: started}}})
	writeJob(t, root, "43", chain.State{Done: true, Step: "implement", Workflow: flow,
		History: []chain.Result{{Role: "implement", Speaker: "implement-process", StartedAt: started, FinishedAt: started}}})
	writeJob(t, root, "44", chain.State{Step: "elicit", Workflow: flow, History: failing})
	for _, id := range []string{"42", "43"} {
		if err := os.WriteFile(filepath.Join(root, "jobs", id, "stop-request.json"), []byte(`{"id":56,"content":"停止"}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "jobs", "44", "stop-request.json"), []byte(`{"id":`), 0600); err != nil {
		t.Fatal(err)
	}
	_, page := get(t, ts, "/jobs/42")
	if strings.Count(page, `class="badge awaiting"`) != 0 || !strings.Contains(page, "stopped by the requester; report pending") {
		t.Error("a request stopped while waiting still says it waits for the requester")
	}
	_, board := get(t, ts, "/")
	expectAll(t, board, `<article class="card delivered" data-key="EXAMPLE-43">`, `<article class="card attention" data-key="EXAMPLE-44">`, "the saved stop instruction is unreadable; the work is held",
		"held: the saved stop instruction is unreadable")
	if !strings.Contains(inColumn(board, "done"), `data-key="EXAMPLE-43"`) {
		t.Error("a request delivered before its stop left the delivered column")
	}
	if strings.Contains(inColumn(board, "elicit"), "retrying after a failure") {
		t.Error("a held request still says it is retrying")
	}
	// The request page says the same as the card, and a delivered request
	// with an unreadable stop record stays delivered.
	_, held := get(t, ts, "/jobs/44")
	if !strings.Contains(held, "the saved stop instruction is unreadable; the work is held") || strings.Contains(held, "retrying after a failure") {
		t.Error("the request page does not say the work is held for an unreadable stop record")
	}
	writeJob(t, root, "45", chain.State{Done: true, Step: "implement", Workflow: flow,
		History: []chain.Result{{Role: "implement", Speaker: "implement-process", StartedAt: started, FinishedAt: started}}})
	if err := os.WriteFile(filepath.Join(root, "jobs", "45", "stop-request.json"), []byte(`{"id":`), 0600); err != nil {
		t.Fatal(err)
	}
	_, again := get(t, ts, "/")
	if !strings.Contains(again, `<article class="card delivered" data-key="EXAMPLE-45">`) || strings.Contains(inColumn(again, "done"), "the work is held") {
		t.Error("a delivered request with an unreadable stop record is not left delivered")
	}
}

func TestTheRequestPageReadsTheRecordLaunchByLaunchAndSaysWhoWroteWhat(t *testing.T) {
	root := fixtureQueue(t)
	started := time.Date(2026, 1, 2, 0, 10, 0, 0, time.UTC)
	stage := "Stage 1 of 2 in the configured run: elicit."
	var history []chain.Result
	for i := 0; i < 3; i++ {
		at := started.Add(time.Duration(i) * 10 * time.Second)
		history = append(history,
			chain.Result{Role: "elicit", Speaker: "elicit-process", Model: "maker/model-a", Instruction: stage, Error: "fork/exec /usr/bin/elicit: no such file or directory", StartedAt: at, FinishedAt: at},
			chain.Result{Role: "elicit", Speaker: "runtime", Instruction: stage, Output: "Runtime record for stage elicit.\nProcess elicit-process did not exit 0: fork/exec /usr/bin/elicit: no such file or directory", StartedAt: at, FinishedAt: at})
	}
	at := started.Add(time.Minute)
	history = append(history,
		chain.Result{Role: "elicit", Speaker: "elicit-process", Model: "maker/model-a", Instruction: stage, Output: "REQUIREMENTS-SETTLED", StartedAt: at, FinishedAt: at.Add(2 * time.Minute)},
		chain.Result{Role: "elicit", Speaker: "runtime", Instruction: stage, Output: "Runtime record for stage elicit.\nProcess elicit-process exited 0.", StartedAt: at.Add(2 * time.Minute), FinishedAt: at.Add(2 * time.Minute)},
		chain.Result{Role: "ask_requester", Speaker: "requester", Output: "ANSWER-ONE", StartedAt: at.Add(3 * time.Minute), FinishedAt: at.Add(3 * time.Minute)},
		chain.Result{Role: "verify", Speaker: "project-build", Output: "build ok", StartedAt: at.Add(4 * time.Minute), FinishedAt: at.Add(5 * time.Minute)},
		chain.Result{Role: "verify", Speaker: "project-tests", Error: "exit status 1\n--- FAIL: TestX", Output: "running tests", StartedAt: at.Add(4 * time.Minute), FinishedAt: at.Add(5 * time.Minute)},
		chain.Result{Role: "verify", Speaker: "runtime", Output: "Runtime record for stage verify.\nProcess project-tests did not exit 0.", StartedAt: at.Add(5 * time.Minute), FinishedAt: at.Add(5 * time.Minute)})
	writeJob(t, root, "60", chain.State{Step: "verify", Workflow: &chain.Workflow{Stages: []chain.Stage{{Name: "elicit"}, {Name: "verify"}}}, History: history})
	ts := serve(t, root, "", "", "")
	_, page := get(t, ts, "/jobs/60")
	expectAll(t, page,
		"written by: the requester", "what the requester wrote", "written by: the operator&#39;s settings", "the role&#39;s purpose and the operator&#39;s instructions",
		"the runtime (how it ended), then the worker&#39;s stderr", "at most the latest sixty",
		"What happened, launch by launch", "(5 launches, 4 entries)",
		"launch 1–3", "could not start", "the same failure, repeated 3 times", "no output: the process could not start",
		"why it ended so", "fork/exec /usr/bin/elicit: no such file or directory",
		"launch 4", "returned", "REQUIREMENTS-SETTLED", "written by: the worker maker/model-a",
		"answer from the requester", "ANSWER-ONE",
		"launch 6", "failed", "exit status 1", "written by: the command project-build", "written by: the command project-tests",
		"what the runtime observed", "written by: the runtime", "what the worker was handed", "your ticket text",
		"raw records (12 entries)")
	if strings.Count(page, `<div class="launch `) != 4 {
		t.Errorf("expected four launch entries, got %d", strings.Count(page, `<div class="launch `))
	}
	if strings.Contains(page, "written by: the command requester") {
		t.Error("the requester's answer is labelled as a command")
	}
	// A process that returned with nothing on stdout says "no output" and
	// nothing more; text a role wrote is escaped.
	writeJob(t, root, "61", chain.State{Step: "verify", Workflow: &chain.Workflow{Stages: []chain.Stage{{Name: "verify"}}}, History: []chain.Result{
		{Role: "verify", Speaker: "verify-process", Output: "", StartedAt: started, FinishedAt: started.Add(time.Minute)},
		{Role: "verify", Speaker: "runtime", Output: "Runtime record for stage verify.\nProcess verify-process exited 0.", StartedAt: started.Add(time.Minute), FinishedAt: started.Add(time.Minute)},
		{Role: "implement", Speaker: "implement-process", Model: "m", Output: "<script>alert(1)</script>", StartedAt: started.Add(2 * time.Minute), FinishedAt: started.Add(3 * time.Minute)},
	}})
	_, quiet := get(t, ts, "/jobs/61")
	if !strings.Contains(quiet, `<p class="empty">no output</p>`) || strings.Contains(quiet, "could not start") || strings.Contains(quiet, "<script>alert(1)</script>") || !strings.Contains(quiet, "&lt;script&gt;") {
		t.Error("an empty output is called a start failure, or a role's text reached the page unescaped")
	}
	request, _ := http.NewRequest("GET", ts.URL+"/jobs/60", nil)
	request.AddCookie(&http.Cookie{Name: "lang", Value: "ja"})
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	japanese, _ := io.ReadAll(response.Body)
	response.Body.Close()
	expectAll(t, string(japanese), "起きたこと (起動ごと)", "5 回の起動", "起動できず", "同じ失敗の繰り返し 3 回", "出力なし: 起動できず", "書いた者: 本体", "書いた者: 担当 maker/model-a", "依頼者の返答", "依頼者が書いたこと", "書いた者: 依頼者", "担当 (LLM) に渡したもの", "あなたが Backlog に書いた本文", "役の説明と運用者の指示", "本体 (終了状態)、続く行は担当の stderr")
	if strings.Contains(string(japanese), ">指示<") {
		t.Error("the page still calls what the runtime hands over an instruction")
	}
}

func TestLaunchesAreToldApartWithoutAStageNoteAndNotesStandOnTheirOwn(t *testing.T) {
	started := time.Date(2026, 1, 2, 0, 10, 0, 0, time.UTC)
	entries := func(results ...chain.Result) []record {
		var records []record
		for i, result := range results {
			records = append(records, record{Index: i + 1, Role: result.Role, Speaker: result.Speaker, Model: result.Model, Started: result.StartedAt, Finished: result.FinishedAt,
				Output: result.Output, Error: result.Error, Diagnostics: result.Diagnostics, Instruction: result.Instruction, Runtime: result.Speaker == "runtime", Person: result.Speaker == "requester"})
		}
		return records
	}
	// A role with no stage behind it writes no runtime note; its launches
	// are told apart by time, and identical failures still fold.
	var free []chain.Result
	for i := 0; i < 3; i++ {
		at := started.Add(time.Duration(i) * 10 * time.Second)
		free = append(free, chain.Result{Role: "ask_requester", Speaker: "ask-process", Model: "m", Error: "fork/exec /usr/bin/ask: no such file or directory", StartedAt: at, FinishedAt: at})
	}
	free = append(free, chain.Result{Role: "ask_requester", Speaker: "ask-process", Model: "m", Output: "asked", StartedAt: started.Add(time.Minute), FinishedAt: started.Add(2 * time.Minute)})
	launches := groupLaunches(entries(free...))
	if len(launches) != 2 || launches[0].Count != 3 || launches[0].Outcome != couldNotStart || launches[1].Outcome != returned || launches[1].Index != 4 {
		t.Fatalf("launches of a role without a stage note were fused or not folded: %+v", launches)
	}
	// An interruption note stands on its own as "interrupted"; it does not
	// rewrite the launch before it. A routing failure is a failure, not a
	// grey note, and a note without a start has no duration.
	noted := groupLaunches(entries(
		chain.Result{Role: "implement", Speaker: "implement-process", Model: "m", Output: "FIRST-RUN", StartedAt: started, FinishedAt: started.Add(time.Minute)},
		chain.Result{Role: "implement", Speaker: "runtime", Error: "The process stopped while this action was pending. Available reports may be partial.", FinishedAt: started.Add(20 * time.Minute)},
		chain.Result{Role: "router", Speaker: "runtime", Error: "routing unavailable: model service returned HTTP 502", FinishedAt: started.Add(21 * time.Minute)},
	))
	if len(noted) != 3 || noted[0].Outcome != returned || noted[0].Duration != "1m00s" || noted[1].Outcome != interrupted || noted[1].Duration != "" || noted[2].Outcome != failed || noted[2].Failure != "routing unavailable: model service returned HTTP 502" {
		t.Fatalf("notes were attached to the launch before them or shown wrongly: %+v", noted)
	}
	// Two processes of which one could not start: the launch failed, it did
	// not "not start". Two different failures never fold, and a folded entry
	// keeps the latest launch's text.
	mixed := groupLaunches(entries(
		chain.Result{Role: "verify", Speaker: "project-build", Output: "BUILD-OK", StartedAt: started, FinishedAt: started.Add(time.Minute)},
		chain.Result{Role: "verify", Speaker: "project-tests", Error: "fork/exec /usr/bin/tests: no such file or directory", StartedAt: started, FinishedAt: started},
		chain.Result{Role: "verify", Speaker: "runtime", Output: "Runtime record for stage verify.\nProcess project-tests did not exit 0.", FinishedAt: started.Add(time.Minute)},
		chain.Result{Role: "verify", Speaker: "project-tests", Error: "exit status 1\n--- FAIL: TestAlpha", Output: "ran", StartedAt: started.Add(2 * time.Minute), FinishedAt: started.Add(3 * time.Minute)},
		chain.Result{Role: "verify", Speaker: "runtime", Output: "Runtime record for stage verify.\nProcess project-tests did not exit 0.", FinishedAt: started.Add(3 * time.Minute)},
		chain.Result{Role: "verify", Speaker: "project-tests", Error: "exit status 1\n--- FAIL: TestBeta", Output: "ran again", StartedAt: started.Add(4 * time.Minute), FinishedAt: started.Add(5 * time.Minute)},
		chain.Result{Role: "verify", Speaker: "runtime", Output: "Runtime record for stage verify.\nProcess project-tests did not exit 0.", FinishedAt: started.Add(5 * time.Minute)},
		chain.Result{Role: "verify", Speaker: "project-tests", Error: "exit status 1\n--- FAIL: TestBeta", Output: "ran once more", StartedAt: started.Add(6 * time.Minute), FinishedAt: started.Add(7 * time.Minute)},
		chain.Result{Role: "verify", Speaker: "runtime", Output: "Runtime record for stage verify.\nProcess project-tests did not exit 0.", FinishedAt: started.Add(7 * time.Minute)},
	))
	if len(mixed) != 3 || mixed[0].Outcome != failed || mixed[1].Count != 1 || mixed[2].Count != 2 || mixed[2].Workers[0].Output != "ran once more" || mixed[2].Index != 3 || mixed[2].Last != 4 {
		t.Fatalf("a mixed launch, two different failures, or the folded text were read wrongly: %+v", mixed)
	}
	// One launch of two processes where the first fell at once and the
	// second started afterwards is still one launch: the boundary is the
	// same process appearing again, not the clock. Identical routing
	// failures fold like any other; a worker's own failure text is shown as
	// written, not translated.
	late := groupLaunches(entries(
		chain.Result{Role: "verify", Speaker: "project-build", Error: "fork/exec /usr/bin/build: no such file or directory", StartedAt: started, FinishedAt: started},
		chain.Result{Role: "verify", Speaker: "project-tests", Output: "TESTS-OK", StartedAt: started.Add(3 * time.Second), FinishedAt: started.Add(5 * time.Minute)},
		chain.Result{Role: "verify", Speaker: "runtime", Output: "Runtime record for stage verify.\nProcess project-build did not exit 0.", FinishedAt: started.Add(5 * time.Minute)},
		chain.Result{Role: "verify", Speaker: "project-build", Output: "built", StartedAt: started.Add(6 * time.Minute), FinishedAt: started.Add(7 * time.Minute)},
		chain.Result{Role: "verify", Speaker: "project-tests", Output: "TESTS-OK", StartedAt: started.Add(6 * time.Minute), FinishedAt: started.Add(7 * time.Minute)},
		chain.Result{Role: "router", Speaker: "runtime", Error: "routing unavailable: HTTP 502", FinishedAt: started.Add(8 * time.Minute)},
		chain.Result{Role: "router", Speaker: "runtime", Error: "routing unavailable: HTTP 502", FinishedAt: started.Add(9 * time.Minute)},
		chain.Result{Role: "implement", Speaker: "implement-process", Model: "m", Error: "Error", StartedAt: started.Add(10 * time.Minute), FinishedAt: started.Add(10 * time.Minute)},
	))
	if len(late) != 4 || late[0].Outcome != failed || len(late[0].Workers) != 2 || late[1].Outcome != returned || late[2].Count != 2 || late[2].Outcome != failed || late[2].RuntimeFailure != true || late[3].RuntimeFailure != false {
		t.Fatalf("a launch whose second process started late was split, or routing failures were not folded: %+v", late)
	}
	root := fixtureQueue(t)
	writeJob(t, root, "62", chain.State{Step: "implement", Workflow: &chain.Workflow{Stages: []chain.Stage{{Name: "implement"}}}, History: []chain.Result{
		{Role: "implement", Speaker: "implement-process", Model: "m", Error: "Error", Output: "wrote", StartedAt: started, FinishedAt: started.Add(time.Minute)},
	}})
	ts := serve(t, root, "", "", "")
	request, _ := http.NewRequest("GET", ts.URL+"/jobs/62", nil)
	request.AddCookie(&http.Cookie{Name: "lang", Value: "ja"})
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	japanese, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if !strings.Contains(string(japanese), "<br>Error</p>") {
		t.Error("a worker's own failure text was replaced by a dictionary word on the Japanese page")
	}
}

func TestAnAcceptedRequestNotYetLaunchedIsShownAsQueuedAtTheFirstStage(t *testing.T) {
	root := fixtureQueue(t)
	flow := &chain.Workflow{Stages: []chain.Stage{{Name: "elicit"}, {Name: "implement"}}}
	// The record written at acceptance holds no run definition (70, 72);
	// a run that has the slot writes its definition before choosing its
	// first stage (71).
	writeJob(t, root, "70", chain.State{})
	writeJob(t, root, "71", chain.State{Workflow: flow})
	writeJob(t, root, "72", chain.State{})
	// A freely routed run saves a definition without stages as it starts:
	// it holds the slot and is not queued, and has no first stage to sit at.
	writeJob(t, root, "73", chain.State{Workflow: &chain.Workflow{Start: []string{"elicit"}, After: map[string][]string{"elicit": {"work"}}}})
	ts := serve(t, root, "", "", "")
	_, body := get(t, ts, "/")
	expectAll(t, body, `Queued <b>2</b>`, `Running <b>3</b>`, `<article class="card queued" data-key="EXAMPLE-70">`, `<article class="card running" data-key="EXAMPLE-71">`, `<article class="card running" data-key="EXAMPLE-73">`,
		"queued: waiting for a free execution slot", "starting: choosing the first stage")
	if !strings.Contains(inColumn(body, "other"), `data-key="EXAMPLE-73"`) || !strings.Contains(body, "starting: choosing the first role") {
		t.Error("a freely routed run that is starting is not in the other column, or is said to choose a stage")
	}
	// Accepted on this tick, before the runtime has written any record:
	// only issue.json exists. That is queued too, not "no run record yet".
	if err := os.MkdirAll(filepath.Join(root, "jobs", "74"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "jobs", "74", "issue.json"), []byte(`{"id":74,"issueKey":"EXAMPLE-74","summary":"request 74"}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, fresh := get(t, ts, "/")
	expectAll(t, fresh, `Queued <b>3</b>`, `<article class="card queued" data-key="EXAMPLE-74">`)
	if strings.Contains(fresh, "no run record yet") || !strings.Contains(inColumn(fresh, "elicit"), `data-key="EXAMPLE-74"`) {
		t.Error("a request accepted this tick is called \"no run record yet\" or left out of the first column")
	}
	// A directory left by an interrupted acceptance, with no issue in it, is
	// not a queued request.
	if err := os.MkdirAll(filepath.Join(root, "jobs", "75"), 0700); err != nil {
		t.Fatal(err)
	}
	_, empty := get(t, ts, "/")
	if strings.Contains(empty, `<article class="card queued" data-key="">`) || !strings.Contains(empty, `Queued <b>3</b>`) || strings.Contains(empty, `<span class="badge running"></span>`) {
		t.Error("an empty request directory is shown as queued, or with an empty badge")
	}
	if column := inColumn(body, "elicit"); !strings.Contains(column, `data-key="EXAMPLE-70"`) || !strings.Contains(column, `data-key="EXAMPLE-71"`) || !strings.Contains(column, `data-key="EXAMPLE-72"`) {
		t.Error("a queued or starting request is not shown at the first stage")
	}
	if strings.Contains(inColumn(body, "other"), `data-key="EXAMPLE-70"`) || strings.Contains(inColumn(body, "other"), `data-key="EXAMPLE-72"`) {
		t.Error("a queued request fell into the other column")
	}
	if strings.Contains(inColumn(body, "elicit"), "between steps") {
		t.Error("a queued request is called between steps")
	}
	request, _ := http.NewRequest("GET", ts.URL+"/", nil)
	request.AddCookie(&http.Cookie{Name: "lang", Value: "ja"})
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	japanese, _ := io.ReadAll(response.Body)
	response.Body.Close()
	expectAll(t, string(japanese), "順番待ち <b>3</b>", "順番待ち (実行枠が空くのを待っています)", "開始中 (最初の工程を決めています)")
}

// A GitHub issue is shown by its number, its title, the account that opened
// it and its own page, all read from its record, with or without a
// configuration. A Backlog issue beside it is shown as before, and each page
// names where its requester wrote.
func TestAGitHubIssueIsShownByItsNumberTitleOpenerAndPage(t *testing.T) {
	root := fixtureQueue(t)
	job := filepath.Join(root, "jobs", "12")
	if err := os.MkdirAll(filepath.Join(job, "run"), 0700); err != nil {
		t.Fatal(err)
	}
	issue := func(page string) {
		t.Helper()
		record := `{"number":12,"title":"Add a docstring on GitHub","body":"BODY","created_at":"2026-01-02T00:00:00Z",` +
			`"user":{"id":11,"login":"fixture-requester"},"html_url":"` + page + `","repository_url":"https://api.github.example/repos/octo-org/widgets"}`
		if err := os.WriteFile(filepath.Join(job, "issue.json"), []byte(record), 0600); err != nil {
			t.Fatal(err)
		}
	}
	issue("https://github.example/octo-org/widgets/issues/12")
	started := time.Date(2026, 1, 2, 0, 10, 0, 0, time.UTC)
	raw, _ := json.Marshal(chain.State{Request: "Original issue: 12\nTitle: Add a docstring on GitHub\n\nBODY", Step: "implement",
		History: []chain.Result{{Role: "elicit", Speaker: "elicit-process", Output: "OUTPUT", Instruction: "INSTRUCTION", StartedAt: started, FinishedAt: started.Add(time.Minute)}}})
	if err := os.WriteFile(filepath.Join(job, "run", "history.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	japanese := func(ts *httptest.Server, path string) string {
		t.Helper()
		request, _ := http.NewRequest("GET", ts.URL+path, nil)
		request.AddCookie(&http.Cookie{Name: "lang", Value: "ja"})
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		return string(body)
	}
	for _, config := range []string{"", fixtureConfig(t)} {
		ts := serve(t, root, config, "", "")
		_, body := get(t, ts, "/")
		expectAll(t, body, `data-key="#12"`, "Add a docstring on GitHub", "fixture-requester", `data-key="EXAMPLE-7"`)
		_, body = get(t, ts, "/jobs/12")
		expectAll(t, body, "<title>#12 status</title>", `<a href="https://github.example/octo-org/widgets/issues/12">`, "fixture-requester · 2026-01-02T00:00:00Z")
		if body := japanese(ts, "/jobs/12"); !strings.Contains(body, "あなたが GitHub の issue に書いた本文") || strings.Contains(body, "Backlog") {
			t.Errorf("config %q: the GitHub issue's page does not name GitHub, or names Backlog", config)
		}
		if body := japanese(ts, "/jobs/7"); !strings.Contains(body, "あなたが Backlog に書いた本文") {
			t.Errorf("config %q: the Backlog issue's page changed its words", config)
		}
	}
	// Only a page on https is linked.
	issue("http://github.example/octo-org/widgets/issues/12")
	ts := serve(t, root, "", "", "")
	if _, body := get(t, ts, "/jobs/12"); strings.Contains(body, "github.example/octo-org/widgets/issues/12") {
		t.Error("a page address that is not https was linked")
	}
}

// A record is a GitHub issue's when it has a positive number, its page and
// its repository, whatever else it holds, and only a link to that very issue
// is offered: on https, with nobody's name or password, to the issue's number.
func TestOnlyAGitHubIssuesOwnPageIsLinkedAndTheRecordDecidesTheKind(t *testing.T) {
	root := t.TempDir()
	write := func(id, record string) {
		t.Helper()
		dir := filepath.Join(root, "jobs", id)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "issue.json"), []byte(record), 0600); err != nil {
			t.Fatal(err)
		}
	}
	github := func(number, page string, extra string) string {
		return `{"number":` + number + `,"title":"GITHUB-TITLE","user":{"login":"fixture-requester"},"html_url":"` + page +
			`","repository_url":"https://api.github.example/repos/octo-org/widgets"` + extra + `}`
	}
	own := "https://github.example/octo-org/widgets/issues/12"
	write("12", github("12", own, `,"issueKey":"EXAMPLE-12"`))
	write("13", github("13", own, ""))
	write("14", github("14", "https://github.example@evil.example/octo-org/widgets/issues/14", ""))
	write("15", github("15", "https://user:secret@github.example/octo-org/widgets/issues/15", ""))
	write("16", github("16", "https://evil.example/phish", ""))
	write("17", github("17", "https:///octo-org/widgets/issues/17", ""))
	write("18", `{"issueKey":"","summary":"BACKLOG-TITLE","html_url":"https://github.example/octo-org/widgets/issues/18","createdUser":{"name":"backlog requester"}}`)
	write("19", github("0", own, ""))
	write("20", github(`"20"`, own, ""))
	write("21", `{"number":21,"title":"GITHUB-TITLE","html_url":"https://github.example/octo-org/widgets/issues/21"}`)
	ts := serve(t, root, "", "", "")
	page := func(id string) string {
		t.Helper()
		_, body := get(t, ts, "/jobs/"+id)
		return body
	}
	// Both shapes at once: the GitHub fields decide.
	expectAll(t, page("12"), "<title>#12 status</title>", "GITHUB-TITLE", `<a href="`+own+`">open the issue on GitHub</a>`)
	if body := page("13"); strings.Contains(body, `href="`+own+`"`) {
		t.Error("a page of another issue was linked")
	}
	for _, id := range []string{"14", "15", "16", "17"} {
		if body := page(id); !strings.Contains(body, "<title>#"+id+" status</title>") || strings.Contains(body, "open the issue") {
			t.Errorf("issue %s: not shown as a GitHub issue, or its page was linked", id)
		}
	}
	// Without a positive number, or without its repository, it is not a
	// GitHub issue's record.
	for _, id := range []string{"18", "19", "20", "21"} {
		if body := page(id); strings.Contains(body, "<title>#") || strings.Contains(body, "GITHUB-TITLE") {
			t.Errorf("record %s was read as a GitHub issue's", id)
		}
	}
	expectAll(t, page("18"), "BACKLOG-TITLE", "backlog requester")
	request, _ := http.NewRequest("GET", ts.URL+"/jobs/12", nil)
	request.AddCookie(&http.Cookie{Name: "lang", Value: "ja"})
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	japanese, _ := io.ReadAll(response.Body)
	response.Body.Close()
	expectAll(t, string(japanese), `<a href="`+own+`">issue を開く</a>`)
	if _, body := get(t, serve(t, fixtureQueue(t), fixtureConfig(t), "", ""), "/jobs/7"); !strings.Contains(body, `<a href="https://space.example/view/EXAMPLE-7">open the issue</a>`) {
		t.Error("the Backlog issue's link changed")
	}
}
