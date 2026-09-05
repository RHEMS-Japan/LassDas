package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/worker"
)

var cliToolSHA = strings.Repeat("c", 40)

func cliTestConfig() worker.Config {
	return worker.Config{
		SchemaVersion: worker.ConfigSchemaVersion,
		Consumers: []worker.ConsumerConfig{{
			Repository: "example/consumer", RepositoryID: 101,
			Delivery: worker.DeliverPullRequest, IntegrationBranch: "stg", ReleaseBranch: "prod",
			StagingOrigin: "https://stg.example.com", ProductionOrigin: "https://example.com",
			StagingWorkflow: "deploy-stg.yml", ProductionWorkflow: "deploy.yml",
			GitHub: worker.ConsumerGitHubContract{
				DefaultBranch: "prod",
				MergeSettings: worker.ConsumerMergeSettings{
					AllowMergeCommit:         true,
					SquashMergeCommitTitle:   "COMMIT_OR_PR_TITLE",
					SquashMergeCommitMessage: "COMMIT_MESSAGES",
					MergeCommitTitle:         "MERGE_MESSAGE",
					MergeCommitMessage:       "PR_TITLE",
				},
				StagingWorkflow: worker.ConsumerWorkflow{ID: 101101, Name: "Deploy (stg)", Path: ".github/workflows/deploy-stg.yml"},
				ProductionWorkflows: []worker.ConsumerWorkflow{
					{ID: 101102, Name: "Deploy", Path: ".github/workflows/deploy.yml"},
				},
			},
			Mode: worker.ModeConfig{
				ID: "client-visible-change", AllowedFilePrefixes: []string{"client/src/"},
				ForbiddenCandidateText: []string{"forbidden-project-name"},
				MaxFiles:               3, MaxFileBytes: 256 * 1024, MaxTotalBytes: 512 * 1024,
				MaxChangedLines: 200, MaxChangedBytes: 64 * 1024,
				Toolchain:              []worker.ToolRequirement{{Binary: "node", Version: "22", StripVPrefix: true}, {Binary: "pnpm", Version: "9.15.4"}},
				VerifyWorkingDirectory: "client",
				InstallCommand:         []string{"pnpm", "install", "--frozen-lockfile"},
				VerifyCommands:         [][]string{{"pnpm", "exec", "tsc", "--noEmit"}, {"pnpm", "build"}},
			},
		}},
		Models: worker.ModelConfig{
			Implementer: worker.ModelEndpoint{ID: "author", Vendor: "Vendor A", Model: "model-a", BaseURL: "https://gateway.example.com/api/v1", APIKeyEnv: "TEST_MODEL_KEY_A", MaxOutputTokens: 4096},
			Reviewers: []worker.ModelEndpoint{
				{ID: "review-a", Vendor: "Vendor A", Model: "model-a", BaseURL: "https://gateway.example.com/api/v1", APIKeyEnv: "TEST_MODEL_KEY_A", Lens: "correctness", MaxOutputTokens: 2048},
				{ID: "review-b", Vendor: "Vendor B", Model: "model-b", BaseURL: "https://gateway.example.com/api/v1", APIKeyEnv: "TEST_MODEL_KEY_B", Lens: "adversarial", MaxOutputTokens: 2048},
			},
			Readiness: worker.ReadinessModels{
				Assessor: worker.ModelEndpoint{ID: "readiness-assessor", Vendor: "Vendor A", Model: "model-a", BaseURL: "https://gateway.example.com/api/v1", APIKeyEnv: "TEST_MODEL_KEY_A", Effort: "high", MaxOutputTokens: 4096},
				Checker:  worker.ModelEndpoint{ID: "readiness-checker", Vendor: "Vendor B", Model: "model-b", BaseURL: "https://gateway.example.com/api/v1", APIKeyEnv: "TEST_MODEL_KEY_B", Lens: "readiness adversarial", StructuredOutput: true, MaxOutputTokens: 2048},
			},
		},
		Agents: worker.AgentSet{
			Implementer: worker.AgentConfig{
				ID: "author-agent", Command: "true", TimeoutSeconds: 900,
				SecretEnv: map[string]string{"AGENT_KEY": "TEST_MODEL_KEY_A"},
			},
			Reviewer: worker.AgentConfig{
				ID: "review-agent", Command: "false", TimeoutSeconds: 900,
				SecretEnv: map[string]string{"AGENT_KEY": "TEST_MODEL_KEY_B"},
			},
		},
		MaxStages: 3,
	}
}

func writeTestJSON(t *testing.T, filename string, value any) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

func cliTestEnvelope(t *testing.T) hook.DispatchEnvelope {
	t.Helper()
	return cliTestEnvelopeWithDescription(t, strings.Join([]string{
		"Automation-Run-ID: run_20260802_alpha",
		"Automation-Mode: client-visible-change",
		"Target-File: client/src/components/Example.tsx",
		"Verification-Path: /settings",
		"Expected-Text: Updated label",
		"Absent-Text: Old label",
		"---",
		"Replace the visible label while preserving surrounding behavior.",
	}, "\n"))
}

func cliTestEnvelopeWithDescription(t *testing.T, description string) hook.DispatchEnvelope {
	t.Helper()
	envelope, err := hook.SealSnapshot(hook.TicketSnapshot{
		SchemaVersion: hook.SnapshotSchemaVersion,
		SpaceKey:      "example", ActivityID: 1, ActivityType: 1, ProjectID: 2, ProjectKey: "TICKET",
		IssueID: 3, IssueKey: "TICKET-4", IssueKeyID: 4, CreatorID: 5,
		RunID: "run_20260802_alpha", CreatedAt: time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC),
		Target: hook.DeliveryTarget{RepositoryID: 101, WorkflowRefSHA256: strings.Repeat("a", 64)},
		Untrusted: hook.UntrustedTicketData{
			Summary: "Change one visible label", Description: description,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}

func TestRunParseTicketAndSnapshot(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.json")
	envelopePath := filepath.Join(directory, "envelope.json")
	ticketPath := filepath.Join(directory, "ticket.json")
	sourcePath := filepath.Join(directory, "source.json")
	config := cliTestConfig()
	writeTestJSON(t, configPath, config)
	writeTestJSON(t, envelopePath, cliTestEnvelope(t))

	if err := run(context.Background(), []string{
		"parse-ticket", "--config", configPath, "--tool-sha", cliToolSHA, "--envelope", envelopePath, "--out", ticketPath,
	}); err != nil {
		t.Fatal(err)
	}
	var request worker.TicketRequest
	if err := worker.ReadJSONFile(ticketPath, worker.MaxTicketJSONBytes, &request); err != nil {
		t.Fatal(err)
	}
	repoRoot := filepath.Join(directory, "repo")
	filename := filepath.Join(repoRoot, filepath.FromSlash(request.TargetFiles[0]))
	if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte("export const label = 'Old label';\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runCLITestGit(t, repoRoot, "init", "-q")
	runCLITestGit(t, repoRoot, "add", "--", request.TargetFiles[0])
	runCLITestGit(t, repoRoot, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-qm", "fixture")
	baseSHA := strings.TrimSpace(runCLITestGit(t, repoRoot, "rev-parse", "HEAD"))
	if err := run(context.Background(), []string{
		"snapshot", "--config", configPath, "--tool-sha", cliToolSHA, "--ticket", ticketPath, "--repo-root", repoRoot,
		"--base-sha", baseSHA, "--out", sourcePath,
	}); err != nil {
		t.Fatal(err)
	}
	var source worker.SourceSnapshot
	if err := worker.ReadJSONFile(sourcePath, worker.MaxArtifactJSONBytes, &source); err != nil {
		t.Fatal(err)
	}
	if err := source.Validate(request, config); err != nil {
		t.Fatal(err)
	}
	for _, artifact := range []string{ticketPath, sourcePath} {
		info, err := os.Stat(artifact)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("artifact mode = %v, error = %v", info, err)
		}
	}
}

func TestRunParseTicketDistinguishesTicketRejectionFromInternalFailure(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.json")
	envelopePath := filepath.Join(directory, "envelope.json")
	writeTestJSON(t, configPath, cliTestConfig())
	envelope := cliTestEnvelope(t)
	envelope.Snapshot.Untrusted.Description = "not the fixed ticket contract"
	var err error
	envelope, err = hook.SealSnapshot(envelope.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	writeTestJSON(t, envelopePath, envelope)
	err = run(context.Background(), []string{
		"parse-ticket", "--config", configPath, "--tool-sha", cliToolSHA, "--envelope", envelopePath,
		"--out", filepath.Join(directory, "ticket.json"),
	})
	if err == nil || commandExitCode(err) != 2 {
		t.Fatalf("ticket rejection error=%v exit=%d", err, commandExitCode(err))
	}
	internal := run(context.Background(), []string{
		"parse-ticket", "--config", filepath.Join(directory, "missing.json"), "--tool-sha", cliToolSHA,
		"--envelope", envelopePath, "--out", filepath.Join(directory, "other.json"),
	})
	if internal == nil || commandExitCode(internal) != 1 {
		t.Fatalf("internal error=%v exit=%d", internal, commandExitCode(internal))
	}
}

func TestRunDoesNotEchoRejectedInput(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.json")
	envelopePath := filepath.Join(directory, "envelope.json")
	if err := os.WriteFile(configPath, []byte(`{"secret_model_value":"do-not-echo"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	writeTestJSON(t, envelopePath, cliTestEnvelope(t))
	err := run(context.Background(), []string{
		"parse-ticket", "--config", configPath, "--tool-sha", cliToolSHA, "--envelope", envelopePath, "--out", filepath.Join(directory, "out.json"),
	})
	if err == nil || strings.Contains(err.Error(), "do-not-echo") {
		t.Fatalf("run() error = %v", err)
	}
}

func runCLITestGit(t *testing.T, root string, arguments ...string) string {
	t.Helper()
	// No background maintenance: a detached `git gc --auto` after the
	// fixture commit kept writing under .git while the tests read the tree.
	command := exec.Command("git", append([]string{"-C", root, "-c", "gc.auto=0", "-c", "maintenance.auto=false"}, arguments...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", arguments, err, output)
	}
	return string(output)
}

// copyWorkingTree copies a repository's working tree, leaving .git behind:
// the copy stands in for the base the agent started from, and nothing in it
// is read through git.
func copyWorkingTree(t *testing.T, source, destination string) {
	t.Helper()
	err := filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == ".git" && relative != "." {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(destination, relative), 0o750)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(destination, relative), content, 0o600)
	})
	if err != nil {
		t.Fatalf("base copy failed: %v", err)
	}
}

// A requester who omits the target file must reach the pipeline: the file is
// found from the wording that has to disappear, so nobody has to read the
// repository before filing a ticket.
func TestRunLocatesTheTargetOfATicketThatNamedNoFile(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.json")
	envelopePath := filepath.Join(directory, "envelope.json")
	draftPath := filepath.Join(directory, "draft.json")
	ticketPath := filepath.Join(directory, "ticket.json")
	writeTestJSON(t, configPath, cliTestConfig())
	writeTestJSON(t, envelopePath, cliTestEnvelopeWithDescription(t, strings.Join([]string{
		"Automation-Run-ID: run_20260802_alpha",
		"Automation-Mode: client-visible-change",
		"Verification-Path: /settings",
		"Expected-Text: Updated label",
		"Absent-Text: Old label",
		"---",
		"Please reword the visible label.",
	}, "\n")))

	if err := run(context.Background(), []string{
		"parse-ticket", "--config", configPath, "--tool-sha", cliToolSHA, "--envelope", envelopePath,
		"--draft-out", draftPath, "--out", ticketPath,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ticketPath); err == nil {
		t.Fatal("a contract was completed before the target was located")
	}

	repoRoot := filepath.Join(directory, "repo")
	for name, content := range map[string]string{
		"client/src/Settings.tsx": "export const heading = 'Old label';\n",
		"client/src/Other.tsx":    "export const heading = 'Unrelated';\n",
	} {
		filename := filepath.Join(repoRoot, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := run(context.Background(), []string{
		"locate-target", "--config", configPath, "--tool-sha", cliToolSHA, "--draft", draftPath,
		"--repo-root", repoRoot, "--out", ticketPath,
	}); err != nil {
		t.Fatal(err)
	}
	var request worker.TicketRequest
	if err := worker.ReadJSONFile(ticketPath, worker.MaxTicketJSONBytes, &request); err != nil {
		t.Fatal(err)
	}
	if len(request.TargetFiles) != 1 || request.TargetFiles[0] != "client/src/Settings.tsx" {
		t.Fatalf("target files = %v", request.TargetFiles)
	}

	// Wording that is nowhere in the writable scope is a ticket rejection,
	// not an internal failure, so the run reports it as an input problem.
	emptyRoot := filepath.Join(directory, "empty", "client", "src")
	if err := os.MkdirAll(emptyRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(emptyRoot, "None.tsx"), []byte("export const heading = 'Nothing';\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := run(context.Background(), []string{
		"locate-target", "--config", configPath, "--tool-sha", cliToolSHA, "--draft", draftPath,
		"--repo-root", filepath.Join(directory, "empty"), "--out", filepath.Join(directory, "unused.json"),
	})
	if err == nil || commandExitCode(err) != 2 {
		t.Fatalf("err = %v, exit = %d, want a ticket rejection", err, commandExitCode(err))
	}
}
