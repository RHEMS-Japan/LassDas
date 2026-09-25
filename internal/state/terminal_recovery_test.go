package state

import (
	"context"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
)

// pendingTerminalRun leaves the ledger where a pod that stopped mid-report
// leaves it: the ending decided and sealed, the comment never recorded.
func pendingTerminalRun(t *testing.T, store *LocalStore) (hook.DispatchEnvelope, hook.TerminalBeginRequest) {
	t.Helper()
	envelope := localClaim(t, store)
	begin := testTerminalBegin(t, envelope, hook.TerminalModelFailed, testQueuedAt.Add(5*time.Second), strings.Repeat("a", 32))
	if _, disposition, err := store.BeginTerminal(context.Background(), begin); err != nil || disposition != hook.TerminalBeginAcquired {
		t.Fatalf("BeginTerminal() = %s, %v", disposition, err)
	}
	return envelope, begin
}

func recoveryClose(begin hook.TerminalBeginRequest, commentID int64, at time.Time) hook.TerminalRecoveryCloseRequest {
	return hook.TerminalRecoveryCloseRequest{
		Route: begin.Route, RunID: begin.Report.AutomationRunID, Code: begin.Report.Code,
		ReportSHA256: begin.ReportSHA256, CommentID: commentID, CompletedAt: at,
	}
}

// A pending report whose record cannot be rebuilt is closed against the
// comment that reported it instead. The run becomes terminal, it names that
// comment, and the project's single pending slot is released — which is
// what lets the next ticket in, and what the endless wait used to hold.
func TestAnUnreproducibleTerminalIsClosedAndReleasesTheProjectSlot(t *testing.T) {
	store := newLocalForTest(t)
	ctx := context.Background()
	_, begin := pendingTerminalRun(t, store)
	at := testQueuedAt.Add(20 * time.Minute)

	if disposition, err := store.CloseUnreproducibleTerminal(ctx, recoveryClose(begin, 4242, at)); err != nil ||
		disposition != hook.TerminalCompleted {
		t.Fatalf("CloseUnreproducibleTerminal() = %s, %v", disposition, err)
	}
	run := claimedRunOverview(t, store)
	if run.State != "terminal" || run.TerminalCode != string(hook.TerminalModelFailed) ||
		run.TerminalReportSHA256 != begin.ReportSHA256 {
		t.Fatalf("run after the close = %+v", run)
	}
	// Replaying the same close is the same answer; closing it against some
	// other comment is not.
	if disposition, err := store.CloseUnreproducibleTerminal(ctx, recoveryClose(begin, 4242, at.Add(time.Minute))); err != nil ||
		disposition != hook.TerminalAlreadyComplete {
		t.Fatalf("replay = %s, %v", disposition, err)
	}
	if disposition, _ := store.CloseUnreproducibleTerminal(ctx, recoveryClose(begin, 99, at.Add(time.Minute))); disposition != hook.TerminalCompleteConflict {
		t.Fatalf("a close against another comment = %s", disposition)
	}
	// The slot is free: another ticket in the same project can be queued.
	next := testQueueRequest(t)
	next.Envelope = otherTicketEnvelope(t)
	next.QueuedAt = at
	if disposition, err := store.Enqueue(ctx, next); err != nil || disposition != hook.QueueCreated {
		t.Fatalf("Enqueue() after the close = %s, %v", disposition, err)
	}
}

// The close is narrow on purpose: it is the only way out of report_pending
// that does not reproduce the report, so everything that could make it the
// wrong row, the wrong ending or a live one refuses it.
func TestTheUnreproducibleCloseRefusesEverythingButItsOwnPendingRow(t *testing.T) {
	ctx := context.Background()
	at := testQueuedAt.Add(20 * time.Minute)

	t.Run("a live lease", func(t *testing.T) {
		store := newLocalForTest(t)
		_, begin := pendingTerminalRun(t, store)
		inside := begin.LeaseUntil.Add(-time.Second)
		if disposition, _ := store.CloseUnreproducibleTerminal(ctx, recoveryClose(begin, 4242, inside)); disposition != hook.TerminalCompleteConflict {
			t.Fatalf("a report still being written was closed: %s", disposition)
		}
	})
	t.Run("another digest", func(t *testing.T) {
		store := newLocalForTest(t)
		_, begin := pendingTerminalRun(t, store)
		request := recoveryClose(begin, 4242, at)
		request.ReportSHA256 = strings.Repeat("d", 64)
		if disposition, _ := store.CloseUnreproducibleTerminal(ctx, request); disposition != hook.TerminalCompleteConflict {
			t.Fatalf("a row was closed against another ending's digest: %s", disposition)
		}
	})
	t.Run("another ending", func(t *testing.T) {
		store := newLocalForTest(t)
		_, begin := pendingTerminalRun(t, store)
		request := recoveryClose(begin, 4242, at)
		request.Code = hook.TerminalSuccess
		if disposition, _ := store.CloseUnreproducibleTerminal(ctx, request); disposition != hook.TerminalCompleteConflict {
			t.Fatalf("a model failure was closed as a success: %s", disposition)
		}
	})
	t.Run("a claimed run", func(t *testing.T) {
		store := newLocalForTest(t)
		envelope := localClaim(t, store)
		begin := testTerminalBegin(t, envelope, hook.TerminalModelFailed, testQueuedAt.Add(5*time.Second), strings.Repeat("a", 32))
		if disposition, _ := store.CloseUnreproducibleTerminal(ctx, recoveryClose(begin, 4242, at)); disposition != hook.TerminalCompleteConflict {
			t.Fatalf("a run that is still working was closed: %s", disposition)
		}
	})
	t.Run("no comment", func(t *testing.T) {
		store := newLocalForTest(t)
		_, begin := pendingTerminalRun(t, store)
		if _, err := store.CloseUnreproducibleTerminal(ctx, recoveryClose(begin, 0, at)); err == nil {
			t.Fatal("a row was closed with nothing on the ticket")
		}
	})
	t.Run("another run", func(t *testing.T) {
		store := newLocalForTest(t)
		_, begin := pendingTerminalRun(t, store)
		request := recoveryClose(begin, 4242, at)
		request.RunID = "run_20260802_absent"
		request.Route.ExpectedRunID = request.RunID
		if _, err := store.CloseUnreproducibleTerminal(ctx, request); err == nil {
			t.Fatal("a run that does not exist was closed")
		}
	})
}

// otherTicketEnvelope is a second ticket in the same project, for checking
// that the closed run no longer holds the project's one slot.
func otherTicketEnvelope(t *testing.T) hook.DispatchEnvelope {
	t.Helper()
	snapshot := testEnvelope(t).Snapshot
	snapshot.ActivityID = 9002
	snapshot.IssueID = 8002
	snapshot.IssueKey = "TICKET-502"
	snapshot.IssueKeyID = 502
	snapshot.RunID = "run_20260802_beta"
	envelope, err := hook.SealSnapshot(snapshot)
	if err != nil {
		t.Fatalf("SealSnapshot() error = %v", err)
	}
	return envelope
}
