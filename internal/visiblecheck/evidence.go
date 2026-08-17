package visiblecheck

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image/png"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"automation.internal/ticket-ingress/internal/githubapi"
	"automation.internal/ticket-ingress/internal/releaseproof"
	"automation.internal/ticket-ingress/internal/worker"
)

const (
	SchemaVersion       = 1
	MaxVisibleText      = 4 * 1024 * 1024
	MaxScreenshotBytes  = 16 * 1024 * 1024
	maxScreenshotWidth  = 4096
	maxScreenshotHeight = 32768
	maxScreenshotPixels = 32 * 1024 * 1024
)

var (
	sha256Pattern  = regexp.MustCompile(`^[a-f0-9]{64}$`)
	versionPattern = regexp.MustCompile(`^[1-9][0-9]{0,3}(?:\.[0-9]{1,6}){2,3}$`)
)

// capture is produced only by the credential-free browser implementation. The
// rendered text is used to evaluate the ticket AC and is not retained in the
// evidence artifact.
type capture struct {
	requestedURL   string
	finalURL       string
	statusCode     int
	visibleText    string
	screenshotPNG  []byte
	browser        string
	BrowserVersion string
	observedAt     time.Time
}

// Evidence binds one in-process browser observation to a sealed release proof.
// The production form also binds the already validated staging observation.
type Evidence struct {
	SchemaVersion         int       `json:"schema_version"`
	DeliveryID            string    `json:"delivery_id"`
	InputSHA256           string    `json:"input_sha256"`
	ConfigSHA256          string    `json:"config_sha256"`
	ToolSHA               string    `json:"tool_sha"`
	IssueKey              string    `json:"issue_key"`
	CandidateSHA256       string    `json:"candidate_sha256"`
	ValidationSHA256      string    `json:"validation_sha256"`
	Environment           string    `json:"environment"`
	ReleaseProofSHA256    string    `json:"release_proof_sha256"`
	ParentEvidenceSHA256  string    `json:"parent_evidence_sha256,omitempty"`
	ReleaseSHA            string    `json:"release_sha"`
	DeploymentWorkflowID  int64     `json:"deployment_workflow_id"`
	DeploymentRunID       int64     `json:"deployment_run_id"`
	DeploymentCompletedAt time.Time `json:"deployment_completed_at"`
	BranchHeadSHA         string    `json:"branch_head_sha"`
	RequestedURL          string    `json:"requested_url"`
	FinalURL              string    `json:"final_url"`
	HTTPStatus            int       `json:"http_status"`
	Browser               string    `json:"browser"`
	BrowserVersion        string    `json:"browser_version"`
	ObservedAt            time.Time `json:"observed_at"`
	VisibleTextBytes      int       `json:"visible_text_bytes"`
	VisibleTextSHA256     string    `json:"visible_text_sha256"`
	ScreenshotBytes       int       `json:"screenshot_bytes"`
	ScreenshotSHA256      string    `json:"screenshot_sha256"`
	ExpectedTextSHA256    string    `json:"expected_text_sha256"`
	AbsentTextSHA256      string    `json:"absent_text_sha256,omitempty"`
	ExpectedTextVisible   bool      `json:"expected_text_visible"`
	AbsentTextNotVisible  bool      `json:"absent_text_not_visible"`
	EvidenceSHA256        string    `json:"evidence_sha256"`
}

type releaseBinding struct {
	environment       string
	proofSHA256       string
	parentEvidenceSHA string
	primaryRun        githubapi.WorkflowRun
	completedAt       time.Time
	releaseSHA        string
	branchHeadSHA     string
	origin            string
}

func stagingBinding(proof releaseproof.StagingProof, input releaseproof.StagingInputs) (releaseBinding, error) {
	consumer, err := input.Request.Consumer(input.Config)
	if err != nil {
		return releaseBinding{}, errors.New("staging release proof is invalid")
	}
	if err := proof.Validate(input); err != nil {
		return releaseBinding{}, errors.New("staging release proof is invalid")
	}
	completedAt, err := releaseproof.ValidateStagingDeployment(proof.StagingDeployment, consumer)
	if err != nil {
		return releaseBinding{}, errors.New("staging deployment proof is invalid")
	}
	run, err := exactPrimaryRun(proof.StagingDeployment.WorkflowRuns, consumer.Contract().StagingWorkflow.ID)
	if err != nil {
		return releaseBinding{}, err
	}
	return releaseBinding{
		environment: "staging", proofSHA256: proof.ProofSHA256,
		primaryRun: run, completedAt: completedAt,
		releaseSHA:    proof.StagingDeployment.Merge.MergeSHA,
		branchHeadSHA: proof.StagingDeployment.BranchHeadSHA,
		origin:        consumer.StagingOrigin,
	}, nil
}

func productionBinding(
	proof releaseproof.ProductionProof,
	staging releaseproof.StagingProof,
	stagingEvidence Evidence,
	stagingScreenshot []byte,
	input releaseproof.StagingInputs,
	now time.Time,
) (releaseBinding, error) {
	consumer, err := input.Request.Consumer(input.Config)
	if err != nil {
		return releaseBinding{}, errors.New("staging release proof is invalid")
	}
	if err := staging.Validate(input); err != nil {
		return releaseBinding{}, errors.New("staging release proof is invalid")
	}
	if err := stagingEvidence.ValidateStagingAt(staging, input, now); err != nil ||
		stagingEvidence.ValidateScreenshot(stagingScreenshot) != nil {
		return releaseBinding{}, errors.New("staging visible proof is invalid")
	}
	if err := proof.Validate(staging, consumer); err != nil || proof.StagingVisibleEvidenceSHA256 != stagingEvidence.EvidenceSHA256 {
		return releaseBinding{}, errors.New("production release proof is invalid")
	}
	if !promotionFollowsObservation(proof.PromotionPullRequest.CreatedAt, stagingEvidence.ObservedAt) {
		return releaseBinding{}, errors.New("production promotion predates staging acceptance")
	}
	completedAt, err := releaseproof.ValidateProductionDeployment(proof.ProductionDeployment, consumer)
	if err != nil {
		return releaseBinding{}, errors.New("production deployment proof is invalid")
	}
	primaryID := consumer.Contract().ProductionWorkflows[0].ID
	run, err := exactPrimaryRun(proof.ProductionDeployment.WorkflowRuns, primaryID)
	if err != nil {
		return releaseBinding{}, err
	}
	return releaseBinding{
		environment: "production", proofSHA256: proof.ProofSHA256,
		parentEvidenceSHA: stagingEvidence.EvidenceSHA256,
		primaryRun:        run, completedAt: completedAt,
		releaseSHA:    proof.ProductionDeployment.Merge.MergeSHA,
		branchHeadSHA: proof.ProductionDeployment.BranchHeadSHA,
		origin:        consumer.ProductionOrigin,
	}, nil
}

func promotionFollowsObservation(createdAt, observedAt time.Time) bool {
	return !createdAt.IsZero() && createdAt.Location() == time.UTC &&
		!observedAt.IsZero() && observedAt.Location() == time.UTC && !createdAt.Before(observedAt.Truncate(time.Second))
}

func exactPrimaryRun(runs []githubapi.WorkflowRun, workflowID int64) (githubapi.WorkflowRun, error) {
	var selected githubapi.WorkflowRun
	matches := 0
	for _, run := range runs {
		if run.WorkflowID == workflowID {
			selected = run
			matches++
		}
	}
	if workflowID <= 0 || matches != 1 {
		return githubapi.WorkflowRun{}, errors.New("primary deployment workflow is invalid")
	}
	return selected, nil
}

func seal(observed capture, binding releaseBinding, input releaseproof.StagingInputs, now time.Time) (Evidence, error) {
	request := input.Request
	if err := validateCapture(observed, binding.origin+request.VerificationPath, binding.completedAt, now, request); err != nil {
		return Evidence{}, err
	}
	evidence := Evidence{
		SchemaVersion: SchemaVersion,
		DeliveryID:    request.DeliveryID, InputSHA256: request.InputSHA256,
		ConfigSHA256: request.ConfigSHA256, ToolSHA: request.ToolSHA, IssueKey: request.IssueKey,
		CandidateSHA256: input.Candidate.CandidateSHA256, ValidationSHA256: input.Validation.ValidationSHA256,
		Environment: binding.environment, ReleaseProofSHA256: binding.proofSHA256,
		ParentEvidenceSHA256: binding.parentEvidenceSHA, ReleaseSHA: binding.releaseSHA,
		DeploymentWorkflowID: binding.primaryRun.WorkflowID, DeploymentRunID: binding.primaryRun.ID,
		DeploymentCompletedAt: binding.completedAt, BranchHeadSHA: binding.branchHeadSHA,
		RequestedURL: binding.origin + request.VerificationPath, FinalURL: observed.finalURL,
		HTTPStatus: observed.statusCode, Browser: observed.browser, BrowserVersion: observed.BrowserVersion,
		ObservedAt: observed.observedAt, VisibleTextBytes: len(observed.visibleText),
		VisibleTextSHA256: digest([]byte(observed.visibleText)), ScreenshotBytes: len(observed.screenshotPNG),
		ScreenshotSHA256: digest(observed.screenshotPNG), ExpectedTextSHA256: digest([]byte(request.ExpectedText)),
		ExpectedTextVisible: true, AbsentTextNotVisible: true,
	}
	if request.AbsentText != "" {
		evidence.AbsentTextSHA256 = digest([]byte(request.AbsentText))
	}
	var err error
	evidence.EvidenceSHA256, err = evidenceDigest(evidence)
	if err != nil {
		return Evidence{}, errors.New("visible evidence could not be sealed")
	}
	if err := evidence.validateAt(binding, input, now); err != nil {
		return Evidence{}, err
	}
	return evidence, nil
}

// ValidateStaging replays the complete worker gate and staging release proof.
func (e Evidence) ValidateStaging(proof releaseproof.StagingProof, input releaseproof.StagingInputs) error {
	return e.ValidateStagingAt(proof, input, time.Now().UTC())
}

func (e Evidence) ValidateStagingAt(proof releaseproof.StagingProof, input releaseproof.StagingInputs, now time.Time) error {
	binding, err := stagingBinding(proof, input)
	if err != nil {
		return err
	}
	return e.validateAt(binding, input, now)
}

// ValidateProduction replays the complete staging chain, its raw screenshot,
// the production proof, and the production browser observation.
func (e Evidence) ValidateProduction(
	proof releaseproof.ProductionProof,
	staging releaseproof.StagingProof,
	stagingEvidence Evidence,
	stagingScreenshot []byte,
	input releaseproof.StagingInputs,
) error {
	now := time.Now().UTC()
	binding, err := productionBinding(proof, staging, stagingEvidence, stagingScreenshot, input, now)
	if err != nil {
		return err
	}
	return e.validateAt(binding, input, now)
}

func (e Evidence) validateAt(binding releaseBinding, input releaseproof.StagingInputs, now time.Time) error {
	if now.IsZero() || now.Location() != time.UTC || input.Validation.Validate(input.Candidate, input.Source, input.Request, input.Config) != nil {
		return errors.New("visible evidence validation binding is invalid")
	}
	request := input.Request
	expectedURL := binding.origin + request.VerificationPath
	if e.SchemaVersion != SchemaVersion || e.DeliveryID != request.DeliveryID || e.InputSHA256 != request.InputSHA256 ||
		e.ConfigSHA256 != request.ConfigSHA256 || e.ToolSHA != request.ToolSHA || e.IssueKey != request.IssueKey ||
		e.CandidateSHA256 != input.Candidate.CandidateSHA256 || e.ValidationSHA256 != input.Validation.ValidationSHA256 ||
		e.Environment != binding.environment || e.ReleaseProofSHA256 != binding.proofSHA256 ||
		e.ParentEvidenceSHA256 != binding.parentEvidenceSHA || e.ReleaseSHA != binding.releaseSHA ||
		e.DeploymentWorkflowID != binding.primaryRun.WorkflowID || e.DeploymentRunID != binding.primaryRun.ID ||
		!e.DeploymentCompletedAt.Equal(binding.completedAt) || e.BranchHeadSHA != binding.branchHeadSHA ||
		e.RequestedURL != expectedURL || e.FinalURL != expectedURL || e.HTTPStatus < 200 || e.HTTPStatus > 299 {
		return errors.New("visible evidence identity is invalid")
	}
	if e.Browser != "chrome" || !versionPattern.MatchString(e.BrowserVersion) ||
		e.ObservedAt.IsZero() || e.ObservedAt.Location() != time.UTC || e.ObservedAt.Before(binding.completedAt) || e.ObservedAt.After(now.Add(time.Minute)) ||
		e.VisibleTextBytes < 1 || e.VisibleTextBytes > MaxVisibleText || !sha256Pattern.MatchString(e.VisibleTextSHA256) ||
		e.ScreenshotBytes < 64 || e.ScreenshotBytes > MaxScreenshotBytes || !sha256Pattern.MatchString(e.ScreenshotSHA256) ||
		e.ExpectedTextSHA256 != digest([]byte(request.ExpectedText)) || !e.ExpectedTextVisible || !e.AbsentTextNotVisible ||
		!sha256Pattern.MatchString(e.EvidenceSHA256) {
		return errors.New("visible evidence result is invalid")
	}
	expectedAbsent := ""
	if request.AbsentText != "" {
		expectedAbsent = digest([]byte(request.AbsentText))
	}
	if e.AbsentTextSHA256 != expectedAbsent {
		return errors.New("visible evidence absent-text binding is invalid")
	}
	sealed, err := evidenceDigest(e)
	if err != nil || sealed != e.EvidenceSHA256 {
		return errors.New("visible evidence digest is invalid")
	}
	return nil
}

// ValidateScreenshot binds the separately uploaded raw screenshot to the
// evidence. Promotion must validate both artifacts from the browser job.
func (e Evidence) ValidateScreenshot(encoded []byte) error {
	if len(encoded) != e.ScreenshotBytes || len(encoded) < 64 || len(encoded) > MaxScreenshotBytes ||
		digest(encoded) != e.ScreenshotSHA256 {
		return errors.New("visible screenshot binding is invalid")
	}
	if err := validatePNG(encoded); err != nil {
		return errors.New("visible screenshot is invalid")
	}
	return nil
}

func validateCapture(observed capture, expectedURL string, completedAt, now time.Time, request worker.TicketRequest) error {
	if observed.requestedURL != expectedURL || observed.finalURL != expectedURL || observed.statusCode < 200 || observed.statusCode > 299 ||
		observed.browser != "chrome" || !versionPattern.MatchString(observed.BrowserVersion) ||
		observed.observedAt.IsZero() || observed.observedAt.Location() != time.UTC || observed.observedAt.Before(completedAt) ||
		observed.observedAt.After(now.Add(time.Minute)) || len(observed.visibleText) == 0 || len(observed.visibleText) > MaxVisibleText ||
		!utf8.ValidString(observed.visibleText) || strings.ContainsRune(observed.visibleText, '\x00') {
		return errors.New("browser observation is invalid")
	}
	if !strings.Contains(observed.visibleText, request.ExpectedText) ||
		request.AbsentText != "" && strings.Contains(observed.visibleText, request.AbsentText) {
		return errors.New("visible acceptance criteria were not met")
	}
	if len(observed.screenshotPNG) < 64 || len(observed.screenshotPNG) > MaxScreenshotBytes {
		return errors.New("browser screenshot is invalid")
	}
	if err := validatePNG(observed.screenshotPNG); err != nil {
		return errors.New("browser screenshot is invalid")
	}
	return nil
}

func validatePNG(encoded []byte) error {
	configuration, err := png.DecodeConfig(bytes.NewReader(encoded))
	if err != nil || configuration.Width < 1 || configuration.Height < 1 ||
		configuration.Width > maxScreenshotWidth || configuration.Height > maxScreenshotHeight ||
		int64(configuration.Width)*int64(configuration.Height) > maxScreenshotPixels {
		return errors.New("PNG dimensions are invalid")
	}
	image, err := png.Decode(bytes.NewReader(encoded))
	if err != nil || image.Bounds().Dx() != configuration.Width || image.Bounds().Dy() != configuration.Height {
		return errors.New("PNG content is invalid")
	}
	return nil
}

func evidenceDigest(evidence Evidence) (string, error) {
	evidence.EvidenceSHA256 = ""
	encoded, err := json.Marshal(evidence)
	if err != nil {
		return "", err
	}
	return digest(encoded), nil
}

func digest(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func validExactHTTPSURL(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.Hostname() == strings.ToLower(parsed.Hostname()) &&
		parsed.User == nil && parsed.Port() == "" && parsed.Path != "" && strings.HasPrefix(parsed.Path, "/") &&
		path.Clean(parsed.Path) == parsed.Path && parsed.RawPath == "" && parsed.RawQuery == "" && parsed.Fragment == "" && parsed.String() == raw
}
