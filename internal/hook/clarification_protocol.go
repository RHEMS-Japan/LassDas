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
	ClarificationProtocolVersion = "clarification-resume-v1"
	// MaxAnswerSetBytes bounds the normalized adopted answers for one round.
	// Three single-choice answers stay far below this.
	MaxAnswerSetBytes = 4 * 1024
	// MaxClarificationRecordBytes bounds the sealed cumulative record. It is
	// sized so that every structurally valid composition fits even under the
	// worst JSON string-escaping expansion (two embedded question records at
	// their 24KB bound double to 48KB each, two answer sets at their 4KB bound
	// expand sixfold to 24KB each), so sealing can never wedge a valid resume.
	MaxClarificationRecordBytes = 192 * 1024
)

// ClarificationRound is one completed question-and-answer exchange. The full
// sealed question record is embedded verbatim so the round carries its own
// deadline and schedule, and the adopted answer is pinned to the Backlog
// comment that delivered it.
type ClarificationRound struct {
	QuestionRecordJSON   string `json:"question_record_json"`
	QuestionRecordSHA256 string `json:"question_record_sha256"`
	QuestionCommentID    int64  `json:"question_comment_id"`
	AnswerCommentID      int64  `json:"answer_comment_id"`
	AnswererID           int64  `json:"answerer_id"`
	AnswerPostedAt       int64  `json:"answer_posted_at"`
	AnswerBodySHA256     string `json:"answer_body_sha256"`
	AnswersJSON          string `json:"answers_json"`
	AnswersSHA256        string `json:"answers_sha256"`
}

// ClarificationRecord is the immutable cumulative clarification outcome that
// resumes a waiting run. Revision N+1 embeds every adopted round so the resumed
// input is self-contained: the original envelope plus this record is the whole
// input revision. It follows the terminal report pattern: one canonical JSON
// encoding, one digest, no attempt timestamp.
type ClarificationRecord struct {
	Protocol          string               `json:"protocol"`
	DeliveryID        string               `json:"delivery_id"`
	InputSHA256       string               `json:"input_sha256"`
	RepositoryID      int64                `json:"repository_id"`
	RepositorySHA256  string               `json:"repository_sha256"`
	WorkflowRefSHA256 string               `json:"workflow_ref_sha256"`
	AutomationRunID   string               `json:"automation_run_id"`
	InputRevision     int                  `json:"input_revision"`
	Rounds            []ClarificationRound `json:"rounds"`
}

func (r ClarificationRecord) ValidateShape() error {
	if r.Protocol != ClarificationProtocolVersion || !validDeliveryID(r.DeliveryID) || !validDigest(r.InputSHA256) ||
		r.RepositoryID <= 0 || !validIdentityDigest(r.RepositorySHA256) || !validIdentityDigest(r.WorkflowRefSHA256) ||
		!runIDPattern.MatchString(r.AutomationRunID) {
		return errors.New("clarification record identity is invalid")
	}
	if len(r.Rounds) < 1 || len(r.Rounds) > MaxClarificationRounds || r.InputRevision != len(r.Rounds)+1 {
		return errors.New("clarification revision is invalid")
	}
	previousAnswerCommentID := int64(0)
	for index, round := range r.Rounds {
		question, err := DecodeQuestionRecord([]byte(round.QuestionRecordJSON))
		if err != nil || !validDigest(round.QuestionRecordSHA256) ||
			TerminalReportDigest([]byte(round.QuestionRecordJSON)) != round.QuestionRecordSHA256 {
			return errors.New("clarification question binding is invalid")
		}
		if question.QuestionRevision != index+1 ||
			question.DeliveryID != r.DeliveryID || question.InputSHA256 != r.InputSHA256 ||
			question.RepositoryID != r.RepositoryID || question.RepositorySHA256 != r.RepositorySHA256 ||
			question.WorkflowRefSHA256 != r.WorkflowRefSHA256 || question.AutomationRunID != r.AutomationRunID {
			return errors.New("clarification question identity is invalid")
		}
		if round.QuestionCommentID <= previousAnswerCommentID || round.AnswerCommentID <= round.QuestionCommentID {
			return errors.New("clarification comment order is invalid")
		}
		// The record is self-contained: a later round's question must chain to
		// the sealed record that adopted the earlier rounds, and that record is
		// exactly this record's prefix, so the chain digest is recomputable
		// without external context.
		if index > 0 {
			prefix := r
			prefix.InputRevision = index + 1
			prefix.Rounds = r.Rounds[:index]
			encodedPrefix, err := json.Marshal(prefix)
			if err != nil || question.ClarificationSHA256 != TerminalReportDigest(encodedPrefix) {
				return errors.New("clarification chain is invalid")
			}
		}
		if round.AnswererID <= 0 || round.AnswerPostedAt <= 0 || round.AnswerPostedAt >= question.AnswerDeadlineAt {
			return errors.New("clarification answer timing is invalid")
		}
		if !validDigest(round.AnswerBodySHA256) {
			return errors.New("clarification answer body binding is invalid")
		}
		if len(round.AnswersJSON) == 0 || len(round.AnswersJSON) > MaxAnswerSetBytes ||
			!validDigest(round.AnswersSHA256) || TerminalReportDigest([]byte(round.AnswersJSON)) != round.AnswersSHA256 {
			return errors.New("clarification answer set binding is invalid")
		}
		if !answerSetShapeValid(round.AnswersJSON) {
			return errors.New("clarification answer set shape is invalid")
		}
		previousAnswerCommentID = round.AnswerCommentID
	}
	return nil
}

func (r ClarificationRecord) ValidateRoute(config ReportRouteConfig) error {
	if err := config.Validate(); err != nil {
		return errors.New("clarification route configuration is invalid")
	}
	if err := r.ValidateShape(); err != nil {
		return err
	}
	if r.RepositoryID != config.RepositoryID || r.RepositorySHA256 != config.RepositorySHA256 ||
		r.WorkflowRefSHA256 != config.WorkflowRefSHA256 || r.AutomationRunID != config.ExpectedRunID {
		return errors.New("clarification route is not allowed")
	}
	for _, round := range r.Rounds {
		if round.AnswererID != config.AllowedCreatorID {
			return errors.New("clarification answerer is not allowed")
		}
	}
	return nil
}

// answerSetShapeValid enforces only the structural bound the sealed store can
// check without the answer grammar: a JSON object mapping 1..3 question ids to
// string choices. Grammar validation (known question ids, known choices, no
// duplicates) is owned by the answer-intake layer that produced the set.
func answerSetShapeValid(encoded string) bool {
	decoder := json.NewDecoder(bytes.NewReader([]byte(encoded)))
	var entries map[string]json.RawMessage
	if err := decoder.Decode(&entries); err != nil {
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return false
	}
	if len(entries) < 1 || len(entries) > MaxClarificationQuestions {
		return false
	}
	for _, value := range entries {
		trimmed := bytes.TrimSpace(value)
		if len(trimmed) == 0 || trimmed[0] != '"' {
			return false
		}
	}
	return true
}

func MarshalClarificationRecord(record ClarificationRecord) ([]byte, error) {
	if err := record.ValidateShape(); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return nil, errors.New("clarification record could not be encoded")
	}
	// The bound is sized so every valid composition fits (see the constant),
	// making this a pure invariant guard rather than a reachable rejection.
	if len(encoded) > MaxClarificationRecordBytes {
		return nil, errors.New("clarification record size is invalid")
	}
	return encoded, nil
}

func DecodeClarificationRecord(encoded []byte) (ClarificationRecord, error) {
	if len(encoded) == 0 || len(encoded) > MaxClarificationRecordBytes {
		return ClarificationRecord{}, errors.New("clarification record size is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var record ClarificationRecord
	if err := decoder.Decode(&record); err != nil {
		return ClarificationRecord{}, errors.New("clarification record is invalid")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return ClarificationRecord{}, errors.New("clarification record is invalid")
	}
	canonical, err := MarshalClarificationRecord(record)
	if err != nil || !bytes.Equal(canonical, encoded) {
		return ClarificationRecord{}, errors.New("clarification record is not canonical")
	}
	return record, nil
}

// ResumeRequest asks the store to adopt the latest round's answer and return
// the run to the queue as one conditional transaction. For a second resume the
// previous sealed record must be presented so the new record is verified to
// extend it verbatim.
type ResumeRequest struct {
	Record               ClarificationRecord
	RecordJSON           string
	RecordSHA256         string
	PreviousRecordJSON   string
	PreviousRecordSHA256 string
	Route                ReportRouteConfig
	ResumedAt            time.Time
}

type ResumeDisposition string

const (
	ResumeCompleted       ResumeDisposition = "resumed"
	ResumeAlreadyComplete ResumeDisposition = "already_resumed"
	ResumeConflict        ResumeDisposition = "conflict"
)

type ResumeStore interface {
	ResumeWithAnswer(context.Context, ResumeRequest) (ResumeDisposition, error)
}
