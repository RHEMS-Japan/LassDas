package worker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/hook"
)

// defaultTestPolicy is the asking policy a destination that configured
// nothing gets: the prompts and schemas every other test reads are the ones
// that destination is shown.
func defaultTestPolicy() askingPolicy { return askingPolicyFor(Config{}, nil) }

// receptionFixture is validArtifactFixture with the destination's own
// settings applied before the ticket is parsed, so the request carries the
// digest of the configuration under test.
func receptionFixture(t *testing.T, tune func(*Config)) (Config, TicketRequest, SourceSnapshot) {
	t.Helper()
	config := validTestConfig()
	if tune != nil {
		tune(&config)
	}
	request, err := ParseTicket(validTicketEnvelope(t, validTicketDescription()), config)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	filename := filepath.Join(root, "client", "src", "components", "Example.tsx")
	if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte("export const label = 'Old label';\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := ReadSourceSnapshot(root, strings.Repeat("a", 40), request, config)
	if err != nil {
		t.Fatal(err)
	}
	return config, request, source
}

// A destination that says nothing about questions gets the standard
// reception: it asks as little as it can, once, and settles the rest itself.
// The settings are absent from the encoded configuration, so a destination
// written before they existed keeps the digest its in-flight runs are sealed
// against.
func TestADestinationThatSaysNothingAsksMinimallyAndOnce(t *testing.T) {
	config := Config{}
	if config.QuestionsMode() != QuestionsMinimal {
		t.Fatalf("questions mode = %q, want %q", config.QuestionsMode(), QuestionsMinimal)
	}
	if config.QuestionItems() != 10 || config.QuestionRounds() != 1 {
		t.Fatalf("questions = %d in %d rounds, want 10 in 1", config.QuestionItems(), config.QuestionRounds())
	}
	if config.AnswerWeekdays() != hook.DefaultQuestionDeadlineWeekdays || config.AssumptionItems() != DefaultAssumptionMaxItems {
		t.Fatalf("answer window = %d weekdays, assumptions = %d", config.AnswerWeekdays(), config.AssumptionItems())
	}
	encoded, err := json.Marshal(validTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"questions", "question_max_items", "question_max_rounds", "question_deadline_weekdays", "assumption_max_items"} {
		if strings.Contains(string(encoded), `"`+key+`"`) {
			t.Fatalf("a configuration that set nothing encodes %q, which moves its digest", key)
		}
	}
	unset := validTestConfig()
	tuned := validTestConfig()
	tuned.Questions = QuestionsMinimal
	unsetDigest, err := unset.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	tunedDigest, err := tuned.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	if unsetDigest == tunedDigest {
		t.Fatal("writing the default out is meant to be visible in the digest, and is not")
	}
}

func TestReceptionSettingsAreRefusedOutsideTheirRange(t *testing.T) {
	for name, tune := range map[string]func(*Config){
		"an unknown mode":            func(c *Config) { c.Questions = "some" },
		"more questions than ids":    func(c *Config) { c.QuestionMaxItems = QuestionItemCeiling + 1 },
		"a negative count":           func(c *Config) { c.QuestionMaxItems = -1 },
		"more rounds than a record":  func(c *Config) { c.QuestionMaxRounds = hook.MaxClarificationRounds + 1 },
		"a window with no room":      func(c *Config) { c.QuestionDeadlineWeekdays = hook.MinQuestionDeadlineWeekdays - 1 },
		"a window without an end":    func(c *Config) { c.QuestionDeadlineWeekdays = hook.MaxQuestionDeadlineWeekdays + 1 },
		"more assumptions than said": func(c *Config) { c.AssumptionMaxItems = AssumptionItemCeiling + 1 },
	} {
		t.Run(name, func(t *testing.T) {
			config := validTestConfig()
			tune(&config)
			if err := config.Validate(); err == nil {
				t.Fatal("the configuration was accepted")
			}
		})
	}
	config := validTestConfig()
	config.Questions, config.QuestionMaxItems, config.QuestionMaxRounds = QuestionsNone, QuestionItemCeiling, 1
	config.QuestionDeadlineWeekdays, config.AssumptionMaxItems = hook.MinQuestionDeadlineWeekdays, 1
	if err := config.Validate(); err != nil {
		t.Fatalf("a configuration inside every range was refused: %v", err)
	}
}

// A destination that asks nothing never leaves a ticket waiting on a person:
// whatever the assessor wanted to ask becomes a decision it made, recorded
// where the requester reads it, and the gate outcome is ready.
func TestQuestionsNoneLeavesNothingToWaitFor(t *testing.T) {
	config, request, source := receptionFixture(t, func(c *Config) { c.Questions = QuestionsNone })
	policy := askingPolicyFor(config, nil)
	if policy.MayAsk() {
		t.Fatal("a destination that asks nothing was handed a question budget")
	}
	if !strings.Contains(readinessJSONSchema(policy), `"questions":{"type":"array","maxItems":0,`) {
		t.Fatal("the schema still lets the assessor return a question")
	}
	if !strings.Contains(readinessSystemPrompt(policy), "Ask nothing.") {
		t.Fatal("the contract does not tell the assessor to ask nothing")
	}

	asked := testClarificationOutput()
	assessment, check := receptionPair(t, 1, asked, "pass", source, request, config)
	if assessment.Decision != ReadinessOutcomeReady || len(assessment.Questions) != 0 {
		t.Fatalf("assessment = %s with %d questions, want ready with none", assessment.Decision, len(assessment.Questions))
	}
	if len(assessment.Assumptions) != 1 || assessment.Assumptions[0].Kind != AssumptionDefensibleDefault {
		t.Fatalf("assumptions = %+v, want the question recorded as a decision", assessment.Assumptions)
	}
	if !strings.Contains(assessment.Assumptions[0].Statement, asked.Questions[0].Question) {
		t.Fatalf("the recorded decision does not say what the point was: %q", assessment.Assumptions[0].Statement)
	}
	decision, err := DecideReadiness([]ReadinessAssessment{assessment}, []ReadinessCheck{check}, source, request, config)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Outcome != ReadinessOutcomeReady || len(decision.Questions) != 0 {
		t.Fatalf("decision = %s with %d questions, want ready with none", decision.Outcome, len(decision.Questions))
	}

	// The other way into a question is the rescue on the last attempt, where
	// the questions a checker did not fault by name still go to the
	// requester. Under this setting an assessment holds no questions at all,
	// so there is nothing for that rescue to send: three failed attempts end
	// with nobody waited on.
	assessments := []ReadinessAssessment{}
	checks := []ReadinessCheck{}
	for attempt := 1; attempt <= MaxReadinessAttempts; attempt++ {
		failed, failedCheck := receptionPair(t, attempt, testClarificationOutput(), "fail", source, request, config)
		assessments, checks = append(assessments, failed), append(checks, failedCheck)
	}
	exhausted, err := DecideReadiness(assessments, checks, source, request, config)
	if err != nil {
		t.Fatal(err)
	}
	if exhausted.Outcome == ReadinessOutcomeClarification || len(exhausted.Questions) != 0 {
		t.Fatalf("a run of this destination was sent to the requester after all: %s with %d questions",
			exhausted.Outcome, len(exhausted.Questions))
	}
}

// Asking minimally is a rule about what a question is for: a point a
// defensible default settles is settled and written down, and only a point
// where taking either answer would change what was asked for is put to the
// requester. Both halves of the reception are told the rule, and a set that
// obeys it goes through the gate as ready with the decision on the record.
func TestMinimalSettlesWhatADefaultSettles(t *testing.T) {
	config, request, source := receptionFixture(t, nil)
	policy := askingPolicyFor(config, nil)
	assessor := readinessSystemPrompt(policy)
	checker := readinessCheckSystemPrompt(config.Models.Readiness.Checker, policy)
	for _, want := range []string{"(5) no defensible default stands", AssumptionDefensibleDefault, "The requester is asked once"} {
		if !strings.Contains(assessor, want) {
			t.Fatalf("the assessor's contract lacks %q", want)
		}
	}
	if !strings.Contains(checker, "(5) no defensible default stands") {
		t.Fatal("the checker is not shown the policy it checks against")
	}

	normal := validTestConfig()
	normal.Questions = QuestionsNormal
	if strings.Contains(readinessSystemPrompt(askingPolicyFor(normal, nil)), "(5) no defensible default stands") {
		t.Fatal("a destination that asks normally was given the minimal rule")
	}

	settled := testReadyOutput()
	settled.Assumptions = []ReadinessAssumption{{
		Kind:      AssumptionDefensibleDefault,
		Statement: "並び順は指定が無いため新着順にする",
		Evidence:  "どちらでも依頼の目的は満たせるため、一覧の既定と同じにした",
	}}
	assessment, check := receptionPair(t, 1, settled, "pass", source, request, config)
	if len(assessment.Questions) != 0 || len(assessment.Assumptions) != 1 {
		t.Fatalf("assessment asked %d questions and recorded %d decisions", len(assessment.Questions), len(assessment.Assumptions))
	}
	decision, err := DecideReadiness([]ReadinessAssessment{assessment}, []ReadinessCheck{check}, source, request, config)
	if err != nil || decision.Outcome != ReadinessOutcomeReady {
		t.Fatalf("decision = %+v, error = %v", decision, err)
	}
}

// One round is the whole conversation. A run that already has its answers
// cannot be sent back to the requester by an assessor that wants more: the
// point is decided, and the record says why nobody was asked.
func TestTheReceptionAsksAtMostOnce(t *testing.T) {
	config, request, source := receptionFixture(t, nil)
	clarification := fixtureClarification(t, request)
	policy := askingPolicyFor(config, clarification)
	if policy.MayAsk() {
		t.Fatalf("a run that already asked once kept a budget of %d", policy.MaxItems)
	}
	if !strings.Contains(askingBudget(policy), "already spent") {
		t.Fatalf("the contract does not say the round is spent: %s", askingBudget(policy))
	}
	// A destination that does ask must not be told it never asks: the
	// sentence has to name the reason this particular run may not, or the
	// model is being corrected for obeying a rule nobody gave it.
	spent := askingConditions(policy)
	if strings.Contains(spent, "does not put questions to the requester") {
		t.Fatalf("a destination that asks was told it does not: %s", spent)
	}
	if !strings.Contains(spent, "already had its round of questions") {
		t.Fatalf("the contract does not say why this run may not ask: %s", spent)
	}
	silent := validTestConfig()
	silent.Questions = QuestionsNone
	if !strings.Contains(askingConditions(askingPolicyFor(silent, nil)), "does not put questions to the requester") {
		t.Fatal("a destination that asks nothing is no longer told so")
	}

	invocation := validTestInvocation(config.Models.Readiness.Assessor)
	assessment, err := NewReadinessAssessment(1, testClarificationOutput(), clarification, nil, source, request, config, invocation, testInvocationTime)
	if err != nil {
		t.Fatal(err)
	}
	if assessment.Decision != ReadinessOutcomeReady || len(assessment.Questions) != 0 {
		t.Fatalf("a second round was asked: %s with %d questions", assessment.Decision, len(assessment.Questions))
	}
	if len(assessment.Assumptions) != 1 || !strings.Contains(assessment.Assumptions[0].Evidence, "確認は 1 回だけ") {
		t.Fatalf("the record does not say why nobody was asked: %+v", assessment.Assumptions)
	}
}

// Ten questions are what one comment carries, so the reception may ask them:
// the set is inside the sealed bound, the comment the requester reads is
// inside the tracker's, and the tenth question is numbered Q10.
func TestTenQuestionsFitOneComment(t *testing.T) {
	config, request, source := receptionFixture(t, nil)
	output := testReadyOutput()
	output.Decision = ReadinessOutcomeClarification
	output.Questions = realisticQuestions(10)
	if err := questionSetFitsComment(output.Questions); err != nil {
		t.Fatalf("ten questions do not fit one comment: %v", err)
	}
	assessment, _ := receptionPair(t, 1, output, "pass", source, request, config)
	if len(assessment.Questions) != 10 || assessment.Questions[9].ID != "Q10" {
		t.Fatalf("sealed %d questions, last id %q", len(assessment.Questions), assessment.Questions[9].ID)
	}
	// The checker can fault the tenth question by name: the id pattern both
	// halves of the reception share reaches Q10 and stops at the ceiling.
	if err := validateModelReadinessCheckOutput(ModelReadinessCheckOutput{
		Verdict: "fail", Reasons: []ReadinessCheckReason{{Code: "false-block", Message: "m", QuestionID: "Q10"}},
	}); err != nil {
		t.Fatalf("a reason naming Q10 was refused: %v", err)
	}
	if err := validateModelReadinessCheckOutput(ModelReadinessCheckOutput{
		Verdict: "fail", Reasons: []ReadinessCheckReason{{Code: "false-block", Message: "m", QuestionID: "Q20"}},
	}); err == nil {
		t.Fatal("a reason naming a question past the ceiling was accepted")
	}
	encoded, err := json.Marshal(assessment.Questions)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > hook.MaxQuestionSetBytes {
		t.Fatalf("the sealed set is %d bytes, over %d", len(encoded), hook.MaxQuestionSetBytes)
	}
	size, err := hook.RenderedQuestionCommentBytes(string(encoded))
	if err != nil {
		t.Fatal(err)
	}
	if size > hook.MaxTrackerCommentBytes {
		t.Fatalf("the comment for ten questions is %d bytes, over %d", size, hook.MaxTrackerCommentBytes)
	}
	t.Logf("ten questions: %d bytes sealed, %d bytes posted (limit %d)", len(encoded), size, hook.MaxTrackerCommentBytes)

	// The comment, not the sealed array, is the binding constraint: there
	// are sets the store would seal happily whose comment the tracker
	// refuses, and those have to be refused here, where the model can still
	// shorten them. The boundary is searched for rather than written down,
	// so the test keeps meaning this when the wording around the questions
	// changes.
	overflowing, grown := realisticQuestions(15), false
	for pad := 0; pad < 400 && !grown; pad += 8 {
		for index := range overflowing {
			overflowing[index].WhyBlocking = realisticQuestions(1)[0].WhyBlocking + strings.Repeat("ん", pad)
		}
		set, err := json.Marshal(overflowing)
		if err != nil {
			t.Fatal(err)
		}
		if len(set) > hook.MaxQuestionSetBytes {
			break
		}
		if posted, err := hook.RenderedQuestionCommentBytes(string(set)); err == nil && posted > hook.MaxTrackerCommentBytes {
			grown = true
			t.Logf("a set of %d bytes - which the sealed bound %d accepts - posts as %d bytes",
				len(set), hook.MaxQuestionSetBytes, posted)
		}
	}
	if !grown {
		t.Fatal("no set was found that the store would seal and the tracker would refuse")
	}
	err = questionSetFitsComment(overflowing)
	if err == nil {
		t.Fatal("a set whose comment the tracker refuses was accepted")
	}
	if !strings.Contains(err.Error(), "the comment for these") {
		t.Fatalf("the refusal does not say the comment is what does not fit: %v", err)
	}
}

// The number of questions is the destination's, and a model that asks past
// it is told the number rather than quietly trimmed.
func TestMoreQuestionsThanTheDestinationAllowsAreRefused(t *testing.T) {
	config, request, source := receptionFixture(t, func(c *Config) { c.QuestionMaxItems = 2 })
	output := testReadyOutput()
	output.Decision = ReadinessOutcomeClarification
	output.Questions = realisticQuestions(3)
	invocation := validTestInvocation(config.Models.Readiness.Assessor)
	_, err := NewReadinessAssessment(1, output, nil, nil, source, request, config, invocation, testInvocationTime)
	if err == nil {
		t.Fatal("a set over the destination's number was sealed")
	}
	if !strings.Contains(err.Error(), "limit 2") {
		t.Fatalf("the refusal does not name the number: %v", err)
	}
	if !strings.Contains(readinessJSONSchema(askingPolicyFor(config, nil)), `"questions":{"type":"array","maxItems":2,`) {
		t.Fatal("the schema does not carry the destination's number")
	}
}

// The kind a decision is sealed under is read by the package that writes the
// plan notice, which cannot import this one and therefore spells the kind
// out. Renaming it here without renaming it there would quietly stop the
// requester from being shown what was decided for them.
func TestTheDecidedKindIsSpelledTheWayTheNoticeReadsIt(t *testing.T) {
	if AssumptionDefensibleDefault != "defensible_default" {
		t.Fatalf("kind = %q; internal/attendant's plan notice reads %q",
			AssumptionDefensibleDefault, "defensible_default")
	}
}

// receptionPair is testAssessmentPair with the clarification the run has.
func receptionPair(t *testing.T, attempt int, output ModelReadinessOutput, verdict string, source SourceSnapshot, request TicketRequest, config Config) (ReadinessAssessment, ReadinessCheck) {
	t.Helper()
	assessorInvocation := validTestInvocation(config.Models.Readiness.Assessor)
	assessorInvocation.RequestID += "-a" + string(rune('0'+attempt))
	assessment, err := NewReadinessAssessment(attempt, output, nil, nil, source, request, config, assessorInvocation, testInvocationTime)
	if err != nil {
		t.Fatal(err)
	}
	checkOutput := ModelReadinessCheckOutput{Verdict: verdict, Reasons: []ReadinessCheckReason{}}
	if verdict == "fail" {
		checkOutput.Reasons = []ReadinessCheckReason{{Code: "false-ready", Message: "A blocking ambiguity remains."}}
	}
	checkerInvocation := validTestInvocation(config.Models.Readiness.Checker)
	checkerInvocation.RequestID += "-c" + string(rune('0'+attempt))
	check, err := NewReadinessCheck(checkOutput, assessment, source, request, config, checkerInvocation, testInvocationTime)
	if err != nil {
		t.Fatal(err)
	}
	return assessment, check
}

// realisticQuestions is a set the size a reception actually produces: a
// sentence of a question, a sentence of why it blocks, and three choices
// with a label and the result of taking it.
func realisticQuestions(count int) []ReadinessQuestion {
	questions := make([]ReadinessQuestion, 0, count)
	for index := 1; index <= count; index++ {
		questions = append(questions, ReadinessQuestion{
			ID:          fmt.Sprintf("Q%d", index),
			Dimension:   "user_visible_behavior",
			Question:    fmt.Sprintf("%d 件目の確認です。一覧の絞り込みを保存したまま画面を離れて戻ったとき、絞り込みは元のまま残すべきですか。", index),
			WhyBlocking: "残すかどうかで、戻ってきた利用者が最初に見る一覧の中身が変わります。どちらでも実装はできます。",
			Choices: []ReadinessChoice{
				{ID: "a", Label: "絞り込みを残す", Effect: "戻ると前回と同じ絞り込みのままの一覧が出ます。"},
				{ID: "b", Label: "毎回すべて表示に戻す", Effect: "戻るたびに絞り込みなしの一覧が出ます。"},
				{ID: "c", Label: "その日のうちだけ残す", Effect: "同じ日は前回の絞り込み、翌日はすべて表示になります。"},
			},
		})
	}
	return questions
}
