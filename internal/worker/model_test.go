package worker

import (
	"context"
	"errors"
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
