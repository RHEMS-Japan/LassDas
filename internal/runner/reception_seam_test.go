package runner

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/worker"
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

// The last link of the chain: what the preparation sealed has to reach the
// cards that ask.
//
// Both halves are measured because the failure is silent either way. A plan
// sealed and not handed over leaves the reception believing nothing is
// missing — the delivery still finishes, and the single question about a
// means this engine was not handed simply never appears. Handing a path
// that was never sealed would make the command refuse a record it cannot
// read, and take the whole reception down with it.
func TestTheSealedPlanReachesTheCardsThatAsk(t *testing.T) {
	for name, sealPlan := range map[string]bool{
		"a destination missing part of its release path": true,
		"a destination whose path is complete":           false,
	} {
		c := runnerFixtureConfig(t, 2, 1, true)
		p, log := configuredRunner(t, c, "converged")

		_, outcome, err := p.PrepareChainRun(context.Background(), func() error {
			if sealPlan {
				sealPlanForTest(t, p.Workspace)
			}
			return nil
		})
		if err != nil || outcome.Code != "" {
			t.Fatalf("%s: PrepareChainRun = %+v, %v", name, outcome, err)
		}
		calls, readErr := os.ReadFile(log)
		if readErr != nil {
			t.Fatal(readErr)
		}
		for _, verb := range []string{"assess-readiness", "check-readiness"} {
			line := callLine(t, string(calls), verb)
			carried := strings.Contains(line, "--release-path")
			if carried != sealPlan {
				t.Fatalf("%s: %s carried --release-path = %v, want %v (%s)", name, verb, carried, sealPlan, line)
			}
		}
	}
}

// callLine is the recorded invocation of one verb.
func callLine(t *testing.T, calls, verb string) string {
	t.Helper()
	for _, line := range strings.Split(calls, "\n") {
		if strings.HasPrefix(line, verb+" ") {
			return line
		}
	}
	t.Fatalf("%s was never called: %s", verb, calls)
	return ""
}

// sealPlanForTest leaves a release path plan where the cards read it, the
// way the attendant's own preparation does.
func sealPlanForTest(t *testing.T, workspace string) {
	t.Helper()
	plan := worker.ReleasePathPlan{
		SchemaVersion: worker.ReleasePathSchemaVersion, Repository: "example/consumer",
		Configured: "production", DecidedAt: time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC),
		Items: []worker.ReleasePathItem{{
			Name: "production_origin", Kind: worker.ReleasePathOrigin,
			Detail: "本番が応答する場所です。",
			Means:  "納品先の環境そのものの値なので、本体には決められません。",
		}},
	}
	if err := plan.Seal(); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ReleasePathPlanFile(workspace), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

var errNothingToPrepare = errPreparation("the destination's release path could not be read")

type errPreparation string

func (e errPreparation) Error() string { return string(e) }

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
