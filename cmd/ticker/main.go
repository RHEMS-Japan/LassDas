// Command ticker wakes the clarification clock: one signed POST to the Lambda
// question-tick endpoint per scheduled run. Every decision — adopt an answer,
// cancel, remind, expire, or do nothing — is made Lambda-side from sealed
// state; this command only proves possession of the shared key and reports
// the outcome.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

import "automation.internal/ticket-ingress/internal/hook"

const (
	maxHMACKeyFileBytes = 1024
	maxResponseBytes    = 8 * 1024
	requestTimeout      = 60 * time.Second
)

var (
	functionHostPattern = regexp.MustCompile(`^[a-z0-9]+\.lambda-url\.ap-northeast-1\.on\.aws$`)
	runIDPattern        = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

type tickerFailure string

func (e tickerFailure) Error() string { return string(e) }

type commandOutput struct {
	Decision string `json:"decision"`
	Code     string `json:"code"`
}

func main() {
	output, err := run(os.Getenv, time.Now, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, `{"decision":"failed","code":"`+err.Error()+`"}`)
		os.Exit(1)
	}
	encoded, _ := json.Marshal(output)
	fmt.Println(string(encoded))
}

func run(getenv func(string) string, now func() time.Time, transport http.RoundTripper) (commandOutput, error) {
	if len(os.Args) > 1 {
		return commandOutput{}, tickerFailure("arguments_invalid")
	}
	tickURL := getenv("QUESTION_TICK_URL")
	runID := getenv("AUTOMATION_RUN_ID")
	keyPath := getenv("TICK_HMAC_KEY_FILE")
	if err := validateTickURL(tickURL); err != nil {
		return commandOutput{}, err
	}
	if !runIDPattern.MatchString(runID) {
		return commandOutput{}, tickerFailure("run_id_invalid")
	}
	keyFile, err := readRegularFile(keyPath, maxHMACKeyFileBytes, true)
	if err != nil {
		return commandOutput{}, tickerFailure("hmac_key_invalid")
	}
	defer clear(keyFile)
	key, err := hook.DecodePullHMACKey(string(keyFile))
	if err != nil {
		return commandOutput{}, tickerFailure("hmac_key_invalid")
	}
	defer clear(key)
	body, err := json.Marshal(hook.QuestionTickRequest{
		Protocol: hook.QuestionTickProtocol, AutomationRunID: runID, IssuedAt: now().UTC(),
	})
	if err != nil {
		return commandOutput{}, tickerFailure("request_invalid")
	}
	client := &http.Client{
		Transport: transportOrDefault(transport), Timeout: requestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, tickURL, bytes.NewReader(body))
	if err != nil {
		return commandOutput{}, tickerFailure("request_invalid")
	}
	request.Header.Set("accept", "application/json")
	request.Header.Set("content-type", "application/json")
	request.Header.Set(hook.QuestionTickSignatureHeader, hook.SignQuestionTickRequest(key, body))
	response, err := client.Do(request)
	if err != nil || response == nil || response.Body == nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return commandOutput{}, tickerFailure("tick_unavailable")
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(responseBody) > maxResponseBytes {
		return commandOutput{}, tickerFailure("response_invalid")
	}
	signatures := response.Header.Values(hook.QuestionTickResponseSignatureHeader)
	if len(signatures) != 1 || !hook.VerifyQuestionTickResponseSignature(key, response.StatusCode, body, responseBody, signatures[0]) {
		return commandOutput{}, tickerFailure("response_signature_invalid")
	}
	contentTypes := response.Header.Values("content-type")
	if len(contentTypes) != 1 {
		return commandOutput{}, tickerFailure("response_invalid")
	}
	mediaType, parameters, err := mime.ParseMediaType(contentTypes[0])
	if err != nil || mediaType != "application/json" || len(parameters) != 0 {
		return commandOutput{}, tickerFailure("response_invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	decoder.DisallowUnknownFields()
	var result hook.Result
	if err := decoder.Decode(&result); err != nil {
		return commandOutput{}, tickerFailure("response_invalid")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return commandOutput{}, tickerFailure("response_invalid")
	}
	canonical, err := json.Marshal(result)
	if err != nil || !bytes.Equal(canonical, responseBody) {
		return commandOutput{}, tickerFailure("response_invalid")
	}
	// A retry-requested tick is fine to leave to the next 5-minute wake-up:
	// every posting is exactly-once, so nothing is lost by waiting.
	if result.Decision != hook.DecisionAccepted && result.Decision != hook.DecisionRetryRequested {
		return commandOutput{Decision: string(result.Decision), Code: result.Code}, tickerFailure("tick_rejected")
	}
	return commandOutput{Decision: string(result.Decision), Code: result.Code}, nil
}

func validateTickURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Port() != "" ||
		!functionHostPattern.MatchString(parsed.Host) || parsed.Path != hook.QuestionTickPath || parsed.RawPath != "" ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.String() != value {
		return tickerFailure("tick_url_invalid")
	}
	return nil
}

func readRegularFile(filePath string, maxBytes int64, private bool) ([]byte, error) {
	if !filepath.IsAbs(filePath) || filepath.Clean(filePath) != filePath || strings.ContainsAny(filePath, "\x00\r\n") {
		return nil, tickerFailure("file_invalid")
	}
	before, err := os.Lstat(filePath)
	if err != nil || !before.Mode().IsRegular() || (private && before.Mode().Perm()&0o077 != 0) {
		return nil, tickerFailure("file_invalid")
	}
	file, err := os.Open(filePath)
	if err != nil {
		return nil, tickerFailure("file_invalid")
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return nil, tickerFailure("file_invalid")
	}
	encoded, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil || int64(len(encoded)) == 0 || int64(len(encoded)) > maxBytes {
		return nil, tickerFailure("file_invalid")
	}
	return encoded, nil
}

func transportOrDefault(transport http.RoundTripper) http.RoundTripper {
	if transport == nil {
		return http.DefaultTransport
	}
	return transport
}
