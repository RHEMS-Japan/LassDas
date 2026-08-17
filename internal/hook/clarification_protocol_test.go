package hook

import (
	"strings"
	"testing"
)

const clarificationTestAnswersJSON = `{"Q1":"a"}`

func clarificationTestRound(t *testing.T, question QuestionRecord, questionCommentID, answerCommentID int64) ClarificationRound {
	t.Helper()
	config := terminalTestConfig()
	encoded, err := MarshalQuestionRecord(question)
	if err != nil {
		t.Fatalf("MarshalQuestionRecord() error = %v", err)
	}
	return ClarificationRound{
		QuestionRecordJSON:   string(encoded),
		QuestionRecordSHA256: TerminalReportDigest(encoded),
		QuestionCommentID:    questionCommentID,
		AnswerCommentID:      answerCommentID,
		AnswererID:           config.AllowedCreatorID,
		AnswerPostedAt:       question.AnswerDeadlineAt - 100,
		AnswerBodySHA256:     strings.Repeat("b", 64),
		AnswersJSON:          clarificationTestAnswersJSON,
		AnswersSHA256:        TerminalReportDigest([]byte(clarificationTestAnswersJSON)),
	}
}

func clarificationTestRecord(t *testing.T) ClarificationRecord {
	t.Helper()
	config := terminalTestConfig()
	question := questionTestRecord()
	return ClarificationRecord{
		Protocol:          ClarificationProtocolVersion,
		DeliveryID:        question.DeliveryID,
		InputSHA256:       question.InputSHA256,
		RepositoryID:      config.RepositoryID,
		RepositorySHA256:  config.RepositorySHA256,
		WorkflowRefSHA256: config.WorkflowRefSHA256,
		AutomationRunID:   config.ExpectedRunID,
		InputRevision:     2,
		Rounds:            []ClarificationRound{clarificationTestRound(t, question, 500, 600)},
	}
}

func TestClarificationRecordRoundTripIsCanonical(t *testing.T) {
	record := clarificationTestRecord(t)
	encoded, err := MarshalClarificationRecord(record)
	if err != nil {
		t.Fatalf("MarshalClarificationRecord() error = %v", err)
	}
	decoded, err := DecodeClarificationRecord(encoded)
	if err != nil {
		t.Fatalf("DecodeClarificationRecord() error = %v", err)
	}
	if decoded.Rounds[0] != record.Rounds[0] || decoded.InputRevision != record.InputRevision {
		t.Fatal("decoded record does not equal the source record")
	}
	if _, err := DecodeClarificationRecord(append(encoded, ' ')); err == nil {
		t.Fatal("non-canonical encoding was accepted")
	}
	tampered := strings.Replace(string(encoded), `"input_revision":2`, `"input_revision":2,"extra":true`, 1)
	if _, err := DecodeClarificationRecord([]byte(tampered)); err == nil {
		t.Fatal("unknown field was accepted")
	}
}

func TestClarificationRecordShapeFailsClosed(t *testing.T) {
	for _, run := range []struct {
		name   string
		mutate func(t *testing.T, record *ClarificationRecord)
	}{
		{name: "protocol mismatch", mutate: func(t *testing.T, record *ClarificationRecord) { record.Protocol = "clarification-resume-v2" }},
		{name: "revision does not equal rounds plus one", mutate: func(t *testing.T, record *ClarificationRecord) { record.InputRevision = 3 }},
		{name: "no rounds", mutate: func(t *testing.T, record *ClarificationRecord) {
			record.Rounds = nil
			record.InputRevision = 1
		}},
		{name: "three rounds exceed the run contract", mutate: func(t *testing.T, record *ClarificationRecord) {
			question := questionTestRecord()
			round := clarificationTestRound(t, question, 500, 600)
			record.Rounds = []ClarificationRound{round, round, round}
			record.InputRevision = 4
		}},
		{name: "round question revision mismatch", mutate: func(t *testing.T, record *ClarificationRecord) {
			question := questionTestRecord()
			question.QuestionRevision = 2
			question.ClarificationSHA256 = strings.Repeat("d", 64)
			record.Rounds = []ClarificationRound{clarificationTestRound(t, question, 500, 600)}
		}},
		{name: "question record not canonical", mutate: func(t *testing.T, record *ClarificationRecord) {
			record.Rounds[0].QuestionRecordJSON += " "
			record.Rounds[0].QuestionRecordSHA256 = TerminalReportDigest([]byte(record.Rounds[0].QuestionRecordJSON))
		}},
		{name: "question record digest mismatch", mutate: func(t *testing.T, record *ClarificationRecord) {
			record.Rounds[0].QuestionRecordSHA256 = strings.Repeat("0", 64)
		}},
		{name: "question identity mismatch", mutate: func(t *testing.T, record *ClarificationRecord) {
			record.DeliveryID = "delivery_ffffffffffffffffffffffffffffffff"
		}},
		{name: "answer comment does not follow question comment", mutate: func(t *testing.T, record *ClarificationRecord) {
			record.Rounds[0].AnswerCommentID = record.Rounds[0].QuestionCommentID
		}},
		{name: "answer posted at or after the deadline", mutate: func(t *testing.T, record *ClarificationRecord) {
			record.Rounds[0].AnswerPostedAt = questionTestRecord().AnswerDeadlineAt
		}},
		{name: "answer body digest malformed", mutate: func(t *testing.T, record *ClarificationRecord) {
			record.Rounds[0].AnswerBodySHA256 = "not-a-digest"
		}},
		{name: "answer set digest mismatch", mutate: func(t *testing.T, record *ClarificationRecord) {
			record.Rounds[0].AnswersSHA256 = strings.Repeat("0", 64)
		}},
		{name: "answer set is not an object of strings", mutate: func(t *testing.T, record *ClarificationRecord) {
			record.Rounds[0].AnswersJSON = `{"Q1":1}`
			record.Rounds[0].AnswersSHA256 = TerminalReportDigest([]byte(record.Rounds[0].AnswersJSON))
		}},
		{name: "answer set with four entries", mutate: func(t *testing.T, record *ClarificationRecord) {
			record.Rounds[0].AnswersJSON = `{"Q1":"a","Q2":"a","Q3":"a","Q4":"a"}`
			record.Rounds[0].AnswersSHA256 = TerminalReportDigest([]byte(record.Rounds[0].AnswersJSON))
		}},
		{name: "answer set trailing data", mutate: func(t *testing.T, record *ClarificationRecord) {
			record.Rounds[0].AnswersJSON = `{"Q1":"a"} {}`
			record.Rounds[0].AnswersSHA256 = TerminalReportDigest([]byte(record.Rounds[0].AnswersJSON))
		}},
		{name: "answerer not the allowlisted creator", mutate: func(t *testing.T, record *ClarificationRecord) {
			record.Rounds[0].AnswererID = terminalTestConfig().AllowedCreatorID + 1
		}},
		{name: "run id mismatch is rejected by route", mutate: func(t *testing.T, record *ClarificationRecord) {
			question := questionTestRecord()
			question.AutomationRunID = "run_20260802_other"
			record.AutomationRunID = "run_20260802_other"
			record.Rounds = []ClarificationRound{clarificationTestRound(t, question, 500, 600)}
		}},
	} {
		t.Run(run.name, func(t *testing.T) {
			record := clarificationTestRecord(t)
			run.mutate(t, &record)
			if err := record.ValidateRoute(terminalTestConfig()); err == nil {
				t.Fatal("invalid record was accepted")
			}
		})
	}
	if err := clarificationTestRecord(t).ValidateRoute(terminalTestConfig()); err != nil {
		t.Fatalf("valid record rejected: %v", err)
	}
}

func TestClarificationSecondRoundChainsCommentsAndRecords(t *testing.T) {
	config := terminalTestConfig()
	roundOneQuestion := questionTestRecord()
	record := clarificationTestRecord(t)
	previous, err := MarshalClarificationRecord(record)
	if err != nil {
		t.Fatalf("MarshalClarificationRecord() error = %v", err)
	}
	roundTwoQuestion := roundOneQuestion
	roundTwoQuestion.QuestionRevision = 2
	roundTwoQuestion.ClarificationSHA256 = TerminalReportDigest(previous)
	second := record
	second.InputRevision = 3
	second.Rounds = append(append([]ClarificationRound{}, record.Rounds...), clarificationTestRound(t, roundTwoQuestion, 700, 800))
	if err := second.ValidateRoute(config); err != nil {
		t.Fatalf("two-round record rejected: %v", err)
	}

	// A round-2 question posted before the round-1 answer breaks the chain.
	broken := second
	broken.Rounds = append([]ClarificationRound{}, second.Rounds...)
	broken.Rounds[1].QuestionCommentID = record.Rounds[0].AnswerCommentID
	if err := broken.ValidateShape(); err == nil {
		t.Fatal("out-of-order round comments were accepted")
	}

	// A round-2 question chained to anything but this record's own sealed
	// prefix is rejected by the record alone, without external context.
	forgedQuestion := roundTwoQuestion
	forgedQuestion.ClarificationSHA256 = strings.Repeat("d", 64)
	forged := second
	forged.Rounds = append([]ClarificationRound{record.Rounds[0]}, clarificationTestRound(t, forgedQuestion, 700, 800))
	if err := forged.ValidateShape(); err == nil {
		t.Fatal("a divergent chain digest was accepted")
	}
}

func TestMarshalClarificationRecordBoundHoldsUnderWorstCaseEscaping(t *testing.T) {
	// Both embedded artifacts sit at their own byte bounds with the most
	// expansion-prone content, proving no valid composition can exceed the
	// sealed record bound (a rejection here would wedge a legitimate resume).
	question := questionTestRecord()
	question.QuestionsJSON = `[{"id":"Q1","question":"` + strings.Repeat(`<`, 3300) + `"}]`
	question.QuestionsSHA256 = TerminalReportDigest([]byte(question.QuestionsJSON))
	questionEncoded, err := MarshalQuestionRecord(question)
	if err != nil {
		t.Fatalf("test setup: question record must marshal: %v", err)
	}
	if len(questionEncoded) < MaxQuestionRecordBytes*3/4 {
		t.Fatalf("test setup: question record is not near its bound: %d", len(questionEncoded))
	}
	answers := `{"Q1":"` + strings.Repeat(`<`, MaxAnswerSetBytes-16) + `"}`
	record := clarificationTestRecord(t)
	round := record.Rounds[0]
	round.QuestionRecordJSON = string(questionEncoded)
	round.QuestionRecordSHA256 = TerminalReportDigest(questionEncoded)
	round.AnswersJSON = answers
	round.AnswersSHA256 = TerminalReportDigest([]byte(answers))
	record.Rounds = []ClarificationRound{round}
	prefixEncoded, err := MarshalClarificationRecord(record)
	if err != nil {
		t.Fatalf("test setup: prefix record must marshal: %v", err)
	}
	secondQuestion := question
	secondQuestion.QuestionRevision = 2
	secondQuestion.ClarificationSHA256 = TerminalReportDigest(prefixEncoded)
	secondRound := clarificationTestRound(t, secondQuestion, 700, 800)
	secondRound.AnswersJSON = answers
	secondRound.AnswersSHA256 = TerminalReportDigest([]byte(answers))
	record.Rounds = []ClarificationRound{round, secondRound}
	record.InputRevision = 3
	encoded, err := MarshalClarificationRecord(record)
	if err != nil {
		t.Fatalf("worst-case valid record was rejected: %v", err)
	}
	if len(encoded) > MaxClarificationRecordBytes {
		t.Fatalf("worst-case record exceeds the bound: %d", len(encoded))
	}
}
