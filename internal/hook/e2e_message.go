package hook

import (
	"fmt"
	"strings"
)

// E2EReport is what the debug role tells the requester after a human merged
// the delivery and the staging deployment finished: what the browser
// actually saw on the deployed page. Verdicts are exactly three — pass,
// fail, unknown — and unknown never dresses up as a pass.
type E2EReport struct {
	Verdict            string // "pass" | "fail" | "unknown"
	TargetURL          string
	ExpectedText       string
	ExpectedSeen       bool
	AbsentText         string
	AbsentGone         bool
	Detail             string
	ScreenshotAttached bool
}

// E2ECommentContent renders the requester-facing observation comment.
func E2ECommentContent(runID string, report E2EReport) string {
	var builder strings.Builder
	switch report.Verdict {
	case "pass":
		builder.WriteString("【マージ後確認】ステージングへの反映後、画面を自動確認しました。結果: 合格です。ご対応は不要です。\n\n")
	case "fail":
		builder.WriteString("【マージ後確認】ステージングへの反映後、画面を自動確認しましたが、期待した表示を確認できませんでした。お手数ですが人の目での確認をお願いします。\n\n")
	default:
		builder.WriteString("【マージ後確認】ステージング反映後の自動確認を実施できませんでした（判定不能）。合格を装わず、その旨のみお知らせします。\n\n")
	}
	if report.TargetURL != "" {
		fmt.Fprintf(&builder, "確認した画面: %s\n", report.TargetURL)
	}
	if report.ExpectedText != "" && report.Verdict != "unknown" {
		fmt.Fprintf(&builder, "出ているべき表示「%s」: %s\n", report.ExpectedText, verdictWord(report.ExpectedSeen))
	}
	if report.AbsentText != "" && report.Verdict != "unknown" {
		fmt.Fprintf(&builder, "消えているべき表示「%s」: %s\n", report.AbsentText, goneWord(report.AbsentGone))
	}
	if report.Detail != "" {
		fmt.Fprintf(&builder, "補足: %s\n", report.Detail)
	}
	if report.ScreenshotAttached {
		builder.WriteString("確認時点の画面全体のスクリーンショットをこのコメントに添付しています。\n")
	}
	facts := CommentFacts{
		NextEvent:  "以後の自動通知はありません",
		Production: "未変更（今回の反映はステージングまでです）",
		AutoRetry:  "なし（この確認は 1 回のみ実施します）",
		Marker:     CommentMarker("e2e", runID),
	}
	switch report.Verdict {
	case "pass":
		facts.State = "マージ後確認: 合格"
		facts.NextActor = "なし（参考情報です）"
		facts.Operation = "対応不要"
	case "fail":
		facts.State = "マージ後確認: 要確認"
		facts.NextActor = "依頼者"
		facts.Operation = "ステージングの画面をご確認のうえ、必要なら修正の依頼を起票してください"
	default:
		facts.State = "マージ後確認: 判定不能"
		facts.NextActor = "運用担当者"
		facts.Operation = "確認環境（ログイン用セッションの設定など）を整えたうえで、必要なら手動で確認します"
	}
	return builder.String() + facts.render()
}

func verdictWord(seen bool) string {
	if seen {
		return "確認できました"
	}
	return "確認できませんでした"
}

func goneWord(gone bool) string {
	if gone {
		return "表示されていないことを確認しました"
	}
	return "まだ表示されています"
}
