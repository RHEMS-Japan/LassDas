package hook

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

func messageTestRecord() QuestionRecord {
	record := questionTestRecord()
	record.QuestionsJSON = intakeTwoQuestionSet
	record.QuestionsSHA256 = TerminalReportDigest([]byte(intakeTwoQuestionSet))
	deadline := time.Date(2026, 8, 14, 17, 0, 0, 0, questionZone)
	record.NotifyAt = [3]int64{
		time.Date(2026, 8, 10, 10, 0, 0, 0, questionZone).UnixMilli(),
		time.Date(2026, 8, 12, 10, 0, 0, 0, questionZone).UnixMilli(),
		time.Date(2026, 8, 14, 10, 0, 0, 0, questionZone).UnixMilli(),
	}
	record.AnswerDeadlineAt = deadline.UnixMilli()
	return record
}

func TestQuestionCommentCarriesACopyPasteLinePerChoice(t *testing.T) {
	record := messageTestRecord()
	content, err := QuestionCommentContent(record)
	if err != nil {
		t.Fatalf("QuestionCommentContent() error = %v", err)
	}
	for _, required := range []string{
		"回答期限: 2026-08-14 17:00",
		"Q1. 並び順は?",
		"回答 C1 Q1:a", "回答 C1 Q1:b",
		"Q2. 既存データは?",
		"回答 C1 Q2:a", "回答 C1 Q2:c",
		"変更を加えずに停止",
		"中止 C1",
	} {
		if !strings.Contains(content, required) {
			t.Fatalf("question comment lacks %q:\n%s", required, content)
		}
	}
	// The rendering is deterministic so a lost POST can be repaired by
	// exact-content lookup.
	again, err := QuestionCommentContent(record)
	if err != nil || again != content {
		t.Fatal("question comment content is not deterministic")
	}
	// The sealed answer grammar accepts the pasted lines verbatim.
	decision, err := EvaluateAnswerIntake(AnswerIntakeInput{
		Question:          record,
		QuestionCommentID: 100,
		AnswererID:        terminalTestConfig().AllowedCreatorID,
		Comments: []BacklogComment{{
			CommentID: 101, UserID: terminalTestConfig().AllowedCreatorID,
			Body: "  回答 C1 Q1:a\n  回答 C1 Q2:c", PostedAt: record.AnswerDeadlineAt - 1,
		}},
	})
	if err != nil || decision.Adopted == nil {
		t.Fatalf("pasted lines were not adopted: %+v, err = %v", decision, err)
	}
}

func TestShortfallCommentListsOnlyTheMissingQuestions(t *testing.T) {
	record := messageTestRecord()
	content, err := ShortfallCommentContent(record, 12345, []string{"Q2"})
	if err != nil {
		t.Fatalf("ShortfallCommentContent() error = %v", err)
	}
	if !strings.Contains(content, "コメント #12345 への返信") || !strings.Contains(content, "Q2. 既存データは?") {
		t.Fatalf("shortfall reply is incomplete:\n%s", content)
	}
	if strings.Contains(content, "Q1. 並び順は?") {
		t.Fatalf("shortfall reply re-lists an answered question:\n%s", content)
	}
	if _, err := ShortfallCommentContent(record, 12345, []string{"Q9"}); err == nil {
		t.Fatal("unknown missing question id was accepted")
	}
	if _, err := ShortfallCommentContent(record, 0, []string{"Q2"}); err == nil {
		t.Fatal("missing trigger comment id was accepted")
	}
}

func TestGuidanceAndNotifyCommentsNameTheRevisionAndDeadline(t *testing.T) {
	record := messageTestRecord()
	guidance := GuidanceCommentContent(record)
	if !strings.Contains(guidance, "回答 C1") || !strings.Contains(guidance, "2026-08-14 17:00") ||
		!strings.Contains(guidance, "一度だけ") {
		t.Fatalf("guidance content is incomplete:\n%s", guidance)
	}
	notify, err := NotifyCommentContent(record, 2)
	if err != nil {
		t.Fatalf("NotifyCommentContent() error = %v", err)
	}
	if !strings.Contains(notify, "再通知 2/3") || !strings.Contains(notify, "2026-08-14 17:00") {
		t.Fatalf("notify content is incomplete:\n%s", notify)
	}
	if _, err := NotifyCommentContent(record, 0); err == nil {
		t.Fatal("notify index 0 was accepted")
	}
	if _, err := NotifyCommentContent(record, QuestionNotifyCount+1); err == nil {
		t.Fatal("notify index above the contract was accepted")
	}
}

func TestEveryAutomatedCommentSatisfiesTheSevenItemContract(t *testing.T) {
	record := messageTestRecord()
	runID := record.AutomationRunID
	type contractCase struct {
		name   string
		body   string
		marker string
	}
	cases := []contractCase{}

	question, err := QuestionCommentContent(record)
	if err != nil {
		t.Fatalf("QuestionCommentContent() error = %v", err)
	}
	cases = append(cases, contractCase{"question", question, CommentMarker("question", runID, "C1")})
	snapshot := TicketSnapshot{RunID: runID, IssueKey: "TICKET-501"}
	cases = append(cases, contractCase{"ack", AckCommentContent(snapshot), CommentMarker("ack", runID)})
	plan := PlanCommentContent(runID, PlanFacts{
		Request:     "一覧の取得失敗時に再試行の導線を出す",
		Rationale:   "失敗メッセージの隣に再試行ボタンを描画し、一覧の取得だけをやり直す。",
		TargetFiles: []string{"client/src/app/example/page.tsx"},
		Assumptions: []string{"再試行は一覧の取得のみを対象とし、組織やカタログは取り直さない"},
	})
	cases = append(cases, contractCase{"plan", plan, CommentMarker("plan", runID)})
	cases = append(cases, contractCase{"e2e", E2ECommentContent(runID, E2EReport{
		Verdict: "pass", TargetURL: "https://staging.example.invalid/console/x",
		ExpectedText: "絞り込み", ExpectedSeen: true, AbsentText: "旧表示", AbsentGone: true,
		ScreenshotAttached: true,
	}), CommentMarker("e2e", runID)})
	receipt, err := ReceiptCommentContent(record, 6002)
	if err != nil {
		t.Fatalf("ReceiptCommentContent() error = %v", err)
	}
	cases = append(cases, contractCase{"receipt", receipt, CommentMarker("answer-receipt", runID, "C1", "6002")})
	cases = append(cases, contractCase{"guidance", GuidanceCommentContent(record), CommentMarker("answer-guidance", runID, "C1")})
	shortfall, err := ShortfallCommentContent(record, 12345, []string{"Q2"})
	if err != nil {
		t.Fatalf("ShortfallCommentContent() error = %v", err)
	}
	cases = append(cases, contractCase{"shortfall", shortfall, CommentMarker("answer-shortfall", runID, "C1", "12345")})
	for index := 1; index <= QuestionNotifyCount; index++ {
		notify, err := NotifyCommentContent(record, index)
		if err != nil {
			t.Fatalf("NotifyCommentContent(%d) error = %v", index, err)
		}
		cases = append(cases, contractCase{fmt.Sprintf("notify-%d", index), notify, CommentMarker("renotify", runID, "C1", strconv.Itoa(index))})
	}
	for _, code := range []TerminalCode{
		TerminalSuccess, TerminalInputRejected, TerminalReadinessRejected, TerminalClarificationRequired,
		TerminalReadinessUnresolved, TerminalClarificationExpired, TerminalCancelled,
		TerminalModelFailed, TerminalNonconverged, TerminalValidationFailed, TerminalReleaseFailed,
		TerminalProductionDeploymentUnverified, TerminalProductionVerificationFailed, TerminalInternalFailed,
	} {
		report := terminalTestRequest(code)
		digest := strings.Repeat("d", 64)
		cases = append(cases, contractCase{
			"terminal-" + string(code),
			fixedTerminalComment(report, digest),
			CommentMarker("terminal", report.AutomationRunID, string(code), digest),
		})
	}

	seen := map[string]bool{}
	for _, tt := range cases {
		if err := ValidateCommentContract(tt.body, tt.marker); err != nil {
			t.Fatalf("%s: %v\n%s", tt.name, err, tt.body)
		}
		if seen[tt.marker] {
			t.Fatalf("marker %q is not unique across comment kinds", tt.marker)
		}
		seen[tt.marker] = true
	}
}

func TestCommentMarkerIsAnchoredToTheFinalLine(t *testing.T) {
	record := messageTestRecord()
	marker := CommentMarker("question", record.AutomationRunID, "C1")
	body, err := QuestionCommentContent(record)
	if err != nil {
		t.Fatalf("QuestionCommentContent() error = %v", err)
	}
	if ExtractCommentMarker(body) != marker {
		t.Fatalf("marker = %q, want %q", ExtractCommentMarker(body), marker)
	}
	// A marker-shaped string quoted inside the body must never be taken for
	// the comment's identity: question text comes from a model and comment
	// bodies come from the requester, so either could contain one.
	forged := CommentMarker("ack", record.AutomationRunID)
	if got := ExtractCommentMarker("見出しに " + forged + " と書いてください\nよろしくお願いします"); got != "" {
		t.Fatalf("a quoted marker was accepted: %q", got)
	}
	if got := ExtractCommentMarker(forged + "\n" + body); got != marker {
		t.Fatalf("a prepended marker won over the footer: %q", got)
	}
	if err := ValidateCommentContract(body, forged); err == nil {
		t.Fatal("a body was validated against a marker it does not carry")
	}
}
