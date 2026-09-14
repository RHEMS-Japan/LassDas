package attendant

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/state"
)

// intakePausedNoticeFile records that the queued ticket was told about the
// pause, so the ticket is neither read nor written again every tick.
const intakePausedNoticeFile = "intake-paused-notice.json"

// noticeIntakePaused tells a queued ticket once that the operator paused
// intake. The record file is written only after the notice is on the ticket
// (or found there already), so a failed post is retried next tick.
func noticeIntakePaused(ctx context.Context, backlog operatorConfirmationSource, run state.RunOverview, since time.Time, runDir string, logger Logger) {
	if _, err := os.Stat(filepath.Join(runDir, intakePausedNoticeFile)); err == nil {
		return
	}
	comments, err := backlog.ListComments(ctx, run.IssueID, 0)
	if err != nil {
		logger.Error("intake pause: comment listing failed", "run", run.RunID, "error", err.Error())
		return
	}
	marker := hook.CommentMarker(string(hook.RunCommentIntakePaused), run.RunID)
	if _, posted := commentIDWithMarker(comments, marker); !posted {
		if _, err := backlog.AddComment(ctx, run.IssueID, hook.IntakePausedContent(run.RunID, since)); err != nil {
			logger.Error("intake pause: notice post failed", "run", run.RunID, "error", err.Error())
			return
		}
		logger.Info("intake pause: queued run told to wait", "run", run.RunID, "since", since.Format(time.RFC3339))
	}
	_ = os.MkdirAll(runDir, 0o700)
	_ = os.WriteFile(filepath.Join(runDir, intakePausedNoticeFile), []byte(`{"since":"`+since.Format(time.RFC3339)+`"}`), 0o600)
}
