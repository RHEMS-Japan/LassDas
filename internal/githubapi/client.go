package githubapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const githubAPIOrigin = "https://api.github.com"

type Client struct {
	config Config
	http   *http.Client

	mu       sync.RWMutex
	verified bool
	sleep    func(context.Context, time.Duration) error
}

func NewClient(config Config, transport http.RoundTripper) (*Client, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	return &Client{
		config: config,
		http:   newHTTPClient(config, transport),
		sleep:  sleepContext,
	}, nil
}

func (c *Client) repositoryPath(suffix string) string {
	return "/repos/" + url.PathEscape(c.config.Owner) + "/" + url.PathEscape(c.config.Repository) + suffix
}

func (c *Client) requireVerified() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.verified {
		return invariant("repository_not_verified")
	}
	return nil
}

func (c *Client) markVerified() {
	c.mu.Lock()
	c.verified = true
	c.mu.Unlock()
}

func (c *Client) get(ctx context.Context, endpoint string, destination any) error {
	return c.do(ctx, http.MethodGet, endpoint, nil, []int{http.StatusOK}, destination)
}

func (c *Client) mutate(ctx context.Context, method, endpoint string, body, destination any, statuses ...int) error {
	if err := c.requireVerified(); err != nil {
		return err
	}
	return c.do(ctx, method, endpoint, body, statuses, destination)
}

func (c *Client) do(ctx context.Context, method, endpoint string, body any, expectedStatuses []int, destination any) error {
	if !strings.HasPrefix(endpoint, "/repos/") || strings.ContainsAny(endpoint, "\r\n") {
		return invariant("invalid_api_endpoint")
	}
	var requestBody io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return invariant("request_encoding_failed")
		}
		requestBody = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, githubAPIOrigin+endpoint, requestBody)
	if err != nil {
		return invariant("request_creation_failed")
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Authorization", "Bearer "+c.config.Token)
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "ticket-automation-controller/1")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := c.http.Do(request)
	if err != nil {
		return &APIError{Code: "request_failed", Method: method, Endpoint: endpoint}
	}
	defer response.Body.Close()
	accepted := false
	for _, status := range expectedStatuses {
		if response.StatusCode == status {
			accepted = true
			break
		}
	}
	if !accepted {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, c.config.MaxResponseBytes))
		return &APIError{Status: response.StatusCode, Code: statusCode(response.StatusCode), Method: method, Endpoint: endpoint}
	}
	if destination == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, c.config.MaxResponseBytes))
		return nil
	}
	limited := io.LimitReader(response.Body, c.config.MaxResponseBytes+1)
	encoded, err := io.ReadAll(limited)
	if err != nil {
		return &APIError{Status: response.StatusCode, Code: "response_read_failed", Method: method, Endpoint: endpoint}
	}
	if int64(len(encoded)) > c.config.MaxResponseBytes {
		return &APIError{Status: response.StatusCode, Code: "response_too_large", Method: method, Endpoint: endpoint}
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	if err := decoder.Decode(destination); err != nil {
		return &APIError{Status: response.StatusCode, Code: "invalid_response", Method: method, Endpoint: endpoint}
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return &APIError{Status: response.StatusCode, Code: "invalid_response", Method: method, Endpoint: endpoint}
	}
	return nil
}

func statusCode(status int) string {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "authentication_failed"
	case status == http.StatusNotFound:
		return "not_found"
	case status == http.StatusConflict:
		return "conflict"
	case status == http.StatusUnprocessableEntity:
		return "unprocessable"
	case status == http.StatusTooManyRequests:
		return "rate_limited"
	case status >= 500:
		return "server_error"
	default:
		return "unexpected_status_" + strconv.Itoa(status)
	}
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func waitContext(parent context.Context, options WaitOptions) (context.Context, context.CancelFunc, error) {
	if err := options.validate(); err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(parent, options.Timeout)
	return ctx, cancel, nil
}

func sleepOrTimeout(ctx context.Context, client *Client, interval time.Duration, timeoutCode string) error {
	if err := client.sleep(ctx, interval); err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return invariant(timeoutCode)
		}
		return err
	}
	return nil
}

func escapePath(value string) string {
	parts := strings.Split(value, "/")
	for index := range parts {
		parts[index] = url.PathEscape(parts[index])
	}
	return strings.Join(parts, "/")
}
