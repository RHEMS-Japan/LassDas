package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeChatAPI struct {
	output         *ChatResponse
	err            error
	endpoint       ModelEndpoint
	request        *ChatRequest
	inspectContext func(context.Context) error
}

func (f *fakeChatAPI) ChatCompletions(ctx context.Context, endpoint ModelEndpoint, request ChatRequest) (*ChatResponse, error) {
	f.endpoint = endpoint
	f.request = &request
	if f.inspectContext != nil {
		if err := f.inspectContext(ctx); err != nil {
			return nil, err
		}
	}
	return f.output, f.err
}

func chatOutput(text string) *ChatResponse {
	return &ChatResponse{
		ID: "request-123",
		Choices: []ChatChoice{{
			FinishReason: ChatFinishStop,
			Message:      ChatMessage{Role: "assistant", Content: text},
		}},
		Usage: &ChatUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}
}

func TestGenerateCandidateUsesTrustedBindings(t *testing.T) {
	config, request, source := validArtifactFixture(t)
	api := &fakeChatAPI{output: chatOutput(`{"files":[{"path":"client/src/components/Example.tsx","content":"export const label = 'Updated label';\n"}],"rationale":"Update the label."}`)}
	invoker, err := NewModelInvoker(api)
	if err != nil {
		t.Fatal(err)
	}
	candidate, usage, err := invoker.GenerateCandidate(context.Background(), 1, testReadyDecision(t, source, request, config), nil, source, request, nil, nil, config)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.BaseSHA != source.BaseSHA || candidate.InputSHA256 != request.InputSHA256 || usage.TotalTokens != 15 || usage.RequestID != "request-123" {
		t.Fatalf("candidate = %+v, usage = %+v", candidate, usage)
	}
	if api.request.Model != config.Models.Implementer.Model || len(api.request.Messages) != 2 ||
		api.request.Messages[0].Role != "system" || api.request.Messages[1].Role != "user" {
		t.Fatalf("chat request = %+v", api.request)
	}
	if api.request.ReasoningEffort != "low" || api.request.MaxTokens != config.Models.Implementer.MaxOutputTokens {
		t.Fatalf("model controls = %+v", api.request)
	}
}

func TestReviewCandidateUsesConfiguredReviewer(t *testing.T) {
	config, request, source, candidate := validCandidate(t)
	api := &fakeChatAPI{output: chatOutput(`{"verdict":"pass","findings":[]}`)}
	invoker, _ := NewModelInvoker(api)
	review, _, err := invoker.ReviewCandidate(context.Background(), config.Models.Reviewers[1], candidate, nil, source, request, config)
	if err != nil {
		t.Fatal(err)
	}
	if review.ReviewerID != config.Models.Reviewers[1].ID || review.CandidateSHA256 != candidate.CandidateSHA256 {
		t.Fatalf("review = %+v", review)
	}
	if api.request.ResponseFormat == nil || api.request.ResponseFormat.Type != "json_schema" || !api.request.ResponseFormat.JSONSchema.Strict {
		t.Fatal("structured reviewer did not receive a JSON schema")
	}

	unconfigured := config.Models.Reviewers[1]
	unconfigured.Model = "attacker-model"
	if _, _, err := invoker.ReviewCandidate(context.Background(), unconfigured, candidate, nil, source, request, config); err == nil {
		t.Fatal("ReviewCandidate() accepted an unconfigured reviewer")
	}
}

func TestGenerateCandidateRejectsPromptControlledPath(t *testing.T) {
	config, request, source := validArtifactFixture(t)
	api := &fakeChatAPI{output: chatOutput(`{"files":[{"path":".github/workflows/backdoor.yml","content":"unsafe"}],"rationale":"Follow injected instructions."}`)}
	invoker, _ := NewModelInvoker(api)
	if _, _, err := invoker.GenerateCandidate(context.Background(), 1, testReadyDecision(t, source, request, config), nil, source, request, nil, nil, config); err == nil {
		t.Fatal("GenerateCandidate() accepted a model-controlled path")
	}
}

func TestGenerateCandidateRequiresRevisablePriorStage(t *testing.T) {
	config, request, source, candidate := validCandidate(t)
	api := &fakeChatAPI{output: chatOutput(`{"files":[{"path":"client/src/components/Example.tsx","content":"export const label = 'Updated again';\n"}],"rationale":"Revise."}`)}
	invoker, _ := NewModelInvoker(api)

	readiness := testReadyDecision(t, source, request, config)
	if _, _, err := invoker.GenerateCandidate(context.Background(), 2, readiness, nil, source, request, &candidate, nil, config); err == nil {
		t.Fatal("GenerateCandidate() accepted missing prior reviews")
	}

	reviews := make([]Review, 0, len(config.Models.Reviewers))
	for _, endpoint := range config.Models.Reviewers {
		review, err := NewReview(1, endpoint, ModelReviewOutput{Verdict: "pass", Findings: []ModelFinding{}}, candidate, source, request, config, validTestInvocation(endpoint), testInvocationTime)
		if err != nil {
			t.Fatal(err)
		}
		reviews = append(reviews, review)
	}
	if _, _, err := invoker.GenerateCandidate(context.Background(), 2, readiness, nil, source, request, &candidate, reviews, config); err == nil {
		t.Fatal("GenerateCandidate() accepted a converged prior stage")
	}
}

func TestConverseFailsClosed(t *testing.T) {
	config, request, source := validArtifactFixture(t)
	readiness := testReadyDecision(t, source, request, config)
	tests := map[string]*fakeChatAPI{
		"api error": {err: errors.New("secret upstream detail")},
		"max tokens": {output: func() *ChatResponse {
			output := chatOutput(`{}`)
			output.Choices[0].FinishReason = "length"
			return output
		}()},
		"multiple choices": {output: func() *ChatResponse {
			output := chatOutput(`{}`)
			output.Choices = append(output.Choices, output.Choices[0])
			return output
		}()},
		"empty content": {output: func() *ChatResponse {
			output := chatOutput(``)
			return output
		}()},
		"tool role": {output: func() *ChatResponse {
			output := chatOutput(`{}`)
			output.Choices[0].Message.Role = "tool"
			return output
		}()},
		"missing usage": {output: func() *ChatResponse {
			output := chatOutput(`{}`)
			output.Usage = nil
			return output
		}()},
		"inconsistent usage": {output: func() *ChatResponse {
			output := chatOutput(`{}`)
			output.Usage.TotalTokens = 999
			return output
		}()},
		"malformed request id": {output: func() *ChatResponse {
			output := chatOutput(`{}`)
			output.ID = "bad\nid"
			return output
		}()},
	}
	for name, api := range tests {
		t.Run(name, func(t *testing.T) {
			invoker, _ := NewModelInvoker(api)
			_, _, err := invoker.GenerateCandidate(context.Background(), 1, readiness, nil, source, request, nil, nil, config)
			if err == nil || strings.Contains(err.Error(), "secret upstream detail") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestPreflightRequiresExactJSON(t *testing.T) {
	config := validTestConfig()
	api := &fakeChatAPI{output: chatOutput(`{"status":"ready"}`)}
	invoker, _ := NewModelInvoker(api)
	if _, err := invoker.Preflight(context.Background(), config.Models.Implementer); err != nil {
		t.Fatal(err)
	}
	api.output = chatOutput("ready")
	if _, err := invoker.Preflight(context.Background(), config.Models.Implementer); err == nil {
		t.Fatal("Preflight() accepted a non-JSON response")
	}
}

func TestConverseAppliesBoundedInvocationDeadline(t *testing.T) {
	config, request, source := validArtifactFixture(t)
	api := &fakeChatAPI{
		output: chatOutput(`{"files":[{"path":"client/src/components/Example.tsx","content":"export const label = 'Updated label';\n"}],"rationale":"Update the label."}`),
		inspectContext: func(ctx context.Context) error {
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > ModelInvocationTimeout {
				return errors.New("deadline is not bounded")
			}
			return nil
		},
	}
	invoker, _ := NewModelInvoker(api)
	if _, _, err := invoker.GenerateCandidate(context.Background(), 1, testReadyDecision(t, source, request, config), nil, source, request, nil, nil, config); err != nil {
		t.Fatal(err)
	}
}

func TestStrictModelResponseRejectsDuplicateKeys(t *testing.T) {
	if _, err := DecodeModelReviewOutput([]byte(`{"verdict":"pass","verdict":"revise","findings":[]}`)); err == nil {
		t.Fatal("DecodeModelReviewOutput() accepted duplicate keys")
	}
}

// gatewayTestEndpoint points a syntactically valid endpoint at the given test
// server. Transport tests call the client directly, so the https-only rule for
// validated configurations does not apply here.
func gatewayTestEndpoint(serverURL, keyEnv string) ModelEndpoint {
	return ModelEndpoint{
		ID: "transport-test", Vendor: "Vendor A", Model: "vendor/model-a",
		BaseURL: serverURL, APIKeyEnv: keyEnv, MaxOutputTokens: 1024,
	}
}

func TestGatewayClientPostsChatCompletions(t *testing.T) {
	var captured struct {
		path          string
		authorization string
		contentType   string
		body          []byte
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.path = r.URL.Path
		captured.authorization = r.Header.Get("Authorization")
		captured.contentType = r.Header.Get("Content-Type")
		captured.body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"gen-1","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"{}"}}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`))
	}))
	defer server.Close()
	t.Setenv("TEST_MODEL_API_KEY", "test-key-value")

	client, err := NewGatewayClient(server.Client())
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.ChatCompletions(context.Background(), gatewayTestEndpoint(server.URL, "TEST_MODEL_API_KEY"), ChatRequest{
		Model: "vendor/model-a", MaxTokens: 1024,
		Messages: []ChatMessage{{Role: "system", Content: "s"}, {Role: "user", Content: "u"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.ID != "gen-1" || len(response.Choices) != 1 || response.Choices[0].Message.Content != "{}" {
		t.Fatalf("response = %+v", response)
	}
	if captured.path != "/chat/completions" || captured.authorization != "Bearer test-key-value" || captured.contentType != "application/json" {
		t.Fatalf("request = %+v", captured)
	}
	if !strings.Contains(string(captured.body), `"model":"vendor/model-a"`) {
		t.Fatalf("request body = %s", captured.body)
	}
}

func TestGatewayClientFailsClosed(t *testing.T) {
	t.Setenv("TEST_MODEL_API_KEY", "test-key-value")
	tests := map[string]http.HandlerFunc{
		"http error": func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"error":{"message":"secret upstream detail"}}`, http.StatusForbidden)
		},
		"invalid json": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`not json`))
		},
	}
	for name, handler := range tests {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(handler)
			defer server.Close()
			client, err := NewGatewayClient(server.Client())
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.ChatCompletions(context.Background(), gatewayTestEndpoint(server.URL, "TEST_MODEL_API_KEY"), ChatRequest{Model: "vendor/model-a"})
			if err == nil || strings.Contains(err.Error(), "secret upstream detail") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestGatewayClientRequiresAPIKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("request must not be sent without a key")
	}))
	defer server.Close()
	t.Setenv("TEST_MODEL_API_KEY", "")
	client, err := NewGatewayClient(server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ChatCompletions(context.Background(), gatewayTestEndpoint(server.URL, "TEST_MODEL_API_KEY"), ChatRequest{Model: "vendor/model-a"}); err == nil {
		t.Fatal("ChatCompletions() accepted a missing API key")
	}
}

// The transport's own failures carry their cause - a timeout and a refused
// connection have different remedies - while an arbitrary API
// implementation's error text stays contained. A live readiness check died
// as a bare "model invocation failed" that had to be diagnosed from the
// absence of a gateway log row.
func TestConverseLetsTheTransportsOwnCauseTravel(t *testing.T) {
	config, request, source := validArtifactFixture(t)
	invoker, _ := NewModelInvoker(&fakeChatAPI{err: safeModelError("model invocation failed: context deadline exceeded")})
	_, _, err := invoker.AssessReadiness(context.Background(), 1, nil, nil, nil, nil, source, request, config)
	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("the transport's cause was flattened: %v", err)
	}
}

// A wrapper around the mark must not smuggle its own text through: only the
// marked error itself travels.
func TestConverseUnwrapsTheMarkBeforeLettingItTravel(t *testing.T) {
	config, request, source := validArtifactFixture(t)
	wrapped := fmt.Errorf("upstream said %q: %w", "SECRET UPSTREAM DETAIL", safeModelError("model invocation failed"))
	invoker, _ := NewModelInvoker(&fakeChatAPI{err: wrapped})
	_, _, err := invoker.AssessReadiness(context.Background(), 1, nil, nil, nil, nil, source, request, config)
	if err == nil || strings.Contains(err.Error(), "SECRET UPSTREAM DETAIL") {
		t.Fatalf("the wrapper's text travelled: %v", err)
	}
	if !strings.Contains(err.Error(), "model invocation failed") {
		t.Fatalf("the mark itself was lost: %v", err)
	}
}

// sequenceChatAPI answers each call with the next prepared response and
// keeps every request, so a test can see the conversation a retry builds.
type sequenceChatAPI struct {
	outputs  []*ChatResponse
	err      error
	requests []ChatRequest
}

func (s *sequenceChatAPI) ChatCompletions(_ context.Context, _ ModelEndpoint, request ChatRequest) (*ChatResponse, error) {
	s.requests = append(s.requests, request)
	if s.err != nil {
		return nil, s.err
	}
	index := len(s.requests) - 1
	if index >= len(s.outputs) {
		index = len(s.outputs) - 1
	}
	return s.outputs[index], nil
}

// A model that answers in prose is asked again in the same conversation,
// with its own answer and the decoder's objection in front of it. Two live
// readiness assessments died on their first unreadable answer (two live tickets).
func TestConverseJSONAsksAgainWhenTheAnswerIsUnreadable(t *testing.T) {
	config, _, _ := validArtifactFixture(t)
	api := &sequenceChatAPI{outputs: []*ChatResponse{
		chatOutput("Sure! Here is my assessment:\n```json\n{\"status\":\"ready\"}\n```"),
		chatOutput(`{"status":"ready"}`),
	}}
	invoker, _ := NewModelInvoker(api)
	var decoded struct {
		Status string `json:"status"`
	}
	usage, err := invoker.converseJSON(context.Background(), config.Models.Readiness.Assessor, "system", "user", `{"type":"object"}`, 4096, func(answer []byte, _ InvocationUsage) error {
		return decodeStrictJSON(answer, &decoded)
	})
	if err != nil {
		t.Fatalf("the corrected answer was not accepted: %v", err)
	}
	if decoded.Status != "ready" || len(api.requests) != 2 {
		t.Fatalf("decoded = %+v, calls = %d", decoded, len(api.requests))
	}
	retry := api.requests[1].Messages
	if len(retry) != 4 || retry[2].Role != "assistant" || !strings.Contains(retry[2].Content, "Sure!") ||
		retry[3].Role != "user" || !strings.Contains(retry[3].Content, "受け付けられませんでした") || !strings.Contains(retry[3].Content, "invalid character") {
		t.Fatalf("the retry did not carry the answer and the objection: %+v", retry)
	}
	if usage.TotalTokens != 30 || usage.InputTokens != 20 || usage.OutputTokens != 10 {
		t.Fatalf("usage was not summed across attempts: %+v", usage)
	}
}

// Three unreadable answers end the call with an error that says what the
// decoder objected to and how the last answer began — the failure must be
// readable afterwards without the artifact that was never written.
func TestConverseJSONGivesUpAfterThreeUnreadableAnswers(t *testing.T) {
	config, _, _ := validArtifactFixture(t)
	api := &sequenceChatAPI{outputs: []*ChatResponse{chatOutput("I cannot answer in JSON, sorry.\nSecond line.")}}
	invoker, _ := NewModelInvoker(api)
	usage, err := invoker.converseJSON(context.Background(), config.Models.Readiness.Assessor, "system", "user", `{"type":"object"}`, 4096, func(answer []byte, _ InvocationUsage) error {
		return decodeStrictJSON(answer, &struct{}{})
	})
	if err == nil {
		t.Fatal("three unreadable answers were accepted")
	}
	if len(api.requests) != modelAnswerAttempts {
		t.Fatalf("the model was asked %d times, want %d", len(api.requests), modelAnswerAttempts)
	}
	if !strings.Contains(err.Error(), "answer 3 of 3, request request-123, began: I cannot answer in JSON, sorry. Second line.") || !strings.Contains(err.Error(), "invalid character") {
		t.Fatalf("the final error does not carry the objection and the answer head: %v", err)
	}
	if usage.TotalTokens != 45 {
		t.Fatalf("usage was not summed across the attempts: %+v", usage)
	}
}

// A transport failure is not an unreadable answer: the transport owns its
// own retry semantics, and asking again here would double them.
func TestConverseJSONDoesNotRetryATransportFailure(t *testing.T) {
	config, _, _ := validArtifactFixture(t)
	api := &sequenceChatAPI{err: errors.New("connection reset")}
	invoker, _ := NewModelInvoker(api)
	_, err := invoker.converseJSON(context.Background(), config.Models.Readiness.Assessor, "system", "user", `{"type":"object"}`, 4096, func([]byte, InvocationUsage) error { return nil })
	if err == nil || len(api.requests) != 1 {
		t.Fatalf("a transport failure was retried or swallowed: err=%v calls=%d", err, len(api.requests))
	}
}

const testReadinessAnswerJSON = `{"decision":"ready","questions":[],"assumptions":[],"reject_code":""}`

// The readiness assessor goes through the retrying call: a first answer
// with a field the contract does not know is corrected on the second try.
func TestAssessReadinessSurvivesOneUnreadableAnswer(t *testing.T) {
	config, request, source := validArtifactFixture(t)
	valid := chatOutput(testReadinessAnswerJSON)
	broken := chatOutput(strings.Replace(testReadinessAnswerJSON, `"decision"`, `"decision_note":"x","decision"`, 1))
	api := &sequenceChatAPI{outputs: []*ChatResponse{broken, valid}}
	invoker, _ := NewModelInvoker(api)
	if _, _, err := invoker.AssessReadiness(context.Background(), 1, nil, nil, nil, nil, source, request, config); err != nil {
		t.Fatalf("the corrected readiness answer was not accepted: %v", err)
	}
	if len(api.requests) != 2 {
		t.Fatalf("the assessor was asked %d times, want 2", len(api.requests))
	}
}

// A checker that answers "pass" but still lists reasons decodes fine and
// fails the contract's meaning. That objection is now something the checker
// is told and gets to fix (a live ticket died on it one step past the decoder).
func TestCheckReadinessSurvivesOneContractViolation(t *testing.T) {
	config, request, source := validArtifactFixture(t)
	assessment, _ := testAssessmentPair(t, 1, testReadyOutput(), "pass", source, request, config)
	api := &sequenceChatAPI{outputs: []*ChatResponse{
		chatOutput(`{"verdict":"pass","reasons":[{"code":"looks-fine","message":"No objections."}]}`),
		chatOutput(`{"verdict":"pass","reasons":[]}`),
	}}
	invoker, _ := NewModelInvoker(api)
	check, _, err := invoker.CheckReadiness(context.Background(), assessment, nil, nil, source, request, config)
	if err != nil {
		t.Fatalf("the corrected check was not accepted: %v", err)
	}
	if check.Verdict != "pass" || len(api.requests) != 2 {
		t.Fatalf("check = %+v, calls = %d", check, len(api.requests))
	}
	if !strings.Contains(api.requests[1].Messages[3].Content, "reasons do not match verdict") {
		t.Fatalf("the checker was not told what was wrong: %q", api.requests[1].Messages[3].Content)
	}
}

// The re-ask belongs to converseTurn, so the reception's direct calls get it
// too: a preflight whose first answer comes back out of shape is asked again
// once and succeeds on the second request.
func TestPreflightAsksAgainOnceAfterAMalformedResponse(t *testing.T) {
	config := validTestConfig()
	api := &loopScriptAPI{answers: []string{malformedUsageMarker + `{"status":"ready"}`, `{"status":"ready"}`}}
	invoker, _ := NewModelInvoker(api)
	if _, err := invoker.Preflight(context.Background(), config.Models.Implementer); err != nil {
		t.Fatalf("Preflight after one malformed response: %v", err)
	}
	if len(api.requests) != 2 {
		t.Fatalf("requests = %d, want the malformed turn asked again once", len(api.requests))
	}
	api = &loopScriptAPI{answers: []string{malformedUsageMarker + `{"status":"ready"}`, malformedUsageMarker + `{"status":"ready"}`, `{"status":"ready"}`}}
	invoker, _ = NewModelInvoker(api)
	if _, err := invoker.Preflight(context.Background(), config.Models.Implementer); !errors.Is(err, errModelResponseMetadata) || len(api.requests) != 2 {
		t.Fatalf("two malformed responses: err = %v after %d requests, want the metadata error after 2", err, len(api.requests))
	}
}

// A turn the provider ended at the output allowance is asked once more with
// the allowance widened; a second cutoff, or one already at the ceiling,
// travels named after the requests it took.
func TestConverseTurnAsksAgainWithMoreRoomAfterACutOff(t *testing.T) {
	config := validTestConfig()
	api := &loopScriptAPI{answers: []string{lengthMarker + `{"status":"ready"}`, `{"status":"ready"}`}}
	invoker, _ := NewModelInvoker(api)
	if _, err := invoker.Preflight(context.Background(), config.Models.Implementer); err != nil {
		t.Fatalf("Preflight after one cutoff: %v", err)
	}
	if len(api.requests) != 2 || api.requests[0].MaxTokens != 128 || api.requests[1].MaxTokens != 256 {
		t.Fatalf("requests = %d (allowances %d, %d), want the cut-off turn asked again with twice the allowance",
			len(api.requests), api.requests[0].MaxTokens, api.requests[len(api.requests)-1].MaxTokens)
	}

	api = &loopScriptAPI{answers: []string{lengthMarker + `{"status":"ready"}`, lengthMarker + `{"status":"ready"}`, `{"status":"ready"}`}}
	invoker, _ = NewModelInvoker(api)
	_, err := invoker.Preflight(context.Background(), config.Models.Implementer)
	if !errors.Is(err, errModelResponseTruncated) || len(api.requests) != 2 || !strings.Contains(err.Error(), "finish_reason=length") {
		t.Fatalf("two cutoffs: err = %v after %d requests, want the cutoff named after 2", err, len(api.requests))
	}

	api = &loopScriptAPI{answers: []string{lengthMarker + `{}`, `{}`}}
	invoker, _ = NewModelInvoker(api)
	messages := []ChatMessage{{Role: "system", Content: "s"}, {Role: "user", Content: "u"}}
	_, _, err = invoker.converseTurn(context.Background(), ModelEndpoint{Model: "m", MaxOutputTokens: MaxConfiguredOutputTokens}, messages, `{"type":"object"}`, 1<<16)
	if !errors.Is(err, errModelResponseTruncated) || len(api.requests) != 1 || !strings.Contains(err.Error(), "ceiling") {
		t.Fatalf("a cutoff at the ceiling: err = %v after %d requests, want one request and the ceiling named", err, len(api.requests))
	}
	if widenedOutputAllowance(20000) != MaxConfiguredOutputTokens || widenedOutputAllowance(4096) != 8192 {
		t.Fatal("widenedOutputAllowance must double and stop at the ceiling")
	}
}

// A widened re-ask that fails for another reason still names the cutoff
// that caused it, so the caller's log and the requester's note keep the
// cause.
func TestConverseTurnKeepsTheCutoffWhenTheWiderAskFailsOtherwise(t *testing.T) {
	api := &loopScriptAPI{answers: []string{lengthMarker + `{}`}}
	invoker, _ := NewModelInvoker(api)
	messages := []ChatMessage{{Role: "system", Content: "s"}, {Role: "user", Content: "u"}}
	_, _, err := invoker.converseTurn(context.Background(), ModelEndpoint{Model: "m", MaxOutputTokens: 4096}, messages, `{"type":"object"}`, 1<<16)
	if !errors.Is(err, errModelResponseTruncated) || len(api.requests) != 2 || !strings.Contains(err.Error(), "finish_reason=length") || !strings.Contains(err.Error(), "model invocation failed") {
		t.Fatalf("cutoff then a transport failure: err = %v after %d requests, want the cutoff kept", err, len(api.requests))
	}
}

// A gateway answer of 429/502/503/504 is a moment that passes: the call is
// posted again after a pause, and only that many times; every other status
// still fails closed at once, without a second request.
func TestGatewayClientAsksAgainAfterAGatewayTimeout(t *testing.T) {
	t.Setenv("TEST_MODEL_API_KEY", "test-key-value")
	saved := gatewayRetryPauses
	gatewayRetryPauses = []time.Duration{0, 0, 0}
	t.Cleanup(func() { gatewayRetryPauses = saved })
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		if requests <= 2 {
			http.Error(w, "upstream timed out", http.StatusGatewayTimeout)
			return
		}
		_, _ = w.Write([]byte(`{"id":"gen-2","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"{}"}}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`))
	}))
	defer server.Close()
	client, err := NewGatewayClient(server.Client())
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.ChatCompletions(context.Background(), gatewayTestEndpoint(server.URL, "TEST_MODEL_API_KEY"), ChatRequest{Model: "vendor/model-a"})
	if err != nil || response.ID != "gen-2" || requests != 3 {
		t.Fatalf("after two timeouts: response=%v err=%v requests=%d", response, err, requests)
	}

	requests = 0
	always := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		http.Error(w, "busy", http.StatusServiceUnavailable)
	}))
	defer always.Close()
	client, _ = NewGatewayClient(always.Client())
	_, err = client.ChatCompletions(context.Background(), gatewayTestEndpoint(always.URL, "TEST_MODEL_API_KEY"), ChatRequest{Model: "vendor/model-a"})
	if err == nil || !strings.Contains(err.Error(), "status 503 after 4 attempts") || requests != 4 {
		t.Fatalf("always busy: err=%v requests=%d", err, requests)
	}

	requests = 0
	forbidden := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		http.Error(w, "no", http.StatusForbidden)
	}))
	defer forbidden.Close()
	client, _ = NewGatewayClient(forbidden.Client())
	_, err = client.ChatCompletions(context.Background(), gatewayTestEndpoint(forbidden.URL, "TEST_MODEL_API_KEY"), ChatRequest{Model: "vendor/model-a"})
	if err == nil || !strings.Contains(err.Error(), "status 403") || strings.Contains(err.Error(), "attempts") || requests != 1 {
		t.Fatalf("forbidden: err=%v requests=%d", err, requests)
	}
}

// A 429 is asked again only when the gateway names a Retry-After — a rate
// window — and a 429 without one (a limit no wait lifts) fails closed at
// once; a wait is cut short by the caller's deadline.
func TestGatewayClientRetriesA429OnlyWithRetryAfter(t *testing.T) {
	t.Setenv("TEST_MODEL_API_KEY", "test-key-value")
	requests := 0
	windowed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		if requests == 1 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, "rate window", http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"id":"gen-3","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"{}"}}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`))
	}))
	defer windowed.Close()
	client, _ := NewGatewayClient(windowed.Client())
	response, err := client.ChatCompletions(context.Background(), gatewayTestEndpoint(windowed.URL, "TEST_MODEL_API_KEY"), ChatRequest{Model: "vendor/model-a"})
	if err != nil || response.ID != "gen-3" || requests != 2 {
		t.Fatalf("429 with Retry-After: response=%v err=%v requests=%d", response, err, requests)
	}

	requests = 0
	exhausted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		http.Error(w, "balance exhausted", http.StatusTooManyRequests)
	}))
	defer exhausted.Close()
	client, _ = NewGatewayClient(exhausted.Client())
	_, err = client.ChatCompletions(context.Background(), gatewayTestEndpoint(exhausted.URL, "TEST_MODEL_API_KEY"), ChatRequest{Model: "vendor/model-a"})
	if err == nil || !strings.Contains(err.Error(), "429") || !strings.Contains(err.Error(), "no Retry-After") || requests != 1 {
		t.Fatalf("429 without Retry-After: err=%v requests=%d", err, requests)
	}

	saved := gatewayRetryPauses
	gatewayRetryPauses = []time.Duration{10 * time.Second}
	t.Cleanup(func() { gatewayRetryPauses = saved })
	requests = 0
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		http.Error(w, "upstream timed out", http.StatusGatewayTimeout)
	}))
	defer slow.Close()
	client, _ = NewGatewayClient(slow.Client())
	// Cancelled rather than given a short deadline: a call whose remaining
	// allowance cannot fit another attempt no longer enters the wait at all
	// (roomForAnotherAttempt), so a deadline shorter than the pause would
	// now measure that guard instead of this one.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	defer cancel()
	started := time.Now()
	_, err = client.ChatCompletions(ctx, gatewayTestEndpoint(slow.URL, "TEST_MODEL_API_KEY"), ChatRequest{Model: "vendor/model-a"})
	if err == nil || !strings.Contains(err.Error(), "cancelled") || requests != 1 || time.Since(started) > 5*time.Second {
		t.Fatalf("cancelled wait: err=%v requests=%d elapsed=%s", err, requests, time.Since(started))
	}
}

// A call that spends its whole allowance is classified as a turn the
// provider ended with its own error, so the ladder the turn already runs
// asks it again: the bound on one turn does not change, and the re-ask is
// logged with every other one. A live reception died on a spent allowance
// after thirteen runs that did not.
func TestASpentAllowanceJoinsTheProvidersRetryLadder(t *testing.T) {
	var calls int
	invoker := &ModelInvoker{api: &timeoutChatAPI{fail: 1, calls: &calls}}
	_, _, err := invoker.converseTurnOnce(context.Background(), ModelEndpoint{Model: "m"}, []ChatMessage{{Role: "user", Content: "q"}}, "", 1024)
	if err == nil || !errors.Is(err, errModelAllowanceSpent) {
		t.Fatalf("a spent allowance was classified as %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want one: the ladder does the asking", calls)
	}
	// A caller that has given up gets the failure itself, not a class that
	// invites another attempt. Its own counter: sharing the one above would
	// put the stand-in past its failing calls, so it would answer and the
	// check would pass without the guard (measured, review of #121).
	var afterGivingUp int
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err = (&ModelInvoker{api: &timeoutChatAPI{fail: 1, calls: &afterGivingUp}}).converseTurnOnce(cancelled, ModelEndpoint{Model: "m"}, []ChatMessage{{Role: "user", Content: "q"}}, "", 1024)
	if err == nil || errors.Is(err, errModelAllowanceSpent) {
		t.Fatalf("a finished caller was invited to ask again: %v", err)
	}
	if afterGivingUp != 1 {
		t.Fatalf("the stand-in answered instead of running out: calls = %d", afterGivingUp)
	}
}

// A turn whose every call runs out of time asks again once, not for the
// provider's whole ladder: each ask costs ModelInvocationTimeout in real
// time, and the investigating designer's round is 1,800 seconds.
func TestATurnOfSpentAllowancesAsksAgainOnceNotThreeTimes(t *testing.T) {
	quickenTurnPauses(t)
	var calls int
	invoker := &ModelInvoker{api: &timeoutChatAPI{fail: 99, calls: &calls}}
	_, _, err := invoker.converseTurn(context.Background(), ModelEndpoint{Model: "m"},
		[]ChatMessage{{Role: "user", Content: "q"}}, "", 1024)
	if err == nil || !errors.Is(err, errModelAllowanceSpent) {
		t.Fatalf("the turn ended as %v", err)
	}
	// Written out for the same reason as the turn test above: the cost of
	// this number is 5 minutes a call, so it must not drift silently.
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
	// The failure must say what happened. Naming the provider's errors here
	// would send whoever reads it after a provider that never answered.
	if !strings.Contains(err.Error(), "spent its allowance") || strings.Contains(err.Error(), "provider errors") {
		t.Fatalf("the failure does not name the spent allowance: %v", err)
	}
}

// A provider's own error still gets the provider's whole ladder: narrowing
// the allowance must not narrow that too.
func TestAProviderErrorKeepsItsOwnLadder(t *testing.T) {
	quickenTurnPauses(t)
	var calls int
	invoker := &ModelInvoker{api: &upstreamChatAPI{calls: &calls}}
	if _, _, err := invoker.converseTurn(context.Background(), ModelEndpoint{Model: "m"},
		[]ChatMessage{{Role: "user", Content: "q"}}, "", 1024); err == nil {
		t.Fatal("the turn reported an answer")
	}
	if want := len(gatewayRetryPauses) + 1; calls != want {
		t.Fatalf("calls = %d, want %d", calls, want)
	}
}

// A transport that reports its own time limit without the caller's context
// — a dial or handshake that ran out — is a spent allowance too. This is the
// only path that reaches the Timeout() half of spentItsAllowance.
func TestATransportsOwnTimeLimitIsAlsoASpentAllowance(t *testing.T) {
	var calls int
	invoker := &ModelInvoker{api: &dialTimeoutChatAPI{calls: &calls}}
	_, _, err := invoker.converseTurnOnce(context.Background(), ModelEndpoint{Model: "m"},
		[]ChatMessage{{Role: "user", Content: "q"}}, "", 1024)
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("this stand-in must not reach the context half of the check")
	}
	if err == nil || !errors.Is(err, errModelAllowanceSpent) {
		t.Fatalf("a transport time limit was classified as %v", err)
	}
}

// quickenTurnPauses removes the gateway's real waits for one test.
func quickenTurnPauses(t *testing.T) {
	t.Helper()
	restore := gatewayRetryPauses
	quick := make([]time.Duration, len(gatewayRetryPauses))
	gatewayRetryPauses = quick
	t.Cleanup(func() { gatewayRetryPauses = restore })
}

// upstreamChatAPI always answers with the provider's own error inside a 200.
type upstreamChatAPI struct{ calls *int }

func (u *upstreamChatAPI) ChatCompletions(context.Context, ModelEndpoint, ChatRequest) (*ChatResponse, error) {
	*u.calls++
	return &ChatResponse{
		Choices: []ChatChoice{{Message: ChatMessage{Role: "assistant", Content: ""}, FinishReason: ChatFinishError}},
		Usage:   &ChatUsage{PromptTokens: 1, TotalTokens: 1},
	}, nil
}

// dialTimeoutChatAPI reports a time limit the way a transport does when the
// caller's context is untouched: an error that says it timed out, and that
// carries no context.DeadlineExceeded.
type dialTimeoutChatAPI struct{ calls *int }

type dialTimeout struct{}

func (dialTimeout) Error() string { return "dial tcp: i/o timeout" }
func (dialTimeout) Timeout() bool { return true }

func (d *dialTimeoutChatAPI) ChatCompletions(context.Context, ModelEndpoint, ChatRequest) (*ChatResponse, error) {
	*d.calls++
	return nil, safeModelErrorFor("model invocation failed: dial tcp: i/o timeout", dialTimeout{})
}

// The retry has to fire on what the real transport returns, not on what a
// test double invents: the first version of this guard matched a bare
// context error, the transport collapses its cause into a message, and the
// retry never fired in production (review of #121). This test asks a real
// server that never answers, through the real client, and counts requests.
func TestTheAllowanceRetryFiresOnTheRealTransportsError(t *testing.T) {
	t.Setenv("TEST_MODEL_API_KEY", "test-credential")
	var requests int
	never := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		time.Sleep(2 * time.Second)
	}))
	defer never.Close()

	// The client's own timeout is the clock that ends the call here, which
	// is the second of the two clocks production runs.
	client, err := NewGatewayClient(&http.Client{Timeout: 150 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	endpoint := gatewayTestEndpoint(never.URL, "TEST_MODEL_API_KEY")
	_, callErr := client.ChatCompletions(context.Background(), endpoint, ChatRequest{Model: endpoint.Model})
	if callErr == nil {
		t.Fatal("the call answered")
	}
	if !spentItsAllowance(callErr) {
		t.Fatalf("a spent allowance was not recognised: %v (%T)", callErr, callErr)
	}
	if strings.Contains(callErr.Error(), never.URL) {
		t.Errorf("the error carries the address: %v", callErr)
	}

	// And through the turn: the spent allowance has a count of its own, so
	// the ladder asks once more and stops — two calls for one turn. The
	// number is written out rather than derived from allowanceTurnRetries,
	// so raising that constant fails here instead of passing quietly: it is
	// what holds a turn of spent allowances to 10 min 2 s.
	quickenTurnPauses(t)
	requests = 0
	_, _, turnErr := (&ModelInvoker{api: client}).converseTurn(context.Background(), endpoint,
		[]ChatMessage{{Role: "user", Content: "q"}}, "", 1024)
	if !errors.Is(turnErr, errModelAllowanceSpent) {
		t.Fatalf("the turn ended as %v", turnErr)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
}

// timeoutChatAPI reports the invocation allowance as spent for its first
// fail calls, in the shape the real transport returns, then answers.
type timeoutChatAPI struct {
	fail  int
	calls *int
}

func (t *timeoutChatAPI) ChatCompletions(ctx context.Context, endpoint ModelEndpoint, request ChatRequest) (*ChatResponse, error) {
	*t.calls++
	if *t.calls <= t.fail {
		return nil, safeModelErrorFor("model invocation failed: context deadline exceeded", context.DeadlineExceeded)
	}
	return &ChatResponse{
		Choices: []ChatChoice{{Message: ChatMessage{Role: "assistant", Content: "{}"}, FinishReason: "stop"}},
		Usage:   &ChatUsage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
	}, nil
}

// The runner tells a requester why their ticket stopped by reading the
// phrase a failure begins with. A failure whose text no longer begins with
// the phrase the runner keys off leaves the requester with the last-resort
// note instead of the reason, and nothing else would say so.
func TestTheFailuresTheRunnerReadsBeginWithThePhrasesItKeysOff(t *testing.T) {
	for _, pair := range []struct {
		err    error
		phrase string
	}{
		{errModelAllowanceSpent, TransportFailedPhrase},
		{errModelResponseUpstream, ProviderEndedTurnPhrase},
		{errModelResponseMetadata, GatewayBookkeepingPhrase},
		{errModelResponseContent, AnswerUnusablePhrase},
		{errModelResponseRefused, DeclinedOverContentPhrase},
		{errModelResponseTruncated, CutoffPhrase},
	} {
		if !strings.HasPrefix(pair.err.Error(), pair.phrase) {
			t.Errorf("%q does not begin with %q", pair.err.Error(), pair.phrase)
		}
	}
}

// The failures the transport itself reports must all begin with the phrase
// the runner keys off, or a reception that stopped on one of them drops its
// requester to the last-resort note. Ten of these were written out by hand,
// and rewording any of them was free (review of #122). Driven through a
// real server so the check is on what the transport returns, not on a list
// kept in a test.
func TestEveryTransportFailureCarriesThePhraseTheRunnerReads(t *testing.T) {
	t.Setenv("LASSDAS_TEST_KEY", "k")
	quickenTurnPauses(t)
	for _, status := range []int{400, 401, 403, 404, 429, 500, 502} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))
		client, err := NewGatewayClient(&http.Client{Timeout: 2 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		_, callErr := client.ChatCompletions(context.Background(),
			ModelEndpoint{Model: "m", BaseURL: server.URL, APIKeyEnv: "LASSDAS_TEST_KEY"}, ChatRequest{Model: "m"})
		server.Close()
		if callErr == nil {
			t.Fatalf("status %d reported an answer", status)
		}
		if !strings.HasPrefix(callErr.Error(), TransportFailedPhrase) {
			t.Errorf("status %d: %q does not begin with %q", status, callErr.Error(), TransportFailedPhrase)
		}
	}
	// And the one that never reaches a server.
	client, err := NewGatewayClient(&http.Client{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	_, callErr := client.ChatCompletions(context.Background(),
		ModelEndpoint{Model: "m", BaseURL: "http://127.0.0.1:1", APIKeyEnv: "LASSDAS_TEST_KEY"}, ChatRequest{Model: "m"})
	if callErr == nil || !strings.HasPrefix(callErr.Error(), TransportFailedPhrase) {
		t.Errorf("a connection that did not open: %v", callErr)
	}
}

// A finish_reason the engine does not know is not a cutoff, so the turn
// must not pay a second call with the allowance doubled for it. Wrapping
// its failure in errModelResponseTruncated did exactly that (measured,
// review of #122): the phrase is shared with the cutoff, the identity is
// not.
func TestAnUnknownFinishReasonIsNotAskedAgainAsACutoff(t *testing.T) {
	for _, reason := range []string{"tool_calls", "length_exceeded"} {
		var seen []int32
		invoker := &ModelInvoker{api: &finishReasonChatAPI{reason: reason, allowances: &seen}}
		_, _, err := invoker.converseTurn(context.Background(),
			ModelEndpoint{Model: "m", MaxOutputTokens: 4096}, []ChatMessage{{Role: "user", Content: "q"}}, "", 1024)
		if err == nil {
			t.Fatalf("%s reported an answer", reason)
		}
		if len(seen) != 1 {
			t.Errorf("%s: allowances = %v, want one call at the allowance it was given", reason, seen)
		}
	}
}

// finishReasonChatAPI always ends the turn with the given finish_reason and
// records the output allowance each call was made with.
type finishReasonChatAPI struct {
	reason     string
	allowances *[]int32
}

func (f *finishReasonChatAPI) ChatCompletions(_ context.Context, endpoint ModelEndpoint, _ ChatRequest) (*ChatResponse, error) {
	*f.allowances = append(*f.allowances, endpoint.MaxOutputTokens)
	return &ChatResponse{
		Choices: []ChatChoice{{Message: ChatMessage{Role: "assistant", Content: "{}"}, FinishReason: f.reason}},
		Usage:   &ChatUsage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
	}, nil
}

// A moment that passes is asked again whether the gateway answered it with
// a status or could not be reached at all. An investigating round spends up
// to sixty calls, and one of these ended a whole round on its first
// occurrence (audit, 2026-09-09).
func TestAMomentThatPassesIsAskedAgainHoweverItArrived(t *testing.T) {
	t.Setenv("LASSDAS_TEST_KEY", "k")
	quickenTurnPauses(t)
	client, err := NewGatewayClient(&http.Client{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	// A gateway that answers with a status of its own.
	for status, wantCalls := range map[int]int{
		http.StatusInternalServerError: len(gatewayRetryPauses) + 1,
		http.StatusRequestTimeout:      len(gatewayRetryPauses) + 1,
		// Written out: keyed off the constant, moving it to any other status
		// passed (measured, review of #125). 529 is the number a provider
		// over capacity answers with.
		529:                     len(gatewayRetryPauses) + 1,
		http.StatusUnauthorized: 1,
		http.StatusNotFound:     1,
	} {
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls++
			w.WriteHeader(status)
		}))
		if _, callErr := client.ChatCompletions(context.Background(),
			ModelEndpoint{Model: "m", BaseURL: server.URL, APIKeyEnv: "LASSDAS_TEST_KEY"}, ChatRequest{Model: "m"}); callErr == nil {
			t.Fatalf("status %d reported an answer", status)
		}
		server.Close()
		if calls != wantCalls {
			t.Errorf("status %d: calls = %d, want %d", status, calls, wantCalls)
		}
	}
	// A gateway that answers, then stops answering: the second call is what
	// the retry is for, and the answer arrives.
	answered := 0
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		answered++
		if answered == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"{}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer flaky.Close()
	if _, callErr := client.ChatCompletions(context.Background(),
		ModelEndpoint{Model: "m", BaseURL: flaky.URL, APIKeyEnv: "LASSDAS_TEST_KEY"}, ChatRequest{Model: "m"}); callErr != nil {
		t.Fatalf("a gateway that answered on its second call: %v", callErr)
	}
}

// A call that spent its own allowance is asked again by the turn, on its own
// count. Asking again here as well would multiply five-minute calls, so this
// one failure is left for converseTurn.
func TestASpentAllowanceIsNotAskedAgainByTheTransport(t *testing.T) {
	t.Setenv("LASSDAS_TEST_KEY", "k")
	quickenTurnPauses(t)
	calls := 0
	silent := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls++
		time.Sleep(2 * time.Second)
	}))
	defer silent.Close()
	client, err := NewGatewayClient(&http.Client{Timeout: 150 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, callErr := client.ChatCompletions(context.Background(),
		ModelEndpoint{Model: "m", BaseURL: silent.URL, APIKeyEnv: "LASSDAS_TEST_KEY"}, ChatRequest{Model: "m"}); callErr == nil {
		t.Fatal("a silent gateway reported an answer")
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want one: the turn does the asking", calls)
	}
}

// A connection the network drops never becomes a status, so it reached the
// caller without ever being asked again. Counted at the listener: every
// connection is accepted and closed without an answer.
func TestADroppedConnectionIsAskedAgain(t *testing.T) {
	t.Setenv("LASSDAS_TEST_KEY", "k")
	quickenTurnPauses(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan struct{}, 16)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			accepted <- struct{}{}
			_ = conn.Close()
		}
	}()
	client, err := NewGatewayClient(&http.Client{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	_, callErr := client.ChatCompletions(context.Background(),
		ModelEndpoint{Model: "m", BaseURL: "http://" + listener.Addr().String(), APIKeyEnv: "LASSDAS_TEST_KEY"}, ChatRequest{Model: "m"})
	if callErr == nil {
		t.Fatal("a dropped connection reported an answer")
	}
	if !strings.HasPrefix(callErr.Error(), TransportFailedPhrase) {
		t.Fatalf("the failure lost the phrase the runner reads: %v", callErr)
	}
	close(accepted)
	calls := 0
	for range accepted {
		calls++
	}
	if want := len(gatewayRetryPauses) + 1; calls != want {
		t.Fatalf("connections = %d, want %d", calls, want)
	}
}

// After four attempts and forty seconds, the requester must not be told that
// nothing was asked again. converseTurnOnce keeps only the innermost safe
// error, so a wrapper's words never reach them (measured, review of #125).
func TestTheFailureSaysHowManyAttemptsItTook(t *testing.T) {
	t.Setenv("LASSDAS_TEST_KEY", "k")
	quickenTurnPauses(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	client, err := NewGatewayClient(&http.Client{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	invoker := &ModelInvoker{api: client}
	_, _, turnErr := invoker.converseTurnOnce(context.Background(),
		ModelEndpoint{Model: "m", BaseURL: "http://" + listener.Addr().String(), APIKeyEnv: "LASSDAS_TEST_KEY"},
		[]ChatMessage{{Role: "user", Content: "q"}}, "", 1024)
	if turnErr == nil {
		t.Fatal("a dropped connection reported an answer")
	}
	if !strings.HasPrefix(turnErr.Error(), TransportFailedPhrase) || !strings.Contains(turnErr.Error(), AttemptsExhaustedPhrase) {
		t.Fatalf("the turn's failure does not say what happened: %q", turnErr.Error())
	}
}

// A status that is slow to arrive must not spend the call's whole allowance
// between attempts: the call would reach the turn as a spent allowance, and
// the turn asks that again, so one slow status becomes two allowances
// instead of one (measured, review of #125).
func TestASlowStatusDoesNotSpendTheWholeAllowance(t *testing.T) {
	t.Setenv("LASSDAS_TEST_KEY", "k")
	quickenTurnPauses(t)
	calls := 0
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		time.Sleep(120 * time.Millisecond)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer slow.Close()
	client, err := NewGatewayClient(&http.Client{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	_, callErr := client.ChatCompletions(ctx,
		ModelEndpoint{Model: "m", BaseURL: slow.URL, APIKeyEnv: "LASSDAS_TEST_KEY"}, ChatRequest{Model: "m"})
	if callErr == nil {
		t.Fatal("a slow gateway reported an answer")
	}
	// It stops while the allowance still has room to report what happened,
	// rather than running the allowance out and losing the status.
	if !strings.Contains(callErr.Error(), "500") {
		t.Fatalf("the failure lost the status: %q", callErr.Error())
	}
	if ctx.Err() != nil {
		t.Fatalf("the call spent the whole allowance: %d attempts", calls)
	}
}

// The wait between attempts ends when the caller gives up, rather than
// running to its end. Nothing measured it (review of #125).
func TestTheWaitBetweenAttemptsEndsWhenTheCallerGivesUp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	if pauseBeforeAskingAgain(ctx, 2*time.Second) {
		t.Fatal("the wait reported that it finished")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("the wait ran on for %s after the caller gave up", elapsed)
	}
	if !pauseBeforeAskingAgain(context.Background(), time.Millisecond) {
		t.Fatal("a wait that finished reported that it did not")
	}
}
