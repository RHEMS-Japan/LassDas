package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
)

const (
	testRepository   = "example/automation-receiver"
	testRepositoryID = int64(123456)
	testWorkflowRef  = "example/automation-receiver/.github/workflows/receive-ticket.yml@refs/heads/main"
	testCallbackURL  = "https://abcdefghijklmnopqrst.lambda-url.ap-northeast-1.on.aws/terminal-report/v1"
	testSecret       = "REPORTER-SECRET-SENTINEL"
)

var reporterTestNow = time.Date(2026, 8, 3, 4, 5, 6, 0, time.UTC)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

type reporterFixture struct {
	envelope     hook.DispatchEnvelope
	envelopePath string
	evidencePath string
	keyPath      string
	key          []byte
	environment  map[string]string
	args         []string
}

func newReporterFixture(t *testing.T, code hook.TerminalCode) reporterFixture {
	t.Helper()
	directory := t.TempDir()
	envelope, err := hook.SealSnapshot(hook.TicketSnapshot{
		SchemaVersion: hook.SnapshotSchemaVersion,
		SpaceKey:      "example-space",
		ActivityID:    303,
		ActivityType:  1,
		ProjectID:     101,
		ProjectKey:    "TICKET",
		IssueID:       404,
		IssueKey:      "TICKET-505",
		IssueKeyID:    505,
		CreatorID:     202,
		RunID:         "run_20260803_reporter",
		CreatedAt:     reporterTestNow.Add(-time.Hour),
		Target: hook.DeliveryTarget{
			RepositoryID:      testRepositoryID,
			WorkflowRefSHA256: hook.HashIdentity(testWorkflowRef),
		},
		Untrusted: hook.UntrustedTicketData{Summary: "UNTRUSTED-TICKET-SUMMARY", Description: "$(UNTRUSTED-TICKET-BODY)"},
	})
	if err != nil {
		t.Fatal(err)
	}
	envelopePath := filepath.Join(directory, "envelope.json")
	writeCanonicalJSON(t, envelopePath, envelope, 0o600)
	evidence := evidenceMetadata{}
	if code == hook.TerminalSuccess || code == hook.TerminalProductionDeploymentUnverified || code == hook.TerminalProductionVerificationFailed {
		evidence = evidenceMetadata{
			PullRequestURL:     "https://github.com/example/target/pull/42",
			CommitSHA:          strings.Repeat("3", 40),
			CommitURL:          "https://github.com/example/target/commit/" + strings.Repeat("3", 40),
			StagingEvidenceURL: "https://staging.example.com/health/ready",
		}
	}
	if code == hook.TerminalSuccess {
		evidence.ProductionEvidenceURL = "https://www.example.com/health/ready"
	}
	evidencePath := filepath.Join(directory, "evidence.json")
	writeCanonicalJSON(t, evidencePath, evidence, 0o600)
	key := bytes.Repeat([]byte{0x42}, 32)
	keyPath := filepath.Join(directory, "hmac-key")
	if err := os.WriteFile(keyPath, []byte(base64.StdEncoding.EncodeToString(key)), 0o600); err != nil {
		t.Fatal(err)
	}
	environment := map[string]string{
		"GITHUB_ACTIONS":       "true",
		"GITHUB_SERVER_URL":    "https://github.com",
		"GITHUB_REPOSITORY":    testRepository,
		"GITHUB_REPOSITORY_ID": "123456",
		"GITHUB_EVENT_NAME":    "schedule",
		"GITHUB_REF":           "refs/heads/main",
		"GITHUB_WORKFLOW_REF":  testWorkflowRef,
		"GITHUB_SHA":           strings.Repeat("a", 40),
		"GITHUB_RUN_ID":        "987654321",
		"GITHUB_RUN_ATTEMPT":   "2",
	}
	args := []string{
		"--envelope-file", envelopePath,
		"--code", string(code),
		"--repository", "example/target",
		"--evidence-file", evidencePath,
		"--callback-url", testCallbackURL,
		"--hmac-key-file", keyPath,
	}
	return reporterFixture{
		envelope: envelope, envelopePath: envelopePath, evidencePath: evidencePath, keyPath: keyPath,
		key: key, environment: environment, args: args,
	}
}

func writeCanonicalJSON(t *testing.T, filePath string, value any, mode os.FileMode) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filePath, encoded, mode); err != nil {
		t.Fatal(err)
	}
}

func fixtureGetenv(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func readRequestBody(t *testing.T, request *http.Request) []byte {
	t.Helper()
	body, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func signedTerminalResponse(status int, key, requestBody []byte, result hook.Result) *http.Response {
	body, _ := json.Marshal(result)
	return &http.Response{
		StatusCode: status,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			http.CanonicalHeaderKey(hook.TerminalReportResponseSignatureHeader): []string{
				hook.SignTerminalReportResponse(key, status, requestBody, body),
			},
		},
		Body: io.NopCloser(bytes.NewReader(body)),
	}
}

func TestReporterSuccessBindsEnvelopeAndGitHubIdentity(t *testing.T) {
	fixture := newReporterFixture(t, hook.TerminalSuccess)
	calls := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.Method != http.MethodPost || request.URL.String() != testCallbackURL ||
			request.Header.Get("content-type") != "application/json" || request.Header.Get("accept") != "application/json" {
			t.Fatalf("unexpected request: method=%s url=%s headers=%v", request.Method, request.URL, request.Header)
		}
		body := readRequestBody(t, request)
		if !hook.VerifyTerminalReportRequestSignature(fixture.key, body, request.Header.Get(hook.TerminalReportSignatureHeader)) {
			t.Fatal("request signature did not verify")
		}
		report, err := hook.DecodeTerminalReportRequest(body)
		if err != nil {
			t.Fatalf("DecodeTerminalReportRequest() error = %v", err)
		}
		if report.DeliveryID != fixture.envelope.DeliveryID || report.InputSHA256 != fixture.envelope.Snapshot.InputSHA256 ||
			report.RepositoryID != testRepositoryID || report.RepositorySHA256 != hook.HashIdentity(testRepository) ||
			report.WorkflowRefSHA256 != hook.HashIdentity(testWorkflowRef) || report.WorkflowSHA != strings.Repeat("a", 40) ||
			report.WorkflowRunID != 987654321 || report.RunAttempt != 2 || report.AutomationRunID != fixture.envelope.Snapshot.RunID ||
			report.RunURL != "https://github.com/example/automation-receiver/actions/runs/987654321/attempts/2" ||
			report.IssuedAt != reporterTestNow {
			t.Fatalf("report identity was not bound: %+v", report)
		}
		return signedTerminalResponse(http.StatusOK, fixture.key, body, hook.Result{
			Decision: hook.DecisionAccepted, Code: "terminal_report_recorded", DeliveryID: fixture.envelope.DeliveryID,
		}), nil
	})
	result, err := run(fixture.args, fixtureGetenv(fixture.environment), func() time.Time { return reporterTestNow }, transport)
	if err != nil || result != (commandOutput{Decision: "accepted", Code: "terminal_report_recorded"}) || calls != 1 {
		t.Fatalf("result=%+v err=%v calls=%d", result, err, calls)
	}
}

func TestReporterAcceptsCanonicalEmptyEvidenceForPreProductionFailure(t *testing.T) {
	fixture := newReporterFixture(t, hook.TerminalValidationFailed)
	encoded, err := os.ReadFile(fixture.evidencePath)
	if err != nil || string(encoded) != `{}` {
		t.Fatalf("failure evidence=%q err=%v", encoded, err)
	}
	calls := 0
	result, err := run(fixture.args, fixtureGetenv(fixture.environment), func() time.Time { return reporterTestNow }, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		body := readRequestBody(t, request)
		return signedTerminalResponse(http.StatusOK, fixture.key, body, hook.Result{
			Decision: hook.DecisionAccepted, Code: "terminal_report_recorded", DeliveryID: fixture.envelope.DeliveryID,
		}), nil
	}))
	if err != nil || result.Decision != string(hook.DecisionAccepted) || calls != 1 {
		t.Fatalf("result=%+v err=%v calls=%d", result, err, calls)
	}
}

func TestReporterBindsDeployedProductionWhenVisibleVerificationFailed(t *testing.T) {
	fixture := newReporterFixture(t, hook.TerminalProductionVerificationFailed)
	calls := 0
	result, err := run(fixture.args, fixtureGetenv(fixture.environment), func() time.Time { return reporterTestNow }, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		body := readRequestBody(t, request)
		report, err := hook.DecodeTerminalReportRequest(body)
		if err != nil {
			t.Fatal(err)
		}
		if report.Code != hook.TerminalProductionVerificationFailed || report.PullRequestURL == "" || report.CommitSHA == "" ||
			report.StagingEvidenceURL == "" || report.ProductionEvidenceURL != "" {
			t.Fatalf("production verification report=%+v", report)
		}
		return signedTerminalResponse(http.StatusOK, fixture.key, body, hook.Result{
			Decision: hook.DecisionAccepted, Code: "terminal_report_recorded", DeliveryID: fixture.envelope.DeliveryID,
		}), nil
	}))
	if err != nil || result.Decision != string(hook.DecisionAccepted) || calls != 1 {
		t.Fatalf("result=%+v err=%v calls=%d", result, err, calls)
	}
}

func TestReporterBindsPromotionWhenProductionDeploymentIsUnverified(t *testing.T) {
	fixture := newReporterFixture(t, hook.TerminalProductionDeploymentUnverified)
	calls := 0
	result, err := run(fixture.args, fixtureGetenv(fixture.environment), func() time.Time { return reporterTestNow }, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		body := readRequestBody(t, request)
		report, err := hook.DecodeTerminalReportRequest(body)
		if err != nil {
			t.Fatal(err)
		}
		if report.Code != hook.TerminalProductionDeploymentUnverified || report.PullRequestURL == "" || report.CommitSHA == "" ||
			report.StagingEvidenceURL == "" || report.ProductionEvidenceURL != "" {
			t.Fatalf("production deployment uncertainty report=%+v", report)
		}
		return signedTerminalResponse(http.StatusOK, fixture.key, body, hook.Result{
			Decision: hook.DecisionAccepted, Code: "terminal_report_recorded", DeliveryID: fixture.envelope.DeliveryID,
		}), nil
	}))
	if err != nil || result.Decision != string(hook.DecisionAccepted) || calls != 1 {
		t.Fatalf("result=%+v err=%v calls=%d", result, err, calls)
	}
}

func TestReporterAcceptsOnlyExplicitFiniteFlags(t *testing.T) {
	fixture := newReporterFixture(t, hook.TerminalValidationFailed)
	tests := map[string][]string{
		"missing":   fixture.args[:10],
		"unknown":   append(append([]string(nil), fixture.args[:10]...), "--unknown", fixture.keyPath),
		"duplicate": append(append([]string(nil), fixture.args[:10]...), "--code", string(hook.TerminalSuccess)),
		"equals": []string{
			"--envelope-file=" + fixture.envelopePath, "--code", string(hook.TerminalValidationFailed),
			"--evidence-file", fixture.evidencePath, "--callback-url", testCallbackURL, "--hmac-key-file", fixture.keyPath, "extra",
		},
	}
	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := run(args, fixtureGetenv(fixture.environment), time.Now, roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Fatal("network called for invalid arguments")
				return nil, nil
			}))
			if errorCode(err) != "arguments_invalid" {
				t.Fatalf("error=%v code=%s", err, errorCode(err))
			}
		})
	}
	args := append([]string(nil), fixture.args...)
	args[3] = "arbitrary_ticket_code"
	if _, err := run(args, fixtureGetenv(fixture.environment), time.Now, nil); errorCode(err) != "terminal_code_invalid" {
		t.Fatalf("unknown code error=%v code=%s", err, errorCode(err))
	}
}

func TestReporterRejectsGitHubIdentitySubstitutionBeforeNetwork(t *testing.T) {
	tests := map[string]func(map[string]string){
		"not actions":   func(v map[string]string) { v["GITHUB_ACTIONS"] = "false" },
		"server":        func(v map[string]string) { v["GITHUB_SERVER_URL"] = "https://attacker.invalid" },
		"repository":    func(v map[string]string) { v["GITHUB_REPOSITORY"] = "attacker/repository" },
		"repository id": func(v map[string]string) { v["GITHUB_REPOSITORY_ID"] = "654321" },
		"repository pair": func(v map[string]string) {
			v["GITHUB_REPOSITORY"] = "attacker/repository"
			v["GITHUB_WORKFLOW_REF"] = "attacker/repository/.github/workflows/receive-ticket.yml@refs/heads/main"
		},
		"event": func(v map[string]string) { v["GITHUB_EVENT_NAME"] = "push" },
		"ref":   func(v map[string]string) { v["GITHUB_REF"] = "refs/heads/stg" },
		"workflow ref": func(v map[string]string) {
			v["GITHUB_WORKFLOW_REF"] = testRepository + "/.github/workflows/other.yml@refs/heads/main"
		},
		"workflow sha":     func(v map[string]string) { v["GITHUB_SHA"] = strings.Repeat("A", 40) },
		"run leading zero": func(v map[string]string) { v["GITHUB_RUN_ID"] = "0987654321" },
		"attempt zero":     func(v map[string]string) { v["GITHUB_RUN_ATTEMPT"] = "0" },
		"attempt too large": func(v map[string]string) {
			v["GITHUB_RUN_ATTEMPT"] = "1000001"
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := newReporterFixture(t, hook.TerminalValidationFailed)
			mutate(fixture.environment)
			calls := 0
			_, err := run(fixture.args, fixtureGetenv(fixture.environment), time.Now, roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return nil, errors.New(testSecret)
			}))
			if errorCode(err) != "github_identity_invalid" || calls != 0 || strings.Contains(err.Error(), testSecret) {
				t.Fatalf("error=%v code=%s calls=%d", err, errorCode(err), calls)
			}
		})
	}
}

func TestReporterRejectsEvidenceInjectionBeforeNetwork(t *testing.T) {
	tests := map[string]func(*evidenceMetadata, *hook.TerminalCode){
		"missing success evidence": func(e *evidenceMetadata, _ *hook.TerminalCode) { e.PullRequestURL = "" },
		"staging without commit": func(e *evidenceMetadata, _ *hook.TerminalCode) {
			e.CommitSHA = ""
			e.CommitURL = ""
			e.ProductionEvidenceURL = ""
		},
		"different repositories": func(e *evidenceMetadata, _ *hook.TerminalCode) {
			e.CommitURL = "https://github.com/attacker/target/commit/" + e.CommitSHA
		},
		"pull query": func(e *evidenceMetadata, _ *hook.TerminalCode) { e.PullRequestURL += "?body=UNTRUSTED" },
		"http evidence": func(e *evidenceMetadata, _ *hook.TerminalCode) {
			e.StagingEvidenceURL = "http://staging.example.com/health/ready"
		},
		"evidence query": func(e *evidenceMetadata, _ *hook.TerminalCode) { e.StagingEvidenceURL += "?secret=value" },
		"same origins": func(e *evidenceMetadata, _ *hook.TerminalCode) {
			e.ProductionEvidenceURL = "https://staging.example.com/other"
		},
		"failure production claim": func(e *evidenceMetadata, code *hook.TerminalCode) { *code = hook.TerminalReleaseFailed },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := newReporterFixture(t, hook.TerminalSuccess)
			var evidence evidenceMetadata
			encoded, _ := os.ReadFile(fixture.evidencePath)
			if err := json.Unmarshal(encoded, &evidence); err != nil {
				t.Fatal(err)
			}
			code := hook.TerminalSuccess
			mutate(&evidence, &code)
			writeCanonicalJSON(t, fixture.evidencePath, evidence, 0o600)
			fixture.args[3] = string(code)
			calls := 0
			_, err := run(fixture.args, fixtureGetenv(fixture.environment), time.Now, roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return nil, nil
			}))
			if errorCode(err) != "evidence_invalid" || calls != 0 {
				t.Fatalf("error=%v code=%s calls=%d", err, errorCode(err), calls)
			}
		})
	}
}

func TestReporterRejectsNoncanonicalInputAndUnsafeFiles(t *testing.T) {
	t.Run("envelope whitespace", func(t *testing.T) {
		fixture := newReporterFixture(t, hook.TerminalValidationFailed)
		encoded, _ := os.ReadFile(fixture.envelopePath)
		if err := os.WriteFile(fixture.envelopePath, append(encoded, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := run(fixture.args, fixtureGetenv(fixture.environment), time.Now, nil)
		if errorCode(err) != "envelope_invalid" {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("evidence unknown field", func(t *testing.T) {
		fixture := newReporterFixture(t, hook.TerminalValidationFailed)
		if err := os.WriteFile(fixture.evidencePath, []byte(`{"pull_request_url":"","commit_sha":"","commit_url":"","staging_evidence_url":"","production_evidence_url":"","comment":"UNTRUSTED"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := run(fixture.args, fixtureGetenv(fixture.environment), time.Now, nil)
		if errorCode(err) != "evidence_invalid" {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("key permissions", func(t *testing.T) {
		fixture := newReporterFixture(t, hook.TerminalValidationFailed)
		if err := os.Chmod(fixture.keyPath, 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := run(fixture.args, fixtureGetenv(fixture.environment), time.Now, nil)
		if errorCode(err) != "hmac_key_invalid" {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("key symlink", func(t *testing.T) {
		fixture := newReporterFixture(t, hook.TerminalValidationFailed)
		link := filepath.Join(t.TempDir(), "key-link")
		if err := os.Symlink(fixture.keyPath, link); err != nil {
			t.Fatal(err)
		}
		fixture.args[11] = link
		_, err := run(fixture.args, fixtureGetenv(fixture.environment), time.Now, nil)
		if errorCode(err) != "hmac_key_invalid" {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("key content", func(t *testing.T) {
		fixture := newReporterFixture(t, hook.TerminalValidationFailed)
		if err := os.WriteFile(fixture.keyPath, []byte(testSecret), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := run(fixture.args, fixtureGetenv(fixture.environment), time.Now, nil)
		if errorCode(err) != "hmac_key_invalid" || strings.Contains(err.Error(), testSecret) {
			t.Fatalf("error=%v", err)
		}
	})
}

func TestReporterAllowsOnlyFixedLambdaCallback(t *testing.T) {
	tests := []string{
		"https://attacker.invalid/terminal-report/v1",
		"http://abcdefghijklmnopqrst.lambda-url.ap-northeast-1.on.aws/terminal-report/v1",
		"https://abcdefghijklmnopqrst.lambda-url.us-east-1.on.aws/terminal-report/v1",
		"https://abcdefghijklmnopqrst.lambda-url.ap-northeast-1.on.aws/pull-claim/v1",
		"https://abcdefghijklmnopqrst.lambda-url.ap-northeast-1.on.aws/terminal-report/v1?secret=value",
	}
	for _, callback := range tests {
		fixture := newReporterFixture(t, hook.TerminalValidationFailed)
		fixture.args[9] = callback
		calls := 0
		_, err := run(fixture.args, fixtureGetenv(fixture.environment), time.Now, roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return nil, nil
		}))
		if errorCode(err) != "callback_url_invalid" || calls != 0 {
			t.Fatalf("callback=%q error=%v calls=%d", callback, err, calls)
		}
	}
}

func TestReporterAuthenticatesAndBoundsEveryResponse(t *testing.T) {
	tests := map[string]func(*reporterFixture, []byte) (*http.Response, error){
		"transport error": func(*reporterFixture, []byte) (*http.Response, error) { return nil, errors.New(testSecret) },
		"oversized": func(*reporterFixture, []byte) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(strings.Repeat(testSecret, 1000)))}, nil
		},
		"missing signature": func(f *reporterFixture, body []byte) (*http.Response, error) {
			response := signedTerminalResponse(200, f.key, body, hook.Result{Decision: hook.DecisionAccepted, Code: "terminal_report_recorded", DeliveryID: f.envelope.DeliveryID})
			response.Header.Del(hook.TerminalReportResponseSignatureHeader)
			return response, nil
		},
		"duplicate signature": func(f *reporterFixture, body []byte) (*http.Response, error) {
			response := signedTerminalResponse(200, f.key, body, hook.Result{Decision: hook.DecisionAccepted, Code: "terminal_report_recorded", DeliveryID: f.envelope.DeliveryID})
			response.Header.Add(hook.TerminalReportResponseSignatureHeader, hook.SignTerminalReportResponse(f.key, 200, body, []byte("other")))
			return response, nil
		},
		"wrong signature": func(f *reporterFixture, body []byte) (*http.Response, error) {
			response := signedTerminalResponse(200, f.key, body, hook.Result{Decision: hook.DecisionAccepted, Code: "terminal_report_recorded", DeliveryID: f.envelope.DeliveryID})
			response.Header.Set(hook.TerminalReportResponseSignatureHeader, "sha256="+strings.Repeat("0", 64))
			return response, nil
		},
		"signed rejection": func(f *reporterFixture, body []byte) (*http.Response, error) {
			return signedTerminalResponse(503, f.key, body, hook.Result{Decision: hook.DecisionInvalid, Code: "REMOTE-" + testSecret}), nil
		},
		"unrecognized retry code": func(f *reporterFixture, body []byte) (*http.Response, error) {
			return signedTerminalResponse(503, f.key, body, hook.Result{
				Decision: hook.DecisionRetryRequested, Code: "REMOTE-" + testSecret, DeliveryID: f.envelope.DeliveryID,
			}), nil
		},
		"wrong decision": func(f *reporterFixture, body []byte) (*http.Response, error) {
			return signedTerminalResponse(200, f.key, body, hook.Result{Decision: hook.DecisionInvalid, Code: "terminal_report_recorded", DeliveryID: f.envelope.DeliveryID}), nil
		},
		"wrong code": func(f *reporterFixture, body []byte) (*http.Response, error) {
			return signedTerminalResponse(200, f.key, body, hook.Result{Decision: hook.DecisionAccepted, Code: "queue_created", DeliveryID: f.envelope.DeliveryID}), nil
		},
		"wrong delivery": func(f *reporterFixture, body []byte) (*http.Response, error) {
			return signedTerminalResponse(200, f.key, body, hook.Result{Decision: hook.DecisionAccepted, Code: "terminal_report_recorded", DeliveryID: "delivery_00000000000000000000000000000000"}), nil
		},
		"wrong content type": func(f *reporterFixture, body []byte) (*http.Response, error) {
			response := signedTerminalResponse(200, f.key, body, hook.Result{Decision: hook.DecisionAccepted, Code: "terminal_report_recorded", DeliveryID: f.envelope.DeliveryID})
			response.Header.Set("content-type", "text/plain")
			return response, nil
		},
		"noncanonical": func(f *reporterFixture, body []byte) (*http.Response, error) {
			response := signedTerminalResponse(200, f.key, body, hook.Result{Decision: hook.DecisionAccepted, Code: "terminal_report_recorded", DeliveryID: f.envelope.DeliveryID})
			encoded, _ := io.ReadAll(response.Body)
			encoded = append(encoded, '\n')
			response.Body = io.NopCloser(bytes.NewReader(encoded))
			response.Header.Set(hook.TerminalReportResponseSignatureHeader, hook.SignTerminalReportResponse(f.key, 200, body, encoded))
			return response, nil
		},
	}
	for name, makeResponse := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := newReporterFixture(t, hook.TerminalValidationFailed)
			calls := 0
			_, err := runWithRetryWait(fixture.args, fixtureGetenv(fixture.environment), func() time.Time { return reporterTestNow }, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				return makeResponse(&fixture, readRequestBody(t, request))
			}), func(context.Context, time.Duration) error { return nil })
			wantCalls := 1
			switch name {
			case "transport error", "oversized", "missing signature", "duplicate signature", "wrong signature":
				wantCalls = maxReportAttempts
			}
			if err == nil || calls != wantCalls || strings.Contains(err.Error(), testSecret) || strings.Contains(errorCode(err), testSecret) {
				t.Fatalf("error=%v code=%s calls=%d", err, errorCode(err), calls)
			}
		})
	}
}

func TestReporterRetriesOnlyAuthenticatedRetryRequestsWithFreshTimestamp(t *testing.T) {
	fixture := newReporterFixture(t, hook.TerminalValidationFailed)
	calls := 0
	nowCalls := 0
	retryDelays := make([]time.Duration, 0, 1)
	requestBodies := make([][]byte, 0, 2)
	recordDigests := make([]string, 0, 2)
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		body := readRequestBody(t, request)
		requestBodies = append(requestBodies, append([]byte(nil), body...))
		report, err := hook.DecodeTerminalReportRequest(body)
		if err != nil {
			t.Fatal(err)
		}
		record, err := hook.MarshalTerminalReportRecord(report)
		if err != nil {
			t.Fatal(err)
		}
		recordDigests = append(recordDigests, hook.TerminalReportDigest(record))
		if calls == 1 {
			return signedTerminalResponse(http.StatusServiceUnavailable, fixture.key, body, hook.Result{
				Decision: hook.DecisionRetryRequested, Code: "terminal_report_pending", DeliveryID: fixture.envelope.DeliveryID,
			}), nil
		}
		return signedTerminalResponse(http.StatusOK, fixture.key, body, hook.Result{
			Decision: hook.DecisionAccepted, Code: "terminal_report_already_recorded", DeliveryID: fixture.envelope.DeliveryID,
		}), nil
	})
	result, err := runWithRetryWait(fixture.args, fixtureGetenv(fixture.environment), func() time.Time {
		value := reporterTestNow.Add(time.Duration(nowCalls) * time.Second)
		nowCalls++
		return value
	}, transport, func(_ context.Context, delay time.Duration) error {
		retryDelays = append(retryDelays, delay)
		return nil
	})
	if err != nil || result != (commandOutput{Decision: "accepted", Code: "terminal_report_already_recorded"}) || calls != 2 || nowCalls != 2 {
		t.Fatalf("result=%+v err=%v calls=%d now_calls=%d", result, err, calls, nowCalls)
	}
	if len(retryDelays) != 1 || retryDelays[0] != 125*time.Second || bytes.Equal(requestBodies[0], requestBodies[1]) || recordDigests[0] != recordDigests[1] {
		t.Fatal("retry did not refresh only the attempt timestamp")
	}
}

func TestReporterDoesNotFollowRedirects(t *testing.T) {
	fixture := newReporterFixture(t, hook.TerminalValidationFailed)
	calls := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		body := readRequestBody(t, request)
		response := signedTerminalResponse(302, fixture.key, body, hook.Result{Decision: hook.DecisionInvalid, Code: "redirect"})
		response.Header.Set("location", "https://attacker.invalid/steal")
		response.Request = request
		return response, nil
	})
	_, err := run(fixture.args, fixtureGetenv(fixture.environment), func() time.Time { return reporterTestNow }, transport)
	if errorCode(err) != "report_rejected:302:redirect" || calls != 1 {
		t.Fatalf("error=%v code=%s calls=%d", err, errorCode(err), calls)
	}
}

func TestReporterHTTPClientHasFixedTimeout(t *testing.T) {
	client := newHTTPClient(roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, context.Canceled }))
	if client.Timeout != reportTimeout || client.CheckRedirect == nil {
		t.Fatalf("client timeout=%s redirect_configured=%t", client.Timeout, client.CheckRedirect != nil)
	}
}

func TestValidTerminalCodeAndEvidenceCoverProtocolFiniteSetOnly(t *testing.T) {
	needsRepositoryEvidence := map[hook.TerminalCode]bool{
		hook.TerminalSuccess:                        true,
		hook.TerminalProductionDeploymentUnverified: true,
		hook.TerminalProductionVerificationFailed:   true,
	}
	for _, code := range []hook.TerminalCode{
		hook.TerminalSuccess, hook.TerminalInputRejected, hook.TerminalReadinessRejected, hook.TerminalClarificationRequired,
		hook.TerminalReadinessUnresolved, hook.TerminalClarificationExpired, hook.TerminalCancelled,
		hook.TerminalModelFailed, hook.TerminalNonconverged,
		hook.TerminalValidationFailed, hook.TerminalReleaseFailed, hook.TerminalProductionDeploymentUnverified,
		hook.TerminalProductionVerificationFailed, hook.TerminalInternalFailed,
	} {
		if !validTerminalCode(code) {
			t.Fatalf("finite code %q rejected by reporter", code)
		}
		err := validateEvidence(evidenceMetadata{}, code)
		if needsRepositoryEvidence[code] && err == nil {
			t.Fatalf("code %q accepted empty evidence", code)
		}
		if !needsRepositoryEvidence[code] && err != nil {
			t.Fatalf("code %q rejected empty evidence: %v", code, err)
		}
	}
	if validTerminalCode(hook.TerminalCode("ticket_text_controls_comment")) {
		t.Fatal("unknown terminal code accepted by reporter")
	}
}

// A failure before any repository work names no destination. The receiving
// protocol accepts the empty repository explicitly; the sender refusing it
// made the very first live failure report die unreported (2026-08-06).
func TestReporterReportsAFailureThatNamesNoDestination(t *testing.T) {
	fixture := newReporterFixture(t, hook.TerminalClarificationRequired)
	for index, argument := range fixture.args {
		if argument == "--repository" {
			fixture.args[index+1] = ""
		}
	}
	calls := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		body := readRequestBody(t, request)
		var report hook.TerminalReportRequest
		if err := json.Unmarshal(body, &report); err != nil {
			t.Fatalf("request did not decode: %v", err)
		}
		if report.Repository != "" || report.Code != hook.TerminalClarificationRequired {
			t.Fatalf("report = %+v", report)
		}
		return signedTerminalResponse(200, fixture.key, body, hook.Result{
			Decision: hook.DecisionAccepted, Code: "terminal_report_recorded", DeliveryID: report.DeliveryID,
		}), nil
	})
	output, err := run(fixture.args, fixtureGetenv(fixture.environment), func() time.Time { return reporterTestNow }, transport)
	if err != nil || calls != 1 {
		t.Fatalf("run() error = %v calls = %d", err, calls)
	}
	if output.Code != "terminal_report_recorded" {
		t.Fatalf("output = %+v", output)
	}
}
