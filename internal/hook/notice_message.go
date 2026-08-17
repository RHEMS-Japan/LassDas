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
