package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func captureFailureDetail(t *testing.T) *bytes.Buffer {
	t.Helper()
	failureDetailSinkMu.Lock()
	saved := failureDetailSink
	buffer := &bytes.Buffer{}
	failureDetailSink = buffer
	failureDetailSinkMu.Unlock()
	t.Cleanup(func() {
		failureDetailSinkMu.Lock()
		failureDetailSink = saved
		failureDetailSinkMu.Unlock()
	})
	return buffer
}

// A turn that gives up leaves one line with what it knew: the phrase that
// ends it, how many calls it made, and the last answer the gateway gave —
// for a reception that reasoned itself to the wall, the finish reason and
// the reasoning tokens that explain it.
func TestATurnThatGivesUpLeavesItsDetailOnOneLine(t *testing.T) {
	buffer := captureFailureDetail(t)
	cut := &ChatResponse{
		ID:      "gen-abc123",
		Choices: []ChatChoice{{FinishReason: ChatFinishLength, Message: ChatMessage{Role: "assistant", Content: ""}}},
		Usage:   &ChatUsage{PromptTokens: 900, CompletionTokens: 32768, TotalTokens: 33668, CompletionTokensDetails: &ChatCompletionTokensDetails{ReasoningTokens: 32768}},
	}
	api := &fakeChatAPI{output: cut}
	invoker, _ := NewModelInvoker(api)
	messages := []ChatMessage{{Role: "system", Content: "s"}, {Role: "user", Content: "u"}}
	_, _, err := invoker.converseTurn(context.Background(), ModelEndpoint{Model: "vendor/model-a", Effort: "high", MaxOutputTokens: MaxConfiguredOutputTokens}, messages, `{"type":"object"}`, 1<<16)
	if err == nil {
		t.Fatal("the turn should have failed")
	}
	lines := strings.Split(strings.TrimSpace(buffer.String()), "\n")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], FailureDetailLinePrefix) {
		t.Fatalf("want exactly one detail line, got %q", buffer.String())
	}
	detail, ok := ParseFailureDetailLine(buffer.String())
	if !ok {
		t.Fatalf("the line must read back: %q", buffer.String())
	}
	if detail.Model != "vendor/model-a" || detail.Calls < 1 || detail.LastFinishReason != ChatFinishLength || detail.LastReasoningTokens != 32768 ||
		detail.LastCompletionTokens != 32768 || detail.LastRequestID != "gen-abc123" || !strings.Contains(detail.Phrase, "finish_reason=length") || detail.Lowered != 2 {
		t.Fatalf("detail = %+v", detail)
	}
	if strings.Contains(buffer.String(), "TEST_MODEL_API_KEY") || strings.Contains(buffer.String(), "Bearer") {
		t.Fatalf("no credential may reach the line: %q", buffer.String())
	}
}

// A gateway status the transport does not ask again for reaches the
// detail as a number, so the page can say "429" without any upstream text.
func TestAGatewayStatusReachesTheDetailAsANumber(t *testing.T) {
	buffer := captureFailureDetail(t)
	api := &fakeChatAPI{err: safeModelStatusError(TransportFailedPhrase+" with status 429 and no Retry-After ("+LimitNotLiftedPhrase+")", http.StatusTooManyRequests)}
	invoker, _ := NewModelInvoker(api)
	_, _, err := invoker.converseTurn(context.Background(), ModelEndpoint{Model: "m", MaxOutputTokens: 4096}, []ChatMessage{{Role: "user", Content: "u"}}, "", 1024)
	if err == nil {
		t.Fatal("the turn should have failed")
	}
	detail, ok := ParseFailureDetailLine(buffer.String())
	if !ok || detail.LastHTTPStatus != 429 || detail.Calls != 1 || !strings.Contains(detail.Phrase, LimitNotLiftedPhrase) {
		t.Fatalf("detail = %+v (ok=%v)", detail, ok)
	}
	var safe *SafeModelError
	if !errors.As(err, &safe) || safe.Status() != 429 {
		t.Fatalf("the status must travel on the error: %v", err)
	}
}

// A successful turn leaves no line at all.
func TestASuccessfulTurnLeavesNoDetailLine(t *testing.T) {
	buffer := captureFailureDetail(t)
	api := &fakeChatAPI{output: chatOutput(`{}`)}
	invoker, _ := NewModelInvoker(api)
	if _, _, err := invoker.converseTurn(context.Background(), ModelEndpoint{Model: "m", MaxOutputTokens: 4096}, []ChatMessage{{Role: "user", Content: "u"}}, "", 1024); err != nil {
		t.Fatal(err)
	}
	if buffer.Len() != 0 {
		t.Fatalf("no detail on success, got %q", buffer.String())
	}
}

// The runner reads the line back strictly: the last line wins, unknown
// fields and shapes outside the pattern are refused whole, and a line
// something else wrote in the worker's name cannot smuggle text.
func TestParseFailureDetailLineIsStrict(t *testing.T) {
	good, _ := json.Marshal(ModelFailureDetail{Phrase: "model invocation failed with status 503", Model: "m", Calls: 2, LastHTTPStatus: 503})
	stderr := "worker: something\n" + FailureDetailLinePrefix + `{"phrase":"first","calls":1}` + "\n" + FailureDetailLinePrefix + string(good) + "\nworker: readiness assessment failed: x\n"
	detail, ok := ParseFailureDetailLine(stderr)
	if !ok || detail.Phrase != "model invocation failed with status 503" || detail.Calls != 2 {
		t.Fatalf("the last line must win: %+v %v", detail, ok)
	}
	for name, line := range map[string]string{
		"unknown field":   `{"phrase":"p","calls":1,"api_key":"sk-x"}`,
		"no calls":        `{"phrase":"p"}`,
		"phrase too long": `{"phrase":"` + strings.Repeat("a", 601) + `","calls":1}`,
		"non-ascii":       `{"phrase":"鍵 sk-live","calls":1}`,
		"bad request id":  `{"phrase":"p","calls":1,"last_request_id":"has space"}`,
		"not json":        `phrase`,
		"absent":          ``,
	} {
		if _, ok := ParseFailureDetailLine(FailureDetailLinePrefix + line); ok {
			t.Errorf("%s: must be refused", name)
		}
	}
}
