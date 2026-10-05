package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func retentionConfig(t *testing.T, value any) config {
	t.Helper()
	cfg := watchConfiguration(t)
	cfg.Intake.StopReportRole = "implement"
	raw, _ := json.Marshal(map[string]any{"stopped_workspace_retention_hours": value})
	if err := json.Unmarshal(raw, cfg.Intake); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestStoppedRetentionRequiresExplicitValidPolicy(t *testing.T) {
	for _, test := range []struct {
		hours     int
		reporter  bool
		wantError bool
	}{{0, false, false}, {1, true, false}, {-1, true, true}, {1, false, true}} {
		cfg := retentionConfig(t, test.hours)
		if !test.reporter {
			cfg.Intake.StopReportRole = ""
		}
		_, _, _, _, err := watchSettings(&cfg, t.TempDir())
		if (err != nil) != test.wantError {
			t.Errorf("hours=%d reporter=%t: error=%v", test.hours, test.reporter, err)
		}
	}
}

func retentionFixture(t *testing.T) (config, string, string, map[string][]byte) {
	t.Helper()
	cfg := retentionConfig(t, 1)
	root, dir, kept := stoppedTrimFixture(t, cfg, []byte(`{"done":true}`))
	raw, _ := json.Marshal(cfg)
	for name, data := range map[string][]byte{
		"engine.json":             raw,
		".workspace.prepared":     []byte(`{"recorded_at":"2026-01-02T03:04:05Z","prepared_again":false}`),
		".workspace.prepare.lock": nil, "run/runner.lock": nil, "stop-report/runner.lock": nil,
		"workspace/report/result.md": []byte("the stopped work was not delivered\n"),
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := writeRuntimeFile(filepath.Join(dir, name), data); err != nil {
			t.Fatal(err)
		}
		kept[name] = data
	}
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		return selectionReply(r, 200, []any{}), nil
	})
	return cfg, root, dir, kept
}

func TestStoppedRetentionWaitsThenReclaimsOnlyItsWorkspace(t *testing.T) {
	cfg, root, dir, kept := retentionFixture(t)
	finish := startStopQueue(t, cfg, root, 10*time.Millisecond, io.Discard)
	defer func() {
		if finish != nil {
			finish()
		}
	}()
	path := filepath.Join(dir, "workspace-removal.json")
	waitFor(t, func() bool { _, err := os.Stat(path); return err == nil })
	finish()
	finish = nil
	assertStoppedFilesKept(t, dir, kept)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	record["after"] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	raw, _ = json.Marshal(record)
	if err := writeRuntimeFile(path, raw); err != nil {
		t.Fatal(err)
	}
	finish = startStopQueue(t, cfg, root, 10*time.Millisecond, io.Discard)
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(dir, "workspace")); return errors.Is(err, os.ErrNotExist) })
	finish()
	finish = nil
	evidenceBytes, err := os.ReadFile(filepath.Join(dir, "workspace-evidence.json"))
	if err != nil {
		t.Fatal(err)
	}
	var evidence map[string][]byte
	if err := json.Unmarshal(evidenceBytes, &evidence); err != nil {
		t.Fatal(err)
	}
	for name, want := range kept {
		if name == "workspace/undelivered.txt" {
			continue
		}
		var got []byte
		var err error
		if name == "workspace/.git/ticket-engine/delivery.json" || name == "workspace/report/result.md" {
			got = evidence[name[len("workspace/"):]]
		} else {
			got, err = os.ReadFile(filepath.Join(dir, name))
		}
		if err != nil || string(got) != string(want) {
			t.Errorf("retained %s: %q %v", name, got, err)
		}
	}
}

func TestStoppedRetentionKeepsIneligibleAndOwnedWork(t *testing.T) {
	for _, kind := range []string{"disabled", "no stop", "unfinished report", "unrecorded preparation", "locked", "unreadable configuration", "missing history", "evidence write fails"} {
		t.Run(kind, func(t *testing.T) {
			cfg, _, dir, kept := retentionFixture(t)
			issue, _ := cfg.source().ReadIssue(kept["issue.json"])
			switch kind {
			case "disabled":
				cfg.Intake.StoppedWorkspaceRetentionHours = 0
			case "unreadable configuration", "missing history":
				name := "engine.json"
				if kind == "missing history" {
					name = "run/history.json"
				}
				kept[name] = []byte(`null`)
				if err := writeRuntimeFile(filepath.Join(dir, name), kept[name]); err != nil {
					t.Fatal(err)
				}
			case "evidence write fails":
				if err := os.Mkdir(filepath.Join(dir, "workspace-evidence.json"), 0700); err != nil {
					t.Fatal(err)
				}
			case "no stop":
				if err := os.Remove(filepath.Join(dir, "stop-request.json")); err != nil {
					t.Fatal(err)
				}
				delete(kept, "stop-request.json")
			case "unfinished report":
				kept["stop-report/history.json"] = []byte(`{"done":false}`)
				if err := writeRuntimeFile(filepath.Join(dir, "stop-report/history.json"), kept["stop-report/history.json"]); err != nil {
					t.Fatal(err)
				}
			case "unrecorded preparation":
				if err := os.Remove(filepath.Join(dir, ".workspace.prepared")); err != nil {
					t.Fatal(err)
				}
				delete(kept, ".workspace.prepared")
			case "locked":
				file, err := os.OpenFile(filepath.Join(dir, "run/runner.lock"), os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer file.Close()
				if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
					t.Fatal(err)
				}
			}
			for _, now := range []time.Time{time.Now(), time.Now().Add(24 * time.Hour)} {
				if removed, _ := reclaimStoppedWorkspace(cfg, issue, dir, now); removed {
					t.Fatal("ineligible work was discarded")
				}
			}
			assertStoppedFilesKept(t, dir, kept)
			if kind == "disabled" {
				if _, err := os.Stat(filepath.Join(dir, removalFile)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("disabled retention created removal progress")
				}
			}
		})
	}
}

func TestStoppedRetentionPreservesConfiguredReceiptsAndOutsideLinks(t *testing.T) {
	cfg, _, dir, kept := retentionFixture(t)
	issue, _ := cfg.source().ReadIssue(kept["issue.json"])
	bound, err := bindRequestConfig(cfg, dir, issue.Key)
	if err != nil {
		t.Fatal(err)
	}
	bound.Roles[0].Processes[0].Directory = filepath.Join(dir, "workspace", "sub")
	bound.Roles[0].Processes[0].Receipt = "result.bin"
	raw, _ := json.Marshal(bound)
	if err := writeRuntimeFile(filepath.Join(dir, "engine.json"), raw); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "workspace", "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	want := []byte{0, 255, 17, 10}
	if err := os.WriteFile(filepath.Join(dir, "workspace/sub/result.bin"), want, 0600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "keep.txt"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "workspace", "outside-link")); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := reclaimStoppedWorkspace(cfg, issue, dir, now); err != nil {
		t.Fatal(err)
	}
	if removed, err := reclaimStoppedWorkspace(cfg, issue, dir, now.Add(30*time.Minute)); err != nil || removed {
		t.Fatalf("grace was shortened: %t %v", removed, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "workspace", "undelivered.txt")); err != nil {
		t.Fatal("workspace was discarded before its deadline")
	}
	if removed, err := reclaimStoppedWorkspace(cfg, issue, dir, now.Add(2*time.Hour)); err != nil || !removed {
		t.Fatalf("remove=%t error=%v", removed, err)
	}
	raw, err = os.ReadFile(filepath.Join(dir, "workspace-evidence.json"))
	if err != nil {
		t.Fatal(err)
	}
	var evidence map[string][]byte
	if err := json.Unmarshal(raw, &evidence); err != nil {
		t.Fatal(err)
	}
	if string(evidence["sub/result.bin"]) != string(want) {
		t.Fatalf("receipt changed: %v", evidence)
	}
	if raw, err := os.ReadFile(filepath.Join(outside, "keep.txt")); err != nil || string(raw) != "outside" {
		t.Fatal("outside link target was changed")
	}
}

func TestStoppedRetentionHoldsUnreadableEvidenceAndResumesPartialRemoval(t *testing.T) {
	cfg, _, dir, kept := retentionFixture(t)
	issue, _ := cfg.source().ReadIssue(kept["issue.json"])
	for i := 0; i < 540; i++ {
		if err := os.WriteFile(filepath.Join(dir, "workspace", fmt.Sprintf("work-%d", i)), []byte("unpublished"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	if _, err := reclaimStoppedWorkspace(cfg, issue, dir, now); err != nil {
		t.Fatal(err)
	}
	if removed, err := reclaimStoppedWorkspace(cfg, issue, dir, now.Add(2*time.Hour)); removed || !errors.Is(err, errRemovalYield) {
		t.Fatalf("partial removal: %t %v", removed, err)
	}
	evidencePath := filepath.Join(dir, "workspace-evidence.json")
	raw, err := os.ReadFile(evidencePath)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadDir(filepath.Join(dir, "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Intake.StoppedWorkspaceRetentionHours = 0
	if _, err := reclaimStoppedWorkspace(cfg, issue, dir, now.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	cfg.Intake.StoppedWorkspaceRetentionHours = 1
	if err := writeRuntimeFile(evidencePath, []byte("unreadable")); err != nil {
		t.Fatal(err)
	}
	if _, err := reclaimStoppedWorkspace(cfg, issue, dir, now.Add(3*time.Hour)); err == nil {
		t.Fatal("deletion continued without readable evidence")
	}
	after, err := os.ReadDir(filepath.Join(dir, "workspace"))
	if err != nil || len(after) != len(before) {
		t.Fatal("held removal changed the remaining workspace")
	}
	if err := writeRuntimeFile(evidencePath, raw); err != nil {
		t.Fatal(err)
	}
	if removed, err := reclaimStoppedWorkspace(cfg, issue, dir, now.Add(4*time.Hour)); err != nil || !removed {
		t.Fatalf("retry: %t %v", removed, err)
	}
	afterBytes, err := os.ReadFile(evidencePath)
	if err != nil || string(afterBytes) != string(raw) {
		t.Fatal("retry replaced the original receipts")
	}
}

func TestStoppedRetentionRejectsReplacedRootsAndLinkedReceipts(t *testing.T) {
	for _, kind := range []string{"replacement", "root link", "receipt link", "mount boundary"} {
		t.Run(kind, func(t *testing.T) {
			cfg, _, dir, kept := retentionFixture(t)
			issue, _ := cfg.source().ReadIssue(kept["issue.json"])
			now := time.Now()
			if _, err := reclaimStoppedWorkspace(cfg, issue, dir, now); err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			if err := os.WriteFile(filepath.Join(outside, "keep.txt"), []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "replacement", "root link":
				if err := os.Rename(filepath.Join(dir, "workspace"), filepath.Join(dir, "original-workspace")); err != nil {
					t.Fatal(err)
				}
				var err error
				if kind == "replacement" {
					err = os.Mkdir(filepath.Join(dir, "workspace"), 0700)
				} else {
					err = os.Symlink(outside, filepath.Join(dir, "workspace"))
				}
				if err != nil {
					t.Fatal(err)
				}
				if kind == "replacement" {
					if err := os.WriteFile(filepath.Join(dir, "workspace", "new-work.txt"), []byte("new work"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			case "receipt link":
				name := filepath.Join(dir, "workspace/.git/ticket-engine/delivery.json")
				if err := os.Remove(name); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(outside, "keep.txt"), name); err != nil {
					t.Fatal(err)
				}
			case "mount boundary":
				root, err := os.OpenRoot(dir)
				if err != nil {
					t.Fatal(err)
				}
				defer root.Close()
				if child, err := removalDirectory(root, "workspace", "different-mount"); err == nil {
					child.Close()
					t.Fatal("a different opened mount was accepted")
				}
				return // Identity mismatch only; this is not a real mounted filesystem test.
			}
			if _, err := reclaimStoppedWorkspace(cfg, issue, dir, now.Add(2*time.Hour)); err == nil {
				t.Fatal("unsafe removal was accepted")
			}
			if kind == "replacement" {
				if raw, err := os.ReadFile(filepath.Join(dir, "workspace", "new-work.txt")); err != nil || string(raw) != "new work" {
					t.Fatal("replacement work was changed before the error was reported")
				}
			}
			if raw, err := os.ReadFile(filepath.Join(outside, "keep.txt")); err != nil || string(raw) != "keep" {
				t.Fatal("outside content changed")
			}
		})
	}
}
