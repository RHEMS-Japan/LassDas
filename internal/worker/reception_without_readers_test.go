package worker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// unreadableAnswers is the same unusable answer as many times as the turn
// will ask. Prose with no JSON value in it is the one thing the tolerant
// reading cannot rescue, so it is what the fallback has to be tested with.
func unreadableAnswers(times int) *sequenceChatAPI {
	outputs := make([]*ChatResponse, 0, times)
	for range times {
		outputs = append(outputs, chatOutput("I am not able to determine what this ticket asks for."))
	}
	return &sequenceChatAPI{outputs: outputs}
}

// TestAnIntakeWhoseEveryAnswerIsUnreadableReadsTheTicketItself is the
// measured ending removed: three requests died at the reception inside two
// minutes because the reader's answers were refused. The request is now the
// ticket's own words and the delivery goes on.
func TestAnIntakeWhoseEveryAnswerIsUnreadableReadsTheTicketItself(t *testing.T) {
	config := validTestConfig()
	raw, err := ReadRawTicket(validTicketEnvelope(t, realTicketDescription()), config, strings.Repeat("c", 40))
	if err != nil {
		t.Fatal(err)
	}
	api := unreadableAnswers(modelAnswerAttempts)
	invoker, _ := NewModelInvoker(api)

	intake, usage, err := invoker.ReadContract(context.Background(), raw, config)
	if err != nil {
		t.Fatalf("an intake whose every answer was unreadable must not fail: %v", err)
	}
	if len(api.requests) != modelAnswerAttempts {
		t.Fatalf("the reader was asked %d times, want %d before the engine read it itself", len(api.requests), modelAnswerAttempts)
	}
	if !intake.Fallback {
		t.Fatal("the reading must say it was made without the model")
	}
	if intake.Request != strings.TrimSpace(raw.Description) {
		t.Fatalf("request = %q, want the ticket's own text", intake.Request)
	}
	if !intake.Complete() {
		t.Fatalf("a single configured destination leaves nothing to ask about, got %+v", intake.Gaps)
	}
	if intake.VerificationPath != "" || intake.ExpectedText != "" || intake.AbsentText != "" {
		t.Fatalf("nothing may be read out of the ticket without the model: %+v", intake)
	}
	// The one thing such a reading says for itself, where the plan notice
	// shows how the request was read.
	if !strings.Contains(intake.Rationale, "チケットの本文をそのまま") {
		t.Fatalf("rationale = %q; it must say the ticket was passed on as written", intake.Rationale)
	}
	if err := intake.Validate(raw, config); err != nil {
		t.Fatalf("the sealed reading must revalidate: %v", err)
	}
	// What the unusable turns spent is still a real cost the report shows.
	if usage.TotalTokens == 0 {
		t.Fatal("the turns that were refused still cost money and must be reported")
	}

	draft, err := intake.ToDraft(raw, config)
	if err != nil {
		t.Fatalf("the reading must complete into a draft the chain can run: %v", err)
	}
	if draft.Request != strings.TrimSpace(raw.Description) || draft.Repository != config.Consumers[0].Repository {
		t.Fatalf("draft = %+v", draft)
	}
}

// TestAReadingMadeWithoutTheModelCannotBeForged: the mark is the licence to
// carry no rationale and no wording promise, so a reading that claims it
// while holding a model's answer must be refused.
func TestAReadingMadeWithoutTheModelCannotBeForged(t *testing.T) {
	config := validTestConfig()
	raw, err := ReadRawTicket(validTicketEnvelope(t, realTicketDescription()), config, strings.Repeat("c", 40))
	if err != nil {
		t.Fatal(err)
	}
	honest, err := FallbackContractIntake(raw, config, validTestInvocation(config.Models.Readiness.Assessor), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}

	forgeries := map[string]func(ContractIntake) ContractIntake{
		"a request that is not the ticket's text": func(c ContractIntake) ContractIntake {
			c.Request = "something else entirely"
			return c
		},
		"a wording promise nobody read": func(c ContractIntake) ContractIntake {
			c.VerificationPath, c.ExpectedText, c.AbsentText = "/settings", "new", "old"
			return c
		},
		"a rationale nobody wrote": func(c ContractIntake) ContractIntake {
			c.Rationale = "read from the acceptance conditions"
			return c
		},
		"a gap no model raised": func(c ContractIntake) ContractIntake {
			c.Gaps = []IntakeGap{{Field: "expected_text", Question: "q", Choices: []IntakeChoice{
				{ID: "a", Label: "one", Effect: "e"}, {ID: "b", Label: "two", Effect: "e"},
			}}}
			return c
		},
	}
	for name, forge := range forgeries {
		sealed, err := SealContractIntake(forge(honest))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := sealed.Validate(raw, config); err == nil {
			t.Errorf("%s was accepted as a reading made without the model", name)
		}
	}

	// A reading a model did answer is untouched by the check.
	answered := honest
	answered.Fallback = false
	answered.Rationale = "read from the acceptance conditions"
	answered.Invocation = validTestInvocation(config.Models.Readiness.Assessor)
	sealed, err := SealContractIntake(answered)
	if err != nil {
		t.Fatal(err)
	}
	if err := sealed.Validate(raw, config); err != nil {
		t.Fatalf("a reading a model answered was refused by the fallback check: %v", err)
	}
}

// TestAReaderThatCouldNotBeReachedStillFails is the boundary on the worker
// side: a provider that refused, a key with nothing left, a network that is
// not there. Reading the ticket itself would hide an instance an operator has
// to change.
func TestAReaderThatCouldNotBeReachedStillFails(t *testing.T) {
	config := validTestConfig()
	raw, err := ReadRawTicket(validTicketEnvelope(t, realTicketDescription()), config, strings.Repeat("c", 40))
	if err != nil {
		t.Fatal(err)
	}
	invoker, _ := NewModelInvoker(&sequenceChatAPI{err: errors.New("connection reset")})

	if _, _, err := invoker.ReadContract(context.Background(), raw, config); err == nil {
		t.Fatal("a reader that could not be reached was read as one that answered badly")
	}
}

// TestTheGateDecidedWithoutItsReadersGoesToTheImplementer pins the record the
// chain reads: ready, a change, no design. The implementer reads the
// destination's repository, which is where the rest of what the readers would
// have supplied is.
func TestTheGateDecidedWithoutItsReadersGoesToTheImplementer(t *testing.T) {
	config, request, source := validArtifactFixture(t)

	decision, err := FallbackReadinessDecision(source, request, config, time.Now().UTC())
	if err != nil {
		t.Fatalf("FallbackReadinessDecision() error = %v", err)
	}
	if decision.Outcome != ReadinessOutcomeReady || decision.RequestKind != RequestKindChange ||
		decision.NeedsDesign || decision.DesignReason != DesignReasonReceptionUnread || !decision.Fallback {
		t.Fatalf("decision = %+v", decision)
	}
	if len(decision.Questions) != 0 || len(decision.Assumptions) != 0 || decision.ReceptionJudgment != nil {
		t.Fatalf("a gate decided without readers asks and settles nothing: %+v", decision)
	}
	if err := decision.ValidateBinding(source, request, config); err != nil {
		t.Fatalf("the sealed gate must revalidate on its own: %v", err)
	}
	if err := decision.Validate(nil, nil, source, request, config); err != nil {
		t.Fatalf("the sealed gate must revalidate with no chain: %v", err)
	}
}

// TestAGateDecidedWithoutItsReadersCannotBeForged: the mark is what lets a
// decision stand with no assessment behind it and skip the design, so every
// other shape carrying the mark must be refused — and a decision the readers
// did answer must not be able to take the mark to skip a design the rule
// keeps.
func TestAGateDecidedWithoutItsReadersCannotBeForged(t *testing.T) {
	config, request, source := validArtifactFixture(t)
	honest, err := FallbackReadinessDecision(source, request, config, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}

	forgeries := map[string]func(ReadinessDecision) ReadinessDecision{
		"a design it skipped for another reason": func(d ReadinessDecision) ReadinessDecision {
			d.DesignReason = DesignReasonApproachInTicket
			return d
		},
		"a design it kept": func(d ReadinessDecision) ReadinessDecision {
			d.NeedsDesign = true
			return d
		},
		"an investigation": func(d ReadinessDecision) ReadinessDecision {
			d.RequestKind = RequestKindInvestigation
			return d
		},
		"an outcome other than ready": func(d ReadinessDecision) ReadinessDecision {
			d.Outcome = ReadinessOutcomeUnresolved
			return d
		},
		"a question it claims to have asked": func(d ReadinessDecision) ReadinessDecision {
			d.Questions = []ReadinessQuestion{{ID: "Q1", Dimension: "user_visible_behavior", Question: "q", WhyBlocking: "w"}}
			d.Outcome = ReadinessOutcomeClarification
			return d
		},
		"an attempt it names": func(d ReadinessDecision) ReadinessDecision {
			d.Attempts = 1
			d.AssessmentSHA256s = []string{strings.Repeat("a", 64)}
			d.CheckSHA256s = []string{strings.Repeat("b", 64)}
			return d
		},
		"an approach it quotes": func(d ReadinessDecision) ReadinessDecision {
			d.ApproachInTicket = true
			d.ApproachExcerpt = "some quote"
			return d
		},
	}
	for name, forge := range forgeries {
		forged := forge(honest)
		digest, err := readinessDecisionDigest(forged)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		forged.DecisionSHA256 = digest
		if err := forged.ValidateBinding(source, request, config); err == nil {
			t.Errorf("%s was accepted as a gate decided without its readers", name)
		}
	}

	// The other direction: a decision with a chain behind it cannot take the
	// reason to skip a design, because the reason belongs to the marked shape
	// alone and the marked shape has no chain.
	withChain := honest
	withChain.Fallback = false
	withChain.Attempts = 1
	withChain.AssessmentSHA256s = []string{strings.Repeat("a", 64)}
	withChain.CheckSHA256s = []string{strings.Repeat("b", 64)}
	digest, err := readinessDecisionDigest(withChain)
	if err != nil {
		t.Fatal(err)
	}
	withChain.DecisionSHA256 = digest
	if err := withChain.ValidateBinding(source, request, config); err == nil {
		t.Fatal("a decision with a chain behind it skipped a design on the fallback's reason")
	}
}
