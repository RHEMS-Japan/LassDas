package main

import (
	"encoding/json"
	"fmt"
	"math"

	"ticket-runner/internal/diskusage"
)

type diskReading struct {
	Usage     string
	Threshold int
	Warning   bool
}

func (s *server) readDisk() (*diskReading, error) {
	var intake struct {
		DiskNoticePercent *int `json:"disk_notice_percent"`
	}
	raw, err := json.Marshal(s.config["intake"])
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &intake); err != nil {
		return nil, fmt.Errorf("capacity warning configuration: %w", err)
	}
	if err := diskusage.Validate(intake.DiskNoticePercent); err != nil {
		return nil, fmt.Errorf("capacity warning configuration: %w", err)
	}
	measure := s.diskPercent
	if measure == nil {
		measure = diskusage.Percent
	}
	percent, err := measure(s.runDir)
	if err != nil {
		return nil, fmt.Errorf("usage is unknown: %w", err)
	}
	threshold := diskusage.Threshold(intake.DiskNoticePercent)
	return &diskReading{Usage: fmt.Sprintf("%.1f%%", math.Floor(percent*10)/10), Threshold: threshold, Warning: diskusage.Over(percent, threshold)}, nil
}
