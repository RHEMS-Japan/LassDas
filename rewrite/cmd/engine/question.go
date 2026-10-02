package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"ticket-runner/internal/chain"
	"ticket-runner/internal/tracker"
)

// Before the work is handed over, a configured role may put a question to the
// person who filed the request. Which role that is, is operator configuration
// naming an existing role; nothing here reads, decodes or grades what any role
// or requester wrote. The reply is carried into the history exactly as posted.
func validateQuestionRole(cfg config) error {
	if cfg.Intake == nil || cfg.Intake.QuestionRole == "" {
		return nil
	}
	for _, role := range cfg.Roles {
		if role.Name != cfg.Intake.QuestionRole || role.Name == "done" {
			continue
		}
		for _, process := range role.Processes {
			if process.TrackerAccess == "comment" || process.TrackerAccess == "every-comment" {
				return nil
			}
		}
	}
	return errors.New("intake.question_role must name a configured role with a comment-capable process")
}

// questionBoundary is how far the assigned issue's comments had gone when the
// question was asked. Everything at or below it was already available to the
// asking role, so only a later comment can be an answer to that question.
type questionBoundary struct {
	After *int64 `json:"after"`
}

func latestComment(source tracker.Tracker, rows []json.RawMessage, issue sourceIssue) (int64, error) {
	highest := int64(0)
	for _, raw := range rows {
		comment, err := source.ReadComment(raw, issue)
		if err != nil || !comment.OnIssue {
			return 0, errors.New("issue comments could not be read for the assigned issue")
		}
		if comment.ID > highest {
			highest = comment.ID
		}
	}
	return highest, nil
}

func recordQuestion(source tracker.Tracker, directory string, rows []json.RawMessage, issue sourceIssue) error {
	highest, err := latestComment(source, rows, issue)
	if err != nil {
		return err
	}
	data, err := json.Marshal(questionBoundary{After: &highest})
	if err != nil {
		return err
	}
	return writeRuntimeFile(filepath.Join(directory, "question.json"), data)
}

// answerToQuestion returns the first comment above the recorded boundary that
// the issue's creator or a configured operator wrote. The account identity
// comes from native tracker metadata, not from a name claimed in the text. A
// stop instruction is never an answer, so stopped work stays stopped.
//
// A comment without words is not an answer either: the tracker records a
// status or field change as a comment with empty content, and the runtime
// itself makes such changes while it waits. Where the runtime's own account
// filed the issue, that is what kept it from waiting at all. Comments by the
// runtime's account are not set aside beyond that: an account that is
// neither the creator nor an operator is already not authorized, and one
// that is may be the only account the requester has.
func answerToQuestion(source tracker.Tracker, rows []json.RawMessage, issue sourceIssue, operators []int64, after int64) (int64, string, error) {
	for _, raw := range rows {
		comment, err := source.ReadComment(raw, issue)
		if err != nil || !comment.OnIssue {
			return 0, "", errors.New("issue comments could not be read for the assigned issue")
		}
		if comment.ID <= after {
			continue
		}
		authorized := comment.Author.ID > 0 && comment.Author.ID == issue.Creator.ID
		for _, id := range operators {
			authorized = authorized || id > 0 && comment.Author.ID == id
		}
		if !authorized || strings.TrimSpace(comment.Body) == "" || firstInstructionLine(comment.Body) == "停止" {
			continue
		}
		return comment.ID, comment.Body, nil
	}
	return 0, "", nil
}

// resumeWaitingRequest reports whether a request that is waiting for a person
// may run again, and whether that is because they answered. An authorized
// stop hands it to the existing stop machinery untouched: it runs again only
// for the stop to be recorded, and it was not answered. Otherwise only a
// reply above the recorded boundary resumes it.
func resumeWaitingRequest(ctx context.Context, cfg config, issue sourceIssue, directory, request string, state chain.State, interval time.Duration) (resume, answered bool, err error) {
	readCtx, release := context.WithTimeout(ctx, interval)
	source := cfg.source()
	rows, err := source.Comments(readCtx, issue)
	release()
	if err != nil {
		return false, false, err
	}
	stop, err := stopInstruction(source, rows, issue, cfg.Intake.StopUserIDs)
	if err != nil {
		return false, false, err
	}
	if stop != nil {
		return true, false, nil
	}
	path := filepath.Join(directory, "question.json")
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		// An interrupted hold left no boundary. Record where the conversation
		// stands now and keep waiting, rather than reading an older comment as
		// an answer to a question it cannot have replied to.
		return false, false, recordQuestion(source, directory, rows, issue)
	}
	if err != nil {
		return false, false, err
	}
	var boundary questionBoundary
	if err := json.Unmarshal(raw, &boundary); err != nil || boundary.After == nil || *boundary.After < 0 {
		return false, false, errors.New("the recorded question is unreadable; the request keeps waiting for the requester's answer")
	}
	id, answer, err := answerToQuestion(source, rows, issue, cfg.Intake.StopUserIDs, *boundary.After)
	if err != nil || id == 0 {
		return false, false, err
	}
	if err := appendAnswer(directory, request, state.Step, answer); err != nil {
		return false, false, err
	}
	// Keep the answered question, so the same comment cannot be read as a
	// second answer after a restart or a later question.
	if err := os.Rename(path, filepath.Join(directory, "answer-"+strconv.FormatInt(id, 10)+".json")); err != nil {
		return false, false, err
	}
	return true, true, nil
}

// The requester's own words join the history like any other report. They are
// not validated, summarized or turned into a completion mark, and the step
// stays where it was so the configured connections still decide what is next.
func appendAnswer(directory, request, role, answer string) error {
	store, err := chain.Open(filepath.Join(directory, "run"), request)
	if err != nil {
		return err
	}
	state, err := store.Load()
	if err != nil {
		store.Close()
		return err
	}
	if !state.Waiting {
		return store.Close()
	}
	state.History = append(state.History, chain.Result{
		Role: role, Speaker: "requester", Output: answer, FinishedAt: time.Now().UTC(),
	})
	state.Waiting = false
	saveErr := store.Save(state)
	closeErr := store.Close()
	if saveErr != nil {
		return saveErr
	}
	return closeErr
}
