package hook

import (
	"strings"
	"testing"
)

// The plan notice tells the requester what the reception decided about the
// design stage, in one line and in their terms: the skip and its reason, or
// the design and why it was kept. A run whose decision predates the stage
// says nothing about it.
func TestPlanCommentContentCarriesTheDesignDecisionLine(t *testing.T) {
	skipped := PlanCommentContent("run-42", PlanFacts{Request: "ラベルの文言を変える", DesignReason: "approach_in_ticket"})
	if !strings.Contains(skipped, "\n設計なし: 方針が本文にあるため設計を省略\n") {
		t.Fatalf("skipped design is not reported:\n%s", skipped)
	}
	kept := PlanCommentContent("run-42", PlanFacts{Request: "ラベルの文言を変える", NeedsDesign: true, DesignReason: "approach_not_in_ticket"})
	if !strings.Contains(kept, "\n設計あり: 本文に「どう直すか」が書かれていないため\n") {
		t.Fatalf("kept design is not reported:\n%s", kept)
	}
	// A reason this package has never heard of still shows the decision, with
	// a neutral sentence - never the machine code itself.
	future := PlanCommentContent("run-42", PlanFacts{NeedsDesign: true, DesignReason: "future_code"})
	if !strings.Contains(future, "\n設計あり: 理由は自動処理の記録に残しています\n") {
		t.Fatalf("unknown reason was dropped:\n%s", future)
	}
	if strings.Contains(future, "future_code") {
		t.Fatalf("a machine code reached the requester:\n%s", future)
	}
	if got := DesignDecisionLine(false, "future_code"); got != "設計なし: 理由は自動処理の記録に残しています" {
		t.Fatalf("DesignDecisionLine(unknown, skipped) = %q", got)
	}
	// No reason, no line: the notice never guesses.
	silent := PlanCommentContent("run-42", PlanFacts{Request: "ラベルの文言を変える"})
	if strings.Contains(silent, "設計あり") || strings.Contains(silent, "設計なし") {
		t.Fatalf("a run without a design decision reported one:\n%s", silent)
	}
	for _, content := range []string{skipped, kept, future, silent} {
		if err := ValidateCommentContract(content, CommentMarker("plan", "run-42")); err != nil {
			t.Fatalf("plan comment violates the contract: %v", err)
		}
	}
}

func TestDesignDecisionLineNamesEveryVerdict(t *testing.T) {
	if got := DesignDecisionLine(false, "investigation"); got != "設計なし: 調査の依頼のため設計は行わない" {
		t.Fatalf("DesignDecisionLine(investigation) = %q", got)
	}
	if got := DesignDecisionLine(true, "checker_disagreed"); !strings.HasPrefix(got, "設計あり: ") || !strings.Contains(got, "確認役") {
		t.Fatalf("DesignDecisionLine(checker_disagreed) = %q", got)
	}
	if _, known := DesignReasonPhrase("not-a-reason"); known {
		t.Fatal("an unknown reason was reported as known")
	}
}

// The acceptance notice follows a reception that has already decided, and
// the two decisions leave the requester opposite jobs. When the reception
// proceeded, nothing is owed and nothing will ever be asked: the notice says
// so outright rather than holding out a question no code path can send.
func TestAcceptanceNoticeAsksForNothingWhenTheReceptionProceeded(t *testing.T) {
	content := AckCommentContent(TicketSnapshot{RunID: "run-42", IssueKey: "TICKET-501"}, false)
	const request = "ご対応のお願い: ありません。受付時の確認は完了しており、以後この依頼について質問することはありません。方針が違う場合は停止の方法をご利用ください。"
	if !strings.Contains(content, "\n"+request+"\n") {
		t.Fatalf("the proceeded reception does not say what is owed:\n%s", content)
	}
	// The promise that was impossible to keep: nothing after the reception
	// asks the requester anything.
	if strings.Contains(content, "質問コメントを通知します") {
		t.Fatalf("the notice still promises a later question:\n%s", content)
	}
	if got := ExtractCommentMarker(content); got != CommentMarker("ack", "run-42") {
		t.Fatalf("marker line = %q", got)
	}
}

// When the reception asked its one question, the notice names the single
// answer the requester owes and closes the door behind it, and its footer
// points at the same answer instead of saying no action is needed.
func TestAcceptanceNoticeAsksOnlyForTheOneAnswerWhenTheReceptionAsked(t *testing.T) {
	content := AckCommentContent(TicketSnapshot{RunID: "run-42", IssueKey: "TICKET-501"}, true)
	const request = "ご対応のお願い: 上の質問への回答だけです。この一度きりで、以後は質問しません。"
	if !strings.Contains(content, "\n"+request+"\n") {
		t.Fatalf("the open question is not named:\n%s", content)
	}
	if strings.Contains(content, "操作: 利用者操作なし") {
		t.Fatalf("the footer contradicts the open question:\n%s", content)
	}
	if got := ExtractCommentMarker(content); got != CommentMarker("ack", "run-42") {
		t.Fatalf("marker line = %q", got)
	}
	// The kinds table carries one acceptance notice, so this rendering is
	// held to the seven-item contract here.
	if err := ValidateCommentContract(content, CommentMarker("ack", "run-42")); err != nil {
		t.Fatalf("the acceptance notice violates the contract: %v", err)
	}
}
