package main

import (
	"context"
	"fmt"
	"time"

	"ticket-runner/internal/diskusage"
)

const diskNotice = "queue-disk-usage"

var queueDiskPercent = diskusage.Percent

func noteQueueDisk(ctx context.Context, cfg config, n notices) error {
	percent, err := queueDiskPercent(n.queue)
	if err != nil {
		return err
	}
	threshold := diskusage.DefaultThreshold
	if cfg.Intake != nil && cfg.Intake.DiskNoticePercent > 0 {
		threshold = cfg.Intake.DiskNoticePercent
	}
	if percent <= float64(threshold) {
		return nil
	}
	text := fmt.Sprintf("作業用の保存領域の使用率は %.1f%% で、設定の閾値 %d%% を超えています。空き容量が無くなると作業を続けられません。運用者は空き容量を確認してください。この通知では処理を停止しません。", percent, threshold)
	return n.post(ctx, diskNotice, text, time.Now().UTC())
}
