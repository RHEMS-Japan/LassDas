package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/worker"
)

type refuseNetwork struct{}

func (refuseNetwork) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("network forbidden in test")
}

// Drive the real entry point, including config loading and its mode
// selection. Testing runLoops alone missed the runner-only disable.
func TestRunnerModeWritesInitialAndPeriodicSnapshots(t *testing.T) {
	oldArgs, oldTransport := os.Args, http.DefaultTransport
	t.Cleanup(func() { os.Args = oldArgs; http.DefaultTransport = oldTransport })
	http.DefaultTransport = refuseNetwork{}
	t.Setenv("BACKLOG_API_KEY", "test-only")
	for _, once := range []bool{true, false} {
		t.Run(fmt.Sprint("once=", once), func(t *testing.T) {
			root := t.TempDir()
			write := func(name string, value any) string {
				t.Helper()
				raw, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(root, name)
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
				return path
			}
			consumer, err := worker.LoadConfig("../../config/m1-consumer.json")
			if err != nil {
				t.Fatal(err)
			}
			consumer.Consumers = consumer.Consumers[:1]
			consumerPath := write("consumer.json", consumer)
			hermes := filepath.Join(root, "hermes")
			if err := os.WriteFile(hermes, []byte("#!/bin/sh\n[ \"$1\" = kanban ] && [ \"$2\" = list ] || exit 94\nprintf '[]\\n'\n"), 0700); err != nil {
				t.Fatal(err)
			}
			config := map[string]any{
				"orchestration": "runner", "ledger_path": filepath.Join(root, "ledger.db"),
				"consumer_config_path": consumerPath, "knowledge_root": root, "worker_bin": "/unused/worker", "controller_bin": "/unused/controller",
				"hermes_bin": hermes, "hermes_profile": "runner",
				"tracker":             map[string]any{"origin": "https://example.backlog.com", "space_key": "example", "project_id": 100, "project_key": "TKT", "allowed_creator_id": 7, "allowed_activity_type": 1},
				"identity":            map[string]any{"repository_id": 1, "repository": "example/consumer", "workflow_ref": "example/consumer/pod@main", "engine_sha": strings.Repeat("a", 40)},
				"automation_run_id":   "run_20260802_" + strings.Repeat("ab", 12),
				"report_destinations": []any{map[string]any{"repository": "example/consumer", "delivery": "pull_request", "staging_origin": "https://stg.example.com", "production_origin": "https://example.com"}},
			}
			configPath := write("runtime.json", config)
			statusDir := filepath.Join(root, "status")
			t.Setenv("LASSDAS_STATUS_DIR", statusDir)
			os.Args = []string{"attendant", "--config", configPath, "--interval", "1h", "--observe-interval", "50ms"}
			if once {
				os.Args = append(os.Args, "--once")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- runContext(ctx) }()
			t.Cleanup(func() { cancel() })
			boardPath := filepath.Join(statusDir, "board.json")
			var first time.Time
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			complete := false
			for !complete {
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
					if !once {
						t.Fatal("resident stopped before periodic observation")
					}
					raw, err := os.ReadFile(boardPath)
					if err != nil {
						t.Fatal("runner produced no initial snapshot:", err)
					}
					var board struct {
						GeneratedAt time.Time `json:"generated_at"`
					}
					if json.Unmarshal(raw, &board) != nil || board.GeneratedAt.IsZero() {
						t.Fatal("invalid snapshot")
					}
					complete = true
				case <-ticker.C:
					raw, _ := os.ReadFile(boardPath)
					var board struct {
						GeneratedAt time.Time `json:"generated_at"`
					}
					if json.Unmarshal(raw, &board) != nil || board.GeneratedAt.IsZero() {
						continue
					}
					if first.IsZero() {
						first = board.GeneratedAt
					}
					if !once && board.GeneratedAt.After(first) {
						cancel()
						if err := <-done; err != nil {
							t.Fatal(err)
						}
						complete = true
					}
				case <-ctx.Done():
					cancel()
					<-done
					t.Fatal("runner did not publish progress")
				}
			}
		})
	}
}
