package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/worker"
)

// releasePathFixture is a plan as the tick that found the gap seals it.
func releasePathPlanFixture(t *testing.T) worker.ReleasePathPlan {
	t.Helper()
	plan := worker.ReleasePathPlan{
		SchemaVersion: worker.ReleasePathSchemaVersion,
		Repository:    "example/consumer", Configured: "production",
		Items: []worker.ReleasePathItem{
			{Name: "デプロイ工程のうちリポジトリの中で動く部分", Kind: worker.ReleasePathWorkflow,
				Detail: "反映に使うマニフェストを用意してください。"},
			{Name: ".github/workflows/deploy-production.yml", Kind: worker.ReleasePathWorkflow,
				Detail: "本番へ反映する workflow がリポジトリにありません。",
				Means:  "先頭がドットのディレクトリの中は本体が書けません。"},
		},
		Instruction: "### この納品先にはまだリリース経路がありません (依頼と同じ変更で作ってください)\n" +
			"- デプロイ工程のうちリポジトリの中で動く部分: 反映に使うマニフェストを用意してください。",
	}
	if err := plan.Seal(); err != nil {
		t.Fatal(err)
	}
	return plan
}

func releasePathConsumer() (worker.TicketDraft, worker.ConsumerConfig, worker.AgentConfig) {
	draft := worker.TicketDraft{IssueKey: "TEST-1", Summary: "件名", Request: "本文", Repository: "example/target"}
	consumer := worker.ConsumerConfig{Repository: "example/target", Mode: worker.ModeConfig{
		AllowedFilePrefixes: []string{"README.md", "docs/"},
		MaxFiles:            5, MaxChangedLines: 400, MaxChangedBytes: 32768, MaxFileBytes: 65536,
	}}
	return draft, consumer, worker.AgentConfig{ID: "implementer", Command: "agent"}
}

// A round run for a destination with no release path is told to build one,
// in the same instruction and the same pull request as the request itself.
// The rule that normally keeps an implementer away from a repository's
// release machinery narrows at the same time: building it is the work, and
// an instruction that asked for it and forbade it in the same breath would
// be answered by doing neither.
func TestTheInstructionCarriesThePathBuildingWork(t *testing.T) {
	draft, consumer, agent := releasePathConsumer()
	plan := releasePathPlanFixture(t)
	prompt, err := implementPrompt(draft, consumer, agent, nil, nil, nil, nil, nil, &plan, "/work/repo", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, plan.Instruction) {
		t.Fatalf("the instruction does not carry the path-building work:\n%s", prompt)
	}
	if strings.Contains(prompt, "自動化・リリース手順・資格情報・権限設定には触れないでください") {
		t.Fatalf("the round was told to build a release path and forbidden to touch one:\n%s", prompt)
	}
	if !strings.Contains(prompt, "資格情報・権限設定・利用上限には触れないでください") {
		t.Fatalf("the credentials are no longer out of bounds:\n%s", prompt)
	}
	// What the engine cannot apply is named in the report, never here: a
	// round can do nothing about it, and naming it would read as work.
	if strings.Contains(prompt, ".github/workflows/deploy-production.yml") {
		t.Fatalf("an unapplied part reached the round's instruction:\n%s", prompt)
	}
}

// A destination whose path is complete gets no plan, and its instruction is
// exactly the one it got before any of this existed.
func TestAnInstructionWithoutAPlanIsUnchanged(t *testing.T) {
	draft, consumer, agent := releasePathConsumer()
	prompt, err := implementPrompt(draft, consumer, agent, nil, nil, nil, nil, nil, nil, "/work/repo", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "自動化・リリース手順・資格情報・権限設定には触れないでください") {
		t.Fatalf("the release machinery is no longer out of bounds by default:\n%s", prompt)
	}
	if strings.Contains(prompt, "リリース経路") {
		t.Fatalf("a destination with a complete path was told to build one:\n%s", prompt)
	}
}

// The flag is read the way the previous round's refused validation is: a
// path that was given and cannot be read is a failure, because the round is
// being rendered to build what is in that file.
func TestAnUnreadableReleasePathPlanIsRefused(t *testing.T) {
	if plan, err := readReleasePath(""); plan != nil || err != nil {
		t.Fatalf("no flag should mean no plan: %v, %v", plan, err)
	}
	path := t.TempDir() + "/release-path.json"
	if err := os.WriteFile(path, []byte(`{"schema_version":1,"repository":"example/target"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readReleasePath(path); err == nil {
		t.Fatal("a record with no digest was accepted")
	}
}

// The rendered file, not a function's return value. The plan is decided by
// the tick that claims the delivery and carried to the round through a
// record on the run directory and a flag, because the round is rendered
// long before anything the delivery seals afterwards exists — and because
// the package that decides it cannot be called from either the stage or
// this command.
func TestTheRenderedInstructionFileCarriesThePathBuildingWork(t *testing.T) {
	fixture := newAgentFixture(t, "true", "true")
	planPath := fixture.path("release-path.json")
	plan := releasePathPlanFixture(t)
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{
		"implement-instruction", "--config", fixture.configPath, "--tool-sha", cliToolSHA,
		"--draft", fixture.draftPath, "--repo-root", fixture.repoRoot,
		"--release-path", planPath, "--out", fixture.path("INSTRUCTION.md"),
	}); err != nil {
		t.Fatalf("implement-instruction: %v", err)
	}
	instruction, err := os.ReadFile(fixture.path("INSTRUCTION.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"### この納品先にはまだリリース経路がありません",
		"デプロイ工程のうちリポジトリの中で動く部分",
		"資格情報・権限設定・利用上限には触れないでください",
	} {
		if !strings.Contains(string(instruction), want) {
			t.Fatalf("the rendered instruction does not carry %q:\n%s", want, instruction)
		}
	}
	if strings.Contains(string(instruction), ".github/workflows/deploy-production.yml") {
		t.Fatalf("an unapplied part reached the rendered instruction:\n%s", instruction)
	}
}

// The plan restates the destination it was decided for, and that
// restatement is checked against the draft this round is bound to. A run
// directory outlives its cards, so a plan left by another destination would
// otherwise tell this round to build a release path for somewhere else.
func TestAReleasePathPlanForAnotherDestinationIsRefused(t *testing.T) {
	fixture := newAgentFixture(t, "true", "true")
	planPath := fixture.path("release-path.json")
	plan := releasePathPlanFixture(t)
	plan.Repository = "example/elsewhere"
	if err := plan.Seal(); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	err = run(context.Background(), []string{
		"implement-instruction", "--config", fixture.configPath, "--tool-sha", cliToolSHA,
		"--draft", fixture.draftPath, "--repo-root", fixture.repoRoot,
		"--release-path", planPath, "--out", fixture.path("INSTRUCTION.md"),
	})
	if err == nil || !strings.Contains(err.Error(), "not bound to this run") {
		t.Fatalf("a plan for another destination was accepted: %v", err)
	}
	if _, statErr := os.Stat(fixture.path("INSTRUCTION.md")); statErr == nil {
		t.Fatal("an instruction was written from a plan that is not this run's")
	}
}

// An instruction too large to render drops the earlier rounds' objections
// and renders again, because the request and the boundaries are the job.
// The path the destination is missing is part of the job too: dropped with
// the objections, a delivery whose findings had filled the budget would
// build the change and quietly leave the destination with no way to deploy
// it, and nothing would say so.
func TestAReleasePathSurvivesTheOverflowRebuild(t *testing.T) {
	draft, consumer, agent := releasePathConsumer()
	// Sized so the objections are what tips it over: the request alone
	// renders, and the 16 KiB the objections are bounded to does not.
	draft.Request = strings.Repeat("本文。", 5600)
	plan := releasePathPlanFixture(t)
	findings := make([]worker.ModelFinding, 0, 16)
	for i := range 16 {
		findings = append(findings, worker.ModelFinding{
			Code: "finding-code", Path: "docs/EXAMPLE.md", Line: i,
			Message: strings.Repeat("指摘の本文。", 200),
		})
	}
	withFindings, err := implementPrompt(draft, consumer, agent, nil, findings, nil, nil, nil, &plan, "/work/repo", nil)
	if err != nil {
		t.Fatalf("the oversized instruction did not render: %v", err)
	}
	if len(withFindings) > worker.MaxAgentPromptBytes {
		t.Fatalf("prompt = %d bytes, bound is %d", len(withFindings), worker.MaxAgentPromptBytes)
	}
	if strings.Contains(withFindings, "### 前回の指摘") {
		t.Fatal("the fixture did not overflow, so nothing was rebuilt")
	}
	if !strings.Contains(withFindings, plan.Instruction) {
		t.Fatalf("the release path was dropped with the objections:\n%s", withFindings)
	}
}
