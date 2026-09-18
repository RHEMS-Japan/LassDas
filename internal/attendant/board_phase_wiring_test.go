package attendant

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/backlog"
	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
)

// Wiring: a real ledger whose one run delivered a pull request and whose
// staging report (merged, not deployable) is posted and sealed, a hermes
// stub listing an empty board, a Backlog transport answering an empty
// ticket, and a recording board. One SyncChains tick projects the ticket
// as delivered; a second tick projects nothing more. Shape adopted from
// the late-word wiring test.
func TestSyncChainsProjectsADeliveredEndOnce(t *testing.T) {
	root := t.TempDir()
	config := runtime.Config{
		Tracker: runtime.TrackerConfig{SpaceKey: "example", ProjectID: 42, ProjectKey: "TICKET", AllowedCreatorID: 7, AllowedActivityType: 1,
			BoardStatuses: runtime.BoardStatuses{Running: 11, AwaitingAnswer: 12, Delivered: 13, NeedsAttention: 14}},
		Identity: runtime.IdentityConfig{RepositoryID: 1, Repository: "o/r", WorkflowRef: "o/r/wf@main", EngineSHA: strings.Repeat("a", 40)},
		Chain: runtime.ChainConfig{RunsRoot: filepath.Join(root, "runs"), Deliver: runtime.DeliverConfig{
			ChecksProfile: "checks", IntegrateProfile: "integrate", PromoteProfile: "promote",
			EnabledAfter: "2026-09-01T00:00:00Z",
		}},
	}
	ledger := filepath.Join(root, "ledger.db")
	store, err := state.NewLocalStore(ledger)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	envelope, err := hook.SealSnapshot(hook.TicketSnapshot{
		SchemaVersion: hook.SnapshotSchemaVersion, SpaceKey: "example", ActivityID: 9001, ActivityType: 1,
		ProjectID: 42, ProjectKey: "TICKET", IssueID: 30, IssueKey: "TICKET-781", IssueKeyID: 781, CreatorID: 7,
		RunID: "TICKET-781", CreatedAt: time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC), Target: config.Target(),
		Untrusted: hook.UntrustedTicketData{Summary: "s", Description: "d"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Enqueue(context.Background(), hook.QueueRequest{Envelope: envelope, QueuedAt: time.Date(2026, 9, 15, 0, 0, 1, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	claimed := time.Now().Add(-time.Hour).UnixMilli()
	db, err := sql.Open("sqlite", ledger+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE ledger SET attrs = json_set(attrs, '$.state', 'terminal', '$.terminal_code', ?, '$.terminal_completed_at', ?, '$.claimed_at', ?) WHERE pk LIKE 'run#%'`,
		string(hook.TerminalSuccess), time.Now().Add(-30*time.Minute).UnixMilli(), claimed); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	runs, err := store.ScanRuns(context.Background())
	if err != nil || len(runs) != 1 || runs[0].TerminalCode != string(hook.TerminalSuccess) || runs[0].ClaimedAt != claimed {
		t.Fatalf("seeded run = %+v err = %v", runs, err)
	}
	runDir := runDirectory(config, envelope.DeliveryID)
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "feature-pr.json"), []byte(`{"number":1111}`), 0o600); err != nil {
		t.Fatal(err)
	}
	report, _ := json.Marshal(runner.DeliverReport{SchemaVersion: 1, Phase: "staging", Verdict: "deploy_not_applicable", Detail: "docs only", ObservedAt: time.Now().UTC()})
	if err := os.WriteFile(filepath.Join(runDir, runner.DeliverStagingReportFile), report, 0o600); err != nil {
		t.Fatal(err)
	}
	sealBoardOutcome(runDir, "staging", "deploy_not_applicable", "")

	client, err := backlog.NewClient(backlog.Config{SpaceKey: "example", APIKey: "k", Origin: "https://example.backlog.com", Timeout: time.Second, MaxResponseBytes: 1 << 20},
		roundTripFunc(func(r *http.Request) (*http.Response, error) {
			body, status := "[]", 200
			switch {
			case r.Method == http.MethodPost:
				encoded, _ := json.Marshal(map[string]any{"id": 9, "issueId": 30, "content": "", "createdUser": map[string]any{"id": 1}, "created": "2026-09-15T12:00:00Z"})
				body, status = string(encoded), 201
			case r.Method == http.MethodGet && r.URL.Path == "/api/v2/issues/30":
				// The ticket sits in the automation's own "running" status.
				body = `{"id":30,"status":{"id":11}}`
			}
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	slogger := slog.New(slog.NewTextHandler(io.Discard, nil))
	target := config.Target()
	hookService, err := hook.NewService(hook.Config{
		SpaceKey: "example", ProjectID: 42, ProjectKey: "TICKET", AllowedCreatorID: 7, AllowedActivityType: 1,
		RunMarker: "Automation-Run-ID", ExpectedRunID: "TICKET-781", Target: target, MaxEnvelopeBytes: 60 * 1024,
	}, client, store, slogger)
	if err != nil {
		t.Fatal(err)
	}
	route := hook.ReportRouteConfig{
		HMACKey: []byte(strings.Repeat("k", 32)), RepositoryID: 1,
		RepositorySHA256: hook.HashIdentity("o/r"), WorkflowRefSHA256: hook.HashIdentity("o/r/wf@main"),
		ExpectedRunID: "TICKET-781",
		Destinations: []hook.ReportDestination{{Repository: "example/consumer", Delivery: hook.DeliverPullRequest,
			StagingOrigin: "https://staging.example.test", ProductionOrigin: "https://www.example.test"}},
		ClockSkew: 2 * time.Minute, LeaseDuration: 2 * time.Minute,
		SpaceKey: "example", ProjectID: 42, ProjectKey: "TICKET", AllowedCreatorID: 7, AllowedActivityType: 1,
		Target: target, RunReferenceScheme: "local",
	}
	reportService, err := hook.NewTerminalReportService(route, store, client, slogger)
	if err != nil {
		t.Fatal(err)
	}
	tick, err := hook.NewQuestionTickService(route, store, client, reportService, hookService, readingStub{}, slogger)
	if err != nil {
		t.Fatal(err)
	}
	// The staging report comment itself is not seeded: the tick service's
	// exactly-once record needs the run's terminal binding, which this
	// fixture does not build. syncDeliver still reaches the projection
	// first, and the report file plus the sealed outcome are the end it
	// reads; the attendant's own report post is refused by the store and
	// ignored, as it is live when a post cannot be recorded.
	bin := filepath.Join(root, "hermes")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ncase \"$2\" in list) echo '[]' ;; *) : ;; esac\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	hermes := runtime.NewHermes(runtime.Config{HermesBin: bin, HermesBoard: "lassdas"})
	board := &recordingBoard{}
	services := &runtime.Services{Store: store, Backlog: client, Tick: tick, Board: board}
	logger := &recordingLogger{}
	if err := SyncChains(context.Background(), config, services, hermes, logger); err != nil {
		t.Fatal(err)
	}
	if len(board.phases) != 1 || board.phases[0] != "30:delivered" {
		t.Fatalf("first tick: phases=%v log=%v", board.phases, logger.lines)
	}
	if _, ok := readBoardPhase(runDir); !ok {
		t.Fatal("no phase record after the first tick")
	}
	if err := SyncChains(context.Background(), config, services, hermes, logger); err != nil {
		t.Fatal(err)
	}
	if len(board.phases) != 1 {
		t.Fatalf("second tick projected again: %v", board.phases)
	}
}

// readingStub answers with whatever a test set for a comment body. The tick
// no longer reads a comment itself, so a test that drives it says what the
// reading was.
type readingStub struct{ byBody map[string]hook.AnswerReading }

func (r readingStub) ReadAnswer(_ context.Context, _, body string) (hook.AnswerReading, error) {
	if reading, ok := r.byBody[body]; ok {
		return reading, nil
	}
	return hook.AnswerReading{Kind: hook.AnswerReadingUnrelated}, nil
}
