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

// The lines the reception's state never moves. The 「ご対応のお願い」 line is
// the one the state chooses, and on an open question the two footer items
// that would contradict it; everything else is the same notice in all three
// readings, so each rendering is held to this.
func assertAcceptanceNoticeFrame(t *testing.T, content, runID string) {
	t.Helper()
	// The headline opens the body, so it is the one line with no newline in
	// front of it.
	if !strings.HasPrefix(content, "【受付】このチケットの自動処理を受け付けました。\n") {
		t.Fatalf("the notice does not open with the acceptance headline:\n%s", content)
	}
	for _, line := range []string{
		"処理の所有者: 自動処理（結果はこのチケットのコメントでお知らせします）",
		"最終報告の目安: 受付から 2 時間以内（質問への回答待ちの期間は除きます）",
		"目安を過ぎても最終報告がない場合は、再起票や再実行はせず、プロジェクトの運用窓口へこのチケットの番号を添えてご連絡ください。",
		"状態: 受付済み・自動処理中",
		"次回通知・期限: 最終結果または確認事項を、受付から 2 時間以内を目安に通知",
		"本番の状態: 未変更",
		"自動再試行: なし（webhook 未達時は 5 分周期の照合で受付を補完）",
	} {
		if !strings.Contains(content, "\n"+line+"\n") {
			t.Fatalf("the notice lost the line %q:\n%s", line, content)
		}
	}
	// The promise that was impossible to keep, in any reading: nothing after
	// the reception asks the requester anything.
	if strings.Contains(content, "質問コメントを通知します") {
		t.Fatalf("the notice still promises a later question:\n%s", content)
	}
	if got := ExtractCommentMarker(content); got != CommentMarker("ack", runID) {
		t.Fatalf("marker line = %q", got)
	}
	// The kinds table carries one acceptance notice, so every rendering is
	// held to the seven-item contract here.
	if err := ValidateCommentContract(content, CommentMarker("ack", runID)); err != nil {
		t.Fatalf("the acceptance notice violates the contract: %v", err)
	}
}

// A run is in flight from the claim, and the reception comes some way after
// it, so the notice can be posted with the reception still to come. It is
// posted once and never revised, so that run's requester is told the check
// is unfinished — never that nothing will be asked, which the reception can
// contradict an hour later. The zero value reads this way too.
func TestAcceptanceNoticePromisesNothingUntilTheReceptionHasDecided(t *testing.T) {
	content := AckCommentContent(TicketSnapshot{RunID: "run-42", IssueKey: "TICKET-501"}, ReceptionPending)
	const request = "ご対応のお願い: いまは何もありません。受付の確認が終わるまでお待ちください。依頼者にしか決められない点があれば、受付の時点でまとめて質問します。受付の質問は設定で許された回数（既定は 1 回）までで、それ以降は質問しません。"
	if !strings.Contains(content, "\n"+request+"\n") {
		t.Fatalf("an undecided reception does not say so:\n%s", content)
	}
	if strings.Contains(content, "これ以上ありません") {
		t.Fatalf("an undecided reception closed a door it cannot close:\n%s", content)
	}
	if content != AckCommentContent(TicketSnapshot{RunID: "run-42", IssueKey: "TICKET-501"}, "") {
		t.Fatal("the zero value does not read as pending")
	}
	// Nothing is owed yet, so the footer still sends no one anywhere.
	if !strings.Contains(content, "\n次に行動する人: 自動処理\n") || !strings.Contains(content, "\n操作: 利用者操作なし\n") {
		t.Fatalf("the footer asks for something before the reception has:\n%s", content)
	}
	assertAcceptanceNoticeFrame(t, content, "run-42")
}

// A reception that concluded and went on is the one state in which nothing
// more will be asked, so it is the only one that says so — and it points at
// the plan comment, which is where the stop method actually is and which the
// same evidence says is on the ticket.
func TestAcceptanceNoticeAsksForNothingWhenTheReceptionProceeded(t *testing.T) {
	content := AckCommentContent(TicketSnapshot{RunID: "run-42", IssueKey: "TICKET-501"}, ReceptionProceeded)
	const request = "ご対応のお願い: ありません。受付の確認は完了しており、受付からの質問はこれ以上ありません。方針が違う場合は、方針コメントにある停止の方法をご利用ください。"
	if !strings.Contains(content, "\n"+request+"\n") {
		t.Fatalf("the proceeded reception does not say what is owed:\n%s", content)
	}
	if !strings.Contains(content, "\n次に行動する人: 自動処理\n") || !strings.Contains(content, "\n操作: 利用者操作なし\n") {
		t.Fatalf("the footer asks for something nothing needs:\n%s", content)
	}
	assertAcceptanceNoticeFrame(t, content, "run-42")
}

// When the reception asked, the notice names the answer the requester owes
// and says the asking ends with the reception, and its footer points at the
// same answer instead of saying no action is needed. The sentence names the
// rule rather than a count, because how many sets of questions one reception
// may put is the destination's setting and this package cannot read it.
func TestAcceptanceNoticeAsksOnlyForTheOneAnswerWhenTheReceptionAsked(t *testing.T) {
	content := AckCommentContent(TicketSnapshot{RunID: "run-42", IssueKey: "TICKET-501"}, ReceptionAsked)
	const request = "ご対応のお願い: 上の質問への回答だけです。受付の質問は設定で許された回数（既定は 1 回）までで、それ以降は質問しません。"
	if !strings.Contains(content, "\n"+request+"\n") {
		t.Fatalf("the open question is not named:\n%s", content)
	}
	if strings.Contains(content, "操作: 利用者操作なし") {
		t.Fatalf("the footer contradicts the open question:\n%s", content)
	}
	if !strings.Contains(content, "\n次に行動する人: 起票者（回答者）\n") {
		t.Fatalf("the footer does not name the answerer:\n%s", content)
	}
	assertAcceptanceNoticeFrame(t, content, "run-42")
}
