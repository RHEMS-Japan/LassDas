package attendant

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
	"automation.internal/ticket-ingress/internal/worker"
)

// sendBackFakeStore is a ledger with no report already begun on the row:
// whatever this run decides to report is what it accepts. The shared
// pending fixture's store pins the digest of a model-failure report, which
// is the one ending these tests exist to rule out.
type sendBackFakeStore struct{}

func (sendBackFakeStore) BeginTerminal(context.Context, hook.TerminalBeginRequest) (hook.TerminalBinding, hook.TerminalBeginDisposition, error) {
	return hook.TerminalBinding{IssueID: 4242, IssueKey: "TKT-4242"}, hook.TerminalBeginAcquired, nil
}

func (sendBackFakeStore) CompleteTerminal(context.Context, hook.TerminalCompleteRequest) (hook.TerminalCompleteDisposition, error) {
	return hook.TerminalCompleted, nil
}

// writeReturnedRound leaves the round an implementing agent handed back:
// its run record, and no candidate sealed from it.
func writeReturnedRound(t *testing.T, runDir, report string) {
	t.Helper()
	stageDir := filepath.Join(runDir, "history", "stage-1")
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sealed, err := worker.SealAgentRun(worker.AgentRun{
		SchemaVersion: worker.ArtifactSchemaVersion, Stage: 1, AgentID: "implementer",
		Transcript: report, RanAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.WriteJSONFileExclusive(filepath.Join(stageDir, "implementer-run.json"), sealed, worker.MaxArtifactJSONBytes); err != nil {
		t.Fatal(err)
	}
}

// standInComposer is a worker that renders the trail the way the real
// compose-trail does for a round with no candidate. The composition itself
// is proven where it lives; what this stands in for is the path from the
// composed trail to the requester's comment.
func standInComposer(t *testing.T, body string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "worker")
	program := "#!/bin/sh\nout=\"\"\nwhile [ $# -gt 0 ]; do\n  if [ \"$1\" = \"--out\" ]; then out=\"$2\"; fi\n  shift\ndone\n" +
		"cat > \"$out\" <<'TRAIL'\n" + body + "\nTRAIL\n"
	if err := os.WriteFile(script, []byte(program), 0o700); err != nil {
		t.Fatal(err)
	}
	return script
}

// The implementer changed nothing and said why, twice, and the engine
// started the first review card on the empty working copy: the seal refused
// it, the card blocked, and the ticket received "model_failed" and a trail
// that could not be generated. The report never reached the requester (live
// 2026-09-25). A blocked implement card whose agent handed the work back
// now ends on its own code, with the report in the comment.
func TestAReturnedImplementationEndsOnItsOwnCodeWithTheReport(t *testing.T) {
	const report = "この依頼は、いまのままでは実現できません。\nその判断は依頼者に返します。"
	fixture := newPendingFixture(t, "")
	runDir := runDirectory(fixture.config, fixture.deliveryID)
	fixture.writeRunDir(t, "example/consumer")
	writeReturnedRound(t, runDir, report)
	config := fixture.config
	config.WorkerBin = standInComposer(t, "### 実装役の報告\n"+report)

	terminal, err := hook.NewTerminalReportService(fixture.services.Route, &sendBackFakeStore{}, fixture.comments,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	fixture.services.Report = terminal

	var envelope hook.DispatchEnvelope
	if err := json.Unmarshal([]byte(fixture.run.EnvelopeJSON), &envelope); err != nil {
		t.Fatal(err)
	}
	hermes, _ := fakeBoard(t)
	card := runtime.BoardTask{ID: "t_r1_impl", Status: "blocked",
		IdempotencyKey: runtime.ChainCardKey(fixture.deliveryID, runtime.StageImplement, 1)}
	view := chainViewFor([]runtime.BoardTask{card}, fixture.deliveryID)
	run := state.RunOverview{DeliveryID: fixture.deliveryID, RunID: "TKT-4242", IssueID: 4242, IssueKey: "TKT-4242"}
	logger := &recordingLogger{}

	if err := handleChainFailure(context.Background(), config, fixture.services, hermes, envelope, run, view,
		runtime.StageImplement, logger); err != nil {
		t.Fatalf("handleChainFailure: %v", err)
	}
	if len(fixture.comments.posted) != 1 {
		t.Fatalf("terminal comments = %q", fixture.comments.posted)
	}
	posted := fixture.comments.posted[0]
	if strings.Contains(posted, string(hook.TerminalModelFailed)) {
		t.Fatalf("the report was published as a model failure:\n%s", posted)
	}
	if !strings.Contains(posted, string(hook.TerminalImplementationReturned)) {
		t.Fatalf("the comment does not name the ending:\n%s", posted)
	}
	// The whole report, on the ticket, where the requester reads it.
	if !strings.Contains(posted, report) {
		t.Fatalf("the report did not reach the comment:\n%s", posted)
	}
	if !strings.Contains(posted, "変更を加えずに理由を報告して作業を返しました") {
		t.Fatalf("the comment does not say what happened:\n%s", posted)
	}
	if log := strings.Join(logger.lines, "\n"); !strings.Contains(log, "chain terminalized") {
		t.Fatalf("log = %q", logger.lines)
	}
}

// A card that blocked for any other reason is unchanged: the round's agent
// did change files, so there is no report to hand back and the ending is
// the model failure it always was.
func TestAnImplementCardThatFailedOtherwiseStillEndsAsAModelFailure(t *testing.T) {
	fixture := newPendingFixture(t, "")
	runDir := runDirectory(fixture.config, fixture.deliveryID)
	fixture.writeRunDir(t, "")
	stageDir := filepath.Join(runDir, "history", "stage-1")
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sealed, err := worker.SealAgentRun(worker.AgentRun{
		SchemaVersion: worker.ArtifactSchemaVersion, Stage: 1, AgentID: "implementer",
		ChangedFiles: []string{"client/src/label.ts"}, Transcript: "直しました。", RanAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.WriteJSONFileExclusive(filepath.Join(stageDir, "implementer-run.json"), sealed, worker.MaxArtifactJSONBytes); err != nil {
		t.Fatal(err)
	}

	var envelope hook.DispatchEnvelope
	if err := json.Unmarshal([]byte(fixture.run.EnvelopeJSON), &envelope); err != nil {
		t.Fatal(err)
	}
	hermes, _ := fakeBoard(t)
	card := runtime.BoardTask{ID: "t_r1_impl", Status: "blocked",
		IdempotencyKey: runtime.ChainCardKey(fixture.deliveryID, runtime.StageImplement, 1)}
	view := chainViewFor([]runtime.BoardTask{card}, fixture.deliveryID)
	run := state.RunOverview{DeliveryID: fixture.deliveryID, RunID: "TKT-4242", IssueID: 4242, IssueKey: "TKT-4242"}

	if err := handleChainFailure(context.Background(), fixture.config, fixture.services, hermes, envelope, run, view,
		runtime.StageImplement, &recordingLogger{}); err != nil {
		t.Fatalf("handleChainFailure: %v", err)
	}
	if len(fixture.comments.posted) != 1 || !strings.Contains(fixture.comments.posted[0], string(hook.TerminalModelFailed)) {
		t.Fatalf("an ordinary implement failure changed its ending: %q", fixture.comments.posted)
	}
}

// The operator's board says what happened, and "ended in failure" over a
// detail line saying the implementer answered leaves the operator to work
// out which half to believe. This ending stands with the other stops.
func TestTheBoardDoesNotCallAReturnedImplementationAFailure(t *testing.T) {
	config := runtime.Config{Chain: runtime.ChainConfig{RunsRoot: t.TempDir()}}
	run := state.RunOverview{DeliveryID: "delivery_abc", TerminalCode: string(hook.TerminalImplementationReturned)}

	var status RunStatus
	classifyAfterTerminal(&status, config, run, nil)
	if status.Step != "stopped" {
		t.Fatalf("step = %q, want it to rest with the other stops", status.Step)
	}
	if strings.Contains(status.StepTitle, "失敗") {
		t.Fatalf("the board calls an answer a failure: %q", status.StepTitle)
	}
	if !strings.Contains(status.StepTitle, "実装役") || !strings.Contains(status.Detail, "理由を報告") {
		t.Fatalf("the board does not say what happened: %q / %q", status.StepTitle, status.Detail)
	}

	// An ending that really is one still reads as one.
	var failed RunStatus
	classifyAfterTerminal(&failed, config,
		state.RunOverview{DeliveryID: "delivery_abc", TerminalCode: string(hook.TerminalModelFailed)}, nil)
	if failed.Step != "failed" {
		t.Fatalf("a model failure = %q", failed.Step)
	}
}
