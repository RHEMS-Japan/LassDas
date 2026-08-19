package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"
)

const (
	QuestionProtocolVersion = "clarification-question-v1"
	// MaxClarificationQuestions bounds one round to the agreed contract
	// (README「質問、回答、再通知、再開」: 1 round 最大 3 件).
	MaxClarificationQuestions = 3
	// MaxClarificationRounds bounds question revisions across the whole run
	// (README: 最大 2 round。round 2 は具体化のみ).
	MaxClarificationRounds = 2
	// QuestionNotifyCount is the fixed number of scheduled renotifications
	// (README: 翌平日、3 平日目、5 平日目の 10:00 に最大 3 回).
	QuestionNotifyCount = 3
	// MaxQuestionSetBytes bounds the canonical questions array. Three
	// questions with four choices each stay far below this.
	MaxQuestionSetBytes = 16 * 1024
	// MaxQuestionRecordBytes bounds the sealed record envelope around the
	// questions array.
	MaxQuestionRecordBytes = 24 * 1024
)

// QuestionRecord is the immutable clarification-question outcome sealed onto
// the run record when readiness decides clarification_required. It mirrors
// terminalReportRecord: one canonical JSON encoding, one digest, no attempt
// timestamp, so a posting retry never becomes a different question. Deadline
// and notification times are absolute UTC unix milliseconds computed by the
// caller; the store validates ordering and seals them, which keeps shortened
// timers a pure input concern.
type QuestionRecord struct {
	Protocol          string `json:"protocol"`
	DeliveryID        string `json:"delivery_id"`
	InputSHA256       string `json:"input_sha256"`
	RepositoryID      int64  `json:"repository_id"`
	RepositorySHA256  string `json:"repository_sha256"`
	WorkflowRefSHA256 string `json:"workflow_ref_sha256"`
	WorkflowSHA       string `json:"workflow_sha"`
	WorkflowRunID     int64  `json:"workflow_run_id"`
	RunAttempt        int    `json:"run_attempt"`
	AutomationRunID   string `json:"automation_run_id"`
	// RunURL is the workflow run that posted the question. It is sealed so a
	// later expiry or cancellation can synthesize the terminal report for the
	// same owning run: the repository digest is one-way, so the URL cannot be
	// reconstructed afterwards.
	RunURL           string `json:"run_url"`
	QuestionRevision int    `json:"question_revision"`
	// ClarificationSHA256 chains a round-2 question to the sealed
	// clarification record it is based on: empty for revision 1, and for
	// revision 2 the digest of the resume record that adopted the round-1
	// answer.
	ClarificationSHA256 string   `json:"clarification_sha256"`
	QuestionsJSON       string   `json:"questions_json"`
	QuestionsSHA256     string   `json:"questions_sha256"`
	DecisionSHA256      string   `json:"decision_sha256"`
	AnswerDeadlineAt    int64    `json:"answer_deadline_at"`
	NotifyAt            [3]int64 `json:"notify_at"`
}

func (r QuestionRecord) ValidateShape() error {
	if r.Protocol != QuestionProtocolVersion || !validDeliveryID(r.DeliveryID) || !validDigest(r.InputSHA256) ||
		r.RepositoryID <= 0 || !validIdentityDigest(r.RepositorySHA256) || !validIdentityDigest(r.WorkflowRefSHA256) ||
		!commitPattern.MatchString(r.WorkflowSHA) || r.WorkflowRunID <= 0 || r.RunAttempt <= 0 ||
		!runIDPattern.MatchString(r.AutomationRunID) ||
		!validRunURL(r.RunURL, r.RepositorySHA256, r.WorkflowRunID, r.RunAttempt) {
		return errors.New("question record identity is invalid")
	}
	if r.QuestionRevision < 1 || r.QuestionRevision > MaxClarificationRounds {
		return errors.New("question revision is invalid")
	}
	if r.QuestionRevision == 1 && r.ClarificationSHA256 != "" {
		return errors.New("question clarification binding is invalid")
	}
	if r.QuestionRevision > 1 && !validDigest(r.ClarificationSHA256) {
		return errors.New("question clarification binding is invalid")
	}
	if len(r.QuestionsJSON) == 0 || len(r.QuestionsJSON) > MaxQuestionSetBytes ||
		!validDigest(r.QuestionsSHA256) || TerminalReportDigest([]byte(r.QuestionsJSON)) != r.QuestionsSHA256 {
		return errors.New("question set binding is invalid")
	}
	if !questionSetCountValid(r.QuestionsJSON) {
		return errors.New("question set shape is invalid")
	}
	if !validDigest(r.DecisionSHA256) {
		return errors.New("question decision binding is invalid")
	}
	if r.NotifyAt[0] <= 0 || r.NotifyAt[1] <= r.NotifyAt[0] || r.NotifyAt[2] <= r.NotifyAt[1] ||
		r.AnswerDeadlineAt <= r.NotifyAt[2] {
		return errors.New("question schedule is invalid")
	}
	return nil
}

func (r QuestionRecord) ValidateRoute(config ReportRouteConfig) error {
	if err := config.Validate(); err != nil {
		return errors.New("question route configuration is invalid")
	}
	if err := r.ValidateShape(); err != nil {
		return err
	}
	if r.RepositoryID != config.RepositoryID || r.RepositorySHA256 != config.RepositorySHA256 ||
		r.WorkflowRefSHA256 != config.WorkflowRefSHA256 || r.AutomationRunID != config.ExpectedRunID {
		return errors.New("question route is not allowed")
	}
	if !runReferenceSchemeAllowed(r.RunURL, config.RunReferenceScheme) {
		return errors.New("question route is not allowed")
	}
	return nil
}

// questionSetCountValid enforces only the structural bound the sealed store
// can check without the readiness schema: a JSON array of 1..3 objects.
// Semantic validation (dimensions, 2..4 choices per question) is owned by the
// readiness artifact contract that produced the set.
func questionSetCountValid(encoded string) bool {
	decoder := json.NewDecoder(bytes.NewReader([]byte(encoded)))
	var items []json.RawMessage
	if err := decoder.Decode(&items); err != nil {
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return false
	}
	if len(items) < 1 || len(items) > MaxClarificationQuestions {
		return false
	}
	for _, item := range items {
		trimmed := bytes.TrimSpace(item)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			return false
		}
	}
	return true
}

func MarshalQuestionRecord(record QuestionRecord) ([]byte, error) {
	if err := record.ValidateShape(); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return nil, errors.New("question record could not be encoded")
	}
	// The encoded record can exceed MaxQuestionRecordBytes even when the
	// questions JSON is inside its own bound, because embedding it as a JSON
	// string escapes characters (`<` becomes `\u003c`, six bytes). Sealing an
	// oversized record would wedge the run: every later read re-checks this
	// bound and would refuse to complete or terminate the wait.
	if len(encoded) > MaxQuestionRecordBytes {
		return nil, errors.New("question record size is invalid")
	}
	return encoded, nil
}

func DecodeQuestionRecord(encoded []byte) (QuestionRecord, error) {
	if len(encoded) == 0 || len(encoded) > MaxQuestionRecordBytes {
		return QuestionRecord{}, errors.New("question record size is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var record QuestionRecord
	if err := decoder.Decode(&record); err != nil {
		return QuestionRecord{}, errors.New("question record is invalid")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return QuestionRecord{}, errors.New("question record is invalid")
	}
	canonical, err := MarshalQuestionRecord(record)
	if err != nil || !bytes.Equal(canonical, encoded) {
		return QuestionRecord{}, errors.New("question record is not canonical")
	}
	return record, nil
}

type QuestionBeginRequest struct {
	Record       QuestionRecord
	RecordJSON   string
	RecordSHA256 string
	Route        ReportRouteConfig
	StartedAt    time.Time
	LeaseUntil   time.Time
	LeaseToken   string
}

type QuestionBeginDisposition string

const (
	QuestionBeginAcquired QuestionBeginDisposition = "acquired"
	QuestionBeginBusy     QuestionBeginDisposition = "busy"
	QuestionBeginComplete QuestionBeginDisposition = "complete"
	QuestionBeginConflict QuestionBeginDisposition = "conflict"
)

type QuestionCompleteRequest struct {
	Record       QuestionRecord
	RecordJSON   string
	RecordSHA256 string
	Route        ReportRouteConfig
	LeaseToken   string
	CommentID    int64
	PostedAt     time.Time
}

type QuestionCompleteDisposition string

const (
	QuestionCompleted        QuestionCompleteDisposition = "completed"
	QuestionAlreadyComplete  QuestionCompleteDisposition = "already_complete"
	QuestionCompleteConflict QuestionCompleteDisposition = "conflict"
)

type QuestionStore interface {
	BeginQuestion(context.Context, QuestionBeginRequest) (TerminalBinding, QuestionBeginDisposition, error)
	CompleteQuestion(context.Context, QuestionCompleteRequest) (QuestionCompleteDisposition, error)
}
