package hook

import (
	"context"
	"time"
)

// A terminal report is sealed in the ledger as a digest over the record
// that produced it, and a re-submission has to reproduce that digest
// exactly. Usually it can: the run wrote its closing comment down beside
// the record it was taken over (runner.TerminalCommentFile), and the words
// and the digest come back together.
//
// A run that ended under an engine from before that, and whose board cards
// and artifacts have since gone, can produce neither. Rebuilding the
// record from what is left gives a different digest, the store refuses it,
// and there is nothing else to send. Before this existed, the engine wrote
// a line in a log saying an operator was needed and waited — for ever, on
// a ticket whose requester was never told anything, while the run kept the
// project's one pending slot and nothing else could start.
//
// So the ending is reported from what the ledger itself holds. It is less
// than the run knew, and the comment says so rather than filling the gap
// with a guess: an ending that says "the details could not be recovered"
// is honest, and an ending that says "the pull request is waiting for you"
// on a run that deployed to production is not.

// TerminalRecoveryRequest asks for the ending of a run whose sealed report
// cannot be rebuilt to be reported from the ledger's own record of it.
//
// IssueID comes from the run row the caller read, not from the sender: the
// only caller is the engine's own tick, reading the same ledger the store
// writes. Reached is how far the delivery is recorded as having gone, or
// "" when nothing in the run directory says.
type TerminalRecoveryRequest struct {
	AutomationRunID string
	DeliveryID      string
	IssueID         int64
	Code            TerminalCode
	ReportSHA256    string
	Reached         string
}

// TerminalRecoveryStore is the part of the ledger that can close a pending
// terminal report whose record cannot be reproduced.
//
// It is asked for separately rather than added to TerminalReportStore
// because only the engine's own ledger has to answer it. The transition it
// performs is one no report can drive — it closes a row without the digest
// that row was begun with — so it is deliberately narrow, and a store that
// does not offer it simply cannot recover a run this way.
type TerminalRecoveryStore interface {
	CloseUnreproducibleTerminal(context.Context, TerminalRecoveryCloseRequest) (TerminalCompleteDisposition, error)
}

// TerminalRecoveryCloseRequest names the row to close and the comment that
// now stands for its report.
type TerminalRecoveryCloseRequest struct {
	Route        ReportRouteConfig
	RunID        string
	Code         TerminalCode
	ReportSHA256 string
	CommentID    int64
	CompletedAt  time.Time
}

func validTerminalRecovery(request TerminalRecoveryRequest) bool {
	return ValidRunID(request.AutomationRunID) && request.IssueID > 0 &&
		request.Code.Valid() && digestPattern.MatchString(request.ReportSHA256) &&
		(request.Reached == "" || validDelivery(request.Reached))
}

// ProcessTerminalRecovery posts the ending of a run whose sealed report
// cannot be rebuilt, and records on the row that it was posted.
//
// The comment carries the marker the lost report would have carried, which
// is what makes this safe to run on every tick and on a run that may
// already have been reported: the tracker is asked for that marker first,
// so a comment posted by the attempt that died is adopted rather than
// duplicated, and the row is closed against whichever comment is there.
func (s *TerminalReportService) ProcessTerminalRecovery(ctx context.Context, request TerminalRecoveryRequest) Result {
	if !validTerminalRecovery(request) {
		return s.reportResult(DecisionInvalid, "terminal_recovery_invalid", request.DeliveryID)
	}
	recovery, ok := s.store.(TerminalRecoveryStore)
	if !ok {
		return s.reportResult(DecisionInternal, "terminal_recovery_unsupported", request.DeliveryID)
	}
	marker := CommentMarker("terminal", request.AutomationRunID, string(request.Code), request.ReportSHA256)
	if !recoveredMarkerIsTerminal(marker) {
		return s.reportResult(DecisionInvalid, "terminal_recovery_invalid", request.DeliveryID)
	}
	commentID, found, err := s.backlog.FindCommentWithMarker(ctx, request.IssueID, marker)
	if err != nil {
		return s.backlogFailure("terminal_recovery_lookup", err, request.DeliveryID)
	}
	if !found {
		commentID, err = s.backlog.AddComment(ctx, request.IssueID, terminalRecoveredCommentContent(request, marker))
		if err != nil {
			return s.backlogFailure("terminal_recovery_add", err, request.DeliveryID)
		}
	}
	if commentID <= 0 {
		return s.reportResult(DecisionInternal, "terminal_comment_id_invalid", request.DeliveryID)
	}
	route := s.config
	route.ExpectedRunID = request.AutomationRunID
	disposition, err := recovery.CloseUnreproducibleTerminal(ctx, TerminalRecoveryCloseRequest{
		Route: route, RunID: request.AutomationRunID, Code: request.Code,
		ReportSHA256: request.ReportSHA256, CommentID: commentID, CompletedAt: s.now().UTC(),
	})
	if err != nil {
		return s.storeFailure("terminal_recovery_close", err, request.DeliveryID)
	}
	switch disposition {
	case TerminalCompleted, TerminalAlreadyComplete:
		projectBoard(ctx, s.board, s.logger, request.IssueID, BoardNeedsAttention)
		return s.reportResult(DecisionAccepted, "terminal_recovery_recorded", request.DeliveryID)
	case TerminalCompleteConflict:
		return s.reportResult(DecisionInvalid, "terminal_recovery_conflict", request.DeliveryID)
	default:
		return s.reportResult(DecisionInternal, "terminal_report_state_invalid", request.DeliveryID)
	}
}

// TerminalRecoveredCommentContent is the comment a run leaves when its own
// closing words could not be recovered. Exported for the same reason every
// other comment builder here is: the wording is the product.
func TerminalRecoveredCommentContent(request TerminalRecoveryRequest) string {
	return terminalRecoveredCommentContent(request,
		CommentMarker("terminal", request.AutomationRunID, string(request.Code), request.ReportSHA256))
}

// terminalRecoveredCommentContent says three things and no more: how the
// run ended, how far it is recorded as having got, and that the account of
// it could not be recovered. Anything further would be invented — the
// places to look, the cost, the record of the rounds all lived in the
// report this comment exists because nothing can rebuild.
func terminalRecoveredCommentContent(request TerminalRecoveryRequest, marker string) string {
	body := "自動処理の最終結果: " + string(request.Code) + "\n" +
		"この依頼の自動処理は終了しています。終了時にお送りするはずだった詳しい報告は、" +
		"この依頼の記録がすでに残っていないため復元できませんでした。ここでは記録に残っていた範囲だけをお伝えします。"
	production := "不明（この依頼の記録からは確認できません）"
	if landing := recoveredLandingText(request.Reached); landing != "" {
		body += "\n\nどこまで進んだか: " + landing
		production = recoveredProductionText(request.Reached)
	}
	facts := CommentFacts{
		State:     "自動処理終了（" + string(request.Code) + "）",
		NextActor: "運用担当者",
		Operation: "起票者の操作は不要です（運用担当者がこの依頼の記録を確認し、必要ならこのチケットでお知らせします）",
		NextEvent: "以後の自動通知はありません",
		// Saying "未変更" here would be a claim about production made from
		// a record that does not hold one. Where the delivery landed is
		// known only when the run directory still says so.
		Production: production,
		AutoRetry:  "なし（自動での再実行・再起票は行いません）",
		Marker:     marker,
	}
	return fitCommentWithin(body, facts.render())
}

func recoveredLandingText(reached string) string {
	switch reached {
	case DeliverProduction:
		return "本番環境への反映と、利用者目線の表示確認まで終わっています。"
	case DeliverIntegration:
		return "staging への反映と表示確認まで終わっています。本番環境は変更していません。"
	case DeliverPullRequest:
		return "取り込み用の Pull Request の作成まで終わっています。本番環境は変更していません。"
	}
	return ""
}

func recoveredProductionText(reached string) string {
	switch reached {
	case DeliverProduction:
		return "確認済み（利用者目線の表示確認まで完了）"
	case DeliverIntegration:
		return "未変更（staging まで反映済み）"
	case DeliverPullRequest:
		return "未変更"
	}
	return "不明（この依頼の記録からは確認できません）"
}

// recoveredMarkerIsTerminal keeps the recovery comment inside the terminal
// marker's own grammar, so a ticket already carrying a terminal comment for
// this run and digest is recognised by every reader of those markers rather
// than only by this one.
func recoveredMarkerIsTerminal(marker string) bool {
	return TerminalCodeFromMarker(marker) != ""
}
