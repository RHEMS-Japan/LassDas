package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
			directory := process.Directory
			if filepath.IsAbs(directory) {
				var err error
				directory, err = filepath.Rel(process.Env["TASK_WORKSPACE"], directory)
				if err != nil || !filepath.IsLocal(directory) {
					return refuse(process.Receipt)
				}
			}
			if !filepath.IsLocal(process.Receipt) {
				return refuse(process.Receipt)
			}
			receipts = append(receipts, filepath.Join(directory, process.Receipt))
		}
	}
	for _, name := range cfg.Intake.TrimInstallDirs {
		if !filepath.IsLocal(name) || strings.ContainsAny(name, "\\*?[]\x00") {
			return refuse(name)
		}
		for _, part := range strings.Split(name, "/") {
			switch strings.ToLower(part) {
			case "", ".", "..", ".git", "logs", "run", "live", "stop-report", "preview-down", "history.json", "task-activity.json", "task-preview.txt":
				return refuse(name)
			}
		}
		folded := strings.ToLower(name)
		for _, receipt := range receipts {
			receipt = strings.ToLower(receipt)
			if folded == receipt || strings.HasPrefix(receipt, folded+"/") || strings.HasPrefix(folded, receipt+"/") {
				return refuse(name)
			}
		}
	}
	return nil
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

func (e installTrimExecutor) Execute(ctx context.Context, next chain.Assignment, state chain.State) []chain.Result {
	if state.Step != "" && state.Step != next.Role {
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

func trimFinishedInstalls(ctx context.Context, directory string, names []string) error {
	job, mount, err := openInstallRoot(directory)
	if err != nil {
		return err
	}
	defer job.Close()
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
			if process.Receipt != "" && (!filepath.IsAbs(process.Directory) || process.Env["TASK_WORKSPACE"] != filepath.Join(directory, "workspace")) {
				return errors.New("the saved receipt is not bound to this workspace; workspace cleanup is held")
			}
		}
	}
	saved.Intake = &intakeConfig{TrimInstallDirs: names}
	if err := validateInstallTrim(saved); err != nil {
		return fmt.Errorf("protecting the saved request's receipts: %w", err)
	}
	return trimOpenedInstallPaths(ctx, job, "workspace", names, mount)
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
	for _, name := range names {
		if err := removeInstallDirectory(ctx, job, filepath.Join(base, name), mount); err != nil && !errors.Is(err, os.ErrNotExist) {
			failures = append(failures, fmt.Errorf("%s/%s: %w", base, name, err))
		}
	}
	return errors.Join(failures...)
}

func removeInstallDirectory(ctx context.Context, root *os.Root, name, mount string) error {
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
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		remaining := 512
		err := removeWorkspaceEntries(target, mount, &remaining)
		if errors.Is(err, errRemovalYield) {
			continue
		}
		if err != nil {
			return err
		}
		break
	}
	after, err := root.Lstat(leaf)
	if err != nil || !os.SameFile(before, after) {
		return errors.New("regenerable directory changed while being removed")
	}
	return root.Remove(leaf)
}
