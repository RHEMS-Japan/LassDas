package attendant

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
)

// pendingFakeStore is the ledger side of a pending report whose lease has
// expired: it re-acquires a report whose digest matches the one the row was
// begun with and refuses any other as a conflict, as the real store does.
type pendingFakeStore struct {
	expected          string
	digests           []string
	begins, completes int
}

func (f *pendingFakeStore) BeginTerminal(_ context.Context, request hook.TerminalBeginRequest) (hook.TerminalBinding, hook.TerminalBeginDisposition, error) {
	f.begins++
	f.digests = append(f.digests, request.ReportSHA256)
	if request.ReportSHA256 != f.expected {
		return hook.TerminalBinding{IssueID: 4242, IssueKey: "TKT-4242"}, hook.TerminalBeginConflict, nil
	}
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
	l.lines = append(l.lines, "ERROR "+message+" "+flattenArgs(args))
}

func flattenArgs(args []any) string {
	parts := make([]string, 0, len(args))
	for _, arg := range args {
		if text, ok := arg.(string); ok {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, " ")
}

// pendingFixture is a pending model_failed report for a run the pod owns:
// the configuration and route agree on the engine's identity, the terminal
// report service runs for real over the fakes, and the row carries the
// digest the first attempt was begun with (computed the way the runner
// computes it, for the repository the first attempt named).
type pendingFixture struct {
	config     runtime.Config
	services   *runtime.Services
	run        state.RunOverview
	store      *pendingFakeStore
	comments   *pendingFakeComments
	deliveryID string
}

func newPendingFixture(t *testing.T, firstRepository string) pendingFixture {
	t.Helper()
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
		ExpectedRunID: "TKT-4242",
		Destinations: []hook.ReportDestination{{Repository: "example/consumer", Delivery: hook.DeliverPullRequest,
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
	runDir := runDirectory(config, deliveryID)
	terminal := runner.NewTerminal(config, services, envelope, chainOwnerRunID(deliveryID), runDir, &pendingTestLogger{})
	firstDigest, err := terminal.ReportDigest(context.Background(), hook.TerminalModelFailed, runner.Outcome{Code: hook.TerminalModelFailed}, firstRepository)
	if err != nil {
		t.Fatal(err)
	}
	store.expected = firstDigest
	run := state.RunOverview{
		RunID: "TKT-4242", DeliveryID: deliveryID, State: "terminal_report_pending",
		TerminalCode: string(hook.TerminalModelFailed), TerminalReportSHA256: firstDigest,
		EnvelopeJSON: string(encoded), IssueID: 4242, IssueKey: "TKT-4242",
	}
	return pendingFixture{config: config, services: services, run: run, store: store, comments: comments, deliveryID: deliveryID}
}

func (f pendingFixture) writeRunDir(t *testing.T, repository string) {
	t.Helper()
	runDir := runDirectory(f.config, f.deliveryID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "ticket-envelope.json"), []byte(f.run.EnvelopeJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "ticket-draft.json"), []byte(`{"repository":"`+repository+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A pending terminal report from before any card existed — here a model
// failure whose completion never landed — is driven to terminal from the
// run row alone: the envelope comes from the ledger's copy, the report is
// re-submitted with the digest the row was begun with, and a second tick
// finds the comment it already posted, so the ticket ends with exactly one
// terminal comment.
func TestPendingTerminalWithoutCardsIsDrivenToTerminalWithOneComment(t *testing.T) {
	fixture := newPendingFixture(t, "")
	logger := &pendingTestLogger{}
	// No run directory, no cards: the report is rebuilt from the row.
	for tick := 0; tick < 2; tick++ {
		if err := resubmitPendingTerminal(context.Background(), fixture.config, fixture.services, nil, fixture.run, chainViewFor(nil, fixture.deliveryID), logger); err != nil {
			t.Fatalf("tick %d: %v", tick+1, err)
		}
	}
	if len(fixture.comments.posted) != 1 {
		t.Fatalf("terminal comments posted = %d, want exactly one across two ticks: %q", len(fixture.comments.posted), fixture.comments.posted)
	}
	if !strings.Contains(fixture.comments.posted[0], "model_failed") || !strings.Contains(hook.ExtractCommentMarker(fixture.comments.posted[0]), "TKT-4242") {
		t.Fatalf("the posted comment is not this run's terminal report: %q", fixture.comments.posted[0])
	}
	if fixture.store.begins != 2 || fixture.store.completes != 2 {
		t.Fatalf("store begins/completes = %d/%d, want the report begun and completed on both ticks", fixture.store.begins, fixture.store.completes)
	}
	log := strings.Join(logger.lines, "\n")
	if strings.Contains(log, "needs an operator") || !strings.Contains(log, "pending terminal report completed") {
		t.Fatalf("log = %q, want the completion and no operator escalation", logger.lines)
	}
}

// The run directory usually survives with the draft in it, and the draft
// names the consumer repository even when the first report named none (the
// consumer was not resolved before the failure). The re-submission sends
// the report that reproduces the row's digest, not the draft's repository.
func TestPendingTerminalIsRebuiltWithTheRepositoryTheRowWasBegunWith(t *testing.T) {
	fixture := newPendingFixture(t, "")
	fixture.writeRunDir(t, "example/consumer")
	logger := &pendingTestLogger{}
	if err := resubmitPendingTerminal(context.Background(), fixture.config, fixture.services, nil, fixture.run, chainViewFor(nil, fixture.deliveryID), logger); err != nil {
		t.Fatal(err)
	}
	if len(fixture.comments.posted) != 1 || fixture.store.completes != 1 {
		t.Fatalf("comments = %d, completes = %d; want the report completed once", len(fixture.comments.posted), fixture.store.completes)
	}
	for _, digest := range fixture.store.digests {
		if digest != fixture.store.expected {
			t.Fatalf("a report with another digest reached the store: %s", digest)
		}
	}
	// The other way round: the first report named the repository.
	named := newPendingFixture(t, "example/consumer")
	named.writeRunDir(t, "example/consumer")
	if err := resubmitPendingTerminal(context.Background(), named.config, named.services, nil, named.run, chainViewFor(nil, named.deliveryID), logger); err != nil {
		t.Fatal(err)
	}
	if len(named.comments.posted) != 1 || named.store.completes != 1 || len(named.store.digests) != 1 || named.store.digests[0] != named.store.expected {
		t.Fatalf("named repository: comments = %d, completes = %d, digests = %v", len(named.comments.posted), named.store.completes, named.store.digests)
	}
}

// A row whose digest no rebuilt report reproduces is left to a person:
// nothing reaches the store or the ticket, and nothing is requeued.
func TestPendingTerminalThatCannotBeReproducedStaysWithTheOperator(t *testing.T) {
	fixture := newPendingFixture(t, "")
	fixture.run.TerminalReportSHA256 = strings.Repeat("f", 64)
	logger := &pendingTestLogger{}
	if err := resubmitPendingTerminal(context.Background(), fixture.config, fixture.services, nil, fixture.run, chainViewFor(nil, fixture.deliveryID), logger); err != nil {
		t.Fatal(err)
	}
	if fixture.store.begins != 0 || len(fixture.comments.posted) != 0 {
		t.Fatalf("store begins = %d, comments = %d; want nothing sent", fixture.store.begins, len(fixture.comments.posted))
	}
	log := strings.Join(logger.lines, "\n")
	if !strings.Contains(log, "needs an operator") || !strings.Contains(log, "digest") {
		t.Fatalf("log = %q, want an operator escalation naming the digest", logger.lines)
	}
	fixture.run.TerminalReportSHA256 = ""
	if err := resubmitPendingTerminal(context.Background(), fixture.config, fixture.services, nil, fixture.run, chainViewFor(nil, fixture.deliveryID), logger); err != nil || fixture.store.begins != 0 {
		t.Fatalf("a row without a digest: err = %v, begins = %d; want an operator", err, fixture.store.begins)
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

// The ledger's envelope is used when the run directory's copy is missing or
// unreadable, and only when it names this delivery.
func TestPendingEnvelopeFallsBackToTheLedgerCopyForThisDeliveryOnly(t *testing.T) {
	deliveryID := "delivery_" + strings.Repeat("ab", 16)
	stored, _ := json.Marshal(hook.DispatchEnvelope{DeliveryID: deliveryID})
	if envelope, err := pendingEnvelope(t.TempDir(), state.RunOverview{DeliveryID: deliveryID, EnvelopeJSON: string(stored)}); err != nil || envelope.DeliveryID != deliveryID {
		t.Fatalf("ledger copy: %+v, %v", envelope, err)
	}
	broken := t.TempDir()
	if err := os.WriteFile(filepath.Join(broken, "ticket-envelope.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if envelope, err := pendingEnvelope(broken, state.RunOverview{DeliveryID: deliveryID, EnvelopeJSON: string(stored)}); err != nil || envelope.DeliveryID != deliveryID {
		t.Fatalf("unreadable file, ledger copy: %+v, %v", envelope, err)
	}
	other, _ := json.Marshal(hook.DispatchEnvelope{DeliveryID: "delivery_" + strings.Repeat("cd", 16)})
	if _, err := pendingEnvelope(t.TempDir(), state.RunOverview{DeliveryID: deliveryID, EnvelopeJSON: string(other)}); err == nil {
		t.Fatal("an envelope naming another delivery was accepted")
	}
	if _, err := pendingEnvelope(t.TempDir(), state.RunOverview{DeliveryID: deliveryID}); err == nil {
		t.Fatal("no envelope anywhere was accepted")
	}
}
