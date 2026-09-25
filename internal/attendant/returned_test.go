package attendant

import (
	"context"
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
	logger   *recordingLogger
}

// stopTracker answers with whatever comments the ticket is standing at.
func stopTracker(t *testing.T, body string) *backlog.Client {
	t.Helper()
	client, err := backlog.NewClient(backlog.Config{
		SpaceKey: "example", APIKey: "k", Origin: "https://example.backlog.com",
		Timeout: time.Second, MaxResponseBytes: 1 << 20,
	}, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
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

	config := fixture.config
	config.Chain.Profiles = designTestProfiles()
	config.Tracker.AllowedCreatorID = 7
	if err := os.WriteFile(config.ConsumerConfigPath, []byte(plainConsumer), 0o600); err != nil {
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
	fixture.services.Backlog = quietTracker(t)

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
		runDir: runDir, hermes: hermes, board: board, logger: &recordingLogger{}}
}

func (s *returnedSetup) tick(t *testing.T) error {
	t.Helper()
	run := state.RunOverview{DeliveryID: s.fixture.deliveryID, RunID: "TKT-4242", IssueID: 4242, IssueKey: "TKT-4242"}
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

// A round that comes back saying exactly the same thing is a round that is
// not moving. It is still answered and still started again — the delivery
// does not stop — but the repetition is left on disk where it can be seen
// and said in the instruction, rather than being a loop nobody counted.
func TestAReturnRepeatedIdenticallyLeavesTheMaterialToSeeIt(t *testing.T) {
	const report = "この依頼は、いまのままでは実現できません。"
	setup := newReturnedSetup(t, report)
	for tick := 1; tick <= 2; tick++ {
		if err := setup.tick(t); err != nil {
			t.Fatalf("tick %d: %v", tick, err)
		}
	}
	record, err := runner.ReadReturns(setup.runDir, 1)
	if err != nil || record == nil {
		t.Fatalf("nothing was recorded beside the round: %+v %v", record, err)
	}
	if len(record.Returns) != 2 {
		t.Fatalf("the round's record holds %d returns, want both", len(record.Returns))
	}
	first, second := record.Returns[0], record.Returns[1]
	if first.ReportSHA256 != second.ReportSHA256 {
		t.Fatalf("the same report left two digests: %q / %q", first.ReportSHA256, second.ReportSHA256)
	}
	if first.Repeated {
		t.Fatal("the first return was recorded as a repeat")
	}
	if !second.Repeated {
		t.Fatal("the round came back saying the same thing and nothing recorded it")
	}
	if !strings.Contains(second.Instruction, "同じ報告をもう一度返しても") {
		t.Fatalf("the repeat was not said to the round:\n%s", second.Instruction)
	}
	if len(setup.fixture.comments.posted) != 0 || len(setup.fixture.store.digests) != 0 {
		t.Fatalf("the delivery stopped over a repeat: %q %v",
			setup.fixture.comments.posted, setup.fixture.store.digests)
	}
}

// The requester's stop is one of the two endings a delivery is allowed to
// have, and a round that starts itself again is exactly where it has to be
// heard. What the engine had decided by then is still written down.
func TestAStopIsHonouredBeforeAReturnedRoundStartsAgain(t *testing.T) {
	setup := newReturnedSetup(t, "この依頼は、いまのままでは実現できません。")
	setup.fixture.services.Backlog = stopTracker(t,
		`[{"id":1,"issueId":4242,"content":"停止","created":"2026-09-25T00:00:00Z","createdUser":{"id":7}}]`)
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
