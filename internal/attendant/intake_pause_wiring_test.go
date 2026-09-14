package attendant

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/backlog"
	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
)

// Wiring: a real ledger with one queued run, a hermes stub that
// lists an empty board, a Backlog transport that answers listings with [] and
// posts with a comment. Paused: the run stays queued and its ticket gets one
// notice. Resumed: SyncChains claims it (and then fails on the missing target
// token, which is fine — the claim is the wiring under test). Adopted from
// the independent review of PR #170.
func TestSyncChainsHoldsQueuedRunsWhilePaused(t *testing.T) {
	root := t.TempDir()
	config := runtime.Config{
		Tracker:  runtime.TrackerConfig{SpaceKey: "example", ProjectID: 42, ProjectKey: "TICKET", AllowedCreatorID: 7, AllowedActivityType: 1},
		Identity: runtime.IdentityConfig{RepositoryID: 1, Repository: "o/r", WorkflowRef: "o/r/wf@main", EngineSHA: strings.Repeat("a", 40)},
		Chain:    runtime.ChainConfig{RunsRoot: filepath.Join(root, "runs"), IntakePausedSince: "2026-09-14T08:30:00+09:00"},
	}
	store, err := state.NewLocalStore(filepath.Join(root, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	envelope, err := hook.SealSnapshot(hook.TicketSnapshot{
		SchemaVersion: hook.SnapshotSchemaVersion, SpaceKey: "example", ActivityID: 9001, ActivityType: 1,
		ProjectID: 42, ProjectKey: "TICKET", IssueID: 30, IssueKey: "TICKET-501", IssueKeyID: 501, CreatorID: 7,
		RunID: "TICKET-501", CreatedAt: time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC), Target: config.Target(),
		Untrusted: hook.UntrustedTicketData{Summary: "s", Description: "d"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Enqueue(context.Background(), hook.QueueRequest{Envelope: envelope, QueuedAt: time.Date(2026, 9, 13, 0, 0, 1, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	var posts atomic.Int32
	client, err := backlog.NewClient(backlog.Config{SpaceKey: "example", APIKey: "k", Origin: "https://example.backlog.com", Timeout: time.Second, MaxResponseBytes: 1 << 20},
		roundTripFunc(func(r *http.Request) (*http.Response, error) {
			body, status := "[]", 200
			if r.Method == http.MethodPost {
				posts.Add(1)
				_ = r.ParseForm()
				encoded, _ := json.Marshal(map[string]any{"id": 5, "issueId": 30, "content": r.PostForm.Get("content"), "createdUser": map[string]any{"id": 1}, "created": "2026-09-14T00:00:00Z"})
				body, status = string(encoded), 201
			}
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "hermes")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ncase \"$2\" in list) echo '[]' ;; *) : ;; esac\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	hermes := runtime.NewHermes(runtime.Config{HermesBin: bin, HermesBoard: "lassdas"})
	services := &runtime.Services{Store: store, Backlog: client}
	logger := &recordingLogger{}

	if err := SyncChains(context.Background(), config, services, hermes, logger); err != nil {
		t.Fatal(err)
	}
	runs, _ := store.ScanRuns(context.Background())
	if len(runs) != 1 || runs[0].State != "queued" || posts.Load() != 1 {
		t.Fatalf("paused: state=%q notices=%d (want queued, 1)", runs[0].State, posts.Load())
	}
	if _, err := os.Stat(filepath.Join(config.Chain.RunsRoot, envelope.DeliveryID, intakePausedNoticeFile)); err != nil {
		t.Fatalf("paused: no record file: %v; log=%v", err, logger.lines)
	}
	config.Chain.IntakePausedSince = ""
	_ = SyncChains(context.Background(), config, services, hermes, logger)
	runs, _ = store.ScanRuns(context.Background())
	if runs[0].State != "claimed" {
		t.Fatalf("resumed: state=%q (want claimed); log=%v", runs[0].State, logger.lines)
	}
	t.Logf("paused → queued with %d notice; resumed → %s", posts.Load(), runs[0].State)
}
