package attendant

import (
	"os"
	"path/filepath"
	"testing"

	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
)

// A hand is only a hand if it changes something. The investigating
// designer's card has no second occupant it could be launched as and its
// instruction is built by a verb this change does not touch, so a rebuild
// there would dispatch the identical investigation again — a whole
// investigation spent to arrive where it already was. It plays nothing and
// waits instead, which is what it did before candidate seats existed.
func TestTheInvestigatingDesignerPlaysNoHandItCannotPlay(t *testing.T) {
	setup := newLadderSetup(t, runner.StageFailure{
		Stage: runtime.StageInvestigate, Round: 1, Class: runner.FailureClassModel, Error: "the provider gave up",
	})
	// A consumer that does configure a designer, so what is measured here
	// is the card having no hand rather than the role being absent.
	withDesigner := `{"max_stages":3,"agents":{"applier":{"command":"launch"}},"models":{
	 "designer":{"id":"designer","vendor":"Vendor A","model":"model-a"},
	 "reviewers":[{"id":"review-a","vendor":"Vendor A","model":"model-a"},
	  {"id":"review-b","vendor":"Vendor B","model":"model-b"}]}}`
	if err := os.WriteFile(setup.config.ConsumerConfigPath, []byte(withDesigner), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := setup.climb(t); err != nil {
		t.Fatal(err)
	}
	record := setup.record()
	if len(record.Tried) != 0 {
		t.Fatalf("tried = %v, want nothing spent on a card nothing can change", record.Tried)
	}
	if record.LadderStep != rungWait {
		t.Fatalf("ladder step = %d, want the waiting rung", record.LadderStep)
	}
	if _, written := runner.ReadSeatRecord(setup.runDir, runtime.StageInvestigate, "designer", 1); written {
		t.Fatal("a seat record was written for a card that was not asked differently")
	}
}

// And a rebuild that could not be written is not recorded as played. The
// record says the card is being asked differently; if the instruction it
// reads was never rewritten, that record is a claim about the next attempt
// that is not true, and the card would run on exactly what it ran on before.
func TestARebuildThatCouldNotBeWrittenIsNotRecorded(t *testing.T) {
	setup := newLadderSetup(t, runner.StageFailure{
		Stage: runtime.StageApply, Round: 1, Class: runner.FailureClassModel, Error: "the provider gave up",
	})
	seatTheConsumer(t, setup)
	// No approved design, so the applier's instruction cannot be rendered.
	// Nothing else about the round changes.
	if _, err := setup.climb(t); err != nil {
		t.Fatal(err)
	}
	if _, written := runner.ReadSeatRecord(setup.runDir, runtime.StageApply, "author", 1); written {
		t.Fatal("the rebuild was recorded although the instruction was never written")
	}
	if _, err := os.Stat(filepath.Join(setup.runDir, "INSTRUCTION.md")); !os.IsNotExist(err) {
		t.Fatalf("an instruction appeared from nowhere: %v", err)
	}
	// The hand is spent either way — the ladder counts what it played
	// before it plays it, so a pod that stops between the two does not
	// play it twice — so the delivery descends rather than trying the same
	// rebuild for ever.
	if tried := setup.record().Tried; len(tried) != 1 || tried[0] != "prompt:shorten" {
		t.Fatalf("tried = %v, want the attempt counted once", tried)
	}
	if _, err := setup.climb(t); err != nil {
		t.Fatal(err)
	}
	if setup.record().LadderStep != rungWait {
		t.Fatalf("ladder step = %d, want the waiting rung after the hand", setup.record().LadderStep)
	}
}
