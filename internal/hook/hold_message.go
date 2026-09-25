package hook

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The notices the attendant puts on a ticket when nobody is at fault and
// nothing has gone wrong with the request: a delivery that cannot start
// because a key is out of money or a sign-in has lapsed, one that is queued
// behind an operator's pause, and one that is still going but is waiting on
// something outside it. Each is said once, in requester terms, with the
// seven-item footer, and each resumes by itself.
//
// The hold that stopped intake after several deliveries ended the same way
// was said here too. It counted endings, and a card that fails is climbed
// away from rather than reported now, so the run of identical endings it
// watched for cannot form. Its two comment kinds stay in the vocabulary:
// tickets carry them.

// DescribeTerminalCode renders a failure ending the way a requester would
// read it, with the machine code kept in parentheses for the operator.
func DescribeTerminalCode(code string) string {
	switch TerminalCode(code) {
	case TerminalInputRejected:
		return "依頼の入力検査で終了 (input_rejected)"
	case TerminalReadinessRejected:
		return "受付の審査で終了 (readiness_rejected)"
	case TerminalClarificationRequired:
		return "依頼内容の確認が必要 (clarification_required)"
	case TerminalReadinessUnresolved:
		return "依頼の不明点を解消できず終了 (readiness_unresolved)"
	case TerminalClarificationExpired:
		return "回答期限を過ぎて終了 (clarification_expired)"
	case TerminalInvestigationIncomplete:
		return "調査・設計の記録を完成できず終了 (investigation_incomplete)"
	case TerminalInvestigationNonconverged:
		return "調査結果のレビューが収束せず終了 (investigation_nonconverged)"
	case TerminalDesignNonconverged:
		return "設計のレビューが収束せず終了 (design_nonconverged)"
	case TerminalDesignRoundsSpent:
		return "設計をやり直す回数を使い切って終了 (design_rounds_spent)"
	case TerminalImplementationReturned:
		return "実装役が変更せずに理由を報告して終了 (implementation_returned)"
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

// LadderWaitRung names the one rung that tells the ticket anything. It is a
// word rather than the rung's number so that the notice already on a ticket
// keeps its meaning if the rungs above it are ever renumbered.
const LadderWaitRung = "wait"

// LadderNoticeMarker names one rung of one stage of one run, so the notice
// below is posted exactly once however many ticks the wait lasts, and a
// later stage waiting for its own reason is still told.
func LadderNoticeMarker(runID, stage, rung string) string {
	return CommentMarker(string(RunCommentLadder), runID, stage, rung)
}

// LadderWaitContent says the delivery is still going and is taking longer
// than usual at one step. It asks for nothing: the work resumes by itself,
// and this exists so that a delivery which has been inside one stage for
// half an hour does not read as a delivery that stopped.
//
// It names no cause on purpose. Everything that runs out of remedies ends
// on this rung — a model that would not answer, a destination that refuses
// the change every time, a failure nobody could name — and a sentence
// blaming an outside service would be wrong for most of them and would send
// an operator looking in the wrong place. What it can say truthfully is
// what the delivery is doing and where the reason is written down.
func LadderWaitContent(runID, stage string) string {
	body := "【処理は続いています】この工程が完了しないため、間隔を空けて試し続けています。" +
		"依頼は止まっていません。完了した時点で、人の操作なしで続きの工程へ進みます。\n\n" +
		"原因の記録は運用担当者が確認できます（この依頼の作業ディレクトリに、工程ごとの失敗の記録が残ります）。\n\n"
	return body + CommentFacts{
		State:      "同じ工程を再試行中（処理は継続中）",
		NextActor:  "なし（自動で再試行します）",
		Operation:  "対応不要（起票者の操作は不要です。運用担当者は必要なら記録を確認してください）",
		NextEvent:  "間隔を空けて再試行し、完了した時点で続きの工程へ進みます",
		Production: "未変更",
		AutoRetry:  "あり（間隔を空けて継続）",
		Marker:     LadderNoticeMarker(runID, stage, LadderWaitRung),
	}.render()
}

// KeyLimitReachedContent says the provider refused because the key has
// reached its spending limit. It is the one thing on this rung that will
// not clear by itself: every model the engine could move to is reached
// through the same key, so the only remedy is the limit being raised or
// resetting. The delivery is kept, not ended, and resumes from where it
// stopped.
func KeyLimitReachedContent(runID, stage string) string {
	body := "【AI の利用枠の上限に達しました】自動処理に使う AI の利用枠が上限に達したため、この工程を間隔を空けて試し続けています。" +
		"依頼は止まっていません。運用担当者が上限を上げるか、利用枠がリセットされると、人の操作なしで続きから進みます。\n\n"
	return body + CommentFacts{
		State:      "利用枠の上限に達して待機中（処理は継続中）",
		NextActor:  "運用担当者",
		Operation:  "利用枠の上限を上げる、またはリセットを待つ（起票者の操作は不要です）",
		NextEvent:  "間隔を空けて再試行し、利用できるようになった時点で続きの工程へ進みます",
		Production: "未変更",
		AutoRetry:  "あり（間隔を空けて継続）",
		Marker:     LadderNoticeMarker(runID, stage, LadderWaitRung),
	}.render()
}

// IntakePausedContent is the one notice a queued ticket gets while the
// operator's pause is in force: the request was received, nothing is
// wrong with it, and it starts when intake resumes.
func IntakePausedContent(runID string, since time.Time) string {
	when := since.In(questionZone).Format("2006-01-02 15:04")
	marker := IntakePausedMarker(runID, since)
	body := fmt.Sprintf(
		"【受付停止中】この依頼は受け付けましたが、現在は運用者の指示で新しい依頼の開始を止めています（停止: %s）。再開後にこの依頼を開始します。実行中の依頼はそのまま進みます。\n\n",
		when)
	return body + CommentFacts{
		State:      "受付停止中（運用者の指示）",
		NextActor:  "運用担当者",
		Operation:  "再開の操作（設定の intake_paused_since を外す）",
		NextEvent:  "再開後に開始し、開始できた時点で実装方針を通知",
		Production: "未変更",
		AutoRetry:  "なし（再開待ち）",
		Marker:     marker,
	}.render()
}

// IntakePausedMarker names one pause on one ticket: the run and the pause
// instant, so a later pause after a resumption is told again while the
// same pause is told once.
func IntakePausedMarker(runID string, since time.Time) string {
	return CommentMarker(string(RunCommentIntakePaused), runID, strconv.FormatInt(since.Unix(), 10))
}

// StopAcknowledgedMarker names the one acknowledgement a run's stop gets,
// so the tick that reads the stop again — and a tick will, for as long as
// the comment is on the ticket — finds its own answer rather than posting
// a second one.
func StopAcknowledgedMarker(runID string) string {
	return CommentMarker(string(RunCommentStopAck), runID)
}

// StopAcknowledgedContent answers 「停止」 in the tick that read it.
//
// It is one line because it says one thing: you were heard, and the steps
// that are running are being stopped. Everything else a requester needs —
// what had already landed, what was left where — belongs to the closing
// report, which is written from the records the steps sealed rather than
// from what the engine believes it did. Saying any of it here would be
// guessing at a moment when a step may still be finishing.
//
// So the production line does not answer either. A stop can arrive after a
// merge has landed and after a deployment has run, and this notice has not
// read the records that would tell it which; the closing report has.
func StopAcknowledgedContent(runID string) string {
	body := "停止を受け付けました。実行中の工程を止めています。\n"
	return body + CommentFacts{
		State:      "停止の処理中",
		NextActor:  "なし（自動で停止します）",
		Operation:  "対応不要（起票者の操作は不要です）",
		NextEvent:  "停止した時点で、ここまでに届いた範囲を最終コメントで報告します",
		Production: "最終コメントで報告します",
		AutoRetry:  "なし（停止します）",
		Marker:     StopAcknowledgedMarker(runID),
	}.render()
}
