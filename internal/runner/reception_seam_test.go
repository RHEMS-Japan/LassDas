package runner

import (
	"context"
	"os"
	"testing"
)

// The reception is the only place this engine asks the requester anything,
// so whatever a question could be about has to be known before it runs.
//
// The seam is between the two halves of the preparation and nowhere else.
// Earlier than that there is nothing to read: the destination's own
// repository is not on the volume and the draft naming the destination has
// not been built. Later is too late: the questions have gone out, and no
// stage after the reception asks anybody anything, so a point raised
// afterwards is a point raised never. Both halves are pinned here because
// moving the seam either way is silent — the run still finishes, and the
// question simply never appears.
func TestTheReceptionIsPreparedBetweenTheTwoHalvesOfThePreparation(t *testing.T) {
	c := runnerFixtureConfig(t, 2, 1, true)
	p, _ := configuredRunner(t, c, "converged")

	called := 0
	var draftReady, cloneReady, receptionRan bool
	_, outcome, err := p.PrepareChainRun(context.Background(), func() error {
		called++
		draftReady = exists(p.path("ticket-draft.json"))
		cloneReady = exists(p.path("target-repo"))
		receptionRan = exists(p.path("history/readiness/assessment-1.json"))
		return nil
	})
	if err != nil || outcome.Code != "" {
		t.Fatalf("PrepareChainRun = %+v, %v", outcome, err)
	}
	if called != 1 {
		t.Fatalf("the reception was prepared %d times", called)
	}
	if !draftReady || !cloneReady {
		t.Fatalf("the reception was prepared before the destination could be read: draft %v, clone %v",
			draftReady, cloneReady)
	}
	if receptionRan {
		t.Fatal("the reception was prepared after it had already asked")
	}
	// And the reception really did run afterwards, so the ordering above is
	// about two things that both happened.
	if !exists(p.path("history/readiness/assessment-1.json")) {
		t.Fatal("the reception never ran")
	}
}

// A caller with nothing to prepare passes nil, and a preparation that fails
// does not fail the run: what it prepares makes the reception better
// informed, and a reception that runs without it asks what it always asked.
func TestAFailedPreparationStillLetsTheReceptionRun(t *testing.T) {
	c := runnerFixtureConfig(t, 2, 1, true)
	p, _ := configuredRunner(t, c, "converged")

	_, outcome, err := p.PrepareChainRun(context.Background(), func() error {
		return errNothingToPrepare
	})
	if err != nil || outcome.Code != "" {
		t.Fatalf("a preparation failure stopped the run: %+v, %v", outcome, err)
	}
	if !exists(p.path("history/readiness/assessment-1.json")) {
		t.Fatal("the reception never ran")
	}
}

var errNothingToPrepare = errPreparation("the destination's release path could not be read")

type errPreparation string

func (e errPreparation) Error() string { return string(e) }

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
