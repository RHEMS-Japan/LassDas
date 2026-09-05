package attendant

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
)

// pendingFakeStore acquires every terminal report and completes it: the
// ledger side of a pending report whose lease has expired.
type pendingFakeStore struct{ begins, completes int }

func (f *pendingFakeStore) BeginTerminal(context.Context, hook.TerminalBeginRequest) (hook.TerminalBinding, hook.TerminalBeginDisposition, error) {
	f.begins++
	return hook.TerminalBinding{IssueID: 4242, IssueKey: "TKT-4242"}, hook.TerminalBeginAcquired, nil
}

func (f *pendingFakeStore) CompleteTerminal(context.Context, hook.TerminalCompleteRequest) (hook.TerminalCompleteDisposition, error) {
	f.completes++
	return hook.TerminalCompleted, nil
}

// pendingFakeComments is the ticket: it remembers what was posted and finds
// a comment by the marker on its final line, as the tracker client does.
type pendingFakeComments struct{ posted []string }

func (f *pendingFakeComments) FindExactComment(context.Context, int64, string) (int64, bool, error) {
	return 0, false, nil
}

func (f *pendingFakeComments) FindCommentWithMarker(_ context.Context, _ int64, marker string) (int64, bool, error) {
	for index, content := range f.posted {
		if hook.ExtractCommentMarker(content) == marker {
			return int64(900 + index), true, nil
		}
	}
	return 0, false, nil
}

func (f *pendingFakeComments) AddComment(_ context.Context, _ int64, content string) (int64, error) {
	f.posted = append(f.posted, content)
	return int64(900 + len(f.posted) - 1), nil
}

type pendingTestLogger struct{ lines []string }

func (l *pendingTestLogger) Info(message string, args ...any) {
	l.lines = append(l.lines, message)
}
func (l *pendingTestLogger) Error(message string, args ...any) {
	l.lines = append(l.lines, "ERROR "+message)
}

// A pending terminal report from before any card existed — here a model
// failure whose completion never landed — is driven to terminal from the
// run row alone: the envelope comes from the ledger's copy, the report is
// re-submitted, and a second tick finds the comment it already posted, so
// the ticket ends with exactly one terminal comment.
func TestPendingTerminalWithoutCardsIsDrivenToTerminalWithOneComment(t *testing.T) {
	const repository = "example/engine"
	const workflowRef = "example/engine/.github/workflows/engine.yml@refs/heads/main"
	deliveryID := "delivery_" + strings.Repeat("ab", 16)
	config := runtime.Config{
		ConsumerConfigPath: filepath.Join(t.TempDir(), "no-consumer.json"),
		Chain:              runtime.ChainConfig{RunsRoot: t.TempDir()},
	}
	config.Identity = runtime.IdentityConfig{RepositoryID: 7, Repository: repository, WorkflowRef: workflowRef, EngineSHA: strings.Repeat("c", 40)}
	route := hook.ReportRouteConfig{
		HMACKey: bytes.Repeat([]byte("k"), 32), RepositoryID: 7,
		RepositorySHA256: hook.HashIdentity(repository), WorkflowRefSHA256: hook.HashIdentity(workflowRef),
		ExpectedRunID: "TKT-4242", Destinations: []hook.ReportDestination{{Repository: "example/consumer", Delivery: hook.DeliverPullRequest,
			StagingOrigin: "https://staging.example.test", ProductionOrigin: "https://www.example.test"}},
		ClockSkew: time.Minute, LeaseDuration: time.Minute, SpaceKey: "space", ProjectID: 1, ProjectKey: "TKT",
		AllowedCreatorID: 1, AllowedActivityType: 1, RunReferenceScheme: "local",
		Target: hook.DeliveryTarget{RepositoryID: 7, WorkflowRefSHA256: hook.HashIdentity(workflowRef)},
	}
	store := &pendingFakeStore{}
	comments := &pendingFakeComments{}
	report, err := hook.NewTerminalReportService(route, store, comments, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	services := &runtime.Services{Config: config, Report: report, Route: route}
	envelope := hook.DispatchEnvelope{DeliveryID: deliveryID, Snapshot: hook.TicketSnapshot{
		RunID: "TKT-4242", IssueID: 4242, IssueKey: "TKT-4242", InputSHA256: strings.Repeat("1", 64),
	}}
	encoded, _ := json.Marshal(envelope)
	run := state.RunOverview{
		RunID: "TKT-4242", DeliveryID: deliveryID, State: "terminal_report_pending",
		TerminalCode: string(hook.TerminalModelFailed), EnvelopeJSON: string(encoded), IssueID: 4242, IssueKey: "TKT-4242",
	}
	logger := &pendingTestLogger{}
	// No run directory, no cards: the report is rebuilt from the row.
	for tick := 0; tick < 2; tick++ {
		if err := resubmitPendingTerminal(context.Background(), config, services, nil, run, chainViewFor(nil, deliveryID), logger); err != nil {
			t.Fatalf("tick %d: %v", tick+1, err)
		}
	}
	if len(comments.posted) != 1 {
		t.Fatalf("terminal comments posted = %d, want exactly one across two ticks: %q", len(comments.posted), comments.posted)
	}
	if !strings.Contains(comments.posted[0], "model_failed") || !strings.Contains(hook.ExtractCommentMarker(comments.posted[0]), "TKT-4242") {
		t.Fatalf("the posted comment is not this run's terminal report: %q", comments.posted[0])
	}
	if store.begins != 2 || store.completes != 2 {
		t.Fatalf("store begins/completes = %d/%d, want the report begun and completed on both ticks", store.begins, store.completes)
	}
	if strings.Join(logger.lines, "\n") == "" || strings.Contains(strings.Join(logger.lines, "\n"), "needs an operator") ||
		!strings.Contains(strings.Join(logger.lines, "\n"), "pending terminal report completed") {
		t.Fatalf("log = %q, want the completion and no operator escalation", logger.lines)
	}
}

// A pending success cannot be rebuilt without its cards and stays with the
// operator; nothing is posted or requeued.
func TestPendingSuccessWithoutCardsStaysWithTheOperator(t *testing.T) {
	logger := &pendingTestLogger{}
	run := state.RunOverview{RunID: "TKT-4242", DeliveryID: "delivery_" + strings.Repeat("ab", 16), TerminalCode: string(hook.TerminalSuccess)}
	if err := resubmitPendingTerminal(context.Background(), runtime.Config{Chain: runtime.ChainConfig{RunsRoot: t.TempDir()}}, nil, nil, run, chainViewFor(nil, run.DeliveryID), logger); err != nil {
		t.Fatal(err)
	}
	if len(logger.lines) != 1 || !strings.Contains(logger.lines[0], "needs an operator") {
		t.Fatalf("log = %q, want one operator escalation", logger.lines)
	}
}

// The ledger's envelope is used only when the run directory has none, and
// only when it names this delivery.
func TestPendingEnvelopeFallsBackToTheLedgerCopyForThisDeliveryOnly(t *testing.T) {
	deliveryID := "delivery_" + strings.Repeat("ab", 16)
	stored, _ := json.Marshal(hook.DispatchEnvelope{DeliveryID: deliveryID})
	if envelope, err := pendingEnvelope(t.TempDir(), state.RunOverview{DeliveryID: deliveryID, EnvelopeJSON: string(stored)}); err != nil || envelope.DeliveryID != deliveryID {
		t.Fatalf("ledger copy: %+v, %v", envelope, err)
	}
	other, _ := json.Marshal(hook.DispatchEnvelope{DeliveryID: "delivery_" + strings.Repeat("cd", 16)})
	if _, err := pendingEnvelope(t.TempDir(), state.RunOverview{DeliveryID: deliveryID, EnvelopeJSON: string(other)}); err == nil {
		t.Fatal("an envelope naming another delivery was accepted")
	}
	if _, err := pendingEnvelope(t.TempDir(), state.RunOverview{DeliveryID: deliveryID}); err == nil {
		t.Fatal("no envelope anywhere was accepted")
	}
}
