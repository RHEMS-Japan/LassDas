package attendant

import (
	"context"
	"encoding/json"
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
)

// A converged round whose deterministic validation refused it, on the tick
// that finds the blocked validate card. This is the state a live delivery
// reaches when the judges pass a change the destination's own commands then
// reject.
func refusedValidationFixture(t *testing.T) (pendingFixture, runtime.Config, hook.DispatchEnvelope, chainView, string) {
	t.Helper()
	fixture := newPendingFixture(t, "")
	runDir := runDirectory(fixture.config, fixture.deliveryID)
	if err := os.MkdirAll(filepath.Join(runDir, "history", "stage-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "history", "stage-1", "decision.json"),
		[]byte(`{"outcome":"converged"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	pipeline := &runner.Pipeline{Workspace: runDir, Logger: &recordingLogger{}}
	pipeline.SealValidationFailure(1, "run-validation", "--- FAIL: TestLabel (0.00s)\n")

	config := fixture.config
	config.Chain.Profiles = designTestProfiles()
	// A stand-in worker that records what it was asked to do and writes
	// whatever --out it was given, so the trail the failure report attaches
	// is composed the way a real one is.
	config.WorkerBin = filepath.Join(t.TempDir(), "worker")
	if err := os.WriteFile(config.WorkerBin, []byte(`#!/bin/sh
printf '%s\n' "$*" >> `+config.WorkerBin+`.log
out=""
prev=""
for a in "$@"; do
  [ "$prev" = "--out" ] && out="$a"
  prev="$a"
done
[ -n "$out" ] && printf 'RECORD\n' > "$out"
exit 0
`), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.ConsumerConfigPath,
		[]byte(`{"max_stages":3,"models":{"reviewers":[{"id":"review-a"},{"id":"review-b"}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// A tracker that answers "no comments", so the stop check passes through.
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
		return runtime.BoardTask{ID: id, Status: status, IdempotencyKey: runtime.ChainCardKey(fixture.deliveryID, stage, 1)}
	}
	view := chainViewFor([]runtime.BoardTask{
		card("t_impl", runtime.StageImplement, "done"),
		card("t_ra", runtime.StageReviewA, "done"),
		card("t_rb", runtime.StageReviewB, "done"),
		card("t_v", runtime.StageValidate, "blocked"),
		card("t_p", runtime.StagePublish, "todo"),
	}, fixture.deliveryID)
	return fixture, config, envelope, view, runDir
}

// The ending this replaces: a refused validation used to report
// validation_failed and archive the delivery. Now the tick starts the next
// round, posts nothing to the requester, and the round it renders carries
// what the validation printed.
func TestARefusedValidationStartsTheNextRoundInsteadOfEndingTheRun(t *testing.T) {
	fixture, config, envelope, view, _ := refusedValidationFixture(t)
	hermes, boardCalls := fakeBoard(t)
	logger := &recordingLogger{}
	run := state.RunOverview{DeliveryID: fixture.deliveryID, RunID: "TKT-4242", IssueID: 4242, IssueKey: "TKT-4242"}
	if err := handleChainFailure(context.Background(), config, fixture.services, hermes, envelope, run, view,
		runtime.StageValidate, logger); err != nil {
		t.Fatalf("the failure was not handled: %v", err)
	}
	if len(fixture.comments.posted) != 0 {
		t.Fatalf("the requester was told the run ended: %q", fixture.comments.posted)
	}
	if fixture.store.begins != 0 {
		t.Fatalf("a terminal report was begun %d times", fixture.store.begins)
	}
	// The one line that says why this round advanced. The regenerate line
	// beside it says a round was created and nothing about a validation that
	// had already been found to fail.
	line := ""
	for _, recorded := range logger.lines {
		if strings.Contains(recorded, "the deterministic validation refused the round") {
			line = recorded
		}
	}
	if line == "" {
		t.Fatalf("no line said why the round advanced:\n%s", strings.Join(logger.lines, "\n"))
	}
	if !strings.Contains(line, "run-validation") {
		t.Fatalf("the line does not name the step that refused: %q", line)
	}
	// The digest is on the line, which is what makes a run repeating itself
	// readable without opening the run directory.
	failure, sealed := runner.ReadValidationFailure(runDirectory(config, fixture.deliveryID), 1)
	if !sealed || !strings.Contains(line, failure.OutputSHA256) {
		t.Fatalf("the line does not carry the output digest: %q", line)
	}
	// And the round that was started is rendered with the record.
	calls, err := os.ReadFile(config.WorkerBin + ".log")
	if err != nil {
		t.Fatalf("no instruction was rendered: %v", err)
	}
	if !strings.Contains(string(calls), "--validation-failure") {
		t.Fatalf("the next round was rendered without what the validation printed:\n%s", calls)
	}
	// A rendered instruction with no card to run it is a delivery that has
	// quietly stopped, which reads from the outside exactly like the ending
	// this replaces.
	board, err := os.ReadFile(boardCalls)
	if err != nil {
		t.Fatalf("the board was not called: %v", err)
	}
	if !strings.Contains(string(board), "|create|") {
		t.Fatalf("no card was created for the next round:\n%s", board)
	}
}

// An operator's own round limit is the one thing left that still ends a run
// from here, and it is the one place a refused validation still names
// itself. The requester reads that sentence: on this path the AI answered
// and both judges passed the change, and the repository's own commands are
// what refused, so saying the AI failed would be false.
//
// A destination declaring three stages no longer reaches this. The number
// that stops a delivery is max_rounds, which is unset by default and has to
// be written down by somebody who wants it.
func TestAConfiguredRoundLimitEndsARefusedValidationAsAValidationFailure(t *testing.T) {
	fixture, config, envelope, _, _ := refusedValidationFixture(t)
	if err := os.WriteFile(config.ConsumerConfigPath,
		[]byte(`{"max_stages":1,"max_rounds":1,"models":{"reviewers":[{"id":"review-a"},{"id":"review-b"}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	card := func(id, stage, status string) runtime.BoardTask {
		return runtime.BoardTask{ID: id, Status: status, IdempotencyKey: runtime.ChainCardKey(fixture.deliveryID, stage, 1)}
	}
	view := chainViewFor([]runtime.BoardTask{
		card("t_impl", runtime.StageImplement, "done"),
		card("t_v", runtime.StageValidate, "blocked"),
	}, fixture.deliveryID)
	hermes, _ := fakeBoard(t)
	run := state.RunOverview{DeliveryID: fixture.deliveryID, RunID: "TKT-4242", IssueID: 4242, IssueKey: "TKT-4242"}

	// The report a code produces is identified by the digest it is begun
	// with, so the two candidate codes are rendered here and the one the run
	// actually began with is read back.
	terminal := runner.NewTerminal(config, fixture.services, envelope, chainOwnerRunID(fixture.deliveryID), runDirectory(config, fixture.deliveryID), &recordingLogger{})
	digestFor := func(code hook.TerminalCode) string {
		t.Helper()
		digest, err := terminal.ReportDigest(context.Background(), code, runner.Outcome{Code: code}, "")
		if err != nil {
			t.Fatal(err)
		}
		return digest
	}
	expected, wrong := digestFor(hook.TerminalValidationFailed), digestFor(hook.TerminalModelFailed)
	fixture.store.expected = expected

	if err := handleChainFailure(context.Background(), config, fixture.services, hermes, envelope, run, view,
		runtime.StageValidate, &recordingLogger{}); err != nil {
		t.Fatalf("the failure was not handled: %v", err)
	}
	if len(fixture.store.digests) != 1 {
		t.Fatalf("terminal reports begun = %d", len(fixture.store.digests))
	}
	if fixture.store.digests[0] == wrong {
		t.Fatal("the requester was told the AI did not answer; it answered and the validation refused it")
	}
	if fixture.store.digests[0] != expected {
		t.Fatalf("the ceiling ended the run under an unexpected code: %q", fixture.store.digests[0])
	}
	// The sentence that reaches the requester is the one this code carries.
	if len(fixture.comments.posted) != 1 ||
		!strings.Contains(fixture.comments.posted[0], "生成した変更が検証を通過しなかった") {
		t.Fatalf("the requester was not told the validation refused the change: %q", fixture.comments.posted)
	}
}

// And with no limit written down, the same delivery does not end at all: the
// declared stage budget of the destination stops nothing, and the next round
// is told what the validation printed.
func TestADeclaredStageBudgetNoLongerEndsARefusedValidation(t *testing.T) {
	fixture, config, envelope, _, _ := refusedValidationFixture(t)
	if err := os.WriteFile(config.ConsumerConfigPath,
		[]byte(`{"max_stages":1,"models":{"reviewers":[{"id":"review-a"},{"id":"review-b"}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	card := func(id, stage, status string) runtime.BoardTask {
		return runtime.BoardTask{ID: id, Status: status, IdempotencyKey: runtime.ChainCardKey(fixture.deliveryID, stage, 1)}
	}
	view := chainViewFor([]runtime.BoardTask{
		card("t_impl", runtime.StageImplement, "done"),
		card("t_v", runtime.StageValidate, "blocked"),
	}, fixture.deliveryID)
	hermes, boardLog := fakeBoard(t)
	run := state.RunOverview{DeliveryID: fixture.deliveryID, RunID: "TKT-4242", IssueID: 4242, IssueKey: "TKT-4242"}
	if err := handleChainFailure(context.Background(), config, fixture.services, hermes, envelope, run, view,
		runtime.StageValidate, &recordingLogger{}); err != nil {
		t.Fatalf("the failure was not handled: %v", err)
	}
	if len(fixture.store.digests) != 0 {
		t.Fatalf("the delivery ended at the declared budget: %v", fixture.store.digests)
	}
	if len(fixture.comments.posted) != 0 {
		t.Fatalf("the requester was told the run ended: %q", fixture.comments.posted)
	}
	board, err := os.ReadFile(boardLog)
	if err != nil {
		t.Fatalf("the board was not called: %v", err)
	}
	if !strings.Contains(string(board), "|create|") {
		t.Fatalf("no card was created for the next round:\n%s", board)
	}
}
