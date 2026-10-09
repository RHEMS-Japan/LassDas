package main

import (
	"fmt"
	"time"
)

// The watcher supplies the accepted job's facts through the existing child
// context; the child's history directory is not the job record's directory.
type acceptedTimeFactsKey struct{}

func requestTimeFacts(directory string) string {
	record, err := readWorkLimit(directory)
	if err != nil {
		return "Time limit for this request: unknown; its saved record could not be read."
	}
	if record.Clock == nil {
		return "Time limit for this request: none was saved when it was accepted; a single launch can still have its own limit."
	}
	return fmt.Sprintf("Time limit for this request: %d minutes of active work, saved when it was accepted; %s of it already used. A single launch can have a shorter limit.",
		record.Clock.MaxMinutes, record.Clock.Elapsed.Round(time.Second))
}
