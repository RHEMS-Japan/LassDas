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
	// NeedsDesign and DesignReason are the reception's sealed design
	// decision (readiness decision.json: needs_design, design_reason). An
	// empty reason means the run's decision did not say - a decision sealed
	// before the design stage existed - and the line is left out rather
	// than guessed.
	NeedsDesign  bool
	DesignReason string
	// RequestKind is the reception's sealed request_kind (change |
	// investigation); empty for a decision sealed before the field existed.
	RequestKind string
}

// designReasonPhrases are the requester-facing sentences for the machine
// codes the reception seals as design_reason (internal/worker's
// DesignReason* constants; a worker test pins that every code has a phrase
// here). The codes are strings on purpose - this package cannot import the
// worker, which imports it.
var designReasonPhrases = map[string]string{
	"approach_in_ticket":     "方針が本文にあるため設計を省略",
	"investigation":          "調査の依頼のため設計は行わない",
	"design_default_off":     "この納品先の設定で設計工程を使わないため設計を省略",
	"approach_not_in_ticket": "本文に「どう直すか」が書かれていないため",
	"target_files_over_two":  "触る予定のファイルが 3 つ以上のため",
	"trigger_words_unset":    "設計を省略できる語彙が納品先に設定されていないため（安全側）",
	"trigger_word":           "本文に稼働環境の観測を示す語があるため",
	"proposer":               "受付の起案役が設計の省略に同意しなかったため",
	"checker_disagreed":      "受付の確認役が設計の省略に同意しなかったため（起案役と不一致）",
}

// designReasonUnknownPhrase is what the ticket says for a reason code this
// package has no sentence for: the decision still shows, the reason stays
// in the sealed record. A machine code is never put in front of a
// requester.
const designReasonUnknownPhrase = "理由は自動処理の記録に残しています"

// DesignReasonPhrase is the requester-facing sentence for one design_reason
// code, and whether the code is one this package knows.
func DesignReasonPhrase(reason string) (string, bool) {
	phrase, known := designReasonPhrases[reason]
	return phrase, known
}

// DesignDecisionLine renders the reception's design decision as the one line
// the plan notice carries: what was decided and why, in the requester's
// terms. It reports the sealed judgment; what the chain does with it is the
// runtime's business. An unknown code still renders the decision with a
// neutral sentence, so a newer engine's reason is neither dropped nor shown
// as a code.
func DesignDecisionLine(needsDesign bool, reason string) string {
	verdict := "設計なし"
	if needsDesign {
		verdict = "設計あり"
	}
	phrase, known := DesignReasonPhrase(reason)
	if !known {
		phrase = designReasonUnknownPhrase
	}
	return verdict + ": " + phrase
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
// planStopSentence is the one true description of how a requester stops a
// run and when the stop takes effect. Every comment that offers a stop
// reuses it, so no comment promises a gate the chain does not have.
const planStopSentence = "\n方針を止めたい場合: このチケットに「停止」とだけ書いたコメントを投稿してください。指摘によるやり直し（次のラウンド）が始まる前までに反映され、以後の新しい工程は開始されません。実行中の工程は最後まで走り切り、指摘なしで最後まで進んだ場合は Pull Request をマージしないことで反映を止められます。確認の質問が出ている間は、質問コメントに記載の中止方法（「中止 C番号」）に従ってください。\n"

func PlanCommentContent(runID string, facts PlanFacts) string {
	var builder strings.Builder
	builder.WriteString(planHeadline(facts))
	if request := truncatePlanText(facts.Request); request != "" {
		builder.WriteString("\n依頼の解釈: " + request + "\n")
	}
	if rationale := truncatePlanText(facts.Rationale); rationale != "" {
		builder.WriteString("\n方針: " + rationale + "\n")
	}
	if reason := strings.TrimSpace(facts.DesignReason); reason != "" {
		builder.WriteString("\n" + truncatePlanRunes(DesignDecisionLine(facts.NeedsDesign, reason), planItemMaxRunes) + "\n")
	}
	writePlanList(&builder, "触る予定の範囲", facts.TargetFiles)
	writePlanList(&builder, "前提とした解釈（曖昧だった点はこう進めます）", facts.Assumptions)
	builder.WriteString(planStopSentence)
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

// planHeadline says what starts now, in the requester's terms: an
// investigation only, an investigation and a design before any code, or the
// implementation straight away.
func planHeadline(facts PlanFacts) string {
	switch {
	case facts.RequestKind == "investigation":
		return "【調査方針】受付審査を通過したため、次の内容を調査します。稼働環境とリポジトリは読み取りだけで、コードの変更と Pull Request はありません。調査の結果はこのチケットに報告します。ご対応は不要です（止めたい場合のみ、下の停止方法をご利用ください）。\n"
	case facts.NeedsDesign:
		return "【実装方針】受付審査を通過したため、まず稼働環境を計って原因と直し方を設計書にまとめ、独立したレビューを通してから実装します。設計書の要約はコードを書く前にこのチケットに掲示します。ご対応は不要です（方針が違う場合のみ、下の停止方法をご利用ください）。\n"
	default:
		return "【実装方針】受付審査を通過したため、次の方針で実装を開始します。ご対応は不要です（方針が違う場合のみ、下の停止方法をご利用ください）。\n"
	}
}
