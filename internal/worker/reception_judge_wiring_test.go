package worker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// countingJudge is the reception judge as the gate sees it: an answer, or an
// error meaning the model has no opinion. It counts because where the judge
// is consulted is the safety argument, and a count is the only way to say
// that a request with nothing to ask never reached a model at all.
type countingJudge struct {
	opinion ReceptionOpinion
	failure error
	calls   int
	asked   string
}

func (j *countingJudge) Proceedable(ctx context.Context, request string) (ReceptionOpinion, error) {
	j.calls++
	j.asked = request
	if j.failure != nil {
		return ReceptionOpinion{}, j.failure
	}
	if err := ctx.Err(); err != nil {
		return ReceptionOpinion{}, err
	}
	return j.opinion, nil
}

const testJudgeModel = "typesafe/jev-1.13"

// judgedFixture is validArtifactFixture with a reception judge named, at a
// threshold the test picks. The role is set before the ticket is parsed
// because the request carries the configuration's digest: a role added
// afterwards would be a configuration the sealed request is not bound to.
func judgedFixture(t *testing.T, threshold float64) (Config, TicketRequest, SourceSnapshot) {
	t.Helper()
	config := validTestConfig()
	config.Models.ReceptionJudge = &ReceptionJudgeConfig{
		Provider: "TypeSafe", Model: testJudgeModel, APIKeyEnv: "MODEL_API_KEY_DECISIONS",
		ProceedThreshold: &threshold,
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

// testProposedDefaultOutput is a reception that wrote two questions and named
// the choice it would take for as many of them as the test asks for.
func testProposedDefaultOutput(defaults ...string) ModelReadinessOutput {
	output := testTwoQuestionOutput()
	for index := range output.Questions {
		if index < len(defaults) {
			output.Questions[index].ProposedDefault = defaults[index]
		}
	}
	return output
}

func decideWithJudge(t *testing.T, ctx context.Context, output ModelReadinessOutput, judge ReceptionJudge, config Config, request TicketRequest, source SourceSnapshot) (ReadinessDecision, error) {
	t.Helper()
	assessment, check := testAssessmentPair(t, 1, output, "pass", source, request, config)
	return DecideReadiness(ctx, []ReadinessAssessment{assessment}, []ReadinessCheck{check}, source, request, config, judge)
}

// A confident yes settles the questions the reception named a default for,
// and only those. The points it settled become the assumptions the requester
// reads; a question it would not name a default for goes on being asked,
// because the alternative is to invent one and show them a decision nobody
// made.
func TestAConfidentJudgeSettlesOnlyTheQuestionsTheReceptionProposedDefaultsFor(t *testing.T) {
	config, request, source := judgedFixture(t, 0.80)
	judge := &countingJudge{opinion: ReceptionOpinion{Model: testJudgeModel, Answer: "yes", Confidence: 0.91}}
	decision, err := decideWithJudge(t, t.Context(), testProposedDefaultOutput("b"), judge, config, request, source)
	if err != nil {
		t.Fatalf("deciding with a judge: %v", err)
	}
	if judge.calls != 1 {
		t.Fatalf("the judge was consulted %d times, want once", judge.calls)
	}
	if decision.Outcome != ReadinessOutcomeClarification {
		t.Fatalf("outcome = %q, want %q while a question is still unanswerable", decision.Outcome, ReadinessOutcomeClarification)
	}
	if len(decision.Questions) != 1 || decision.Questions[0].ID != "Q1" {
		t.Fatalf("questions = %+v, want the one with no proposed default, renumbered Q1", decision.Questions)
	}
	if !strings.Contains(decision.Questions[0].Question, "empty input") {
		t.Fatalf("the wrong question survived: %q", decision.Questions[0].Question)
	}
	if len(decision.Assumptions) != 1 {
		t.Fatalf("settled %d points, want 1: %+v", len(decision.Assumptions), decision.Assumptions)
	}
	settled := decision.Assumptions[0]
	if settled.Kind != AssumptionDefensibleDefault {
		t.Errorf("settled point kind = %q, want %q", settled.Kind, AssumptionDefensibleDefault)
	}
	// The reception's own words on both sides: the point it was going to
	// ask about, and the answer it said it would defend.
	if !strings.Contains(settled.Statement, "both language screens") || !strings.Contains(settled.Statement, "Both languages") {
		t.Errorf("settled point does not say what was decided: %q", settled.Statement)
	}
	if !strings.Contains(settled.Evidence, "Both screens show the new label.") {
		t.Errorf("settled point does not say what it means: %q", settled.Evidence)
	}
	judgment := decision.ReceptionJudgment
	if judgment == nil {
		t.Fatal("the decision records no judgment for the questions it settled")
	}
	if judgment.Answer != "yes" || judgment.Confidence != 0.91 || judgment.Threshold != 0.80 || judgment.Model != testJudgeModel {
		t.Errorf("recorded judgment = %+v", *judgment)
	}
	// The request as the requester wrote it is what was judged, not a
	// summary of it.
	if !strings.Contains(judge.asked, request.Request) {
		t.Error("the judge was asked about something other than the request")
	}
	if err := decision.Validate([]ReadinessAssessment{}, nil, source, request, config); err == nil {
		t.Error("a decision validated against no chain at all was accepted")
	}
}

// Every question settled means nothing is left to ask, and the gate seals
// ready - the whole point of consulting a judge at all.
func TestASettledSetOfQuestionsSealsReady(t *testing.T) {
	config, request, source := judgedFixture(t, 0.80)
	judge := &countingJudge{opinion: ReceptionOpinion{Model: testJudgeModel, Answer: "yes", Confidence: 0.99}}
	decision, err := decideWithJudge(t, t.Context(), testProposedDefaultOutput("b", "a"), judge, config, request, source)
	if err != nil {
		t.Fatalf("deciding with a judge: %v", err)
	}
	if decision.Outcome != ReadinessOutcomeReady {
		t.Fatalf("outcome = %q, want %q", decision.Outcome, ReadinessOutcomeReady)
	}
	if len(decision.Questions) != 0 {
		t.Fatalf("a ready decision carries %d questions", len(decision.Questions))
	}
	if len(decision.Assumptions) != 2 {
		t.Fatalf("settled %d points, want 2", len(decision.Assumptions))
	}
	// The gate artifact has to be re-derivable from what is sealed, or a
	// reader of a finished run finds ready over an assessment that asked
	// two questions and nothing to explain the difference.
	assessment, check := testAssessmentPair(t, 1, testProposedDefaultOutput("b", "a"), "pass", source, request, config)
	if err := decision.Validate([]ReadinessAssessment{assessment}, []ReadinessCheck{check}, source, request, config); err != nil {
		t.Fatalf("the sealed decision does not re-derive: %v", err)
	}
}

// The rescue path - a final attempt whose check failed, with questions the
// checker did not blame - reaches the judge the same way, because those
// questions go to the requester just as the others do.
func TestTheQuestionsThatOutliveAFailedCheckAreJudgedToo(t *testing.T) {
	config, request, source := judgedFixture(t, 0.80)
	judge := &countingJudge{opinion: ReceptionOpinion{Model: testJudgeModel, Answer: "yes", Confidence: 0.95}}
	blamesFirst := ModelReadinessCheckOutput{Verdict: "fail", Reasons: []ReadinessCheckReason{
		{Code: "false-block", Message: "The first question is answerable from the repository.", QuestionID: "Q1"},
	}}
	output := testProposedDefaultOutput("b", "a")
	first, firstCheck := testAssessmentPair(t, 1, output, "fail", source, request, config)
	second, secondCheck := testAssessmentPair(t, 2, output, "fail", source, request, config)
	third, thirdCheck := testCheckedPair(t, 3, output, blamesFirst, source, request, config)
	decision, err := DecideReadiness(t.Context(),
		[]ReadinessAssessment{first, second, third},
		[]ReadinessCheck{firstCheck, secondCheck, thirdCheck},
		source, request, config, judge)
	if err != nil {
		t.Fatalf("deciding with a judge: %v", err)
	}
	if judge.calls != 1 {
		t.Fatalf("the judge was consulted %d times, want once", judge.calls)
	}
	if decision.Outcome != ReadinessOutcomeReady || len(decision.Assumptions) != 1 {
		t.Fatalf("outcome = %q with %d settled points, want ready with 1", decision.Outcome, len(decision.Assumptions))
	}
}

// Everything that is not a confident yes leaves the decision exactly as it
// was - the same bytes, not merely the same outcome. A record of a judge
// that said no is still a difference in a sealed artifact, and the claim
// being made here is that a destination which turns this on cannot have a
// run it would otherwise have had come out differently.
func TestAJudgeThatSettlesNothingLeavesTheDecisionByteIdentical(t *testing.T) {
	config, request, source := judgedFixture(t, 0.80)
	output := testProposedDefaultOutput("b", "a")
	untouched, err := decideWithJudge(t, t.Context(), output, nil, config, request, source)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(untouched)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	for _, testCase := range []struct {
		name  string
		ctx   context.Context
		judge *countingJudge
		calls int
	}{
		{name: "said no", ctx: t.Context(), calls: 1,
			judge: &countingJudge{opinion: ReceptionOpinion{Model: testJudgeModel, Answer: "no", Confidence: 1.0}}},
		{name: "was not sure enough", ctx: t.Context(), calls: 1,
			judge: &countingJudge{opinion: ReceptionOpinion{Model: testJudgeModel, Answer: "yes", Confidence: 0.79}}},
		{name: "could not be reached", ctx: t.Context(), calls: 1,
			judge: &countingJudge{failure: errors.New("decisions: the call failed")}},
		{name: "ran out of time", ctx: cancelled, calls: 1, judge: &countingJudge{
			opinion: ReceptionOpinion{Model: testJudgeModel, Answer: "yes", Confidence: 1.0}}},
		{name: "answered for another model", ctx: t.Context(), calls: 1,
			judge: &countingJudge{opinion: ReceptionOpinion{Model: "someone/else", Answer: "yes", Confidence: 1.0}}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			decision, err := decideWithJudge(t, testCase.ctx, output, testCase.judge, config, request, source)
			if err != nil {
				t.Fatalf("deciding: %v", err)
			}
			if testCase.judge.calls != testCase.calls {
				t.Errorf("the judge was consulted %d times, want %d", testCase.judge.calls, testCase.calls)
			}
			got, err := json.Marshal(decision)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) {
				t.Errorf("the decision moved:\n got %s\nwant %s", got, want)
			}
		})
	}

	// A destination that named no judge is the same case, reached without
	// the model being asked anything at all.
	plainConfig, plainRequest, plainSource := validArtifactFixture(t)
	judge := &countingJudge{opinion: ReceptionOpinion{Model: testJudgeModel, Answer: "yes", Confidence: 1.0}}
	withRole, err := decideWithJudge(t, t.Context(), output, judge, plainConfig, plainRequest, plainSource)
	if err != nil {
		t.Fatal(err)
	}
	withoutRole, err := decideWithJudge(t, t.Context(), output, nil, plainConfig, plainRequest, plainSource)
	if err != nil {
		t.Fatal(err)
	}
	if judge.calls != 0 {
		t.Errorf("a destination that named no judge consulted one %d times", judge.calls)
	}
	first, err := json.Marshal(withRole)
	if err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(withoutRole)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Errorf("an unconfigured judge moved the decision:\n got %s\nwant %s", first, second)
	}
}

// Nothing to ask, nothing to judge. This is the guard the whole design rests
// on: a "no" cannot add a question or raise one, because a request with no
// questions never reaches the model.
func TestTheJudgeIsNeverConsultedWhenNothingWouldBeAsked(t *testing.T) {
	config, request, source := judgedFixture(t, 0.80)
	reject := testReadyOutput()
	reject.Decision = ReadinessOutcomeReject
	reject.RejectCode = "out-of-scope"
	unresolvable := testReadyOutput()
	unresolvable.Decision = ReadinessAssessorUnresolvable
	for _, testCase := range []struct {
		name   string
		output ModelReadinessOutput
	}{
		{"nothing was left to ask", testReadyOutput()},
		{"the request was refused", reject},
		{"nobody could turn it into a question", unresolvable},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			judge := &countingJudge{opinion: ReceptionOpinion{Model: testJudgeModel, Answer: "yes", Confidence: 1.0}}
			if _, err := decideWithJudge(t, t.Context(), testCase.output, judge, config, request, source); err != nil {
				t.Fatalf("deciding: %v", err)
			}
			if judge.calls != 0 {
				t.Fatalf("the judge was consulted %d times, want none", judge.calls)
			}
		})
	}
}

// A judgment in a sealed decision has to be one this destination's role
// could have produced. Without this, a hand-edited record could show a gate
// that settled its requester's questions on an authority it never had.
func TestASealedJudgmentIsHeldToTheRoleThatCouldHaveGivenIt(t *testing.T) {
	config, request, source := judgedFixture(t, 0.80)
	judge := &countingJudge{opinion: ReceptionOpinion{Model: testJudgeModel, Answer: "yes", Confidence: 0.99}}
	sealed, err := decideWithJudge(t, t.Context(), testProposedDefaultOutput("b", "a"), judge, config, request, source)
	if err != nil {
		t.Fatal(err)
	}
	if err := sealed.ValidateBinding(source, request, config); err != nil {
		t.Fatalf("the decision this gate sealed does not bind: %v", err)
	}
	for _, testCase := range []struct {
		name   string
		break_ func(*ReadinessDecision)
	}{
		{"said no", func(d *ReadinessDecision) { d.ReceptionJudgment.Answer = "no" }},
		{"was not sure enough", func(d *ReadinessDecision) { d.ReceptionJudgment.Confidence = 0.1 }},
		{"asked for another certainty", func(d *ReadinessDecision) { d.ReceptionJudgment.Threshold = 0.5 }},
		{"names another model", func(d *ReadinessDecision) { d.ReceptionJudgment.Model = "someone/else" }},
		{"settled points with nothing that settled them", func(d *ReadinessDecision) { d.ReceptionJudgment = nil }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			broken := sealed
			judgment := *sealed.ReceptionJudgment
			broken.ReceptionJudgment = &judgment
			broken.Assumptions = append([]ReadinessAssumption(nil), sealed.Assumptions...)
			testCase.break_(&broken)
			digest, err := readinessDecisionDigest(broken)
			if err != nil {
				t.Fatal(err)
			}
			broken.DecisionSHA256 = digest
			if err := broken.ValidateBinding(source, request, config); err == nil {
				t.Fatal("the gate accepted a judgment its role could not have given")
			}
		})
	}
	// The same decision read by a destination that has since dropped the
	// role: the questions were settled on an authority that is no longer
	// there, and the record is refused rather than honoured.
	plain := config
	plain.Models.ReceptionJudge = nil
	if err := sealed.ValidateBinding(source, request, plain); err == nil {
		t.Error("a judgment survived the role being taken away")
	}
}

// A default that names a choice the question does not offer is not a default
// the requester could ever have been shown, and the assessment carrying it
// is refused where every other malformed question is.
func TestAProposedDefaultMustNameOneOfTheQuestionsOwnChoices(t *testing.T) {
	output := testProposedDefaultOutput("c")
	if err := validateModelReadinessOutput(output); err == nil {
		t.Fatal("a question proposing a choice it does not offer was accepted")
	}
	if err := validateModelReadinessOutput(testProposedDefaultOutput("a")); err != nil {
		t.Fatalf("a question proposing one of its own choices was refused: %v", err)
	}
	if err := validateModelReadinessOutput(testProposedDefaultOutput()); err != nil {
		t.Fatalf("a question proposing no default at all was refused: %v", err)
	}
	// Both sides of the contract: the models are shown the field, under the
	// pattern the engine holds it to.
	schema := readinessJSONSchema(defaultTestPolicy())
	if !strings.Contains(schema, `"proposed_default":{"type":"string","pattern":"^[a-d]?$"}`) {
		t.Error("the prompt schema no longer offers the proposed default")
	}
	if !strings.Contains(readinessSystemPrompt(defaultTestPolicy()), "proposed_default is your own answer to the question you are asking") {
		t.Error("the assessor is no longer told what the proposed default is for")
	}
}

// The threshold is a setting, and a setting outside the measured range is a
// number nobody measured. A destination that names none takes the default.
func TestTheProceedThresholdIsHeldToItsRange(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		value   float64
		refused bool
	}{
		{"the floor", MinReceptionProceedThreshold, false},
		{"the ceiling, which never settles anything", MaxReceptionProceedThreshold, false},
		{"under the floor", 0.49, true},
		{"over the ceiling", 1.01, true},
		{"zero, which is not silence", 0, true},
		{"negative", -1, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			value := testCase.value
			role := ReceptionJudgeConfig{Provider: "TypeSafe", Model: testJudgeModel,
				APIKeyEnv: "MODEL_API_KEY_DECISIONS", ProceedThreshold: &value}
			err := role.validate()
			if testCase.refused && err == nil {
				t.Fatalf("threshold %v was accepted", testCase.value)
			}
			if !testCase.refused && err != nil {
				t.Fatalf("threshold %v was refused: %v", testCase.value, err)
			}
			if !testCase.refused && role.Threshold() != testCase.value {
				t.Fatalf("threshold reads back as %v", role.Threshold())
			}
		})
	}
	named := ReceptionJudgeConfig{Provider: "TypeSafe", Model: testJudgeModel, APIKeyEnv: "MODEL_API_KEY_DECISIONS"}
	if named.Threshold() != DefaultReceptionProceedThreshold {
		t.Errorf("a destination that named no threshold takes %v, want the default %v", named.Threshold(), DefaultReceptionProceedThreshold)
	}
	// The default is a measured number, written out here rather than named,
	// so moving it is moving it deliberately. Every request the reception
	// really asked about was judged between 0.64 and 0.81 over 35 cases, so
	// anything at or under 0.81 settles a question the reception was right
	// to ask; 0.85 is the lowest that overrules none of them and this keeps
	// a margin above it (docs/SETUP.md).
	if DefaultReceptionProceedThreshold != 0.90 {
		t.Errorf("the measured default moved to %v without the measurement moving", DefaultReceptionProceedThreshold)
	}
	if err := named.validate(); err != nil {
		t.Errorf("a destination that named no threshold was refused: %v", err)
	}
}

// The key reaches the service and nothing else. A service that answers with
// a page quoting what it refused is the way a key would otherwise reach an
// operator's log, so the refusal is checked against the key itself.
func TestTheJudgeNeverPutsTheKeyInAnError(t *testing.T) {
	const keyVariable = "MODEL_API_KEY_DECISIONS"
	const secret = "sk-test-0123456789abcdef"
	t.Setenv(keyVariable, secret)
	var reached string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusForbidden)
		// The service's own words, quoting the key it refused.
		_, _ = w.Write([]byte("this key is not allowed this model: " + secret))
	}))
	defer server.Close()
	config := validTestConfig()
	config.Models.ReceptionJudge = &ReceptionJudgeConfig{
		Provider: "TypeSafe", Model: testJudgeModel, BaseURL: server.URL, APIKeyEnv: keyVariable,
	}
	judge, configured, err := NewReceptionJudge(config, server.Client())
	if err != nil || !configured {
		t.Fatalf("building the judge: configured = %v, err = %v", configured, err)
	}
	_, err = judge.Proceedable(t.Context(), "make the label say something else")
	if err == nil {
		t.Fatal("a refused call came back as an opinion")
	}
	if reached != "Bearer "+secret {
		t.Fatalf("the key did not reach the service as a bearer token")
	}
	if strings.Contains(err.Error(), secret) {
		t.Error("the key is in the error")
	}
	if strings.Contains(err.Error(), "Bearer") {
		t.Error("an authorization header is in the error")
	}
}

// A role the engine cannot call at all is a configuration error, reported as
// one. Left to look like a model with no opinion, a destination would turn
// this on, see nothing change, and have nothing to read.
func TestAReceptionJudgeThatCannotBeBuiltIsReported(t *testing.T) {
	config := validTestConfig()
	if judge, configured, err := NewReceptionJudge(config, &http.Client{}); judge != nil || configured || err != nil {
		t.Fatalf("a destination naming no judge: judge = %v, configured = %v, err = %v", judge, configured, err)
	}
	config.Models.ReceptionJudge = &ReceptionJudgeConfig{
		Provider: "TypeSafe", Model: "  ", APIKeyEnv: "MODEL_API_KEY_DECISIONS",
	}
	if _, _, err := NewReceptionJudge(config, &http.Client{}); err == nil {
		t.Error("a role naming no model was built anyway")
	}
}
