package runner

import (
	"path/filepath"
	"syscall"
	"testing"

	"automation.internal/ticket-ingress/internal/runtime"
)

// A delivery card seals its account of a failure where the tick looks for
// it, and the tick finds it there.
//
// A delivery card counts in no round of its own — the change it carries was
// sealed by a round that is over — so the two sides have to agree on one
// number. Disagree, and the write is refused without a word and the read
// comes back empty: every delivery failure then reads as one nobody could
// name, and the remedy is always to wait. A volume that filled would be
// waited on instead of swept.
func TestADeliveryCardSealsItsFailureWhereTheTickLooks(t *testing.T) {
	for _, stage := range []string{runtime.DeliverStageChecks, runtime.DeliverStageIntegrate, runtime.DeliverStagePromote} {
		workspace := t.TempDir()
		pipeline := &Pipeline{Workspace: workspace}
		if round := pipeline.failureRound(stage); round != runtime.DeliverRound {
			t.Fatalf("%s: failureRound() = %d, want %d", stage, round, runtime.DeliverRound)
		}
		pipeline.SealStageFailure(stage, &verbFailure{verb: "deliver", code: -1, err: syscall.ENOSPC})

		record, ok := ReadStageFailure(workspace, stage, runtime.DeliverRound)
		if !ok {
			t.Fatalf("%s: the sealed record did not read back", stage)
		}
		if record.Stage != stage || record.Round != runtime.DeliverRound {
			t.Fatalf("%s: the record names %s round %d", stage, record.Stage, record.Round)
		}
		if record.Class != FailureClassDisk {
			t.Fatalf("%s: class = %q, want the volume that filled", stage, record.Class)
		}
		// Beside the chain's own rounds, never inside one: a delivery
		// record landing in an implementation round would sit among the
		// records that round is re-derived from.
		want := filepath.Join(workspace, "history", "deliver-1", stage+"-failure.json")
		if got := StageFailureFile(workspace, stage, runtime.DeliverRound); got != want {
			t.Fatalf("%s: the record is written to %s, want %s", stage, got, want)
		}
	}
}

// A name that is neither the chain's nor the delivery's seals nothing: it
// would otherwise choose the directory the record lands in.
func TestAnUnknownStageSealsNothing(t *testing.T) {
	workspace := t.TempDir()
	pipeline := &Pipeline{Workspace: workspace}
	pipeline.SealStageFailure("promote-to-somewhere", &verbFailure{verb: "x", code: 1})
	if _, ok := ReadStageFailure(workspace, "promote-to-somewhere", runtime.DeliverRound); ok {
		t.Fatal("an unknown stage sealed a record")
	}
	if err := SealStageFailureRecord(workspace, StageFailure{Stage: "promote-to-somewhere", Round: 1}); err == nil {
		t.Fatal("an unknown stage's record was accepted")
	}
}
