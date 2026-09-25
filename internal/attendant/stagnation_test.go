package attendant

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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

var stagnationReviewers = []string{"review-a", "review-b"}

// seedRound writes one round the way the cards seal it: the change, and each
// seat's verdict. findings is the objections the second seat raised; the
// first seat passes.
func seedRound(t *testing.T, runDir string, round int, content string, findings []worker.ModelFinding) {
	t.Helper()
	stageDir := filepath.Join(runDir, "history", fmt.Sprintf("stage-%d", round))
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	candidate := worker.Candidate{
		SchemaVersion: 1, Stage: round,
		Files: []worker.CandidateFile{{Path: "README.md", Content: content}},
	}
	writeJSON(t, filepath.Join(stageDir, "candidate.json"), candidate)
	verdict := "revise"
	if len(findings) == 0 {
		verdict = "pass"
	}
	writeJSON(t, filepath.Join(stageDir, "review-a.json"), worker.Review{
		SchemaVersion: 1, Stage: round, ReviewerID: "review-a",
		Verdict: "pass", Findings: []worker.ModelFinding{},
	})
	writeJSON(t, filepath.Join(stageDir, "review-b.json"), worker.Review{
		SchemaVersion: 1, Stage: round, ReviewerID: "review-b",
		Verdict: verdict, Findings: findings,
	})
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

func finding(code string) worker.ModelFinding {
	return worker.ModelFinding{Code: code, Path: "README.md", Message: "そこが足りません。"}
}

// A round that answered two of three objections is a delivery converging.
// Interrupting it to rule on a deadlock would take the decision away from
// the seats exactly when they were getting somewhere.
func TestARoundThatFixedSomeObjectionsIsNotStagnation(t *testing.T) {
	runDir := t.TempDir()
	seedRound(t, runDir, 1, "one\n", []worker.ModelFinding{finding("a"), finding("b"), finding("c")})
	seedRound(t, runDir, 2, "two\n", []worker.ModelFinding{finding("a")})
	if stagnated(runDir, stagnationReviewers, 2, 1) {
		t.Error("a round that answered two of three objections was called a deadlock")
	}
}

// The objections did not move: the same seat raised the same codes about the
// same paths, so asking again produces the same round.
func TestIdenticalObjectionsAreStagnation(t *testing.T) {
	runDir := t.TempDir()
	seedRound(t, runDir, 1, "one\n", []worker.ModelFinding{finding("a"), finding("b")})
	seedRound(t, runDir, 2, "two\n", []worker.ModelFinding{finding("b"), finding("a")})
	if !stagnated(runDir, stagnationReviewers, 2, 1) {
		t.Error("two rounds carrying exactly the same objections were not called a deadlock")
	}
	// The message is not part of the identity: the same complaint worded
	// differently is the same complaint.
	reworded := []worker.ModelFinding{
		{Code: "a", Path: "README.md", Message: "別の言い方をしました。"},
		{Code: "b", Path: "README.md", Message: "これも書き換えました。"},
	}
	seedRound(t, runDir, 2, "two\n", reworded)
	if !stagnated(runDir, stagnationReviewers, 2, 1) {
		t.Error("the same objections in different words were not recognised")
	}
	// A code nobody raised before is movement, whatever else repeats.
	seedRound(t, runDir, 2, "two\n", []worker.ModelFinding{finding("a"), finding("d")})
	if stagnated(runDir, stagnationReviewers, 2, 1) {
		t.Error("a round raising a new objection was called a deadlock")
	}
}

// The change did not move: the round wrote the same bytes to the same paths,
// whatever anyone said about it.
func TestAnIdenticalChangeIsStagnation(t *testing.T) {
	runDir := t.TempDir()
	seedRound(t, runDir, 1, "same\n", []worker.ModelFinding{finding("a")})
	seedRound(t, runDir, 2, "same\n", []worker.ModelFinding{finding("b")})
	if !stagnated(runDir, stagnationReviewers, 2, 1) {
		t.Error("a round that changed not one byte was not called a deadlock")
	}
	seedRound(t, runDir, 2, "different\n", []worker.ModelFinding{finding("b")})
	if stagnated(runDir, stagnationReviewers, 2, 1) {
		t.Error("a round that wrote something else was called a deadlock")
	}
}

// A round the seats passed and the destination's own commands refused has no
// standing objections to compare. There, stagnation is the same change
// meeting the same refusal — and the same change meeting a different refusal
// is the delivery getting somewhere.
func TestARefusedValidationRepeatingItselfIsStagnation(t *testing.T) {
	runDir := t.TempDir()
	pipeline := &runner.Pipeline{Workspace: runDir, Logger: &recordingLogger{}}
	seedRound(t, runDir, 1, "same\n", nil)
	seedRound(t, runDir, 2, "same\n", nil)
	pipeline.SealValidationFailure(1, "run-validation", "--- FAIL: TestLabel\n")
	pipeline.SealValidationFailure(2, "run-validation", "--- FAIL: TestLabel\n")
	if !stagnated(runDir, stagnationReviewers, 2, 1) {
		t.Error("the same change refused by the same output twice was not called a deadlock")
	}
	if err := os.Remove(runner.ValidationFailureFile(runDir, 2)); err != nil {
		t.Fatal(err)
	}
	pipeline.SealValidationFailure(2, "run-validation", "--- FAIL: SomethingElse\n")
	if stagnated(runDir, stagnationReviewers, 2, 1) {
		t.Error("the same change refused for a different reason was called a deadlock")
	}
}

// A round that cannot be read says nothing about whether the delivery is
// moving, so it is not a deadlock. The delivery goes on, which an unreadable
// record was always least able to justify stopping.
func TestAnUnreadableRoundIsNotStagnation(t *testing.T) {
	runDir := t.TempDir()
	seedRound(t, runDir, 1, "same\n", []worker.ModelFinding{finding("a")})
	seedRound(t, runDir, 2, "same\n", []worker.ModelFinding{finding("a")})
	if !stagnated(runDir, stagnationReviewers, 2, 1) {
		t.Fatal("the fixture is not a deadlock to begin with")
	}
	for _, missing := range []string{"history/stage-1/review-b.json", "history/stage-2/candidate.json"} {
		path := filepath.Join(runDir, missing)
		saved, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if stagnated(runDir, stagnationReviewers, 2, 1) {
			t.Errorf("a round missing %s was called a deadlock", missing)
		}
		if err := os.WriteFile(path, []byte("{ this is not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		if stagnated(runDir, stagnationReviewers, 2, 1) {
			t.Errorf("a round whose %s will not read was called a deadlock", missing)
		}
		if err := os.WriteFile(path, saved, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// And the first round has nothing before it to be compared against.
	if stagnated(runDir, stagnationReviewers, 1, 1) {
		t.Error("the first round was called a deadlock")
	}
}

// A destination can ask for more than one repeat before the engine rules.
func TestStagnationRepeatRoundsIsHonoured(t *testing.T) {
	runDir := t.TempDir()
	seedRound(t, runDir, 1, "one\n", []worker.ModelFinding{finding("a")})
	seedRound(t, runDir, 2, "two\n", []worker.ModelFinding{finding("a")})
	if !stagnated(runDir, stagnationReviewers, 2, 1) {
		t.Fatal("one repeat was not enough with a setting of one")
	}
	if stagnated(runDir, stagnationReviewers, 2, 2) {
		t.Error("two repeats were claimed from one")
	}
	seedRound(t, runDir, 3, "three\n", []worker.ModelFinding{finding("a")})
	if !stagnated(runDir, stagnationReviewers, 3, 2) {
		t.Error("three rounds carrying the same objection were not two repeats")
	}
}

// stagnantFixture is a delivery whose round 2 objected to exactly what round
// 1 objected to, on the tick that finds the blocked validate card.
func stagnantFixture(t *testing.T, consumerConfig string) (pendingFixture, runtime.Config, hook.DispatchEnvelope, chainView, string, string) {
	t.Helper()
	fixture := newPendingFixture(t, "")
	runDir := runDirectory(fixture.config, fixture.deliveryID)
	seedRound(t, runDir, 1, "one\n", []worker.ModelFinding{finding("missed-escalation")})
	seedRound(t, runDir, 2, "two\n", []worker.ModelFinding{finding("missed-escalation")})
	if err := os.WriteFile(filepath.Join(runDir, "history", "stage-2", "decision.json"),
		[]byte(`{"outcome":"revise"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "ticket-draft.json"),
		[]byte(`{"repository":"example/consumer"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(runDir, "history", "readiness"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "history", "readiness", "decision.json"),
		[]byte(`{"request_kind":"change","needs_design":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	config := fixture.config
	config.Chain.Profiles = designTestProfiles()
	// Whose 「停止」 counts. Nobody else's does, so without this the stop
	// reads below could never match a comment.
	config.Tracker.AllowedCreatorID = 7
	// A stand-in worker that records what it was asked and writes whatever
	// --out it was given. The ruling it writes is the one this delivery is
	// being tested under.
	config.WorkerBin = filepath.Join(t.TempDir(), "worker")
	ruling := filepath.Join(runDir, "history", "stage-2", "ruling.json")
	if err := os.WriteFile(config.WorkerBin, []byte(`#!/bin/sh
printf '%s\n' "$*" >> `+config.WorkerBin+`.log
out=""
prev=""
for a in "$@"; do
  [ "$prev" = "--out" ] && out="$a"
  prev="$a"
done
case "$1" in
  arbitrate) printf '%s' '`+rulingBody+`' > "$out" ;;
  *) [ -n "$out" ] && printf 'RECORD\n' > "$out" ;;
esac
exit 0
`), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.ConsumerConfigPath, []byte(consumerConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	quiet, err := backlog.NewClient(backlog.Config{
		SpaceKey: "example", APIKey: "k", Origin: "https://example.backlog.com",
		Timeout: time.Second, MaxResponseBytes: 1 << 20,
	}, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("[]")), Header: http.Header{}}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	fixture.services.Backlog = quiet
	var envelope hook.DispatchEnvelope
	if err := json.Unmarshal([]byte(fixture.run.EnvelopeJSON), &envelope); err != nil {
		t.Fatal(err)
	}
	card := func(id, stage, status string) runtime.BoardTask {
		return runtime.BoardTask{ID: id, Status: status, IdempotencyKey: runtime.ChainCardKey(fixture.deliveryID, stage, 2)}
	}
	view := chainViewFor([]runtime.BoardTask{
		card("t_impl", runtime.StageImplement, "done"),
		card("t_ra", runtime.StageReviewA, "done"),
		card("t_rb", runtime.StageReviewB, "done"),
		card("t_v", runtime.StageValidate, "blocked"),
		card("t_p", runtime.StagePublish, "todo"),
	}, fixture.deliveryID)
	return fixture, config, envelope, view, runDir, ruling
}

// rulingBody is what the stand-in arbiter writes: a ruling that tells the
// implementer what to satisfy. The attendant reads only the ruling and the
// instruction, which is what makes a hand-written record usable here.
const rulingBody = `{"schema_version":1,"prompt_version":1,"stage":2,"ruling":"instruct_implementer",` +
	`"instruction":"抑制を押した後、記録が空のときでも悪化した通知が届くこと。",` +
	`"assumption":{"kind":"arbiter_ruling","statement":"検収条件は悪化の通知を含む。","evidence":"チケット本文。"},` +
	`"ruling_sha256":"` + "aaaaaaaa" + `"}`

const plainConsumer = `{"max_stages":3,"models":{"reviewers":[{"id":"review-a"},{"id":"review-b"}]}}`

// The delivery that used to be handed back to its requester as a question:
// two rounds, the same objection, nobody yielding. The engine rules on it,
// the ruling is sealed beside the round, and the next round is told what to
// satisfy. Nothing is posted to the ticket.
func TestADeadlockedDeliveryIsRuledOnRatherThanAsked(t *testing.T) {
	fixture, config, envelope, view, _, rulingPath := stagnantFixture(t, plainConsumer)
	hermes, boardLog := fakeBoard(t)
	logger := &recordingLogger{}
	run := state.RunOverview{DeliveryID: fixture.deliveryID, RunID: "TKT-4242", IssueID: 4242, IssueKey: "TKT-4242"}
	if err := handleChainFailure(context.Background(), config, fixture.services, hermes, envelope, run, view,
		runtime.StageValidate, logger); err != nil {
		t.Fatalf("the failure was not handled: %v", err)
	}
	if len(fixture.comments.posted) != 0 {
		t.Fatalf("the requester was asked or told something: %q", fixture.comments.posted)
	}
	if len(fixture.store.digests) != 0 {
		t.Fatalf("the delivery ended: %v", fixture.store.digests)
	}
	if _, err := os.Stat(rulingPath); err != nil {
		t.Fatalf("no ruling was sealed beside the round: %v", err)
	}
	calls, err := os.ReadFile(config.WorkerBin + ".log")
	if err != nil {
		t.Fatalf("no verb was run: %v", err)
	}
	if !strings.Contains(string(calls), "arbitrate ") {
		t.Fatalf("the arbiter was not asked:\n%s", calls)
	}
	// And the round that was started carries the ruling.
	if !strings.Contains(string(calls), "--ruling "+rulingPath) {
		t.Fatalf("the next round was rendered without the ruling:\n%s", calls)
	}
	board, err := os.ReadFile(boardLog)
	if err != nil {
		t.Fatalf("the board was not called: %v", err)
	}
	if !strings.Contains(string(board), "|create|") {
		t.Fatalf("no card was created for the next round:\n%s", board)
	}
	said := strings.Join(logger.lines, "\n")
	if !strings.Contains(said, "the rounds have stopped moving") {
		t.Errorf("nothing said the delivery had stopped moving:\n%s", said)
	}
}

// A ruling that sets the objections aside puts the same round back to work:
// the decision counted without the ruling is dropped, and the validate card
// is dispatched again to count it under the ruling.
func TestAnOverrulingSendsTheSameRoundBackToBeDecided(t *testing.T) {
	fixture, config, envelope, view, runDir, rulingPath := stagnantFixture(t, plainConsumer)
	overruling := strings.Replace(rulingBody, `"ruling":"instruct_implementer"`, `"ruling":"overrule_reviewer"`, 1)
	overruling = strings.Replace(overruling, `"instruction":"抑制を押した後、記録が空のときでも悪化した通知が届くこと。"`,
		`"overruled":[{"reviewer_id":"review-b","code":"missed-escalation","path":"README.md","reason":"依頼の範囲外です。"}]`, 1)
	script, err := os.ReadFile(config.WorkerBin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.WorkerBin,
		[]byte(strings.Replace(string(script), rulingBody, overruling, 1)), 0o700); err != nil {
		t.Fatal(err)
	}
	hermes, boardLog := fakeBoard(t)
	run := state.RunOverview{DeliveryID: fixture.deliveryID, RunID: "TKT-4242", IssueID: 4242, IssueKey: "TKT-4242"}
	if err := handleChainFailure(context.Background(), config, fixture.services, hermes, envelope, run, view,
		runtime.StageValidate, &recordingLogger{}); err != nil {
		t.Fatalf("the failure was not handled: %v", err)
	}
	if _, err := os.Stat(rulingPath); err != nil {
		t.Fatalf("no ruling was sealed: %v", err)
	}
	// The decision counted without the ruling is gone, so the card that runs
	// next decides the round again instead of reading the old answer back.
	if _, err := os.Stat(filepath.Join(runDir, "history", "stage-2", "decision.json")); err == nil {
		t.Error("the decision counted without the ruling is still there")
	}
	board, err := os.ReadFile(boardLog)
	if err != nil {
		t.Fatalf("the board was not called: %v", err)
	}
	// The same round, not the next one: the validate card is rebuilt at
	// round 2.
	if !strings.Contains(string(board), fixture.deliveryID+":validate:r2") {
		t.Fatalf("the round was not sent back to be decided:\n%s", board)
	}
	if strings.Contains(string(board), fixture.deliveryID+":implement:r3") {
		t.Fatalf("an overruling started another round instead of deciding this one:\n%s", board)
	}
}

// An operator who writes down a round limit gets a delivery that stops at
// it, and the deadlock is still ruled on before that number is reached.
func TestAConfiguredRoundLimitStopsTheDeliveryAndStagnationFiresFirst(t *testing.T) {
	fixture, config, envelope, view, runDir, rulingPath := stagnantFixture(t,
		`{"max_stages":3,"max_rounds":2,"models":{"reviewers":[{"id":"review-a"},{"id":"review-b"}]}}`)
	hermes, _ := fakeBoard(t)
	run := state.RunOverview{DeliveryID: fixture.deliveryID, RunID: "TKT-4242", IssueID: 4242, IssueKey: "TKT-4242"}
	// The ending an operator's own limit produces: the seats answered and
	// did not agree within the rounds that operator paid for.
	terminal := runner.NewTerminal(config, fixture.services, envelope, chainOwnerRunID(fixture.deliveryID), runDir, &recordingLogger{})
	digest, err := terminal.ReportDigest(context.Background(), hook.TerminalNonconverged,
		runner.Outcome{Code: hook.TerminalNonconverged}, "")
	if err != nil {
		t.Fatal(err)
	}
	fixture.store.expected = digest
	if err := handleChainFailure(context.Background(), config, fixture.services, hermes, envelope, run, view,
		runtime.StageValidate, &recordingLogger{}); err != nil {
		t.Fatalf("the failure was not handled: %v", err)
	}
	// Round 2 is the limit, so the delivery ends here, and the deadlock was
	// never ruled on because the limit was reached first.
	if len(fixture.store.digests) != 1 {
		t.Fatalf("terminal reports begun = %d, want 1", len(fixture.store.digests))
	}
	if _, err := os.Stat(rulingPath); err == nil {
		t.Error("the limit was reached and the engine ruled anyway")
	}
	// One round earlier the same deadlock is ruled on instead.
	fixture2, config2, envelope2, view2, _, rulingPath2 := stagnantFixture(t,
		`{"max_stages":3,"max_rounds":5,"models":{"reviewers":[{"id":"review-a"},{"id":"review-b"}]}}`)
	hermes2, _ := fakeBoard(t)
	if err := handleChainFailure(context.Background(), config2, fixture2.services, hermes2, envelope2, run, view2,
		runtime.StageValidate, &recordingLogger{}); err != nil {
		t.Fatalf("the failure was not handled: %v", err)
	}
	if len(fixture2.store.digests) != 0 {
		t.Fatalf("the delivery ended below its limit: %v", fixture2.store.digests)
	}
	if _, err := os.Stat(rulingPath2); err != nil {
		t.Fatalf("the deadlock below the limit was not ruled on: %v", err)
	}
}

// The record ceiling ends a delivery instead of rendering a round nothing
// can seal.
//
// Without it the regenerate arm kept creating rounds: round 51's
// instruction would be rendered, the implementer would run and be paid for,
// and only then would the seal refuse a round number no record may carry —
// a failure the ladder reads as the model's and dispatches again, for ever.
func TestTheRecordCeilingEndsTheDeliveryRatherThanRenderingAnotherRound(t *testing.T) {
	fixture, config, envelope, _, runDir, _ := stagnantFixture(t, plainConsumer)
	card := func(id, stage, status string) runtime.BoardTask {
		return runtime.BoardTask{ID: id, Status: status, IdempotencyKey: runtime.ChainCardKey(fixture.deliveryID, stage, worker.StageCeiling)}
	}
	view := chainViewFor([]runtime.BoardTask{
		card("t_impl", runtime.StageImplement, "done"),
		card("t_v", runtime.StageValidate, "blocked"),
	}, fixture.deliveryID)
	if err := os.MkdirAll(filepath.Join(runDir, "history", fmt.Sprintf("stage-%d", worker.StageCeiling)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "history", fmt.Sprintf("stage-%d", worker.StageCeiling), "decision.json"),
		[]byte(`{"outcome":"revise"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	terminal := runner.NewTerminal(config, fixture.services, envelope, chainOwnerRunID(fixture.deliveryID), runDir, &recordingLogger{})
	digest, err := terminal.ReportDigest(context.Background(), hook.TerminalNonconverged,
		runner.Outcome{Code: hook.TerminalNonconverged}, "")
	if err != nil {
		t.Fatal(err)
	}
	fixture.store.expected = digest

	hermes, boardLog := fakeBoard(t)
	logger := &recordingLogger{}
	run := state.RunOverview{DeliveryID: fixture.deliveryID, RunID: "TKT-4242", IssueID: 4242, IssueKey: "TKT-4242"}
	if err := handleChainFailure(context.Background(), config, fixture.services, hermes, envelope, run, view,
		runtime.StageValidate, logger); err != nil {
		t.Fatalf("the failure was not handled: %v", err)
	}
	if len(fixture.store.digests) != 1 {
		t.Fatalf("terminal reports begun = %d, want 1", len(fixture.store.digests))
	}
	board, err := os.ReadFile(boardLog)
	if err == nil && strings.Contains(string(board), fmt.Sprintf(":implement:r%d", worker.StageCeiling+1)) {
		t.Fatalf("a round past the ceiling was created:\n%s", board)
	}
	if !strings.Contains(strings.Join(logger.lines, "\n"), "the highest round any record can carry") {
		t.Errorf("nothing said why the delivery ended:\n%s", strings.Join(logger.lines, "\n"))
	}
}

// A requester who writes 「停止」 while the engine is ruling on a deadlock is
// answered before the next round starts spending. The stop read at the top
// of the tick was minutes and one model call ago.
func TestAStopDuringArbitrationStopsTheNextRound(t *testing.T) {
	fixture, config, envelope, view, _, _ := stagnantFixture(t, plainConsumer)
	// A tracker that says nothing the first time it is asked and carries
	// the stop the second: the first read is the tick's own, the second is
	// the one taken after the ruling.
	reads := 0
	talkative, err := backlog.NewClient(backlog.Config{
		SpaceKey: "example", APIKey: "k", Origin: "https://example.backlog.com",
		Timeout: time.Second, MaxResponseBytes: 1 << 20,
	}, roundTripFunc(func(*http.Request) (*http.Response, error) {
		reads++
		body := "[]"
		if reads > 1 {
			body = `[{"id":1,"issueId":4242,"content":"停止","created":"2026-09-25T00:00:00Z","createdUser":{"id":7}}]`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	fixture.services.Backlog = talkative
	runDir := runDirectory(config, fixture.deliveryID)
	terminal := runner.NewTerminal(config, fixture.services, envelope, chainOwnerRunID(fixture.deliveryID), runDir, &recordingLogger{})
	digest, err := terminal.ReportDigest(context.Background(), hook.TerminalCancelled,
		runner.Outcome{Code: hook.TerminalCancelled}, "")
	if err != nil {
		t.Fatal(err)
	}
	fixture.store.expected = digest

	hermes, boardLog := fakeBoard(t)
	run := state.RunOverview{DeliveryID: fixture.deliveryID, RunID: "TKT-4242", IssueID: 4242, IssueKey: "TKT-4242"}
	if err := handleChainFailure(context.Background(), config, fixture.services, hermes, envelope, run, view,
		runtime.StageValidate, &recordingLogger{}); err != nil {
		t.Fatalf("the failure was not handled: %v", err)
	}
	if reads < 2 {
		t.Fatalf("the stop was read %d time(s); the second read is the one this is about", reads)
	}
	if len(fixture.store.digests) != 1 {
		t.Fatalf("terminal reports begun = %d, want the cancelled one", len(fixture.store.digests))
	}
	board, err := os.ReadFile(boardLog)
	if err == nil && strings.Contains(string(board), fixture.deliveryID+":implement:r3") {
		t.Fatalf("the next round started after the requester asked to stop:\n%s", board)
	}
}

// The deadlock with no objections in it, end to end: both seats passed the
// change and the destination's own commands refused it, twice, printing the
// same thing. The engine rules, and the next round carries the instruction.
func TestARoundRefusedTwiceByTheSameOutputIsRuledOn(t *testing.T) {
	fixture, config, envelope, view, runDir, rulingPath := stagnantFixture(t, plainConsumer)
	// Rewrite the fixture as two rounds the seats passed, each refused by
	// the same output, and decided converged.
	seedRound(t, runDir, 1, "same\n", nil)
	seedRound(t, runDir, 2, "same\n", nil)
	if err := os.WriteFile(filepath.Join(runDir, "history", "stage-2", "decision.json"),
		[]byte(`{"outcome":"converged"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	pipeline := &runner.Pipeline{Workspace: runDir, Logger: &recordingLogger{}}
	pipeline.SealValidationFailure(1, "run-validation", "--- FAIL: TestLabel (0.00s)\n")
	pipeline.SealValidationFailure(2, "run-validation", "--- FAIL: TestLabel (0.00s)\n")

	hermes, boardLog := fakeBoard(t)
	run := state.RunOverview{DeliveryID: fixture.deliveryID, RunID: "TKT-4242", IssueID: 4242, IssueKey: "TKT-4242"}
	if err := handleChainFailure(context.Background(), config, fixture.services, hermes, envelope, run, view,
		runtime.StageValidate, &recordingLogger{}); err != nil {
		t.Fatalf("the failure was not handled: %v", err)
	}
	if len(fixture.store.digests) != 0 || len(fixture.comments.posted) != 0 {
		t.Fatalf("the delivery ended: %v %q", fixture.store.digests, fixture.comments.posted)
	}
	if _, err := os.Stat(rulingPath); err != nil {
		t.Fatalf("the repeated refusal was not ruled on: %v", err)
	}
	calls, err := os.ReadFile(config.WorkerBin + ".log")
	if err != nil {
		t.Fatalf("no verb was run: %v", err)
	}
	// The arbiter was given what the commands printed, and the next round
	// carries what it ruled.
	//
	// Read off the arbiter's own line. The next round's instruction carries
	// the same refusal by its own rule, so looking for the flag anywhere in
	// the log would pass with the arbiter shown nothing.
	arbitrated := ""
	for _, line := range strings.Split(string(calls), "\n") {
		if strings.HasPrefix(line, "arbitrate ") {
			arbitrated = line
		}
	}
	if arbitrated == "" {
		t.Fatalf("the arbiter was not run at all:\n%s", calls)
	}
	if !strings.Contains(arbitrated, "--validation-failure "+runner.ValidationFailureFile(runDir, 2)) {
		t.Fatalf("the arbiter was not shown the refusal: %q", arbitrated)
	}
	if !strings.Contains(string(calls), "--ruling "+rulingPath) {
		t.Fatalf("the next round was rendered without the ruling:\n%s", calls)
	}
	board, err := os.ReadFile(boardLog)
	if err != nil {
		t.Fatalf("the board was not called: %v", err)
	}
	if !strings.Contains(string(board), "|create|") {
		t.Fatalf("no card was created for the next round:\n%s", board)
	}
}
