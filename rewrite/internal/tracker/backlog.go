// Package tracker exchanges ordinary prose with the configured tracker.
// It does not turn a request or report into a model-authored contract.
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
	"strconv"
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
	data, err := b.call(ctx, http.MethodGet, "/issues/"+url.PathEscape(issue), nil, nil, http.StatusOK)
	if err != nil {
		return "", err
	}
	return RequestText(data)
}

// RequestText renders native issue data without a model-authored admission
// contract. Intake can retain the exact original from its discovery response.
func RequestText(data []byte) (string, error) {
	var ticket struct {
		Key         string `json:"issueKey"`
		Summary     string `json:"summary"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(data, &ticket); err != nil {
		return "", errors.New("tracker response could not be read")
	}
	if ticket.Key == "" {
		return "", errors.New("tracker response has no issue identity")
	}
	return "Original issue: " + ticket.Key + "\nTitle: " + ticket.Summary + "\n\n" + ticket.Description, nil
}

// AddComment sends ordinary prose once. An ambiguous transport result is not
// permission to repeat a visible post: inspect Comments before deciding.
func (b Backlog) AddComment(ctx context.Context, issue, content string) (json.RawMessage, error) {
	data, err := b.call(ctx, http.MethodPost, "/issues/"+url.PathEscape(issue)+"/comments", nil,
		url.Values{"content": {content}}, http.StatusCreated)
	if err != nil {
		return nil, fmt.Errorf("comment submission not confirmed; it may already be posted; inspect comments before retrying: %w", err)
	}
	var receipt struct{ ID int64 }
	if err := json.Unmarshal(data, &receipt); err != nil || receipt.ID <= 0 {
		return nil, errors.New("comment submission returned no readable id; it may already be posted; inspect comments before retrying")
	}
	return data, nil
}

// Comments reads all pages after the supplied API comment id in ascending order.
// API metadata remains intact; the content is not decoded as a model verdict.
func (b Backlog) Comments(ctx context.Context, issue string, after int64) ([]json.RawMessage, error) {
	if after < 0 {
		return nil, errors.New("comment cursor must not be negative")
	}
	comments := []json.RawMessage{}
	for {
		cursor := after
		query := url.Values{"order": {"asc"}, "count": {"100"}, "minId": {strconv.FormatInt(after, 10)}}
		data, err := b.call(ctx, http.MethodGet, "/issues/"+url.PathEscape(issue)+"/comments", query, nil, http.StatusOK)
		if err != nil {
			return nil, err
		}
		var page []json.RawMessage
		if err := json.Unmarshal(data, &page); err != nil || page == nil {
			return nil, errors.New("tracker comments response is not an array")
		}
		for i, entry := range page {
			var comment struct{ ID int64 }
			if err := json.Unmarshal(entry, &comment); err != nil || comment.ID <= 0 {
				return nil, errors.New("tracker comment has no readable id")
			}
			// The documented minId is a minimum, without an exclusivity promise.
			// Tolerate that one boundary item, never skip a new comment.
			if i == 0 && comment.ID == cursor {
				continue
			}
			if comment.ID <= after {
				return nil, errors.New("tracker comment pagination did not advance; no incomplete list returned")
			}
			after = comment.ID
			comments = append(comments, entry)
		}
		if len(page) < 100 {
			return comments, nil
		}
		if after == cursor {
			return nil, errors.New("tracker comment pagination did not advance")
		}
	}
}

func (b Backlog) Comment(ctx context.Context, issue string, id int64) (json.RawMessage, error) {
	if id <= 0 {
		return nil, errors.New("provide a positive comment id")
	}
	data, err := b.call(ctx, http.MethodGet, "/issues/"+url.PathEscape(issue)+"/comments/"+strconv.FormatInt(id, 10), nil, nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var comment struct{ ID int64 }
	if err := json.Unmarshal(data, &comment); err != nil || comment.ID != id {
		return nil, errors.New("tracker did not return the requested comment")
	}
	return data, nil
}

func (b Backlog) call(ctx context.Context, method, path string, query, form url.Values, expected int) ([]byte, error) {
	base, err := url.Parse(b.BaseURL)
	if err != nil || base.Scheme != "https" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("tracker endpoint must be an HTTPS URL without credentials or query")
	}
	key := os.Getenv(b.KeyEnv)
	if key == "" {
		return nil, errors.New("tracker credential is unavailable")
	}
	address := strings.TrimRight(b.BaseURL, "/") + path
	if query == nil {
		query = url.Values{}
	}
	query.Set("apiKey", key)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, address+"?"+query.Encode(), body)
	if err != nil {
		return nil, errors.New("cannot construct tracker request")
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
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
		return nil, errors.New(redact(err.Error()))
	}
	defer response.Body.Close()
	const maxResponse = 4 << 20
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponse+1))
	if err != nil {
		return nil, errors.New(redact(err.Error()))
	}
	if len(data) > maxResponse {
		return nil, fmt.Errorf("tracker returned HTTP %d; response exceeds 4 MiB; no truncated response returned", response.StatusCode)
	}
	if response.StatusCode != expected {
		return nil, fmt.Errorf("tracker returned HTTP %d: %s", response.StatusCode, redact(string(data)))
	}
	return data, nil
}
