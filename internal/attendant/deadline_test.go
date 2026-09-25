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
	"automation.internal/ticket-ingress/internal/state"
)

// blockChainStage puts one chain card in the state a failed card leaves the
// board in — everything before it finished, it is blocked, everything after
// it is waiting — and seals the account the card itself would have sealed.
func blockChainStage(t *testing.T, h *depthHarness, stage string, class runner.FailureClass) {
	t.Helper()
	tasks := []runtime.BoardTask{}
	after := false
	for _, name := range []string{
		runtime.StageImplement, runtime.StageReviewA, runtime.StageReviewB,
		runtime.StageValidate, runtime.StagePublish,
	} {
		status := "done"
		switch {
		case name == stage:
			status, after = "blocked", true
		case after:
			status = "todo"
		}
		tasks = append(tasks, runtime.BoardTask{ID: "t_" + name, Status: status,
			IdempotencyKey: runtime.ChainCardKey(h.deliveryID, name, 1)})
	}
	encoded, err := json.Marshal(tasks)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.boardFile, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	sealCardFailure(t, h.runDir, runner.StageFailure{
		Stage: stage, Round: 1, Class: class, Error: "whatever it was",
		FailedAt: time.Now().UTC(),
	})
}

func claimedRun(claimedAtMillis int64) state.RunOverview {
	return state.RunOverview{RunID: "TKT-4242", DeliveryID: "delivery_" + strings.Repeat("ab", 16),
		State: "claimed", ClaimedAt: claimedAtMillis}
}

// A delivery that cannot get past a failure now has an end.
//
// Every rung of the ladder is a remedy and the last rung is a wait that
// grows, so a failure nobody clears left a run climbing for as long as the
// pod lived: measured 2026-09-26, twenty passes over the same refusal, one
// notice on the ticket, seventy-six cards rebuilt and no report at all. The
// morning after is now a comment saying where the delivery got to.
func TestARunOutOfTimeInTheLadderEndsWithAnAccountOfWhereItGotTo(t *testing.T) {
	h := newDepthHarnessClaimedAt(t, hook.DeliverPullRequest, false, "", time.Now().UTC().Add(-9*time.Hour))
	blockChainStage(t, h, runtime.StageReviewA, runner.FailureClassCredit)

	h.tick()

	row := h.runRow()
	if row.State != "terminal" || row.TerminalCode != string(hook.TerminalDeadlineReached) {
		t.Fatalf("row = %s/%s, want a terminal delivery that ran out of time", row.State, row.TerminalCode)
	}
	if len(*h.posted) != 1 {
		t.Fatalf("comments posted = %d, want exactly one final report: %q", len(*h.posted), *h.posted)
	}
	report := (*h.posted)[0]
	// The ending is never the machinery's own breakdown. Nothing broke:
	// the engine worked, met something it could not get past, and ran out
	// of the night it was given.
	if strings.Contains(report, string(hook.TerminalInternalFailed)) {
		t.Fatalf("the delivery reported an internal failure:\n%s", report)
	}
	for _, want := range []string{
		"deadline_reached",
		"処理時間",              // it says the time ran out
		"8 時間",              // and how much it was given
		"AI の利用枠",           // and what it kept meeting
		"Pull Request は作成済み", // and what landed
		"## 続けるために必要なこと",    // and what a person would have to change
	} {
		if !strings.Contains(report, want) {
			t.Fatalf("the report does not say %q:\n%s", want, report)
		}
	}

	// And it is said once. A second pass over the same delivery must not
	// post a second report.
	h.tick()
	if len(*h.posted) != 1 {
		t.Fatalf("comments posted after a second tick = %d, want the one: %q", len(*h.posted), *h.posted)
	}
}

// And the account names whichever kind of failure the delivery kept
// meeting, including the one this change added: a step that will not fit in
// the time it is given.
func TestTheAccountOfARunOutOfTimeNamesAStepThatWouldNotFit(t *testing.T) {
	h := newDepthHarnessClaimedAt(t, hook.DeliverPullRequest, false, "", time.Now().UTC().Add(-9*time.Hour))
	blockChainStage(t, h, runtime.StageReviewA, runner.FailureClassTimeout)

	h.tick()

	if len(*h.posted) != 1 {
		t.Fatalf("comments posted = %d, want exactly one final report: %q", len(*h.posted), *h.posted)
	}
	report := (*h.posted)[0]
	if !strings.Contains(report, "時間内に終わりませんでした") {
		t.Fatalf("the report does not name a step that would not fit in its time:\n%s", report)
	}
	if !strings.Contains(report, "小さく分けて") {
		t.Fatalf("the report does not say what would make it fit:\n%s", report)
	}
}

// A delivery cut short with its change already on staging gives the screen.
//
// It is the likeliest shape of this ending — the promotion is the phase
// most apt to keep failing — and the one where saying where to look matters
// most. The report said twice that staging holds the change and never once
// gave the URL the run had in hand.
func TestARunOutOfTimeAfterStagingNamesTheScreen(t *testing.T) {
	h := newDepthHarnessClaimedAt(t, hook.DeliverProduction, true, "", time.Now().UTC().Add(-9*time.Hour))
	h.write(runner.DeliverChecksFile, `{"ok":true}`)
	h.sealPhase(runner.DeliverStagingReportFile, h.stagingPass())
	if err := os.MkdirAll(filepath.Join(h.runDir, "history", "deliver-1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := runner.SealStageFailureRecord(h.runDir, runner.StageFailure{
		Stage: deliverStagePromote, Round: deliverLadderRound, Class: runner.FailureClassNetwork,
		Error: "connection reset by peer", FailedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	h.setBoard(h.card(deliverStageChecks, "done", 1), h.card(deliverStageIntegrate, "done", 1),
		h.card(deliverStagePromote, "blocked", 1))

	h.tick()

	row := h.runRow()
	if row.State != "terminal" || row.TerminalCode != string(hook.TerminalDeadlineReached) {
		t.Fatalf("row = %s/%s, want a terminal delivery that ran out of time", row.State, row.TerminalCode)
	}
	if len(*h.posted) != 1 {
		t.Fatalf("comments posted = %d, want exactly one final report: %q", len(*h.posted), *h.posted)
	}
	report := (*h.posted)[0]
	if !strings.Contains(report, depthStagingHost+"/feature") {
		t.Fatalf("the report says staging holds the change and never gives the screen:\n%s", report)
	}
	if !strings.Contains(report, "staging への反映と確認までは完了しています") {
		t.Fatalf("the report does not say how far the delivery got:\n%s", report)
	}
}

// Inside its deadline the same delivery is climbed, not reported: the card
// is rebuilt and the ticket gets the ladder's single notice. Without this
// the test above would pass on a deadline that ended every delivery the
// moment it met a failure.
func TestADeliveryInsideItsDeadlineIsStillClimbed(t *testing.T) {
	h := newDepthHarnessClaimedAt(t, hook.DeliverPullRequest, false, "", time.Now().UTC().Add(-time.Hour))
	blockChainStage(t, h, runtime.StageReviewA, runner.FailureClassCredit)

	for tick := 0; tick < 3; tick++ {
		h.tick()
	}

	row := h.runRow()
	if row.State != "claimed" || row.TerminalCode != "" {
		t.Fatalf("row = %s/%s, want a delivery still being climbed", row.State, row.TerminalCode)
	}
	// A key at its limit is told at once, and told once however many
	// passes the wait lasts.
	if len(*h.posted) != 1 {
		t.Fatalf("comments posted = %d, want the ladder's single notice: %q", len(*h.posted), *h.posted)
	}
	if strings.Contains((*h.posted)[0], string(hook.TerminalDeadlineReached)) {
		t.Fatalf("a delivery inside its deadline reported:\n%s", (*h.posted)[0])
	}
}

// The same wall, for the other way a delivery goes round for ever: an
// implementing agent that answers every time and refuses every time.
//
// That path never reaches the ladder — a round the engine answers is
// relaunched rather than climbed — so the deadline had to be asked of it
// separately. Measured 2026-09-26: fifteen returns, seventy cards rebuilt,
// nothing reported.
func TestARunOutOfTimeWhileTheWorkIsHandedBackEndsWithAnAccount(t *testing.T) {
	const refusal = "この依頼は、いまのままでは実現できません。"
	s := newReturnedSetup(t, refusal)

	// A few hours of the engine answering the return and the agent handing
	// the work straight back. Nothing is reported while there is time.
	s.claimedAt = time.Now().UTC().Add(-time.Hour)
	for pass := 0; pass < 3; pass++ {
		if err := s.tick(t); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		s.returnAgain(t, refusal)
	}
	if len(s.fixture.comments.posted) != 0 {
		t.Fatalf("a delivery inside its deadline reported: %q", s.fixture.comments.posted)
	}

	// And then the night is over.
	s.claimedAt = time.Now().UTC().Add(-9 * time.Hour)
	s.fixture.store.expected = deadlineReportDigest(t, s)
	created := s.boardCreations(t)
	if err := s.tick(t); err != nil {
		t.Fatal(err)
	}

	if len(s.fixture.comments.posted) != 1 {
		t.Fatalf("comments posted = %d, want the one final report: %q",
			len(s.fixture.comments.posted), s.fixture.comments.posted)
	}
	report := s.fixture.comments.posted[0]
	if !strings.Contains(report, string(hook.TerminalDeadlineReached)) {
		t.Fatalf("the delivery did not end as a run out of time:\n%s", report)
	}
	// The account names the kind of failure it kept meeting. An agent that
	// answers and refuses seals an ordinary model failure, so the class
	// alone would say "the AI did not answer" about a round where it
	// answered every time.
	if !strings.Contains(report, "作業を返してきました") {
		t.Fatalf("the report does not say the work was handed back:\n%s", report)
	}
	if s.boardCreations(t) != created {
		t.Fatalf("the round was launched again after the time ran out: %d cards created, %d before",
			s.boardCreations(t), created)
	}
}

// And it stops before the round is launched again, not after.
//
// A first return is one the engine answers: it decides what the report
// asked about, writes the assumption down and starts the same round with it
// in the instruction. Out of time, that launch must not happen — the round
// it starts could only be cut off. This is the guard the ladder's own clock
// cannot stand in for, because an answered return never reaches the ladder.
func TestARunOutOfTimeBeforeTheReturnIsAnsweredIsNotLaunchedAgain(t *testing.T) {
	s := newReturnedSetup(t, "この依頼は、いまのままでは実現できません。")
	s.claimedAt = time.Now().UTC().Add(-9 * time.Hour)
	s.fixture.store.expected = deadlineReportDigest(t, s)

	if err := s.tick(t); err != nil {
		t.Fatal(err)
	}

	if len(s.fixture.comments.posted) != 1 {
		t.Fatalf("comments posted = %d, want the one final report: %q",
			len(s.fixture.comments.posted), s.fixture.comments.posted)
	}
	report := s.fixture.comments.posted[0]
	if !strings.Contains(report, string(hook.TerminalDeadlineReached)) {
		t.Fatalf("the delivery did not end as a run out of time:\n%s", report)
	}
	// The round's own account of why it stopped is sealed on the way out,
	// because the step that would have sealed one is the step being
	// skipped. Without it the report would say the time ran out and
	// nothing at all about what it ran out on.
	if !strings.Contains(report, "## 続けるために必要なこと") {
		t.Fatalf("the report says nothing about what was met or what to do:\n%s", report)
	}
	if s.boardCreations(t) != 0 {
		t.Fatalf("the round was launched again after the time ran out: %d cards created", s.boardCreations(t))
	}
}

// Before the deadline nothing changes: the return is answered and the round
// runs again.
func TestAReturnedRoundInsideTheDeadlineIsStillAnswered(t *testing.T) {
	s := newReturnedSetup(t, "この依頼は、いまのままでは実現できません。")
	// The fixture's own claim, not one this test sets: a row with no claim
	// time is one whose deadline is never read, and this would then prove
	// nothing about a delivery that is inside it.
	if s.claimedAt.IsZero() {
		t.Fatal("the fixture's ledger row carries no claim time, so no deadline is read from it")
	}

	if err := s.tick(t); err != nil {
		t.Fatal(err)
	}

	if len(s.fixture.comments.posted) != 0 {
		t.Fatalf("a delivery inside its deadline reported: %q", s.fixture.comments.posted)
	}
	if s.boardCreations(t) == 0 {
		t.Fatal("the answered round was not launched again")
	}
}

// deadlineReportDigest is the digest the store will be asked to bind this
// delivery's deadline report to. The fake ledger holds a report to the
// digest its row was begun with, as the real one does.
func deadlineReportDigest(t *testing.T, s *returnedSetup) string {
	t.Helper()
	terminal := runner.NewTerminal(s.config, s.fixture.services, s.envelope,
		chainOwnerRunID(s.fixture.deliveryID), s.runDir, s.logger)
	digest, err := terminal.ReportDigest(context.Background(), hook.TerminalDeadlineReached,
		runner.Outcome{Code: hook.TerminalDeadlineReached}, "example/consumer")
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

// A run whose ledger row carries no claim time is left alone. Those are
// rows from before the field and overviews built by hand; ending them on a
// clock that reads zero would report every delivery as out of time the
// moment it met its first failure.
func TestARunWithNoClaimTimeIsNeverOutOfTime(t *testing.T) {
	config := runtime.Config{}
	if _, passed := runDeadlinePassed(config, claimedRun(0), time.Now().UTC()); passed {
		t.Fatal("a run with no claim time was ended on the deadline")
	}
	// A claim in the future is a clock that moved backwards under the pod,
	// not a delivery that has been going for a negative length of time.
	ahead := time.Now().UTC().Add(time.Hour).UnixMilli()
	if _, passed := runDeadlinePassed(config, claimedRun(ahead), time.Now().UTC()); passed {
		t.Fatal("a claim in the future was read as a deadline that had passed")
	}
}

// The clock is the operator's, and the default is a night.
func TestTheDeadlineIsTheConfiguredOneAndDefaultsToANight(t *testing.T) {
	now := time.Now().UTC()
	if _, passed := runDeadlinePassed(runtime.Config{}, claimedRun(now.Add(-7*time.Hour).UnixMilli()), now); passed {
		t.Fatal("seven hours ended a delivery whose default deadline is eight")
	}
	run := claimedRun(now.Add(-9 * time.Hour).UnixMilli())
	if spent, passed := runDeadlinePassed(runtime.Config{}, run, now); !passed || spent < 9*time.Hour {
		t.Fatalf("spent = %v passed = %v, want nine hours past the eight-hour default", spent, passed)
	}
	// And a destination that asked for longer gets longer.
	twenty := 20
	longer := runtime.Config{Chain: runtime.ChainConfig{RunDeadlineHours: &twenty}}
	if _, passed := runDeadlinePassed(longer, run, now); passed {
		t.Fatal("nine hours ended a delivery whose operator allowed twenty")
	}
}
