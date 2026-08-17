package hook

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func validPullRequest() PullRequest {
	return PullRequest{
		Protocol: PullProtocolVersion, RepositoryID: 123456,
		RepositorySHA256: strings.Repeat("a", 64), EventName: "schedule",
		WorkflowRefSHA256: strings.Repeat("b", 64), Ref: "refs/heads/main",
		WorkflowSHA: strings.Repeat("c", 40), WorkflowRunID: 987654, RunAttempt: 1,
		AutomationRunID: testRunID, IssuedAt: testTime,
	}
}

// A manual dispatch by someone with write access is the same rail as the
// schedule: it exists so a waiting ticket can be claimed now instead of at
// the next poll.
func TestPullRequestAcceptsAManualDispatch(t *testing.T) {
	request := validPullRequest()
	request.EventName = "workflow_dispatch"
	if err := request.ValidateShape(); err != nil {
		t.Fatalf("ValidateShape() rejected a manual dispatch: %v", err)
	}
}

func TestPullRequestCanonicalRoundTrip(t *testing.T) {
	request := validPullRequest()
	encoded, err := MarshalPullRequest(request)
	if err != nil {
		t.Fatalf("MarshalPullRequest() error = %v", err)
	}
	decoded, err := DecodePullRequest(encoded)
	if err != nil {
		t.Fatalf("DecodePullRequest() error = %v", err)
	}
	if decoded != request {
		t.Fatalf("decoded = %+v", decoded)
	}
	for _, invalid := range [][]byte{
		append([]byte(" "), encoded...),
		append(encoded, '\n'),
		[]byte(strings.Replace(string(encoded), `"protocol":`, `"unknown":1,"protocol":`, 1)),
		[]byte(`{}`),
	} {
		if _, err := DecodePullRequest(invalid); err == nil {
			t.Fatalf("DecodePullRequest() accepted %q", invalid)
		}
	}
}

func TestPullRequestRejectsEveryIdentityBoundary(t *testing.T) {
	tests := map[string]func(*PullRequest){
		"protocol":                  func(v *PullRequest) { v.Protocol = "other" },
		"repository id":             func(v *PullRequest) { v.RepositoryID = 0 },
		"repository digest":         func(v *PullRequest) { v.RepositorySHA256 = "bad" },
		"empty repository identity": func(v *PullRequest) { v.RepositorySHA256 = HashIdentity("") },
		"event":                     func(v *PullRequest) { v.EventName = "push" },
		"workflow ref":              func(v *PullRequest) { v.WorkflowRefSHA256 = "bad" },
		"empty workflow identity":   func(v *PullRequest) { v.WorkflowRefSHA256 = HashIdentity("") },
		"ref":                       func(v *PullRequest) { v.Ref = "refs/heads/feature" },
		"sha":                       func(v *PullRequest) { v.WorkflowSHA = strings.Repeat("g", 40) },
		"workflow run":              func(v *PullRequest) { v.WorkflowRunID = 0 },
		"attempt":                   func(v *PullRequest) { v.RunAttempt = 0 },
		"automation run":            func(v *PullRequest) { v.AutomationRunID = "bad" },
		"time":                      func(v *PullRequest) { v.IssuedAt = time.Time{} },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			request := validPullRequest()
			mutate(&request)
			if _, err := MarshalPullRequest(request); err == nil {
				t.Fatal("invalid request was accepted")
			}
		})
	}
}

func TestPullSignaturesBindExactRequestAndResponse(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	body, err := MarshalPullRequest(validPullRequest())
	if err != nil {
		t.Fatal(err)
	}
	signature := SignPullRequest(key, body)
	if !VerifyPullRequestSignature(key, body, signature) || VerifyPullRequestSignature(key, append(body, ' '), signature) || VerifyPullRequestSignature(bytes.Repeat([]byte{0x43}, 32), body, signature) {
		t.Fatal("request signature did not bind key and exact body")
	}
	responseBody := []byte(`{"decision":"accepted"}`)
	responseSignature := SignPullResponse(key, 200, body, responseBody)
	if !VerifyPullResponseSignature(key, 200, body, responseBody, responseSignature) ||
		VerifyPullResponseSignature(key, 204, body, responseBody, responseSignature) ||
		VerifyPullResponseSignature(key, 200, append(body, ' '), responseBody, responseSignature) ||
		VerifyPullResponseSignature(key, 200, body, append(responseBody, ' '), responseSignature) {
		t.Fatal("response signature did not bind status, request, and response")
	}
}

func TestDecodePullHMACKeyRequiresCanonicalStrongKey(t *testing.T) {
	valid := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32))
	key, err := DecodePullHMACKey(valid)
	if err != nil || len(key) != 32 {
		t.Fatalf("DecodePullHMACKey() = %d, %v", len(key), err)
	}
	for _, value := range []string{"", " " + valid, valid + "\n", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 31)), strings.Repeat("!", 44)} {
		if _, err := DecodePullHMACKey(value); err == nil {
			t.Fatalf("weak/noncanonical key %q was accepted", value)
		}
	}
}

func TestHashIdentityIsStableAndCaseSensitive(t *testing.T) {
	if HashIdentity("owner/repo") != "65e817eec8cd71edae741f73b5aa07d98e8f79c623a5f05014f1a88f89fb089c" ||
		HashIdentity("owner/repo") == HashIdentity("Owner/Repo") {
		t.Fatal("identity hash is not stable and case-sensitive")
	}
}
