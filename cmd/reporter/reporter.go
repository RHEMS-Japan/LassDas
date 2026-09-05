package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
)

const (
	maxEnvelopeFileBytes = hook.MaxDeliveredEnvelopeBytes
	maxEvidenceFileBytes = 8 * 1024
	maxHMACKeyFileBytes  = 256
	maxResponseBytes     = 8 * 1024
	maxRunAttempt        = 1_000_000
	reportTimeout        = 30 * time.Second
	maxReportAttempts    = 3
)

var (
	decimalPattern          = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)
	commitPattern           = regexp.MustCompile(`^[a-f0-9]{40}$`)
	repositoryPattern       = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}$`)
	pullRequestPattern      = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9_.-]{1,100})/([A-Za-z0-9_.-]{1,100})/pull/[1-9][0-9]{0,18}$`)
	commitURLPattern        = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9_.-]{1,100})/([A-Za-z0-9_.-]{1,100})/commit/([a-f0-9]{40})$`)
	reportRepositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}$`)
	functionHostPattern     = regexp.MustCompile(`^[a-z0-9]{20,64}\.lambda-url\.ap-northeast-1\.on\.aws$`)
	dnsNamePattern          = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,251}[a-z0-9]$`)
)

type commandFlags struct {
	envelopeFile string
	code         hook.TerminalCode
	repository   string
	evidenceFile string
	trailFile    string
	callbackURL  string
	hmacKeyFile  string
}

type evidenceMetadata struct {
	PullRequestURL        string `json:"pull_request_url,omitempty"`
	CommitSHA             string `json:"commit_sha,omitempty"`
	CommitURL             string `json:"commit_url,omitempty"`
	StagingEvidenceURL    string `json:"staging_evidence_url,omitempty"`
	ProductionEvidenceURL string `json:"production_evidence_url,omitempty"`
}

type githubIdentity struct {
	repository       string
	repositoryID     int64
	workflowSHA      string
	workflowRunID    int64
	runAttempt       int
	repositoryDigest string
	workflowDigest   string
}

type reporterFailure string

func (e reporterFailure) Error() string { return string(e) }

func errorCode(err error) string {
	var failure reporterFailure
	if errors.As(err, &failure) {
		return string(failure)
	}
	return "unexpected_failure"
}

func run(args []string, getenv func(string) string, now func() time.Time, transport http.RoundTripper) (commandOutput, error) {
	return runWithRetryWait(args, getenv, now, transport, waitForReportRetry)
}

func runWithRetryWait(args []string, getenv func(string) string, now func() time.Time, transport http.RoundTripper, retryWait func(context.Context, time.Duration) error) (commandOutput, error) {
	flags, err := parseFlags(args)
	if err != nil {
		return commandOutput{}, err
	}
	if getenv == nil || now == nil || retryWait == nil {
		return commandOutput{}, reporterFailure("configuration_invalid")
	}
	envelope, err := loadEnvelope(flags.envelopeFile)
	if err != nil {
		return commandOutput{}, err
	}
	evidence, err := loadEvidence(flags.evidenceFile, flags.code)
	if err != nil {
		return commandOutput{}, err
	}
	trail := ""
	if flags.trailFile != "" {
		encoded, err := readRegularFile(flags.trailFile, hook.MaxTerminalTrailBytes, false)
		if err != nil || hook.ValidateTrailText(string(encoded)) != nil {
			return commandOutput{}, reporterFailure("trail_invalid")
		}
		trail = string(encoded)
	}
	identity, err := loadGitHubIdentity(getenv, envelope)
	if err != nil {
		return commandOutput{}, err
	}
	if err := validateCallbackURL(flags.callbackURL); err != nil {
		return commandOutput{}, err
	}
	keyFile, err := readRegularFile(flags.hmacKeyFile, maxHMACKeyFileBytes, true)
	if err != nil {
		return commandOutput{}, reporterFailure("hmac_key_invalid")
	}
	defer clear(keyFile)
	key, err := hook.DecodePullHMACKey(string(keyFile))
	if err != nil {
		return commandOutput{}, reporterFailure("hmac_key_invalid")
	}
	defer clear(key)

	report := hook.TerminalReportRequest{
		Protocol:   hook.TerminalReportProtocolVersion,
		DeliveryID: envelope.DeliveryID, InputSHA256: envelope.Snapshot.InputSHA256,
		RepositoryID: identity.repositoryID, RepositorySHA256: identity.repositoryDigest,
		WorkflowRefSHA256: identity.workflowDigest, WorkflowSHA: identity.workflowSHA,
		WorkflowRunID: identity.workflowRunID, RunAttempt: identity.runAttempt,
		AutomationRunID: envelope.Snapshot.RunID, Code: flags.code, Repository: flags.repository,
		RunURL: "https://github.com/" + identity.repository + "/actions/runs/" +
			strconv.FormatInt(identity.workflowRunID, 10) + "/attempts/" + strconv.Itoa(identity.runAttempt),
		PullRequestURL: evidence.PullRequestURL, CommitSHA: evidence.CommitSHA, CommitURL: evidence.CommitURL,
		StagingEvidenceURL: evidence.StagingEvidenceURL, ProductionEvidenceURL: evidence.ProductionEvidenceURL,
		TrailText: trail,
	}
	client := newHTTPClient(transport)
	result, err := submitTerminalReport(context.Background(), client, flags.callbackURL, key, report, now, retryWait)
	if err != nil {
		return commandOutput{}, err
	}
	return commandOutput{Decision: string(result.Decision), Code: result.Code}, nil
}

func parseFlags(args []string) (commandFlags, error) {
	if len(args) != 12 && len(args) != 14 {
		return commandFlags{}, reporterFailure("arguments_invalid")
	}
	values := make(map[string]string, 7)
	allowed := map[string]bool{
		"--envelope-file": true,
		"--code":          true,
		"--repository":    true,
		"--evidence-file": true,
		"--callback-url":  true,
		"--hmac-key-file": true,
		"--trail-file":    true,
	}
	for index := 0; index < len(args); index += 2 {
		name, value := args[index], args[index+1]
		// A failure before any repository work names no destination, and the
		// receiving protocol accepts that explicitly. Refusing the empty
		// value here made the failure report itself fail (measured
		// 2026-08-06 on the first live ticket).
		if !allowed[name] || (value == "" && name != "--repository") || strings.ContainsAny(value, "\x00\r\n") {
			return commandFlags{}, reporterFailure("arguments_invalid")
		}
		if _, duplicate := values[name]; duplicate {
			return commandFlags{}, reporterFailure("arguments_invalid")
		}
		values[name] = value
	}
	code := hook.TerminalCode(values["--code"])
	if !validTerminalCode(code) {
		return commandFlags{}, reporterFailure("terminal_code_invalid")
	}
	if repository := values["--repository"]; repository != "" && !reportRepositoryPattern.MatchString(repository) {
		return commandFlags{}, reporterFailure("arguments_invalid")
	}
	if len(args) == 14 && values["--trail-file"] == "" {
		return commandFlags{}, reporterFailure("arguments_invalid")
	}
	return commandFlags{
		envelopeFile: values["--envelope-file"], code: code, repository: values["--repository"],
		evidenceFile: values["--evidence-file"], trailFile: values["--trail-file"],
		callbackURL: values["--callback-url"], hmacKeyFile: values["--hmac-key-file"],
	}, nil
}

func validTerminalCode(code hook.TerminalCode) bool { return code.Valid() }

func loadEnvelope(filePath string) (hook.DispatchEnvelope, error) {
	encoded, err := readRegularFile(filePath, maxEnvelopeFileBytes, false)
	if err != nil {
		return hook.DispatchEnvelope{}, reporterFailure("envelope_invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var envelope hook.DispatchEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return hook.DispatchEnvelope{}, reporterFailure("envelope_invalid")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return hook.DispatchEnvelope{}, reporterFailure("envelope_invalid")
	}
	canonical, err := json.Marshal(envelope)
	if err != nil || !bytes.Equal(canonical, encoded) || hook.ValidateEnvelope(envelope) != nil {
		return hook.DispatchEnvelope{}, reporterFailure("envelope_invalid")
	}
	return envelope, nil
}

func loadEvidence(filePath string, code hook.TerminalCode) (evidenceMetadata, error) {
	encoded, err := readRegularFile(filePath, maxEvidenceFileBytes, false)
	if err != nil {
		return evidenceMetadata{}, reporterFailure("evidence_invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var evidence evidenceMetadata
	if err := decoder.Decode(&evidence); err != nil {
		return evidenceMetadata{}, reporterFailure("evidence_invalid")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return evidenceMetadata{}, reporterFailure("evidence_invalid")
	}
	canonical, err := json.Marshal(evidence)
	if err != nil || !bytes.Equal(canonical, encoded) || validateEvidence(evidence, code) != nil {
		return evidenceMetadata{}, reporterFailure("evidence_invalid")
	}
	return evidence, nil
}

func validateEvidence(evidence evidenceMetadata, code hook.TerminalCode) error {
	values := []string{evidence.PullRequestURL, evidence.CommitSHA, evidence.CommitURL, evidence.StagingEvidenceURL, evidence.ProductionEvidenceURL}
	for _, value := range values {
		if strings.ContainsAny(value, "\x00\r\n") {
			return reporterFailure("evidence_invalid")
		}
	}
	pullRepository := ""
	if evidence.PullRequestURL != "" {
		matches := pullRequestPattern.FindStringSubmatch(evidence.PullRequestURL)
		if len(matches) != 3 {
			return reporterFailure("evidence_invalid")
		}
		pullRepository = matches[1] + "/" + matches[2]
	}
	if (evidence.CommitSHA == "") != (evidence.CommitURL == "") {
		return reporterFailure("evidence_invalid")
	}
	if evidence.CommitSHA != "" {
		matches := commitURLPattern.FindStringSubmatch(evidence.CommitURL)
		if !commitPattern.MatchString(evidence.CommitSHA) || len(matches) != 4 || matches[3] != evidence.CommitSHA {
			return reporterFailure("evidence_invalid")
		}
		commitRepository := matches[1] + "/" + matches[2]
		if pullRepository != "" && commitRepository != pullRepository {
			return reporterFailure("evidence_invalid")
		}
	}
	if evidence.StagingEvidenceURL != "" && !validHTTPSURL(evidence.StagingEvidenceURL) {
		return reporterFailure("evidence_invalid")
	}
	if evidence.ProductionEvidenceURL != "" && !validHTTPSURL(evidence.ProductionEvidenceURL) {
		return reporterFailure("evidence_invalid")
	}
	if evidence.StagingEvidenceURL != "" && evidence.ProductionEvidenceURL != "" {
		staging, _ := url.Parse(evidence.StagingEvidenceURL)
		production, _ := url.Parse(evidence.ProductionEvidenceURL)
		if staging.Scheme+"://"+staging.Host == production.Scheme+"://"+production.Host {
			return reporterFailure("evidence_invalid")
		}
	}
	switch code {
	case hook.TerminalSuccess:
		// Success evidence is what the configured stopping point produced: a
		// proposal-only delivery carries the pull request alone. The endpoint
		// knows each destination's stopping point and enforces the exact
		// shape; this preflight only requires the chain to be consistent.
		if evidence.PullRequestURL == "" {
			return reporterFailure("evidence_invalid")
		}
		if evidence.ProductionEvidenceURL != "" && (evidence.CommitSHA == "" || evidence.StagingEvidenceURL == "") {
			return reporterFailure("evidence_invalid")
		}
		if evidence.StagingEvidenceURL != "" && evidence.CommitSHA == "" {
			return reporterFailure("evidence_invalid")
		}
	case hook.TerminalProductionDeploymentUnverified, hook.TerminalProductionVerificationFailed:
		if evidence.PullRequestURL == "" || evidence.CommitSHA == "" || evidence.StagingEvidenceURL == "" || evidence.ProductionEvidenceURL != "" {
			return reporterFailure("evidence_invalid")
		}
	case hook.TerminalReadinessRejected, hook.TerminalClarificationRequired, hook.TerminalReadinessUnresolved,
		hook.TerminalClarificationExpired, hook.TerminalCancelled:
		if evidence.PullRequestURL != "" || evidence.CommitSHA != "" || evidence.StagingEvidenceURL != "" || evidence.ProductionEvidenceURL != "" {
			return reporterFailure("evidence_invalid")
		}
	default:
		if evidence.ProductionEvidenceURL != "" {
			return reporterFailure("evidence_invalid")
		}
	}
	return nil
}

func loadGitHubIdentity(getenv func(string) string, envelope hook.DispatchEnvelope) (githubIdentity, error) {
	if getenv("GITHUB_ACTIONS") != "true" || getenv("GITHUB_SERVER_URL") != "https://github.com" ||
		!hook.ValidTriggerEvent(getenv("GITHUB_EVENT_NAME")) || getenv("GITHUB_REF") != "refs/heads/main" {
		return githubIdentity{}, reporterFailure("github_identity_invalid")
	}
	repository := getenv("GITHUB_REPOSITORY")
	workflowRef := getenv("GITHUB_WORKFLOW_REF")
	workflowSHA := getenv("GITHUB_SHA")
	if !repositoryPattern.MatchString(repository) || strings.TrimSpace(repository) != repository ||
		!validWorkflowRef(repository, workflowRef) || !commitPattern.MatchString(workflowSHA) {
		return githubIdentity{}, reporterFailure("github_identity_invalid")
	}
	repositoryID, err := positiveInt64(getenv("GITHUB_REPOSITORY_ID"))
	if err != nil {
		return githubIdentity{}, reporterFailure("github_identity_invalid")
	}
	workflowRunID, err := positiveInt64(getenv("GITHUB_RUN_ID"))
	if err != nil {
		return githubIdentity{}, reporterFailure("github_identity_invalid")
	}
	runAttempt64, err := positiveInt64(getenv("GITHUB_RUN_ATTEMPT"))
	if err != nil || runAttempt64 > maxRunAttempt {
		return githubIdentity{}, reporterFailure("github_identity_invalid")
	}
	repositoryDigest := hook.HashIdentity(repository)
	workflowDigest := hook.HashIdentity(workflowRef)
	if repositoryID != envelope.Snapshot.Target.RepositoryID || workflowDigest != envelope.Snapshot.Target.WorkflowRefSHA256 {
		return githubIdentity{}, reporterFailure("github_identity_invalid")
	}
	return githubIdentity{
		repository: repository, repositoryID: repositoryID, workflowSHA: workflowSHA,
		workflowRunID: workflowRunID, runAttempt: int(runAttempt64), repositoryDigest: repositoryDigest, workflowDigest: workflowDigest,
	}, nil
}

func validWorkflowRef(repository, workflowRef string) bool {
	if strings.TrimSpace(workflowRef) != workflowRef || strings.ContainsAny(workflowRef, "\x00\r\n") {
		return false
	}
	pattern := `^` + regexp.QuoteMeta(repository) + `/\.github/workflows/[A-Za-z0-9_.-]{1,100}\.ya?ml@refs/heads/main$`
	return regexp.MustCompile(pattern).MatchString(workflowRef)
}

func positiveInt64(value string) (int64, error) {
	if !decimalPattern.MatchString(value) {
		return 0, errors.New("invalid decimal")
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, errors.New("invalid decimal")
	}
	return parsed, nil
}

func validateCallbackURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Port() != "" ||
		!functionHostPattern.MatchString(parsed.Host) || parsed.Path != hook.TerminalReportPath || parsed.RawPath != "" ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.String() != value {
		return reporterFailure("callback_url_invalid")
	}
	return nil
}

func validHTTPSURL(value string) bool {
	if len(value) == 0 || len(value) > 2048 || strings.ContainsAny(value, "\x00\r\n") {
		return false
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Port() != "" || parsed.RawQuery != "" ||
		parsed.Fragment != "" || parsed.RawPath != "" || parsed.Path == "" || parsed.Path[0] != '/' ||
		path.Clean(parsed.Path) != parsed.Path || parsed.String() != value {
		return false
	}
	host := parsed.Hostname()
	return host == strings.ToLower(host) && strings.Contains(host, ".") && !strings.Contains(host, "..") &&
		dnsNamePattern.MatchString(host) && net.ParseIP(host) == nil
}

func readRegularFile(filePath string, maxBytes int64, private bool) ([]byte, error) {
	if !filepath.IsAbs(filePath) || filepath.Clean(filePath) != filePath || strings.ContainsAny(filePath, "\x00\r\n") {
		return nil, errors.New("file path is invalid")
	}
	before, err := os.Lstat(filePath)
	if err != nil || !before.Mode().IsRegular() || (private && before.Mode().Perm()&0o077 != 0) {
		return nil, errors.New("file is invalid")
	}
	file, err := os.Open(filePath)
	if err != nil {
		return nil, errors.New("file is invalid")
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return nil, errors.New("file is invalid")
	}
	encoded, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil || int64(len(encoded)) == 0 || int64(len(encoded)) > maxBytes {
		return nil, errors.New("file is invalid")
	}
	return encoded, nil
}

func newHTTPClient(transport http.RoundTripper) *http.Client {
	if transport == nil {
		transport = http.DefaultTransport
	}
	return &http.Client{
		Transport: transport,
		Timeout:   reportTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func submitTerminalReport(ctx context.Context, client *http.Client, callbackURL string, key []byte, report hook.TerminalReportRequest, now func() time.Time, retryWait func(context.Context, time.Duration) error) (hook.Result, error) {
	for attempt := 0; attempt < maxReportAttempts; attempt++ {
		report.IssuedAt = now().UTC()
		body, err := hook.MarshalTerminalReportRequest(report)
		if err != nil {
			return hook.Result{}, reporterFailure("request_invalid")
		}
		result, retryable, err := postTerminalReport(ctx, client, callbackURL, key, body, report.DeliveryID)
		if !retryable || attempt == maxReportAttempts-1 {
			return result, err
		}
		if err := retryWait(ctx, reportRetryDelay(attempt)); err != nil {
			return hook.Result{}, reporterFailure("report_unavailable")
		}
	}
	return hook.Result{}, reporterFailure("report_unavailable")
}

func reportRetryDelay(attempt int) time.Duration {
	return 125 * time.Second
}

func waitForReportRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func postTerminalReport(ctx context.Context, client *http.Client, callbackURL string, key, body []byte, deliveryID string) (hook.Result, bool, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, callbackURL, bytes.NewReader(body))
	if err != nil {
		return hook.Result{}, false, reporterFailure("request_invalid")
	}
	request.Header.Set("accept", "application/json")
	request.Header.Set("content-type", "application/json")
	request.Header.Set(hook.TerminalReportSignatureHeader, hook.SignTerminalReportRequest(key, body))
	response, err := client.Do(request)
	if err != nil || response == nil || response.Body == nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return hook.Result{}, true, reporterFailure("report_unavailable")
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(responseBody) > maxResponseBytes {
		return hook.Result{}, true, reporterFailure("response_invalid")
	}
	signatures := response.Header.Values(hook.TerminalReportResponseSignatureHeader)
	if len(signatures) != 1 || !hook.VerifyTerminalReportResponseSignature(key, response.StatusCode, body, responseBody, signatures[0]) {
		return hook.Result{}, true, reporterFailure("response_signature_invalid")
	}
	contentTypes := response.Header.Values("content-type")
	if len(contentTypes) != 1 {
		return hook.Result{}, false, reporterFailure("response_invalid")
	}
	mediaType, parameters, err := mime.ParseMediaType(contentTypes[0])
	if err != nil || mediaType != "application/json" || len(parameters) != 0 {
		return hook.Result{}, false, reporterFailure("response_invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	decoder.DisallowUnknownFields()
	var result hook.Result
	if err := decoder.Decode(&result); err != nil {
		return hook.Result{}, false, reporterFailure("response_invalid")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return hook.Result{}, false, reporterFailure("response_invalid")
	}
	canonical, err := json.Marshal(result)
	if err != nil || !bytes.Equal(canonical, responseBody) {
		return hook.Result{}, false, reporterFailure("response_invalid")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if response.StatusCode == http.StatusServiceUnavailable && result.Decision == hook.DecisionRetryRequested &&
			result.DeliveryID == deliveryID && retryableTerminalReportCode(result.Code) {
			return hook.Result{}, true, reporterFailure("report_unavailable")
		}
		// The receiver's code names which gate refused; without it a refusal
		// is undiagnosable from this side (measured 2026-08-06: a silent 403
		// cost a night). The response is remote input, so only a code in the
		// receiver's own enum shape is echoed; anything else is not repeated.
		return hook.Result{}, false, reporterFailure("report_rejected:" + strconv.Itoa(response.StatusCode) + ":" + safeRemoteCode(result.Code))
	}
	if result.Decision != hook.DecisionAccepted || result.DeliveryID != deliveryID {
		return hook.Result{}, false, reporterFailure("response_invalid")
	}
	if result.Code != "terminal_report_recorded" && result.Code != "terminal_report_already_recorded" {
		return hook.Result{}, false, reporterFailure("response_invalid")
	}
	return result, false, nil
}

func retryableTerminalReportCode(code string) bool {
	switch code {
	case "terminal_report_pending", "terminal_report_begin_failed", "terminal_comment_lookup_failed",
		"terminal_comment_add_failed", "terminal_report_complete_failed":
		return true
	default:
		return false
	}
}

// safeRemoteCode admits only the shape the receiver's own enum uses, so a
// hostile response cannot smuggle arbitrary text into this side's output.
func safeRemoteCode(code string) string {
	if len(code) == 0 || len(code) > 64 {
		return "unrecognized"
	}
	for _, r := range code {
		if r != '_' && (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return "unrecognized"
		}
	}
	return code
}
