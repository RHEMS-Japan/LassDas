package chain

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
	"strings"
	"time"
)

// Jev connects to the decisions API. URL is the complete configured endpoint;
// it does not inherit a chat endpoint or depend on the previous runtime.
type Jev struct {
	URL    string       `json:"url"`
	Model  string       `json:"model"`
	KeyEnv string       `json:"key_env"`
	Client *http.Client `json:"-"`
}

func (j Jev) Choose(ctx context.Context, state State, instructions string, choices map[string]string) (string, error) {
	data, err := j.request(ctx, map[string]any{
		"model": j.Model, "state": state,
		"questions": map[string]any{"next": map[string]any{
			"type": "choice", "instructions": instructions, "criteria": choices,
		}},
	})
	if err != nil {
		return "", err
	}
	var answer struct {
		Answers map[string]struct {
			Choice string `json:"choice"`
		} `json:"answers"`
	}
	if err := json.Unmarshal(data, &answer); err != nil {
		return "", errors.New("decision service did not return a choice")
	}
	return answer.Answers["next"].Choice, nil
}

// Both routing APIs use the same credential and HTTP transport rules. Their
// service protocols differ; neither decodes the working roles' prose.
func (j Jev) request(ctx context.Context, payload any) ([]byte, error) {
	address, err := url.Parse(j.URL)
	if err != nil || address.Scheme != "https" || address.Host == "" || address.User != nil || address.RawQuery != "" || address.Fragment != "" {
		return nil, errors.New("model endpoint must be an HTTPS URL without credentials or query parameters")
	}
	key := os.Getenv(j.KeyEnv)
	if key == "" || strings.ContainsAny(key, "\r\n") {
		return nil, errors.New("model credential is unavailable")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	// This transport also carries ordinary reasoning-model routing/selection.
	// A live completion took 35–40 seconds: a short RPC timeout discarded it
	// and retried the same work. Bound a stalled inference, not normal latency.
	// Parent cancellation (including requester stop) still interrupts at once.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, j.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+key)
	client := http.Client{}
	if j.Client != nil {
		client = *j.Client
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		detail := strings.ReplaceAll(string(data), key, "[credential]")
		return nil, fmt.Errorf("model service returned HTTP %d: %s", response.StatusCode, excerpt(detail, 1000))
	}
	return data, nil
}
