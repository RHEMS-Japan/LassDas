package worker

import (
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/worker/investigate"
)

func designReviewIdentity(t *testing.T, config Config) investigate.Identity {
	t.Helper()
	configSHA, err := config.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	return investigate.Identity{
		DeliveryID: "delivery_" + strings.Repeat("d", 32), InputSHA256: strings.Repeat("1", 64),
		ConfigSHA256: configSHA, ToolSHA: strings.Repeat("c", 40), BaseSHA: strings.Repeat("a", 40),
	}
}

func sealedDesignReviewRun(t *testing.T, agent AgentConfig, identity investigate.Identity, round int, transcript string) AgentRun {
	t.Helper()
	run, err := SealAgentRun(AgentRun{
		SchemaVersion: ArtifactSchemaVersion, Stage: round,
		DeliveryID: identity.DeliveryID, InputSHA256: identity.InputSHA256,
		ConfigSHA256: identity.ConfigSHA256, ToolSHA: identity.ToolSHA, BaseSHA: identity.BaseSHA,
		AgentID: agent.ID, Command: agent.Command, PromptBytes: 42,
		Transcript: transcript, RanAt: time.Date(2026, 9, 4, 1, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// A design review is this reviewer's own launch, bound to the round it
// judged; any other run - the implementer's, another round's, another
// delivery's - is not its judgment however valid its verdict reads.
func TestAgentDesignReviewFromRunBindsTheLaunchAndTheRound(t *testing.T) {
	config := validTestConfig()
	identity := designReviewIdentity(t, config)
	subject := investigate.ReviewSubject{Kind: investigate.SubjectDesign, Round: 2, SHA256: strings.Repeat("e", 64)}
	endpoint := config.Models.Reviewers[1]
	transcript := "I read the records.\n" + ReviewAnswerRulesTail + "\n" + `{"verdict":"pass","findings":[]}`

	own := sealedDesignReviewRun(t, config.Agents.Reviewer, identity, 2, transcript)
	review, err := AgentDesignReviewFromRun(endpoint, DesignLensApproach, own, identity, subject, config, testInvocationTime)
	if err != nil {
		t.Fatalf("AgentDesignReviewFromRun() rejected the reviewer's own launch: %v", err)
	}
	if review.Lens != DesignLensApproach || review.ReviewerID != "review-b" || review.Invocation.RequestID != own.RunSHA256 || review.Round != 2 {
		t.Fatalf("sealed review: %+v", review)
	}
	if err := review.Validate(identity, subject); err != nil {
		t.Fatalf("the sealed review was rejected: %v", err)
	}

	foreign := sealedDesignReviewRun(t, config.Agents.Implementer, identity, 2, transcript)
	if _, err := AgentDesignReviewFromRun(endpoint, DesignLensApproach, foreign, identity, subject, config, testInvocationTime); err == nil {
		t.Fatal("the implementer's run was accepted as a design review")
	}
	otherRound := sealedDesignReviewRun(t, config.Agents.Reviewer, identity, 1, transcript)
	if _, err := AgentDesignReviewFromRun(endpoint, DesignLensApproach, otherRound, identity, subject, config, testInvocationTime); err == nil {
		t.Fatal("a run of another round was accepted")
	}
	otherDelivery := identity
	otherDelivery.DeliveryID = "delivery_" + strings.Repeat("e", 32)
	elsewhere := sealedDesignReviewRun(t, config.Agents.Reviewer, otherDelivery, 2, transcript)
	if _, err := AgentDesignReviewFromRun(endpoint, DesignLensApproach, elsewhere, identity, subject, config, testInvocationTime); err == nil {
		t.Fatal("a run of another delivery was accepted")
	}
	unconfigured := endpoint
	unconfigured.Model = "model-elsewhere"
	if _, err := AgentDesignReviewFromRun(unconfigured, DesignLensApproach, own, identity, subject, config, testInvocationTime); err == nil {
		t.Fatal("an unconfigured reviewer endpoint was accepted")
	}
	silent := sealedDesignReviewRun(t, config.Agents.Reviewer, identity, 2, "I would rather not say.")
	if _, err := AgentDesignReviewFromRun(endpoint, DesignLensApproach, silent, identity, subject, config, testInvocationTime); err == nil {
		t.Fatal("a run without a verdict was accepted")
	}
}

// The design reviewer answers in its own shape. A finding carrying the
// candidate review's fields, an extra field or a duplicate key is a reviewer
// that did not follow the contract, not a verdict to trim into shape.
func TestDecodeAgentDesignReviewOutputIsStrict(t *testing.T) {
	output, err := DecodeAgentDesignReviewOutput("Read the records.\n" +
		`{"verdict":"revise","findings":[{"code":"unmeasured-cache","section":"cause","message":"The cache path was never measured."}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if output.Verdict != "revise" || len(output.Findings) != 1 || output.Findings[0].Section != "cause" {
		t.Fatalf("verdict was not read: %+v", output)
	}
	echoed := "## 答え方\n" + `{"verdict":"revise","findings":[{"code":"x-y","section":"cause","message":"example"}]}` + "\n" + ReviewAnswerRulesTail + "\nI need approval before I start."
	if _, err := DecodeAgentDesignReviewOutput(echoed); err == nil {
		t.Fatal("an echoed format example was taken as the verdict")
	}
	for _, transcript := range []string{
		`{"verdict":"revise","findings":[{"code":"c-d","section":"cause","message":"m","path":"client/src/label.ts"}]}`,
		`{"verdict":"revise","findings":[{"code":"c-d","section":"cause","message":"m","line":3}]}`,
		`{"verdict":"pass","findings":[],"note":"looks fine"}`,
		`{"verdict":"pass","findings":[],"verdict":"revise"}`,
		"Looks good to me.",
	} {
		if _, err := DecodeAgentDesignReviewOutput(transcript); err == nil {
			t.Errorf("a verdict was accepted from %q", transcript)
		}
	}
}

func TestResolveDesignLensDefaultsByPosition(t *testing.T) {
	config := validTestConfig()
	cases := []struct {
		reviewer, selector, subject, want string
		refused                           bool
	}{
		{"review-a", "", investigate.SubjectDesign, DesignLensEvidence, false},
		{"review-b", "", investigate.SubjectDesign, DesignLensApproach, false},
		{"review-b", "A", investigate.SubjectDesign, DesignLensEvidence, false},
		{"review-a", "B", investigate.SubjectDesign, DesignLensApproach, false},
		{"review-a", "", investigate.SubjectInvestigation, DesignLensEvidence, false},
		{"review-b", "", investigate.SubjectInvestigation, "", true},
		{"review-a", "B", investigate.SubjectInvestigation, "", true},
		{"review-a", "C", investigate.SubjectDesign, "", true},
		{"review-zz", "", investigate.SubjectDesign, "", true},
	}
	for _, tc := range cases {
		lens, err := ResolveDesignLens(config, tc.reviewer, tc.selector, tc.subject)
		if (err != nil) != tc.refused || lens != tc.want {
			t.Errorf("ResolveDesignLens(%s, %q, %s) = %q, %v", tc.reviewer, tc.selector, tc.subject, lens, err)
		}
	}
	// A consumer's own wording replaces the built-in seat, for either subject.
	config.Models.Reviewers[1].DesignLens = "Is the fix the smallest one that could work?"
	for _, subject := range []string{investigate.SubjectDesign, investigate.SubjectInvestigation} {
		lens, err := ResolveDesignLens(config, "review-b", "", subject)
		if err != nil || lens != config.Models.Reviewers[1].DesignLens {
			t.Errorf("custom lens for %s: %q, %v", subject, lens, err)
		}
	}
}

func TestValidateDesignReviewSet(t *testing.T) {
	config := validTestConfig()
	config.Models.Reviewers = append(config.Models.Reviewers, ModelEndpoint{
		ID: "review-c", Vendor: "Vendor A", Model: "model-c", BaseURL: "https://gateway.example.com/api/v1",
		APIKeyEnv: "TEST_MODEL_KEY_A", Lens: "another reading", MaxOutputTokens: 2048,
	})
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	identity := designReviewIdentity(t, config)
	subject := investigate.ReviewSubject{Kind: investigate.SubjectDesign, Round: 1, SHA256: strings.Repeat("e", 64)}
	review := func(endpoint ModelEndpoint) investigate.DesignReview {
		record, err := investigate.NewDesignReview(identity, subject,
			investigate.Reviewer{ID: endpoint.ID, Vendor: endpoint.Vendor, Model: endpoint.Model, BaseURL: endpoint.BaseURL, Lens: DesignLensEvidence},
			investigate.ModelDesignReviewOutput{Verdict: investigate.VerdictPass, Findings: []investigate.DesignFinding{}},
			investigate.Usage{RequestedModel: endpoint.Model, RequestID: "run-" + endpoint.ID, StopReason: ChatFinishStop, InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
			testInvocationTime)
		if err != nil {
			t.Fatal(err)
		}
		return record
	}
	a, b, c := review(config.Models.Reviewers[0]), review(config.Models.Reviewers[1]), review(config.Models.Reviewers[2])
	if err := ValidateDesignReviewSet(config, subject, []investigate.DesignReview{a, b}); err != nil {
		t.Fatalf("two reviewers of different vendors were refused: %v", err)
	}
	stranger := b
	stranger.ReviewerID = "review-zz"
	refused := map[string][]investigate.DesignReview{
		"one review of a design":    {a},
		"the same reviewer twice":   {a, a},
		"an unconfigured reviewer":  {a, stranger},
		"two reviewers of a vendor": {a, c},
	}
	for name, reviews := range refused {
		if err := ValidateDesignReviewSet(config, subject, reviews); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	report := investigate.ReviewSubject{Kind: investigate.SubjectInvestigation, Round: 1, SHA256: strings.Repeat("e", 64)}
	if err := ValidateDesignReviewSet(config, report, []investigate.DesignReview{a}); err != nil {
		t.Errorf("the evidence review alone was refused for a report: %v", err)
	}
	if err := ValidateDesignReviewSet(config, report, []investigate.DesignReview{a, b}); err == nil {
		t.Error("two reviews were accepted for a report")
	}
}
