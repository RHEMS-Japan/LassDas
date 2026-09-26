package worker

import (
	"context"
	"strings"
	"testing"
)

// The engine has no entrance that turns a request away for what it says. A
// request is refused only when it cannot be processed at all — empty,
// oversized, not valid UTF-8, carrying control characters — and those are
// checked on the input itself, before any model sees it.
//
// Measured live (2026-09-26): a reader answered "reject" on a request naming a
// specification document the destination's repository does not have, with no
// question and no reason recorded. The requester was told the ticket "did not
// meet the reception conditions", an operator was named as the next person to
// act, and neither of them was left anything to do. Every test here fails
// without the mapping.

// rejectingAssessor is the answer a reader gives when it refuses: the refusal,
// the word it refused with, and whatever questions it drafted alongside.
func rejectingAssessor(rejectCode string, questions string) string {
	return `{"decision":"reject","questions":[` + questions + `],"assumptions":[],"reject_code":"` + rejectCode + `",` +
		`"request_kind":"change","approach_in_ticket":false,"approach_excerpt":"","needs_design":true}`
}

// oneDraftedQuestion is a question in the shape the reader drafts them, with
// the bounded choices the contract requires.
const oneDraftedQuestion = `{"id":"Q1","dimension":"preapproved_scope_choice",` +
	`"question":"仕様書が見つかりませんが、どちらで進めますか","why_blocking":"どちらを選ぶかで画面の文言が変わります",` +
	`"choices":[{"id":"a","label":"既存の文言に合わせる","effect":"画面の文言は今のまま変わりません"},` +
	`{"id":"b","label":"依頼の文言をそのまま使う","effect":"画面に依頼に書かれた文言が出ます"}],"proposed_default":"a"}`

// decisionFor runs one reader answer through the whole gate and returns the
// sealed decision.
func decisionFor(t *testing.T, answer string) (ReadinessDecision, TicketRequest, SourceSnapshot, Config) {
	t.Helper()
	config, request, source := validArtifactFixture(t)
	api := &scriptedChatAPI{responses: []scriptedResponse{
		{text: answer, requestID: "request-assessor"},
		{text: `{"verdict":"pass","reasons":[],"request_kind":"change","needs_design":true}`, requestID: "request-checker"},
	}}
	invoker, err := NewModelInvoker(api)
	if err != nil {
		t.Fatal(err)
	}
	assessment, _, err := invoker.AssessReadiness(context.Background(), 1, nil, nil, nil, nil, source, request, config, nil)
	if err != nil {
		t.Fatalf("AssessReadiness() error = %v", err)
	}
	check, _, err := invoker.CheckReadiness(context.Background(), assessment, nil, nil, source, request, config, nil)
	if err != nil {
		t.Fatalf("CheckReadiness() error = %v", err)
	}
	decision, err := DecideReadiness(t.Context(), []ReadinessAssessment{assessment}, []ReadinessCheck{check}, source, request, config, nil)
	if err != nil {
		t.Fatalf("DecideReadiness() error = %v", err)
	}
	return decision, request, source, config
}

// TestAReaderThatRefusesAndAsksIsAsking: a refusal that drafted questions has
// found something worth asking, and the single round of questions is what that
// is for. Nothing the reader wrote is lost.
func TestAReaderThatRefusesAndAsksIsAsking(t *testing.T) {
	decision, request, source, config := decisionFor(t, rejectingAssessor("out-of-scope", oneDraftedQuestion))

	if decision.Outcome != ReadinessOutcomeClarification {
		t.Fatalf("outcome = %q, want the question path", decision.Outcome)
	}
	if len(decision.Questions) != 1 {
		t.Fatalf("questions = %+v, want the one the reader drafted", decision.Questions)
	}
	question := decision.Questions[0]
	if question.ID != "Q1" || !strings.Contains(question.Question, "仕様書が見つかりません") ||
		len(question.Choices) != 2 || question.Choices[0].ID != "a" || question.ProposedDefault != "a" {
		t.Fatalf("the question the reader drafted was not carried whole: %+v", question)
	}
	// The word it refused with travels beside the outcome, so an operator can
	// read what it balked at even though the requester is being asked.
	if decision.RejectedReading != "out-of-scope" {
		t.Fatalf("rejected reading = %q, want the reader's own word", decision.RejectedReading)
	}
	if decision.RejectCode != "" {
		t.Fatalf("a refusal is not an outcome, so no reject code is sealed: %q", decision.RejectCode)
	}
	if err := decision.Validate([]ReadinessAssessment{}, []ReadinessCheck{}, source, request, config); err == nil {
		t.Fatal("the sealed decision must still be held to its own chain")
	}
}

// TestAReaderThatRefusesAndAsksNothingDoesNotTurnTheRequestAway is the
// measured ending removed: the request goes on as written, and the word the
// reader balked at is sealed so both the requester's comment and an operator
// can read it.
func TestAReaderThatRefusesAndAsksNothingDoesNotTurnTheRequestAway(t *testing.T) {
	decision, request, source, config := decisionFor(t, rejectingAssessor("out-of-scope", ""))

	if decision.Outcome != ReadinessOutcomeReady {
		t.Fatalf("outcome = %q, want the request to go on", decision.Outcome)
	}
	if len(decision.Questions) != 0 {
		t.Fatalf("a reader that drafted nothing leaves nothing to ask: %+v", decision.Questions)
	}
	if decision.RejectedReading != "out-of-scope" {
		t.Fatalf("rejected reading = %q, want the reader's own word", decision.RejectedReading)
	}
	// Ready and a change with no design: the implementer stage is what this
	// decision dispatches, and it reads the destination's repository itself.
	if decision.RequestKind != RequestKindChange {
		t.Fatalf("request kind = %q", decision.RequestKind)
	}
	if err := decision.ValidateBinding(source, request, config); err != nil {
		t.Fatalf("the sealed decision must revalidate: %v", err)
	}
}

// TestTheReadersRefusalIsNeverTheSealedOutcome is the pin the mutation aims
// at: whatever the reader answers, the outcome the gate seals is one the
// engine can act on.
func TestTheReadersRefusalIsNeverTheSealedOutcome(t *testing.T) {
	answers := map[string]string{
		"a refusal with a word and no question": rejectingAssessor("out-of-scope", ""),
		"a refusal with a question":             rejectingAssessor("out-of-scope", oneDraftedQuestion),
		"a refusal with another word":           rejectingAssessor("needs-governance", ""),
	}
	// The mapping itself, asserted where it is made and before the gate below.
	// Going through the whole gate also proves it, but by way of the
	// decision's content rules refusing a rejection they were never going to
	// be handed — which fails for the wrong reason and reads as a different
	// defect, so the plain statement comes first.
	for _, refusal := range []ReadinessAssessment{
		{Decision: ReadinessOutcomeReject, RejectCode: "out-of-scope"},
		{Decision: ReadinessOutcomeReject, RejectCode: "needs-governance"},
		{Decision: ReadinessOutcomeReject, RejectCode: "out-of-scope", Questions: testClarificationOutput().Questions},
	} {
		outcome, questions, reading := sealedReceptionOutcome(refusal)
		if outcome == ReadinessOutcomeReject {
			t.Errorf("a refusal carrying %d questions was mapped to a rejection", len(refusal.Questions))
		}
		if reading != refusal.RejectCode {
			t.Errorf("reading = %q, want the reader's own word %q", reading, refusal.RejectCode)
		}
		if len(questions) != len(refusal.Questions) {
			t.Errorf("questions = %d, want the %d the reader drafted", len(questions), len(refusal.Questions))
		}
	}
	// And the two outcomes it does pass through unchanged.
	for _, kept := range []ReadinessAssessment{
		{Decision: ReadinessOutcomeReady},
		{Decision: ReadinessOutcomeClarification, Questions: testClarificationOutput().Questions},
	} {
		outcome, _, reading := sealedReceptionOutcome(kept)
		if outcome != kept.Decision || reading != "" {
			t.Errorf("a %q reading was changed to %q/%q", kept.Decision, outcome, reading)
		}
	}
	// An assessor that could not bound the ambiguity is still the operator's
	// to rework, and is not a refusal of the request's content.
	if outcome, _, _ := sealedReceptionOutcome(ReadinessAssessment{Decision: ReadinessAssessorUnresolvable}); outcome != ReadinessOutcomeUnresolved {
		t.Errorf("an unresolvable reading was mapped to %q", outcome)
	}
	// And the same through the whole gate, so the seal agrees with the map.
	for name, answer := range answers {
		decision, _, _, _ := decisionFor(t, answer)
		if decision.Outcome == ReadinessOutcomeReject {
			t.Errorf("%s reached the sealed outcome as a rejection", name)
		}
		if decision.RejectCode != "" {
			t.Errorf("%s sealed a reject code: %q", name, decision.RejectCode)
		}
		if decision.RejectedReading == "" {
			t.Errorf("%s lost the word the reader refused with", name)
		}
	}
}

// A decision's refusal is bound to the actual assessment, not to an alphabet.
// A different sentence is still forged even though both sentences are readable.
func TestAWordNoReaderWroteIsRefused(t *testing.T) {
	config, request, source := validArtifactFixture(t)
	assessment, check := receptionPair(t, 1, ModelReadinessOutput{
		Decision: ReadinessOutcomeReject, RejectCode: "out-of-scope",
	}, "pass", source, request, config)
	decision, err := DecideReadiness(t.Context(), []ReadinessAssessment{assessment}, []ReadinessCheck{check}, source, request, config, nil)
	if err != nil {
		t.Fatal(err)
	}

	forged := decision
	forged.RejectedReading = "運用担当者が確認します"
	digest, err := readinessDecisionDigest(forged)
	if err != nil {
		t.Fatal(err)
	}
	forged.DecisionSHA256 = digest
	if err := forged.Validate([]ReadinessAssessment{assessment}, []ReadinessCheck{check}, source, request, config); err == nil {
		t.Fatal("a rejected reading no assessment carried was accepted")
	}
}

// TestAGateDecidedWithoutItsReadersRefusesNothing: the two paths must not be
// able to claim each other. A gate decided because no reader answered has no
// reader's word to carry.
func TestAGateDecidedWithoutItsReadersRefusesNothing(t *testing.T) {
	config, request, source := validArtifactFixture(t)
	honest, err := FallbackReadinessDecision(source, request, config, testInvocationTime)
	if err != nil {
		t.Fatal(err)
	}
	forged := honest
	forged.RejectedReading = "out-of-scope"
	digest, err := readinessDecisionDigest(forged)
	if err != nil {
		t.Fatal(err)
	}
	forged.DecisionSHA256 = digest
	if err := forged.ValidateBinding(source, request, config); err == nil {
		t.Fatal("a gate decided without its readers carried a word one of them refused with")
	}
}

// TestADestinationThatAsksNothingIsNotAskedThroughARefusal closes the hole the
// mapping would otherwise open. A refusal that drafted questions becomes the
// question round, so the destination's own "ask nobody anything" has to reach
// the refusal too — or a destination that turned questions off would be asked
// one through the single answer the policy never looked at.
func TestADestinationThatAsksNothingIsNotAskedThroughARefusal(t *testing.T) {
	config, request, source := receptionFixture(t, func(c *Config) { c.Questions = QuestionsNone })

	refused := testClarificationOutput()
	refused.Decision = ReadinessOutcomeReject
	refused.RejectCode = "out-of-scope"
	assessment, check := receptionPair(t, 1, refused, "pass", source, request, config)

	// The refusal stays on the record, because it is what the reading said and
	// the word it balked at is sealed from it. What is gone is the question.
	if assessment.Decision != ReadinessOutcomeReject || len(assessment.Questions) != 0 {
		t.Fatalf("assessment = %s with %d questions, want the refusal with none",
			assessment.Decision, len(assessment.Questions))
	}
	if len(assessment.Assumptions) != 1 || assessment.Assumptions[0].Kind != AssumptionDefensibleDefault {
		t.Fatalf("assumptions = %+v, want the question recorded as a decision instead", assessment.Assumptions)
	}
	if !strings.Contains(assessment.Assumptions[0].Statement, refused.Questions[0].Question) {
		t.Fatalf("the recorded decision does not say what the point was: %q", assessment.Assumptions[0].Statement)
	}

	decision, err := DecideReadiness(t.Context(), []ReadinessAssessment{assessment}, []ReadinessCheck{check}, source, request, config, nil)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Outcome != ReadinessOutcomeReady || len(decision.Questions) != 0 {
		t.Fatalf("decision = %s with %d questions, want ready with none", decision.Outcome, len(decision.Questions))
	}
	if decision.RejectedReading != "out-of-scope" {
		t.Fatalf("rejected reading = %q, want the reader's own word", decision.RejectedReading)
	}
}
