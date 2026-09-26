package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/worker"
)

func receptionRecoveryFixture(t *testing.T) (string, string, worker.ReadinessDecision) {
	t.Helper()
	return judgedRecoveryFixture(t, false)
}

func judgedRecoveryFixture(t *testing.T, judged bool) (string, string, worker.ReadinessDecision) {
	t.Helper()
	config, err := worker.LoadConfig("../../config/m1-consumer.json")
	if err != nil {
		t.Fatal(err)
	}
	config.Consumers[0].Design = &worker.DesignConfig{Default: worker.DesignDefaultOn}
	if judged {
		config.Models.ReceptionJudge = &worker.ReceptionJudgeConfig{Provider: "TypeSafe", Model: "typesafe/jev-1.13", APIKeyEnv: "MODEL_API_KEY_DECISIONS"}
	}
	dir := t.TempDir()
	configPath := filepath.Join(dir, "consumer.json")
	recoveryJSON(t, configPath, config)
	digest, err := config.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	request := worker.TicketRequest{SchemaVersion: 1, DeliveryID: "delivery_" + strings.Repeat("a", 32),
		InputSHA256: strings.Repeat("b", 64), ConfigSHA256: digest, ToolSHA: strings.Repeat("c", 40),
		IssueKey: "TICKET-4242", RunID: "TICKET-4242", Summary: "Implement the requested behavior",
		Repository: config.Consumers[0].Repository, Mode: config.Consumers[0].Mode.ID,
		Request: "Implement the requested behavior within the permitted scope."}
	source, err := worker.ReadSourceSnapshot(dir, strings.Repeat("d", 40), request, config)
	if err != nil {
		t.Fatal(err)
	}
	recoveryJSON(t, filepath.Join(dir, "readiness-ticket.json"), request)
	recoveryJSON(t, filepath.Join(dir, "readiness-source.json"), source)
	usage := func(endpoint worker.ModelEndpoint) worker.InvocationUsage {
		return worker.InvocationUsage{RequestedModel: endpoint.Model, RequestID: "fixture-" + endpoint.ID,
			StopReason: worker.ChatFinishStop, InputTokens: 10, OutputTokens: 5, TotalTokens: 15}
	}
	when := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
	output := worker.ModelReadinessOutput{
		Decision: "ready", Questions: []worker.ReadinessQuestion{}, Assumptions: []worker.ReadinessAssumption{},
	}
	var judge worker.ReceptionJudge
	if judged {
		output.Decision = "clarification_required"
		output.Questions = []worker.ReadinessQuestion{{ID: "Q1", Dimension: "acceptance_criterion",
			Question: "What should an empty input produce?", WhyBlocking: "The choice changes the result.",
			Choices: []worker.ReadinessChoice{{ID: "a", Label: "Empty list", Effect: "Return an empty list."},
				{ID: "b", Label: "Error", Effect: "Return a validation error."}}, ProposedDefault: "a"}}
		judge = recoveryJudge{}
	}
	assessment, err := worker.NewReadinessAssessment(1, output, nil, nil, source, request, config, usage(config.Models.Readiness.Assessor), when)
	if err != nil {
		t.Fatal(err)
	}
	check, err := worker.NewReadinessCheck(worker.ModelReadinessCheckOutput{Verdict: "pass", Reasons: []worker.ReadinessCheckReason{}},
		assessment, source, request, config, usage(config.Models.Readiness.Checker), when)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := worker.DecideReadiness(t.Context(), []worker.ReadinessAssessment{assessment}, []worker.ReadinessCheck{check}, source, request, config, judge)
	if err != nil {
		t.Fatal(err)
	}
	if !decision.NeedsDesign || decision.Outcome != "ready" {
		t.Fatalf("fixture did not require an accepted design: %+v", decision)
	}
	for name, value := range map[string]any{"assessment-1.json": assessment, "check-1.json": check, "decision.json": decision} {
		recoveryJSON(t, filepath.Join(dir, "history", "readiness", name), value)
	}
	return dir, configPath, decision
}

type recoveryJudge struct{}

func (recoveryJudge) Proceedable(context.Context, string) (worker.ReceptionOpinion, error) {
	return worker.ReceptionOpinion{Model: "typesafe/jev-1.13", Answer: "yes", Confidence: 0.99}, nil
}

func TestReceptionRecoveryDoesNotTakeOverAnUnfinishedGate(t *testing.T) {
	dir := t.TempDir()
	recoveryJSON(t, filepath.Join(dir, "history/readiness/assessment-1.json"), map[string]any{})
	if ReceptionStarted(dir) {
		t.Fatal("an unfinished reception was treated as an accepted handoff")
	}
	for _, outcome := range []string{"clarification_required", "reject", "unresolved"} {
		writeDecision(t, dir, `{"outcome":"`+outcome+`"}`)
		if ReceptionStarted(dir) {
			t.Fatalf("the unaccepted %s gate lost its reception handling", outcome)
		}
	}
	writeDecision(t, dir, "{broken")
	if !ReceptionStarted(dir) {
		t.Fatal("a damaged handoff would reset the reception")
	}
	writeDecision(t, dir, `{"outcome":"clarification_required"}`)
	recoveryJSON(t, filepath.Join(dir, acceptedReceptionFile), map[string]any{})
	if !ReceptionStarted(dir) {
		t.Fatal("a damaged current record overrode an accepted handoff")
	}
}

func TestConcurrentReadersRestoreTheSameAcceptedDecision(t *testing.T) {
	dir, configPath, _ := receptionRecoveryFixture(t)
	if _, err := ChainPlanFromDecision(dir, configPath); err != nil {
		t.Fatal(err)
	}
	writeDecision(t, dir, "{broken")
	errorsSeen := make(chan error, 12)
	for range cap(errorsSeen) {
		go func() {
			plan, err := ChainPlanFromDecision(dir, configPath)
			if err == nil && plan.Shape != runtime.ShapeDesign {
				err = errors.New("concurrent recovery lost the design")
			}
			errorsSeen <- err
		}()
	}
	for range cap(errorsSeen) {
		if err := <-errorsSeen; err != nil {
			t.Fatal(err)
		}
	}
}

func recoveryJSON(t *testing.T, path string, value any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAnAcceptedReceptionSurvivesRepeatedRecordDamage(t *testing.T) {
	dir, configPath, decision := receptionRecoveryFixture(t)
	if plan, err := ChainPlanFromDecision(dir, configPath); err != nil || plan.Shape != runtime.ShapeDesign {
		t.Fatalf("initial accepted plan=%+v %v", plan, err)
	}
	// More than the old single regeneration, without asking the models or
	// requester again, losing the design requirement, or clearing the run.
	for _, damaged := range []string{"{broken", "{}", `{"request_kind":"change","needs_design":false}`} {
		writeDecision(t, dir, damaged)
		plan, err := ChainPlanFromDecision(dir, configPath)
		if err != nil || plan.Shape != runtime.ShapeDesign {
			t.Fatalf("accepted reception was not restored after %q: %+v %v", damaged, plan, err)
		}
		var recovered worker.ReadinessDecision
		if err := worker.ReadJSONFile(filepath.Join(dir, "history/readiness/decision.json"), worker.MaxReadinessJSONBytes, &recovered); err != nil || recovered.DecisionSHA256 != decision.DecisionSHA256 {
			t.Fatalf("the exact accepted decision was not restored: %+v %v", recovered, err)
		}
		retained, err := filepath.Glob(filepath.Join(dir, "history/readiness/decision-damaged-*.json"))
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, path := range retained {
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			found = found || string(body) == damaged
		}
		if !found {
			t.Fatalf("the damaged evidence was not retained: %q", damaged)
		}
	}
}

func TestRecoveryPreservesTheJudgmentThatSettledQuestions(t *testing.T) {
	dir, configPath, want := judgedRecoveryFixture(t, true)
	if want.ReceptionJudgment == nil || len(want.Assumptions) == 0 || len(want.Questions) != 0 {
		t.Fatalf("fixture did not settle a question: %+v", want)
	}
	if _, err := ChainPlanFromDecision(dir, configPath); err != nil {
		t.Fatal(err)
	}
	writeDecision(t, dir, "{broken")
	// The saved judgment, unlike re-running DecideReadiness without the
	// judge, must not send the accepted request back to clarification.
	if plan, err := ChainPlanFromDecision(dir, configPath); err != nil || plan.Shape != runtime.ShapeDesign {
		t.Fatalf("the accepted judged plan was lost: %+v %v", plan, err)
	}
	var got worker.ReadinessDecision
	if err := worker.ReadJSONFile(filepath.Join(dir, "history/readiness/decision.json"), worker.MaxReadinessJSONBytes, &got); err != nil || got.DecisionSHA256 != want.DecisionSHA256 {
		t.Fatalf("the settled assumptions or judgment changed: %+v %v", got, err)
	}
}

func TestRecoveryDoesNotInventALostJudgment(t *testing.T) {
	dir, configPath, _ := judgedRecoveryFixture(t, true)
	writeDecision(t, dir, "{broken") // no checkpoint, only the question-bearing pairs remain
	if _, err := ChainPlanFromDecision(dir, configPath); !errors.Is(err, ErrReadinessDecisionUnreadable) {
		t.Fatalf("a missing judgment was invented or replaced with a new question: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, acceptedReceptionFile)); !os.IsNotExist(err) {
		t.Fatalf("an unproved acceptance was checkpointed: %v", err)
	}
}

func TestRecoveryRefusesForeignAndTamperedRecords(t *testing.T) {
	for _, broken := range []string{"foreign checkpoint", "tampered checkpoint", "tampered check", "tampered source"} {
		t.Run(broken, func(t *testing.T) {
			dir, configPath, want := receptionRecoveryFixture(t)
			writeDecision(t, dir, "{broken")
			switch broken {
			case "foreign checkpoint":
				config, request, _, err := receptionInputs(dir, configPath)
				if err != nil {
					t.Fatal(err)
				}
				request.InputSHA256 = strings.Repeat("e", 64)
				source, err := worker.ReadSourceSnapshot(dir, strings.Repeat("d", 40), request, config)
				if err != nil {
					t.Fatal(err)
				}
				foreign, err := worker.FallbackReadinessDecision(source, request, config, time.Now().UTC())
				if err != nil {
					t.Fatal(err)
				}
				recoveryJSON(t, filepath.Join(dir, acceptedReceptionFile), foreign)
				recoveryJSON(t, filepath.Join(dir, "history/readiness/check-1.json"), map[string]any{})
			case "tampered checkpoint":
				want.NeedsDesign = false
				recoveryJSON(t, filepath.Join(dir, acceptedReceptionFile), want)
				recoveryJSON(t, filepath.Join(dir, "history/readiness/check-1.json"), map[string]any{})
			case "tampered check":
				var check worker.ReadinessCheck
				path := filepath.Join(dir, "history/readiness/check-1.json")
				if err := worker.ReadJSONFile(path, worker.MaxReadinessJSONBytes, &check); err != nil {
					t.Fatal(err)
				}
				check.Verdict = "fail"
				recoveryJSON(t, path, check)
			case "tampered source":
				recoveryJSON(t, filepath.Join(dir, acceptedReceptionFile), want)
				recoveryJSON(t, filepath.Join(dir, "readiness-source.json"), map[string]any{})
			}
			if _, err := ChainPlanFromDecision(dir, configPath); !errors.Is(err, ErrReadinessDecisionUnreadable) {
				t.Fatalf("unsafe reception material was used: %v", err)
			}
		})
	}
}

func TestRecoveryHandlesMissingDirectoriesWithoutFollowingLinks(t *testing.T) {
	for _, linked := range []bool{false, true} {
		name := "missing directory"
		if linked {
			name = "linked directory"
		}
		t.Run(name, func(t *testing.T) {
			dir, configPath, _ := receptionRecoveryFixture(t)
			if _, err := ChainPlanFromDecision(dir, configPath); err != nil {
				t.Fatal(err)
			}
			saved := filepath.Join(t.TempDir(), "history")
			if err := os.Rename(filepath.Join(dir, "history"), saved); err != nil {
				t.Fatal(err)
			}
			if linked {
				if err := os.Symlink(saved, filepath.Join(dir, "history")); err != nil {
					t.Fatal(err)
				}
			}
			plan, err := ChainPlanFromDecision(dir, configPath)
			if linked {
				if !errors.Is(err, ErrReadinessDecisionUnreadable) {
					t.Fatalf("followed a replaced directory: %+v %v", plan, err)
				}
			} else if err != nil || plan.Shape != runtime.ShapeDesign {
				t.Fatalf("missing working directories could not be restored: %+v %v", plan, err)
			}
		})
	}
}

func TestAnUnavailableCheckpointDoesNotDiscardASoundDecision(t *testing.T) {
	dir, configPath, _ := receptionRecoveryFixture(t)
	// A directory in place of the file is deterministic even as root;
	// chmod-only tests would accidentally test successful writes in CI.
	if err := os.Mkdir(filepath.Join(dir, acceptedReceptionFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if plan, err := ChainPlanFromDecision(dir, configPath); err != nil || plan.Shape != runtime.ShapeDesign {
		t.Fatalf("checkpoint failure discarded a sound decision: %+v %v", plan, err)
	}
}

func TestAReceptionCanBeRecoveredFromItsCheckedInputs(t *testing.T) {
	dir, configPath, decision := receptionRecoveryFixture(t)
	// No checkpoint yet: an older run still has its checked input chain.
	writeDecision(t, dir, "{broken")
	plan, err := ChainPlanFromDecision(dir, configPath)
	if err != nil || plan.Shape != runtime.ShapeDesign {
		t.Fatalf("checked reception inputs were not reused: %+v %v", plan, err)
	}
	var recovered worker.ReadinessDecision
	if err := worker.ReadJSONFile(filepath.Join(dir, "history/readiness/decision.json"), worker.MaxReadinessJSONBytes, &recovered); err != nil || recovered.DecisionSHA256 != decision.DecisionSHA256 {
		t.Fatalf("re-derived decision changed: %+v %v", recovered, err)
	}
}

func TestAnEmptyReceptionRecordIsNotPermissionToSkipDesign(t *testing.T) {
	dir := t.TempDir()
	writeDecision(t, dir, "{}")
	if _, err := ChainPlanFromDecision(dir, filepath.Join(dir, "absent.json")); !errors.Is(err, ErrReadinessDecisionUnreadable) {
		t.Fatalf("an empty record selected an implementation chain: %v", err)
	}
}
