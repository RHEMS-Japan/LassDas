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
	"fmt"
	"log/slog"
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
	config.Chain.Profiles = designTestProfiles()
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

// The third way back to the designer: an objection transition that died
// between archiving the round and creating the next one, resumed on a later
// tick with no design round left. It reaches the ending through a different
// function again, and without this the cause could be mislabelled there
// alone with the whole suite green (review of #201).
func TestAResumedObjectionAtTheLimitEndsAsRoundsSpent(t *testing.T) {
	fixture := newPendingFixture(t, "")
	runDir := runDirectory(fixture.config, fixture.deliveryID)
	for _, dir := range []string{"history/readiness", "history/design-3"} {
		if err := os.MkdirAll(filepath.Join(runDir, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(runDir, "history/readiness/decision.json"),
		[]byte(`{"request_kind":"change","needs_design":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "history/design-3/objection.json"),
		[]byte(`{"reason":"the label is not in that file","section":"files"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "ticket-envelope.json"),
		[]byte(fixture.run.EnvelopeJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	config := fixture.config
	config.Chain.Profiles = designTestProfiles()
	if err := os.WriteFile(config.ConsumerConfigPath, []byte(
		`{"max_stages":3,"models":{"reviewers":[{"id":"review-a"},{"id":"review-b"}]},"agents":{"applier":{"command":"true"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

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

	// Only the done design cards remain: the implementation cards were
	// archived and the next design round was never created.
	card := func(id, stage string, round int) runtime.BoardTask {
		return runtime.BoardTask{ID: id, Status: "done", IdempotencyKey: runtime.ChainCardKey(fixture.deliveryID, stage, round)}
	}
	view := chainViewFor([]runtime.BoardTask{
		card("t_i3", runtime.StageInvestigate, 3), card("t_a3", runtime.StageDesignReviewA, 3),
		card("t_b3", runtime.StageDesignReviewB, 3), card("t_d3", runtime.StageDesignDecide, 3),
	}, fixture.deliveryID)
	if view.round != 0 || view.designRound != 3 {
		t.Fatalf("view rounds: %+v", view.rounds())
	}

	hermes, _ := fakeBoard(t)
	run := state.RunOverview{DeliveryID: fixture.deliveryID, RunID: "TKT-4242", IssueID: 4242, IssueKey: "TKT-4242"}
	if err := advanceClaimedRun(context.Background(), config, fixture.services, hermes, run, view, &recordingLogger{}); err != nil {
		t.Fatalf("the resumed transition failed: %v", err)
	}
	if len(fixture.comments.posted) != 1 {
		t.Fatalf("comments posted = %d, want 1: %q", len(fixture.comments.posted), fixture.comments.posted)
	}
	if !strings.Contains(fixture.comments.posted[0], "設計をやり直せる回数を使い切っていた") {
		t.Errorf("the requester was not told why the run stopped: %q", fixture.comments.posted[0])
	}
}

// designTestProfiles is a pod with every design card's profile configured.
func designTestProfiles() runtime.ChainProfiles {
	return runtime.ChainProfiles{
		Implementer: "lassdas-implementer", ReviewA: "lassdas-review-a", ReviewB: "lassdas-review-b",
		Validate: "lassdas-validate", Publish: "lassdas-publish", Investigate: "lassdas-investigate",
		DesignReviewA: "lassdas-design-review-a", DesignReviewB: "lassdas-design-review-b",
		DesignDecide: "lassdas-design-decide", Applier: "lassdas-applier",
	}
}

// The impasse question is built from the design reviews' standing
// objections, so it belongs to the ending those objections produce. Here
// the design's judges agreed and the objection came from the applier: the
// requester gets the ending, not a question about a design nobody
// objected to. The whole question machinery is wired so the guard is what
// is measured, not its absence (review of #201).
func TestARoundsSpentEndingDoesNotAskTheDesignQuestion(t *testing.T) {
	fixture, envelope, view, runDir := designSpentFixture(t)
	// All three rounds, each with the records the question is built from:
	// the newest round is found by walking from the first, so a fixture
	// holding only the last one makes the question refuse itself and the
	// guard below untested (review of #201).
	for number := 1; number <= 3; number++ {
		round := filepath.Join(runDir, "history", fmt.Sprintf("design-%d", number))
		if err := os.MkdirAll(round, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, body := range map[string]string{
			"decision.json":               `{"outcome":"approved"}`,
			"design.json":                 `{}`,
			"investigation.json":          `{}`,
			"review-a-design-review.json": `{}`,
			"review-b-design-review.json": `{}`,
		} {
			if err := os.WriteFile(filepath.Join(round, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.WriteFile(fixture.config.ConsumerConfigPath,
		[]byte(`{"models":{"reviewers":[{"id":"review-a"},{"id":"review-b"}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// A worker that would write a question, and a ticket that would accept
	// it. If the guard were gone, both would be used.
	decision := filepath.Join(runDir, "history", "question", "decision.json")
	script := filepath.Join(t.TempDir(), "worker")
	body := "#!/bin/sh\nmkdir -p " + filepath.Dir(decision) +
		"\nprintf '%s' '{\"outcome\":\"clarification_required\",\"questions\":[{\"id\":\"Q1\",\"question\":\"どちらにしますか\",\"why_blocking\":\"決められません\",\"dimension\":\"user_visible_behavior\",\"choices\":[{\"id\":\"a\",\"label\":\"A\",\"effect\":\"あ\"},{\"id\":\"b\",\"label\":\"B\",\"effect\":\"い\"}]}],\"decision_sha256\":\"" + strings.Repeat("d", 64) + "\"}' > " + decision + "\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	config := fixture.config
	config.WorkerBin = script
	poster := &designQuestionFakes{}
	question, err := hook.NewQuestionReportService(fixture.services.Route, poster, poster, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	fixture.services.Question = question

	terminal := runner.NewTerminal(config, fixture.services, envelope, chainOwnerRunID(fixture.deliveryID), runDir, &recordingLogger{})
	digest, err := terminal.ReportDigest(context.Background(), hook.TerminalDesignRoundsSpent,
		runner.Outcome{Code: hook.TerminalDesignRoundsSpent}, "")
	if err != nil {
		t.Fatal(err)
	}
	fixture.store.expected = digest

	hermes, _ := fakeBoard(t)
	run := state.RunOverview{DeliveryID: fixture.deliveryID, RunID: "TKT-4242", IssueID: 4242, IssueKey: "TKT-4242"}
	handled, err := handleDesignChainFailure(context.Background(), config, fixture.services, hermes, envelope, run, view,
		runtime.ChainPlan{Shape: runtime.ShapeDesign}, runtime.StageApply, &recordingLogger{})
	if !handled || err != nil {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if len(poster.posted) != 0 {
		t.Fatalf("a question was asked about a design its judges agreed on: %q", poster.posted)
	}
	if len(fixture.comments.posted) != 1 {
		t.Fatalf("comments posted = %d, want 1: %q", len(fixture.comments.posted), fixture.comments.posted)
	}
}

// A delivery whose design was approved, whose change was written, and
// whose reviewers then asked for a different design does not die when the
// design rounds are spent: the change is written again under the design it
// has, because that is the only work left and it is work that can finish.
// Measured live: exactly that delivery ended with nothing delivered
// (完遂率を最優先、発注者指示 2026-09-17).
func TestAChangeIsWrittenAgainWhenTheDesignCannotBe(t *testing.T) {
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
		// The change the round wrote, and both reviewers asking for a
		// different design.
		"history/stage-1/candidate.json": `{}`,
		"history/stage-1/review-a.json":  `{"findings":[{"code":"design-wrong"}]}`,
		"history/stage-1/review-b.json":  `{"findings":[{"code":"design-wrong"}]}`,
		// The design the reviews approved, which the next attempt writes
		// again from.
		"history/design-1/investigation.json": `{}`,
		"history/design-2/investigation.json": `{}`,
		"history/design-3/investigation.json": `{}`,
		"history/design-3/decision.json":      `{"outcome":"approved"}`,
		"history/design-3/design.json":        `{}`,
		"history/design-3/DESIGN.md":          "# 設計\n\nREADME.md のみを変更する。\n",
		"ticket-draft.json":                   `{"repository":"example/consumer"}`,
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(runDir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(runDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	config := fixture.config
	config.Chain.Profiles = designTestProfiles()
	if err := os.WriteFile(config.ConsumerConfigPath, []byte(
		`{"max_stages":3,"models":{"reviewers":[{"id":"review-a"},{"id":"review-b"}]},"agents":{"applier":{"command":"true"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
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

	hermes, callLog := fakeBoard(t)
	run := state.RunOverview{DeliveryID: fixture.deliveryID, RunID: "TKT-4242", IssueID: 4242, IssueKey: "TKT-4242"}
	if err := handleChainFailure(context.Background(), config, fixture.services, hermes, envelope, run, view,
		runtime.StageValidate, &recordingLogger{}); err != nil {
		t.Fatalf("the delivery did not carry on: %v", err)
	}
	if len(fixture.comments.posted) != 0 {
		t.Fatalf("the delivery ended instead of writing the change again: %q", fixture.comments.posted)
	}
	_, created := boardCalls(t, callLog)
	if !containsID(created, fixture.deliveryID+":apply:r2") {
		t.Errorf("no second implementation round was created: %v", created)
	}
}

// A delivery whose records were written by another engine is recognised,
// and the reception is what decides it - checked before anything that
// would read one of those records. Every stage refuses a draft written by
// a different engine, so such a delivery could not take another step, and
// the failed card was healed and dispatched again every minute for ever:
// measured live, thirty-one minutes in 工程の復旧処理中 after an upgrade
// (完遂率を最優先、発注者指示 2026-09-17).
func TestADeliveryCaughtByAnEngineUpdateIsRecognised(t *testing.T) {
	runDir := t.TempDir()
	running := strings.Repeat("b", 40)
	config := runtime.Config{Identity: runtime.IdentityConfig{EngineSHA: running}}

	// No draft yet: nothing to compare, and the run carries on.
	if _, changed := engineChangedUnderRun(config, runDir); changed {
		t.Error("a delivery with no draft was called interrupted")
	}
	write := func(sha string) {
		if err := os.WriteFile(filepath.Join(runDir, "ticket-draft.json"),
			[]byte(`{"tool_sha":"`+sha+`","repository":"example/consumer"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(running)
	if _, changed := engineChangedUnderRun(config, runDir); changed {
		t.Error("a delivery written by the engine that is running was called interrupted")
	}
	write(strings.Repeat("a", 40))
	wrote, changed := engineChangedUnderRun(config, runDir)
	if !changed || wrote != strings.Repeat("a", 40) {
		t.Fatalf("an interrupted delivery was not recognised: %q %v", wrote, changed)
	}
}

// And it is decided before anything reads a record that would refuse it.
func TestTheEngineCheckComesFirst(t *testing.T) {
	body, err := os.ReadFile("chains.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(body)
	start := strings.Index(source, "func advanceClaimedRun(")
	if start < 0 {
		t.Fatal("the function was not found; this check is looking in the wrong place")
	}
	end := strings.Index(source[start:], "\n}\n")
	if end < 0 {
		t.Fatal("the function does not end")
	}
	within := source[start : start+end]
	checked := strings.Index(within, "engineChangedUnderRun(")
	if checked < 0 {
		t.Fatal("a delivery caught by an engine update is no longer recognised, so it would be healed for ever")
	}
	// readEnvelope is the first record read, and the plan after it; both
	// belong to the engine that wrote them.
	if reads := strings.Index(within, "readEnvelope("); reads >= 0 && reads < checked {
		t.Error("a record is read before the engine is compared, so the delivery fails on it instead of starting again")
	}
}
