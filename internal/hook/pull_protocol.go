package hook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ValidTriggerEvent names the GitHub trigger kinds this pipeline trusts: the
// schedule that polls the queue, and a manual dispatch by someone with write
// access to the repository, which exists so a waiting ticket can be claimed
// now instead of at the next poll. Every identity check on the rail (pull,
// terminal report, question report) shares this definition; relaxing one
// without the others leaves a run that can start but never report.
func ValidTriggerEvent(name string) bool {
	return name == "schedule" || name == "workflow_dispatch"
}

const (
	PullProtocolVersion         = "pull-claim-v1"
	PullClaimPath               = "/pull-claim/v1"
	PullSignatureHeader         = "x-pull-claim-signature"
	PullResponseSignatureHeader = "x-pull-claim-response-signature"
	MaxPullRequestBytes         = 8 * 1024
	MaxPullClockSkew            = 10 * time.Minute
)

var commitPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)

type PullRequest struct {
	Protocol          string    `json:"protocol"`
	RepositoryID      int64     `json:"repository_id"`
	RepositorySHA256  string    `json:"repository_sha256"`
	EventName         string    `json:"event_name"`
	WorkflowRefSHA256 string    `json:"workflow_ref_sha256"`
	Ref               string    `json:"ref"`
	WorkflowSHA       string    `json:"workflow_sha"`
	WorkflowRunID     int64     `json:"workflow_run_id"`
	RunAttempt        int       `json:"run_attempt"`
	AutomationRunID   string    `json:"automation_run_id"`
	IssuedAt          time.Time `json:"issued_at"`
}

func (r PullRequest) ValidateShape() error {
	if r.Protocol != PullProtocolVersion || r.RepositoryID <= 0 || r.WorkflowRunID <= 0 || r.RunAttempt <= 0 {
		return errors.New("pull identity is invalid")
	}
	if !validIdentityDigest(r.RepositorySHA256) || !validIdentityDigest(r.WorkflowRefSHA256) || !commitPattern.MatchString(r.WorkflowSHA) {
		return errors.New("pull digest is invalid")
	}
	if !ValidTriggerEvent(r.EventName) || r.Ref != "refs/heads/main" || !runIDPattern.MatchString(r.AutomationRunID) {
		return errors.New("pull route is invalid")
	}
	if r.IssuedAt.IsZero() || !r.IssuedAt.Equal(r.IssuedAt.UTC()) {
		return errors.New("pull timestamp is invalid")
	}
	return nil
}

func MarshalPullRequest(request PullRequest) ([]byte, error) {
	request.IssuedAt = request.IssuedAt.UTC()
	if err := request.ValidateShape(); err != nil {
		return nil, err
	}
	return json.Marshal(request)
}

func DecodePullRequest(encoded []byte) (PullRequest, error) {
	if len(encoded) == 0 || len(encoded) > MaxPullRequestBytes {
		return PullRequest{}, errors.New("pull request size is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var request PullRequest
	if err := decoder.Decode(&request); err != nil {
		return PullRequest{}, errors.New("pull request is invalid")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return PullRequest{}, errors.New("pull request is invalid")
	}
	canonical, err := MarshalPullRequest(request)
	if err != nil || !bytes.Equal(canonical, encoded) {
		return PullRequest{}, errors.New("pull request is not canonical")
	}
	return request, nil
}

func DecodePullHMACKey(value string) ([]byte, error) {
	if strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n") {
		return nil, errors.New("pull key is invalid")
	}
	key, err := base64.StdEncoding.Strict().DecodeString(value)
	if err != nil || len(key) < 32 || len(key) > 64 {
		return nil, errors.New("pull key is invalid")
	}
	return key, nil
}

func SignPullRequest(key, body []byte) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(PullProtocolVersion + "\nrequest\nPOST\n" + PullClaimPath + "\n"))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func VerifyPullRequestSignature(key, body []byte, signature string) bool {
	expected := SignPullRequest(key, body)
	return len(signature) == len(expected) && hmac.Equal([]byte(signature), []byte(expected))
}

func SignPullResponse(key []byte, status int, requestBody, responseBody []byte) string {
	requestDigest := sha256.Sum256(requestBody)
	responseDigest := sha256.Sum256(responseBody)
	message := strings.Join([]string{
		PullProtocolVersion,
		"response",
		strconv.Itoa(status),
		hex.EncodeToString(requestDigest[:]),
		hex.EncodeToString(responseDigest[:]),
	}, "\n")
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(message))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func VerifyPullResponseSignature(key []byte, status int, requestBody, responseBody []byte, signature string) bool {
	expected := SignPullResponse(key, status, requestBody, responseBody)
	return len(signature) == len(expected) && hmac.Equal([]byte(signature), []byte(expected))
}

func HashIdentity(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func ValidatePullKey(key []byte) error {
	if len(key) < 32 || len(key) > 64 {
		return fmt.Errorf("pull key must contain 32 to 64 bytes")
	}
	return nil
}
