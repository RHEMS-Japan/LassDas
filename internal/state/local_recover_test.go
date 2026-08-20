package state

import (
	"context"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
)

func claimedRunOverview(t *testing.T, store *LocalStore) RunOverview {
	t.Helper()
	runs, err := store.ScanRuns(context.Background())
	if err != nil || len(runs) != 1 {
		t.Fatalf("ScanRuns() = %v, %v", runs, err)
	}
	return runs[0]
}

func localClaim(t *testing.T, store *LocalStore) hook.DispatchEnvelope {
	t.Helper()
	ctx := context.Background()
	queue := testQueueRequest(t)
	if _, err := store.Enqueue(ctx, queue); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	envelope, disposition, err := store.Pull(ctx, testPullRequest(t))
	if err != nil || disposition != hook.PullAcquired {
		t.Fatalf("Pull() = %s, %v", disposition, err)
	}
	return envelope
}

// A dead claim is returned to the queue only against the observed claim
// timestamp, and a fresh owner can then claim the recovered run.
func TestRecoverLostClaimRequeuesADeadClaim(t *testing.T) {
	store := newLocalForTest(t)
	ctx := context.Background()
	localClaim(t, store)

	run := claimedRunOverview(t, store)
	if run.State != "claimed" || run.ClaimedAt == 0 {
		t.Fatalf("claimed run overview = %+v", run)
	}
	if err := store.RecoverLostClaim(ctx, run.Key, run.ClaimedAt+1, testQueuedAt.Add(5*time.Minute)); err == nil {
		t.Fatal("a recovery bound to another claim timestamp was accepted")
	}
	if err := store.RecoverLostClaim(ctx, run.Key, run.ClaimedAt, testQueuedAt.Add(5*time.Minute)); err != nil {
		t.Fatalf("RecoverLostClaim() error = %v", err)
	}
	if got := claimedRunOverview(t, store); got.State != "queued" {
		t.Fatalf("state after recovery = %s", got.State)
	}
	// Idempotent replay of the same recovery.
	if err := store.RecoverLostClaim(ctx, run.Key, run.ClaimedAt, testQueuedAt.Add(6*time.Minute)); err != nil {
		t.Fatalf("idempotent RecoverLostClaim() error = %v", err)
	}

	repull := testPullRequest(t)
	repull.Owner.WorkflowRunID++
	repull.IssuedAt = testQueuedAt.Add(5*time.Minute + time.Second)
	repull.ClaimedAt = testQueuedAt.Add(5*time.Minute + 2*time.Second)
	if _, disposition, err := store.Pull(ctx, repull); err != nil || disposition != hook.PullAcquired {
		t.Fatalf("re-pull after recovery = %s, %v", disposition, err)
	}
}

// A runner that died between the two phases of its own terminal report
// leaves terminal_report_pending with no question evidence; recovery
// refuses it while the lease lives, then clears the partial terminal
// fields and requeues, and the run can be claimed and closed afresh.
func TestRecoverLostClaimRequeuesADeadRunnerReport(t *testing.T) {
	store := newLocalForTest(t)
	ctx := context.Background()
	envelope := localClaim(t, store)

	run := claimedRunOverview(t, store)
	begin := testTerminalBegin(t, envelope, hook.TerminalModelFailed, testQueuedAt.Add(5*time.Second), strings.Repeat("a", 32))
	if _, disposition, err := store.BeginTerminal(ctx, begin); err != nil || disposition != hook.TerminalBeginAcquired {
		t.Fatalf("BeginTerminal() = %s, %v", disposition, err)
	}
	if got := claimedRunOverview(t, store); got.State != "terminal_report_pending" || got.QuestionSealed {
		t.Fatalf("overview after begin = %+v", got)
	}
	if err := store.RecoverLostClaim(ctx, run.Key, run.ClaimedAt, testQueuedAt.Add(6*time.Second)); err == nil {
		t.Fatal("a report with a live lease was requeued")
	}
	if err := store.RecoverLostClaim(ctx, run.Key, run.ClaimedAt, testQueuedAt.Add(10*time.Minute)); err != nil {
		t.Fatalf("RecoverLostClaim() error = %v", err)
	}
	if got := claimedRunOverview(t, store); got.State != "queued" {
		t.Fatalf("state after recovery = %s", got.State)
	}

	// The store accepts any owner on a queued run; the sealed rule that the
	// closing report's owner must equal the claim's is exercised by keeping
	// the helper report's fixed owner here.
	repull := testPullRequest(t)
	repull.IssuedAt = testQueuedAt.Add(10*time.Minute + time.Second)
	repull.ClaimedAt = testQueuedAt.Add(10*time.Minute + 2*time.Second)
	if _, disposition, err := store.Pull(ctx, repull); err != nil || disposition != hook.PullAcquired {
		t.Fatalf("re-pull after recovery = %s, %v", disposition, err)
	}
	// The fresh attempt seals a fresh report (a real retry stamps its own
	// issued-at; the begin gate binds it to the new started-at).
	report := testTerminalReport(t, envelope, hook.TerminalModelFailed)
	startedAt := testQueuedAt.Add(11 * time.Minute)
	report.IssuedAt = startedAt
	body, err := hook.MarshalTerminalReportRecord(report)
	if err != nil {
		t.Fatalf("MarshalTerminalReportRecord() error = %v", err)
	}
	route := testTerminalRoute(t)
	again := hook.TerminalBeginRequest{
		Report: report, ReportJSON: string(body), ReportSHA256: hook.TerminalReportDigest(body), Route: route,
		StartedAt: startedAt, LeaseUntil: startedAt.Add(route.LeaseDuration), LeaseToken: strings.Repeat("b", 32),
	}
	if _, disposition, err := store.BeginTerminal(ctx, again); err != nil || disposition != hook.TerminalBeginAcquired {
		t.Fatalf("BeginTerminal() after recovery = %s, %v", disposition, err)
	}
}
