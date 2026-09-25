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
// key" but "a stand-in was built and this is what has to be supplied".
//
// The answering is bounded, and that bound is the whole of what stands
// between this and a delivery that launches an agent every hour all night
// for nothing. The engine's answer is the same three rules every time, so
// an implementer that has heard them and come back anyway is not being
// persuaded; past maxAnsweredReturns, and at once for a report that simply
// repeats, the return stops being read as an answer to decide about and
// starts being read as what it has become — the seat's model declining to
// do the work. That is a model failure, it is sealed as one, and the
// ladder takes it from there: a shorter instruction, a different occupant,
// a wait that grows, the one notice on the ticket, and an operator's own
// limit on the attempts if they set one. Nothing about it asks the
// requester anything.

// returnVerdict is what became of a round that was handed back.
type returnVerdict int

const (
	// returnRelaunched: the round is running again under the engine's own
	// answer, and this tick is finished.
	returnRelaunched returnVerdict = iota
	// returnStopped: the requester asked the delivery to stop, which is
	// read at the boundary before anything is started again.
	returnStopped
	// returnToLadder: the engine will not answer this return. The round's
	// failure is sealed as the model failure it is and the caller hands it
	// to the ladder.
	returnToLadder
)

// answerReturnedWork decides what a returned round is told and starts it
// again, or declines to answer it and leaves it for the ladder.
func answerReturnedWork(
	ctx context.Context,
	config runtime.Config,
	services *runtime.Services,
	hermes *runtime.Hermes,
	envelope hook.DispatchEnvelope,
	run state.RunOverview,
	view chainView,
	stageName string,
	logger Logger,
) (returnVerdict, error) {
	runDir := runDirectory(config, run.DeliveryID)
	agentRun, err := worker.ReadImplementingRun(filepath.Join(runDir, "history"), view.round)
	if err != nil {
		// The record was readable a moment ago — it is what classified this
		// card as an answer — so this is a volume that stopped answering
		// rather than a round that said nothing. Returned as an error, so
		// the next tick reads it again: reported instead, the ticket would
		// carry an internal failure for a delivery the ladder has never
		// been given a chance at.
		return returnToLadder, fmt.Errorf("the returned report of round %d could not be read back: %w", view.round, err)
	}
	previous, err := runner.ReadReturns(runDir, view.round)
	if err != nil {
		// Read as absent this would answer an already-answered round as a
		// first return, for ever. The count is what decides whether the
		// engine is still answering or the ladder has the round, so an
		// unreadable record hands it to the ladder rather than guessing.
		logger.Error("this round's earlier answers could not be read; the return is left to the ladder",
			"run", run.RunID, "round", view.round, "error", err.Error())
		return refuseReturn(config, runDir, run, view, stageName, "the record of this round's earlier returns could not be read", logger)
	}
	// The same return the ladder is already working on, seen again by a
	// later tick. The card it failed on stays blocked until the ladder
	// rebuilds it, and the tick reads a blocked card every few seconds; the
	// round's history is written for what an agent did, not for what a tick
	// saw. Nothing to record, nothing to seal again, and the ladder below
	// still gets its turn.
	if previous.AlreadyLeftToTheLadder(agentRun) {
		return returnToLadder, nil
	}
	answer := worker.AnswerReturn(agentRun, previous, time.Now().UTC())
	// Recorded whether or not it is answered. The count has to survive the
	// ladder's own relaunches: a round the ladder dispatches again and that
	// comes back returned is attempt N+1, goes straight past the answering
	// and climbs one rung higher, which is only true if every return was
	// written down.
	if err := runner.RecordReturn(runDir, view.round, answer); err != nil {
		// The relaunch reads this file. Started again without it, the agent
		// is handed the instruction it has already answered, which is the
		// silent loop this whole path is built to avoid.
		return returnToLadder, fmt.Errorf("the answer to round %d could not be recorded: %w", view.round, err)
	}
	if !answer.Answered {
		reason := fmt.Sprintf("the implementing agent returned the work for the %d%s time without doing it", answer.Attempt, ordinalSuffix(answer.Attempt))
		if answer.Repeated {
			reason = "the implementing agent returned the work again with the report the engine had already answered"
		}
		logger.Info("the returned round is not answered again; it is handled as a model that will not do the work",
			"run", run.RunID, "round", view.round, "attempt", answer.Attempt, "repeated", answer.Repeated)
		return refuseReturn(config, runDir, run, view, stageName, reason, logger)
	}
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
		return returnToLadder, stopErr
	}
	logger.Info("the implementer handed the work back; the engine answered it and the round runs again",
		"run", run.RunID, "round", view.round, "attempt", answer.Attempt,
		"assumption", answer.Assumption.Kind, "supplies", len(answer.Supply))
	if stopped {
		return returnStopped, nil
	}
	return relaunchRound(ctx, config, services, hermes, envelope, run, view, stageName, logger)
}

// refuseReturn seals the round's failure as the model failure it is, so the
// ladder that reads sealed failures finds one that says what to do about it.
//
// Written here rather than left to whatever the card sealed. The card's own
// account of a return is a verb that exited non-zero, which classifies as a
// model failure by the verb it was — true, but true by accident, and a
// record that read as unknown would leave the ladder with nothing but the
// wait. Which seat answered, and where it may move to, the ladder reads
// from the configuration and the round's seat record; what it needs from
// here is the class.
func refuseReturn(
	config runtime.Config,
	runDir string,
	run state.RunOverview,
	view chainView,
	stageName string,
	reason string,
	logger Logger,
) (returnVerdict, error) {
	record := runner.StageFailure{
		ToolSHA: config.Identity.EngineSHA,
		Stage:   stageName,
		Round:   view.round,
		Class:   runner.FailureClassModel,
		Error:   reason,
		// Deliberately not interrupted: nothing stopped this card from
		// outside. A reader counting model failures to decide the model
		// will not answer has to count this one.
		FailedAt: time.Now().UTC(),
	}
	record.DeliveryID, _ = readField(runDir, "ticket-draft.json", "delivery_id")
	record.InputSHA256, _ = readField(runDir, "ticket-draft.json", "input_sha256")
	record.ConfigSHA256, _ = readField(runDir, "ticket-draft.json", "config_sha256")
	if err := runner.SealStageFailureRecord(runDir, record); err != nil {
		// The ladder still climbs — a card with no sealed account is a
		// failure nobody could name, which waits and tries again — so this
		// costs the shorter instruction and the seat move, not the
		// delivery.
		logger.Error("the returned round's failure could not be sealed; the ladder climbs it as an unnamed failure",
			"run", run.RunID, "round", view.round, "error", err.Error())
	}
	return returnToLadder, nil
}

// ordinalSuffix is the English ending for a count in a record's sentence.
func ordinalSuffix(count int) string {
	if count%100 >= 11 && count%100 <= 13 {
		return "th"
	}
	switch count % 10 {
	case 1:
		return "st"
	case 2:
		return "nd"
	case 3:
		return "rd"
	default:
		return "th"
	}
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
	config runtime.Config,
	services *runtime.Services,
	hermes *runtime.Hermes,
	envelope hook.DispatchEnvelope,
	run state.RunOverview,
	view chainView,
	stageName string,
	logger Logger,
) (returnVerdict, error) {
	runDir := runDirectory(config, run.DeliveryID)
	// Which cards this delivery has. A designed request runs its applier
	// where an ordinary one runs its implementer, and the two read
	// different instructions and sit in different chains; rebuilding either
	// one as the other would drop the design cards and start a delivery
	// that has a design from a request that does not.
	plan, err := chainPlanFor(config, runDir, run, logger)
	if err != nil {
		return returnToLadder, fmt.Errorf("the chain shape of round %d could not be read: %w", view.round, err)
	}
	pipeline := &runner.Pipeline{Config: config, Workspace: runDir, Logger: logger}
	if plan.Shape == runtime.ShapeDesign {
		_, designRound := pipeline.ApprovedDesign()
		if designRound < 1 {
			return returnToLadder, fmt.Errorf("the designed round %d has no approved design to apply again", view.round)
		}
		err = pipeline.RenderApplyInstruction(ctx, designRound)
	} else {
		err = pipeline.RenderImplementInstruction(ctx, view.round)
	}
	if err != nil {
		return returnToLadder, err
	}
	// The ladder's own dispatcher does the rest: archive the failed stage
	// and everything after it, keep what finished, and build the missing
	// cards back in this delivery's own shape and rounds. It reads 「停止」
	// once more on the way, which is the last thing between a requester who
	// has asked the delivery to stop and a card that starts spending again.
	verdict, err := dispatchAgain(ctx, newClimb(config, services, hermes, envelope, run, view, plan, stageName, logger))
	switch {
	case err != nil:
		return returnToLadder, err
	case verdict == ladderStopped:
		return returnStopped, nil
	case verdict != ladderHandled:
		return returnToLadder, fmt.Errorf("the returned round %d could not be started again", view.round)
	}
	logger.Info("the returned round was started again", "run", run.RunID, "round", view.round, "stage", stageName)
	return returnRelaunched, nil
}
