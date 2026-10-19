//go:build linux || darwin

package diskusage

import "testing"

func TestFilesystemReadingHasAPercentageRange(t *testing.T) {
	value, err := Percent(t.TempDir())
	if err != nil || value < 0 || value > 100 {
		t.Fatalf("percent=%v error=%v", value, err)
	}
}
