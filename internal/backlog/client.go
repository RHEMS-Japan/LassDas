package backlog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	// selfUserID is the tracker user the API key belongs to, read once and
	// kept: the automation's own comments are told apart from everyone
	// else's by author, and the key's owner cannot change under it.
	selfMu     sync.Mutex
	selfUserID int64
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

// IssueStatusID reads the issue's current status id: what the tracker says
// the ticket is in right now, which the board projection consults before it
// moves a ticket the automation does not own (a ticket a person closed).
func (c *Client) IssueStatusID(ctx context.Context, issueID int64) (int64, error) {
	if issueID <= 0 {
		return 0, hook.NewExternalFailure("backlog", hook.FailureRejected, "invalid_issue_id")
	}
	var payload struct {
		ID     int64 `json:"id"`
		Status struct {
			ID int64 `json:"id"`
		} `json:"status"`
	}
	if err := c.getJSON(ctx, "/api/v2/issues/"+strconv.FormatInt(issueID, 10), &payload); err != nil {
		return 0, err
	}
	if payload.ID != issueID || payload.Status.ID <= 0 {
		return 0, hook.NewExternalFailure("backlog", hook.FailureRejected, "invalid_response")
	}
	return payload.Status.ID, nil
}

// latestComments reads the newest 100 comments of an issue, newest first —
// the window both comment lookups scan.
func (c *Client) latestComments(ctx context.Context, issueID int64) ([]commentResponse, error) {
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
		return nil, hook.NewExternalFailure("backlog", hook.FailureRejected, "request_invalid")
	}
	request.Header.Set("Accept", "application/json")
	var comments []commentResponse
	if err := c.doJSON(request, http.StatusOK, &comments); err != nil {
		return nil, err
	}
	if len(comments) > 100 {
		return nil, hook.NewExternalFailure("backlog", hook.FailureRejected, "invalid_response")
	}
	for _, comment := range comments {
		if comment.ID <= 0 || comment.IssueID != issueID {
			return nil, hook.NewExternalFailure("backlog", hook.FailureRejected, "invalid_response")
		}
	}
	return comments, nil
}

// FindExactComment answers the id of the newest comment whose body equals
// content exactly, among the issue's latest 100 comments.
func (c *Client) FindExactComment(ctx context.Context, issueID int64, content string) (int64, bool, error) {
	if issueID <= 0 || !validCommentContent(content) {
		return 0, false, hook.NewExternalFailure("backlog", hook.FailureRejected, "invalid_comment_lookup")
	}
	comments, err := c.latestComments(ctx, issueID)
	if err != nil {
		return 0, false, err
	}
	for _, comment := range comments {
		if comment.Content == content {
			return comment.ID, true, nil
		}
	}
	return 0, false, nil
}

// FindCommentWithMarker answers the id of the newest comment whose FINAL
// line is marker — the machine tag every automation comment ends with —
// among the issue's latest 100 comments. The final-line anchor is the
// contract's forgery defence (hook.ExtractCommentMarker): a requester's
// quote of the footer ("> [...]") or a paste of it in the body does not
// match. A re-submitted terminal report is found by its marker even when
// the body around it changed (the spend line is read live), so the report
// completes without a second comment (live 2026-09-05).
func (c *Client) FindCommentWithMarker(ctx context.Context, issueID int64, marker string) (int64, bool, error) {
	if issueID <= 0 || !validCommentMarker(marker) {
		return 0, false, hook.NewExternalFailure("backlog", hook.FailureRejected, "invalid_comment_lookup")
	}
	comments, err := c.latestComments(ctx, issueID)
	if err != nil {
		return 0, false, err
	}
	for _, comment := range comments {
		if hook.ExtractCommentMarker(comment.Content) == marker {
			return comment.ID, true, nil
		}
	}
	return 0, false, nil
}

// SelfUserID is the tracker user the API key belongs to: the author of every
// comment this automation has posted. Read once and kept for the process; a
// failed read is reported, never guessed, because callers use it to decide
// whether a comment is the automation's own.
func (c *Client) SelfUserID(ctx context.Context) (int64, error) {
	c.selfMu.Lock()
	known := c.selfUserID
	c.selfMu.Unlock()
	if known > 0 {
		return known, nil
	}
	// Asked outside the lock: one caller's slow or hanging request must not
	// hold every other caller for its whole timeout. Two callers may ask at
	// once; both get the same answer and the second write is the same value.
	var payload struct {
		ID int64 `json:"id"`
	}
	if err := c.getJSON(ctx, "/api/v2/users/myself", &payload); err != nil {
		return 0, err
	}
	if payload.ID <= 0 {
		return 0, hook.NewExternalFailure("backlog", hook.FailureRejected, "invalid_response")
	}
	c.selfMu.Lock()
	c.selfUserID = payload.ID
	c.selfMu.Unlock()
	return payload.ID, nil
}

// FindCommentWithMarkerPrefix answers the marker of the newest comment the
// automation itself posted, among the issue's latest 100, whose final-line
// marker starts with prefix. The prefix must itself be the opening of a
// marker and end with a separator, so one run's prefix cannot match a longer
// run id.
//
// Author, not only shape, decides. The end-of-line anchor keeps a marker
// quoted inside our own comment from counting, but a whole comment a person
// writes can still end with a marker-shaped line, and treating that as the
// automation's own report would silence the ticket for good: no queue, no
// comment, nobody told.
//
// The cost is that a report posted under a different tracker account - a
// deployment whose key belongs to another bot user - is not recognised, and
// that ticket is worked again. That direction is the safe one: a second pull
// request is visible and closable, a silenced ticket is neither.
func (c *Client) FindCommentWithMarkerPrefix(ctx context.Context, issueID int64, prefix string) (string, bool, error) {
	if issueID <= 0 || !validCommentMarkerPrefix(prefix) {
		return "", false, hook.NewExternalFailure("backlog", hook.FailureRejected, "invalid_comment_lookup")
	}
	self, err := c.SelfUserID(ctx)
	if err != nil {
		return "", false, err
	}
	comments, err := c.latestComments(ctx, issueID)
	if err != nil {
		return "", false, err
	}
	for _, comment := range comments {
		if comment.CreatedUser.ID != self {
			continue
		}
		if marker := hook.ExtractCommentMarker(comment.Content); marker != "" && strings.HasPrefix(marker, prefix) {
			return marker, true, nil
		}
	}
	return "", false, nil
}

// validCommentMarkerPrefix accepts the opening of one kind of marker for one
// run: the automation's own marker prefix, then printable ASCII without
// whitespace, ending at a separator so a prefix cannot match a longer run id.
func validCommentMarkerPrefix(prefix string) bool {
	if len(prefix) > 256 || !strings.HasPrefix(prefix, "["+hook.CommentMarkerPrefix+":") || prefix[len(prefix)-1] != ':' {
		return false
	}
	for _, r := range prefix {
		if r <= ' ' || r > '~' {
			return false
		}
	}
	// The opening alone would match every kind of marker for every run. A
	// usable prefix names at least the kind and the run it belongs to, and
	// every segment it names must be a real one.
	segments := strings.Split(strings.TrimSuffix(strings.TrimPrefix(prefix, "["+hook.CommentMarkerPrefix+":"), ":"), ":")
	if len(segments) < 2 {
		return false
	}
	for _, segment := range segments {
		if segment == "" {
			return false
		}
	}
	return true
}

// validCommentMarker accepts the bracketed one-line tag the automation
// writes: printable ASCII, no whitespace, bounded.
func validCommentMarker(marker string) bool {
	if len(marker) < 8 || len(marker) > 256 || marker[0] != '[' || marker[len(marker)-1] != ']' {
		return false
	}
	for _, r := range marker {
		if r <= ' ' || r > '~' {
			return false
		}
	}
	return true
}

const (
	commentPageSize = 100
	// maxCommentPages bounds one listing to 1,000 comments after whatever it
	// was asked to start from. Every caller that must see everything starts
	// from a recent point - the open question, or the position it last read
	// to - so the bound is never the answer window; a caller that reads a
	// whole ticket is asking for something extra and is told when the
	// ticket is too long to give it.
	maxCommentPages = 10
)

// ListComments returns every comment after minCommentID in ascending comment
// order, with author and server timestamp, following pagination to the end.
// This is the source of truth for answer intake: comment ID, author and
// server time come from this read, never from webhook payloads. When the
// server still reports full pages past the bound, the listing fails closed —
// an incomplete view must never feed an adoption decision. It is a
// retryable failure and it names itself, so a caller that asked to read
// more of the ticket than fits can ask for less instead of giving up on
// the run.
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

// maxAttachmentBytes bounds one uploaded file. Screenshots are the only
// caller today and a full-page PNG stays well under this.
const maxAttachmentBytes = 8 << 20

var attachmentFilenamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

// UploadAttachment stages one file on the space (POST /api/v2/space/attachment)
// and returns the attachment id a subsequent comment can bind.
func (c *Client) UploadAttachment(ctx context.Context, filename string, content []byte) (int64, error) {
	if !attachmentFilenamePattern.MatchString(filename) || len(content) == 0 || len(content) > maxAttachmentBytes {
		return 0, hook.NewExternalFailure("backlog", hook.FailureRejected, "invalid_attachment")
	}
	endpoint := *c.origin
	endpoint.Path = "/api/v2/space/attachment"
	query := endpoint.Query()
	query.Set("apiKey", c.apiKey)
	endpoint.RawQuery = query.Encode()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("file", filename)
	if err != nil {
		return 0, hook.NewExternalFailure("backlog", hook.FailureRejected, "request_invalid")
	}
	if _, err := part.Write(content); err != nil {
		return 0, hook.NewExternalFailure("backlog", hook.FailureRejected, "request_invalid")
	}
	if err := form.Close(); err != nil {
		return 0, hook.NewExternalFailure("backlog", hook.FailureRejected, "request_invalid")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), &body)
	if err != nil {
		return 0, hook.NewExternalFailure("backlog", hook.FailureRejected, "request_invalid")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", form.FormDataContentType())
	var uploaded struct {
		ID int64 `json:"id"`
	}
	if err := c.doJSON(request, http.StatusOK, &uploaded); err != nil {
		return 0, err
	}
	if uploaded.ID <= 0 {
		return 0, hook.NewExternalFailure("backlog", hook.FailureRejected, "invalid_response")
	}
	return uploaded.ID, nil
}

// AddCommentNotifying posts a comment and sends a Backlog notification to the
// listed users, which is how the question and its reminders actually reach
// the requester instead of sitting unread on the issue.
func (c *Client) AddCommentNotifying(ctx context.Context, issueID int64, content string, notifiedUserIDs []int64) (int64, error) {
	return c.AddCommentNotifyingWithAttachments(ctx, issueID, content, notifiedUserIDs, nil)
}

// AddCommentNotifyingWithAttachments additionally binds previously uploaded
// attachments (UploadAttachment) to the comment.
func (c *Client) AddCommentNotifyingWithAttachments(ctx context.Context, issueID int64, content string, notifiedUserIDs, attachmentIDs []int64) (int64, error) {
	if issueID <= 0 || !validCommentContent(content) || len(attachmentIDs) > 10 {
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
	for _, attachmentID := range attachmentIDs {
		if attachmentID <= 0 {
			return 0, hook.NewExternalFailure("backlog", hook.FailureRejected, "invalid_comment")
		}
		form.Add("attachmentId[]", strconv.FormatInt(attachmentID, 10))
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

// validCommentContent holds a comment body to the tracker's own size limit
// before the request leaves this process: the API rejects a longer body, and
// a rejection here is a comment the requester never sees. The number is
// hook.MaxTrackerCommentBytes so the composers that have to fit inside it and
// the client that enforces it cannot drift apart.
func validCommentContent(content string) bool {
	return content != "" && len([]byte(content)) <= hook.MaxTrackerCommentBytes && !strings.ContainsRune(content, '\x00')
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
