package main

import (
	"strings"
	"time"
)

const defaultStalledAfter = 30 * time.Minute

// The runtime log has no timestamps. This is a source-labelled observation,
// not evidence that an earlier error still explains the current delay.
func latestRequestLog(log, id string) string {
	lines := strings.Split(log, "\n")
	if len(lines) > 1 && strings.Contains(lines[0], " earlier bytes not shown; the whole file is under files]") {
		lines = lines[2:] // The first bytes after the tail marker may be mid-line.
	}
	for i := len(lines) - 1; i >= 0; i-- {
		line := lines[i]
		if strings.HasPrefix(line, "request "+id+": ") || strings.HasPrefix(line, "request "+id+" ") {
			return line
		}
	}
	return ""
}
