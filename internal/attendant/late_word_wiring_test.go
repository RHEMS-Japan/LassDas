package attendant

import (
	"context"
	"database/sql"
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

	_ "modernc.org/sqlite"
)

// Wiring: a real ledger whose one run ended as clarification_expired an
// hour ago, a hermes stub listing an empty board, and a Backlog transport
// whose ticket carries the terminal report and, after it, a requester
// comment. One SyncChains tick posts the one late-word reply and records
// the check; a second tick posts nothing. Shape adopted from the
// independent review of PR #171.
func TestSyncChainsAnswersALateAnswerOnce(t *testing.T) {
	root := t.TempDir()
	config := runtime.Config{
		Tracker:  runtime.TrackerConfig{SpaceKey: "example", ProjectID: 42, ProjectKey: "TICKET", AllowedCreatorID: 7, AllowedActivityType: 1},
		Identity: runtime.IdentityConfig{RepositoryID: 1, Repository: "o/r", WorkflowRef: "o/r/wf@main", EngineSHA: strings.Repeat("a", 40)},
		Chain:    runtime.ChainConfig{RunsRoot: filepath.Join(root, "runs")},
	}
	ledger := filepath.Join(root, "ledger.db")
	store, err := state.NewLocalStore(ledger)
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
	// Seed the ending the tick would otherwise have to reach through the
	// question protocol: terminal, clarification_expired, an hour ago.
	db, err := sql.Open("sqlite", ledger+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE ledger SET attrs = json_set(attrs, '$.state', 'terminal', '$.terminal_code', ?, '$.terminal_completed_at', ?) WHERE pk LIKE 'run#%'`,
		string(hook.TerminalClarificationExpired), time.Now().Add(-time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	runs, err := store.ScanRuns(context.Background())
	if err != nil || len(runs) != 1 || runs[0].State != "terminal" || runs[0].TerminalCode != string(hook.TerminalClarificationExpired) {
		t.Fatalf("seeded run = %+v err = %v", runs, err)
	}
	terminal := "expired\n" + hook.CommentMarker("terminal", "TICKET-501", string(hook.TerminalClarificationExpired), strings.Repeat("a", 64))
	var posts atomic.Int32
	var lastPost atomic.Value
	client, err := backlog.NewClient(backlog.Config{SpaceKey: "example", APIKey: "k", Origin: "https://example.backlog.com", Timeout: time.Second, MaxResponseBytes: 1 << 20},
		roundTripFunc(func(r *http.Request) (*http.Response, error) {
			body, status := "[]", 200
			switch {
			case r.Method == http.MethodPost:
				posts.Add(1)
				_ = r.ParseForm()
				lastPost.Store(r.PostForm.Get("content"))
				encoded, _ := json.Marshal(map[string]any{"id": 9, "issueId": 30, "content": r.PostForm.Get("content"), "createdUser": map[string]any{"id": 1}, "created": "2026-09-14T12:00:00Z"})
				body, status = string(encoded), 201
			case r.URL.Query().Get("minId") == "" || r.URL.Query().Get("minId") == "0":
				encoded, _ := json.Marshal([]map[string]any{
					{"id": 2, "issueId": 30, "content": terminal, "createdUser": map[string]any{"id": 1}, "created": "2026-09-14T10:00:00Z"},
					{"id": 3, "issueId": 30, "content": "遅れましたが、A でお願いします", "createdUser": map[string]any{"id": 7}, "created": "2026-09-14T11:00:00Z"},
				})
				body = string(encoded)
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
	posted, _ := lastPost.Load().(string)
	if posts.Load() != 1 || hook.ExtractCommentMarker(posted) != hook.LateWordMarker("TICKET-501") {
		t.Fatalf("first tick: posts=%d marker=%q log=%v", posts.Load(), hook.ExtractCommentMarker(posted), logger.lines)
	}
	if _, err := os.Stat(filepath.Join(config.Chain.RunsRoot, envelope.DeliveryID, lateWordCheckFile)); err != nil {
		t.Fatalf("no check record: %v", err)
	}
	if err := SyncChains(context.Background(), config, services, hermes, logger); err != nil {
		t.Fatal(err)
	}
	if posts.Load() != 1 {
		t.Fatalf("second tick posted again: %d", posts.Load())
	}
}
