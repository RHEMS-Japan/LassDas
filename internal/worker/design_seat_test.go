package worker

import (
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/worker/investigate"
)

// seatedDesignJudgesConfig gives the first design judge a candidate on a
// third vendor, with the launch that goes with it.
func seatedDesignJudgesConfig(t *testing.T) Config {
	t.Helper()
	config := designJudgesConfig(t)
	config.Models.DesignReviewers[0].Candidates = []ModelEndpoint{{
		Vendor: "Vendor C", Model: "judge-c-heavy", BaseURL: "https://gateway.example.com/api/v1",
		APIKeyEnv: "TEST_JUDGE_KEY_C", Lens: "evidence", MaxOutputTokens: 8192,
	}}
	config.Agents.DesignReviewerAgents[0].Candidates = []AgentConfig{{
		ID: "judge-a-candidate-agent", Command: "judge", Args: []string{"--profile", "judge-a-next"},
		Profile: "judge-a-next", SecretEnv: map[string]string{"JUDGE_KEY": "TEST_JUDGE_KEY_C"}, TimeoutSeconds: 900,
	}}
	if err := config.Validate(); err != nil {
		t.Fatalf("the seated design judges were refused: %v", err)
	}
	return config
}

// A design judged by a seat that moved. The decision gate used to hold a
// design review to the configured endpoint alone, so the ladder could move
// a design seat and the decision would then refuse the very review the
// move produced — a delivery moving and getting nowhere.
func TestADesignDecisionAcceptsAJudgeThatMoved(t *testing.T) {
	config := seatedDesignJudgesConfig(t)
	identity := designReviewIdentity(t, config)
	subject := investigate.ReviewSubject{Kind: investigate.SubjectDesign, Round: 1, SHA256: strings.Repeat("e", 64)}
	moved, _ := config.Models.DesignReviewers[0].SeatOccupant(1)
	reviews := []investigate.DesignReview{
		designSeatReview(t, identity, subject, moved, DesignLensEvidence),
		designSeatReview(t, identity, subject, config.Models.DesignReviewers[1], DesignLensApproach),
	}
	if err := ValidateDesignReviewSet(config, subject, reviews); err != nil {
		t.Fatalf("a design review by a candidate seat was refused: %v", err)
	}
	decision, err := investigate.DecideDesign(identity, subject, reviews, 1, config.DesignRounds(), len(config.Models.DesignJudges()))
	if err != nil {
		t.Fatalf("the decision refused a judge that moved: %v", err)
	}
	if decision.Outcome != investigate.OutcomeApproved {
		t.Fatalf("outcome = %q, want the verdicts counted as they are", decision.Outcome)
	}
	// And a judge nobody ever seated is refused exactly as before.
	stranger := moved
	stranger.Model = "judge-z"
	strange := []investigate.DesignReview{
		designSeatReview(t, identity, subject, stranger, DesignLensEvidence), reviews[1],
	}
	if err := ValidateDesignReviewSet(config, subject, strange); err == nil {
		t.Fatal("a design review naming a model no occupant of the seat ever was passed")
	}
}

// Two judges exist so that one provider's blind spot is not every judge's.
// The configuration refuses two judges seated on one vendor; a seat that
// moves can land on the vendor the other one holds, and by then the
// configuration has nothing left to say. The decision derives its own
// refusal from the records, so a reader re-deriving it reaches the same
// answer as the gate that sealed it.
func TestADesignDecisionRefusesTwoJudgesOnOneVendor(t *testing.T) {
	config := seatedDesignJudgesConfig(t)
	// The candidate on the vendor the other judge holds: a configuration
	// may list it, because the two are only forbidden from being there at
	// the same time.
	config.Models.DesignReviewers[0].Candidates[0].Vendor = config.Models.DesignReviewers[1].Vendor
	if err := config.Validate(); err != nil {
		t.Fatalf("a candidate sharing the other judge's vendor was refused at load: %v", err)
	}
	identity := designReviewIdentity(t, config)
	subject := investigate.ReviewSubject{Kind: investigate.SubjectDesign, Round: 1, SHA256: strings.Repeat("e", 64)}
	clashing, _ := config.Models.DesignReviewers[0].SeatOccupant(1)
	reviews := []investigate.DesignReview{
		designSeatReview(t, identity, subject, clashing, DesignLensEvidence),
		designSeatReview(t, identity, subject, config.Models.DesignReviewers[1], DesignLensApproach),
	}
	if _, err := investigate.DecideDesign(identity, subject, reviews, 1, config.DesignRounds(), 2); err == nil {
		t.Fatal("a design was approved by two judges answering from one vendor")
	}
	if err := ValidateDesignReviewSet(config, subject, reviews); err == nil {
		t.Fatal("the review set gate accepted two judges from one vendor")
	}
	// The seats where the configuration put them still decide.
	seated := []investigate.DesignReview{
		designSeatReview(t, identity, subject, config.Models.DesignReviewers[0].Seat()[0], DesignLensEvidence), reviews[1],
	}
	decision, err := investigate.DecideDesign(identity, subject, seated, 1, config.DesignRounds(), 2)
	if err != nil {
		t.Fatalf("two judges on two vendors were refused: %v", err)
	}
	// A reader re-deriving the sealed decision reaches the same refusal.
	if err := decision.Validate(identity, subject, reviews, config.DesignRounds(), 2); err == nil {
		t.Fatal("a sealed decision re-derived over two judges on one vendor")
	}
}

func designSeatReview(t *testing.T, identity investigate.Identity, subject investigate.ReviewSubject, endpoint ModelEndpoint, lens string) investigate.DesignReview {
	t.Helper()
	record, err := investigate.NewDesignReview(identity, subject,
		investigate.Reviewer{ID: endpoint.ID, Vendor: endpoint.Vendor, Model: endpoint.Model, BaseURL: endpoint.BaseURL, Lens: lens},
		investigate.ModelDesignReviewOutput{Verdict: investigate.VerdictPass, Findings: []investigate.DesignFinding{}},
		investigate.Usage{RequestedModel: endpoint.Model, RequestID: "run-" + endpoint.ID + "-" + endpoint.Model,
			StopReason: ChatFinishStop, InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
		testInvocationTime)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

// A launch written down for a seat that cannot move is a seat that reads as
// movable and is not one: nothing can ever reach that launch, and the
// configuration says otherwise.
func TestAConfigurationRefusesALaunchNoSeatCanReach(t *testing.T) {
	config := seatedTestConfig(t)
	config.Models.Reviewers[0].Candidates = nil
	if err := config.Validate(); err == nil {
		t.Fatal("Validate() accepted a candidate launch with no endpoint candidate to run")
	}
	judges := seatedDesignJudgesConfig(t)
	judges.Models.DesignReviewers[0].Candidates = nil
	if err := judges.Validate(); err == nil {
		t.Fatal("Validate() accepted a design judge's candidate launch with no endpoint candidate to run")
	}
}
