package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
)

// ModelFailureDetail is what a turn knew when it gave up: the class of
// failure that ends it, how many calls it made and why, and the last answer
// the gateway gave (its id, finish reason, token counts, status). It reaches
// the runner on one stderr line, FailureDetailLinePrefix followed by the
// JSON, so the run directory can keep it beside the failed step and the
// ticket page can show why an AI stage failed instead of only that it did
// (live 2026-09-15: a reception that spent its whole allowance reasoning
// left nothing but a step name; the cause was found in the gateway's logs).
//
// Nothing here is upstream text. The phrase is composed from this
// package's own constants and numbers (detailPhrase), never from an error's
// message - a transport failure quotes what the wire carried, and that must
// not land in a record. The ids and enums are checked against patterns; a
// field outside its pattern is blanked, not written. The key never appears,
// not even by name.
type ModelFailureDetail struct {
	Phrase string `json:"phrase"`
	Model  string `json:"model,omitempty"`
	// Effort and MaxOutputTokens are the configured values the turn
	// started with; FinalEffort and FinalMaxOutputTokens are what the last
	// call used after the turn lowered the effort or widened the allowance.
	Effort               string `json:"effort,omitempty"`
	MaxOutputTokens      int32  `json:"max_output_tokens"`
	FinalEffort          string `json:"final_effort,omitempty"`
	FinalMaxOutputTokens int32  `json:"final_max_output_tokens,omitempty"`
	Calls                int    `json:"calls"`
	Lowered              int    `json:"lowered,omitempty"`
	Widened              bool   `json:"widened,omitempty"`
	Malformed            int    `json:"malformed,omitempty"`
	ProviderErrors       int    `json:"provider_errors,omitempty"`
	AllowanceSpent       int    `json:"allowance_spent,omitempty"`
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
	// A model name, an effort, a finish reason: provider and configuration
	// vocabularies (vendor/model:tag, model@date, high, content_filter).
	failureDetailWordPattern = regexp.MustCompile(`^[A-Za-z0-9._/:@+-]{1,128}$`)
)

// failureDetailSink is where the line is written; tests swap it.
var (
	failureDetailSink   io.Writer = os.Stderr
	failureDetailSinkMu sync.Mutex
)

// detailPhrase names the failure from the error's class, in the package's
// own words: the constants the runner's notes already key on, the status as
// a number, and nothing an error message may have quoted from the wire.
func detailPhrase(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	var parts []string
	var safe *SafeModelError
	switch {
	case errors.Is(err, errModelResponseTruncated):
		parts = append(parts, CutoffPhrase+": finish_reason="+ChatFinishLength)
		if errors.Is(err, errModelReasoningExhausted) {
			parts = append(parts, ReasoningExhaustedPhrase)
		}
	case errors.Is(err, errModelAllowanceSpent):
		parts = append(parts, TransportFailedPhrase+": "+SpentAllowancePhrase)
	case errors.Is(err, errModelResponseUpstream):
		parts = append(parts, ProviderEndedTurnPhrase)
	case errors.Is(err, errModelResponseRefused):
		parts = append(parts, DeclinedOverContentPhrase)
	case errors.Is(err, errModelResponseMetadata):
		parts = append(parts, GatewayBookkeepingPhrase)
	case errors.Is(err, errModelResponseContent):
		parts = append(parts, AnswerUnusablePhrase)
	case errors.As(err, &safe) && safe.Status() != 0:
		parts = append(parts, fmt.Sprintf("%s with status %d", TransportFailedPhrase, safe.Status()))
	case errors.As(err, &safe), strings.HasPrefix(message, TransportFailedPhrase):
		parts = append(parts, TransportFailedPhrase)
	case strings.HasPrefix(message, CutoffPhrase):
		// A finish reason outside the provider's enum ended the turn; the
		// reason itself is a provider string and travels only as its class.
		parts = append(parts, CutoffPhrase)
	default:
		parts = append(parts, "the turn failed")
	}
	// What the turn did about it, in the words the message carries - each
	// one a constant of this package, so its presence is safe to echo.
	for _, phrase := range []string{EffortLoweredPhrase, CutoffAskedAgainPhrase, CutoffAtCeilingPhrase, LimitNotLiftedPhrase, RetryAfterTooLongPhrase} {
		if strings.Contains(message, phrase) && !strings.Contains(strings.Join(parts, "; "), phrase) {
			parts = append(parts, phrase)
		}
	}
	if strings.Contains(message, AttemptsExhaustedPhrase) {
		parts = append(parts, "asked again until the attempts ran out")
	}
	return strings.Join(parts, "; ")
}

// sanitized blanks every field that is outside its shape and clamps the
// counts, so one odd value costs that field and not the record. The phrase
// is composed by detailPhrase; it is still bounded here.
func (d ModelFailureDetail) sanitized() ModelFailureDetail {
	word := func(value string) string {
		if failureDetailWordPattern.MatchString(value) {
			return value
		}
		return ""
	}
	clamp := func(n int) int {
		if n < 0 {
			return 0
		}
		return n
	}
	clamp32 := func(n int32) int32 {
		if n < 0 {
			return 0
		}
		return n
	}
	d.Model, d.Effort, d.FinalEffort, d.LastFinishReason = word(d.Model), word(d.Effort), word(d.FinalEffort), word(d.LastFinishReason)
	if !modelRequestIDPattern.MatchString(d.LastRequestID) {
		d.LastRequestID = ""
	}
	if !failureDetailPhrasePattern.MatchString(d.Phrase) {
		d.Phrase = "the turn failed"
	}
	d.Calls = clamp(d.Calls)
	if d.Calls == 0 {
		d.Calls = 1
	}
	if d.Calls > 64 {
		d.Calls = 64
	}
	d.Lowered, d.Malformed, d.ProviderErrors, d.AllowanceSpent = clamp(d.Lowered), clamp(d.Malformed), clamp(d.ProviderErrors), clamp(d.AllowanceSpent)
	d.MaxOutputTokens, d.FinalMaxOutputTokens = clamp32(d.MaxOutputTokens), clamp32(d.FinalMaxOutputTokens)
	d.LastPromptTokens, d.LastCompletionTokens, d.LastReasoningTokens = clamp32(d.LastPromptTokens), clamp32(d.LastCompletionTokens), clamp32(d.LastReasoningTokens)
	if d.LastHTTPStatus < 0 || d.LastHTTPStatus > 999 {
		d.LastHTTPStatus = 0
	}
	return d
}

// Validate refuses a detail outside the shape above. The runner applies it
// to what it reads back, so a line something else wrote in the worker's
// name is dropped rather than recorded.
func (d ModelFailureDetail) Validate() error {
	if !failureDetailPhrasePattern.MatchString(d.Phrase) {
		return errors.New("model failure detail phrase is invalid")
	}
	for _, value := range []string{d.Model, d.Effort, d.FinalEffort, d.LastFinishReason} {
		if value != "" && !failureDetailWordPattern.MatchString(value) {
			return errors.New("model failure detail word is invalid")
		}
	}
	if d.LastRequestID != "" && !modelRequestIDPattern.MatchString(d.LastRequestID) {
		return errors.New("model failure detail request id is invalid")
	}
	if d.MaxOutputTokens < 0 || d.FinalMaxOutputTokens < 0 || d.Calls < 1 || d.Calls > 64 || d.Lowered < 0 || d.Malformed < 0 || d.ProviderErrors < 0 || d.AllowanceSpent < 0 ||
		d.LastPromptTokens < 0 || d.LastCompletionTokens < 0 || d.LastReasoningTokens < 0 || d.LastHTTPStatus < 0 || d.LastHTTPStatus > 999 {
		return errors.New("model failure detail counts are invalid")
	}
	return nil
}

// writeFailureDetail puts the line on the sink. Best-effort: a detail that
// cannot be written changes nothing about the failure it describes.
func writeFailureDetail(detail ModelFailureDetail) {
	detail = detail.sanitized()
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
