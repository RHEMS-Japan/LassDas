package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	if !errors.Is(err, errModelResponseTruncated) || len(api.requests) != 2 || !strings.Contains(err.Error(), "finish_reason=length") || !strings.Contains(err.Error(), "script exhausted") && !strings.Contains(err.Error(), "model invocation failed") {
		t.Fatalf("cutoff then a transport failure: err = %v after %d requests, want the cutoff kept", err, len(api.requests))
	}
}
