package attendant

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
)

// Three deliveries in a row dying the same way is a fact about the
// automation, not about the tickets (live, 2026-09-02: one implementer
// setting failed three tickets identically, and nothing but a person
// reading logs connected them). The attendant now stops taking new
// deliveries when that happens, says so once on the newest failed ticket,
// and resumes on the operator's 「確認済み」 there.

const (
	streakResolutionFile = "failure-streak-resolution.json"
	// streakCheckFile records when the held ticket was last read, so a
	// hold that lasts days does not read the tracker every minute.
	streakCheckFile     = "failure-streak-check.json"
	streakCheckInterval = 2 * time.Minute
)

// streakExemptCodes are endings that say nothing about the automation's
// health: successes, the requester's own stops, and honest refusals of
// tickets that could not be worked.
var streakExemptCodes = map[string]bool{
	string(hook.TerminalSuccess):               true,
	string(hook.TerminalInvestigated):          true,
	string(hook.TerminalCancelled):             true,
	string(hook.TerminalClarificationExpired):  true,
	string(hook.TerminalClarificationRequired): true,
	string(hook.TerminalInputRejected):         true,
	string(hook.TerminalReadinessRejected):     true,
	string(hook.TerminalReadinessUnresolved):   true,
}

type failureStreak struct {
	Code   string
	Count  int
	Newest state.RunOverview
	Active bool
}

// detectFailureStreak walks the terminal runs newest-first and counts how
// many in a row ended with the same failure. A run an operator has
// confirmed ends the walk: the streak before it is history. In-flight runs
// are not endings and are skipped.
func detectFailureStreak(runs []state.RunOverview, limit int, resolved func(state.RunOverview) bool) failureStreak {
	terminal := make([]state.RunOverview, 0, len(runs))
	for _, run := range runs {
		if run.State == "terminal" {
			terminal = append(terminal, run)
		}
	}
	sort.SliceStable(terminal, func(a, b int) bool { return terminal[a].ClaimedAt > terminal[b].ClaimedAt })
	var streak failureStreak
	for _, run := range terminal {
		if run.TerminalCode == "" || streakExemptCodes[run.TerminalCode] || resolved(run) {
			break
		}
		if streak.Count == 0 {
			streak.Code, streak.Newest = run.TerminalCode, run
		}
		if run.TerminalCode != streak.Code {
			break
		}
		streak.Count++
	}
	streak.Active = limit > 0 && streak.Count >= limit
	return streak
}

func streakResolvedIn(config runtime.Config) func(state.RunOverview) bool {
	return func(run state.RunOverview) bool {
		_, err := os.Stat(filepath.Join(runDirectory(config, run.DeliveryID), streakResolutionFile))
		return err == nil
	}
}

// holdForStreak keeps the hold honest on the ticket: the notice is posted
// once on the newest failed ticket, and the operator's 「確認済み」 after it
// lifts the hold — acknowledged once, then recorded as the file the
// detection stops at. Returns whether the hold is still active.
func holdForStreak(ctx context.Context, tracker runtime.TrackerConfig, backlog operatorConfirmationSource, streak failureStreak, runDir string, logger Logger) bool {
	if streakCheckedRecently(runDir, time.Now()) {
		return true
	}
	comments, err := backlog.ListComments(ctx, streak.Newest.IssueID, 0)
	if err != nil {
		logger.Error("failure streak: comment listing failed", "run", streak.Newest.RunID, "error", err.Error())
		return true
	}
	recordStreakCheck(runDir, time.Now())
	holdID, posted := commentIDWithMarker(comments, hook.CommentMarker(string(hook.RunCommentStreakHold), streak.Newest.RunID))
	if !posted {
		if _, err := backlog.AddComment(ctx, streak.Newest.IssueID, hook.FailureStreakContent(streak.Newest.RunID, streak.Code, streak.Count)); err != nil {
			logger.Error("failure streak: notice post failed", "run", streak.Newest.RunID, "error", err.Error())
		} else {
			logger.Info("failure streak: intake held", "run", streak.Newest.RunID, "code", streak.Code, "count", streak.Count)
		}
		return true
	}
	confirmation, ok := operatorConfirmation(comments, tracker, holdID)
	if !ok {
		return true
	}
	if _, acked := commentIDWithMarker(comments, hook.CommentMarker(string(hook.RunCommentStreakResolved), streak.Newest.RunID)); !acked {
		if _, err := backlog.AddComment(ctx, streak.Newest.IssueID, hook.StreakResolvedContent(streak.Newest.RunID)); err != nil {
			logger.Error("failure streak: acknowledgement post failed", "run", streak.Newest.RunID, "error", err.Error())
			return true
		}
	}
	encoded, err := json.Marshal(map[string]any{
		"code": streak.Code, "count": streak.Count,
		"comment_id": confirmation.CommentID, "user_id": confirmation.UserID, "at": time.Now().UTC(),
	})
	if err == nil {
		err = os.MkdirAll(runDir, 0o755)
	}
	if err == nil {
		err = os.WriteFile(filepath.Join(runDir, streakResolutionFile), encoded, 0o644)
	}
	if err != nil {
		logger.Error("failure streak: resolution record failed", "run", streak.Newest.RunID, "error", err.Error())
		return true
	}
	logger.Info("failure streak: resolved by operator, intake resumes", "run", streak.Newest.RunID, "comment", confirmation.CommentID)
	return false
}

// streakNotice is the board's one-line banner while intake is held.
func streakNotice(streak failureStreak) string {
	return fmt.Sprintf("同じ失敗 (%s) が %d 回連続 — 運用担当者の確認待ち。%s に「確認済み」と書くと受付を再開します",
		hook.DescribeTerminalCode(streak.Code), streak.Count, streak.Newest.IssueKey)
}

func streakCheckedRecently(runDir string, now time.Time) bool {
	raw, err := os.ReadFile(filepath.Join(runDir, streakCheckFile))
	if err != nil {
		return false
	}
	var record struct {
		At time.Time `json:"at"`
	}
	if json.Unmarshal(raw, &record) != nil || record.At.IsZero() {
		return false
	}
	return now.Before(record.At.Add(streakCheckInterval))
}

// recordStreakCheck is best-effort: a failed write costs one extra read of
// the ticket next tick, nothing else.
func recordStreakCheck(runDir string, now time.Time) {
	encoded, err := json.Marshal(map[string]time.Time{"at": now.UTC()})
	if err != nil {
		return
	}
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(runDir, streakCheckFile), encoded, 0o644)
}
