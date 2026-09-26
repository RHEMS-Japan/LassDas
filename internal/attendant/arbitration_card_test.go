package attendant

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
	"automation.internal/ticket-ingress/internal/worker"
)

func TestStagnationSchedulesExistingValidateCardWithoutAModelCall(t *testing.T) {
	fixture, config, envelope, view, _, rulingPath := stagnantFixture(t, plainConsumer)
	hermes, boardLog := fakeBoard(t)
	run := state.RunOverview{DeliveryID: fixture.deliveryID, RunID: "TKT-4242", IssueID: 4242, IssueKey: "TKT-4242"}
	if err := handleChainFailure(context.Background(), config, fixture.services, hermes, envelope, run, view,
		runtime.StageValidate, &recordingLogger{}); err != nil {
		t.Fatal(err)
	}
	calls, _ := os.ReadFile(config.WorkerBin + ".log")
	if strings.Contains(string(calls), "arbitrate ") {
		t.Fatal("the attendant still calls the model while holding the chain tick")
	}
	if _, err := os.Stat(rulingPath); !os.IsNotExist(err) {
		t.Fatalf("scheduling manufactured a ruling: %v", err)
	}
	board, err := os.ReadFile(boardLog)
	if err != nil || !strings.Contains(string(board), fixture.deliveryID+":validate:r2") || strings.Contains(string(board), ":implement:r3") {
		t.Fatalf("the same validation card was not handed the arbitration: %v\n%s", err, board)
	}
	if fixture.store.begins != 0 || len(fixture.comments.posted) != 0 {
		t.Fatal("scheduling asked the requester or ended the delivery")
	}
}

func TestAStopIsAcknowledgedWhileTheArbiterIsStillRunning(t *testing.T) {
	_, cardConfig, _, _, _, _ := stagnantFixture(t, plainConsumer)
	h := newDepthHarness(t, "production", true, "")
	unpublished(t, h)
	runningChainCard(t, h, runtime.StageValidate)
	board, err := os.ReadFile(h.boardFile)
	if err != nil {
		t.Fatal(err)
	}
	var cards []runtime.BoardTask
	if err := json.Unmarshal(board, &cards); err != nil {
		t.Fatal(err)
	}
	for i := range cards {
		cards[i].IdempotencyKey = strings.TrimSuffix(cards[i].IdempotencyKey, ":r1") + ":r2"
	}
	writeJSON(t, h.boardFile, cards)
	for round := 1; round <= 2; round++ {
		seedRound(t, h.runDir, round, "unchanged", []worker.ModelFinding{finding("missing-behaviour")})
	}
	writeJSON(t, filepath.Join(h.runDir, "history/stage-2/decision.json"), map[string]string{"outcome": "revise"})
	writeJSON(t, filepath.Join(h.runDir, "baseline.json"), map[string]any{"baseline": map[string]any{"Integration": map[string]string{"SHA": strings.Repeat("a", 40)}}})
	script, err := os.ReadFile(cardConfig.WorkerBin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cardConfig.WorkerBin, []byte(strings.Replace(string(script), "  arbitrate)", "  arbitrate) exec sleep 30 ;;\n  unused)", 1)), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	pipeline := &runner.Pipeline{Config: cardConfig, Workspace: h.runDir, Logger: &recordingLogger{}}
	go func() { finished <- pipeline.RunChainStage(ctx, runtime.StageValidate) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("the fixture card failed to exit after cancellation")
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if calls, _ := os.ReadFile(cardConfig.WorkerBin + ".log"); strings.Contains(string(calls), "arbitrate ") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the validation card never started arbitration")
		}
		time.Sleep(10 * time.Millisecond)
	}
	*h.comments = append(*h.comments, ticketComment(stopCommentID, depthRequester, "停止"))
	h.tick()
	if row := h.runRow(); row.State != "terminal" || row.TerminalCode != string(hook.TerminalCancelled) || stopAcknowledgements(*h.posted) != 1 {
		t.Fatalf("the stop waited for the arbiter instead of one tick: %+v", row)
	}
	if !strings.Contains(h.calls(), "|archive|chain_validate|") || strings.Contains(h.calls(), "|create|") {
		t.Fatalf("the running card was not retired without new work:\n%s", h.calls())
	}
	select {
	case err := <-finished:
		// Put it back so cleanup can collect it, even on failure.
		finished <- err
		t.Fatalf("arbitration had already ended; this did not measure an in-flight stop: %v", err)
	default:
	}
	// The fake kanban records archive but does not run a supervisor. Model
	// cancellation is exercised explicitly after verifying the stop/retire.
	cancel()
	select {
	case err := <-finished:
		finished <- err
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the card did not preserve its cancellation: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled arbiter kept running")
	}
}

func TestAnUnfinishedArbiterRecoversTheCardWithoutRepeatingImplementation(t *testing.T) {
	setup := newLadderSetup(t, runner.StageFailure{Stage: runtime.StageValidate, Round: 1, Step: "arbitrate", Class: runner.FailureClassModel})
	writeJSON(t, filepath.Join(setup.runDir, "history/stage-1/decision.json"), map[string]string{"outcome": "revise"})
	setup.config.Chain.RetryBackoffBaseSeconds = 1
	for pass := 0; pass < 2; pass++ {
		if err := handleChainFailure(context.Background(), setup.config, setup.fixture.services, setup.hermes,
			setup.envelope, setup.run(), setup.view, runtime.StageValidate, setup.logger); err != nil {
			t.Fatal(err)
		}
		record := setup.record()
		if record.LadderStep != rungWait {
			t.Fatalf("an unfinished ruling was not recovered: %+v", record)
		}
		record.LastAt = time.Now().Add(-time.Hour)
		writeLadderRecord(setup.runDir, runtime.StageValidate, 1, record, setup.logger)
	}
	calls, _ := os.ReadFile(setup.calls)
	if !strings.Contains(string(calls), ":validate:r1") || strings.Contains(string(calls), ":implement:r2") || strings.Contains(string(calls), "|archive|t_impl|") {
		t.Fatalf("recovery repeated implementation instead of the ruling:\n%s", calls)
	}
	if setup.fixture.store.begins != 0 {
		t.Fatal("an arbiter failure ended the delivery")
	}
}
