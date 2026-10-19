package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"ticket-runner/internal/chain"
)

func validateInstallTrim(cfg config) error {
	if cfg.Intake == nil || len(cfg.Intake.TrimInstallDirs) == 0 {
		return nil
	}
	refuse := func(name string) error {
		return fmt.Errorf("intake.trim_install_dirs: %q must be an exact relative regenerable directory, outside records, logs and receipts", name)
	}
	receipts := []string{".git/ticket-engine/delivery.json", "report/result.md"}
	for _, role := range cfg.Roles {
		for _, process := range role.Processes {
			if process.Receipt == "" {
				continue
			}
			receipt := filepath.Join(process.Directory, process.Receipt)
			if filepath.IsAbs(receipt) && process.Env["TASK_WORKSPACE"] != "" {
				var err error
				receipt, err = filepath.Rel(process.Env["TASK_WORKSPACE"], receipt)
				if err != nil {
					return fmt.Errorf("receipt %q cannot be located for workspace cleanup: %w", process.Receipt, err)
				}
			}
			receipts = append(receipts, receipt)
		}
	}
	for _, name := range cfg.Intake.TrimInstallDirs {
		if !filepath.IsLocal(name) || strings.ContainsAny(name, "\\*?[]\x00") {
			return refuse(name)
		}
		for _, part := range strings.Split(name, "/") {
			for _, c := range part {
				if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._@+-", c)) {
					return refuse(name)
				}
			}
			switch strings.ToLower(part) {
			case "", ".", "..", ".git", "logs", "run", "live", "stop-report", "preview-down", "history.json", "task-activity.json", "task-preview.txt":
				return refuse(name)
			}
		}
		for _, receipt := range receipts {
			if installPathsOverlap(name, receipt) {
				return refuse(name)
			}
		}
	}
	return nil
}

func installPathsOverlap(a, b string) bool {
	x, y := strings.Split(a, "/"), strings.Split(b, "/")
	for i := 0; i < len(x) && i < len(y); i++ {
		if !strings.EqualFold(x[i], y[i]) {
			return false
		}
	}
	return true
}

// The chain saves every returned process result before starting another role.
// Its launcher must also terminate detached descendants before returning.
// No history, cleanup marker or alternative execution path is introduced here.
type installTrimExecutor struct {
	next    chain.Executor
	cfg     config
	job     string
	observe func(string)
}

func withInstallTrim(next chain.Executor, cfg config, job string, observe func(string)) chain.Executor {
	if cfg.Intake == nil || len(cfg.Intake.TrimInstallDirs) == 0 {
		return next
	}
	return installTrimExecutor{next: next, cfg: cfg, job: job, observe: observe}
}

func (e installTrimExecutor) Execute(ctx context.Context, next chain.Assignment, state chain.State) []chain.Result {
	if state.Step != "" && state.Step != next.Role && next.Role != e.cfg.Intake.QuestionRole {
		for i, role := range e.cfg.Roles {
			if role.Name != state.Step {
				continue
			}
			for j, process := range role.Processes {
				home := filepath.Join("homes", fmt.Sprintf("%d-%d", i, j))
				// Only the request-bound home is eligible, never an operator's
				// ordinary home or an arbitrary TASK_HOME from a direct run.
				var err error
				if process.Env["TASK_HOME"] != filepath.Join(e.job, home) || process.Env["TASK_WORKSPACE"] != filepath.Join(e.job, "workspace") {
					err = errors.New("the role has no request-bound home")
				} else {
					err = trimInstallPaths(ctx, e.job, home, e.cfg.Intake.TrimInstallDirs)
				}
				if err != nil && e.observe != nil {
					e.observe("regenerable directories not removed for " + role.Name + "/" + process.Name + ": " + err.Error())
				}
			}
		}
	}
	return e.next.Execute(ctx, next, state)
}

func trimInstallPaths(ctx context.Context, directory, base string, names []string) error {
	if len(names) == 0 {
		return nil
	}
	job, mount, err := openInstallRoot(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer job.Close()
	return trimOpenedInstallPaths(ctx, job, base, names, mount, true)
}

func trimFinishedInstalls(ctx context.Context, directory string, names []string) error {
	if len(names) == 0 {
		return nil
	}
	job, mount, err := openInstallRoot(directory)
	if err != nil {
		return err
	}
	defer job.Close()
	workspace, err := removalDirectory(job, "workspace", mount)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer workspace.Close()
	var present []string
	for _, name := range names {
		// Root confines this existence check. Parent links and mounts are
		// checked again component by component before any removal.
		if _, err := workspace.Lstat(name); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		present = append(present, name)
	}
	if len(present) == 0 {
		return nil
	}
	raw, err := removalRead(job, "engine.json", mount)
	if err != nil {
		return fmt.Errorf("reading the saved configuration before workspace cleanup: %w", err)
	}
	saved, err := readConfig(raw)
	if err != nil || len(saved.Roles) == 0 {
		return errors.New("the saved configuration cannot identify its receipts; workspace cleanup is held")
	}
	for _, role := range saved.Roles {
		for _, process := range role.Processes {
			if process.Receipt != "" && process.Env["TASK_WORKSPACE"] != filepath.Join(directory, "workspace") {
				return errors.New("the saved receipt is not bound to this workspace; workspace cleanup is held")
			}
		}
	}
	saved.Intake = &intakeConfig{TrimInstallDirs: names}
	if err := validateInstallTrim(saved); err != nil {
		return fmt.Errorf("protecting the saved request's receipts: %w", err)
	}
	if err := protectTrackedInstallPaths(ctx, workspace, filepath.Join(directory, "workspace"), present); err != nil {
		return err
	}
	return trimOpenedInstallPaths(ctx, job, "workspace", present, mount, false)
}

func protectTrackedInstallPaths(ctx context.Context, workspace *os.Root, directory string, names []string) error {
	// A plain directory must not discover an unrelated ancestor repository.
	if _, err := workspace.Lstat(".git"); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	readGit := func(args ...string) ([]byte, error) {
		command := exec.CommandContext(ctx, "git", append([]string{"-C", directory, "-c", "core.fsmonitor=false"}, args...)...)
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "LC_ALL=C", "GIT_CONFIG_GLOBAL=" + os.DevNull,
			"GIT_CONFIG_SYSTEM=" + os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0"}
		return command.Output()
	}
	top, err := readGit("rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("workspace Git root could not be checked; cleanup is held: %w", err)
	}
	actual, err := os.Stat(strings.TrimSpace(string(top)))
	expected, expectedErr := workspace.Stat(".")
	if err != nil || expectedErr != nil || !os.SameFile(expected, actual) {
		return errors.New("Git root is not this workspace; cleanup is held")
	}
	// Git's icase pathspec does not cover every filesystem case alias.
	// Compare index paths (not their contents) with the same rule as receipts.
	out, err := readGit("ls-files", "--cached", "-z")
	if err != nil {
		return fmt.Errorf("Git-tracked workspace files could not be checked; cleanup is held: %w", err)
	}
	for _, tracked := range strings.Split(string(out), "\x00") {
		for _, name := range names {
			if tracked != "" && installPathsOverlap(name, tracked) {
				return errors.New("a selected workspace directory contains Git-tracked work; cleanup is held")
			}
		}
	}
	return nil
}

func openInstallRoot(directory string) (*os.Root, string, error) {
	parent, err := os.OpenRoot(filepath.Dir(directory))
	if err != nil {
		return nil, "", err
	}
	defer parent.Close()
	file, err := parent.Open(".")
	if err != nil {
		return nil, "", err
	}
	mount, err := removalMount(file)
	file.Close()
	if err != nil {
		return nil, "", err
	}
	job, err := removalDirectory(parent, filepath.Base(directory), mount)
	return job, mount, err
}

func trimOpenedInstallPaths(ctx context.Context, job *os.Root, base string, names []string, mount string, repeat bool) error {
	var failures []error
	remaining, removed := 512, 0
	for _, name := range names {
		for {
			err := removeInstallDirectory(ctx, job, filepath.Join(base, name), mount, &remaining, &removed)
			if errors.Is(err, errRemovalYield) {
				if removed == 0 {
					err = fmt.Errorf("no removal progress within the entry budget; cleanup is held: %w", err)
				} else if repeat {
					remaining = 512
					removed = 0
					continue
				}
				return errors.Join(append(failures, fmt.Errorf("%s/%s: %w", base, name, err))...)
			}
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				failures = append(failures, fmt.Errorf("%s/%s: %w", base, name, err))
			}
			break
		}
	}
	return errors.Join(failures...)
}

func removeInstallDirectory(ctx context.Context, root *os.Root, name, mount string, remaining, removed *int) error {
	parts := strings.Split(name, string(filepath.Separator))
	for _, part := range parts[:len(parts)-1] {
		next, err := removalDirectory(root, part, mount)
		if err != nil {
			return err
		}
		defer next.Close()
		root = next
	}
	leaf := parts[len(parts)-1]
	before, err := root.Lstat(leaf)
	if err != nil {
		return err
	}
	target, err := removalDirectory(root, leaf, mount)
	if err != nil {
		return err
	}
	defer target.Close()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := removeWorkspaceEntries(target, mount, remaining, removed); err != nil {
		return err
	}
	return removeInstallTarget(root, leaf, before)
}

func removeInstallTarget(root *os.Root, leaf string, before os.FileInfo) error {
	after, err := root.Lstat(leaf)
	if err != nil || !os.SameFile(before, after) {
		return errors.New("regenerable directory changed while being removed")
	}
	return root.Remove(leaf)
}
