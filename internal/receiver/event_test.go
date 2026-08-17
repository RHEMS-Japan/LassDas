package receiver

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
)

const (
	testRepository  = "example/ticket-automation"
	testWorkflowRef = "example/ticket-automation/.github/workflows/receive-backlog-ticket.yml@refs/heads/main"
)

var testHMACKey = []byte("0123456789abcdef0123456789abcdef")

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func validConfig() Config {
	workflowRefDigest := identityDigest(testWorkflowRef)
	return Config{
		ClaimURL: "https://abc123.lambda-url.ap-northeast-1.on.aws" + hook.PullClaimPath,
		HMACKey:  append([]byte(nil), testHMACKey...),
		Request: hook.PullRequest{
			Protocol:          hook.PullProtocolVersion,
			RepositoryID:      123456,
			RepositorySHA256:  identityDigest(testRepository),
			EventName:         "schedule",
			WorkflowRefSHA256: workflowRefDigest,
			Ref:               "refs/heads/main",
			WorkflowSHA:       strings.Repeat("a", 40),
			WorkflowRunID:     987654,
			RunAttempt:        1,
			AutomationRunID:   "run_20260802_alpha",
		},
		SpaceKey:            "example",
		ProjectID:           909057,
		ProjectKey:          "TICKET",
		AllowedCreatorID:    9903853,
		AllowedActivityType: 1,
		Target: hook.DeliveryTarget{
			RepositoryID:      123456,
			WorkflowRefSHA256: workflowRefDigest,
		},
		Timeout: 15 * time.Second,
	}
}

func validEnvironment() map[string]string {
	return map[string]string{
		"PULL_CLAIM_URL":         "https://abc123.lambda-url.ap-northeast-1.on.aws" + hook.PullClaimPath,
		"PULL_HMAC_KEY":          base64.StdEncoding.EncodeToString(testHMACKey),
		"GITHUB_REPOSITORY_ID":   "123456",
		"GITHUB_REPOSITORY":      testRepository,
		"GITHUB_EVENT_NAME":      "schedule",
		"GITHUB_WORKFLOW_REF":    testWorkflowRef,
		"GITHUB_REF":             "refs/heads/main",
		"GITHUB_SHA":             strings.Repeat("a", 40),
		"GITHUB_RUN_ID":          "987654",
		"GITHUB_RUN_ATTEMPT":     "1",
		"AUTOMATION_RUN_ID":      "run_20260802_alpha",
		"EXPECTED_SPACE_KEY":     "example",
		"EXPECTED_PROJECT_ID":    "909057",
		"EXPECTED_PROJECT_KEY":   "TICKET",
		"EXPECTED_CREATOR_ID":    "9903853",
		"EXPECTED_ACTIVITY_TYPE": "1",
	}
}

func validEnvelope(t *testing.T, config Config) hook.DispatchEnvelope {
	t.Helper()
	snapshot := hook.TicketSnapshot{
		SchemaVersion: hook.SnapshotSchemaVersion,
		SpaceKey:      config.SpaceKey,
		ActivityID:    9001,
		ActivityType:  config.AllowedActivityType,
		ProjectID:     config.ProjectID,
		ProjectKey:    config.ProjectKey,
		IssueID:       8001,
		IssueKey:      config.ProjectKey + "-501",
		IssueKeyID:    501,
		CreatorID:     config.AllowedCreatorID,
		RunID:         config.ProjectKey + "-501",
		CreatedAt:     time.Date(2026, 8, 2, 1, 2, 3, 0, time.UTC),
		Target:        config.Target,
		Untrusted: hook.UntrustedTicketData{
			Summary:     "sample ticket",
			Description: "Automation-Run-ID: run_20260802_alpha\nperform the requested change",
		},
	}
	envelope, err := hook.SealSnapshot(snapshot)
	if err != nil {
		t.Fatalf("SealSnapshot() error = %v", err)
	}
	return envelope
}

func TestLoadConfigHashesGitHubIdentities(t *testing.T) {
	values := validEnvironment()
	config, err := LoadConfig(func(key string) string { return values[key] })
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}

	if got, want := config.Request.RepositorySHA256, identityDigest(values["GITHUB_REPOSITORY"]); got != want {
		t.Fatalf("repository digest = %q, want %q", got, want)
	}
	if got, want := config.Request.WorkflowRefSHA256, identityDigest(values["GITHUB_WORKFLOW_REF"]); got != want {
		t.Fatalf("workflow ref digest = %q, want %q", got, want)
	}
	if config.Request.RepositorySHA256 == values["GITHUB_REPOSITORY"] || config.Request.WorkflowRefSHA256 == values["GITHUB_WORKFLOW_REF"] {
		t.Fatal("LoadConfig() retained a plaintext GitHub identity")
	}
	if config.Target.RepositoryID != config.Request.RepositoryID || config.Target.WorkflowRefSHA256 != config.Request.WorkflowRefSHA256 {
		t.Fatalf("target = %+v, request = %+v", config.Target, config.Request)
	}
	if !bytes.Equal(config.HMACKey, testHMACKey) {
		t.Fatal("LoadConfig() decoded an unexpected HMAC key")
	}

	request := config.Request
	request.IssuedAt = time.Date(2026, 8, 2, 3, 4, 5, 0, time.UTC)
	encoded, err := hook.MarshalPullRequest(request)
	if err != nil {
		t.Fatalf("MarshalPullRequest() error = %v", err)
	}
	if bytes.Contains(encoded, []byte(values["GITHUB_REPOSITORY"])) || bytes.Contains(encoded, []byte(values["GITHUB_WORKFLOW_REF"])) {
		t.Fatalf("pull request exposed a plaintext GitHub identity: %s", encoded)
	}
}

func TestLoadConfigFailsClosed(t *testing.T) {
	if _, err := LoadConfig(nil); err == nil || err.Error() != "receiver configuration is invalid" {
		t.Fatalf("LoadConfig(nil) error = %v", err)
	}

	for _, key := range []string{
		"PULL_CLAIM_URL",
		"PULL_HMAC_KEY",
		"GITHUB_REPOSITORY_ID",
		"GITHUB_REPOSITORY",
		"GITHUB_EVENT_NAME",
		"GITHUB_WORKFLOW_REF",
		"GITHUB_REF",
		"GITHUB_SHA",
		"GITHUB_RUN_ID",
		"GITHUB_RUN_ATTEMPT",
		"AUTOMATION_RUN_ID",
		"EXPECTED_SPACE_KEY",
		"EXPECTED_PROJECT_ID",
		"EXPECTED_PROJECT_KEY",
		"EXPECTED_CREATOR_ID",
		"EXPECTED_ACTIVITY_TYPE",
	} {
		t.Run(key, func(t *testing.T) {
			values := validEnvironment()
			values[key] = ""
			_, err := LoadConfig(func(name string) string { return values[name] })
			if err == nil || err.Error() != "receiver configuration is invalid" {
				t.Fatalf("LoadConfig() error = %v", err)
			}
		})
	}

	const secretSentinel = "SECRET-SENTINEL-MUST-NOT-LEAK"
	values := validEnvironment()
	values["PULL_HMAC_KEY"] = secretSentinel
	_, err := LoadConfig(func(name string) string { return values[name] })
	if err == nil || strings.Contains(err.Error(), secretSentinel) {
		t.Fatalf("LoadConfig() error = %v", err)
	}
}

func TestValidateClaimURLRequiresExactAWSFunctionURL(t *testing.T) {
	valid := "https://abc123.lambda-url.ap-northeast-1.on.aws" + hook.PullClaimPath
	if err := validateClaimURL(valid); err != nil {
		t.Fatalf("validateClaimURL(valid) error = %v", err)
	}

	invalid := map[string]string{
		"http":           "http://abc123.lambda-url.ap-northeast-1.on.aws" + hook.PullClaimPath,
		"wrong region":   "https://abc123.lambda-url.us-east-1.on.aws" + hook.PullClaimPath,
		"host suffix":    "https://abc123.lambda-url.ap-northeast-1.on.aws.evil.example" + hook.PullClaimPath,
		"userinfo":       "https://user@abc123.lambda-url.ap-northeast-1.on.aws" + hook.PullClaimPath,
		"port":           "https://abc123.lambda-url.ap-northeast-1.on.aws:443" + hook.PullClaimPath,
		"wrong path":     "https://abc123.lambda-url.ap-northeast-1.on.aws/other",
		"trailing slash": valid + "/",
		"escaped path":   "https://abc123.lambda-url.ap-northeast-1.on.aws/%70ull-claim/v1",
		"query":          valid + "?claim=1",
		"fragment":       valid + "#claim",
		"missing id":     "https://lambda-url.ap-northeast-1.on.aws" + hook.PullClaimPath,
	}
	for name, value := range invalid {
		t.Run(name, func(t *testing.T) {
			if err := validateClaimURL(value); err == nil {
				t.Fatalf("validateClaimURL(%q) accepted an invalid URL", value)
			}
		})
	}
}

func TestClientPullSendsCanonicalSignedRequest(t *testing.T) {
	config := validConfig()
	now := time.Date(2026, 8, 2, 12, 34, 56, 789000000, time.FixedZone("test", 9*60*60))
	expectedRequest := config.Request
	expectedRequest.IssuedAt = now.UTC()
	expectedBody, err := hook.MarshalPullRequest(expectedRequest)
	if err != nil {
		t.Fatalf("MarshalPullRequest() error = %v", err)
	}

	calls := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.Method != http.MethodPost || request.URL.String() != config.ClaimURL {
			t.Fatalf("request route = %s %s", request.Method, request.URL)
		}
		if got := request.Header.Get("accept"); got != "application/json" {
			t.Fatalf("accept = %q", got)
		}
		if got := request.Header.Get("content-type"); got != "application/json" {
			t.Fatalf("content-type = %q", got)
		}
		body := readRequestBody(t, request)
		if !bytes.Equal(body, expectedBody) {
			t.Fatalf("request body = %s, want %s", body, expectedBody)
		}
		if got, want := request.Header.Get(hook.PullSignatureHeader), independentRequestSignature(config.HMACKey, expectedBody); got != want {
			t.Fatalf("request signature = %q, want %q", got, want)
		}
		return signedResponse(config.HMACKey, http.StatusNoContent, body, nil, ""), nil
	})
	client, err := NewClient(config, transport)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	result, err := client.Pull(context.Background(), now)
	if err != nil {
		t.Fatalf("Pull() error = %v", err)
	}
	if !result.NoWork || calls != 1 {
		t.Fatalf("result = %+v, calls = %d", result, calls)
	}
}

func TestClientPullAcceptsSignedResponses(t *testing.T) {
	config := validConfig()
	envelope := validEnvelope(t, config)
	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	t.Run("claimed envelope", func(t *testing.T) {
		client := newSignedResponseClient(t, config, http.StatusOK, "application/json; charset=utf-8", encoded)
		result, err := client.Pull(context.Background(), time.Date(2026, 8, 2, 4, 5, 6, 0, time.UTC))
		if err != nil {
			t.Fatalf("Pull() error = %v", err)
		}
		if result.NoWork || !reflect.DeepEqual(result.Envelope, envelope) {
			t.Fatalf("result = %+v", result)
		}
		wantReceipt := Receipt{
			DeliveryID:  envelope.DeliveryID,
			IssueKey:    envelope.Snapshot.IssueKey,
			InputSHA256: envelope.Snapshot.InputSHA256,
		}
		if result.Receipt != wantReceipt {
			t.Fatalf("receipt = %+v, want %+v", result.Receipt, wantReceipt)
		}
	})

	t.Run("no work", func(t *testing.T) {
		client := newSignedResponseClient(t, config, http.StatusNoContent, "", nil)
		result, err := client.Pull(context.Background(), time.Date(2026, 8, 2, 4, 5, 6, 0, time.UTC))
		if err != nil {
			t.Fatalf("Pull() error = %v", err)
		}
		if !result.NoWork || result.Envelope != (hook.DispatchEnvelope{}) || result.Receipt != (Receipt{}) {
			t.Fatalf("result = %+v", result)
		}
	})
}

func TestClientPullRetriesAmbiguousOutcomeWithExactCanonicalRequest(t *testing.T) {
	config := validConfig()
	envelope := validEnvelope(t, config)
	envelopeBody, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 2, 4, 5, 6, 0, time.UTC)

	for name, firstResponse := range map[string]func([]byte) (*http.Response, error){
		"transport loss": func([]byte) (*http.Response, error) {
			return nil, errors.New("response lost")
		},
		"signed unavailable": func(requestBody []byte) (*http.Response, error) {
			body, marshalErr := json.Marshal(hook.Result{Decision: hook.DecisionInvalid, Code: "pull_unavailable"})
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			return signedResponse(config.HMACKey, http.StatusServiceUnavailable, requestBody, body, "application/json"), nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			var requestBodies [][]byte
			var signatures []string
			transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				body := readRequestBody(t, request)
				requestBodies = append(requestBodies, append([]byte(nil), body...))
				signatures = append(signatures, request.Header.Get(hook.PullSignatureHeader))
				if calls == 1 {
					return firstResponse(body)
				}
				return signedResponse(config.HMACKey, http.StatusOK, body, envelopeBody, "application/json"), nil
			})
			client, err := NewClient(config, transport)
			if err != nil {
				t.Fatal(err)
			}
			result, err := client.Pull(context.Background(), now)
			if err != nil || result.Envelope != envelope || calls != 2 {
				t.Fatalf("result=%+v err=%v calls=%d", result, err, calls)
			}
			if !bytes.Equal(requestBodies[0], requestBodies[1]) || signatures[0] != signatures[1] {
				t.Fatal("ambiguous retry changed the canonical request or signature")
			}
		})
	}
}

func TestClientPullBoundsAmbiguousRetriesAndDoesNotRetryExplicitConflict(t *testing.T) {
	config := validConfig()
	now := time.Date(2026, 8, 2, 4, 5, 6, 0, time.UTC)

	t.Run("bounded transport loss", func(t *testing.T) {
		calls := 0
		client, err := NewClient(config, roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return nil, errors.New("response lost")
		}))
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.Pull(context.Background(), now)
		requireValidationCode(t, err, "pull_unavailable")
		if calls != maxPullAttempts {
			t.Fatalf("calls=%d want=%d", calls, maxPullAttempts)
		}
	})

	t.Run("signed conflict", func(t *testing.T) {
		calls := 0
		client, err := NewClient(config, roundTripFunc(func(request *http.Request) (*http.Response, error) {
			calls++
			requestBody := readRequestBody(t, request)
			body, marshalErr := json.Marshal(hook.Result{Decision: hook.DecisionInvalid, Code: "pull_conflict"})
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			return signedResponse(config.HMACKey, http.StatusConflict, requestBody, body, "application/json"), nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.Pull(context.Background(), now)
		requireValidationCode(t, err, "claim_rejected")
		if calls != 1 {
			t.Fatalf("calls=%d want=1", calls)
		}
	})
}

func TestClientPullDoesNotFollowRedirects(t *testing.T) {
	config := validConfig()
	calls := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		response := plainResponse(http.StatusTemporaryRedirect, "", nil)
		response.Header.Set("location", "https://evil.example/claim")
		return response, nil
	})
	client, err := NewClient(config, transport)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	_, err = client.Pull(context.Background(), time.Date(2026, 8, 2, 4, 5, 6, 0, time.UTC))
	requireValidationCode(t, err, "pull_unavailable")
	if calls != 1 {
		t.Fatalf("RoundTrip() calls = %d, want 1", calls)
	}
}

func TestClientPullRejectsInvalidResponses(t *testing.T) {
	config := validConfig()
	envelope := validEnvelope(t, config)
	validBody, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	unknownFieldBody := append(append([]byte(nil), validBody[:len(validBody)-1]...), []byte(`,"unexpected":true}`)...)

	tests := map[string]struct {
		status            int
		contentType       string
		body              []byte
		sign              bool
		signedStatus      int
		signedRequestBody []byte
		signedBody        []byte
		wantCode          string
	}{
		"missing signature": {
			status: http.StatusOK, contentType: "application/json", body: validBody,
			wantCode: "response_signature_invalid",
		},
		"body differs from signature": {
			status: http.StatusOK, contentType: "application/json", body: validBody, sign: true,
			signedStatus: http.StatusOK, signedBody: []byte(`{}`), wantCode: "response_signature_invalid",
		},
		"status differs from signature": {
			status: http.StatusCreated, contentType: "application/json", body: validBody, sign: true,
			signedStatus: http.StatusOK, signedBody: validBody, wantCode: "response_signature_invalid",
		},
		"request differs from signature": {
			status: http.StatusOK, contentType: "application/json", body: validBody, sign: true,
			signedStatus: http.StatusOK, signedRequestBody: []byte(`{}`), signedBody: validBody, wantCode: "response_signature_invalid",
		},
		"oversized body": {
			status: http.StatusOK, contentType: "application/json", body: bytes.Repeat([]byte("x"), MaxResponseBytes+1), sign: true,
			signedStatus: http.StatusOK, wantCode: "response_invalid",
		},
		"unexpected status": {
			status: http.StatusConflict, contentType: "application/json", body: []byte(`{"code":"claimed"}`), sign: true,
			signedStatus: http.StatusConflict, wantCode: "claim_rejected",
		},
		"wrong content type": {
			status: http.StatusOK, contentType: "text/plain", body: validBody, sign: true,
			signedStatus: http.StatusOK, wantCode: "response_invalid",
		},
		"missing content type": {
			status: http.StatusOK, body: validBody, sign: true,
			signedStatus: http.StatusOK, wantCode: "response_invalid",
		},
		"malformed body": {
			status: http.StatusOK, contentType: "application/json", body: []byte(`{`), sign: true,
			signedStatus: http.StatusOK, wantCode: "envelope_invalid",
		},
		"noncanonical body": {
			status: http.StatusOK, contentType: "application/json", body: append(append([]byte(nil), validBody...), '\n'), sign: true,
			signedStatus: http.StatusOK, wantCode: "envelope_invalid",
		},
		"unknown body field": {
			status: http.StatusOK, contentType: "application/json", body: unknownFieldBody, sign: true,
			signedStatus: http.StatusOK, wantCode: "envelope_invalid",
		},
		"no-content body": {
			status: http.StatusNoContent, body: []byte("x"), sign: true,
			signedStatus: http.StatusNoContent, wantCode: "response_invalid",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
				requestBody := readRequestBody(t, request)
				response := plainResponse(test.status, test.contentType, test.body)
				if test.sign {
					signedRequestBody := test.signedRequestBody
					if signedRequestBody == nil {
						signedRequestBody = requestBody
					}
					signedBody := test.signedBody
					if signedBody == nil {
						signedBody = test.body
					}
					response.Header.Set(hook.PullResponseSignatureHeader, hook.SignPullResponse(config.HMACKey, test.signedStatus, signedRequestBody, signedBody))
				}
				return response, nil
			})
			client, err := NewClient(config, transport)
			if err != nil {
				t.Fatalf("NewClient() error = %v", err)
			}
			_, err = client.Pull(context.Background(), time.Date(2026, 8, 2, 4, 5, 6, 0, time.UTC))
			requireValidationCode(t, err, test.wantCode)
		})
	}
}

func TestClientPullRejectsSnapshotOutsideAllowlist(t *testing.T) {
	config := validConfig()
	tests := map[string]func(*hook.TicketSnapshot){
		"space":         func(snapshot *hook.TicketSnapshot) { snapshot.SpaceKey = "other" },
		"project id":    func(snapshot *hook.TicketSnapshot) { snapshot.ProjectID++ },
		"project key":   func(snapshot *hook.TicketSnapshot) { snapshot.ProjectKey = "OTHER"; snapshot.IssueKey = "OTHER-501" },
		"creator":       func(snapshot *hook.TicketSnapshot) { snapshot.CreatorID++ },
		"activity type": func(snapshot *hook.TicketSnapshot) { snapshot.ActivityType++ },
		"run":           func(snapshot *hook.TicketSnapshot) { snapshot.RunID = "run_20260802_other" },
		"repository":    func(snapshot *hook.TicketSnapshot) { snapshot.Target.RepositoryID++ },
		"workflow ref": func(snapshot *hook.TicketSnapshot) {
			snapshot.Target.WorkflowRefSHA256 = identityDigest("other workflow")
		},
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			envelope := validEnvelope(t, config)
			snapshot := envelope.Snapshot
			mutate(&snapshot)
			envelope, err := hook.SealSnapshot(snapshot)
			if err != nil {
				t.Fatalf("SealSnapshot() error = %v", err)
			}
			body, err := json.Marshal(envelope)
			if err != nil {
				t.Fatalf("json.Marshal() error = %v", err)
			}
			client := newSignedResponseClient(t, config, http.StatusOK, "application/json", body)
			_, err = client.Pull(context.Background(), time.Date(2026, 8, 2, 4, 5, 6, 0, time.UTC))
			requireValidationCode(t, err, "envelope_invalid")
		})
	}
}

func TestClientPullRejectsSnapshotTampering(t *testing.T) {
	config := validConfig()
	tests := map[string]func(*hook.DispatchEnvelope){
		"ticket body": func(envelope *hook.DispatchEnvelope) {
			envelope.Snapshot.Untrusted.Description = "changed after sealing"
		},
		"input digest": func(envelope *hook.DispatchEnvelope) {
			envelope.Snapshot.InputSHA256 = strings.Repeat("b", 64)
		},
		"delivery binding": func(envelope *hook.DispatchEnvelope) {
			envelope.DeliveryID = "delivery_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		},
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			envelope := validEnvelope(t, config)
			mutate(&envelope)
			body, err := json.Marshal(envelope)
			if err != nil {
				t.Fatalf("json.Marshal() error = %v", err)
			}
			client := newSignedResponseClient(t, config, http.StatusOK, "application/json", body)
			_, err = client.Pull(context.Background(), time.Date(2026, 8, 2, 4, 5, 6, 0, time.UTC))
			requireValidationCode(t, err, "envelope_invalid")
		})
	}
}

func TestClientErrorsDoNotLeakSecretsOrTicketBody(t *testing.T) {
	const (
		secretSentinel = "SECRET-SENTINEL-MUST-NOT-LEAK"
		ticketSentinel = "TICKET-BODY-SENTINEL-MUST-NOT-LEAK"
	)
	config := validConfig()
	config.HMACKey = []byte(secretSentinel + "-padding")

	t.Run("transport error", func(t *testing.T) {
		transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New(secretSentinel + " " + ticketSentinel)
		})
		client, err := NewClient(config, transport)
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}
		_, err = client.Pull(context.Background(), time.Date(2026, 8, 2, 4, 5, 6, 0, time.UTC))
		requireErrorExcludes(t, err, "pull_unavailable", secretSentinel, ticketSentinel)
	})

	t.Run("invalid envelope", func(t *testing.T) {
		envelope := validEnvelope(t, config)
		envelope.Snapshot.Untrusted.Description = ticketSentinel
		body, err := json.Marshal(envelope)
		if err != nil {
			t.Fatalf("json.Marshal() error = %v", err)
		}
		client := newSignedResponseClient(t, config, http.StatusOK, "application/json", body)
		_, err = client.Pull(context.Background(), time.Date(2026, 8, 2, 4, 5, 6, 0, time.UTC))
		requireErrorExcludes(t, err, "envelope_invalid", secretSentinel, ticketSentinel)
	})
}

func newSignedResponseClient(t *testing.T, config Config, status int, contentType string, responseBody []byte) *Client {
	t.Helper()
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requestBody := readRequestBody(t, request)
		return signedResponse(config.HMACKey, status, requestBody, responseBody, contentType), nil
	})
	client, err := NewClient(config, transport)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	return client
}

func signedResponse(key []byte, status int, requestBody, responseBody []byte, contentType string) *http.Response {
	response := plainResponse(status, contentType, responseBody)
	response.Header.Set(hook.PullResponseSignatureHeader, hook.SignPullResponse(key, status, requestBody, responseBody))
	return response
}

func plainResponse(status int, contentType string, body []byte) *http.Response {
	header := make(http.Header)
	if contentType != "" {
		header.Set("content-type", contentType)
	}
	return &http.Response{
		StatusCode: status,
		Header:     header,
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}

func readRequestBody(t *testing.T, request *http.Request) []byte {
	t.Helper()
	body, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatalf("ReadAll(request.Body) error = %v", err)
	}
	return body
}

func requireValidationCode(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("Pull() error = nil, want %q", want)
	}
	var validation *ValidationError
	if !errors.As(err, &validation) || validation.Code != want || err.Error() != want {
		t.Fatalf("Pull() error = %T(%v), want ValidationError(%q)", err, err, want)
	}
}

func requireErrorExcludes(t *testing.T, err error, wantCode string, sentinels ...string) {
	t.Helper()
	requireValidationCode(t, err, wantCode)
	for _, sentinel := range sentinels {
		if strings.Contains(err.Error(), sentinel) {
			t.Fatalf("error leaked sentinel %q: %v", sentinel, err)
		}
	}
}

func identityDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func independentRequestSignature(key, body []byte) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("pull-claim-v1\nrequest\nPOST\n/pull-claim/v1\n"))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// TestClientPullDeliversAResumedEnvelopeBeyondTheOldCap proves the receiver
// accepts a resumed envelope larger than the former 64KB response cap: the
// sealed clarification travels intact from the pull response into the result.
func TestClientPullDeliversAResumedEnvelopeBeyondTheOldCap(t *testing.T) {
	config := validConfig()
	envelope := validEnvelope(t, config)
	questionsJSON := `[{"id":"Q1","question":"` + strings.Repeat(`\"`, 5000) + `"}]`
	question := hook.QuestionRecord{
		Protocol:          hook.QuestionProtocolVersion,
		DeliveryID:        envelope.DeliveryID,
		InputSHA256:       envelope.Snapshot.InputSHA256,
		RepositoryID:      envelope.Snapshot.Target.RepositoryID,
		RepositorySHA256:  hook.HashIdentity("example/automation-receiver"),
		WorkflowRefSHA256: envelope.Snapshot.Target.WorkflowRefSHA256,
		WorkflowSHA:       strings.Repeat("2", 40),
		WorkflowRunID:     123456789,
		RunAttempt:        1,
		AutomationRunID:   envelope.Snapshot.RunID,
		RunURL:            "https://github.com/example/automation-receiver/actions/runs/123456789/attempts/1",
		QuestionRevision:  1,
		QuestionsJSON:     questionsJSON,
		QuestionsSHA256:   hook.TerminalReportDigest([]byte(questionsJSON)),
		DecisionSHA256:    strings.Repeat("c", 64),
		AnswerDeadlineAt:  4_000,
		NotifyAt:          [3]int64{1_000, 2_000, 3_000},
	}
	encodedQuestion, err := hook.MarshalQuestionRecord(question)
	if err != nil {
		t.Fatalf("MarshalQuestionRecord() error = %v", err)
	}
	answers := `{"Q1":"a"}`
	record := hook.ClarificationRecord{
		Protocol:          hook.ClarificationProtocolVersion,
		DeliveryID:        question.DeliveryID,
		InputSHA256:       question.InputSHA256,
		RepositoryID:      question.RepositoryID,
		RepositorySHA256:  question.RepositorySHA256,
		WorkflowRefSHA256: question.WorkflowRefSHA256,
		AutomationRunID:   question.AutomationRunID,
		InputRevision:     2,
		Rounds: []hook.ClarificationRound{{
			QuestionRecordJSON:   string(encodedQuestion),
			QuestionRecordSHA256: hook.TerminalReportDigest(encodedQuestion),
			QuestionCommentID:    500,
			AnswerCommentID:      600,
			AnswererID:           7,
			AnswerPostedAt:       3_500,
			AnswerBodySHA256:     strings.Repeat("b", 64),
			AnswersJSON:          answers,
			AnswersSHA256:        hook.TerminalReportDigest([]byte(answers)),
		}},
	}
	sealed, err := hook.MarshalClarificationRecord(record)
	if err != nil {
		t.Fatalf("MarshalClarificationRecord() error = %v", err)
	}
	envelope.ClarificationJSON = string(sealed)
	envelopeBody, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if len(envelopeBody) <= 64*1024 || len(envelopeBody) > hook.MaxDeliveredEnvelopeBytes {
		t.Fatalf("test setup: envelope is %d bytes, want between the old cap and the delivery bound", len(envelopeBody))
	}

	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := readRequestBody(t, request)
		return signedResponse(config.HMACKey, http.StatusOK, body, envelopeBody, "application/json"), nil
	})
	client, err := NewClient(config, transport)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 2, 4, 5, 6, 0, time.UTC)
	result, err := client.Pull(context.Background(), now)
	if err != nil {
		t.Fatalf("Pull() error = %v", err)
	}
	if result.NoWork || result.Envelope.ClarificationJSON != string(sealed) {
		t.Fatalf("resumed envelope was not delivered intact: noWork=%v clarification bytes=%d", result.NoWork, len(result.Envelope.ClarificationJSON))
	}
}
