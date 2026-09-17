package attendant

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/backlog"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
)

// A delivery whose design was agreed, written, and then judged to need a
// different plan ends as its own thing. It used to end as
// design_nonconverged, and a requester whose three design rounds had all
// passed was told the design reviews never agreed: the reason pointed at a
// disagreement that had not happened, so nobody looked where the run
// actually stopped (live 2026-09-17).
func TestAPlanCalledWrongAfterItWasAgreedEndsAsItsOwnThing(t *testing.T) {
	fixture, envelope, view, runDir := designSpentFixture(t)

	// The store accepts exactly the report this ending should produce, so a
	// run that reports the other ending is refused rather than counted.
	terminal := runner.NewTerminal(fixture.config, fixture.services, envelope, chainOwnerRunID(fixture.deliveryID), runDir, &recordingLogger{})
	digest, err := terminal.ReportDigest(context.Background(), hook.TerminalDesignRoundsSpent,
		runner.Outcome{Code: hook.TerminalDesignRoundsSpent}, "")
	if err != nil {
		t.Fatal(err)
	}
	fixture.store.expected = digest

	hermes, _ := fakeBoard(t)
	run := state.RunOverview{DeliveryID: fixture.deliveryID, RunID: "TKT-4242", IssueID: 4242, IssueKey: "TKT-4242"}
	handled, err := handleDesignChainFailure(context.Background(), fixture.config, fixture.services, hermes, envelope, run, view,
		runtime.ChainPlan{Shape: runtime.ShapeDesign}, runtime.StageApply, &recordingLogger{})
	if !handled || err != nil {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if len(fixture.comments.posted) != 1 {
		t.Fatalf("comments posted = %d, want 1: %q", len(fixture.comments.posted), fixture.comments.posted)
	}
	posted := fixture.comments.posted[0]
	// What the requester reads has to say what happened to their design,
	// not the opposite of it.
	if !strings.Contains(posted, "設計をやり直せる回数を使い切っていた") {
		t.Errorf("the requester was not told why the run stopped: %q", posted)
	}
	if strings.Contains(posted, "合意に至らなかった") {
		t.Errorf("the requester was told the design reviews disagreed, which they did not: %q", posted)
	}
}

// The other ending keeps its own words: the design's own judges never
// agreed, and that requester still reads that.
func TestDesignReviewsThatNeverAgreedKeepTheirOwnEnding(t *testing.T) {
	for _, shape := range []runtime.ChainShape{runtime.ShapeDesign, runtime.ShapeInvestigation} {
		if code := designReviewsDisagreed.terminalCode(shape); shape == runtime.ShapeDesign && code != hook.TerminalDesignNonconverged {
			t.Errorf("a design whose reviews disagreed ended as %q", code)
		} else if shape == runtime.ShapeInvestigation && code != hook.TerminalInvestigationNonconverged {
			t.Errorf("an investigation whose reviews disagreed ended as %q", code)
		}
	}
	// An investigation carries no implementation, so nothing downstream of
	// it can call its plan wrong; it keeps the one ending it can reach.
	if code := designCalledWrongLater.terminalCode(runtime.ShapeInvestigation); code != hook.TerminalInvestigationNonconverged {
		t.Errorf("an investigation ended as %q", code)
	}
	if code := designCalledWrongLater.terminalCode(runtime.ShapeDesign); code != hook.TerminalDesignRoundsSpent {
		t.Errorf("a plan called wrong after it was agreed ended as %q", code)
	}
}

// designSpentFixture is a delivery at its last design round whose applier
// objected to the agreed design: the point where the rounds run out.
func designSpentFixture(t *testing.T) (pendingFixture, hook.DispatchEnvelope, chainView, string) {
	t.Helper()
	fixture := newPendingFixture(t, "")
	runDir := runDirectory(fixture.config, fixture.deliveryID)
	round := filepath.Join(runDir, "history", "design-3")
	if err := os.MkdirAll(round, 0o755); err != nil {
		t.Fatal(err)
	}
	// The applier's objection to the design it was handed.
	if err := os.WriteFile(filepath.Join(round, "objection.json"), []byte(`{"reason":"the label is not in that file","section":"files"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var envelope hook.DispatchEnvelope
	if err := json.Unmarshal([]byte(fixture.run.EnvelopeJSON), &envelope); err != nil {
		t.Fatal(err)
	}
	card := func(id, stage string, round int) runtime.BoardTask {
		return runtime.BoardTask{ID: id, Status: "done", IdempotencyKey: runtime.ChainCardKey(fixture.deliveryID, stage, round)}
	}
	// Three design rounds, all decided, and the apply card that objected.
	tasks := []runtime.BoardTask{
		card("t_i3", runtime.StageInvestigate, 3), card("t_a3", runtime.StageDesignReviewA, 3),
		card("t_b3", runtime.StageDesignReviewB, 3), card("t_d3", runtime.StageDesignDecide, 3),
		{ID: "t_apply", Status: "failed", IdempotencyKey: runtime.ChainCardKey(fixture.deliveryID, runtime.StageApply, 1)},
	}
	return fixture, envelope, chainViewFor(tasks, fixture.deliveryID), runDir
}

// The path the live run took: the design converged in three rounds, the
// change was written, its reviewers found the design itself wrong, and
// there was no design round left. A blocked validate card is the first
// failed card the tick sees, so this arrives through the general failure
// handler rather than the design one - which is why mislabelling only that
// call site would otherwise go unnoticed (live 2026-09-17).
func TestAReviewFindingTheDesignWrongAtTheLimitEndsAsRoundsSpent(t *testing.T) {
	fixture := newPendingFixture(t, "")
	runDir := runDirectory(fixture.config, fixture.deliveryID)
	for _, dir := range []string{"history/readiness", "history/stage-1", "history/design-3"} {
		if err := os.MkdirAll(filepath.Join(runDir, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{
		"history/readiness/decision.json": `{"request_kind":"change","needs_design":true}`,
		"history/stage-1/decision.json":   `{"outcome":"revise"}`,
		// Both reviewers of the written change say the plan is wrong.
		"history/stage-1/review-a.json": `{"findings":[{"code":"design-wrong"}]}`,
		"history/stage-1/review-b.json": `{"findings":[{"code":"design-wrong"}]}`,
	} {
		if err := os.WriteFile(filepath.Join(runDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	config := fixture.config
	config.Chain.Profiles = runtime.ChainProfiles{
		Implementer: "lassdas-implementer", ReviewA: "lassdas-review-a", ReviewB: "lassdas-review-b",
		Validate: "lassdas-validate", Publish: "lassdas-publish", Investigate: "lassdas-investigate",
		DesignReviewA: "lassdas-design-review-a", DesignReviewB: "lassdas-design-review-b",
		DesignDecide: "lassdas-design-decide", Applier: "lassdas-applier",
	}
	if err := os.WriteFile(config.ConsumerConfigPath, []byte(
		`{"max_stages":3,"models":{"reviewers":[{"id":"review-a"},{"id":"review-b"}]},"agents":{"applier":{"command":"true"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// A tracker that answers "no comments", so the stop check passes through.
	quiet, err := backlog.NewClient(backlog.Config{SpaceKey: "example", APIKey: "k", Origin: "https://example.backlog.com", Timeout: time.Second, MaxResponseBytes: 1 << 20},
		roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("[]")), Header: http.Header{}}, nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	fixture.services.Backlog = quiet

	var envelope hook.DispatchEnvelope
	if err := json.Unmarshal([]byte(fixture.run.EnvelopeJSON), &envelope); err != nil {
		t.Fatal(err)
	}
	terminal := runner.NewTerminal(config, fixture.services, envelope, chainOwnerRunID(fixture.deliveryID), runDir, &recordingLogger{})
	digest, err := terminal.ReportDigest(context.Background(), hook.TerminalDesignRoundsSpent,
		runner.Outcome{Code: hook.TerminalDesignRoundsSpent}, "")
	if err != nil {
		t.Fatal(err)
	}
	fixture.store.expected = digest

	card := func(id, stage, status string, round int) runtime.BoardTask {
		return runtime.BoardTask{ID: id, Status: status, IdempotencyKey: runtime.ChainCardKey(fixture.deliveryID, stage, round)}
	}
	view := chainViewFor([]runtime.BoardTask{
		card("t_i3", runtime.StageInvestigate, "done", 3), card("t_a3", runtime.StageDesignReviewA, "done", 3),
		card("t_b3", runtime.StageDesignReviewB, "done", 3), card("t_d3", runtime.StageDesignDecide, "done", 3),
		card("t_apply", runtime.StageApply, "done", 1), card("t_ra", runtime.StageReviewA, "done", 1),
		card("t_rb", runtime.StageReviewB, "done", 1), card("t_v", runtime.StageValidate, "blocked", 1),
		card("t_p", runtime.StagePublish, "todo", 1),
	}, fixture.deliveryID)

	hermes, _ := fakeBoard(t)
	run := state.RunOverview{DeliveryID: fixture.deliveryID, RunID: "TKT-4242", IssueID: 4242, IssueKey: "TKT-4242"}
	if err := handleChainFailure(context.Background(), config, fixture.services, hermes, envelope, run, view,
		runtime.StageValidate, &recordingLogger{}); err != nil {
		t.Fatalf("the failure was not handled: %v", err)
	}
	if len(fixture.comments.posted) != 1 {
		t.Fatalf("comments posted = %d, want 1: %q", len(fixture.comments.posted), fixture.comments.posted)
	}
	posted := fixture.comments.posted[0]
	if !strings.Contains(posted, "設計をやり直せる回数を使い切っていた") {
		t.Errorf("the requester was not told why the run stopped: %q", posted)
	}
	if strings.Contains(posted, "合意に至らなかった") {
		t.Errorf("the requester was told the design reviews disagreed, which they did not: %q", posted)
	}
}
