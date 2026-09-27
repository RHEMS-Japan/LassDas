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
)

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
		first := ""
		for _, line := range strings.Split(comment.Content, "\n") {
			if first = strings.TrimSpace(line); first != "" {
				break
			}
		}
		if first != "停止" {
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
func runWatchedRequest(ctx context.Context, cfg config, issue sourceIssue, directory, configPath, requestPath string, interval time.Duration, slots chan struct{}, log io.Writer) error {
	observe := func(message string) { fmt.Fprintf(log, "request %d: %s\n", issue.ID, message) }
	tick := time.NewTicker(interval)
	defer tick.Stop()
	var result chan error
	var cancel context.CancelFunc
	stopChild := func() {
		if cancel != nil {
			cancel()
			<-result
			<-slots
			cancel, result = nil, nil
		}
	}
	defer stopChild()
	var instruction json.RawMessage
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var err error
		if instruction == nil {
			// Bound an unresponsive read independently of an executing role. A
			// missing control channel pauses work, not the request's goal.
			readCtx, release := context.WithTimeout(ctx, interval)
			var rows []json.RawMessage
			rows, err = cfg.Backlog.Comments(readCtx, issue.Key, 0)
			release()
			if err == nil {
				instruction, err = stopInstruction(rows, issue, cfg.Intake.StopUserIDs)
			}
			if err == nil && issue.Creator.ID <= 0 && len(cfg.Intake.StopUserIDs) == 0 {
				err = errors.New("requester identity is unavailable for stop instructions")
			}
		}
		if instruction != nil {
			stopChild()
			if err := writeRuntimeFile(filepath.Join(directory, "stop-request.json"), instruction); err == nil {
				observe("stopped at an authorized user's request; earlier external effects have not been undone")
				return nil
			} else {
				observe("stopped; waiting to retain the original stop instruction: " + err.Error())
			}
		} else if err != nil {
			stopChild()
			observe("work paused while stop instructions are unavailable: " + err.Error())
		} else if result == nil {
			select {
			case slots <- struct{}{}:
				workCtx, releaseWork := context.WithCancel(ctx)
				cancel = releaseWork
				result = make(chan error, 1)
				outcome := result
				fmt.Fprintf(log, "starting accepted request %d\n", issue.ID)
				go func() {
					defer releaseWork()
					outcome <- run(workCtx, []string{"--config", configPath, "--request", requestPath, "--run-dir", filepath.Join(directory, "run")}, io.Discard, log)
				}()
			default:
				// Keep reading this queued request's control comments even while
				// all execution slots are occupied by other work.
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-result:
			cancel()
			<-slots
			cancel, result = nil, nil
			return err
		case <-tick.C:
		}
	}
}
