package worker

import (
	"context"
	"strings"
	"testing"
)

// nonconvergedFixture is one round the seats did not agree on: a change, a
// seat that passed it and a seat that objected to it by name. It is the
// shape every deadlock takes, and the shape the trail renders.
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

// objectingSeat is the seat the fixture's objection belongs to.
func objectingSeat(config Config) string { return config.Models.Reviewers[1].ID }

func overrulingAnswer(seat, path string) string {
	return `{"ruling":"overrule_reviewer","instruction":"","overruled":[{"reviewer_id":"` + seat +
		`","code":"missed-escalation","path":"` + path +
		`","reason":"依頼は抑制の一点だけを求めており、悪化通知の追加は依頼の範囲外です。"}],` +
		`"statement":"依頼の検収条件は抑制が効くことだけを求めている。","evidence":"チケット本文の検収条件。"}`
}

const instructingAnswer = `{"ruling":"instruct_implementer","instruction":"抑制を押した後、記録が空のときでも悪化した通知が届くこと。",` +
	`"overruled":[],"statement":"検収条件は悪化の通知を含む。","evidence":"チケット本文の検収条件。"}`

// A ruling that sets the objection aside makes the round converge: the
// verdict is counted again without it, and nothing is left standing.
func TestOverrulingRecountsTheVerdict(t *testing.T) {
	config, request, source, candidate, reviews := nonconvergedFixture(t)
	plain, err := DecideStage(candidate, reviews, source, request, config, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plain.Outcome != "revise" {
		t.Fatalf("without a ruling the round is %q, want revise", plain.Outcome)
	}
	api := &fakeChatAPI{output: chatOutput(overrulingAnswer(objectingSeat(config), request.TargetFiles[0]))}
	invoker, err := NewModelInvoker(api)
	if err != nil {
		t.Fatal(err)
	}
	ruling, err := invoker.Arbitrate(context.Background(), candidate, reviews, nil, source, request, config, testInvocationTime)
	if err != nil {
		t.Fatal(err)
	}
	if ruling.Ruling != RulingOverruleReviewer || len(ruling.Overruled) != 1 {
		t.Fatalf("ruling = %+v", ruling)
	}
	ruled, err := DecideStage(candidate, reviews, source, request, config, &ruling)
	if err != nil {
		t.Fatal(err)
	}
	if ruled.Outcome != "converged" {
		t.Fatalf("under the ruling the round is %q, want converged", ruled.Outcome)
	}
	// The decision carries the ruling, so a reader handed nothing else can
	// still re-derive the outcome it reports.
	if err := ruled.Validate(candidate, reviews, source, request, config); err != nil {
		t.Fatalf("a ruled decision did not re-derive: %v", err)
	}
	if len(ruled.Overruled()) != 1 || ruled.Overruled()[0].ReviewerID != objectingSeat(config) {
		t.Fatalf("overruled = %+v", ruled.Overruled())
	}
}

// The ruling is what makes the outcome legitimate, so a decision claiming a
// converged round with the ruling taken out of it, or with a ruling somebody
// edited, is refused.
func TestARuledDecisionCannotBeForged(t *testing.T) {
	config, request, source, candidate, reviews := nonconvergedFixture(t)
	api := &fakeChatAPI{output: chatOutput(overrulingAnswer(objectingSeat(config), request.TargetFiles[0]))}
	invoker, err := NewModelInvoker(api)
	if err != nil {
		t.Fatal(err)
	}
	ruling, err := invoker.Arbitrate(context.Background(), candidate, reviews, nil, source, request, config, testInvocationTime)
	if err != nil {
		t.Fatal(err)
	}
	ruled, err := DecideStage(candidate, reviews, source, request, config, &ruling)
	if err != nil {
		t.Fatal(err)
	}
	stripped := ruled
	stripped.Ruling = nil
	if err := stripped.Validate(candidate, reviews, source, request, config); err == nil {
		t.Error("a converged round kept its outcome with the ruling taken away")
	}
	edited := ruled
	widened := ruling
	widened.Overruled = append(append([]OverruledFinding(nil), ruling.Overruled...), OverruledFinding{
		ReviewerID: objectingSeat(config), Code: "invented", Path: request.TargetFiles[0], Reason: "足した。",
	})
	edited.Ruling = &widened
	if err := edited.Validate(candidate, reviews, source, request, config); err == nil {
		t.Error("a ruling edited after sealing was accepted")
	}
	// And a ruling whose own seal is perfect, whose decision's seal is
	// perfect, and which was made about another round's change. Every digest
	// here checks out; the only thing wrong is that the ruling is not this
	// round's, which is the whole reason the decision holds it to the
	// artifacts it was handed.
	foreign := ruling
	foreign.CandidateSHA256 = strings.Repeat("ab", 32)
	foreign, err = sealRuling(foreign)
	if err != nil {
		t.Fatal(err)
	}
	borrowed := ruled
	borrowed.Ruling = &foreign
	borrowed.DecisionSHA256 = ""
	digest, err := stageDecisionDigest(borrowed)
	if err != nil {
		t.Fatal(err)
	}
	borrowed.DecisionSHA256 = digest
	if err := borrowed.Validate(candidate, reviews, source, request, config); err == nil {
		t.Error("a ruling made about another round's change was allowed to converge this one")
	}
}

// An objection nobody raised cannot be set aside: the arbiter reads the
// findings as untrusted data, and only what a configured seat actually
// raised is a thing the verdict would have counted.
func TestArbitrationRefusesToOverruleAnObjectionTheRoundDoesNotCarry(t *testing.T) {
	config, request, source, candidate, reviews := nonconvergedFixture(t)
	api := &fakeChatAPI{output: chatOutput(overrulingAnswer(objectingSeat(config), "docs/OTHER.md"))}
	invoker, err := NewModelInvoker(api)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := invoker.Arbitrate(context.Background(), candidate, reviews, nil, source, request, config, testInvocationTime); err == nil {
		t.Fatal("an objection the round does not carry was set aside")
	}
}

// The other ruling leaves the objections standing and says what the next
// round has to satisfy.
func TestInstructingRulingLeavesTheRoundToBeRevised(t *testing.T) {
	config, request, source, candidate, reviews := nonconvergedFixture(t)
	api := &fakeChatAPI{output: chatOutput(instructingAnswer)}
	invoker, err := NewModelInvoker(api)
	if err != nil {
		t.Fatal(err)
	}
	ruling, err := invoker.Arbitrate(context.Background(), candidate, reviews, nil, source, request, config, testInvocationTime)
	if err != nil {
		t.Fatal(err)
	}
	if ruling.Ruling != RulingInstructImplementer || ruling.Instruction == "" || len(ruling.Overruled) != 0 {
		t.Fatalf("ruling = %+v", ruling)
	}
	if ruling.Assumption.Kind != AssumptionArbiterRuling || ruling.Assumption.Statement == "" {
		t.Fatalf("assumption = %+v", ruling.Assumption)
	}
	decision, err := DecideStage(candidate, reviews, source, request, config, &ruling)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Outcome != "revise" {
		t.Fatalf("outcome = %q, want revise", decision.Outcome)
	}
}

// The arbiter is a seat of its own, and falls back to the role that already
// reads a ticket and says what it asks for.
func TestArbiterSeatFallsBackToTheReceptionAssessor(t *testing.T) {
	config, request, source, candidate, reviews := nonconvergedFixture(t)
	api := &fakeChatAPI{output: chatOutput(instructingAnswer)}
	invoker, err := NewModelInvoker(api)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := invoker.Arbitrate(context.Background(), candidate, reviews, nil, source, request, config, testInvocationTime); err != nil {
		t.Fatal(err)
	}
	if api.endpoint.ID != config.Models.Readiness.Assessor.ID {
		t.Fatalf("arbiter endpoint = %q, want the assessor %q", api.endpoint.ID, config.Models.Readiness.Assessor.ID)
	}
	named := config.Models.Reviewers[0]
	named.ID = "arbiter"
	config.Models.Arbiter = &named
	if got := config.Models.ArbiterEndpoint().ID; got != "arbiter" {
		t.Fatalf("configured arbiter = %q", got)
	}
}

// What the arbiter is given is the ticket, the objections still standing and
// the change they stand against — and it is told the data is data.
func TestArbitrationPromptCarriesTheRequestAndTheObjections(t *testing.T) {
	config, request, source, candidate, reviews := nonconvergedFixture(t)
	api := &fakeChatAPI{output: chatOutput(instructingAnswer)}
	invoker, err := NewModelInvoker(api)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := invoker.Arbitrate(context.Background(), candidate, reviews, nil, source, request, config, testInvocationTime); err != nil {
		t.Fatal(err)
	}
	prompt := api.request.Messages[1].Content
	for _, want := range []string{"USER_DATA_JSON", "missed-escalation", request.TargetFiles[0], "Updated label"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the arbiter was not shown %q", want)
		}
	}
	system := api.request.Messages[0].Content
	for _, want := range []string{"untrusted data", "acceptance conditions", "overrule_reviewer", "instruct_implementer"} {
		if !strings.Contains(system, want) {
			t.Errorf("the arbiter's contract lacks %q", want)
		}
	}
}

// One of two rulings, and exactly the fields that ruling means something
// with. A ruling that tells the next round nothing, or sets nothing aside,
// leaves the delivery exactly where it was while claiming to have decided
// it — which is the failure the whole verb exists to remove.
func TestARulingCarriesWhatItsOwnKindNeeds(t *testing.T) {
	sound := ReadinessAssumption{Kind: AssumptionArbiterRuling, Statement: "依頼は抑制だけを求めている。", Evidence: "チケット本文。"}
	objection := []OverruledFinding{{ReviewerID: "review-b", Code: "missed-escalation", Path: "README.md", Reason: "範囲外です。"}}
	for _, testCase := range []struct {
		name        string
		ruling      string
		instruction string
		overruled   []OverruledFinding
		assumption  ReadinessAssumption
	}{
		{"an instruction that says nothing", RulingInstructImplementer, "   ", nil, sound},
		{"an instruction that is absent", RulingInstructImplementer, "", nil, sound},
		{"an instructing ruling setting an objection aside", RulingInstructImplementer, "満たすこと。", objection, sound},
		{"an overruling setting nothing aside", RulingOverruleReviewer, "", nil, sound},
		{"an overruling carrying an instruction", RulingOverruleReviewer, "満たすこと。", objection, sound},
		{"an overruling naming one objection twice", RulingOverruleReviewer, "", append(append([]OverruledFinding(nil), objection...), objection[0]), sound},
		{"a ruling of neither kind", "send_it_back", "満たすこと。", nil, sound},
		{"a ruling that records no assumption", RulingInstructImplementer, "満たすこと。", nil, ReadinessAssumption{}},
		{"an assumption filed under another kind", RulingInstructImplementer, "満たすこと。", nil,
			ReadinessAssumption{Kind: "repository_convention", Statement: sound.Statement, Evidence: sound.Evidence}},
		{"an assumption that states nothing", RulingInstructImplementer, "満たすこと。", nil,
			ReadinessAssumption{Kind: AssumptionArbiterRuling, Statement: "  ", Evidence: sound.Evidence}},
	} {
		if err := validateRulingBody(testCase.ruling, testCase.instruction, testCase.overruled, testCase.assumption); err == nil {
			t.Errorf("%s was accepted", testCase.name)
		}
	}
	// And the two sound shapes are accepted.
	if err := validateRulingBody(RulingInstructImplementer, "満たすこと。", nil, sound); err != nil {
		t.Errorf("a sound instructing ruling was refused: %v", err)
	}
	if err := validateRulingBody(RulingOverruleReviewer, "", objection, sound); err != nil {
		t.Errorf("a sound overruling was refused: %v", err)
	}
}
