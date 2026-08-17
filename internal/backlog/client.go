package backlog

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
)

var spacePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,62}$`)

type Config struct {
	SpaceKey         string
	Origin           string
	APIKey           string
	Timeout          time.Duration
	MaxResponseBytes int64
}

func (c Config) Validate() error {
	if !spacePattern.MatchString(c.SpaceKey) {
		return errors.New("space key is invalid")
	}
	if c.APIKey == "" || strings.ContainsAny(c.APIKey, "\r\n") {
		return errors.New("backlog api key is invalid")
	}
	origin, err := url.Parse(c.Origin)
	if err != nil || origin.Scheme != "https" || origin.Host == "" {
		return errors.New("backlog origin must be https")
	}
	if origin.User != nil || origin.RawQuery != "" || origin.Fragment != "" || (origin.Path != "" && origin.Path != "/") {
		return errors.New("backlog origin must not contain credentials, path, query, or fragment")
	}
	if origin.Port() != "" {
		return errors.New("backlog origin must not contain a port")
	}
	host := strings.ToLower(origin.Hostname())
	space := strings.ToLower(c.SpaceKey)
	if host != space+".backlog.com" && host != space+".backlog.jp" && host != space+".backlogtool.com" {
		return errors.New("backlog origin does not match the space")
	}
	if c.Timeout <= 0 {
		return errors.New("backlog timeout must be positive")
	}
	if c.MaxResponseBytes <= 0 || c.MaxResponseBytes > 4*1024*1024 {
		return errors.New("backlog response byte limit is invalid")
	}
	return nil
}

type Client struct {
	origin           *url.URL
	apiKey           string
	maxResponseBytes int64
	http             *http.Client
}

func NewClient(config Config, transport http.RoundTripper) (*Client, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	origin, err := url.Parse(config.Origin)
	if err != nil {
		return nil, errors.New("backlog origin is invalid")
	}
	if transport == nil {
		transport = http.DefaultTransport
	}
	return &Client{
		origin:           origin,
		apiKey:           config.APIKey,
		maxResponseBytes: config.MaxResponseBytes,
		http: &http.Client{
			Transport: transport,
			Timeout:   config.Timeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (c *Client) GetActivity(ctx context.Context, activityID int64) (hook.CanonicalActivity, error) {
	if activityID <= 0 {
		return hook.CanonicalActivity{}, hook.NewExternalFailure("backlog", hook.FailureRejected, "invalid_activity_id")
	}
	var payload activityResponse
	if err := c.getJSON(ctx, "/api/v2/activities/"+strconv.FormatInt(activityID, 10), &payload); err != nil {
		return hook.CanonicalActivity{}, err
	}
	createdAt, err := time.Parse(time.RFC3339, payload.Created)
	if err != nil {
		return hook.CanonicalActivity{}, hook.NewExternalFailure("backlog", hook.FailureRejected, "invalid_response")
	}
	keyID := payload.Content.KeyID
	if keyID == 0 {
		keyID = payload.Content.KeyIDCamel
	}
	return hook.CanonicalActivity{
		ID:          payload.ID,
		Type:        payload.Type,
		ProjectID:   payload.Project.ID,
		ProjectKey:  payload.Project.ProjectKey,
		CreatorID:   payload.CreatedUser.ID,
		IssueID:     payload.Content.ID,
		IssueKeyID:  keyID,
		Summary:     payload.Content.Summary,
		Description: payload.Content.Description,
		CreatedAt:   createdAt.UTC(),
	}, nil
}

func (c *Client) GetIssue(ctx context.Context, issueID int64) (hook.CanonicalIssue, error) {
	if issueID <= 0 {
		return hook.CanonicalIssue{}, hook.NewExternalFailure("backlog", hook.FailureRejected, "invalid_issue_id")
	}
	var payload issueResponse
	if err := c.getJSON(ctx, "/api/v2/issues/"+strconv.FormatInt(issueID, 10), &payload); err != nil {
		return hook.CanonicalIssue{}, err
	}
	createdAt, err := time.Parse(time.RFC3339, payload.Created)
	if err != nil {
		return hook.CanonicalIssue{}, hook.NewExternalFailure("backlog", hook.FailureRejected, "invalid_response")
	}
	categoryIDs := make([]int64, 0, len(payload.Category))
	for _, category := range payload.Category {
		categoryIDs = append(categoryIDs, category.ID)
	}
	return hook.CanonicalIssue{
		ID:          payload.ID,
		ProjectID:   payload.ProjectID,
		IssueKey:    payload.IssueKey,
		KeyID:       payload.KeyID,
		CreatorID:   payload.CreatedUser.ID,
		CategoryIDs: categoryIDs,
		CreatedAt:   createdAt.UTC(),
	}, nil
}

func (c *Client) FindExactComment(ctx context.Context, issueID int64, content string) (int64, bool, error) {
	if issueID <= 0 || !validCommentContent(content) {
		return 0, false, hook.NewExternalFailure("backlog", hook.FailureRejected, "invalid_comment_lookup")
	}
	path := "/api/v2/issues/" + strconv.FormatInt(issueID, 10) + "/comments"
	endpoint := *c.origin
	endpoint.Path = path
	query := endpoint.Query()
	query.Set("apiKey", c.apiKey)
	query.Set("count", "100")
	query.Set("order", "desc")
	endpoint.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return 0, false, hook.NewExternalFailure("backlog", hook.FailureRejected, "request_invalid")
	}
	request.Header.Set("Accept", "application/json")
	var comments []commentResponse
	if err := c.doJSON(request, http.StatusOK, &comments); err != nil {
		return 0, false, err
	}
	if len(comments) > 100 {
		return 0, false, hook.NewExternalFailure("backlog", hook.FailureRejected, "invalid_response")
	}
	for _, comment := range comments {
		if comment.ID <= 0 || comment.IssueID != issueID {
			return 0, false, hook.NewExternalFailure("backlog", hook.FailureRejected, "invalid_response")
		}
		if comment.Content == content {
			return comment.ID, true, nil
		}
	}
	return 0, false, nil
}

const (
	commentPageSize = 100
	// maxCommentPages bounds one listing to 1,000 comments after the question.
	// A single-issue M1 thread never approaches this; the bound only protects
	// against a runaway loop on a hostile or broken server.
	maxCommentPages = 10
)

// ListComments returns every comment after minCommentID in ascending comment
// order, with author and server timestamp, following pagination to the end.
// This is the source of truth for answer intake: comment ID, author and
// server time come from this read, never from webhook payloads. When the
// server still reports full pages past the bound, the listing fails closed —
// an incomplete view must never feed an expiry or adoption decision.
func (c *Client) ListComments(ctx context.Context, issueID, minCommentID int64) ([]hook.BacklogComment, error) {
	if issueID <= 0 || minCommentID < 0 {
		return nil, hook.NewExternalFailure("backlog", hook.FailureRejected, "invalid_comment_lookup")
	}
	comments := []hook.BacklogComment{}
	cursor := minCommentID
	for page := 0; page < maxCommentPages; page++ {
		batch, err := c.listCommentPage(ctx, issueID, cursor)
		if err != nil {
			return nil, err
		}
		for _, comment := range batch {
			// Tolerate an inclusive minId without double-counting.
			if comment.ID == cursor {
				continue
			}
			if comment.ID < cursor || comment.IssueID != issueID || comment.CreatedUser.ID <= 0 {
				return nil, hook.NewExternalFailure("backlog", hook.FailureRejected, "invalid_response")
			}
			createdAt, err := time.Parse(time.RFC3339, comment.Created)
			if err != nil {
				return nil, hook.NewExternalFailure("backlog", hook.FailureRejected, "invalid_response")
			}
			cursor = comment.ID
			comments = append(comments, hook.BacklogComment{
				CommentID: comment.ID,
				UserID:    comment.CreatedUser.ID,
				Body:      comment.Content,
				PostedAt:  createdAt.UTC().UnixMilli(),
			})
		}
		if len(batch) < commentPageSize {
			return comments, nil
		}
	}
	return nil, hook.NewExternalFailure("backlog", hook.FailureRetryable, "comment_window_exhausted")
}

func (c *Client) listCommentPage(ctx context.Context, issueID, cursor int64) ([]commentResponse, error) {
	endpoint := *c.origin
	endpoint.Path = "/api/v2/issues/" + strconv.FormatInt(issueID, 10) + "/comments"
	query := endpoint.Query()
	query.Set("apiKey", c.apiKey)
	query.Set("count", strconv.Itoa(commentPageSize))
	query.Set("order", "asc")
	if cursor > 0 {
		query.Set("minId", strconv.FormatInt(cursor, 10))
	}
	endpoint.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, hook.NewExternalFailure("backlog", hook.FailureRejected, "request_invalid")
	}
	request.Header.Set("Accept", "application/json")
	var payload []commentResponse
	if err := c.doJSON(request, http.StatusOK, &payload); err != nil {
		return nil, err
	}
	if len(payload) > commentPageSize {
		return nil, hook.NewExternalFailure("backlog", hook.FailureRejected, "invalid_response")
	}
	return payload, nil
}

// FindCommentByMarker returns the newest comment whose machine marker is the
// given one. The marker must be the comment's final line, so a marker-shaped
// string quoted inside question text or a requester's comment cannot be
// mistaken for an automated posting.
func (c *Client) FindCommentByMarker(ctx context.Context, issueID int64, marker string) (int64, bool, error) {
	if issueID <= 0 || marker == "" || len(marker) > 256 || strings.ContainsAny(marker, "\x00\r\n") {
		return 0, false, hook.NewExternalFailure("backlog", hook.FailureRejected, "invalid_comment_lookup")
	}
	endpoint := *c.origin
	endpoint.Path = "/api/v2/issues/" + strconv.FormatInt(issueID, 10) + "/comments"
	query := endpoint.Query()
	query.Set("apiKey", c.apiKey)
	query.Set("count", "100")
	query.Set("order", "desc")
	endpoint.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return 0, false, hook.NewExternalFailure("backlog", hook.FailureRejected, "request_invalid")
	}
	request.Header.Set("Accept", "application/json")
	var comments []commentResponse
	if err := c.doJSON(request, http.StatusOK, &comments); err != nil {
		return 0, false, err
	}
	if len(comments) > 100 {
		return 0, false, hook.NewExternalFailure("backlog", hook.FailureRejected, "invalid_response")
	}
	for _, comment := range comments {
		if comment.ID <= 0 || comment.IssueID != issueID {
			return 0, false, hook.NewExternalFailure("backlog", hook.FailureRejected, "invalid_response")
		}
		if hook.ExtractCommentMarker(comment.Content) == marker {
			return comment.ID, true, nil
		}
	}
	return 0, false, nil
}

func (c *Client) AddComment(ctx context.Context, issueID int64, content string) (int64, error) {
	return c.AddCommentNotifying(ctx, issueID, content, nil)
}

// AddCommentNotifying posts a comment and sends a Backlog notification to the
// listed users, which is how the question and its reminders actually reach
// the requester instead of sitting unread on the issue.
func (c *Client) AddCommentNotifying(ctx context.Context, issueID int64, content string, notifiedUserIDs []int64) (int64, error) {
	if issueID <= 0 || !validCommentContent(content) {
		return 0, hook.NewExternalFailure("backlog", hook.FailureRejected, "invalid_comment")
	}
	endpoint := *c.origin
	endpoint.Path = "/api/v2/issues/" + strconv.FormatInt(issueID, 10) + "/comments"
	query := endpoint.Query()
	query.Set("apiKey", c.apiKey)
	endpoint.RawQuery = query.Encode()
	form := url.Values{"content": []string{content}}
	for _, userID := range notifiedUserIDs {
		if userID <= 0 {
			return 0, hook.NewExternalFailure("backlog", hook.FailureRejected, "invalid_comment")
		}
		form.Add("notifiedUserId[]", strconv.FormatInt(userID, 10))
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return 0, hook.NewExternalFailure("backlog", hook.FailureRejected, "request_invalid")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var comment commentResponse
	if err := c.doJSON(request, http.StatusCreated, &comment); err != nil {
		return 0, err
	}
	if comment.ID <= 0 || comment.IssueID != issueID || comment.Content != content {
		return 0, hook.NewExternalFailure("backlog", hook.FailureRejected, "invalid_response")
	}
	return comment.ID, nil
}

func (c *Client) getJSON(ctx context.Context, path string, destination any) error {
	endpoint := *c.origin
	endpoint.Path = path
	query := endpoint.Query()
	query.Set("apiKey", c.apiKey)
	endpoint.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return hook.NewExternalFailure("backlog", hook.FailureRejected, "request_invalid")
	}
	request.Header.Set("Accept", "application/json")

	return c.doJSON(request, http.StatusOK, destination)
}

func (c *Client) doJSON(request *http.Request, expectedStatus int, destination any) error {
	response, err := c.http.Do(request)
	if err != nil {
		return hook.NewExternalFailure("backlog", hook.FailureUnknown, "request_failed")
	}
	defer response.Body.Close()
	if response.StatusCode != expectedStatus {
		return backlogStatusFailure(response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, c.maxResponseBytes+1))
	if err != nil {
		return hook.NewExternalFailure("backlog", hook.FailureUnknown, "response_read_failed")
	}
	if int64(len(body)) > c.maxResponseBytes {
		return hook.NewExternalFailure("backlog", hook.FailureRejected, "response_too_large")
	}
	if err := json.Unmarshal(body, destination); err != nil {
		return hook.NewExternalFailure("backlog", hook.FailureRejected, "invalid_response")
	}
	return nil
}

func validCommentContent(content string) bool {
	return content != "" && len([]byte(content)) <= 16*1024 && !strings.ContainsRune(content, '\x00')
}

func backlogStatusFailure(status int) error {
	switch {
	case status == http.StatusTooManyRequests:
		return hook.NewExternalFailure("backlog", hook.FailureRetryable, "rate_limited")
	case status >= 500:
		return hook.NewExternalFailure("backlog", hook.FailureRetryable, "server_error")
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return hook.NewExternalFailure("backlog", hook.FailureRejected, "authentication_failed")
	case status == http.StatusNotFound:
		return hook.NewExternalFailure("backlog", hook.FailureRejected, "not_found")
	default:
		return hook.NewExternalFailure("backlog", hook.FailureRejected, "unexpected_status")
	}
}

type activityResponse struct {
	ID      int64 `json:"id"`
	Type    int   `json:"type"`
	Project struct {
		ID         int64  `json:"id"`
		ProjectKey string `json:"projectKey"`
	} `json:"project"`
	CreatedUser struct {
		ID int64 `json:"id"`
	} `json:"createdUser"`
	Content struct {
		ID          int64  `json:"id"`
		KeyID       int64  `json:"key_id"`
		KeyIDCamel  int64  `json:"keyId"`
		Summary     string `json:"summary"`
		Description string `json:"description"`
	} `json:"content"`
	Created string `json:"created"`
}

type issueResponse struct {
	ID          int64  `json:"id"`
	ProjectID   int64  `json:"projectId"`
	IssueKey    string `json:"issueKey"`
	KeyID       int64  `json:"keyId"`
	CreatedUser struct {
		ID int64 `json:"id"`
	} `json:"createdUser"`
	Category []struct {
		ID int64 `json:"id"`
	} `json:"category"`
	Created string `json:"created"`
}

type commentResponse struct {
	ID          int64  `json:"id"`
	IssueID     int64  `json:"issueId"`
	Content     string `json:"content"`
	CreatedUser struct {
		ID int64 `json:"id"`
	} `json:"createdUser"`
	Created string `json:"created"`
}

// ProjectRecentUpdates lists the project's activities after minActivityID in
// ascending order, mapped to webhook hints. It backs the lost-webhook
// completion: hints found here run through the exact same validated ingest
// path as a live webhook.
func (c *Client) ProjectRecentUpdates(ctx context.Context, projectID, minActivityID int64) ([]hook.WebhookHint, error) {
	if projectID <= 0 || minActivityID < 0 {
		return nil, hook.NewExternalFailure("backlog", hook.FailureRejected, "invalid_activity_lookup")
	}
	endpoint := *c.origin
	endpoint.Path = "/api/v2/projects/" + strconv.FormatInt(projectID, 10) + "/activities"
	query := endpoint.Query()
	query.Set("apiKey", c.apiKey)
	query.Set("count", "20")
	if minActivityID > 0 {
		query.Set("order", "asc")
		query.Set("minId", strconv.FormatInt(minActivityID, 10))
	} else {
		// With no cursor yet the scan must start at the present, not at the
		// project's first ever activity: crawling forward one page per
		// wake-up would take days on a busy project and could never catch up
		// at all, leaving a lost webhook uncompensated exactly when the
		// compensation is needed.
		query.Set("order", "desc")
	}
	endpoint.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, hook.NewExternalFailure("backlog", hook.FailureRejected, "request_invalid")
	}
	request.Header.Set("Accept", "application/json")
	var payload []activityResponse
	if err := c.doJSON(request, http.StatusOK, &payload); err != nil {
		return nil, err
	}
	if len(payload) > 100 {
		return nil, hook.NewExternalFailure("backlog", hook.FailureRejected, "invalid_response")
	}
	hints := make([]hook.WebhookHint, 0, len(payload))
	for _, activity := range payload {
		if activity.ID <= minActivityID {
			continue
		}
		keyID := activity.Content.KeyID
		if keyID == 0 {
			keyID = activity.Content.KeyIDCamel
		}
		hints = append(hints, hook.WebhookHint{
			ActivityID: activity.ID, ActivityType: activity.Type,
			ProjectID: activity.Project.ID, ProjectKey: activity.Project.ProjectKey,
			CreatorID: activity.CreatedUser.ID, IssueID: activity.Content.ID, IssueKeyID: keyID,
		})
	}
	// The caller consumes hints in order and stops advancing its cursor at
	// the first unresolved one, so the listing must always ascend regardless
	// of which order the page was fetched in.
	sort.Slice(hints, func(left, right int) bool { return hints[left].ActivityID < hints[right].ActivityID })
	return hints, nil
}
