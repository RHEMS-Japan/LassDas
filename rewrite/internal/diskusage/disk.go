// Package diskusage measures the filesystem containing a queue, not its files.
package diskusage

import (
	"errors"
	"syscall"
)

const DefaultThreshold = 85

func Percent(path string) (float64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	if stat.Blocks == 0 || stat.Bavail > stat.Blocks {
		return 0, errors.New("filesystem capacity is unavailable")
	}
	return 100 * (1 - float64(stat.Bavail)/float64(stat.Blocks)), nil
}
