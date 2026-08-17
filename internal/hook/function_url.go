package hook

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"strings"
	"time"
)

const backlogPath = "/backlog"

type PullRouteConfig struct {
	HMACKey             []byte
	RepositoryID        int64
	RepositorySHA256    string
	WorkflowRefSHA256   string
	SpaceKey            string
	ProjectID           int64
	ProjectKey          string
	AllowedCreatorID    int64
	AllowedActivityType int
	Target              DeliveryTarget
	ClockSkew           time.Duration
}

func (c PullRouteConfig) Validate() error {
	if err := ValidatePullKey(c.HMACKey); err != nil {
		return errors.New("pull authentication is invalid")
	}
	if c.RepositoryID <= 0 || !validIdentityDigest(c.RepositorySHA256) || !validIdentityDigest(c.WorkflowRefSHA256) {
		return errors.New("pull repository is invalid")
	}
	if c.Target.RepositoryID != c.RepositoryID || c.Target.WorkflowRefSHA256 != c.WorkflowRefSHA256 || c.Target.Validate() != nil {
		return errors.New("pull target is invalid")
	}
	if !componentPattern.MatchString(c.SpaceKey) || c.ProjectID <= 0 || !componentPattern.MatchString(c.ProjectKey) ||
		c.AllowedCreatorID <= 0 || c.AllowedActivityType <= 0 {
		return errors.New("pull allowlist is invalid")
	}
	if c.ClockSkew <= 0 || c.ClockSkew > MaxPullClockSkew {
		return errors.New("pull clock skew is invalid")
	}
	return nil
}

type FunctionURLConfig struct {
	BasicUsername string
	BasicPassword string
	MaxBodyBytes  int
	Pull          PullRouteConfig
	Report        ReportRouteConfig
}

func (c FunctionURLConfig) Validate() error {
	if c.BasicUsername == "" || c.BasicPassword == "" {
		return errors.New("basic authentication is required")
	}
	if strings.ContainsAny(c.BasicUsername, ":\r\n") || strings.ContainsAny(c.BasicPassword, "\r\n") {
		return errors.New("basic credentials are invalid")
	}
	if c.MaxBodyBytes <= 0 || c.MaxBodyBytes > 1024*1024 {
		return errors.New("body byte limit must be between 1 and 1048576")
	}
	if err := c.Pull.Validate(); err != nil {
		return err
	}
	if !c.Report.isZero() {
		return c.Report.Validate()
	}
	return nil
}

type HookProcessor interface {
	Process(context.Context, WebhookHint) Result
}

type PullProcessor interface {
	Pull(context.Context, PullClaimRequest) (DispatchEnvelope, PullDisposition, error)
}

// FunctionURLRequest contains only the AWS Lambda Function URL fields used by
// the two fixed routes. lambda.Start decodes the AWS event into this type.
type FunctionURLRequest struct {
	Body            string            `json:"body"`
	Headers         map[string]string `json:"headers"`
	IsBase64Encoded bool              `json:"isBase64Encoded"`
	RawPath         string            `json:"rawPath"`
	RawQueryString  string            `json:"rawQueryString"`
	RequestContext  RequestContext    `json:"requestContext"`
}

type RequestContext struct {
	HTTP RequestContextHTTP `json:"http"`
}

type RequestContextHTTP struct {
	Method   string `json:"method"`
	SourceIP string `json:"sourceIp"`
}

type FunctionURLResponse struct {
	StatusCode      int               `json:"statusCode"`
	Headers         map[string]string `json:"headers,omitempty"`
	Body            string            `json:"body"`
	IsBase64Encoded bool              `json:"isBase64Encoded"`
}

type FunctionURLHandler struct {
	processor    HookProcessor
	puller       PullProcessor
	reporter     TerminalReportProcessor
	questioner   QuestionReportProcessor
	ticker       QuestionTickProcessor
	usernameHash [sha256.Size]byte
	passwordHash [sha256.Size]byte
	maxBodyBytes int
	pull         PullRouteConfig
	report       ReportRouteConfig
	now          func() time.Time
}

func NewFunctionURLHandler(config FunctionURLConfig, processor HookProcessor, puller PullProcessor) (*FunctionURLHandler, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if processor == nil || puller == nil {
		return nil, errors.New("function processors must not be nil")
	}
	pull := config.Pull
	pull.HMACKey = append([]byte(nil), config.Pull.HMACKey...)
	return &FunctionURLHandler{
		processor: processor, puller: puller,
		usernameHash: sha256.Sum256([]byte(config.BasicUsername)),
		passwordHash: sha256.Sum256([]byte(config.BasicPassword)),
		maxBodyBytes: config.MaxBodyBytes, pull: pull, now: time.Now,
	}, nil
}

func NewFunctionURLHandlerWithReporter(config FunctionURLConfig, processor HookProcessor, puller PullProcessor, reporter TerminalReportProcessor) (*FunctionURLHandler, error) {
	if config.Report.isZero() || config.Report.Validate() != nil {
		return nil, errors.New("terminal report configuration is invalid")
	}
	if reporter == nil {
		return nil, errors.New("terminal report processor must not be nil")
	}
	handler, err := NewFunctionURLHandler(config, processor, puller)
	if err != nil {
		return nil, err
	}
	handler.reporter = reporter
	handler.report = config.Report
	handler.report.HMACKey = append([]byte(nil), config.Report.HMACKey...)
	return handler, nil
}

// NewFunctionURLHandlerWithQuestions additionally routes the clarification
// question submission and the scheduled question tick.
func NewFunctionURLHandlerWithQuestions(config FunctionURLConfig, processor HookProcessor, puller PullProcessor, reporter TerminalReportProcessor, questioner QuestionReportProcessor, ticker QuestionTickProcessor) (*FunctionURLHandler, error) {
	if questioner == nil || ticker == nil {
		return nil, errors.New("question processors must not be nil")
	}
	handler, err := NewFunctionURLHandlerWithReporter(config, processor, puller, reporter)
	if err != nil {
		return nil, err
	}
	handler.questioner = questioner
	handler.ticker = ticker
	return handler, nil
}

func (h *FunctionURLHandler) Handle(ctx context.Context, request FunctionURLRequest) (FunctionURLResponse, error) {
	switch request.RawPath {
	case backlogPath:
		return h.handleBacklog(ctx, request), nil
	case PullClaimPath:
		return h.handlePull(ctx, request), nil
	case TerminalReportPath:
		if h.reporter == nil {
			return resultResponse(404, Result{Decision: DecisionInvalid, Code: "route_not_found"}), nil
		}
		return h.handleTerminalReport(ctx, request), nil
	case QuestionReportPath:
		if h.questioner == nil {
			return resultResponse(404, Result{Decision: DecisionInvalid, Code: "route_not_found"}), nil
		}
		return h.handleQuestionReport(ctx, request), nil
	case QuestionTickPath:
		if h.ticker == nil {
			return resultResponse(404, Result{Decision: DecisionInvalid, Code: "route_not_found"}), nil
		}
		return h.handleQuestionTick(ctx, request), nil
	default:
		return resultResponse(404, Result{Decision: DecisionInvalid, Code: "route_not_found"}), nil
	}
}

func (h *FunctionURLHandler) handleQuestionReport(ctx context.Context, request FunctionURLRequest) FunctionURLResponse {
	body := []byte(request.Body)
	if !VerifyQuestionReportRequestSignature(h.report.HMACKey, body, header(request.Headers, QuestionReportSignatureHeader)) {
		return h.signedQuestionResponse(401, body, Result{Decision: DecisionInvalid, Code: "unauthorized"})
	}
	if status, code := h.validateJSONTransport(request, MaxQuestionReportRequestBytes); status != 0 {
		return h.signedQuestionResponse(status, body, Result{Decision: DecisionInvalid, Code: code})
	}
	report, err := DecodeQuestionReportRequest(body)
	if err != nil {
		return h.signedQuestionResponse(400, body, Result{Decision: DecisionInvalid, Code: "question_report_invalid"})
	}
	// Same rebinding as the terminal route: the run is named by the ticket.
	route := h.report
	route.ExpectedRunID = report.Record.AutomationRunID
	if err := report.Record.ValidateRoute(route); err != nil {
		return h.signedQuestionResponse(403, body, Result{Decision: DecisionInvalid, Code: "question_report_not_allowed"})
	}
	now := h.now().UTC()
	if report.IssuedAt.Before(now.Add(-h.report.ClockSkew)) || report.IssuedAt.After(now.Add(h.report.ClockSkew)) {
		return h.signedQuestionResponse(403, body, Result{Decision: DecisionInvalid, Code: "question_report_timestamp_not_allowed"})
	}
	result := h.questioner.ProcessQuestionReport(ctx, report)
	return h.signedQuestionResponse(statusForQuestionResult(result), body, result)
}

func (h *FunctionURLHandler) handleQuestionTick(ctx context.Context, request FunctionURLRequest) FunctionURLResponse {
	body := []byte(request.Body)
	if !VerifyQuestionTickRequestSignature(h.report.HMACKey, body, header(request.Headers, QuestionTickSignatureHeader)) {
		return h.signedTickResponse(401, body, Result{Decision: DecisionInvalid, Code: "unauthorized"})
	}
	if status, code := h.validateJSONTransport(request, maxQuestionTickBodyBytes); status != 0 {
		return h.signedTickResponse(status, body, Result{Decision: DecisionInvalid, Code: code})
	}
	tick, err := DecodeQuestionTickRequest(body)
	if err != nil {
		return h.signedTickResponse(400, body, Result{Decision: DecisionInvalid, Code: "question_tick_invalid"})
	}
	result := h.ticker.ProcessQuestionTick(ctx, tick)
	return h.signedTickResponse(statusForQuestionResult(result), body, result)
}

func (h *FunctionURLHandler) handleTerminalReport(ctx context.Context, request FunctionURLRequest) FunctionURLResponse {
	body := []byte(request.Body)
	if !VerifyTerminalReportRequestSignature(h.report.HMACKey, body, header(request.Headers, TerminalReportSignatureHeader)) {
		return h.signedTerminalResult(401, "unauthorized", body)
	}
	if status, code := h.validateJSONTransport(request, MaxTerminalReportRequestBytes); status != 0 {
		return h.signedTerminalResult(status, code, body)
	}
	report, err := DecodeTerminalReportRequest(body)
	if err != nil {
		return h.signedTerminalResult(400, "terminal_report_invalid", body)
	}
	// The run is named by the ticket, so the route is checked against the run
	// this report is about — the same rebinding the service performs. The
	// store still refuses a report whose envelope, delivery and claim owner do
	// not match that run, so naming another one gains nothing.
	route := h.report
	route.ExpectedRunID = report.AutomationRunID
	if err := report.ValidateRoute(route); err != nil {
		return h.signedTerminalResult(403, "terminal_report_not_allowed", body)
	}
	now := h.now().UTC()
	if report.IssuedAt.Before(now.Add(-h.report.ClockSkew)) || report.IssuedAt.After(now.Add(h.report.ClockSkew)) {
		return h.signedTerminalResult(403, "terminal_report_timestamp_not_allowed", body)
	}
	result := h.reporter.ProcessTerminalReport(ctx, report)
	return h.signedTerminalResponse(statusForTerminalResult(result), body, result)
}

func (h *FunctionURLHandler) handleBacklog(ctx context.Context, request FunctionURLRequest) FunctionURLResponse {
	if !h.authenticated(header(request.Headers, "authorization")) {
		response := resultResponse(401, Result{Decision: DecisionInvalid, Code: "unauthorized"})
		response.Headers["www-authenticate"] = `Basic realm="backlog-hook"`
		return response
	}
	if status, code := h.validateJSONTransport(request, h.maxBodyBytes); status != 0 {
		return resultResponse(status, Result{Decision: DecisionInvalid, Code: code})
	}
	var payload backlogWebhookPayload
	decoder := json.NewDecoder(strings.NewReader(request.Body))
	if err := decoder.Decode(&payload); err != nil {
		return resultResponse(400, Result{Decision: DecisionInvalid, Code: "invalid_json"})
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return resultResponse(400, Result{Decision: DecisionInvalid, Code: "multiple_json_values"})
	}
	hint := WebhookHint{
		ActivityID: payload.ID, ActivityType: payload.Type,
		ProjectID: payload.Project.ID, ProjectKey: payload.Project.ProjectKey,
		CreatorID: payload.CreatedUser.ID, IssueID: payload.Content.ID, IssueKeyID: payload.Content.KeyID,
	}
	if err := hint.ValidateShape(); err != nil {
		return resultResponse(400, Result{Decision: DecisionInvalid, Code: "invalid_webhook_shape"})
	}
	result := h.processor.Process(ctx, hint)
	return resultResponse(statusForResult(result), result)
}

func (h *FunctionURLHandler) handlePull(ctx context.Context, request FunctionURLRequest) FunctionURLResponse {
	body := []byte(request.Body)
	if !VerifyPullRequestSignature(h.pull.HMACKey, body, header(request.Headers, PullSignatureHeader)) {
		return resultResponse(401, Result{Decision: DecisionInvalid, Code: "unauthorized"})
	}
	if status, code := h.validateJSONTransport(request, MaxPullRequestBytes); status != 0 {
		return h.signedPullResult(status, code, body)
	}
	pullRequest, err := DecodePullRequest(body)
	if err != nil {
		return h.signedPullResult(400, "pull_request_invalid", body)
	}
	// The caller proves which workflow it is, not which ticket it wants. Asking
	// it to name a ticket meant a single value had to be configured on both
	// sides, and that value became the record key: one ticket, ever.
	if pullRequest.RepositoryID != h.pull.RepositoryID || pullRequest.RepositorySHA256 != h.pull.RepositorySHA256 ||
		pullRequest.WorkflowRefSHA256 != h.pull.WorkflowRefSHA256 {
		return h.signedPullResult(403, "pull_route_not_allowed", body)
	}
	now := h.now().UTC()
	if pullRequest.IssuedAt.Before(now.Add(-h.pull.ClockSkew)) || pullRequest.IssuedAt.After(now.Add(h.pull.ClockSkew)) {
		return h.signedPullResult(403, "pull_timestamp_not_allowed", body)
	}
	envelope, disposition, err := h.puller.Pull(ctx, PullClaimRequest{
		SpaceKey: h.pull.SpaceKey, ProjectID: h.pull.ProjectID, ProjectKey: h.pull.ProjectKey,
		AllowedCreatorID: h.pull.AllowedCreatorID, AllowedActivityType: h.pull.AllowedActivityType,
		Target: h.pull.Target,
		Owner: PullOwner{
			RepositoryID: pullRequest.RepositoryID, RepositorySHA256: pullRequest.RepositorySHA256,
			WorkflowRefSHA256: pullRequest.WorkflowRefSHA256, WorkflowSHA: pullRequest.WorkflowSHA,
			WorkflowRunID: pullRequest.WorkflowRunID, RunAttempt: pullRequest.RunAttempt,
		},
		IssuedAt: pullRequest.IssuedAt, ClaimedAt: now, ClockSkew: h.pull.ClockSkew,
	})
	if err != nil {
		class, _ := FailureDetails(err)
		if class == FailureRetryable {
			return h.signedPullResult(503, "pull_unavailable", body)
		}
		return h.signedPullResult(409, "pull_rejected", body)
	}
	switch disposition {
	case PullEmpty, PullClaimed:
		return h.signedPullResponse(204, body, nil, "")
	case PullConflict:
		return h.signedPullResult(409, "pull_conflict", body)
	case PullAcquired:
		if ValidateEnvelope(envelope) != nil || !snapshotMatchesRoute(envelope.Snapshot, h.pull) {
			return h.signedPullResult(500, "pull_envelope_invalid", body)
		}
		encoded, marshalErr := json.Marshal(envelope)
		// The delivered envelope may carry the sealed clarification of a
		// resumed run, so its bound is the shared delivery bound, not the
		// ingress body cap: a validly sealed resume must always be
		// deliverable, or the claimed run wedges silently.
		if marshalErr != nil || len(encoded) > MaxDeliveredEnvelopeBytes {
			return h.signedPullResult(500, "pull_response_failed", body)
		}
		return h.signedPullResponse(200, body, encoded, "application/json")
	default:
		return h.signedPullResult(500, "pull_state_invalid", body)
	}
}

func (h *FunctionURLHandler) validateJSONTransport(request FunctionURLRequest, maxBytes int) (int, string) {
	if request.RequestContext.HTTP.Method != "POST" {
		return 405, "method_not_allowed"
	}
	if request.RawQueryString != "" {
		return 400, "query_not_allowed"
	}
	if request.IsBase64Encoded {
		return 400, "base64_not_allowed"
	}
	mediaType, _, err := mime.ParseMediaType(header(request.Headers, "content-type"))
	if err != nil || mediaType != "application/json" {
		return 415, "content_type_not_allowed"
	}
	if len([]byte(request.Body)) > maxBytes {
		return 413, "body_too_large"
	}
	return 0, ""
}

func snapshotMatchesRoute(snapshot TicketSnapshot, config PullRouteConfig) bool {
	// The run identity is the ticket itself: a claimed envelope must carry the
	// run id of its own issue, never a value configured on either side.
	return snapshot.SpaceKey == config.SpaceKey && snapshot.ProjectID == config.ProjectID &&
		snapshot.ProjectKey == config.ProjectKey && snapshot.CreatorID == config.AllowedCreatorID &&
		snapshot.ActivityType == config.AllowedActivityType && snapshot.RunID == snapshot.IssueKey && snapshot.Target == config.Target
}

type backlogWebhookPayload struct {
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
		ID    int64 `json:"id"`
		KeyID int64 `json:"key_id"`
	} `json:"content"`
}

func (h *FunctionURLHandler) authenticated(value string) bool {
	if len(value) < 7 || !strings.EqualFold(value[:5], "Basic") || value[5] != ' ' || strings.Contains(value[6:], " ") {
		return false
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(value[6:])
	if err != nil {
		return false
	}
	username, password, ok := strings.Cut(string(decoded), ":")
	if !ok {
		return false
	}
	usernameHash := sha256.Sum256([]byte(username))
	passwordHash := sha256.Sum256([]byte(password))
	usernameOK := subtle.ConstantTimeCompare(usernameHash[:], h.usernameHash[:])
	passwordOK := subtle.ConstantTimeCompare(passwordHash[:], h.passwordHash[:])
	return usernameOK&passwordOK == 1
}

func header(headers map[string]string, name string) string {
	result := ""
	found := false
	for key, headerValue := range headers {
		if strings.EqualFold(key, name) {
			if found {
				return ""
			}
			found = true
			result = headerValue
		}
	}
	return result
}

func statusForResult(result Result) int {
	switch result.Decision {
	case DecisionAccepted, DecisionIgnored:
		return 202
	case DecisionInvalid:
		return 400
	case DecisionRetryRequested:
		return 503
	case DecisionDependencyFailed:
		return 502
	case DecisionInternal:
		return 500
	default:
		return 500
	}
}

func resultResponse(status int, result Result) FunctionURLResponse {
	body, err := json.Marshal(result)
	if err != nil {
		body = []byte(`{"decision":"internal_error","code":"response_encode_failed"}`)
		status = 500
	}
	return baseResponse(status, body, "application/json")
}

func (h *FunctionURLHandler) signedPullResult(status int, code string, requestBody []byte) FunctionURLResponse {
	body, err := json.Marshal(Result{Decision: DecisionInvalid, Code: code})
	if err != nil {
		body = []byte(`{"decision":"internal_error","code":"response_encode_failed"}`)
		status = 500
	}
	return h.signedPullResponse(status, requestBody, body, "application/json")
}

func (h *FunctionURLHandler) signedPullResponse(status int, requestBody, responseBody []byte, contentType string) FunctionURLResponse {
	response := baseResponse(status, responseBody, contentType)
	response.Headers[PullResponseSignatureHeader] = SignPullResponse(h.pull.HMACKey, status, requestBody, responseBody)
	return response
}

func statusForTerminalResult(result Result) int {
	switch result.Decision {
	case DecisionAccepted:
		return 200
	case DecisionInvalid:
		if result.Code == "terminal_report_conflict" {
			return 409
		}
		return 400
	case DecisionRetryRequested:
		return 503
	case DecisionDependencyFailed:
		return 502
	case DecisionInternal:
		return 500
	default:
		return 500
	}
}

func statusForQuestionResult(result Result) int {
	switch result.Decision {
	case DecisionAccepted:
		return 200
	case DecisionInvalid:
		if result.Code == "question_report_conflict" || result.Code == "question_tick_resume_conflict" {
			return 409
		}
		return 400
	case DecisionRetryRequested:
		return 503
	case DecisionDependencyFailed:
		return 502
	case DecisionInternal:
		return 500
	default:
		return 500
	}
}

func (h *FunctionURLHandler) signedQuestionResponse(status int, requestBody []byte, result Result) FunctionURLResponse {
	body, err := json.Marshal(result)
	if err != nil {
		body = []byte(`{"decision":"internal_error","code":"response_encode_failed"}`)
		status = 500
	}
	response := baseResponse(status, body, "application/json")
	response.Headers[QuestionReportResponseSignatureHeader] = SignQuestionReportResponse(h.report.HMACKey, status, requestBody, body)
	return response
}

func (h *FunctionURLHandler) signedTickResponse(status int, requestBody []byte, result Result) FunctionURLResponse {
	body, err := json.Marshal(result)
	if err != nil {
		body = []byte(`{"decision":"internal_error","code":"response_encode_failed"}`)
		status = 500
	}
	response := baseResponse(status, body, "application/json")
	response.Headers[QuestionTickResponseSignatureHeader] = SignQuestionTickResponse(h.report.HMACKey, status, requestBody, body)
	return response
}

func (h *FunctionURLHandler) signedTerminalResult(status int, code string, requestBody []byte) FunctionURLResponse {
	return h.signedTerminalResponse(status, requestBody, Result{Decision: DecisionInvalid, Code: code})
}

func (h *FunctionURLHandler) signedTerminalResponse(status int, requestBody []byte, result Result) FunctionURLResponse {
	body, err := json.Marshal(result)
	if err != nil {
		body = []byte(`{"decision":"internal_error","code":"response_encode_failed"}`)
		status = 500
	}
	response := baseResponse(status, body, "application/json")
	response.Headers[TerminalReportResponseSignatureHeader] = SignTerminalReportResponse(h.report.HMACKey, status, requestBody, body)
	return response
}

func baseResponse(status int, body []byte, contentType string) FunctionURLResponse {
	headers := map[string]string{"cache-control": "no-store"}
	if contentType != "" {
		headers["content-type"] = contentType
	}
	return FunctionURLResponse{StatusCode: status, Headers: headers, Body: string(body)}
}
