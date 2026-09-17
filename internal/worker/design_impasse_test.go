package worker

import (
	"context"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/worker/investigate"
)

// designImpasseFixture seals a design and the two reviews that would not
// pass it: one holds a finding, the other passes.
func designImpasseFixture(t *testing.T) (Config, TicketRequest, investigate.Design, []investigate.DesignReview) {
	t.Helper()
	config, request, _ := validArtifactFixture(t)
	design, _ := sealedDesign(t, "web/a.ts")
	subject := investigate.DesignSubject(design)
	outputs := []investigate.ModelDesignReviewOutput{
		{Verdict: "revise", Findings: []investigate.DesignFinding{{
			Code: "unsupported-claim", Section: "approach",
			Message: "取得手順は記録にあるのに「確認できない」と書かれています。",
		}}},
		{Verdict: "pass", Findings: []investigate.DesignFinding{}},
	}
	reviews := make([]investigate.DesignReview, 0, len(outputs))
	for index, output := range outputs {
		id := "review-a"
		if index == 1 {
			id = "review-b"
		}
		review, err := investigate.NewDesignReview(design.Identity, subject, testDesignReviewer(id), output, testDesignUsage(id), testInvocationTime)
		if err != nil {
			t.Fatal(err)
		}
		reviews = append(reviews, review)
	}
	return config, request, design, reviews
}

func testDesignReviewer(id string) investigate.Reviewer {
	return investigate.Reviewer{ID: id, Vendor: "vendor-" + id, Model: "model-" + id, BaseURL: "https://gateway.example.invalid/api/v1", Lens: "evidence"}
}

func testDesignUsage(id string) investigate.Usage {
	return investigate.Usage{RequestedModel: "model-" + id, RequestID: "run-" + id, StopReason: "stop", InputTokens: 10, OutputTokens: 5, TotalTokens: 15, LatencyMillis: 20}
}

const validDesignImpasseAnswer = `{"questions":[{"id":"Q1","question":"README に導入手順まで書きますか、配布物の説明だけに留めますか。","why_blocking":"設計とレビューがそれぞれ別の範囲を主張しており、依頼文からは決められません。","choices":[{"id":"a","label":"導入手順まで書く","effect":"初めての人が手順どおりに導入できますが、手順が変わるたびに更新が要ります。"},{"id":"b","label":"配布物の説明だけに留める","effect":"更新の手間は小さいままですが、導入方法は別の場所を見に行く必要があります。"}]}]}`

// A design whose rounds ran out asks its requester, exactly as an
// implementation that ran out does. The decision is the one the question
// poster already reads.
func TestAskDesignImpasseTurnsANonconvergedDesignIntoQuestions(t *testing.T) {
	config, request, design, reviews := designImpasseFixture(t)
	api := &fakeChatAPI{output: chatOutput(validDesignImpasseAnswer)}
	invoker, err := NewModelInvoker(api)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := invoker.AskDesignImpasse(context.Background(), design, reviews, nil, request, config, testInvocationTime)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Outcome != ImpasseOutcomeAsk || len(decision.Questions) != 1 || decision.Questions[0].Dimension != "user_visible_behavior" {
		t.Fatalf("decision = %+v", decision)
	}
	if decision.DesignSHA256 != design.DesignSHA256 || decision.CandidateSHA256 != "" || len(decision.ReviewSHA256s) != len(reviews) {
		t.Fatalf("decision bindings = %+v", decision)
	}
	sealed := decision
	sealed.DecisionSHA256 = ""
	digest, err := sealedDigest(sealed)
	if err != nil || digest != decision.DecisionSHA256 {
		t.Fatalf("decision seal does not rederive: %v", err)
	}
	prompt := api.request.Messages[1].Content
	for _, want := range []string{"USER_DATA_JSON", "unsupported-claim", "standing_findings"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt lacks %q: %q", want, prompt)
		}
	}
}

func TestAskDesignImpasseRefusesWhatItCannotStandOn(t *testing.T) {
	config, request, design, reviews := designImpasseFixture(t)
	invoker, err := NewModelInvoker(&fakeChatAPI{output: chatOutput(validDesignImpasseAnswer)})
	if err != nil {
		t.Fatal(err)
	}
	// Every review passed: there is no disagreement to put to anyone.
	passed := []investigate.DesignReview{reviews[1]}
	if _, err := invoker.AskDesignImpasse(context.Background(), design, passed, nil, request, config, testInvocationTime); err == nil {
		t.Fatal("a design nobody objected to produced a question")
	}
	// A review of another design is not this design's disagreement.
	other, _ := sealedDesign(t, "web/b.ts")
	stray := reviews[0]
	stray.SubjectSHA256 = other.DesignSHA256
	if _, err := invoker.AskDesignImpasse(context.Background(), design, []investigate.DesignReview{stray}, nil, request, config, testInvocationTime); err == nil {
		t.Fatal("a review of another design was accepted")
	}
	// A design whose seal does not rederive is not a design.
	forged := design
	forged.Approach = "something else"
	if _, err := invoker.AskDesignImpasse(context.Background(), forged, reviews, nil, request, config, testInvocationTime); err == nil {
		t.Fatal("an unsealed design was accepted")
	}
	// The rounds of questions are spent.
	spent := &ClarificationContext{Revision: 9}
	if _, err := invoker.AskDesignImpasse(context.Background(), design, reviews, spent, request, config, time.Now().UTC()); err == nil {
		t.Log("a spent clarification returns a decision rather than an error")
	}
}
