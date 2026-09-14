package attendant

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
)

const (
	// lateWordCheckFile throttles the ticket reads a finished run still
	// makes: once an hour, for lateWordWindow after it ended.
	lateWordCheckFile     = "late-word-check.json"
	lateWordCheckInterval = time.Hour
	lateWordWindow        = 14 * 24 * time.Hour
)

// dueGoReminders lists the reminders (1-based) whose instant has passed
// while the wait is still open: the 1st, 3rd and 5th weekday after the
// staging report at 10:00 Asia/Tokyo — the questions' rhythm — cut at the
// Go deadline, so a short wait gets fewer reminders and never one after
// the report that closes it.
func dueGoReminders(now, observedAt, deadline time.Time) []int {
	notifyAt, _ := hook.ComputeQuestionSchedule(observedAt)
	var due []int
	for index, at := range notifyAt {
		instant := time.UnixMilli(at)
		if !now.Before(instant) && instant.Before(deadline) {
			due = append(due, index+1)
		}
	}
	return due
}

// remindGo posts the due reminders that are not yet on the ticket. The
// comments were listed by the caller this tick; a reminder posted now is
// found by its marker next tick.
func remindGo(ctx context.Context, backlog operatorConfirmationSource, run state.RunOverview, observedAt, deadline time.Time, comments []hook.BacklogComment, logger Logger) {
	for _, n := range dueGoReminders(time.Now(), observedAt, deadline) {
		if _, posted := commentIDWithMarker(comments, hook.GoReminderMarker(run.RunID, n)); posted {
			continue
		}
		if _, err := backlog.AddComment(ctx, run.IssueID, hook.GoReminderContent(run.RunID, n, deadline)); err != nil {
			logger.Error("go reminder: post failed", "run", run.RunID, "n", n, "error", err.Error())
			return
		}
		logger.Info("go reminder posted", "run", run.RunID, "n", n)
	}
}

// noticeLateGo answers a Go that arrived after the Go wait expired: the
// release report says the wait is over; a later Go by the requester gets
// one reply saying nothing resumes and how to continue.
func noticeLateGo(ctx context.Context, config runtime.Config, backlog operatorConfirmationSource, run state.RunOverview, runDir string, logger Logger) {
	outcome, sealed := readBoardOutcome(runDir)
	if !sealed || outcome.Phase != "release" || outcome.Verdict != "expired" {
		return
	}
	noticeLateWord(ctx, backlog, run, runDir, outcome.At, func(comments []hook.BacklogComment) bool {
		reportID, found := commentIDWithMarker(comments, hook.CommentMarker(string(hook.RunCommentReleaseReport), run.RunID))
		return found && containsGoComment(comments, config.Tracker.AllowedCreatorID, reportID)
	}, "Go", logger)
}

// noticeLateAnswer answers a requester comment that arrived after a
// question's deadline: the terminal report says the run ended unanswered;
// a later comment by the requester gets one reply.
func noticeLateAnswer(ctx context.Context, config runtime.Config, backlog operatorConfirmationSource, run state.RunOverview, runDir string, logger Logger) {
	if run.TerminalCode != string(hook.TerminalClarificationExpired) || run.CompletedAt <= 0 {
		return
	}
	noticeLateWord(ctx, backlog, run, runDir, time.UnixMilli(run.CompletedAt), func(comments []hook.BacklogComment) bool {
		prefix := hook.TerminalCommentMarkerPrefix(run.RunID, string(hook.TerminalClarificationExpired))
		terminalID, found := int64(0), false
		for _, comment := range comments {
			if strings.HasPrefix(hook.ExtractCommentMarker(comment.Body), prefix) {
				terminalID, found = comment.CommentID, true
			}
		}
		if !found {
			return false
		}
		for _, comment := range comments {
			if comment.CommentID > terminalID && comment.UserID == config.Tracker.AllowedCreatorID && hook.ExtractCommentMarker(comment.Body) == "" {
				return true
			}
		}
		return false
	}, "回答", logger)
}

// noticeLateWord is the shared shape: within the window after the ending,
// at most once an hour, read the ticket; if the caller's detector finds the
// late word after the ending's own comment and no reply is there yet, post
// the one reply.
func noticeLateWord(ctx context.Context, backlog operatorConfirmationSource, run state.RunOverview, runDir string, endedAt time.Time, late func([]hook.BacklogComment) bool, word string, logger Logger) {
	now := time.Now()
	if endedAt.IsZero() || now.After(endedAt.Add(lateWordWindow)) || lateWordCheckedRecently(runDir, now) {
		return
	}
	recordLateWordCheck(runDir, now)
	comments, err := backlog.ListComments(ctx, run.IssueID, 0)
	if err != nil {
		logger.Error("late word: comment listing failed", "run", run.RunID, "error", err.Error())
		return
	}
	if _, replied := commentIDWithMarker(comments, hook.LateWordMarker(run.RunID)); replied {
		return
	}
	if !late(comments) {
		return
	}
	if _, err := backlog.AddComment(ctx, run.IssueID, hook.LateWordContent(run.RunID, word)); err != nil {
		logger.Error("late word: reply post failed", "run", run.RunID, "error", err.Error())
		return
	}
	logger.Info("late word answered", "run", run.RunID, "word", word)
}

type lateWordCheck struct {
	At time.Time `json:"at"`
}

func lateWordCheckedRecently(runDir string, now time.Time) bool {
	raw, err := os.ReadFile(filepath.Join(runDir, lateWordCheckFile))
	if err != nil {
		return false
	}
	var check lateWordCheck
	if json.Unmarshal(raw, &check) != nil {
		return false
	}
	return now.Before(check.At.Add(lateWordCheckInterval))
}

func recordLateWordCheck(runDir string, now time.Time) {
	_ = os.MkdirAll(runDir, 0o711)
	encoded, _ := json.Marshal(lateWordCheck{At: now.UTC()})
	_ = os.WriteFile(filepath.Join(runDir, lateWordCheckFile), encoded, 0o600)
}
