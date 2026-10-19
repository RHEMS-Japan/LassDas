//go:build linux || darwin

package diskusage

import "syscall"

func Percent(path string) (float64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return percentOf(stat.Blocks, stat.Bavail)
}
