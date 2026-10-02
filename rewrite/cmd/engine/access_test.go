package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ticket-runner/internal/chain"
	"ticket-runner/internal/tracker"
)

const accessProse = "結果は自由文。\n{\"unknown\":null} $(literal) & + %\n"

// A real subprocess uses only the access given by the public engine. The model
// and upstream tracker are fixtures; this is not a model-quality experiment.
func TestScopedWorkerHelper(t *testing.T) {
	mode := os.Getenv("SCOPED_TEST_WORKER")
	if mode == "" {
		return
	}
	if os.Getenv("ACCESS_TEST_ACCOUNT") != "" || os.Getenv("ACCESS_TEST_ROUTER") != "" {
		t.Fatal("controller credentials reached worker")
	}
	client, err := tracker.CertificateClient(os.Getenv("TASK_TRACKER_CERT"))
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	b := tracker.Backlog{BaseURL: os.Getenv("TASK_TRACKER_URL"), KeyEnv: "TASK_TRACKER_KEY", Client: client}
	issue := os.Getenv("TASK_TRACKER_ISSUE")
	if issue != "EXAMPLE-1" {
		t.Fatalf("assignment=%q", issue)
	}
	ctx := context.Background()
	request, err := b.Request(ctx, issue)
	if err != nil || !strings.Contains(request, "EXAMPLE-2 is prose, not authority") {
		t.Fatalf("original request missing: %v %q", err, request)
	}
	if _, err := b.Request(ctx, "EXAMPLE-2"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("foreign issue accessible: %v", err)
	}
	_, err = b.AddComment(ctx, issue, accessProse)
	if mode == "read" {
		if err == nil || !strings.Contains(err.Error(), "403") {
			t.Fatalf("read worker posted: %v", err)
		}
	} else {
		if err == nil || !strings.Contains(err.Error(), "503: saved but receipt unavailable") || !strings.Contains(err.Error(), "inspect comments") {
			t.Fatalf("ambiguous write hidden: %v", err)
		}
	}
	comments, err := b.Comments(ctx, issue, 0)
	if err != nil || len(comments) != 1 {
		t.Fatalf("cannot inspect posted work: %s %v", comments, err)
	}
	row, err := b.Comment(ctx, issue, 7)
	var comment struct {
		ID      int
		Content string
	}
	if err != nil || json.Unmarshal(row, &comment) != nil || comment.ID != 7 || comment.Content != accessProse {
		t.Fatalf("readback changed: %s %v", row, err)
	}
	// Deliberately echo this synthetic access key: production history must scrub it.
	fmt.Printf("URL %s\nKEY %s\n%s", b.BaseURL, os.Getenv("TASK_TRACKER_KEY"), accessProse)
	os.Exit(0)
}

func accessConfiguration(t *testing.T) config {
	t.Helper()
	t.Setenv("ACCESS_TEST_ACCOUNT", "synthetic-account-controller-only")
	t.Setenv("ACCESS_TEST_ROUTER", "synthetic-router-controller-only")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var cfg config
	cfg.Router.Mode = "jev"
	cfg.Router.Decision = chain.Jev{URL: "https://access-router.example/decisions", Model: "fixture", KeyEnv: "ACCESS_TEST_ROUTER"}
	cfg.Backlog = tracker.Backlog{BaseURL: "https://access-tracker.example/api/v2", KeyEnv: "ACCESS_TEST_ACCOUNT"}
	for _, access := range []string{"comment", "read"} {
		cfg.Roles = append(cfg.Roles, chain.Role{Name: access, Processes: []chain.Process{{Name: access, TrackerAccess: access,
			Command: []string{binary, "-test.run=^TestScopedWorkerHelper$"}, Env: map[string]string{"SCOPED_TEST_WORKER": access}}}})
	}
	return cfg
}

func TestEngineProvidesPerLaunchIssueAccessThenRevokesIt(t *testing.T) {
	cfg := accessConfiguration(t)
	var posts, requests, routes atomic.Int32
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "access-router.example" {
			n := routes.Add(1)
			choice := "comment"
			if n == 2 {
				choice = "read"
			}
			if n >= 3 {
				choice = "done"
			}
			return selectionReply(r, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": choice}}}), nil
		}
		if r.URL.Host != "access-tracker.example" || r.URL.Query().Get("apiKey") != "synthetic-account-controller-only" {
			return nil, fmt.Errorf("unexpected source authority")
		}
		requests.Add(1)
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v2/issues/EXAMPLE-1":
			return selectionReply(r, 200, map[string]any{"issueKey": "EXAMPLE-1", "summary": "Assigned request", "description": "EXAMPLE-2 is prose, not authority"}), nil
		case "POST /api/v2/issues/EXAMPLE-1/comments":
			posts.Add(1)
			if err := r.ParseForm(); err != nil || r.PostForm.Get("content") != accessProse || len(r.PostForm) != 1 {
				t.Error("changed report or added side effect")
			}
			return catalogReply(r, 503, "saved but receipt unavailable"), nil
		case "GET /api/v2/issues/EXAMPLE-1/comments":
			return selectionReply(r, 200, []any{map[string]any{"id": 7, "content": accessProse}}), nil
		case "GET /api/v2/issues/EXAMPLE-1/comments/7":
			return selectionReply(r, 200, map[string]any{"id": 7, "content": accessProse}), nil
		default:
			return nil, fmt.Errorf("unexpected upstream operation: %s %s", r.Method, r.URL.Path)
		}
	})
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var log bytes.Buffer
	if err := run(ctx, []string{"--config", configPath, "--issue", "EXAMPLE-1", "--run-dir", filepath.Join(dir, "run")}, io.Discard, &log); err != nil {
		t.Fatalf("engine: %v %s", err, &log)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "run", "history.json"))
	var state chain.State
	if err != nil || json.Unmarshal(raw, &state) != nil || !state.Done || len(state.History) != 2 {
		t.Fatalf("history: %s %v", raw, err)
	}
	if posts.Load() != 1 || requests.Load() != 8 || routes.Load() != 3 {
		t.Fatalf("unexpected operations posts=%d source=%d routing=%d", posts.Load(), requests.Load(), routes.Load())
	}
	for _, result := range state.History {
		if result.Error != "" || !strings.Contains(result.Output, "KEY [credential]") || !strings.HasSuffix(result.Output, accessProse) {
			t.Fatalf("process/report: %#v", result)
		}
		address, err := url.Parse(strings.Fields(result.Output)[1])
		if err != nil {
			t.Fatal(err)
		}
		conn, err := net.DialTimeout("tcp", address.Host, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			t.Fatal("worker access remained available after its process exited")
		}
	}
	if strings.Contains(string(raw), "synthetic-") || strings.Contains(log.String(), "synthetic-") {
		t.Fatal("controller credential in durable output")
	}
	// Completion does not repeat either worker or the visible post.
	if err := run(ctx, []string{"--config", configPath, "--issue", "EXAMPLE-1", "--run-dir", filepath.Join(dir, "run")}, io.Discard, &log); err != nil {
		t.Fatal(err)
	}
	if posts.Load() != 1 || routes.Load() != 3 {
		t.Fatal("finished work ran again")
	}
	if len(cfg.Roles[0].Processes[0].Credentials) != 0 || len(cfg.Roles[0].Processes[0].Env) != 1 {
		t.Fatal("runtime access mutated persistent configuration")
	}
}

func TestIssueAccessConfigurationCannotBorrowBroadCredentialsOrModelAuthority(t *testing.T) {
	for _, test := range []string{"missing-issue", "mode", "env", "secret", "model", "broad-key", "broad-key-other-role", "no-source"} {
		t.Run(test, func(t *testing.T) {
			cfg := accessConfiguration(t)
			issue := "EXAMPLE-1"
			p := &cfg.Roles[0].Processes[0]
			switch test {
			case "missing-issue":
				issue = ""
			case "mode":
				p.TrackerAccess = "all"
			case "env":
				p.Env["TASK_TRACKER_URL"] = "https://elsewhere.invalid"
			case "secret":
				p.Secrets = map[string]string{"TASK_TRACKER_KEY": "OTHER"}
			case "model":
				p.ModelEnv = "TASK_TRACKER_KEY"
			case "broad-key":
				p.Secrets = map[string]string{"KEY": cfg.Backlog.KeyEnv}
			case "broad-key-other-role":
				cfg.Roles[1].Processes[0].TrackerAccess = ""
				cfg.Roles[1].Processes[0].Secrets = map[string]string{"KEY": cfg.Backlog.KeyEnv}
			case "no-source":
				cfg.Backlog.KeyEnv = ""
			}
			if prepare, err := roleAccess(cfg, issue); err == nil || prepare != nil {
				t.Fatal("invalid operator permission accepted")
			}
		})
	}
	// Both posting grants are operator choices: one comment per launch, or
	// every post kept.
	for _, grant := range []string{"comment", "comments"} {
		cfg := accessConfiguration(t)
		cfg.Roles[0].Processes[0].TrackerAccess = grant
		if prepare, err := roleAccess(cfg, "EXAMPLE-1"); err != nil || prepare == nil {
			t.Fatalf("tracker_access %q was refused: %v", grant, err)
		}
	}
	cfg := accessConfiguration(t)
	bound, err := bindRequestConfig(cfg, t.TempDir(), "EXAMPLE-1")
	if err != nil || bound.AssignedIssue != "EXAMPLE-1" || cfg.AssignedIssue != "" {
		t.Fatalf("watch identity not bound: %v %#v", err, bound)
	}
	data, _ := json.Marshal(bound)
	if strings.Contains(string(data), "TASK_TRACKER_KEY") || strings.Contains(string(data), "BEGIN CERTIFICATE") {
		t.Fatal("ephemeral access persisted into watch config")
	}
}

func TestWatchAcceptedIdentityReachesScopedWorker(t *testing.T) {
	cfg := accessConfiguration(t)
	cfg.Roles = cfg.Roles[:1]
	cfg.Intake = &intakeConfig{ProjectID: 17, CreatedSince: "2026-01-02T00:00:00Z", PollIntervalSeconds: 1, MaxRunning: 1}
	var posts, routes atomic.Int32
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "access-router.example" {
			choice := "comment"
			if routes.Add(1) > 1 {
				choice = "done"
			}
			return selectionReply(r, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": choice}}}), nil
		}
		if r.URL.Host != "access-tracker.example" || r.URL.Query().Get("apiKey") != "synthetic-account-controller-only" {
			return nil, fmt.Errorf("unexpected source authority")
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v2/issues":
			if r.URL.Query().Get("projectId[]") != "17" {
				t.Error("discovery not scoped")
			}
			return selectionReply(r, 200, []any{watchedIssue(1, "EXAMPLE-2 is prose, not authority", "2026-01-03T00:00:00Z")}), nil
		case "GET /api/v2/issues/EXAMPLE-1":
			return selectionReply(r, 200, watchedIssue(1, "EXAMPLE-2 is prose, not authority", "2026-01-03T00:00:00Z")), nil
		case "POST /api/v2/issues/EXAMPLE-1/comments":
			posts.Add(1)
			if err := r.ParseForm(); err != nil || r.PostForm.Get("content") != accessProse {
				t.Error("report changed")
			}
			return catalogReply(r, 503, "saved but receipt unavailable"), nil
		case "GET /api/v2/issues/EXAMPLE-1/comments":
			comments := []any{}
			if posts.Load() > 0 {
				comments = append(comments, map[string]any{"id": 7, "issueId": 1, "projectId": 17, "content": accessProse, "createdUser": map[string]int{"id": 999}})
			}
			return selectionReply(r, 200, comments), nil
		case "GET /api/v2/issues/EXAMPLE-1/comments/7":
			return selectionReply(r, 200, map[string]any{"id": 7, "content": accessProse}), nil
		default:
			return nil, fmt.Errorf("unexpected upstream path %s", r.URL.Path)
		}
	})
	dir := t.TempDir()
	data, _ := json.Marshal(cfg)
	file := filepath.Join(dir, "config.json")
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	var log bytes.Buffer
	go func() {
		finished <- run(ctx, []string{"--config", file, "--watch", "--run-dir", dir}, io.Discard, &log)
	}()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
waiting:
	for {
		select {
		case <-ctx.Done():
			break waiting
		case <-tick.C:
			state, err := loadWatchState(dir, 1)
			if err == nil && (state.Done || (len(state.History) > 0 && state.History[0].Error != "")) {
				break waiting
			}
		}
	}
	cancel()
	<-finished
	state, err := loadWatchState(dir, 1)
	if err != nil || !state.Done || len(state.History) != 1 || state.History[0].Error != "" || !strings.HasSuffix(state.History[0].Output, accessProse) || posts.Load() != 1 || routes.Load() != 2 {
		t.Fatalf("watch failed: %#v %v posts=%d routes=%d log=%s", state, err, posts.Load(), routes.Load(), &log)
	}
	bound, err := os.ReadFile(filepath.Join(dir, "jobs", "1", "engine.json"))
	var saved config
	if err != nil || json.Unmarshal(bound, &saved) != nil || saved.AssignedIssue != "EXAMPLE-1" || strings.Contains(string(bound), "TASK_TRACKER_KEY") {
		t.Fatalf("wrong persisted assignment: %s %v", bound, err)
	}
}
