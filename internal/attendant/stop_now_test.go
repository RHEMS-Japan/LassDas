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
			if record := readStopRead(h.runDir); record.Waiting != stage || !record.stopped() || !record.Acknowledged {
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

// What the stop costs the tracker.
//
// The read is the first thing every claimed run's pass does, and the pass
// comes round every ten seconds. Unthrottled that is six listings a minute
// for every delivery at once, each paging through a long ticket — the cost
// the ladder's own waiting stages already refused to pay. So the ticket is
// listed at most once per interval per run, and the stop is honoured within
// that interval plus one pass, which is inside the minute the operating
// guide promises.
func TestTheTicketIsListedAtMostOnceAnIntervalForAStop(t *testing.T) {
	h := newDepthHarness(t, "production", true, "")
	unpublished(t, h)
	runningChainCard(t, h, runtime.StageImplement)

	const passes = 200
	started := time.Now()
	for range passes {
		h.tick()
	}
	elapsed := time.Since(started)

	// The bound is the contract rather than a fixed number, so a slow
	// machine cannot make this either flaky or meaningless: however long
	// the passes took, the ticket may have been read once per interval
	// they spanned, and no more.
	allowed := int32(elapsed/tickStopReadInterval) + 1
	got := h.listings.Load()
	if got > allowed {
		t.Fatalf("%d passes in %s listed the ticket %d times, want at most %d",
			passes, elapsed.Round(time.Millisecond), got, allowed)
	}
	t.Logf("%d passes in %s listed the ticket %d time(s)", passes, elapsed.Round(time.Millisecond), got)

	// A stop written inside the interval waits for it, and is then honoured
	// on the very next pass. The wait is made to have passed by moving the
	// recorded read time back, which is how the ladder's own throttle is
	// measured (TestAuditWaitingStopThrottle).
	*h.comments = append(*h.comments, ticketComment(stopCommentID, depthRequester, "停止"))
	h.tick()
	if row := h.runRow(); row.State != "claimed" {
		t.Fatalf("run = %s / %s inside the interval, want it untouched until the ticket is read again",
			row.State, row.TerminalCode)
	}
	rewindStopRead(t, h.runDir, 2*tickStopReadInterval)

	h.tick()

	row := h.runRow()
	if row.State != "terminal" || row.TerminalCode != string(hook.TerminalCancelled) {
		t.Fatalf("run = %s / %s once the interval had passed, want a terminal cancelled (log: %v)",
			row.State, row.TerminalCode, h.logger.lines)
	}
	if got := stopAcknowledgements(*h.posted); got != 1 {
		t.Fatalf("stop acknowledgements = %d, want exactly one: %q", got, *h.posted)
	}
}

// The throttle is on the tick's own read and on nothing else. Every read
// that stands between a requester and money being spent is unthrottled and
// stays so: here the merge, whose own read finds a stop the tick had not
// got round to looking for, and refuses to merge.
func TestTheReadBeforeTheMergeIsNotThrottled(t *testing.T) {
	h := newDepthHarness(t, "production", true, "")
	h.setBoard(h.card(deliverStageChecks, "done", 1))
	h.write(runner.DeliverChecksFile, `{"ok":true}`)
	// The tick has just read the ticket and found nothing, so its own read
	// is closed for the interval.
	writeStopRead(h.runDir, stopReadRecord{LastReadAt: time.Now().UTC()}, h.logger)
	*h.comments = append(*h.comments, ticketComment(stopCommentID, depthRequester, "停止"))

	h.tick()

	row := h.runRow()
	if row.TerminalCode != string(hook.TerminalCancelled) {
		t.Fatalf("run = %s / %s, want the merge's own read to have stopped it (log: %v)",
			row.State, row.TerminalCode, h.logger.lines)
	}
	if calls := h.calls(); strings.Contains(calls, "deliver:integrate") {
		t.Fatalf("the change was merged after the requester asked to stop:\n%s", calls)
	}
}

// A pod replaced between the acknowledgement and the ending. The record on
// the volume is gone, so the run reads its ticket again from scratch — and
// finds its own answer there, by the marker, rather than posting a second
// one. The requester gets one acknowledgement and one closing report for
// the whole run.
func TestAStopSurvivesALostRecordWithoutAnsweringTwice(t *testing.T) {
	h := newDepthHarness(t, "production", true, "")
	board := []runtime.BoardTask{h.card(deliverStageChecks, "done", 1)}
	h.write(runner.DeliverChecksFile, `{"ok":true}`)
	h.setBoard(append(board, h.card(deliverStageIntegrate, "in_progress", 1))...)
	*h.comments = append(*h.comments, ticketComment(stopCommentID, depthRequester, "停止"))

	h.tick()
	if row := h.runRow(); row.State != "claimed" {
		t.Fatalf("run = %s / %s, want it kept while the merge finishes", row.State, row.TerminalCode)
	}
	forgetStopRead(t, h.runDir)

	h.tick()

	if row := h.runRow(); row.State != "claimed" {
		t.Fatalf("run = %s / %s after the record was lost, want it still kept", row.State, row.TerminalCode)
	}
	if got := stopAcknowledgements(*h.posted); got != 1 {
		t.Fatalf("stop acknowledgements after a lost record = %d, want one: %q", got, *h.posted)
	}
	if record := readStopRead(h.runDir); !record.stopped() || !record.Acknowledged {
		t.Fatalf("the record was not rebuilt from the ticket: %+v", record)
	}

	// The merge finishes, and the record is lost once more before the pass
	// that ends the run.
	h.setBoard(append(board, h.card(deliverStageIntegrate, "done", 1))...)
	forgetStopRead(t, h.runDir)
	h.tick()

	if row := h.runRow(); row.TerminalCode != string(hook.TerminalCancelled) {
		t.Fatalf("run = %s / %s, want cancelled (log: %v)", row.State, row.TerminalCode, h.logger.lines)
	}
	if got := stopAcknowledgements(*h.posted); got != 1 {
		t.Fatalf("stop acknowledgements over the whole run = %d, want one: %q", got, *h.posted)
	}
	if got := terminalComments(*h.posted); got != 1 {
		t.Fatalf("closing reports = %d, want one: %q", got, *h.posted)
	}
}

// An investigation-only delivery is stopped by the same early read as every
// other shape. It used to have a stop read of its own, just before its
// report left the pod; the early read reaches it first, so that one was
// removed and this is what holds the behaviour.
func TestAnInvestigationOnlyDeliveryIsStoppedBeforeItsReport(t *testing.T) {
	h := newDepthHarness(t, "production", true, "")
	unpublished(t, h)
	h.config.Chain.Profiles = designTestProfiles()
	h.write("history/readiness/decision.json", `{"request_kind":"investigation"}`)
	investigating(t, h)
	*h.comments = append(*h.comments, ticketComment(stopCommentID, depthRequester, "停止"))

	h.tick()

	row := h.runRow()
	if row.State != "terminal" || row.TerminalCode != string(hook.TerminalCancelled) {
		t.Fatalf("run = %s / %s, want a terminal cancelled (log: %v)",
			row.State, row.TerminalCode, h.logger.lines)
	}
	if got := stopAcknowledgements(*h.posted); got != 1 {
		t.Fatalf("stop acknowledgements = %d, want exactly one: %q", got, *h.posted)
	}
	report := hook.CommentMarker(string(hook.RunCommentInvestigation), depthRunID)
	if strings.Contains(strings.Join(*h.posted, "\n"), report) {
		t.Fatalf("the investigation report was posted after the requester asked to stop: %q", *h.posted)
	}
	if calls := h.calls(); !strings.Contains(calls, "|archive|design_"+runtime.StageInvestigate+"|") {
		t.Fatalf("the running card was not retired:\n%s", calls)
	}
}

// investigating puts an investigation-only delivery's first card in flight.
func investigating(t *testing.T, h *depthHarness) {
	t.Helper()
	tasks := []runtime.BoardTask{{ID: "design_" + runtime.StageInvestigate, Status: "in_progress",
		IdempotencyKey: runtime.ChainCardKey(h.deliveryID, runtime.StageInvestigate, 1)}}
	encoded, err := json.Marshal(tasks)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.boardFile, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

// terminalComments counts the closing reports on the ticket.
func terminalComments(posted []string) int {
	count := 0
	for _, comment := range posted {
		if strings.Contains(comment, hook.TerminalMarkerPrefix(depthRunID)) {
			count++
		}
	}
	return count
}

// rewindStopRead moves the recorded read time back, so the next pass is due
// to read the ticket again without the test having to wait.
func rewindStopRead(t *testing.T, runDir string, by time.Duration) {
	t.Helper()
	record := readStopRead(runDir)
	if record.LastReadAt.IsZero() {
		t.Fatal("nothing has read the ticket yet; this test is looking at the wrong state")
	}
	record.LastReadAt = record.LastReadAt.Add(-by)
	writeStopRead(runDir, record, &recordingLogger{})
}

// forgetStopRead takes the record off the volume, which is what a pod
// replaced mid-stop comes back to.
func forgetStopRead(t *testing.T, runDir string) {
	t.Helper()
	if err := os.Remove(filepath.Join(runDir, stopReadFile)); err != nil {
		t.Fatal(err)
	}
}
