package worker

import (
	"encoding/json"
	"strings"
	"testing"
)

func arbiterSeatFixture(t *testing.T) (Config, TicketRequest, SourceSnapshot, Candidate, []Review) {
	t.Helper()
	config, request, source, candidate := seatedFixture(t, func(config Config) Config {
		seat := config.Models.Readiness.Assessor
		seat.ID = "arbiter"
		alternative := seat
		alternative.ID, alternative.Vendor, alternative.Model = "", "Vendor C", "alternate-arbiter"
		alternative.BaseURL, alternative.APIKeyEnv = "https://alternate.example/v1", "TEST_ALTERNATE_KEY"
		seat.Candidates = []ModelEndpoint{alternative}
		config.Models.Arbiter = &seat
		return config
	})
	var reviews []Review
	for index, seat := range config.Models.Reviewers {
		occupant, _ := seat.SeatOccupant(0)
		output := ModelReviewOutput{Verdict: "pass", Findings: []ModelFinding{}}
		if index == 1 {
			output = ModelReviewOutput{Verdict: "revise", Findings: []ModelFinding{{Code: "missed-escalation", Path: request.TargetFiles[0], Message: "Required behavior is missing."}}}
		}
		review, err := NewReview(candidate.Stage, occupant, output, candidate, source, request, config, validTestInvocation(occupant), testInvocationTime)
		if err != nil {
			t.Fatal(err)
		}
		reviews = append(reviews, review)
	}
	return config, request, source, candidate, reviews
}

func TestAMovedArbiterIsRecordedAndStillBoundToItsConfiguredSeat(t *testing.T) {
	config, request, source, candidate, reviews := arbiterSeatFixture(t)
	api := &fakeChatAPI{output: chatOutput(overrulingAnswer(objectingSeat(config), request.TargetFiles[0]))}
	invoker, err := NewModelInvoker(api)
	if err != nil {
		t.Fatal(err)
	}
	ruling, err := invoker.ArbitrateWithOptions(t.Context(), candidate, reviews, nil, nil, source, request, config, testInvocationTime, ArbitrationOptions{SeatCandidate: 1})
	if err != nil {
		t.Fatal(err)
	}
	occupant, _ := config.Models.ArbiterEndpoint().SeatOccupant(1)
	if !api.endpoint.sameOccupant(occupant) || ruling.Arbiter == nil || !ruling.Arbiter.sameOccupant(occupant) || len(ruling.Arbiter.Candidates) != 0 {
		t.Fatal("the actual configured occupant was not called and recorded")
	}
	decision, err := DecideStage(candidate, reviews, source, request, config, &ruling)
	if err != nil || decision.Outcome != "converged" {
		t.Fatalf("the legitimate moved arbiter's ruling was not re-derived: %v", err)
	}
	for name, change := range map[string]func(*Ruling){
		"unconfigured model":      func(r *Ruling) { r.Arbiter.Model = "stranger"; r.Invocation.RequestedModel = "stranger" },
		"unconfigured route":      func(r *Ruling) { r.Arbiter.BaseURL = "https://stranger.example/v1" },
		"unconfigured key name":   func(r *Ruling) { r.Arbiter.APIKeyEnv = "STRANGER_KEY" },
		"wrong invocation":        func(r *Ruling) { r.Invocation.RequestedModel = config.Models.ArbiterEndpoint().Model },
		"missing invocation":      func(r *Ruling) { r.Invocation = nil },
		"unsealed candidate list": func(r *Ruling) { r.Arbiter.Candidates = []ModelEndpoint{occupant} },
	} {
		t.Run(name, func(t *testing.T) {
			forged := ruling
			endpoint, usage := *ruling.Arbiter, *ruling.Invocation
			forged.Arbiter, forged.Invocation = &endpoint, &usage
			change(&forged)
			forged, err = sealRuling(forged)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecideStage(candidate, reviews, source, request, config, &forged); err == nil {
				t.Fatal("a rehashed but unconfigured arbiter or false invocation evidence passed the decision gate")
			}
		})
	}
	// Old rulings never had this optional field. They remain readable with
	// their original shape, rather than being relabeled as a guessed model.
	legacy := ruling
	legacy.Arbiter = nil
	legacy, err = sealRuling(legacy)
	if err != nil || legacy.Validate(candidate, reviews, request, config) != nil {
		t.Fatalf("the legacy ruling shape was invalidated: %v", err)
	}
	encoded, err := json.Marshal(legacy)
	if err != nil || strings.Contains(string(encoded), `"arbiter":`) {
		t.Fatal("an absent legacy arbiter changed the encoded record")
	}
}

func TestAnUnconfiguredArbiterCandidateIsRefusedBeforeCallingAModel(t *testing.T) {
	config, request, source, candidate, reviews := arbiterSeatFixture(t)
	for _, place := range []int{-1, 2, 100} {
		api := &fakeChatAPI{output: chatOutput(instructingAnswer)}
		invoker, err := NewModelInvoker(api)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := invoker.ArbitrateWithOptions(t.Context(), candidate, reviews, nil, nil, source, request, config, testInvocationTime, ArbitrationOptions{SeatCandidate: place}); err == nil || api.request != nil {
			t.Fatalf("out-of-range candidate %d reached a model or was accepted: %v", place, err)
		}
	}
}

func TestArbiterIdentityCountsAgainstTheRulingSizeLimit(t *testing.T) {
	output := ModelArbitrationOutput{Ruling: RulingInstructImplementer, Instruction: "Implement the missing behavior."}
	if err := arbitrationOutputFits(output, nil, false, ModelEndpoint{Model: "small-model"}); err != nil {
		t.Fatal(err)
	}
	// Model names are operator data, not assumed small by the record writer.
	if err := arbitrationOutputFits(output, nil, false, ModelEndpoint{Model: strings.Repeat("m", int(MaxReviewJSONBytes))}); err == nil {
		t.Fatal("the new arbiter identity overflowed the record after the model answer was accepted")
	}
}
