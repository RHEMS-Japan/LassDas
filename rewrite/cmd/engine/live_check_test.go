package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"ticket-runner/internal/chain"
)

// A live check is the installation's own command, added as one more process
// of the verify stage (deploy/ticket-engine/SETUP.md, "A live check of the
// running change"). The engine has no setting for it and reads none of its
// words: its exit status counts like the build's and the tests', and what it
// prints joins the history, where the report stage reads it.

const liveCheckRequest = "サービスの挨拶を Hello 日本語 にしてください。"
const liveCheckGreeting = "Hello 日本語\n"
const liveCheckNone = "なし (導入先に検証の手段が無い)"
const liveCheckUnitObservation = "project tests: src/greeting.txt is readable"
const liveCheckBlockStart = "ライブ確認 ("
const liveCheckBlockEnd = "結果: "

// liveCheckObservation is the closing block a live check prints: from its
// heading to its last line, which states the result. Without that heading it
// is everything the check printed.
func liveCheckObservation(output string) string {
	start := strings.LastIndex(output, liveCheckBlockStart)
	if start < 0 {
		return strings.TrimSpace(output)
	}
	block := output[start:]
	if end := strings.Index(block, "\n"+liveCheckBlockEnd); end >= 0 {
		line := block[end+1:]
		if stop := strings.Index(line, "\n"); stop >= 0 {
			line = line[:stop]
		}
		block = block[:end+1] + line
	}
	return strings.TrimSpace(block)
}

// liveCheckReport is the scripted report stage. As the shipped instructions
// ask of a reporting model, it puts the latest live check's observation
// under ライブ確認, or なし when its input holds no live check at all. It shows
// what reaches the report stage; it is not evidence that a model writes it.
func liveCheckReport(prompt string) string {
	live := liveCheckNone
	const heading = "\nRole verify, speaker live-check\n"
	if at := strings.LastIndex(prompt, heading); at >= 0 {
		record := prompt[at+len(heading):]
		if end := strings.Index(record, "\nRole "); end >= 0 {
			record = record[:end]
		}
		live = liveCheckObservation(record)
	}
	return "できるようになったこと\nサービスの挨拶が Hello 日本語 になりました。マージ前の変更で確かめたもので、本番ではありません。\n\n" +
		"観察できる結果\nsrc/greeting.txt: Hello 日本語\n\n単体テスト\n" + liveCheckUnitObservation + "\n\nライブ確認\n" + live + "\n"
}

// liveCheckSection is what a stored report says under ライブ確認.
func liveCheckSection(report string) string {
	_, section, found := strings.Cut(report, "\nライブ確認\n")
	if !found {
		return ""
	}
	return strings.TrimSpace(section)
}

// Actual subprocess fixture for the live check runs. Every stage but the live
// check is a scripted stand-in; the live check is whatever the test names.
func TestLiveCheckRoleHelper(t *testing.T) {
	role := os.Getenv("LIVE_CHECK_ROLE")
	if role == "" {
		return
	}
	prompt, err := io.ReadAll(os.Stdin)
	if err != nil || !bytes.Contains(prompt, []byte(liveCheckRequest)) {
		t.Fatal("lost original request", err)
	}
	write := func(path, text string) {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	switch role {
	case "elicit":
		fmt.Println("要件: サービスの挨拶を Hello 日本語 にする。起動したサービスがそれを返せば完了。")
	case "work":
		// The work stage can leave the greeting empty, which the project's
		// service answers with an error: once, or every time.
		greeting := liveCheckGreeting
		switch os.Getenv("LIVE_CHECK_WORK") {
		case "broken":
			greeting = ""
		case "fixed-second":
			marker := filepath.Join(os.Getenv("TASK_HOME"), "worked-once")
			if _, err := os.Stat(marker); os.IsNotExist(err) {
				greeting = ""
				write(marker, "")
			}
		}
		write("src/greeting.txt", greeting)
		fmt.Printf("src/greeting.txt に %q を書いた。\n", greeting)
	case "verify":
		switch os.Getenv("LIVE_CHECK_PROCESS") {
		case "live-check":
			// A stand-in for an installation's live check that fails, whatever it prints.
			fmt.Print(os.Getenv("LIVE_CHECK_STANDIN_OUTPUT"))
			os.Exit(1)
		case "project-tests":
			fmt.Println(liveCheckUnitObservation)
		default:
			fmt.Println("project build: nothing to compile")
		}
	case "review":
		fmt.Println("Review by fixture/reviewer: PASSED. Send-backs so far: 0 of at most 2.")
	case "deliver":
		write(os.Getenv("LIVE_CHECK_RECEIPT"), "delivered src/greeting.txt\n")
	case "verify_merged":
		fmt.Println("merged check (fixture): the delivered branch is not looked at in this test")
	case "report":
		// Keep what this launch was given, for the test to read.
		write(filepath.Join(os.Getenv("TASK_HOME"), "logs", fmt.Sprintf("input-%d.txt", time.Now().UnixNano())), string(prompt))
		report := liveCheckReport(string(prompt))
		write("report/result.md", report)
		if !slices.Contains(fixtureComments(t), report) {
			fixturePost(t, report)
		}
		fmt.Print(report)
	case "confirm_report":
		reported, err := os.ReadFile("report/result.md")
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(fixtureComments(t), string(reported)) {
			fmt.Println("no stored comment matches the reported text")
			os.Exit(1)
		}
	case "stop_report":
		fixturePost(t, "停止の指示に従って作業を止めました。\n")
	}
	os.Exit(0)
}

type liveCheckOptions struct {
	issue int
	// work is LIVE_CHECK_WORK for the work stage: "fixed", "broken" or "fixed-second".
	work string
	// live is the installation's live check, added to the verify stage; nil
	// supplies none. Its command runs after the workspace preparation.
	live *chain.Process
	// instructions is the operator's sentence about the live check.
	instructions string
	// project adds files to the project's repository, by path.
	project map[string]string
}

type liveCheckRun struct {
	t      *testing.T
	issue  int
	root   string
	cfg    config
	mu     sync.Mutex
	stored []string
	log    bytes.Buffer
	finish func()
}

// startLiveCheckRun runs one request through the shipped ordered example: a
// real queue, real processes, the real workspace preparation from a Git
// repository of the project, and the tracker and model services as stand-ins.
func startLiveCheckRun(t *testing.T, options liveCheckOptions) *liveCheckRun {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("the workspace preparation needs Python")
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("the workspace preparation needs Git")
	}
	prepare, err := filepath.Abs("../../harnesses/git_workspace.py")
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	git := func(directory string, args ...string) {
		t.Helper()
		arguments := []string{"-C", directory, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "core.hooksPath=/dev/null"}
		command := exec.Command(gitPath, append(arguments, args...)...)
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0"}
		if out, err := command.CombinedOutput(); err != nil {
			t.Fatalf("fixture Git %v: %v: %s", args, err, out)
		}
	}
	source := filepath.Join(base, "source")
	files := map[string]string{"src/greeting.txt": "Hello\n"}
	for path, content := range options.project {
		files[path] = content
	}
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(source, path)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, path), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git(source, "init", "--initial-branch=master")
	git(source, "add", "-A")
	git(source, "commit", "-m", "The project as the request finds it")
	target := filepath.Join(base, "project.git")
	git(base, "clone", "--bare", source, target)

	cfg := stagesExample(t)
	t.Setenv("MODEL_API_KEY", "synthetic-example-model")
	t.Setenv("TRACKER_API_KEY", "synthetic-example-tracker")
	t.Setenv("DELIVERY_GITHUB_TOKEN", "synthetic-example-delivery")
	cfg.Intake.ProjectID, cfg.Intake.CreatedSince = 17, "2026-01-02T00:00:00Z"
	if options.instructions != "" {
		cfg.Instructions += "\n" + options.instructions
	}
	for i := range cfg.Roles {
		if cfg.Roles[i].Name == "verify" && options.live != nil {
			cfg.Roles[i].Processes = append(cfg.Roles[i].Processes, *options.live)
		}
		for j := range cfg.Roles[i].Processes {
			p := &cfg.Roles[i].Processes[j]
			env := map[string]string{"TASK_REPOSITORY": target, "TASK_BRANCH": "master", "PYTHONDONTWRITEBYTECODE": "1",
				"LIVE_CHECK_ROLE": cfg.Roles[i].Name, "LIVE_CHECK_PROCESS": p.Name, "LIVE_CHECK_WORK": options.work}
			command := []string{binary, "-test.run=^TestLiveCheckRoleHelper$"}
			if cfg.Roles[i].Name == "verify" && options.live != nil && p.Name == options.live.Name {
				for name, value := range options.live.Env {
					env[name] = value
				}
				if len(options.live.Command) > 0 {
					command = options.live.Command
				}
			}
			if p.Receipt != "" {
				env["LIVE_CHECK_RECEIPT"] = p.Receipt
			}
			p.Command = append([]string{python, "-B", prepare, "--"}, command...)
			p.Env = env
		}
	}
	run := &liveCheckRun{t: t, issue: options.issue, root: filepath.Join(base, "queue"), cfg: cfg}
	key := fmt.Sprintf("EXAMPLE-%d", options.issue)
	var rows []any
	catalogs := 0
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		run.mu.Lock()
		defer run.mu.Unlock()
		if r.URL.Host == "tracker.example.invalid" {
			switch r.Method + " " + r.URL.Path {
			case "GET /api/v2/issues":
				return selectionReply(r, 200, []any{watchedIssue(options.issue, liveCheckRequest, "2026-01-03T00:00:00Z")}), nil
			case "GET /api/v2/issues/" + key + "/comments":
				return selectionReply(r, 200, append([]any{}, rows...)), nil
			case "POST /api/v2/issues/" + key + "/comments":
				if err := r.ParseForm(); err != nil {
					return nil, err
				}
				row := map[string]any{"id": len(rows) + 1, "issueId": options.issue, "projectId": 17, "content": r.Form.Get("content"), "createdUser": map[string]any{"id": 99}}
				rows, run.stored = append(rows, row), append(run.stored, r.Form.Get("content"))
				return selectionReply(r, 201, row), nil
			}
		}
		if r.URL.Host == "openrouter.ai" {
			switch r.URL.Path {
			case "/api/v1/models":
				catalogs++
				return selectionReply(r, 200, map[string]any{"data": []any{selectionModel(fmt.Sprintf("qwen/fixture-%d", catalogs)), selectionModel(fmt.Sprintf("z-ai/fixture-%d", catalogs))}}), nil
			case "/api/alpha/decisions":
				var input struct {
					Questions map[string]struct{ Criteria map[string]string }
				}
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					return nil, err
				}
				choice := fmt.Sprintf("qwen/fixture-%d", catalogs)
				if _, ok := input.Questions["next"].Criteria[choice]; !ok {
					choice = fmt.Sprintf("z-ai/fixture-%d", catalogs)
				}
				return selectionReply(r, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": choice}}}), nil
			case "/api/v1/chat/completions":
				// After elicitation the decision always goes on to the work.
				return routingSelectionReply(r, chain.Assignment{Role: "work"}), nil
			}
		}
		return nil, fmt.Errorf("unexpected fixture destination %s %s", r.Method, r.URL.Path)
	})
	run.start()
	return run
}

func (run *liveCheckRun) start() {
	run.finish = startStopQueue(run.t, run.cfg, run.root, 30*time.Millisecond, &run.log)
}

func (run *liveCheckRun) state() (chain.State, error) {
	return loadWatchState(run.root, run.issue)
}

// waitFor polls the saved history until the condition holds.
func (run *liveCheckRun) waitFor(what string, condition func(chain.State) bool) chain.State {
	run.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		state, err := run.state()
		if err == nil && condition(state) {
			return state
		}
		// A request that has ended changes no more.
		if err == nil && state.Done || time.Now().After(deadline) {
			run.finish()
			run.t.Fatalf("%s did not happen: stage=%s done=%t error=%v\n%s", what, state.Step, state.Done, err, run.log.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (run *liveCheckRun) comments() []string {
	run.mu.Lock()
	defer run.mu.Unlock()
	return slices.Clone(run.stored)
}

// home is a process's own directory in this request: TASK_HOME of the given
// role's process, as the queue numbers it.
func (run *liveCheckRun) home(role, process string) string {
	for i, r := range run.cfg.Roles {
		for j, p := range r.Processes {
			if r.Name == role && p.Name == process {
				return filepath.Join(run.root, "jobs", fmt.Sprint(run.issue), "homes", fmt.Sprintf("%d-%d", i, j))
			}
		}
	}
	run.t.Fatalf("no process %s/%s", role, process)
	return ""
}

// stageLaunch is one launch of a stage as the runtime recorded it: the
// results of its processes and the runtime's record that ends it.
type stageLaunch struct {
	role    string
	results []chain.Result
	record  chain.Result
}

func (launch stageLaunch) result(speaker string) (chain.Result, bool) {
	for _, result := range launch.results {
		if result.Speaker == speaker {
			return result, true
		}
	}
	return chain.Result{}, false
}

func stageLaunches(history []chain.Result) []stageLaunch {
	var launches []stageLaunch
	var current []chain.Result
	for _, result := range history {
		if result.Speaker == "runtime" {
			if strings.HasPrefix(result.Output, "Runtime record for stage ") {
				launches = append(launches, stageLaunch{role: result.Role, results: current, record: result})
			}
			current = nil
			continue
		}
		if result.Speaker != "requester" {
			current = append(current, result)
		}
	}
	return launches
}

// Not supplied and supplied but failing are different facts in the history,
// and only the first reaches the report as なし: a live check that does not
// exit 0 sends the work back to elicitation like any failing command, and the
// report stage is not launched over it, whatever the check printed.
func TestALiveCheckNotSuppliedAndOneThatFailsAreDifferentRecords(t *testing.T) {
	t.Run("not supplied", func(t *testing.T) {
		run := startLiveCheckRun(t, liveCheckOptions{issue: 71, work: "fixed",
			instructions: "Live verification method: none is supplied."})
		state := run.waitFor("the request ending", func(s chain.State) bool { return s.Done })
		run.finish()
		verified := 0
		for _, launch := range stageLaunches(state.History) {
			for _, result := range launch.results {
				if result.Speaker == "live-check" {
					t.Fatalf("a live check that was never supplied is in the history: %+v", result)
				}
			}
			if launch.role == "verify" {
				verified++
				if strings.Contains(launch.record.Output, "live-check") || !strings.Contains(launch.record.Output, "Process project-tests exited 0.") {
					t.Fatalf("the verify stage's record: %q", launch.record.Output)
				}
			}
		}
		reports := 0
		for _, comment := range run.comments() {
			if section := liveCheckSection(comment); section != "" {
				reports++
				if section != liveCheckNone {
					t.Fatalf("the report says %q under ライブ確認", section)
				}
			}
		}
		if verified != 1 || reports != 1 {
			t.Fatalf("verify ran %d times and %d reports were stored", verified, reports)
		}
	})
	for _, printed := range []struct{ name, output string }{
		{"supplied and failing", "合否: 不合格\n後始末: 起動したプロセスを止めた\n"},
		{"supplied and failing while it writes none and a pass", liveCheckNone + "\n合否: 合格\n"},
	} {
		t.Run(printed.name, func(t *testing.T) {
			live := chain.Process{Name: "live-check", Env: map[string]string{"LIVE_CHECK_STANDIN_OUTPUT": printed.output}}
			run := startLiveCheckRun(t, liveCheckOptions{issue: 72, work: "fixed", live: &live,
				instructions: "Live verification method: the verify stage's live-check process."})
			failed := func(s chain.State) int {
				count := 0
				for _, launch := range stageLaunches(s.History) {
					if launch.role == "verify" {
						count++
					}
				}
				return count
			}
			state := run.waitFor("two failed verify launches", func(s chain.State) bool {
				launches := stageLaunches(s.History)
				return failed(s) >= 2 && launches[len(launches)-1].role != "verify"
			})
			run.finish()
			launches := stageLaunches(state.History)
			for i, launch := range launches {
				switch launch.role {
				case "verify":
					check, found := launch.result("live-check")
					if !found || check.Error == "" || check.Output != printed.output {
						t.Fatalf("the failing live check's own record: %+v", check)
					}
					if !strings.Contains(launch.record.Output, "Process live-check did not exit 0: exit status 1") {
						t.Fatalf("the verify stage's record: %q", launch.record.Output)
					}
					if i+1 < len(launches) && launches[i+1].role != "elicit" {
						t.Fatalf("after a failed live check the run went to %s, not elicitation", launches[i+1].role)
					}
				case "review", "deliver", "verify_merged", "report", "confirm_report":
					t.Fatalf("the %s stage ran although every live check failed", launch.role)
				}
			}
			for _, comment := range run.comments() {
				if strings.Contains(comment, "ライブ確認") {
					t.Fatalf("a report was stored over a failing live check: %q", comment)
				}
			}
			if state.Done {
				t.Fatal("the request ended over a failing live check")
			}
		})
	}
}

// documentedLiveCheck is the process deploy/ticket-engine/SETUP.md tells an
// installation to append to the verify stage, read from the guide itself and
// held to the rules the engine reads a configuration by.
func documentedLiveCheck(t *testing.T) chain.Process {
	t.Helper()
	guide, err := os.ReadFile("../../../deploy/ticket-engine/SETUP.md")
	if err != nil {
		t.Fatal(err)
	}
	_, rest, found := strings.Cut(string(guide), "<!-- setup-live-check-process -->\n```json\n")
	if !found {
		t.Fatal("SETUP.md no longer shows the live check's process")
	}
	block, _, found := strings.Cut(rest, "\n```\n")
	if !found {
		t.Fatal("the live check's process in SETUP.md does not end")
	}
	if err := checkKeys(json.NewDecoder(strings.NewReader(block)), reflect.TypeOf(chain.Process{}), &[]string{}); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(strings.NewReader(block))
	decoder.DisallowUnknownFields()
	var process chain.Process
	if err := decoder.Decode(&process); err != nil {
		t.Fatal(err)
	}
	return process
}

// The shipped ordered examples supply no live check, so an installation
// without one is not failed on every request by a command that is not there.
// The process the guide shows joins their verify stage as it is: the engine
// accepts the run, the process launches no model, holds no credential, writes
// nothing in the checkout, starts no shell and is launched like the tests.
func TestTheDocumentedLiveCheckJoinsTheShippedVerifyStage(t *testing.T) {
	documented := documentedLiveCheck(t)
	for _, name := range []string{"operator-stages", "operator-github-stages"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile("../../examples/" + name + ".json")
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := readConfig(data)
			if err != nil {
				t.Fatal(err)
			}
			verify := slices.IndexFunc(cfg.Roles, func(role chain.Role) bool { return role.Name == "verify" })
			if verify < 0 {
				t.Fatal("the example has no verify role")
			}
			var shipped []string
			for _, process := range cfg.Roles[verify].Processes {
				shipped = append(shipped, process.Name)
			}
			if !slices.Equal(shipped, []string{"project-build", "project-tests"}) {
				t.Fatalf("the shipped verify stage runs %v", shipped)
			}
			tests := cfg.Roles[verify].Processes[1]
			launcher := tests.Command[:len(tests.Command)-1]
			if len(documented.Command) <= len(launcher) || !slices.Equal(documented.Command[:len(launcher)], launcher) {
				t.Fatalf("the live check is not launched like the tests: %q", documented.Command)
			}
			if !strings.HasPrefix(documented.Command[len(launcher)], "/opt/ticket-automation/operator/") {
				t.Fatalf("the live check's program is not an operator script: %q", documented.Command[len(launcher)])
			}
			for _, argument := range documented.Command {
				if slices.Contains([]string{"--write", "--create", "/bin/sh", "-c"}, argument) {
					t.Fatalf("the live check's command carries %q", argument)
				}
			}
			if documented.ModelEnv != "" || len(documented.Secrets) != 0 || documented.TrackerAccess != "" || documented.Receipt != "" || documented.PromptArgument {
				t.Fatalf("the live check asks for more than a command: %+v", documented)
			}
			for _, key := range []string{"TASK_REPOSITORY", "TASK_BRANCH"} {
				if documented.Env[key] != tests.Env[key] {
					t.Fatalf("%s differs from the tests': %q", key, documented.Env[key])
				}
			}
			if documented.HistoryEnvironmentConflict() || documented.Env["TASK_HOME"] != "" || documented.Env["TASK_WORKSPACE"] != "" || documented.Env["TASK_ISSUE"] != "" {
				t.Fatal("the live check sets a name the runtime owns")
			}
			if documented.TimeoutMinutes <= 0 {
				t.Fatal("the live check has no time limit, so a service that stops answering holds the stage")
			}
			cfg.Roles[verify].Processes = append(cfg.Roles[verify].Processes, documented)
			stages := slices.Clone(cfg.Workflow.Stages)
			if err := prepareStages(&cfg); err != nil {
				t.Fatal(err)
			}
			purposes := map[string]string{}
			for _, role := range cfg.Roles {
				purposes[role.Name] = role.Purpose
			}
			if err := cfg.Workflow.Validate(purposes); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(cfg.Workflow.Stages, stages) {
				t.Fatal("adding the live check changed the ordered run")
			}
		})
	}
}

const liveCheckTestUser = "live-check-user"
const liveCheckMethod = "Live verification method: the verify stage's live-check process starts server.py from the checkout on 127.0.0.1, uses the greeting as the test user live-check-user following docs/FEATURES.md, and prints the request, the response and the result. It checks the change before merge, not production."

// liveCheckExample is the guide's live check with rewrite/examples/live-check
// as its program, and the project that example checks: the fictional service
// and its Feature Map, where they sit in the project's repository.
func liveCheckExample(t *testing.T, env map[string]string) (*chain.Process, map[string]string) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("the example live check needs Python")
	}
	example, err := filepath.Abs("../../examples/live-check")
	if err != nil {
		t.Fatal(err)
	}
	process := documentedLiveCheck(t)
	program := -1
	for i, argument := range process.Command {
		if argument == "--" {
			program = i + 1
		}
	}
	if program < 0 || program >= len(process.Command) {
		t.Fatalf("no program in the guide's command: %q", process.Command)
	}
	// The launcher stays out (it needs Linux namespaces); the arguments after
	// the guide's program are passed to the example as written.
	process.Command = append([]string{python, "-B", filepath.Join(example, "verify_feature.py")}, process.Command[program+1:]...)
	process.Env = map[string]string{"LIVE_CHECK_TEST_USER": liveCheckTestUser}
	for name, value := range env {
		process.Env[name] = value
	}
	project := map[string]string{}
	for path, name := range map[string]string{"server.py": "server.py", "docs/FEATURES.md": "FEATURES.md"} {
		content, err := os.ReadFile(filepath.Join(example, name))
		if err != nil {
			t.Fatal(err)
		}
		project[path] = string(content)
	}
	return &process, project
}

func liveCheckWaitForPath(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not appear", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// liveCheckLaunchDirectories are the directories the example check made in
// its home, one per launch, oldest first.
func liveCheckLaunchDirectories(t *testing.T, home string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(home, "logs", "live-check"))
	if err != nil {
		t.Fatal(err)
	}
	var directories []string
	for _, entry := range entries {
		if entry.IsDir() {
			directories = append(directories, filepath.Join(home, "logs", "live-check", entry.Name()))
		}
	}
	return directories
}

func liveCheckRecord(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return record
}

// liveCheckLeftNothing reads one launch's directory: its stop is recorded as
// complete, the service it started is gone, and its evidence is still there.
func liveCheckLeftNothing(t *testing.T, launch string) {
	t.Helper()
	if stopped := liveCheckRecord(t, filepath.Join(launch, "stopped.json")); stopped["ok"] != true {
		t.Fatalf("%s: the stop is not recorded as complete: %v", launch, stopped)
	}
	pid, ok := liveCheckRecord(t, filepath.Join(launch, "state.json"))["pid"].(float64)
	if !ok || pid <= 0 {
		t.Fatalf("%s: no process recorded", launch)
	}
	if err := syscall.Kill(int(pid), 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("%s: the service %d the check started is still there (%v)", launch, int(pid), err)
	}
	for _, name := range []string{"request.txt", "service.log", "observation.json"} {
		if _, err := os.Stat(filepath.Join(launch, name)); err != nil {
			t.Fatalf("%s: evidence %s is gone: %v", launch, name, err)
		}
	}
}

// liveCheckUsers are the test users left in the fictional service's store; a
// store already removed with the finished request's home holds none.
func liveCheckUsers(t *testing.T, home string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(home, "live-check-store", "users"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var users []string
	for _, entry := range entries {
		users = append(users, entry.Name())
	}
	return users
}

func liveCheckReportSections(run *liveCheckRun) []string {
	var sections []string
	for _, comment := range run.comments() {
		if section := liveCheckSection(comment); section != "" {
			sections = append(sections, section)
		}
	}
	return sections
}

// liveCheckFailuresGoBack holds the order of the run: every verify launch
// whose live check did not exit 0 is followed by elicitation, and every
// launch of a later stage follows a verify launch whose check exited 0.
func liveCheckFailuresGoBack(t *testing.T, launches []stageLaunch) (checks []chain.Result) {
	t.Helper()
	passed := false
	for i, launch := range launches {
		switch launch.role {
		case "verify":
			check, found := launch.result("live-check")
			if !found {
				t.Fatal("a verify launch ran without the live check")
			}
			checks = append(checks, check)
			passed = check.Error == ""
			if !passed && i+1 < len(launches) && launches[i+1].role != "elicit" {
				t.Fatalf("after a live check that did not exit 0 the run went to %s, not elicitation", launches[i+1].role)
			}
		case "review", "deliver", "verify_merged", "report", "confirm_report":
			if !passed {
				t.Fatalf("the %s stage ran after a live check that did not exit 0", launch.role)
			}
		}
	}
	return checks
}

// One request through the shipped ordered run with the example live check
// (rewrite/examples/live-check) in its verify stage. The work leaves the
// greeting empty the first time: the service answers 500, the check cleans
// up and ends 1, and the work goes back to elicitation. The second time the
// check sends its request over HTTP and observes the greeting, and the report
// stage puts that observation, with the request and the answer, under
// ライブ確認. The report stage is scripted (liveCheckReport): this shows what
// reaches it, not that a model copies it.
func TestALiveCheckObservationReachesTheReportAsObserved(t *testing.T) {
	live, project := liveCheckExample(t, nil)
	run := startLiveCheckRun(t, liveCheckOptions{issue: 73, work: "fixed-second", live: live, project: project, instructions: liveCheckMethod})
	// The failed launch as it was saved when it happened.
	failedAt, failed := -1, chain.Result{}
	run.waitFor("a live check that did not exit 0", func(s chain.State) bool {
		for i, result := range s.History {
			if result.Speaker == "live-check" && result.Error != "" {
				failedAt, failed = i, result
				return true
			}
		}
		return false
	})
	state := run.waitFor("the request ending", func(s chain.State) bool { return s.Done })
	// Once the request has ended the queue trims its homes.
	liveCheckWaitForPath(t, filepath.Join(run.root, "jobs", fmt.Sprint(run.issue), "homes", ".trimmed"))
	run.finish()

	if !reflect.DeepEqual(state.History[failedAt], failed) {
		t.Fatalf("the failed live check's record changed after it was saved:\nthen %+v\nnow  %+v", failed, state.History[failedAt])
	}
	for _, phrase := range []string{"要求: GET http://127.0.0.1:", "観測: 状態 500", "合否: 不合格", "後始末: サービス", "結果: 1 で終わる"} {
		if !strings.Contains(failed.Output, phrase) {
			t.Fatalf("the failed live check does not say %q:\n%s", phrase, failed.Output)
		}
	}
	checks := liveCheckFailuresGoBack(t, stageLaunches(state.History))
	if len(checks) != 2 || checks[0].Error == "" || checks[1].Error != "" {
		t.Fatalf("live checks: %+v", checks)
	}
	observation := liveCheckObservation(checks[1].Output)
	for _, phrase := range []string{"要求: GET http://127.0.0.1:", "観測: 状態 200、本文「Hello 日本語, " + liveCheckTestUser + "」", "合否: 合格", "結果: 0 で終わる"} {
		if !strings.Contains(observation, phrase) {
			t.Fatalf("the passing live check does not say %q:\n%s", phrase, observation)
		}
	}
	// The report carries exactly what the check observed, not なし.
	if sections := liveCheckReportSections(run); len(sections) != 1 || sections[0] != observation {
		t.Fatalf("ライブ確認 in the stored report is not what the check observed:\nreport: %q\ncheck:  %q", sections, observation)
	}
	// The report stage was given the failed launch as well.
	inputs, err := filepath.Glob(filepath.Join(run.home("report", "reporter"), "logs", "input-*.txt"))
	if err != nil || len(inputs) != 1 {
		t.Fatalf("report inputs: %v %v", inputs, err)
	}
	input, err := os.ReadFile(inputs[0])
	if err != nil || !bytes.Contains(input, []byte(failed.Output)) || !bytes.Contains(input, []byte("Process live-check did not exit 0")) {
		t.Fatal("the report stage was not given the failed live check", err)
	}
	// The request reached the service: the service's own log has it.
	_, request, _ := strings.Cut(observation, "要求: GET http://127.0.0.1:")
	request, _, _ = strings.Cut(request, "\n")
	_, path, found := strings.Cut(request, "/")
	home := run.home("verify", "live-check")
	directories := liveCheckLaunchDirectories(t, home)
	if !found || len(directories) != 2 {
		t.Fatalf("request %q, launches %v", request, directories)
	}
	served, err := os.ReadFile(filepath.Join(directories[1], "service.log"))
	if err != nil || !bytes.Contains(served, []byte(`"GET /`+path+` HTTP/1.1" 200`)) {
		t.Fatalf("the service did not log the check's request /%s: %s %v", path, served, err)
	}
	for _, directory := range directories {
		liveCheckLeftNothing(t, directory)
	}
	// The finished request's home kept the evidence under logs and lost the
	// rest, the fictional service's store with it.
	if _, err := os.Stat(filepath.Join(home, "live-check-store")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the check's store outlived the trim: %v", err)
	}
	t.Logf("ライブ確認 as stored:\n%s", observation)
}

// The engine stops while the check waits for the service's answer: the
// check's process group gets SIGTERM, the example check cleans up before it
// ends, and its launch is recorded as interrupted. Started again, the engine
// sends the stopped verify stage back to elicitation as it does a failure,
// the check runs again, and the request reaches its report.
func TestALiveCheckStoppedWithTheEngineCleansUpAndRunsAgain(t *testing.T) {
	hold := filepath.Join(t.TempDir(), "hold")
	if err := os.WriteFile(hold, nil, 0600); err != nil {
		t.Fatal(err)
	}
	live, project := liveCheckExample(t, map[string]string{"GREETING_HOLD_FILE": hold})
	run := startLiveCheckRun(t, liveCheckOptions{issue: 74, work: "fixed", live: live, project: project, instructions: liveCheckMethod})
	liveCheckWaitForPath(t, hold+".waiting")
	stopping := time.Now()
	run.finish()
	t.Logf("the engine stopped %v after it was told to", time.Since(stopping).Round(time.Millisecond))
	home := run.home("verify", "live-check")
	directories := liveCheckLaunchDirectories(t, home)
	if len(directories) != 1 {
		t.Fatalf("launches: %v", directories)
	}
	liveCheckLeftNothing(t, directories[0])
	if users := liveCheckUsers(t, home); len(users) != 0 {
		t.Fatalf("test users left behind: %v", users)
	}
	state, err := run.state()
	if err != nil {
		t.Fatal(err)
	}
	var stopped chain.Result
	for _, result := range state.History {
		if result.Speaker == "live-check" {
			stopped = result
		}
	}
	if !stopped.Interrupted || !strings.Contains(stopped.Output, "観測: 応答なし (中断された") || !strings.Contains(stopped.Output, "後始末: サービス") {
		t.Fatalf("the stopped check's record: %+v", stopped)
	}
	if state.Pending == nil || state.Pending.Role != "verify" {
		t.Fatalf("the stopped verify stage is not left pending: %+v", state.Pending)
	}
	if err := os.Remove(hold); err != nil {
		t.Fatal(err)
	}
	run.start()
	state = run.waitFor("the request ending after the restart", func(s chain.State) bool { return s.Done })
	run.finish()
	checks := liveCheckFailuresGoBack(t, stageLaunches(state.History))
	if len(checks) != 2 || !checks[0].Interrupted || checks[1].Error != "" {
		t.Fatalf("live checks: %+v", checks)
	}
	if sections := liveCheckReportSections(run); len(sections) != 1 || sections[0] != liveCheckObservation(checks[1].Output) {
		t.Fatalf("ライブ確認 in the stored report: %q", sections)
	}
	directories = liveCheckLaunchDirectories(t, home)
	if len(directories) != 2 {
		t.Fatalf("launches: %v", directories)
	}
	for _, directory := range directories {
		liveCheckLeftNothing(t, directory)
	}
}

// The check's cleanup fails after a passing observation: the test makes the
// fictional service's user store read-only while the check waits for its
// answer. The check ends 1 although the feature worked, the work goes back to
// elicitation, and the next launch removes what the earlier one left before
// it passes.
func TestALiveCheckWhoseCleanupFailsSendsTheWorkBack(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes files from a directory it may not write")
	}
	hold := filepath.Join(t.TempDir(), "hold")
	if err := os.WriteFile(hold, nil, 0600); err != nil {
		t.Fatal(err)
	}
	live, project := liveCheckExample(t, map[string]string{"GREETING_HOLD_FILE": hold})
	run := startLiveCheckRun(t, liveCheckOptions{issue: 75, work: "fixed", live: live, project: project, instructions: liveCheckMethod})
	liveCheckWaitForPath(t, hold+".waiting")
	home := run.home("verify", "live-check")
	users := filepath.Join(home, "live-check-store", "users")
	if err := os.Chmod(users, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(users, 0700) })
	if err := os.Remove(hold); err != nil {
		t.Fatal(err)
	}
	var failed chain.Result
	run.waitFor("the check to fail on its cleanup", func(s chain.State) bool {
		for _, result := range s.History {
			if result.Speaker == "live-check" && result.Error != "" {
				failed = result
				return true
			}
		}
		return false
	})
	if err := os.Chmod(users, 0700); err != nil {
		t.Fatal(err)
	}
	state := run.waitFor("the request ending", func(s chain.State) bool { return s.Done })
	run.finish()
	for _, phrase := range []string{"合否: 合格", "後始末: 失敗", "結果: 1 で終わる"} {
		if !strings.Contains(failed.Output, phrase) {
			t.Fatalf("the check whose cleanup failed does not say %q:\n%s", phrase, failed.Output)
		}
	}
	checks := liveCheckFailuresGoBack(t, stageLaunches(state.History))
	last := checks[len(checks)-1]
	if len(checks) < 2 || checks[0].Error == "" || last.Error != "" || !strings.Contains(last.Output, "前の起動の後始末") {
		t.Fatalf("live checks: %+v", checks)
	}
	if left := liveCheckUsers(t, home); len(left) != 0 {
		t.Fatalf("test users left behind: %v", left)
	}
	for _, directory := range liveCheckLaunchDirectories(t, home) {
		liveCheckLeftNothing(t, directory)
	}
}
