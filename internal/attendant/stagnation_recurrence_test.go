package attendant

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
	"automation.internal/ticket-ingress/internal/worker"
)

// Registration is requested, added and rejected for exceeding the request,
// then removed and requested again. A second seat can join the complaint,
// so comparing whole sets, even with a longer lookback, misses this loop.
func seedReviewOscillation(t *testing.T, runDir string) {
	t.Helper()
	seedRound(t, runDir, 1, "first change", []worker.ModelFinding{finding("design-wrong")})
	seedRound(t, runDir, 2, "registered change", []worker.ModelFinding{finding("scope-violation"), finding("missing-metrics")})
	seedRound(t, runDir, 3, "different unregistered change", []worker.ModelFinding{finding("design-wrong")})
	writeJSON(t, filepath.Join(runDir, "history", "stage-3", "review-a.json"), worker.Review{
		SchemaVersion: 1, Stage: 3, ReviewerID: "review-a", Verdict: "revise",
		Findings: []worker.ModelFinding{finding("missing-registration")},
	})
}

func TestAResolvedFindingReturningWithOtherFindingsIsStagnation(t *testing.T) {
	runDir := t.TempDir()
	seedReviewOscillation(t, runDir)
	if stagnated(runDir, stagnationReviewers, 2, 1) {
		t.Fatal("two different rounds do not yet establish an oscillation")
	}
	if !stagnated(runDir, stagnationReviewers, 3, 1) {
		t.Fatal("a resolved objection returned, but the review loop was missed")
	}
}

func TestReviewRecurrenceDoesNotConfuseProgressOrUnrelatedFindings(t *testing.T) {
	for _, tc := range []struct {
		name     string
		findings [][]worker.ModelFinding
	}{
		{"retained finding", [][]worker.ModelFinding{{finding("a"), finding("b")}, {finding("a"), finding("c")}, {finding("a"), finding("d")}}},
		{"strict subset", [][]worker.ModelFinding{{finding("a")}, {finding("a"), finding("b")}, {finding("a")}}},
		{"new path", [][]worker.ModelFinding{{finding("a")}, {finding("b")}, {{Code: "a", Path: "docs/OTHER.md", Message: "Another file."}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runDir := t.TempDir()
			for i, findings := range tc.findings {
				seedRound(t, runDir, i+1, fmt.Sprintf("change %d", i), findings)
			}
			if stagnated(runDir, stagnationReviewers, 3, 1) {
				t.Fatal("progress or an unrelated finding was called a recurrence")
			}
		})
	}
	t.Run("different seat", func(t *testing.T) {
		runDir := t.TempDir()
		seedRound(t, runDir, 1, "one", []worker.ModelFinding{finding("a")})
		seedRound(t, runDir, 2, "two", []worker.ModelFinding{finding("b")})
		seedRound(t, runDir, 3, "three", nil)
		writeJSON(t, filepath.Join(runDir, "history", "stage-3", "review-a.json"), worker.Review{
			SchemaVersion: 1, Stage: 3, ReviewerID: "review-a", Verdict: "revise", Findings: []worker.ModelFinding{finding("a")},
		})
		if stagnated(runDir, stagnationReviewers, 3, 1) {
			t.Fatal("findings from different seats were treated as the same finding")
		}
	})
}

func TestReviewRecurrenceCountsReturnsNotConsecutivePresence(t *testing.T) {
	runDir := t.TempDir()
	for i, code := range []string{"a", "b", "a", "b", "a"} {
		seedRound(t, runDir, i+1, fmt.Sprintf("change %d", i), []worker.ModelFinding{finding(code)})
	}
	if stagnated(runDir, stagnationReviewers, 3, 2) || stagnated(runDir, stagnationReviewers, 4, 2) {
		t.Fatal("one return was counted as two")
	}
	if !stagnated(runDir, stagnationReviewers, 5, 2) {
		t.Fatal("two returns of the same objection were not recognised")
	}
}

func TestAnUnreadableInterveningRoundDoesNotProveAReviewCycle(t *testing.T) {
	for _, missing := range []string{"review-b.json", "candidate.json"} {
		t.Run(missing, func(t *testing.T) {
			runDir := t.TempDir()
			seedReviewOscillation(t, runDir)
			if err := os.Remove(filepath.Join(runDir, "history", "stage-2", missing)); err != nil {
				t.Fatal(err)
			}
			if stagnated(runDir, stagnationReviewers, 3, 1) {
				t.Fatal("a missing round was treated as proof that a finding disappeared")
			}
		})
	}
	for _, body := range []string{
		`null`,
		`{}`,
		`{"stage":1,"reviewer_id":"review-b","verdict":"pass"}`,
		`{"stage":2,"reviewer_id":"review-a","verdict":"pass"}`,
		`{"stage":2,"reviewer_id":"review-b","verdict":"unknown"}`,
	} {
		t.Run(body, func(t *testing.T) {
			runDir := t.TempDir()
			seedReviewOscillation(t, runDir)
			if err := os.WriteFile(filepath.Join(runDir, "history", "stage-2", "review-b.json"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if stagnated(runDir, stagnationReviewers, 3, 1) {
				t.Fatal("an incomplete or unrelated review was treated as an observed absence")
			}
		})
	}
}

func TestReturningToTheSameRejectedBytesIsStagnation(t *testing.T) {
	runDir := t.TempDir()
	pipeline := &runner.Pipeline{Workspace: runDir, Logger: &recordingLogger{}}
	for i, content := range []string{"first", "second", "first"} {
		seedRound(t, runDir, i+1, content, nil)
		pipeline.SealValidationFailure(i+1, "run-validation", "--- FAIL: TestBehavior\n")
	}
	if !stagnated(runDir, stagnationReviewers, 3, 1) {
		t.Fatal("returning to identical bytes and validation failure was missed")
	}
	if err := os.Remove(runner.ValidationFailureFile(runDir, 3)); err != nil {
		t.Fatal(err)
	}
	pipeline.SealValidationFailure(3, "run-validation", "--- FAIL: TestAnotherBehavior\n")
	if stagnated(runDir, stagnationReviewers, 3, 1) {
		t.Fatal("a different validation result was treated as the same rejected change")
	}
}

func TestAnOscillatingReviewIsRuledOnAndTheNextAttemptReceivesTheRuling(t *testing.T) {
	fixture, config, envelope, _, runDir, _ := stagnantFixture(t, plainConsumer)
	seedReviewOscillation(t, runDir)
	writeJSON(t, filepath.Join(runDir, "history", "stage-3", "decision.json"), map[string]string{"outcome": "revise"})
	script, err := os.ReadFile(config.WorkerBin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.WorkerBin, []byte(strings.Replace(string(script), `"stage":2`, `"stage":3`, 1)), 0o700); err != nil {
		t.Fatal(err)
	}
	card := func(id, stage, status string) runtime.BoardTask {
		return runtime.BoardTask{ID: id, Status: status, IdempotencyKey: runtime.ChainCardKey(fixture.deliveryID, stage, 3)}
	}
	view := chainViewFor([]runtime.BoardTask{
		card("t_impl", runtime.StageImplement, "done"),
		card("t_ra", runtime.StageReviewA, "done"),
		card("t_rb", runtime.StageReviewB, "done"),
		card("t_v", runtime.StageValidate, "blocked"),
		card("t_p", runtime.StagePublish, "todo"),
	}, fixture.deliveryID)
	hermes, boardLog := fakeBoard(t)
	run := state.RunOverview{DeliveryID: fixture.deliveryID, RunID: "TKT-4242", IssueID: 4242, IssueKey: "TKT-4242"}
	if err := handleChainFailure(context.Background(), config, fixture.services, hermes, envelope, run, view,
		runtime.StageValidate, &recordingLogger{}); err != nil {
		t.Fatal(err)
	}
	if len(fixture.comments.posted) != 0 || len(fixture.store.digests) != 0 {
		t.Fatal("the review loop asked the requester or ended the delivery")
	}
	calls, err := os.ReadFile(config.WorkerBin + ".log")
	if err != nil {
		t.Fatal(err)
	}
	rulingPath := runner.RulingFile(runDir, 3)
	if !strings.Contains(string(calls), "arbitrate ") || !strings.Contains(string(calls), "--ruling "+rulingPath) {
		t.Fatalf("the next attempt did not receive a ruling on the oscillation:\n%s", calls)
	}
	arbitrated := ""
	for _, line := range strings.Split(string(calls), "\n") {
		if strings.HasPrefix(line, "arbitrate ") {
			arbitrated = line
		}
	}
	if !strings.Contains(arbitrated, "--history "+filepath.Join(runDir, "history")) {
		t.Fatalf("the arbiter was not given the earlier attempts: %s", arbitrated)
	}
	board, err := os.ReadFile(boardLog)
	if err != nil || !strings.Contains(string(board), fixture.deliveryID+":implement:r4") {
		t.Fatalf("the next implementation attempt was not dispatched: %v\n%s", err, board)
	}
}
