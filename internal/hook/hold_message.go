package hook

import (
	"fmt"
	"strings"
)

// The attendant's two operator holds. Neither is a failure of the delivery:
// one waits for money, the other for a person to look at a pattern. Both
// are announced once, in requester terms, with the seven-item footer.

// DescribeTerminalCode renders a failure ending the way a requester would
// read it, with the machine code kept in parentheses for the operator.
func DescribeTerminalCode(code string) string {
	switch TerminalCode(code) {
	case TerminalModelFailed:
		return "AI の応答が得られず終了 (model_failed)"
	case TerminalNonconverged:
		return "レビューが収束せず終了 (nonconverged)"
	case TerminalValidationFailed:
		return "検証が失敗して終了 (validation_failed)"
	case TerminalReleaseFailed:
		return "納品に失敗して終了 (release_failed)"
	case TerminalInternalFailed:
		return "内部エラーで終了 (internal_failed)"
	case TerminalProductionDeploymentUnverified, TerminalProductionVerificationFailed:
		return "本番反映の確認ができず終了 (" + code + ")"
	default:
		return code
	}
}

// BudgetHoldContent announces that a delivery cannot start because the
// gateway refuses a role's key for budget: the run waits and retries by
// itself once the cap is raised, so the requester has nothing to do.
func BudgetHoldContent(runID string, roles []string) string {
	var builder strings.Builder
	builder.WriteString("【予算不足のため開始できません】自動処理に使う AI の利用枠が上限に達しているため、この依頼の処理を開始できません。運用担当者が上限を上げると、人の操作なしで自動的に開始します。\n\n")
	if len(roles) > 0 {
		builder.WriteString("上限に達している役割: " + strings.Join(roles, "、") + "\n")
	}
	return builder.String() + CommentFacts{
		State:      "予算不足で待機中",
		NextActor:  "運用担当者",
		Operation:  "該当する利用枠の上限を上げる（起票者の操作は不要です）",
		NextEvent:  "10 分ごとに再確認し、開始できた時点で実装方針を通知",
		Production: "未変更",
		AutoRetry:  "あり（10 分ごと）",
		Marker:     CommentMarker(string(RunCommentBudgetHold), runID),
	}.render()
}

// SessionHoldContent announces that a delivery cannot start because the
// observation browser could not sign in to a destination's staging: the
// session jar an operator provisioned is no longer accepted, and the run
// would only end as an unjudged screen. It waits and retries by itself
// once the jar is renewed, so the requester has nothing to do.
func SessionHoldContent(runID string, destinations []string) string {
	var builder strings.Builder
	builder.WriteString("【確認用のログイン状態が切れているため開始できません】納品後の画面確認に使うログイン状態を取り直せないため、この依頼の処理を開始できません。運用担当者が確認用のログインをやり直すと、人の操作なしで自動的に開始します。\n\n")
	if len(destinations) > 0 {
		builder.WriteString("ログインできなかった確認先: " + strings.Join(destinations, "、") + "\n")
	}
	return builder.String() + CommentFacts{
		State:      "確認用のログイン状態切れで待機中",
		NextActor:  "運用担当者",
		Operation:  "確認用のログインをやり直し、セッション情報を更新する（起票者の操作は不要です）",
		NextEvent:  "10 分ごとに再確認し、開始できた時点で実装方針を通知",
		Production: "未変更",
		AutoRetry:  "あり（10 分ごと）",
		Marker:     CommentMarker(string(RunCommentSessionHold), runID),
	}.render()
}

// FailureStreakContent announces that intake is held because the same
// failure ended the last N deliveries in a row.
func FailureStreakContent(runID, code string, count int) string {
	body := fmt.Sprintf(
		"【同じ失敗が %d 回連続】直近の自動処理が %d 回続けて同じ結果 — %s — になりました。仕組みの側に原因がある可能性が高いため、運用担当者が原因を確認するまで、新しい依頼の受付を止めます。\n\n確認が済んだら、このチケットに「確認済み」とだけ書いたコメントを投稿してください。受付を再開します。\n",
		count, count, DescribeTerminalCode(code))
	return body + CommentFacts{
		State:      "受付停止（同じ失敗の連続）",
		NextActor:  "運用担当者",
		Operation:  "原因を確認し、このチケットに「確認済み」とコメント",
		NextEvent:  "「確認済み」を検知した時点で受付を再開（以後の自動通知はありません）",
		Production: "未変更",
		AutoRetry:  "なし（人の確認待ち）",
		Marker:     CommentMarker(string(RunCommentStreakHold), runID),
	}.render()
}

// StreakResolvedContent acknowledges the operator's 「確認済み」 on a streak
// hold: intake resumes.
func StreakResolvedContent(runID string) string {
	return "【運用担当者の確認を記録】受付停止を解除し、新しい依頼の受付を再開します。\n" + CommentFacts{
		State:      "受付再開",
		NextActor:  "なし（記録です）",
		Operation:  "対応不要",
		NextEvent:  "以後の自動通知はありません",
		Production: "未変更",
		AutoRetry:  "なし",
		Marker:     CommentMarker(string(RunCommentStreakResolved), runID),
	}.render()
}
