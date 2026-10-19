package main

import (
	"fmt"
	"time"
)

// This is recorded activity, not an assessment of a worker's progress.
func (s *server) markStalled(j *job, now time.Time) {
	if s.stalledAfter <= 0 || j.State == nil || j.State.Done || j.State.Waiting ||
		j.Stopped || j.StopBroken || j.PauseBroken || j.WorkPause != "" || j.Lane == "queued" {
		return
	}
	last := j.Started
	if n := len(j.State.History); n > 0 {
		record := j.State.History[n-1]
		last = record.FinishedAt
		if last.IsZero() {
			last = record.StartedAt
		}
	}
	if last.IsZero() || now.Sub(last) <= s.stalledAfter {
		return
	}
	budgetPaused := false
	for _, notice := range j.Notices {
		written, err := time.Parse(time.RFC3339Nano, stringOf(notice["written_at"]))
		if err != nil || !written.After(last) {
			continue
		}
		switch stringOf(notice["kind"]) {
		case "budget-paused":
			budgetPaused = true
		case "budget-restored":
			budgetPaused = false
		}
	}
	if budgetPaused {
		return
	}
	j.Status, j.Lane = "stalled", "attention"
	if j.Attention == "" {
		j.Attention = fmt.Sprintf("No new history record for %s.", humanDuration(now.Sub(last)))
	}
	for i := len(j.State.History) - 1; i >= 0; i-- {
		if failure := j.State.History[i].Error; failure != "" && !j.State.History[i].Interrupted {
			j.Failure = firstLine(failure)
			break
		}
	}
}
