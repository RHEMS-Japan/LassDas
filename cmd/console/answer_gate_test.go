package main

import (
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/hook"
)

const gateTestRunID = "run_20260819_0123456789abcdef01234567"

// gateQuestionBody mirrors the shape hook.QuestionCommentContent renders:
// headline, questions with one printed copy-paste line per choice, the
// footer, and the machine marker as the final line. The marker itself is
// built by the engine's own constructor so the console's parsing is pinned
// to the real format, not a hand-typed copy.
func gateQuestionBody(tag string, midLines ...string) string {
	var builder strings.Builder
	builder.WriteString("【確認のお願い " + tag + "】回答期限: 2026-08-20 17:00\n\n")
	builder.WriteString("Q1. 並び順は?\n")
	builder.WriteString("- a: 新着順\n  回答 " + tag + " Q1:a\n")
	builder.WriteString("- b: 名前順\n  回答 " + tag + " Q1:b\n")
	builder.WriteString("Q2. 既存データは?\n")
	builder.WriteString("- a: 残す\n  回答 " + tag + " Q2:a\n")
	builder.WriteString("- c: 消す\n  回答 " + tag + " Q2:c\n")
	for _, line := range midLines {
		builder.WriteString(line + "\n")
	}
	builder.WriteString("\n---\n状態: 回答待ち（質問 " + tag + "）\n")
	builder.WriteString(hook.CommentMarker("question", gateTestRunID, tag) + "\n")
	return builder.String()
}

const answerer = int64(1111)
const bystander = int64(999)

func TestAnswerGate(t *testing.T) {
	question := rawComment{ID: 100, UserID: 42, Content: gateQuestionBody("C1")}
	cases := []struct {
		name       string
		comments   []rawComment
		questionID int64
		lines      []string
		wantStatus int
		wantReason string
	}{
		{
			name:     "printed lines for every question pass",
			comments: []rawComment{question}, questionID: 100,
			lines: []string{"回答 C1 Q1:a", "回答 C1 Q2:c"},
		},
		{
			name:     "a partial answer is refused before it wastes a round-trip",
			comments: []rawComment{question}, questionID: 100,
			lines:      []string{"回答 C1 Q1:b"},
			wantStatus: 400, wantReason: "every question",
		},
		{
			name: "a marker in a middle line does not make a question",
			comments: []rawComment{{ID: 100, UserID: 42, Content: hook.CommentMarker("question", gateTestRunID, "C1") +
				"\n  回答 C1 Q1:a\nlast line is prose"}},
			questionID: 100, lines: []string{"回答 C1 Q1:a"},
			wantStatus: 409, wantReason: "not a question",
		},
		{
			name: "a headline without the engine marker is not a question",
			comments: []rawComment{{ID: 100, UserID: bystander,
				Content: "【確認のお願い C1】\nQ1. 並び順は?\n- a: 新着順\n  回答 C1 Q1:a\n- b: 名前順\n  回答 C1 Q1:b"}},
			questionID: 100, lines: []string{"回答 C1 Q1:a"},
			wantStatus: 409, wantReason: "not a question",
		},
		{
			name: "a newer question closes the older one",
			comments: []rawComment{question,
				{ID: 200, UserID: 42, Content: gateQuestionBody("C2")}},
			questionID: 100, lines: []string{"回答 C1 Q1:a", "回答 C1 Q2:c"},
			wantStatus: 409, wantReason: "newer question",
		},
		{
			name: "an engine receipt for the same round refuses a duplicate",
			comments: []rawComment{question,
				{ID: 150, UserID: 42, Content: "【回答受領 C1】\n---\n" + hook.CommentMarker("answer-receipt", gateTestRunID, "C1", "149") + "\n"}},
			questionID: 100, lines: []string{"回答 C1 Q1:a", "回答 C1 Q2:c"},
			wantStatus: 409, wantReason: "already answered",
		},
		{
			name: "the answerer's own pending answer refuses a duplicate",
			comments: []rawComment{question,
				{ID: 150, UserID: answerer, Content: "回答 C1 Q1:a\n回答 C1 Q2:a"}},
			questionID: 100, lines: []string{"回答 C1 Q1:a", "回答 C1 Q2:c"},
			wantStatus: 409, wantReason: "already posted",
		},
		{
			name: "the answerer's cancel refuses an answer",
			comments: []rawComment{question,
				{ID: 150, UserID: answerer, Content: "中止 C1"}},
			questionID: 100, lines: []string{"回答 C1 Q1:a", "回答 C1 Q2:c"},
			wantStatus: 409, wantReason: "already posted",
		},
		{
			name: "a bystander's answer-shaped comment blocks nothing",
			comments: []rawComment{question,
				{ID: 150, UserID: bystander, Content: "回答 C1 Q1:b"}},
			questionID: 100, lines: []string{"回答 C1 Q1:a", "回答 C1 Q2:c"},
		},
		{
			name:     "a line the question never printed is refused",
			comments: []rawComment{question}, questionID: 100,
			lines:      []string{"回答 C1 Q1:z"},
			wantStatus: 409, wantReason: "not printed",
		},
		{
			name: "a printed line from a different round is refused",
			comments: []rawComment{{ID: 100, UserID: 42,
				Content: gateQuestionBody("C2", "  回答 C1 Q1:a")}},
			questionID: 100, lines: []string{"回答 C1 Q1:a"},
			wantStatus: 409, wantReason: "different question round",
		},
		{
			name:     "two lines for the same question are refused",
			comments: []rawComment{question}, questionID: 100,
			lines:      []string{"回答 C1 Q1:a", "回答 C1 Q1:b"},
			wantStatus: 400, wantReason: "same question",
		},
		{
			name:     "no lines are refused",
			comments: []rawComment{question}, questionID: 100,
			lines:      nil,
			wantStatus: 400, wantReason: "between one and three",
		},
		{
			name:     "more lines than questions can exist are refused",
			comments: []rawComment{question}, questionID: 100,
			lines:      []string{"回答 C1 Q1:a", "回答 C1 Q1:b", "回答 C1 Q2:a", "回答 C1 Q2:c"},
			wantStatus: 400, wantReason: "between one and three",
		},
		{
			name:     "an unknown comment id is refused",
			comments: []rawComment{question}, questionID: 999,
			lines:      []string{"回答 C1 Q1:a"},
			wantStatus: 409, wantReason: "not on this ticket",
		},
		{
			name: "the engine's format guidance reopens the panel",
			comments: []rawComment{question,
				{ID: 150, UserID: answerer, Content: "回答 C1 だめな書式"},
				{ID: 151, UserID: 42, Content: "【回答書式のご案内 C1】\n---\n" + hook.CommentMarker("answer-guidance", gateTestRunID, "C1") + "\n"}},
			questionID: 100, lines: []string{"回答 C1 Q1:a", "回答 C1 Q2:c"},
		},
		{
			name: "a shortfall reply reopens the panel",
			comments: []rawComment{question,
				{ID: 150, UserID: answerer, Content: "回答 C1 Q1:a"},
				{ID: 151, UserID: 42, Content: "【回答の不足 C1】\n---\n" + hook.CommentMarker("answer-shortfall", gateTestRunID, "C1", "150") + "\n"}},
			questionID: 100, lines: []string{"回答 C1 Q1:a", "回答 C1 Q2:c"},
		},
		{
			name: "an answer after the guidance is pending again",
			comments: []rawComment{question,
				{ID: 150, UserID: answerer, Content: "回答 C1 だめな書式"},
				{ID: 151, UserID: 42, Content: "【回答書式のご案内 C1】\n---\n" + hook.CommentMarker("answer-guidance", gateTestRunID, "C1") + "\n"},
				{ID: 152, UserID: answerer, Content: "回答 C1 Q1:a\n回答 C1 Q2:c"}},
			questionID: 100, lines: []string{"回答 C1 Q1:a", "回答 C1 Q2:c"},
			wantStatus: 409, wantReason: "already posted",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			gateErr := evaluateAnswerGate(testCase.comments, testCase.questionID, testCase.lines, answerer)
			if testCase.wantStatus == 0 {
				if gateErr != nil {
					t.Fatalf("evaluateAnswerGate() = %v, want pass", gateErr)
				}
				return
			}
			if gateErr == nil {
				t.Fatalf("evaluateAnswerGate() passed, want status %d", testCase.wantStatus)
			}
			if gateErr.Status != testCase.wantStatus || !strings.Contains(gateErr.Reason, testCase.wantReason) {
				t.Fatalf("evaluateAnswerGate() = %d %q, want %d containing %q",
					gateErr.Status, gateErr.Reason, testCase.wantStatus, testCase.wantReason)
			}
		})
	}
}

// TestAnswerContextFromRows pins the ledger reading that decides whether
// answering is offered at all: the awaiting row wins over stale generations,
// and anything unknown stays zero - the fail-closed direction.
func TestAnswerContextFromRows(t *testing.T) {
	rows := []map[string]string{
		{"pk": "run#stale", "record_type": "run", "run_id": "TEST-1", "state": "terminal",
			"envelope_json": `{"snapshot":{"creator_id":1111}}`},
		{"pk": "run#live", "record_type": "run", "run_id": "TEST-1", "state": "awaiting_answer",
			"envelope_json":       `{"snapshot":{"creator_id":1111}}`,
			"question_comment_id": "796000001",
			"question_record_json": `{"answer_deadline_at":4000}`},
		{"pk": "run#other", "record_type": "run", "run_id": "TEST-2", "state": "claimed",
			"envelope_json": `{"snapshot":{"creator_id":2222}}`},
		{"pk": "ingest#x", "last_activity_id": "5"},
	}
	got := answerContextFromRows(rows, "TEST-1")
	if got.State != "awaiting_answer" || got.AnswererID != 1111 ||
		got.QuestionCommentID != 796000001 || got.AnswerDeadlineAt != 4000 {
		t.Fatalf("answerContextFromRows() = %+v, want the awaiting row's context", got)
	}
	if empty := answerContextFromRows(rows, "TEST-9"); empty != (answerContext{}) {
		t.Fatalf("unknown ticket yields %+v, want zeroes", empty)
	}
	noCreator := answerContextFromRows([]map[string]string{
		{"pk": "run#v1", "record_type": "run", "run_id": "TEST-3", "state": "awaiting_answer",
			"envelope_json": `{"schema_version":1}`},
	}, "TEST-3")
	if noCreator.AnswererID != 0 {
		t.Fatalf("an envelope without creator_id yields answerer %d, want 0", noCreator.AnswererID)
	}
}

// consoleQuestionRecord builds a QuestionRecord the hook validation accepts,
// so tests can render a real question comment instead of a hand-typed copy.
func consoleQuestionRecord(t *testing.T) hook.QuestionRecord {
	t.Helper()
	questions := `[{"id":"Q1","dimension":"user_visible_behavior","question":"並び順は?","why_blocking":"表示が変わる","choices":[{"id":"a","label":"新着順","effect":"新しい順"},{"id":"b","label":"名前順","effect":"五十音順"}]},{"id":"Q2","dimension":"data_lifecycle","question":"既存データは?","why_blocking":"移行が変わる","choices":[{"id":"a","label":"残す","effect":"全件保持"},{"id":"c","label":"消す","effect":"初期化"}]}]`
	record := hook.QuestionRecord{
		Protocol:          hook.QuestionProtocolVersion,
		DeliveryID:        "delivery_0123456789abcdef0123456789abcdef",
		InputSHA256:       strings.Repeat("1", 64),
		RepositoryID:      1,
		RepositorySHA256:  hook.HashIdentity("example/automation-receiver"),
		WorkflowRefSHA256: hook.HashIdentity("example/automation-receiver/.github/workflows/receive.yml@refs/heads/main"),
		WorkflowSHA:       strings.Repeat("2", 40),
		WorkflowRunID:     123456789,
		RunAttempt:        1,
		AutomationRunID:   gateTestRunID,
		RunURL:            "https://github.com/example/automation-receiver/actions/runs/123456789/attempts/1",
		QuestionRevision:  1,
		QuestionsJSON:     questions,
		QuestionsSHA256:   hook.TerminalReportDigest([]byte(questions)),
		DecisionSHA256:    strings.Repeat("c", 64),
		AnswerDeadlineAt:  4_000,
		NotifyAt:          [3]int64{1_000, 2_000, 3_000},
	}
	if err := record.ValidateShape(); err != nil {
		t.Fatalf("fixture record is invalid: %v", err)
	}
	return record
}

// TestAnswerGateApprovalMatchesTheEngineIntake renders a question with the
// engine's own generator, passes its printed lines through the gate, and
// feeds the exact body the console would post into the engine's own answer
// intake. Adoption there is the whole point of the panel: if this fails,
// the screen's "posted" is a lie.
func TestAnswerGateApprovalMatchesTheEngineIntake(t *testing.T) {
	record := consoleQuestionRecord(t)
	content, err := hook.QuestionCommentContent(record)
	if err != nil {
		t.Fatalf("QuestionCommentContent() error = %v", err)
	}
	question := rawComment{ID: 100, UserID: 42, Content: content}
	lines := []string{"回答 C1 Q1:a", "回答 C1 Q2:c"}
	if gateErr := evaluateAnswerGate([]rawComment{question}, 100, lines, answerer); gateErr != nil {
		t.Fatalf("gate refused lines the engine's own question printed: %v", gateErr)
	}
	decision, err := hook.EvaluateAnswerIntake(hook.AnswerIntakeInput{
		Question:          record,
		QuestionCommentID: 100,
		AnswererID:        answerer,
		Comments: []hook.BacklogComment{{
			CommentID: 101, UserID: answerer,
			Body:     strings.Join(lines, "\n"),
			PostedAt: record.AnswerDeadlineAt - 1,
		}},
	})
	if err != nil {
		t.Fatalf("EvaluateAnswerIntake() error = %v", err)
	}
	if decision.Adopted == nil {
		t.Fatalf("the engine did not adopt the console's approved post: %+v", decision)
	}
	// The hand-typed fixture the table tests use must stay aligned with the
	// engine's real rendering: every printed line in the real comment must
	// also parse under the console's printed-line shape.
	printedSeen := 0
	for _, raw := range strings.Split(content, "\n") {
		if printedAnswerLinePattern.MatchString(strings.TrimSpace(raw)) {
			printedSeen++
		}
	}
	if printedSeen != 4 {
		t.Fatalf("the real question prints %d recognizable answer lines, want 4:\n%s", printedSeen, content)
	}
}
