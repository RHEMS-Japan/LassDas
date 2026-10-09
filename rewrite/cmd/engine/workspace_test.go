package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ticket-runner/internal/chain"
)

// This checks the saved-answer boundary through actual processes and history.
// It does not claim that a live tracker or model supplied the fixture reply.
func TestSavedEntranceReplyRefreshesWorkspaceAndKeepsDiagnosticInHistory(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("workspace launcher needs Python")
	}
	launcher, err := filepath.Abs("../../harnesses/git_workspace.py")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	source, workspace := filepath.Join(root, "source"), filepath.Join(root, "workspace")
	for _, directory := range []string{source, workspace} {
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	environmentGit(t, source, "init", "--initial-branch=main")
	environmentWrite(t, filepath.Join(source, "entry.txt"), []byte("before the reply\n"))
	environmentGit(t, source, "add", "entry.txt")
	environmentGit(t, source, "commit", "-m", "Codex: fixture before reply")
	head := func(directory string) string {
		// The shared Git helper includes platform warnings from stderr.
		output := environmentGit(t, directory, "rev-parse", "HEAD")
		return output[strings.LastIndex(output, "\n")+1:]
	}
	before := head(source)
	store, err := chain.Open(filepath.Join(root, "run"), "Check the requested result without changing its scope.")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	process := chain.Process{Name: "fixture", Directory: workspace,
		Command: []string{python, "-B", launcher, "--", "/bin/cat"},
		Env: map[string]string{"TASK_WORKSPACE": workspace, "TASK_REPOSITORY": source,
			"TASK_BRANCH": "main", "GIT_ALLOW_PROTOCOL": "file"}}
	verify := process
	verify.Command = []string{python, "-B", launcher, "--", "git", "show", "HEAD:entry.txt"}
	roles := map[string]chain.Role{
		"understand": {Name: "understand", Processes: []chain.Process{process}},
		"verify":     {Name: "verify", Processes: []chain.Process{verify}},
		"question":   {Name: "question", Processes: []chain.Process{{Name: "fixture", Command: []string{"/bin/echo", "Use the recommended target?"}}}},
	}
	cfg := watchConfiguration(t)
	useWatchTransport(t, func(r *http.Request) (*http.Response, error) {
		var body struct{ State chain.State }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return nil, err
		}
		choice := "question"
		for _, result := range body.State.History {
			if result.Speaker == "requester" {
				choice = "verify"
			}
		}
		return selectionReply(r, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": choice}}}), nil
	})
	engine := chain.Chain{Store: store, WaitAfter: "question",
		Workflow: &chain.Workflow{Question: "question", Stages: []chain.Stage{
			{Name: "understand", Kind: chain.ModelStage}, {Name: "verify", Kind: chain.CommandStage, OnFailure: "understand"}}},
		Router: chain.StageRouter{Entrance: chain.DecisionRouter{Judge: cfg.Router.Decision,
			Roles: map[string]string{"understand": "Settle the request", "verify": "Check the tree", "question": "Ask the requester"}}},
		Executor: chain.Processes{Roles: roles, HistoryPath: filepath.Join(store.Dir, "history.json")}}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := engine.Run(ctx); !errors.Is(err, chain.ErrWaiting) {
		t.Fatalf("entrance did not wait: %v", err)
	}
	environmentWrite(t, filepath.Join(source, "entry.txt"), []byte("after the reply\n"))
	environmentGit(t, source, "commit", "-am", "Codex: fixture source advanced")
	after := head(source)
	state, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	state.Waiting = false
	state.History = append(state.History, chain.Result{Role: "question", Speaker: "requester", Output: "Use the recommendation."})
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
	if err := engine.Run(ctx); err != nil {
		t.Fatal(err)
	}
	state, err = store.Load()
	if err != nil || !state.Done || head(workspace) != after {
		t.Fatalf("resumed chain did not verify the new tree: done=%v error=%v", state.Done, err)
	}
	notices, checked := 0, false
	for _, result := range state.History {
		notices += strings.Count(result.Diagnostics, "Workspace updated from "+before+" to "+after)
		if result.Role == "verify" && result.Speaker == "fixture" && result.Error == "" && result.Output == "after the reply\n" {
			checked = true
		}
	}
	if notices != 1 || !checked {
		for _, result := range state.History {
			t.Logf("role=%s speaker=%s diagnostics=%q error=%q", result.Role, result.Speaker, result.Diagnostics, result.Error)
		}
		t.Fatalf("the saved result lost the update or verification: notices=%d verified=%v", notices, checked)
	}
}

// The existing configured command is enough to prepare work. This exercises
// actual intake, job binding, Git, cancellation and restart; model/tracker
// replies are fixtures, not evidence of autonomous task judgment or delivery.
func TestWatchGitLauncherKeepsTwoRequestsWorkThroughRestart(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("workspace launcher needs Python")
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("workspace launcher needs Git")
	}
	launcher, err := filepath.Abs("../../harnesses/git_workspace.py")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	source := filepath.Join(root, "source with spaces")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	runGit := func(directory string, args ...string) string {
		t.Helper()
		arguments := []string{"-C", directory, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "core.hooksPath=/dev/null"}
		command := exec.Command(git, append(arguments, args...)...)
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0"}
		out, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("fixture Git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGit(source, "init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(source, "entry.txt"), []byte("upstream original\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runGit(source, "add", "entry.txt")
	runGit(source, "commit", "-m", "Codex: fixture original")
	initialHead := runGit(source, "rev-parse", "HEAD")
	cfg := watchConfiguration(t)
	cfg.Roles[0].Processes[0].Command = []string{python, launcher, "--", "/bin/sh", "-c", `
cat > latest-prompt.txt
if test -f ongoing.txt; then
  printf 'Resumed %s using retained work\n' "$TASK_ISSUE"
  exit 0
fi
printf 'unfinished edit for %s\n' "$TASK_ISSUE" > entry.txt
printf 'untracked progress for %s\n' "$TASK_ISSUE" > ongoing.txt
exec sleep 60
`}
	cfg.Roles[0].Processes[0].Env = map[string]string{"TASK_REPOSITORY": source, "TASK_BRANCH": "main", "GIT_ALLOW_PROTOCOL": "file", "PYTHONDONTWRITEBYTECODE": "1"}
	const original = "元の条件\n$(literal) `not a shell command`"
	var mu sync.Mutex
	sawInterrupted := map[string]bool{}
	useWatchTransport(t, func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Host == "watch-tracker.example" {
			return selectionReply(r, 200, []any{watchedIssue(41, original, "2026-01-03T00:00:00Z"), watchedIssue(42, original, "2026-01-03T00:00:01Z")}), nil
		}
		var input struct{ State chain.State }
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			return nil, err
		}
		if !strings.Contains(input.State.Request, original) {
			t.Error("accepted original request changed")
		}
		choice := "implement"
		for _, result := range input.State.History {
			if result.Speaker == "runtime" && strings.Contains(result.Error, "may have taken effect") {
				sawInterrupted[input.State.Request] = true
			}
			if strings.HasPrefix(result.Output, "Resumed EXAMPLE-") {
				choice = "done"
			}
		}
		return selectionReply(r, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": choice}}}), nil
	})
	queue := filepath.Join(root, "queue")
	workspace := func(id int) string { return filepath.Join(queue, "jobs", strconv.Itoa(id), "workspace") }
	start := func() (context.CancelFunc, <-chan error) {
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			result <- watchRequests(ctx, cfg, queue, io.Discard)
		}()
		t.Cleanup(func() {
			cancel()
			select {
			case <-finished:
			case <-time.After(7 * time.Second):
				t.Error("test cleanup could not reap its watch process")
			}
		})
		return cancel, result
	}
	stop := func(cancel context.CancelFunc, result <-chan error) {
		t.Helper()
		cancel()
		select {
		case err := <-result:
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		case <-time.After(7 * time.Second):
			t.Fatal("watch failed to reap the configured launchers")
		}
	}
	cancel, result := start()
	defer cancel()
	waitFor(t, func() bool {
		for _, id := range []int{41, 42} {
			data, err := os.ReadFile(filepath.Join(workspace(id), "ongoing.txt"))
			if err != nil || !strings.Contains(string(data), "EXAMPLE-"+strconv.Itoa(id)) {
				return false
			}
		}
		return true
	})
	stop(cancel, result)
	// New upstream work and even an unavailable configured source must not cause
	// an already-started task to reset, refetch or replace its unfinished files.
	if err := os.WriteFile(filepath.Join(source, "entry.txt"), []byte("new upstream\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runGit(source, "commit", "-am", "Codex: fixture upstream moved")
	cfg.Roles[0].Processes[0].Env["TASK_REPOSITORY"] = filepath.Join(root, "unavailable-source")
	cancel, result = start()
	defer cancel()
	waitFor(t, func() bool {
		a, e1 := loadWatchState(queue, 41)
		b, e2 := loadWatchState(queue, 42)
		return e1 == nil && e2 == nil && a.Done && b.Done
	})
	stop(cancel, result)
	for _, id := range []int{41, 42} {
		dir := workspace(id)
		if head := runGit(dir, "rev-parse", "HEAD"); head != initialHead {
			t.Fatal("restart changed the request's repository base")
		}
		for name, want := range map[string]string{"entry.txt": "unfinished edit for EXAMPLE-" + strconv.Itoa(id) + "\n", "ongoing.txt": "untracked progress for EXAMPLE-" + strconv.Itoa(id) + "\n"} {
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil || string(data) != want {
				t.Fatalf("%s was replaced or crossed request boundaries: %q %v", name, data, err)
			}
		}
		prompt, err := os.ReadFile(filepath.Join(dir, "latest-prompt.txt"))
		if err != nil || !strings.Contains(string(prompt), original) {
			t.Fatal("original prose did not reach the resumed process")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sawInterrupted) != 2 {
		t.Fatal("resume lost the observations of interrupted work")
	}
	if got, _ := os.ReadFile(filepath.Join(source, "entry.txt")); string(got) != "new upstream\n" {
		t.Fatal("request work mutated the source fixture")
	}
}
