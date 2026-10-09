package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"ticket-runner/internal/chain"
)

type manualWorkTimer struct {
	mu     sync.Mutex
	at     time.Time
	f      func()
	closed bool
}

func (t *manualWorkTimer) Stop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return false
	}
	t.closed = true
	return true
}

type manualWorkTime struct {
	mu     sync.Mutex
	now    time.Time
	timers []*manualWorkTimer
}

func newWorkTime() *manualWorkTime {
	return &manualWorkTime{now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
}

func (c *manualWorkTime) source() workTimeSource {
	return workTimeSource{now: func() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }, after: func(d time.Duration, f func()) workTimer {
		c.mu.Lock()
		defer c.mu.Unlock()
		timer := &manualWorkTimer{at: c.now.Add(d), f: f}
		c.timers = append(c.timers, timer)
		return timer
	}}
}

func (c *manualWorkTime) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var due []func()
	for _, timer := range c.timers {
		timer.mu.Lock()
		if !timer.closed && !timer.at.After(c.now) {
			timer.closed = true
			due = append(due, timer.f)
		}
		timer.mu.Unlock()
	}
	c.mu.Unlock()
	for _, f := range due {
		f()
	}
}

func useWorkTime(t *testing.T, clock *manualWorkTime) {
	t.Helper()
	original := activeWorkTime
	activeWorkTime = clock.source()
	t.Cleanup(func() { activeWorkTime = original })
}

func loadWorkLimit(t *testing.T, directory string) workLimitRecord {
	t.Helper()
	record, err := readWorkLimit(directory)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestWorkLimitSettingsAndAcceptanceRemainOwnerScoped(t *testing.T) {
	for _, minutes := range []int{0, 1, 90, -1, int(time.Duration(1<<63-1)/time.Minute) + 1} {
		cfg := watchConfiguration(t)
		cfg.Intake.MaxActiveMinutes = minutes
		root := filepath.Join(t.TempDir(), "untouched")
		_, _, _, _, err := watchSettings(&cfg, root)
		if (err == nil) != validWorkMinutes(minutes) {
			t.Fatalf("minutes=%d error=%v", minutes, err)
		}
		if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("settings touched the queue")
		}
	}
	directory := t.TempDir()
	if err := acceptWorkLimit(directory, 3, 0); err != nil {
		t.Fatal(err)
	}
	if err := acceptWorkLimit(directory, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := acceptWorkLimit(directory, 7, 0); err != nil {
		t.Fatal(err)
	}
	if got := loadWorkLimit(t, directory); got.Clock.MaxMinutes != 3 || got.Clock.MaxHardExits != 3 {
		t.Fatal("a staged acceptance changed its saved cap")
	}
	if err := writeRuntimeFile(filepath.Join(directory, workLimitFile), []byte("broken")); err != nil {
		t.Fatal(err)
	}
	if err := acceptWorkLimit(directory, 7, 0); err == nil {
		t.Fatal("a damaged interrupted acceptance was replaced")
	}
	unlimited := t.TempDir()
	if err := acceptWorkLimit(unlimited, 0, 0); err != nil {
		t.Fatal(err)
	}
	if got := loadWorkLimit(t, unlimited); got.Clock != nil {
		t.Fatal("zero created a cap")
	}
	clock := newWorkTime()
	work, err := beginActiveWork(unlimited, func() { t.Fatal("unlimited work was canceled") }, clock.source(), 0)
	if err != nil || work == nil || work.timer != nil {
		t.Fatalf("unlimited request was given a timer or no crash marker: %v %v", work, err)
	}
	if got := loadWorkLimit(t, unlimited); got.Clock != nil || got.StageExits == nil || got.StageExits.Max != 3 || got.StageExits.Active == nil || got.StageExits.Count != 0 {
		t.Fatalf("a launch without a time limit did not keep only its crash marker: %+v", got.StageExits)
	}
	clock.advance(24 * time.Hour)
	if err := work.finish(clock.source().now()); err != nil {
		t.Fatal(err)
	}
	if got := loadWorkLimit(t, unlimited); got.Clock != nil || got.StageExits.Active != nil || got.StageExits.Count != 0 || got.held() {
		t.Fatalf("a run that returned was counted or charged: %+v", got.StageExits)
	}
}

func TestWorkLimitIntakePublishesTheCapBeforeTheIssue(t *testing.T) {
	for _, broken := range []bool{false, true} {
		t.Run(map[bool]string{false: "saved", true: "save fails"}[broken], func(t *testing.T) {
			cfg := watchConfiguration(t)
			cfg.Intake.MaxActiveMinutes = 4
			jobs := t.TempDir()
			directory := filepath.Join(jobs, "51")
			if broken {
				if err := os.MkdirAll(filepath.Join(directory, workLimitFile), 0700); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			useWatchTransport(t, func(r *http.Request) (*http.Response, error) {
				return selectionReply(r, 200, []any{watchedIssue(51, "fixture", "2026-01-03T00:00:00Z")}), nil
			})
			since, _ := time.Parse(time.RFC3339, cfg.Intake.CreatedSince)
			collectIssues(ctx, cfg, jobs, since, time.Millisecond, make(chan struct{}, 1), func(message string) {
				if strings.HasPrefix(message, "accepted ") {
					stored := loadWorkLimit(t, directory)
					if broken || stored.Clock == nil || stored.Clock.MaxMinutes != 4 {
						t.Error("issue was published without its cap")
					}
					cancel()
				}
				if strings.Contains(message, "saving accepted work limit") {
					cancel()
				}
			})
			_, err := os.Stat(filepath.Join(directory, "issue.json"))
			if broken != errors.Is(err, os.ErrNotExist) {
				t.Fatalf("issue visibility: %v", err)
			}
		})
	}
}

func TestWorkLimitAccumulatesRunsAndExcludesEveryIdleGap(t *testing.T) {
	directory := t.TempDir()
	if err := acceptWorkLimit(directory, 1, 0); err != nil {
		t.Fatal(err)
	}
	clock := newWorkTime()
	var canceled atomic.Bool
	for _, elapsed := range []time.Duration{20 * time.Second, 15 * time.Second, 25 * time.Second} {
		clock.advance(9 * time.Hour) // slots, answers, credit and normal downtime
		work, err := beginActiveWork(directory, func() { canceled.Store(true) }, clock.source(), 0)
		if err != nil {
			t.Fatal(err)
		}
		clock.advance(elapsed)
		if err := work.finish(clock.source().now()); err != nil {
			t.Fatal(err)
		}
	}
	record := loadWorkLimit(t, directory)
	if record.Clock.Elapsed != time.Minute || record.Clock.Active != nil || !record.held() || !canceled.Load() || record.Pauses[0].Reason != activeLimitPause {
		t.Fatalf("clock=%+v pauses=%+v canceled=%t", record.Clock, record.Pauses, canceled.Load())
	}
	if record.Clock.MaxMinutes != 1 || record.Pauses[0].Elapsed != time.Minute {
		t.Fatal("pause lost measured values")
	}
	if _, err := beginActiveWork(directory, func() {}, clock.source(), 0); !errors.Is(err, errWorkHeld) {
		t.Fatalf("cap relaunched: %v", err)
	}
	text := pausedWorkText(record.Pauses[0], record.Clock, nil)
	if !strings.Contains(text, "確定済みの実稼働時間は 1 分 0 秒 です") {
		t.Fatalf("pause does not use requester-facing time units: %s", text)
	}
}

func TestWorkLimitHardExitIsUnknownAndNormalExitKeepsRemainingTime(t *testing.T) {
	for _, crash := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal restart", true: "forced exit"}[crash], func(t *testing.T) {
			directory := t.TempDir()
			if err := acceptWorkLimit(directory, 2, 0); err != nil {
				t.Fatal(err)
			}
			clock := newWorkTime()
			work, err := beginActiveWork(directory, func() {}, clock.source(), 0)
			if err != nil {
				t.Fatal(err)
			}
			clock.advance(30 * time.Second)
			if !crash {
				if err := work.finish(clock.source().now()); err != nil {
					t.Fatal(err)
				}
			} else {
				work.timer.Stop()
			}
			clock.advance(24 * time.Hour)
			if err := recoverWorkClock(directory); err != nil {
				t.Fatal(err)
			}
			record := loadWorkLimit(t, directory)
			if crash {
				if record.held() || record.Clock.Elapsed != 0 || record.Clock.Active != nil {
					t.Fatalf("unknown interval invented: %+v", record)
				}
			} else {
				if record.held() || record.Clock.Elapsed != 30*time.Second || record.Clock.Active != nil {
					t.Fatalf("normal restart: %+v", record)
				}
				var canceled bool
				next, err := beginActiveWork(directory, func() { canceled = true }, clock.source(), 0)
				if err != nil {
					t.Fatal(err)
				}
				clock.advance(89 * time.Second)
				if canceled {
					t.Fatal("remaining time shortened")
				}
				clock.advance(time.Second)
				if !canceled {
					t.Fatal("remaining time reset on restart")
				}
				if err := next.finish(clock.source().now()); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestWorkLimitHardExitRecoveryRunsThenPausesAtSavedCountAndResets(t *testing.T) {
	for _, configured := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("configured=%d", configured), func(t *testing.T) {
			cfg := watchConfiguration(t)
			holdingWorker(&cfg)
			clock := newWorkTime()
			useWorkTime(t, clock)
			remote := &noticeTracker{}
			remote.install(t, alwaysChoose("implement"))
			root, directory := noticeJob(t, chain.State{})
			if err := acceptWorkLimit(directory, 2, configured); err != nil {
				t.Fatal(err)
			}
			issue := sourceIssue{ID: 51, Key: "EXAMPLE-51"}
			issue.Creator.ID = 55
			limit := configured
			if limit == 0 {
				limit = 3
			}
			for count := 1; count <= limit; count++ {
				record := loadWorkLimit(t, directory)
				at := clock.source().now().Add(-24 * time.Hour)
				record.Clock.Active = &at
				if count == 1 {
					record.Clock.Elapsed = 30 * time.Second
				}
				measured := record.Clock.Elapsed
				if err := saveWorkLimit(directory, record); err != nil {
					t.Fatal(err)
				}
				for tick := 0; tick < 2; tick++ {
					held, err := holdPausedRequest(context.Background(), cfg, issue, directory, noticeRequest, time.Second, func(string) {})
					if err != nil || held != (count == limit) {
						t.Fatalf("count=%d tick=%d held=%t err=%v", count, tick, held, err)
					}
				}
				recovered := loadWorkLimit(t, directory)
				if recovered.Clock.HardExits != count || recovered.Clock.MaxHardExits != limit || recovered.Clock.Elapsed != measured || recovered.Clock.Active != nil {
					t.Fatalf("recovery counted twice, invented time or changed cap: %+v", recovered.Clock)
				}
				if count < limit {
					posts := remote.withPrefix("強制終了から自動で再開")
					if len(posts) != count || !strings.Contains(posts[count-1], fmt.Sprintf("%d 回目、上限 %d 回", count, limit)) {
						t.Fatalf("recovery notice: %v", posts)
					}
				}
				if count == 1 && count < limit {
					finish := startStopQueue(t, cfg, root, 10*time.Millisecond, io.Discard)
					pid := waitTestPID(t, filepath.Join(directory, "workspace", "child-pid"))
					clock.advance(89 * time.Second)
					if err := syscall.Kill(pid, 0); err != nil {
						t.Fatal("recovered child lost remaining time")
					}
					finish()
					if saved := loadWorkLimit(t, directory); saved.held() || saved.Clock.Elapsed != 119*time.Second {
						t.Fatalf("remaining time not used: %+v", saved.Clock)
					}
					t.Logf("forced exit 1: child started without a reply; measured 30s -> 1m59s; limit=%d", limit)
				}
			}
			record := loadWorkLimit(t, directory)
			if record.Pauses[0].Reason != hardExitPause || loadJobState(t, directory).Done {
				t.Fatal("count limit became completion")
			}
			pauses := remote.withPrefix("この依頼の自動処理")
			if len(pauses) != 1 || !strings.Contains(pauses[0], fmt.Sprintf("上限の %d 回", limit)) {
				t.Fatalf("pause did not name its limit: %v", pauses)
			}
			if !strings.Contains(pauses[0], "確定済みの実稼働時間は "+spentText(record.Pauses[0].Elapsed)+" です") {
				t.Fatalf("posted pause does not use requester-facing time units: %v", pauses)
			}
			remote.mu.Lock()
			remote.rows = append(remote.rows, issueComment(990, 55, "再開"))
			remote.mu.Unlock()
			held, err := holdPausedRequest(context.Background(), cfg, issue, directory, noticeRequest, time.Second, func(string) {})
			after := loadWorkLimit(t, directory)
			if err != nil || held || after.Clock.HardExits != 0 || after.Clock.Elapsed != 0 || after.Clock.MaxHardExits != limit {
				t.Fatalf("resume did not reset just this allowance: held=%t err=%v clock=%+v", held, err, after.Clock)
			}
			t.Logf("forced exit %d: paused, not done; authorized resume -> count=0 measured=0 saved limit=%d", limit, limit)
		})
	}
}

func TestWorkLimitSaveFailureStillCancelsAndRetainsRecoveryEvidence(t *testing.T) {
	for _, retry := range []bool{false, true} {
		t.Run(map[bool]string{false: "persistent", true: "final save recovers"}[retry], func(t *testing.T) {
			directory := t.TempDir()
			if err := acceptWorkLimit(directory, 1, 0); err != nil {
				t.Fatal(err)
			}
			clock := newWorkTime()
			var canceled bool
			work, err := beginActiveWork(directory, func() { canceled = true }, clock.source(), 0)
			if err != nil {
				t.Fatal(err)
			}
			work.save = func(string, workLimitRecord) error { return errors.New("synthetic storage failure") }
			clock.advance(time.Minute)
			if !canceled {
				t.Fatal("storage failure kept the child running")
			}
			if retry {
				work.save = saveWorkLimit
			}
			err = work.finish(clock.source().now())
			if (err == nil) != retry {
				t.Fatalf("final save: %v", err)
			}
			before := loadWorkLimit(t, directory)
			if !retry && before.Clock.Active == nil {
				t.Fatal("failed final save cleared the crash marker")
			}
			if err := recoverWorkClock(directory); err != nil {
				t.Fatal(err)
			}
			after := loadWorkLimit(t, directory)
			if retry && (!after.held() || after.Pauses[0].Reason != activeLimitPause) ||
				!retry && (after.held() || after.Clock.Active != nil || after.Clock.HardExits != 1 || after.Clock.RecoveryNoticeAt == nil) {
				t.Fatalf("save recovery: %+v", after)
			}
		})
	}
}

func TestWorkLimitCancellationDoesNotWaitForStorage(t *testing.T) {
	directory := t.TempDir()
	if err := acceptWorkLimit(directory, 1, 0); err != nil {
		t.Fatal(err)
	}
	clock := newWorkTime()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	work, err := beginActiveWork(directory, cancel, clock.source(), 0)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	work.save = func(string, workLimitRecord) error {
		<-release
		return errors.New("storage unavailable")
	}
	advanced := make(chan struct{})
	go func() { clock.advance(time.Minute); close(advanced) }()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Error("blocked storage prevented cancellation at the active-work limit")
	}
	close(release)
	<-advanced
	work.save = saveWorkLimit
	if err := work.finish(clock.source().now()); err != nil {
		t.Fatal(err)
	}
	if loadWorkLimit(t, directory).Clock.Active != nil {
		t.Fatal("actual child completion was not retained")
	}
}

func TestWorkLimitResumeResetsOnlyItsAppliedTransition(t *testing.T) {
	for _, historyWritten := range []bool{false, true} {
		t.Run(map[bool]string{false: "before history", true: "after history"}[historyWritten], func(t *testing.T) {
			state := chain.State{Step: "verify", Recovering: true, Waiting: true, Pending: &chain.Assignment{Role: "verify", Instruction: "inspect prior effects"}}
			cfg, issue, directory, record := pausedFixture(t, state)
			at := time.Date(2026, 1, 2, 3, 5, 0, 0, time.UTC)
			record.Clock = &workClock{MaxHardExits: 3, HardExits: 3, MaxMinutes: 2, Elapsed: 2 * time.Minute, Active: &at}
			record.Pauses[0].Elapsed = 2 * time.Minute
			raw := issueComment(901, 55, "再開")
			record.Pauses[0].Resume = &pauseResume{Comment: raw, HistoryIndex: 0, RecordedAt: at}
			if historyWritten {
				state.History = []chain.Result{{Speaker: "requester", Output: "再開", FinishedAt: at}}
				writeJobHistory(t, directory, state)
			}
			if err := saveWorkLimit(directory, record); err != nil {
				t.Fatal(err)
			}
			if err := applyPauseResume(context.Background(), cfg, issue, directory, noticeRequest, &record, nil); err != nil {
				t.Fatal(err)
			}
			got := loadWorkLimit(t, directory)
			if got.Clock.Elapsed != 0 || got.Clock.Active != nil || got.Clock.MaxMinutes != 2 || got.Clock.HardExits != 0 || got.Pauses[0].Elapsed != 2*time.Minute {
				t.Fatalf("new interval: %+v", got)
			}
			after := loadJobState(t, directory)
			if !after.Waiting || !after.Recovering || !reflect.DeepEqual(after.Pending, state.Pending) || after.Step != "verify" || len(after.History) != 1 || after.History[0].Role != "" {
				t.Fatalf("work changed: %+v", after)
			}
			clock := newWorkTime()
			work, err := beginActiveWork(directory, func() {}, clock.source(), 0)
			if err != nil {
				t.Fatal(err)
			}
			clock.advance(5 * time.Second)
			if err := work.finish(clock.source().now()); err != nil {
				t.Fatal(err)
			}
			got = loadWorkLimit(t, directory)
			if err := applyPauseResume(context.Background(), cfg, issue, directory, noticeRequest, &got, nil); err != nil {
				t.Fatal(err)
			}
			if loadWorkLimit(t, directory).Clock.Elapsed != 5*time.Second {
				t.Fatal("reading an applied resume reset the new interval")
			}
		})
	}
}

func TestWorkLimitDamagedClockDoesNotRunAndAuthorizedStopWins(t *testing.T) {
	for _, clock := range []any{map[string]any{"max_minutes": -1}, map[string]any{"max_minutes": 1, "elapsed_ns": -1}, map[string]any{"max_minutes": 1, "active_since": "0001-01-01T00:00:00Z"}} {
		directory := t.TempDir()
		raw, _ := json.Marshal(map[string]any{"version": 1, "clock": clock})
		if err := writeRuntimeFile(filepath.Join(directory, workLimitFile), raw); err != nil {
			t.Fatal(err)
		}
		if _, err := beginActiveWork(directory, func() {}, newWorkTime().source(), 0); err == nil {
			t.Fatal("damaged clock started")
		}
	}
	cfg, issue, directory, _ := pausedFixture(t, chain.State{})
	if err := writeRuntimeFile(filepath.Join(directory, workLimitFile), []byte("broken")); err != nil {
		t.Fatal(err)
	}
	tracker := &noticeTracker{rows: []json.RawMessage{issueComment(905, 55, "停止")}}
	tracker.install(t, nil)
	if held, err := holdPausedRequest(context.Background(), cfg, issue, directory, noticeRequest, time.Second, func(string) {}); !held || err != nil {
		t.Fatalf("stop: %t %v", held, err)
	}
	if stopped, err := savedStop(cfg.source(), directory, issue, nil); !stopped || err != nil {
		t.Fatalf("stop not retained: %t %v", stopped, err)
	}
}

func TestWorkLimitCancelsARealChildDespiteBlockedControlReads(t *testing.T) {
	cfg := watchConfiguration(t)
	cfg.Intake.MaxActiveMinutes = 1
	cfg.Roles[0].Processes[0].Command = []string{"/bin/sh", "-c", `cat > received.txt; printf '%s' "$$" > child-pid; exec sleep 60`}
	clock := newWorkTime()
	useWorkTime(t, clock)
	var block atomic.Bool
	entered := make(chan struct{})
	var once sync.Once
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "watch-tracker.example" {
			if strings.HasSuffix(r.URL.Path, "/comments") {
				if r.Method == http.MethodGet && block.Load() {
					once.Do(func() { close(entered) })
					<-r.Context().Done()
					return nil, r.Context().Err()
				}
				if r.Method == http.MethodPost {
					return selectionReply(r, 200, map[string]any{"id": 900}), nil
				}
				return selectionReply(r, 200, []any{}), nil
			}
			return selectionReply(r, 200, []any{watchedIssue(51, "fixture", "2026-01-03T00:00:00Z")}), nil
		}
		return selectionReply(r, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": "implement"}}}), nil
	})
	root := t.TempDir()
	var log lockedLog
	t.Cleanup(func() { log.mu.Lock(); defer log.mu.Unlock(); t.Logf("queue: %s", log.text.String()) })
	finish := startStopQueue(t, cfg, root, 80*time.Millisecond, &log)
	directory := filepath.Join(root, "jobs", "51")
	pid := waitTestPID(t, filepath.Join(directory, "workspace", "child-pid"))
	block.Store(true)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("control read did not block")
	}
	clock.advance(time.Minute)
	waitFor(t, func() bool { return syscall.Kill(pid, 0) == syscall.ESRCH })
	waitFor(t, func() bool { r, e := readWorkLimit(directory); return e == nil && r.held() && r.Clock.Active == nil })
	finish()
	if log.starts() != 1 {
		t.Fatal("expired child was relaunched")
	}
	if got := loadWorkLimit(t, directory); got.Clock.Elapsed != time.Minute || got.Pauses[0].Reason != activeLimitPause {
		t.Fatalf("canceled child clock: %+v", got)
	}
}

func TestWorkLimitQueueNormalRestartRetainsTheSavedCapAndRemainingTime(t *testing.T) {
	cfg := watchConfiguration(t)
	cfg.Intake.MaxActiveMinutes = 1
	holdingWorker(&cfg)
	clock := newWorkTime()
	useWorkTime(t, clock)
	fixture := &noticeTracker{}
	fixture.install(t, alwaysChoose("implement"))
	root, directory := noticeJob(t, chain.State{})
	if err := acceptWorkLimit(directory, 1, 0); err != nil {
		t.Fatal(err)
	}
	finish := startStopQueue(t, cfg, root, 10*time.Millisecond, io.Discard)
	path := filepath.Join(directory, "workspace", "child-pid")
	pid := waitTestPID(t, path)
	clock.advance(30 * time.Second)
	finish()
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatal("normal shutdown did not reap the child")
	}
	before := loadWorkLimit(t, directory)
	if before.Clock.Elapsed != 30*time.Second || before.Clock.Active != nil || before.held() {
		t.Fatalf("normal shutdown did not close its interval: %+v", before)
	}
	clock.advance(24 * time.Hour)
	cfg.Intake.MaxActiveMinutes = 9
	finishAgain := startStopQueue(t, cfg, root, 10*time.Millisecond, io.Discard)
	var nextPID int
	waitFor(t, func() bool {
		raw, _ := os.ReadFile(path)
		nextPID, _ = strconv.Atoi(string(raw))
		return nextPID > 0 && nextPID != pid
	})
	clock.advance(29 * time.Second)
	if err := syscall.Kill(nextPID, 0); err != nil {
		t.Fatal("restarted child lost its remaining time")
	}
	clock.advance(time.Second)
	waitFor(t, func() bool { return errors.Is(syscall.Kill(nextPID, 0), syscall.ESRCH) })
	waitFor(t, func() bool { r, e := readWorkLimit(directory); return e == nil && r.Clock.Active == nil && r.held() })
	finishAgain()
	after := loadWorkLimit(t, directory)
	if after.Clock.MaxMinutes != 1 || after.Clock.Elapsed != time.Minute || after.Pauses[0].Reason != activeLimitPause {
		t.Fatalf("restart replaced the cap or reset time: %+v", after)
	}
}

func TestWorkLimitBudgetRecoveryDoesNotResetTheInterval(t *testing.T) {
	cfg := watchConfiguration(t)
	cfg.Intake.MinModelCredit = 5
	holdingWorker(&cfg)
	clock := newWorkTime()
	useWorkTime(t, clock)
	var low atomic.Bool
	server := creditServer(t, func() string {
		if low.Load() {
			return `{"data":{"limit_remaining":1}}`
		}
		return `{"data":{"limit_remaining":20}}`
	})
	cfg.Intake.ModelCreditURL, cfg.Intake.Client = server.URL, server.Client()
	fixture := &noticeTracker{}
	fixture.install(t, alwaysChoose("implement"))
	root, directory := noticeJob(t, chain.State{})
	if err := acceptWorkLimit(directory, 1, 0); err != nil {
		t.Fatal(err)
	}
	finish := startStopQueue(t, cfg, root, 10*time.Millisecond, io.Discard)
	path := filepath.Join(directory, "workspace", "child-pid")
	pid := waitTestPID(t, path)
	clock.advance(20 * time.Second)
	low.Store(true)
	waitFor(t, func() bool { r, e := readWorkLimit(directory); return e == nil && r.Clock.Active == nil })
	if !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		t.Fatal("credit hold did not reap child before closing time")
	}
	clock.advance(12 * time.Hour)
	if r := loadWorkLimit(t, directory); r.Clock.Elapsed != 20*time.Second || r.held() {
		t.Fatalf("credit wait changed delegated interval: %+v", r)
	}
	low.Store(false)
	var nextPID int
	waitFor(t, func() bool {
		raw, _ := os.ReadFile(path)
		nextPID, _ = strconv.Atoi(string(raw))
		return nextPID > 0 && nextPID != pid
	})
	clock.advance(40 * time.Second)
	waitFor(t, func() bool { return errors.Is(syscall.Kill(nextPID, 0), syscall.ESRCH) })
	finish()
	if r := loadWorkLimit(t, directory); !r.held() || r.Clock.Elapsed != time.Minute {
		t.Fatalf("credit recovery reset time: %+v", r)
	}
}

func TestWorkLimitSuccessfulAndFailedStagesCannotResetTheClock(t *testing.T) {
	for _, failures := range []bool{false, true} {
		t.Run(map[bool]string{false: "successful repetition", true: "failure and success"}[failures], func(t *testing.T) {
			cfg := watchConfiguration(t)
			if failures {
				cfg.Roles[0].Processes[0].Command = []string{"/bin/sh", "-c", `cat > received.txt; if test -f once; then rm once; exit 0; else touch once; exit 1; fi`}
			}
			clock := newWorkTime()
			useWorkTime(t, clock)
			fixture := &noticeTracker{}
			var calls atomic.Int64
			fixture.install(t, func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				clock.advance(10 * time.Second)
				return alwaysChoose("implement")(r)
			})
			root, directory := noticeJob(t, chain.State{})
			if err := acceptWorkLimit(directory, 1, 0); err != nil {
				t.Fatal(err)
			}
			finish := startStopQueue(t, cfg, root, 10*time.Millisecond, io.Discard)
			waitFor(t, func() bool { r, e := readWorkLimit(directory); return e == nil && r.Clock.Active == nil && r.held() })
			finish()
			if calls.Load() != 6 {
				t.Fatalf("loop escaped the one interval: %d decisions", calls.Load())
			}
			state := loadJobState(t, directory)
			var successes, failed int
			for _, row := range state.History {
				if row.Role == "implement" && row.Speaker == "worker" {
					if row.Error == "" {
						successes++
					} else {
						failed++
					}
				}
			}
			if successes == 0 || (failures && failed == 0) || state.Done {
				t.Fatalf("fixture did not exercise the claimed outcomes: success=%d failure=%d done=%t", successes, failed, state.Done)
			}
		})
	}
}

func TestWorkLimitQuestionReceiptSurvivesTimeoutAndAnExplicitResume(t *testing.T) {
	cfg, issue, directory, record := pausedFixture(t, chain.State{})
	record.Pauses = nil
	record.Clock = &workClock{MaxHardExits: 3, MaxMinutes: 1}
	if err := saveWorkLimit(directory, record); err != nil {
		t.Fatal(err)
	}
	clock := newWorkTime()
	work, err := beginActiveWork(directory, func() {}, clock.source(), 0)
	if err != nil {
		t.Fatal(err)
	}
	state := chain.State{Waiting: true, Step: "ask", Pending: &chain.Assignment{Role: "verify", Instruction: "inspect prior effect"}, Recovering: true}
	writeJobHistory(t, directory, state)
	if err := writeRuntimeFile(filepath.Join(directory, "run", "question-post.json"), []byte(`{"after":700}`)); err != nil {
		t.Fatal(err)
	}
	clock.advance(time.Minute)
	if err := work.finish(clock.source().now()); err != nil {
		t.Fatal(err)
	}
	fixture := &noticeTracker{rows: []json.RawMessage{issueComment(700, 99, "question"), issueComment(701, 55, "ordinary answer")}}
	fixture.install(t, nil)
	if held, err := holdPausedRequest(context.Background(), cfg, issue, directory, noticeRequest, time.Second, func(string) {}); !held || err != nil {
		t.Fatalf("pause: %t %v", held, err)
	}
	stored := loadWorkLimit(t, directory)
	resumeID := stored.Pauses[0].NoticeID + 1
	fixture.mu.Lock()
	fixture.rows = append(fixture.rows, issueComment(resumeID, 55, "再開"))
	fixture.mu.Unlock()
	if held, err := holdPausedRequest(context.Background(), cfg, issue, directory, noticeRequest, time.Second, func(string) {}); held || err != nil {
		t.Fatalf("resume: %t %v", held, err)
	}
	current := loadJobState(t, directory)
	resumed, answered, err := resumeWaitingRequest(context.Background(), cfg, issue, directory, noticeRequest, current, time.Second)
	if err != nil || !resumed || !answered {
		t.Fatalf("question: resumed=%t answered=%t error=%v", resumed, answered, err)
	}
	current = loadJobState(t, directory)
	if len(current.History) != 2 || current.History[1].Output != "ordinary answer" {
		t.Fatalf("timeout lost the true question receipt: %+v", current.History)
	}
	if r := loadWorkLimit(t, directory); r.Clock.Elapsed != 0 || r.Clock.MaxMinutes != 1 {
		t.Fatal("ordinary answer changed the new saved interval")
	}
}

func TestWorkLimitRealTimerAndStartMarkerPrecedeTheChild(t *testing.T) {
	directory := t.TempDir()
	if err := saveWorkLimit(directory, workLimitRecord{Version: 1, Clock: &workClock{MaxHardExits: 3, MaxMinutes: 1, Elapsed: time.Minute - 25*time.Millisecond}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	work, err := beginActiveWork(directory, cancel, activeWorkTime, 0)
	if err != nil {
		t.Fatal(err)
	}
	if loadWorkLimit(t, directory).Clock.Active == nil {
		t.Fatal("child could start without the crash marker")
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("local timer did not cancel the child")
	}
	if err := work.finish(time.Now()); err != nil {
		t.Fatal(err)
	}
	if r := loadWorkLimit(t, directory); !r.held() || r.Clock.Active != nil || r.Clock.Elapsed < time.Minute {
		t.Fatalf("real timer was not retained: %+v", r)
	}
}

func TestWorkLimitLateTimerCannotChargeTimeAfterTheChildEnded(t *testing.T) {
	directory := t.TempDir()
	if err := acceptWorkLimit(directory, 1, 0); err != nil {
		t.Fatal(err)
	}
	clock := newWorkTime()
	work, err := beginActiveWork(directory, func() {}, clock.source(), 0)
	if err != nil {
		t.Fatal(err)
	}
	clock.advance(40 * time.Second)
	ended := clock.source().now()
	clock.advance(20 * time.Second)
	if err := work.finish(ended); err != nil {
		t.Fatal(err)
	}
	if r := loadWorkLimit(t, directory); r.Clock.Elapsed != 40*time.Second || r.Clock.Active != nil || r.held() {
		t.Fatalf("time after return was counted: %+v", r)
	}
}

func TestWorkLimitQueuedRequestStartsAfterTheTimedOutChildIsReaped(t *testing.T) {
	cfg := watchConfiguration(t)
	cfg.Intake.MaxActiveMinutes = 1
	cfg.Intake.MaxRunning = 1
	holdingWorker(&cfg)
	clock := newWorkTime()
	useWorkTime(t, clock)
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "watch-tracker.example" {
			if strings.HasSuffix(r.URL.Path, "/comments") {
				if r.Method == http.MethodPost {
					if err := r.ParseForm(); err != nil {
						return nil, err
					}
					id := 51
					if strings.Contains(r.URL.Path, "/52/") {
						id = 52
					}
					return selectionReply(r, 201, map[string]any{"id": 900, "issueId": id, "projectId": 17, "createdUser": map[string]any{"id": 99}, "content": r.Form.Get("content")}), nil
				}
				return selectionReply(r, 200, []any{}), nil
			}
			return selectionReply(r, 200, []any{watchedIssue(51, "first fixture", "2026-01-03T00:00:00Z"), watchedIssue(52, "second fixture", "2026-01-03T00:00:01Z")}), nil
		}
		return alwaysChoose("implement")(r)
	})
	root := t.TempDir()
	finish := startStopQueue(t, cfg, root, 10*time.Millisecond, io.Discard)
	first := filepath.Join(root, "jobs", "51")
	second := filepath.Join(root, "jobs", "52")
	pid := waitTestPID(t, filepath.Join(first, "workspace", "child-pid"))
	waitFor(t, func() bool { r, e := readWorkLimit(second); return e == nil && r.Clock != nil })
	if r := loadWorkLimit(t, second); r.Clock.Active != nil || r.Clock.Elapsed != 0 {
		t.Fatal("waiting for a slot started the clock")
	}
	clock.advance(time.Minute)
	nextPID := waitTestPID(t, filepath.Join(second, "workspace", "child-pid"))
	if !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		t.Fatal("the slot was returned before the child ended")
	}
	if err := syscall.Kill(nextPID, 0); err != nil {
		t.Fatal("the next request did not retain its own interval")
	}
	finish()
	if r := loadWorkLimit(t, second); r.Clock.Elapsed != 0 || r.held() {
		t.Fatalf("slot wait was charged: %+v", r)
	}
}

func TestWorkLimitCompletedAndStoppedRequestsRemainTerminal(t *testing.T) {
	for _, done := range []bool{false, true} {
		t.Run(fmt.Sprint("done=", done), func(t *testing.T) {
			cfg := watchConfiguration(t)
			fixture := &noticeTracker{}
			if !done {
				fixture.rows = []json.RawMessage{issueComment(903, 55, "停止")}
			}
			fixture.install(t, func(*http.Request) (*http.Response, error) {
				t.Error("terminal request invoked a model")
				return nil, errors.New("unexpected model")
			})
			root, directory := noticeJob(t, chain.State{Done: done})
			at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
			if err := saveWorkLimit(directory, workLimitRecord{Version: 1, Clock: &workClock{MaxHardExits: 3, MaxMinutes: 1, Elapsed: time.Minute, Active: &at}}); err != nil {
				t.Fatal(err)
			}
			var log lockedLog
			finish := startStopQueue(t, cfg, root, 10*time.Millisecond, &log)
			waitFor(t, func() bool { return fixture.readings() >= 12 })
			finish()
			if log.starts() != 0 || len(fixture.withPrefix("この依頼の自動処理")) != 0 {
				t.Fatal("timer obscured stop or delivery")
			}
			if !done {
				issue := sourceIssue{ID: 51}
				issue.Creator.ID = 55
				if stopped, err := savedStop(cfg.source(), directory, issue, nil); !stopped || err != nil {
					t.Fatalf("native stop lost: %t %v", stopped, err)
				}
			}
		})
	}
}

func TestWorkLimitStartNoticeRequiresALaunchedChildAndCannotBlockItsTimer(t *testing.T) {
	for _, saveFails := range []bool{true, false} {
		t.Run(map[bool]string{true: "start marker cannot be saved", false: "start notice is blocked"}[saveFails], func(t *testing.T) {
			cfg := watchConfiguration(t)
			cfg.Intake.Announce = true
			holdingWorker(&cfg)
			_, directory := noticeJob(t, chain.State{})
			if err := acceptWorkLimit(directory, 1, 0); err != nil {
				t.Fatal(err)
			}
			issue := sourceIssue{ID: 51, Key: "EXAMPLE-51"}
			issue.Creator.ID = 55
			bound, err := bindRequestConfig(cfg, directory, issue.Key)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(directory, "workspace"), 0700); err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(bound)
			if err != nil {
				t.Fatal(err)
			}
			configPath, requestPath := filepath.Join(directory, "engine.json"), filepath.Join(directory, "request.txt")
			if err := writeRuntimeFile(configPath, data); err != nil {
				t.Fatal(err)
			}
			if err := writeRuntimeFile(requestPath, []byte(noticeRequest)); err != nil {
				t.Fatal(err)
			}
			clock := newWorkTime()
			useWorkTime(t, clock)
			if saveFails {
				var once sync.Once
				activeWorkTime.now = func() time.Time {
					once.Do(func() {
						path := filepath.Join(directory, workLimitFile)
						if err := os.Rename(path, filepath.Join(directory, "retained-clock.json")); err != nil {
							t.Error(err)
						}
						if err := os.Mkdir(path, 0700); err != nil {
							t.Error(err)
						}
					})
					return clock.source().now()
				}
			}
			var reads, posts, models atomic.Int64
			var postReturned atomic.Bool
			entered := make(chan struct{})
			var once sync.Once
			useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
				if r.URL.Host == "watch-tracker.example" {
					if r.Method == http.MethodPost {
						posts.Add(1)
						once.Do(func() { close(entered) })
						<-r.Context().Done()
						postReturned.Store(true)
						return nil, r.Context().Err()
					}
					reads.Add(1)
					return selectionReply(r, 200, []any{}), nil
				}
				models.Add(1)
				return alwaysChoose("implement")(r)
			})
			turns := newTurnstile(1)
			if !turns.try(1) {
				t.Fatal("fixture did not occupy the only slot")
			}
			turns.enter(issue.ID)
			ctx, cancel := context.WithCancel(context.Background())
			result := make(chan error, 1)
			var log lockedLog
			go func() {
				result <- runWatchedRequest(ctx, cfg, issue, directory, configPath, requestPath, 10*time.Millisecond, turns, &log)
			}()
			var finishOnce sync.Once
			finish := func() {
				finishOnce.Do(func() {
					cancel()
					select {
					case <-result:
					case <-time.After(3 * time.Second):
						t.Error("watcher did not reap its child")
					}
				})
			}
			t.Cleanup(finish)
			waitFor(t, func() bool { return reads.Load() >= 2 })
			turns.release()
			turns.leave(1)
			if saveFails {
				select {
				case err := <-result:
					if err == nil {
						t.Error("start marker save unexpectedly succeeded")
					}
					finishOnce.Do(cancel)
				case <-entered:
					t.Error("a start was announced before its marker could be saved")
					finish()
				case <-time.After(time.Second):
					t.Error("marker failure did not hold the request")
					finish()
				}
				if posts.Load() != 0 || models.Load() != 0 || log.starts() != 0 {
					t.Fatalf("unlaunched work announced: posts=%d models=%d starts=%d", posts.Load(), models.Load(), log.starts())
				}
				return
			}
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("start POST did not block")
			}
			pid := waitTestPID(t, filepath.Join(directory, "workspace", "child-pid"))
			clock.advance(time.Minute)
			waitFor(t, func() bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) })
			waitFor(t, func() bool { r, e := readWorkLimit(directory); return e == nil && r.held() && r.Clock.Active == nil })
			if postReturned.Load() {
				t.Fatal("fixture released the blocked POST before observing cancellation")
			}
			finish()
			if len(turns.slots) != 0 {
				t.Fatal("the canceled child retained the execution slot")
			}
		})
	}
}

// pickUp does to a pending action what the chain does when the request runs
// again: the action becomes the runtime's note that it was cut short.
func pickUp(state *chain.State) {
	if state.Pending == nil {
		return
	}
	state.Step, state.Recovering = state.Pending.Role, true
	state.History = append(state.History, chain.Result{Role: state.Pending.Role, Speaker: "runtime", Interrupted: true,
		Error: "The process stopped while this action was pending."})
	state.Pending = nil
}

// cutOffAt leaves what a forced exit in a watched launch of the stage leaves:
// the launch's crash marker, never cleared, and the chain's pending action.
// An empty stage is a forced exit while the next action is being chosen.
func cutOffAt(t *testing.T, directory, stage string, hardExits int, clock *manualWorkTime) {
	t.Helper()
	if _, err := beginActiveWork(directory, func() {}, clock.source(), hardExits); err != nil {
		t.Fatal(err)
	}
	state := loadJobState(t, directory)
	pickUp(&state)
	if stage != "" {
		state.Pending = &chain.Assignment{Role: stage}
	}
	writeJobHistory(t, directory, state)
	clock.advance(time.Minute)
}

// A request without a time limit used to restart after every forced exit with
// nothing to bound it, so a stage cut off at the same point every time ran all
// night. Forced exits in a row at one stage now pause it at the saved count
// with one notice, and one fewer goes on as before.
func TestStageExitsWithoutATimeLimitPauseAtTheSavedCountWithOneNotice(t *testing.T) {
	for _, configured := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("configured=%d", configured), func(t *testing.T) {
			cfg := watchConfiguration(t)
			holdingWorker(&cfg)
			cfg.Intake.MaxHardExits = configured
			clock := newWorkTime()
			useWorkTime(t, clock)
			remote := &noticeTracker{}
			remote.install(t, alwaysChoose("implement"))
			root, directory := noticeJob(t, chain.State{})
			issue := sourceIssue{ID: 51, Key: "EXAMPLE-51"}
			issue.Creator.ID = 55
			limit := configured
			if limit == 0 {
				limit = 3
			}
			hold := func(want bool, when string) {
				t.Helper()
				for tick := 0; tick < 2; tick++ {
					held, err := holdPausedRequest(context.Background(), cfg, issue, directory, noticeRequest, time.Second, func(string) {})
					if err != nil || held != want {
						t.Fatalf("%s tick=%d held=%t err=%v", when, tick, held, err)
					}
				}
			}
			for count := 1; count <= limit; count++ {
				cutOffAt(t, directory, "implement", configured, clock)
				hold(count == limit, fmt.Sprintf("forced exit %d", count))
				exits := loadWorkLimit(t, directory).StageExits
				if exits == nil || exits.Count != count || exits.Max != limit || exits.Stage != "implement" || exits.Active != nil {
					t.Fatalf("forced exit %d counted twice, at another stage or with another limit: %+v", count, exits)
				}
				if count == limit-1 {
					// One fewer than the limit goes on: the queue launches the
					// stage again, and its ordinary shutdown is not a forced exit.
					finish := startStopQueue(t, cfg, root, 10*time.Millisecond, io.Discard)
					waitTestPID(t, filepath.Join(directory, "workspace", "child-pid"))
					finish()
					after := loadWorkLimit(t, directory)
					if after.held() || after.StageExits.Count != count || after.StageExits.Active != nil {
						t.Fatalf("an ordinary shutdown was counted or kept its marker: %+v", after.StageExits)
					}
					stopped := false
					for _, result := range loadJobState(t, directory).History {
						stopped = stopped || result.Role == "implement" && result.Speaker == "worker" && result.Interrupted
					}
					if !stopped {
						t.Fatal("the shutdown did not leave the stage's process as stopped by the controller")
					}
					t.Logf("forced exit %d of %d: the queue launched the stage again; its shutdown left the count at %d", count, limit, count)
				}
			}
			record := loadWorkLimit(t, directory)
			if len(record.Pauses) != 1 || record.Pauses[0].Reason != hardExitPause || record.Clock != nil || loadJobState(t, directory).Done {
				t.Fatalf("the count became completion or a time limit: %+v", record)
			}
			pauses := remote.withPrefix("この依頼の自動処理を一時停止しています。")
			reason := fmt.Sprintf("「実装」の工程で強制終了が %d 回続き、上限の %d 回に達したためです。", limit, limit)
			if limit == 1 {
				reason = "「実装」の工程で強制終了が起き、上限の 1 回に達したためです。"
			}
			if len(pauses) != 1 || !strings.Contains(pauses[0], reason) || !strings.Contains(pauses[0], "「再開」") || !strings.Contains(pauses[0], "「停止」") || strings.Contains(pauses[0], "実稼働時間") {
				t.Fatalf("pause notice: %q", pauses)
			}
			if posts := remote.withPrefix("強制終了から自動で再開"); len(posts) != 0 {
				t.Fatalf("a request without a time limit was told of a remaining time: %q", posts)
			}
			hold(true, "held")
			if _, err := beginActiveWork(directory, func() {}, clock.source(), configured); !errors.Is(err, errWorkHeld) {
				t.Fatalf("a paused request could be launched: %v", err)
			}
			if posts := remote.withPrefix("この依頼の自動処理"); len(posts) != 1 {
				t.Fatalf("the pause was said more than once: %q", posts)
			}
			remote.mu.Lock()
			remote.rows = append(remote.rows, issueComment(990, 55, "再開"))
			remote.mu.Unlock()
			held, err := holdPausedRequest(context.Background(), cfg, issue, directory, noticeRequest, time.Second, func(string) {})
			after := loadWorkLimit(t, directory)
			if err != nil || held || after.held() || after.StageExits.Count != 0 || after.StageExits.Stage != "" || after.StageExits.Max != limit {
				t.Fatalf("resume did not start the count again: held=%t err=%v exits=%+v", held, err, after.StageExits)
			}
			t.Logf("forced exit %d of %d at implement: paused with one notice %q; resume -> count 0", limit, limit, pauses[0])
		})
	}
}

// The count is of forced exits in a row at one stage. A forced exit at another
// stage, a process of the stage that ended by itself, or the requester's words
// start it again from one. The runtime's own notes, a process the controller
// stopped and another stage that ran in between do not.
func TestStageExitsCountAgainOnlyWhenTheStageMoves(t *testing.T) {
	type step func(t *testing.T, directory string, clock *manualWorkTime)
	cut := func(stage string) step {
		return func(t *testing.T, directory string, clock *manualWorkTime) {
			cutOffAt(t, directory, stage, 0, clock)
			if err := recoverWorkClock(directory); err != nil {
				t.Fatal(err)
			}
		}
	}
	ran := func(results ...chain.Result) step {
		return func(t *testing.T, directory string, _ *manualWorkTime) {
			state := loadJobState(t, directory)
			pickUp(&state)
			state.History = append(state.History, results...)
			writeJobHistory(t, directory, state)
		}
	}
	for _, test := range []struct {
		name  string
		steps []step
		count int
	}{
		{"three in a row", []step{cut("implement"), cut("implement"), cut("implement")}, 3},
		{"a routing note between", []step{cut("implement"), cut("implement"), ran(chain.Result{Role: "router", Speaker: "runtime", Error: "routing unavailable"}), cut("implement")}, 3},
		{"stopped by the controller between", []step{cut("implement"), cut("implement"), ran(chain.Result{Role: "implement", Speaker: "worker", Interrupted: true, Error: "context canceled"}), cut("implement")}, 3},
		{"another stage ran between", []step{cut("implement"), cut("implement"), ran(chain.Result{Role: "inspect", Speaker: "worker", Output: "looked at the tree"}), cut("implement")}, 3},
		{"cut while choosing what runs next", []step{cut("implement"), cut(""), cut("implement")}, 3},
		{"the stage ended by itself", []step{cut("implement"), cut("implement"), ran(chain.Result{Role: "implement", Speaker: "worker", Output: "built"}), cut("implement")}, 1},
		{"the stage failed by itself", []step{cut("implement"), cut("implement"), ran(chain.Result{Role: "implement", Speaker: "worker", Error: "exit status 101"}), cut("implement")}, 1},
		{"the requester's words", []step{cut("implement"), cut("implement"), ran(chain.Result{Role: "ask", Speaker: "requester", Output: "please go on"}), cut("implement")}, 1},
		{"a forced exit at another stage", []step{cut("implement"), cut("implement"), cut("verify"), cut("implement")}, 1},
		{"the stage moved and came back", []step{cut("implement"), ran(chain.Result{Role: "implement", Speaker: "worker", Output: "built"}, chain.Result{Role: "verify", Speaker: "worker", Error: "tests failed"}), cut("implement"), cut("implement")}, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, directory := noticeJob(t, chain.State{})
			clock := newWorkTime()
			for _, step := range test.steps {
				step(t, directory, clock)
			}
			record := loadWorkLimit(t, directory)
			if exits := record.StageExits; exits == nil || exits.Count != test.count || exits.Stage != "implement" || exits.Active != nil || record.held() != (test.count == 3) {
				t.Fatalf("count=%+v held=%t, want %d at implement", exits, record.held(), test.count)
			}
		})
	}
}

// A request with a time limit keeps its own count as before: forced exits are
// counted across stages, each automatic recovery is said, the configured count
// at a launch does not replace the saved one, and no stage count is kept.
func TestStageExitsLeaveTheTimeLimitedCountAsItWas(t *testing.T) {
	cfg := watchConfiguration(t)
	clock := newWorkTime()
	useWorkTime(t, clock)
	remote := &noticeTracker{}
	remote.install(t, alwaysChoose("implement"))
	_, directory := noticeJob(t, chain.State{})
	if err := acceptWorkLimit(directory, 120, 0); err != nil {
		t.Fatal(err)
	}
	issue := sourceIssue{ID: 51, Key: "EXAMPLE-51"}
	issue.Creator.ID = 55
	for i, stage := range []string{"implement", "verify", "deliver"} {
		cutOffAt(t, directory, stage, 1, clock)
		held, err := holdPausedRequest(context.Background(), cfg, issue, directory, noticeRequest, time.Second, func(string) {})
		record := loadWorkLimit(t, directory)
		if err != nil || held != (i == 2) || record.StageExits != nil || record.Clock.HardExits != i+1 || record.Clock.MaxHardExits != 3 {
			t.Fatalf("forced exit %d at %s: held=%t err=%v clock=%+v exits=%+v", i+1, stage, held, err, record.Clock, record.StageExits)
		}
	}
	if posts := remote.withPrefix("強制終了から自動で再開"); len(posts) != 2 || !strings.Contains(posts[1], "2 回目、上限 3 回") {
		t.Fatalf("recovery notices: %q", posts)
	}
	if pauses := remote.withPrefix("この依頼の自動処理"); len(pauses) != 1 || !strings.Contains(pauses[0], "強制終了が上限の 3 回に達したためです。") || strings.Contains(pauses[0], "工程で強制終了") {
		t.Fatalf("pause notice: %q", pauses)
	}
}
