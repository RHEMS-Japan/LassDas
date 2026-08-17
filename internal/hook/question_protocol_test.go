package hook

import (
	"strings"
	"testing"
)

const questionTestSetJSON = `[{"id":"Q1","dimension":"user_visible_behavior","question":"一覧の並び順はどちらにしますか","why_blocking":"利用者に見える並びが変わる","choices":[{"id":"a","label":"新着順","effect":"新しい項目が先頭に出る"},{"id":"b","label":"名前順","effect":"五十音順に並ぶ"}]}]`

func questionTestRecord() QuestionRecord {
	config := terminalTestConfig()
	return QuestionRecord{
		Protocol:          QuestionProtocolVersion,
		DeliveryID:        "delivery_0123456789abcdef0123456789abcdef",
		InputSHA256:       strings.Repeat("1", 64),
		RepositoryID:      config.RepositoryID,
		RepositorySHA256:  config.RepositorySHA256,
		WorkflowRefSHA256: config.WorkflowRefSHA256,
		WorkflowSHA:       strings.Repeat("2", 40),
		WorkflowRunID:     123456789,
		RunAttempt:        1,
		AutomationRunID:   config.ExpectedRunID,
		RunURL:            "https://github.com/example/automation-receiver/actions/runs/123456789/attempts/1",
		QuestionRevision:  1,
		QuestionsJSON:     questionTestSetJSON,
		QuestionsSHA256:   TerminalReportDigest([]byte(questionTestSetJSON)),
		DecisionSHA256:    strings.Repeat("c", 64),
		AnswerDeadlineAt:  4_000,
		NotifyAt:          [3]int64{1_000, 2_000, 3_000},
	}
}

func TestQuestionRecordRoundTripIsCanonical(t *testing.T) {
	record := questionTestRecord()
	encoded, err := MarshalQuestionRecord(record)
	if err != nil {
		t.Fatalf("MarshalQuestionRecord() error = %v", err)
	}
	decoded, err := DecodeQuestionRecord(encoded)
	if err != nil {
		t.Fatalf("DecodeQuestionRecord() error = %v", err)
	}
	if decoded != record {
		t.Fatal("decoded record does not equal the source record")
	}
	if _, err := DecodeQuestionRecord(append(encoded, ' ')); err == nil {
		t.Fatal("non-canonical encoding was accepted")
	}
	if _, err := DecodeQuestionRecord(append(encoded, encoded...)); err == nil {
		t.Fatal("trailing data was accepted")
	}
	tampered := strings.Replace(string(encoded), `"question_revision":1`, `"question_revision":1,"extra":true`, 1)
	if _, err := DecodeQuestionRecord([]byte(tampered)); err == nil {
		t.Fatal("unknown field was accepted")
	}
}

func TestQuestionRecordShapeFailsClosed(t *testing.T) {
	for _, run := range []struct {
		name   string
		mutate func(record *QuestionRecord)
	}{
		{name: "protocol mismatch", mutate: func(record *QuestionRecord) { record.Protocol = "clarification-question-v2" }},
		{name: "revision zero", mutate: func(record *QuestionRecord) { record.QuestionRevision = 0 }},
		{name: "revision beyond max rounds", mutate: func(record *QuestionRecord) { record.QuestionRevision = MaxClarificationRounds + 1 }},
		{name: "empty question set", mutate: func(record *QuestionRecord) {
			record.QuestionsJSON = "[]"
			record.QuestionsSHA256 = TerminalReportDigest([]byte("[]"))
		}},
		{name: "four questions exceed the round contract", mutate: func(record *QuestionRecord) {
			record.QuestionsJSON = `[{"id":"Q1"},{"id":"Q2"},{"id":"Q3"},{"id":"Q4"}]`
			record.QuestionsSHA256 = TerminalReportDigest([]byte(record.QuestionsJSON))
		}},
		{name: "question set is not an object array", mutate: func(record *QuestionRecord) {
			record.QuestionsJSON = `["Q1"]`
			record.QuestionsSHA256 = TerminalReportDigest([]byte(record.QuestionsJSON))
		}},
		{name: "question set digest mismatch", mutate: func(record *QuestionRecord) {
			record.QuestionsSHA256 = strings.Repeat("0", 64)
		}},
		{name: "question set trailing data", mutate: func(record *QuestionRecord) {
			record.QuestionsJSON = `[{"id":"Q1"}] []`
			record.QuestionsSHA256 = TerminalReportDigest([]byte(record.QuestionsJSON))
		}},
		{name: "decision digest malformed", mutate: func(record *QuestionRecord) { record.DecisionSHA256 = "not-a-digest" }},
		{name: "notify times not increasing", mutate: func(record *QuestionRecord) { record.NotifyAt = [3]int64{2_000, 1_000, 3_000} }},
		{name: "deadline before final notification", mutate: func(record *QuestionRecord) { record.AnswerDeadlineAt = 2_500 }},
		{name: "notify times unset", mutate: func(record *QuestionRecord) { record.NotifyAt = [3]int64{} }},
		{name: "run id mismatch is rejected by route", mutate: func(record *QuestionRecord) { record.AutomationRunID = "run_20260802_other" }},
		{name: "oversized question set", mutate: func(record *QuestionRecord) {
			padding := `{"id":"Q1","question":"` + strings.Repeat("あ", MaxQuestionSetBytes/3) + `"}`
			record.QuestionsJSON = "[" + padding + "]"
			record.QuestionsSHA256 = TerminalReportDigest([]byte(record.QuestionsJSON))
		}},
	} {
		t.Run(run.name, func(t *testing.T) {
			record := questionTestRecord()
			run.mutate(&record)
			if err := record.ValidateRoute(terminalTestConfig()); err == nil {
				t.Fatal("invalid record was accepted")
			}
		})
	}
	if err := questionTestRecord().ValidateRoute(terminalTestConfig()); err != nil {
		t.Fatalf("valid record rejected: %v", err)
	}
}

func TestQuestionRecordAllowsBothContractRevisions(t *testing.T) {
	for revision := 1; revision <= MaxClarificationRounds; revision++ {
		record := questionTestRecord()
		record.QuestionRevision = revision
		if revision > 1 {
			record.ClarificationSHA256 = strings.Repeat("d", 64)
		}
		if err := record.ValidateShape(); err != nil {
			t.Fatalf("revision %d rejected: %v", revision, err)
		}
	}
}

func TestMarshalQuestionRecordRejectsRecordThatEscapesBeyondTheSealBound(t *testing.T) {
	// The raw questions JSON stays inside MaxQuestionSetBytes, but JSON string
	// escaping while embedding it into the record ("<" becomes "\u003c")
	// expands it past MaxQuestionRecordBytes. Sealing such a record would
	// wedge the run: every later read re-validates the bound and would refuse
	// to complete or terminate the wait.
	record := questionTestRecord()
	record.QuestionsJSON = `[{"id":"Q1","question":"` + strings.Repeat("<", 5000) + `"}]`
	record.QuestionsSHA256 = TerminalReportDigest([]byte(record.QuestionsJSON))
	if len(record.QuestionsJSON) > MaxQuestionSetBytes {
		t.Fatalf("test setup: questions JSON must stay within its own bound, got %d", len(record.QuestionsJSON))
	}
	if err := record.ValidateShape(); err != nil {
		t.Fatalf("test setup: shape must pass so only the seal bound rejects: %v", err)
	}
	if _, err := MarshalQuestionRecord(record); err == nil {
		t.Fatal("oversized sealed record was accepted")
	}
}
