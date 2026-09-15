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

// The terminal report projects a delivered ticket onto the tracker's board
// as "running" when the delivery continues past the pull request (hook:
// deliveryContinues), and nothing projected the ending after that: a ticket
// merged to staging and finished - or one that asked an operator to look -
// stayed 自動処理中 for ever, and every finished ticket was closed by hand
// (live 2026-09-15: a ticket's report said 完了, its status said 自動処理中).
//
// projectDeliveryEnd reads the run's end the way the status board does
// (classifyAfterTerminal, the one definition of what a run's files and
// cards mean) and projects the phase that end calls for, once per phase,
// on every tick: a projection lost to a crash is made on the next tick,
// and a run whose end changes (an operator's 「確認済み」 after an attention
// report) is projected again.

const boardPhaseFile = "board-phase.json"

type boardPhaseRecord struct {
	Phase string    `json:"phase"`
	At    time.Time `json:"at"`
}

// deliveryEndPhase is the tracker phase a status board step calls for: a
// run that rests as done with nothing for a person to do is delivered; a
// run resting as done with an action for the operator, or as failed,
// attention or stopped, needs attention; a run still moving (a Go wait, a
// card in flight) is projected by nobody here.
func deliveryEndPhase(status RunStatus) hook.BoardPhase {
	switch status.Step {
	case "done":
		if status.NextAction == "" || strings.HasPrefix(status.NextAction, "対応不要") {
			return hook.BoardDelivered
		}
		return hook.BoardNeedsAttention
	case "failed", "attention", "stopped":
		return hook.BoardNeedsAttention
	}
	return ""
}

// issueStatusReader is what the guard below needs from the tracker: the
// ticket's current status id. *backlog.Client provides it.
type issueStatusReader interface {
	IssueStatusID(ctx context.Context, issueID int64) (int64, error)
}

// boardOwned says the ticket sits in one of the four statuses the automation
// itself moves tickets between. A ticket in any other status - one a person
// closed as 完了, or moved by hand - is not the automation's to move: the
// projection leaves it alone and remembers that it did.
func boardOwned(statusID int64, statuses runtime.BoardStatuses) bool {
	return statusID > 0 && (statusID == statuses.Running || statusID == statuses.AwaitingAnswer ||
		statusID == statuses.Delivered || statusID == statuses.NeedsAttention)
}

// untouchedRecord is the phase record written when the ticket was not the
// automation's to move: the phase it would have projected, marked, so the
// tracker is not asked again until the run's end changes.
func untouchedRecord(phase hook.BoardPhase) string { return "untouched:" + string(phase) }

// projectDeliveryEnd projects the end of a delivered run onto the tracker's
// board, once per phase. Best-effort: a failed projection is logged and
// tried again next tick; nothing about the delivery depends on it.
//
// The end is read through classifyAfterTerminal, not classifyRun: the
// latter fills a default NextAction on every done step, which would read as
// an operator's action here.
func projectDeliveryEnd(ctx context.Context, config runtime.Config, services *runtime.Services, run state.RunOverview, tasks []runtime.BoardTask, logger Logger) {
	if services == nil || services.Board == nil || run.IssueID <= 0 {
		return
	}
	// Only an end that was posted counts, and only the end of the latest
	// phase the run reached. The run is in its release phase once a
	// production report file exists (the promote card wrote it) or a
	// release seal exists (the release report was posted: a stop or an
	// expiry during the Go wait, a dead promote card, seal without any
	// file). A production report file whose release report is not posted
	// yet is the previous story still: the classification prefers the
	// file, the ticket has not been told, so nothing is projected until
	// the release seal arrives.
	runDir := runDirectory(config, run.DeliveryID)
	outcome, sealed := readBoardOutcome(runDir)
	latest := "staging"
	if deliverFileExists(runDir, runner.DeliverProductionReportFile) || sealed && outcome.Phase == "release" {
		latest = "release"
	}
	if _, resolved := readDeliverResolution(runDir); !resolved && !(sealed && outcome.Phase == latest) {
		return
	}
	var status RunStatus
	classifyAfterTerminal(&status, config, run, tasks)
	phase := deliveryEndPhase(status)
	if phase == "" {
		return
	}
	if recorded, ok := readBoardPhase(runDir); ok && (recorded.Phase == string(phase) || recorded.Phase == untouchedRecord(phase)) {
		return
	}
	var reader issueStatusReader
	if services.Backlog != nil {
		reader = services.Backlog
	}
	if reader == nil {
		logger.Error("delivery end: no tracker client to read the ticket's status", "run", run.RunID)
		return
	}
	current, err := reader.IssueStatusID(ctx, run.IssueID)
	if err != nil {
		if class, kind := hook.FailureDetails(err); class == hook.FailureRejected && kind == "not_found" {
			// The ticket is gone: nothing to move, and nothing to ask
			// again (the hook treats a vanished issue the same way).
			logger.Info("delivery end: ticket not found; left alone", "run", run.RunID, "phase", string(phase))
			writeBoardPhase(runDir, untouchedRecord(phase), logger, run.RunID)
			return
		}
		logger.Error("delivery end: ticket status read failed", "run", run.RunID, "error", err.Error())
		return
	}
	if !boardOwned(current, config.Tracker.BoardStatuses) {
		logger.Info("delivery end: ticket is not in an automation status; left alone", "run", run.RunID, "phase", string(phase), "status_id", current)
		writeBoardPhase(runDir, untouchedRecord(phase), logger, run.RunID)
		return
	}
	if err := services.Board.ProjectBoardPhase(ctx, run.IssueID, phase); err != nil {
		logger.Error("delivery end: board projection failed", "run", run.RunID, "phase", string(phase), "error", err.Error())
		return
	}
	logger.Info("delivery end projected", "run", run.RunID, "phase", string(phase), "step", status.Step)
	writeBoardPhase(runDir, string(phase), logger, run.RunID)
}

// writeBoardPhase records the projected (or deliberately untouched) phase.
// A record that cannot be written is logged: without it the same
// projection would be made on every tick.
func writeBoardPhase(runDir, phase string, logger Logger, runID string) {
	encoded, err := json.Marshal(boardPhaseRecord{Phase: phase, At: time.Now().UTC()})
	if err != nil {
		return
	}
	if err := os.WriteFile(filepath.Join(runDir, boardPhaseFile), encoded, 0o600); err != nil {
		logger.Error("delivery end: phase record write failed", "run", runID, "error", err.Error())
	}
}

func readBoardPhase(runDir string) (boardPhaseRecord, bool) {
	raw, err := os.ReadFile(filepath.Join(runDir, boardPhaseFile))
	if err != nil {
		return boardPhaseRecord{}, false
	}
	var record boardPhaseRecord
	if json.Unmarshal(raw, &record) != nil || record.Phase == "" {
		return boardPhaseRecord{}, false
	}
	return record, true
}
