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

func noteQueueDisk(ctx context.Context, cfg config, n notices, percent float64, needed bool) (settled bool, err error) {
	threshold := diskThreshold(cfg)
	if cfg.Intake == nil || !diskusage.Over(percent, threshold) || !needed {
		return false, nil
	}
	stopped, err := savedStop(n.source, n.directory, n.issue, cfg.Intake.StopUserIDs)
	if err != nil {
		// The request loop diagnoses its state; this is not a send failure.
		return false, nil
	}
	if stopped {
		return obsoleteCapacity(n)
	}
	state, err := savedHistory(n.directory)
	if err != nil {
		return false, nil
	}
	if state.Done {
		return obsoleteCapacity(n)
	}
	text := fmt.Sprintf("作業用の保存領域の使用率が設定の閾値に達したため、この通知を記録しました（閾値 %d%%）。空き容量が無くなると作業を続けられません。運用者は空き容量を確認してください。この通知では処理を停止しません。", threshold)
	// A current condition must not predate a late first notice-kind save.
	if err := n.post(ctx, diskNotice, text, time.Time{}); err != nil || n.recordOnly {
		return false, err
	}
	_, settled, err = capacityNoticeNeeded(n, false)
	return settled, err
}

// A request can end after the collector records but before its watcher flushes.
// Keep that evidence without publishing a warning after the request has ended.
func obsoleteCapacity(n notices) (bool, error) {
	mutex := n.mutex()
	if !mutex.TryLock() {
		return false, nil
	}
	defer mutex.Unlock()
	log, err := n.load()
	if err != nil {
		return false, err
	}
	changed := false
	for i, record := range log.Notices {
		if record.Kind == diskNotice && record.PostedAt == nil && !record.Predates && !record.Obsolete {
			log.Notices[i].Obsolete = true
			changed = true
		}
	}
	if changed {
		err = n.save(log)
	}
	return err == nil, err
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
			confirmed = record.PostedAt != nil || record.Predates || record.Obsolete
			return !confirmed && !active, confirmed, nil
		}
	}
	return true, false, nil
}
