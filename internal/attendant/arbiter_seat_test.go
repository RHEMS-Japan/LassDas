package attendant

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/worker"
)

func arbiterSeatConfig(t *testing.T, setup *ladderSetup, fallback bool) worker.ModelEndpoint {
	t.Helper()
	var config map[string]any
	if err := json.Unmarshal([]byte(seatedConsumerConfig), &config); err != nil {
		t.Fatal(err)
	}
	seat := worker.ModelEndpoint{ID: "arbiter", Vendor: "Vendor A", Model: "model-a", BaseURL: "https://first.example/v1", APIKeyEnv: "FIRST_MODEL_KEY",
		Candidates: []worker.ModelEndpoint{{Vendor: "Vendor B", Model: "model-b", BaseURL: "https://second.example/v1", APIKeyEnv: "SECOND_MODEL_KEY"}}}
	models := config["models"].(map[string]any)
	if fallback {
		seat.ID = "assessor"
		models["readiness"] = map[string]any{"assessor": seat}
	} else {
		models["arbiter"] = seat
	}
	writeJSON(t, setup.config.ConsumerConfigPath, config)
	return seat
}

func TestAFailedArbiterTriesConfiguredCandidates(t *testing.T) {
	for _, class := range []runner.FailureClass{runner.FailureClassModel, runner.FailureClassTimeout, runner.FailureClassCredit} {
		for _, fallback := range []bool{false, true} {
			t.Run(string(class)+map[bool]string{false: "/explicit", true: "/assessor"}[fallback], func(t *testing.T) {
				setup := newLadderSetup(t, runner.StageFailure{Stage: runtime.StageValidate, Round: 1, Step: "arbitrate", Class: class})
				seat := arbiterSeatConfig(t, setup, fallback)
				before, err := os.ReadFile(setup.config.ConsumerConfigPath)
				if err != nil {
					t.Fatal(err)
				}
				writeJSON(t, filepath.Join(setup.runDir, "history/stage-1/decision.json"), map[string]string{"outcome": "revise"})
				if err := handleChainFailure(context.Background(), setup.config, setup.fixture.services, setup.hermes,
					setup.envelope, setup.run(), setup.view, runtime.StageValidate, setup.logger); err != nil {
					t.Fatal(err)
				}
				record, moved := seatRecord(t, setup, runtime.StageValidate, seat.ID)
				if !moved || record.Candidate != 1 || record.MovedTo.Model != "model-b" || record.PromptRebuilt != "" {
					t.Fatalf("the configured arbiter alternative was not tried: %+v, %v", record, moved)
				}
				if !slices.Contains(createdStages(ladderBoardLines(t, setup.calls)), runtime.StageValidate) || setup.fixture.store.begins != 0 {
					t.Fatal("the unfinished arbitration was not resumed on its own card")
				}
				after, err := os.ReadFile(setup.config.ConsumerConfigPath)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatal("recovery changed the operator's configuration")
				}
				if _, err := setup.climb(t); err != nil {
					t.Fatal(err)
				}
				if record := setup.record(); record.LadderStep != rungWait || !slices.Equal(record.Tried, []string{"seat:1"}) {
					t.Fatalf("the exhausted arbiter invented a prompt remedy: %+v", record)
				}
				sealCardFailure(t, setup.runDir, runner.StageFailure{Stage: runtime.StageValidate, Round: 1, Step: "arbitrate", Class: runner.FailureClassCredit})
				if _, err := setup.climb(t); err != nil {
					t.Fatal(err)
				}
				if record := setup.record(); record.LadderStep != rungWait || !slices.Equal(record.Tried, []string{"seat:1"}) {
					t.Fatalf("recovery repeated a spent candidate or claimed an unsupported prompt rebuild: %+v", record)
				}
			})
		}
	}
}

func TestArbiterRecoveryHonoursAStopBeforeMovingTheSeat(t *testing.T) {
	setup := newLadderSetup(t, runner.StageFailure{Stage: runtime.StageValidate, Round: 1, Step: "arbitrate", Class: runner.FailureClassModel})
	seat := arbiterSeatConfig(t, setup, false)
	setup.config.Tracker.AllowedCreatorID = 7
	setup.tracker.comments = []hook.BacklogComment{{CommentID: 1, UserID: 7, Body: "停止"}}
	if verdict, err := setup.climb(t); err != nil || verdict != ladderStopped {
		t.Fatalf("the stopped run tried to recover: %v, %v", verdict, err)
	}
	if _, found := seatRecord(t, setup, runtime.StageValidate, seat.ID); found || len(ladderBoardLines(t, setup.calls)) != 0 || setup.record().Attempts != 0 {
		t.Fatal("the stop still allowed a seat or card change")
	}
}

func TestAnOrdinaryValidationFailureDoesNotMoveTheArbiter(t *testing.T) {
	setup := newLadderSetup(t, runner.StageFailure{Stage: runtime.StageValidate, Round: 1, Class: runner.FailureClassModel})
	seat := arbiterSeatConfig(t, setup, false)
	if _, err := setup.climb(t); err != nil {
		t.Fatal(err)
	}
	if _, found := seatRecord(t, setup, runtime.StageValidate, seat.ID); found || len(setup.record().Tried) != 0 {
		t.Fatal("an unrelated validation failure moved the model that did not fail")
	}
}
