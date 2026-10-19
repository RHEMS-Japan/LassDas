//go:build !linux && !darwin

package diskusage

import "errors"

func Percent(string) (float64, error) {
	return 0, errors.New("filesystem usage is unavailable on this operating system")
}
