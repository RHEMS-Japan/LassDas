package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"ticket-runner/internal/chain"
	"ticket-runner/internal/tracker"
)

func watchConfiguration(t *testing.T) config {
	t.Helper()
	t.Setenv("WATCH_TEST_KEY", "synthetic-watch-key")
	var cfg config
	cfg.Router.Mode = "jev"
	cfg.Router.Decision = chain.Jev{URL: "https://watch-model.example/decisions", Model: "fixture-router", KeyEnv: "WATCH_TEST_KEY"}
	cfg.Backlog = tracker.Backlog{BaseURL: "https://watch-tracker.example/api/v2", KeyEnv: "WATCH_TEST_KEY"}
	cfg.Intake = &intakeConfig{ProjectID: 17, CreatedSince: "2026-01-02T00:00:00Z", PollIntervalSeconds: 1, MaxRunning: 2}
	cfg.Roles = []chain.Role{{Name: "implement", Processes: []chain.Process{{Name: "worker", Command: []string{"/bin/sh", "-c", `cat > received.txt; pwd > actual-directory.txt; printf '%s\n' "$TASK_HOME" "$TASK_ISSUE" > actual-context.txt; printf 'work returned in ordinary prose\n'`}}}}}
	return cfg
}

func watchedIssue(id int, description, created string) map[string]any {
	return map[string]any{"id": id, "projectId": 17, "issueKey": fmt.Sprintf("EXAMPLE-%d", id), "summary": "Original title", "description": description, "created": created, "createdUser": map[string]any{"id": 55}, "unknown": true}
}

func useWatchTransport(t *testing.T, fn roundTripFunc) {
	t.Helper()
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "watch-tracker.example" && strings.HasSuffix(r.URL.Path, "/comments") {
			return selectionReply(r, 200, []any{}), nil
		}
		return fn(r)
	})
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("expected experiment state did not arrive")
}

func loadWatchState(root string, id int) (chain.State, error) {
	var state chain.State
	data, err := os.ReadFile(filepath.Join(root, "jobs", fmt.Sprint(id), "run", "history.json"))
	if err == nil {
		err = json.Unmarshal(data, &state)
	}
	return state, err
}

func TestWatchCLICollectsNewIssuesOnceIntoSeparateWorkingDirectories(t *testing.T) {
	cfg := watchConfiguration(t)
	root := t.TempDir()
	const original = "Original conditions\n日本語 and $(literal) `prose`"
	var mu sync.Mutex
	scans := 0
	modelCalls := map[string]int{}
	useWatchTransport(t, func(request *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		if request.URL.Host == "watch-tracker.example" {
			scans++
			if request.Method != "GET" || request.URL.Query().Get("projectId[]") != "17" {
				t.Error("unscoped intake")
			}
			body := original
			if scans > 1 {
				body = "later remote edit, not the accepted original"
			}
			rows := []any{watchedIssue(10, "old task must not start", "2026-01-01T00:00:00Z"), watchedIssue(11, body, "2026-01-03T00:00:00Z")}
			if scans > 1 {
				rows = append(rows, watchedIssue(12, "second request", "2026-01-03T00:00:01Z"))
			}
			return selectionReply(request, 200, rows), nil
		}
		var input struct{ State chain.State }
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			return nil, err
		}
		modelCalls[input.State.Request]++
		choice := "implement"
		if len(input.State.History) == 1 {
			choice = "done"
		}
		return selectionReply(request, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": choice}}}), nil
	})
	data, _ := json.Marshal(cfg)
	configPath := filepath.Join(root, "operator.json")
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	var log bytes.Buffer
	go func() {
		result <- run(ctx, []string{"--config", configPath, "--watch", "--run-dir", root}, io.Discard, &log)
	}()
	waitFor(t, func() bool {
		a, e1 := loadWatchState(root, 11)
		b, e2 := loadWatchState(root, 12)
		return e1 == nil && e2 == nil && a.Done && b.Done
	})
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return scans >= 3 })
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("watch ended unexpectedly: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "jobs", "10")); !os.IsNotExist(err) {
		t.Fatal("historical issue was accepted")
	}
	for _, id := range []int{11, 12} {
		state, err := loadWatchState(root, id)
		if err != nil || !state.Done || len(state.History) != 1 {
			t.Fatalf("bad request history: %#v %v", state, err)
		}
		if id == 11 && !strings.HasSuffix(state.Request, original) {
			t.Fatal("remote edit replaced original request")
		}
		// Inspect the intake snapshot too: an unchanged engine history could
		// otherwise hide a replaced source that blocks the next restart.
		raw, err := os.ReadFile(filepath.Join(root, "jobs", fmt.Sprint(id), "issue.json"))
		var snapshot struct {
			Description string
			Unknown     bool
		}
		if err != nil || json.Unmarshal(raw, &snapshot) != nil {
			t.Fatalf("could not read the accepted source: %v", err)
		}
		wantDescription := original
		if id == 12 {
			wantDescription = "second request"
		}
		if snapshot.Description != wantDescription || !snapshot.Unknown {
			t.Fatalf("accepted native record was replaced: %s", raw)
		}
		if count := strings.Count(log.String(), fmt.Sprintf("starting accepted request %d\n", id)); count != 1 {
			t.Fatalf("completed engine was launched again: %d launches for %d", count, id)
		}
		workspace := filepath.Join(root, "jobs", fmt.Sprint(id), "workspace")
		received, err := os.ReadFile(filepath.Join(workspace, "received.txt"))
		if err != nil || !strings.Contains(string(received), state.Request) {
			t.Fatalf("child missed original: %s %v", received, err)
		}
		cwd, _ := os.ReadFile(filepath.Join(workspace, "actual-directory.txt"))
		resolved, _ := filepath.EvalSymlinks(workspace)
		if strings.TrimSpace(string(cwd)) != resolved {
			t.Fatalf("shared work directory: %s", cwd)
		}
		contextText, _ := os.ReadFile(filepath.Join(workspace, "actual-context.txt"))
		if string(contextText) != filepath.Join(root, "jobs", fmt.Sprint(id), "homes", "0-0")+"\nEXAMPLE-"+fmt.Sprint(id)+"\n" {
			t.Fatalf("wrong request context: %s", contextText)
		}
		mu.Lock()
		calls := modelCalls[state.Request]
		mu.Unlock()
		if calls != 2 {
			t.Fatalf("request executed repeatedly: %d", calls)
		}
	}
}

func TestWatchResumesAcceptedPendingWorkDuringDiscoveryOutage(t *testing.T) {
	cfg := watchConfiguration(t)
	root := t.TempDir()
	jobs := filepath.Join(root, "jobs")
	directory := filepath.Join(jobs, "21")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(watchedIssue(21, "Keep this original", "2026-01-03T00:00:00Z"))
	if err := writeRuntimeFile(filepath.Join(directory, "issue.json"), raw); err != nil {
		t.Fatal(err)
	}
	request, _ := tracker.RequestText(raw)
	store, err := chain.Open(filepath.Join(directory, "run"), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(chain.State{Request: request, Pending: &chain.Assignment{Role: "implement"}, History: []chain.Result{}}); err != nil {
		t.Fatal(err)
	}
	store.Close()
	var mu sync.Mutex
	sawInterrupted, scans := false, 0
	useWatchTransport(t, func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Host == "watch-tracker.example" {
			scans++
			return catalogReply(r, 503, "intake source offline: synthetic-watch-key"), nil
		}
		var input struct{ State chain.State }
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			return nil, err
		}
		if input.State.Request != request {
			t.Error("resume did not retain original")
		}
		choice := "implement"
		if len(input.State.History) > 0 && input.State.History[0].Speaker == "runtime" && strings.Contains(input.State.History[0].Error, "may have taken effect") {
			sawInterrupted = true
		}
		if len(input.State.History) == 2 {
			choice = "done"
		}
		return selectionReply(r, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": choice}}}), nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var log bytes.Buffer
	locked := &serialLog{writer: &log}
	result := make(chan error, 1)
	since, _ := time.Parse(time.RFC3339, cfg.Intake.CreatedSince)
	go func() { result <- pollRequests(ctx, cfg, jobs, since, 10*time.Millisecond, 1, locked) }()
	waitFor(t, func() bool { s, e := loadWatchState(root, 21); return e == nil && s.Done })
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !sawInterrupted || scans == 0 || !strings.Contains(log.String(), "intake source offline") || strings.Contains(log.String(), "synthetic-watch-key") {
		t.Fatalf("lost recovery observation: interrupted=%v scans=%d log=%s", sawInterrupted, scans, &log)
	}
}

func TestWatchBindingsPreserveOriginalConfigurationAndSeparateRoleHomes(t *testing.T) {
	cfg := watchConfiguration(t)
	cfg.Roles[0].Processes[0].Env = map[string]string{"EXTRA": "unchanged"}
	cfg.Roles[0].Processes = append(cfg.Roles[0].Processes, cfg.Roles[0].Processes[0])
	before, _ := json.Marshal(cfg)
	first, err := bindRequestConfig(cfg, "/example/job-one", "EXAMPLE-1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := bindRequestConfig(cfg, "/example/job-two", "EXAMPLE-2")
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(cfg)
	if !bytes.Equal(before, after) {
		t.Fatal("binding changed shared config")
	}
	if first.Roles[0].Processes[0].Env["TASK_HOME"] == first.Roles[0].Processes[1].Env["TASK_HOME"] || first.Roles[0].Processes[0].Env["TASK_HOME"] == second.Roles[0].Processes[0].Env["TASK_HOME"] {
		t.Fatal("role/request home was shared")
	}
	for _, directory := range []string{"/shared/checkout", "../other-request"} {
		cfg.Roles[0].Processes[0].Directory = directory
		if _, err := bindRequestConfig(cfg, "/example/job", "EXAMPLE-1"); err == nil {
			t.Fatal("watch used an external directory")
		}
	}
	cfg.Roles[0].Processes[0].Directory = ""
	for _, key := range []string{"TASK_HOME", "TASK_WORKSPACE", "TASK_ISSUE"} {
		cfg.Roles[0].Processes[0].Env = map[string]string{key: "shared value"}
		if _, err := bindRequestConfig(cfg, "/example/job", "EXAMPLE-1"); err == nil {
			t.Fatal("operator environment replaced request context")
		}
	}
	cfg.Roles[0].Processes[0].Env = nil
	for _, key := range []string{"TASK_HOME", "TASK_WORKSPACE", "TASK_ISSUE"} {
		cfg.Roles[0].Processes[0].Secrets = map[string]string{key: "SOME_KEY"}
		if _, err := bindRequestConfig(cfg, "/example/job", "EXAMPLE-1"); err == nil {
			t.Fatal("credential was overwritten")
		}
	}
	cfg.Roles[0].Processes[0].Secrets = nil
	for _, key := range []string{"TASK_HOME", "TASK_WORKSPACE", "TASK_ISSUE"} {
		cfg.Roles[0].Processes[0].ModelEnv = key
		if _, err := bindRequestConfig(cfg, "/example/job", "EXAMPLE-1"); err == nil {
			t.Fatal("selected model overwrote request context")
		}
	}
}

func TestWatchCancellationReapsActiveChildAndSameQueueResumesPendingWork(t *testing.T) {
	cfg := watchConfiguration(t)
	cfg.Roles[0].Processes[0].Command = []string{"/bin/sh", "-c", `if test -f started-once; then printf 'Recovered using the same workspace\n'; else cat > received.txt; touch started-once; printf '%s' "$$" > child-pid; exec sleep 60; fi`}
	root := t.TempDir()
	var mu sync.Mutex
	offline, sawInterrupted := false, false
	useWatchTransport(t, func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Host == "watch-tracker.example" {
			if offline {
				return catalogReply(r, 503, "source still offline"), nil
			}
			return selectionReply(r, 200, []any{watchedIssue(31, "original before shutdown", "2026-01-03T00:00:00Z")}), nil
		}
		var input struct{ State chain.State }
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			return nil, err
		}
		choice := "implement"
		if len(input.State.History) > 0 && input.State.History[0].Speaker == "runtime" && strings.Contains(input.State.History[0].Error, "may have taken effect") {
			sawInterrupted = true
		}
		if len(input.State.History) == 2 && strings.Contains(input.State.History[1].Output, "Recovered using the same workspace") {
			choice = "done"
		}
		return selectionReply(r, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": choice}}}), nil
	})
	start := func() (context.CancelFunc, <-chan error) {
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() { result <- watchRequests(ctx, cfg, root, io.Discard) }()
		return cancel, result
	}
	cancel, result := start()
	defer cancel()
	pidPath := filepath.Join(root, "jobs", "31", "workspace", "child-pid")
	pid := 0
	waitFor(t, func() bool {
		data, err := os.ReadFile(pidPath)
		if err != nil {
			return false
		}
		pid, _ = strconv.Atoi(string(data))
		return pid > 0
	})
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("watch did not wait for/reap cancelled work")
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("known experiment child is still alive: %v", err)
	}
	state, err := loadWatchState(root, 31)
	if err != nil || state.Done || state.Pending == nil || state.Pending.Role != "implement" {
		t.Fatalf("shutdown lost unfinished work: %#v %v", state, err)
	}
	mu.Lock()
	offline = true
	mu.Unlock()
	cancelAgain, resumed := start()
	defer cancelAgain()
	waitFor(t, func() bool { s, e := loadWatchState(root, 31); return e == nil && s.Done })
	cancelAgain()
	if err := <-resumed; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !sawInterrupted {
		t.Fatal("restart silently repeated the interrupted action")
	}
}

func TestWatchExclusiveQueueOwnershipPreventsASecondCollector(t *testing.T) {
	cfg := watchConfiguration(t)
	cfg.Intake.PollIntervalSeconds = 60
	var mu sync.Mutex
	scans := 0
	useWatchTransport(t, func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		scans++
		return selectionReply(r, 200, []any{}), nil
	})
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := make(chan error, 1)
	go func() { first <- watchRequests(ctx, cfg, root, io.Discard) }()
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return scans == 1 })
	secondCtx, stopSecond := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer stopSecond()
	var log bytes.Buffer
	err := watchRequests(secondCtx, cfg, root, &log)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(log.String(), "another process owns") {
		t.Fatalf("second collector started: %v %s", err, &log)
	}
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if scans != 1 {
		t.Fatalf("two collectors queried the queue: %d", scans)
	}
}

func TestWatchRequiresExplicitIntakeScopeBeforeCreatingAnything(t *testing.T) {
	for _, mutate := range []func(*config){func(c *config) { c.Intake = nil }, func(c *config) { c.Intake.ProjectID = 0 }, func(c *config) { c.Intake.CreatedSince = "" }, func(c *config) { c.Intake.MaxRunning = -1 }, func(c *config) { c.Intake.PollIntervalSeconds = -1 }, func(c *config) { c.Intake.PollIntervalSeconds = 1 << 40 }, func(c *config) { c.Intake.StopUserIDs = []int64{0} }} {
		cfg := watchConfiguration(t)
		mutate(&cfg)
		root := filepath.Join(t.TempDir(), "must-not-exist")
		if err := watchRequests(context.Background(), cfg, root, io.Discard); err == nil {
			t.Fatal("unscoped watcher started")
		}
		if _, err := os.Stat(root); !os.IsNotExist(err) {
			t.Fatal("invalid intake wrote state")
		}
	}
}

func TestWatchHonorsCapacityAndStartsWaitingRequestsAfterAChildReturns(t *testing.T) {
	cfg := watchConfiguration(t)
	cfg.Roles[0].Processes[0].Command = []string{"/bin/sh", "-c", `cat > received.txt; touch started; while ! test -f proceed; do sleep 0.05; done; printf 'work returned\n'`}
	root := t.TempDir()
	var mu sync.Mutex
	running, peak := 0, 0
	useWatchTransport(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "watch-tracker.example" {
			return selectionReply(r, 200, []any{watchedIssue(41, "one", "2026-01-03T00:00:00Z"), watchedIssue(42, "two", "2026-01-03T00:00:00Z"), watchedIssue(43, "three", "2026-01-03T00:00:00Z")}), nil
		}
		var input struct{ State chain.State }
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			return nil, err
		}
		choice := "implement"
		mu.Lock()
		if len(input.State.History) == 1 {
			choice = "done"
			running--
		} else {
			running++
			if running > peak {
				peak = running
			}
		}
		mu.Unlock()
		return selectionReply(r, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": choice}}}), nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- watchRequests(ctx, cfg, root, io.Discard) }()
	defer func() {
		cancel()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	}()
	jobFile := func(id int, name string) string {
		return filepath.Join(root, "jobs", fmt.Sprint(id), "workspace", name)
	}
	var started []int
	waitFor(t, func() bool {
		started = nil
		for _, id := range []int{41, 42, 43} {
			if _, err := os.Stat(jobFile(id, "started")); err == nil {
				started = append(started, id)
			}
		}
		return len(started) >= 2
	})
	if len(started) != 2 {
		t.Fatal("more than two request children started")
	}
	waiting := 41 + 42 + 43 - started[0] - started[1]
	// Both capacity slots contain real running children. A third has been
	// accepted durably, but must not have started its own child.
	if _, err := os.Stat(filepath.Join(root, "jobs", fmt.Sprint(waiting), "issue.json")); err != nil {
		t.Fatal("waiting request was not retained:", err)
	}
	if _, err := os.Stat(jobFile(waiting, "started")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("more children than the configured capacity")
	}
	if err := os.WriteFile(jobFile(started[0], "proceed"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { _, err := os.Stat(jobFile(waiting, "started")); return err == nil })
	if state, err := loadWatchState(root, started[1]); err != nil || state.Done || state.Pending == nil {
		t.Fatalf("second job no longer running while third started: %#v %v", state, err)
	}
	for _, id := range []int{started[1], waiting} {
		if err := os.WriteFile(jobFile(id, "proceed"), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, func() bool {
		for _, id := range []int{41, 42, 43} {
			if state, err := loadWatchState(root, id); err != nil || !state.Done || len(state.History) != 1 {
				return false
			}
		}
		return true
	})
	mu.Lock()
	defer mu.Unlock()
	if running != 0 || peak != 2 {
		t.Fatalf("configured capacity was not respected: running=%d peak=%d", running, peak)
	}
}
