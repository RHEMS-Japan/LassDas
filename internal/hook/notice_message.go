package hook

import (
	"fmt"
	"strings"
)

// AckCommentContent is the acceptance notice posted once per run: who owns
// the processing, that no user action is needed unless a question follows,
// and when to expect the final report (README「Backlog 上の表示と通知」受付 /
// 結果未着). Deterministic from the sealed snapshot.
func AckCommentContent(snapshot TicketSnapshot) string {
	var builder strings.Builder
	builder.WriteString("【受付】このチケットの自動処理を受け付けました。\n\n")
	builder.WriteString("処理の所有者: 自動処理（結果はこのチケットのコメントでお知らせします）\n")
	builder.WriteString("ご対応のお願い: いまは何もありません。依頼者にしか決められない確認事項が見つかった場合のみ、質問コメントを通知します。\n")
	builder.WriteString("最終報告の目安: 受付から 2 時間以内（質問への回答待ちの期間は除きます）\n")
	builder.WriteString("目安を過ぎても最終報告がない場合は、再起票や再実行はせず、プロジェクトの運用窓口へこのチケットの番号を添えてご連絡ください。\n")
	return builder.String() + CommentFacts{
		State:      "受付済み・自動処理中",
		NextActor:  "自動処理",
		Operation:  "利用者操作なし",
		NextEvent:  "最終結果または確認事項を、受付から 2 時間以内を目安に通知",
		Production: "未変更",
		AutoRetry:  "なし（webhook 未達時は 5 分周期の照合で受付を補完）",
		Marker:     CommentMarker("ack", snapshot.RunID),
	}.render()
}

// PlanFacts is what the plan notice shows the requester: the automation's
// reading of the ticket, where it intends to write, and the assumptions the
// readiness gate decided to proceed on. Every field is optional — the notice
// renders whatever the run directory could provide.
type PlanFacts struct {
	Request     string
	Rationale   string
	TargetFiles []string
	Assumptions []string
}

const (
	// planTextMaxRunes keeps each prose part to a requester-sized paragraph;
	// the full text stays in the sealed run directory, not the ticket.
	planTextMaxRunes = 600
	planListMaxItems = 12
	planItemMaxRunes = 200
	// planBodyMaxBytes keeps the body clear of the tracker's comment-size
	// limit (16 KiB) with room to spare for the footer: an overflowing body
	// is cut, the footer — and with it the machine marker the exactly-once
	// machinery anchors on — never is.
	planBodyMaxBytes = 12 * 1024
)

// PlanCommentContent is the implementation-plan notice posted once per run
// after the readiness gate passes and before the first card dispatches: what
// the automation is about to build, where it intends to write, and how to
// stop it. A notice, not a gate — the run continues whether or not the
// requester reads it.
func PlanCommentContent(runID string, facts PlanFacts) string {
	var builder strings.Builder
	builder.WriteString("【実装方針】受付審査を通過したため、次の方針で実装を開始します。ご対応は不要です（方針が違う場合のみ、下の停止方法をご利用ください）。\n")
	if request := truncatePlanText(facts.Request); request != "" {
		builder.WriteString("\n依頼の解釈: " + request + "\n")
	}
	if rationale := truncatePlanText(facts.Rationale); rationale != "" {
		builder.WriteString("\n方針: " + rationale + "\n")
	}
	writePlanList(&builder, "触る予定の範囲", facts.TargetFiles)
	writePlanList(&builder, "前提とした解釈（曖昧だった点はこう進めます）", facts.Assumptions)
	builder.WriteString("\n方針を止めたい場合: このチケットに「停止」とだけ書いたコメントを投稿してください。指摘によるやり直し（次のラウンド）が始まる前までに反映され、以後の新しい工程は開始されません。実行中の工程は最後まで走り切り、指摘なしで最後まで進んだ場合は Pull Request をマージしないことで反映を止められます。確認の質問が出ている間は、質問コメントに記載の中止方法（「中止 C番号」）に従ってください。\n")
	return capPlanBody(builder.String()) + CommentFacts{
		State:      "実装方針を掲示・自動処理中",
		NextActor:  "自動処理（方針を変えたい場合のみ依頼者）",
		Operation:  "方針が違う場合のみ「停止」とコメント",
		NextEvent:  "最終結果または確認事項を、受付から 2 時間以内を目安に通知",
		Production: "未変更",
		AutoRetry:  "なし（この掲示の投稿に失敗しても再送されず、処理はそのまま継続します）",
		Marker:     CommentMarker("plan", runID),
	}.render()
}

func truncatePlanText(text string) string {
	return truncatePlanRunes(strings.TrimSpace(text), planTextMaxRunes)
}

func truncatePlanRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…（以下略）"
}

// capPlanBody bounds the whole body below the tracker's comment limit. The
// cut lands on a rune boundary so the notice stays valid UTF-8.
func capPlanBody(body string) string {
	if len(body) <= planBodyMaxBytes {
		return body
	}
	runes := []rune(body)
	for len(runes) > 0 && len(string(runes)) > planBodyMaxBytes {
		runes = runes[:len(runes)*planBodyMaxBytes/len(string(runes))]
	}
	return string(runes) + "\n…（長いため以下略）\n"
}

func writePlanList(builder *strings.Builder, heading string, items []string) {
	shown := 0
	for index, item := range items {
		entry := truncatePlanRunes(strings.TrimSpace(item), planItemMaxRunes)
		if entry == "" {
			continue
		}
		if shown == 0 {
			builder.WriteString("\n" + heading + ":\n")
		}
		if shown == planListMaxItems {
			fmt.Fprintf(builder, "- （他 %d 件）\n", len(items)-index)
			return
		}
		builder.WriteString("- " + entry + "\n")
		shown++
	}
}

// ReceiptCommentContent is the answer receipt posted once per adopted round:
// which answer comment won, and that the run resumed with it (README 回答受領
// の 10 分 SLO).
func ReceiptCommentContent(record QuestionRecord, answerCommentID int64) (string, error) {
	if err := record.ValidateShape(); err != nil {
		return "", err
	}
	if answerCommentID <= 0 {
		return "", fmt.Errorf("receipt answer comment is invalid")
	}
	tag := questionRevisionTag(record.QuestionRevision)
	var builder strings.Builder
	fmt.Fprintf(&builder, "【回答受領 %s】回答（コメント #%d)を受領し、その内容で自動処理を再開しました。\n\n", tag, answerCommentID)
	builder.WriteString("選択いただいた内容は実装とレビューの判断に反映されます。追加のご対応は不要です。\n")
	return builder.String() + CommentFacts{
		State:      "回答受領・自動処理再開（質問 " + tag + "）",
		NextActor:  "自動処理",
		Operation:  "利用者操作なし",
		NextEvent:  "最終結果または追加の確認事項を、再開から 2 時間以内を目安に通知",
		Production: "未変更",
		AutoRetry:  "なし（再開後の処理は自動で進みます）",
		Marker:     CommentMarker("answer-receipt", record.AutomationRunID, tag, fmt.Sprintf("%d", answerCommentID)),
	}.render(), nil
}
