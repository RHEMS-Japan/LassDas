package worker

import (
	"automation.internal/ticket-ingress/internal/probe"
	"context"
	"regexp"
	"strings"
	"testing"
)

func testReadyOutput() ModelReadinessOutput {
	return ModelReadinessOutput{
		Decision: ReadinessOutcomeReady, Questions: []ReadinessQuestion{}, Assumptions: []ReadinessAssumption{}, RejectCode: "",
	}
}

// The assumption cap is the destination's, and the default is sixty-four: a
// reception that asks as little as it can settles the rest itself, and every
// point it settles is one more line on the record the requester reads. Eight
// was too few for a well-specified ticket even when the reception still
// asked (2026-08-17); sixteen is too few once it decides. Both sides of the
// boundary are pinned so neither the validator nor the prompt schema can
// drift alone unnoticed.
func TestAssumptionsAreCappedByTheDestinationNotTheCode(t *testing.T) {
	build := func(count int) ModelReadinessOutput {
		output := testReadyOutput()
		for index := 0; index < count; index++ {
			output.Assumptions = append(output.Assumptions, ReadinessAssumption{
				Kind:      AssumptionRepositoryConvention,
				Statement: "settled point " + strings.Repeat("s", index+1),
				Evidence:  "written in the ticket",
			})
		}
		return output
	}
	policy := defaultTestPolicy()
	if err := policy.refuse(build(DefaultAssumptionMaxItems)); err != nil {
		t.Fatalf("sixty-four assumptions must be recordable: %v", err)
	}
	if err := policy.refuse(build(DefaultAssumptionMaxItems + 1)); err == nil {
		t.Fatal("a sixty-fifth assumption must be refused")
	}
	if !strings.Contains(readinessJSONSchema(policy), `"assumptions":{"type":"array","maxItems":64,`) {
		t.Fatal("the prompt schema no longer matches the default assumption cap")
	}
	// The sealed record is held to the protocol ceiling instead, so a
	// destination that lowers its number later cannot make the records it
	// already sealed unreadable.
	if err := validateModelReadinessOutput(build(DefaultAssumptionMaxItems + 1)); err != nil {
		t.Fatalf("a sealed assessment inside the ceiling must stay readable: %v", err)
	}
	if err := validateModelReadinessOutput(build(AssumptionItemCeiling + 1)); err == nil {
		t.Fatal("an assessment past the protocol ceiling must be rejected")
	}
}

func testClarificationOutput() ModelReadinessOutput {
	return ModelReadinessOutput{
		Decision: ReadinessOutcomeClarification,
		Questions: []ReadinessQuestion{{
			ID: "Q1", Dimension: "user_visible_behavior",
			Question: "Should the label change on both language screens?", WhyBlocking: "The choice changes which screens the user sees updated.",
			Choices: []ReadinessChoice{
				{ID: "a", Label: "Japanese only", Effect: "The English screen keeps the old label."},
				{ID: "b", Label: "Both languages", Effect: "Both screens show the new label."},
			},
		}},
		Assumptions: []ReadinessAssumption{}, RejectCode: "",
	}
}

func testAssessmentPair(t *testing.T, attempt int, output ModelReadinessOutput, checkVerdict string, source SourceSnapshot, request TicketRequest, config Config) (ReadinessAssessment, ReadinessCheck) {
	t.Helper()
	assessorInvocation := validTestInvocation(config.Models.Readiness.Assessor)
	assessorInvocation.RequestID = assessorInvocation.RequestID + "-a" + string(rune('0'+attempt))
	assessment, err := NewReadinessAssessment(attempt, output, nil, nil, source, request, config, assessorInvocation, testInvocationTime)
	if err != nil {
		t.Fatal(err)
	}
	checkOutput := ModelReadinessCheckOutput{Verdict: checkVerdict, Reasons: []ReadinessCheckReason{}}
	if checkVerdict == "fail" {
		checkOutput.Reasons = []ReadinessCheckReason{{Code: "false-ready", Message: "A blocking ambiguity remains."}}
	}
	checkerInvocation := validTestInvocation(config.Models.Readiness.Checker)
	checkerInvocation.RequestID = checkerInvocation.RequestID + "-c" + string(rune('0'+attempt))
	check, err := NewReadinessCheck(checkOutput, assessment, source, request, config, checkerInvocation, testInvocationTime)
	if err != nil {
		t.Fatal(err)
	}
	return assessment, check
}

func testReadyDecision(t *testing.T, source SourceSnapshot, request TicketRequest, config Config) ReadinessDecision {
	t.Helper()
	assessment, check := testAssessmentPair(t, 1, testReadyOutput(), "pass", source, request, config)
	decision, err := DecideReadiness(t.Context(), []ReadinessAssessment{assessment}, []ReadinessCheck{check}, source, request, config, nil)
	if err != nil {
		t.Fatal(err)
	}
	return decision
}

func TestAssessReadinessSealsAssessment(t *testing.T) {
	config, request, source := validArtifactFixture(t)
	api := &fakeChatAPI{output: chatOutput(`{"decision":"ready","questions":[],"assumptions":[],"reject_code":""}`)}
	invoker, err := NewModelInvoker(api)
	if err != nil {
		t.Fatal(err)
	}
	assessment, usage, err := invoker.AssessReadiness(context.Background(), 1, nil, nil, nil, nil, source, request, config, nil)
	if err != nil {
		t.Fatal(err)
	}
	if assessment.Decision != ReadinessOutcomeReady || assessment.SourceSHA256 != source.SourceSHA256 || usage.RequestID != "request-123" {
		t.Fatalf("assessment = %+v, usage = %+v", assessment, usage)
	}
	if api.request.Model != config.Models.Readiness.Assessor.Model || api.request.ReasoningEffort != "high" {
		t.Fatalf("chat request = %+v", api.request)
	}
	if err := assessment.Validate(source, request, config); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestAssessReadinessReadsContentDespiteInconsistentDecision(t *testing.T) {
	config, request, source := validArtifactFixture(t)
	invoker, _ := NewModelInvoker(&fakeChatAPI{output: chatOutput(`{"decision":"ready","questions":[` + oneDraftedQuestion + `],"assumptions":[],"reject_code":""}`)})
	if assessment, _, err := invoker.AssessReadiness(context.Background(), 1, nil, nil, nil, nil, source, request, config, nil); err != nil || assessment.Decision != ReadinessOutcomeClarification || len(assessment.Questions) != 1 {
		t.Fatalf("a readable question was lost: assessment=%+v err=%v", assessment, err)
	}
	invoker, _ = NewModelInvoker(&fakeChatAPI{output: chatOutput(`{"decision":"clarification_required","questions":[],"assumptions":[],"reject_code":""}`)})
	if assessment, _, err := invoker.AssessReadiness(context.Background(), 1, nil, nil, nil, nil, source, request, config, nil); err != nil || assessment.Decision != ReadinessOutcomeReady {
		t.Fatalf("an empty question set became an unread answer: assessment=%+v err=%v", assessment, err)
	}
	invoker, _ = NewModelInvoker(&fakeChatAPI{output: chatOutput(`{"decision":"reject","questions":[],"assumptions":[],"reject_code":""}`)})
	if assessment, _, err := invoker.AssessReadiness(context.Background(), 1, nil, nil, nil, nil, source, request, config, nil); err != nil || assessment.Decision != ReadinessOutcomeReject || assessment.RejectCode != "" {
		t.Fatalf("a readable refusal without a reason was lost: assessment=%+v err=%v", assessment, err)
	}
}

func TestReadinessQuestionShapeIsStrict(t *testing.T) {
	output := testClarificationOutput()
	output.Questions[0].ID = "Q2"
	if err := validateModelReadinessOutput(output); err == nil {
		t.Fatal("validateModelReadinessOutput() accepted a non-sequential question id")
	}
	// However many choices a question offers is how many it offers. One is
	// a question the requester would have read fine.
	output = testClarificationOutput()
	output.Questions[0].Choices = output.Questions[0].Choices[:1]
	if err := validateModelReadinessOutput(output); err != nil {
		t.Fatalf("a question with one choice was refused: %v", err)
	}
	output = testClarificationOutput()
	output.Questions[0].Choices[1].ID = "c"
	if err := validateModelReadinessOutput(output); err == nil {
		t.Fatal("validateModelReadinessOutput() accepted out-of-order choice ids")
	}
	output = testClarificationOutput()
	output.Questions[0].Dimension = "styling_preference"
	if err := validateModelReadinessOutput(output); err == nil {
		t.Fatal("validateModelReadinessOutput() accepted an unknown dimension")
	}
	// A question with no choices at all is still a question a requester can
	// read and answer in their own words.
	output = testClarificationOutput()
	output.Questions[0].Choices = []ReadinessChoice{}
	if err := validateModelReadinessOutput(output); err != nil {
		t.Fatalf("a question with no listed choices was refused: %v", err)
	}
	unresolvable := ModelReadinessOutput{Decision: ReadinessAssessorUnresolvable, Questions: []ReadinessQuestion{}, Assumptions: []ReadinessAssumption{}}
	if err := validateModelReadinessOutput(unresolvable); err != nil {
		t.Fatalf("validateModelReadinessOutput() rejected an unresolvable decision: %v", err)
	}
}

func TestAssessReadinessRetryRequiresFailedPrior(t *testing.T) {
	config, request, source := validArtifactFixture(t)
	assessment, failedCheck := testAssessmentPair(t, 1, testReadyOutput(), "fail", source, request, config)
	api := &fakeChatAPI{output: chatOutput(`{"decision":"ready","questions":[],"assumptions":[],"reject_code":""}`)}
	invoker, _ := NewModelInvoker(api)

	if _, _, err := invoker.AssessReadiness(context.Background(), 2, nil, nil, nil, nil, source, request, config, nil); err == nil {
		t.Fatal("AssessReadiness() accepted a retry without the failed prior attempt")
	}
	passedAssessment, passedCheck := testAssessmentPair(t, 1, testReadyOutput(), "pass", source, request, config)
	if _, _, err := invoker.AssessReadiness(context.Background(), 2, &passedAssessment, &passedCheck, nil, nil, source, request, config, nil); err == nil {
		t.Fatal("AssessReadiness() accepted a retry after a passing check")
	}
	retried, _, err := invoker.AssessReadiness(context.Background(), 2, &assessment, &failedCheck, nil, nil, source, request, config, nil)
	if err != nil || retried.Attempt != 2 {
		t.Fatalf("retry = %+v, error = %v", retried, err)
	}
}

func TestCheckReadinessBindsAssessment(t *testing.T) {
	config, request, source := validArtifactFixture(t)
	assessment, _ := testAssessmentPair(t, 1, testReadyOutput(), "pass", source, request, config)
	api := &fakeChatAPI{output: chatOutput(`{"verdict":"pass","reasons":[]}`)}
	invoker, _ := NewModelInvoker(api)
	check, _, err := invoker.CheckReadiness(context.Background(), assessment, nil, nil, source, request, config, nil)
	if err != nil {
		t.Fatal(err)
	}
	if check.AssessmentSHA256 != assessment.AssessmentSHA256 || check.CheckerID != config.Models.Readiness.Checker.ID {
		t.Fatalf("check = %+v", check)
	}
	if api.request.ResponseFormat == nil || api.request.ResponseFormat.Type != "json_schema" {
		t.Fatal("structured checker did not receive a JSON schema")
	}
	// A tampered assessment is refused before any model call — not asked
	// about three times inside the retry loop.
	counting := &sequenceChatAPI{outputs: []*ChatResponse{chatOutput(`{"verdict":"pass","reasons":[]}`)}}
	invoker, _ = NewModelInvoker(counting)
	tampered := assessment
	tampered.Decision = ReadinessOutcomeClarification
	if _, _, err := invoker.CheckReadiness(context.Background(), tampered, nil, nil, source, request, config, nil); err == nil {
		t.Fatal("CheckReadiness() accepted a tampered assessment")
	}
	if len(counting.requests) != 0 {
		t.Fatalf("a tampered assessment reached the model %d times", len(counting.requests))
	}
}

func TestDecideReadinessOutcomes(t *testing.T) {
	config, request, source := validArtifactFixture(t)

	ready := testReadyDecision(t, source, request, config)
	if ready.Outcome != ReadinessOutcomeReady || len(ready.Questions) != 0 {
		t.Fatalf("decision = %+v", ready)
	}
	if err := ready.ValidateBinding(source, request, config); err != nil {
		t.Fatalf("ValidateBinding() error = %v", err)
	}

	assessment, check := testAssessmentPair(t, 1, testClarificationOutput(), "pass", source, request, config)
	clarification, err := DecideReadiness(t.Context(), []ReadinessAssessment{assessment}, []ReadinessCheck{check}, source, request, config, nil)
	if err != nil || clarification.Outcome != ReadinessOutcomeClarification || len(clarification.Questions) != 1 {
		t.Fatalf("decision = %+v, error = %v", clarification, err)
	}

	failedOnce, failedCheck := testAssessmentPair(t, 1, testReadyOutput(), "fail", source, request, config)
	if _, err := DecideReadiness(t.Context(), []ReadinessAssessment{failedOnce}, []ReadinessCheck{failedCheck}, source, request, config, nil); err == nil {
		t.Fatal("DecideReadiness(t.Context(), , nil) sealed a decision before the required retry")
	}

	secondAssessment, secondCheck := testAssessmentPair(t, 2, testClarificationOutput(), "fail", source, request, config)
	if _, err := DecideReadiness(t.Context(), []ReadinessAssessment{failedOnce, secondAssessment}, []ReadinessCheck{failedCheck, secondCheck}, source, request, config, nil); err == nil {
		t.Fatal("DecideReadiness(t.Context(), , nil) sealed a decision before the final permitted attempt")
	}

	thirdAssessment, thirdCheck := testAssessmentPair(t, 3, testClarificationOutput(), "fail", source, request, config)
	unresolved, err := DecideReadiness(t.Context(), []ReadinessAssessment{failedOnce, secondAssessment, thirdAssessment}, []ReadinessCheck{failedCheck, secondCheck, thirdCheck}, source, request, config, nil)
	if err != nil || unresolved.Outcome != ReadinessOutcomeReady || !unresolved.InconclusiveReading || len(unresolved.Questions) != 0 {
		t.Fatalf("decision = %+v, error = %v", unresolved, err)
	}

	passedFirst, passedCheck := testAssessmentPair(t, 1, testReadyOutput(), "pass", source, request, config)
	if _, err := DecideReadiness(t.Context(), []ReadinessAssessment{passedFirst, secondAssessment}, []ReadinessCheck{passedCheck, secondCheck}, source, request, config, nil); err == nil {
		t.Fatal("DecideReadiness(t.Context(), , nil) accepted a retry after a passing check")
	}
}

func TestGenerateCandidateRequiresReadyDecision(t *testing.T) {
	config, request, source := validArtifactFixture(t)
	api := &fakeChatAPI{output: chatOutput(`{"files":[{"path":"client/src/components/Example.tsx","content":"export const label = 'Updated label';\n"}],"rationale":"Update the label."}`)}
	invoker, _ := NewModelInvoker(api)

	assessment, check := testAssessmentPair(t, 1, testClarificationOutput(), "pass", source, request, config)
	clarification, err := DecideReadiness(t.Context(), []ReadinessAssessment{assessment}, []ReadinessCheck{check}, source, request, config, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := invoker.GenerateCandidate(context.Background(), 1, clarification, nil, source, request, nil, nil, config); err == nil {
		t.Fatal("GenerateCandidate() accepted a clarification_required decision")
	}

	forged := testReadyDecision(t, source, request, config)
	forged.Outcome = ReadinessOutcomeReady
	forged.SourceSHA256 = strings.Repeat("d", 64)
	if _, _, err := invoker.GenerateCandidate(context.Background(), 1, forged, nil, source, request, nil, nil, config); err == nil {
		t.Fatal("GenerateCandidate() accepted a decision bound to a different source")
	}

	if _, _, err := invoker.GenerateCandidate(context.Background(), 1, testReadyDecision(t, source, request, config), nil, source, request, nil, nil, config); err != nil {
		t.Fatalf("GenerateCandidate() rejected a valid ready decision: %v", err)
	}
}

// A sound assessment must not be lost to its labels: an unknown dimension or
// assumption kind is coerced to its default, and known values survive
// untouched. A resumed live run died on exactly an unlisted assumption kind.
func TestNormalizeReadinessTaxonomyCoercesOnlyUnknownLabels(t *testing.T) {
	output := ModelReadinessOutput{
		Questions: []ReadinessQuestion{
			{ID: "Q1", Dimension: "acceptance_criterion"},
			{ID: "Q2", Dimension: "仕様の確認"},
			{ID: "Q3", Dimension: ""},
		},
		Assumptions: []ReadinessAssumption{
			{Kind: "repository_convention", Statement: "既存の通貨表記に合わせる", Evidence: "currency.ts"},
			{Kind: "scope_decision", Statement: "範囲は回答どおり", Evidence: "C1"},
			{Kind: "", Statement: "空の種別", Evidence: "-"},
		},
	}
	normalizeReadinessTaxonomy(&output)
	if output.Questions[0].Dimension != "acceptance_criterion" {
		t.Fatalf("known dimension was rewritten: %q", output.Questions[0].Dimension)
	}
	if output.Questions[1].Dimension != "user_visible_behavior" || output.Questions[2].Dimension != "user_visible_behavior" {
		t.Fatalf("unknown dimensions = %q, %q", output.Questions[1].Dimension, output.Questions[2].Dimension)
	}
	if output.Assumptions[0].Kind != "repository_convention" {
		t.Fatalf("known assumption kind was rewritten: %q", output.Assumptions[0].Kind)
	}
	if output.Assumptions[1].Kind != "non_user_visible_implementation" || output.Assumptions[2].Kind != "non_user_visible_implementation" {
		t.Fatalf("unknown assumption kinds = %q, %q", output.Assumptions[1].Kind, output.Assumptions[2].Kind)
	}
	for _, assumption := range output.Assumptions {
		if assumption.Statement == "" {
			t.Fatal("statement was lost in normalization")
		}
	}
	// The normalized assumptions now pass the exact validation that killed
	// the live run, evidence intact.
	validatable := ModelReadinessOutput{Decision: ReadinessOutcomeReady, Assumptions: output.Assumptions}
	if err := validateModelReadinessOutput(validatable); err != nil {
		t.Fatalf("normalized output must validate: %v", err)
	}
}

func testTwoQuestionOutput() ModelReadinessOutput {
	output := testClarificationOutput()
	output.Questions = append(output.Questions, ReadinessQuestion{
		ID: "Q2", Dimension: "acceptance_criterion",
		Question: "What should an empty input produce?", WhyBlocking: "The choice changes the accepted result.",
		Choices: []ReadinessChoice{
			{ID: "a", Label: "An empty result", Effect: "The user sees an empty list."},
			{ID: "b", Label: "An error", Effect: "The user sees a validation message."},
		},
	})
	return output
}

func testCheckedPair(t *testing.T, attempt int, output ModelReadinessOutput, checkOutput ModelReadinessCheckOutput, source SourceSnapshot, request TicketRequest, config Config) (ReadinessAssessment, ReadinessCheck) {
	t.Helper()
	assessorInvocation := validTestInvocation(config.Models.Readiness.Assessor)
	assessorInvocation.RequestID = assessorInvocation.RequestID + "-a" + string(rune('0'+attempt))
	assessment, err := NewReadinessAssessment(attempt, output, nil, nil, source, request, config, assessorInvocation, testInvocationTime)
	if err != nil {
		t.Fatal(err)
	}
	checkerInvocation := validTestInvocation(config.Models.Readiness.Checker)
	checkerInvocation.RequestID = checkerInvocation.RequestID + "-c" + string(rune('0'+attempt))
	check, err := NewReadinessCheck(checkOutput, assessment, source, request, config, checkerInvocation, testInvocationTime)
	if err != nil {
		t.Fatal(err)
	}
	return assessment, check
}

// The final attempt used to end readiness_unresolved whenever the checker
// failed it - even when the checker's only objection named one over-asked
// question and a checker-approved question stood right next to it. Measured
// live: the valid empty-input question reached the requester only after an
// operator rewrote the ticket.
func TestDecideReadinessSurvivingQuestionsOutliveABlamedOne(t *testing.T) {
	config, request, source := validArtifactFixture(t)
	chain := func(finalCheck ModelReadinessCheckOutput) (ReadinessDecision, error) {
		first, firstCheck := testAssessmentPair(t, 1, testReadyOutput(), "fail", source, request, config)
		second, secondCheck := testAssessmentPair(t, 2, testReadyOutput(), "fail", source, request, config)
		third, thirdCheck := testCheckedPair(t, 3, testTwoQuestionOutput(), finalCheck, source, request, config)
		return DecideReadiness(t.Context(),
			[]ReadinessAssessment{first, second, third},
			[]ReadinessCheck{firstCheck, secondCheck, thirdCheck},
			source, request, config, nil)
	}

	// Every objection names Q1; Q2 survives, renumbered to Q1.
	decision, err := chain(ModelReadinessCheckOutput{Verdict: "fail", Reasons: []ReadinessCheckReason{
		{Code: "false-block", Message: "Q1 is answerable from the provided source.", QuestionID: "Q1"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Outcome != ReadinessOutcomeClarification || len(decision.Questions) != 1 {
		t.Fatalf("decision = %+v", decision)
	}
	if decision.Questions[0].ID != "Q1" || !strings.Contains(decision.Questions[0].Question, "empty input") {
		t.Fatalf("the surviving question was not renumbered from the invariant start: %+v", decision.Questions[0])
	}

	// A set-level objection (no question_id) discards every unchecked question,
	// but continues with the original request instead of ending the delivery.
	decision, err = chain(ModelReadinessCheckOutput{Verdict: "fail", Reasons: []ReadinessCheckReason{
		{Code: "false-block", Message: "Q1 is answerable from the provided source.", QuestionID: "Q1"},
		{Code: "inconsistent-decision", Message: "The assessment contradicts itself."},
	}})
	if err != nil || decision.Outcome != ReadinessOutcomeReady || !decision.InconclusiveReading || len(decision.Questions) != 0 {
		t.Fatalf("decision = %+v, error = %v", decision, err)
	}

	// Blaming every question leaves nothing to surface.
	decision, err = chain(ModelReadinessCheckOutput{Verdict: "fail", Reasons: []ReadinessCheckReason{
		{Code: "false-block", Message: "Q1 is answerable from the provided source.", QuestionID: "Q1"},
		{Code: "invalid-question", Message: "Q2 duplicates the first question.", QuestionID: "Q2"},
	}})
	if err != nil || decision.Outcome != ReadinessOutcomeReady || !decision.InconclusiveReading || len(decision.Questions) != 0 {
		t.Fatalf("decision = %+v, error = %v", decision, err)
	}

	// A failed non-clarification assessment also continues without adopting it.
	first, firstCheck := testAssessmentPair(t, 1, testReadyOutput(), "fail", source, request, config)
	second, secondCheck := testAssessmentPair(t, 2, testReadyOutput(), "fail", source, request, config)
	third, thirdCheck := testAssessmentPair(t, 3, testReadyOutput(), "fail", source, request, config)
	decision, err = DecideReadiness(t.Context(), []ReadinessAssessment{first, second, third}, []ReadinessCheck{firstCheck, secondCheck, thirdCheck}, source, request, config, nil)
	if err != nil || decision.Outcome != ReadinessOutcomeReady || !decision.InconclusiveReading {
		t.Fatalf("decision = %+v, error = %v", decision, err)
	}
}

// The rescue is sealed and re-derivable: the decision that surfaces surviving
// questions must round-trip through full validation like any other outcome.
func TestDecideReadinessRescueRoundTripsThroughValidate(t *testing.T) {
	config, request, source := validArtifactFixture(t)
	first, firstCheck := testAssessmentPair(t, 1, testReadyOutput(), "fail", source, request, config)
	second, secondCheck := testAssessmentPair(t, 2, testReadyOutput(), "fail", source, request, config)
	third, thirdCheck := testCheckedPair(t, 3, testTwoQuestionOutput(), ModelReadinessCheckOutput{
		Verdict: "fail", Reasons: []ReadinessCheckReason{
			{Code: "false-block", Message: "Q2 is answerable from the provided source.", QuestionID: "Q2"},
		}}, source, request, config)
	assessments := []ReadinessAssessment{first, second, third}
	checks := []ReadinessCheck{firstCheck, secondCheck, thirdCheck}
	decision, err := DecideReadiness(t.Context(), assessments, checks, source, request, config, nil)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Outcome != ReadinessOutcomeClarification || len(decision.Questions) != 1 || decision.Questions[0].ID != "Q1" {
		t.Fatalf("decision = %+v", decision)
	}
	if err := decision.Validate(assessments, checks, source, request, config); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	tampered := decision
	tampered.Outcome = ReadinessOutcomeUnresolved
	tampered.Questions = []ReadinessQuestion{}
	if err := tampered.Validate(assessments, checks, source, request, config); err == nil {
		t.Fatal("Validate() accepted a decision whose rescue was stripped")
	}
}

// A checker cannot blame a question the assessment never asked, and a blamed
// id must look like a question id at all.
func TestReadinessCheckQuestionBlameIsBound(t *testing.T) {
	config, request, source := validArtifactFixture(t)
	assessorInvocation := validTestInvocation(config.Models.Readiness.Assessor)
	assessment, err := NewReadinessAssessment(1, testClarificationOutput(), nil, nil, source, request, config, assessorInvocation, testInvocationTime)
	if err != nil {
		t.Fatal(err)
	}
	checkerInvocation := validTestInvocation(config.Models.Readiness.Checker)
	if _, err := NewReadinessCheck(ModelReadinessCheckOutput{Verdict: "fail", Reasons: []ReadinessCheckReason{
		{Code: "false-block", Message: "Blames a question that does not exist.", QuestionID: "Q7"},
	}}, assessment, source, request, config, checkerInvocation, testInvocationTime); err == nil {
		t.Fatal("NewReadinessCheck() accepted blame for an absent question")
	}
	if err := validateModelReadinessCheckOutput(ModelReadinessCheckOutput{Verdict: "fail", Reasons: []ReadinessCheckReason{
		{Code: "false-block", Message: "Malformed id.", QuestionID: "question-one"},
	}}); err == nil {
		t.Fatal("validateModelReadinessCheckOutput() accepted a malformed question id")
	}
}

// The rescue honors question-scoped codes only. A set-level defect - a scope
// miss, a wrong decision, a code this engine has never heard of - condemns
// the whole set even when the checker attributed it to a question: the
// checker's veto must not be dissolvable by how it fills in a label.
func TestDecideReadinessRescueIgnoresSetLevelCodes(t *testing.T) {
	config, request, source := validArtifactFixture(t)
	for _, code := range []string{"scope-miss", "false-ready", "inconsistent-decision", "secret-request", "novel-code"} {
		first, firstCheck := testAssessmentPair(t, 1, testReadyOutput(), "fail", source, request, config)
		second, secondCheck := testAssessmentPair(t, 2, testReadyOutput(), "fail", source, request, config)
		third, thirdCheck := testCheckedPair(t, 3, testTwoQuestionOutput(), ModelReadinessCheckOutput{
			Verdict: "fail", Reasons: []ReadinessCheckReason{
				{Code: code, Message: "A defect attributed to a question.", QuestionID: "Q1"},
			}}, source, request, config)
		decision, err := DecideReadiness(t.Context(),
			[]ReadinessAssessment{first, second, third},
			[]ReadinessCheck{firstCheck, secondCheck, thirdCheck},
			source, request, config, nil)
		if err != nil || decision.Outcome != ReadinessOutcomeReady || !decision.InconclusiveReading || len(decision.Questions) != 0 {
			t.Fatalf("code %s: decision = %+v, error = %v", code, decision, err)
		}
	}
}

// A hallucinated question id must not end the run: it is dropped to
// set-level before sealing, keeping the reason's full force and the
// fail-closed outcome. Two live tickets already died to mislabeled
// enum-ish fields; this is the same class of entry.
func TestCheckReadinessDropsAHallucinatedQuestionID(t *testing.T) {
	config, request, source := validArtifactFixture(t)
	assessorInvocation := validTestInvocation(config.Models.Readiness.Assessor)
	assessment, err := NewReadinessAssessment(1, testClarificationOutput(), nil, nil, source, request, config, assessorInvocation, testInvocationTime)
	if err != nil {
		t.Fatal(err)
	}
	api := &fakeChatAPI{output: chatOutput(`{"verdict":"fail","reasons":[{"code":"false-block","message":"Blames a question that does not exist.","question_id":"Q3"}]}`)}
	invoker, _ := NewModelInvoker(api)
	check, _, err := invoker.CheckReadiness(context.Background(), assessment, nil, nil, source, request, config, nil)
	if err != nil {
		t.Fatalf("a hallucinated question id killed the check: %v", err)
	}
	if len(check.Reasons) != 1 || check.Reasons[0].QuestionID != "" {
		t.Fatalf("the hallucinated id was not dropped to set-level: %+v", check.Reasons)
	}
}

// The reception measures nothing: a question whose choices carry latencies
// and record numbers, or an assumption that cites a record, is refused as
// invented — the live case handed the requester three made-up records to
// choose from and preserved the choice as an answer.
func TestReadinessRefusesFabricatedMeasurements(t *testing.T) {
	needsDesign := true
	question := func(label string) ModelReadinessOutput {
		return ModelReadinessOutput{
			Decision: ReadinessOutcomeClarification,
			Questions: []ReadinessQuestion{{
				ID: "Q1", Dimension: "acceptance_criterion", Question: "Which basis should the guide use?", WhyBlocking: "The threshold differs by basis.",
				Choices: []ReadinessChoice{{ID: "a", Label: label, Effect: "The guide states that basis."}, {ID: "b", Label: "Through the public entry point", Effect: "The guide states that basis."}},
			}},
			RequestKind: "change", NeedsDesign: &needsDesign,
		}
	}
	ticket := "Write the guide with the measured thresholds and their record numbers."
	for _, invented := range []string{
		"Inside the cluster (HTTP 200, under 50ms, 記録番号: REC-2026-HEALTH-INT01)",
		"Inside the cluster, record number: m-0007",
		"Inside the cluster, Record ID: m-12345",
		"Inside the cluster (REC-2026-HEALTH-INT01)",
	} {
		if err := refuseFabricatedEvidence(question(invented), ticket); err == nil || !strings.Contains(err.Error(), "never made") {
			t.Fatalf("%q: err = %v, want a refusal", invented, err)
		}
		// A sealed assessment is read back through the output validation,
		// which does not carry this rule: a record sealed before it stays
		// readable.
		if err := validateModelReadinessOutput(question(invented)); err != nil {
			t.Fatalf("%q: the read-back validation refused it: %v", invented, err)
		}
	}
	if err := refuseFabricatedEvidence(question("From inside the cluster"), ticket); err != nil {
		t.Fatalf("a basis described in words was refused: %v", err)
	}
	// A record the ticket itself names is the requester's, not invented —
	// whatever the width of the colon; and a ticket that names one record
	// does not license another.
	quoted := "Use the requester's own record 記録番号: REC-77 as the baseline"
	if err := refuseFabricatedEvidence(question(quoted), "The baseline is 記録番号：REC-77 from last week."); err != nil {
		t.Fatalf("a record quoted from the ticket was refused: %v", err)
	}
	if err := refuseFabricatedEvidence(question("Inside the cluster (記録番号: REC-2026-HEALTH-INT01)"), "The baseline is 記録番号: REC-77."); err == nil {
		t.Fatal("a ticket naming one record licensed another")
	}
	if err := refuseFabricatedEvidence(question("Inside the cluster, record number: m-9999"), "Compare with record number: m-0007."); err == nil {
		t.Fatal("a ticket naming one record number licensed another")
	}
	if err := refuseFabricatedEvidence(question("The rec-room label stays"), ticket); err != nil {
		t.Fatalf("a lowercase word was taken for a record: %v", err)
	}
	// The identifier ends where the identifier ends: a bracket or a
	// sentence continuing after it (Japanese has no space to stop at) is
	// not part of it.
	if err := refuseFabricatedEvidence(question("（記録番号: REC-77）とする"), "基準は 記録番号：REC-77。"); err != nil {
		t.Fatalf("a bracketed quote of the ticket's record was refused: %v", err)
	}
	if err := refuseFabricatedEvidence(question("Record ID: REC-77, then compare"), "Compare with record REC-77 from last week."); err != nil {
		t.Fatalf("a record followed by a comma was refused: %v", err)
	}
	withAssumption := question("From inside the cluster")
	withAssumption.Assumptions = []ReadinessAssumption{{Kind: "non_user_visible_implementation", Statement: "The guide cites 記録番号: REC-2026-HEALTH-EXT01.", Evidence: "ticket"}}
	if err := refuseFabricatedEvidence(withAssumption, ticket); err == nil || !strings.Contains(err.Error(), "never made") {
		t.Fatalf("an assumption citing an invented record was accepted: %v", err)
	}
}

// The checker answers under a strict schema: every defect code its prompt
// names must be in the schema's enum, or the prompt asks for an answer the
// checker cannot give.
func TestCheckerPromptCodesAreInItsSchema(t *testing.T) {
	prompt := readinessCheckSystemPrompt(ModelEndpoint{Lens: "test"}, defaultTestPolicy())
	schema := readinessCheckJSONSchema(defaultTestPolicy())
	codes := regexp.MustCompile(`(?m)^- ([a-z]+(?:-[a-z]+)+):`).FindAllStringSubmatch(prompt, -1)
	if len(codes) < 7 {
		t.Fatalf("the prompt names %d codes; expected the defect list", len(codes))
	}
	enum := regexp.MustCompile(`"code":\{"type":"string","enum":\[([^\]]*)\]`).FindStringSubmatch(schema)
	if enum == nil {
		t.Fatal("the checker schema carries no code enum")
	}
	for _, code := range codes {
		if !strings.Contains(enum[1], `"`+code[1]+`"`) {
			t.Fatalf("the prompt names defect code %q, which the schema enum %s does not allow", code[1], enum[1])
		}
	}
	if !strings.Contains(enum[1], `"fabricated-evidence"`) {
		t.Fatal("the schema enum lacks fabricated-evidence")
	}
}

// The reception is told what the pipeline can measure and that a
// measurable point is not a requester's question: the catalogue travels in
// both prompts' USER_DATA_JSON, the assessor's contract carries the
// measurement rule and the text limits, and the checker's false-block names
// the catalogue (live: the reception, believing production out of reach,
// asked whether to write placeholders instead of measurements).
func TestReceptionKnowsTheCatalogueAndTheTextLimits(t *testing.T) {
	config, request, source := validArtifactFixture(t)
	config.Probes = []probe.Spec{{ID: "http.timing", Kind: probe.KindHTTP}, {ID: "k8s.workloads", Kind: probe.KindExec}}
	prompt, err := readinessPrompt(source, request, config, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, `"catalogue":[{"id":"http.timing","kind":"http"},{"id":"k8s.workloads","kind":"exec"}]`) {
		t.Errorf("assessor data lacks the catalogue:\n%s", prompt)
	}
	// No probes, or a destination whose design stage is off: nothing will
	// measure, so the catalogue is absent and the rule tells the assessor
	// to ask or assume as before.
	bare := config
	bare.Probes = nil
	if prompt, err := readinessPrompt(source, request, bare, nil, nil, nil, nil, nil); err != nil || strings.Contains(prompt, `"catalogue"`) {
		t.Errorf("a destination without probes was handed a catalogue (%v)", err)
	}
	off := config
	off.Consumers = append([]ConsumerConfig(nil), config.Consumers...)
	for index := range off.Consumers {
		off.Consumers[index].Design = &DesignConfig{Default: DesignDefaultOff}
	}
	if prompt, err := readinessPrompt(source, request, off, nil, nil, nil, nil, nil); err != nil || strings.Contains(prompt, `"catalogue"`) {
		t.Errorf("a destination with the design stage off was handed a catalogue (%v)", err)
	}
	assessment := ReadinessAssessment{Decision: ReadinessOutcomeReady, RequestKind: "change"}
	check, err := readinessCheckPrompt(assessment, source, request, config, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(check, `"catalogue":[{"id":"http.timing"`) {
		t.Errorf("checker data lacks the catalogue:\n%s", check)
	}
	system := readinessSystemPrompt(defaultTestPolicy())
	for _, want := range []string{"when USER_DATA_JSON.catalogue is present", "When USER_DATA_JSON.catalogue is present, an investigation stage follows you", "answer needs_design true whenever you left a point to that measurement", "When catalogue is absent, nothing is measured", "never assume that production or the repository cannot be reached or measured", "question and why_blocking are at most 2000 bytes", "reject_code matches ^[a-z][a-z0-9-]{1,63}$"} {
		if !strings.Contains(system, want) {
			t.Errorf("assessor contract lacks %q", want)
		}
	}
	checker := readinessCheckSystemPrompt(ModelEndpoint{Lens: "lens"}, defaultTestPolicy())
	if !strings.Contains(checker, "would be answered by a measurement with a probe in USER_DATA_JSON.catalogue when that key is present") {
		t.Error("checker's false-block does not name the catalogue")
	}
	if len(readinessCatalogue(Config{}, ConsumerConfig{})) != 0 {
		t.Error("an empty catalogue invented entries")
	}
}

// A refusal names the field, its size and the limit, so the next answer
// can be right (live: "question text is invalid" named nothing).
func TestReceptionRefusalsNameTheFieldAndTheLimit(t *testing.T) {
	needsDesign := true
	base := func() ModelReadinessOutput {
		return ModelReadinessOutput{Decision: ReadinessOutcomeClarification, RequestKind: "change", NeedsDesign: &needsDesign,
			Questions: []ReadinessQuestion{{ID: "Q1", Dimension: "acceptance_criterion", Question: "Which?", WhyBlocking: "It differs.",
				Choices: []ReadinessChoice{{ID: "a", Label: "one", Effect: "the first"}, {ID: "b", Label: "two", Effect: "the second"}}}}}
	}
	cases := []struct {
		name   string
		mutate func(*ModelReadinessOutput)
		want   string
	}{
		{"long question", func(o *ModelReadinessOutput) { o.Questions[0].Question = strings.Repeat("q", 2001) }, "question Q1 text is 2001 bytes (limit 2000)"},
		{"newline in why", func(o *ModelReadinessOutput) { o.Questions[0].WhyBlocking = "line\nbreak" }, "question Q1 why_blocking contains a newline"},
		{"long label", func(o *ModelReadinessOutput) { o.Questions[0].Choices[0].Label = strings.Repeat("l", 801) }, "question Q1 choice a label is 801 bytes (limit 800)"},
		{"empty effect", func(o *ModelReadinessOutput) { o.Questions[0].Choices[1].Effect = "" }, "question Q1 choice b effect is empty"},
		{"padded assumption", func(o *ModelReadinessOutput) {
			o.Assumptions = []ReadinessAssumption{{Kind: "repository_convention", Statement: " padded", Evidence: "e"}}
		}, "assumption 1 statement has leading or trailing whitespace"},
		{"empty evidence", func(o *ModelReadinessOutput) {
			o.Assumptions = []ReadinessAssumption{{Kind: "repository_convention", Statement: "s", Evidence: ""}}
		}, "assumption 1 evidence is empty"},
		{"control character in reject code", func(o *ModelReadinessOutput) {
			o.Decision = ReadinessOutcomeReject
			o.Questions = nil
			o.RejectCode = "Out\x00Of Scope"
		}, `reject_code "Out\x00Of Scope" has a control character`},
		{"long reject code is cut", func(o *ModelReadinessOutput) {
			o.Decision = ReadinessOutcomeReject
			o.Questions = nil
			o.RejectCode = strings.Repeat("x", 500)
		}, `reject_code "` + strings.Repeat("x", 64) + `…" is 500 bytes (limit 64)`},
		{"bad assumption kind", func(o *ModelReadinessOutput) {
			o.Assumptions = []ReadinessAssumption{{Kind: "guess", Statement: "s", Evidence: "e"}}
		}, `assumption 1 kind "guess" is not repository_convention, non_user_visible_implementation or defensible_default`},
	}
	for _, tc := range cases {
		output := base()
		tc.mutate(&output)
		err := validateModelReadinessOutput(output)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
}

// A record number the requester wrote in an earlier answer — this run's
// clarification or a preserved answer from an earlier ticket — is the
// requester's, and quoting it is not fabrication.
func TestEarlierAnswersLicenseRecordNumbers(t *testing.T) {
	needsDesign := true
	output := ModelReadinessOutput{Decision: ReadinessOutcomeClarification, RequestKind: "change", NeedsDesign: &needsDesign,
		Questions: []ReadinessQuestion{{ID: "Q1", Dimension: "acceptance_criterion", Question: "Keep the basis of REC-77?", WhyBlocking: "It was the requester's own basis.",
			Choices: []ReadinessChoice{{ID: "a", Label: "Keep REC-77 as the basis", Effect: "The guide cites it."}, {ID: "b", Label: "Drop it", Effect: "The guide cites nothing."}}}}}
	request := TicketRequest{Summary: "Write the guide", Request: "Write the guide from the measurements."}
	if err := refuseFabricatedEvidence(output, licensedEvidenceText(request, nil, nil)); err == nil {
		t.Fatal("a record number from nowhere was accepted")
	}
	preserved := []PreservedAnswer{{Name: "TKT-1.md", Content: "Adopted: the basis is record REC-77 (measured through the public entry point)."}}
	if err := refuseFabricatedEvidence(output, licensedEvidenceText(request, nil, preserved)); err != nil {
		t.Fatalf("a preserved answer's record number was refused: %v", err)
	}
	clarification := &ClarificationContext{Exchanges: []ClarificationExchange{{
		Questions: []ReadinessQuestion{{ID: "Q1", Question: "Which basis?", Choices: []ReadinessChoice{{ID: "a", Label: "REC-77 through the public entry point", Effect: "e"}, {ID: "b", Label: "REC-99 from inside", Effect: "e"}}}},
		Answers:   map[string]string{"Q1": "a"},
	}}}
	if err := refuseFabricatedEvidence(output, licensedEvidenceText(request, clarification, nil)); err != nil {
		t.Fatalf("an earlier answer's record number was refused: %v", err)
	}
	// The choice the requester did not take, and a question nobody
	// answered, are the reception's words: they license nothing.
	rejected := output
	rejected.Questions = []ReadinessQuestion{{ID: "Q1", Dimension: "acceptance_criterion", Question: "Keep REC-99?", WhyBlocking: "w",
		Choices: []ReadinessChoice{{ID: "a", Label: "Keep REC-99", Effect: "e"}, {ID: "b", Label: "Drop it", Effect: "e"}}}}
	if err := refuseFabricatedEvidence(rejected, licensedEvidenceText(request, clarification, nil)); err == nil {
		t.Fatal("a record number from a rejected choice was licensed")
	}
	unanswered := &ClarificationContext{Exchanges: []ClarificationExchange{{
		Questions: []ReadinessQuestion{{ID: "Q1", Question: "Keep REC-77?", Choices: []ReadinessChoice{{ID: "a", Label: "REC-77", Effect: "e"}}}},
		Answers:   map[string]string{},
	}}}
	if err := refuseFabricatedEvidence(output, licensedEvidenceText(request, unanswered, nil)); err == nil {
		t.Fatal("a record number from an unanswered question was licensed")
	}
}

// The reception itself (not only the helper) licenses a record number from
// a preserved answer: NewReadinessAssessment must build its sources from
// the answers it is given, or the helper is decoration.
func TestReceptionLicensesAPreservedAnswersRecordNumber(t *testing.T) {
	config, request, source := validArtifactFixture(t)
	needsDesign := true
	output := ModelReadinessOutput{Decision: ReadinessOutcomeClarification, RequestKind: "change", NeedsDesign: &needsDesign,
		Questions: []ReadinessQuestion{{ID: "Q1", Dimension: "acceptance_criterion", Question: "Keep the basis of REC-77?", WhyBlocking: "It was the requester's own basis.",
			Choices: []ReadinessChoice{{ID: "a", Label: "Keep REC-77 as the basis", Effect: "The guide cites it."}, {ID: "b", Label: "Drop it", Effect: "The guide cites nothing."}}}}}
	invocation := validTestInvocation(config.Models.Readiness.Assessor)
	if _, err := NewReadinessAssessment(1, output, nil, nil, source, request, config, invocation, testInvocationTime); err == nil {
		t.Fatal("a record number from nowhere was accepted by the reception")
	}
	preserved := []PreservedAnswer{{Name: "TKT-1.md", Content: "Adopted: the basis is record REC-77."}}
	if _, err := NewReadinessAssessment(1, output, nil, preserved, source, request, config, invocation, testInvocationTime); err != nil {
		t.Fatalf("the reception refused a preserved answer's record number: %v", err)
	}
}

// The reception judges an ordinary change request with no file in front of
// it: nothing is chosen before the change is made, so the snapshot it is
// bound to is empty. Both contracts have to say so. Telling the assessor
// that "the provided source files" were read for it, while sending none,
// turns the asking policy inside out - every implementation detail becomes
// something "not derivable from the provided source files" and therefore
// askable, and the requester is asked to read the repository after all.
func TestTheReceptionContractDoesNotClaimSourceFilesItNeverGot(t *testing.T) {
	config, draft := unnamedFilesDraft(t)
	request, err := draft.WithTargetFiles(nil, config)
	if err != nil {
		t.Fatal(err)
	}
	source, err := ReadSourceSnapshot(t.TempDir(), strings.Repeat("a", 40), request, config)
	if err != nil {
		t.Fatalf("ReadSourceSnapshot() error = %v", err)
	}
	if len(source.Files) != 0 {
		t.Fatalf("source files = %+v, want none", source.Files)
	}
	prompt, err := readinessPrompt(source, request, config, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, `"files":[]`) {
		t.Fatalf("the assessor's data does not carry an empty file set:\n%s", prompt)
	}

	system := readinessSystemPrompt(defaultTestPolicy())
	checker := readinessCheckSystemPrompt(ModelEndpoint{Lens: "lens"}, defaultTestPolicy())
	for _, contract := range []struct{ name, text string }{{"assessor", system}, {"checker", checker}} {
		if strings.Contains(contract.text, "provided source") {
			t.Errorf("the %s contract still claims source files were provided", contract.name)
		}
	}
	for _, want := range []string{
		"USER_DATA_JSON.source.files is empty for an ordinary change request",
		"the implementer reads the repository itself",
		"by reading the repository the change is made in",
		"anything that can be found by reading the repository",
		"anything that can be found there is not a requester's decision and is not a question",
		"Ask only what the requester alone can decide: user-visible behavior, acceptance criteria, pre-approved scope, safety or data behavior",
		"never reject a ticket because no file is shown to you",
	} {
		if !strings.Contains(system, want) {
			t.Errorf("the assessor contract lacks %q", want)
		}
	}
	for _, want := range []string{
		"is answerable from the ticket, by reading the repository the change is made in",
		"USER_DATA_JSON.source.files is empty for an ordinary change request and is never the boundary",
	} {
		if !strings.Contains(checker, want) {
			t.Errorf("the checker contract lacks %q", want)
		}
	}

	// A ticket promising a visible wording change still gets the files that
	// hold the wording, and the contract still describes that case.
	if !strings.Contains(system, "It carries files only when the ticket promises a visible wording change") {
		t.Error("the assessor contract no longer describes the wording ticket's files")
	}
}
