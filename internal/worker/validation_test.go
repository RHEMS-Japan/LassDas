package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidationHelperProcess(t *testing.T) {
	logPath, label, ok := validationHelperArguments(os.Args)
	if !ok {
		return
	}
	if label == "spawn-background" {
		command := exec.Command("sh", "-c", `sleep 1; touch "$1"`, "validation-child", logPath)
		command.Stdin = nil
		command.Stdout = io.Discard
		command.Stderr = io.Discard
		if err := command.Start(); err != nil {
			os.Exit(24)
		}
		os.Exit(0)
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		os.Exit(20)
	}
	entry := struct {
		WorkingDirectory   string `json:"working_directory"`
		AWSRemoved         bool   `json:"aws_removed"`
		GitHubRemoved      bool   `json:"github_removed"`
		AppRemoved         bool   `json:"app_removed"`
		PullRemoved        bool   `json:"pull_removed"`
		IngressPullRemoved bool   `json:"ingress_pull_removed"`
		GenericRemoved     bool   `json:"generic_removed"`
		WebhookRemoved     bool   `json:"webhook_removed"`
		FreshHome          bool   `json:"fresh_home"`
	}{
		WorkingDirectory:   workingDirectory,
		AWSRemoved:         os.Getenv("AWS_ACCESS_KEY_ID") == "",
		GitHubRemoved:      os.Getenv("GITHUB_TOKEN") == "",
		AppRemoved:         os.Getenv("APP_PRIVATE_KEY") == "",
		PullRemoved:        os.Getenv("PULL_HMAC_KEY") == "",
		IngressPullRemoved: os.Getenv("TICKET_INGRESS_PULL_HMAC_KEY") == "",
		GenericRemoved:     os.Getenv("BACKLOG_API_KEY") == "",
		WebhookRemoved:     os.Getenv("SLACK_WEBHOOK_URL") == "",
		FreshHome:          os.Getenv("HOME") != "" && !strings.Contains(os.Getenv("HOME"), "original-home"),
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		os.Exit(21)
	}
	file, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		os.Exit(22)
	}
	_, writeErr := file.Write(append(encoded, '\n'))
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		os.Exit(23)
	}
	os.Exit(0)
}

func validationHelperArguments(arguments []string) (string, string, bool) {
	for index, argument := range arguments {
		if argument == "validation-helper" && index+2 < len(arguments) {
			return arguments[index+1], arguments[index+2], true
		}
	}
	return "", "", false
}

func validationTestCommand(logPath, label string) []string {
	return []string{os.Args[0], "-test.run=TestValidationHelperProcess", "--", "validation-helper", logPath, label}
}

func TestRunValidationUsesFixedWorkingDirectoryAndSanitizedEnvironment(t *testing.T) {
	config := validTestConfig()
	logPath := filepath.Join(t.TempDir(), "validation.jsonl")
	config.Consumers[0].Mode.InstallCommand = validationTestCommand(logPath, "install")
	config.Consumers[0].Mode.VerifyCommands = [][]string{validationTestCommand(logPath, "typecheck"), validationTestCommand(logPath, "build")}
	root := t.TempDir()
	workingDirectory := filepath.Join(root, config.Consumers[0].Mode.VerifyWorkingDirectory)
	if err := os.MkdirAll(workingDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	resolvedWorkingDirectory, err := filepath.EvalSymlinks(workingDirectory)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", "original-home")
	t.Setenv("AWS_ACCESS_KEY_ID", "sensitive")
	t.Setenv("GITHUB_TOKEN", "sensitive")
	t.Setenv("APP_PRIVATE_KEY", "sensitive")
	t.Setenv("PULL_HMAC_KEY", "sensitive")
	t.Setenv("TICKET_INGRESS_PULL_HMAC_KEY", "sensitive")
	t.Setenv("BACKLOG_API_KEY", "sensitive")
	t.Setenv("SLACK_WEBHOOK_URL", "sensitive")
	if err := runValidationWithTimeout(context.Background(), root, config.Consumers[0], ValidationCommandTimeout); err != nil {
		t.Fatal(err)
	}
	encoded, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(encoded)), "\n")
	if len(lines) != 3 {
		t.Fatalf("command count = %d", len(lines))
	}
	for _, line := range lines {
		var entry struct {
			WorkingDirectory   string `json:"working_directory"`
			AWSRemoved         bool   `json:"aws_removed"`
			GitHubRemoved      bool   `json:"github_removed"`
			AppRemoved         bool   `json:"app_removed"`
			PullRemoved        bool   `json:"pull_removed"`
			IngressPullRemoved bool   `json:"ingress_pull_removed"`
			GenericRemoved     bool   `json:"generic_removed"`
			WebhookRemoved     bool   `json:"webhook_removed"`
			FreshHome          bool   `json:"fresh_home"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		if entry.WorkingDirectory != resolvedWorkingDirectory || !entry.AWSRemoved || !entry.GitHubRemoved || !entry.AppRemoved || !entry.PullRemoved || !entry.IngressPullRemoved || !entry.GenericRemoved || !entry.WebhookRemoved || !entry.FreshHome {
			t.Fatalf("validation child contract = %+v", entry)
		}
	}
}

func TestRunValidationRejectsSymlinkWorkingDirectory(t *testing.T) {
	config := validTestConfig()
	logPath := filepath.Join(t.TempDir(), "unused.jsonl")
	config.Consumers[0].Mode.InstallCommand = validationTestCommand(logPath, "install")
	config.Consumers[0].Mode.VerifyCommands = [][]string{validationTestCommand(logPath, "build")}
	root := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(root, config.Consumers[0].Mode.VerifyWorkingDirectory)); err != nil {
		t.Fatal(err)
	}
	if err := runValidationWithTimeout(context.Background(), root, config.Consumers[0], ValidationCommandTimeout); err == nil {
		t.Fatal("runValidationWithTimeout() accepted a symlink working directory")
	}
}

// A live validation failure used to be a two-word epitaph. The error must
// name the failing command and carry the tail of what it printed, or every
// CI-only failure becomes an archaeology project.
func TestRunValidationReportsTheFailingCommandAndOutput(t *testing.T) {
	config := validTestConfig()
	config.Consumers[0].Mode.InstallCommand = []string{"sh", "-c", "echo compiling; echo boom-detail >&2; exit 1"}
	config.Consumers[0].Mode.VerifyCommands = [][]string{{"true"}}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, config.Consumers[0].Mode.VerifyWorkingDirectory), 0o700); err != nil {
		t.Fatal(err)
	}
	err := runValidationWithTimeout(context.Background(), root, config.Consumers[0], ValidationCommandTimeout)
	if err == nil {
		t.Fatal("runValidationWithTimeout() accepted a failing command")
	}
	var failure *ValidationCommandError
	if !errors.As(err, &failure) {
		t.Fatalf("failure carries no command detail: %v", err)
	}
	if len(failure.Arguments) != 3 || failure.Arguments[0] != "sh" {
		t.Fatalf("failing command = %v", failure.Arguments)
	}
	tail := string(failure.Tail)
	if !strings.Contains(tail, "compiling") || !strings.Contains(tail, "boom-detail") {
		t.Fatalf("output tail = %q", tail)
	}
}

func TestRunValidationHonorsPerCommandTimeout(t *testing.T) {
	config := validTestConfig()
	config.Consumers[0].Mode.InstallCommand = []string{"sleep", "5"}
	config.Consumers[0].Mode.VerifyCommands = [][]string{{"sleep", "5"}}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, config.Consumers[0].Mode.VerifyWorkingDirectory), 0o700); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	err := runValidationWithTimeout(context.Background(), root, config.Consumers[0], 25*time.Millisecond)
	if err == nil || time.Since(started) > 2*time.Second {
		t.Fatalf("timeout error = %v, elapsed = %v", err, time.Since(started))
	}
}

func TestCredentialFreeCommandTerminatesBackgroundProcessGroup(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "background-finished")
	environment, cleanup, err := createValidationEnvironment(os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	_, err = runCredentialFreeCommand(
		context.Background(), t.TempDir(), environment, 2*time.Second, 0,
		validationTestCommand(marker, "spawn-background"),
	)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("background process survived validation command: %v", err)
	}
}
