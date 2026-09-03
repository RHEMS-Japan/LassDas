package hook

import (
	"fmt"
	"strings"
)

// The v2 delivery talks to the requester exactly twice: once when the
// change reached staging (with the evidence and the Go instructions), and
// once when the production run ended (with the production evidence). Both
// comments ride the same exactly-once machinery as every other run comment.

// PromotionPreview is what a Go would carry to production RIGHT NOW: the
// whole integration branch, never one ticket, so the requester sees the
// full list before approving it.
type PromotionPreview struct {
	AheadBy int
	Titles  []string
	// Behind marks a release branch that carries commits the integration
	// branch does not (a direct hotfix): "already identical" would be a
	// lie there.
	Behind      bool
	Truncated   bool
	Unavailable bool
}

// DeliverStagingReport is the staging-phase summary the attendant renders.
type DeliverStagingReport struct {
	Verdict string // pass | checks_failed | merge_failed | merge_unverified | deploy_failed | observe_failed | observe_blocked | stopped | card_failed
	// Block names, with observe_blocked, why the page could not be judged:
	// "sign_in" (the login did not land) or "redirect" (the page sent the
	// browser elsewhere). It decides who has to act.
	Block              string
	TargetURL          string
	ExpectedText       string
	AbsentText         string
	Detail             string
	ScreenshotAttached bool
	Preview            PromotionPreview
	GoDeadlineDays     int
	// PromotionHold, when set, replaces the Go instructions: the promotion
	// gate could not pass right now, and asking for a Go would only fail.
	PromotionHold string
	// ScreenChecked distinguishes a machine-verified pass from the
	// reference path's deploy-only pass; the headline must never claim a
	// screen check that did not happen.
	ScreenChecked bool
}

// DeliverStagingContent renders the staging report with the Go
// instructions. Only a pass carries them: without the sealed evidence a Go
// cannot open the promotion anyway.
func DeliverStagingContent(runID string, report DeliverStagingReport) string {
	var builder strings.Builder
	switch report.Verdict {
	case "pass":
		if report.ScreenChecked {
			builder.WriteString("【ステージング反映済み】変更をステージングに反映し、画面を自動確認しました。結果: 合格です。\n\n")
		} else {
			builder.WriteString("【ステージング反映済み】変更をステージングに反映しました。画面の合否確認は行っていません（チケットに画面表示の約束が無いため）。\n\n")
		}
	case "checks_failed":
		builder.WriteString("【ステージング反映できず】納品した変更の自動検査 (CI) が全部緑になりませんでした。ステージングへの反映は行っていません。\n\n")
	case "merge_failed":
		builder.WriteString("【ステージング反映できず】ステージングへの自動マージが完了しませんでした。\n\n")
	case "deploy_failed":
		builder.WriteString("【ステージング反映が未確認】マージ後、ステージングの自動デプロイの完了を確認できませんでした。\n\n")
	case "observe_failed":
		builder.WriteString("【ステージング確認が不合格】変更はステージングに反映されましたが、画面の自動確認が合格しませんでした。本番反映は行えません。\n\n")
	case "observe_blocked":
		builder.WriteString("【ステージング確認ができず】変更はステージングに反映されましたが、確認用の画面を開けなかったため、画面の合否を判定できませんでした。変更が正しいかどうかはこの結果からは分かりません。本番反映は行えません。\n\n")
	case "stopped":
		builder.WriteString("【停止を受け付けました】ご指示によりステージングへの反映を行わず停止しました。\n\n")
	case "merge_unverified":
		builder.WriteString("【ステージング反映が不明】自動マージの結果を確認できませんでした。ステージングに反映された可能性があります。お手数ですが状態の確認をお願いします。\n\n")
	default:
		builder.WriteString("【ステージング反映の工程が停止】工程が結果を残さず終了しました（実行上限または実行環境の問題）。\n\n")
	}
	if report.TargetURL != "" {
		fmt.Fprintf(&builder, "確認した画面: %s\n", report.TargetURL)
	}
	if report.ExpectedText != "" {
		fmt.Fprintf(&builder, "出ているべき表示: 「%s」\n", report.ExpectedText)
	}
	if report.AbsentText != "" {
		fmt.Fprintf(&builder, "消えているべき表示: 「%s」\n", report.AbsentText)
	}
	if report.Detail != "" {
		fmt.Fprintf(&builder, "補足: %s\n", report.Detail)
	}
	if report.ScreenshotAttached {
		builder.WriteString("確認時点の画面全体のスクリーンショットをこのコメントに添付しています。\n")
	}
	if report.Verdict == "pass" && report.PromotionHold != "" {
		builder.WriteString("\n■ 本番反映について\n")
		builder.WriteString(report.PromotionHold + "\n")
		builder.WriteString("この状態では「Go」とコメントいただいても自動の本番反映は行われません。\n")
		builder.WriteString(renderHeldPreview(report.Preview))
	} else if report.Verdict == "pass" {
		builder.WriteString("\n■ 本番へ反映するには\n")
		fmt.Fprintf(&builder, "このチケットに「Go」とだけコメントしてください（依頼者ご本人のコメントのみ有効・%d 日以内）。\n", report.GoDeadlineDays)
		builder.WriteString(renderPreview(report.Preview))
	}
	facts := CommentFacts{
		AutoRetry: "なし（この確認は 1 回のみ実施します）",
		Marker:    CommentMarker("stg-report", runID),
	}
	switch {
	case report.Verdict == "pass" && report.PromotionHold != "":
		facts.State = "ステージング反映済み・本番反映は自動では行いません"
		facts.NextActor = "運用担当者"
		facts.Operation = "本番へ入れる場合は運用の昇格手順で反映します"
		facts.NextEvent = "以後の自動通知はありません"
		facts.Production = "未変更"
	case report.Verdict == "pass":
		facts.State = "ステージング反映済み・本番反映の承認待ち"
		facts.NextActor = "依頼者"
		facts.Operation = "内容に問題がなければ「Go」とコメント。反映しない場合は何もしないでください"
		facts.NextEvent = fmt.Sprintf("「Go」で本番反映が始まります（%d 日以内にない場合、本番反映は行わず終了します）", report.GoDeadlineDays)
		facts.Production = "未変更（Go があるまで本番には反映されません）"
	case report.Verdict == "deploy_failed" || report.Verdict == "merge_unverified":
		// The staging state itself needs an operator's eyes; the requester
		// has nothing to fix.
		facts.State = "ステージング反映の工程で停止"
		facts.NextActor = "運用担当者"
		facts.Operation = "ステージングのブランチとデプロイの状態を確認します"
		facts.NextEvent = "以後の自動通知はありません"
		facts.Production = "未変更（本番反映は行われません）"
	case report.Verdict == "observe_blocked" && report.Block == "sign_in":
		// The observation never reached the page: the session jar is the
		// operator's to renew, and the requester's change is unjudged.
		facts.State = "ステージング反映済み・画面確認は判定不能（ログイン状態切れ）"
		facts.NextActor = "運用担当者"
		facts.Operation = "確認用のログインをやり直し、このチケットに「確認済み」とコメント。画面の確認が必要なら依頼を起票し直します"
		facts.NextEvent = "以後の自動通知はありません"
		facts.Production = "未変更（本番反映は行われません）"
	case report.Verdict == "observe_blocked":
		facts.State = "ステージング反映済み・画面確認は判定不能（別の画面へ転送）"
		facts.NextActor = "依頼者"
		facts.Operation = "転送されない画面を確認先に指定して起票し直してください（この依頼はここで終了。運用担当者がこのチケットに「確認済み」とコメントすると、進み具合の板から消えます）"
		facts.NextEvent = "以後の自動通知はありません"
		facts.Production = "未変更（本番反映は行われません）"
	default:
		facts.State = "ステージング反映の工程で停止"
		facts.NextActor = "依頼者"
		facts.Operation = "内容をご確認のうえ、必要なら修正の依頼を起票してください"
		facts.NextEvent = "以後の自動通知はありません"
		facts.Production = "未変更（本番反映は行われません）"
	}
	return builder.String() + facts.render()
}

// renderHeldPreview lists what sits on staging when the promotion gate is
// held. The Go-flavoured wording of renderPreview ("would go to production
// with this") has no place here — nothing is going to production.
func renderHeldPreview(preview PromotionPreview) string {
	if preview.Unavailable {
		return "※ ステージングに滞留している変更の一覧は取得できませんでした。\n"
	}
	if preview.AheadBy == 0 {
		// identical / behind: the hold reason already describes the state.
		return ""
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "ステージングに滞留している変更（全 %d 件）:\n", preview.AheadBy)
	for _, title := range preview.Titles {
		fmt.Fprintf(&builder, "- %s\n", title)
	}
	if preview.Truncated || len(preview.Titles) < preview.AheadBy {
		builder.WriteString("- （ほか省略）\n")
	}
	return builder.String()
}

func renderPreview(preview PromotionPreview) string {
	if preview.Unavailable {
		return "※ 一緒に本番へ行く変更の一覧は取得できませんでした。本番反映はステージング全体を写します。\n"
	}
	if preview.AheadBy == 0 {
		if preview.Behind {
			return "一緒に本番へ行く変更: なし。※ 本番にはステージングに無い変更が入っています。反映前に運用確認が必要です。\n"
		}
		return "一緒に本番へ行く変更: なし（ステージングと本番は既に同じ内容です）。\n"
	}
	if preview.Behind {
		return "※ 本番にはステージングに無い変更が入っています（分岐状態）。この状態では通常の本番反映は通りません。運用確認が必要です。\n"
	}
	if preview.AheadBy == 1 && len(preview.Titles) <= 1 {
		return "一緒に本番へ行く変更: この依頼の変更のみです。\n"
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "一緒に本番へ行く変更（全 %d 件・本番反映はステージング全体を写します）:\n", preview.AheadBy)
	for _, title := range preview.Titles {
		fmt.Fprintf(&builder, "- %s\n", title)
	}
	if preview.Truncated || len(preview.Titles) < preview.AheadBy {
		builder.WriteString("- （ほか省略）\n")
	}
	return builder.String()
}

// DeliverReleaseReport is the production-phase summary.
type DeliverReleaseReport struct {
	Verdict string // pass | promotion_failed | merge_unverified | deploy_failed | observe_failed | observe_blocked | expired | card_failed
	// Block is the observe_blocked reason, as on the staging report.
	Block              string
	TargetURL          string
	PullRequestURL     string
	Detail             string
	ScreenshotAttached bool
}

// DeliverReleaseContent renders the final production report.
func DeliverReleaseContent(runID string, report DeliverReleaseReport) string {
	var builder strings.Builder
	switch report.Verdict {
	case "pass":
		builder.WriteString("【本番反映完了】Go を受けて本番に反映し、本番の画面を自動確認しました。結果: 合格です。\n\n")
	case "promotion_failed":
		builder.WriteString("【本番反映できず】Go を受けましたが、本番反映の準備が関所で止まりました。ステージングが確認時点から進んだ場合は、再確認からやり直す必要があります。\n\n")
	case "deploy_failed":
		builder.WriteString("【本番反映が未確認】本番ブランチへの反映は行われましたが、本番の自動デプロイの完了を確認できませんでした。\n\n")
	case "observe_failed":
		builder.WriteString("【本番反映済み・画面確認は不合格】本番への反映は完了しましたが、本番画面の自動確認が合格しませんでした。お手数ですが人の目での確認をお願いします。\n\n")
	case "observe_blocked":
		builder.WriteString("【本番反映済み・画面確認ができず】本番への反映は完了しましたが、確認用の画面を開けなかったため、本番画面の合否を判定できませんでした。お手数ですが人の目での確認をお願いします。\n\n")
	case "expired":
		builder.WriteString("【本番反映は行われませんでした】期限内に「Go」がなかったため、本番への反映は行わず終了しました。ステージングには反映済みのままです。\n\n")
	case "stopped":
		builder.WriteString("【本番反映は行いません】ご指示（停止）を受け付けました。本番への反映は行わず終了します。ステージングには反映済みのままです。\n\n")
	case "merge_unverified":
		builder.WriteString("【本番反映の成否が不明】昇格マージの結果を確認できませんでした。本番に反映された可能性があります。お手数ですが状態の確認をお願いします。\n\n")
	default:
		builder.WriteString("【本番反映の工程が停止】工程が結果を残さず終了しました（実行上限または実行環境の問題）。本番の状態は下の記載をご確認ください。\n\n")
	}
	if report.TargetURL != "" {
		fmt.Fprintf(&builder, "確認した画面: %s\n", report.TargetURL)
	}
	if report.PullRequestURL != "" {
		fmt.Fprintf(&builder, "反映内容 (昇格 PR): %s\n", report.PullRequestURL)
	}
	if report.Detail != "" {
		fmt.Fprintf(&builder, "補足: %s\n", report.Detail)
	}
	if report.ScreenshotAttached {
		builder.WriteString("確認時点の本番画面のスクリーンショットをこのコメントに添付しています。\n")
	}
	facts := CommentFacts{
		NextEvent: "以後の自動通知はありません",
		AutoRetry: "なし",
		Marker:    CommentMarker("rel-report", runID),
	}
	switch report.Verdict {
	case "pass":
		facts.State = "本番反映完了"
		facts.NextActor = "なし（完了報告です）"
		facts.Operation = "対応不要"
		facts.Production = "反映済み（本番の画面確認まで合格）"
	case "promotion_failed":
		facts.State = "本番反映は開始されず終了"
		facts.NextActor = "依頼者"
		facts.Operation = "必要なら再度の確認・反映を起票してください"
		facts.Production = "未変更"
	case "deploy_failed":
		facts.State = "本番反映は実行・完了確認は不能"
		facts.NextActor = "運用担当者"
		facts.Operation = "本番デプロイの状態を確認します"
		facts.Production = "反映操作済み（完了は未確認）"
	case "expired":
		facts.State = "期限切れで終了（本番反映なし）"
		facts.NextActor = "依頼者"
		facts.Operation = "本番反映が必要になったら再度起票してください"
		facts.Production = "未変更（ステージングには反映済み）"
	case "stopped":
		facts.State = "停止指示で終了（本番反映なし）"
		facts.NextActor = "なし（停止の確認報告です）"
		facts.Operation = "対応不要。本番反映が必要になったら再度起票してください"
		facts.Production = "未変更（ステージングには反映済み）"
	case "observe_failed":
		facts.State = "本番反映済み・画面は要確認"
		facts.NextActor = "依頼者"
		facts.Operation = "本番の画面をご確認ください"
		facts.Production = "反映済み（画面の機械確認は不合格）"
	case "observe_blocked":
		facts.State = "本番反映済み・画面確認は判定不能"
		facts.NextActor = "運用担当者"
		if report.Block == "sign_in" {
			facts.Operation = "確認用のログインをやり直し、本番の画面を確認したうえで、このチケットに「確認済み」とコメント"
		} else {
			facts.Operation = "本番の画面を人の目で確認し、このチケットに「確認済み」とコメント"
		}
		facts.Production = "反映済み（画面の機械確認は判定不能）"
	case "merge_unverified":
		facts.State = "本番反映の成否不明"
		facts.NextActor = "運用担当者"
		facts.Operation = "本番ブランチとデプロイの状態を確認します"
		facts.Production = "不明（手動確認が必要）"
	default:
		facts.State = "本番反映の工程が停止"
		facts.NextActor = "運用担当者"
		facts.Operation = "実行環境と本番の状態を確認します"
		facts.Production = "本文の記載どおり"
	}
	return builder.String() + facts.render()
}

// DeliverResolvedReport is an operator's confirmation of an attention
// state (a report whose verdict asked a person to look), recorded once.
type DeliverResolvedReport struct {
	Phase   string // staging | release
	Verdict string
}

// DeliverResolvedContent renders the acknowledgement of an operator's
// 「確認済み」. The automation's part of this delivery ends here, and what
// happens to production is said in so many words: the ticket must never
// read as if the confirmation itself changed anything.
func DeliverResolvedContent(runID string, report DeliverResolvedReport) string {
	var builder strings.Builder
	facts := CommentFacts{
		State:     "運用担当者が確認済み・自動処理終了",
		NextEvent: "以後の自動通知はありません",
		AutoRetry: "なし",
		Marker:    CommentMarker(string(RunCommentResolved), runID),
	}
	if report.Phase == "release" {
		builder.WriteString("【運用担当者の確認を記録】本番反映の状態は運用担当者が確認しました。この依頼の自動処理はここで終了します。\n")
		facts.NextActor = "なし（記録です）"
		facts.Operation = "対応不要"
		facts.Production = "確認済み（運用担当者による確認。詳細はこのコメントより前の報告を参照）"
	} else {
		builder.WriteString("【運用担当者の確認を記録】ステージング反映の状態は運用担当者が確認しました。本番への反映は自動では行わず、運用手順で行います。この依頼の自動処理はここで終了します。\n")
		facts.NextActor = "運用担当者"
		facts.Operation = "本番反映は運用手順で行います"
		facts.Production = "未変更（自動処理による本番反映は行いません）"
	}
	builder.WriteString(facts.render())
	return builder.String()
}
