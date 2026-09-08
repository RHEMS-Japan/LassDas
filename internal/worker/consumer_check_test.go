package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckConsumerNeedsNoModelsAndUsesCredentialFreeValidation(t *testing.T) {
	root, sha := buildAgentRepository(t)
	consumer := validTestConfig().Consumers[0]
	consumer.Mode.Toolchain = nil
	log := filepath.Join(t.TempDir(), "commands.jsonl")
	consumer.Mode.InstallCommand = validationTestCommand(log, "install")
	consumer.Mode.VerifyCommands = [][]string{validationTestCommand(log, "verify")}
	t.Setenv("GITHUB_TOKEN", "trial-secret")
	result, err := CheckConsumer(context.Background(), root, consumer, sha)
	if err != nil {
		t.Fatal(err)
	}
	if result.BaseSHA != sha || len(result.Commands) != 2 || result.StartedAt.IsZero() || result.CompletedAt.Before(result.StartedAt) {
		t.Fatalf("unexpected check: %+v", result)
	}
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var entry struct {
			GitHubRemoved bool `json:"github_removed"`
		}
		if json.Unmarshal([]byte(line), &entry) != nil || !entry.GitHubRemoved {
			t.Fatal("validation inherited a credential")
		}
	}
}

func TestCheckConsumerRejectsWrongBaselineAndTrackedEdits(t *testing.T) {
	root, sha := buildAgentRepository(t)
	consumer := validTestConfig().Consumers[0]
	consumer.Mode.Toolchain = nil
	marker := filepath.Join(t.TempDir(), "ran")
	consumer.Mode.InstallCommand = []string{"touch", marker}
	consumer.Mode.VerifyCommands = [][]string{{"true"}}
	if _, err := CheckConsumer(context.Background(), root, consumer, strings.Repeat("f", 40)); err == nil {
		t.Fatal("wrong base passed")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("validation ran before the base check")
	}
	consumer.Mode.VerifyCommands = [][]string{{"sh", "-c", "printf changed > src/label.ts"}}
	if _, err := CheckConsumer(context.Background(), root, consumer, sha); err == nil || !strings.Contains(err.Error(), "tracked source") {
		t.Fatalf("changed source passed: %v", err)
	}
}

func TestCheckConsumerReportsMissingToolAndFailedCommand(t *testing.T) {
	root, sha := buildAgentRepository(t)
	consumer := validTestConfig().Consumers[0]
	consumer.Mode.Toolchain = []ToolRequirement{{Binary: "absent-trial-tool", Version: "1"}}
	consumer.Mode.InstallCommand = []string{"true"}
	consumer.Mode.VerifyCommands = [][]string{{"false"}}
	if _, err := CheckConsumer(context.Background(), root, consumer, sha); err == nil || !strings.Contains(err.Error(), "absent-trial-tool") {
		t.Fatalf("missing tool not identified: %v", err)
	}
	consumer.Mode.Toolchain = nil
	if _, err := CheckConsumer(context.Background(), root, consumer, sha); err == nil {
		t.Fatal("failed verification passed")
	}
}

func TestObservedGoVersionUsesGoVersionAndChecksRelease(t *testing.T) {
	directory := t.TempDir()
	program := "#!/bin/sh\n[ \"$1\" = version ] || exit 7\nprintf 'go version go1.26.0 linux/arm64\\n'\n"
	if err := os.WriteFile(filepath.Join(directory, "go"), []byte(program), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	env, cleanup, err := createValidationEnvironment(os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	got, err := observedToolVersion(context.Background(), directory, env, "go", "1.26", false)
	if err != nil || got != "1.26.0" {
		t.Fatalf("go observation = %q, %v", got, err)
	}
	if _, err := observedToolVersion(context.Background(), directory, env, "go", "1.25", false); err == nil {
		t.Fatal("different Go release passed")
	}
}
