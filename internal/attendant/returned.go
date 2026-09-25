package attendant

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
	"automation.internal/ticket-ingress/internal/worker"
)

// An implementing agent that hands the work back used to be the end of the
// delivery. The report was true and the ending was honest, and it still
// left a requester who went to bed with a ticket waking up to a question.
// Three things a report asks for — a decision the ticket did not make, a
// key, a way into something outside the run — and the requester asked for
// none of them to come back: decide it, stand something in for it, and go
// on.
//
// So the report is read, answered here, and the same round is started
// again. The same round rather than the next one: nothing was produced, so
// there is nothing to review, nothing to compare against and nothing to
// carry forward. What changes is the instruction, which now carries what
// the engine decided and the agent's own previous words.
//
// What the engine decided is written down before the round starts again.
// That is what the requester reads at the end — not "the AI asked for a
// key" but "a stand-in was built and this is what has to be supplied" — and
// it is also what makes a round that keeps coming back visible as exactly
// that, instead of a loop nobody can see.

// returnVerdict is what became of a round that was handed back.
type returnVerdict int

const (
	// returnRelaunched: the round is running again under the engine's own
	// answer, and this tick is finished.
	returnRelaunched returnVerdict = iota
	// returnStopped: the requester asked the delivery to stop, which is
	// read at the boundary before anything is started again.
	returnStopped
	// returnUnanswered: the engine could not start the round again. The
	// caller reports the failure the classification carried, which names
	// the machinery rather than the agent — the agent answered.
	returnUnanswered
)

// answerReturnedWork decides what a returned round is told and starts it
// again.
func answerReturnedWork(
	ctx context.Context,
	config runtime.Config,
	services *runtime.Services,
	hermes *runtime.Hermes,
	envelope hook.DispatchEnvelope,
	run state.RunOverview,
	view chainView,
	logger Logger,
) (returnVerdict, error) {
	runDir := runDirectory(config, run.DeliveryID)
	agentRun, err := worker.ReadImplementingRun(filepath.Join(runDir, "history"), view.round)
	if err != nil {
		// The record was readable a moment ago — it is what classified this
		// card as an answer — so this is a volume that stopped answering
		// rather than a round that said nothing.
		logger.Error("the returned report could not be read back; the failure is reported instead",
			"run", run.RunID, "round", view.round, "error", err.Error())
		return returnUnanswered, nil
	}
	previous, err := runner.ReadReturns(runDir, view.round)
	if err != nil {
		// The cost of going on is that a repeat is not recognised as one:
		// the round is answered as though it were the first time. The cost
		// of stopping is handing the work back, which is the one thing this
		// path exists to prevent, so it goes on.
		logger.Error("this round's earlier answers could not be read; it is answered as a first return",
			"run", run.RunID, "round", view.round, "error", err.Error())
		previous = nil
	}
	answer := worker.AnswerReturn(agentRun.Transcript, previous, time.Now().UTC())
	// 「停止」 at this boundary, through the same reader the regenerating
	// paths use: this is the third place a round starts spending again, and
	// a delivery whose requester cannot be heard at all goes on rather than
	// stopping on a reader that is missing.
	//
	// Read after the answer is composed rather than before. A requester who
	// asked the delivery to stop is obeyed either way, and what the engine
	// had decided by the time they asked is still worth writing down.
	stopped, stopErr := roundBoundaryStop(ctx, services, config, envelope)
	if stopErr != nil {
		return returnUnanswered, stopErr
	}
	if err := runner.RecordReturn(runDir, view.round, answer); err != nil {
		// The relaunch reads this file. Started again without it, the agent
		// is handed the instruction it has already answered, which is the
		// silent loop this whole path is built to avoid. The tick fails and
		// the next one answers the round again from the same records.
		return returnUnanswered, fmt.Errorf("the answer to round %d could not be recorded: %w", view.round, err)
	}
	logger.Info("the implementer handed the work back; the engine answered it and the round runs again",
		"run", run.RunID, "round", view.round, "attempt", answer.Attempt,
		"assumption", answer.Assumption.Kind, "supplies", len(answer.Supply), "repeated", answer.Repeated)
	if stopped {
		return returnStopped, nil
	}
	if err := relaunchRound(ctx, hermes, config, run, view, logger); err != nil {
		return returnUnanswered, err
	}
	return returnRelaunched, nil
}

// relaunchRound renders this round's instruction again and builds its cards
// back.
//
// The same round number throughout. A returned round sealed no candidate,
// so the stage directory it wrote its run record into is the one the cards
// will write into again; moving to the next number would leave a round that
// exists only as a record of having produced nothing, and every later
// reader counts rounds by what they sealed.
//
// The instruction is rendered before anything is archived, which is the one
// place this differs from the ordinary next round. A render that fails here
// is the reason the round is being started again at all — and a delivery
// whose cards were archived for a render that then failed has no card left
// to drive it. Rendered first, a failure leaves the board exactly as it
// was and the next tick tries the whole answer again.
func relaunchRound(
	ctx context.Context,
	hermes *runtime.Hermes,
	config runtime.Config,
	run state.RunOverview,
	view chainView,
	logger Logger,
) error {
	pipeline := &runner.Pipeline{Config: config, Workspace: runDirectory(config, run.DeliveryID), Logger: logger}
	if err := pipeline.RenderImplementInstruction(ctx, view.round); err != nil {
		return err
	}
	for _, task := range view.all {
		if task.Status == "done" {
			continue
		}
		// Archiving is the only thing that releases a card's idempotency
		// key, so the round's own cards cannot be built again until the
		// failed ones are archived. Everything unfinished goes, not only
		// the implement card: the kanban treats an archived parent as
		// satisfied, and a card rebuilt underneath a waiting one would run
		// beside the chain's remainder on the one run directory they share.
		if err := hermes.Archive(ctx, task.ID); err != nil {
			return err
		}
	}
	terminalCard, err := runtime.EnsureChain(ctx, hermes, config.Chain, nil, run.DeliveryID, run.RunID, run.Summary, view.round)
	if err != nil {
		return err
	}
	logger.Info("the returned round was started again", "run", run.RunID, "round", view.round, "terminal_card", terminalCard)
	return nil
}
