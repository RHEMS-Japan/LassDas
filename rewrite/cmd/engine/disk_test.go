package main

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"ticket-runner/internal/chain"
)

func TestQueueDiskWarningCrossesThresholdOnceAcrossRestarts(t *testing.T) {
	for _, threshold := range []int{0, 92} {
		t.Run(fmt.Sprint(threshold), func(t *testing.T) {
			cfg := watchConfiguration(t)
			cfg.Intake.DiskNoticePercent = threshold
			root, directory := noticeJob(t, chain.State{})
			queueRanSince(t, root, cfg, time.Now().Add(-time.Minute))
			tracker := &noticeTracker{}
			tracker.install(t, func(r *http.Request) (*http.Response, error) { return nil, fmt.Errorf("unexpected request") })
			original := queueDiskPercent
			t.Cleanup(func() { queueDiskPercent = original })
			limit := float64(threshold)
			if threshold == 0 {
				limit = 85
			}
			for i, percent := range []float64{limit - 1, limit, limit + 0.5, limit + 1, limit - 1, limit + 1} {
				queueDiskPercent = func(path string) (float64, error) {
					if path != root {
						t.Fatalf("measured %q, want queue %q", path, root)
					}
					return percent, nil
				}
				// Reconstruct from disk each time, like a controller restart.
				if err := noteQueueDisk(context.Background(), cfg, requestNotices(cfg, announcedIssue(), directory)); err != nil {
					t.Fatal(err)
				}
				want := 0
				if i >= 2 {
					want = 1
				}
				if len(tracker.posted) != want {
					t.Fatalf("at %.1f%% posted %d, want %d", percent, len(tracker.posted), want)
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
