package runner

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/worker"
)

// What the card passes on to the verb about its own seat. A delivery that
// has not failed passes nothing, which is what keeps every command line in
// a healthy delivery exactly as it was.
func TestTheCardTellsTheVerbWhereTheSeatIsSitting(t *testing.T) {
	workspace := t.TempDir()
	pipeline := &Pipeline{Workspace: workspace}
	if args := pipeline.seatArguments(runtime.StageReviewA, "review-a", 1); len(args) != 0 {
		t.Fatalf("a delivery that has not failed passed %v to its card", args)
	}
	if err := WriteSeatRecord(workspace, SeatRecord{
		Seat: "review-a", Stage: runtime.StageReviewA, Round: 1, Candidate: 2, At: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if args := pipeline.seatArguments(runtime.StageReviewA, "review-a", 1); !slices.Equal(args, []string{"--seat-candidate", "2"}) {
		t.Fatalf("args = %v, want the place the seat moved to", args)
	}
	if err := WriteSeatRecord(workspace, SeatRecord{
		Seat: "review-a", Stage: runtime.StageReviewA, Round: 1, Candidate: 2,
		PromptRebuilt: worker.PromptRebuildShorten, At: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if args := pipeline.seatArguments(runtime.StageReviewA, "review-a", 1); !slices.Equal(args,
		[]string{"--seat-candidate", "2", "--rebuild-prompt", "shorten"}) {
		t.Fatalf("args = %v, want both of the ladder's decisions", args)
	}
	// Another seat, another round and another stage are not this card's.
	for _, elsewhere := range [][3]any{
		{runtime.StageReviewA, "review-b", 1}, {runtime.StageReviewB, "review-a", 1}, {runtime.StageReviewA, "review-a", 2},
	} {
		stage, seat, round := elsewhere[0].(string), elsewhere[1].(string), elsewhere[2].(int)
		if args := pipeline.seatArguments(stage, seat, round); len(args) != 0 {
			t.Fatalf("%s/%s round %d read another card's seat: %v", stage, seat, round, args)
		}
	}
}

// A design round's records live in the design directory, the way every
// other record of a design round does.
func TestADesignSeatIsWrittenToTheDesignRound(t *testing.T) {
	workspace := t.TempDir()
	if err := WriteSeatRecord(workspace, SeatRecord{
		Seat: "review-a", Stage: runtime.StageDesignReviewA, Round: 2, Candidate: 1, At: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	path := SeatRecordFile(workspace, runtime.StageDesignReviewA, "review-a", 2)
	if filepath.Dir(path) != filepath.Join(workspace, "history", "design-2") {
		t.Fatalf("a design seat was written to %s", filepath.Dir(path))
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the record is not there: %v", err)
	}
}

// Which seat a stage runs, read out of the consumer's own model
// configuration. The stages that run no model at all have no seat, which
// is the same answer as a seat with nobody else in it.
func TestEveryModelStageKnowsItsSeat(t *testing.T) {
	designer := worker.ModelEndpoint{ID: "designer"}
	arbiter := worker.ModelEndpoint{ID: "arbiter"}
	models := worker.ModelConfig{
		Implementer: worker.ModelEndpoint{ID: "author"},
		Reviewers:   []worker.ModelEndpoint{{ID: "review-a"}, {ID: "review-b"}},
		Designer:    &designer,
		Arbiter:     &arbiter,
	}
	// The roles whose card can actually be asked differently on a second
	// attempt: the judges, who are launched as one of their seat's
	// occupants, and the implementing cards, whose instruction is written
	// again before they are dispatched.
	for stage, want := range map[string]string{
		runtime.StageReviewA:       "review-a",
		runtime.StageReviewB:       "review-b",
		runtime.StageDesignReviewA: "review-a",
		runtime.StageDesignReviewB: "review-b",
		runtime.StageImplement:     "author",
		runtime.StageApply:         "author",
		runtime.StageValidate:      "arbiter",
	} {
		seat, found := SeatFor(models, stage)
		if !found || seat.ID != want {
			t.Fatalf("%s sits in %q (%v), want %q", stage, seat.ID, found, want)
		}
	}
	for _, none := range []string{runtime.StagePublish, runtime.StageDesignDecide} {
		if _, found := SeatFor(models, none); found {
			t.Fatalf("%s was given a seat; it runs no model", none)
		}
	}
	// The investigating designer spends a model turn and still has no
	// seat: it has no second occupant it could be launched as, and its
	// instruction is built by a verb this change does not touch. A seat
	// here would spend a whole investigation on a hand that changes
	// nothing before the delivery waited.
	if _, found := SeatFor(models, runtime.StageInvestigate); found {
		t.Fatal("the investigating designer was given a seat with no hand to play")
	}
}

// A record naming a candidate the configuration no longer has leaves the
// role where it started rather than refusing to run it: a delivery that
// cannot run its own role at all is worse than one running it where the
// configuration put it.
func TestASeatRecordPastTheEndOfTheSeatFallsBack(t *testing.T) {
	workspace := t.TempDir()
	seat := worker.ModelEndpoint{ID: "review-a", Vendor: "Vendor A", Model: "model-a"}
	if err := WriteSeatRecord(workspace, SeatRecord{
		Seat: "review-a", Stage: runtime.StageReviewA, Round: 1, Candidate: 3, At: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	occupant, place := SeatOccupantFor(workspace, runtime.StageReviewA, seat, 1)
	if place != 0 || occupant.Model != "model-a" {
		t.Fatalf("occupant = %s at place %d, want the configured endpoint", occupant.Model, place)
	}
}
