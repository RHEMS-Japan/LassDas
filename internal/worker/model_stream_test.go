package worker

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/livelog"
)

// The pieces add up to the one answer the rest of the engine expects, and
// the answer's own text appears on the live view as each piece lands. The
// model's reasoning is not the answer and is not shown.
func TestReadChatStreamAssemblesTheAnswerAndShowsIt(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"gen-1","choices":[{"delta":{"role":"assistant","content":"{\"repo"}}]}`,
		`data: {"id":"gen-1","choices":[{"delta":{"reasoning":"考えています"}}]}`,
		`data: {"id":"gen-1","choices":[{"delta":{"content":"sitory\":\"x\"}"},"finish_reason":"stop"}]}`,
		`data: {"id":"gen-1","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		"data: [DONE]",
		"",
	}, "\n\n")
	var shown strings.Builder
	response, err := readChatStream(strings.NewReader(stream), &shown)
	if err != nil {
		t.Fatal(err)
	}
	if response.ID != "gen-1" || len(response.Choices) != 1 {
		t.Fatalf("response = %+v", response)
	}
	if response.Choices[0].Message.Content != `{"repository":"x"}` {
		t.Fatalf("content = %q", response.Choices[0].Message.Content)
	}
	if response.Choices[0].FinishReason != ChatFinishStop {
		t.Fatalf("finish_reason = %q", response.Choices[0].FinishReason)
	}
	if response.Usage == nil || response.Usage.TotalTokens != 15 {
		t.Fatalf("usage = %+v", response.Usage)
	}
	if got := shown.String(); !strings.HasPrefix(got, `{"repository":"x"}`) {
		t.Fatalf("the live view showed %q", got)
	}
	if strings.Contains(shown.String(), "考えています") {
		t.Fatalf("the reasoning was shown: %q", shown.String())
	}
}

func TestReadChatStreamRefusesWhatIsNotAStream(t *testing.T) {
	if _, err := readChatStream(strings.NewReader(`{"id":"gen-1","choices":[]}`), nil); err == nil {
		t.Fatal("a plain JSON body was accepted as a stream")
	}
	if _, err := readChatStream(strings.NewReader("data: {not json}\n\n"), nil); err == nil {
		t.Fatal("a malformed piece was accepted")
	}
	response, err := readChatStream(strings.NewReader("data: {\"id\":\"gen-2\",\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\ndata: [DONE]\n\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.Usage != nil {
		t.Fatal("usage was invented for a stream that carried none")
	}
}

// With somewhere to show the answer, the call asks for it in pieces; the
// caller still receives one assembled response. An endpoint that streams
// without accounting is asked again in one piece, because a turn cannot
// work without the usage - and the fallback is remembered.
func TestGatewayStreamsWhenThereIsALiveViewAndFallsBackWithoutUsage(t *testing.T) {
	var bodies []string
	usage := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))
		if !strings.Contains(string(raw), `"stream":true`) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"gen-plain","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"plain"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"gen-stream\",\"choices\":[{\"delta\":{\"content\":\"streamed\"},\"finish_reason\":\"stop\"}]}\n\n")
		if usage {
			_, _ = io.WriteString(w, "data: {\"id\":\"gen-stream\",\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":4,\"total_tokens\":7}}\n\n")
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "live", "read-contract.log")
	t.Setenv(livelog.PathEnv, path)
	t.Setenv("FIXTURE_MODEL_KEY", "secret-key")
	client, err := NewGatewayClient(server.Client())
	if err != nil {
		t.Fatal(err)
	}
	endpoint := ModelEndpoint{ID: "assessor", Vendor: "OpenAI", Model: "vendor/model", BaseURL: server.URL, APIKeyEnv: "FIXTURE_MODEL_KEY", MaxOutputTokens: 1024}
	request := ChatRequest{Model: endpoint.Model, MaxTokens: endpoint.MaxOutputTokens, Messages: []ChatMessage{{Role: "user", Content: "hi"}}}

	response, err := client.ChatCompletions(context.Background(), endpoint, request)
	if err != nil {
		t.Fatal(err)
	}
	if response.Choices[0].Message.Content != "streamed" || response.Usage == nil || response.Usage.TotalTokens != 7 {
		t.Fatalf("streamed answer was not assembled: %+v", response)
	}
	if len(bodies) != 1 || !strings.Contains(bodies[0], `"include_usage":true`) {
		t.Fatalf("the call did not ask for a stream with accounting: %v", bodies)
	}
	if raw, err := os.ReadFile(path); err != nil || !strings.Contains(string(raw), "streamed") {
		t.Fatalf("the answer was not shown live: %v %q", err, raw)
	}

	usage = false
	bodies = nil
	fallbackClient, err := NewGatewayClient(server.Client())
	if err != nil {
		t.Fatal(err)
	}
	response, err = fallbackClient.ChatCompletions(context.Background(), endpoint, request)
	if err != nil {
		t.Fatal(err)
	}
	if response.Choices[0].Message.Content != "plain" {
		t.Fatalf("the call was not asked again in one piece: %+v", response)
	}
	if len(bodies) != 2 || strings.Contains(bodies[1], `"stream":true`) {
		t.Fatalf("the fallback did not drop streaming: %v", bodies)
	}
	if !fallbackClient.streamOff {
		t.Fatal("the endpoint's inability to stream was not remembered")
	}
}
