package attendant

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
)

// Every stage a chain can run needs a name a requester recognises. The list
// comes from runtime rather than from a copy kept here: a copy is a
// self-comparison, and a stage added to the engine without being named
// would pass it while reporting the older sentence that names no step.
func TestEveryStepOfTheWorkHasARequesterFacingName(t *testing.T) {
	for _, step := range runtime.AllStages() {
		name, ok := requesterStepNames[step]
		if !ok || name == "" {
			t.Errorf("step %q has no requester-facing name", step)
			continue
		}
		// A name past the bound fails the report's shape check, and a report
		// that fails it never reaches the requester: the run retries for
		// ever with no comment at all.
		if len(name) > hook.MaxFailedStepBytes {
			t.Errorf("name for %q is %d bytes, past the %d the report accepts: %q", step, len(name), hook.MaxFailedStepBytes, name)
		}
		for _, internal := range []string{"stage", "card", "chain", "review-a", "review-b", "verb"} {
			if strings.Contains(name, internal) {
				t.Errorf("name for %q carries internal vocabulary %q: %q", step, internal, name)
			}
		}
		if strings.ContainsAny(name, "\n\r") {
			t.Errorf("name for %q is not one line: %q", step, name)
		}
	}
	if len(requesterStepNames) != len(runtime.AllStages()) {
		t.Errorf("the table holds %d names for %d stages", len(requesterStepNames), len(runtime.AllStages()))
	}
	if len(candidateSealStep) > hook.MaxFailedStepBytes {
		t.Errorf("the seal step's name is %d bytes, past the bound", len(candidateSealStep))
	}
}

// The sentence each step produces, in full. A name is only as good as the
// sentence it lands in, and three of these steps ask no model anything: a
// sentence that says the AI failed at them is a specific false claim where
// the older one was only a vague one.
func TestTheSentenceEveryStepProducesIsTrueOfThatStep(t *testing.T) {
	want := map[string]string{
		runtime.StageInvestigate:   "AI による調査と設計を完了できなかったため、本番環境には反映していません。",
		runtime.StageDesignReviewA: "AI による設計のレビューを完了できなかったため、本番環境には反映していません。",
		runtime.StageDesignReviewB: "AI による設計のレビューを完了できなかったため、本番環境には反映していません。",
		runtime.StageDesignDecide:  "設計レビューの集計を完了できなかったため、本番環境には反映していません。",
		runtime.StageApply:         "AI による設計にもとづく変更の作成を完了できなかったため、本番環境には反映していません。",
		runtime.StageImplement:     "AI による変更の作成を完了できなかったため、本番環境には反映していません。",
		runtime.StageReviewA:       "AI による変更のレビューを完了できなかったため、本番環境には反映していません。",
		runtime.StageReviewB:       "AI による変更のレビューを完了できなかったため、本番環境には反映していません。",
		runtime.StageValidate:      "変更の検証を完了できなかったため、本番環境には反映していません。",
		runtime.StagePublish:       "Pull Request の公開を完了できなかったため、本番環境には反映していません。",
	}
	for _, step := range runtime.AllStages() {
		sentence, ok := want[step]
		if !ok {
			t.Fatalf("step %q has no expected sentence: a stage was added without one", step)
		}
		comment := hook.TerminalCommentContent(hook.TerminalReportRequest{
			Code: hook.TerminalModelFailed, AutomationRunID: "run-1", FailedStep: requesterStepNames[step],
		}, strings.Repeat("0", 64))
		// The whole line, not a substring of it: a name with anything added
		// in front still contains the sentence it should have produced, so
		// Contains would pass a step that newly claims a model ran.
		if got := modelFailureSentence(comment); got != sentence {
			t.Errorf("step %q produces the wrong sentence.\nwant: %s\ngot:  %s", step, sentence, got)
		}
	}
	// The seal's own sentence, for the half of the first review card that is
	// not a review at all.
	comment := hook.TerminalCommentContent(hook.TerminalReportRequest{
		Code: hook.TerminalModelFailed, AutomationRunID: "run-1", FailedStep: candidateSealStep,
	}, strings.Repeat("0", 64))
	if got := modelFailureSentence(comment); got != "変更の確定を完了できなかったため、本番環境には反映していません。" {
		t.Errorf("the seal's sentence is wrong: %s", got)
	}
}

// modelFailureSentence is the one line of a terminal comment that says what
// could not be completed.
func modelFailureSentence(comment string) string {
	for _, line := range strings.Split(comment, "\n") {
		if strings.HasSuffix(line, "を完了できなかったため、本番環境には反映していません。") {
			return line
		}
	}
	return ""
}

// The publish card never ends a run as a model failure, which is why its
// name is never seen. That is a fact about classifyChainFailure, so it is
// held there: if publish ever did end this way, its name would be in front
// of a requester unmeasured.
func TestThePublishCardNeverEndsARunAsAModelFailure(t *testing.T) {
	action, code := classifyChainFailure(runtime.StagePublish,
		func() (string, error) { return "", nil }, func() (string, error) { return "", nil })
	if action != actionReport || code != hook.TerminalReleaseFailed {
		t.Fatalf("publish failure = (%v, %q), want a release failure", action, code)
	}
}

// The first review card of a round seals the candidate before it reviews
// anything, and the two fail differently. Reporting the seal as the review
// tells the requester the change exists and the judging failed, when
// nothing was written at all — which is what live runs did.
func TestTheSealAndTheReviewAreNotToldAsTheSameStep(t *testing.T) {
	runDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(runDir, "history", "stage-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := failedStepFor(runDir, runtime.StageReviewA, 1); got != candidateSealStep {
		t.Fatalf("with no candidate sealed the step = %q, want %q", got, candidateSealStep)
	}
	if err := os.WriteFile(filepath.Join(runDir, "history", "stage-1", "candidate.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := failedStepFor(runDir, runtime.StageReviewA, 1); got != requesterStepNames[runtime.StageReviewA] {
		t.Fatalf("with a candidate sealed the step = %q, want the review", got)
	}
	// The second review card seals nothing, so it is the review either way.
	if got := failedStepFor(t.TempDir(), runtime.StageReviewB, 1); got != requesterStepNames[runtime.StageReviewB] {
		t.Fatalf("the second review card's step = %q", got)
	}
}

// The behaviour, end to end: a run that ends model_failed says which step it
// failed at, in the comment the requester actually reads.
func TestAModelFailureTellsTheRequesterWhichStepFailed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stage string
		want  string
	}{
		{"investigation", runtime.StageInvestigate, "AI による調査と設計を完了できなかった"},
		{"design review", runtime.StageDesignReviewA, "AI による設計のレビューを完了できなかった"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPendingFixture(t, "")
			runDir := runDirectory(fixture.config, fixture.deliveryID)
			if err := os.MkdirAll(filepath.Join(runDir, "history", "design-1"), 0o755); err != nil {
				t.Fatal(err)
			}
			var envelope hook.DispatchEnvelope
			if err := json.Unmarshal([]byte(fixture.run.EnvelopeJSON), &envelope); err != nil {
				t.Fatal(err)
			}
			terminal := runner.NewTerminal(fixture.config, fixture.services, envelope, chainOwnerRunID(fixture.deliveryID), runDir, &pendingTestLogger{})
			digest, err := terminal.ReportDigest(context.Background(), hook.TerminalModelFailed, runner.Outcome{Code: hook.TerminalModelFailed}, "")
			if err != nil {
				t.Fatal(err)
			}
			fixture.store.expected = digest
			hermes, _ := fakeBoard(t)
			card := runtime.BoardTask{ID: "t_m1", Status: "failed", IdempotencyKey: runtime.ChainCardKey(fixture.deliveryID, tc.stage, 1)}
			view := chainViewFor([]runtime.BoardTask{card}, fixture.deliveryID)
			run := state.RunOverview{DeliveryID: fixture.deliveryID, RunID: "TKT-4242", IssueID: 4242, IssueKey: "TKT-4242"}
			handled, err := handleDesignChainFailure(context.Background(), fixture.config, fixture.services, hermes, envelope, run, view,
				runtime.ChainPlan{Shape: runtime.ShapeDesign}, tc.stage, &recordingLogger{})
			if !handled || err != nil {
				t.Fatalf("handled=%v err=%v", handled, err)
			}
			if len(fixture.comments.posted) != 1 {
				t.Fatalf("comments posted = %d, want one: %q", len(fixture.comments.posted), fixture.comments.posted)
			}
			posted := fixture.comments.posted[0]
			if !strings.Contains(posted, tc.want) {
				t.Fatalf("the comment does not name the step (%q):\n%s", tc.want, posted)
			}
			if strings.Contains(posted, "成果物の生成またはレビューを完了できなかった") {
				t.Fatalf("the comment still carries the sentence that names no step:\n%s", posted)
			}
			// The step is written down for a report that has to be posted
			// again: the run row does not carry it and the board will be gone.
			if recorded := runner.RecordedFailedStep(runDir); recorded["failed_step"] == "" {
				t.Fatal("the step was not recorded for a second attempt")
			}
		})
	}
}

// The implementation side ends through its own handler, and a call site
// added on one of the two is the way half of this goes missing.
func TestAModelFailureOnTheImplementationSideAlsoNamesItsStep(t *testing.T) {
	fixture := newPendingFixture(t, "")
	runDir := runDirectory(fixture.config, fixture.deliveryID)
	if err := os.MkdirAll(filepath.Join(runDir, "history", "stage-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "history", "stage-1", "candidate.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var envelope hook.DispatchEnvelope
	if err := json.Unmarshal([]byte(fixture.run.EnvelopeJSON), &envelope); err != nil {
		t.Fatal(err)
	}
	terminal := runner.NewTerminal(fixture.config, fixture.services, envelope, chainOwnerRunID(fixture.deliveryID), runDir, &pendingTestLogger{})
	digest, err := terminal.ReportDigest(context.Background(), hook.TerminalModelFailed, runner.Outcome{Code: hook.TerminalModelFailed}, "")
	if err != nil {
		t.Fatal(err)
	}
	fixture.store.expected = digest
	hermes, _ := fakeBoard(t)
	card := runtime.BoardTask{ID: "t_r1", Status: "failed", IdempotencyKey: runtime.ChainCardKey(fixture.deliveryID, runtime.StageReviewA, 1)}
	view := chainViewFor([]runtime.BoardTask{card}, fixture.deliveryID)
	run := state.RunOverview{DeliveryID: fixture.deliveryID, RunID: "TKT-4242", IssueID: 4242, IssueKey: "TKT-4242"}
	if err := handleChainFailure(context.Background(), fixture.config, fixture.services, hermes, envelope, run, view,
		runtime.StageReviewA, &recordingLogger{}); err != nil {
		t.Fatal(err)
	}
	if len(fixture.comments.posted) != 1 {
		t.Fatalf("comments posted = %d, want one: %q", len(fixture.comments.posted), fixture.comments.posted)
	}
	if !strings.Contains(fixture.comments.posted[0], "AI による変更のレビューを完了できなかった") {
		t.Fatalf("the comment does not name the step:\n%s", fixture.comments.posted[0])
	}
	// And written down for a second attempt. Held on this side too: the
	// design side's writer was the only one measured, and the two that
	// carry most model failures were free to delete (review of #132).
	if recorded := runner.RecordedFailedStep(runDir); recorded["failed_step"] == "" {
		t.Fatal("the step was not recorded for a second attempt")
	}
}

// The first attempt's comment can be the thing that failed: the row stays
// pending and a later tick posts it. The stage that failed is not in the
// run row and the board that named it is gone by then, so without the
// written record the second attempt posts the sentence that names no step —
// and nothing notices, because the step is not part of the report digest.
func TestAReportPostedOnTheSecondAttemptStillNamesTheStep(t *testing.T) {
	fixture := newPendingFixture(t, "")
	fixture.writeRunDir(t, "example/consumer")
	runDir := runDirectory(fixture.config, fixture.deliveryID)
	if err := os.WriteFile(filepath.Join(runDir, runner.FailedStepFile), []byte("AI による調査と設計"), 0o600); err != nil {
		t.Fatal(err)
	}

	logger := &pendingTestLogger{}
	if err := resubmitPendingTerminal(context.Background(), fixture.config, fixture.services, nil,
		fixture.run, chainViewFor(nil, fixture.deliveryID), logger); err != nil {
		t.Fatal(err)
	}
	if len(fixture.comments.posted) != 1 {
		t.Fatalf("comments posted = %d, want one: %q", len(fixture.comments.posted), fixture.comments.posted)
	}
	posted := fixture.comments.posted[0]
	if !strings.Contains(posted, "AI による調査と設計を完了できなかった") {
		t.Fatalf("the second attempt lost the step:\n%s", posted)
	}
	if strings.Contains(posted, "成果物の生成またはレビューを完了できなかった") {
		t.Fatalf("the second attempt posted the sentence that names no step:\n%s", posted)
	}
}

// The recorded step is the one source that can arrive malformed: the write
// is not atomic and the situation it exists for is a pod that stopped, so a
// file cut mid-rune is exactly the file this finds. A name the report
// refuses costs the requester the whole comment — the report is rejected,
// nothing is posted, and the row retries for ever — where a missing one
// costs only the vaguer sentence (review of #132).
func TestAMalformedRecordCostsTheSentenceNotTheComment(t *testing.T) {
	for _, tc := range []struct {
		name    string
		written string
	}{
		{"a write torn mid-rune", "AI による調査と設" + "\xe8\xa8"},
		{"a trailing newline", "AI による調査と設計\n"},
		{"a carriage return", "AI による\r調査と設計"},
		{"past the bound", strings.Repeat("あ", hook.MaxFailedStepBytes)},
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPendingFixture(t, "")
			fixture.writeRunDir(t, "example/consumer")
			runDir := runDirectory(fixture.config, fixture.deliveryID)
			if err := os.WriteFile(filepath.Join(runDir, runner.FailedStepFile), []byte(tc.written), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := resubmitPendingTerminal(context.Background(), fixture.config, fixture.services, nil,
				fixture.run, chainViewFor(nil, fixture.deliveryID), &pendingTestLogger{}); err != nil {
				t.Fatalf("the report was refused rather than degraded: %v", err)
			}
			if len(fixture.comments.posted) != 1 {
				t.Fatalf("comments posted = %d, want one: the requester got nothing at all", len(fixture.comments.posted))
			}
			if !strings.Contains(fixture.comments.posted[0], "成果物の生成またはレビューを完了できなかった") {
				t.Fatalf("a malformed record was used rather than dropped:\n%s", fixture.comments.posted[0])
			}
		})
	}
}
