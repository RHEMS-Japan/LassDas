package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validationEvidenceFixture(t *testing.T) (string, Config, TicketRequest, SourceSnapshot, Candidate, string) {
	t.Helper()
	binDirectory := t.TempDir()
	writeVersionTool := func(name, output string) {
		t.Helper()
		filename := filepath.Join(binDirectory, name)
		content := "#!/bin/sh\nprintf '%s\\n' '" + output + "'\n"
		if err := os.WriteFile(filename, []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeVersionTool("node", "v22.12.0")
	writeVersionTool("pnpm", "9.15.4")
	t.Setenv("PATH", binDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))

	logPath := filepath.Join(t.TempDir(), "commands.jsonl")
	config := validTestConfig()
	config.Consumers[0].Mode.InstallCommand = validationTestCommand(logPath, "install")
	config.Consumers[0].Mode.VerifyCommands = [][]string{validationTestCommand(logPath, "typecheck"), validationTestCommand(logPath, "build")}
	request, err := ParseTicket(validTicketEnvelope(t, validTicketDescription()), config)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	filename := filepath.Join(root, filepath.FromSlash(request.TargetFiles[0]))
	if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte("export const label = 'Old label';\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	source, err := ReadSourceSnapshot(root, strings.Repeat("a", 40), request, config)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := NewCandidate(1, ModelCandidateOutput{
		Files:     []ModelCandidateFile{{Path: request.TargetFiles[0], Content: "export const label = 'Updated label';\n"}},
		Rationale: "Update the requested visible label.",
	}, source, request, config, validTestInvocation(config.Models.Implementer), testInvocationTime)
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplyCandidate(root, candidate, source, request, config); err != nil {
		t.Fatal(err)
	}
	return root, config, request, source, candidate, logPath
}

func TestRunValidationEvidenceBindsExecutionAndPublishGate(t *testing.T) {
	root, config, request, source, candidate, logPath := validationEvidenceFixture(t)
	evidence, err := RunValidationEvidence(context.Background(), root, candidate, source, request, config, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence.Tools) != 2 || evidence.Tools[0].Version != "22.12.0" || evidence.Tools[1].Version != "9.15.4" || len(evidence.Commands) != 3 {
		t.Fatalf("evidence = %+v", evidence)
	}
	if err := evidence.Validate(candidate, source, request, config); err != nil {
		t.Fatal(err)
	}
	encoded, err := os.ReadFile(logPath)
	if err != nil || len(strings.Split(strings.TrimSpace(string(encoded)), "\n")) != 3 {
		t.Fatalf("command log = %q; error = %v", encoded, err)
	}

	reviews := make([]Review, 0, len(config.Models.Reviewers))
	for _, endpoint := range config.Models.Reviewers {
		review, err := NewReview(1, endpoint, ModelReviewOutput{Verdict: "pass", Findings: []ModelFinding{}}, candidate, source, request, config, validTestInvocation(endpoint), testInvocationTime)
		if err != nil {
			t.Fatal(err)
		}
		reviews = append(reviews, review)
	}
	decision, err := DecideStage(candidate, reviews, source, request, config)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidatePublishGate(decision, evidence, candidate, reviews, source, request, config); err != nil {
		t.Fatal(err)
	}

	tampered := evidence
	tampered.Commands = expectedValidationCommands(config.Consumers[0])
	tampered.Commands[0][0] = "weaker-command"
	if err := ValidatePublishGate(decision, tampered, candidate, reviews, source, request, config); err == nil {
		t.Fatal("ValidatePublishGate() accepted tampered validation evidence")
	}
}

func TestRunValidationEvidenceRejectsConfigDrift(t *testing.T) {
	root, config, request, source, candidate, _ := validationEvidenceFixture(t)
	config.Consumers[0].Mode.VerifyCommands = [][]string{{"sh", "-c", "exit 0"}}
	if _, err := RunValidationEvidence(context.Background(), root, candidate, source, request, config, ""); err == nil {
		t.Fatal("RunValidationEvidence() accepted config drift")
	}
}
