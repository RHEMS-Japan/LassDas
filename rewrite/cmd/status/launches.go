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
	signature   string
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

// stageNotePrefix opens the runtime's own note about one launch of a stage;
// that note is the only record that closes a launch. Any other note of the
// runtime, an interruption, a routing failure, a limit reached, stands on its
// own. A process record joins the open launch of its role only when it began
// no later than that launch's latest end: processes of one launch start
// together, and a later launch starts after the earlier one ended.
const stageNotePrefix = "Runtime record for stage "

func groupLaunches(records []record) []launch {
	var launches []launch
	open := -1 // index in launches of the launch still taking records
	for _, entry := range records {
		switch {
		case entry.Person:
			launches = append(launches, launch{Role: entry.Role, Started: entry.Started, Finished: entry.Finished, Outcome: answered,
				Workers: []worker{{Speaker: entry.Speaker, Output: entry.Output}}, Records: []int{entry.Index}, Gap: entry.Gap})
			open = -1
		case entry.Runtime && entry.Error == "" && strings.HasPrefix(entry.Output, stageNotePrefix) && open >= 0 && launches[open].Role == entry.Role:
			current := &launches[open]
			current.Records = append(current.Records, entry.Index)
			current.Notes = append(current.Notes, entry.Output)
			if entry.Finished.After(current.Finished) {
				current.Finished = entry.Finished
			}
			open = -1
		case entry.Runtime:
			note := launch{Role: entry.Role, Started: entry.Started, Finished: entry.Finished, Outcome: noted, Instruction: entry.Instruction, Records: []int{entry.Index}, Gap: entry.Gap}
			switch {
			case strings.HasPrefix(entry.Error, "The process stopped while this action was pending"):
				note.Outcome, note.Failure, note.Notes = interrupted, firstLine(entry.Error), []string{entry.Error}
			case entry.Error != "":
				note.Outcome, note.Failure, note.Notes = failed, firstLine(entry.Error), []string{entry.Error}
			default:
				note.Notes = []string{entry.Output}
			}
			launches = append(launches, note)
			open = -1
		default:
			if open < 0 || launches[open].Role != entry.Role || (!entry.Started.IsZero() && entry.Started.After(launches[open].Finished)) {
				launches = append(launches, launch{Role: entry.Role, Started: entry.Started, Finished: entry.Finished, Outcome: returned,
					Instruction: entry.Instruction, Gap: entry.Gap})
				open = len(launches) - 1
			}
			current := &launches[open]
			current.Records = append(current.Records, entry.Index)
			current.Workers = append(current.Workers, worker{Speaker: entry.Speaker, Model: entry.Model, Output: entry.Output, Error: entry.Error, Diagnostics: entry.Diagnostics})
			if current.Started.IsZero() || (!entry.Started.IsZero() && entry.Started.Before(current.Started)) {
				current.Started = entry.Started
			}
			if entry.Finished.After(current.Finished) {
				current.Finished = entry.Finished
			}
		}
	}
	for i := range launches {
		current := &launches[i]
		current.Index, current.Last, current.Count = i+1, i+1, 1
		if len(current.Workers) > 0 && current.Outcome == returned {
			var errors []string
			started := 0
			for _, w := range current.Workers {
				if w.Error != "" {
					errors = append(errors, w.Error)
				}
				if w.Output != "" || !startFailure(w.Error) {
					started++
				}
			}
			if len(errors) > 0 {
				current.Outcome, current.Failure = failed, firstLine(errors[0])
				if started == 0 {
					current.Outcome = couldNotStart
				}
			}
			current.signature = current.Role + "\x00" + current.Outcome + "\x00" + strings.Join(errors, "\x00")
		}
		if !current.Started.IsZero() && !current.Finished.IsZero() && !current.Finished.Before(current.Started) {
			current.Duration = humanDuration(current.Finished.Sub(current.Started))
		}
	}
	return foldLaunches(launches)
}

// startFailure recognises a launch that never ran: the runtime could not
// start the process, or could not prepare what it needed first.
func startFailure(text string) bool {
	return strings.HasPrefix(text, "fork/exec") || strings.HasPrefix(text, "Preparing role access") || strings.HasPrefix(text, "exec: ")
}

// foldLaunches joins consecutive launches of one role that ended the same way
// with the same failures, word for word, into one entry that keeps the latest
// of them and says how many there were.
func foldLaunches(launches []launch) []launch {
	var folded []launch
	for _, current := range launches {
		if n := len(folded); n > 0 {
			previous := &folded[n-1]
			same := previous.signature != "" && previous.signature == current.signature && (current.Outcome == failed || current.Outcome == couldNotStart)
			if same {
				count, first, started := previous.Count+1, previous.Index, previous.Started
				records := append(previous.Records, current.Records...)
				*previous = current
				previous.Count, previous.Index, previous.Started, previous.Records = count, first, started, records
				if !previous.Started.IsZero() && !previous.Finished.IsZero() {
					previous.Duration = humanDuration(previous.Finished.Sub(previous.Started))
				}
				continue
			}
		}
		folded = append(folded, current)
	}
	return folded
}
