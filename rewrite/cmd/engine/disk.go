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
	if !diskusage.Over(percent, threshold) {
		return nil
	}
	if stopped, err := savedStop(n.source, n.directory, n.issue, cfg.Intake.StopUserIDs); err != nil || stopped {
		return err
	}
	state, err := savedHistory(n.directory)
	if err != nil || state.Done {
		return err
	}
	text := fmt.Sprintf("作業用の保存領域の使用率が設定の閾値に達したため、この通知を記録しました（閾値 %d%%）。空き容量が無くなると作業を続けられません。運用者は空き容量を確認してください。この通知では処理を停止しません。", threshold)
	// A current condition must not predate a late first notice-kind save.
	return n.post(ctx, diskNotice, text, time.Time{})
}
