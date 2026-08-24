package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/worker"
)

// sealCandidate runs the verb against the fixture's working copy, as the M2
// orchestration would after its own implementer card finished.
func (f agentFixture) sealCandidate(t *testing.T, extra ...string) error {
	t.Helper()
	args := []string{
		"seal-candidate", "--config", f.configPath, "--tool-sha", cliToolSHA, "--draft", f.draftPath,
		"--repo-root", f.repoRoot, "--base-root", f.baseRoot, "--base-sha", f.baseSHA, "--stage", "1",
		"--run-out", f.path("run.json"), "--ticket-out", f.path("ticket.json"),
		"--source-out", f.path("source.json"), "--out", f.path("candidate.json"),
	}
	return run(context.Background(), append(args, extra...))
}

// The change is made by nobody this program started: the file is edited
// directly, standing in for a Hermes-launched implementer, and the verb seals
// exactly what it observes.
func TestSealCandidateSealsAnExternallyMadeChange(t *testing.T) {
	fixture := newAgentFixture(t, "true", "true")
	target := filepath.Join(fixture.repoRoot, "client", "src", "label.ts")
	if err := os.WriteFile(target, []byte("export const label = 'Updated label';\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The report lives outside the working copy: inside it, the report itself
	// would be an observed change.
	reportPath := fixture.path("report.txt")
	if err := os.WriteFile(reportPath, []byte("I changed the label as asked."), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fixture.sealCandidate(t, "--report", reportPath); err != nil {
		t.Fatal(err)
	}

	var record worker.AgentRun
	var request worker.TicketRequest
	var source worker.SourceSnapshot
	var candidate worker.Candidate
	readAgentArtifact(t, fixture.path("run.json"), worker.MaxArtifactJSONBytes, &record)
	readAgentArtifact(t, fixture.path("ticket.json"), worker.MaxTicketJSONBytes, &request)
	readAgentArtifact(t, fixture.path("source.json"), worker.MaxArtifactJSONBytes, &source)
	readAgentArtifact(t, fixture.path("candidate.json"), worker.MaxArtifactJSONBytes, &candidate)

	if record.Kind != worker.AgentRunKindExternal || record.AgentID != worker.AgentRunKindExternal ||
		record.Command != "" || record.PromptBytes != 0 || record.ExitCode != 0 {
		t.Fatalf("the run record carries launch claims: %+v", record)
	}
	if err := record.Validate(fixture.config); err != nil {
		t.Fatalf("the sealed run was rejected: %v", err)
	}
	if len(request.TargetFiles) != 1 || request.TargetFiles[0] != "client/src/label.ts" {
		t.Fatalf("the contract did not follow the observed change: %v", request.TargetFiles)
	}
	if err := candidate.Validate(source, request, fixture.config); err != nil {
		t.Fatalf("the sealed candidate was rejected: %v", err)
	}
	if source.Files[0].Content != "export const label = 'Old label';\n" {
		t.Fatalf("the before-bytes were not taken from the base revision: %q", source.Files[0].Content)
	}
	if candidate.Files[0].Content != "export const label = 'Updated label';\n" {
		t.Fatalf("the after-bytes were not taken from the working copy: %q", candidate.Files[0].Content)
	}
	if !strings.Contains(candidate.Rationale, "I changed the label as asked.") {
		t.Fatalf("the implementer report was not recorded: %q", candidate.Rationale)
	}
}

func TestSealCandidateWithoutReportSealsAnEmptyAccount(t *testing.T) {
	fixture := newAgentFixture(t, "true", "true")
	target := filepath.Join(fixture.repoRoot, "client", "src", "label.ts")
	if err := os.WriteFile(target, []byte("export const label = 'Updated label';\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fixture.sealCandidate(t); err != nil {
		t.Fatal(err)
	}
	var candidate worker.Candidate
	readAgentArtifact(t, fixture.path("candidate.json"), worker.MaxArtifactJSONBytes, &candidate)
	if candidate.Rationale != "The agent reported nothing." {
		t.Fatalf("a missing report was not recorded as silence: %q", candidate.Rationale)
	}
}

func TestSealCandidateRejectsAChangeOutsideTheScope(t *testing.T) {
	fixture := newAgentFixture(t, "true", "true")
	if err := os.WriteFile(filepath.Join(fixture.repoRoot, "outside.txt"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fixture.sealCandidate(t); err == nil {
		t.Fatal("a change outside the allowed scope was sealed")
	}
	if _, err := os.Stat(fixture.path("candidate.json")); err == nil {
		t.Fatal("a candidate was written for a rejected change")
	}
}

func TestSealCandidateRejectsWhenNothingChanged(t *testing.T) {
	fixture := newAgentFixture(t, "true", "true")
	if err := fixture.sealCandidate(t); err == nil {
		t.Fatal("an untouched working copy was sealed")
	}
}

// The cards mode's implement card is a native agent whose whole prompt is
// this rendered file: the kernel authors the instruction even though the
// kanban launches the agent.
func TestImplementInstructionRendersTheKernelPrompt(t *testing.T) {
	fixture := newAgentFixture(t, "true", "true")
	previous := fixture.path("prior-review.json")
	writeTestJSON(t, previous, map[string]any{"findings": []map[string]any{
		{"code": "from-first-reviewer", "path": "client/src/label.ts", "message": "First objection."},
	}})
	out := fixture.path("INSTRUCTION.md")
	if err := run(context.Background(), []string{
		"implement-instruction", "--config", fixture.configPath, "--tool-sha", cliToolSHA,
		"--draft", fixture.draftPath, "--previous-findings", previous, "--out", out,
	}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"## 依頼", "## 守ること", "from-first-reviewer"} {
		if !strings.Contains(string(content), expected) {
			t.Fatalf("the rendered instruction lacks %q: %q", expected, content)
		}
	}
}
