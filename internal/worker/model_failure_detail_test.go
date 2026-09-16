package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	// The configured effort and allowance, and what the last call used
	// after the turn lowered and widened them.
	if detail.Effort != "high" || detail.FinalEffort != "low" || detail.MaxOutputTokens != MaxConfiguredOutputTokens || detail.FinalMaxOutputTokens != MaxConfiguredOutputTokens {
		t.Fatalf("configured vs final: %+v", detail)
	}
	if !strings.Contains(detail.Phrase, ReasoningExhaustedPhrase) || !strings.Contains(detail.Phrase, EffortLoweredPhrase) {
		t.Fatalf("the phrase should name the class and what the turn did: %q", detail.Phrase)
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
		"bad word":        `{"phrase":"p","calls":1,"model":"a b"}`,
		"not json":        `phrase`,
		"absent":          ``,
	} {
		if _, ok := ParseFailureDetailLine(FailureDetailLinePrefix + line); ok {
			t.Errorf("%s: must be refused", name)
		}
	}
}

// A transport failure's message quotes what the wire carried (Go's HTTP
// client quotes a malformed status line); none of it may reach the record.
// The phrase is composed from the turn's own words.
func TestTheDetailPhraseCarriesNoUpstreamText(t *testing.T) {
	buffer := captureFailureDetail(t)
	wire := `sk-live-SECRETVALUE0123456789abcdef`
	api := &fakeChatAPI{err: safeModelErrorFor(TransportFailedPhrase+`: net/http: HTTP/1.x transport connection broken: malformed HTTP response "`+wire+`" after 4`+AttemptsExhaustedPhrase, errors.New("broken"))}
	invoker, _ := NewModelInvoker(api)
	_, _, err := invoker.converseTurn(context.Background(), ModelEndpoint{Model: "m", MaxOutputTokens: 4096}, []ChatMessage{{Role: "user", Content: "u"}}, "", 1024)
	if err == nil {
		t.Fatal("the turn should have failed")
	}
	if strings.Contains(buffer.String(), "SECRETVALUE") || strings.Contains(buffer.String(), "malformed") {
		t.Fatalf("upstream text reached the line: %q", buffer.String())
	}
	detail, ok := ParseFailureDetailLine(buffer.String())
	if !ok || !strings.HasPrefix(detail.Phrase, TransportFailedPhrase) || !strings.Contains(detail.Phrase, "attempts ran out") {
		t.Fatalf("detail = %+v (ok=%v)", detail, ok)
	}
	for _, err := range []error{
		fmt.Errorf("%w: finish_reason=%s (output allowance 100 tokens); %w", errModelResponseTruncated, ChatFinishLength, errModelReasoningExhausted),
		errors.New(CutoffPhrase + `: finish_reason=weird "quoted" value`),
		fmt.Errorf("%w (finish_reason=error)", errModelResponseUpstream),
		safeModelStatusError(TransportFailedPhrase+" with status 429 and no Retry-After ("+LimitNotLiftedPhrase+")", 429),
	} {
		phrase := detailPhrase(err)
		if strings.Contains(phrase, "quoted") || strings.Contains(phrase, "weird") || !failureDetailPhrasePattern.MatchString(phrase) {
			t.Errorf("phrase for %v = %q", err, phrase)
		}
	}
	if got := detailPhrase(safeModelStatusError("x", 429)); !strings.Contains(got, "with status 429") {
		t.Errorf("status phrase = %q", got)
	}
}

// One field outside its shape costs that field, not the record: a Vertex
// model name is kept (the pattern admits it), a finish reason with a space
// is blanked, and the line is still written.
func TestOneOddFieldDoesNotCostTheWholeDetail(t *testing.T) {
	buffer := captureFailureDetail(t)
	odd := &ChatResponse{ID: "gen/with/slashes", Choices: []ChatChoice{{FinishReason: "stop early", Message: ChatMessage{Role: "assistant", Content: "x"}}},
		Usage: &ChatUsage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}}
	api := &fakeChatAPI{output: odd}
	invoker, _ := NewModelInvoker(api)
	_, _, err := invoker.converseTurn(context.Background(), ModelEndpoint{Model: "claude-3-5-sonnet@20240620", MaxOutputTokens: 4096}, []ChatMessage{{Role: "user", Content: "u"}}, "", 1024)
	if err == nil {
		t.Fatal("the turn should have failed")
	}
	detail, ok := ParseFailureDetailLine(buffer.String())
	if !ok || detail.Model != "claude-3-5-sonnet@20240620" || detail.LastFinishReason != "" || detail.LastRequestID != "gen/with/slashes" {
		t.Fatalf("detail = %+v (ok=%v) line=%q", detail, ok, buffer.String())
	}
}

// A turn cut off, asked again with a wider allowance and then refused for
// a reason of its own carries both in its phrase: the cutoff and the
// refusal's class and status - what the runner's own line says.
func TestTheDetailPhraseKeepsEveryClassOfARetriedTurn(t *testing.T) {
	cases := map[string]struct {
		err  error
		want []string
	}{
		"cutoff then 429": {afterCutoff(fmt.Errorf("%w: finish_reason=length", errModelResponseTruncated), "widened",
			safeModelStatusError(TransportFailedPhrase+" with status 429 and no Retry-After ("+LimitNotLiftedPhrase+")", 429)),
			[]string{"finish_reason=length", "with status 429", LimitNotLiftedPhrase}},
		"cutoff then provider errors": {afterCutoff(fmt.Errorf("%w: finish_reason=length", errModelResponseTruncated), "widened",
			fmt.Errorf("%w after 4 provider errors", errModelResponseUpstream)),
			[]string{"finish_reason=length", ProviderEndedTurnPhrase}},
		"cutoff then no usage": {afterCutoff(fmt.Errorf("%w: finish_reason=length", errModelResponseTruncated), "widened",
			fmt.Errorf("%w (no usage)", errModelResponseMetadata)),
			[]string{"finish_reason=length", GatewayBookkeepingPhrase}},
		"cutoff then spent allowance": {afterCutoff(fmt.Errorf("%w: finish_reason=length", errModelResponseTruncated), "widened",
			fmt.Errorf("%w after 2 such calls", errModelAllowanceSpent)),
			[]string{"finish_reason=length", SpentAllowancePhrase}},
		"literal setting failure":    {safeModelLiteral("model API key is unavailable"), []string{"model API key is unavailable"}},
		"quoted wire is not literal": {safeModelErrorFor(TransportFailedPhrase+`: malformed HTTP response "sk-live-SECRET"`, errors.New("x")), []string{TransportFailedPhrase}},
		"the round's wall":           {fmt.Errorf(TransportFailedPhrase+": %w", context.DeadlineExceeded), []string{TransportFailedPhrase, "wall"}},
	}
	for name, test := range cases {
		phrase := detailPhrase(test.err)
		for _, want := range test.want {
			if !strings.Contains(phrase, want) {
				t.Errorf("%s: %q lacks %q", name, phrase, want)
			}
		}
		if strings.Contains(phrase, "SECRET") || !failureDetailPhrasePattern.MatchString(phrase) {
			t.Errorf("%s: phrase %q is not safe", name, phrase)
		}
	}
}
