package hook

import (
	"fmt"
	"strings"
)

// InvestigationFacts is what the requester is shown of a sealed
// investigation report: the findings with their standing (measured or
// inferred), what stayed unknown, and the next step. The raw measurements
// travel as attachments; the comment never carries their bodies.
type InvestigationFacts struct {
	Round             int
	Questions         []string
	Findings          []InvestigationFindingFact
	Unknowns          []string
	Next              string
	MeasurementsCount int
	// AttachedCount is how many files travel with the comment (the
	// measurements file counts); AttachmentsOmitted counts raw outputs that
	// did not fit the attachment budget and stayed in the run directory.
	AttachedCount      int
	AttachmentsOmitted int
	// EndsHere says the request asked for the investigation only.
	EndsHere bool
}

// InvestigationFindingFact is one finding as the requester reads it.
type InvestigationFindingFact struct {
	Claim    string
	Measured bool
	Evidence []string
}

const investigationBodyMaxBytes = 16 * 1024

// InvestigationCommentContent renders the investigation report comment.
func InvestigationCommentContent(runID string, facts InvestigationFacts) string {
	var builder strings.Builder
	builder.WriteString("【調査報告】稼働環境とリポジトリを読み取りだけで計り、分かったことを報告します。")
	if facts.EndsHere {
		builder.WriteString("この依頼は調査のみのため、ここで完了です（コードの変更や Pull Request はありません）。\n")
	} else {
		builder.WriteString("この報告を元に設計書を作り、レビューを通してから実装に進みます。\n")
	}
	if facts.Round > 1 {
		fmt.Fprintf(&builder, "\n（%d 巡目の報告です。前の巡の指摘を受けて計り直しました）\n", facts.Round)
	}
	writePlanList(&builder, "確かめようとしたこと", facts.Questions)
	if len(facts.Findings) > 0 {
		builder.WriteString("\n分かったこと:\n")
		for index, finding := range facts.Findings {
			if index >= planListMaxItems {
				fmt.Fprintf(&builder, "- …他 %d 件\n", len(facts.Findings)-index)
				break
			}
			standing := "推測"
			if finding.Measured {
				standing = "実測 " + strings.Join(finding.Evidence, ", ")
			}
			fmt.Fprintf(&builder, "- %s（%s）\n", truncatePlanRunes(strings.TrimSpace(finding.Claim), planItemMaxRunes), standing)
		}
	}
	writePlanList(&builder, "分からなかったこと", facts.Unknowns)
	if next := truncatePlanText(facts.Next); next != "" {
		builder.WriteString("\n次の一手: " + next + "\n")
	}
	fmt.Fprintf(&builder, "\n実測は %d 件です。", facts.MeasurementsCount)
	switch {
	case facts.AttachedCount > 0:
		fmt.Fprintf(&builder, "生の出力は添付ファイル %d 件（measurements.jsonl と measurement-<番号>.txt）で確認できます。", facts.AttachedCount)
	default:
		builder.WriteString("生の出力は添付できなかったため、運用担当者が実行記録から取り出します。")
	}
	if facts.AttachmentsOmitted > 0 {
		fmt.Fprintf(&builder, "添付の上限を超えた %d 件は省略しました（運用担当者は実行記録から取り出せます）。", facts.AttachmentsOmitted)
	}
	builder.WriteString("\n")
	state, nextActor, nextEvent := "調査報告を掲示・設計へ進行中", "自動処理", "設計書の要約を、この報告のあとに通知"
	if facts.EndsHere {
		state, nextActor, nextEvent = "調査のみの依頼として完了", "なし", "なし（このチケットでの自動処理は終了）"
	}
	return capBody(builder.String(), investigationBodyMaxBytes) + CommentFacts{
		State:      state,
		NextActor:  nextActor,
		Operation:  "不要",
		NextEvent:  nextEvent,
		Production: "未変更",
		AutoRetry:  "なし",
		Marker:     CommentMarker(string(RunCommentInvestigation), runID),
	}.render()
}

// DesignFacts is the approved design as the requester reads it before any
// code is written.
type DesignFacts struct {
	Round        int
	Cause        string
	Approach     string
	Files        []string
	Verification string
	BlastRadius  []string
	NotDoing     []string
}

// DesignCommentContent renders the approved design's summary.
func DesignCommentContent(runID string, facts DesignFacts) string {
	var builder strings.Builder
	builder.WriteString("【設計】調査の結果から次の直し方を決め、独立したレビューを通しました。この設計書どおりに実装に進みます。ご対応は不要です（方針が違う場合のみ、下の停止方法をご利用ください）。\n")
	if facts.Round > 1 {
		fmt.Fprintf(&builder, "\n（設計 %d 巡目です。前の設計はレビューまたは実装役の指摘で差し戻され、この設計に置き換わりました）\n", facts.Round)
	}
	if cause := truncatePlanText(facts.Cause); cause != "" {
		builder.WriteString("\n原因: " + cause + "\n")
	}
	if approach := truncatePlanText(facts.Approach); approach != "" {
		builder.WriteString("\n直し方: " + approach + "\n")
	}
	writePlanList(&builder, "変更するファイル（これ以外は触りません）", facts.Files)
	if verification := truncatePlanText(facts.Verification); verification != "" {
		builder.WriteString("\n反映後の確認: " + verification + "\n")
	}
	writePlanList(&builder, "影響する範囲", facts.BlastRadius)
	writePlanList(&builder, "やらないこと", facts.NotDoing)
	builder.WriteString(planStopSentence)
	return capBody(builder.String(), planBodyMaxBytes) + CommentFacts{
		State:      "設計書を掲示・実装へ進行中",
		NextActor:  "自動処理（方針を変えたい場合のみ依頼者）",
		Operation:  "方針が違う場合のみ「停止」とコメント",
		NextEvent:  "最終結果または確認事項を通知",
		Production: "未変更",
		AutoRetry:  "なし",
		Marker:     CommentMarker(string(RunCommentDesign), runID),
	}.render()
}

func capBody(body string, limit int) string {
	if len(body) <= limit {
		return body
	}
	runes := []rune(body)
	for len(runes) > 0 && len(string(runes)) > limit {
		runes = runes[:len(runes)*limit/len(string(runes))]
	}
	return string(runes) + "\n…（長いため以下略）\n"
}
