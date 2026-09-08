package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// ConsumerCheck records an installer's trial on the unchanged destination
// baseline. It deliberately has no delivery identity or publication seal.
type ConsumerCheck struct {
	BaseSHA     string         `json:"base_sha"`
	Tools       []ObservedTool `json:"tools"`
	Commands    [][]string     `json:"commands"`
	StartedAt   time.Time      `json:"started_at"`
	CompletedAt time.Time      `json:"completed_at"`
}

// CheckConsumer checks one consumer before any model or agent is configured.
// The installer invokes it inside the same image used for deliveries, in a
// disposable clone with no credentials. Validation shares the normal command,
// tool-version, environment, and timeout rules, but cannot authorize a PR.
func CheckConsumer(ctx context.Context, repoRoot string, consumer ConsumerConfig, baseSHA string) (ConsumerCheck, error) {
	if ctx == nil || !commitPattern.MatchString(baseSHA) {
		return ConsumerCheck{}, errors.New("consumer check identity is invalid")
	}
	if err := consumer.validate(); err != nil {
		return ConsumerCheck{}, fmt.Errorf("consumer check configuration: %w", err)
	}
	directory, err := validationWorkingDirectory(repoRoot, consumer.Mode.VerifyWorkingDirectory)
	if err != nil {
		return ConsumerCheck{}, errors.New("consumer check working directory is invalid")
	}
	environment, cleanup, err := createValidationEnvironment(os.Environ())
	if err != nil {
		return ConsumerCheck{}, err
	}
	defer cleanup()
	if err := verifyGitCheckout(ctx, repoRoot, baseSHA, nil, environment); err != nil {
		return ConsumerCheck{}, err
	}
	result := ConsumerCheck{BaseSHA: baseSHA, Commands: expectedValidationCommands(consumer), StartedAt: time.Now().UTC(), Tools: []ObservedTool{}}
	for _, tool := range consumer.Mode.Toolchain {
		version, err := observedToolVersion(ctx, directory, environment, tool.Binary, tool.Version, tool.StripVPrefix)
		if err != nil {
			return ConsumerCheck{}, fmt.Errorf("consumer check tool %s: %w", tool.Binary, err)
		}
		result.Tools = append(result.Tools, ObservedTool{Binary: tool.Binary, Version: version})
	}
	if err := runValidationCommands(ctx, directory, consumer, ValidationCommandTimeout, environment); err != nil {
		return ConsumerCheck{}, err
	}
	head, err := runGitSourceCommand(ctx, repoRoot, environment, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil || strings.TrimSpace(string(head)) != baseSHA {
		return ConsumerCheck{}, errors.New("consumer check changed the baseline commit")
	}
	// Install/build residue is allowed in this disposable clone, but tracked
	// source (including lockfiles and the index) must still be the baseline.
	if _, err := runGitSourceCommand(ctx, repoRoot, environment, "diff", "--quiet", "HEAD", "--"); err != nil {
		return ConsumerCheck{}, errors.New("consumer check changed tracked source")
	}
	result.CompletedAt = time.Now().UTC()
	return result, nil
}
