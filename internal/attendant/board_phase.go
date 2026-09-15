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

// projectDeliveryEnd projects the end of a delivered run onto the tracker's
// board, once per phase. Best-effort: a failed projection is logged and
// tried again next tick; nothing about the delivery depends on it.
func projectDeliveryEnd(ctx context.Context, config runtime.Config, services *runtime.Services, run state.RunOverview, tasks []runtime.BoardTask, logger Logger) {
	if services == nil || services.Board == nil || run.IssueID <= 0 {
		return
	}
	// Only an end that was posted counts: the sealed outcome of a staging,
	// release or e2e report, or an operator's sealed 「確認済み」. Files and
	// cards alone can still be the previous story (boardoutcome.go).
	runDir := runDirectory(config, run.DeliveryID)
	if _, sealed := readBoardOutcome(runDir); !sealed {
		if _, resolved := readDeliverResolution(runDir); !resolved {
			return
		}
	}
	var status RunStatus
	classifyAfterTerminal(&status, config, run, tasks)
	phase := deliveryEndPhase(status)
	if phase == "" {
		return
	}
	if recorded, ok := readBoardPhase(runDir); ok && recorded.Phase == string(phase) {
		return
	}
	if err := services.Board.ProjectBoardPhase(ctx, run.IssueID, phase); err != nil {
		logger.Error("delivery end: board projection failed", "run", run.RunID, "phase", string(phase), "error", err.Error())
		return
	}
	logger.Info("delivery end projected", "run", run.RunID, "phase", string(phase), "step", status.Step)
	encoded, err := json.Marshal(boardPhaseRecord{Phase: string(phase), At: time.Now().UTC()})
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(runDir, boardPhaseFile), encoded, 0o600)
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
