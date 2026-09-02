package attendant

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
)

// An attention state ("運用担当者が状態を確認します") used to be a dead end:
// the ticket told an operator to look, and nothing could record that they
// had. The board kept the delivery as in progress indefinitely (measured on
// a live delivery, 2026-09-02: hours of "処理中 1 / 要対応 1" after the
// staging deploy had in fact succeeded). The resolution is the operator's
// own word on the ticket — a comment whose first non-blank line is exactly
// 「確認済み」, posted after the report that asked — under the same rules as
// stop and Go: identity, order, exact text; never a mention further down.

const (
	deliverResolutionFile = "deliver-resolution.json"
	operatorConfirmedWord = "確認済み"
	// operatorConfirmationWindow bounds how long an unresolved attention
	// state keeps the attendant reading the ticket every tick. Past it the
	// board still shows the state; the tracker is no longer polled for it.
	operatorConfirmationWindow = 60 * 24 * time.Hour
)

type deliverResolution struct {
	Phase     string    `json:"phase"`   // staging | release
	Verdict   string    `json:"verdict"` // the report verdict that asked for attention
	CommentID int64     `json:"comment_id"`
	UserID    int64     `json:"user_id"`
	At        time.Time `json:"at"`
}

// attentionVerdict reports whether a report verdict is one that waits for
// an operator rather than ending the delivery.
func attentionVerdict(verdict string) bool {
	return verdict == "deploy_failed" || verdict == "merge_unverified"
}

// releaseAttentionVerdict names the attention verdict of a posted release
// report and when it was reported: the sealed outcome first, the
// production report file when the seal is missing (its write is
// best-effort), so a lost seal cannot turn the operator's confirmation
// into an unreachable state again.
func releaseAttentionVerdict(runDir string) (verdict string, since time.Time, waits bool) {
	if outcome, sealed := readBoardOutcome(runDir); sealed && outcome.Phase == "release" {
		return outcome.Verdict, outcome.At, attentionVerdict(outcome.Verdict)
	}
	if report, err := readDeliverReport(runDir, runner.DeliverProductionReportFile); err == nil {
		return report.Verdict, report.ObservedAt, attentionVerdict(report.Verdict)
	}
	return "", time.Time{}, false
}

type operatorConfirmationSource interface {
	ListComments(ctx context.Context, issueID, minCommentID int64) ([]hook.BacklogComment, error)
	AddComment(ctx context.Context, issueID int64, content string) (int64, error)
}

func readDeliverResolution(runDir string) (deliverResolution, bool) {
	raw, err := os.ReadFile(filepath.Join(runDir, deliverResolutionFile))
	if err != nil || len(raw) > 1<<16 {
		return deliverResolution{}, false
	}
	var resolution deliverResolution
	if json.Unmarshal(raw, &resolution) != nil || resolution.Phase == "" {
		return deliverResolution{}, false
	}
	return resolution, true
}

// resolveAttention records an operator's confirmation for one attention
// state reported at `since`: it posts the acknowledgement once
// (marker-idempotent, so a crash between the post and the record cannot
// double-post) and then writes the resolution file the board reads. Every
// failure is logged and retried next tick; nothing here touches the
// ledger.
func resolveAttention(ctx context.Context, tracker runtime.TrackerConfig, backlog operatorConfirmationSource, run state.RunOverview, runDir, phase, verdict string, since time.Time, logger Logger) {
	if _, resolved := readDeliverResolution(runDir); resolved {
		return
	}
	if !since.IsZero() && time.Now().After(since.Add(operatorConfirmationWindow)) {
		return
	}
	comments, err := backlog.ListComments(ctx, run.IssueID, 0)
	if err != nil {
		logger.Error("deliver attention: comment listing failed", "run", run.RunID, "error", err.Error())
		return
	}
	reportKind := hook.RunCommentStagingReport
	if phase == "release" {
		reportKind = hook.RunCommentReleaseReport
	}
	reportID, found := commentIDWithMarker(comments, hook.CommentMarker(string(reportKind), run.RunID))
	if !found {
		return
	}
	confirmation, ok := operatorConfirmation(comments, tracker, reportID)
	if !ok {
		return
	}
	ackMarker := hook.CommentMarker(string(hook.RunCommentResolved), run.RunID)
	if _, posted := commentIDWithMarker(comments, ackMarker); !posted {
		content := hook.DeliverResolvedContent(run.RunID, hook.DeliverResolvedReport{Phase: phase, Verdict: verdict})
		if _, err := backlog.AddComment(ctx, run.IssueID, content); err != nil {
			logger.Error("deliver attention: acknowledgement post failed", "run", run.RunID, "error", err.Error())
			return
		}
	}
	encoded, err := json.Marshal(deliverResolution{
		Phase: phase, Verdict: verdict, CommentID: confirmation.CommentID, UserID: confirmation.UserID, At: time.Now().UTC(),
	})
	if err != nil {
		logger.Error("deliver attention: resolution encode failed", "run", run.RunID, "error", err.Error())
		return
	}
	if err := os.WriteFile(filepath.Join(runDir, deliverResolutionFile), encoded, 0o644); err != nil {
		logger.Error("deliver attention: resolution write failed", "run", run.RunID, "error", err.Error())
		return
	}
	logger.Info("deliver attention resolved by operator", "run", run.RunID, "phase", phase, "comment", confirmation.CommentID)
}

// operatorConfirmation finds the requester's or a listed operator's
// comment, posted after the report that asked for attention, whose first
// non-blank line is exactly 「確認済み」. Only the first content line
// decides, exactly as for 「停止」 and "Go".
func operatorConfirmation(comments []hook.BacklogComment, tracker runtime.TrackerConfig, afterCommentID int64) (hook.BacklogComment, bool) {
	for _, comment := range comments {
		if comment.CommentID <= afterCommentID || !tracker.OperatorAllowed(comment.UserID) {
			continue
		}
		for _, line := range strings.Split(comment.Body, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}
			if trimmed == operatorConfirmedWord {
				return comment, true
			}
			break
		}
	}
	return hook.BacklogComment{}, false
}
