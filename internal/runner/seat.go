package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/worker"
)

// Where a moved seat is written down.
//
// The ladder decides that a role's seat should move; the card that runs the
// role next has to find out. Nothing per-card can carry it — a stage card's
// command is fixed for its profile, which is why the round a stage belongs
// to is derived from the artifacts rather than passed in — so the move goes
// where everything else about a round goes: a record in the run directory,
// beside that round's other records, on the volume that outlives the pod.
//
// The record is also the delivery's account of itself. A night in which two
// providers went quiet and the work was finished by a third is a thing the
// requester is owed in the morning, and the report is built from these
// files (§6.5 of the plan: the move is an assumption like any other).

// SeatRecordSchemaVersion is this record's shape.
const SeatRecordSchemaVersion = 1

// maxSeatRecordBytes bounds the read. The record is a seat id, a place and
// two short names; anything larger is not one of ours.
const maxSeatRecordBytes = 8 * 1024

// SeatOccupantNote names one occupant in a record, in the words a person
// reading the report will see.
type SeatOccupantNote struct {
	Vendor string `json:"vendor"`
	Model  string `json:"model"`
}

// SeatRecord is what the ladder has done about one seat, for one stage of
// one round: which occupant the seat has moved to, and whether the
// instruction that occupant is given has been rebuilt.
//
// Both live in one record because they are one climb. The seat moves while
// there is somewhere to move it; when there is not, the same seat is asked
// again with a different instruction; and the record has to say which of
// those have been spent so the next tick does neither of them twice.
type SeatRecord struct {
	SchemaVersion int    `json:"schema_version"`
	Seat          string `json:"seat"`
	Stage         string `json:"stage"`
	Round         int    `json:"round"`
	// Candidate is the place in the seat the role now runs from: 0 is the
	// configured endpoint, 1 the first candidate.
	Candidate int              `json:"candidate"`
	MovedFrom SeatOccupantNote `json:"moved_from,omitempty"`
	MovedTo   SeatOccupantNote `json:"moved_to,omitempty"`
	// Reason is why the seat moved, in the engine's own words.
	Reason string `json:"reason,omitempty"`
	// PromptRebuilt names the rebuild played for this stage, empty until
	// one is. A stage asked the same question the same way twice is the
	// loop the ladder replaces; the name says which shape was tried.
	PromptRebuilt string    `json:"prompt_rebuilt,omitempty"`
	At            time.Time `json:"at"`
}

// SeatRecordFile is where one seat's record for one round lives. Design
// stages keep design rounds and everything else keeps implementation
// rounds, the split every other record in the run directory observes.
func SeatRecordFile(runDir, stage, seat string, round int) string {
	directory := "stage"
	if runtime.IsDesignStage(stage) {
		directory = "design"
	}
	return filepath.Join(runDir, "history", fmt.Sprintf("%s-%d", directory, round), seat+"-seat.json")
}

// ReadSeatRecord reads back what the ladder has done about this seat. A
// record that is missing, too large, unreadable, or bound to another seat,
// stage or round reads as a seat that has not moved: the role then runs
// where the configuration seats it, which is the right answer for an
// unreadable record and never leaves a delivery unable to run at all.
func ReadSeatRecord(runDir, stage, seat string, round int) (SeatRecord, bool) {
	if round < 1 || seat == "" {
		return SeatRecord{}, false
	}
	encoded, err := readWorkspaceFile(SeatRecordFile(runDir, stage, seat, round), maxSeatRecordBytes)
	if err != nil {
		return SeatRecord{}, false
	}
	var record SeatRecord
	if json.Unmarshal(encoded, &record) != nil {
		return SeatRecord{}, false
	}
	if record.SchemaVersion != SeatRecordSchemaVersion || record.Seat != seat ||
		record.Stage != stage || record.Round != round || record.Candidate < 0 {
		return SeatRecord{}, false
	}
	return record, true
}

// WriteSeatRecord seals what the ladder has decided about this seat, with
// the manners the run directory's other records are written with: the
// round's own directory mode, this user's file alone, and a link left at
// the path must not carry the write somewhere else.
func WriteSeatRecord(runDir string, record SeatRecord) error {
	record.SchemaVersion = SeatRecordSchemaVersion
	if record.Round < 1 || record.Seat == "" || record.Candidate < 0 {
		return errors.New("a seat record names a seat and a round this chain does not have")
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	path := SeatRecordFile(runDir, record.Stage, record.Seat, record.Round)
	mode := os.FileMode(0o755)
	if runtime.IsDesignStage(record.Stage) {
		mode = 0o700
	}
	if err := os.MkdirAll(filepath.Dir(path), mode); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return writeRecordAtomically(path, encoded)
}

// DropReviewAndDecision clears one round for another attempt at one
// review: the unreadable record itself, and the decision that was derived
// from it.
//
// The decision goes because it can no longer be re-derived. The decide
// verb reads every configured reviewer's record and refuses to seal unless
// all of them are there; a record that will not read now leaves the sealed
// decision standing on evidence that is gone, and every reader of it holds
// it to that evidence. Dropping it is also what puts the round back where
// the cards can work on it — the round a review card runs is the first one
// without a decision, so a round still carrying one would send the card
// forward to a round that has no change in it yet.
//
// Nothing else in the round is touched. The candidate, the other seat's
// review and the earlier rounds stand: they are still exactly what they
// were, and the round is being completed again rather than done again.
func DropReviewAndDecision(runDir, reviewer string, round int) error {
	if round < 1 || reviewer == "" {
		return errors.New("a review to drop names a seat and a round this chain does not have")
	}
	stageDir := filepath.Join(runDir, "history", fmt.Sprintf("stage-%d", round))
	for _, name := range []string{reviewer + ".json", reviewer + "-run.json", "decision.json"} {
		if err := os.Remove(filepath.Join(stageDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// SeatFor is the seat one stage's role sits in, read out of the consumer's
// model configuration.
//
// Only the roles a card actually runs are here. The validate, publish and
// decide cards run no model at all, and a stage that is not in this list
// has no seat to move — which is the same answer as a seat with nobody
// else in it, and is reached without the caller having to know the
// difference.
func SeatFor(models worker.ModelConfig, stage string) (worker.ModelEndpoint, bool) {
	judgeAt := func(seats []worker.ModelEndpoint, index int) (worker.ModelEndpoint, bool) {
		if len(seats) <= index {
			return worker.ModelEndpoint{}, false
		}
		return seats[index], true
	}
	switch stage {
	case runtime.StageReviewA:
		return judgeAt(models.Reviewers, 0)
	case runtime.StageReviewB:
		return judgeAt(models.Reviewers, 1)
	case runtime.StageDesignReviewA:
		return judgeAt(models.DesignJudges(), 0)
	case runtime.StageDesignReviewB:
		return judgeAt(models.DesignJudges(), 1)
	case runtime.StageImplement, runtime.StageApply:
		return models.Implementer, models.Implementer.ID != ""
	default:
		// The investigating designer's card is deliberately absent. Its
		// seat has no second occupant it could be launched as — the roles
		// that can be are the ones with their own launch definitions — and
		// its instruction is built from the ticket, the repository and the
		// previous round by a verb this change does not touch, so a
		// rebuild here would change nothing about the next attempt. A hand
		// that changes nothing must not be recorded as played: it costs a
		// whole investigation and leaves the delivery no better off. What
		// a shorter investigation should be is the investigating
		// designer's own contract to decide.
		return worker.ModelEndpoint{}, false
	}
}

// reviewStage and designReviewStage name the card one of the two review
// positions runs as. The chain runs exactly two of each, and the position
// is what the stage functions carry; the seat's record is bound to the
// stage name, so the two have to agree on which card this is.
func reviewStage(index int) string {
	if index == 1 {
		return runtime.StageReviewB
	}
	return runtime.StageReviewA
}

func designReviewStage(index int) string {
	if index == 1 {
		return runtime.StageDesignReviewB
	}
	return runtime.StageDesignReviewA
}

// seatArguments are what a stage card passes on to the verb about its own
// seat: which occupant to run as, and whether the instruction is to be
// rebuilt. Empty for a stage the ladder has not touched, which is every
// stage of a delivery that is going well.
//
// The place travels as a number rather than as an endpoint: the verb loads
// the configuration anyway, and a card that named a vendor and a model
// would be a second place for the seat to be defined.
func (p *Pipeline) seatArguments(stage, seat string, round int) []string {
	record, found := ReadSeatRecord(p.Workspace, stage, seat, round)
	if !found {
		return nil
	}
	var args []string
	if record.Candidate > 0 {
		args = append(args, "--seat-candidate", strconv.Itoa(record.Candidate))
	}
	if record.PromptRebuilt != "" {
		args = append(args, "--rebuild-prompt", record.PromptRebuilt)
	}
	return args
}

// seatRebuilt reports whether the ladder has asked for this stage's
// instruction to be rebuilt in the round it is about to run again.
func (p *Pipeline) seatRebuilt(stage, seat string, round int) bool {
	record, found := ReadSeatRecord(p.Workspace, stage, seat, round)
	return found && record.PromptRebuilt != ""
}

// RerenderInstruction writes the implementing card's instruction again for
// the round it is already in.
//
// The reviews build their own instruction on every dispatch, so a rebuild
// reaches them as an argument on the card. The implementer and the applier
// read a file, written once when the round began, and a card dispatched
// again reads exactly what it read the first time — so a rebuild that only
// wrote down its intention would be a hand spent on nothing. This is the
// hand actually being played: the same round's instruction, shorter,
// before the card is dispatched again.
func (p *Pipeline) RerenderInstruction(ctx context.Context, stage string) error {
	switch stage {
	case runtime.StageImplement:
		return p.RenderImplementInstruction(ctx, p.currentRound())
	case runtime.StageApply:
		design, round := p.ApprovedDesign()
		if design == "" || round < 1 {
			return errors.New("the applier's instruction has no approved design to render from")
		}
		return p.RenderApplyInstruction(ctx, round)
	default:
		// Every other card builds its own instruction when it runs, and
		// reads the rebuild off its own command line.
		return nil
	}
}

// SeatOccupantFor is the occupant a stage runs from now: the place the
// record names, or the configured endpoint when no record says otherwise.
// An unreadable place — a record naming a candidate the configuration no
// longer has — falls back to the configured endpoint rather than refusing
// the stage, because a delivery that cannot run its own role at all is
// worse than one running it where it started.
func SeatOccupantFor(runDir, stage string, seat worker.ModelEndpoint, round int) (worker.ModelEndpoint, int) {
	record, found := ReadSeatRecord(runDir, stage, seat.ID, round)
	if !found || record.Candidate == 0 {
		occupant, _ := seat.SeatOccupant(0)
		return occupant, 0
	}
	occupant, seated := seat.SeatOccupant(record.Candidate)
	if !seated {
		occupant, _ = seat.SeatOccupant(0)
		return occupant, 0
	}
	return occupant, record.Candidate
}
