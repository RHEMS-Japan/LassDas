package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/worker"
)

// argvPipeline is a stage whose worker records the arguments it was given,
// which is the only place the joining of the plan to the round is visible:
// the tick that decides the plan and the command that renders the
// instruction are tested on either side of this, and neither can see it.
func argvPipeline(t *testing.T) (*Pipeline, string) {
	t.Helper()
	record := filepath.Join(t.TempDir(), "argv.txt")
	script := filepath.Join(t.TempDir(), "worker")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+record+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	consumerPath := filepath.Join(workspace, "consumer.json")
	if err := os.WriteFile(consumerPath, []byte(`{"models":{"reviewers":[{"id":"review-a"},{"id":"review-b"}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	config := runtime.Config{WorkerBin: script, ConsumerConfigPath: consumerPath}
	config.Identity.EngineSHA = strings.Repeat("ab", 20)
	pipeline := &Pipeline{Config: config, Workspace: workspace, Logger: trailTestLogger{}}
	if err := os.WriteFile(pipeline.path("readiness-ticket.json"), []byte(`{"target_files":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return pipeline, record
}

// sealArgvReleasePath writes a plan onto the run directory the way the tick
// that claimed the delivery writes it.
func sealArgvReleasePath(t *testing.T, workspace string) {
	t.Helper()
	plan := worker.ReleasePathPlan{
		SchemaVersion: worker.ReleasePathSchemaVersion,
		Repository:    "example/target", Configured: "production",
		Items: []worker.ReleasePathItem{{
			Name: "デプロイ工程のうちリポジトリの中で動く部分", Kind: worker.ReleasePathWorkflow,
			Detail: "反映に使うマニフェストを用意してください。",
		}},
		Instruction: "### この納品先にはまだリリース経路がありません",
	}
	if err := plan.Seal(); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ReleasePathPlanFile(workspace), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

// The plan reaches the command that renders the round. Everything either
// side of this is covered — the tick decides the plan, the command places
// it — and without the argument that joins them the round is rendered with
// no path-building work in it and nothing anywhere says so.
func TestTheRoundIsRenderedWithTheSealedReleasePath(t *testing.T) {
	pipeline, record := argvPipeline(t)
	sealArgvReleasePath(t, pipeline.Workspace)

	if err := pipeline.RenderImplementInstruction(context.Background(), 1); err != nil {
		t.Fatalf("RenderImplementInstruction: %v", err)
	}
	argv, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("the worker was not run: %v", err)
	}
	if !strings.Contains(string(argv), "--release-path "+ReleasePathPlanFile(pipeline.Workspace)) {
		t.Fatalf("the sealed release path did not reach the round: %s", argv)
	}
}

// A delivery with no plan renders exactly the arguments it rendered before
// any of this existed. The command refuses a path it cannot read, so a flag
// pointing at nothing would end every round of every destination whose path
// is already complete.
func TestARoundWithNoSealedReleasePathPassesNoFlag(t *testing.T) {
	pipeline, record := argvPipeline(t)

	if err := pipeline.RenderImplementInstruction(context.Background(), 1); err != nil {
		t.Fatalf("RenderImplementInstruction: %v", err)
	}
	argv, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("the worker was not run: %v", err)
	}
	if strings.Contains(string(argv), "--release-path") {
		t.Fatalf("a flag was passed with no plan behind it: %s", argv)
	}
}

// A record that does not read back whole is not passed at all. The command
// refuses a path it cannot read — correctly, because a round rendered
// without what it was being run for has quietly lost half its job — so the
// stage has to decide, not discover.
func TestAHalfWrittenReleasePathIsNotPassedToTheRound(t *testing.T) {
	pipeline, record := argvPipeline(t)
	if err := os.WriteFile(ReleasePathPlanFile(pipeline.Workspace),
		[]byte(`{"schema_version":1,"repository":"example/target"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := pipeline.RenderImplementInstruction(context.Background(), 1); err != nil {
		t.Fatalf("RenderImplementInstruction: %v", err)
	}
	argv, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("the worker was not run: %v", err)
	}
	if strings.Contains(string(argv), "--release-path") {
		t.Fatalf("a record with no digest was passed to the round: %s", argv)
	}
}

// The round carries every record that explains why it is being run, and
// they do not displace each other. A round that was handed back and whose
// destination has no release path is told both things: what the engine
// decided in the agent's place, and the path it is also building.
func TestAReturnedRoundAndAReleasePathBothReachTheRound(t *testing.T) {
	pipeline, record := argvPipeline(t)
	sealArgvReleasePath(t, pipeline.Workspace)
	answer := worker.AnswerReturn(returnedLaunch("鍵が渡されていません。", 1), nil, time.Now().UTC())
	if err := RecordReturn(pipeline.Workspace, 1, answer); err != nil {
		t.Fatalf("RecordReturn: %v", err)
	}

	if err := pipeline.RenderImplementInstruction(context.Background(), 1); err != nil {
		t.Fatalf("RenderImplementInstruction: %v", err)
	}
	argv, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("the worker was not run: %v", err)
	}
	for _, want := range []string{
		"--returned " + ReturnRecordFile(pipeline.Workspace, 1),
		"--release-path " + ReleasePathPlanFile(pipeline.Workspace),
	} {
		if !strings.Contains(string(argv), want) {
			t.Fatalf("the round was not given %q: %s", want, argv)
		}
	}
}
