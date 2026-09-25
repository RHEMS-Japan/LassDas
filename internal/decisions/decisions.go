// Package decisions asks a decision model a fixed set of typed questions
// about a state and returns typed answers with a confidence for each.
//
// It is not a chat client and shares nothing with one. A chat seat is
// reached at an OpenAI-compatible /chat/completions address and answers with
// prose this engine then has to parse; a decision is reached at its own
// address, takes named questions, and answers with an option id and a
// number. The two addresses are different services behind the same key, so
// the role that names this one carries its own base URL and is never
// defaulted to a gateway's.
//
// Nothing in this package may end a run. Every failure - an address that
// does not answer, a body that cannot be read, an answer for a question
// nobody asked - comes back as an error, and the only thing a caller may
// conclude from one is that the model has no opinion. The caller then does
// exactly what it did before the model existed.
package decisions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// DefaultBaseURL is the decisions service's own address. It is not the
// address the chat seats use: posting a decision to a chat gateway reaches
// either nothing or a completions handler that answers prose, and a role
// that inherited a gateway's base URL would fail on every call in a way
// that looks like the model being down.
const DefaultBaseURL = "https://openrouter.ai/api/alpha"

// decisionsPath is appended to the base URL. It is held apart from the base
// so an operator who pins a different address still reaches the same verb.
const decisionsPath = "/decisions"

// DefaultDeadline bounds one call. The reception asks this before it starts
// work a requester is waiting on, so a model that has stopped answering must
// cost the run seconds, not minutes; ten is several times the observed
// answer and still short enough that losing it changes nothing.
const DefaultDeadline = 10 * time.Second

// MaxStateBytes bounds the state one call may carry. The model's context is
// 32,000 tokens; this is the engine's own bound, comfortably inside it, so a
// caller whose input grew is refused here with a number rather than by the
// service with a status.
const MaxStateBytes = 48 * 1024

// MaxQuestions bounds one call's questions. They are judged in parallel
// against the same state, so asking several costs one round trip - but an
// unbounded set is a caller's mistake, not a feature.
const MaxQuestions = 32

// maxResponseBytes bounds the body read back. An answer is ids and numbers,
// so it is small; the limit is what stops an error page from being parsed.
const maxResponseBytes = 1 << 16

// Kind is the shape of one question's answer.
type Kind string

const (
	// KindChoice asks which one of named options applies. Its criteria are
	// the options: an id for each, and what that id means.
	KindChoice Kind = "choice"
	// KindNoul asks whether a condition holds, answered as the probability
	// that it does. Its criteria, when it carries any, say what true means
	// and what false means, under exactly those two names.
	KindNoul Kind = "noul"
	// KindScore asks where something falls on an ordered scale. Its criteria
	// are the scale's bands, lowest first.
	KindScore Kind = "score"
)

// Question is one question asked of a state.
type Question struct {
	Type         Kind   `json:"type"`
	Instructions string `json:"instructions"`
	// Criteria carries a choice's options as a map of id to meaning, or a
	// score's bands as an ordered list. A noul carries none. The shapes are
	// not interchangeable, so Validate checks which one is present rather
	// than letting the service answer a malformed body with a status the
	// caller reads as "the model is down".
	Criteria any `json:"criteria,omitempty"`
}

// Questions is one call's questions, by the id their answers come back under.
type Questions map[string]Question

// Request is one call: which model, the state judged, and the questions.
type Request struct {
	Model string `json:"model"`
	// State is the thing being judged. It travels as whatever JSON it
	// marshals to - a string or an object - because what a state should
	// look like belongs to the caller asking the questions, not here.
	State     any       `json:"state"`
	Questions Questions `json:"questions"`
}

// Answer is one answered question. Which value is meaningful is decided by
// Type, and each is a pointer so an answer that carried nothing is told
// apart from one that carried zero: read as zero, an absent probability
// says "certainly not" about a question nobody answered.
type Answer struct {
	Type   Kind     `json:"type"`
	Choice *string  `json:"choice,omitempty"`
	Noul   *float64 `json:"noul,omitempty"`
	Score  *float64 `json:"score,omitempty"`
	// Confidence is how sure the model is of the answer it gave, apart from
	// the probability it gave to each option. A choice and a score carry
	// one; a noul's probability is its own confidence.
	Confidence *float64 `json:"confidence,omitempty"`
	// Probabilities is the mass the model put on each option, by option id
	// for a choice and by band index for a score.
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	// Legend names a score's bands back, so a caller reading a record long
	// afterwards can tell what band 2 meant.
	Legend map[string]string `json:"legend,omitempty"`
}

// Usage is what the call cost. Input tokens are billed and output tokens are
// not, so the cost of asking is decided by the size of the state.
type Usage struct {
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	Cost         float64 `json:"cost"`
}

// Answers is one call's reply: the answers by question id, and the record of
// which model answered and what it cost. The ids are the caller's own, so a
// caller reads its answers back under the names it asked under.
type Answers struct {
	ID       string            `json:"id"`
	Model    string            `json:"model"`
	Provider string            `json:"provider"`
	Answers  map[string]Answer `json:"answers"`
	Usage    Usage             `json:"usage"`
}

// Choice reads one choice answer: the option the model picked and how sure
// it was. It reports false when the question was not answered as a choice,
// so a caller never has to tell an absent answer from a picked option by
// looking at a zero value.
func (a Answers) Choice(id string) (option string, confidence float64, ok bool) {
	answer, present := a.Answers[id]
	if !present || answer.Choice == nil {
		return "", 0, false
	}
	if answer.Confidence != nil {
		confidence = *answer.Confidence
	}
	return *answer.Choice, confidence, true
}

// Endpoint is where a decision is asked and how the call is authorized. The
// key itself is never held here: APIKeyEnv names the variable the operator
// injects it through, so nothing that prints an endpoint can print a key.
type Endpoint struct {
	Provider  string
	Model     string
	BaseURL   string
	APIKeyEnv string
}

// Client posts decisions to one endpoint.
type Client struct {
	endpoint Endpoint
	http     *http.Client
	// deadline bounds one call. Zero means DefaultDeadline; a test sets a
	// short one so the behaviour of a model that never answers can be
	// observed without waiting for the real bound.
	deadline time.Duration
}

// New builds a client for one endpoint. An endpoint that cannot be called is
// refused here rather than on the first call, so a misconfigured role is a
// configuration error and not an intermittent model failure.
func New(endpoint Endpoint, httpClient *http.Client) (*Client, error) {
	if httpClient == nil {
		return nil, errors.New("decisions: no transport was given")
	}
	if strings.TrimSpace(endpoint.Model) == "" {
		return nil, errors.New("decisions: the endpoint names no model")
	}
	if endpoint.BaseURL == "" {
		endpoint.BaseURL = DefaultBaseURL
	}
	if err := validateBaseURL(endpoint.BaseURL); err != nil {
		return nil, err
	}
	if !apiKeyEnvPattern(endpoint.APIKeyEnv) {
		return nil, errors.New("decisions: the endpoint names no key variable")
	}
	return &Client{endpoint: endpoint, http: httpClient}, nil
}

// WithDeadline returns the client with a different per-call bound. Zero
// restores DefaultDeadline.
func (c *Client) WithDeadline(deadline time.Duration) *Client {
	if c == nil {
		return nil
	}
	copied := *c
	copied.deadline = deadline
	return &copied
}

// Endpoint reports where this client asks, without its key.
func (c *Client) Endpoint() Endpoint {
	if c == nil {
		return Endpoint{}
	}
	return c.endpoint
}

// validateBaseURL refuses an address a key could leak through. A transport
// failure is reported with the address it happened at, so an address that
// carried credentials in its userinfo or its query would put them in an
// error string and from there into a log. A port is allowed, unlike the
// chat seats' stricter spelling rule, because nothing here compares two
// addresses for equality.
func validateBaseURL(value string) error {
	if len(value) > 256 || strings.HasSuffix(value, "/") || strings.ContainsAny(value, "\r\n\x00 ") {
		return errors.New("decisions: the endpoint address is not usable")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("decisions: the endpoint address is not usable")
	}
	return nil
}

// apiKeyEnvPattern is the shape of an environment variable name, the same
// shape the other roles' key variables are held to.
func apiKeyEnvPattern(name string) bool {
	if len(name) == 0 || len(name) > 64 {
		return false
	}
	for index, r := range name {
		switch {
		case r >= 'A' && r <= 'Z':
		case index > 0 && (r == '_' || (r >= '0' && r <= '9')):
		default:
			return false
		}
	}
	return true
}

// Validate refuses a call that cannot mean anything, naming the question
// that is wrong. The service answers a malformed body with its own status,
// and that reaches a caller as "the decision could not be read" - which
// says nothing about which question was built wrong.
func (r Request) Validate() error {
	if strings.TrimSpace(r.Model) == "" {
		return errors.New("decisions: no model was named")
	}
	if r.State == nil {
		return errors.New("decisions: no state was given to judge")
	}
	encoded, err := json.Marshal(r.State)
	if err != nil {
		return errors.New("decisions: the state cannot be encoded")
	}
	if len(encoded) > MaxStateBytes {
		return fmt.Errorf("decisions: the state is %d bytes, over the %d allowed", len(encoded), MaxStateBytes)
	}
	if len(r.Questions) == 0 {
		return errors.New("decisions: nothing was asked")
	}
	if len(r.Questions) > MaxQuestions {
		return fmt.Errorf("decisions: %d questions were asked, over the %d allowed", len(r.Questions), MaxQuestions)
	}
	for _, id := range SortedIDs(r.Questions) {
		question := r.Questions[id]
		if strings.TrimSpace(id) == "" {
			return errors.New("decisions: a question has no id")
		}
		if strings.TrimSpace(question.Instructions) == "" {
			return errors.New("decisions: question " + id + " says nothing to decide")
		}
		switch question.Type {
		case KindNoul:
			// A noul may go without criteria, and when it carries them the
			// service names exactly two sides. Another key is a meaning the
			// answer has no room to carry: a noul comes back as one
			// probability, so a third side could never be reported.
			if question.Criteria != nil {
				sides, ok := question.Criteria.(map[string]string)
				if !ok {
					return errors.New("decisions: question " + id + " is a noul and needs its criteria as the meaning of true and false")
				}
				for side, meaning := range sides {
					if side != "true" && side != "false" {
						return errors.New("decisions: question " + id + " is a noul offering " + side + ", which is neither true nor false")
					}
					if strings.TrimSpace(meaning) == "" {
						return errors.New("decisions: question " + id + " has a side that means nothing")
					}
				}
			}
		case KindChoice:
			options, ok := question.Criteria.(map[string]string)
			if !ok {
				return errors.New("decisions: question " + id + " is a choice and needs its options as a map of id to meaning")
			}
			if len(options) < 2 {
				return errors.New("decisions: question " + id + " is a choice with fewer than two options")
			}
			for option, meaning := range options {
				if strings.TrimSpace(option) == "" || strings.TrimSpace(meaning) == "" {
					return errors.New("decisions: question " + id + " has an option that means nothing")
				}
			}
		case KindScore:
			bands, ok := question.Criteria.([]string)
			if !ok || len(bands) < 2 {
				return errors.New("decisions: question " + id + " is a score and needs at least two bands as a list")
			}
		default:
			return errors.New("decisions: question " + id + " asks for " + string(question.Type) + ", which is not noul, choice or score")
		}
	}
	return nil
}

// SortedIDs lists a question set's ids in a stable order, so a refusal names
// the same question every time the same set is wrong.
func SortedIDs(questions Questions) []string {
	ids := make([]string, 0, len(questions))
	for id := range questions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Judge asks the questions about the state and returns the answers. It takes
// its own deadline: a caller that passed a longer context still gets an
// answer or an error within the bound, because the thing waiting on it is a
// run that must not stall on a model.
//
// Every return path but the last is an error, and an error here means the
// model has no opinion. A caller acts as it would have acted with no model
// configured at all - it never treats an error as a "no".
func (c *Client) Judge(ctx context.Context, state any, questions Questions) (Answers, error) {
	if c == nil || c.http == nil {
		return Answers{}, errors.New("decisions: no client")
	}
	if ctx == nil {
		return Answers{}, errors.New("decisions: no context")
	}
	request := Request{Model: c.endpoint.Model, State: state, Questions: questions}
	if err := request.Validate(); err != nil {
		return Answers{}, err
	}
	apiKey := os.Getenv(c.endpoint.APIKeyEnv)
	// A key with a newline in it would split the header, and a key with
	// surrounding space is one that was pasted with the line it came on.
	// Both are refused without saying what was read.
	if apiKey == "" || strings.TrimSpace(apiKey) != apiKey || strings.ContainsAny(apiKey, "\r\n\x00") {
		return Answers{}, errors.New("decisions: the key variable holds nothing usable")
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return Answers{}, errors.New("decisions: the request could not be encoded")
	}
	deadline := c.deadline
	if deadline <= 0 {
		deadline = DefaultDeadline
	}
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.endpoint.BaseURL, "/")+decisionsPath, bytes.NewReader(encoded))
	if err != nil {
		return Answers{}, errors.New("decisions: the request could not be built")
	}
	// The key travels in this header and nowhere else. Nothing below puts a
	// header into an error, and the address carries no credentials, so no
	// failure path can print it.
	httpRequest.Header.Set("Authorization", "Bearer "+apiKey)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpResponse, err := c.http.Do(httpRequest)
	if err != nil {
		// The cause without the key: a timeout, a refused connection and a
		// reset each want a different remedy, and hiding which one happened
		// leaves an operator nothing to act on.
		return Answers{}, fmt.Errorf("decisions: the call failed: %w", err)
	}
	defer httpResponse.Body.Close()
	body, err := io.ReadAll(io.LimitReader(httpResponse.Body, maxResponseBytes+1))
	if err != nil || len(body) > maxResponseBytes {
		return Answers{}, errors.New("decisions: the answer could not be read")
	}
	// Only 200 is an answer. The service documents no other success status
	// for this verb, so anything else is refused rather than parsed: reading
	// a body the service did not mean as an answer is how a redirect or a
	// maintenance page becomes a judgment.
	if httpResponse.StatusCode != http.StatusOK {
		// The service's own words: a key that is not allowed this model is
		// refused with a sentence saying so, and dropping it would leave the
		// operator guessing. The words are the service's, not this engine's,
		// so a service that quotes the key it refused would otherwise be the
		// way the key reaches a log - it is taken back out here, where its
		// value is still known.
		return Answers{}, fmt.Errorf("decisions: the service answered %d: %s",
			httpResponse.StatusCode, firstLine(withoutKey(string(body), apiKey), 200))
	}
	var answers Answers
	if err := json.Unmarshal(body, &answers); err != nil {
		return Answers{}, errors.New("decisions: the answer could not be read")
	}
	if err := answers.bind(questions); err != nil {
		return Answers{}, err
	}
	return answers, nil
}

// bind refuses a reply that does not answer what was asked. An answer for a
// question nobody asked is ignored; a question left unanswered, or answered
// in a shape it was not asked in, is an error - a caller reading a missing
// answer as a zero would act on an opinion the model never gave.
func (a Answers) bind(questions Questions) error {
	if len(a.Answers) == 0 {
		return errors.New("decisions: the answer carries no decisions")
	}
	for _, id := range SortedIDs(questions) {
		answer, ok := a.Answers[id]
		if !ok {
			return errors.New("decisions: " + id + " was left unanswered")
		}
		if err := answer.valueFor(questions[id], id); err != nil {
			return err
		}
	}
	return nil
}

// valueFor holds one answer to the question it answers: the value its kind
// carries is present, a probability really is one, and a choice names an
// option that was actually offered. A choice outside its own options is the
// one failure that would otherwise pass silently into a caller's switch and
// fall through to whatever that switch does by default.
func (a Answer) valueFor(question Question, id string) error {
	switch question.Type {
	case KindNoul:
		if a.Noul == nil {
			return errors.New("decisions: " + id + " came back with no probability")
		}
		if !isProbability(*a.Noul) {
			return errors.New("decisions: " + id + " came back with a probability that is not one")
		}
	case KindChoice:
		if a.Choice == nil || strings.TrimSpace(*a.Choice) == "" {
			return errors.New("decisions: " + id + " names no option")
		}
		options, _ := question.Criteria.(map[string]string)
		if _, offered := options[*a.Choice]; !offered {
			return errors.New("decisions: " + id + " names an option that was not offered")
		}
	case KindScore:
		if a.Score == nil {
			return errors.New("decisions: " + id + " came back with no score")
		}
		bands, _ := question.Criteria.([]string)
		if *a.Score < 0 || *a.Score > float64(len(bands)-1) {
			return errors.New("decisions: " + id + " came back with a score off its scale")
		}
	}
	if a.Confidence != nil && !isProbability(*a.Confidence) {
		return errors.New("decisions: " + id + " came back with a confidence that is not a probability")
	}
	for _, mass := range a.Probabilities {
		if !isProbability(mass) {
			return errors.New("decisions: " + id + " came back with a probability that is not one")
		}
	}
	return nil
}

// isProbability also refuses a NaN, which compares false against every
// bound and would otherwise pass a range check written the obvious way.
func isProbability(value float64) bool {
	return value >= 0 && value <= 1
}

// withoutKey takes the key back out of text this engine did not write. The
// key is bounded away from the empty string by the caller, so this never
// redacts everything.
func withoutKey(text, apiKey string) string {
	if apiKey == "" {
		return text
	}
	return strings.ReplaceAll(text, apiKey, "[redacted]")
}

// firstLine is a message without its body: enough to say what was refused,
// bounded so an error page cannot fill a log.
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
