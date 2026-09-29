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
		"LOG-LINE-2", "issue_ids", "project_id", "elicit &rarr; implement &rarr; verify", "&#34;mode&#34;: &#34;stages&#34;")
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
