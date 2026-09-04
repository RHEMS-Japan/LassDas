package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/visiblecheck"
)

// A stand-in observation tool that exits with `refuseCode` for the first N
// calls and passes afterwards, counting its calls in a file and writing
// the evidence only when it passes — like the real tool.
func settlingBrowsercheck(t *testing.T, pipeline *Pipeline, refusals, refuseCode int) string {
	t.Helper()
	counter := filepath.Join(t.TempDir(), "calls.txt")
	script := filepath.Join(t.TempDir(), "browsercheck.sh")
	body := "#!/bin/sh\n" +
		"n=0; [ -f " + counter + " ] && n=$(cat " + counter + ")\n" +
		"n=$((n+1)); echo $n > " + counter + "\n" +
		"[ $n -le " + strconv.Itoa(refusals) + " ] && exit " + strconv.Itoa(refuseCode) + "\n" +
		"out=\"\"; prev=\"\"\nfor a in \"$@\"; do [ \"$prev\" = \"--evidence-out\" ] && out=\"$a\"; prev=\"$a\"; done\n" +
		"[ -n \"$out\" ] && echo '{}' > \"$out\"\n" +
		"exit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	pipeline.Config.BrowserCheckBin = script
	return counter
}

func settleStage(t *testing.T, pipeline *Pipeline) string {
	t.Helper()
	stageDir := "history/stage-1"
	if err := os.MkdirAll(pipeline.path(stageDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pipeline.path(stageDir+"/ticket.json"), []byte(`{"tool_sha":"`+strings.Repeat("a", 40)+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return stageDir
}

func calls(t *testing.T, counter string) int {
	t.Helper()
	raw, err := os.ReadFile(counter)
	if err != nil {
		return 0
	}
	count, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	return count
}

func fastSettle(t *testing.T) {
	t.Helper()
	previous := observationSettleInterval
	observationSettleInterval = time.Millisecond
	t.Cleanup(func() { observationSettleInterval = previous })
}

var settleReviews = []string{"review-a.json", "review-b.json"}

// A deployment that serves the old build for a while: the observation is
// repeated until it passes, and only the pass leaves evidence.
func TestObserveUntilSettledRepeatsARefusedObservation(t *testing.T) {
	fastSettle(t)
	pipeline := deliverPipeline(t)
	stageDir := settleStage(t, pipeline)
	counter := settlingBrowsercheck(t, pipeline, 2, visiblecheck.ExitEvidenceRejected)
	evidence := pipeline.path("staging-visible.json")
	code, err := pipeline.observeUntilSettled(context.Background(), stageDir, settleReviews, "staging", "--evidence-out", evidence)
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if calls(t, counter) != 3 {
		t.Fatalf("two refusals then a pass = 3 calls, got %d", calls(t, counter))
	}
	if _, err := os.Stat(evidence); err != nil {
		t.Fatal("the passing attempt must have left the evidence")
	}
}

// A page that never shows the promise: the refusal stands after the settle
// budget, no sooner and no later.
func TestObserveUntilSettledGivesUpAfterTheBudget(t *testing.T) {
	fastSettle(t)
	pipeline := deliverPipeline(t)
	stageDir := settleStage(t, pipeline)
	counter := settlingBrowsercheck(t, pipeline, 99, visiblecheck.ExitEvidenceRejected)
	code, err := pipeline.observeUntilSettled(context.Background(), stageDir, settleReviews, "staging", "--evidence-out", pipeline.path("staging-visible.json"))
	if err != nil || code != visiblecheck.ExitEvidenceRejected {
		t.Fatalf("a page that never passes must still be refused: code=%d err=%v", code, err)
	}
	if calls(t, counter) != observationSettleAttempts {
		t.Fatalf("the budget is exactly %d attempts, got %d", observationSettleAttempts, calls(t, counter))
	}
	if _, err := os.Stat(pipeline.path("staging-visible.json")); err == nil {
		t.Fatal("a refused observation must leave no evidence")
	}
}

// Only the page's refusal is worth waiting out: a login the destination
// refused, an invalid input, and a tool that cannot run all return at
// once, after a single attempt.
func TestObserveUntilSettledReturnsEveryOtherOutcomeAtOnce(t *testing.T) {
	fastSettle(t)
	for _, code := range []int{visiblecheck.ExitSignInRefused, 1} {
		pipeline := deliverPipeline(t)
		stageDir := settleStage(t, pipeline)
		counter := settlingBrowsercheck(t, pipeline, 99, code)
		got, err := pipeline.observeUntilSettled(context.Background(), stageDir, settleReviews, "staging")
		if err != nil || got != code || calls(t, counter) != 1 {
			t.Fatalf("exit %d: got=%d err=%v calls=%d", code, got, err, calls(t, counter))
		}
	}
	pipeline := deliverPipeline(t)
	stageDir := settleStage(t, pipeline)
	pipeline.Config.BrowserCheckBin = filepath.Join(t.TempDir(), "absent-browsercheck")
	if _, err := pipeline.observeUntilSettled(context.Background(), stageDir, settleReviews, "staging"); err == nil {
		t.Fatal("a missing tool must be an error")
	}
}

// A cancelled card does not sit out the settle interval.
func TestObserveUntilSettledStopsWhenCancelled(t *testing.T) {
	previous := observationSettleInterval
	observationSettleInterval = time.Hour
	t.Cleanup(func() { observationSettleInterval = previous })
	pipeline := deliverPipeline(t)
	stageDir := settleStage(t, pipeline)
	settlingBrowsercheck(t, pipeline, 99, visiblecheck.ExitEvidenceRejected)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	_, err := pipeline.observeUntilSettled(ctx, stageDir, settleReviews, "staging")
	if !errors.Is(err, context.Canceled) || time.Since(started) > 10*time.Second {
		t.Fatalf("a cancelled context must end the wait at once: err=%v after %s", err, time.Since(started))
	}
}
