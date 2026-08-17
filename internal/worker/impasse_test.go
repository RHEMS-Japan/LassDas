package worker

import (
	"context"
	"strings"
	"testing"
)

// nonconvergedFixture builds a final-stage candidate whose reviews still
// disagree: the correctness reviewer passes, the adversarial reviewer holds a
// finding. DecideStage turns that into nonconverged, which is the only state
// AskImpasse may act on.
func nonconvergedFixture(t *testing.T) (Config, TicketRequest, SourceSnapshot, Candidate, []Review) {
	t.Helper()
	config, request, source := validArtifactFixture(t)
	candidate, err := NewCandidate(config.MaxStages, ModelCandidateOutput{
		Files:     []ModelCandidateFile{{Path: request.TargetFiles[0], Content: "export const label = 'Updated label';\n"}},
		Rationale: "記録が空のときは抑制を優先する側に倒した。",
	}, source, request, config, validTestInvocation(config.Models.Implementer), testInvocationTime)
	if err != nil {
		t.Fatal(err)
	}
	outputs := []ModelReviewOutput{
		{Verdict: "pass", Findings: []ModelFinding{}},
		{Verdict: "revise", Findings: []ModelFinding{{
			Code: "missed-escalation", Path: request.TargetFiles[0],
			Message: "基準の記録が空のあいだは悪化しても通知されません。",
		}}},
	}
	reviews := make([]Review, 0, len(config.Models.Reviewers))
	for index, endpoint := range config.Models.Reviewers {
		review, err := NewReview(candidate.Stage, endpoint, outputs[index%len(outputs)], candidate, source, request, config, validTestInvocation(endpoint), testInvocationTime)
		if err != nil {
			t.Fatal(err)
		}
		reviews = append(reviews, review)
	}
	return config, request, source, candidate, reviews
}

const validImpasseAnswer = `{"questions":[{"id":"Q1","question":"通知記録が壊れているあいだ、抑制と悪化通知のどちらを優先しますか。","why_blocking":"実装とレビューがそれぞれ別の側を守っており、どちらも仕様からは決められません。","choices":[{"id":"a","label":"「止める」を優先する","effect":"押した抑制は必ず効きますが、まれに悪化の通知を取りこぼします。"},{"id":"b","label":"悪化の通知を優先する","effect":"悪化は必ず届きますが、記録が壊れた障害には抑制が効かなくなります。"}]}]}`

func TestAskImpasseTurnsANonconvergedStageIntoRequesterQuestions(t *testing.T) {
	config, request, source, candidate, reviews := nonconvergedFixture(t)
	api := &fakeChatAPI{output: chatOutput(validImpasseAnswer)}
	invoker, err := NewModelInvoker(api)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := invoker.AskImpasse(context.Background(), candidate, reviews, nil, source, request, config, testInvocationTime)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Outcome != ImpasseOutcomeAsk || len(decision.Questions) != 1 || decision.Questions[0].ID != "Q1" {
		t.Fatalf("decision = %+v", decision)
	}
	if decision.Questions[0].Dimension != "user_visible_behavior" {
		t.Fatalf("dimension was not stamped: %+v", decision.Questions[0])
	}
	if decision.CandidateSHA256 != candidate.CandidateSHA256 || len(decision.ReviewSHA256s) != len(reviews) {
		t.Fatalf("decision bindings = %+v", decision)
	}
	sealed := decision
	sealed.DecisionSHA256 = ""
	digest, err := sealedDigest(sealed)
	if err != nil || digest != decision.DecisionSHA256 {
		t.Fatalf("decision seal does not rederive: %v", err)
	}
	if api.endpoint.ID != config.Models.Readiness.Assessor.ID {
		t.Fatalf("question author endpoint = %+v", api.endpoint)
	}
	prompt := api.request.Messages[1].Content
	if !strings.Contains(prompt, "USER_DATA_JSON") || !strings.Contains(prompt, "missed-escalation") ||
		!strings.Contains(prompt, "Updated label") {
		t.Fatalf("prompt lacks the deadlock evidence: %q", prompt)
	}
}

func TestAskImpasseRefusesAConvergedStage(t *testing.T) {
	config, request, source, candidate, _ := nonconvergedFixture(t)
	reviews := make([]Review, 0, len(config.Models.Reviewers))
	for _, endpoint := range config.Models.Reviewers {
		review, err := NewReview(candidate.Stage, endpoint, ModelReviewOutput{Verdict: "pass", Findings: []ModelFinding{}}, candidate, source, request, config, validTestInvocation(endpoint), testInvocationTime)
		if err != nil {
			t.Fatal(err)
		}
		reviews = append(reviews, review)
	}
	api := &fakeChatAPI{output: chatOutput(validImpasseAnswer)}
	invoker, _ := NewModelInvoker(api)
	if _, err := invoker.AskImpasse(context.Background(), candidate, reviews, nil, source, request, config, testInvocationTime); err == nil {
		t.Fatal("AskImpasse() asked a question for a converged stage")
	}
	if api.request != nil {
		t.Fatal("a converged stage still reached the model")
	}
}

// Once the answer rounds are spent, the question endpoint would reject a new
// ask, so the decision records exhaustion without spending a model call and
// the workflow falls back to the honest nonconverged terminal.
func TestAskImpasseDeclinesWhenQuestionRoundsAreSpent(t *testing.T) {
	config, request, source, candidate, reviews := nonconvergedFixture(t)
	clarification := &ClarificationContext{
		SHA256:      strings.Repeat("ab", 32),
		Revision:    3,
		DeliveryID:  request.DeliveryID,
		InputSHA256: request.InputSHA256,
	}
	api := &fakeChatAPI{output: chatOutput(validImpasseAnswer)}
	invoker, _ := NewModelInvoker(api)
	decision, err := invoker.AskImpasse(context.Background(), candidate, reviews, clarification, source, request, config, testInvocationTime)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Outcome != ImpasseOutcomeExhausted || len(decision.Questions) != 0 || decision.Invocation != nil {
		t.Fatalf("decision = %+v", decision)
	}
	if api.request != nil {
		t.Fatal("an exhausted run still reached the model")
	}
}

func TestAskImpasseRejectsMalformedQuestions(t *testing.T) {
	config, request, source, candidate, reviews := nonconvergedFixture(t)
	for _, answer := range []string{
		`{"questions":[]}`,
		`{"questions":[{"id":"Q2","question":"順序が飛んでいます。","why_blocking":"x","choices":[{"id":"a","label":"a","effect":"1"},{"id":"b","label":"b","effect":"2"}]}]}`,
		`{"questions":[{"id":"Q1","question":"選択肢が 1 つしかありません。","why_blocking":"x","choices":[{"id":"a","label":"a","effect":"1"}]}]}`,
		`{"questions":[{"id":"Q1","question":"選択肢の記号が飛んでいます。","why_blocking":"x","choices":[{"id":"a","label":"a","effect":"1"},{"id":"c","label":"c","effect":"2"}]}]}`,
	} {
		api := &fakeChatAPI{output: chatOutput(answer)}
		invoker, _ := NewModelInvoker(api)
		if _, err := invoker.AskImpasse(context.Background(), candidate, reviews, nil, source, request, config, testInvocationTime); err == nil {
			t.Fatalf("AskImpasse() accepted %q", answer)
		}
	}
}

// The endpoint cannot enforce a schema (structured output is off for the
// assessor rail), so the model may volunteer taxonomy of its own. A sound
// question must survive that: the label is normalized, never fatal. The first
// live ask was lost to exactly this before the stamp existed (2026-08-07).
func TestAskImpasseNormalizesAModelSuppliedDimension(t *testing.T) {
	config, request, source, candidate, reviews := nonconvergedFixture(t)
	answer := `{"questions":[{"id":"Q1","dimension":"behavior_tradeoff","question":"どちらを優先しますか。","why_blocking":"レビューが収束しませんでした。","choices":[{"id":"a","label":"抑制を優先","effect":"悪化通知を取りこぼしえます。"},{"id":"b","label":"悪化通知を優先","effect":"抑制が効かない障害が残ります。"}]}]}`
	api := &fakeChatAPI{output: chatOutput(answer)}
	invoker, _ := NewModelInvoker(api)
	decision, err := invoker.AskImpasse(context.Background(), candidate, reviews, nil, source, request, config, testInvocationTime)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Questions[0].Dimension != "user_visible_behavior" {
		t.Fatalf("dimension was not normalized: %+v", decision.Questions[0])
	}
}
