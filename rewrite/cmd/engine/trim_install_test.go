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
	"sync"
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
	// Finished-request fixtures keep a receipt under .git. Make that a real,
	// isolated repository so the cleanup checks its index, never an ancestor's.
	if _, err := os.Stat(filepath.Join(directory, "workspace/.git")); err == nil {
		environmentGit(t, filepath.Join(directory, "workspace"), "-c", "init.templateDir=", "init", "-q")
		environmentGit(t, filepath.Join(directory, "workspace"), "rev-parse", "--show-toplevel")
	}
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
if [ "$TRIM_ON" = false ]; then for home in "$FIRST_HOME" "$SECOND_HOME" "$REVIEW_HOME"; do test -f "$home/client/node_modules/package"; test -f "$home/.cache/go-build/blob"; done; fi
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
if [ "$TRIM_ON" = false ]; then for home in "$FIRST_HOME" "$SECOND_HOME" "$REVIEW_HOME"; do test -f "$home/client/node_modules/package"; test -f "$home/.cache/go-build/blob"; done; fi
mkdir -p "$TASK_HOME/client/node_modules"
cp "$TASK_HOME/download/package" "$TASK_HOME/client/node_modules/package"
echo second-returned`
			next := `set -eu
cat >/dev/null
if [ "$TRIM_ON" = false ]; then for home in "$FIRST_HOME" "$SECOND_HOME" "$REVIEW_HOME"; do test -f "$home/client/node_modules/package"; test -f "$home/.cache/go-build/blob"; done; fi
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
					cfg.Roles[i].Processes[j].Env = map[string]string{"TRIM_ON": fmt.Sprint(enabled), "FIRST_HOME": filepath.Join(job, "homes/0-0"), "SECOND_HOME": filepath.Join(job, "homes/0-1"), "REVIEW_HOME": filepath.Join(job, "homes/1-0")}
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
			kept["workspace/client/src/main"] = "source sibling"
			for _, home := range []string{"0-0", "0-1", "1-0"} {
				kept["homes/"+home+"/download/package"] = "reusable"
				kept["homes/"+home+"/logs/session.txt"] = "original log"
				kept["homes/"+home+"/client/keep"] = "sibling"
				kept["homes/"+home+"/.cache/go-build/blob"] = "unlisted"
				if !enabled {
					kept["homes/"+home+"/client/node_modules/package"] = "reusable"
				}
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
			sibling := filepath.Join(directory, "workspace/client/src/main")
			installTrimFile(t, sibling, "must remain")
			files.hold(t)
			log := &lockedLog{}
			defer func() {
				if t.Failed() {
					log.mu.Lock()
					defer log.mu.Unlock()
					t.Log(log.text.String())
				}
			}()
			finish := startStopQueue(t, cfg, root, 10*time.Millisecond, log)
			waitFor(t, func() bool { return files.count(t, "start EXAMPLE-51") == 1 })
			if _, err := os.Stat(install); err != nil {
				t.Fatalf("preview lost installation: %v", err)
			}
			files.let(t)
			waitFor(t, func() bool { _, err := os.Stat(install); return errors.Is(err, os.ErrNotExist) })
			finish()
			if got, err := os.ReadFile(sibling); err != nil || string(got) != "must remain" {
				t.Fatalf("sibling removed: %q %v", got, err)
			}
			if record := pageState(t, directory); record.State != "down" {
				t.Fatalf("cleanup before page down: %+v", record)
			}
		})
	}
}

func TestInstallTrimRejectsUnsafeConfiguration(t *testing.T) {
	for _, name := range []string{"", ".", "../outside", "/outside", "client//node_modules", "client/../source", "**/node_modules", ".git", ".GIT", "client/.git/objects", "logs", "LOGS", "run", "report", "REPORT", "logſ", "résultats", "a\nb"} {
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

func TestInstallTrimReceiptLocationIsNotNewPolicy(t *testing.T) {
	cfg := installTrimConfig(t, watchConfiguration(t), "installed")
	cfg.Roles[0].Processes[0].Directory = "client"
	cfg.Roles[0].Processes[0].Receipt = "../report/result.json"
	if err := validateInstallTrim(cfg); err != nil {
		t.Fatalf("unrelated receipt rejected: %v", err)
	}
	cfg.Roles[0].Processes[0].Receipt = "../installed/result.json"
	if err := validateInstallTrim(cfg); err == nil {
		t.Fatal("overlapping receipt accepted")
	}
}

func TestInstallTrimExecutorSelectedOnlyWhenConfigured(t *testing.T) {
	next := &chain.Processes{}
	for _, cfg := range []config{{}, {Intake: &intakeConfig{}}} {
		if got := withInstallTrim(next, cfg, "not-an-existing-request", nil); got != next {
			t.Fatalf("unset cleanup installed a wrapper: %T", got)
		}
	}
	if got := withInstallTrim(next, config{Intake: &intakeConfig{TrimInstallDirs: []string{"installed"}}}, "request", nil); got == next {
		t.Fatal("configured cleanup omitted the wrapper")
	}
}

func TestInstallTrimMountAndReplacementBoundaries(t *testing.T) {
	job := t.TempDir()
	installTrimFile(t, filepath.Join(job, "installed/keep"), "original")
	root, mount, err := openInstallRoot(job)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	remaining, removed := 512, 0
	if err := removeInstallDirectory(context.Background(), root, "installed", mount+"-different", &remaining, &removed); err == nil || !strings.Contains(err.Error(), "mount") || removed != 0 {
		t.Fatalf("mount mismatch did not preserve target: removed=%d err=%v", removed, err)
	}
	before, err := root.Lstat("installed")
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Rename("installed", "original"); err != nil {
		t.Fatal(err)
	}
	if err := root.Mkdir("installed", 0700); err != nil {
		t.Fatal(err)
	}
	if err := removeInstallTarget(root, "installed", before); err == nil {
		t.Fatal("replacement directory removed")
	}
	if _, err := root.Stat("installed"); err != nil {
		t.Fatal("replacement no longer exists", err)
	}
	if got, err := root.ReadFile("original/keep"); err != nil || string(got) != "original" {
		t.Fatalf("original target changed: %q %v", got, err)
	}
}

func TestInstallTrimCaseAliasProtectsRecordsBeforeLaunching(t *testing.T) {
	for _, tc := range []struct{ name, receipt string }{
		{".GIT", "artifacts/receipt.json"},
		{"client/ARTIFACTS", "artifacts/receipt.json"},
		{"client/artifacts/RECEIPT.json/child", "artifacts/receipt.json"},
		{"client/results", "reſults/receipt.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useCatalogTransport(t, alwaysChoose("done"))
			job := t.TempDir()
			cfg := installTrimConfig(t, watchConfiguration(t), tc.name)
			cfg.Roles[0].Processes[0].Directory = "client"
			cfg.Roles[0].Processes[0].Receipt = tc.receipt
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
			receiptPath := filepath.Join("workspace/client", tc.receipt)
			for _, path := range []string{"workspace/.git/metadata", receiptPath} {
				installTrimFile(t, filepath.Join(job, path), "must remain")
			}
			var output, log bytes.Buffer
			err = run(context.Background(), []string{"--config", filepath.Join(job, "engine.json"), "--request", filepath.Join(job, "request.txt"), "--run-dir", filepath.Join(job, "run")}, &output, &log)
			if err == nil || !strings.Contains(err.Error(), "trim_install_dirs") {
				t.Fatalf("case alias allowed a run: %v", err)
			}
			for _, path := range []string{"workspace/.git/metadata", receiptPath} {
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
	for _, boundary := range []string{"parent link", "target link", "descendant link", "special file", "unbound home", "deep home"} {
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
			case "deep home":
				installTrimDeepTree(t, install)
			}
			var observations []string
			executor := installTrimExecutor{next: chain.Processes{Roles: map[string]chain.Role{"review": cfg.Roles[1]}}, cfg: cfg, job: job, observe: func(s string) { observations = append(observations, s) }}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			results := executor.Execute(ctx, chain.Assignment{Role: "review"}, chain.State{Step: "implement"})
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
			} else if len(observations) != 1 || !strings.Contains(observations[0], "not removed") || strings.Contains(observations[0], "workspace removal") {
				t.Fatalf("refusal not observed: %v", observations)
			}
			if boundary == "deep home" && (ctx.Err() != nil || !strings.Contains(observations[0], "no removal progress")) {
				t.Fatalf("home blocked the next role or omitted its reason: %v %v", ctx.Err(), observations)
			}
		})
	}
}

// Existing final-home markers must not suppress new workspace cleanup or a
// retry after a restart. With no setting a real installation is untouched and
// does not produce a new observation. A held path is not inspected each tick.
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
			for _, name := range []string{"node_modules/keep", ".cache/go-build/keep"} {
				installTrimFile(t, filepath.Join(directory, "workspace", name), "unlisted")
			}
			outside := t.TempDir()
			installTrimFile(t, filepath.Join(outside, "keep.txt"), "outside")
			target := filepath.Join(directory, "workspace/client/node_modules")
			if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				t.Fatal(err)
			}
			if enabled {
				if err := os.Symlink(outside, target); err != nil {
					t.Fatal(err)
				}
			} else {
				installTrimFile(t, filepath.Join(target, "package"), "keep when unset")
			}
			var log trimTestLog
			finish := startStopQueue(t, cfg, root, 10*time.Millisecond, &log)
			if enabled {
				waitFor(t, func() bool { return strings.Contains(log.String(), "finished workspace cleanup incomplete") })
				if err := os.Remove(target); err != nil {
					t.Fatal(err)
				}
				installTrimFile(t, filepath.Join(target, "package"), "recreated")
				waitTicks()
				if _, err := os.Stat(filepath.Join(target, "package")); err != nil {
					t.Fatalf("held path was retried before restart: %v", err)
				}
				finish()
				finish = startStopQueue(t, cfg, root, 10*time.Millisecond, &log)
				waitFor(t, func() bool { _, err := os.Lstat(target); return errors.Is(err, os.ErrNotExist) })
			} else {
				waitTicks()
				if got, err := os.ReadFile(filepath.Join(target, "package")); err != nil || string(got) != "keep when unset" {
					t.Fatalf("off changed real installation: %q %v", got, err)
				}
				if strings.Contains(log.String(), "not removed") || strings.Contains(log.String(), "workspace cleanup") {
					t.Fatalf("off inspected target: %s", log.String())
				}
			}
			finish()
			for _, name := range []string{"node_modules/keep", ".cache/go-build/keep"} {
				if got, err := os.ReadFile(filepath.Join(directory, "workspace", name)); err != nil || string(got) != "unlisted" {
					t.Fatalf("unlisted workspace install changed: %s %q %v", name, got, err)
				}
			}
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
			waitFor(t, func() bool {
				return trimmed(job) && strings.Contains(log.String(), "finished workspace cleanup incomplete")
			})
			finish()
			if got, err := os.ReadFile(marker); err != nil || string(got) != "saved delivery evidence" {
				t.Fatalf("saved receipt lost after operator config changed: %q %v", got, err)
			}
		})
	}
}

func TestInstallTrimDeepTreeDoesNotBlockOtherFinishedRequests(t *testing.T) {
	cfg := installTrimConfig(t, watchConfiguration(t), "installed", "plain")
	(&noticeTracker{}).install(t, alwaysChoose("done"))
	root := t.TempDir()
	first := deliveredJob(t, root, 51, time.Now().Add(-time.Minute))
	second := deliveredJob(t, root, 52, time.Now().Add(-time.Minute))
	for _, job := range []string{first, second} {
		saveInstallTrimJobConfig(t, cfg, job)
	}
	deep := filepath.Join(first, "workspace/installed")
	installTrimDeepTree(t, deep)
	plain := filepath.Join(first, "workspace/plain/file")
	installTrimFile(t, plain, "regenerable")
	installTrimFile(t, filepath.Join(second, "workspace/installed/file"), "regenerable")
	var log trimTestLog
	finish := startStopQueue(t, cfg, root, 10*time.Millisecond, &log)
	waitFor(t, func() bool {
		_, err := os.Stat(filepath.Join(second, "workspace/installed"))
		_, plainErr := os.Stat(plain)
		return errors.Is(err, os.ErrNotExist) && errors.Is(plainErr, os.ErrNotExist)
	})
	finish()
	if !strings.Contains(log.String(), "no removal progress") {
		t.Fatalf("deep tree not explained: %s", log.String())
	}
	if _, err := os.Stat(deep); err != nil {
		t.Fatalf("deep tree disappeared: %v", err)
	}
}

func installTrimDeepTree(t *testing.T, target string) {
	t.Helper()
	if err := os.MkdirAll(target, 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.OpenRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 600; i++ {
		if err := dir.Mkdir("d", 0700); err != nil {
			t.Fatal(err)
		}
		next, err := dir.OpenRoot("d")
		dir.Close()
		if err != nil {
			t.Fatal(err)
		}
		dir = next
	}
	file, err := dir.Create("marker")
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	dir.Close()
}

func TestInstallTrimFinishedBudgetAndAbsentPaths(t *testing.T) {
	job := t.TempDir()
	cfg := installTrimConfig(t, watchConfiguration(t), "installed")
	saveInstallTrimJobConfig(t, cfg, job)
	for i := 0; i < 600; i++ {
		installTrimFile(t, filepath.Join(job, "workspace/installed", fmt.Sprint(i)), "output")
	}
	memory := &installTrimMemory{}
	err := trimFinishedInstalls(context.Background(), job, cfg.Intake.TrimInstallDirs, memory)
	if !errors.Is(err, errRemovalYield) {
		t.Fatalf("large cleanup did not yield: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(job, "workspace/installed"))
	if err != nil || len(entries) != 88 {
		t.Fatalf("one pass left %d entries: %v", len(entries), err)
	}
	if err := trimFinishedInstalls(context.Background(), job, cfg.Intake.TrimInstallDirs, memory); err != nil {
		t.Fatal(err)
	}
	// A preceding name can leave too little budget even for a shallow tree.
	// That is a next-tick retry, not a permanent zero-progress hold.
	for i := 0; i < 511; i++ {
		installTrimFile(t, filepath.Join(job, "workspace/first", fmt.Sprint(i)), "output")
	}
	installTrimFile(t, filepath.Join(job, "workspace/second/child/file"), "output")
	names := []string{"first", "second"}
	if err := trimFinishedInstalls(context.Background(), job, names, memory); !errors.Is(err, errRemovalYield) || len(memory.held) != 0 {
		t.Fatalf("partial budget permanently held a shallow path: %v held=%v", err, memory.held)
	}
	if err := trimFinishedInstalls(context.Background(), job, names, memory); err != nil {
		t.Fatalf("next tick did not finish the shallow path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(job, "workspace/second")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("next tick retained regenerable output: %v", err)
	}
	// Changing the list, like restarting the queue, releases remembered holds.
	target := filepath.Join(job, "workspace/installed")
	if err := os.Symlink(t.TempDir(), target); err != nil {
		t.Fatal(err)
	}
	if err := trimFinishedInstalls(context.Background(), job, cfg.Intake.TrimInstallDirs, memory); err == nil {
		t.Fatal("symlink was not held")
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	installTrimFile(t, filepath.Join(target, "package"), "recreated")
	if err := trimFinishedInstalls(context.Background(), job, cfg.Intake.TrimInstallDirs, memory); err == nil {
		t.Fatal("same list retried a held path")
	}
	if _, err := os.Stat(filepath.Join(target, "package")); err != nil {
		t.Fatalf("remembered hold lost its directory: %v", err)
	}
	if err := trimFinishedInstalls(context.Background(), job, []string{"installed", "absent"}, memory); err != nil {
		t.Fatalf("changed list did not release the hold: %v", err)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("changed list retained regenerable output: %v", err)
	}
	// A corrupt saved configuration is irrelevant once there is nothing to remove.
	installTrimFile(t, filepath.Join(job, "engine.json"), "no longer readable")
	if err := trimFinishedInstalls(context.Background(), job, cfg.Intake.TrimInstallDirs, memory); err != nil {
		t.Fatalf("finished cleanup still read configuration: %v", err)
	}
	if err := trimFinishedInstalls(context.Background(), filepath.Join(job, "absent"), nil, memory); err != nil {
		t.Fatalf("unset cleanup inspected request: %v", err)
	}
}

func TestInstallTrimProtectsTrackedWork(t *testing.T) {
	for _, tc := range []struct {
		stopped, corrupt bool
		name, tracked    string
	}{{false, false, "vendor", "vendor"}, {true, false, "VENDOR", "vendor"}, {true, false, "results", "reſults"}, {false, true, "broken-index", "broken-index"}} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := installTrimConfig(t, watchConfiguration(t), tc.name, "installed")
			root := t.TempDir()
			job := deliveredJob(t, root, 51, time.Now().Add(-time.Minute))
			if tc.stopped {
				root, job, _ = stoppedTrimFixture(t, cfg, nil)
			}
			saveInstallTrimJobConfig(t, cfg, job)
			workspace := filepath.Join(job, "workspace")
			source := filepath.Join(tc.tracked, "lib/source.go")
			installTrimFile(t, filepath.Join(workspace, source), "unpublished work")
			// Exercise the case alias even on a case-sensitive test filesystem.
			installTrimFile(t, filepath.Join(workspace, tc.name, "untracked"), "rebuildable")
			environmentGit(t, workspace, "-c", "init.templateDir=", "init", "-q")
			environmentGit(t, workspace, "add", source)
			if tc.corrupt {
				installTrimFile(t, filepath.Join(workspace, ".git/index"), "not an index")
			}
			plain := filepath.Join(workspace, "installed/output")
			installTrimFile(t, plain, "regenerable")
			(&noticeTracker{}).install(t, alwaysChoose("done"))
			var log trimTestLog
			finish := startStopQueue(t, cfg, root, 10*time.Millisecond, &log)
			waitFor(t, func() bool {
				if tc.corrupt {
					_, err := os.Stat(filepath.Join(workspace, source))
					return strings.Contains(log.String(), "Git-tracked workspace files could not be checked") || errors.Is(err, os.ErrNotExist)
				}
				_, err := os.Stat(plain)
				return strings.Contains(log.String(), "contains Git-tracked work") && errors.Is(err, os.ErrNotExist)
			})
			finish()
			if got, err := os.ReadFile(filepath.Join(workspace, source)); err != nil || string(got) != "unpublished work" {
				t.Fatalf("tracked work lost: %q %v", got, err)
			}
			if tc.corrupt {
				if !strings.Contains(log.String(), "Git-tracked workspace files could not be checked") {
					t.Fatalf("broken index not explained: %s", log.String())
				}
				if _, err := os.Stat(plain); err != nil {
					t.Fatalf("unchecked index allowed deletion: %v", err)
				}
			}
		})
	}
}

func TestInstallTrimGitTimeoutAndCancellation(t *testing.T) {
	for _, cancellation := range []bool{true, false} {
		t.Run(fmt.Sprint(cancellation), func(t *testing.T) {
			cfg := installTrimConfig(t, watchConfiguration(t), "installed")
			(&noticeTracker{}).install(t, alwaysChoose("done"))
			root := t.TempDir()
			job := deliveredJob(t, root, 51, time.Now().Add(-time.Minute))
			saveInstallTrimJobConfig(t, cfg, job)
			target := filepath.Join(job, "workspace/installed/package")
			installTrimFile(t, target, "must remain")
			environmentGit(t, filepath.Join(job, "workspace"), "-c", "init.templateDir=", "init", "-q")
			bin := t.TempDir()
			entered := filepath.Join(bin, "entered")
			fakeGit := filepath.Join(bin, "git")
			installTrimFile(t, fakeGit, fmt.Sprintf("#!/bin/sh\nprintf entered > %q\nexec /bin/sleep 30\n", entered))
			if err := os.Chmod(fakeGit, 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			if cancellation {
				var log trimTestLog
				finish := startStopQueue(t, cfg, root, 10*time.Millisecond, &log)
				waitFor(t, func() bool { _, err := os.Stat(entered); return err == nil })
				finish()
				if !strings.Contains(log.String(), "finished workspace cleanup stopped") || strings.Contains(log.String(), "signal: killed") || strings.Contains(log.String(), "held until") {
					t.Fatalf("queue cancellation misreported: %s", log.String())
				}
			} else {
				memory := &installTrimMemory{}
				err := trimFinishedInstalls(context.Background(), job, cfg.Intake.TrimInstallDirs, memory)
				if err == nil || errors.Is(err, context.DeadlineExceeded) || memory.held["installed"] == "" || !strings.Contains(err.Error(), "timed out") {
					t.Fatalf("Git timeout was not remembered as a hold: %v held=%v", err, memory.held)
				}
			}
			if got, err := os.ReadFile(target); err != nil || string(got) != "must remain" {
				t.Fatalf("cancelled or timed-out cleanup removed files: %q %v", got, err)
			}
		})
	}
}

func TestInstallTrimUsesOnlyWorkspaceGitRoot(t *testing.T) {
	ancestor := t.TempDir()
	environmentGit(t, ancestor, "-c", "init.templateDir=", "init", "-q")
	job := filepath.Join(ancestor, "job")
	workspace := filepath.Join(job, "workspace")
	installTrimFile(t, filepath.Join(workspace, "installed/output"), "regenerable")
	// An ancestor tracks this path, but this request did not make a Git
	// workspace. No ancestor index or configuration should be consulted.
	environmentGit(t, ancestor, "add", "job/workspace/installed/output")
	cfg := installTrimConfig(t, watchConfiguration(t), "installed")
	saveInstallTrimJobConfig(t, cfg, job)
	if err := trimFinishedInstalls(context.Background(), job, cfg.Intake.TrimInstallDirs, &installTrimMemory{}); err != nil {
		t.Fatalf("plain workspace consulted ancestor Git: %v", err)
	}
	installTrimFile(t, filepath.Join(workspace, "installed/output"), "regenerable")
	environmentGit(t, workspace, "-c", "init.templateDir=", "init", "-q")
	environmentGit(t, workspace, "config", "core.worktree", ancestor)
	if err := trimFinishedInstalls(context.Background(), job, cfg.Intake.TrimInstallDirs, &installTrimMemory{}); err == nil || !strings.Contains(err.Error(), "Git root") {
		t.Fatalf("redirected Git root accepted: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(workspace, "installed/output")); err != nil || string(got) != "regenerable" {
		t.Fatalf("redirected root removed files: %q %v", got, err)
	}
}

func TestInstallTrimQuestionPreservesPreviousHomeThroughAnswer(t *testing.T) {
	for _, next := range []string{"implement", "review"} {
		t.Run(next, func(t *testing.T) { testInstallTrimAfterAnswer(t, next) })
	}
}

func testInstallTrimAfterAnswer(t *testing.T, next string) {
	root := t.TempDir()
	cfg := installTrimConfig(t, questionConfiguration(t), ".cache/go-build")
	cfg.Roles = append(cfg.Roles, chain.Role{Name: "implement", Processes: []chain.Process{{Name: "worker", Command: []string{"/bin/sh", "-c", `set -eu
cat >/dev/null
if [ -f "$TASK_HOME/ran" ]; then test -f "$TASK_HOME/.cache/go-build/blob"; echo reused > "$TASK_WORKSPACE/reused"; fi
mkdir -p "$TASK_HOME/.cache/go-build"; echo keep > "$TASK_HOME/.cache/go-build/blob"; touch "$TASK_HOME/ran"; echo work-returned`}}}})
	cfg.Roles = append(cfg.Roles, chain.Role{Name: "review", Processes: []chain.Process{{Name: "reader", Command: []string{"/bin/sh", "-c", `set -eu
cat >/dev/null
test ! -e "$PREVIOUS_HOME/.cache/go-build"
echo reused > "$TASK_WORKSPACE/reused"; echo review-returned`}, Env: map[string]string{"PREVIOUS_HOME": filepath.Join(root, "jobs/51/homes/1-0")}}}})
	var mu sync.Mutex
	comments := []json.RawMessage{}
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Host == "watch-tracker.example" {
			if strings.HasSuffix(r.URL.Path, "/comments") {
				if r.Method == http.MethodPost {
					comments = append(comments, issueComment(702, 900, postedQuestion))
					return selectionReply(r, 201, comments[len(comments)-1]), nil
				}
				return selectionReply(r, 200, comments), nil
			}
			return selectionReply(r, 200, []any{watchedIssue(51, "Original conditions", "2026-01-03T00:00:00Z")}), nil
		}
		var input struct{ State chain.State }
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			return nil, err
		}
		works, answered, reviewed := 0, false, false
		for _, row := range input.State.History {
			if row.Role == "implement" {
				works++
			}
			if row.Speaker == "requester" {
				answered = true
			}
			if row.Role == "review" {
				reviewed = true
			}
		}
		choice := "implement"
		if works == 1 && !answered {
			choice = "ask_requester"
		}
		if answered {
			choice = next
		}
		if works == 2 || reviewed {
			choice = "done"
		}
		return selectionReply(r, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": choice}}}), nil
	})
	log := &lockedLog{}
	defer func() {
		if t.Failed() {
			log.mu.Lock()
			defer log.mu.Unlock()
			t.Log(log.text.String())
		}
	}()
	finish := startStopQueue(t, cfg, root, 10*time.Millisecond, log)
	waitFor(t, func() bool { state, err := loadWatchState(root, 51); return err == nil && state.Waiting })
	if _, err := os.Stat(filepath.Join(root, "jobs/51/homes/1-0/.cache/go-build/blob")); err != nil {
		t.Fatalf("question removed previous home: %v", err)
	}
	mu.Lock()
	comments = append(comments, issueComment(703, 55, "continue"))
	mu.Unlock()
	waitFor(t, func() bool { state, err := loadWatchState(root, 51); return err == nil && state.Done })
	finish()
	if got, err := os.ReadFile(filepath.Join(root, "jobs/51/workspace/reused")); err != nil || string(got) != "reused\n" {
		t.Fatalf("answer did not reuse previous work: %q %v", got, err)
	}
}
