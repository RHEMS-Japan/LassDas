package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
)

const questionerTestDecision = `{
  "outcome": "clarification_required",
  "attempts": 1,
  "assessment_sha256s": ["1111111111111111111111111111111111111111111111111111111111111111"],
  "check_sha256s": ["2222222222222222222222222222222222222222222222222222222222222222"],
  "questions": [
    {"id":"Q1","dimension":"user_visible_behavior","question":"一覧の並び順はどちらにしますか","why_blocking":"利用者に見える並びが変わる","choices":[{"id":"a","label":"新着順","effect":"新しい項目が先頭に出る"},{"id":"b","label":"名前順","effect":"五十音順に並ぶ"}]}
  ],
  "decision_sha256": "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
  "tool_sha": "3333333333333333333333333333333333333333",
  "source_sha256": "4444444444444444444444444444444444444444444444444444444444444444"
}`

func writeQuestionerFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// The decision artifact must round-trip through the canonical re-encoding
// into a sealed record that the message renderer and the answer intake both
// accept — a key-order or escaping mismatch here would only surface on the
// first real ticket otherwise.
func TestQuestionerDecisionRoundTripsIntoASealedAnswerableRecord(t *testing.T) {
	path := writeQuestionerFile(t, "decision.json", questionerTestDecision)
	questionsJSON, decisionDigest, err := loadDecision(path)
	if err != nil {
		t.Fatalf("loadDecision() error = %v", err)
	}
	if decisionDigest != strings.Repeat("c", 64) {
		t.Fatalf("decision digest = %s", decisionDigest)
	}
	notifyAt, deadlineAt := hook.ComputeQuestionSchedule(time.Date(2026, 8, 3, 6, 0, 0, 0, time.UTC))
	record := hook.QuestionRecord{
		Protocol:          hook.QuestionProtocolVersion,
		DeliveryID:        "delivery_0123456789abcdef0123456789abcdef",
		InputSHA256:       strings.Repeat("1", 64),
		RepositoryID:      42,
		RepositorySHA256:  hook.HashIdentity("example/automation-receiver"),
		WorkflowRefSHA256: hook.HashIdentity("example/automation-receiver/.github/workflows/m1-worker.yml@refs/heads/main"),
		WorkflowSHA:       strings.Repeat("2", 40),
		WorkflowRunID:     123456789,
		RunAttempt:        1,
		AutomationRunID:   "run_20260802_fixed",
		RunURL:            "https://github.com/example/automation-receiver/actions/runs/123456789/attempts/1",
		QuestionRevision:  1,
		QuestionsJSON:     questionsJSON,
		QuestionsSHA256:   hook.TerminalReportDigest([]byte(questionsJSON)),
		DecisionSHA256:    decisionDigest,
		AnswerDeadlineAt:  deadlineAt,
		NotifyAt:          notifyAt,
	}
	if _, err := hook.MarshalQuestionRecord(record); err != nil {
		t.Fatalf("re-encoded questions do not seal: %v", err)
	}
	content, err := hook.QuestionCommentContent(record)
	if err != nil {
		t.Fatalf("re-encoded questions do not render: %v", err)
	}
	if !strings.Contains(content, "回答 C1 Q1:a") || !strings.Contains(content, "新着順") {
		t.Fatalf("rendered question lacks the pasteable line:\n%s", content)
	}
	decision, err := hook.EvaluateAnswerIntake(hook.AnswerIntakeInput{
		Question:          record,
		QuestionCommentID: 100,
		AnswererID:        7,
		Comments: []hook.BacklogComment{{
			CommentID: 101, UserID: 7, Body: "回答 C1 Q1:a", PostedAt: record.NotifyAt[0],
		}},
	})
	if err != nil || decision.Adopted == nil || decision.Adopted.AnswersJSON != `{"Q1":"a"}` {
		t.Fatalf("re-encoded questions are not answerable: %+v, err = %v", decision, err)
	}
}

func TestQuestionerLoadDecisionFailsClosed(t *testing.T) {
	for _, run := range []struct {
		name string
		body string
	}{
		{name: "wrong outcome", body: strings.Replace(questionerTestDecision, "clarification_required", "ready", 1)},
		{name: "no questions", body: strings.Replace(questionerTestDecision, `"questions": [
    {"id":"Q1","dimension":"user_visible_behavior","question":"一覧の並び順はどちらにしますか","why_blocking":"利用者に見える並びが変わる","choices":[{"id":"a","label":"新着順","effect":"新しい項目が先頭に出る"},{"id":"b","label":"名前順","effect":"五十音順に並ぶ"}]}
  ]`, `"questions": []`, 1)},
		{name: "broken digest", body: strings.Replace(questionerTestDecision, strings.Repeat("c", 64), "short", 1)},
		{name: "not json", body: "readiness says maybe"},
	} {
		t.Run(run.name, func(t *testing.T) {
			path := writeQuestionerFile(t, "decision.json", run.body)
			if _, _, err := loadDecision(path); err == nil {
				t.Fatal("broken decision was accepted")
			}
		})
	}
}

func TestQuestionerDerivesTheRoundFromTheEnvelopeClarification(t *testing.T) {
	// Round 1: no clarification on the envelope.
	revision, digest, err := questionRevisionFromEnvelope(hook.DispatchEnvelope{})
	if err != nil || revision != 1 || digest != "" {
		t.Fatalf("fresh envelope: revision=%d digest=%q err=%v", revision, digest, err)
	}

	// Round 2: the sealed record of the adopted round-1 answer rides on the
	// envelope; the next question must chain to its digest.
	question := hook.QuestionRecord{
		Protocol:          hook.QuestionProtocolVersion,
		DeliveryID:        "delivery_0123456789abcdef0123456789abcdef",
		InputSHA256:       strings.Repeat("1", 64),
		RepositoryID:      42,
		RepositorySHA256:  hook.HashIdentity("example/automation-receiver"),
		WorkflowRefSHA256: hook.HashIdentity("example/automation-receiver/.github/workflows/m1-worker.yml@refs/heads/main"),
		WorkflowSHA:       strings.Repeat("2", 40),
		WorkflowRunID:     123456789,
		RunAttempt:        1,
		AutomationRunID:   "run_20260802_fixed",
		RunURL:            "https://github.com/example/automation-receiver/actions/runs/123456789/attempts/1",
		QuestionRevision:  1,
		QuestionsJSON:     `[{"id":"Q1","question":"scope?","choices":[{"id":"a"},{"id":"b"}]}]`,
		DecisionSHA256:    strings.Repeat("c", 64),
		AnswerDeadlineAt:  4_000,
		NotifyAt:          [3]int64{1_000, 2_000, 3_000},
	}
	question.QuestionsSHA256 = hook.TerminalReportDigest([]byte(question.QuestionsJSON))
	encodedQuestion, err := hook.MarshalQuestionRecord(question)
	if err != nil {
		t.Fatalf("MarshalQuestionRecord() error = %v", err)
	}
	answers := `{"Q1":"a"}`
	record := hook.ClarificationRecord{
		Protocol:          hook.ClarificationProtocolVersion,
		DeliveryID:        question.DeliveryID,
		InputSHA256:       question.InputSHA256,
		RepositoryID:      question.RepositoryID,
		RepositorySHA256:  question.RepositorySHA256,
		WorkflowRefSHA256: question.WorkflowRefSHA256,
		AutomationRunID:   question.AutomationRunID,
		InputRevision:     2,
		Rounds: []hook.ClarificationRound{{
			QuestionRecordJSON:   string(encodedQuestion),
			QuestionRecordSHA256: hook.TerminalReportDigest(encodedQuestion),
			QuestionCommentID:    500,
			AnswerCommentID:      600,
			AnswererID:           7,
			AnswerPostedAt:       3_500,
			AnswerBodySHA256:     strings.Repeat("b", 64),
			AnswersJSON:          answers,
			AnswersSHA256:        hook.TerminalReportDigest([]byte(answers)),
		}},
	}
	sealed, err := hook.MarshalClarificationRecord(record)
	if err != nil {
		t.Fatalf("MarshalClarificationRecord() error = %v", err)
	}
	revision, digest, err = questionRevisionFromEnvelope(hook.DispatchEnvelope{ClarificationJSON: string(sealed)})
	if err != nil || revision != 2 || digest != hook.TerminalReportDigest(sealed) {
		t.Fatalf("resumed envelope: revision=%d digest=%q err=%v", revision, digest, err)
	}
	if _, _, err := questionRevisionFromEnvelope(hook.DispatchEnvelope{ClarificationJSON: `{"forged":true}`}); err == nil {
		t.Fatal("a corrupted clarification was accepted")
	}
}
