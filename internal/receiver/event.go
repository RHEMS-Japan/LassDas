package receiver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
)

const (
	MaxResponseBytes = hook.MaxDeliveredEnvelopeBytes
	maxPullAttempts  = 3
)

var functionHostPattern = regexp.MustCompile(`^[a-z0-9]+\.lambda-url\.ap-northeast-1\.on\.aws$`)

type Config struct {
	ClaimURL            string
	HMACKey             []byte
	Request             hook.PullRequest
	SpaceKey            string
	ProjectID           int64
	ProjectKey          string
	AllowedCreatorID    int64
	AllowedActivityType int
	Target              hook.DeliveryTarget
	Timeout             time.Duration
}

func LoadConfig(getenv func(string) string) (Config, error) {
	if getenv == nil {
		return Config{}, errors.New("receiver configuration is invalid")
	}
	repository := getenv("GITHUB_REPOSITORY")
	workflowRef := getenv("GITHUB_WORKFLOW_REF")
	if repository == "" || workflowRef == "" || strings.TrimSpace(repository) != repository || strings.TrimSpace(workflowRef) != workflowRef ||
		strings.ContainsAny(repository+workflowRef, "\r\n") {
		return Config{}, errors.New("receiver configuration is invalid")
	}
	repositoryID, err := positiveInt64(getenv("GITHUB_REPOSITORY_ID"))
	if err != nil {
		return Config{}, errors.New("receiver configuration is invalid")
	}
	workflowRunID, err := positiveInt64(getenv("GITHUB_RUN_ID"))
	if err != nil {
		return Config{}, errors.New("receiver configuration is invalid")
	}
	runAttempt, err := positiveInt(getenv("GITHUB_RUN_ATTEMPT"))
	if err != nil {
		return Config{}, errors.New("receiver configuration is invalid")
	}
	projectID, err := positiveInt64(getenv("EXPECTED_PROJECT_ID"))
	if err != nil {
		return Config{}, errors.New("receiver configuration is invalid")
	}
	creatorID, err := positiveInt64(getenv("EXPECTED_CREATOR_ID"))
	if err != nil {
		return Config{}, errors.New("receiver configuration is invalid")
	}
	activityType, err := positiveInt(getenv("EXPECTED_ACTIVITY_TYPE"))
	if err != nil {
		return Config{}, errors.New("receiver configuration is invalid")
	}
	key, err := hook.DecodePullHMACKey(getenv("PULL_HMAC_KEY"))
	if err != nil {
		return Config{}, errors.New("receiver configuration is invalid")
	}
	workflowRefDigest := hook.HashIdentity(workflowRef)
	config := Config{
		ClaimURL: getenv("PULL_CLAIM_URL"), HMACKey: key,
		Request: hook.PullRequest{
			Protocol: hook.PullProtocolVersion, RepositoryID: repositoryID,
			RepositorySHA256: hook.HashIdentity(repository), EventName: getenv("GITHUB_EVENT_NAME"),
			WorkflowRefSHA256: workflowRefDigest, Ref: getenv("GITHUB_REF"), WorkflowSHA: getenv("GITHUB_SHA"),
			WorkflowRunID: workflowRunID, RunAttempt: runAttempt, AutomationRunID: getenv("AUTOMATION_RUN_ID"),
		},
		SpaceKey: getenv("EXPECTED_SPACE_KEY"), ProjectID: projectID, ProjectKey: getenv("EXPECTED_PROJECT_KEY"),
		AllowedCreatorID: creatorID, AllowedActivityType: activityType,
		Target:  hook.DeliveryTarget{RepositoryID: repositoryID, WorkflowRefSHA256: workflowRefDigest},
		Timeout: 15 * time.Second,
	}
	if err := config.Validate(); err != nil {
		return Config{}, errors.New("receiver configuration is invalid")
	}
	return config, nil
}

func (c Config) Validate() error {
	if err := validateClaimURL(c.ClaimURL); err != nil {
		return err
	}
	if err := hook.ValidatePullKey(c.HMACKey); err != nil {
		return err
	}
	request := c.Request
	request.IssuedAt = time.Unix(1, 0).UTC()
	if _, err := hook.MarshalPullRequest(request); err != nil {
		return err
	}
	if c.SpaceKey == "" || c.ProjectID <= 0 || c.ProjectKey == "" || c.AllowedCreatorID <= 0 || c.AllowedActivityType <= 0 {
		return errors.New("receiver allowlist is invalid")
	}
	if c.Target.RepositoryID != c.Request.RepositoryID || c.Target.WorkflowRefSHA256 != c.Request.WorkflowRefSHA256 || c.Target.Validate() != nil {
		return errors.New("receiver target is invalid")
	}
	if c.Timeout <= 0 || c.Timeout > time.Minute {
		return errors.New("receiver timeout is invalid")
	}
	return nil
}

type Result struct {
	NoWork   bool
	Envelope hook.DispatchEnvelope
	Receipt  Receipt
}

type Receipt struct {
	DeliveryID  string `json:"delivery_id"`
	IssueKey    string `json:"issue_key"`
	InputSHA256 string `json:"input_sha256"`
}

type ValidationError struct {
	Code string
}

func (e *ValidationError) Error() string { return e.Code }

type Client struct {
	config Config
	http   *http.Client
}

func NewClient(config Config, transport http.RoundTripper) (*Client, error) {
	if err := config.Validate(); err != nil {
		return nil, errors.New("receiver configuration is invalid")
	}
	if transport == nil {
		transport = http.DefaultTransport
	}
	config.HMACKey = append([]byte(nil), config.HMACKey...)
	return &Client{config: config, http: &http.Client{
		Timeout: config.Timeout, Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect not allowed") },
	}}, nil
}

func (c *Client) Pull(ctx context.Context, now time.Time) (Result, error) {
	requestValue := c.config.Request
	requestValue.IssuedAt = now.UTC()
	body, err := hook.MarshalPullRequest(requestValue)
	if err != nil {
		return Result{}, &ValidationError{Code: "request_invalid"}
	}
	for attempt := 0; attempt < maxPullAttempts; attempt++ {
		result, retryable, err := c.pullOnce(ctx, body)
		if err == nil || !retryable || attempt == maxPullAttempts-1 {
			return result, err
		}
	}
	return Result{}, &ValidationError{Code: "pull_unavailable"}
}

func (c *Client) pullOnce(ctx context.Context, body []byte) (Result, bool, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.ClaimURL, bytes.NewReader(body))
	if err != nil {
		return Result{}, false, &ValidationError{Code: "request_invalid"}
	}
	request.Header.Set("accept", "application/json")
	request.Header.Set("content-type", "application/json")
	request.Header.Set(hook.PullSignatureHeader, hook.SignPullRequest(c.config.HMACKey, body))
	response, err := c.http.Do(request)
	if err != nil {
		if response != nil && response.StatusCode >= 300 && response.StatusCode < 400 {
			return Result{}, false, &ValidationError{Code: "pull_unavailable"}
		}
		return Result{}, true, &ValidationError{Code: "pull_unavailable"}
	}
	if response == nil || response.Body == nil {
		return Result{}, true, &ValidationError{Code: "pull_unavailable"}
	}
	encoded, err := io.ReadAll(io.LimitReader(response.Body, MaxResponseBytes+1))
	closeErr := response.Body.Close()
	if err != nil || len(encoded) > MaxResponseBytes {
		return Result{}, true, &ValidationError{Code: "response_invalid"}
	}
	if closeErr != nil {
		return Result{}, true, &ValidationError{Code: "response_invalid"}
	}
	if !hook.VerifyPullResponseSignature(c.config.HMACKey, response.StatusCode, body, encoded, response.Header.Get(hook.PullResponseSignatureHeader)) {
		return Result{}, true, &ValidationError{Code: "response_signature_invalid"}
	}
	if response.StatusCode == http.StatusNoContent {
		if len(encoded) != 0 {
			return Result{}, true, &ValidationError{Code: "response_invalid"}
		}
		return Result{NoWork: true}, false, nil
	}
	if response.StatusCode != http.StatusOK {
		if retryableSignedPullResult(response.StatusCode, encoded) {
			return Result{}, true, &ValidationError{Code: "pull_unavailable"}
		}
		return Result{}, false, &ValidationError{Code: "claim_rejected"}
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("content-type"))
	if err != nil || mediaType != "application/json" {
		return Result{}, true, &ValidationError{Code: "response_invalid"}
	}
	envelope, err := decodeEnvelope(encoded)
	if err != nil || !c.snapshotAllowed(envelope.Snapshot) {
		return Result{}, true, &ValidationError{Code: "envelope_invalid"}
	}
	return Result{Envelope: envelope, Receipt: Receipt{
		DeliveryID: envelope.DeliveryID, IssueKey: envelope.Snapshot.IssueKey, InputSHA256: envelope.Snapshot.InputSHA256,
	}}, false, nil
}

func retryableSignedPullResult(status int, encoded []byte) bool {
	if status != http.StatusServiceUnavailable && status != http.StatusInternalServerError {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var result hook.Result
	if err := decoder.Decode(&result); err != nil {
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return false
	}
	canonical, err := json.Marshal(result)
	if err != nil || !bytes.Equal(canonical, encoded) || result.Decision != hook.DecisionInvalid || result.DeliveryID != "" {
		return false
	}
	return status == http.StatusServiceUnavailable && result.Code == "pull_unavailable" ||
		status == http.StatusInternalServerError && result.Code == "pull_response_failed"
}

func (c *Client) snapshotAllowed(snapshot hook.TicketSnapshot) bool {
	// The run identity is the ticket itself. The AutomationRunID this client
	// sends names the polling route, never the ticket it expects back.
	return snapshot.SpaceKey == c.config.SpaceKey && snapshot.ProjectID == c.config.ProjectID &&
		snapshot.ProjectKey == c.config.ProjectKey && snapshot.CreatorID == c.config.AllowedCreatorID &&
		snapshot.ActivityType == c.config.AllowedActivityType && snapshot.RunID == snapshot.IssueKey &&
		snapshot.Target == c.config.Target
}

func decodeEnvelope(encoded []byte) (hook.DispatchEnvelope, error) {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var envelope hook.DispatchEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return hook.DispatchEnvelope{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return hook.DispatchEnvelope{}, errors.New("multiple envelope values")
	}
	canonical, err := json.Marshal(envelope)
	if err != nil || !bytes.Equal(canonical, encoded) || hook.ValidateEnvelope(envelope) != nil {
		return hook.DispatchEnvelope{}, errors.New("envelope is invalid")
	}
	return envelope, nil
}

func validateClaimURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Host == "" || parsed.Port() != "" ||
		!functionHostPattern.MatchString(parsed.Hostname()) || parsed.Path != hook.PullClaimPath || parsed.RawPath != "" ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("claim url is invalid")
	}
	return nil
}

func positiveInt64(value string) (int64, error) {
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, errors.New("value must be positive")
	}
	return parsed, nil
}

func positiveInt(value string) (int, error) {
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return 0, errors.New("value must be positive")
	}
	return parsed, nil
}
