package attendant

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
)

// sealFailureRecord puts one card's own account of its failure in the run
// directory, the way a stage card leaves it. The earlier rounds are given
// their decisions because the card derives its round from them, never from an
// argument.
func sealFailureRecord(t *testing.T, runDir, stage string, round int, failure error) {
	t.Helper()
	directory := "stage"
	if runtime.IsDesignStage(stage) {
		directory = "design"
	}
	for earlier := 1; earlier <= round; earlier++ {
		roundDir := filepath.Join(runDir, "history", fmt.Sprintf("%s-%d", directory, earlier))
		if err := os.MkdirAll(roundDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if earlier == round {
			break
		}
		if err := os.WriteFile(filepath.Join(roundDir, "decision.json"), []byte(`{"outcome":"revise"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pipeline := &runner.Pipeline{Workspace: runDir, Logger: &recordingLogger{}}
	pipeline.SealStageFailure(stage, failure)
	if _, ok := runner.ReadStageFailure(runDir, stage, round); !ok {
		t.Fatalf("no record sealed for %s round %d", stage, round)
	}
}

// The class the tick logs comes from the card itself, on either half of the
// chain, and says so plainly when the card left nothing to read — a card
// dispatched by an older engine, or one that died before it could write.
func TestFailedCardClassReadsWhatTheCardSaidOrSaysNone(t *testing.T) {
	runDir := t.TempDir()
	view := chainView{round: 2, designRound: 1}
	if got := failedCardClass(runDir, runtime.StageReviewA, view); got != "none" {
		t.Fatalf("class without a record = %q", got)
	}
	if got := failedCardClass(runDir, runtime.StageInvestigate, view); got != "none" {
		t.Fatalf("design class without a record = %q", got)
	}
	sealFailureRecord(t, runDir, runtime.StageReviewA, 2, runner.ErrValidationRejected)
	if got := failedCardClass(runDir, runtime.StageReviewA, view); got != string(runner.FailureClassValidation) {
		t.Fatalf("class = %q", got)
	}
	// The design half counts its own rounds; a design stage read against the
	// implementation round would find nothing.
	sealFailureRecord(t, runDir, runtime.StageInvestigate, 1, errors.New("nothing in particular"))
	if got := failedCardClass(runDir, runtime.StageInvestigate, view); got != string(runner.FailureClassUnknown) {
		t.Fatalf("design class = %q", got)
	}
}

// The classification the run actually ends on is the sealed artifacts, and it
// is exactly what it was: a card's own account of what kind of thing went
// wrong is evidence for later, and changes no ending today.
func TestClassifyChainFailureIgnoresTheCardsOwnAccount(t *testing.T) {
	runDir := t.TempDir()
	undecided := func() (string, error) { return "", errors.New("missing") }
	converged := func() (string, error) { return "converged", nil }
	unchanged := func() bool { return false }

	type expectation struct {
		stage    string
		decision func() (string, error)
		action   failureAction
		code     hook.TerminalCode
	}
	expectations := []expectation{
		{runtime.StageImplement, undecided, actionReport, hook.TerminalModelFailed},
		{runtime.StagePublish, undecided, actionReport, hook.TerminalReleaseFailed},
		{runtime.StageValidate, converged, actionRegenerate, hook.TerminalModelFailed},
		{runtime.StageReviewA, undecided, actionReport, hook.TerminalModelFailed},
	}
	for _, sealed := range []bool{false, true} {
		if sealed {
			// Every class the vocabulary has, sealed against the very rounds
			// the classification reads, so a reading of it would show.
			sealFailureRecord(t, runDir, runtime.StageImplement, 1, errors.New("nothing in particular"))
			sealFailureRecord(t, runDir, runtime.StagePublish, 1, runner.ErrValidationRejected)
			sealFailureRecord(t, runDir, runtime.StageValidate, 1, runner.ErrValidationRejected)
			sealFailureRecord(t, runDir, runtime.StageReviewA, 1, runner.ErrValidationRejected)
		}
		for _, want := range expectations {
			action, code := classifyChainFailure(want.stage, want.decision, undecided, unchanged)
			if action != want.action || code != want.code {
				t.Fatalf("classifyChainFailure(%s) with sealed=%v = %v %v, want %v %v",
					want.stage, sealed, action, code, want.action, want.code)
			}
		}
	}
}
