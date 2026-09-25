package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/state"
	"automation.internal/ticket-ingress/internal/worker"
)

type refuseNetwork struct{}

func (refuseNetwork) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("network forbidden in test")
}

// Drive the real entry point, including config loading, rather than
// runLoops alone.
func TestTheResidentWritesInitialAndPeriodicSnapshots(t *testing.T) {
	for _, chainInterval := range []string{"0", "20ms"} {
		for _, once := range []bool{true, false} {
			t.Run(fmt.Sprintf("chain=%s/once=%v", chainInterval, once), func(t *testing.T) {
				configPath, boardPath, _ := observationFixture(t)
				args := []string{"--observe-interval", "50ms", "--chain-interval", chainInterval}
				if once {
					args = append(args, "--once")
				}
				done, stop := startObservationFixture(t, configPath, args...)
				var first time.Time
				ticker := time.NewTicker(10 * time.Millisecond)
				defer ticker.Stop()
				deadline := time.NewTimer(20 * time.Second)
				defer deadline.Stop()
				for {
					select {
					case <-done:
						if !once {
							t.Fatal("resident stopped before periodic observation")
						}
						if readSnapshotTime(boardPath).IsZero() {
							t.Fatal("no initial snapshot was produced")
						}
						return
					case <-ticker.C:
						at := readSnapshotTime(boardPath)
						if at.IsZero() {
							continue
						}
						if first.IsZero() {
							first = at
						}
						if !once && at.After(first) {
							stop()
							<-done
							return
						}
					case <-deadline.C:
						t.Fatal("progress was not published")
					}
				}
			})
		}
	}
}

// With fast observation off, each later board read comes from the actual
// SyncChains path, not a mocked runLoops callback. A waiting run makes the
// path observable without starting an agent, changing a card, or posting.
func TestChainPollingIsIndependentOfObservation(t *testing.T) {
	for _, chainInterval := range []string{"0", "20ms"} {
		t.Run("chain="+chainInterval, func(t *testing.T) {
			configPath, boardPath, callsPath := observationFixture(t)
			done, stop := startObservationFixture(t, configPath,
				"--observe-interval", "0", "--chain-interval", chainInterval)
			waitFor := func(ready func() bool) {
				t.Helper()
				ticker := time.NewTicker(10 * time.Millisecond)
				defer ticker.Stop()
				deadline := time.NewTimer(20 * time.Second)
				defer deadline.Stop()
				for !ready() {
					select {
					case <-done:
						t.Fatal("resident stopped before polling could be checked")
					case <-ticker.C:
					case <-deadline.C:
						t.Fatal("expected polling was not observed")
					}
				}
			}
			waitFor(func() bool { return !readSnapshotTime(boardPath).IsZero() })
			first := readSnapshotTime(boardPath)
			calls := func() int {
				raw, _ := os.ReadFile(callsPath)
				return strings.Count(string(raw), "\n")
			}
			if chainInterval != "0" {
				waitFor(func() bool { return calls() >= 5 })
			} else {
				// Several fast-loop periods pass; there must still be
				// only the tick's sync and its one initial observation.
				timer := time.NewTimer(200 * time.Millisecond)
				defer timer.Stop()
				select {
				case <-timer.C:
				case <-done:
					t.Fatal("resident stopped during the no-fast-poll check")
				}
			}
			stop()
			<-done
			if chainInterval != "0" {
				if calls() < 5 {
					t.Fatal("the chain did not advance independently")
				}
			} else if calls() != 2 {
				t.Fatalf("unexpected fast sync with chain=%s: %d reads", chainInterval, calls())
			}
			if !readSnapshotTime(boardPath).Equal(first) {
				t.Fatal("chain polling re-enabled the disabled observation loop")
			}
		})
	}
}

func readSnapshotTime(path string) time.Time {
	raw, _ := os.ReadFile(path)
	var board struct {
		GeneratedAt time.Time `json:"generated_at"`
	}
	if json.Unmarshal(raw, &board) != nil {
		return time.Time{}
	}
	return board.GeneratedAt
}

func startObservationFixture(t *testing.T, configPath string, args ...string) (<-chan struct{}, context.CancelFunc) {
	t.Helper()
	oldArgs, oldTransport := os.Args, http.DefaultTransport
	t.Cleanup(func() { os.Args = oldArgs; http.DefaultTransport = oldTransport })
	http.DefaultTransport = refuseNetwork{}
	t.Setenv("BACKLOG_API_KEY", "test-only")
	os.Args = append([]string{"attendant", "--config", configPath, "--interval", "1h"}, args...)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		runErr = runContext(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
			if runErr != nil {
				t.Errorf("attendant: %v", runErr)
			}
		case <-time.After(25 * time.Second):
			t.Error("attendant did not stop")
		}
	})
	return done, cancel
}

func observationFixture(t *testing.T) (configPath, boardPath, callsPath string) {
	t.Helper()
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
	callsPath = filepath.Join(root, "hermes-calls")
	t.Setenv("ATTENDANT_TEST_CALLS", callsPath)
	hermes := filepath.Join(root, "hermes")
	script := "#!/bin/sh\n[ \"$1\" = kanban ] && [ \"$2\" = list ] || exit 94\nprintf '%s\\n' \"$*\" >> \"$ATTENDANT_TEST_CALLS\"\nprintf '[]\\n'\n"
	if err := os.WriteFile(hermes, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	ledgerPath := filepath.Join(root, "ledger.db")
	store, err := state.NewLocalStore(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// A synthetic, waiting row: no model, task mutation or real instance.
	row, err := json.Marshal(map[string]string{
		"record_type": "run", "state": "awaiting_answer",
		"run_id": "run-example", "delivery_id": "delivery-example",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO ledger(pk, attrs) VALUES(?, ?)", "run#example", string(row)); err != nil {
		t.Fatal(err)
	}
	config := map[string]any{
		"orchestration": "cards", "ledger_path": ledgerPath,
		"consumer_config_path": consumerPath, "knowledge_root": root, "worker_bin": "/unused/worker", "controller_bin": "/unused/controller",
		"hermes_bin":          hermes,
		"tracker":             map[string]any{"origin": "https://example.backlog.com", "space_key": "example", "project_id": 100, "project_key": "TKT", "allowed_creator_id": 7, "allowed_activity_type": 1},
		"identity":            map[string]any{"repository_id": 1, "repository": "example/consumer", "workflow_ref": "example/consumer/pod@main", "engine_sha": strings.Repeat("a", 40)},
		"automation_run_id":   "run_20260802_" + strings.Repeat("ab", 12),
		"report_destinations": []any{map[string]any{"repository": "example/consumer", "delivery": "pull_request", "staging_origin": "https://stg.example.com", "production_origin": "https://example.com"}},
		"chain": map[string]any{
			"runs_root": filepath.Join(root, "runs"), "target_token_path": filepath.Join(root, "unused-token"),
			"profiles": map[string]string{"implementer": "implement", "review_a": "review-a", "review_b": "review-b", "validate": "validate", "publish": "publish"},
		},
	}
	configPath = write("runtime.json", config)
	statusDir := filepath.Join(root, "status")
	t.Setenv("LASSDAS_STATUS_DIR", statusDir)
	return configPath, filepath.Join(statusDir, "board.json"), callsPath
}
