package hook

import (
	"context"
	"log/slog"
)

// BoardPhase is the engine's own vocabulary for where a ticket stands, spoken
// without knowing any tracker. Whatever board humans watch is a projection of
// these four words; the mapping to a concrete tracker's statuses lives in that
// tracker's adapter and nowhere else.
type BoardPhase string

const (
	// BoardRunning: the automation holds the ticket and is working on it.
	BoardRunning BoardPhase = "running"
	// BoardAwaitingAnswer: a question is posted and the requester must act.
	BoardAwaitingAnswer BoardPhase = "awaiting_answer"
	// BoardDelivered: the run ended at its configured stopping point.
	BoardDelivered BoardPhase = "delivered"
	// BoardNeedsAttention: the run ended without delivering; a person decides
	// what happens next.
	BoardNeedsAttention BoardPhase = "needs_attention"
)

// BoardProjector mirrors a ticket's phase onto the board humans watch. The
// board is a view, never the truth: a projection that fails must not change
// what the automation does, and a manual move on the board is overwritten by
// the next real transition.
type BoardProjector interface {
	ProjectBoardPhase(ctx context.Context, issueID int64, phase BoardPhase) error
}

// projectBoard performs one fail-open projection: an unconfigured board is
// silence, a failed projection is a log line, and neither ever alters the
// decision that triggered it.
func projectBoard(ctx context.Context, board BoardProjector, logger *slog.Logger, issueID int64, phase BoardPhase) {
	if board == nil || issueID <= 0 {
		return
	}
	if err := board.ProjectBoardPhase(ctx, issueID, phase); err != nil {
		logger.Warn("board projection failed", "issue_id", issueID, "phase", string(phase), "error", err)
		return
	}
	logger.Info("board projected", "issue_id", issueID, "phase", string(phase))
}
