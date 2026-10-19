package main

import (
	"context"
	"fmt"
	"time"

	"ticket-runner/internal/diskusage"
)

const diskNotice = "queue-disk-usage"

var queueDiskPercent = diskusage.Percent

func diskThreshold(cfg config) int {
	if cfg.Intake == nil {
		return diskusage.DefaultThreshold
	}
	return diskusage.Threshold(cfg.Intake.DiskNoticePercent)
}

func noteQueueDisk(ctx context.Context, cfg config, n notices, percent float64) error {
	threshold := diskThreshold(cfg)
	if cfg.Intake == nil || !diskusage.Over(percent, threshold) {
		return nil
	}
	needed, _, err := capacityNoticeNeeded(n, n.recordOnly)
	if err != nil || !needed {
		return err
	}
	if stopped, err := savedStop(n.source, n.directory, n.issue, cfg.Intake.StopUserIDs); err != nil || stopped {
		// The request loop diagnoses its state; this is not a send failure.
		return nil
	}
	state, err := savedHistory(n.directory)
	if err != nil || state.Done {
		return nil
	}
	text := fmt.Sprintf("作業用の保存領域の使用率が設定の閾値に達したため、この通知を記録しました（閾値 %d%%）。空き容量が無くなると作業を続けられません。運用者は空き容量を確認してください。この通知では処理を停止しません。", threshold)
	// A current condition must not predate a late first notice-kind save.
	return n.post(ctx, diskNotice, text, time.Time{})
}

// Confirmed notices can be cached for the collector's lifetime. Pending ones
// remain retryable for inactive requests; active watchers own their flushing.
func capacityNoticeNeeded(n notices, active bool) (needed, confirmed bool, err error) {
	log, err := n.load() // atomic file replacement permits a lock-free read
	if err != nil {
		return false, false, err
	}
	for _, record := range log.Notices {
		if record.Kind == diskNotice {
			confirmed = record.PostedAt != nil || record.Predates
			return !confirmed && !active, confirmed, nil
		}
	}
	return true, false, nil
}
