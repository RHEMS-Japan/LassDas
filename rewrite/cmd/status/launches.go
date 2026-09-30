package main

import (
	"strings"
	"time"
)

// A launch is what the requester can read as one event: the runtime started
// one role once, its processes wrote what they wrote, and the runtime noted
// how it ended. The saved record keeps every process and the runtime's note
// as separate entries; this view puts them back together, says who wrote each
// text, and folds a failure that repeated unchanged into one entry that says
// how often.
type launch struct {
	Index       int
	Last        int
	Role        string
	Started     time.Time
	Finished    time.Time
	Duration    string
	Outcome     string
	Failure     string
	Workers     []worker
	Notes       []string
	Instruction string
	Count       int
	Records     []int
	Gap         string
}

type worker struct {
	Speaker     string
	Model       string
	Output      string
	Error       string
	Diagnostics string
}

const (
	returned      = "returned"
	failed        = "failed"
	couldNotStart = "could not start"
	interrupted   = "interrupted"
	answered      = "answer from the requester"
	noted         = "note by the runtime"
)

func groupLaunches(records []record) []launch {
	var launches []launch
	open := -1 // index in launches of the launch still taking records
	for _, entry := range records {
		switch {
		case entry.Person:
			launches = append(launches, launch{Role: entry.Role, Started: entry.Started, Finished: entry.Finished, Outcome: answered,
				Workers: []worker{{Speaker: entry.Speaker, Output: entry.Output}}, Records: []int{entry.Index}, Gap: entry.Gap})
			open = -1
		case entry.Runtime:
			if open >= 0 && launches[open].Role == entry.Role {
				current := &launches[open]
				current.Records = append(current.Records, entry.Index)
				if entry.Finished.After(current.Finished) {
					current.Finished = entry.Finished
				}
				if entry.Error != "" {
					current.Notes = append(current.Notes, entry.Error)
					if strings.HasPrefix(entry.Error, "The process stopped while this action was pending") {
						current.Outcome, current.Failure = interrupted, firstLine(entry.Error)
					}
				} else if entry.Output != "" {
					current.Notes = append(current.Notes, entry.Output)
				}
				open = -1
				continue
			}
			text := entry.Output
			if entry.Error != "" {
				text = entry.Error
			}
			launches = append(launches, launch{Role: entry.Role, Started: entry.Started, Finished: entry.Finished, Outcome: noted,
				Failure: firstLine(entry.Error), Notes: []string{text}, Instruction: entry.Instruction, Records: []int{entry.Index}, Gap: entry.Gap})
			open = -1
		default:
			if open < 0 || launches[open].Role != entry.Role {
				launches = append(launches, launch{Role: entry.Role, Started: entry.Started, Finished: entry.Finished, Outcome: returned,
					Instruction: entry.Instruction, Gap: entry.Gap})
				open = len(launches) - 1
			}
			current := &launches[open]
			current.Records = append(current.Records, entry.Index)
			current.Workers = append(current.Workers, worker{Speaker: entry.Speaker, Model: entry.Model, Output: entry.Output, Error: entry.Error, Diagnostics: entry.Diagnostics})
			if entry.Started.Before(current.Started) || current.Started.IsZero() {
				current.Started = entry.Started
			}
			if entry.Finished.After(current.Finished) {
				current.Finished = entry.Finished
			}
			if entry.Error != "" && current.Outcome == returned {
				current.Outcome, current.Failure = failed, firstLine(entry.Error)
				if entry.Output == "" && startFailure(entry.Error) {
					current.Outcome = couldNotStart
				}
			}
		}
	}
	for i := range launches {
		launches[i].Index = i + 1
		launches[i].Last = i + 1
		launches[i].Count = 1
		launches[i].Duration = humanDuration(launches[i].Finished.Sub(launches[i].Started))
	}
	return foldLaunches(launches)
}

// startFailure recognises a launch that never ran: the runtime could not
// start the process, or could not prepare what it needed first.
func startFailure(text string) bool {
	return strings.Contains(text, "fork/exec") || strings.HasPrefix(text, "Preparing role access") || strings.Contains(text, "executable file not found")
}

// foldLaunches joins consecutive launches of one role that ended the same way
// with the same failure into one entry that keeps the first and last of them.
func foldLaunches(launches []launch) []launch {
	var folded []launch
	for _, current := range launches {
		if n := len(folded); n > 0 {
			previous := &folded[n-1]
			same := previous.Role == current.Role && previous.Outcome == current.Outcome && previous.Failure == current.Failure &&
				(current.Outcome == failed || current.Outcome == couldNotStart)
			if same {
				previous.Count++
				previous.Last = current.Index
				previous.Finished = current.Finished
				previous.Records = append(previous.Records, current.Records...)
				previous.Duration = humanDuration(previous.Finished.Sub(previous.Started))
				continue
			}
		}
		folded = append(folded, current)
	}
	return folded
}
