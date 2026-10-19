package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"ticket-runner/internal/chain"
)

func installTrimConfig(t *testing.T, cfg config, names ...string) config {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if err := json.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	values["intake"].(map[string]any)["trim_install_dirs"] = names
	raw, err = json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	got, err := readConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func installTrimFile(t *testing.T, path, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}

func saveInstallTrimJobConfig(t *testing.T, cfg config, directory string) {
	t.Helper()
	bound, err := bindRequestConfig(cfg, directory, "EXAMPLE-51")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(bound)
	if err != nil {
		t.Fatal(err)
	}
	installTrimFile(t, filepath.Join(directory, "engine.json"), string(raw))
}

// Real processes revisit both roles. Each command prepares from its own
// retained download, even after an earlier role failed. The same-role retry
// must keep its installation; a different role must see both previous homes
// reclaimed, but never the workspace installation or the request's records.
func TestInstallTrimRoleTransitionsAndPreparation(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			cfg := watchConfiguration(t)
			job := t.TempDir()
			first := `set -eu
cat >/dev/null
n=0; test ! -f "$TASK_HOME/count" || n=$(cat "$TASK_HOME/count")
n=$((n+1)); echo "$n" > "$TASK_HOME/count"
if [ "$n" = 2 ]; then test -f "$TASK_HOME/client/node_modules/package"; fi
if [ "$n" = 3 ] && [ "$TRIM_ON" = true ]; then test ! -e "$TASK_HOME/client/node_modules"; fi
mkdir -p "$TASK_HOME/client/node_modules"
cp "$TASK_HOME/download/package" "$TASK_HOME/client/node_modules/package"
test "$(cat "$TASK_HOME/client/node_modules/package")" = reusable
printf 'prepared %s\n' "$n"
if [ "$n" = 1 ]; then exit 1; fi`
			second := `set -eu
cat >/dev/null
mkdir -p "$TASK_HOME/client/node_modules"
cp "$TASK_HOME/download/package" "$TASK_HOME/client/node_modules/package"
echo second-returned`
			next := `set -eu
cat >/dev/null
if [ "$TRIM_ON" = true ]; then
 test ! -e "$FIRST_HOME/client/node_modules"
 test ! -e "$SECOND_HOME/client/node_modules"
fi
test -f "$TASK_WORKSPACE/client/node_modules/package"
n=0; test ! -f "$TASK_HOME/count" || n=$(cat "$TASK_HOME/count")
n=$((n+1)); echo "$n" > "$TASK_HOME/count"
if [ "$n" = 2 ] && [ "$TRIM_ON" = true ]; then test ! -e "$TASK_HOME/client/node_modules"; fi
mkdir -p "$TASK_HOME/client/node_modules"
cp "$TASK_HOME/download/package" "$TASK_HOME/client/node_modules/package"
printf 'checked %s\n' "$n"
if [ "$n" = 1 ]; then exit 1; fi`
			cfg.Roles = []chain.Role{
				{Name: "work", Processes: []chain.Process{{Name: "first", Command: []string{"/bin/sh", "-c", first}}, {Name: "second", Command: []string{"/bin/sh", "-c", second}}}},
				{Name: "review", Processes: []chain.Process{{Name: "reviewer", Command: []string{"/bin/sh", "-c", next}}}},
			}
			for i := range cfg.Roles {
				for j := range cfg.Roles[i].Processes {
					cfg.Roles[i].Processes[j].Env = map[string]string{"TRIM_ON": fmt.Sprint(enabled), "FIRST_HOME": filepath.Join(job, "homes/0-0"), "SECOND_HOME": filepath.Join(job, "homes/0-1")}
				}
			}
			if enabled {
				cfg = installTrimConfig(t, cfg, "client/node_modules")
			}
			cfg, err := bindRequestConfig(cfg, job, "EXAMPLE-7")
			if err != nil {
				t.Fatal(err)
			}
			kept := map[string]string{"request.txt": "Preserve the original request", "workspace/client/node_modules/package": "workspace must remain", "workspace/source.txt": "source", "workspace/.git/ticket-engine/delivery.json": "receipt"}
			for _, home := range []string{"0-0", "0-1", "1-0"} {
				kept["homes/"+home+"/download/package"] = "reusable"
				kept["homes/"+home+"/logs/session.txt"] = "original log"
			}
			for name, value := range kept {
				installTrimFile(t, filepath.Join(job, name), value)
			}
			choices, call := []string{"work", "work", "review", "work", "review", "done"}, 0
			useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
				if call >= len(choices) {
					return nil, errors.New("unexpected extra choice")
				}
				choice := choices[call]
				call++
				return selectionReply(r, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": choice}}}), nil
			})
			raw, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			installTrimFile(t, filepath.Join(job, "engine.json"), string(raw))
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var output, log bytes.Buffer
			if err := run(ctx, []string{"--config", filepath.Join(job, "engine.json"), "--request", filepath.Join(job, "request.txt"), "--run-dir", filepath.Join(job, "run")}, &output, &log); err != nil {
				t.Fatalf("run: %v\n%s", err, log.String())
			}
			raw, err = os.ReadFile(filepath.Join(job, "run/history.json"))
			if err != nil {
				t.Fatal(err)
			}
			var state chain.State
			if err := json.Unmarshal(raw, &state); err != nil {
				t.Fatal(err)
			}
			if !state.Done || len(state.History) != 8 {
				t.Fatalf("unexpected completion: %+v", state)
			}
			for i, result := range state.History {
				wantFailure := i == 0 || i == 4
				if (result.Error != "") != wantFailure || result.Output == "" {
					t.Errorf("result %d did not prepare or preserve the expected files: %+v", i, result)
				}
			}
			for name, want := range kept {
				got, err := os.ReadFile(filepath.Join(job, name))
				if err != nil || string(got) != want {
					t.Errorf("retained %s: %q, %v", name, got, err)
				}
			}
		})
	}
}

func TestInstallTrimWorkspaceWaitsForPreviewDown(t *testing.T) {
	for _, ending := range []string{"delivered", "stopped"} {
		t.Run(ending, func(t *testing.T) {
			cfg, files := previewDownConfig(t)
			cfg = installTrimConfig(t, cfg, "client/node_modules")
			(&noticeTracker{}).install(t, alwaysChoose("done"))
			var root, directory string
			if ending == "delivered" {
				root = t.TempDir()
				directory = pageUpJob(t, root, 51, time.Now().Add(-time.Hour))
			} else {
				root, directory, _ = stoppedTrimFixture(t, cfg, nil)
				if err := writePreviewRecord(directory, previewRecord{State: "up", Role: "preview_up", StartedAt: time.Now().Add(-time.Hour), URL: previewAddress}); err != nil {
					t.Fatal(err)
				}
			}
			install := filepath.Join(directory, "workspace/client/node_modules/package")
			saveInstallTrimJobConfig(t, cfg, directory)
			installTrimFile(t, install, "still served")
			files.hold(t)
			finish := startStopQueue(t, cfg, root, 10*time.Millisecond, &lockedLog{})
			waitFor(t, func() bool { return files.count(t, "start EXAMPLE-51") == 1 })
			if _, err := os.Stat(install); err != nil {
				t.Fatalf("preview lost installation: %v", err)
			}
			files.let(t)
			waitFor(t, func() bool { _, err := os.Stat(install); return errors.Is(err, os.ErrNotExist) })
			finish()
			if record := pageState(t, directory); record.State != "down" {
				t.Fatalf("cleanup before page down: %+v", record)
			}
		})
	}
}

func TestInstallTrimRejectsUnsafeConfiguration(t *testing.T) {
	for _, name := range []string{"", ".", "../outside", "/outside", "client//node_modules", "client/../source", "**/node_modules", ".git", ".GIT", "client/.git/objects", "logs", "LOGS", "run", "report", "REPORT"} {
		t.Run(name, func(t *testing.T) {
			cfg := installTrimConfig(t, watchConfiguration(t), name)
			cfg.Roles[0].Processes[0].Receipt = "report/result.json"
			_, _, _, _, err := watchSettings(&cfg, t.TempDir())
			if err == nil || !strings.Contains(err.Error(), "trim_install_dirs") {
				t.Fatalf("unsafe path %q accepted: %v", name, err)
			}
		})
	}
}

func TestInstallTrimCaseAliasProtectsRecordsBeforeLaunching(t *testing.T) {
	for _, name := range []string{".GIT", "client/ARTIFACTS", "client/artifacts/RECEIPT.json/child"} {
		t.Run(name, func(t *testing.T) {
			useCatalogTransport(t, alwaysChoose("done"))
			job := t.TempDir()
			cfg := installTrimConfig(t, watchConfiguration(t), name)
			cfg.Roles[0].Processes[0].Directory = "client"
			cfg.Roles[0].Processes[0].Receipt = "artifacts/receipt.json"
			cfg.Roles[0].Processes[0].Command = []string{"/bin/sh", "-c", "touch started"}
			cfg, err := bindRequestConfig(cfg, job, "EXAMPLE-7")
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			installTrimFile(t, filepath.Join(job, "engine.json"), string(raw))
			installTrimFile(t, filepath.Join(job, "request.txt"), "Preserve the records")
			for _, path := range []string{"workspace/.git/metadata", "workspace/client/artifacts/receipt.json"} {
				installTrimFile(t, filepath.Join(job, path), "must remain")
			}
			var output, log bytes.Buffer
			err = run(context.Background(), []string{"--config", filepath.Join(job, "engine.json"), "--request", filepath.Join(job, "request.txt"), "--run-dir", filepath.Join(job, "run")}, &output, &log)
			if err == nil || !strings.Contains(err.Error(), "trim_install_dirs") {
				t.Fatalf("case alias allowed a run: %v", err)
			}
			for _, path := range []string{"workspace/.git/metadata", "workspace/client/artifacts/receipt.json"} {
				if got, err := os.ReadFile(filepath.Join(job, path)); err != nil || string(got) != "must remain" {
					t.Fatalf("record changed: %q %v", got, err)
				}
			}
			if _, err := os.Stat(filepath.Join(job, "workspace/client/started")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid setting launched a process: %v", err)
			}
		})
	}
}

func TestInstallTrimBoundariesDoNotFailTheNextRole(t *testing.T) {
	for _, boundary := range []string{"parent link", "target link", "descendant link", "special file", "unbound home"} {
		t.Run(boundary, func(t *testing.T) {
			job, outside := t.TempDir(), t.TempDir()
			installTrimFile(t, filepath.Join(outside, "keep.txt"), "not owned by this request")
			cfg := installTrimConfig(t, watchConfiguration(t), "client/node_modules")
			cfg.Roles[0].Processes[0].Command = []string{"/bin/sh", "-c", "cat >/dev/null; echo next-role-returned"}
			cfg.Roles = append(cfg.Roles, chain.Role{Name: "review", Processes: cfg.Roles[0].Processes})
			cfg, err := bindRequestConfig(cfg, job, "EXAMPLE-7")
			if err != nil {
				t.Fatal(err)
			}
			install := filepath.Join(job, "homes/0-0/client/node_modules")
			installTrimFile(t, filepath.Join(install, "package"), "regenerable")
			if err := os.MkdirAll(filepath.Join(job, "workspace"), 0700); err != nil {
				t.Fatal(err)
			}
			switch boundary {
			case "parent link", "target link":
				target := install
				if boundary == "parent link" {
					target = filepath.Dir(install)
				}
				if err := os.Rename(target, target+"-kept"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, target); err != nil {
					t.Fatal(err)
				}
			case "descendant link":
				if err := os.Symlink(outside, filepath.Join(install, "linked")); err != nil {
					t.Fatal(err)
				}
			case "special file":
				if err := syscall.Mkfifo(filepath.Join(install, "fifo"), 0600); err != nil {
					t.Fatal(err)
				}
			case "unbound home":
				cfg.Roles[0].Processes[0].Env["TASK_HOME"] = outside
			}
			var observations []string
			executor := installTrimExecutor{next: chain.Processes{Roles: map[string]chain.Role{"review": cfg.Roles[1]}}, cfg: cfg, job: job, observe: func(s string) { observations = append(observations, s) }}
			results := executor.Execute(context.Background(), chain.Assignment{Role: "review"}, chain.State{Step: "implement"})
			if len(results) != 1 || results[0].Error != "" || !strings.Contains(results[0].Output, "next-role-returned") {
				t.Fatalf("cleanup failed the next role: %+v", results)
			}
			got, err := os.ReadFile(filepath.Join(outside, "keep.txt"))
			if err != nil || string(got) != "not owned by this request" {
				t.Fatalf("external files changed: %q %v", got, err)
			}
			if boundary == "descendant link" {
				if _, err := os.Lstat(install); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("selected directory remains: %v", err)
				}
				if len(observations) != 0 {
					t.Fatalf("safe removal failed: %v", observations)
				}
			} else if len(observations) != 1 || !strings.Contains(observations[0], "not removed") {
				t.Fatalf("refusal not observed: %v", observations)
			}
		})
	}
}

// Existing final-home markers must not suppress new workspace cleanup or a
// retry after a boundary refusal. With no setting the same link is untouched
// and does not produce a new observation.
func TestInstallTrimFinishedRetryAndDefaultOff(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			cfg := watchConfiguration(t)
			if enabled {
				cfg = installTrimConfig(t, cfg, "client/node_modules")
			}
			(&noticeTracker{}).install(t, alwaysChoose("done"))
			root := t.TempDir()
			directory := deliveredJob(t, root, 51, time.Now().Add(-time.Minute))
			saveInstallTrimJobConfig(t, cfg, directory)
			installTrimFile(t, filepath.Join(directory, "homes/.trimmed"), "already cleaned")
			outside := t.TempDir()
			installTrimFile(t, filepath.Join(outside, "keep.txt"), "outside")
			target := filepath.Join(directory, "workspace/client/node_modules")
			if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, target); err != nil {
				t.Fatal(err)
			}
			var log trimTestLog
			finish := startStopQueue(t, cfg, root, 10*time.Millisecond, &log)
			if enabled {
				waitFor(t, func() bool { return strings.Contains(log.String(), "finished caches not removed") })
				if err := os.Remove(target); err != nil {
					t.Fatal(err)
				}
				installTrimFile(t, filepath.Join(target, "package"), "recreated")
				waitFor(t, func() bool { _, err := os.Lstat(target); return errors.Is(err, os.ErrNotExist) })
			} else {
				waitTicks()
				if info, err := os.Lstat(target); err != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Fatalf("off changed target: %v", err)
				}
				if strings.Contains(log.String(), "not removed") {
					t.Fatalf("off inspected target: %s", log.String())
				}
			}
			finish()
			if got, err := os.ReadFile(filepath.Join(outside, "keep.txt")); err != nil || string(got) != "outside" {
				t.Fatalf("external target changed: %q %v", got, err)
			}
		})
	}
}

func TestInstallTrimStoppedReportRetainsWorkspace(t *testing.T) {
	cfg := installTrimConfig(t, watchConfiguration(t), "client/node_modules")
	cfg.Intake.StopReportRole = "implement"
	root, directory, kept := stoppedTrimFixture(t, cfg, []byte("unreadable history"))
	install := filepath.Join(directory, "workspace/client/node_modules/package")
	installTrimFile(t, install, "needed to reproduce the failure")
	(&noticeTracker{}).install(t, alwaysChoose("done"))
	var log trimTestLog
	finish := startStopQueue(t, cfg, root, 10*time.Millisecond, &log)
	waitFor(t, func() bool { return strings.Contains(log.String(), "reading stopped report") })
	finish()
	if got, err := os.ReadFile(install); err != nil || string(got) != "needed to reproduce the failure" {
		t.Fatalf("unfinished report lost workspace: %q %v", got, err)
	}
	assertStoppedFilesKept(t, directory, kept)
}

func TestInstallTrimFinishedUsesSavedReceipts(t *testing.T) {
	for _, saved := range []string{"receipt", "missing", "unreadable"} {
		t.Run(saved, func(t *testing.T) {
			cfg := installTrimConfig(t, watchConfiguration(t), "installed")
			(&noticeTracker{}).install(t, alwaysChoose("done"))
			root := t.TempDir()
			job := deliveredJob(t, root, 51, time.Now().Add(-time.Minute))
			old := watchConfiguration(t)
			old.Roles[0].Processes[0].Receipt = "installed/result.json"
			if saved == "receipt" {
				saveInstallTrimJobConfig(t, old, job)
			}
			if saved == "unreadable" {
				installTrimFile(t, filepath.Join(job, "engine.json"), "not a configuration")
			}
			marker := filepath.Join(job, "workspace/installed/result.json")
			installTrimFile(t, marker, "saved delivery evidence")
			installTrimFile(t, filepath.Join(job, "homes/0-0/cache/blob"), "regenerable")
			var log trimTestLog
			finish := startStopQueue(t, cfg, root, 10*time.Millisecond, &log)
			waitFor(t, func() bool { return trimmed(job) && strings.Contains(log.String(), "finished caches not removed") })
			finish()
			if got, err := os.ReadFile(marker); err != nil || string(got) != "saved delivery evidence" {
				t.Fatalf("saved receipt lost after operator config changed: %q %v", got, err)
			}
		})
	}
}
