package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ticket-runner/internal/chain"
)

func TestMain(m *testing.M) {
	// Watch tests never depend on the developer machine's free space.
	queueDiskPercent = func(string) (float64, error) { return 10, nil }
	os.Exit(m.Run())
}

func checkQueueDisk(ctx context.Context, cfg config, n notices, percent float64) error {
	needed, _, err := capacityNoticeNeeded(n, n.recordOnly)
	if err != nil {
		return err
	}
	_, err = noteQueueDisk(ctx, cfg, n, percent, needed)
	return err
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
				if err := checkQueueDisk(context.Background(), cfg, requestNotices(cfg, announcedIssue(), directory), percent); err != nil {
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
	if err := checkQueueDisk(context.Background(), cfg, n, 100); err == nil {
		t.Fatal("failure hidden")
	}
	if err := checkQueueDisk(context.Background(), cfg, n, 100); err != nil {
		t.Fatal(err)
	}
	if len(tracker.posted) != 1 {
		t.Fatalf("posts %v", tracker.posted)
	}
	if err := checkQueueDisk(context.Background(), cfg, n, 100); err != nil || len(tracker.posted) != 1 {
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
			var calls atomic.Int32
			queueDiskPercent = func(path string) (float64, error) {
				count := calls.Add(1)
				if path != root {
					t.Errorf("measured %s", path)
				}
				if count == 1 {
					return 10, nil
				}
				return 100, nil
			}
			defer func() { queueDiskPercent = original }()
			var output bytes.Buffer
			finish := startStopQueue(t, cfg, root, 20*time.Millisecond, &output)
			if mode == "done" || mode == "stopped" {
				waitFor(t, func() bool { return calls.Load() >= 3 })
				// A terminal request is no longer re-read by the capacity path.
				if err := os.WriteFile(filepath.Join(directory, "notices.json"), []byte("broken fixture"), 0600); err != nil {
					t.Fatal(err)
				}
				waitFor(t, func() bool { return calls.Load() >= 5 })
			} else if mode == "off" {
				waitFor(t, func() bool { _, err := os.Stat(filepath.Join(directory, "workspace", "active.txt")); return err == nil })
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
			if strings.Contains(output.String(), "capacity notice record unavailable") {
				t.Fatal("terminal capacity record was read again: " + output.String())
			}
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
			if mode == "off" && calls.Load() != 0 {
				t.Fatal("off still measured")
			}
			if mode == "off" && slices.Contains(postableKinds(cfg), diskNotice) {
				t.Fatal("off still registers capacity notice")
			}
		})
	}
}

func TestRecordedCapacityDoesNotReadHistoryOrFlushOtherPendingNotices(t *testing.T) {
	cfg := watchConfiguration(t)
	_, directory := noticeJob(t, chain.State{})
	n := requestNotices(cfg, announcedIssue(), directory)
	now := time.Now()
	if err := n.save(noticeLog{Notices: []noticeRecord{
		{Kind: diskNotice, Text: "already said", PostedAt: &now},
		{Kind: resumeNotice, Text: "another pending notice"},
	}}); err != nil {
		t.Fatal(err)
	}
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		t.Error("recorded capacity made an unexpected tracker request")
		return nil, fmt.Errorf("unexpected tracker request")
	})
	if err := checkQueueDisk(context.Background(), cfg, n, 100); err != nil {
		t.Fatal(err)
	}
}

func TestTerminalCapacitySettlesOnlyItsPendingRecord(t *testing.T) {
	for _, mode := range []string{"done", "stopped"} {
		t.Run(mode, func(t *testing.T) {
			cfg := watchConfiguration(t)
			cfg.Intake.StopUserIDs = []int64{55}
			_, directory := noticeJob(t, chain.State{})
			n := requestNotices(cfg, announcedIssue(), directory)
			n.recordOnly = true
			if err := n.save(noticeLog{Notices: []noticeRecord{{Kind: resumeNotice, Text: "keep pending"}}}); err != nil {
				t.Fatal(err)
			}
			useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
				t.Error("terminal capacity must not contact the tracker")
				return nil, fmt.Errorf("unexpected tracker request")
			})
			if err := checkQueueDisk(context.Background(), cfg, n, 100); err != nil {
				t.Fatal(err)
			}
			if mode == "done" {
				writeJobHistory(t, directory, chain.State{Done: true})
			} else if err := writeRuntimeFile(filepath.Join(directory, "stop-request.json"), stopComment(51, 55, "停止")); err != nil {
				t.Fatal(err)
			}
			n.recordOnly = false
			unlock := n.hold()
			settled, err := noteQueueDisk(context.Background(), cfg, n, 100, true)
			unlock()
			if err != nil || settled {
				t.Fatalf("busy terminal record was cached: %v %v", settled, err)
			}
			settled, err = noteQueueDisk(context.Background(), cfg, n, 100, true)
			if err != nil || !settled {
				t.Fatalf("terminal record not settled: %v %v", settled, err)
			}
			saved, err := n.load()
			if err != nil || len(saved.Notices) != 2 {
				t.Fatalf("records lost: %+v %v", saved, err)
			}
			if pendingNotice(noticeLog{Notices: saved.Notices[1:]}) >= 0 || saved.Notices[1].PostedAt != nil {
				t.Fatal("terminal capacity was left pending or falsely marked posted")
			}
			if pendingNotice(saved) != 0 {
				t.Fatal("another pending notice was changed")
			}
			needed, settled, err := capacityNoticeNeeded(n, false)
			if err != nil || needed || !settled {
				t.Fatalf("restart would retry obsolete capacity: %v %v %v", needed, settled, err)
			}
		})
	}
}

func TestTerminalCapacityIsSettledWhenNewWarningsAreOff(t *testing.T) {
	for _, mode := range []string{"done", "stopped"} {
		t.Run(mode, func(t *testing.T) {
			cfg := watchConfiguration(t)
			off := 0
			cfg.Intake.DiskNoticePercent = &off
			root, directory := noticeJob(t, chain.State{Done: mode == "done"})
			if mode == "stopped" {
				if err := writeRuntimeFile(filepath.Join(directory, "stop-request.json"), stopComment(51, 55, "停止")); err != nil {
					t.Fatal(err)
				}
			}
			n := requestNotices(cfg, announcedIssue(), directory)
			if err := n.save(noticeLog{Notices: []noticeRecord{{Kind: diskNotice, Text: "recorded before the run ended"}}}); err != nil {
				t.Fatal(err)
			}
			tracker := &noticeTracker{}
			tracker.install(t, func(r *http.Request) (*http.Response, error) { return nil, fmt.Errorf("unexpected tracker request") })
			finish := startStopQueue(t, cfg, root, 20*time.Millisecond, io.Discard)
			waitFor(t, func() bool {
				saved, err := n.load()
				return err == nil && len(saved.Notices) == 1 && saved.Notices[0].Obsolete
			})
			finish()
			if tracker.attempts() != 0 {
				t.Fatal("terminal warning was posted")
			}
		})
	}
}

func TestCapacityOnlyReportsItsOwnConfirmation(t *testing.T) {
	cfg := watchConfiguration(t)
	_, directory := noticeJob(t, chain.State{})
	n := requestNotices(cfg, announcedIssue(), directory)
	if err := n.save(noticeLog{Notices: []noticeRecord{
		{Kind: resumeNotice, Text: "another notice"},
		{Kind: diskNotice, Text: "capacity warning"},
	}}); err != nil {
		t.Fatal(err)
	}
	tracker := &noticeTracker{}
	tracker.install(t, func(r *http.Request) (*http.Response, error) { return nil, fmt.Errorf("unexpected tracker request") })
	settled, err := noteQueueDisk(context.Background(), cfg, n, 100, true)
	if err != nil || settled || len(tracker.posted) != 1 || tracker.posted[0] != "another notice" {
		t.Fatalf("another notice counted as capacity: settled=%v posts=%v err=%v", settled, tracker.posted, err)
	}
	settled, err = noteQueueDisk(context.Background(), cfg, n, 100, true)
	if err != nil || !settled || len(tracker.posted) != 2 || tracker.posted[1] != "capacity warning" {
		t.Fatalf("capacity not confirmed: settled=%v posts=%v err=%v", settled, tracker.posted, err)
	}
}

func TestActiveCapacityOnlyRecordsWithoutWaitingForNoticeTraffic(t *testing.T) {
	cfg := watchConfiguration(t)
	_, directory := noticeJob(t, chain.State{})
	tracker := &noticeTracker{}
	tracker.install(t, func(r *http.Request) (*http.Response, error) { return nil, fmt.Errorf("unexpected tracker call") })
	n := requestNotices(cfg, announcedIssue(), directory)
	n.recordOnly = true
	if err := n.save(noticeLog{Notices: []noticeRecord{{Kind: resumeNotice, Text: "pending"}}}); err != nil {
		t.Fatal(err)
	}
	if err := checkQueueDisk(context.Background(), cfg, n, 100); err != nil {
		t.Fatal(err)
	}
	saved, err := n.load()
	if err != nil || len(saved.Notices) != 2 || len(tracker.posted) != 0 {
		t.Fatalf("record-only: %+v %v", saved, err)
	}
	// Remove only the fixture's capacity entry so this call must try the lock.
	if err := n.save(noticeLog{Notices: saved.Notices[:1]}); err != nil {
		t.Fatal(err)
	}
	unlock := n.hold()
	done := make(chan error, 1)
	go func() { done <- checkQueueDisk(context.Background(), cfg, n, 100) }()
	select {
	case err := <-done:
		unlock()
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		unlock()
		<-done
		t.Fatal("queue waited for another notice's network operation")
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
	if err := checkQueueDisk(context.Background(), cfg, n, 100); err != nil {
		t.Fatal(err)
	}
	if len(tracker.posted) != 1 {
		t.Fatal("current condition was not posted")
	}
}
