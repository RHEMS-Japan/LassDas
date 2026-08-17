package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const GitSourceCommandTimeout = 30 * time.Second

// ReadVerifiedSourceSnapshot binds source bytes to a clean checkout whose HEAD
// and exact regular Git blobs match baseSHA. It runs only fixed local Git argv
// and must be used by the production snapshot CLI.
func ReadVerifiedSourceSnapshot(ctx context.Context, repoRoot, baseSHA string, request TicketRequest, config Config) (SourceSnapshot, error) {
	if ctx == nil || request.Validate(config) != nil || !commitPattern.MatchString(baseSHA) {
		return SourceSnapshot{}, errors.New("verified source request is invalid")
	}
	root, err := filepath.Abs(repoRoot)
	if err != nil || filepath.Clean(root) != root {
		return SourceSnapshot{}, errors.New("verified source root is invalid")
	}
	isolation, err := os.MkdirTemp("", "ticket-source-git-")
	if err != nil {
		return SourceSnapshot{}, errors.New("verified source environment could not be created")
	}
	defer os.RemoveAll(isolation)
	environment, err := sourceGitEnvironment(os.Environ(), isolation)
	if err != nil {
		return SourceSnapshot{}, errors.New("verified source environment is invalid")
	}
	if err := verifyGitCheckout(ctx, root, baseSHA, nil, environment); err != nil {
		return SourceSnapshot{}, err
	}
	snapshot, err := ReadSourceSnapshot(root, baseSHA, request, config)
	if err != nil {
		return SourceSnapshot{}, err
	}
	if err := verifyGitCheckout(ctx, root, baseSHA, snapshot.Files, environment); err != nil {
		return SourceSnapshot{}, err
	}
	return snapshot, nil
}

func verifyGitCheckout(ctx context.Context, root, baseSHA string, files []SourceFile, environment []string) error {
	head, err := runGitSourceCommand(ctx, root, environment, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil || strings.TrimSpace(string(head)) != baseSHA {
		return errors.New("source checkout HEAD does not match base SHA")
	}
	status, err := runGitSourceCommand(ctx, root, environment, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil || len(status) != 0 {
		return errors.New("source checkout is not clean")
	}
	for _, file := range files {
		output, err := runGitSourceCommand(ctx, root, environment, "ls-tree", "-z", baseSHA, "--", file.Path)
		expected := fmt.Sprintf("100644 blob %s\t%s%c", file.GitBlobSHA, file.Path, byte(0))
		if err != nil || string(output) != expected {
			return errors.New("source Git blob does not match snapshot")
		}
	}
	return nil
}

func runGitSourceCommand(ctx context.Context, root string, environment []string, arguments ...string) ([]byte, error) {
	fixed := []string{"git", "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "--literal-pathspecs", "-C", root}
	fixed = append(fixed, arguments...)
	return runCredentialFreeCommand(ctx, root, environment, GitSourceCommandTimeout, 1024*1024, fixed)
}

func sourceGitEnvironment(host []string, isolation string) ([]string, error) {
	pathValue := selectedEnvironmentValue(host, "PATH")
	if pathValue == "" {
		return nil, errors.New("PATH is unavailable")
	}
	return []string{
		"PATH=" + pathValue,
		"HOME=" + isolation,
		"LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_TERMINAL_PROMPT=0",
	}, nil
}
