package attendant

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
)

// seatedConsumerConfig is a consumer whose first review seat has two
// candidates — one on the vendor the other seat is already sitting on, one
// on a vendor nobody holds — each with a launch of its own. Written as the
// file the attendant actually reads, because the seats the ladder can
// reach are exactly the seats a consumer wrote down.
const seatedConsumerConfig = `{
 "max_stages": 3,
 "agents": {
  "applier": {"command": "launch"},
  "reviewer_agents": [
   {"reviewer_id": "review-a", "agent": {"id": "judge-a"},
    "candidates": [{"id": "judge-a-held"}, {"id": "judge-a-free"}]},
   {"reviewer_id": "review-b", "agent": {"id": "judge-b"}}
  ]
 },
 "models": {
  "implementer": {"id": "author", "vendor": "Vendor A", "model": "model-a"},
  "reviewers": [
   {"id": "review-a", "vendor": "Vendor A", "model": "model-a",
    "candidates": [
     {"vendor": "Vendor B", "model": "model-b-held"},
     {"vendor": "Vendor C", "model": "model-c"}
    ]},
   {"id": "review-b", "vendor": "Vendor B", "model": "model-b"}
  ]
 }
}`

// seatTheConsumer replaces the fixture's consumer configuration with one
// whose review seats have somewhere to go.
func seatTheConsumer(t *testing.T, setup *ladderSetup) {
	t.Helper()
	if err := os.WriteFile(setup.config.ConsumerConfigPath, []byte(seatedConsumerConfig), 0o600); err != nil {
		t.Fatal(err)
	}
}

func seatRecord(t *testing.T, setup *ladderSetup, stage, seat string) (runner.SeatRecord, bool) {
	t.Helper()
	return runner.ReadSeatRecord(setup.runDir, stage, seat, 1)
}

// The night this whole ladder was built for, reproduced. A review card went
// twenty-six minutes without an answer and the delivery ended there with
// nothing done — the card's own retries never ran, because a failure that
// slow is not the upstream lottery they exist for, and the tick had no
// other answer than to report it.
//
// Now the seat moves to another model on another provider and the stage is
// dispatched again. Nothing is reported, nothing is asked of anybody, and
// the delivery is still going.
func TestASilentReviewMovesTheSeatAndTheRunGoesOn(t *testing.T) {
	setup := newLadderSetup(t, runner.StageFailure{
		Stage: runtime.StageReviewA, Round: 1, Class: runner.FailureClassModel,
		Error: "review by review-a did not finish: the agent-review step exited 1",
	})
	seatTheConsumer(t, setup)

	verdict, err := setup.climb(t)
	if err != nil || verdict != ladderHandled {
		t.Fatalf("verdict = %v err = %v, want the silent review climbed", verdict, err)
	}
	record, moved := seatRecord(t, setup, runtime.StageReviewA, "review-a")
	if !moved {
		t.Fatal("the seat did not move; the delivery would meet the same silence")
	}
	// Not onto the vendor the other seat is holding: two judges on one
	// provider are one provider's blind spot counted twice.
	if record.Candidate != 2 || record.MovedTo.Vendor != "Vendor C" || record.MovedTo.Model != "model-c" {
		t.Fatalf("the seat moved to %+v, want the candidate on the vendor nobody holds", record)
	}
	if record.MovedFrom.Vendor != "Vendor A" || record.Reason == "" {
		t.Fatalf("the record does not say where the seat came from or why: %+v", record)
	}
	// The stage and the ones after it were rebuilt, and the requester was
	// told nothing, because nothing has gone wrong that a person could act on.
	lines := ladderBoardLines(t, setup.calls)
	if created := createdStages(lines); !slices.Contains(created, runtime.StageReviewA) {
		t.Fatalf("created = %v, want the review dispatched again", created)
	}
	if len(setup.fixture.comments.posted) != 0 || setup.fixture.store.begins != 0 {
		t.Fatalf("the delivery was reported instead of climbed: %q", setup.fixture.comments.posted)
	}
	if ladder := setup.record(); ladder.LadderStep != rungSeat || !slices.Contains(ladder.Tried, "seat:2") {
		t.Fatalf("the ladder record does not name the seat hand: %+v", ladder)
	}
}

// Candidates first, then the instruction, then the wait. Every hand is a
// different hand, the vendor the other seat holds is passed over rather
// than taken, and when there is nothing left to change the delivery waits
// instead of ending — with the judges still two.
func TestTheSeatRungSpendsItsCandidatesThenRebuildsThenWaits(t *testing.T) {
	setup := newLadderSetup(t, runner.StageFailure{
		Stage: runtime.StageReviewA, Round: 1, Class: runner.FailureClassModel, Error: "the provider gave up",
	})
	seatTheConsumer(t, setup)
	var steps []int
	for pass := 0; pass < 3; pass++ {
		verdict, err := setup.climb(t)
		if err != nil || verdict != ladderHandled {
			t.Fatalf("pass %d: verdict = %v err = %v", pass, verdict, err)
		}
		steps = append(steps, setup.record().LadderStep)
	}
	if tried := setup.record().Tried; !slices.Equal(tried, []string{"seat:2", "prompt:shorten"}) {
		t.Fatalf("tried = %v, want the free candidate and then the rebuilt instruction, each once", tried)
	}
	if !slices.Equal(steps, []int{rungSeat, rungSeat, rungWait}) {
		t.Fatalf("rungs = %v, want the seat rung twice and then the wait", steps)
	}
	record, _ := seatRecord(t, setup, runtime.StageReviewA, "review-a")
	if record.PromptRebuilt != "shorten" || record.Candidate != 2 {
		t.Fatalf("the seat record lost one of the two hands: %+v", record)
	}
	// The held candidate was never taken, and the delivery is still going
	// with two judges rather than one.
	if record.MovedTo.Vendor == "Vendor B" {
		t.Fatal("the seat took the vendor the other seat is holding")
	}
}

// A seat with nobody else in it — every configuration written before
// candidates existed — still has one hand: the same occupant, asked
// shorter. Only then does the delivery wait.
func TestASeatWithNoCandidatesStillRebuildsBeforeWaiting(t *testing.T) {
	setup := newLadderSetup(t, runner.StageFailure{
		Stage: runtime.StageReviewA, Round: 1, Class: runner.FailureClassModel, Error: "the provider gave up",
	})
	if _, err := setup.climb(t); err != nil {
		t.Fatal(err)
	}
	if tried := setup.record().Tried; !slices.Equal(tried, []string{"prompt:shorten"}) {
		t.Fatalf("tried = %v, want the rebuilt instruction alone", tried)
	}
	record, rebuilt := seatRecord(t, setup, runtime.StageReviewA, "review-a")
	if !rebuilt || record.PromptRebuilt != "shorten" || record.Candidate != 0 {
		t.Fatalf("the rebuild was not written down: %+v", record)
	}
	if _, err := setup.climb(t); err != nil {
		t.Fatal(err)
	}
	if setup.record().LadderStep != rungWait {
		t.Fatalf("a spent seat rung did not descend to the wait: %+v", setup.record())
	}
}

// An endpoint candidate with no launch of its own is not a move: the same
// program would go on talking to the same provider through the same key
// while the record claimed another vendor answered. So it is passed over,
// and the seat reaches the rebuild instead.
func TestACandidateWithoutALaunchIsNotAMove(t *testing.T) {
	setup := newLadderSetup(t, runner.StageFailure{
		Stage: runtime.StageReviewA, Round: 1, Class: runner.FailureClassModel, Error: "the provider gave up",
	})
	unlaunched := `{"max_stages":3,"agents":{"applier":{"command":"launch"}},"models":{"reviewers":[
	 {"id":"review-a","vendor":"Vendor A","model":"model-a","candidates":[{"vendor":"Vendor C","model":"model-c"}]},
	 {"id":"review-b","vendor":"Vendor B","model":"model-b"}]}}`
	if err := os.WriteFile(setup.config.ConsumerConfigPath, []byte(unlaunched), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.climb(t); err != nil {
		t.Fatal(err)
	}
	if tried := setup.record().Tried; !slices.Equal(tried, []string{"prompt:shorten"}) {
		t.Fatalf("tried = %v, want the candidate passed over and the instruction rebuilt", tried)
	}
	if record, moved := seatRecord(t, setup, runtime.StageReviewA, "review-a"); moved && record.Candidate != 0 {
		t.Fatalf("a candidate with no launch was taken: %+v", record)
	}
}

// The seat records live under the run directory, which is the volume, so a
// pod replaced mid-climb comes back to a seat that is still where the
// climb put it.
func TestAMovedSeatSurvivesAPodBeingReplaced(t *testing.T) {
	setup := newLadderSetup(t, runner.StageFailure{
		Stage: runtime.StageReviewA, Round: 1, Class: runner.FailureClassModel, Error: "the provider gave up",
	})
	seatTheConsumer(t, setup)
	if _, err := setup.climb(t); err != nil {
		t.Fatal(err)
	}
	path := runner.SeatRecordFile(setup.runDir, runtime.StageReviewA, "review-a", 1)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the move was not written to the volume: %v", err)
	}
	if directory := filepath.Dir(path); directory != filepath.Join(setup.runDir, "history", "stage-1") {
		t.Fatalf("the move was written to %s, want the round's own directory", directory)
	}
	// Read back exactly as another process reads it, and refused for
	// another seat or another round rather than answered wrongly.
	if _, found := runner.ReadSeatRecord(setup.runDir, runtime.StageReviewA, "review-a", 1); !found {
		t.Fatal("the record did not read back")
	}
	if _, found := runner.ReadSeatRecord(setup.runDir, runtime.StageReviewA, "review-a", 2); found {
		t.Fatal("another round read this round's seat")
	}
	if _, found := runner.ReadSeatRecord(setup.runDir, runtime.StageReviewB, "review-b", 1); found {
		t.Fatal("another seat read this seat's record")
	}
}
