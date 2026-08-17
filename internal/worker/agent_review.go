package worker

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// AgentReviewFromRun turns a reviewing agent's run into the sealed Review the
// rest of the chain already checks. The reviewer works in the repository rather
// than on the diff alone, so it can read whatever it needs to judge the change
// in context; what it reports is still held to the same shape as any review.
func AgentReviewFromRun(
	endpoint ModelEndpoint,
	run AgentRun,
	candidate Candidate,
	source SourceSnapshot,
	request TicketRequest,
	config Config,
	reviewedAt time.Time,
) (Review, error) {
	output, err := DecodeAgentReviewOutput(run.Transcript)
	if err != nil {
		return Review{}, err
	}
	transcriptBytes := int32(len(run.Transcript)) // #nosec G115 -- bounded by MaxAgentTranscriptBytes.
	if transcriptBytes < 1 {
		transcriptBytes = 1
	}
	promptBytes := int32(run.PromptBytes) // #nosec G115 -- bounded by MaxAgentPromptBytes.
	// Measured bytes, not tokens: an agent run has no single token meter.
	invocation := InvocationUsage{
		RequestedModel: endpoint.Model, RequestID: run.RunSHA256, StopReason: ChatFinishStop,
		InputTokens: promptBytes, OutputTokens: transcriptBytes, TotalTokens: promptBytes + transcriptBytes,
		LatencyMillis: run.DurationMs,
	}
	return NewReview(candidate.Stage, endpoint, output, candidate, source, request, config, invocation, reviewedAt)
}

// ConfirmTreeMatchesCandidate checks that the working copy still holds exactly
// the change that was submitted for review. The reviewer is told to read only;
// this is what makes that instruction binding, so a reviewer cannot quietly
// fix what it was asked to judge.
func ConfirmTreeMatchesCandidate(root string, candidate Candidate, consumer ConsumerConfig) error {
	changed, err := ChangedFilesUnder(root, consumer.Mode.AllowedFilePrefixes)
	if err != nil {
		return errors.New("the tree could not be read after review")
	}
	if len(changed) != len(candidate.Files) {
		return errors.New("the reviewing agent changed the tree")
	}
	byPath := make(map[string]string, len(candidate.Files))
	for _, file := range candidate.Files {
		byPath[file.Path] = file.Content
	}
	for _, path := range changed {
		expected, submitted := byPath[path]
		if !submitted {
			return errors.New("the reviewing agent changed the tree")
		}
		filename, err := regularFileWithin(root, path)
		if err != nil {
			return errors.New("the tree could not be read after review")
		}
		actual, err := readTextFile(filename, consumer.Mode.MaxFileBytes)
		if err != nil || string(actual) != expected {
			return errors.New("the reviewing agent changed the tree")
		}
	}
	return nil
}

// ReviewAnswerRulesTail is the last line of the answer-format rules in the
// reviewing agent's instruction. The transcript usually echoes the whole
// instruction — including the verdict-shaped format examples — so the decoder
// uses this line as the boundary and only reads what the agent printed after
// it. Prompt builder and decoder must share the exact string for the boundary
// to hold.
const ReviewAnswerRulesTail = "- findings は 16 件までです。"

// DecodeAgentReviewOutput reads the verdict out of what the reviewing agent
// printed. A CLI agent writes prose and then its answer, so the last balanced
// JSON object in the output is taken as the verdict; anything else is a failed
// review rather than a guess. Text up to the echoed instruction's final rule
// line is ignored so a format example inside the instruction can never be
// mistaken for the verdict.
func DecodeAgentReviewOutput(transcript string) (ModelReviewOutput, error) {
	if len(transcript) > MaxAgentTranscriptBytes {
		return ModelReviewOutput{}, errors.New("transcript is too large")
	}
	if boundary := strings.LastIndex(transcript, ReviewAnswerRulesTail); boundary >= 0 {
		transcript = transcript[boundary+len(ReviewAnswerRulesTail):]
	}
	block, err := lastJSONObject(transcript)
	if err != nil {
		return ModelReviewOutput{}, errors.New("the reviewing agent did not report a verdict")
	}
	return DecodeModelReviewOutput([]byte(block))
}

// lastJSONObject returns the final balanced {...} run in the text, ignoring
// braces inside strings so prose containing braces cannot split an object.
func lastJSONObject(text string) (string, error) {
	if len(text) > MaxAgentTranscriptBytes {
		return "", errors.New("transcript is too large")
	}
	for end := strings.LastIndexByte(text, '}'); end >= 0; end = strings.LastIndexByte(text[:end], '}') {
		start, ok := matchingObjectStart(text[:end+1])
		if !ok {
			continue
		}
		block := text[start : end+1]
		var probe map[string]json.RawMessage
		if json.Unmarshal([]byte(block), &probe) != nil {
			continue
		}
		if _, carriesVerdict := probe["verdict"]; carriesVerdict {
			return block, nil
		}
	}
	return "", errors.New("no verdict object was printed")
}

// matchingObjectStart scans backwards from a closing brace at the end of text
// to the brace that opens it.
func matchingObjectStart(text string) (int, bool) {
	depth := 0
	inString := false
	for index := len(text) - 1; index >= 0; index-- {
		character := text[index]
		if inString {
			if character == '"' && !escapedAt(text, index) {
				inString = false
			}
			continue
		}
		switch character {
		case '"':
			inString = true
		case '}':
			depth++
		case '{':
			depth--
			if depth == 0 {
				return index, true
			}
		}
	}
	return 0, false
}

func escapedAt(text string, index int) bool {
	backslashes := 0
	for cursor := index - 1; cursor >= 0 && text[cursor] == '\\'; cursor-- {
		backslashes++
	}
	return backslashes%2 == 1
}
