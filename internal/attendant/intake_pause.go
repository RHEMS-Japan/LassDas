package attendant

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
)

// intakePausedNoticeFile records which pause the queued ticket was told
// about, so the ticket is neither read nor written again every tick while
// that pause lasts. A later pause (another instant) is told again.
const intakePausedNoticeFile = "intake-paused-notice.json"

// holdQueuedRun decides whether a queued run stays queued this tick: the
// operator's pause holds it, with one notice on its ticket. False means the
// run may start.
//
// The hold that counted deliveries ending the same way was read here too.
// A card that fails is climbed away from rather than reported now, so the
// run of identical endings it watched for cannot form.
func holdQueuedRun(ctx context.Context, config runtime.Config, backlog operatorConfirmationSource, run state.RunOverview, runDir string, logger Logger) bool {
	if since, paused := config.Chain.IntakePaused(); paused {
		noticeIntakePaused(ctx, backlog, run, since, runDir, logger)
		return true
	}
	return false
}

// noticeIntakePaused tells a queued ticket once per pause that the operator
// paused intake. The record is written only after the notice is on the
// ticket (posted now, or found there by its marker), so a failed post is
// retried next tick.
func noticeIntakePaused(ctx context.Context, backlog operatorConfirmationSource, run state.RunOverview, since time.Time, runDir string, logger Logger) {
	if toldAbout(runDir, since) {
		return
	}
	comments, err := backlog.ListComments(ctx, run.IssueID, 0)
	if err != nil {
		logger.Error("intake pause: comment listing failed", "run", run.RunID, "error", err.Error())
		return
	}
	if _, posted := commentIDWithMarker(comments, hook.IntakePausedMarker(run.RunID, since)); !posted {
		if _, err := backlog.AddComment(ctx, run.IssueID, hook.IntakePausedContent(run.RunID, since)); err != nil {
			logger.Error("intake pause: notice post failed", "run", run.RunID, "error", err.Error())
			return
		}
		logger.Info("intake pause: queued run told to wait", "run", run.RunID, "since", since.Format(time.RFC3339))
	}
	recordTold(runDir, since)
}

type intakePausedRecord struct {
	Since string `json:"since"`
}

func toldAbout(runDir string, since time.Time) bool {
	raw, err := os.ReadFile(filepath.Join(runDir, intakePausedNoticeFile))
	if err != nil {
		return false
	}
	var record intakePausedRecord
	if json.Unmarshal(raw, &record) != nil {
		return false
	}
	return record.Since == since.UTC().Format(time.RFC3339)
}

func recordTold(runDir string, since time.Time) {
	// 0711 like every other writer of the run directory: the agent user
	// must be able to enter it later (docs/RUNTIME_POD.md).
	_ = os.MkdirAll(runDir, 0o711)
	encoded, _ := json.Marshal(intakePausedRecord{Since: since.UTC().Format(time.RFC3339)})
	_ = os.WriteFile(filepath.Join(runDir, intakePausedNoticeFile), encoded, 0o600)
}
