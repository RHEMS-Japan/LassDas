// Package diskusage measures the filesystem containing a queue, not its files.
package diskusage

import (
	"errors"
)

const DefaultThreshold = 85

func percentOf(blocks, available uint64) (float64, error) {
	if blocks == 0 || available > blocks {
		return 0, errors.New("filesystem capacity is unavailable")
	}
	return 100 * (1 - float64(available)/float64(blocks)), nil
}

func Threshold(configured *int) int {
	if configured == nil {
		return DefaultThreshold
	}
	return *configured
}

func Validate(configured *int) error {
	if configured != nil && (*configured < 0 || *configured > 100) {
		return errors.New("intake.disk_notice_percent must be between 0 and 100 (zero disables warnings)")
	}
	return nil
}

func Over(percent float64, threshold int) bool { return threshold > 0 && percent >= float64(threshold) }
