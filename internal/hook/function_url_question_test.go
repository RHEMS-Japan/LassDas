package hook

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type functionURLFakeQuestionProcessor struct {
	result  Result
	calls   int
	records []QuestionReportRequest
}

func (f *functionURLFakeQuestionProcessor) ProcessQuestionReport(_ context.Context, request QuestionReportRequest) Result {
	f.calls++
	f.records = append(f.records, request)
	return f.result
}

type functionURLFakeTickProcessor struct {
	result Result
	calls  int
}

func (f *functionURLFakeTickProcessor) ProcessQuestionTick(_ context.Context, _ QuestionTickRequest) Result {
	f.calls++
	return f.result
}

func newFunctionURLQuestionHandler(t *testing.T, questioner *functionURLFakeQuestionProcessor, ticker *functionURLFakeTickProcessor) *FunctionURLHandler {
	t.Helper()
	config := functionURLTestConfig(0)
	config.Report = terminalTestConfig()
	if questioner == nil {
		questioner = &functionURLFakeQuestionProcessor{result: Result{Decision: DecisionAccepted, Code: "question_report_recorded"}}
	}
	if ticker == nil {
		ticker = &functionURLFakeTickProcessor{result: Result{Decision: DecisionAccepted, Code: "question_tick_idle"}}
	}
	reporter := &functionURLFakeReportProcessor{result: Result{Decision: DecisionAccepted, Code: "terminal_report_recorded"}}
	handler, err := NewFunctionURLHandlerWithQuestions(config, &functionURLFakeHookProcessor{}, &functionURLFakePullProcessor{}, reporter, questioner, ticker)
	if err != nil {
		t.Fatalf("NewFunctionURLHandlerWithQuestions() error = %v", err)
	}
	handler.now = func() time.Time { return functionURLTestNow }
	return handler
}

func functionURLValidQuestionRequest(t *testing.T, config ReportRouteConfig, issuedAt time.Time) FunctionURLRequest {
	t.Helper()
	body, err := json.Marshal(QuestionReportRequest{Record: questionTestRecord(), IssuedAt: issuedAt.UTC()})
	if err != nil {
		t.Fatalf("marshal question report: %v", err)
	}
	return FunctionURLRequest{
		Body:    string(body),
		RawPath: QuestionReportPath,
		Headers: map[string]string{
			"Content-Type":                "application/json",
			QuestionReportSignatureHeader: SignQuestionReportRequest(config.HMACKey, body),
		},
		RequestContext: RequestContext{HTTP: RequestContextHTTP{Method: "POST", SourceIP: "192.0.2.3"}},
	}
}

func TestFunctionURLQuestionReportIsRouteBoundAndResponseSigned(t *testing.T) {
	config := terminalTestConfig()
	questioner := &functionURLFakeQuestionProcessor{result: Result{Decision: DecisionAccepted, Code: "question_report_recorded"}}
	request := functionURLValidQuestionRequest(t, config, functionURLTestNow)
	response, err := newFunctionURLQuestionHandler(t, questioner, nil).Handle(context.Background(), request)
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	functionURLAssertResult(t, response, 200, "question_report_recorded")
	signature := header(response.Headers, QuestionReportResponseSignatureHeader)
	if signature == "" || !VerifyQuestionReportResponseSignature(config.HMACKey, response.StatusCode, []byte(request.Body), []byte(response.Body), signature) {
		t.Fatalf("question response signature did not verify: %+v", response)
	}
	if questioner.calls != 1 || questioner.records[0].Record.DeliveryID != questionTestRecord().DeliveryID {
		t.Fatalf("questioner calls = %d records = %+v", questioner.calls, questioner.records)
	}
}

func TestFunctionURLQuestionReportRejectsBeforeProcessingAndAlwaysSigns(t *testing.T) {
	config := terminalTestConfig()
	tests := []struct {
		name   string
		mutate func(*FunctionURLRequest)
		status int
		code   string
	}{
		{name: "wrong signature", mutate: func(r *FunctionURLRequest) {
			r.Headers[QuestionReportSignatureHeader] = "sha256=" + strings.Repeat("0", 64)
		}, status: 401, code: "unauthorized"},
		{name: "terminal signature replayed against the question route", mutate: func(r *FunctionURLRequest) {
			r.Headers[QuestionReportSignatureHeader] = SignTerminalReportRequest(config.HMACKey, []byte(r.Body))
		}, status: 401, code: "unauthorized"},
		{name: "garbage body", mutate: func(r *FunctionURLRequest) {
			r.Body = `{"record":{}}`
			r.Headers[QuestionReportSignatureHeader] = SignQuestionReportRequest(config.HMACKey, []byte(r.Body))
		}, status: 400, code: "question_report_invalid"},
		{name: "wrong route", mutate: func(r *FunctionURLRequest) {
			record := questionTestRecord()
			record.RepositoryID++
			body, err := json.Marshal(QuestionReportRequest{Record: record, IssuedAt: functionURLTestNow})
			if err != nil {
				panic(err)
			}
			r.Body = string(body)
			r.Headers[QuestionReportSignatureHeader] = SignQuestionReportRequest(config.HMACKey, body)
		}, status: 403, code: "question_report_not_allowed"},
		{name: "stale issued at", mutate: func(r *FunctionURLRequest) {
			body, err := json.Marshal(QuestionReportRequest{Record: questionTestRecord(), IssuedAt: functionURLTestNow.Add(-config.ClockSkew - time.Second)})
			if err != nil {
				panic(err)
			}
			r.Body = string(body)
			r.Headers[QuestionReportSignatureHeader] = SignQuestionReportRequest(config.HMACKey, body)
		}, status: 403, code: "question_report_timestamp_not_allowed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			questioner := &functionURLFakeQuestionProcessor{}
			request := functionURLValidQuestionRequest(t, config, functionURLTestNow)
			tt.mutate(&request)
			response, err := newFunctionURLQuestionHandler(t, questioner, nil).Handle(context.Background(), request)
			if err != nil {
				t.Fatalf("Handle() error = %v", err)
			}
			functionURLAssertResult(t, response, tt.status, tt.code)
			signature := header(response.Headers, QuestionReportResponseSignatureHeader)
			if signature == "" || !VerifyQuestionReportResponseSignature(config.HMACKey, response.StatusCode, []byte(request.Body), []byte(response.Body), signature) {
				t.Fatalf("rejection was not signed: %+v", response)
			}
			if questioner.calls != 0 {
				t.Fatalf("questioner calls = %d, want 0", questioner.calls)
			}
		})
	}
}

func TestFunctionURLQuestionTickIsSignatureGated(t *testing.T) {
	config := terminalTestConfig()
	ticker := &functionURLFakeTickProcessor{result: Result{Decision: DecisionAccepted, Code: "question_tick_idle"}}
	body, err := json.Marshal(QuestionTickRequest{Protocol: QuestionTickProtocol, AutomationRunID: config.ExpectedRunID, IssuedAt: functionURLTestNow})
	if err != nil {
		t.Fatalf("marshal tick: %v", err)
	}
	request := FunctionURLRequest{
		Body:    string(body),
		RawPath: QuestionTickPath,
		Headers: map[string]string{
			"Content-Type":              "application/json",
			QuestionTickSignatureHeader: SignQuestionTickRequest(config.HMACKey, body),
		},
		RequestContext: RequestContext{HTTP: RequestContextHTTP{Method: "POST", SourceIP: "192.0.2.3"}},
	}
	response, err := newFunctionURLQuestionHandler(t, nil, ticker).Handle(context.Background(), request)
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	functionURLAssertResult(t, response, 200, "question_tick_idle")
	signature := header(response.Headers, QuestionTickResponseSignatureHeader)
	if signature == "" || !VerifyQuestionTickResponseSignature(config.HMACKey, response.StatusCode, []byte(request.Body), []byte(response.Body), signature) {
		t.Fatalf("tick response signature did not verify: %+v", response)
	}
	if ticker.calls != 1 {
		t.Fatalf("ticker calls = %d, want 1", ticker.calls)
	}

	// A question-report signature replayed against the tick route is refused,
	// and a wrong protocol tag never reaches the processor.
	replay := request
	replay.Headers = map[string]string{"Content-Type": "application/json",
		QuestionTickSignatureHeader: SignQuestionReportRequest(config.HMACKey, body)}
	if response, err := newFunctionURLQuestionHandler(t, nil, ticker).Handle(context.Background(), replay); err != nil || response.StatusCode != 401 {
		t.Fatalf("replayed signature response = %+v, err = %v", response, err)
	}
	badProtocol, err := json.Marshal(QuestionTickRequest{Protocol: "question-tick-v2", AutomationRunID: config.ExpectedRunID, IssuedAt: functionURLTestNow})
	if err != nil {
		t.Fatalf("marshal tick: %v", err)
	}
	broken := request
	broken.Body = string(badProtocol)
	broken.Headers = map[string]string{"Content-Type": "application/json",
		QuestionTickSignatureHeader: SignQuestionTickRequest(config.HMACKey, badProtocol)}
	before := ticker.calls
	if response, err := newFunctionURLQuestionHandler(t, nil, ticker).Handle(context.Background(), broken); err != nil || response.StatusCode != 400 {
		t.Fatalf("broken protocol response = %+v, err = %v", response, err)
	}
	if ticker.calls != before {
		t.Fatal("broken tick reached the processor")
	}
}

// resumedOversizeEnvelope attaches a validly sealed, quote-heavy one-round
// clarification to the envelope so the combined encoding exceeds the old
// 64KB ingress cap while staying inside the delivery bound.
func resumedOversizeEnvelope(t *testing.T, envelope DispatchEnvelope) DispatchEnvelope {
	t.Helper()
	questionsJSON := `[{"id":"Q1","question":"` + strings.Repeat(`\"`, 5000) + `"}]`
	question := QuestionRecord{
		Protocol:          QuestionProtocolVersion,
		DeliveryID:        envelope.DeliveryID,
		InputSHA256:       envelope.Snapshot.InputSHA256,
		RepositoryID:      envelope.Snapshot.Target.RepositoryID,
		RepositorySHA256:  HashIdentity("example/automation-receiver"),
		WorkflowRefSHA256: envelope.Snapshot.Target.WorkflowRefSHA256,
		WorkflowSHA:       strings.Repeat("2", 40),
		WorkflowRunID:     123456789,
		RunAttempt:        1,
		AutomationRunID:   envelope.Snapshot.RunID,
		RunURL:            "https://github.com/example/automation-receiver/actions/runs/123456789/attempts/1",
		QuestionRevision:  1,
		QuestionsJSON:     questionsJSON,
		QuestionsSHA256:   TerminalReportDigest([]byte(questionsJSON)),
		DecisionSHA256:    strings.Repeat("c", 64),
		AnswerDeadlineAt:  4_000,
		NotifyAt:          [3]int64{1_000, 2_000, 3_000},
	}
	encodedQuestion, err := MarshalQuestionRecord(question)
	if err != nil {
		t.Fatalf("MarshalQuestionRecord() error = %v", err)
	}
	answers := `{"Q1":"a"}`
	record := ClarificationRecord{
		Protocol:          ClarificationProtocolVersion,
		DeliveryID:        question.DeliveryID,
		InputSHA256:       question.InputSHA256,
		RepositoryID:      question.RepositoryID,
		RepositorySHA256:  question.RepositorySHA256,
		WorkflowRefSHA256: question.WorkflowRefSHA256,
		AutomationRunID:   question.AutomationRunID,
		InputRevision:     2,
		Rounds: []ClarificationRound{{
			QuestionRecordJSON:   string(encodedQuestion),
			QuestionRecordSHA256: TerminalReportDigest(encodedQuestion),
			QuestionCommentID:    500,
			AnswerCommentID:      600,
			AnswererID:           7,
			AnswerPostedAt:       3_500,
			AnswerBodySHA256:     strings.Repeat("b", 64),
			AnswersJSON:          answers,
			AnswersSHA256:        TerminalReportDigest([]byte(answers)),
		}},
	}
	sealed, err := MarshalClarificationRecord(record)
	if err != nil {
		t.Fatalf("MarshalClarificationRecord() error = %v", err)
	}
	envelope.ClarificationJSON = string(sealed)
	if err := ValidateEnvelope(envelope); err != nil {
		t.Fatalf("resumed envelope does not validate: %v", err)
	}
	return envelope
}

func TestFunctionURLPullDeliversAResumedEnvelopeBeyondTheIngressCap(t *testing.T) {
	config := functionURLTestPullConfig()
	envelope := resumedOversizeEnvelope(t, functionURLTestEnvelope(t, config))
	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if len(encoded) <= 64*1024 || len(encoded) > MaxDeliveredEnvelopeBytes {
		t.Fatalf("test setup: envelope is %d bytes, want between the old cap and the delivery bound", len(encoded))
	}
	handler := newFunctionURLTestHandler(t, nil, &functionURLFakePullProcessor{envelope: envelope, disposition: PullAcquired}, 0)
	request := functionURLValidPullRequest(t, config, functionURLTestNow)
	response, err := handler.Handle(context.Background(), request)
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if response.StatusCode != 200 || response.Body != string(encoded) {
		t.Fatalf("status = %d, body bytes = %d, want the resumed envelope delivered", response.StatusCode, len(response.Body))
	}
}

// TestQuestionRoutesAnswer404BeforeWiringAnd401After pins the smoke test the
// instance-side activation runbook relies on: an unsigned request tells
// the operator whether the new routes exist yet, without needing the shared
// key. 404 means the deployed function predates them; 401 means they are live
// and authenticated.
func TestQuestionRoutesAnswer404BeforeWiringAnd401After(t *testing.T) {
	config := functionURLTestConfig(0)
	config.Report = terminalTestConfig()
	before, err := NewFunctionURLHandlerWithReporter(config, &functionURLFakeHookProcessor{}, &functionURLFakePullProcessor{},
		&functionURLFakeReportProcessor{result: Result{Decision: DecisionAccepted, Code: "terminal_report_recorded"}})
	if err != nil {
		t.Fatalf("NewFunctionURLHandlerWithReporter() error = %v", err)
	}
	after := newFunctionURLQuestionHandler(t, nil, nil)

	for _, path := range []string{QuestionReportPath, QuestionTickPath} {
		unsigned := FunctionURLRequest{
			Body:           `{}`,
			RawPath:        path,
			Headers:        map[string]string{"Content-Type": "application/json"},
			RequestContext: RequestContext{HTTP: RequestContextHTTP{Method: "POST", SourceIP: "192.0.2.3"}},
		}
		response, err := before.Handle(context.Background(), unsigned)
		if err != nil || response.StatusCode != 404 {
			t.Fatalf("%s before wiring: status = %d, err = %v", path, response.StatusCode, err)
		}
		response, err = after.Handle(context.Background(), unsigned)
		if err != nil || response.StatusCode != 401 {
			t.Fatalf("%s after wiring: status = %d, err = %v", path, response.StatusCode, err)
		}
		if response.Body != `{"decision":"invalid","code":"unauthorized"}` {
			t.Fatalf("%s unsigned body = %s", path, response.Body)
		}
	}
	// The existing pull route must keep answering 401 to the same probe.
	pull := FunctionURLRequest{
		Body:           `{}`,
		RawPath:        PullClaimPath,
		Headers:        map[string]string{"Content-Type": "application/json"},
		RequestContext: RequestContext{HTTP: RequestContextHTTP{Method: "POST", SourceIP: "192.0.2.3"}},
	}
	if response, err := after.Handle(context.Background(), pull); err != nil || response.StatusCode != 401 {
		t.Fatalf("pull route: status = %d, err = %v", response.StatusCode, err)
	}
}
