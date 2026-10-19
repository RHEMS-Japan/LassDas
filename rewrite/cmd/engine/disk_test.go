package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"ticket-runner/internal/chain"
)

func TestMain(m *testing.M) {
	// Watch tests never depend on the developer machine's free space.
	queueDiskPercent = func(string) (float64, error) { return 10, nil }
	os.Exit(m.Run())
}

func TestQueueDiskWarningCrossesThresholdOnceAcrossRestarts(t *testing.T) {
	for _, threshold := range []int{85, 92, 100} {
		t.Run(fmt.Sprint(threshold), func(t *testing.T) {
			cfg := watchConfiguration(t)
			cfg.Intake.DiskNoticePercent = &threshold
			root, directory := noticeJob(t, chain.State{})
			queueRanSince(t, root, cfg, time.Now().Add(-time.Minute))
			tracker := &noticeTracker{}
			tracker.install(t, func(r *http.Request) (*http.Response, error) { return nil, fmt.Errorf("unexpected request") })
			limit := float64(threshold)
			for i, percent := range []float64{limit - 1, limit, limit + 0.5, limit + 1, limit - 1, limit + 1} {
				// Reconstruct from disk each time, like a controller restart.
				if err := noteQueueDisk(context.Background(), cfg, requestNotices(cfg, announcedIssue(), directory), percent); err != nil {
					t.Fatal(err)
				}
				want := 0
				if i >= 1 {
					want = 1
				}
				if len(tracker.posted) != want {
					t.Fatalf("at %.1f%% posted %d, want %d", percent, len(tracker.posted), want)
				}
				if want == 1 && !strings.Contains(tracker.posted[0], "閾値に達したため") {
					t.Fatal("missing condition")
				}
				if i == 2 {
					n := requestNotices(cfg, announcedIssue(), directory)
					saved, err := n.load()
					if err != nil {
						t.Fatal(err)
					}
					for j := range saved.Notices {
						saved.Notices[j].WrittenAt = time.Now().Add(-24 * time.Hour)
					}
					if err := n.save(saved); err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}

func TestQueueDiskRecoversPostAndDoesNotBackdateCurrentCondition(t *testing.T) {
	cfg := watchConfiguration(t)
	_, directory := noticeJob(t, chain.State{}) // no notice-kind registration yet
	tracker := &noticeTracker{postFail: 1}
	tracker.install(t, func(r *http.Request) (*http.Response, error) { return nil, fmt.Errorf("unexpected request") })
	n := requestNotices(cfg, announcedIssue(), directory)
	if err := noteQueueDisk(context.Background(), cfg, n, 100); err == nil {
		t.Fatal("failure hidden")
	}
	if err := noteQueueDisk(context.Background(), cfg, n, 100); err != nil {
		t.Fatal(err)
	}
	if len(tracker.posted) != 1 {
		t.Fatalf("posts %v", tracker.posted)
	}
	if err := noteQueueDisk(context.Background(), cfg, n, 100); err != nil || len(tracker.posted) != 1 {
		t.Fatal("repeated", err)
	}
}

func TestQueueDiskPollCoversWaitingPausedHeldAndActive(t *testing.T) {
	for _, mode := range []string{"waiting", "paused", "credit", "active", "done", "stopped", "off"} {
		t.Run(mode, func(t *testing.T) {
			cfg := watchConfiguration(t)
			state := chain.State{}
			if mode == "waiting" {
				state.Waiting = true
			}
			if mode == "done" {
				state.Done = true
			}
			root, directory := noticeJob(t, state)
			if mode == "stopped" {
				if err := writeRuntimeFile(filepath.Join(directory, "stop-request.json"), stopComment(51, 55, "停止")); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "paused" {
				err := saveWorkLimit(directory, workLimitRecord{Version: 1, Pauses: []pauseEpisode{{Reason: activeLimitPause, At: time.Now(), NoticeID: 900}}})
				if err != nil {
					t.Fatal(err)
				}
			}
			if mode == "credit" {
				cfg.Intake.MinModelCredit = 1
				cfg.Router.Decision.KeyEnv = "TEST_DISK_MODEL_KEY"
				t.Setenv("TEST_DISK_MODEL_KEY", "synthetic-value")
				cfg.Intake.ModelCreditURL = "https://credit.example/balance"
			}
			if mode == "active" || mode == "off" {
				cfg.Router.Mode = "single"
				cfg.Roles[0].Processes[0].Command = []string{"/bin/sh", "-c", "cat >/dev/null; printf active > active.txt; exec sleep 60"}
			}
			if mode == "off" {
				n := 0
				cfg.Intake.DiskNoticePercent = &n
			}
			tracker := &noticeTracker{}
			tracker.install(t, func(r *http.Request) (*http.Response, error) {
				if r.URL.Host == "credit.example" {
					return selectionReply(r, 200, map[string]any{"data": map[string]any{"total_credits": 0, "total_usage": 0}}), nil
				}
				return nil, fmt.Errorf("unexpected request")
			})
			original := queueDiskPercent
			calls := 0
			queueDiskPercent = func(path string) (float64, error) {
				calls++
				if path != root {
					t.Errorf("measured %s", path)
				}
				if calls == 1 {
					return 10, nil
				}
				return 100, nil
			}
			defer func() { queueDiskPercent = original }()
			finish := startStopQueue(t, cfg, root, 20*time.Millisecond, io.Discard)
			if mode == "done" || mode == "stopped" || mode == "off" {
				time.Sleep(80 * time.Millisecond)
			} else {
				waitFor(t, func() bool {
					tracker.mu.Lock()
					defer tracker.mu.Unlock()
					for _, text := range tracker.posted {
						if strings.Contains(text, "保存領域") {
							return true
						}
					}
					return false
				})
			}
			if mode == "active" {
				waitFor(t, func() bool { _, err := os.Stat(filepath.Join(directory, "workspace", "active.txt")); return err == nil })
			}
			finish()
			warnings := 0
			for _, text := range tracker.posted {
				if strings.Contains(text, "保存領域") {
					warnings++
				}
			}
			want := 1
			if mode == "done" || mode == "stopped" || mode == "off" {
				want = 0
			}
			if warnings != want {
				t.Fatalf("warnings=%d want=%d", warnings, want)
			}
			if mode == "off" && calls != 0 {
				t.Fatal("off still measured")
			}
			if mode == "off" && slices.Contains(postableKinds(cfg), diskNotice) {
				t.Fatal("off still registers capacity notice")
			}
		})
	}
}

func TestDiskNoticeRangeIsCheckedWithOtherNoticeSettings(t *testing.T) {
	cfg := watchConfiguration(t)
	for _, value := range []int{-1, 0, 1, 100, 101} {
		cfg.Intake.DiskNoticePercent = &value
		err := validateNotices(cfg)
		if (err != nil) != (value < 0 || value > 100) {
			t.Fatalf("%d: %v", value, err)
		}
	}
}

func TestCurrentCapacityConditionDoesNotReviveOtherUndatedEvents(t *testing.T) {
	cfg := watchConfiguration(t)
	_, directory := noticeJob(t, chain.State{})
	tracker := &noticeTracker{}
	tracker.install(t, func(r *http.Request) (*http.Response, error) { return nil, fmt.Errorf("unexpected request") })
	n := requestNotices(cfg, announcedIssue(), directory)
	if err := n.post(context.Background(), resumeNotice, "old undated event", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if len(tracker.posted) != 0 {
		t.Fatal("undated historical event was published as new")
	}
	if err := noteQueueDisk(context.Background(), cfg, n, 100); err != nil {
		t.Fatal(err)
	}
	if len(tracker.posted) != 1 {
		t.Fatal("current condition was not posted")
	}
}
