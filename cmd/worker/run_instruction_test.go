package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/worker"
)

// The cards orchestration's implement and apply cards run their agent
// through the worker on the rendered instruction — the same launch path
// as every reviewing agent — and record the run; a role without a launch
// fails closed instead of running anything as the engine.
func TestRunInstructionRunsTheAgentOnTheRenderedInstruction(t *testing.T) {
	fixture := newAgentFixture(t, `printf 'instruction: %s\n' "$1"; `+editTheLabel, "true")
	instruction := filepath.Join(t.TempDir(), "INSTRUCTION.md")
	if err := os.WriteFile(instruction, []byte("Change the label exactly as the ticket says.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(t.TempDir(), "implementer-run.json")
	common := []string{"--config", fixture.configPath, "--tool-sha", cliToolSHA, "--draft", fixture.draftPath,
		"--instruction", instruction, "--repo-root", fixture.repoRoot, "--base-sha", fixture.baseSHA, "--stage", "1"}
	err := run(context.Background(), append([]string{"run-instruction", "--role", "implementer", "--out", record}, common...))
	if err != nil {
		t.Fatalf("run-instruction implementer: %v", err)
	}
	var sealed worker.AgentRun
	if err := worker.ReadJSONFile(record, worker.MaxArtifactJSONBytes, &sealed); err != nil {
		t.Fatalf("run record: %v", err)
	}
	if sealed.AgentID != fixture.config.Agents.Implementer.ID || sealed.PromptBytes != len("Change the label exactly as the ticket says.\n") ||
		sealed.Stage != 1 || sealed.BaseSHA != fixture.baseSHA || len(sealed.ChangedFiles) == 0 ||
		!strings.Contains(sealed.Transcript, "instruction: Change the label") {
		t.Fatalf("run record = %+v", sealed)
	}

	applierRecord := filepath.Join(t.TempDir(), "applier-run.json")
	err = run(context.Background(), append([]string{"run-instruction", "--role", "applier", "--out", applierRecord}, common...))
	if err == nil || !strings.Contains(err.Error(), "applier agent is not configured") {
		t.Fatalf("applier without a launch: %v", err)
	}
	if _, statErr := os.Stat(applierRecord); statErr == nil {
		t.Fatal("a record was written for a role that did not run")
	}
	err = run(context.Background(), append([]string{"run-instruction", "--role", "reviewer", "--out", applierRecord}, common...))
	if err == nil || !strings.Contains(err.Error(), "role is invalid") {
		t.Fatalf("unknown role: %v", err)
	}
}
