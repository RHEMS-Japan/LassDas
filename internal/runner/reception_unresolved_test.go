package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/worker"
)

// The model commands are stand-ins. Decisions and the complete chain use the
// real worker contract; the resulting plan uses the real runner handoff.
func inconclusivePipeline(t *testing.T, exhausted bool, oldLabel string) (*Pipeline, []byte) {
	t.Helper()
	dir, configPath, _ := receptionRecoveryFixture(t)
	config, request, source, err := receptionInputs(dir, configPath)
	if err != nil {
		t.Fatal(err)
	}
	request.Request = "Change only app/feature.go. If the reference is missing, make no edits and report that fact."
	recoveryJSON(t, filepath.Join(dir, "readiness-ticket.json"), request)
	original, err := os.ReadFile(filepath.Join(dir, "readiness-ticket.json"))
	if err != nil {
		t.Fatal(err)
	}
	var assessments []worker.ReadinessAssessment
	var checks []worker.ReadinessCheck
	attempts := 1
	if exhausted {
		attempts = worker.MaxReadinessAttempts
	}
	inputs := filepath.Join(dir, "reader-records")
	for attempt := 1; attempt <= attempts; attempt++ {
		usage := func(endpoint worker.ModelEndpoint) worker.InvocationUsage {
			return worker.InvocationUsage{RequestedModel: endpoint.Model, RequestID: fmt.Sprintf("fixture-%s-%d", endpoint.ID, attempt),
				StopReason: worker.ChatFinishStop, InputTokens: 10, OutputTokens: 5, TotalTokens: 15}
		}
		output := worker.ModelReadinessOutput{Decision: worker.ReadinessAssessorUnresolvable,
			Questions: []worker.ReadinessQuestion{}, Assumptions: []worker.ReadinessAssumption{{
				Kind: "repository_convention", Statement: "An unverified interpretation.", Evidence: "Unconfirmed."}}}
		assessment, err := worker.NewReadinessAssessment(attempt, output, nil, nil, source, request, config,
			usage(config.Models.Readiness.Assessor), time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		checkOutput := worker.ModelReadinessCheckOutput{Verdict: "pass", Reasons: []worker.ReadinessCheckReason{}}
		if exhausted {
			checkOutput.Verdict = "fail"
			checkOutput.Reasons = []worker.ReadinessCheckReason{{Code: "inconsistent-decision", Message: "The reading does not follow from the request."}}
		}
		check, err := worker.NewReadinessCheck(checkOutput, assessment, source, request, config,
			usage(config.Models.Readiness.Checker), time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		assessments, checks = append(assessments, assessment), append(checks, check)
		recoveryJSON(t, filepath.Join(inputs, fmt.Sprintf("assessment-%d.json", attempt)), assessment)
		recoveryJSON(t, filepath.Join(inputs, fmt.Sprintf("check-%d.json", attempt)), check)
	}
	decision, err := worker.DecideReadiness(t.Context(), assessments, checks, source, request, config, nil)
	if err != nil {
		t.Fatal(err)
	}
	if oldLabel != "" {
		decision.Outcome, decision.InconclusiveReading, decision.DecisionSHA256 = oldLabel, false, ""
		raw, err := json.Marshal(decision)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(raw)
		decision.DecisionSHA256 = hex.EncodeToString(digest[:])
	}
	recoveryJSON(t, filepath.Join(inputs, "decision.json"), decision)
	stub := filepath.Join(dir, "stand-in-worker")
	body := `#!/bin/sh
set -eu
verb="$1"; shift
out=""; attempt=""; assessment=""
while [ $# -gt 0 ]; do
 case "$1" in --out) out="$2";; --attempt) attempt="$2";; --assessment) assessment="$2";; esac
 shift
done
inputs='` + inputs + `'
case "$verb" in
 assess-readiness) cp "$inputs/assessment-$attempt.json" "$out";;
 check-readiness) number="${assessment##*-}"; cp "$inputs/check-$number" "$out";;
 decide-readiness) cp "$inputs/decision.json" "$out";;
 *) exit 9;;
esac
`
	if err := os.WriteFile(stub, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	pipeline := receptionPipeline(t, stub)
	pipeline.Workspace, pipeline.Config.ConsumerConfigPath = dir, configPath
	return pipeline, original
}

func TestAnUnresolvedReceptionDoesNotEndTheRequest(t *testing.T) {
	for _, exhausted := range []bool{false, true} {
		for _, label := range []string{"", worker.ReadinessOutcomeUnresolved, "unresolved"} {
			t.Run(fmt.Sprintf("exhausted=%v/old=%s", exhausted, label), func(t *testing.T) {
				pipeline, original := inconclusivePipeline(t, exhausted, label)
				outcome, err := pipeline.readinessGate(t.Context())
				if err != nil || outcome.Code != "" {
					t.Fatalf("an inconclusive but readable reception ended the request: %+v, %v", outcome, err)
				}
				plan, err := ChainPlanFromDecision(pipeline.Workspace, pipeline.Config.ConsumerConfigPath)
				if err != nil || plan.Shape != runtime.ShapeDesign {
					t.Fatalf("the actual card plan lost the required design: %+v, %v", plan, err)
				}
				if got, err := os.ReadFile(pipeline.path("readiness-ticket.json")); err != nil || string(got) != string(original) {
					t.Fatal("the recovery changed the request or its restrictions")
				}
				if len(LoadRecordedDecisions(pipeline.Workspace)) != 1 {
					t.Fatal("the actual gate did not record why it continued")
				}
				pipeline.recordInconclusiveReception()
				notes := LoadRecordedDecisions(pipeline.Workspace)
				if len(notes) != 1 || !strings.Contains(notes[0].Statement, "本文と制約") ||
					strings.Contains(notes[0].Statement, "読める形で答えなかった") || strings.Contains(notes[0].Statement, "読めなかった") {
					t.Fatalf("the recovery needs one truthful note: %+v", notes)
				}
				decided, assumed, _ := receptionAssumptions(pipeline.Workspace, &outcomeNotes{})
				if strings.Contains(strings.Join(append(decided, assumed...), "\n"), "An unverified interpretation.") {
					t.Fatal("an unchecked interpretation was reported as adopted")
				}
				if label != "" {
					retained, _ := filepath.Glob(pipeline.path("history/readiness/decision-damaged-*.json"))
					if len(retained) != 1 {
						t.Fatal("the old decision was not retained")
					}
					before, err := os.ReadFile(pipeline.path("reader-records/decision.json"))
					if err != nil {
						t.Fatal(err)
					}
					after, err := os.ReadFile(retained[0])
					if err != nil || string(before) != string(after) {
						t.Fatal("retaining the old decision changed its evidence")
					}
				}
				writeDecision(t, pipeline.Workspace, "{broken")
				if restored, err := ChainPlanFromDecision(pipeline.Workspace, pipeline.Config.ConsumerConfigPath); err != nil || restored.Shape != plan.Shape {
					t.Fatalf("the accepted inconclusive reading did not survive: %+v, %v", restored, err)
				}
			})
		}
	}
}

func TestAnUnresolvedLabelAloneCannotAuthorizeWork(t *testing.T) {
	pipeline, _ := inconclusivePipeline(t, false, worker.ReadinessOutcomeUnresolved)
	recoveryJSON(t, pipeline.path("reader-records/check-1.json"), map[string]string{"verdict": "pass"})
	outcome, err := pipeline.readinessGate(t.Context())
	if err == nil || outcome.Code == "" {
		t.Fatalf("an incomplete chain authorized work: %+v, %v", outcome, err)
	}
	if len(LoadRecordedDecisions(pipeline.Workspace)) != 0 {
		t.Fatal("a failed recovery claimed it handed off the request")
	}
}
