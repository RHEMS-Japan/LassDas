package worker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const ValidationCommandTimeout = 10 * time.Minute

func runValidationWithTimeout(ctx context.Context, repoRoot string, consumer ConsumerConfig, commandTimeout time.Duration) error {
	if ctx == nil || commandTimeout <= 0 || consumer.validate() != nil {
		return errors.New("validation contract is invalid")
	}
	workingDirectory, err := validationWorkingDirectory(repoRoot, consumer.Mode.VerifyWorkingDirectory)
	if err != nil {
		return errors.New("validation working directory is invalid")
	}
	environment, cleanup, err := createValidationEnvironment(os.Environ())
	if err != nil {
		return errors.New("validation environment is invalid")
	}
	defer cleanup()
	return runValidationCommands(ctx, workingDirectory, consumer, commandTimeout, environment)
}

func runValidationCommands(ctx context.Context, workingDirectory string, consumer ConsumerConfig, commandTimeout time.Duration, environment []string) error {
	commands := make([][]string, 0, 1+len(consumer.Mode.VerifyCommands))
	commands = append(commands, consumer.Mode.InstallCommand)
	commands = append(commands, consumer.Mode.VerifyCommands...)
	for _, arguments := range commands {
		if !validCommand(arguments) {
			return errors.New("validation command is invalid")
		}
		if err := runReportedValidationCommand(ctx, workingDirectory, environment, commandTimeout, arguments); err != nil {
			return err
		}
	}
	return nil
}

// ValidationCommandError names the fixed command that failed and keeps the
// tail of what it printed. The text is untrusted build output: it belongs in
// the job log for diagnosis and never in a comment to the requester. Without
// it, a live validation failure is a two-word epitaph (observed 2026-08-08:
// a converged candidate died in CI with no way to tell which command or why).
type ValidationCommandError struct {
	Arguments []string
	Tail      []byte
}

func (e *ValidationCommandError) Error() string { return "validation command failed" }

const validationOutputTailBytes = 4096

// outputTail keeps the last bytes written, because build and test runners put
// their verdicts at the end. It never fails a write, so output volume cannot
// change a command's outcome.
type outputTail struct {
	limit int
	data  []byte
}

func (t *outputTail) Write(value []byte) (int, error) {
	t.data = append(t.data, value...)
	if len(t.data) > t.limit {
		t.data = append([]byte(nil), t.data[len(t.data)-t.limit:]...)
	}
	return len(value), nil
}

// runReportedValidationCommand mirrors runCredentialFreeCommand's process
// discipline but captures a bounded tail of stdout and stderr, so a failure
// carries what the command actually said.
func runReportedValidationCommand(ctx context.Context, directory string, environment []string, timeout time.Duration, arguments []string) error {
	commandContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(commandContext, arguments[0], arguments[1:]...)
	command.Dir = directory
	command.Env = append([]string(nil), environment...)
	command.Stdin = nil
	tail := &outputTail{limit: validationOutputTailBytes}
	command.Stdout = tail
	command.Stderr = tail
	configureProcessGroup(command)
	err := command.Run()
	terminateProcessGroup(command)
	if err != nil || commandContext.Err() != nil {
		return &ValidationCommandError{
			Arguments: append([]string(nil), arguments...),
			Tail:      append([]byte(nil), tail.data...),
		}
	}
	return nil
}

func validationWorkingDirectory(repoRoot, relative string) (string, error) {
	root, err := filepath.Abs(repoRoot)
	if err != nil || filepath.Clean(root) != root {
		return "", errors.New("repository root is invalid")
	}
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("repository root is invalid")
	}
	current := root
	for _, component := range strings.Split(filepath.FromSlash(relative), string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("working directory is invalid")
		}
	}
	resolvedRelative, err := filepath.Rel(root, current)
	if err != nil || resolvedRelative != filepath.FromSlash(relative) {
		return "", errors.New("working directory escaped repository")
	}
	return current, nil
}

func createValidationEnvironment(host []string) ([]string, func(), error) {
	isolation, err := os.MkdirTemp("", "ticket-validation-")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(isolation) }
	for _, directory := range []string{"home", "tmp", "config", "cache"} {
		if err := os.Mkdir(filepath.Join(isolation, directory), 0o700); err != nil {
			cleanup()
			return nil, nil, err
		}
	}
	pathValue := selectedEnvironmentValue(host, "PATH")
	if pathValue == "" {
		cleanup()
		return nil, nil, errors.New("PATH is unavailable")
	}
	environment := []string{
		"PATH=" + pathValue,
		"HOME=" + filepath.Join(isolation, "home"),
		"TMPDIR=" + filepath.Join(isolation, "tmp"),
		"XDG_CONFIG_HOME=" + filepath.Join(isolation, "config"),
		"XDG_CACHE_HOME=" + filepath.Join(isolation, "cache"),
		"LC_ALL=C",
		"CI=true",
		"NO_COLOR=1",
	}
	return environment, cleanup, nil
}

func selectedEnvironmentValue(environment []string, selected string) string {
	value := ""
	for _, entry := range environment {
		name, candidate, found := strings.Cut(entry, "=")
		if found && name == selected && candidate != "" {
			value = candidate
		}
	}
	return value
}
