package worker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// maxStreamEventBytes bounds one server-sent event. A gateway that sends a
// longer line is answering something other than a chat completion.
const maxStreamEventBytes = 1 << 20

// postStreaming makes the same call post makes, asks for the answer in
// pieces, and hands back the one response those pieces add up to. The rest
// of the transport is unchanged: the caller cannot tell how the answer
// travelled, which is the point - streaming exists to show the work, never
// to change what a turn decides.
//
// A non-200 answer is not a stream: it is read and returned as post would,
// so the retry ladder above judges it exactly as before.
func (g *GatewayClient) postStreaming(ctx context.Context, baseURL, apiKey string, encoded []byte) ([]byte, int, *time.Duration, error) {
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/chat/completions", bytes.NewReader(encoded))
	if err != nil {
		return nil, 0, nil, safeModelLiteral("model request could not be built")
	}
	httpRequest.Header.Set("Authorization", "Bearer "+apiKey)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "text/event-stream")
	httpResponse, err := g.client.Do(httpRequest)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return nil, 0, nil, safeModelErrorFor(TransportFailedPhrase+": "+urlErr.Err.Error(), urlErr.Err)
		}
		return nil, 0, nil, safeModelLiteral(TransportFailedPhrase)
	}
	defer func() { _ = httpResponse.Body.Close() }()
	var retryAfter *time.Duration
	if seconds, err := strconv.Atoi(strings.TrimSpace(httpResponse.Header.Get("Retry-After"))); err == nil && seconds >= 0 {
		wait := time.Duration(seconds) * time.Second
		retryAfter = &wait
	}
	if httpResponse.StatusCode != http.StatusOK {
		body, err := io.ReadAll(io.LimitReader(httpResponse.Body, maxTransportResponseBytes+1))
		if err != nil || len(body) > maxTransportResponseBytes {
			return nil, 0, nil, safeModelLiteral("model response could not be read")
		}
		return body, httpResponse.StatusCode, retryAfter, nil
	}
	assembled, err := readChatStream(io.LimitReader(httpResponse.Body, maxTransportResponseBytes+1), g.live)
	if err != nil {
		return nil, 0, nil, err
	}
	body, err := json.Marshal(assembled)
	if err != nil {
		return nil, 0, nil, safeModelLiteral("model response could not be assembled")
	}
	return body, http.StatusOK, retryAfter, nil
}

// streamChunk is one piece of a streamed answer.
type streamChunk struct {
	ID      string             `json:"id"`
	Error   *ChatResponseError `json:"error,omitempty"`
	Usage   *ChatUsage         `json:"usage"`
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Delta        struct {
			Role      string `json:"role"`
			Content   string `json:"content"`
			Reasoning string `json:"reasoning"`
		} `json:"delta"`
	} `json:"choices"`
}

// liveWriter is what the assembled answer is shown on as it arrives.
type liveWriter interface{ Write([]byte) (int, error) }

// readChatStream turns the pieces into the response the caller expects and
// shows the answer's own text on the live view as each piece lands. The
// model's reasoning is not shown: it is not the answer, and a reader
// watching a contract being written should see the contract.
func readChatStream(body io.Reader, live liveWriter) (*ChatResponse, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), maxStreamEventBytes)
	var answer strings.Builder
	response := &ChatResponse{Choices: []ChatChoice{{Message: ChatMessage{Role: "assistant"}}}}
	pieces := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk streamChunk
		if json.Unmarshal([]byte(data), &chunk) != nil {
			return nil, safeModelLiteral("model response is not valid JSON")
		}
		pieces++
		if chunk.ID != "" {
			response.ID = chunk.ID
		}
		if chunk.Error != nil {
			response.Error = chunk.Error
		}
		if chunk.Usage != nil {
			response.Usage = chunk.Usage
		}
		for _, choice := range chunk.Choices {
			if choice.FinishReason != "" {
				response.Choices[0].FinishReason = choice.FinishReason
			}
			if choice.Delta.Content == "" {
				continue
			}
			answer.WriteString(choice.Delta.Content)
			if live != nil {
				_, _ = live.Write([]byte(choice.Delta.Content))
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, safeModelLiteral("model response could not be read")
	}
	if pieces == 0 {
		return nil, safeModelLiteral("model response carried no pieces")
	}
	if live != nil {
		_, _ = live.Write([]byte("\n"))
	}
	response.Choices[0].Message.Content = answer.String()
	return response, nil
}
