package hook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"
)

const (
	QuestionReportPath                    = "/question-report/v1"
	QuestionTickPath                      = "/question-tick/v1"
	QuestionTickProtocol                  = "question-tick-v1"
	QuestionReportSignatureHeader         = "x-question-report-signature"
	QuestionReportResponseSignatureHeader = "x-question-report-response-signature"
	QuestionTickSignatureHeader           = "x-question-tick-signature"
	QuestionTickResponseSignatureHeader   = "x-question-tick-response-signature"
	MaxQuestionReportRequestBytes         = MaxQuestionRecordBytes + 1024
	maxQuestionTickBodyBytes              = 4 * 1024
)

// QuestionReportRequest is the transport envelope for posting a clarification
// question: the sealed record plus a per-attempt IssuedAt, mirroring the
// terminal report split so a retry authenticates freshly without becoming a
// different question.
type QuestionReportRequest struct {
	Record   QuestionRecord `json:"record"`
	IssuedAt time.Time      `json:"issued_at"`
}

func DecodeQuestionReportRequest(body []byte) (QuestionReportRequest, error) {
	if len(body) == 0 || len(body) > MaxQuestionReportRequestBytes {
		return QuestionReportRequest{}, errors.New("question report request size is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var request QuestionReportRequest
	if err := decoder.Decode(&request); err != nil {
		return QuestionReportRequest{}, errors.New("question report request is invalid")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return QuestionReportRequest{}, errors.New("question report request is invalid")
	}
	if _, err := MarshalQuestionRecord(request.Record); err != nil {
		return QuestionReportRequest{}, err
	}
	if request.IssuedAt.IsZero() {
		return QuestionReportRequest{}, errors.New("question report request is invalid")
	}
	return request, nil
}

func SignQuestionReportRequest(key, body []byte) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(QuestionProtocolVersion + "\nrequest\nPOST\n" + QuestionReportPath + "\n"))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func VerifyQuestionReportRequestSignature(key, body []byte, signature string) bool {
	expected := SignQuestionReportRequest(key, body)
	return len(signature) == len(expected) && hmac.Equal([]byte(signature), []byte(expected))
}

func SignQuestionReportResponse(key []byte, status int, requestBody, responseBody []byte) string {
	return signQuestionResponse(key, QuestionProtocolVersion, status, requestBody, responseBody)
}

func VerifyQuestionReportResponseSignature(key []byte, status int, requestBody, responseBody []byte, signature string) bool {
	expected := SignQuestionReportResponse(key, status, requestBody, responseBody)
	return len(signature) == len(expected) && hmac.Equal([]byte(signature), []byte(expected))
}

// QuestionTickRequest wakes the clarification clock: the caller proves it
// holds the shared key and names the fixed run; every decision about what the
// tick should do is derived from sealed state on the Lambda side.
type QuestionTickRequest struct {
	Protocol        string    `json:"protocol"`
	AutomationRunID string    `json:"automation_run_id"`
	IssuedAt        time.Time `json:"issued_at"`
}

func DecodeQuestionTickRequest(body []byte) (QuestionTickRequest, error) {
	if len(body) == 0 || len(body) > maxQuestionTickBodyBytes {
		return QuestionTickRequest{}, errors.New("question tick request size is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var request QuestionTickRequest
	if err := decoder.Decode(&request); err != nil {
		return QuestionTickRequest{}, errors.New("question tick request is invalid")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return QuestionTickRequest{}, errors.New("question tick request is invalid")
	}
	if request.Protocol != QuestionTickProtocol || !runIDPattern.MatchString(request.AutomationRunID) || request.IssuedAt.IsZero() {
		return QuestionTickRequest{}, errors.New("question tick request is invalid")
	}
	return request, nil
}

func SignQuestionTickRequest(key, body []byte) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(QuestionTickProtocol + "\nrequest\nPOST\n" + QuestionTickPath + "\n"))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func VerifyQuestionTickRequestSignature(key, body []byte, signature string) bool {
	expected := SignQuestionTickRequest(key, body)
	return len(signature) == len(expected) && hmac.Equal([]byte(signature), []byte(expected))
}

func SignQuestionTickResponse(key []byte, status int, requestBody, responseBody []byte) string {
	return signQuestionResponse(key, QuestionTickProtocol, status, requestBody, responseBody)
}

func VerifyQuestionTickResponseSignature(key []byte, status int, requestBody, responseBody []byte, signature string) bool {
	expected := SignQuestionTickResponse(key, status, requestBody, responseBody)
	return len(signature) == len(expected) && hmac.Equal([]byte(signature), []byte(expected))
}

func signQuestionResponse(key []byte, protocol string, status int, requestBody, responseBody []byte) string {
	requestDigest := sha256.Sum256(requestBody)
	responseDigest := sha256.Sum256(responseBody)
	message := strings.Join([]string{
		protocol,
		"response",
		strconv.Itoa(status),
		hex.EncodeToString(requestDigest[:]),
		hex.EncodeToString(responseDigest[:]),
	}, "\n")
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(message))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
