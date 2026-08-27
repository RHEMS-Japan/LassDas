package hook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	TerminalReportProtocolVersion         = "terminal-report-v1"
	TerminalReportPath                    = "/terminal-report/v1"
	TerminalReportSignatureHeader         = "x-terminal-report-signature"
	TerminalReportResponseSignatureHeader = "x-terminal-report-response-signature"
	MaxTerminalReportRequestBytes         = 16 * 1024
	MaxTerminalReportClockSkew            = 10 * time.Minute
	MaxTerminalReportLease                = 10 * time.Minute
)

var (
	repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}$`)
	pullURLPattern    = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9_.-]{1,100})/([A-Za-z0-9_.-]{1,100})/pull/([1-9][0-9]{0,18})$`)
	runURLPattern     = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9_.-]{1,100})/([A-Za-z0-9_.-]{1,100})/actions/runs/([1-9][0-9]{0,18})/attempts/([1-9][0-9]{0,9})$`)
	// localRunURLPattern is the run reference of a pod-resident engine run
	// (docs: HERMES_AS_LASSDAS_RUNTIME): no workflow page exists to link,
	// but the reference still seals the same three identities — repository,
	// run id, attempt — so a stored record binds to exactly one engine run.
	localRunURLPattern = regexp.MustCompile(`^local-run://([A-Za-z0-9_.-]{1,100})/([A-Za-z0-9_.-]{1,100})/([1-9][0-9]{0,18})/attempts/([1-9][0-9]{0,9})$`)
)

type TerminalCode string

const (
	TerminalSuccess                        TerminalCode = "success"
	TerminalInputRejected                  TerminalCode = "input_rejected"
	TerminalReadinessRejected              TerminalCode = "readiness_rejected"
	TerminalClarificationRequired          TerminalCode = "clarification_required"
	TerminalReadinessUnresolved            TerminalCode = "readiness_unresolved"
	TerminalClarificationExpired           TerminalCode = "clarification_expired"
	TerminalCancelled                      TerminalCode = "cancelled"
	TerminalModelFailed                    TerminalCode = "model_failed"
	TerminalNonconverged                   TerminalCode = "nonconverged"
	TerminalValidationFailed               TerminalCode = "validation_failed"
	TerminalReleaseFailed                  TerminalCode = "release_failed"
	TerminalProductionDeploymentUnverified TerminalCode = "production_deployment_unverified"
	TerminalProductionVerificationFailed   TerminalCode = "production_verification_failed"
	TerminalInternalFailed                 TerminalCode = "internal_failed"
)

func (c TerminalCode) valid() bool {
	switch c {
	case TerminalSuccess, TerminalInputRejected, TerminalReadinessRejected, TerminalClarificationRequired,
		TerminalReadinessUnresolved, TerminalClarificationExpired, TerminalCancelled,
		TerminalModelFailed, TerminalNonconverged,
		TerminalValidationFailed, TerminalReleaseFailed, TerminalProductionDeploymentUnverified,
		TerminalProductionVerificationFailed, TerminalInternalFailed:
		return true
	default:
		return false
	}
}

// Delivery names how far a run travels without a person. The hook holds it per
// destination so it can require exactly the evidence that stopping point
// produces: a run that only proposes a change cannot claim a deployment, and a
// run that was configured to reach production cannot claim success without one.
const (
	DeliverPullRequest = "pull_request"
	DeliverIntegration = "integration"
	DeliverProduction  = "production"
)

// ReportDestination is one place the automation may deliver to, as the hook
// knows it. Nothing about a destination is inferred: a report naming a
// repository absent from this list is refused.
type ReportDestination struct {
	Repository       string `json:"repository"`
	Delivery         string `json:"delivery"`
	StagingOrigin    string `json:"staging_origin"`
	ProductionOrigin string `json:"production_origin"`
}

func (d ReportDestination) validate() error {
	if !repositoryPattern.MatchString(d.Repository) {
		return errors.New("report destination repository is invalid")
	}
	switch d.Delivery {
	case DeliverPullRequest, DeliverIntegration, DeliverProduction:
	default:
		return errors.New("report destination delivery is invalid")
	}
	if !validEvidenceOrigin(d.StagingOrigin) || !validEvidenceOrigin(d.ProductionOrigin) ||
		d.StagingOrigin == d.ProductionOrigin {
		return errors.New("report destination origins are invalid")
	}
	return nil
}

type ReportRouteConfig struct {
	HMACKey             []byte
	RepositoryID        int64
	RepositorySHA256    string
	WorkflowRefSHA256   string
	ExpectedRunID       string
	Destinations        []ReportDestination
	ClockSkew           time.Duration
	LeaseDuration       time.Duration
	SpaceKey            string
	ProjectID           int64
	ProjectKey          string
	AllowedCreatorID    int64
	AllowedActivityType int
	Target              DeliveryTarget
	// RunReferenceScheme names the run-reference form this deployment seals:
	// "" or "github" for workflow runs (the only form before the pod
	// constitution), "local" for pod-resident runs. The other form is
	// refused, so a workflow deployment cannot seal an unclickable local
	// reference and a pod cannot seal a fabricated workflow link.
	RunReferenceScheme string
}

func runReferenceSchemeAllowed(raw, scheme string) bool {
	if scheme == "local" {
		return localRunURLPattern.MatchString(raw)
	}
	return runURLPattern.MatchString(raw)
}

// DestinationFor resolves the destination a report names. The match is exact;
// nothing is guessed from a prefix or an owner.
func (c ReportRouteConfig) DestinationFor(repository string) (ReportDestination, error) {
	for _, destination := range c.Destinations {
		if destination.Repository == repository {
			return destination, nil
		}
	}
	return ReportDestination{}, errors.New("report repository is not a configured destination")
}

func (c ReportRouteConfig) Validate() error {
	if err := ValidatePullKey(c.HMACKey); err != nil {
		return errors.New("report authentication is invalid")
	}
	if c.RepositoryID <= 0 || !validIdentityDigest(c.RepositorySHA256) || !validIdentityDigest(c.WorkflowRefSHA256) {
		return errors.New("report repository identity is invalid")
	}
	if len(c.Destinations) == 0 || len(c.Destinations) > 8 {
		return errors.New("report destinations are invalid")
	}
	seen := make(map[string]struct{}, len(c.Destinations))
	for _, destination := range c.Destinations {
		if err := destination.validate(); err != nil {
			return err
		}
		if _, exists := seen[destination.Repository]; exists {
			return errors.New("report destinations contain duplicates")
		}
		seen[destination.Repository] = struct{}{}
	}
	if c.ClockSkew <= 0 || c.ClockSkew > MaxTerminalReportClockSkew ||
		c.LeaseDuration <= 0 || c.LeaseDuration > MaxTerminalReportLease {
		return errors.New("report timing is invalid")
	}
	if !runIDPattern.MatchString(c.ExpectedRunID) || !componentPattern.MatchString(c.SpaceKey) ||
		c.ProjectID <= 0 || !componentPattern.MatchString(c.ProjectKey) || c.AllowedCreatorID <= 0 || c.AllowedActivityType <= 0 {
		return errors.New("report allowlist is invalid")
	}
	if c.Target.RepositoryID != c.RepositoryID || c.Target.WorkflowRefSHA256 != c.WorkflowRefSHA256 || c.Target.Validate() != nil {
		return errors.New("report target binding is invalid")
	}
	return nil
}

func (c ReportRouteConfig) isZero() bool {
	return len(c.HMACKey) == 0 && c.RepositoryID == 0 && c.RepositorySHA256 == "" && c.WorkflowRefSHA256 == "" &&
		c.ExpectedRunID == "" && len(c.Destinations) == 0 && c.ClockSkew == 0 &&
		c.LeaseDuration == 0 && c.SpaceKey == "" && c.ProjectID == 0 && c.ProjectKey == "" &&
		c.AllowedCreatorID == 0 && c.AllowedActivityType == 0 && c.Target == (DeliveryTarget{})
}

type TerminalReportRequest struct {
	Protocol              string       `json:"protocol"`
	DeliveryID            string       `json:"delivery_id"`
	InputSHA256           string       `json:"input_sha256"`
	RepositoryID          int64        `json:"repository_id"`
	RepositorySHA256      string       `json:"repository_sha256"`
	WorkflowRefSHA256     string       `json:"workflow_ref_sha256"`
	WorkflowSHA           string       `json:"workflow_sha"`
	WorkflowRunID         int64        `json:"workflow_run_id"`
	RunAttempt            int          `json:"run_attempt"`
	AutomationRunID       string       `json:"automation_run_id"`
	Code                  TerminalCode `json:"code"`
	Repository            string       `json:"repository"`
	RunURL                string       `json:"run_url"`
	PullRequestURL        string       `json:"pull_request_url"`
	CommitSHA             string       `json:"commit_sha"`
	CommitURL             string       `json:"commit_url"`
	StagingEvidenceURL    string       `json:"staging_evidence_url"`
	ProductionEvidenceURL string       `json:"production_evidence_url"`
	TrailText             string       `json:"trail_text,omitempty"`
	// SpendText is what this run was billed, rendered for the requester.
	// Empty when no reading was available — the report then simply omits the
	// cost line rather than printing a zero that reads as "this was free".
	SpendText string    `json:"spend_text,omitempty"`
	IssuedAt  time.Time `json:"issued_at"`
}

// MaxTerminalTrailBytes bounds the requester-facing run record a terminal
// report may carry. It matches the composer's bound on the worker side.
const MaxTerminalTrailBytes = 6 * 1024

// ValidateTrailText holds the trail to the same plain-text discipline as
// every other requester-facing string: bounded, valid UTF-8, newlines only.
func ValidateTrailText(value string) error {
	if len(value) > MaxTerminalTrailBytes || !utf8.ValidString(value) ||
		strings.ContainsAny(value, "\x00\r") {
		return errors.New("trail text is invalid")
	}
	return nil
}

func (r TerminalReportRequest) ValidateShape() error {
	if r.Protocol != TerminalReportProtocolVersion || !validDeliveryID(r.DeliveryID) || !validDigest(r.InputSHA256) ||
		r.RepositoryID <= 0 || !validIdentityDigest(r.RepositorySHA256) || !validIdentityDigest(r.WorkflowRefSHA256) ||
		!commitPattern.MatchString(r.WorkflowSHA) || r.WorkflowRunID <= 0 || r.RunAttempt <= 0 ||
		!runIDPattern.MatchString(r.AutomationRunID) || !r.Code.valid() {
		return errors.New("terminal report identity is invalid")
	}
	// A report names a destination exactly when it claims something happened
	// there. A stop before any repository work names none, and a report that
	// shows a pull request or a deployment must say where.
	claimsEvidence := r.PullRequestURL != "" || r.CommitSHA != "" || r.StagingEvidenceURL != "" || r.ProductionEvidenceURL != ""
	if r.Repository != "" && !repositoryPattern.MatchString(r.Repository) {
		return errors.New("terminal report repository is invalid")
	}
	if claimsEvidence && r.Repository == "" {
		return errors.New("terminal report evidence names no destination")
	}
	if r.IssuedAt.IsZero() || !r.IssuedAt.Equal(r.IssuedAt.UTC()) {
		return errors.New("terminal report timestamp is invalid")
	}
	if r.RunURL == "" || len(r.RunURL) > 512 || len(r.PullRequestURL) > 512 || len(r.CommitURL) > 512 ||
		len(r.StagingEvidenceURL) > 2048 || len(r.ProductionEvidenceURL) > 2048 {
		return errors.New("terminal report evidence shape is invalid")
	}
	if err := ValidateTrailText(r.TrailText); err != nil {
		return err
	}
	if (r.CommitSHA == "") != (r.CommitURL == "") || (r.CommitSHA != "" && !commitPattern.MatchString(r.CommitSHA)) {
		return errors.New("terminal report commit binding is invalid")
	}
	return nil
}

func (r TerminalReportRequest) ValidateRoute(config ReportRouteConfig) error {
	if err := config.Validate(); err != nil {
		return errors.New("terminal report route configuration is invalid")
	}
	if err := r.ValidateShape(); err != nil {
		return err
	}
	if r.RepositoryID != config.RepositoryID || r.RepositorySHA256 != config.RepositorySHA256 ||
		r.WorkflowRefSHA256 != config.WorkflowRefSHA256 || r.AutomationRunID != config.ExpectedRunID {
		return errors.New("terminal report route is not allowed")
	}
	if !validRunURL(r.RunURL, config.RepositorySHA256, r.WorkflowRunID, r.RunAttempt) ||
		!runReferenceSchemeAllowed(r.RunURL, config.RunReferenceScheme) {
		return errors.New("terminal report run url is invalid")
	}
	if r.Repository == "" {
		// Nothing was delivered anywhere, so there is no destination to check
		// against; the code-specific rules below still forbid any evidence.
		return validateTerminalEvidenceShape(r, ReportDestination{})
	}
	destination, err := config.DestinationFor(r.Repository)
	if err != nil {
		return errors.New("terminal report repository is not allowed")
	}
	if r.PullRequestURL != "" && !validPullRequestURL(r.PullRequestURL, destination.Repository) {
		return errors.New("terminal report pull request url is invalid")
	}
	if r.CommitSHA != "" {
		expectedCommitURL := "https://github.com/" + destination.Repository + "/commit/" + r.CommitSHA
		if r.CommitURL != expectedCommitURL {
			return errors.New("terminal report commit url is invalid")
		}
	}
	if r.StagingEvidenceURL != "" && !validEvidenceURL(r.StagingEvidenceURL, destination.StagingOrigin) {
		return errors.New("terminal report staging evidence url is invalid")
	}
	if r.ProductionEvidenceURL != "" && !validEvidenceURL(r.ProductionEvidenceURL, destination.ProductionOrigin) {
		return errors.New("terminal report production evidence url is invalid")
	}
	return validateTerminalEvidenceShape(r, destination)
}

// validateTerminalEvidenceShape checks that the evidence a report carries
// matches both its outcome and how far its destination was configured to go.
func validateTerminalEvidenceShape(r TerminalReportRequest, destination ReportDestination) error {
	switch r.Code {
	case TerminalSuccess:
		// Success means the run reached the stopping point this destination is
		// configured for, so the evidence it must carry is exactly what that
		// stopping point produces — no less, and nothing it never reached.
		switch destination.Delivery {
		case DeliverPullRequest:
			if r.PullRequestURL == "" {
				return errors.New("successful terminal report is missing evidence")
			}
			if r.CommitSHA != "" || r.StagingEvidenceURL != "" || r.ProductionEvidenceURL != "" {
				return errors.New("proposal-only success cannot claim a deployment")
			}
		case DeliverIntegration:
			if r.PullRequestURL == "" || r.CommitSHA == "" || r.StagingEvidenceURL == "" {
				return errors.New("successful terminal report is missing evidence")
			}
			if r.ProductionEvidenceURL != "" {
				return errors.New("integration success cannot claim production evidence")
			}
		default:
			if r.PullRequestURL == "" || r.CommitSHA == "" || r.StagingEvidenceURL == "" || r.ProductionEvidenceURL == "" {
				return errors.New("successful terminal report is missing evidence")
			}
		}
	case TerminalProductionDeploymentUnverified, TerminalProductionVerificationFailed:
		if r.PullRequestURL == "" || r.CommitSHA == "" || r.StagingEvidenceURL == "" || r.ProductionEvidenceURL != "" {
			return errors.New("post-promotion failure evidence is invalid")
		}
	case TerminalReadinessRejected, TerminalClarificationRequired, TerminalReadinessUnresolved,
		TerminalClarificationExpired, TerminalCancelled:
		if r.PullRequestURL != "" || r.CommitSHA != "" || r.StagingEvidenceURL != "" || r.ProductionEvidenceURL != "" {
			return errors.New("pre-generation stop cannot claim repository evidence")
		}
	default:
		if r.ProductionEvidenceURL != "" {
			return errors.New("failed terminal report cannot claim production evidence")
		}
	}
	return nil
}

func MarshalTerminalReportRequest(request TerminalReportRequest) ([]byte, error) {
	request.IssuedAt = request.IssuedAt.UTC()
	if err := request.ValidateShape(); err != nil {
		return nil, err
	}
	return json.Marshal(request)
}

func DecodeTerminalReportRequest(encoded []byte) (TerminalReportRequest, error) {
	if len(encoded) == 0 || len(encoded) > MaxTerminalReportRequestBytes {
		return TerminalReportRequest{}, errors.New("terminal report request size is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var request TerminalReportRequest
	if err := decoder.Decode(&request); err != nil {
		return TerminalReportRequest{}, errors.New("terminal report request is invalid")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return TerminalReportRequest{}, errors.New("terminal report request is invalid")
	}
	canonical, err := MarshalTerminalReportRequest(request)
	if err != nil || !bytes.Equal(canonical, encoded) {
		return TerminalReportRequest{}, errors.New("terminal report request is not canonical")
	}
	return request, nil
}

type terminalReportRecord struct {
	Protocol              string       `json:"protocol"`
	DeliveryID            string       `json:"delivery_id"`
	InputSHA256           string       `json:"input_sha256"`
	RepositoryID          int64        `json:"repository_id"`
	RepositorySHA256      string       `json:"repository_sha256"`
	WorkflowRefSHA256     string       `json:"workflow_ref_sha256"`
	WorkflowSHA           string       `json:"workflow_sha"`
	WorkflowRunID         int64        `json:"workflow_run_id"`
	RunAttempt            int          `json:"run_attempt"`
	AutomationRunID       string       `json:"automation_run_id"`
	Code                  TerminalCode `json:"code"`
	Repository            string       `json:"repository"`
	RunURL                string       `json:"run_url"`
	PullRequestURL        string       `json:"pull_request_url"`
	CommitSHA             string       `json:"commit_sha"`
	CommitURL             string       `json:"commit_url"`
	StagingEvidenceURL    string       `json:"staging_evidence_url"`
	ProductionEvidenceURL string       `json:"production_evidence_url"`
}

// MarshalTerminalReportRecord returns the immutable terminal outcome. IssuedAt
// authenticates one HTTP attempt and is deliberately excluded so a retry can
// use a fresh timestamp without becoming a different terminal result.
func MarshalTerminalReportRecord(request TerminalReportRequest) ([]byte, error) {
	if err := request.ValidateShape(); err != nil {
		return nil, err
	}
	return json.Marshal(terminalReportRecord{
		Protocol: request.Protocol, DeliveryID: request.DeliveryID, InputSHA256: request.InputSHA256,
		RepositoryID: request.RepositoryID, RepositorySHA256: request.RepositorySHA256,
		WorkflowRefSHA256: request.WorkflowRefSHA256, WorkflowSHA: request.WorkflowSHA,
		WorkflowRunID: request.WorkflowRunID, RunAttempt: request.RunAttempt, AutomationRunID: request.AutomationRunID,
		Code: request.Code, RunURL: request.RunURL, PullRequestURL: request.PullRequestURL,
		CommitSHA: request.CommitSHA, CommitURL: request.CommitURL,
		StagingEvidenceURL: request.StagingEvidenceURL, ProductionEvidenceURL: request.ProductionEvidenceURL,
	})
}

func TerminalReportDigest(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

func SignTerminalReportRequest(key, body []byte) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(TerminalReportProtocolVersion + "\nrequest\nPOST\n" + TerminalReportPath + "\n"))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func VerifyTerminalReportRequestSignature(key, body []byte, signature string) bool {
	expected := SignTerminalReportRequest(key, body)
	return len(signature) == len(expected) && hmac.Equal([]byte(signature), []byte(expected))
}

func SignTerminalReportResponse(key []byte, status int, requestBody, responseBody []byte) string {
	requestDigest := sha256.Sum256(requestBody)
	responseDigest := sha256.Sum256(responseBody)
	message := strings.Join([]string{
		TerminalReportProtocolVersion,
		"response",
		strconv.Itoa(status),
		hex.EncodeToString(requestDigest[:]),
		hex.EncodeToString(responseDigest[:]),
	}, "\n")
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(message))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func VerifyTerminalReportResponseSignature(key []byte, status int, requestBody, responseBody []byte, signature string) bool {
	expected := SignTerminalReportResponse(key, status, requestBody, responseBody)
	return len(signature) == len(expected) && hmac.Equal([]byte(signature), []byte(expected))
}

func validEvidenceOrigin(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.Port() == "" && parsed.User == nil &&
		parsed.Path == "" && parsed.RawPath == "" && parsed.RawQuery == "" && parsed.Fragment == "" && parsed.String() == raw
}

func validEvidenceURL(raw, origin string) bool {
	if len(raw) == 0 || len(raw) > 2048 || strings.ContainsAny(raw, "\r\n") || !validEvidenceOrigin(origin) {
		return false
	}
	parsed, err := url.Parse(raw)
	base, baseErr := url.Parse(origin)
	if err != nil || baseErr != nil || parsed.Scheme != base.Scheme || parsed.Host != base.Host || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" || parsed.Path == "" || parsed.Path[0] != '/' ||
		path.Clean(parsed.Path) != parsed.Path || parsed.String() != raw {
		return false
	}
	return !strings.Contains(parsed.EscapedPath(), "\\")
}

func validPullRequestURL(raw, repository string) bool {
	matches := pullURLPattern.FindStringSubmatch(raw)
	return len(matches) == 4 && matches[1]+"/"+matches[2] == repository
}

func validRunURL(raw, repositoryDigest string, workflowRunID int64, runAttempt int) bool {
	matches := runURLPattern.FindStringSubmatch(raw)
	if len(matches) != 5 {
		matches = localRunURLPattern.FindStringSubmatch(raw)
	}
	if len(matches) != 5 || HashIdentity(matches[1]+"/"+matches[2]) != repositoryDigest {
		return false
	}
	runID, runErr := strconv.ParseInt(matches[3], 10, 64)
	attempt, attemptErr := strconv.Atoi(matches[4])
	return runErr == nil && attemptErr == nil && runID == workflowRunID && attempt == runAttempt
}
