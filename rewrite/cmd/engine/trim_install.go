package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
	previous := state.Step
	if previous == e.cfg.Intake.QuestionRole {
		previous = ""
	history:
		for i := len(state.History) - 1; i >= 0; i-- {
			row := state.History[i]
			if row.Role == e.cfg.Intake.QuestionRole || row.Speaker == "requester" || row.Speaker == "runtime" || row.Speaker == "system" {
				continue
			}
			for _, role := range e.cfg.Roles {
				if role.Name == row.Role {
					for _, process := range role.Processes {
						if process.Name == row.Speaker {
							previous = row.Role
							break history
						}
					}
				}
			}
		}
	}
	if previous != "" && previous != next.Role && next.Role != e.cfg.Intake.QuestionRole {
		for i, role := range e.cfg.Roles {
			if role.Name != previous {
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
					e.observe("regenerable directories not removed for " + role.Name + "/" + process.Name + ": " + strings.ReplaceAll(err.Error(), "workspace removal", "cleanup"))
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
	return trimOpenedInstallPaths(ctx, job, base, names, mount)
}

type installTrimMemory struct {
	list string
	held map[string]string
}

func trimFinishedInstalls(ctx context.Context, directory string, names []string, memory *installTrimMemory) error {
	if len(names) == 0 {
		memory.list, memory.held = "", nil
		return nil
	}
	ordered := append([]string(nil), names...)
	sort.Strings(ordered)
	key := strings.Join(ordered, "\x00")
	if memory.list != key || memory.held == nil {
		memory.list, memory.held = key, map[string]string{}
	}
	var failures []error
	var pending []string
	for _, name := range names {
		if reason := memory.held[name]; reason != "" {
			failures = append(failures, errors.New(reason))
		} else {
			pending = append(pending, name)
		}
	}
	if len(pending) == 0 {
		return errors.Join(failures...)
	}
	hold := func(name string, err error) {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			failures = append(failures, err)
			return
		}
		reason := fmt.Sprintf("workspace/%s: %v; held until restart or listed paths change", name, err)
		memory.held[name] = reason
		failures = append(failures, errors.New(reason))
	}
	holdAll := func(err error) error {
		for _, name := range pending {
			hold(name, err)
		}
		return errors.Join(failures...)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	job, mount, err := openInstallRoot(directory)
	if err != nil {
		return holdAll(err)
	}
	defer job.Close()
	workspace, err := removalDirectory(job, "workspace", mount)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return holdAll(err)
	}
	defer workspace.Close()
	var present []string
	for _, name := range pending {
		// Root confines this existence check. Parent links and mounts are
		// checked again component by component before any removal.
		if _, err := workspace.Lstat(name); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			hold(name, err)
			continue
		}
		present = append(present, name)
	}
	if len(present) == 0 {
		return errors.Join(failures...)
	}
	pending = present
	raw, err := removalRead(job, "engine.json", mount)
	if err != nil {
		return holdAll(fmt.Errorf("reading the saved configuration before workspace cleanup: %w", err))
	}
	saved, err := readConfig(raw)
	if err != nil || len(saved.Roles) == 0 {
		return holdAll(errors.New("the saved configuration cannot identify its receipts"))
	}
	for _, role := range saved.Roles {
		for _, process := range role.Processes {
			if process.Receipt != "" && process.Env["TASK_WORKSPACE"] != filepath.Join(directory, "workspace") {
				return holdAll(errors.New("the saved receipt is not bound to this workspace"))
			}
		}
	}
	tracked, err := installTrackedPaths(ctx, workspace, filepath.Join(directory, "workspace"))
	if err != nil {
		return holdAll(err)
	}
	remaining := 512
	for _, name := range present {
		if err := ctx.Err(); err != nil {
			return err
		}
		saved.Intake = &intakeConfig{TrimInstallDirs: []string{name}}
		if err := validateInstallTrim(saved); err != nil {
			hold(name, fmt.Errorf("protecting the saved request's receipts: %w", err))
			continue
		}
		trackedOverlap := false
		for _, path := range tracked {
			trackedOverlap = trackedOverlap || installPathsOverlap(name, path)
		}
		if trackedOverlap {
			hold(name, errors.New("a selected workspace directory contains Git-tracked work"))
			continue
		}
		if remaining <= 0 {
			failures = append(failures, errRemovalYield)
			break // unvisited names are not held; they get the next tick's budget
		}
		removed := 0
		budget := remaining
		err := removeInstallDirectory(ctx, job, filepath.Join("workspace", name), mount, &remaining, &removed)
		if errors.Is(err, errRemovalYield) {
			if removed == 0 && budget == 512 {
				hold(name, errors.New("no removal progress within the entry budget"))
			} else {
				failures = append(failures, err)
			}
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			hold(name, err)
		}
	}
	return errors.Join(failures...)
}

func installTrackedPaths(ctx context.Context, workspace *os.Root, directory string) ([]string, error) {
	// A plain directory must not discover an unrelated ancestor repository.
	if _, err := workspace.Lstat(".git"); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	gitContext, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	readGit := func(args ...string) ([]byte, error) {
		command := exec.CommandContext(gitContext, "git", append([]string{"-C", directory, "-c", "core.fsmonitor=false"}, args...)...)
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "LC_ALL=C", "GIT_CONFIG_GLOBAL=" + os.DevNull,
			"GIT_CONFIG_SYSTEM=" + os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0"}
		out, err := command.Output()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if gitContext.Err() != nil {
			return nil, errors.New("Git check timed out after 20 seconds")
		}
		return out, err
	}
	top, err := readGit("rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("workspace Git root could not be checked: %w", err)
	}
	actual, err := os.Stat(strings.TrimSpace(string(top)))
	expected, expectedErr := workspace.Stat(".")
	if err != nil || expectedErr != nil || !os.SameFile(expected, actual) {
		return nil, errors.New("Git root is not this workspace")
	}
	// Git's icase pathspec does not cover every filesystem case alias.
	// Compare index paths (not their contents) with the same rule as receipts.
	out, err := readGit("ls-files", "--cached", "-z")
	if err != nil {
		return nil, fmt.Errorf("Git-tracked workspace files could not be checked: %w", err)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00"), nil
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

func trimOpenedInstallPaths(ctx context.Context, job *os.Root, base string, names []string, mount string) error {
	var failures []error
	remaining, removed := 512, 0
	for _, name := range names {
		for {
			err := removeInstallDirectory(ctx, job, filepath.Join(base, name), mount, &remaining, &removed)
			if errors.Is(err, errRemovalYield) {
				if removed == 0 {
					failures = append(failures, fmt.Errorf("%s/%s: no removal progress within the entry budget; cleanup is held", base, name))
					remaining, removed = 512, 0
					break
				}
				remaining, removed = 512, 0
				continue
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
