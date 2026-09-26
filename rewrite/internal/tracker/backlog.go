// Package tracker reads the requester's own words from the configured tracker.
// It does not turn them into a model-authored acceptance contract.
package tracker

import (
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

type Backlog struct {
	BaseURL string       `json:"base_url"`
	KeyEnv  string       `json:"key_env"`
	Client  *http.Client `json:"-"`
}

// Request retains the full description; no model is asked whether to admit it.
func (b Backlog) Request(ctx context.Context, issue string) (string, error) {
	base, err := url.Parse(b.BaseURL)
	if err != nil || base.Scheme != "https" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return "", errors.New("tracker endpoint must be an HTTPS URL without credentials or query")
	}
	key := os.Getenv(b.KeyEnv)
	if key == "" {
		return "", errors.New("tracker credential is unavailable")
	}
	address := strings.TrimRight(b.BaseURL, "/") + "/issues/" + url.PathEscape(issue)
	query := url.Values{"apiKey": []string{key}}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address+"?"+query.Encode(), nil)
	if err != nil {
		return "", errors.New("cannot construct tracker request")
	}
	client := http.Client{}
	if b.Client != nil {
		client = *b.Client
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	redact := func(text string) string {
		return strings.ReplaceAll(strings.ReplaceAll(text, url.QueryEscape(key), "[credential]"), key, "[credential]")
	}
	response, err := client.Do(req)
	if err != nil {
		return "", errors.New(redact(err.Error()))
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return "", errors.New(redact(err.Error()))
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("tracker returned HTTP %d: %s", response.StatusCode, redact(string(data)))
	}
	var ticket struct {
		Key         string `json:"issueKey"`
		Summary     string `json:"summary"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(data, &ticket); err != nil {
		return "", errors.New("tracker response could not be read")
	}
	// These fields are the tracker's API envelope, not requirements for the
	// requester's prose. The description may use any format or be empty.
	if ticket.Key == "" {
		return "", errors.New("tracker response has no issue identity")
	}
	return "Original issue: " + ticket.Key + "\nTitle: " + ticket.Summary + "\n\n" + ticket.Description, nil
}
