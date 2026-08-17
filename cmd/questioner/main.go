// Command questioner posts the clarification questions of a readiness
// decision to the Lambda question endpoint. It mirrors cmd/reporter: the
// sealed record is built from the envelope, the runner's GitHub identity and
// the readiness decision artifact, signed with the shared HMAC key, and
// submitted with bounded retries. The weekday notification schedule is
// computed here, at posting time, and sealed into the record.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
)

const (
	maxEnvelopeFileBytes = hook.MaxDeliveredEnvelopeBytes
	maxDecisionFileBytes = 256 * 1024
	maxHMACKeyFileBytes  = 1024
	maxResponseBytes     = 8 * 1024
	maxAttempts          = 3
	retryDelay           = 125 * time.Second
	requestTimeout       = 30 * time.Second
	maxRunAttempt        = 1000
)

var (
	repositoryPattern   = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}$`)
	commitPattern       = regexp.MustCompile(`^[a-f0-9]{40}$`)
	decimalPattern      = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)
	functionHostPattern = regexp.MustCompile(`^[a-z0-9]+\.lambda-url\.ap-northeast-1\.on\.aws$`)
)

type questionerFailure string

func (e questionerFailure) Error() string { return string(e) }

type commandOutput struct {
	Decision string `json:"decision"`
	Code     string `json:"code"`
}

func main() {
	output, err := run(os.Args[1:], os.Getenv, time.Now, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, `{"decision":"failed","code":"`+err.Error()+`"}`)
		os.Exit(1)
	}
	encoded, _ := json.Marshal(output)
	fmt.Println(string(encoded))
}

func run(args []string, getenv func(string) string, now func() time.Time, transport http.RoundTripper) (commandOutput, error) {
	return runWithRetryWait(args, getenv, now, transport, waitForRetry)
}

func runWithRetryWait(args []string, getenv func(string) string, now func() time.Time, transport http.RoundTripper, retryWait func(context.Context, time.Duration) error) (commandOutput, error) {
	flags, err := parseFlags(args)
	if err != nil {
		return commandOutput{}, err
	}
	if getenv == nil || now == nil || retryWait == nil {
		return commandOutput{}, questionerFailure("configuration_invalid")
	}
	envelope, err := loadEnvelope(flags.envelopeFile)
	if err != nil {
		return commandOutput{}, err
	}
	questionsJSON, decisionDigest, err := loadDecision(flags.decisionFile)
	if err != nil {
		return commandOutput{}, err
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
		return commandOutput{}, questionerFailure("hmac_key_invalid")
	}
	defer clear(keyFile)
	key, err := hook.DecodePullHMACKey(string(keyFile))
	if err != nil {
		return commandOutput{}, questionerFailure("hmac_key_invalid")
	}
	defer clear(key)

	revision, clarificationDigest, err := questionRevisionFromEnvelope(envelope)
	if err != nil {
		return commandOutput{}, err
	}
	notifyAt, deadlineAt := hook.ComputeQuestionSchedule(now().UTC())
	record := hook.QuestionRecord{
		Protocol:   hook.QuestionProtocolVersion,
		DeliveryID: envelope.DeliveryID, InputSHA256: envelope.Snapshot.InputSHA256,
		RepositoryID: identity.repositoryID, RepositorySHA256: identity.repositoryDigest,
		WorkflowRefSHA256: identity.workflowDigest, WorkflowSHA: identity.workflowSHA,
		WorkflowRunID: identity.workflowRunID, RunAttempt: identity.runAttempt,
		AutomationRunID: envelope.Snapshot.RunID,
		RunURL: "https://github.com/" + identity.repository + "/actions/runs/" +
			strconv.FormatInt(identity.workflowRunID, 10) + "/attempts/" + strconv.Itoa(identity.runAttempt),
		QuestionRevision:    revision,
		ClarificationSHA256: clarificationDigest,
		QuestionsJSON:       questionsJSON,
		QuestionsSHA256:     hook.TerminalReportDigest([]byte(questionsJSON)),
		DecisionSHA256:      decisionDigest,
		AnswerDeadlineAt:    deadlineAt,
		NotifyAt:            notifyAt,
	}
	if _, err := hook.MarshalQuestionRecord(record); err != nil {
		return commandOutput{}, questionerFailure("question_record_invalid")
	}
	client := &http.Client{
		Transport: transportOrDefault(transport), Timeout: requestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	result, err := submit(context.Background(), client, flags.callbackURL, key, record, now, retryWait)
	if err != nil {
		return commandOutput{}, err
	}
	return commandOutput{Decision: string(result.Decision), Code: result.Code}, nil
}

type commandFlags struct {
	envelopeFile string
	decisionFile string
	callbackURL  string
	hmacKeyFile  string
}

func parseFlags(args []string) (commandFlags, error) {
	if len(args) != 8 {
		return commandFlags{}, questionerFailure("arguments_invalid")
	}
	values := make(map[string]string, 4)
	allowed := map[string]bool{
		"--envelope-file": true,
		"--decision-file": true,
		"--callback-url":  true,
		"--hmac-key-file": true,
	}
	for index := 0; index < len(args); index += 2 {
		name, value := args[index], args[index+1]
		if !allowed[name] || value == "" || strings.ContainsAny(value, "\x00\r\n") {
			return commandFlags{}, questionerFailure("arguments_invalid")
		}
		if _, duplicate := values[name]; duplicate {
			return commandFlags{}, questionerFailure("arguments_invalid")
		}
		values[name] = value
	}
	return commandFlags{
		envelopeFile: values["--envelope-file"], decisionFile: values["--decision-file"],
		callbackURL: values["--callback-url"], hmacKeyFile: values["--hmac-key-file"],
	}, nil
}

func loadEnvelope(filePath string) (hook.DispatchEnvelope, error) {
	encoded, err := readRegularFile(filePath, maxEnvelopeFileBytes, false)
	if err != nil {
		return hook.DispatchEnvelope{}, questionerFailure("envelope_invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var envelope hook.DispatchEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return hook.DispatchEnvelope{}, questionerFailure("envelope_invalid")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return hook.DispatchEnvelope{}, questionerFailure("envelope_invalid")
	}
	canonical, err := json.Marshal(envelope)
	if err != nil || !bytes.Equal(canonical, encoded) || hook.ValidateEnvelope(envelope) != nil {
		return hook.DispatchEnvelope{}, questionerFailure("envelope_invalid")
	}
	return envelope, nil
}

// loadDecision extracts the clarification questions and the decision digest
// from the readiness decision artifact, and re-encodes the questions
// canonically so the sealed set is deterministic.
func loadDecision(filePath string) (string, string, error) {
	encoded, err := readRegularFile(filePath, maxDecisionFileBytes, false)
	if err != nil {
		return "", "", questionerFailure("decision_invalid")
	}
	var decision struct {
		Outcome        string           `json:"outcome"`
		Questions      []map[string]any `json:"questions"`
		DecisionSHA256 string           `json:"decision_sha256"`
	}
	if err := json.Unmarshal(encoded, &decision); err != nil {
		return "", "", questionerFailure("decision_invalid")
	}
	if decision.Outcome != "clarification_required" || len(decision.Questions) == 0 ||
		!regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(decision.DecisionSHA256) {
		return "", "", questionerFailure("decision_invalid")
	}
	questions, err := json.Marshal(decision.Questions)
	if err != nil {
		return "", "", questionerFailure("decision_invalid")
	}
	return string(questions), decision.DecisionSHA256, nil
}

// questionRevisionFromEnvelope derives the next question round from the
// sealed clarification the envelope carries: a never-resumed run asks round
// 1; a resumed run asks the round after its adopted answers, chained by
// digest to the sealed record. Past the round contract nothing may be asked.
func questionRevisionFromEnvelope(envelope hook.DispatchEnvelope) (int, string, error) {
	if envelope.ClarificationJSON == "" {
		return 1, "", nil
	}
	record, err := hook.DecodeClarificationRecord([]byte(envelope.ClarificationJSON))
	if err != nil {
		return 0, "", questionerFailure("clarification_invalid")
	}
	if record.InputRevision > hook.MaxClarificationRounds {
		return 0, "", questionerFailure("question_rounds_exhausted")
	}
	return record.InputRevision, hook.TerminalReportDigest([]byte(envelope.ClarificationJSON)), nil
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

func loadGitHubIdentity(getenv func(string) string, envelope hook.DispatchEnvelope) (githubIdentity, error) {
	if getenv("GITHUB_ACTIONS") != "true" || getenv("GITHUB_SERVER_URL") != "https://github.com" ||
		!hook.ValidTriggerEvent(getenv("GITHUB_EVENT_NAME")) || getenv("GITHUB_REF") != "refs/heads/main" {
		return githubIdentity{}, questionerFailure("github_identity_invalid")
	}
	repository := getenv("GITHUB_REPOSITORY")
	workflowRef := getenv("GITHUB_WORKFLOW_REF")
	workflowSHA := getenv("GITHUB_SHA")
	if !repositoryPattern.MatchString(repository) || strings.TrimSpace(repository) != repository ||
		!validWorkflowRef(repository, workflowRef) || !commitPattern.MatchString(workflowSHA) {
		return githubIdentity{}, questionerFailure("github_identity_invalid")
	}
	repositoryID, err := positiveInt64(getenv("GITHUB_REPOSITORY_ID"))
	if err != nil {
		return githubIdentity{}, questionerFailure("github_identity_invalid")
	}
	workflowRunID, err := positiveInt64(getenv("GITHUB_RUN_ID"))
	if err != nil {
		return githubIdentity{}, questionerFailure("github_identity_invalid")
	}
	runAttempt64, err := positiveInt64(getenv("GITHUB_RUN_ATTEMPT"))
	if err != nil || runAttempt64 > maxRunAttempt {
		return githubIdentity{}, questionerFailure("github_identity_invalid")
	}
	repositoryDigest := hook.HashIdentity(repository)
	workflowDigest := hook.HashIdentity(workflowRef)
	if repositoryID != envelope.Snapshot.Target.RepositoryID || workflowDigest != envelope.Snapshot.Target.WorkflowRefSHA256 {
		return githubIdentity{}, questionerFailure("github_identity_invalid")
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
		!functionHostPattern.MatchString(parsed.Host) || parsed.Path != hook.QuestionReportPath || parsed.RawPath != "" ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.String() != value {
		return questionerFailure("callback_url_invalid")
	}
	return nil
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

func transportOrDefault(transport http.RoundTripper) http.RoundTripper {
	if transport == nil {
		return http.DefaultTransport
	}
	return transport
}

func submit(ctx context.Context, client *http.Client, callbackURL string, key []byte, record hook.QuestionRecord, now func() time.Time, retryWait func(context.Context, time.Duration) error) (hook.Result, error) {
	for attempt := 0; attempt < maxAttempts; attempt++ {
		body, err := json.Marshal(hook.QuestionReportRequest{Record: record, IssuedAt: now().UTC()})
		if err != nil {
			return hook.Result{}, questionerFailure("request_invalid")
		}
		result, retryable, err := post(ctx, client, callbackURL, key, body, record.DeliveryID)
		if !retryable || attempt == maxAttempts-1 {
			return result, err
		}
		if err := retryWait(ctx, retryDelay); err != nil {
			return hook.Result{}, questionerFailure("report_unavailable")
		}
	}
	return hook.Result{}, questionerFailure("report_unavailable")
}

func post(ctx context.Context, client *http.Client, callbackURL string, key, body []byte, deliveryID string) (hook.Result, bool, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, callbackURL, bytes.NewReader(body))
	if err != nil {
		return hook.Result{}, false, questionerFailure("request_invalid")
	}
	request.Header.Set("accept", "application/json")
	request.Header.Set("content-type", "application/json")
	request.Header.Set(hook.QuestionReportSignatureHeader, hook.SignQuestionReportRequest(key, body))
	response, err := client.Do(request)
	if err != nil || response == nil || response.Body == nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return hook.Result{}, true, questionerFailure("report_unavailable")
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(responseBody) > maxResponseBytes {
		return hook.Result{}, true, questionerFailure("response_invalid")
	}
	signatures := response.Header.Values(hook.QuestionReportResponseSignatureHeader)
	if len(signatures) != 1 || !hook.VerifyQuestionReportResponseSignature(key, response.StatusCode, body, responseBody, signatures[0]) {
		return hook.Result{}, true, questionerFailure("response_signature_invalid")
	}
	contentTypes := response.Header.Values("content-type")
	if len(contentTypes) != 1 {
		return hook.Result{}, false, questionerFailure("response_invalid")
	}
	mediaType, parameters, err := mime.ParseMediaType(contentTypes[0])
	if err != nil || mediaType != "application/json" || len(parameters) != 0 {
		return hook.Result{}, false, questionerFailure("response_invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	decoder.DisallowUnknownFields()
	var result hook.Result
	if err := decoder.Decode(&result); err != nil {
		return hook.Result{}, false, questionerFailure("response_invalid")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return hook.Result{}, false, questionerFailure("response_invalid")
	}
	canonical, err := json.Marshal(result)
	if err != nil || !bytes.Equal(canonical, responseBody) {
		return hook.Result{}, false, questionerFailure("response_invalid")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if response.StatusCode == http.StatusServiceUnavailable && result.Decision == hook.DecisionRetryRequested &&
			result.DeliveryID == deliveryID && retryableQuestionReportCode(result.Code) {
			return hook.Result{}, true, questionerFailure("report_unavailable")
		}
		return hook.Result{}, false, questionerFailure("report_rejected")
	}
	if result.Decision != hook.DecisionAccepted || result.DeliveryID != deliveryID {
		return hook.Result{}, false, questionerFailure("response_invalid")
	}
	if result.Code != "question_report_recorded" && result.Code != "question_report_already_recorded" {
		return hook.Result{}, false, questionerFailure("response_invalid")
	}
	return result, false, nil
}

func retryableQuestionReportCode(code string) bool {
	switch code {
	case "question_report_pending", "question_report_begin_failed", "question_comment_lookup_failed",
		"question_comment_add_failed", "question_report_complete_failed":
		return true
	default:
		return false
	}
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
