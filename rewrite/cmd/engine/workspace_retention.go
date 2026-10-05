package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"ticket-runner/internal/chain"
)

const removalFile = "workspace-removal.json"

// This is garbage-collection progress, not a completion verdict or a model format.
type workspaceRemoval struct {
	Hours       int        `json:"hours"`
	After       time.Time  `json:"after"`
	WorkspaceID string     `json:"workspace_id"`
	Removing    bool       `json:"removing,omitempty"`
	RemovedAt   *time.Time `json:"removed_at,omitempty"`
}

func fileIdentity(info os.FileInfo) string {
	s := info.Sys().(*syscall.Stat_t)
	return fmt.Sprintf("%d:%d", s.Dev, s.Ino)
}

// Device identity alone cannot detect Linux bind mounts. Read the mount identity
// of the opened descriptor, not a pathname that can resolve to a different tree.
func removalMount(file *os.File) (string, error) {
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if runtime.GOOS != "linux" {
		if runtime.GOOS != "darwin" {
			return "", errors.New("workspace removal is unsupported on this platform")
		}
		return fmt.Sprint(info.Sys().(*syscall.Stat_t).Dev), nil
	}
	raw, err := os.ReadFile(fmt.Sprintf("/proc/self/fdinfo/%d", file.Fd()))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "mnt_id:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "mnt_id:")), nil
		}
	}
	return "", errors.New("the workspace mount could not be determined")
}

func removalDirectory(parent *os.Root, name, mount string) (*os.Root, error) {
	before, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.IsDir() {
		return nil, errors.New("workspace removal will not follow a directory link")
	}
	child, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	file, err := child.Open(".")
	if err == nil {
		var info os.FileInfo
		info, err = file.Stat()
		if err == nil && !os.SameFile(before, info) {
			err = errors.New("workspace directory changed during inspection")
		}
		if got, e := removalMount(file); err == nil && (e != nil || got != mount) {
			err = errors.New("workspace removal will not cross a mount")
		}
		file.Close()
	}
	if err != nil {
		child.Close()
		return nil, err
	}
	return child, nil
}

func removalOpen(root *os.Root, name, mount string, flags int) (*os.File, error) {
	if !filepath.IsLocal(name) {
		return nil, errors.New("retained evidence must be inside the workspace")
	}
	parts := strings.Split(filepath.Clean(name), string(filepath.Separator))
	for _, part := range parts[:len(parts)-1] {
		next, err := removalDirectory(root, part, mount)
		if err != nil {
			return nil, err
		}
		defer next.Close()
		root = next
	}
	file, err := root.OpenFile(parts[len(parts)-1], flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err == nil && (!info.Mode().IsRegular() || info.Sys().(*syscall.Stat_t).Nlink != 1 || info.Size() > 64<<20) {
		err = errors.New("workspace evidence is not an ordinary bounded file")
	}
	if got, e := removalMount(file); err == nil && (e != nil || got != mount) {
		err = errors.New("workspace evidence crosses a mount")
	}
	if err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func removalRead(root *os.Root, name, mount string) ([]byte, error) {
	file, err := removalOpen(root, name, mount, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, (64<<20)+1))
}

func reclaimStoppedWorkspace(cfg config, issue sourceIssue, directory string, now time.Time) (bool, error) {
	if cfg.Intake == nil || cfg.Intake.StoppedWorkspaceRetentionHours <= 0 || cfg.Intake.StopReportRole == "" {
		return false, nil
	}
	parent, err := os.OpenRoot(filepath.Dir(directory))
	if err != nil {
		return false, err
	}
	defer parent.Close()
	file, err := parent.Open(".")
	if err != nil {
		return false, err
	}
	mount, err := removalMount(file)
	file.Close()
	if err != nil {
		return false, err
	}
	job, err := removalDirectory(parent, filepath.Base(directory), mount)
	if err != nil {
		return false, err
	}
	defer job.Close()
	for _, name := range []string{"run/runner.lock", "stop-report/runner.lock", ".workspace.prepare.lock"} {
		lock, err := removalOpen(job, name, mount, os.O_RDWR)
		if err != nil {
			return false, err
		}
		defer lock.Close()
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			return false, errors.New("workspace is still owned by a process")
		}
	}
	stopRaw, err := removalRead(job, "stop-request.json", mount)
	if err != nil {
		return false, err
	}
	stop, err := stopInstruction(cfg.source(), []json.RawMessage{stopRaw}, issue, cfg.Intake.StopUserIDs)
	if err != nil || stop == nil {
		return false, errors.New("workspace has no readable authorized stop")
	}
	for _, name := range []string{"run/history.json", "stop-report/history.json"} {
		raw, err := removalRead(job, name, mount)
		if err != nil {
			return false, err
		}
		var state *chain.State
		if json.Unmarshal(raw, &state) != nil || state == nil || name == "stop-report/history.json" && !state.Done {
			return false, errors.New("stopped history is unreadable or stop reporting is incomplete")
		}
	}
	var prepared struct {
		RecordedAt    time.Time       `json:"recorded_at"`
		PreparedAgain *bool           `json:"prepared_again"`
		FoundInPlace  json.RawMessage `json:"found_in_place"`
	}
	raw, err := removalRead(job, ".workspace.prepared", mount)
	if err != nil {
		return false, err
	}
	if json.Unmarshal(raw, &prepared) != nil || prepared.RecordedAt.IsZero() || prepared.PreparedAgain == nil || prepared.FoundInPlace != nil {
		return false, errors.New("the workspace was not recorded as prepared by this launcher")
	}
	var record workspaceRemoval
	raw, err = removalRead(job, removalFile, mount)
	if err == nil {
		if json.Unmarshal(raw, &record) != nil || record.Hours <= 0 || record.After.IsZero() || record.WorkspaceID == "" {
			return false, errors.New("workspace removal progress is unreadable")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if record.RemovedAt != nil {
		return false, nil
	}
	if record.Removing {
		raw, err := removalRead(job, "workspace-evidence.json", mount)
		var evidence map[string][]byte
		if err != nil || json.Unmarshal(raw, &evidence) != nil || evidence == nil {
			return false, errors.New("retained workspace evidence is unreadable; removal is held")
		}
		if _, present := evidence[".git/ticket-engine/delivery.json"]; !present {
			return false, errors.New("retained workspace evidence is incomplete; removal is held")
		}
	}
	info, err := job.Lstat("workspace")
	if errors.Is(err, os.ErrNotExist) && record.Removing {
		record.RemovedAt = &now
		return true, saveWorkspaceRemoval(directory, record)
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, errors.New("workspace is not a real directory")
	}
	identity := fileIdentity(info)
	if record.WorkspaceID != "" && identity != record.WorkspaceID {
		return false, errors.New("workspace was replaced; the new directory will not be discarded")
	}
	if record.WorkspaceID == "" || !record.Removing && record.Hours != cfg.Intake.StoppedWorkspaceRetentionHours {
		record = workspaceRemoval{Hours: cfg.Intake.StoppedWorkspaceRetentionHours, After: now.Add(time.Duration(cfg.Intake.StoppedWorkspaceRetentionHours) * time.Hour), WorkspaceID: identity}
		return false, saveWorkspaceRemoval(directory, record)
	}
	if !record.Removing && now.Before(record.After) {
		return false, nil
	}
	workspace, err := removalDirectory(job, "workspace", mount)
	if err != nil {
		return false, err
	}
	defer workspace.Close()
	if !record.Removing {
		if err := retainWorkspaceEvidence(job, workspace, directory, mount); err != nil {
			return false, err
		}
		record.Removing = true
		if err := saveWorkspaceRemoval(directory, record); err != nil {
			return false, err
		}
	}
	remaining := 512
	if err := removeWorkspaceEntries(workspace, mount, &remaining); err != nil {
		return false, err
	}
	current, err := job.Lstat("workspace")
	if err != nil || fileIdentity(current) != record.WorkspaceID {
		return false, errors.New("workspace changed while being removed")
	}
	if err := job.Remove("workspace"); err != nil {
		return false, err
	}
	record.RemovedAt = &now
	return true, saveWorkspaceRemoval(directory, record)
}

func saveWorkspaceRemoval(directory string, record workspaceRemoval) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return writeRuntimeFile(filepath.Join(directory, removalFile), raw)
}

func retainWorkspaceEvidence(job, workspace *os.Root, directory, mount string) error {
	raw, err := removalRead(job, "engine.json", mount)
	if err != nil {
		return err
	}
	var cfg config
	if json.Unmarshal(raw, &cfg) != nil || len(cfg.Roles) == 0 {
		return errors.New("the saved configuration cannot identify its receipts")
	}
	paths := map[string]bool{".git/ticket-engine/delivery.json": true, "report/result.md": true}
	for _, role := range cfg.Roles {
		for _, process := range role.Processes {
			if process.Receipt == "" {
				continue
			}
			if !filepath.IsAbs(process.Directory) || !filepath.IsLocal(process.Receipt) {
				return errors.New("configured receipt is outside its saved workspace")
			}
			rel, err := filepath.Rel(filepath.Join(directory, "workspace"), filepath.Join(process.Directory, process.Receipt))
			if err != nil || !filepath.IsLocal(rel) {
				return errors.New("configured receipt is outside its saved workspace")
			}
			paths[rel] = true
		}
	}
	evidence := map[string][]byte{}
	var names []string
	for name := range paths {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		data, err := removalRead(workspace, name, mount)
		if errors.Is(err, os.ErrNotExist) {
			evidence[name] = nil
		} else if err != nil {
			return err
		} else {
			evidence[name] = data
		}
	}
	raw, err = json.Marshal(evidence)
	if err != nil {
		return err
	}
	if len(raw) > 64<<20 {
		return errors.New("workspace evidence is too large to retain without omission")
	}
	return writeRuntimeFile(filepath.Join(directory, "workspace-evidence.json"), raw)
}

var errRemovalYield = errors.New("workspace removal will continue on the next tick")

func removeWorkspaceEntries(root *os.Root, mount string, remaining *int) error {
	file, err := root.Open(".")
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Chmod(0700); err != nil {
		return err
	}
	for {
		entries, err := file.ReadDir(32)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		for _, entry := range entries {
			if *remaining <= 0 {
				return errRemovalYield
			}
			*remaining--
			if entry.IsDir() {
				child, err := removalDirectory(root, entry.Name(), mount)
				if err != nil {
					return err
				}
				err = removeWorkspaceEntries(child, mount, remaining)
				child.Close()
				if err != nil {
					return err
				}
			} else if !entry.Type().IsRegular() && entry.Type()&os.ModeSymlink == 0 {
				return errors.New("workspace removal encountered a special file")
			}
			if err := root.Remove(entry.Name()); err != nil {
				return err
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
	}
}
