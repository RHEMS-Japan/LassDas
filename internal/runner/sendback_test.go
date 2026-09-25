package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/worker"
)

// writeImplementRun leaves the record the implement verb writes before the
// seal refuses an unchanged working copy.
func writeImplementRun(t *testing.T, workspace string, round int, changed []string, transcript string) {
	t.Helper()
	stageDir := filepath.Join(workspace, "history", "stage-"+string(rune('0'+round)))
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sealed, err := worker.SealAgentRun(worker.AgentRun{
		SchemaVersion: worker.ArtifactSchemaVersion, Stage: round, AgentID: "implementer",
		ChangedFiles: changed, Transcript: transcript, RanAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.WriteJSONFileExclusive(filepath.Join(stageDir, "implement-run.json"), sealed, worker.MaxArtifactJSONBytes); err != nil {
		t.Fatal(err)
	}
}

// The one-process mode reached the same defect by a different road: the
// implement verb refused to seal an empty change, the round ended as
// model_failed, and the implementer's reason went nowhere (live
// 2026-09-25). The round reads the record the verb left and ends on the
// code that says what actually happened.
func TestImplementRoundsEndsOnTheReportWhenTheImplementerReturnsTheWork(t *testing.T) {
	workspace := t.TempDir()
	script := filepath.Join(t.TempDir(), "worker")
	// A worker that fails the way the implement verb fails when it will
	// not seal an empty change.
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 9\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := runtime.Config{WorkerBin: script, ConsumerConfigPath: writeRunnerConfig(t, runnerFixtureConfig(t, 2, 3, true))}
	config.Identity.EngineSHA = strings.Repeat("ab", 20)
	pipeline := &Pipeline{Config: config, Workspace: workspace, Logger: trailTestLogger{}}
	writeImplementRun(t, workspace, 1, nil, "この依頼は実現できません。その判断は依頼者に返します。")

	outcome, _ := pipeline.implementRounds(context.Background(),
		filepath.Join(workspace, "target-repo"), filepath.Join(workspace, "target-base"), strings.Repeat("c", 40))
	if outcome.Code != hook.TerminalImplementationReturned {
		t.Fatalf("outcome = %+v, want the implementation returned", outcome)
	}

	// A round whose agent changed files and then failed is the model
	// failure it always was.
	changed := t.TempDir()
	writeImplementRun(t, changed, 1, []string{"client/src/label.ts"}, "直しました。")
	other := &Pipeline{Config: config, Workspace: changed, Logger: trailTestLogger{}}
	outcome, _ = other.implementRounds(context.Background(),
		filepath.Join(changed, "target-repo"), filepath.Join(changed, "target-base"), strings.Repeat("c", 40))
	if outcome.Code != hook.TerminalModelFailed {
		t.Fatalf("an ordinary failure = %+v", outcome)
	}

	// So is a round that left no record at all.
	empty := &Pipeline{Config: config, Workspace: t.TempDir(), Logger: trailTestLogger{}}
	outcome, _ = empty.implementRounds(context.Background(),
		filepath.Join(workspace, "target-repo"), filepath.Join(workspace, "target-base"), strings.Repeat("c", 40))
	if outcome.Code != hook.TerminalModelFailed {
		t.Fatalf("a round with no record = %+v", outcome)
	}
}

// The trail of a round that sealed nothing is rendered from the run record,
// which cannot say which card then blocked; the attendant's step name is
// what supplies that, so it has to reach the composer.
func TestTheBlockedStepReachesTheTrailComposer(t *testing.T) {
	record := filepath.Join(t.TempDir(), "argv.txt")
	script := filepath.Join(t.TempDir(), "worker")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+record+"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	pipeline := trailPipeline(t, script)
	pipeline.NoteBlockedStep("変更の確定")
	if err := pipeline.EnsureTrail(context.Background()); err != nil {
		t.Fatalf("EnsureTrail: %v", err)
	}
	argv, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(argv), "--blocked-step 変更の確定") {
		t.Fatalf("the blocked step did not reach the composer: %s", argv)
	}

	// A run with no card to name passes nothing rather than an empty value.
	plain := trailPipeline(t, script)
	if err := plain.EnsureTrail(context.Background()); err != nil {
		t.Fatalf("EnsureTrail: %v", err)
	}
	second, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(second), "--blocked-step") != 1 {
		t.Fatalf("a run with no card named a blocked step: %s", second)
	}
}
