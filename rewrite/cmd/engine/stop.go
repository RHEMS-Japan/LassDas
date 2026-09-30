package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ticket-runner/internal/chain"
)

// An instruction is read from the first nonblank line only, so a quotation, an
// example or a later mention of the word is not one.
func firstInstructionLine(content string) string {
	first := ""
	for _, line := range strings.Split(content, "\n") {
		if first = strings.TrimSpace(line); first != "" {
			break
		}
	}
	return first
}

// A stop is an instruction from the requester or an explicitly configured
// operator. It is never inferred from a role report or approved by a model.
func stopInstruction(rows []json.RawMessage, issue sourceIssue, operators []int64) (json.RawMessage, error) {
	for _, raw := range rows {
		var comment struct {
			ID, IssueID, ProjectID int64
			Content                string
			CreatedUser            struct{ ID int64 }
		}
		if err := json.Unmarshal(raw, &comment); err != nil || comment.ID <= 0 || comment.IssueID != issue.ID || comment.ProjectID != issue.ProjectID {
			return nil, errors.New("stop comments could not be read for the assigned issue")
		}
		if firstInstructionLine(comment.Content) != "停止" {
			continue
		}
		authorized := comment.CreatedUser.ID > 0 && comment.CreatedUser.ID == issue.Creator.ID
		for _, id := range operators {
			authorized = authorized || id > 0 && comment.CreatedUser.ID == id
		}
		if authorized {
			return raw, nil
		}
	}
	return nil, nil
}

// Preserve the actual user's native instruction, not a model completion mark.
// Missing/deleted remote comments cannot silently resume stopped work. Damaged
// local stop records hold the work; they are not permission to resume either.
func savedStop(directory string, issue sourceIssue, operators []int64) (bool, error) {
	raw, err := os.ReadFile(filepath.Join(directory, "stop-request.json"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	stop, err := stopInstruction([]json.RawMessage{raw}, issue, operators)
	if err != nil || stop == nil {
		return false, errors.New("saved stop instruction is unreadable or no longer matches its source; work remains held")
	}
	return true, nil
}

// Monitor queued and running work independently of discovery and of the model.
// Slots restrict actual engine executions, not observation of a queued stop.
// toleratedUnreadableTicks is how many ticks in a row the tracker may fail to
// answer before the work is paused for want of its control channel. A single
// slow answer used to end a role's launch mid-way and cost its work; a stop
// filed during those ticks is still read as soon as the tracker answers.
const toleratedUnreadableTicks = 3

func runWatchedRequest(ctx context.Context, cfg config, issue sourceIssue, directory, configPath, requestPath string, interval time.Duration, turns *turnstile, log io.Writer) error {
	observe := func(message string) { fmt.Fprintf(log, "request %d: %s\n", issue.ID, message) }
	tick := time.NewTicker(interval)
	defer tick.Stop()
	var result chan error
	var cancel context.CancelFunc
	stopChild := func() {
		if cancel != nil {
			cancel()
			<-result
			turns.release()
			cancel, result = nil, nil
		}
	}
	defer stopChild()
	defer turns.leave(issue.ID)
	var instruction json.RawMessage
	var rows []json.RawMessage
	waiting := false
	// A tracker that does not answer for one tick is not a lost control
	// channel; the work goes on, and only a read that keeps failing pauses it.
	unreadable := 0
	notice := requestNotices(cfg, issue, directory)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// A notice that was recorded but never confirmed belongs to this
		// request, not to the tick that first met its condition.
		if err := notice.flush(ctx); err != nil {
			observe("earlier notice not confirmed: " + err.Error())
		}
		var err error
		if instruction == nil {
			// Bound an unresponsive read independently of an executing role. A
			// missing control channel pauses work, not the request's goal.
			readCtx, release := context.WithTimeout(ctx, interval)
			rows, err = cfg.Backlog.Comments(readCtx, issue.Key, 0)
			release()
			if err == nil {
				instruction, err = stopInstruction(rows, issue, cfg.Intake.StopUserIDs)
			}
			if err == nil && issue.Creator.ID <= 0 && len(cfg.Intake.StopUserIDs) == 0 {
				err = errors.New("requester identity is unavailable for stop instructions")
			}
			if err == nil {
				unreadable = 0
			}
		}
		// The shared model key running out is the one failure no role can
		// recover from. Hold the work while it lasts, say so once, and carry
		// on by itself when the budget returns.
		hold := false
		if instruction == nil && err == nil && !waiting {
			low, known := modelCreditHold(ctx, cfg, observe)
			if known {
				hold = low
				if noticeErr := applyBudgetNotice(ctx, notice, low); noticeErr != nil {
					observe("budget notice not confirmed: " + noticeErr.Error())
				}
			}
			// Nothing here changes routing; it only tells the requester that a
			// long silence is retrying, not finished and not abandoned.
			if noticeErr := noteStall(ctx, cfg, notice, directory); noticeErr != nil {
				observe("no-progress notice not confirmed: " + noticeErr.Error())
			}
		}
		if instruction != nil {
			stopChild()
			if err := writeRuntimeFile(filepath.Join(directory, "stop-request.json"), instruction); err == nil {
				observe("stopped at an authorized user's request; earlier external effects have not been undone")
				return reportStoppedRequest(ctx, cfg, issue, directory, turns, log)
			} else {
				turns.leave(issue.ID)
				observe("stopped; waiting to retain the original stop instruction: " + err.Error())
			}
		} else if err != nil {
			unreadable++
			if unreadable < toleratedUnreadableTicks {
				observe(fmt.Sprintf("stop instructions could not be read (%d of %d before the work pauses): %s", unreadable, toleratedUnreadableTicks, err.Error()))
			} else {
				stopChild()
				turns.leave(issue.ID)
				observe("work paused while stop instructions are unavailable: " + err.Error())
			}
		} else if waiting {
			// The engine put a question to the requester and stopped there.
			// Record how far the comments had gone, then leave the request to
			// the collector, which starts it again when the answer arrives.
			if err := recordQuestion(directory, rows, issue); err != nil {
				turns.leave(issue.ID)
				observe("waiting to record the question put to the requester: " + err.Error())
			} else {
				observe("waiting for the requester's answer at the assigned issue")
				return nil
			}
		} else if hold {
			// Stop the child the same way an authorized stop does, but keep the
			// request: the next tick relaunches it once the budget is back.
			stopChild()
			turns.leave(issue.ID)
		} else if result == nil {
			// The slot goes to the earliest request in line; a later one keeps
			// reading its control comments and asks again on the next tick.
			if turns.try(issue.ID) {
				workCtx, releaseWork := context.WithCancel(ctx)
				cancel = releaseWork
				result = make(chan error, 1)
				outcome := result
				fmt.Fprintf(log, "starting accepted request %d\n", issue.ID)
				if cfg.Intake != nil && cfg.Intake.Announce && startDeservesNotice(cfg, issue, directory) {
					if noticeErr := notice.post(ctx, startedNotice, startedNoticeText); noticeErr != nil {
						observe("start not announced: " + noticeErr.Error())
					}
				}
				go func() {
					defer releaseWork()
					outcome <- run(workCtx, []string{"--config", configPath, "--request", requestPath, "--run-dir", filepath.Join(directory, "run")}, io.Discard, log)
				}()
			}
		}
		if result != nil {
			announceStages(ctx, cfg, issue, directory, observe)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-result:
			cancel()
			turns.release()
			cancel, result = nil, nil
			if !errors.Is(err, chain.ErrWaiting) {
				return err
			}
			waiting = true
		case <-tick.C:
		}
	}
}
