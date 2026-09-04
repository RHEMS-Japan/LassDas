package worker

import (
	"context"
	"strings"
	"testing"
)

func testReadyOutput() ModelReadinessOutput {
	return ModelReadinessOutput{
		Decision: ReadinessOutcomeReady, Questions: []ReadinessQuestion{}, Assumptions: []ReadinessAssumption{}, RejectCode: "",
	}
}

// The assumption cap is sixteen: a well-specified ticket measurably needed
// more than the old eight, and dying at the cap turned thoroughness into a
// model_failed terminal (2026-08-17). Both sides of the boundary are pinned
// so neither the validator nor the prompt schema can drift alone unnoticed.
func TestReadinessOutputAcceptsSixteenAssumptionsAndRejectsSeventeen(t *testing.T) {
	build := func(count int) ModelReadinessOutput {
		output := testReadyOutput()
		for index := 0; index < count; index++ {
			output.Assumptions = append(output.Assumptions, ReadinessAssumption{
				Kind:      "repository_convention",
				Statement: "settled point " + strings.Repeat("s", index+1),
				Evidence:  "written in the ticket",
			})
		}
		return output
	}
	if err := validateModelReadinessOutput(build(16)); err != nil {
		t.Fatalf("sixteen assumptions must validate: %v", err)
	}
	if err := validateModelReadinessOutput(build(17)); err == nil {
		t.Fatal("seventeen assumptions must be rejected")
	}
	if !strings.Contains(readinessJSONSchema(), `"assumptions":{"type":"array","maxItems":16,`) {
		t.Fatal("the prompt schema no longer matches the sixteen-assumption cap")
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
	decision, err := DecideReadiness([]ReadinessAssessment{assessment}, []ReadinessCheck{check}, source, request, config)
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
	assessment, usage, err := invoker.AssessReadiness(context.Background(), 1, nil, nil, nil, nil, source, request, config)
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

func TestAssessReadinessRejectsInconsistentDecision(t *testing.T) {
	config, request, source := validArtifactFixture(t)
	invoker, _ := NewModelInvoker(&fakeChatAPI{output: chatOutput(`{"decision":"ready","questions":[{"id":"Q1","dimension":"user_visible_behavior","question":"Which one?","why_blocking":"Changes result.","choices":[]}],"assumptions":[],"reject_code":""}`)})
	if _, _, err := invoker.AssessReadiness(context.Background(), 1, nil, nil, nil, nil, source, request, config); err == nil {
		t.Fatal("AssessReadiness() accepted ready with questions")
	}
	invoker, _ = NewModelInvoker(&fakeChatAPI{output: chatOutput(`{"decision":"clarification_required","questions":[],"assumptions":[],"reject_code":""}`)})
	if _, _, err := invoker.AssessReadiness(context.Background(), 1, nil, nil, nil, nil, source, request, config); err == nil {
		t.Fatal("AssessReadiness() accepted clarification without questions")
	}
	invoker, _ = NewModelInvoker(&fakeChatAPI{output: chatOutput(`{"decision":"reject","questions":[],"assumptions":[],"reject_code":""}`)})
	if _, _, err := invoker.AssessReadiness(context.Background(), 1, nil, nil, nil, nil, source, request, config); err == nil {
		t.Fatal("AssessReadiness() accepted reject without a reject code")
	}
}

func TestReadinessQuestionShapeIsStrict(t *testing.T) {
	output := testClarificationOutput()
	output.Questions[0].ID = "Q2"
	if err := validateModelReadinessOutput(output); err == nil {
		t.Fatal("validateModelReadinessOutput() accepted a non-sequential question id")
	}
	output = testClarificationOutput()
	output.Questions[0].Choices = output.Questions[0].Choices[:1]
	if err := validateModelReadinessOutput(output); err == nil {
		t.Fatal("validateModelReadinessOutput() accepted a single choice")
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
	output = testClarificationOutput()
	output.Questions[0].Choices = []ReadinessChoice{}
	if err := validateModelReadinessOutput(output); err == nil {
		t.Fatal("validateModelReadinessOutput() accepted a question without bounded choices")
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

	if _, _, err := invoker.AssessReadiness(context.Background(), 2, nil, nil, nil, nil, source, request, config); err == nil {
		t.Fatal("AssessReadiness() accepted a retry without the failed prior attempt")
	}
	passedAssessment, passedCheck := testAssessmentPair(t, 1, testReadyOutput(), "pass", source, request, config)
	if _, _, err := invoker.AssessReadiness(context.Background(), 2, &passedAssessment, &passedCheck, nil, nil, source, request, config); err == nil {
		t.Fatal("AssessReadiness() accepted a retry after a passing check")
	}
	retried, _, err := invoker.AssessReadiness(context.Background(), 2, &assessment, &failedCheck, nil, nil, source, request, config)
	if err != nil || retried.Attempt != 2 {
		t.Fatalf("retry = %+v, error = %v", retried, err)
	}
}

func TestCheckReadinessBindsAssessment(t *testing.T) {
	config, request, source := validArtifactFixture(t)
	assessment, _ := testAssessmentPair(t, 1, testReadyOutput(), "pass", source, request, config)
	api := &fakeChatAPI{output: chatOutput(`{"verdict":"pass","reasons":[]}`)}
	invoker, _ := NewModelInvoker(api)
	check, _, err := invoker.CheckReadiness(context.Background(), assessment, nil, nil, source, request, config)
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
	if _, _, err := invoker.CheckReadiness(context.Background(), tampered, nil, nil, source, request, config); err == nil {
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
	clarification, err := DecideReadiness([]ReadinessAssessment{assessment}, []ReadinessCheck{check}, source, request, config)
	if err != nil || clarification.Outcome != ReadinessOutcomeClarification || len(clarification.Questions) != 1 {
		t.Fatalf("decision = %+v, error = %v", clarification, err)
	}

	failedOnce, failedCheck := testAssessmentPair(t, 1, testReadyOutput(), "fail", source, request, config)
	if _, err := DecideReadiness([]ReadinessAssessment{failedOnce}, []ReadinessCheck{failedCheck}, source, request, config); err == nil {
		t.Fatal("DecideReadiness() sealed a decision before the required retry")
	}

	secondAssessment, secondCheck := testAssessmentPair(t, 2, testClarificationOutput(), "fail", source, request, config)
	if _, err := DecideReadiness([]ReadinessAssessment{failedOnce, secondAssessment}, []ReadinessCheck{failedCheck, secondCheck}, source, request, config); err == nil {
		t.Fatal("DecideReadiness() sealed a decision before the final permitted attempt")
	}

	thirdAssessment, thirdCheck := testAssessmentPair(t, 3, testClarificationOutput(), "fail", source, request, config)
	unresolved, err := DecideReadiness([]ReadinessAssessment{failedOnce, secondAssessment, thirdAssessment}, []ReadinessCheck{failedCheck, secondCheck, thirdCheck}, source, request, config)
	if err != nil || unresolved.Outcome != ReadinessOutcomeUnresolved || len(unresolved.Questions) != 0 {
		t.Fatalf("decision = %+v, error = %v", unresolved, err)
	}

	passedFirst, passedCheck := testAssessmentPair(t, 1, testReadyOutput(), "pass", source, request, config)
	if _, err := DecideReadiness([]ReadinessAssessment{passedFirst, secondAssessment}, []ReadinessCheck{passedCheck, secondCheck}, source, request, config); err == nil {
		t.Fatal("DecideReadiness() accepted a retry after a passing check")
	}
}

func TestGenerateCandidateRequiresReadyDecision(t *testing.T) {
	config, request, source := validArtifactFixture(t)
	api := &fakeChatAPI{output: chatOutput(`{"files":[{"path":"client/src/components/Example.tsx","content":"export const label = 'Updated label';\n"}],"rationale":"Update the label."}`)}
	invoker, _ := NewModelInvoker(api)

	assessment, check := testAssessmentPair(t, 1, testClarificationOutput(), "pass", source, request, config)
	clarification, err := DecideReadiness([]ReadinessAssessment{assessment}, []ReadinessCheck{check}, source, request, config)
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
		return DecideReadiness(
			[]ReadinessAssessment{first, second, third},
			[]ReadinessCheck{firstCheck, secondCheck, thirdCheck},
			source, request, config)
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

	// A set-level objection (no question_id) keeps the fail-closed outcome.
	decision, err = chain(ModelReadinessCheckOutput{Verdict: "fail", Reasons: []ReadinessCheckReason{
		{Code: "false-block", Message: "Q1 is answerable from the provided source.", QuestionID: "Q1"},
		{Code: "inconsistent-decision", Message: "The assessment contradicts itself."},
	}})
	if err != nil || decision.Outcome != ReadinessOutcomeUnresolved || len(decision.Questions) != 0 {
		t.Fatalf("decision = %+v, error = %v", decision, err)
	}

	// Blaming every question leaves nothing to surface.
	decision, err = chain(ModelReadinessCheckOutput{Verdict: "fail", Reasons: []ReadinessCheckReason{
		{Code: "false-block", Message: "Q1 is answerable from the provided source.", QuestionID: "Q1"},
		{Code: "invalid-question", Message: "Q2 duplicates the first question.", QuestionID: "Q2"},
	}})
	if err != nil || decision.Outcome != ReadinessOutcomeUnresolved || len(decision.Questions) != 0 {
		t.Fatalf("decision = %+v, error = %v", decision, err)
	}

	// A question-scoped failure on a non-clarification assessment changes nothing.
	first, firstCheck := testAssessmentPair(t, 1, testReadyOutput(), "fail", source, request, config)
	second, secondCheck := testAssessmentPair(t, 2, testReadyOutput(), "fail", source, request, config)
	third, thirdCheck := testAssessmentPair(t, 3, testReadyOutput(), "fail", source, request, config)
	decision, err = DecideReadiness([]ReadinessAssessment{first, second, third}, []ReadinessCheck{firstCheck, secondCheck, thirdCheck}, source, request, config)
	if err != nil || decision.Outcome != ReadinessOutcomeUnresolved {
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
	decision, err := DecideReadiness(assessments, checks, source, request, config)
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
		decision, err := DecideReadiness(
			[]ReadinessAssessment{first, second, third},
			[]ReadinessCheck{firstCheck, secondCheck, thirdCheck},
			source, request, config)
		if err != nil || decision.Outcome != ReadinessOutcomeUnresolved || len(decision.Questions) != 0 {
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
	check, _, err := invoker.CheckReadiness(context.Background(), assessment, nil, nil, source, request, config)
	if err != nil {
		t.Fatalf("a hallucinated question id killed the check: %v", err)
	}
	if len(check.Reasons) != 1 || check.Reasons[0].QuestionID != "" {
		t.Fatalf("the hallucinated id was not dropped to set-level: %+v", check.Reasons)
	}
}
