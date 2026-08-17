package worker

import (
	"encoding/json"
	"errors"

	"automation.internal/ticket-ingress/internal/hook"
)

// MaxClarificationJSONBytes bounds the sealed clarification artifact file.
const MaxClarificationJSONBytes = hook.MaxClarificationRecordBytes

// ClarificationExchange is one resolved question round as the readiness
// models see it: the questions that were asked and the requester's chosen
// answers, keyed by question ID.
type ClarificationExchange struct {
	Questions []ReadinessQuestion `json:"questions"`
	Answers   map[string]string   `json:"answers"`
}

// ClarificationContext is the decoded, digest-bound view of the sealed
// clarification record that resumes a run. SHA256 is the digest of the sealed
// canonical bytes and is stamped into every assessment built with it.
type ClarificationContext struct {
	SHA256      string
	Revision    int
	DeliveryID  string
	InputSHA256 string
	Exchanges   []ClarificationExchange
}

// DecodeClarificationContext validates the sealed clarification bytes and
// unfolds every round into the question texts and adopted answers. Any decode
// or shape failure is terminal: a resumed run must never be assessed with a
// partial view of its answers.
func DecodeClarificationContext(encoded []byte) (*ClarificationContext, error) {
	record, err := hook.DecodeClarificationRecord(encoded)
	if err != nil {
		return nil, errors.New("clarification record is invalid")
	}
	context := &ClarificationContext{
		SHA256:      hook.TerminalReportDigest(encoded),
		Revision:    record.InputRevision,
		DeliveryID:  record.DeliveryID,
		InputSHA256: record.InputSHA256,
	}
	for _, round := range record.Rounds {
		question, err := hook.DecodeQuestionRecord([]byte(round.QuestionRecordJSON))
		if err != nil {
			return nil, errors.New("clarification record is invalid")
		}
		var questions []ReadinessQuestion
		if err := json.Unmarshal([]byte(question.QuestionsJSON), &questions); err != nil || len(questions) == 0 {
			return nil, errors.New("clarification record is invalid")
		}
		var answers map[string]string
		if err := json.Unmarshal([]byte(round.AnswersJSON), &answers); err != nil || len(answers) == 0 {
			return nil, errors.New("clarification record is invalid")
		}
		context.Exchanges = append(context.Exchanges, ClarificationExchange{Questions: questions, Answers: answers})
	}
	return context, nil
}

func clarificationDigestOf(clarification *ClarificationContext) string {
	if clarification == nil {
		return ""
	}
	return clarification.SHA256
}

// clarificationMatchesRequest pins the clarification to the same sealed input
// the ticket request came from.
func clarificationMatchesRequest(clarification *ClarificationContext, request TicketRequest) error {
	if clarification == nil {
		return nil
	}
	if clarification.DeliveryID != request.DeliveryID || clarification.InputSHA256 != request.InputSHA256 {
		return errors.New("clarification does not belong to this ticket")
	}
	return nil
}
