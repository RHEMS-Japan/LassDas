package attendant

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"slices"
	"testing"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/worker"
)

func creditSeatConfig(t *testing.T, setup *ladderSetup, design bool) {
	t.Helper()
	var config map[string]any
	if err := json.Unmarshal([]byte(seatedConsumerConfig), &config); err != nil {
		t.Fatal(err)
	}
	if design {
		models := config["models"].(map[string]any)
		models["design_reviewers"] = models["reviewers"]
		agents := config["agents"].(map[string]any)
		agents["design_reviewer_agents"] = agents["reviewer_agents"]
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(setup.config.ConsumerConfigPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCreditFailureMovesAConfiguredReviewSeatBeforeWaiting(t *testing.T) {
	for _, stage := range []string{runtime.StageReviewA, runtime.StageDesignReviewA} {
		t.Run(stage, func(t *testing.T) {
			setup := newLadderSetup(t, runner.StageFailure{
				Stage: stage, Round: 1, Class: runner.FailureClassCredit, Error: "provider refused: insufficient credits",
			})
			creditSeatConfig(t, setup, runtime.IsDesignStage(stage))
			before, err := os.ReadFile(setup.config.ConsumerConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			if runtime.IsDesignStage(stage) {
				var handled bool
				handled, err = handleDesignChainFailure(context.Background(), setup.config, setup.fixture.services, setup.hermes,
					setup.envelope, setup.run(), setup.view, runtime.ChainPlan{Shape: runtime.ShapeDesign}, stage, setup.logger)
				if !handled {
					t.Fatal("the design failure was not handled")
				}
			} else {
				err = handleChainFailure(context.Background(), setup.config, setup.fixture.services, setup.hermes,
					setup.envelope, setup.run(), setup.view, stage, setup.logger)
			}
			if err != nil {
				t.Fatal(err)
			}
			record, moved := seatRecord(t, setup, stage, "review-a")
			if !moved || record.Candidate != 2 || record.MovedTo.Vendor != "Vendor C" || record.PromptRebuilt != "" || record.Reason != "the previous attempt failed as credit" {
				t.Fatalf("the credit refusal did not select the available independent launch: %+v, moved=%v", record, moved)
			}
			models, agents, err := loadSeatConfig(setup.config.ConsumerConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			seat, _ := runner.SeatFor(models, stage)
			occupant, place := runner.SeatOccupantFor(setup.runDir, stage, seat, 1)
			launch, found := agents.ReviewerAgentSeat(seat.ID, place)
			if runtime.IsDesignStage(stage) {
				launch, found = agents.DesignReviewerAgentSeat(seat.ID, place)
			}
			if !found || launch.ID != "judge-a-free" || occupant.Model != "model-c" {
				t.Fatalf("next card would not use the selected launch: %+v, %+v", occupant, launch)
			}
			if !slices.Contains(createdStages(ladderBoardLines(t, setup.calls)), stage) {
				t.Fatal("the next card was not dispatched")
			}
			if len(setup.tracker.added) != 0 || len(setup.fixture.comments.posted) != 0 || setup.fixture.store.begins != 0 {
				t.Fatal("the requester was asked, told to wait, or sent a terminal report before the configured alternative was tried")
			}
			after, err := os.ReadFile(setup.config.ConsumerConfigPath)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("recovery changed the operator's configuration")
			}
		})
	}
}

func TestCreditFailureSpendsCandidatesWithoutShorteningAnUnfundedAsk(t *testing.T) {
	setup := newLadderSetup(t, runner.StageFailure{Stage: runtime.StageReviewA, Round: 1, Class: runner.FailureClassCredit})
	creditSeatConfig(t, setup, false)
	for pass := 0; pass < 4; pass++ {
		if verdict, err := setup.climb(t); verdict != ladderHandled || err != nil {
			t.Fatalf("pass %d: %v, %v", pass, verdict, err)
		}
	}
	if record := setup.record(); !slices.Equal(record.Tried, []string{"seat:2"}) || record.Attempts != 1 || record.LadderStep != rungWait {
		t.Fatalf("credit recovery replayed a spent launch or shortened a prompt instead of waiting: %+v", record)
	}
	if record, _ := seatRecord(t, setup, runtime.StageReviewA, "review-a"); record.PromptRebuilt != "" {
		t.Fatalf("a shorter prompt cannot fund an exhausted key: %+v", record)
	}
	if len(setup.tracker.added) != 1 || setup.fixture.store.begins != 0 {
		t.Fatalf("waiting must not end the delivery or repeat its notice: notices=%d reports=%d", len(setup.tracker.added), setup.fixture.store.begins)
	}
}

func TestCreditRecoveryDoesNotInventALaunchOrCollapseReviewIndependence(t *testing.T) {
	for _, reason := range []string{"missing-launch", "held-vendor", "implementer-has-no-candidate-launch"} {
		t.Run(reason, func(t *testing.T) {
			stage := runtime.StageReviewA
			if reason == "implementer-has-no-candidate-launch" {
				stage = runtime.StageImplement
			}
			setup := newLadderSetup(t, runner.StageFailure{Stage: stage, Round: 1, Class: runner.FailureClassCredit})
			creditSeatConfig(t, setup, false)
			models, agents, err := loadSeatConfig(setup.config.ConsumerConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			switch reason {
			case "missing-launch":
				agents.ReviewerAgents[0].Candidates = nil
			case "held-vendor":
				models.Reviewers[0].Candidates[1].Vendor = models.Reviewers[1].Vendor
			case "implementer-has-no-candidate-launch":
				models.Implementer.Candidates = []worker.ModelEndpoint{{Vendor: "Vendor C", Model: "model-c"}}
			}
			encoded, err := json.Marshal(struct {
				Models worker.ModelConfig `json:"models"`
				Agents worker.AgentSet    `json:"agents"`
			}{models, agents})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(setup.config.ConsumerConfigPath, encoded, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := setup.climb(t); err != nil {
				t.Fatal(err)
			}
			if record := setup.record(); len(record.Tried) != 0 || record.LadderStep != rungWait {
				t.Fatalf("credit recovery invented a usable alternative: %+v", record)
			}
			if len(createdStages(ladderBoardLines(t, setup.calls))) != 0 {
				t.Fatal("an unavailable or clashing seat was dispatched")
			}
		})
	}
}

func TestCreditAfterAnotherFailureDoesNotReplayASpentSeat(t *testing.T) {
	setup := newLadderSetup(t, runner.StageFailure{Stage: runtime.StageReviewA, Round: 1, Class: runner.FailureClassModel})
	creditSeatConfig(t, setup, false)
	if _, err := setup.climb(t); err != nil {
		t.Fatal(err)
	}
	sealCardFailure(t, setup.runDir, runner.StageFailure{Stage: runtime.StageReviewA, Round: 1, Class: runner.FailureClassCredit})
	before := len(createdStages(ladderBoardLines(t, setup.calls)))
	if _, err := setup.climb(t); err != nil {
		t.Fatal(err)
	}
	if record := setup.record(); record.Attempts != 1 || !slices.Equal(record.Tried, []string{"seat:2"}) || record.LadderStep != rungWait {
		t.Fatalf("credit refusal forgot the already tried alternative: %+v", record)
	}
	if len(createdStages(ladderBoardLines(t, setup.calls))) != before {
		t.Fatal("the same failed alternative was immediately dispatched again")
	}
}

func TestCreditRecoveryStillHonoursAStopBeforeDispatch(t *testing.T) {
	setup := newLadderSetup(t, runner.StageFailure{Stage: runtime.StageReviewA, Round: 1, Class: runner.FailureClassCredit})
	creditSeatConfig(t, setup, false)
	setup.config.Tracker.AllowedCreatorID = 7
	setup.tracker.comments = []hook.BacklogComment{{CommentID: 1, UserID: 7, Body: "停止"}}
	if verdict, err := setup.climb(t); verdict != ladderStopped || err != nil {
		t.Fatalf("stop = %v, %v", verdict, err)
	}
	if len(ladderBoardLines(t, setup.calls)) != 0 {
		t.Fatal("recovery changed cards after the stop request")
	}
}
