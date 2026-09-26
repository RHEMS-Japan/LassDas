package attendant

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
	"automation.internal/ticket-ingress/internal/worker"
)

// An old card or an interrupted recount may leave a refused decision at
// the last permitted round. Recovering that round is not buying another
// implementation round, and its ruling must not be discarded by the cap.
func TestTheLastRoundCanStillRecoverItsArbitration(t *testing.T) {
	for _, round := range []int{2, worker.StageCeiling} {
		for _, saved := range []bool{false, true} {
			t.Run(fmt.Sprintf("round-%d/saved-%v", round, saved), func(t *testing.T) {
				consumer := plainConsumer
				if round == 2 {
					consumer = strings.Replace(consumer, `"max_stages":3`, `"max_stages":3,"max_rounds":2`, 1)
				}
				fixture, config, envelope, _, runDir, _ := stagnantFixture(t, consumer)
				for _, number := range []int{round - 1, round} {
					seedRound(t, runDir, number, fmt.Sprintf("change-%d", number), []worker.ModelFinding{finding("same-objection")})
				}
				stageDir := filepath.Join(runDir, "history", fmt.Sprintf("stage-%d", round))
				writeJSON(t, filepath.Join(stageDir, "decision.json"), map[string]string{"outcome": "revise"})
				if saved {
					writeJSON(t, filepath.Join(stageDir, "ruling.json"), worker.Ruling{
						Stage: round, Ruling: worker.RulingOverruleReviewer, RulingSHA256: strings.Repeat("a", 64),
					})
				}
				var cards []runtime.BoardTask
				for _, stage := range []string{runtime.StageImplement, runtime.StageReviewA, runtime.StageReviewB, runtime.StageValidate, runtime.StagePublish} {
					status := "done"
					if stage == runtime.StageValidate {
						status = "blocked"
					} else if stage == runtime.StagePublish {
						status = "todo"
					}
					cards = append(cards, runtime.BoardTask{ID: "t_" + stage, Status: status,
						IdempotencyKey: runtime.ChainCardKey(fixture.deliveryID, stage, round)})
				}
				view := chainViewFor(cards, fixture.deliveryID)
				hermes, boardLog := fakeBoard(t)
				run := state.RunOverview{DeliveryID: fixture.deliveryID, RunID: "TKT-4242", IssueID: 4242, IssueKey: "TKT-4242"}
				err := handleChainFailure(context.Background(), config, fixture.services, hermes, envelope, run, view,
					runtime.StageValidate, &recordingLogger{})
				if err != nil || fixture.store.begins != 0 {
					t.Fatalf("the last round ended before its arbitration could recover: reports=%d, err=%v", fixture.store.begins, err)
				}
				board, err := os.ReadFile(boardLog)
				if err != nil || !strings.Contains(string(board), runtime.ChainCardKey(fixture.deliveryID, runtime.StageValidate, round)) ||
					strings.Contains(string(board), fmt.Sprintf(":implement:r%d", round+1)) {
					t.Fatalf("the existing round was not recovered within its cap: %v\n%s", err, board)
				}
				if calls, _ := os.ReadFile(config.WorkerBin + ".log"); strings.Contains(string(calls), "arbitrate ") {
					t.Fatal("the attendant performed the model call instead of scheduling the card")
				}
			})
		}
	}
}

func TestTheLastRoundDoesNotRecountAnAppliedRuling(t *testing.T) {
	consumer := strings.Replace(plainConsumer, `"max_stages":3`, `"max_stages":3,"max_rounds":2`, 1)
	fixture, config, envelope, view, runDir, rulingPath := stagnantFixture(t, consumer)
	ruling := worker.Ruling{Stage: 2, Ruling: worker.RulingOverruleReviewer, RulingSHA256: strings.Repeat("a", 64)}
	writeJSON(t, rulingPath, ruling)
	writeJSON(t, filepath.Join(runDir, "history/stage-2/decision.json"), worker.StageDecision{Outcome: "revise", Ruling: &ruling})
	terminal := runner.NewTerminal(config, fixture.services, envelope, chainOwnerRunID(fixture.deliveryID), runDir, &recordingLogger{})
	digest, err := terminal.ReportDigest(context.Background(), hook.TerminalNonconverged, runner.Outcome{Code: hook.TerminalNonconverged}, "")
	if err != nil {
		t.Fatal(err)
	}
	fixture.store.expected = digest
	hermes, boardLog := fakeBoard(t)
	run := state.RunOverview{DeliveryID: fixture.deliveryID, RunID: "TKT-4242", IssueID: 4242, IssueKey: "TKT-4242"}
	if err := handleChainFailure(context.Background(), config, fixture.services, hermes, envelope, run, view,
		runtime.StageValidate, &recordingLogger{}); err != nil {
		t.Fatal(err)
	}
	if calls, _ := os.ReadFile(boardLog); strings.Contains(string(calls), "|create|") || fixture.store.begins != 1 {
		t.Fatal("a ruling already counted into a refused decision caused another card or bypassed the round limit")
	}
}
