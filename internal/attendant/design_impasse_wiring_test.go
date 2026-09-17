package attendant

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
)

// A design whose rounds are spent puts the disagreement to its requester,
// and does it exactly once: the ask replaces the ending, it does not join
// it. The first attempt at this wiring sat on a branch the decide verb
// never takes at the limit, so the question was never asked at all (review
// of #199).
func TestDesignNonconvergenceAsksInsteadOfEnding(t *testing.T) {
	fixture := newPendingFixture(t, "")
	runDir := runDirectory(fixture.config, fixture.deliveryID)
	round := filepath.Join(runDir, "history", "design-1")
	if err := os.MkdirAll(round, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"decision.json":               `{"outcome":"nonconverged"}`,
		"design.json":                 `{}`,
		"investigation.json":          `{}`,
		"review-a-design-review.json": `{}`,
		"review-b-design-review.json": `{}`,
	} {
		if err := os.WriteFile(filepath.Join(round, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// The consumer configuration the reviewer ids come from.
	if err := os.WriteFile(fixture.config.ConsumerConfigPath, []byte(`{"models":{"reviewers":[{"id":"review-a"},{"id":"review-b"}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// A worker that writes the question the poster reads.
	decision := filepath.Join(runDir, "history", "question", "decision.json")
	script := filepath.Join(t.TempDir(), "worker")
	body := "#!/bin/sh\nmkdir -p " + filepath.Dir(decision) +
		"\nprintf '%s' '{\"outcome\":\"clarification_required\",\"questions\":[{\"id\":\"Q1\",\"question\":\"どちらにしますか\",\"why_blocking\":\"決められません\",\"dimension\":\"user_visible_behavior\",\"choices\":[{\"id\":\"a\",\"label\":\"A\",\"effect\":\"あ\"},{\"id\":\"b\",\"label\":\"B\",\"effect\":\"い\"}]}],\"decision_sha256\":\"" + strings.Repeat("d", 64) + "\"}' > " + decision + "\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	config := fixture.config
	config.WorkerBin = script

	var envelope hook.DispatchEnvelope
	if err := json.Unmarshal([]byte(fixture.run.EnvelopeJSON), &envelope); err != nil {
		t.Fatal(err)
	}
	hermes, _ := fakeBoard(t)
	card := runtime.BoardTask{ID: "t_d1", Status: "failed", IdempotencyKey: runtime.ChainCardKey(fixture.deliveryID, runtime.StageDesignDecide, 1)}
	view := chainViewFor([]runtime.BoardTask{card}, fixture.deliveryID)
	run := state.RunOverview{DeliveryID: fixture.deliveryID, RunID: "TKT-4242", IssueID: 4242, IssueKey: "TKT-4242"}

	handled, err := handleDesignChainFailure(context.Background(), config, fixture.services, hermes, envelope, run, view,
		runtime.ChainPlan{Shape: runtime.ShapeDesign}, runtime.StageDesignDecide, &recordingLogger{})
	if !handled {
		t.Fatalf("the failure was not handled: err=%v", err)
	}
	if _, statErr := os.Stat(decision); statErr != nil {
		t.Fatalf("no question was written: %v (handler err=%v)", statErr, err)
	}
	// This fixture wires no question poster, so the ask reports that and
	// the run does not end instead: the point is the ordering - the ask
	// comes first, and the ending is not also posted.
	if err == nil || !strings.Contains(err.Error(), "question poster") {
		t.Fatalf("the ask did not reach the poster: %v", err)
	}
	for _, posted := range fixture.comments.posted {
		if strings.Contains(posted, "design_nonconverged") {
			t.Fatalf("the run both asked and ended:\n%s", posted)
		}
	}
}
