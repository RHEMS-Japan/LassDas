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
// SetStatus moves the issue to the given status. Which status means what is
// the operator's, configured by id; nothing here reads or names a status.
func (b Backlog) SetStatus(ctx context.Context, issue string, statusID int64) error {
	if statusID <= 0 {
		return errors.New("provide a positive status id")
	}
	form := url.Values{}
	form.Set("statusId", strconv.FormatInt(statusID, 10))
	data, err := b.call(ctx, http.MethodPatch, "/issues/"+url.PathEscape(issue), nil, form, http.StatusOK)
	if err != nil {
		return err
	}
	var updated struct {
		Status struct {
			ID int64 `json:"id"`
		} `json:"status"`
	}
	if err := json.Unmarshal(data, &updated); err != nil || updated.Status.ID != statusID {
		return errors.New("tracker did not confirm the status change")
	}
	return nil
}

// SetCategories replaces the issue's categories with the given ids. The
// caller passes the whole list, so a category the requester set stays.
func (b Backlog) SetCategories(ctx context.Context, issue string, ids []int64) error {
	form := url.Values{}
	for _, id := range ids {
		if id <= 0 {
			return errors.New("provide positive category ids")
		}
		form.Add("categoryId[]", strconv.FormatInt(id, 10))
	}
	if len(ids) == 0 {
		form.Set("categoryId[]", "")
	}
	data, err := b.call(ctx, http.MethodPatch, "/issues/"+url.PathEscape(issue), nil, form, http.StatusOK)
	if err != nil {
		return err
	}
	var updated struct {
		Category []struct {
			ID int64 `json:"id"`
		} `json:"category"`
	}
	if err := json.Unmarshal(data, &updated); err != nil {
		return errors.New("tracker did not confirm the category change")
	}
	for _, id := range ids {
		found := false
		for _, c := range updated.Category {
			found = found || c.ID == id
		}
		if !found {
			return errors.New("tracker did not confirm the category change")
		}
	}
	return nil
}

// Myself returns the id of the account the credential belongs to, so the
// runtime can hand an issue back to itself after the requester's turn.
func (b Backlog) Myself(ctx context.Context) (int64, error) {
	data, err := b.call(ctx, http.MethodGet, "/users/myself", nil, nil, http.StatusOK)
	if err != nil {
		return 0, err
	}
	var me struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(data, &me); err != nil || me.ID <= 0 {
		return 0, errors.New("tracker did not identify the credential's account")
	}
	return me.ID, nil
}

// SetAssignee hands the issue to one account: the requester while a question
// waits for them or the result waits for their check, the runtime's own
// account while it works.
func (b Backlog) SetAssignee(ctx context.Context, issue string, userID int64) error {
	if userID <= 0 {
		return errors.New("provide a positive user id")
	}
	form := url.Values{}
	form.Set("assigneeId", strconv.FormatInt(userID, 10))
	data, err := b.call(ctx, http.MethodPatch, "/issues/"+url.PathEscape(issue), nil, form, http.StatusOK)
	if err != nil {
		return err
	}
	var updated struct {
		Assignee *struct {
			ID int64 `json:"id"`
		} `json:"assignee"`
	}
	if err := json.Unmarshal(data, &updated); err != nil || updated.Assignee == nil || updated.Assignee.ID != userID {
		return errors.New("tracker did not confirm the assignee change")
	}
	return nil
}

// SetActualHours records how long the work took on the issue.
func (b Backlog) SetActualHours(ctx context.Context, issue string, hours float64) error {
	if hours < 0 {
		return errors.New("actual hours must not be negative")
	}
	form := url.Values{}
	form.Set("actualHours", strconv.FormatFloat(hours, 'f', 2, 64))
	if _, err := b.call(ctx, http.MethodPatch, "/issues/"+url.PathEscape(issue), nil, form, http.StatusOK); err != nil {
		return err
	}
	return nil
}

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
