package attendant

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
)

// The requester's stop, measured from the outside: one pass of the chain
// loop with 「停止」 on the ticket, while a card is running.
//
// The engine used to read the stop only at its boundaries, so these are
// exactly the six positions a delivery could be in and be deaf: an
// implementing, reviewing or validating card in flight, and a checking,
// merging or promoting one. Three passes left every one of them claimed
// with nothing on the ticket.

// stopCommentID is the stop's own comment id. It sits below the ids the
// harness gives to comments this engine posts, because the tracker hands
// comments back oldest first and the client refuses a listing that goes
// backwards — a stop written after the engine's own comments would be a
// different test than the one intended here.
const stopCommentID = int64(800)

// runningChainCard puts one chain card in flight: everything before it
// finished, it is running, everything after it is waiting its turn.
func runningChainCard(t *testing.T, h *depthHarness, active string) {
	t.Helper()
	tasks := []runtime.BoardTask{}
	waiting := false
	for _, stage := range []string{runtime.StageImplement, runtime.StageReviewA,
		runtime.StageReviewB, runtime.StageValidate, runtime.StagePublish} {
		status := "done"
		switch {
		case stage == active:
			status, waiting = "in_progress", true
		case waiting:
			status = "todo"
		}
		tasks = append(tasks, runtime.BoardTask{ID: "chain_" + stage, Status: status,
			IdempotencyKey: runtime.ChainCardKey(h.deliveryID, stage, 1)})
	}
	encoded, err := json.Marshal(tasks)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.boardFile, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

// stopAcknowledgements counts this engine's answers to the stop on the
// ticket. The marker is what makes them countable: a second answer would
// carry the same one.
func stopAcknowledgements(posted []string) int {
	count := 0
	for _, comment := range posted {
		if strings.Contains(comment, hook.StopAcknowledgedMarker(depthRunID)) {
			count++
		}
	}
	return count
}

// terminalComment is the closing report on the ticket, found by the marker
// every terminal report of this run carries whatever code it ended on.
func terminalComment(t *testing.T, posted []string) string {
	t.Helper()
	for _, comment := range posted {
		if strings.Contains(comment, hook.TerminalMarkerPrefix(depthRunID)) {
			return comment
		}
	}
	t.Fatalf("no closing report was posted: %q", posted)
	return ""
}

// unpublished takes away the records a publish card seals. A delivery whose
// implementation is still running has not proposed anything, and what the
// closing report may claim is what the cards actually sealed.
func unpublished(t *testing.T, h *depthHarness) {
	t.Helper()
	for _, name := range []string{"feature-pr.json", runner.ChainOutcomeFile} {
		if err := os.Remove(filepath.Join(h.runDir, name)); err != nil {
			t.Fatal(err)
		}
	}
}

// A stop while the implementer, a reviewer or the validation is running:
// the run is over inside the same pass, the ticket is answered once, the
// card is retired, and the report claims nothing — because nothing had
// landed.
func TestAStopDuringARunningChainCardEndsTheRunInOneTick(t *testing.T) {
	for _, stage := range []string{runtime.StageImplement, runtime.StageReviewA, runtime.StageValidate} {
		t.Run(stage, func(t *testing.T) {
			h := newDepthHarness(t, "production", true, "")
			unpublished(t, h)
			runningChainCard(t, h, stage)
			*h.comments = append(*h.comments, ticketComment(stopCommentID, depthRequester, "停止"))

			h.tick()

			row := h.runRow()
			if row.State != "terminal" || row.TerminalCode != string(hook.TerminalCancelled) {
				t.Fatalf("run = %s / %s, want a terminal cancelled in one tick (log: %v)",
					row.State, row.TerminalCode, h.logger.lines)
			}
			if got := stopAcknowledgements(*h.posted); got != 1 {
				t.Fatalf("stop acknowledgements = %d, want exactly one: %q", got, *h.posted)
			}
			report := terminalComment(t, *h.posted)
			for _, claim := range []string{"pull/9", depthStagingHost, depthProdHost} {
				if strings.Contains(report, claim) {
					t.Fatalf("the report claims %q, which nothing sealed:\n%s", claim, report)
				}
			}
			if calls := h.calls(); !strings.Contains(calls, "|archive|chain_"+stage+"|") {
				t.Fatalf("the running card was not retired:\n%s", calls)
			}
			if calls := h.calls(); strings.Contains(calls, "|create|") {
				t.Fatalf("a card was dispatched after the stop:\n%s", calls)
			}

			// And nothing starts afterwards either: the run is closed, and
			// the tail the terminal state runs is for successes only.
			h.tick()
			if calls := h.calls(); strings.Contains(calls, "|create|") {
				t.Fatalf("a card was dispatched on the tick after the stop:\n%s", calls)
			}
			if got := stopAcknowledgements(*h.posted); got != 1 {
				t.Fatalf("stop acknowledgements after a second tick = %d, want one: %q", got, *h.posted)
			}
		})
	}
}

// A stop while the CI wait is running. The change is proposed by then, so
// the report names the pull request — and nothing deeper, because the merge
// had not happened.
func TestAStopDuringTheRunningChecksCardEndsTheRunInOneTick(t *testing.T) {
	h := newDepthHarness(t, "production", true, "")
	h.setBoard(h.card(deliverStageChecks, "in_progress", 1))
	*h.comments = append(*h.comments, ticketComment(stopCommentID, depthRequester, "停止"))

	h.tick()

	row := h.runRow()
	if row.State != "terminal" || row.TerminalCode != string(hook.TerminalCancelled) {
		t.Fatalf("run = %s / %s, want a terminal cancelled in one tick (log: %v)",
			row.State, row.TerminalCode, h.logger.lines)
	}
	if got := stopAcknowledgements(*h.posted); got != 1 {
		t.Fatalf("stop acknowledgements = %d, want exactly one: %q", got, *h.posted)
	}
	report := terminalComment(t, *h.posted)
	if !strings.Contains(report, "https://github.com/example/consumer/pull/9") {
		t.Fatalf("the report does not name the pull request that was opened:\n%s", report)
	}
	for _, claim := range []string{depthStagingHost, depthProdHost, depthMergeSHA} {
		if strings.Contains(report, claim) {
			t.Fatalf("the report claims %q, which nothing sealed:\n%s", claim, report)
		}
	}
	if calls := h.calls(); !strings.Contains(calls, "|archive|t_"+deliverStageChecks+"|") {
		t.Fatalf("the running card was not retired:\n%s", calls)
	}
	if calls := h.calls(); strings.Contains(calls, "|create|") {
		t.Fatalf("a card was dispatched after the stop:\n%s", calls)
	}
}

// A stop while one of the two cards that merge is running.
//
// This is the one card the stop does not abandon. Retiring it would stop
// this engine reading the result, not the merge, and a report written over
// a merge in flight would name a depth that changed a second later. So the
// requester is answered in the same pass, nothing else is dispatched, and
// the ending waits for the card — then carries what actually landed.
func TestAStopDuringAMergeIsAcknowledgedAndEndsAfterTheCard(t *testing.T) {
	for _, stage := range []string{deliverStageIntegrate, deliverStagePromote} {
		t.Run(stage, func(t *testing.T) {
			h := newDepthHarness(t, "production", true, "")
			board := []runtime.BoardTask{h.card(deliverStageChecks, "done", 1)}
			h.write(runner.DeliverChecksFile, `{"ok":true}`)
			if stage == deliverStagePromote {
				board = append(board, h.card(deliverStageIntegrate, "done", 1))
				h.sealPhase(runner.DeliverStagingReportFile, h.stagingPass())
			}
			h.setBoard(append(board, h.card(stage, "in_progress", 1))...)
			*h.comments = append(*h.comments, ticketComment(stopCommentID, depthRequester, "停止"))

			h.tick()

			row := h.runRow()
			if row.State != "claimed" || row.TerminalCode != "" {
				t.Fatalf("run = %s / %s, want it kept while the merge finishes (log: %v)",
					row.State, row.TerminalCode, h.logger.lines)
			}
			if record := readStopRequest(h.runDir); record.Waiting != stage || record.At.IsZero() || !record.Acknowledged {
				t.Fatalf("the stop record = %+v, want it waiting on %s and acknowledged", record, stage)
			}
			if got := stopAcknowledgements(*h.posted); got != 1 {
				t.Fatalf("stop acknowledgements = %d, want exactly one: %q", got, *h.posted)
			}
			if calls := h.calls(); strings.Contains(calls, "|create|") || strings.Contains(calls, "|archive|") {
				t.Fatalf("a card was dispatched or retired over a merge in flight:\n%s", calls)
			}

			// A second pass while it is still running changes nothing, and
			// does not answer the ticket a second time.
			h.tick()
			if row := h.runRow(); row.State != "claimed" || row.TerminalCode != "" {
				t.Fatalf("run = %s / %s on the second pass, want it still kept", row.State, row.TerminalCode)
			}
			if got := stopAcknowledgements(*h.posted); got != 1 {
				t.Fatalf("stop acknowledgements after a second tick = %d, want one: %q", got, *h.posted)
			}

			// The card finishes. Now the run ends, carrying what the cards
			// sealed: the proposal, and the staging landing when there was
			// one.
			h.setBoard(append(board, h.card(stage, "done", 1))...)
			h.tick()

			row = h.runRow()
			if row.TerminalCode != string(hook.TerminalCancelled) {
				t.Fatalf("run = %s / %s once the merge finished, want cancelled (log: %v)",
					row.State, row.TerminalCode, h.logger.lines)
			}
			report := terminalComment(t, *h.posted)
			if !strings.Contains(report, "https://github.com/example/consumer/pull/9") {
				t.Fatalf("the report does not name the pull request that was opened:\n%s", report)
			}
			landed := stage == deliverStagePromote
			if strings.Contains(report, depthMergeSHA) != landed {
				t.Fatalf("the report's account of the staging landing does not match what was sealed:\n%s", report)
			}
			if strings.Contains(report, depthProdHost) {
				t.Fatalf("the report claims production, which nothing sealed:\n%s", report)
			}
			if got := stopAcknowledgements(*h.posted); got != 1 {
				t.Fatalf("stop acknowledgements = %d over the whole run, want one: %q", got, *h.posted)
			}
		})
	}
}

// The requester's stop outranks the operator's pause on the intake. The
// request was never started, so nothing changed anywhere — and the ticket
// gets the closing report rather than a notice that it is queued behind a
// pause it will never come out of.
func TestAQueuedRunIsStoppedWhileIntakeIsPaused(t *testing.T) {
	h := newDepthHarness(t, "production", true, "")
	requeue(t, h)
	h.config.Chain.IntakePausedSince = "2026-09-25T00:00:00Z"
	*h.comments = append(*h.comments, ticketComment(stopCommentID, depthRequester, "停止"))

	h.tick()

	assertStoppedWithNothingChanged(t, h)
	if strings.Contains(strings.Join(*h.posted, "\n"), string(hook.RunCommentIntakePaused)) {
		t.Fatalf("a withdrawn request was told to wait for the intake to resume: %q", *h.posted)
	}
}

// The same for the budget hold, which re-probes every ten minutes: a
// withdrawn request is not worth finding out whether it could afford to
// start.
func TestAQueuedRunIsStoppedWhileTheBudgetHoldIsOn(t *testing.T) {
	h := newDepthHarness(t, "production", true, "")
	requeue(t, h)
	writeJSON(t, filepath.Join(h.runDir, budgetHoldFile), budgetHold{Roles: []string{"review"}, At: time.Now().UTC()})
	*h.comments = append(*h.comments, ticketComment(stopCommentID, depthRequester, "停止"))

	h.tick()

	assertStoppedWithNothingChanged(t, h)
}

// requeue puts the harness's claimed run back in the queue, which is where
// both intake holds act.
func requeue(t *testing.T, h *depthHarness) {
	t.Helper()
	row := h.runRow()
	if err := h.services.Store.RecoverLostClaim(context.Background(), row.Key, row.ClaimedAt, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if row := h.runRow(); row.State != "queued" {
		t.Fatalf("the run is %s, not queued; this test is about the queue", row.State)
	}
}

func assertStoppedWithNothingChanged(t *testing.T, h *depthHarness) {
	t.Helper()
	row := h.runRow()
	if row.State != "terminal" || row.TerminalCode != string(hook.TerminalCancelled) {
		t.Fatalf("run = %s / %s, want a terminal cancelled in one tick (log: %v)",
			row.State, row.TerminalCode, h.logger.lines)
	}
	report := terminalComment(t, *h.posted)
	for _, claim := range []string{"pull/9", depthStagingHost, depthProdHost, depthMergeSHA} {
		if strings.Contains(report, claim) {
			t.Fatalf("a request that never started claims %q:\n%s", claim, report)
		}
	}
	if calls := h.calls(); strings.Contains(calls, "|create|") {
		t.Fatalf("a card was dispatched for a withdrawn request:\n%s", calls)
	}
}
