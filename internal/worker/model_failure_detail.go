package worker

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
)

// ModelFailureDetail is what a turn knew when it gave up: the phrase that
// ends it, how many calls it made and why, and the last answer the gateway
// gave (its id, finish reason, token counts, status). It reaches the runner
// on one stderr line, FailureDetailLinePrefix followed by the JSON, so the
// run directory can keep it beside the failed step and the ticket page can
// show why an AI stage failed instead of only that it did (live 2026-09-15:
// a reception that spent its whole allowance reasoning left nothing but a
// step name; the cause was found in the gateway's own logs).
//
// Nothing here is upstream text: the phrase is the turn's own error (the
// package's constants, counts and provider enums), the ids and counts are
// numbers and patterns, and the key never appears - not even by name.
type ModelFailureDetail struct {
	Phrase          string `json:"phrase"`
	Model           string `json:"model"`
	Effort          string `json:"effort,omitempty"`
	MaxOutputTokens int32  `json:"max_output_tokens"`
	Calls           int    `json:"calls"`
	Lowered         int    `json:"lowered,omitempty"`
	Widened         bool   `json:"widened,omitempty"`
	Malformed       int    `json:"malformed,omitempty"`
	ProviderErrors  int    `json:"provider_errors,omitempty"`
	AllowanceSpent  int    `json:"allowance_spent,omitempty"`
	// The last answer the gateway gave, when it gave one.
	LastRequestID        string `json:"last_request_id,omitempty"`
	LastFinishReason     string `json:"last_finish_reason,omitempty"`
	LastPromptTokens     int32  `json:"last_prompt_tokens,omitempty"`
	LastCompletionTokens int32  `json:"last_completion_tokens,omitempty"`
	LastReasoningTokens  int32  `json:"last_reasoning_tokens,omitempty"`
	LastHTTPStatus       int    `json:"last_http_status,omitempty"`
}

// FailureDetailLinePrefix begins the one stderr line that carries the
// detail. It is not the worker's failure-line prefix ("worker: "), so the
// runner's reading of the cause line is untouched by it.
const FailureDetailLinePrefix = "worker-evidence: model-failure "

// maxFailureDetailBytes bounds the line on both sides.
const maxFailureDetailBytes = 4096

var (
	failureDetailPhrasePattern = regexp.MustCompile(`^[\x20-\x7e]{1,600}$`)
	failureDetailIDPattern     = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	failureDetailWordPattern   = regexp.MustCompile(`^[A-Za-z0-9._/:-]{1,128}$`)
)

// failureDetailSink is where the line is written; tests swap it.
var (
	failureDetailSink   io.Writer = os.Stderr
	failureDetailSinkMu sync.Mutex
)

// Validate refuses a detail outside the shape above. The runner applies it
// to what it reads back, so a line something else wrote in the worker's
// name is dropped rather than recorded.
func (d ModelFailureDetail) Validate() error {
	if !failureDetailPhrasePattern.MatchString(d.Phrase) {
		return errors.New("model failure detail phrase is invalid")
	}
	if d.Model != "" && !failureDetailWordPattern.MatchString(d.Model) {
		return errors.New("model failure detail model is invalid")
	}
	if d.Effort != "" && !failureDetailWordPattern.MatchString(d.Effort) {
		return errors.New("model failure detail effort is invalid")
	}
	if d.LastRequestID != "" && !failureDetailIDPattern.MatchString(d.LastRequestID) {
		return errors.New("model failure detail request id is invalid")
	}
	if d.LastFinishReason != "" && !failureDetailWordPattern.MatchString(d.LastFinishReason) {
		return errors.New("model failure detail finish reason is invalid")
	}
	if d.MaxOutputTokens < 0 || d.Calls < 1 || d.Calls > 64 || d.Lowered < 0 || d.Malformed < 0 || d.ProviderErrors < 0 || d.AllowanceSpent < 0 ||
		d.LastPromptTokens < 0 || d.LastCompletionTokens < 0 || d.LastReasoningTokens < 0 || d.LastHTTPStatus < 0 || d.LastHTTPStatus > 999 {
		return errors.New("model failure detail counts are invalid")
	}
	return nil
}

// writeFailureDetail puts the line on the sink. Best-effort: a detail that
// cannot be written changes nothing about the failure it describes.
func writeFailureDetail(detail ModelFailureDetail) {
	if err := detail.Validate(); err != nil {
		return
	}
	encoded, err := json.Marshal(detail)
	if err != nil || len(encoded) > maxFailureDetailBytes {
		return
	}
	failureDetailSinkMu.Lock()
	defer failureDetailSinkMu.Unlock()
	_, _ = io.WriteString(failureDetailSink, FailureDetailLinePrefix+string(encoded)+"\n")
}

// ParseFailureDetailLine reads the detail back from a worker's stderr: the
// last line carrying the prefix, decoded strictly and validated. Anything
// else - no such line, unknown fields, a shape outside Validate - is
// reported as absent, never as a partial detail.
func ParseFailureDetailLine(stderr string) (ModelFailureDetail, bool) {
	var encoded string
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, FailureDetailLinePrefix) {
			encoded = strings.TrimPrefix(line, FailureDetailLinePrefix)
		}
	}
	if encoded == "" || len(encoded) > maxFailureDetailBytes {
		return ModelFailureDetail{}, false
	}
	decoder := json.NewDecoder(strings.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var detail ModelFailureDetail
	if err := decoder.Decode(&detail); err != nil || detail.Validate() != nil {
		return ModelFailureDetail{}, false
	}
	return detail, true
}
