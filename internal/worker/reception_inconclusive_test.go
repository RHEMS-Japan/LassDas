package worker

import (
	"encoding/json"
	"strings"
	"testing"
)

func inconclusiveFixture(t *testing.T, exhausted bool) (Config, TicketRequest, SourceSnapshot, []ReadinessAssessment, []ReadinessCheck, ReadinessDecision) {
	t.Helper()
	config, request, source := receptionFixture(t, func(c *Config) {
		c.Consumers[0].Design = &DesignConfig{Default: DesignDefaultOn}
	})
	var assessments []ReadinessAssessment
	var checks []ReadinessCheck
	attempts := 1
	if exhausted {
		attempts = MaxReadinessAttempts
	}
	for attempt := 1; attempt <= attempts; attempt++ {
		output := testReadyOutput()
		output.Decision = ReadinessAssessorUnresolvable
		verdict := "pass"
		if exhausted {
			output = testClarificationOutput()
			verdict = "fail"
		}
		assessment, check := testAssessmentPair(t, attempt, output, verdict, source, request, config)
		assessments, checks = append(assessments, assessment), append(checks, check)
	}
	decision, err := DecideReadiness(t.Context(), assessments, checks, source, request, config, nil)
	if err != nil {
		t.Fatal(err)
	}
	return config, request, source, assessments, checks, decision
}

func TestAnInconclusiveReceptionKeepsItsCheckedChainAndDesign(t *testing.T) {
	for _, exhausted := range []bool{false, true} {
		config, request, source, assessments, checks, decision := inconclusiveFixture(t, exhausted)
		if decision.Outcome != ReadinessOutcomeReady || !decision.InconclusiveReading || decision.Fallback {
			t.Fatalf("a readable inconclusive reception ended or discarded its readers: %+v", decision)
		}
		if !decision.NeedsDesign || decision.DesignReason == DesignReasonReceptionUnread || len(decision.Questions) != 0 ||
			len(decision.Assumptions) != 0 || decision.Attempts != len(assessments) {
			t.Fatalf("the handoff weakened its design, invented an interpretation, or lost evidence: %+v", decision)
		}
		if err := decision.Validate(assessments, checks, source, request, config); err != nil {
			t.Fatal(err)
		}
		for index := range assessments {
			if decision.AssessmentSHA256s[index] != assessments[index].AssessmentSHA256 || decision.CheckSHA256s[index] != checks[index].CheckSHA256 {
				t.Fatal("the reader's evidence was discarded")
			}
		}
		// A recovery marker cannot detach the decision from its actual inputs.
		checks[0].AssessmentSHA256 = strings.Repeat("f", 64)
		if err := decision.Validate(assessments, checks, source, request, config); err == nil {
			t.Fatal("an inconclusive reading accepted a tampered chain")
		}
	}
}

func TestAnOldUnresolvedDecisionStillValidatesWithoutChangingItsBytes(t *testing.T) {
	config, request, source, assessments, checks, old := inconclusiveFixture(t, true)
	old.InconclusiveReading = false
	old.Outcome = ReadinessOutcomeUnresolved
	var err error
	old.DecisionSHA256, err = readinessDecisionDigest(old)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(old)
	if strings.Contains(string(before), "inconclusive_reading") {
		t.Fatal("an old decision's encoded fields changed")
	}
	if err := old.Validate(assessments, checks, source, request, config); err != nil {
		t.Fatalf("the old checked artifact became invalid: %v", err)
	}
	after, _ := json.Marshal(old)
	if string(before) != string(after) {
		t.Fatal("validation rewrote old evidence")
	}
}

func TestAnInconclusiveMarkerCannotSuppressACheckedQuestion(t *testing.T) {
	config, request, source := validArtifactFixture(t)
	for _, output := range []ModelReadinessOutput{testClarificationOutput(), testReadyOutput()} {
		assessment, check := testAssessmentPair(t, 1, output, "pass", source, request, config)
		decision, err := DecideReadiness(t.Context(), []ReadinessAssessment{assessment}, []ReadinessCheck{check}, source, request, config, nil)
		if err != nil {
			t.Fatal(err)
		}
		decision.Outcome, decision.InconclusiveReading, decision.Questions = ReadinessOutcomeReady, true, []ReadinessQuestion{}
		decision.DecisionSHA256, err = readinessDecisionDigest(decision)
		if err != nil {
			t.Fatal(err)
		}
		if err := decision.Validate([]ReadinessAssessment{assessment}, []ReadinessCheck{check}, source, request, config); err == nil {
			t.Fatalf("the inconclusive marker invented an unresolved reading over %s", output.Decision)
		}
	}
}

func TestAnInconclusiveMarkerCannotClaimAnUnaskedOrSettledReading(t *testing.T) {
	config, request, source, _, _, original := inconclusiveFixture(t, false)
	for name, forge := range map[string]func(*ReadinessDecision){
		"fallback": func(d *ReadinessDecision) { d.Fallback = true },
		"question": func(d *ReadinessDecision) { d.Questions = testClarificationOutput().Questions },
		"refusal":  func(d *ReadinessDecision) { d.RejectedReading = "out-of-scope" },
		"assumption": func(d *ReadinessDecision) {
			d.Assumptions = []ReadinessAssumption{{Kind: AssumptionDefensibleDefault, Statement: "Invented."}}
		},
		"lost design":    func(d *ReadinessDecision) { d.NeedsDesign = false },
		"foreign source": func(d *ReadinessDecision) { d.SourceSHA256 = strings.Repeat("f", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			decision := original
			forge(&decision)
			var err error
			decision.DecisionSHA256, err = readinessDecisionDigest(decision)
			if err != nil {
				t.Fatal(err)
			}
			if err := decision.ValidateBinding(source, request, config); err == nil {
				t.Fatalf("forged %s was accepted", name)
			}
		})
	}
}
