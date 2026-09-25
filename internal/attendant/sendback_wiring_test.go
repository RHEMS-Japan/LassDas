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
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
	"automation.internal/ticket-ingress/internal/worker"
)

// sendBackFakeStore is a ledger with no report already begun on the row:
// whatever this run decides to report is what it accepts. The shared
// pending fixture's store pins the digest of a model-failure report, and
// these tests measure a delivery that ends on something else.
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

// A card that blocked for any other reason still takes the other branch:
// the round's agent did change files, so there is no report to hand back.
// What that branch does is no longer to end the delivery — the ladder
// climbs it — so the ending is measured where an operator asked for one by
// capping the attempts.
func TestAnImplementCardThatFailedOtherwiseIsNotReadAsAnAnswer(t *testing.T) {
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
	exhaustTheLadder(t, &fixture.config, runDir, runtime.StageImplement, 1)
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
