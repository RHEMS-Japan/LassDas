package hook

import (
	"strings"
	"testing"
)

// Every
// comment body the automation itself posts must be completely inert to the
// answer intake (no cancel, no adoption, no reply), because with the answer
// signal each of these comments now triggers a tick that re-reads them.
func TestAutomationCommentsAreInertToIntake(t *testing.T) {
	record := messageTestRecord()
	question, err := QuestionCommentContent(record)
	if err != nil {
		t.Fatal(err)
	}
	shortfall, err := ShortfallCommentContent(record, 101, []string{"Q2"})
	if err != nil {
		t.Fatal(err)
	}
	notify1, err := NotifyCommentContent(record, 1)
	if err != nil {
		t.Fatal(err)
	}
	notify3, err := NotifyCommentContent(record, 3)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := ReceiptCommentContent(record, 777)
	if err != nil {
		t.Fatal(err)
	}
	bodies := map[string]string{
		"question":  question,
		"guidance":  GuidanceCommentContent(record),
		"shortfall": shortfall,
		"notify1":   notify1,
		"notify3":   notify3,
		"ack":       AckCommentContent(TicketSnapshot{RunID: "TICKET-505"}),
		"receipt":   receipt,
	}
	creator := terminalTestConfig().AllowedCreatorID
	id := int64(200)
	for name, body := range bodies {
		id++
		trimmed := strings.TrimSpace(normalizeAnswerBody(body))
		if answerCandidatePattern.MatchString(trimmed) {
			t.Fatalf("%s body is an answer candidate:\n%s", name, body)
		}
		if cancelPattern.MatchString(firstContentLine(normalizeAnswerBody(body))) {
			t.Fatalf("%s body is a cancel:\n%s", name, body)
		}
		decision, err := EvaluateAnswerIntake(AnswerIntakeInput{
			Question: record, QuestionCommentID: 100, AnswererID: creator,
			Comments: []BacklogComment{{CommentID: id, UserID: creator, Body: body, PostedAt: record.AnswerDeadlineAt - 1}},
		})
		if err != nil {
			t.Fatalf("%s: intake error %v", name, err)
		}
		if decision.Cancel != nil || decision.Adopted != nil || len(decision.Replies) != 0 {
			t.Fatalf("%s body is not inert: %+v", name, decision)
		}
	}
}
