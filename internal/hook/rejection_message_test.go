package hook

import (
	"strings"
	"testing"
)

// The one rejection this engine has is on the input itself: a body that is
// empty, too large, or not readable as text. Nothing else is refused for what
// it says, so the comment that ends a rejection has to name a mechanical
// reason and the one person who can act on it.
//
// Measured live (2026-09-26): a request naming a specification document the
// destination's repository does not have ended here. The requester was told
// their ticket "did not meet the reception conditions", an operator was named
// as the next person to act, and the comment promised the operator would
// explain. Nothing was going to send that message, and the requester had
// nothing to act on either.
func TestARejectedTicketIsToldWhyAndWhoActs(t *testing.T) {
	digest := strings.Repeat("0", 64)
	report := TerminalReportRequest{Code: TerminalReadinessRejected, AutomationRunID: "run-1"}

	comment := TerminalCommentContent(report, digest)
	for _, want := range []string{"読み取れる形ではなかった", "読める本文で起票し直して"} {
		if !strings.Contains(comment, want) {
			t.Errorf("the rejection comment lacks %q:\n%s", want, comment)
		}
	}
	// The sentence that sent the requester away to wait for a message nobody
	// was going to send.
	for _, gone := range []string{"受付条件を満たさなかった", "詳細は運用担当者が確認し"} {
		if strings.Contains(comment, gone) {
			t.Errorf("the rejection comment still says %q:\n%s", gone, comment)
		}
	}
	facts := terminalCommentFacts(report, digest)
	if facts.NextActor != "起票者" {
		t.Errorf("next actor = %q, want the one person who can re-file the ticket", facts.NextActor)
	}
	if !strings.Contains(facts.Operation, "起票し直して") || strings.Contains(facts.Operation, "起票者の操作は不要") {
		t.Errorf("operation = %q, want what the requester does about it", facts.Operation)
	}
	if facts.Production != "未変更" {
		t.Errorf("production = %q", facts.Production)
	}
}

// The actual read-ticket exit uses input_rejected, not readiness_rejected.
// Its requester must get a reason and an action too.
func TestTheMechanicalInputRejectionNamesTheReasonAndTheRequester(t *testing.T) {
	digest := strings.Repeat("0", 64)
	report := TerminalReportRequest{Code: TerminalInputRejected, AutomationRunID: "run-1"}

	comment := TerminalCommentContent(report, digest)
	if !strings.Contains(comment, "入力の読み取り検査") || !strings.Contains(comment, "読める本文で起票し直してください") {
		t.Errorf("the input rejection lacks a reason and action:\n%s", comment)
	}
	if facts := terminalCommentFacts(report, digest); facts.Production != "未変更" || facts.NextActor != "起票者" {
		t.Errorf("facts = %+v", facts)
	}
}
