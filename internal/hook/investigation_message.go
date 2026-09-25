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
	// ReportAttached says the whole report travels as an attachment beside
	// this comment because it did not fit inside one. The comment then names
	// that file for the part it could not show, instead of ending in an
	// ellipsis that reads as "there was no more".
	ReportAttached bool
}

// InvestigationFindingFact is one finding as the requester reads it.
type InvestigationFindingFact struct {
	Claim    string
	Measured bool
	Evidence []string
}

// InvestigationReportFilename is the file that carries the whole report when
// one ticket comment cannot. The measurements this report cites already
// travel to the requester as attachments, so its overflow takes the same
// road rather than a new one.
const InvestigationReportFilename = "investigation-report.txt"

// investigationOverflowReserveBytes is what the renderer holds back from the
// comment's budget for the sentence naming where the rest of the report is.
// It is well over the longest such sentence on purpose: underestimating it
// would push the comment past the tracker's limit, and a comment the tracker
// refuses is a report nobody reads at all.
const investigationOverflowReserveBytes = 512

// maxTrackerCommentAttachments is how many files the tracker binds to one
// comment. It is used here only to size the sentence that counts them; the
// caller owns how the budget is spent.
const maxTrackerCommentAttachments = 10

// InvestigationCommentContent renders the investigation report comment.
func InvestigationCommentContent(runID string, facts InvestigationFacts) string {
	comment, _ := renderInvestigationComment(runID, facts)
	return comment
}

// InvestigationReportOverflow is the whole report as the body of a file to
// attach beside the comment, or nil when one comment holds all of it. A
// caller that uploads it reserves an attachment slot for it and sets
// ReportAttached, so the comment names the file for what it could not show.
//
// Findings used to stop at a dozen items with each line clipped, and the
// rest became "他 N 件" — on a live investigation (2026-09-25) twenty
// findings reached the ticket as ten, several of them cut, and the whole
// text existed only inside the run directory, where the requester cannot
// look.
func InvestigationReportOverflow(runID string, facts InvestigationFacts) []byte {
	if _, fits := renderInvestigationComment(runID, facts); fits {
		return nil
	}
	var whole strings.Builder
	writeInvestigationReport(&whole, facts, 0)
	return []byte(whole.String())
}

// renderInvestigationComment builds the comment and says whether the whole
// report fitted inside it. The report gives way to the tracker's comment
// limit; the heading, the measurement sentence and the footer never do — the
// footer's final line is the marker the exactly-once machinery anchors on.
func renderInvestigationComment(runID string, facts InvestigationFacts) (string, bool) {
	head := investigationHead(facts)
	tail := investigationTail(facts)
	footer := investigationFooter(runID, facts)
	var whole strings.Builder
	writeInvestigationReport(&whole, facts, 0)
	// The budget is taken against the longest the measurement sentence can
	// grow to, not the one this render happens to carry. Whether the report
	// fits is asked before the attachments exist and answered again after
	// they do, and the two answers have to agree: otherwise a file is
	// attached that the comment never mentions, or the comment cuts the
	// report with nothing behind it.
	room := MaxTrackerCommentBytes - len(head) - len(investigationLongestTail(facts)) - len(footer)
	if whole.Len() <= room {
		return head + whole.String() + tail + footer, true
	}
	var shortened strings.Builder
	findings, unknowns := writeInvestigationReport(&shortened, facts, room-investigationOverflowReserveBytes)
	comment := head + shortened.String() + investigationOverflowLine(facts, findings, unknowns) + tail + footer
	if len(comment) > MaxTrackerCommentBytes {
		// Nothing of the report fits beside the rest of the comment. Say that
		// and keep the pointer, rather than letting the tracker refuse the
		// whole comment.
		comment = head + investigationOverflowLine(facts, len(facts.Findings), len(facts.Unknowns)) + tail + footer
	}
	return comment, false
}

func investigationHead(facts InvestigationFacts) string {
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
	return builder.String()
}

// writeInvestigationReport writes what the requester reads as the report:
// what was asked, what was found, what stayed unknown and what comes next.
// A budget of zero writes all of it. A positive budget writes whole items
// until the next one would not fit, and returns how many findings and
// unknowns were left for the attachment to carry — an item is written whole
// or not at all, so no line ends mid-sentence.
func writeInvestigationReport(builder *strings.Builder, facts InvestigationFacts, budget int) (int, int) {
	writePlanList(builder, "確かめようとしたこと", facts.Questions)
	findings := make([]string, 0, len(facts.Findings))
	for _, finding := range facts.Findings {
		standing := "推測"
		if finding.Measured {
			standing = "実測 " + strings.Join(finding.Evidence, ", ")
		}
		findings = append(findings, fmt.Sprintf("%s（%s）", strings.TrimSpace(finding.Claim), standing))
	}
	droppedFindings := writeReportList(builder, "分かったこと", findings, budget)
	droppedUnknowns := writeReportList(builder, "分からなかったこと", facts.Unknowns, budget)
	if next := truncatePlanText(facts.Next); next != "" {
		builder.WriteString("\n次の一手: " + next + "\n")
	}
	return droppedFindings, droppedUnknowns
}

// writeReportList writes every item whole, heading included only once there
// is something under it. It returns how many trailing items the budget left
// out; a budget of zero leaves out none.
func writeReportList(builder *strings.Builder, heading string, items []string, budget int) int {
	written := 0
	for index, item := range items {
		entry := "- " + strings.TrimSpace(item) + "\n"
		if strings.TrimSpace(item) == "" {
			continue
		}
		opening := ""
		if written == 0 {
			opening = "\n" + heading + ":\n"
		}
		if budget > 0 && builder.Len()+len(opening)+len(entry) > budget {
			return len(items) - index
		}
		builder.WriteString(opening + entry)
		written++
	}
	return 0
}

// investigationOverflowLine says what the comment could not show and where
// it is. Nothing is dropped in silence: either the whole report travels as
// an attachment beside this comment, or it waits in the run record.
func investigationOverflowLine(facts InvestigationFacts, findings, unknowns int) string {
	where := "運用担当者が実行記録から取り出します"
	if facts.ReportAttached {
		where = "添付ファイル " + InvestigationReportFilename + " に全文があります"
	}
	return fmt.Sprintf("\nこの報告はコメントに収まらないため、分かったこと %d 件・分からなかったこと %d 件をここでは省いています（%s）。\n",
		findings, unknowns, where)
}

// investigationLongestTail is the measurement sentence at the largest it can
// become once the attachments are uploaded: every slot the tracker binds
// filled, and every measurement left over.
func investigationLongestTail(facts InvestigationFacts) string {
	crowded := facts
	crowded.AttachedCount, crowded.AttachmentsOmitted = maxTrackerCommentAttachments, facts.MeasurementsCount
	return investigationTail(crowded)
}

func investigationTail(facts InvestigationFacts) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "\n実測は %d 件です。", facts.MeasurementsCount)
	switch {
	case facts.AttachedCount > 0:
		fmt.Fprintf(&builder, "添付ファイル %d 件で確認できます（measurements-index.jsonl = 実測の索引、measurement-<番号>.txt = 引用した実測の生の出力）。", facts.AttachedCount)
	default:
		builder.WriteString("生の出力は添付できなかったため、運用担当者が実行記録から取り出します。")
	}
	if facts.AttachmentsOmitted > 0 {
		fmt.Fprintf(&builder, "添付の上限を超えた %d 件は省略しました（運用担当者は実行記録から取り出せます）。", facts.AttachmentsOmitted)
	}
	builder.WriteString("\n")
	return builder.String()
}

func investigationFooter(runID string, facts InvestigationFacts) string {
	state, nextActor, nextEvent := "調査報告を掲示・設計へ進行中", "自動処理", "設計書の要約を、この報告のあとに通知"
	if facts.EndsHere {
		state, nextActor, nextEvent = "調査のみの依頼として完了", "なし", "なし（このチケットでの自動処理は終了）"
	}
	return CommentFacts{
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
