package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
)

// A decision model answers a question about a state with a typed value and a
// confidence, and never with prose. It is not a smaller chat model: it has
// no text output at all, so it can take the seats where this engine already
// throws the prose away and keeps a field — and it cannot take the seats
// that produce a change, a design or a finding.
//
// The gateway is the one this instance already uses: the endpoint is
// POST {base}/systemone beside the /chat/completions this engine posts to,
// reached with the same key. Nothing new is configured to use one.

// MaxSystemOneStateBytes bounds the state sent for a decision. The models
// take 32,000 tokens; this is the engine's own bound, well inside it, so a
// caller that grew its input is refused here instead of by the gateway.
const MaxSystemOneStateBytes = 48 * 1024

// MaxSystemOneQuestions bounds one call's questions. They are evaluated in
// parallel against the same state, so asking several costs one round trip —
// but an unbounded map is a caller mistake, not a feature.
const MaxSystemOneQuestions = 32

// maxSystemOneResponseBytes bounds the answer. A decision names ids and
// numbers, so it is small.
const maxSystemOneResponseBytes = 1 << 15

// SystemOneQuestionKind is what shape an answer takes.
type SystemOneQuestionKind string

const (
	// SystemOneNoul asks whether something holds, answered as the
	// probability that it does.
	SystemOneNoul SystemOneQuestionKind = "noul"
	// SystemOneChoice asks which of named options applies.
	SystemOneChoice SystemOneQuestionKind = "choice"
	// SystemOneScore asks for a number against named criteria.
	SystemOneScore SystemOneQuestionKind = "score"
)

// SystemOneQuestion is one question asked of a state.
type SystemOneQuestion struct {
	Kind         SystemOneQuestionKind `json:"type"`
	Instructions string                `json:"instructions"`
	// Criteria carries a choice's options (id to what it means) or a
	// score's named bands. The two shapes differ, and sending the wrong one
	// is refused by the gateway, so Validate checks which is present.
	Criteria any `json:"criteria,omitempty"`
}

// SystemOneAnswer is one answered question. Exactly one of the three values
// is meaningful, decided by Kind.
type SystemOneAnswer struct {
	Kind       SystemOneQuestionKind `json:"type"`
	Noul       *float64              `json:"noul,omitempty"`
	Choice     *string               `json:"choice,omitempty"`
	Score      *float64              `json:"score,omitempty"`
	Confidence *float64              `json:"confidence,omitempty"`
}

// SystemOneRequest is one call: a state, and the questions asked of it.
type SystemOneRequest struct {
	Model     string                       `json:"model"`
	State     string                       `json:"state"`
	Questions map[string]SystemOneQuestion `json:"questions"`
}

// Validate refuses a call that cannot mean anything, naming which question
// is wrong: the gateway answers a malformed body with its own 400, and that
// reached the run as "the decision could not be read".
func (r SystemOneRequest) Validate() error {
	if strings.TrimSpace(r.Model) == "" {
		return safeModelLiteral("decision model is not named")
	}
	if strings.TrimSpace(r.State) == "" {
		return safeModelLiteral("decision state is empty")
	}
	if len(r.State) > MaxSystemOneStateBytes {
		return safeModelError(fmt.Sprintf("decision state is %d bytes, over the %d allowed", len(r.State), MaxSystemOneStateBytes))
	}
	if len(r.Questions) == 0 {
		return safeModelLiteral("no decision was asked for")
	}
	if len(r.Questions) > MaxSystemOneQuestions {
		return safeModelError(fmt.Sprintf("%d questions were asked, over the %d allowed", len(r.Questions), MaxSystemOneQuestions))
	}
	for _, id := range sortedQuestionIDs(r.Questions) {
		question := r.Questions[id]
		if strings.TrimSpace(id) == "" {
			return safeModelLiteral("a decision question has no id")
		}
		if strings.TrimSpace(question.Instructions) == "" {
			return safeModelError("decision question " + id + " says nothing to decide")
		}
		switch question.Kind {
		case SystemOneNoul:
			if question.Criteria != nil {
				return safeModelError("decision question " + id + " is a noul and takes no criteria")
			}
		case SystemOneChoice:
			if _, ok := question.Criteria.(map[string]string); !ok {
				return safeModelError("decision question " + id + " is a choice and needs its options as a map of id to meaning")
			}
		case SystemOneScore:
			if _, ok := question.Criteria.([]string); !ok {
				return safeModelError("decision question " + id + " is a score and needs its criteria as a list")
			}
		default:
			return safeModelError("decision question " + id + " asks for " + string(question.Kind) + ", which is not noul, choice or score")
		}
	}
	return nil
}

func sortedQuestionIDs(questions map[string]SystemOneQuestion) []string {
	ids := make([]string, 0, len(questions))
	for id := range questions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// SystemOneClient posts decisions to the gateway this instance already uses.
type SystemOneClient struct {
	client *http.Client
}

func NewSystemOneClient(client *http.Client) (*SystemOneClient, error) {
	if client == nil {
		return nil, safeModelLiteral("decision transport is invalid")
	}
	return &SystemOneClient{client: client}, nil
}

// Decide asks the questions and returns the answers by question id. Every
// failure says what it was: a run that cannot get a decision falls back to
// the seat that was doing the work before, and the reason it fell back is
// the only way anyone learns the decision model is unreachable.
func (c *SystemOneClient) Decide(ctx context.Context, endpoint ModelEndpoint, request SystemOneRequest) (map[string]SystemOneAnswer, error) {
	if c == nil || c.client == nil || ctx == nil {
		return nil, safeModelLiteral("decision transport is invalid")
	}
	if err := request.Validate(); err != nil {
		return nil, err
	}
	apiKey := os.Getenv(endpoint.APIKeyEnv)
	if endpoint.APIKeyEnv == "" || apiKey == "" || strings.TrimSpace(apiKey) != apiKey || strings.ContainsAny(apiKey, "\r\n\x00") {
		return nil, safeModelLiteral("decision API key is unavailable")
	}
	if strings.TrimSpace(endpoint.BaseURL) == "" {
		return nil, safeModelLiteral("decision endpoint has no base URL")
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return nil, safeModelLiteral("decision request could not be encoded")
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(endpoint.BaseURL, "/")+"/systemone", bytes.NewReader(encoded))
	if err != nil {
		return nil, safeModelLiteral("decision request could not be built")
	}
	httpRequest.Header.Set("Authorization", "Bearer "+apiKey)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpResponse, err := c.client.Do(httpRequest)
	if err != nil {
		// The cause without the URL, the way the chat transport reports it:
		// a timeout, a refused connection and a reset each have a different
		// remedy. The key travels in a header, never in the error.
		return nil, safeModelErrorFor("the decision transport failed: "+err.Error(), err)
	}
	defer httpResponse.Body.Close()
	body, err := io.ReadAll(io.LimitReader(httpResponse.Body, maxSystemOneResponseBytes+1))
	if err != nil || len(body) > maxSystemOneResponseBytes {
		return nil, safeModelLiteral("the decision could not be read")
	}
	if httpResponse.StatusCode != http.StatusOK {
		// The gateway's own words: a key that is not allowed this model
		// answers 403 with that sentence, and hiding it leaves the operator
		// nothing to act on.
		return nil, safeModelError(fmt.Sprintf("the decision gateway answered %d: %s",
			httpResponse.StatusCode, firstLine(string(body), 200)))
	}
	var answers map[string]SystemOneAnswer
	if err := json.Unmarshal(body, &answers); err != nil {
		return nil, safeModelLiteral("the decision could not be read")
	}
	for _, id := range sortedQuestionIDs(request.Questions) {
		answer, ok := answers[id]
		if !ok {
			return nil, safeModelError("the decision left " + id + " unanswered")
		}
		if err := answer.valueFor(request.Questions[id].Kind, id); err != nil {
			return nil, err
		}
	}
	return answers, nil
}

// valueFor refuses an answer whose value is missing or the wrong shape for
// what was asked. A noul of nil is not a noul of zero: read as zero it would
// say "certainly not" about a question nobody answered.
func (a SystemOneAnswer) valueFor(kind SystemOneQuestionKind, id string) error {
	switch kind {
	case SystemOneNoul:
		if a.Noul == nil {
			return safeModelError("the decision for " + id + " carries no probability")
		}
		if *a.Noul < 0 || *a.Noul > 1 {
			return safeModelError("the decision for " + id + " is not a probability")
		}
	case SystemOneChoice:
		if a.Choice == nil || strings.TrimSpace(*a.Choice) == "" {
			return safeModelError("the decision for " + id + " names no choice")
		}
	case SystemOneScore:
		if a.Score == nil {
			return safeModelError("the decision for " + id + " carries no score")
		}
	}
	return nil
}

// firstLine is the gateway's message without its body: enough to say what
// was refused, bounded so an HTML error page cannot fill a log.
func firstLine(text string, limit int) string {
	text = strings.TrimSpace(text)
	if index := strings.IndexAny(text, "\r\n"); index >= 0 {
		text = text[:index]
	}
	if len(text) > limit {
		text = text[:limit]
	}
	return text
}
