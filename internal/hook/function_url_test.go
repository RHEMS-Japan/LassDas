package hook

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

const functionURLTestRunID = "run_20260802_function_url"

var functionURLTestNow = time.Date(2026, 8, 2, 12, 34, 56, 0, time.UTC)

type functionURLFakeHookProcessor struct {
	result Result
	calls  int
	hints  []WebhookHint
}

func TestHeaderRejectsCaseInsensitiveDuplicates(t *testing.T) {
	headers := map[string]string{"Authorization": "first", "authorization": "second"}
	if got := header(headers, "authorization"); got != "" {
		t.Fatalf("header() = %q", got)
	}
}

func (f *functionURLFakeHookProcessor) Process(_ context.Context, hint WebhookHint) Result {
	f.calls++
	f.hints = append(f.hints, hint)
	return f.result
}

type functionURLFakePullProcessor struct {
	envelope    DispatchEnvelope
	disposition PullDisposition
	err         error
	calls       int
	requests    []PullClaimRequest
}

type functionURLFakeReportProcessor struct {
	result  Result
	calls   int
	reports []TerminalReportRequest
}

func (f *functionURLFakeReportProcessor) ProcessTerminalReport(_ context.Context, report TerminalReportRequest) Result {
	f.calls++
	f.reports = append(f.reports, report)
	return f.result
}

func (f *functionURLFakePullProcessor) Pull(_ context.Context, request PullClaimRequest) (DispatchEnvelope, PullDisposition, error) {
	f.calls++
	f.requests = append(f.requests, request)
	return f.envelope, f.disposition, f.err
}

func functionURLValidWebhookBody() string {
	return `{"id":303,"project":{"id":101,"projectKey":"TICKET","name":"ignored"},"type":1,"content":{"id":404,"key_id":505,"summary":"UNTRUSTED-TICKET-TEXT"},"createdUser":{"id":202,"name":"ignored"},"notifications":[]}`
}

func functionURLExpectedHint() WebhookHint {
	return WebhookHint{
		ActivityID: 303, ActivityType: 1,
		ProjectID: 101, ProjectKey: "TICKET",
		CreatorID: 202, IssueID: 404, IssueKeyID: 505,
	}
}

func functionURLTestPullConfig() PullRouteConfig {
	repositoryID := int64(987654321)
	workflowRefSHA256 := HashIdentity(".github/workflows/receive-ticket.yml@refs/heads/main")
	return PullRouteConfig{
		HMACKey:             []byte("0123456789abcdef0123456789abcdef"),
		RepositoryID:        repositoryID,
		RepositorySHA256:    HashIdentity("example/automation-receiver"),
		WorkflowRefSHA256:   workflowRefSHA256,
		SpaceKey:            "example-space",
		ProjectID:           101,
		ProjectKey:          "TICKET",
		AllowedCreatorID:    202,
		AllowedActivityType: 1,
		Target: DeliveryTarget{
			RepositoryID:      repositoryID,
			WorkflowRefSHA256: workflowRefSHA256,
		},
		ClockSkew: 5 * time.Minute,
	}
}

func functionURLTestConfig(maxBodyBytes int) FunctionURLConfig {
	if maxBodyBytes == 0 {
		maxBodyBytes = 16 * 1024
	}
	return FunctionURLConfig{
		BasicUsername: "hook-user",
		BasicPassword: "hook-password",
		MaxBodyBytes:  maxBodyBytes,
		Pull:          functionURLTestPullConfig(),
	}
}

func newFunctionURLTestHandler(t *testing.T, hookProcessor *functionURLFakeHookProcessor, pullProcessor *functionURLFakePullProcessor, maxBodyBytes int) *FunctionURLHandler {
	t.Helper()
	if hookProcessor == nil {
		hookProcessor = &functionURLFakeHookProcessor{result: Result{Decision: DecisionAccepted, Code: "queue_created"}}
	}
	if pullProcessor == nil {
		pullProcessor = &functionURLFakePullProcessor{disposition: PullEmpty}
	}
	handler, err := NewFunctionURLHandler(functionURLTestConfig(maxBodyBytes), hookProcessor, pullProcessor)
	if err != nil {
		t.Fatalf("NewFunctionURLHandler() error = %v", err)
	}
	handler.now = func() time.Time { return functionURLTestNow }
	return handler
}

func functionURLValidBacklogRequest() FunctionURLRequest {
	credential := base64.StdEncoding.EncodeToString([]byte("hook-user:hook-password"))
	return FunctionURLRequest{
		Body:    functionURLValidWebhookBody(),
		RawPath: backlogPath,
		Headers: map[string]string{
			"Authorization": "Basic " + credential,
			"Content-Type":  "application/json; charset=utf-8",
		},
		RequestContext: RequestContext{HTTP: RequestContextHTTP{Method: "POST", SourceIP: "192.0.2.1"}},
	}
}

func functionURLValidPull(t *testing.T, config PullRouteConfig, issuedAt time.Time) (PullRequest, []byte) {
	t.Helper()
	request := PullRequest{
		Protocol:          PullProtocolVersion,
		RepositoryID:      config.RepositoryID,
		RepositorySHA256:  config.RepositorySHA256,
		EventName:         "schedule",
		WorkflowRefSHA256: config.WorkflowRefSHA256,
		Ref:               "refs/heads/main",
		WorkflowSHA:       strings.Repeat("a", 40),
		WorkflowRunID:     123456789,
		RunAttempt:        1,
		AutomationRunID:   functionURLTestRunID,
		IssuedAt:          issuedAt.UTC(),
	}
	body, err := MarshalPullRequest(request)
	if err != nil {
		t.Fatalf("MarshalPullRequest() error = %v", err)
	}
	return request, body
}

func functionURLValidPullRequest(t *testing.T, config PullRouteConfig, issuedAt time.Time) FunctionURLRequest {
	t.Helper()
	_, body := functionURLValidPull(t, config, issuedAt)
	return FunctionURLRequest{
		Body:    string(body),
		RawPath: PullClaimPath,
		Headers: map[string]string{
			"Content-Type":      "application/json; charset=utf-8",
			PullSignatureHeader: SignPullRequest(config.HMACKey, body),
		},
		RequestContext: RequestContext{HTTP: RequestContextHTTP{Method: "POST", SourceIP: "192.0.2.2"}},
	}
}

func functionURLResignPull(request *FunctionURLRequest, key []byte) {
	request.Headers[PullSignatureHeader] = SignPullRequest(key, []byte(request.Body))
}

func functionURLValidTerminalRequest(t *testing.T, config ReportRouteConfig, issuedAt time.Time) FunctionURLRequest {
	t.Helper()
	report := terminalTestRequest(TerminalSuccess)
	report.IssuedAt = issuedAt.UTC()
	body, err := MarshalTerminalReportRequest(report)
	if err != nil {
		t.Fatalf("MarshalTerminalReportRequest() error = %v", err)
	}
	return FunctionURLRequest{
		Body:    string(body),
		RawPath: TerminalReportPath,
		Headers: map[string]string{
			"Content-Type":                "application/json; charset=utf-8",
			TerminalReportSignatureHeader: SignTerminalReportRequest(config.HMACKey, body),
		},
		RequestContext: RequestContext{HTTP: RequestContextHTTP{Method: "POST", SourceIP: "192.0.2.3"}},
	}
}

func newFunctionURLReportHandler(t *testing.T, reportProcessor *functionURLFakeReportProcessor) *FunctionURLHandler {
	t.Helper()
	config := functionURLTestConfig(0)
	config.Report = terminalTestConfig()
	if reportProcessor == nil {
		reportProcessor = &functionURLFakeReportProcessor{result: Result{Decision: DecisionAccepted, Code: "terminal_report_recorded"}}
	}
	handler, err := NewFunctionURLHandlerWithReporter(config, &functionURLFakeHookProcessor{}, &functionURLFakePullProcessor{}, reportProcessor)
	if err != nil {
		t.Fatalf("NewFunctionURLHandlerWithReporter() error = %v", err)
	}
	handler.now = func() time.Time { return functionURLTestNow }
	return handler
}

func functionURLAssertSignedTerminalResponse(t *testing.T, key []byte, request FunctionURLRequest, response FunctionURLResponse) {
	t.Helper()
	signature := header(response.Headers, TerminalReportResponseSignatureHeader)
	if signature == "" || !VerifyTerminalReportResponseSignature(key, response.StatusCode, []byte(request.Body), []byte(response.Body), signature) {
		t.Fatalf("terminal response signature did not verify: response=%+v", response)
	}
}

func functionURLTestEnvelope(t *testing.T, config PullRouteConfig) DispatchEnvelope {
	t.Helper()
	envelope, err := SealSnapshot(TicketSnapshot{
		SchemaVersion: SnapshotSchemaVersion,
		SpaceKey:      config.SpaceKey,
		ActivityID:    303,
		ActivityType:  config.AllowedActivityType,
		ProjectID:     config.ProjectID,
		ProjectKey:    config.ProjectKey,
		IssueID:       404,
		IssueKey:      "TICKET-505",
		IssueKeyID:    505,
		CreatorID:     config.AllowedCreatorID,
		RunID:         "TICKET-505",
		CreatedAt:     functionURLTestNow.Add(-time.Hour),
		Target:        config.Target,
		Untrusted: UntrustedTicketData{
			Summary:     "UNTRUSTED-TICKET-TEXT",
			Description: "UNTRUSTED-TICKET-DESCRIPTION",
		},
	})
	if err != nil {
		t.Fatalf("SealSnapshot() error = %v", err)
	}
	return envelope
}

func functionURLAssertResult(t *testing.T, response FunctionURLResponse, status int, code string) {
	t.Helper()
	if response.StatusCode != status {
		t.Fatalf("status = %d, want %d; response = %+v", response.StatusCode, status, response)
	}
	var result Result
	if err := json.Unmarshal([]byte(response.Body), &result); err != nil {
		t.Fatalf("response body is not a Result: %q: %v", response.Body, err)
	}
	if result.Code != code {
		t.Fatalf("result code = %q, want %q; response = %+v", result.Code, code, response)
	}
}

func functionURLAssertSignedPullResponse(t *testing.T, key []byte, request FunctionURLRequest, response FunctionURLResponse) {
	t.Helper()
	signature := header(response.Headers, PullResponseSignatureHeader)
	if signature == "" {
		t.Fatal("pull response is missing its signature")
	}
	if !VerifyPullResponseSignature(key, response.StatusCode, []byte(request.Body), []byte(response.Body), signature) {
		t.Fatalf("pull response signature did not verify: status=%d body=%q", response.StatusCode, response.Body)
	}
	if response.Headers["cache-control"] != "no-store" {
		t.Fatalf("cache-control = %q, want no-store", response.Headers["cache-control"])
	}
}

func TestFunctionURLBacklogAuthenticationStopsBeforeProcessing(t *testing.T) {
	tests := map[string]func(*FunctionURLRequest){
		"missing": func(r *FunctionURLRequest) {
			delete(r.Headers, "Authorization")
		},
		"wrong scheme": func(r *FunctionURLRequest) {
			r.Headers["Authorization"] = "Bearer token"
		},
		"invalid base64": func(r *FunctionURLRequest) {
			r.Headers["Authorization"] = "Basic !!!"
		},
		"embedded space": func(r *FunctionURLRequest) {
			r.Headers["Authorization"] = "Basic " + base64.StdEncoding.EncodeToString([]byte("hook-user:hook-password")) + " trailing"
		},
		"wrong username": func(r *FunctionURLRequest) {
			r.Headers["Authorization"] = "Basic " + base64.StdEncoding.EncodeToString([]byte("other:hook-password"))
		},
		"wrong password": func(r *FunctionURLRequest) {
			r.Headers["Authorization"] = "Basic " + base64.StdEncoding.EncodeToString([]byte("hook-user:other"))
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			hookProcessor := &functionURLFakeHookProcessor{}
			pullProcessor := &functionURLFakePullProcessor{}
			handler := newFunctionURLTestHandler(t, hookProcessor, pullProcessor, 0)
			request := functionURLValidBacklogRequest()
			mutate(&request)

			response, err := handler.Handle(context.Background(), request)
			if err != nil {
				t.Fatalf("Handle() error = %v", err)
			}
			functionURLAssertResult(t, response, 401, "unauthorized")
			if hookProcessor.calls != 0 || pullProcessor.calls != 0 {
				t.Fatalf("processors called: hook=%d pull=%d", hookProcessor.calls, pullProcessor.calls)
			}
			if response.Headers["www-authenticate"] != `Basic realm="backlog-hook"` {
				t.Fatalf("www-authenticate = %q", response.Headers["www-authenticate"])
			}
			for _, sensitive := range []string{"hook-password", "UNTRUSTED-TICKET-TEXT"} {
				if strings.Contains(response.Body, sensitive) {
					t.Fatalf("authentication error leaked %q", sensitive)
				}
			}
		})
	}
}

func TestFunctionURLBacklogParsesOnlyCanonicalHintFields(t *testing.T) {
	hookProcessor := &functionURLFakeHookProcessor{result: Result{Decision: DecisionAccepted, Code: "queue_created"}}
	pullProcessor := &functionURLFakePullProcessor{}
	response, err := newFunctionURLTestHandler(t, hookProcessor, pullProcessor, 0).Handle(context.Background(), functionURLValidBacklogRequest())
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	functionURLAssertResult(t, response, 202, "queue_created")
	if hookProcessor.calls != 1 || len(hookProcessor.hints) != 1 {
		t.Fatalf("hook calls = %d, hints = %d", hookProcessor.calls, len(hookProcessor.hints))
	}
	if hookProcessor.hints[0] != functionURLExpectedHint() {
		t.Fatalf("hint = %+v, want %+v", hookProcessor.hints[0], functionURLExpectedHint())
	}
	if pullProcessor.calls != 0 {
		t.Fatalf("pull processor calls = %d, want 0", pullProcessor.calls)
	}
}

func TestFunctionURLBacklogMapsProcessorDecisions(t *testing.T) {
	tests := map[Decision]int{
		DecisionAccepted:         202,
		DecisionIgnored:          202,
		DecisionInvalid:          400,
		DecisionRetryRequested:   503,
		DecisionDependencyFailed: 502,
		DecisionInternal:         500,
		Decision("unexpected"):   500,
	}
	for decision, status := range tests {
		t.Run(string(decision), func(t *testing.T) {
			hookProcessor := &functionURLFakeHookProcessor{result: Result{Decision: decision, Code: "fixed_code"}}
			response, err := newFunctionURLTestHandler(t, hookProcessor, nil, 0).Handle(context.Background(), functionURLValidBacklogRequest())
			if err != nil {
				t.Fatalf("Handle() error = %v", err)
			}
			functionURLAssertResult(t, response, status, "fixed_code")
			if hookProcessor.calls != 1 {
				t.Fatalf("hook processor calls = %d, want 1", hookProcessor.calls)
			}
		})
	}
}

func TestFunctionURLBacklogTransportFailuresStopBeforeProcessing(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*FunctionURLRequest)
		status int
		code   string
	}{
		{name: "method", mutate: func(r *FunctionURLRequest) { r.RequestContext.HTTP.Method = "GET" }, status: 405, code: "method_not_allowed"},
		{name: "query", mutate: func(r *FunctionURLRequest) { r.RawQueryString = "token=unexpected" }, status: 400, code: "query_not_allowed"},
		{name: "base64", mutate: func(r *FunctionURLRequest) { r.IsBase64Encoded = true }, status: 400, code: "base64_not_allowed"},
		{name: "missing content type", mutate: func(r *FunctionURLRequest) { delete(r.Headers, "Content-Type") }, status: 415, code: "content_type_not_allowed"},
		{name: "wrong content type", mutate: func(r *FunctionURLRequest) { r.Headers["Content-Type"] = "text/plain" }, status: 415, code: "content_type_not_allowed"},
		{name: "invalid json", mutate: func(r *FunctionURLRequest) { r.Body = `{` }, status: 400, code: "invalid_json"},
		{name: "multiple json values", mutate: func(r *FunctionURLRequest) { r.Body += `{}` }, status: 400, code: "multiple_json_values"},
		{name: "missing shape", mutate: func(r *FunctionURLRequest) { r.Body = `{}` }, status: 400, code: "invalid_webhook_shape"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hookProcessor := &functionURLFakeHookProcessor{}
			pullProcessor := &functionURLFakePullProcessor{}
			request := functionURLValidBacklogRequest()
			tt.mutate(&request)

			response, err := newFunctionURLTestHandler(t, hookProcessor, pullProcessor, 0).Handle(context.Background(), request)
			if err != nil {
				t.Fatalf("Handle() error = %v", err)
			}
			functionURLAssertResult(t, response, tt.status, tt.code)
			if hookProcessor.calls != 0 || pullProcessor.calls != 0 {
				t.Fatalf("processors called: hook=%d pull=%d", hookProcessor.calls, pullProcessor.calls)
			}
			if strings.Contains(response.Body, "UNTRUSTED-TICKET-TEXT") {
				t.Fatal("transport error echoed untrusted ticket text")
			}
		})
	}
}

func TestFunctionURLBacklogBodyByteBoundary(t *testing.T) {
	request := functionURLValidBacklogRequest()
	hookProcessor := &functionURLFakeHookProcessor{result: Result{Decision: DecisionAccepted, Code: "queue_created"}}
	handler := newFunctionURLTestHandler(t, hookProcessor, nil, len([]byte(request.Body)))

	response, err := handler.Handle(context.Background(), request)
	if err != nil {
		t.Fatalf("boundary Handle() error = %v", err)
	}
	functionURLAssertResult(t, response, 202, "queue_created")
	if hookProcessor.calls != 1 {
		t.Fatalf("boundary hook calls = %d, want 1", hookProcessor.calls)
	}

	hookProcessor.calls = 0
	request.Body += " "
	response, err = handler.Handle(context.Background(), request)
	if err != nil {
		t.Fatalf("over-boundary Handle() error = %v", err)
	}
	functionURLAssertResult(t, response, 413, "body_too_large")
	if hookProcessor.calls != 0 {
		t.Fatalf("over-boundary hook calls = %d, want 0", hookProcessor.calls)
	}
}

func TestFunctionURLPullRejectsInvalidHMACBeforeTransportAndProcessing(t *testing.T) {
	config := functionURLTestPullConfig()
	tests := map[string]func(*FunctionURLRequest){
		"missing": func(r *FunctionURLRequest) {
			delete(r.Headers, PullSignatureHeader)
		},
		"wrong": func(r *FunctionURLRequest) {
			r.Headers[PullSignatureHeader] = "sha256=" + strings.Repeat("0", 64)
		},
		"body changed after signing": func(r *FunctionURLRequest) {
			r.Body += " "
		},
		"valid signature for wrong key": func(r *FunctionURLRequest) {
			r.Headers[PullSignatureHeader] = SignPullRequest([]byte("abcdef0123456789abcdef0123456789"), []byte(r.Body))
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			hookProcessor := &functionURLFakeHookProcessor{}
			pullProcessor := &functionURLFakePullProcessor{}
			request := functionURLValidPullRequest(t, config, functionURLTestNow)
			request.RequestContext.HTTP.Method = "GET"
			request.RawQueryString = "should=not-be-evaluated"
			mutate(&request)

			response, err := newFunctionURLTestHandler(t, hookProcessor, pullProcessor, 0).Handle(context.Background(), request)
			if err != nil {
				t.Fatalf("Handle() error = %v", err)
			}
			functionURLAssertResult(t, response, 401, "unauthorized")
			if hookProcessor.calls != 0 || pullProcessor.calls != 0 {
				t.Fatalf("processors called: hook=%d pull=%d", hookProcessor.calls, pullProcessor.calls)
			}
		})
	}
}

func TestFunctionURLPullRequiresCanonicalJSON(t *testing.T) {
	config := functionURLTestPullConfig()
	request := functionURLValidPullRequest(t, config, functionURLTestNow)
	request.Body += "\n"
	functionURLResignPull(&request, config.HMACKey)
	pullProcessor := &functionURLFakePullProcessor{}

	response, err := newFunctionURLTestHandler(t, nil, pullProcessor, 0).Handle(context.Background(), request)
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	functionURLAssertResult(t, response, 400, "pull_request_invalid")
	functionURLAssertSignedPullResponse(t, config.HMACKey, request, response)
	if pullProcessor.calls != 0 {
		t.Fatalf("pull processor calls = %d, want 0", pullProcessor.calls)
	}
}

func TestFunctionURLPullPassesFixedRouteAndOwnerIdentity(t *testing.T) {
	config := functionURLTestPullConfig()
	pullRequest, body := functionURLValidPull(t, config, functionURLTestNow)
	request := functionURLValidPullRequest(t, config, functionURLTestNow)
	if request.Body != string(body) {
		t.Fatal("pull request helpers produced different canonical bodies")
	}
	pullProcessor := &functionURLFakePullProcessor{disposition: PullEmpty}

	response, err := newFunctionURLTestHandler(t, nil, pullProcessor, 0).Handle(context.Background(), request)
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if response.StatusCode != 204 || response.Body != "" {
		t.Fatalf("response = %+v, want signed empty 204", response)
	}
	functionURLAssertSignedPullResponse(t, config.HMACKey, request, response)
	if pullProcessor.calls != 1 || len(pullProcessor.requests) != 1 {
		t.Fatalf("pull calls = %d, requests = %d", pullProcessor.calls, len(pullProcessor.requests))
	}
	want := PullClaimRequest{
		SpaceKey:            config.SpaceKey,
		ProjectID:           config.ProjectID,
		ProjectKey:          config.ProjectKey,
		AllowedCreatorID:    config.AllowedCreatorID,
		AllowedActivityType: config.AllowedActivityType,
		Target:              config.Target,
		Owner: PullOwner{
			RepositoryID:      pullRequest.RepositoryID,
			RepositorySHA256:  pullRequest.RepositorySHA256,
			WorkflowRefSHA256: pullRequest.WorkflowRefSHA256,
			WorkflowSHA:       pullRequest.WorkflowSHA,
			WorkflowRunID:     pullRequest.WorkflowRunID,
			RunAttempt:        pullRequest.RunAttempt,
		},
		IssuedAt:  pullRequest.IssuedAt,
		ClaimedAt: functionURLTestNow,
		ClockSkew: config.ClockSkew,
	}
	if pullProcessor.requests[0] != want {
		t.Fatalf("pull request = %+v, want %+v", pullProcessor.requests[0], want)
	}
}

func TestFunctionURLPullRejectsNonAllowlistedRouteIdentity(t *testing.T) {
	config := functionURLTestPullConfig()
	tests := map[string]func(*PullRequest){
		"repository id": func(r *PullRequest) { r.RepositoryID++ },
		"repository hash": func(r *PullRequest) {
			r.RepositorySHA256 = HashIdentity("example/different-repository")
		},
		"workflow ref hash": func(r *PullRequest) {
			r.WorkflowRefSHA256 = HashIdentity(".github/workflows/different.yml@refs/heads/main")
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			pullRequest, _ := functionURLValidPull(t, config, functionURLTestNow)
			mutate(&pullRequest)
			body, err := MarshalPullRequest(pullRequest)
			if err != nil {
				t.Fatalf("MarshalPullRequest() error = %v", err)
			}
			request := functionURLValidPullRequest(t, config, functionURLTestNow)
			request.Body = string(body)
			functionURLResignPull(&request, config.HMACKey)
			pullProcessor := &functionURLFakePullProcessor{}

			response, handleErr := newFunctionURLTestHandler(t, nil, pullProcessor, 0).Handle(context.Background(), request)
			if handleErr != nil {
				t.Fatalf("Handle() error = %v", handleErr)
			}
			functionURLAssertResult(t, response, 403, "pull_route_not_allowed")
			functionURLAssertSignedPullResponse(t, config.HMACKey, request, response)
			if pullProcessor.calls != 0 {
				t.Fatalf("pull processor calls = %d, want 0", pullProcessor.calls)
			}
		})
	}
}

func TestFunctionURLPullRejectsInvalidWorkflowRunIdentity(t *testing.T) {
	config := functionURLTestPullConfig()
	tests := map[string]func(*PullRequest){
		"event":        func(r *PullRequest) { r.EventName = "push" },
		"ref":          func(r *PullRequest) { r.Ref = "refs/heads/stg" },
		"workflow sha": func(r *PullRequest) { r.WorkflowSHA = strings.Repeat("A", 40) },
		"workflow run": func(r *PullRequest) { r.WorkflowRunID = 0 },
		"run attempt":  func(r *PullRequest) { r.RunAttempt = 0 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			pullRequest, _ := functionURLValidPull(t, config, functionURLTestNow)
			mutate(&pullRequest)
			body, err := json.Marshal(pullRequest)
			if err != nil {
				t.Fatalf("json.Marshal() error = %v", err)
			}
			request := functionURLValidPullRequest(t, config, functionURLTestNow)
			request.Body = string(body)
			functionURLResignPull(&request, config.HMACKey)
			pullProcessor := &functionURLFakePullProcessor{}

			response, handleErr := newFunctionURLTestHandler(t, nil, pullProcessor, 0).Handle(context.Background(), request)
			if handleErr != nil {
				t.Fatalf("Handle() error = %v", handleErr)
			}
			functionURLAssertResult(t, response, 400, "pull_request_invalid")
			functionURLAssertSignedPullResponse(t, config.HMACKey, request, response)
			if pullProcessor.calls != 0 {
				t.Fatalf("pull processor calls = %d, want 0", pullProcessor.calls)
			}
		})
	}
}

func TestFunctionURLPullClockSkewBoundaries(t *testing.T) {
	config := functionURLTestPullConfig()
	tests := []struct {
		name       string
		issuedAt   time.Time
		wantStatus int
		wantCode   string
		wantCalls  int
	}{
		{name: "lower boundary", issuedAt: functionURLTestNow.Add(-config.ClockSkew), wantStatus: 204, wantCalls: 1},
		{name: "upper boundary", issuedAt: functionURLTestNow.Add(config.ClockSkew), wantStatus: 204, wantCalls: 1},
		{name: "too old", issuedAt: functionURLTestNow.Add(-config.ClockSkew - time.Nanosecond), wantStatus: 403, wantCode: "pull_timestamp_not_allowed"},
		{name: "too far in future", issuedAt: functionURLTestNow.Add(config.ClockSkew + time.Nanosecond), wantStatus: 403, wantCode: "pull_timestamp_not_allowed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := functionURLValidPullRequest(t, config, tt.issuedAt)
			pullProcessor := &functionURLFakePullProcessor{disposition: PullEmpty}
			response, err := newFunctionURLTestHandler(t, nil, pullProcessor, 0).Handle(context.Background(), request)
			if err != nil {
				t.Fatalf("Handle() error = %v", err)
			}
			if response.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d; response = %+v", response.StatusCode, tt.wantStatus, response)
			}
			if tt.wantCode != "" {
				functionURLAssertResult(t, response, tt.wantStatus, tt.wantCode)
			}
			functionURLAssertSignedPullResponse(t, config.HMACKey, request, response)
			if pullProcessor.calls != tt.wantCalls {
				t.Fatalf("pull calls = %d, want %d", pullProcessor.calls, tt.wantCalls)
			}
		})
	}
}

func TestFunctionURLPullTransportFailuresAreSignedAndStopBeforeProcessing(t *testing.T) {
	config := functionURLTestPullConfig()
	tests := []struct {
		name   string
		mutate func(*FunctionURLRequest)
		status int
		code   string
	}{
		{name: "method", mutate: func(r *FunctionURLRequest) { r.RequestContext.HTTP.Method = "GET" }, status: 405, code: "method_not_allowed"},
		{name: "query", mutate: func(r *FunctionURLRequest) { r.RawQueryString = "token=unexpected" }, status: 400, code: "query_not_allowed"},
		{name: "base64", mutate: func(r *FunctionURLRequest) { r.IsBase64Encoded = true }, status: 400, code: "base64_not_allowed"},
		{name: "missing content type", mutate: func(r *FunctionURLRequest) { delete(r.Headers, "Content-Type") }, status: 415, code: "content_type_not_allowed"},
		{name: "wrong content type", mutate: func(r *FunctionURLRequest) { r.Headers["Content-Type"] = "text/plain" }, status: 415, code: "content_type_not_allowed"},
		{name: "body too large", mutate: func(r *FunctionURLRequest) {
			r.Body = strings.Repeat("x", MaxPullRequestBytes+1)
		}, status: 413, code: "body_too_large"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := functionURLValidPullRequest(t, config, functionURLTestNow)
			tt.mutate(&request)
			functionURLResignPull(&request, config.HMACKey)
			pullProcessor := &functionURLFakePullProcessor{}

			response, err := newFunctionURLTestHandler(t, nil, pullProcessor, 0).Handle(context.Background(), request)
			if err != nil {
				t.Fatalf("Handle() error = %v", err)
			}
			functionURLAssertResult(t, response, tt.status, tt.code)
			functionURLAssertSignedPullResponse(t, config.HMACKey, request, response)
			if pullProcessor.calls != 0 {
				t.Fatalf("pull processor calls = %d, want 0", pullProcessor.calls)
			}
		})
	}
}

func TestFunctionURLRoutesAndAuthenticationAreSeparated(t *testing.T) {
	config := functionURLTestPullConfig()
	tests := []struct {
		name       string
		request    func(*testing.T) FunctionURLRequest
		wantStatus int
		wantHook   int
		wantPull   int
	}{
		{
			name: "backlog uses basic only",
			request: func(_ *testing.T) FunctionURLRequest {
				request := functionURLValidBacklogRequest()
				request.Headers[PullSignatureHeader] = "invalid-but-ignored-on-backlog"
				return request
			},
			wantStatus: 202, wantHook: 1,
		},
		{
			name: "pull uses hmac only",
			request: func(t *testing.T) FunctionURLRequest {
				return functionURLValidPullRequest(t, config, functionURLTestNow)
			},
			wantStatus: 204, wantPull: 1,
		},
		{
			name: "pull rejects basic without hmac",
			request: func(t *testing.T) FunctionURLRequest {
				request := functionURLValidPullRequest(t, config, functionURLTestNow)
				delete(request.Headers, PullSignatureHeader)
				request.Headers["Authorization"] = functionURLValidBacklogRequest().Headers["Authorization"]
				return request
			},
			wantStatus: 401,
		},
		{
			name: "backlog rejects hmac without basic",
			request: func(_ *testing.T) FunctionURLRequest {
				request := functionURLValidBacklogRequest()
				delete(request.Headers, "Authorization")
				request.Headers[PullSignatureHeader] = SignPullRequest(config.HMACKey, []byte(request.Body))
				return request
			},
			wantStatus: 401,
		},
		{
			name: "backlog trailing slash is not routed",
			request: func(_ *testing.T) FunctionURLRequest {
				request := functionURLValidBacklogRequest()
				request.RawPath += "/"
				return request
			},
			wantStatus: 404,
		},
		{
			name: "pull trailing slash is not routed",
			request: func(t *testing.T) FunctionURLRequest {
				request := functionURLValidPullRequest(t, config, functionURLTestNow)
				request.RawPath += "/"
				return request
			},
			wantStatus: 404,
		},
		{
			name: "unknown route",
			request: func(_ *testing.T) FunctionURLRequest {
				request := functionURLValidBacklogRequest()
				request.RawPath = "/unknown"
				return request
			},
			wantStatus: 404,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hookProcessor := &functionURLFakeHookProcessor{result: Result{Decision: DecisionAccepted, Code: "queue_created"}}
			pullProcessor := &functionURLFakePullProcessor{disposition: PullEmpty}
			response, err := newFunctionURLTestHandler(t, hookProcessor, pullProcessor, 0).Handle(context.Background(), tt.request(t))
			if err != nil {
				t.Fatalf("Handle() error = %v", err)
			}
			if response.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d; response = %+v", response.StatusCode, tt.wantStatus, response)
			}
			if hookProcessor.calls != tt.wantHook || pullProcessor.calls != tt.wantPull {
				t.Fatalf("processor calls: hook=%d want=%d, pull=%d want=%d", hookProcessor.calls, tt.wantHook, pullProcessor.calls, tt.wantPull)
			}
		})
	}
}

func TestFunctionURLPullReturnsSignedEnvelope(t *testing.T) {
	config := functionURLTestPullConfig()
	envelope := functionURLTestEnvelope(t, config)
	pullProcessor := &functionURLFakePullProcessor{envelope: envelope, disposition: PullAcquired}
	request := functionURLValidPullRequest(t, config, functionURLTestNow)

	response, err := newFunctionURLTestHandler(t, nil, pullProcessor, 0).Handle(context.Background(), request)
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if response.StatusCode != 200 || response.Headers["content-type"] != "application/json" {
		t.Fatalf("response = %+v, want JSON 200", response)
	}
	functionURLAssertSignedPullResponse(t, config.HMACKey, request, response)
	var got DispatchEnvelope
	if err := json.Unmarshal([]byte(response.Body), &got); err != nil {
		t.Fatalf("json.Unmarshal(response) error = %v", err)
	}
	if got != envelope {
		t.Fatalf("envelope = %+v, want %+v", got, envelope)
	}
	if VerifyPullResponseSignature(config.HMACKey, response.StatusCode, []byte(request.Body), []byte(response.Body+" "), header(response.Headers, PullResponseSignatureHeader)) {
		t.Fatal("response signature accepted a modified body")
	}
}

func TestFunctionURLPullReturnsSignedEmptyResponse(t *testing.T) {
	config := functionURLTestPullConfig()
	for _, disposition := range []PullDisposition{PullEmpty, PullClaimed} {
		t.Run(string(disposition), func(t *testing.T) {
			pullProcessor := &functionURLFakePullProcessor{disposition: disposition}
			request := functionURLValidPullRequest(t, config, functionURLTestNow)
			response, err := newFunctionURLTestHandler(t, nil, pullProcessor, 0).Handle(context.Background(), request)
			if err != nil {
				t.Fatalf("Handle() error = %v", err)
			}
			if response.StatusCode != 204 || response.Body != "" {
				t.Fatalf("response = %+v, want empty 204", response)
			}
			if _, ok := response.Headers["content-type"]; ok {
				t.Fatalf("empty 204 included content-type: %+v", response.Headers)
			}
			functionURLAssertSignedPullResponse(t, config.HMACKey, request, response)
		})
	}
}

func TestFunctionURLPullReturnsSignedFixedErrors(t *testing.T) {
	config := functionURLTestPullConfig()
	tests := []struct {
		name       string
		processor  func(*testing.T) *functionURLFakePullProcessor
		maxBytes   int
		wantStatus int
		wantCode   string
	}{
		{
			name: "conflict", wantStatus: 409, wantCode: "pull_conflict",
			processor: func(*testing.T) *functionURLFakePullProcessor {
				return &functionURLFakePullProcessor{disposition: PullConflict}
			},
		},
		{
			name: "retryable dependency error", wantStatus: 503, wantCode: "pull_unavailable",
			processor: func(*testing.T) *functionURLFakePullProcessor {
				return &functionURLFakePullProcessor{err: NewExternalFailure("store", FailureRetryable, "timeout")}
			},
		},
		{
			name: "rejected dependency error", wantStatus: 409, wantCode: "pull_rejected",
			processor: func(*testing.T) *functionURLFakePullProcessor {
				return &functionURLFakePullProcessor{err: NewExternalFailure("store", FailureRejected, "conflict")}
			},
		},
		{
			name: "unknown dependency error", wantStatus: 409, wantCode: "pull_rejected",
			processor: func(*testing.T) *functionURLFakePullProcessor {
				return &functionURLFakePullProcessor{err: errors.New("AWS_SECRET=SUPER-SECRET; UNTRUSTED-TICKET-TEXT")}
			},
		},
		{
			name: "invalid disposition", wantStatus: 500, wantCode: "pull_state_invalid",
			processor: func(*testing.T) *functionURLFakePullProcessor {
				return &functionURLFakePullProcessor{disposition: PullDisposition("unexpected")}
			},
		},
		{
			name: "invalid envelope", wantStatus: 500, wantCode: "pull_envelope_invalid",
			processor: func(t *testing.T) *functionURLFakePullProcessor {
				envelope := functionURLTestEnvelope(t, config)
				envelope.Snapshot.Untrusted.Summary = "UNTRUSTED-TICKET-TEXT-TAMPERED"
				return &functionURLFakePullProcessor{envelope: envelope, disposition: PullAcquired}
			},
		},
		{
			// The pull response is bounded by the shared delivery bound, not
			// the ingress body cap: a resumed envelope may legitimately exceed
			// the cap and must still be delivered (see the oversize case for
			// the bound itself).
			name: "response exceeds the delivery bound", maxBytes: 1, wantStatus: 500, wantCode: "pull_response_failed",
			processor: func(t *testing.T) *functionURLFakePullProcessor {
				oversize := functionURLTestEnvelope(t, config)
				snapshot := oversize.Snapshot
				snapshot.Untrusted.Description = strings.Repeat("a", MaxDeliveredEnvelopeBytes)
				resealed, err := SealSnapshot(snapshot)
				if err != nil {
					t.Fatalf("SealSnapshot() error = %v", err)
				}
				return &functionURLFakePullProcessor{envelope: resealed, disposition: PullAcquired}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := functionURLValidPullRequest(t, config, functionURLTestNow)
			response, err := newFunctionURLTestHandler(t, nil, tt.processor(t), tt.maxBytes).Handle(context.Background(), request)
			if err != nil {
				t.Fatalf("Handle() error = %v", err)
			}
			functionURLAssertResult(t, response, tt.wantStatus, tt.wantCode)
			functionURLAssertSignedPullResponse(t, config.HMACKey, request, response)
			for _, sensitive := range []string{"SUPER-SECRET", "UNTRUSTED-TICKET-TEXT", "UNTRUSTED-TICKET-DESCRIPTION"} {
				if strings.Contains(response.Body, sensitive) {
					t.Fatalf("error response leaked %q: %s", sensitive, response.Body)
				}
			}
		})
	}
}

func TestFunctionURLPullMalformedRequestDoesNotEchoTicketText(t *testing.T) {
	config := functionURLTestPullConfig()
	request := functionURLValidPullRequest(t, config, functionURLTestNow)
	request.Body = `{"protocol":"pull-claim-v1","ticket":"UNTRUSTED-TICKET-TEXT","secret":"SUPER-SECRET"}`
	functionURLResignPull(&request, config.HMACKey)

	response, err := newFunctionURLTestHandler(t, nil, &functionURLFakePullProcessor{}, 0).Handle(context.Background(), request)
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	functionURLAssertResult(t, response, 400, "pull_request_invalid")
	functionURLAssertSignedPullResponse(t, config.HMACKey, request, response)
	for _, sensitive := range []string{"SUPER-SECRET", "UNTRUSTED-TICKET-TEXT"} {
		if strings.Contains(response.Body, sensitive) {
			t.Fatalf("malformed-request response leaked %q", sensitive)
		}
	}
}

func TestFunctionURLConfigFailsClosed(t *testing.T) {
	tests := map[string]func(*FunctionURLConfig){
		"missing basic username": func(c *FunctionURLConfig) { c.BasicUsername = "" },
		"missing basic password": func(c *FunctionURLConfig) { c.BasicPassword = "" },
		"colon in username":      func(c *FunctionURLConfig) { c.BasicUsername = "bad:user" },
		"newline in password":    func(c *FunctionURLConfig) { c.BasicPassword = "bad\npass" },
		"zero body limit":        func(c *FunctionURLConfig) { c.MaxBodyBytes = 0 },
		"oversized body limit":   func(c *FunctionURLConfig) { c.MaxBodyBytes = 1024*1024 + 1 },
		"short pull key":         func(c *FunctionURLConfig) { c.Pull.HMACKey = []byte("short") },
		"missing repository id":  func(c *FunctionURLConfig) { c.Pull.RepositoryID = 0 },
		"invalid repository hash": func(c *FunctionURLConfig) {
			c.Pull.RepositorySHA256 = "not-a-hash"
		},
		"invalid workflow hash": func(c *FunctionURLConfig) {
			c.Pull.WorkflowRefSHA256 = "not-a-hash"
		},
		"target repository mismatch": func(c *FunctionURLConfig) {
			c.Pull.Target.RepositoryID++
		},
		"target workflow mismatch": func(c *FunctionURLConfig) {
			c.Pull.Target.WorkflowRefSHA256 = HashIdentity("different workflow")
		},
		"zero clock skew":  func(c *FunctionURLConfig) { c.Pull.ClockSkew = 0 },
		"large clock skew": func(c *FunctionURLConfig) { c.Pull.ClockSkew = 10*time.Minute + time.Nanosecond },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			config := functionURLTestConfig(0)
			mutate(&config)
			if _, err := NewFunctionURLHandler(config, &functionURLFakeHookProcessor{}, &functionURLFakePullProcessor{}); err == nil {
				t.Fatal("NewFunctionURLHandler() accepted unsafe config")
			}
		})
	}

	valid := functionURLTestConfig(0)
	if _, err := NewFunctionURLHandler(valid, nil, &functionURLFakePullProcessor{}); err == nil {
		t.Fatal("NewFunctionURLHandler() accepted a nil hook processor")
	}
	if _, err := NewFunctionURLHandler(valid, &functionURLFakeHookProcessor{}, nil); err == nil {
		t.Fatal("NewFunctionURLHandler() accepted a nil pull processor")
	}
}

func TestFunctionURLHandlerCopiesPullHMACKey(t *testing.T) {
	config := functionURLTestConfig(0)
	originalKey := append([]byte(nil), config.Pull.HMACKey...)
	handler, err := NewFunctionURLHandler(config, &functionURLFakeHookProcessor{}, &functionURLFakePullProcessor{disposition: PullEmpty})
	if err != nil {
		t.Fatalf("NewFunctionURLHandler() error = %v", err)
	}
	handler.now = func() time.Time { return functionURLTestNow }
	for index := range config.Pull.HMACKey {
		config.Pull.HMACKey[index] = 0
	}
	request := functionURLValidPullRequest(t, functionURLTestPullConfig(), functionURLTestNow)
	request.Headers[PullSignatureHeader] = SignPullRequest(originalKey, []byte(request.Body))

	response, err := handler.Handle(context.Background(), request)
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if response.StatusCode != 204 {
		t.Fatalf("status = %d, want 204; response = %+v", response.StatusCode, response)
	}
	functionURLAssertSignedPullResponse(t, originalKey, request, response)
}

func TestFunctionURLResponseResultJSONNeverContainsFormattingArtifacts(t *testing.T) {
	response := resultResponse(400, Result{Decision: DecisionInvalid, Code: "fixed_code"})
	if got, want := response.Body, `{"decision":"invalid","code":"fixed_code"}`; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
	if response.IsBase64Encoded {
		t.Fatal("response unexpectedly marked as base64")
	}
	if response.Headers["content-type"] != "application/json" || response.Headers["cache-control"] != "no-store" {
		t.Fatalf("response headers = %+v", response.Headers)
	}
}

func TestFunctionURLTestFixturesRemainWithinConfiguredLimits(t *testing.T) {
	config := functionURLTestConfig(0)
	request := functionURLValidPullRequest(t, config.Pull, functionURLTestNow)
	if len([]byte(request.Body)) > MaxPullRequestBytes {
		t.Fatalf("valid pull fixture is %d bytes, limit is %d", len([]byte(request.Body)), MaxPullRequestBytes)
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("valid function URL config rejected: %v", err)
	}
}

func TestFunctionURLTerminalReportIsRouteBoundAndResponseSigned(t *testing.T) {
	config := terminalTestConfig()
	processor := &functionURLFakeReportProcessor{result: Result{Decision: DecisionAccepted, Code: "terminal_report_recorded"}}
	request := functionURLValidTerminalRequest(t, config, functionURLTestNow)
	response, err := newFunctionURLReportHandler(t, processor).Handle(context.Background(), request)
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	functionURLAssertResult(t, response, 200, "terminal_report_recorded")
	functionURLAssertSignedTerminalResponse(t, config.HMACKey, request, response)
	if processor.calls != 1 || len(processor.reports) != 1 || processor.reports[0].DeliveryID != terminalTestRequest(TerminalSuccess).DeliveryID {
		t.Fatalf("report processor calls = %d reports = %+v", processor.calls, processor.reports)
	}
}

func TestFunctionURLTerminalReportRejectsBeforeProcessingAndAlwaysSigns(t *testing.T) {
	config := terminalTestConfig()
	tests := []struct {
		name   string
		mutate func(*FunctionURLRequest)
		status int
		code   string
	}{
		{name: "wrong signature", mutate: func(r *FunctionURLRequest) {
			r.Headers[TerminalReportSignatureHeader] = "sha256=" + strings.Repeat("0", 64)
		}, status: 401, code: "unauthorized"},
		{name: "wrong method", mutate: func(r *FunctionURLRequest) {
			r.RequestContext.HTTP.Method = "GET"
			r.Headers[TerminalReportSignatureHeader] = SignTerminalReportRequest(config.HMACKey, []byte(r.Body))
		}, status: 405, code: "method_not_allowed"},
		{name: "non canonical", mutate: func(r *FunctionURLRequest) {
			r.Body += "\n"
			r.Headers[TerminalReportSignatureHeader] = SignTerminalReportRequest(config.HMACKey, []byte(r.Body))
		}, status: 400, code: "terminal_report_invalid"},
		{name: "wrong route", mutate: func(r *FunctionURLRequest) {
			report := terminalTestRequest(TerminalSuccess)
			report.RepositoryID++
			body, _ := MarshalTerminalReportRequest(report)
			r.Body = string(body)
			r.Headers[TerminalReportSignatureHeader] = SignTerminalReportRequest(config.HMACKey, body)
		}, status: 403, code: "terminal_report_not_allowed"},
		{name: "stale", mutate: func(r *FunctionURLRequest) {
			report := terminalTestRequest(TerminalSuccess)
			report.IssuedAt = functionURLTestNow.Add(-config.ClockSkew - time.Nanosecond)
			body, _ := MarshalTerminalReportRequest(report)
			r.Body = string(body)
			r.Headers[TerminalReportSignatureHeader] = SignTerminalReportRequest(config.HMACKey, body)
		}, status: 403, code: "terminal_report_timestamp_not_allowed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			processor := &functionURLFakeReportProcessor{}
			request := functionURLValidTerminalRequest(t, config, functionURLTestNow)
			tt.mutate(&request)
			response, err := newFunctionURLReportHandler(t, processor).Handle(context.Background(), request)
			if err != nil {
				t.Fatalf("Handle() error = %v", err)
			}
			functionURLAssertResult(t, response, tt.status, tt.code)
			functionURLAssertSignedTerminalResponse(t, config.HMACKey, request, response)
			if processor.calls != 0 {
				t.Fatalf("processor calls = %d, want 0", processor.calls)
			}
		})
	}
}

func TestFunctionURLTerminalReportMapsOnlyFixedResults(t *testing.T) {
	config := terminalTestConfig()
	tests := []struct {
		result Result
		status int
	}{
		{Result{Decision: DecisionAccepted, Code: "terminal_report_recorded"}, 200},
		{Result{Decision: DecisionInvalid, Code: "terminal_report_conflict"}, 409},
		{Result{Decision: DecisionInvalid, Code: "terminal_report_invalid"}, 400},
		{Result{Decision: DecisionRetryRequested, Code: "terminal_report_pending"}, 503},
		{Result{Decision: DecisionDependencyFailed, Code: "terminal_comment_add_rejected"}, 502},
		{Result{Decision: DecisionInternal, Code: "terminal_report_state_invalid"}, 500},
	}
	for _, tt := range tests {
		processor := &functionURLFakeReportProcessor{result: tt.result}
		request := functionURLValidTerminalRequest(t, config, functionURLTestNow)
		response, err := newFunctionURLReportHandler(t, processor).Handle(context.Background(), request)
		if err != nil {
			t.Fatalf("Handle() error = %v", err)
		}
		functionURLAssertResult(t, response, tt.status, tt.result.Code)
		functionURLAssertSignedTerminalResponse(t, config.HMACKey, request, response)
	}
}

func TestFunctionURLReporterConstructorFailsClosed(t *testing.T) {
	config := functionURLTestConfig(0)
	if _, err := NewFunctionURLHandlerWithReporter(config, &functionURLFakeHookProcessor{}, &functionURLFakePullProcessor{}, &functionURLFakeReportProcessor{}); err == nil {
		t.Fatal("reporter constructor accepted an empty report route")
	}
	config.Report = terminalTestConfig()
	if _, err := NewFunctionURLHandlerWithReporter(config, &functionURLFakeHookProcessor{}, &functionURLFakePullProcessor{}, nil); err == nil {
		t.Fatal("reporter constructor accepted a nil report processor")
	}
}

// The run is named by the ticket, so a report naming its own issue key must
// pass the edge even though the configured run id is a different fixed value.
// This is the gate that silently refused the first live failure report
// (2026-08-06): the service had been rebound to per-ticket run ids, the edge
// had not.
func TestFunctionURLTerminalReportAcceptsARunNamedByItsTicket(t *testing.T) {
	config := terminalTestConfig()
	processor := &functionURLFakeReportProcessor{result: Result{Decision: DecisionAccepted, Code: "terminal_report_recorded"}}
	report := terminalTestRequest(TerminalInternalFailed)
	report.AutomationRunID = "TICKET-486"
	report.IssuedAt = functionURLTestNow.UTC()
	body, err := MarshalTerminalReportRequest(report)
	if err != nil {
		t.Fatalf("MarshalTerminalReportRequest() error = %v", err)
	}
	request := FunctionURLRequest{
		Body:    string(body),
		RawPath: TerminalReportPath,
		Headers: map[string]string{
			"Content-Type":                "application/json; charset=utf-8",
			TerminalReportSignatureHeader: SignTerminalReportRequest(config.HMACKey, body),
		},
		RequestContext: RequestContext{HTTP: RequestContextHTTP{Method: "POST", SourceIP: "192.0.2.3"}},
	}
	response, handleErr := newFunctionURLReportHandler(t, processor).Handle(context.Background(), request)
	if handleErr != nil {
		t.Fatalf("Handle() error = %v", handleErr)
	}
	functionURLAssertResult(t, response, 200, "terminal_report_recorded")
	if processor.calls != 1 {
		t.Fatalf("the edge refused a run named by its ticket: calls = %d", processor.calls)
	}
}
