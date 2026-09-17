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

// maxStreamChoices bounds how many candidates one streamed answer may carry.
const maxStreamChoices = 7

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

// errStreamUnsupported marks an answer that did not arrive as a stream at
// all: a gateway that ignored the request, or one whose pieces cannot be
// read. The caller asks the same question again in one piece and stops
// asking for streams - without that, a gateway that ignores streaming
// failed every model call of every run, because the live view asks for one
// on every step (review of #187).
var errStreamUnsupported = errors.New("model endpoint did not stream")

// streamChunk is one piece of a streamed answer.
type streamChunk struct {
	ID      string             `json:"id"`
	Error   *ChatResponseError `json:"error,omitempty"`
	Usage   *ChatUsage         `json:"usage"`
	Choices []struct {
		Index        int    `json:"index"`
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
	// One answer per choice index: a turn that asks for one gets one, and a
	// provider that sends several must not have them read as one answer
	// with somebody else's finish reason (review of #187).
	answers := map[int]*strings.Builder{}
	finishes := map[int]string{}
	response := &ChatResponse{}
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
			return nil, errStreamUnsupported
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
			// The index comes from the wire. A turn asks for one answer and
			// no provider returns eight; anything else would size an
			// allocation from a number a stranger chose (review of #187).
			if choice.Index < 0 || choice.Index > maxStreamChoices {
				continue
			}
			if choice.FinishReason != "" {
				finishes[choice.Index] = choice.FinishReason
			}
			if choice.Delta.Content == "" {
				continue
			}
			if answers[choice.Index] == nil {
				answers[choice.Index] = &strings.Builder{}
			}
			answers[choice.Index].WriteString(choice.Delta.Content)
			// Only the answer a single-choice turn is reading is shown: a
			// second candidate on the same screen would read as one text.
			if live != nil && choice.Index == 0 {
				_, _ = live.Write([]byte(choice.Delta.Content))
			}
		}
	}
	if err := scanner.Err(); err != nil {
		// A line longer than the bound is not a chat completion; so is a
		// body that could not be read as a stream.
		return nil, errStreamUnsupported
	}
	if pieces == 0 {
		return nil, errStreamUnsupported
	}
	if live != nil && answers[0] != nil {
		_, _ = live.Write([]byte("\n"))
	}
	highest := -1
	for index := range answers {
		if index > highest {
			highest = index
		}
	}
	for index := range finishes {
		if index > highest {
			highest = index
		}
	}
	for index := 0; index <= highest; index++ {
		content := ""
		if builder := answers[index]; builder != nil {
			content = builder.String()
		}
		response.Choices = append(response.Choices, ChatChoice{
			FinishReason: finishes[index],
			Message:      ChatMessage{Role: "assistant", Content: content},
		})
	}
	// A provider's own error with nothing else is exactly the shape the
	// caller judges as the provider ending the turn; an invented empty
	// answer would be judged as a malformed one instead.
	return response, nil
}
