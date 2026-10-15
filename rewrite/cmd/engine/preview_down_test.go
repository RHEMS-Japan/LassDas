package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"ticket-runner/internal/chain"
)

// downScript stands in for the operator's command that takes the page down.
// It notes when it begins and how it returned in a file the test reads, waits
// while the release file is absent, and fails while the failure file is
// present.
const downScript = `cat > /dev/null
echo "start $TASK_ISSUE" >> "$DOWN_LOG"
while [ ! -f "$DOWN_RELEASE" ]; do sleep 0.05; done
if [ -f "$DOWN_FAIL" ]; then echo "failed $TASK_ISSUE" >> "$DOWN_LOG"; echo 'the cluster did not answer' >&2; exit 1; fi
echo "end $TASK_ISSUE" >> "$DOWN_LOG"`

// previewHoldHours is how long the engine waits for the page to be taken
// down in these tests' configuration.
const previewHoldHours = 73

type downFiles struct{ log, release, fail string }

func (f downFiles) lines(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(f.log)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(strings.ReplaceAll(strings.TrimSpace(string(raw)), " ", "_"))
}

func (f downFiles) count(t *testing.T, line string) int {
	t.Helper()
	total := 0
	for _, got := range f.lines(t) {
		if got == strings.ReplaceAll(line, " ", "_") {
			total++
		}
	}
	return total
}

func (f downFiles) hold(t *testing.T) {
	t.Helper()
	if err := os.Remove(f.release); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func (f downFiles) let(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(f.release, nil, 0600); err != nil {
		t.Fatal(err)
	}
}

// previewDownConfig is previewStagesConfig whose down role is downScript,
// released unless a test holds it.
func previewDownConfig(t *testing.T) (config, downFiles) {
	t.Helper()
	cfg := previewStagesConfig(t)
	dir := t.TempDir()
	files := downFiles{log: filepath.Join(dir, "downs.txt"), release: filepath.Join(dir, "release"), fail: filepath.Join(dir, "fail")}
	files.let(t)
	down := &cfg.Roles[5].Processes[0]
	down.Env = map[string]string{"DOWN_LOG": files.log, "DOWN_RELEASE": files.release, "DOWN_FAIL": files.fail}
	down.Command = []string{"/bin/sh", "-c", downScript}
	if cfg.Workflow.Preview.HoldHours != previewHoldHours {
		t.Fatalf("the configuration holds the page for %d hours", cfg.Workflow.Preview.HoldHours)
	}
	return cfg, files
}

// pageUpJob lays down a delivered request whose page is up, with its
// workspace and a cache in a role's home that the trim removes.
func pageUpJob(t *testing.T, root string, id int, started time.Time) string {
	t.Helper()
	directory := deliveredJob(t, root, id, time.Now().Add(-time.Minute))
	for _, name := range []string{"workspace", "homes/0-0/.cache"} {
		if err := os.MkdirAll(filepath.Join(directory, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(directory, "homes/0-0/.cache/blob"), []byte("rebuildable"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writePreviewRecord(directory, previewRecord{State: "up", Role: "preview_up", StartedAt: started, URL: previewAddress}); err != nil {
		t.Fatal(err)
	}
	return directory
}

func trimmed(directory string) bool {
	_, err := os.Stat(filepath.Join(directory, "homes", ".trimmed"))
	return err == nil
}

func pageState(t *testing.T, directory string) previewRecord {
	t.Helper()
	return readPreviewFile(t, filepath.Join(directory, previewFile))
}

// waitTicks lets the queue go round several more times.
func waitTicks() { time.Sleep(250 * time.Millisecond) }

// A delivered or a stopped request whose page was put up has it taken down
// once by the down role, launched for that request with its own home and
// issue. The request's record then says so with the time, and only after that
// are the request's caches removed.
func TestADeliveredOrStoppedRequestsPageIsTakenDownOnce(t *testing.T) {
	for _, ending := range []string{"delivered", "stopped"} {
		t.Run(ending, func(t *testing.T) {
			cfg, files := previewDownConfig(t)
			fixture := &noticeTracker{}
			fixture.install(t, alwaysChoose("done"))
			var root, directory string
			if ending == "delivered" {
				root = t.TempDir()
				directory = pageUpJob(t, root, 51, time.Now().Add(-time.Hour))
			} else {
				root, directory, _ = stoppedTrimFixture(t, cfg, nil)
				if err := os.MkdirAll(filepath.Join(directory, "workspace"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := writePreviewRecord(directory, previewRecord{State: "up", Role: "preview_up", StartedAt: time.Now().Add(-time.Hour), URL: previewAddress}); err != nil {
					t.Fatal(err)
				}
			}
			files.hold(t)
			finish := startStopQueue(t, cfg, root, 10*time.Millisecond, &lockedLog{})
			waitFor(t, func() bool { return files.count(t, "start EXAMPLE-51") == 1 })
			waitTicks()
			if trimmed(directory) {
				t.Fatal("the request's caches were removed before its page was taken down")
			}
			files.let(t)
			waitFor(t, func() bool { return trimmed(directory) })
			waitTicks()
			finish()
			if got := files.lines(t); len(got) != 2 || got[0] != "start_EXAMPLE-51" || got[1] != "end_EXAMPLE-51" {
				t.Fatalf("the page was not taken down once: %v", got)
			}
			if record := pageState(t, directory); record.State != "down" || record.DownAt == nil || record.DownAt.Before(record.StartedAt) {
				t.Fatalf("the request's record does not say when the page was taken down: %+v", record)
			}
		})
	}
}

// A page that cannot be taken down is tried again later, spaced out as a
// refused status is, and its reason is said once while it stays the same.
// The request's caches stay until the page is down.
func TestAPageThatCannotBeTakenDownIsTriedAgainAndSaidOnce(t *testing.T) {
	cfg, files := previewDownConfig(t)
	fixture := &noticeTracker{}
	fixture.install(t, alwaysChoose("done"))
	var offset atomic.Int64
	previous := turnClock
	turnClock = func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	t.Cleanup(func() { turnClock = previous })
	root := t.TempDir()
	directory := pageUpJob(t, root, 51, time.Now().Add(-time.Hour))
	if err := os.WriteFile(files.fail, nil, 0600); err != nil {
		t.Fatal(err)
	}
	log := &lockedLog{}
	finish := startStopQueue(t, cfg, root, 10*time.Millisecond, log)
	waitFor(t, func() bool { return files.count(t, "failed EXAMPLE-51") == 1 })
	waitTicks()
	if got := files.count(t, "start EXAMPLE-51"); got != 1 {
		t.Fatalf("a refused take-down was tried again at once: %d launches", got)
	}
	record := pageState(t, directory)
	if record.State != "up" || record.Refused != 1 || !strings.Contains(record.DownReason, "the cluster did not answer") || record.Next.IsZero() {
		t.Fatalf("the refusal was not recorded with when to try again: %+v", record)
	}
	// The minute after the first refusal has passed.
	offset.Store(int64(2 * time.Minute))
	waitFor(t, func() bool { return files.count(t, "failed EXAMPLE-51") == 2 })
	waitTicks()
	if trimmed(directory) {
		t.Fatal("the caches were removed while the page was still up")
	}
	said := strings.Count(func() string { log.mu.Lock(); defer log.mu.Unlock(); return log.text.String() }(), "the cluster did not answer")
	if said != 1 {
		t.Fatalf("the same reason was said %d times", said)
	}
	if err := os.Remove(files.fail); err != nil {
		t.Fatal(err)
	}
	offset.Store(int64(time.Hour))
	waitFor(t, func() bool { return trimmed(directory) })
	finish()
	if record := pageState(t, directory); record.State != "down" {
		t.Fatalf("the page was not recorded down once the role exited 0: %+v", record)
	}
}

// Taking a page down holds up nothing else. While one take-down has not
// returned, the queue takes up and runs another request although it has a
// single execution slot, and does not begin a second take-down; that one
// begins once the first has returned.
func TestTakingAPageDownHoldsUpNoOtherRequest(t *testing.T) {
	cfg, files := previewDownConfig(t)
	cfg.Intake.MaxRunning = 1
	fixture := &noticeTracker{}
	fixture.install(t, askAfterReading)
	root := t.TempDir()
	// The stand-in tracker keeps every comment on issue 51, so the request
	// that runs is that one. The two delivered ones with pages up come
	// before it in the queue's order, so a take-down that held up the queue
	// would keep it from ever being launched.
	pageUpJob(t, root, 49, time.Now().Add(-time.Hour))
	pageUpJob(t, root, 50, time.Now().Add(-time.Hour))
	_, waiting := noticeJobAt(t, root, 51)
	files.hold(t)
	finish := startStopQueue(t, cfg, root, 10*time.Millisecond, &lockedLog{})
	waitFor(t, func() bool { return len(files.lines(t)) == 1 })
	waitFor(t, func() bool {
		state, err := savedHistory(waiting)
		return err == nil && state.Waiting
	})
	if got := files.lines(t); len(got) != 1 {
		t.Fatalf("a second take-down began while the first had not returned: %v", got)
	}
	files.let(t)
	waitFor(t, func() bool { return files.count(t, "end EXAMPLE-49")+files.count(t, "end EXAMPLE-50") == 2 })
	finish()
	got := files.lines(t)
	for i := 0; i+1 < len(got); i += 2 {
		if !strings.HasPrefix(got[i], "start_") || got[i+1] != "end_"+strings.TrimPrefix(got[i], "start_") {
			t.Fatalf("take-downs overlapped: %v", got)
		}
	}
}

// noticeJobAt lays down an accepted request that has not started.
func noticeJobAt(t *testing.T, root string, id int) (string, string) {
	t.Helper()
	directory := filepath.Join(root, "jobs", strconv.Itoa(id))
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(watchedIssue(id, "Show the item count on the list screen.", "2026-01-03T00:00:00Z"))
	if err := writeRuntimeFile(filepath.Join(directory, "issue.json"), raw); err != nil {
		t.Fatal(err)
	}
	return root, directory
}

// stoppedPageFixture is a stopped request whose page is up, whose stop report
// has not run yet, and whose workspace the operator lets the engine reclaim.
func stoppedPageFixture(t *testing.T, cfg config) (string, string) {
	t.Helper()
	root, directory, _ := stoppedTrimFixture(t, cfg, nil)
	raw, _ := json.Marshal(cfg)
	for name, data := range map[string][]byte{
		"engine.json":             raw,
		".workspace.prepared":     []byte(`{"recorded_at":"2026-01-02T03:04:05Z","prepared_again":false}`),
		".workspace.prepare.lock": nil, "run/runner.lock": nil,
		"workspace/report/result.md": []byte("the stopped work was not delivered\n"),
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(directory, name)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := writeRuntimeFile(filepath.Join(directory, name), data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writePreviewRecord(directory, previewRecord{State: "up", Role: "preview_up", StartedAt: time.Now().Add(-time.Hour), URL: previewAddress}); err != nil {
		t.Fatal(err)
	}
	return root, directory
}

// withStopReport adds a stop report role that notes when it ran in the same
// file as the take-down, and lets the engine reclaim a stopped workspace.
func withStopReport(cfg config, files downFiles) config {
	cfg.Intake.StopReportRole = "report_stop"
	cfg.Intake.StoppedWorkspaceRetentionHours = 1
	cfg.Roles = append(cfg.Roles, chain.Role{Name: "report_stop", Purpose: "Report the stop.", Processes: []chain.Process{{Name: "reporter",
		Env: map[string]string{"DOWN_LOG": files.log}, Command: []string{"/bin/sh", "-c", `cat > /dev/null; echo "report $TASK_ISSUE" >> "$DOWN_LOG"`}}}})
	return cfg
}

// A stopped request reports its stop at once; the report does not wait for
// the page to be taken down. Only the trim and the reclaiming of the stopped
// workspace wait for that: while the take-down runs, the reclaim finds it
// holding the request and leaves the workspace alone.
func TestAStoppedRequestsReportGoesOnWhileItsPageIsTakenDown(t *testing.T) {
	cfg, files := previewDownConfig(t)
	cfg = withStopReport(cfg, files)
	fixture := &noticeTracker{}
	fixture.install(t, alwaysChoose("done"))
	root, directory := stoppedPageFixture(t, cfg)
	files.hold(t)
	finish := startStopQueue(t, cfg, root, 10*time.Millisecond, &lockedLog{})
	waitFor(t, func() bool { done, err := stoppedReportDone(directory); return err == nil && done })
	waitTicks()
	if got := files.lines(t); len(got) != 2 || !slices.Contains(got, "start_EXAMPLE-51") || !slices.Contains(got, "report_EXAMPLE-51") {
		t.Fatalf("the stop report waited for the take-down, or the take-down did not begin: %v", got)
	}
	if trimmed(directory) {
		t.Fatal("the stopped request's caches were removed while its page was being taken down")
	}
	if _, err := os.Stat(filepath.Join(directory, removalFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the stopped workspace was reclaimed while its page was being taken down: %v", err)
	}
	issue, err := cfg.source().ReadIssue(readFile(t, filepath.Join(directory, "issue.json")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reclaimStoppedWorkspace(cfg, issue, directory, time.Now()); err == nil || !strings.Contains(err.Error(), "owned") {
		t.Fatalf("the reclaim did not find the take-down holding the workspace: %v", err)
	}
	files.let(t)
	waitFor(t, func() bool {
		_, err := os.Stat(filepath.Join(directory, removalFile))
		return err == nil && trimmed(directory)
	})
	finish()
	if record := pageState(t, directory); record.State != "down" {
		t.Fatalf("the page is not recorded down: %+v", record)
	}
}

// The request's record says a page is going up before its role runs. A stop
// while the page is going up stops that role, and the stopped request's page,
// whatever was made of it, is taken down.
func TestAPageCutShortByAStopIsTakenDown(t *testing.T) {
	cfg, files := previewDownConfig(t)
	cfg.Roles[4].Processes[0].Command = []string{"/bin/sh", "-c", previewUpScript + "\necho going-up > ../going-up\nexec sleep 30"}
	fixture := &noticeTracker{}
	fixture.install(t, askAfterReading)
	root, directory := noticeJobAt(t, t.TempDir(), 51)
	finish := startStopQueue(t, cfg, root, 10*time.Millisecond, &lockedLog{})
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(directory, "going-up")); return err == nil })
	if record := pageState(t, directory); record.State != "starting" {
		t.Fatalf("the request's record did not say a page was going up while its role ran: %+v", record)
	}
	fixture.set(func(n *noticeTracker) { n.rows = append(n.rows, issueComment(800, 55, "停止")) })
	waitFor(t, func() bool { return files.count(t, "end EXAMPLE-51") == 1 })
	waitTicks()
	finish()
	if got := files.lines(t); len(got) != 2 {
		t.Fatalf("the stopped request's page was not taken down once: %v", got)
	}
	if record := pageState(t, directory); record.State != "down" {
		t.Fatalf("the page is not recorded down: %+v", record)
	}
}

// A page that has not been confirmed taken down by the time the operator's
// hold runs out, counted from when it was put up, holds the request no longer:
// that is said once, the caches are removed, and the take-down is not tried
// again, since a reclaimed workspace leaves the role nowhere to run.
func TestAPageNotConfirmedDownByTheHoldNoLongerHoldsTheRequest(t *testing.T) {
	cfg, files := previewDownConfig(t)
	fixture := &noticeTracker{}
	fixture.install(t, alwaysChoose("done"))
	if err := os.WriteFile(files.fail, nil, 0600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	directory := pageUpJob(t, root, 51, time.Now().Add(-(previewHoldHours+1)*time.Hour))
	log := &lockedLog{}
	finish := startStopQueue(t, cfg, root, 10*time.Millisecond, log)
	waitFor(t, func() bool { return trimmed(directory) })
	waitTicks()
	finish()
	if got := files.lines(t); len(got) != 0 {
		t.Fatalf("the take-down was tried after the hold ran out: %v", got)
	}
	if record := pageState(t, directory); record.State != "unconfirmed" {
		t.Fatalf("the request's record does not say the page was not confirmed down: %+v", record)
	}
	log.mu.Lock()
	said := strings.Count(log.text.String(), "not confirmed taken down")
	log.mu.Unlock()
	if said != 1 {
		t.Fatalf("the end of the hold was said %d times", said)
	}
}

// The reclaim of a stopped workspace also stops at a running take-down, but a
// request that never put a page up has no such lock and is reclaimed as
// before, and the three locks it already waited on are read as before.
func TestTheReclaimWaitsOnlyForATakeDownThatIsRunning(t *testing.T) {
	for _, lock := range []string{"never-put-up", "running", "finished", "missing-run-lock"} {
		t.Run(lock, func(t *testing.T) {
			cfg, _, dir, kept := retentionFixture(t)
			issue, _ := cfg.source().ReadIssue(kept["issue.json"])
			path := filepath.Join(dir, previewDownLock)
			switch lock {
			case "running", "finished":
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
				if err != nil {
					t.Fatal(err)
				}
				defer file.Close()
				if lock == "running" {
					if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
						t.Fatal(err)
					}
				}
			case "missing-run-lock":
				if err := os.Remove(filepath.Join(dir, "run/runner.lock")); err != nil {
					t.Fatal(err)
				}
			}
			_, err := reclaimStoppedWorkspace(cfg, issue, dir, time.Now())
			_, recorded := os.Stat(filepath.Join(dir, removalFile))
			switch lock {
			case "never-put-up", "finished":
				if err != nil || recorded != nil {
					t.Fatalf("the reclaim did not go on: %v %v", err, recorded)
				}
			case "running":
				if err == nil || !strings.Contains(err.Error(), "owned") || recorded == nil {
					t.Fatalf("the reclaim did not stop at the running take-down: %v %v", err, recorded)
				}
			case "missing-run-lock":
				if !errors.Is(err, os.ErrNotExist) || recorded == nil {
					t.Fatalf("a missing lock the reclaim already waited on is read differently: %v %v", err, recorded)
				}
			}
		})
	}
}
