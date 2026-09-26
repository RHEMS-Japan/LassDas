package attendant

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/worker"
)

// The first rung: move the role to somebody else.
//
// A model that will not answer is not an argument about the request, and
// until now it ended the delivery anyway. The remedy is another model —
// and, because the answer comes through a provider's key rather than out
// of the air, another provider and another launch with it. The consumer
// writes those down as the seat's candidates; this chooses the next one
// and writes down that it did.
//
// Two rules shape the choice. A candidate whose launch is missing is not a
// move at all: the same program would go on talking to the same provider
// through the same key while the record claimed otherwise. And the two
// review seats must not end up on one provider — the whole reason there
// are two of them is that one vendor's blind spot should not be every
// judge's — so a candidate sitting on the other seat's current vendor is
// passed over rather than taken. When that leaves nothing, the seat stays
// where it is and the ladder asks the same occupant a different way
// (promptRebuildHand), and when that is spent too the delivery waits.
// Reducing the judges to one to get past it is never on the list.

// seatClimb is the seat of the stage that failed, and everything needed to
// decide where it should sit next.
type seatClimb struct {
	stage  string
	round  int
	seat   worker.ModelEndpoint
	agents worker.AgentSet
	// heldVendor is the vendor the other review seat is sitting on right
	// now, empty when this stage has no counterpart seat.
	heldVendor string
	// record is what the ladder has already done about this seat.
	record runner.SeatRecord
	runDir string
}

// modelHands are the hands for a model that would not answer: every
// candidate this seat can actually be moved to, in the consumer's order,
// and then the one rebuild of the instruction.
//
// The list is rebuilt on every tick rather than fixed once, because what
// the other seat is sitting on can change between ticks: a candidate
// passed over for a vendor clash becomes available again the moment the
// other seat moves off it, and the ladder's record names hands, so it
// comes back into play without anything having to remember it.
func modelHands(climb ladderClimb) []ladderHand {
	seat, found := seatOfFailedStage(climb)
	if !found {
		return nil
	}
	hands := candidateHands(seat)
	if seat.record.PromptRebuilt == "" {
		hands = append(hands, ladderHand{
			step: rungSeat, name: "prompt:" + worker.PromptRebuildShorten,
			reclaim: seat.rebuildPrompt(),
		})
	}
	return hands
}

// candidateHands is shared by unanswered-model and credit failures. The
// configured launch, not an endpoint's label, is what can change accounts.
// Different launch definitions can still use one account: without reading
// credentials or changing billing, the only evidence that another launch
// can answer is its actual attempt. The ladder spends each hand once, even
// when the failure class changes, and retains reviewer independence.
func candidateHands(seat seatClimb) []ladderHand {
	hands := make([]ladderHand, 0, seat.seat.SeatDepth())
	for place := 1; place < seat.seat.SeatDepth(); place++ {
		occupant, seated := seat.seat.SeatOccupant(place)
		if !seated || !seat.launchable(place) || seat.clashes(occupant) {
			continue
		}
		hands = append(hands, ladderHand{
			step: rungSeat, name: fmt.Sprintf("seat:%d", place),
			reclaim: seat.moveTo(place, occupant),
		})
	}
	return hands
}

// launchable reports whether this place has a launch of its own.
//
// Only the review seats can have one. An implementing seat is launched by
// the one implementer definition, so moving its endpoint would change what
// the record says without changing who answers — and a record that says a
// model answered when another one did is worse than no move at all. Those
// seats reach the rebuild below instead, which is a real change to a real
// ask.
func (s seatClimb) launchable(place int) bool {
	switch s.stage {
	case runtime.StageReviewA, runtime.StageReviewB:
		_, launched := s.agents.ReviewerAgentSeat(s.seat.ID, place)
		return launched
	case runtime.StageDesignReviewA, runtime.StageDesignReviewB:
		_, launched := s.agents.DesignReviewerAgentSeat(s.seat.ID, place)
		return launched
	default:
		return false
	}
}

// clashes reports whether taking this occupant would put both review seats
// on one vendor.
func (s seatClimb) clashes(occupant worker.ModelEndpoint) bool {
	return s.heldVendor != "" && worker.SameVendor(s.heldVendor, occupant.Vendor)
}

// moveTo writes down that the seat has moved. The card the ladder
// dispatches next reads the record and runs the role as that occupant; a
// write that fails leaves the seat where it was, which the ladder's own
// record has already counted as a hand spent — one attempt lost, never a
// delivery stopped.
func (s seatClimb) moveTo(place int, occupant worker.ModelEndpoint) func(context.Context, ladderClimb) error {
	return func(_ context.Context, climb ladderClimb) error {
		from, _ := s.seat.SeatOccupant(s.record.Candidate)
		record := s.record
		record.Seat, record.Stage, record.Round = s.seat.ID, s.stage, s.round
		record.Candidate = place
		record.MovedFrom = runner.SeatOccupantNote{Vendor: from.Vendor, Model: from.Model}
		record.MovedTo = runner.SeatOccupantNote{Vendor: occupant.Vendor, Model: occupant.Model}
		record.Reason = seatMoveReason(climb, s.stage, s.round)
		record.At = time.Now().UTC()
		if err := runner.WriteSeatRecord(s.runDir, record); err != nil {
			return err
		}
		climb.logger.Info("the seat moved to another model and another provider",
			"run", climb.run.RunID, "stage", s.stage, "round", s.round, "seat", s.seat.ID,
			"candidate", place, "vendor", occupant.Vendor, "model", occupant.Model)
		return nil
	}
}

// rebuildPrompt writes down that the next attempt is asked differently:
// the same occupant, a shorter instruction (§5.3 of the plan). It is the
// last hand of this rung, played when the seat has nowhere left to move —
// including a seat that never had anywhere to move, which is every
// configuration written before candidates existed.
func (s seatClimb) rebuildPrompt() func(context.Context, ladderClimb) error {
	return func(ctx context.Context, climb ladderClimb) error {
		// The reviews read the rebuild off the card they are dispatched
		// with; the implementing cards read a file that was written when
		// the round began, so for those the instruction is written again
		// here, before the dispatch. Without it the card would read
		// exactly what it read the first time and the hand would have
		// changed nothing.
		//
		// The record is written after it, and only if it succeeded. A
		// record saying the instruction was rebuilt while the file still
		// holds the one that was not answered is the same silent failure
		// in a second place: the card would be told it is being asked
		// differently when it is not.
		pipeline := &runner.Pipeline{Config: climb.config, Workspace: s.runDir, Logger: climb.logger}
		if err := pipeline.RerenderInstruction(ctx, s.stage); err != nil {
			return err
		}
		record := s.record
		record.Seat, record.Stage, record.Round = s.seat.ID, s.stage, s.round
		record.PromptRebuilt = worker.PromptRebuildShorten
		record.At = time.Now().UTC()
		if err := runner.WriteSeatRecord(s.runDir, record); err != nil {
			return err
		}
		climb.logger.Info("the seat stays and the instruction is rebuilt shorter",
			"run", climb.run.RunID, "stage", s.stage, "round", s.round, "seat", s.seat.ID,
			"rebuild", worker.PromptRebuildShorten)
		return nil
	}
}

// seatMoveReason is why the seat moved, in the engine's own words: what
// the card said about its own failure, never what a model said. The
// sentence reaches the requester's report, so it is composed here from the
// record's own fields rather than quoted from anything the provider sent.
func seatMoveReason(climb ladderClimb, stage string, round int) string {
	failure, sealed := runner.ReadStageFailure(climb.runDir, stage, round)
	if !sealed {
		return "the card did not say why it stopped"
	}
	return "the previous attempt failed as " + string(failure.Class)
}

// seatOfFailedStage gathers the seat the failed stage runs, what has
// already been done about it, and what the other review seat is sitting on.
func seatOfFailedStage(climb ladderClimb) (seatClimb, bool) {
	models, agents, err := loadSeatConfig(climb.config.ConsumerConfigPath)
	if err != nil {
		climb.logger.Error("the consumer's seats could not be read; the seat cannot be moved",
			"run", climb.run.RunID, "stage", climb.stage, "error", err.Error())
		return seatClimb{}, false
	}
	seat, found := runner.SeatFor(models, climb.stage)
	if !found || seat.ID == "" {
		return seatClimb{}, false
	}
	round := stageRound(climb.view, climb.stage)
	if round < 1 {
		return seatClimb{}, false
	}
	record, _ := runner.ReadSeatRecord(climb.runDir, climb.stage, seat.ID, round)
	return seatClimb{
		stage: climb.stage, round: round, seat: seat, agents: agents,
		heldVendor: otherReviewVendor(climb.runDir, models, climb.stage, round),
		record:     record, runDir: climb.runDir,
	}, true
}

// otherReviewVendor is the vendor the counterpart review seat is sitting on
// at this moment: its configured endpoint, or the candidate its own record
// says it moved to. Empty for a stage with no counterpart — an implementing
// card has nobody to differ from, and neither has a delivery configured
// with one judge.
func otherReviewVendor(runDir string, models worker.ModelConfig, stage string, round int) string {
	var seats []worker.ModelEndpoint
	other := ""
	switch stage {
	case runtime.StageReviewA:
		seats, other = models.Reviewers, runtime.StageReviewB
	case runtime.StageReviewB:
		seats, other = models.Reviewers, runtime.StageReviewA
	case runtime.StageDesignReviewA:
		seats, other = models.DesignJudges(), runtime.StageDesignReviewB
	case runtime.StageDesignReviewB:
		seats, other = models.DesignJudges(), runtime.StageDesignReviewA
	default:
		return ""
	}
	seat, found := runner.SeatFor(models, other)
	if !found || len(seats) < 2 {
		return ""
	}
	occupant, _ := runner.SeatOccupantFor(runDir, other, seat, round)
	return occupant.Vendor
}

// loadSeatConfig reads the seats and the launches out of the consumer
// configuration. A lenient decode, like the budget probe's: the file's
// other sections are the worker's business and are validated there, and
// what is needed here is who a role may be and how each of them starts.
func loadSeatConfig(path string) (worker.ModelConfig, worker.AgentSet, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return worker.ModelConfig{}, worker.AgentSet{}, err
	}
	if int64(len(raw)) > worker.MaxConfigJSONBytes {
		return worker.ModelConfig{}, worker.AgentSet{}, fmt.Errorf("consumer config exceeds %d bytes", worker.MaxConfigJSONBytes)
	}
	var config struct {
		Models worker.ModelConfig `json:"models"`
		Agents worker.AgentSet    `json:"agents"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		return worker.ModelConfig{}, worker.AgentSet{}, err
	}
	return config.Models, config.Agents, nil
}
