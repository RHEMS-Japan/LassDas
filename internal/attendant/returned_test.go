package attendant

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
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
	"automation.internal/ticket-ingress/internal/worker"
)

// returnedSetup is a delivery whose round-1 implement card blocked because
// the agent changed nothing and explained why.
type returnedSetup struct {
	fixture  pendingFixture
	config   runtime.Config
	envelope hook.DispatchEnvelope
	view     chainView
	runDir   string
	hermes   *runtime.Hermes
	board    string
	tracker  *returnTracker
	logger   *recordingLogger
	// claimedAt is when the ledger says this delivery was claimed, which
	// is what its deadline is measured from. A minute ago by default, so a
	// test about the answering is not also a test about the clock; a test
	// about the clock moves it.
	claimedAt time.Time
}

// returningConsumer is a destination with the three seats an
// implementation round runs, which is what the ladder reads to decide what
// it may change about a card that keeps failing.
const returningConsumer = `{"max_stages":3,"models":{"implementer":{"id":"implementer"},` +
	`"reviewers":[{"id":"review-a"},{"id":"review-b"}]}}`

// returnTracker answers a listing with whatever comments the ticket is
// standing at, and keeps what was posted to it. The ladder's one notice
// goes through this rather than through the terminal report's own comments.
type returnTracker struct {
	listing string
	posted  []string
}

func (r *returnTracker) client(t *testing.T) *backlog.Client {
	t.Helper()
	client, err := backlog.NewClient(backlog.Config{
		SpaceKey: "example", APIKey: "k", Origin: "https://example.backlog.com",
		Timeout: time.Second, MaxResponseBytes: 1 << 20,
	}, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, status := r.listing, http.StatusOK
		if request.Method == http.MethodPost {
			raw, _ := io.ReadAll(request.Body)
			values, _ := url.ParseQuery(string(raw))
			content := values.Get("content")
			r.posted = append(r.posted, content)
			// Answered the way the tracker answers, so the caller accepts
			// it: a rejected answer would leave the notice unrecorded and
			// posted again on the next tick, which is the very thing the
			// record exists to prevent.
			encoded, _ := json.Marshal(map[string]any{
				"id": int64(9000 + len(r.posted)), "issueId": int64(4242), "content": content,
				"created": "2026-09-25T00:00:00Z", "createdUser": map[string]any{"id": int64(7)},
			})
			body, status = string(encoded), http.StatusCreated
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func newReturnedSetup(t *testing.T, report string) *returnedSetup {
	t.Helper()
	fixture := newPendingFixture(t, "")
	fixture.writeRunDir(t, "example/consumer")
	runDir := runDirectory(fixture.config, fixture.deliveryID)
	writeReturnedRound(t, runDir, report)
	// The shape the reception decided, which is what says which cards this
	// delivery has and therefore which instruction its round is rendered
	// from.
	writeChainShape(t, runDir, false)

	config := fixture.config
	config.Chain.Profiles = designTestProfiles()
	config.Tracker.AllowedCreatorID = 7
	// An implementer seat, because the ladder's remedy for an implementing
	// card is the seat's: the instruction rebuilt shorter. Without one the
	// ladder has no hand to play and every return would go straight to the
	// wait, which is not the delivery this measures.
	if err := os.WriteFile(config.ConsumerConfigPath, []byte(returningConsumer), 0o600); err != nil {
		t.Fatal(err)
	}
	// A stand-in worker that records what verb it was asked for and writes
	// whatever --out it was given: the instruction this round is rendered
	// again from is the thing under test, not its contents.
	config.WorkerBin = filepath.Join(t.TempDir(), "worker")
	if err := os.WriteFile(config.WorkerBin, []byte(`#!/bin/sh
printf '%s\n' "$*" >> `+config.WorkerBin+`.log
out=""
prev=""
for a in "$@"; do
  [ "$prev" = "--out" ] && out="$a"
  prev="$a"
done
[ -n "$out" ] && printf 'INSTRUCTION\n' > "$out"
exit 0
`), 0o700); err != nil {
		t.Fatal(err)
	}
	tracker := &returnTracker{listing: "[]"}
	fixture.services.Backlog = tracker.client(t)

	var envelope hook.DispatchEnvelope
	if err := json.Unmarshal([]byte(fixture.run.EnvelopeJSON), &envelope); err != nil {
		t.Fatal(err)
	}
	card := func(id, stage, status string) runtime.BoardTask {
		return runtime.BoardTask{ID: id, Status: status, IdempotencyKey: runtime.ChainCardKey(fixture.deliveryID, stage, 1)}
	}
	view := chainViewFor([]runtime.BoardTask{
		card("t_impl", runtime.StageImplement, "blocked"),
		card("t_ra", runtime.StageReviewA, "todo"),
		card("t_rb", runtime.StageReviewB, "todo"),
		card("t_v", runtime.StageValidate, "todo"),
		card("t_p", runtime.StagePublish, "todo"),
	}, fixture.deliveryID)
	hermes, board := fakeBoard(t)
	return &returnedSetup{fixture: fixture, config: config, envelope: envelope, view: view,
		runDir: runDir, hermes: hermes, board: board, tracker: tracker, logger: &recordingLogger{},
		claimedAt: time.Now().UTC().Add(-time.Minute)}
}

// writeChainShape leaves the reception's decision about which cards this
// delivery has.
func writeChainShape(t *testing.T, runDir string, designed bool) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(runDir, "history", "readiness"), 0o755); err != nil {
		t.Fatal(err)
	}
	shape := `{"request_kind":"change","needs_design":false}`
	if designed {
		shape = `{"request_kind":"change","needs_design":true}`
	}
	if err := os.WriteFile(filepath.Join(runDir, "history", "readiness", "decision.json"), []byte(shape), 0o600); err != nil {
		t.Fatal(err)
	}
}

// returnAgain replaces the round's run record with another report, the way
// the card that ran again would have left it.
func (s *returnedSetup) returnAgain(t *testing.T, report string) {
	t.Helper()
	if err := os.Remove(filepath.Join(s.runDir, "history", "stage-1", "implementer-run.json")); err != nil {
		t.Fatal(err)
	}
	writeReturnedRound(t, s.runDir, report)
}

// boardCreations counts the cards the board was asked to make.
func (s *returnedSetup) boardCreations(t *testing.T) int {
	t.Helper()
	return strings.Count(s.boardCalls(t), "|create|")
}

func (s *returnedSetup) tick(t *testing.T) error {
	t.Helper()
	run := state.RunOverview{DeliveryID: s.fixture.deliveryID, RunID: "TKT-4242", IssueID: 4242, IssueKey: "TKT-4242"}
	if !s.claimedAt.IsZero() {
		run.ClaimedAt = s.claimedAt.UnixMilli()
	}
	return handleChainFailure(context.Background(), s.config, s.fixture.services, s.hermes, s.envelope, run, s.view,
		runtime.StageImplement, s.logger)
}

func (s *returnedSetup) workerCalls(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(s.config.WorkerBin + ".log")
	if err != nil {
		t.Fatalf("no verb was run: %v", err)
	}
	return string(raw)
}

func (s *returnedSetup) boardCalls(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(s.board)
	if err != nil {
		t.Fatalf("the board was not called: %v", err)
	}
	return string(raw)
}

// The implementer changed nothing and said why. That used to end the
// delivery and leave the requester holding the question overnight. The
// engine now answers it — the most defensible reading of what was left open
// — writes down what it assumed, and starts the same round again with that
// in the instruction. Nothing is posted to the ticket.
func TestAReturnedRoundIsAnsweredAndStartedAgain(t *testing.T) {
	const report = "依頼に、並び順を新しい順にするか古い順にするかが書かれていません。判断は依頼者に返します。"
	setup := newReturnedSetup(t, report)
	if err := setup.tick(t); err != nil {
		t.Fatalf("the returned round was not answered: %v", err)
	}
	if len(setup.fixture.comments.posted) != 0 {
		t.Fatalf("the requester was told something: %q", setup.fixture.comments.posted)
	}
	if len(setup.fixture.store.digests) != 0 {
		t.Fatalf("the delivery ended: %v", setup.fixture.store.digests)
	}

	record, err := runner.ReadReturns(setup.runDir, 1)
	if err != nil || record == nil {
		t.Fatalf("nothing was recorded beside the round: %+v %v", record, err)
	}
	answer := record.Latest()
	if answer == nil || answer.Attempt != 1 {
		t.Fatalf("the round's record = %+v", record)
	}
	if answer.Assumption.Kind != worker.AssumptionImplementerReturn {
		t.Fatalf("the engine recorded no assumption of its own: %+v", answer.Assumption)
	}
	if !strings.Contains(answer.Report, "並び順") {
		t.Fatalf("the record lost what the agent said: %q", answer.Report)
	}

	// The same round, rendered again, carrying the answer.
	calls := setup.workerCalls(t)
	if !strings.Contains(calls, "implement-instruction ") {
		t.Fatalf("the round's instruction was not rendered again:\n%s", calls)
	}
	want := "--returned " + runner.ReturnRecordFile(setup.runDir, 1)
	if !strings.Contains(calls, want) {
		t.Fatalf("the instruction was rendered without %q:\n%s", want, calls)
	}

	// The same round on the board too: a returned round sealed no candidate,
	// so moving to round 2 would leave a round that produced nothing and
	// count it against every later reader.
	board := setup.boardCalls(t)
	if !strings.Contains(board, "|create|") {
		t.Fatalf("no card was built back:\n%s", board)
	}
	if !strings.Contains(board, "implement r1") {
		t.Fatalf("the round that was started again is not round 1:\n%s", board)
	}
	if strings.Contains(board, " r2") {
		t.Fatalf("a returned round advanced the round number:\n%s", board)
	}
	if !strings.Contains(board, "|archive|t_impl|") {
		t.Fatalf("the blocked card was not archived before being built back:\n%s", board)
	}
	said := strings.Join(setup.logger.lines, "\n")
	if !strings.Contains(said, "the engine answered it and the round runs again") {
		t.Fatalf("nothing said what was done:\n%s", said)
	}
	if strings.Contains(said, "chain terminalized") {
		t.Fatalf("the delivery ended on a returned round:\n%s", said)
	}
}

// A report that asks for a key is answered the same way: a stand-in is
// asked for, and what has to be supplied for the real thing is written down
// where the requester's report is built from — not sent back as a request.
func TestAReturnAskingForAKeyRecordsTheStandInAndTheSupply(t *testing.T) {
	const report = "外部の決済サービスを呼ぶ必要がありますが、API キーが渡されていません。"
	setup := newReturnedSetup(t, report)
	if err := setup.tick(t); err != nil {
		t.Fatalf("the returned round was not answered: %v", err)
	}
	record, err := runner.ReadReturns(setup.runDir, 1)
	if err != nil || record.Latest() == nil {
		t.Fatalf("nothing was recorded beside the round: %+v %v", record, err)
	}
	answer := record.Latest()
	if answer.Assumption.Kind != worker.AssumptionCredentialSubstituted {
		t.Fatalf("the record does not say a stand-in was put in place: %+v", answer.Assumption)
	}
	if len(answer.Supply) != 1 || !strings.Contains(answer.Supply[0], "API キー") {
		t.Fatalf("what must be supplied was not recorded: %q", answer.Supply)
	}
	if !strings.Contains(answer.Instruction, "代役 (test double / fake)") {
		t.Fatalf("the round was not told to build a stand-in:\n%s", answer.Instruction)
	}
	if len(setup.fixture.comments.posted) != 0 {
		t.Fatalf("the requester was asked for a key: %q", setup.fixture.comments.posted)
	}
}

// A round that comes back saying exactly the same thing is a round the
// engine's answer did not move, so it is not answered a second time: the
// record says it repeated, the round's failure is sealed as the model
// declining the work, and the ladder takes it from there. Nothing about
// that reaches the requester.
func TestARepeatedReturnGoesToTheLadderAtOnce(t *testing.T) {
	const report = "この依頼は、いまのままでは実現できません。"
	setup := newReturnedSetup(t, report)
	if err := setup.tick(t); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	built := setup.boardCreations(t)
	if built == 0 {
		t.Fatal("the first return did not start the round again")
	}
	setup.returnAgain(t, report)
	if err := setup.tick(t); err != nil {
		t.Fatalf("second tick: %v", err)
	}

	record, err := runner.ReadReturns(setup.runDir, 1)
	if err != nil || record == nil || len(record.Returns) != 2 {
		t.Fatalf("the round's record = %+v %v", record, err)
	}
	first, second := record.Returns[0], record.Returns[1]
	if !first.Answered || first.Repeated {
		t.Fatalf("the first return = %+v", first)
	}
	if !second.Repeated || second.Answered {
		t.Fatalf("the second return = %+v, want it left to the ladder", second)
	}
	if len(record.Assumptions()) != 1 {
		t.Fatalf("assumptions = %d, want only the answer the engine made", len(record.Assumptions()))
	}
	// Sealed as what it is, so the ladder that reads sealed failures finds
	// one it has remedies for rather than one it can only wait out.
	failure, sealed := runner.ReadStageFailure(setup.runDir, runtime.StageImplement, 1)
	if !sealed || failure.Class != runner.FailureClassModel || failure.Interrupted {
		t.Fatalf("the round's failure = %+v (sealed %v)", failure, sealed)
	}
	if len(setup.fixture.comments.posted) != 0 || len(setup.fixture.store.digests) != 0 {
		t.Fatalf("the delivery ended over a repeat: %q %v",
			setup.fixture.comments.posted, setup.fixture.store.digests)
	}
	said := strings.Join(setup.logger.lines, "\n")
	if !strings.Contains(said, "handled as a model that will not do the work") {
		t.Fatalf("nothing said what the repeat became:\n%s", said)
	}
}

// Two returns that name different things are both answered, because each is
// a position the engine has not decided about yet. The third, repeating the
// second, is not.
func TestReturnsThatNameSomethingNewAreAnsweredAndARepeatIsNot(t *testing.T) {
	setup := newReturnedSetup(t, "並び順が依頼に書かれていません。")
	if err := setup.tick(t); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	const second = "表示件数も依頼に書かれていません。"
	setup.returnAgain(t, second)
	if err := setup.tick(t); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	record, err := runner.ReadReturns(setup.runDir, 1)
	if err != nil || len(record.Assumptions()) != 2 {
		t.Fatalf("assumptions after two different returns = %+v %v", record, err)
	}
	if _, sealed := runner.ReadStageFailure(setup.runDir, runtime.StageImplement, 1); sealed {
		t.Fatal("a return that named something new was refused")
	}

	setup.returnAgain(t, second)
	if err := setup.tick(t); err != nil {
		t.Fatalf("third tick: %v", err)
	}
	record, err = runner.ReadReturns(setup.runDir, 1)
	if err != nil || len(record.Returns) != 3 || len(record.Assumptions()) != 2 {
		t.Fatalf("the round's record after the repeat = %+v %v", record, err)
	}
	if _, sealed := runner.ReadStageFailure(setup.runDir, runtime.StageImplement, 1); !sealed {
		t.Fatal("the repeat did not reach the ladder")
	}
}

// The probe that found this change unbounded: an implementer that answers
// every relaunch the same way was started again two hundred times, with
// nothing on the ticket and no way out but the requester writing 「停止」 —
// the very thing this change exists to remove. It reaches the ladder
// instead, which waits, says so once, and stops launching an agent every
// tick.
func TestAnImplementerThatKeepsReturningReachesTheLadderAndWaits(t *testing.T) {
	const report = "この依頼は、いまのままでは実現できません。"
	setup := newReturnedSetup(t, report)
	setup.config.Chain.RetryNoticeAttempts = 1
	setup.config.Chain.RetryBackoffBaseSeconds = 3600
	for tick := 1; tick <= 8; tick++ {
		if err := setup.tick(t); err != nil {
			t.Fatalf("tick %d: %v", tick, err)
		}
		setup.returnAgain(t, report)
	}
	// The ladder is keeping the count, on the volume, where a replaced pod
	// picks it up again.
	retry := filepath.Join(setup.runDir, "retry", "implement-r1.json")
	if _, err := os.Stat(retry); err != nil {
		t.Fatalf("the delivery never reached the ladder: %v", err)
	}
	// It stops launching an agent every tick. One relaunch for the answer
	// the engine made, one dispatch for the ladder's single hand on an
	// implementing seat (the instruction rebuilt shorter), and then the
	// wait, which this test's backoff makes an hour long.
	if built := setup.boardCreations(t); built > 2*len(runtime.ChainStagesFor(setup.config.Chain, runtime.ChainPlan{Shape: runtime.ShapeImplement})) {
		t.Fatalf("cards built across eight ticks = %d, want the delivery waiting rather than launching", built)
	}
	// Said once, for sharing. Nothing waits for a reply.
	notices := 0
	for _, posted := range setup.tracker.posted {
		if strings.Contains(posted, setup.fixture.run.RunID) {
			notices++
		}
	}
	if notices != 1 {
		t.Fatalf("ladder notices posted = %d across eight ticks: %q", notices, setup.tracker.posted)
	}
	if len(setup.fixture.comments.posted) != 0 || len(setup.fixture.store.digests) != 0 {
		t.Fatalf("the delivery ended: %q %v", setup.fixture.comments.posted, setup.fixture.store.digests)
	}
}

// And when an operator did put a limit on the attempts, the delivery ends
// on the ladder's own code. Reported as an internal failure it would tell
// the requester the machinery broke, when what happened is that the
// implementing model would not do the work.
func TestAnOperatorsLimitEndsAReturningDeliveryOnTheLaddersCode(t *testing.T) {
	const report = "この依頼は、いまのままでは実現できません。"
	setup := newReturnedSetup(t, report)
	setup.config.Chain.RetryMaxAttempts = 1
	report2, err := hook.NewTerminalReportService(setup.fixture.services.Route, &sendBackFakeStore{},
		setup.fixture.comments, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	setup.fixture.services.Report = report2
	for tick := 1; tick <= 4; tick++ {
		if err := setup.tick(t); err != nil {
			t.Fatalf("tick %d: %v", tick, err)
		}
		setup.returnAgain(t, report)
	}
	if len(setup.fixture.comments.posted) != 1 {
		t.Fatalf("terminal comments = %q, want the one ending", setup.fixture.comments.posted)
	}
	posted := setup.fixture.comments.posted[0]
	if strings.Contains(posted, string(hook.TerminalInternalFailed)) {
		t.Fatalf("a model that would not work was reported as the machinery breaking:\n%s", posted)
	}
	if !strings.Contains(posted, string(hook.TerminalModelFailed)) {
		t.Fatalf("the delivery did not end on the ladder's code:\n%s", posted)
	}
}

// A render that fails must leave the board exactly as it was. Archived
// first, a failed render would take every card of the delivery with it and
// leave nothing to drive the run.
func TestAFailedRenderLeavesTheBoardAlone(t *testing.T) {
	setup := newReturnedSetup(t, "この依頼は、いまのままでは実現できません。")
	if err := os.WriteFile(setup.config.WorkerBin, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := setup.tick(t); err == nil {
		t.Fatal("a failed render was treated as a relaunch")
	}
	board, err := os.ReadFile(setup.board)
	if err == nil && (strings.Contains(string(board), "|archive|") || strings.Contains(string(board), "|create|")) {
		t.Fatalf("the board was changed for a render that failed:\n%s", board)
	}
}

// A designed request's applier can hand the work back exactly as an
// implementer can, and the round it is started again in has to be the
// delivery's own shape: rebuilt as an ordinary chain it would lose the
// design cards and apply a design the request no longer has.
func TestADesignedRoundThatIsHandedBackKeepsItsShape(t *testing.T) {
	setup := newDesignedReturnedSetup(t, "設計のこの部分は、いまのままでは適用できません。")
	run := state.RunOverview{DeliveryID: setup.fixture.deliveryID, RunID: "TKT-4242", IssueID: 4242, IssueKey: "TKT-4242"}
	if err := handleChainFailure(context.Background(), setup.config, setup.fixture.services, setup.hermes,
		setup.envelope, run, setup.view, runtime.StageApply, setup.logger); err != nil {
		t.Fatalf("the returned apply round was not answered: %v", err)
	}
	record, err := runner.ReadReturns(setup.runDir, 1)
	if err != nil || record.Latest() == nil || !record.Latest().Answered {
		t.Fatalf("the applier's return was not answered: %+v %v", record, err)
	}
	board := setup.boardCalls(t)
	if !strings.Contains(board, "apply r1") {
		t.Fatalf("the applier's card was not built back:\n%s", board)
	}
	if strings.Contains(board, "implement r1") {
		t.Fatalf("a designed delivery was rebuilt as an ordinary chain:\n%s", board)
	}
	// And the instruction it will read is the applier's, carrying what the
	// engine decided.
	instruction, err := os.ReadFile(filepath.Join(setup.runDir, "INSTRUCTION.md"))
	if err != nil {
		t.Fatalf("no instruction was rendered: %v", err)
	}
	if !strings.Contains(string(instruction), "この巡は一度戻ってきています") {
		t.Fatalf("the applier's instruction lost the engine's answer:\n%s", instruction)
	}
	if !strings.Contains(string(instruction), "設計のこの部分") {
		t.Fatalf("the applier's instruction lost its own previous report:\n%s", instruction)
	}
}

// The requester's stop is one of the two endings a delivery is allowed to
// have, and a round that starts itself again is exactly where it has to be
// heard. What the engine had decided by then is still written down.
func TestAStopIsHonouredBeforeAReturnedRoundStartsAgain(t *testing.T) {
	setup := newReturnedSetup(t, "この依頼は、いまのままでは実現できません。")
	setup.tracker.listing = `[{"id":1,"issueId":4242,"content":"停止","created":"2026-09-25T00:00:00Z","createdUser":{"id":7}}]`
	// A ledger with no report already begun on the row: the shared fixture
	// pins a model failure's digest, and this delivery ends as a stop.
	report, err := hook.NewTerminalReportService(setup.fixture.services.Route, &sendBackFakeStore{},
		setup.fixture.comments, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	setup.fixture.services.Report = report
	if err := setup.tick(t); err != nil {
		t.Fatalf("the stop was not handled: %v", err)
	}
	if len(setup.fixture.comments.posted) != 1 {
		t.Fatalf("terminal comments = %q, want the one that says it stopped", setup.fixture.comments.posted)
	}
	if !strings.Contains(setup.fixture.comments.posted[0], string(hook.TerminalCancelled)) {
		t.Fatalf("the delivery did not end as a stop:\n%s", setup.fixture.comments.posted[0])
	}
	if board := setup.boardCalls(t); strings.Contains(board, "|create|") {
		t.Fatalf("a card was built after the requester asked to stop:\n%s", board)
	}
	// Read before the round is rendered, not only before the cards are
	// built. The ladder's dispatcher reads 「停止」 too, so without this the
	// engine would still pay to render an instruction for a delivery the
	// requester has already stopped.
	if strings.Contains(setup.workerCalls(t), "implement-instruction ") {
		t.Fatalf("the round was rendered after the requester asked to stop:\n%s", setup.workerCalls(t))
	}
	// What was decided before the stop was read still belongs to the record.
	record, readErr := runner.ReadReturns(setup.runDir, 1)
	if readErr != nil || record.Latest() == nil {
		t.Fatalf("the answer was dropped when the stop was read: %+v %v", record, readErr)
	}
}

// The ending this path used to have. It stays in the vocabulary — ledger
// rows and comments already posted name it — and nothing produces it any
// more, whatever the card and whatever the round sealed.
func TestNoClassificationProducesImplementationReturned(t *testing.T) {
	outcomes := []func() (string, error){
		func() (string, error) { return "revise", nil },
		func() (string, error) { return "converged", nil },
		func() (string, error) { return "nonconverged", nil },
		func() (string, error) { return "", os.ErrNotExist },
	}
	stages := []string{
		runtime.StageImplement, runtime.StageApply, runtime.StageValidate,
		runtime.StagePublish, runtime.StageReviewA, runtime.StageReviewB,
	}
	for _, stage := range stages {
		for _, decision := range outcomes {
			for _, returned := range []func() bool{func() bool { return true }, func() bool { return false }} {
				action, code := classifyChainFailure(stage, decision, returned)
				if code == hook.TerminalImplementationReturned {
					t.Fatalf("%s still ends as a returned implementation (action %v)", stage, action)
				}
			}
		}
	}
	// And the implement card's own answer routes into the round rather than
	// into a report.
	action, _ := classifyChainFailure(runtime.StageImplement,
		func() (string, error) { return "", os.ErrNotExist }, func() bool { return true })
	if action != actionAnswerReturn {
		t.Fatalf("a returned round is classified as %v", action)
	}
}

// newDesignedReturnedSetup is the same delivery as a designed request: the
// design was approved, and the applier that was to copy it into the working
// copy changed nothing and said why.
func newDesignedReturnedSetup(t *testing.T, report string) *returnedSetup {
	t.Helper()
	setup := newReturnedSetup(t, report)
	writeChainShape(t, setup.runDir, true)
	if err := os.WriteFile(setup.config.ConsumerConfigPath,
		[]byte(`{"max_stages":3,"agents":{"applier":{"command":"launch"}},"models":{"implementer":{"id":"implementer"},`+
			`"reviewers":[{"id":"review-a"},{"id":"review-b"}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// The report is the applier's on a designed delivery, under the name
	// that card writes.
	if err := os.Remove(filepath.Join(setup.runDir, "history", "stage-1", "implementer-run.json")); err != nil {
		t.Fatal(err)
	}
	sealed, err := worker.SealAgentRun(worker.AgentRun{
		SchemaVersion: worker.ArtifactSchemaVersion, Stage: 1, AgentID: "applier",
		Transcript: report, RanAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.WriteJSONFileExclusive(
		filepath.Join(setup.runDir, "history", "stage-1", "applier-run.json"), sealed, worker.MaxArtifactJSONBytes); err != nil {
		t.Fatal(err)
	}
	// The approved design the round is applying.
	designDir := filepath.Join(setup.runDir, "history", "design-1")
	if err := os.MkdirAll(designDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"investigation.json": `{"measurements":[]}`,
		"decision.json":      `{"outcome":"approved"}`,
		"design.json":        `{"files":[]}`,
		"DESIGN.md":          "# 設計\n\nラベルを差し替える。\n",
	} {
		if err := os.WriteFile(filepath.Join(designDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	design := func(id, stage string) runtime.BoardTask {
		return runtime.BoardTask{ID: id, Status: "done", IdempotencyKey: runtime.ChainCardKey(setup.fixture.deliveryID, stage, 1)}
	}
	card := func(id, stage, status string) runtime.BoardTask {
		return runtime.BoardTask{ID: id, Status: status, IdempotencyKey: runtime.ChainCardKey(setup.fixture.deliveryID, stage, 1)}
	}
	setup.view = chainViewFor([]runtime.BoardTask{
		design("t_inv", runtime.StageInvestigate),
		design("t_dra", runtime.StageDesignReviewA),
		design("t_drb", runtime.StageDesignReviewB),
		design("t_dd", runtime.StageDesignDecide),
		card("t_apply", runtime.StageApply, "blocked"),
		card("t_ra", runtime.StageReviewA, "todo"),
		card("t_rb", runtime.StageReviewB, "todo"),
		card("t_v", runtime.StageValidate, "todo"),
		card("t_p", runtime.StagePublish, "todo"),
	}, setup.fixture.deliveryID)
	return setup
}

// While the ladder works on a card, that card stays blocked, and the tick
// looks at a blocked card every few seconds. Each of those ticks reads the
// same records and finds the same return, because no agent has run. A probe
// measured what that cost: two hundred ticks over two real launches, the
// round's history rewritten every ten seconds and the attempt count reading
// two hundred. The round's history is written for what an agent did.
func TestAWaitingLadderDoesNotRecordTheSameReturnEveryTick(t *testing.T) {
	const report = "この依頼は、いまのままでは実現できません。"
	setup := newReturnedSetup(t, report)
	setup.config.Chain.RetryBackoffBaseSeconds = 3600
	// One answered return, then the same report again, which the engine
	// declines and leaves to the ladder.
	if err := setup.tick(t); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	setup.returnAgain(t, report)
	if err := setup.tick(t); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	returns := filepath.Join(setup.runDir, "history", "stage-1", worker.ReturnRecordFileName)
	failure := filepath.Join(setup.runDir, "history", "stage-1", "implement-failure.json")
	settled, err := os.ReadFile(returns)
	if err != nil {
		t.Fatalf("the round's record: %v", err)
	}
	sealed, err := os.ReadFile(failure)
	if err != nil {
		t.Fatalf("the round's failure was not sealed: %v", err)
	}

	// And now the ticks the ladder's wait is made of, with no agent running
	// between them.
	for tick := 0; tick < 200; tick++ {
		if err := setup.tick(t); err != nil {
			t.Fatalf("waiting tick %d: %v", tick, err)
		}
	}
	after, err := os.ReadFile(returns)
	if err != nil {
		t.Fatalf("the round's record: %v", err)
	}
	if string(after) != string(settled) {
		t.Fatalf("two hundred waiting ticks rewrote the round's history:\n%s", after)
	}
	if resealed, err := os.ReadFile(failure); err != nil || string(resealed) != string(sealed) {
		t.Fatalf("two hundred waiting ticks sealed the failure again: %v", err)
	}
	record, err := runner.ReadReturns(setup.runDir, 1)
	if err != nil || record == nil {
		t.Fatalf("the round's record = %+v %v", record, err)
	}
	if len(record.Returns) != 2 {
		t.Fatalf("returns recorded = %d, want the two the agent made", len(record.Returns))
	}
	if latest := record.Latest(); latest == nil || latest.Attempt != 2 {
		t.Fatalf("the newest return = %+v, want the second launch rather than the last tick", record.Latest())
	}
	if len(setup.fixture.comments.posted) != 0 || len(setup.fixture.store.digests) != 0 {
		t.Fatalf("the delivery ended: %q %v", setup.fixture.comments.posted, setup.fixture.store.digests)
	}

	// And a launch the ladder itself made is counted, word for word the
	// same report or not. What the skip above recognises is the tick that
	// saw no agent run, not a round that has stopped being written down.
	setup.returnAgain(t, report)
	if err := setup.tick(t); err != nil {
		t.Fatalf("the tick after the ladder ran the card again: %v", err)
	}
	record, err = runner.ReadReturns(setup.runDir, 1)
	if err != nil || record == nil || len(record.Returns) != 3 {
		t.Fatalf("a launch the ladder made was not counted: %+v %v", record, err)
	}
	if latest := record.Latest(); latest == nil || latest.Attempt != 3 || latest.Answered {
		t.Fatalf("the newest return = %+v, want the third launch and no answer", record.Latest())
	}
}

// The bound on the answering holds however the report is worded. A model
// that finds a new reason every time is still a model that will not do the
// work, and the third answer is where the engine stops paying to say the
// same three rules again.
func TestAFourthReturnGoesToTheLadderHoweverItIsWorded(t *testing.T) {
	setup := newReturnedSetup(t, "理由 1 を見つけました。")
	setup.config.Chain.RetryBackoffBaseSeconds = 3600
	for tick := 1; tick <= 8; tick++ {
		if err := setup.tick(t); err != nil {
			t.Fatalf("tick %d: %v", tick, err)
		}
		setup.returnAgain(t, fmt.Sprintf("理由 %d を見つけました。", tick+1))
	}
	// Three answers, and no fourth: the engine's answer is the same every
	// time and a model that has heard it three times is not being
	// persuaded.
	record, err := runner.ReadReturns(setup.runDir, 1)
	if err != nil || record == nil {
		t.Fatalf("the round's record = %+v %v", record, err)
	}
	if answers := len(record.Assumptions()); answers != 3 {
		t.Fatalf("answers the engine made = %d, want it to stop at three", answers)
	}
	// Counted in launches, which is what the bound is really about: three
	// rounds started again by the engine, then one by the ladder's single
	// hand on an implementing seat, and then the wait.
	stages := len(runtime.ChainStagesFor(setup.config.Chain, runtime.ChainPlan{Shape: runtime.ShapeImplement}))
	if built := setup.boardCreations(t); built != 4*stages {
		t.Fatalf("cards built across eight ticks = %d, want the four launches of %d", built, 4*stages)
	}
	if _, err := os.Stat(filepath.Join(setup.runDir, "retry", "implement-r1.json")); err != nil {
		t.Fatalf("the fourth return never reached the ladder: %v", err)
	}
	if len(setup.fixture.comments.posted) != 0 || len(setup.fixture.store.digests) != 0 {
		t.Fatalf("the delivery ended: %q %v", setup.fixture.comments.posted, setup.fixture.store.digests)
	}
}
