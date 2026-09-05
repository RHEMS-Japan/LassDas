package hook

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"
	"time"
)

type TerminalBeginRequest struct {
	Report       TerminalReportRequest
	ReportJSON   string
	ReportSHA256 string
	Route        ReportRouteConfig
	StartedAt    time.Time
	LeaseUntil   time.Time
	LeaseToken   string
}

type TerminalBinding struct {
	IssueID  int64
	IssueKey string
}

type TerminalBeginDisposition string

const (
	TerminalBeginAcquired TerminalBeginDisposition = "acquired"
	TerminalBeginBusy     TerminalBeginDisposition = "busy"
	TerminalBeginComplete TerminalBeginDisposition = "complete"
	TerminalBeginConflict TerminalBeginDisposition = "conflict"
)

type TerminalCompleteRequest struct {
	Report       TerminalReportRequest
	ReportJSON   string
	ReportSHA256 string
	Route        ReportRouteConfig
	LeaseToken   string
	CommentID    int64
	CompletedAt  time.Time
}

type TerminalCompleteDisposition string

const (
	TerminalCompleted        TerminalCompleteDisposition = "completed"
	TerminalAlreadyComplete  TerminalCompleteDisposition = "already_complete"
	TerminalCompleteConflict TerminalCompleteDisposition = "conflict"
)

type TerminalReportStore interface {
	BeginTerminal(context.Context, TerminalBeginRequest) (TerminalBinding, TerminalBeginDisposition, error)
	CompleteTerminal(context.Context, TerminalCompleteRequest) (TerminalCompleteDisposition, error)
}

type TerminalCommentClient interface {
	FindExactComment(context.Context, int64, string) (int64, bool, error)
	// FindCommentWithMarker finds a posted comment by the machine tag it
	// ends with, so a re-submitted report whose body drifted (the spend
	// line is read live) is recognised instead of posted twice.
	FindCommentWithMarker(context.Context, int64, string) (int64, bool, error)
	AddComment(context.Context, int64, string) (int64, error)
}

type TerminalReportProcessor interface {
	ProcessTerminalReport(context.Context, TerminalReportRequest) Result
}

type TerminalReportService struct {
	config  ReportRouteConfig
	store   TerminalReportStore
	backlog TerminalCommentClient
	logger  *slog.Logger
	board   BoardProjector
	now     func() time.Time
	token   func() (string, error)
}

// UseBoard mirrors endings onto the board humans watch: a delivery in its own
// column, everything else where a person picks it up.
func (s *TerminalReportService) UseBoard(board BoardProjector) { s.board = board }

func terminalBoardPhase(code TerminalCode) BoardPhase {
	if code == TerminalSuccess || code == TerminalInvestigated {
		return BoardDelivered
	}
	return BoardNeedsAttention
}

func NewTerminalReportService(config ReportRouteConfig, store TerminalReportStore, backlog TerminalCommentClient, logger *slog.Logger) (*TerminalReportService, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if store == nil || backlog == nil || logger == nil {
		return nil, errors.New("terminal report dependencies must not be nil")
	}
	config.HMACKey = append([]byte(nil), config.HMACKey...)
	return &TerminalReportService{
		config: config, store: store, backlog: backlog, logger: logger,
		now: time.Now, token: randomLeaseToken,
	}, nil
}

func (s *TerminalReportService) ProcessTerminalReport(ctx context.Context, report TerminalReportRequest) Result {
	// The run is named by the ticket, so the route works on the run this report
	// is about rather than on one value configured for the deployment. The
	// store still refuses a report whose sealed envelope, delivery and claim
	// owner do not match that run, so naming another one gains nothing.
	route := s.config
	route.ExpectedRunID = report.AutomationRunID
	if err := report.ValidateRoute(route); err != nil {
		return s.reportResult(DecisionInvalid, "terminal_report_invalid", report.DeliveryID)
	}
	now := s.now().UTC()
	if report.IssuedAt.Before(now.Add(-s.config.ClockSkew)) || report.IssuedAt.After(now.Add(s.config.ClockSkew)) {
		return s.reportResult(DecisionInvalid, "terminal_report_timestamp_invalid", report.DeliveryID)
	}
	record, err := MarshalTerminalReportRecord(report)
	if err != nil {
		return s.reportResult(DecisionInvalid, "terminal_report_invalid", report.DeliveryID)
	}
	reportDigest := TerminalReportDigest(record)
	leaseToken, err := s.token()
	if err != nil {
		return s.reportResult(DecisionInternal, "terminal_report_token_failed", report.DeliveryID)
	}
	binding, disposition, err := s.store.BeginTerminal(ctx, TerminalBeginRequest{
		Report: report, ReportJSON: string(record), ReportSHA256: reportDigest, Route: route,
		StartedAt: now, LeaseUntil: now.Add(s.config.LeaseDuration), LeaseToken: leaseToken,
	})
	if err != nil {
		return s.storeFailure("terminal_report_begin", err, report.DeliveryID)
	}
	switch disposition {
	case TerminalBeginBusy:
		return s.reportResult(DecisionRetryRequested, "terminal_report_pending", report.DeliveryID)
	case TerminalBeginComplete:
		return s.reportResult(DecisionAccepted, "terminal_report_already_recorded", report.DeliveryID)
	case TerminalBeginConflict:
		return s.reportResult(DecisionInvalid, "terminal_report_conflict", report.DeliveryID)
	case TerminalBeginAcquired:
	default:
		return s.reportResult(DecisionInternal, "terminal_report_state_invalid", report.DeliveryID)
	}

	comment := fixedTerminalComment(report, reportDigest)
	// Backlog's comment API has no idempotency key. The lease serializes live
	// writers, and this lookup repairs the ambiguous case where a previous
	// POST succeeded but the terminal store update did not: the posted
	// comment is found by the marker on its final line (run, code and
	// digest — the same report even when the trail or the live spend line
	// drifted). Every rendered terminal comment ends with that marker, so an
	// exact-content match could never succeed where the marker missed.
	commentID, found, err := s.backlog.FindCommentWithMarker(ctx, binding.IssueID, terminalCommentFacts(report, reportDigest).Marker)
	if err != nil {
		return s.backlogFailure("terminal_comment_lookup", err, report.DeliveryID)
	}
	if !found {
		commentID, err = s.backlog.AddComment(ctx, binding.IssueID, comment)
		if err != nil {
			return s.backlogFailure("terminal_comment_add", err, report.DeliveryID)
		}
	}
	if commentID <= 0 {
		return s.reportResult(DecisionInternal, "terminal_comment_id_invalid", report.DeliveryID)
	}
	complete, err := s.store.CompleteTerminal(ctx, TerminalCompleteRequest{
		Report: report, ReportJSON: string(record), ReportSHA256: reportDigest, Route: route,
		LeaseToken: leaseToken, CommentID: commentID, CompletedAt: s.now().UTC(),
	})
	if err != nil {
		return s.storeFailure("terminal_report_complete", err, report.DeliveryID)
	}
	switch complete {
	case TerminalCompleted, TerminalAlreadyComplete:
		projectBoard(ctx, s.board, s.logger, binding.IssueID, terminalBoardPhase(report.Code))
		return s.reportResult(DecisionAccepted, "terminal_report_recorded", report.DeliveryID)
	case TerminalCompleteConflict:
		return s.reportResult(DecisionInvalid, "terminal_report_conflict", report.DeliveryID)
	default:
		return s.reportResult(DecisionInternal, "terminal_report_state_invalid", report.DeliveryID)
	}
}

// OverrideClock replaces the wall clock and lease-token source so tests can
// drive the schedule deterministically. Production wiring never calls this.
func (s *TerminalReportService) OverrideClock(now func() time.Time, token func() (string, error)) {
	s.now, s.token = now, token
}

func (s *TerminalReportService) backlogFailure(operation string, err error, deliveryID string) Result {
	class, _ := FailureDetails(err)
	if class == FailureRejected {
		return s.reportResult(DecisionDependencyFailed, operation+"_rejected", deliveryID)
	}
	return s.reportResult(DecisionRetryRequested, operation+"_failed", deliveryID)
}

func (s *TerminalReportService) storeFailure(operation string, err error, deliveryID string) Result {
	class, _ := FailureDetails(err)
	if class == FailureRejected {
		return s.reportResult(DecisionInvalid, operation+"_rejected", deliveryID)
	}
	return s.reportResult(DecisionRetryRequested, operation+"_failed", deliveryID)
}

func (s *TerminalReportService) reportResult(decision Decision, code, deliveryID string) Result {
	s.logger.Info("terminal report decision", "decision", decision, "code", code, "delivery_id", deliveryID)
	return Result{Decision: decision, Code: code, DeliveryID: deliveryID}
}

// successMessage states what the run's stopping point actually was. The
// evidence shape is the truth here: a proposal-only delivery carries the pull
// request alone and has touched no environment, and telling its requester
// that production was verified would be a false completion report.
func successMessage(report TerminalReportRequest) string {
	if report.ProductionEvidenceURL != "" {
		return "自動処理が完了し、本番環境への反映と確認が完了しました。"
	}
	if report.StagingEvidenceURL != "" {
		return "自動処理が完了し、staging への反映と確認まで完了しました。本番への反映は人が行います。"
	}
	return "自動処理が完了し、取り込み用の Pull Request の作成まで完了しました。マージと以後の反映は人が行います。本番環境は変更していません。"
}

func fixedTerminalComment(report TerminalReportRequest, reportDigest string) string {
	message := map[TerminalCode]string{
		TerminalSuccess:                        successMessage(report),
		TerminalInputRejected:                  "入力が許可された形式または範囲に一致しなかったため、変更していません。",
		TerminalReadinessRejected:              "チケットの内容が自動処理の受付条件を満たさなかったため、対象リポジトリと本番環境は変更していません。詳細は運用担当者が確認し、このチケットのコメントでお知らせします。",
		TerminalClarificationRequired:          "実装に着手する前に、依頼者にしか決められない確認事項が見つかったため、対象リポジトリと本番環境は変更せず停止しました。確認事項は運用担当者が確認し、必要に応じてこのチケットのコメントでお知らせします。同じチケットの再投入は不要です。",
		TerminalReadinessUnresolved:            "着手可否の自動判定が規定回数内に確定しなかったため、対象リポジトリと本番環境は変更せず停止しました。運用担当者が内容を確認します。同じチケットの再投入は不要です。",
		TerminalClarificationExpired:           "確認事項への回答が期限までに得られなかったため、対象リポジトリと本番環境は変更せず停止しました。このチケットでの自動処理は終了しています。再度依頼する場合は、確認事項への回答内容を反映した新しいチケットとして起票してください。",
		TerminalCancelled:                      "起票者による中止の指示を確認したため、対象リポジトリと本番環境は変更せず停止しました。このチケットでの自動処理は終了しています。",
		TerminalModelFailed:                    "AIによる成果物の生成またはレビューを完了できなかったため、本番環境には反映していません。",
		TerminalNonconverged:                   "自動レビューが最大回数内に収束しなかったため、本番環境には反映していません。",
		TerminalValidationFailed:               "生成した変更が検証を通過しなかったため、本番環境には反映していません。",
		TerminalReleaseFailed:                  "既存のリリース経路で処理を完了できなかったため、本番環境への反映は完了していません。",
		TerminalProductionDeploymentUnverified: "prodブランチへの反映は完了しましたが、既存の本番デプロイが完了したことを確認できませんでした。自動的な追加変更やロールバックは行っていません。",
		TerminalProductionVerificationFailed:   "本番デプロイは完了しましたが、利用者目線の表示確認に失敗しました。自動的な追加変更やロールバックは行っていません。",
		TerminalInternalFailed:                 "自動処理中の内部エラーにより、本番環境への反映は完了していません。",
		TerminalInvestigated:                   "調査のみの依頼として、稼働環境とリポジトリを読み取りだけで計った報告をこのチケットに掲示しました。コードの変更と Pull Request はなく、対象リポジトリと本番環境は変更していません。このチケットでの自動処理は終了しています。",
		TerminalInvestigationIncomplete:        "調査に使える回数と時間の上限に達し、報告をまとめられなかったため、対象リポジトリと本番環境は変更せず停止しました。依頼の範囲を絞って再度起票すると、改めて調査します。",
		TerminalInvestigationNonconverged:      "調査報告が根拠のレビューを規定回数内に通らなかったため、対象リポジトリと本番環境は変更せず停止しました。運用担当者が内容を確認します。",
		TerminalDesignNonconverged:             "直し方の設計がレビューで規定回数内に合意に至らなかったため、コードは変更せず停止しました。争点は運用担当者が確認し、必要に応じてこのチケットでお知らせします。",
	}[report.Code]
	if message == "" {
		message = "自動処理は終了しました。詳細は実行履歴を参照してください。"
	}
	lines := []string{
		"自動処理の最終結果: " + string(report.Code),
		message,
		"実行履歴: " + report.RunURL,
	}
	if report.PullRequestURL != "" {
		lines = append(lines, "Pull Request: "+report.PullRequestURL)
	}
	if report.CommitURL != "" {
		lines = append(lines, "反映commit: "+report.CommitSHA+" "+report.CommitURL)
	}
	if report.StagingEvidenceURL != "" {
		lines = append(lines, "staging確認先: "+report.StagingEvidenceURL)
	}
	if report.ProductionEvidenceURL != "" {
		lines = append(lines, "production確認先: "+report.ProductionEvidenceURL)
	}
	if report.TrailText != "" {
		lines = append(lines, "", "## 証跡 (自動処理の実行記録)", report.TrailText)
	}
	if report.SpendText != "" {
		lines = append(lines, "", "## この依頼にかかった費用", report.SpendText)
	}
	return strings.Join(lines, "\n") + terminalCommentFacts(report, reportDigest).render()
}

// terminalCommentFacts maps every finite terminal code onto the seven-item
// comment contract: who acts next, what production verifiably looks like, and
// that the automation never retries on its own.
func terminalCommentFacts(report TerminalReportRequest, reportDigest string) CommentFacts {
	facts := CommentFacts{
		State:      "自動処理終了（" + string(report.Code) + "）",
		NextActor:  "運用担当者",
		Operation:  "起票者の操作は不要です（運用担当者が内容を確認し、必要ならこのチケットでお知らせします）",
		NextEvent:  "以後の自動通知はありません",
		Production: "未変更",
		AutoRetry:  "なし（自動での再実行・再起票は行いません）",
		Marker:     CommentMarker("terminal", report.AutomationRunID, string(report.Code), reportDigest),
	}
	switch report.Code {
	case TerminalSuccess:
		facts.NextActor = "起票者"
		switch {
		case report.ProductionEvidenceURL != "":
			facts.Operation = "本番の表示をご確認ください（対応は不要です）"
			facts.Production = "確認済み（利用者目線の表示確認まで完了）"
		case report.StagingEvidenceURL != "":
			facts.Operation = "staging の表示をご確認ください（本番への反映は人が行います）"
			facts.Production = "未変更（staging まで反映済み）"
		default:
			facts.Operation = "Pull Request の内容をご確認のうえ、マージをご判断ください"
			facts.Production = "未変更（Pull Request 作成まで）"
		}
	case TerminalInvestigated:
		facts.NextActor = "起票者"
		facts.Operation = "このチケットに掲示した調査報告と添付の実測をご確認ください（対応は不要です）"
		facts.Production = "未変更（コードの変更も Pull Request もありません）"
	case TerminalInvestigationIncomplete:
		facts.NextActor = "起票者"
		facts.Operation = "調査の範囲を絞って再度起票すると、改めて調査します"
	case TerminalClarificationExpired:
		facts.NextActor = "起票者"
		facts.Operation = "再度依頼する場合は、確認事項への回答内容を反映した新しいチケットとして起票してください"
	case TerminalCancelled:
		facts.NextActor = "起票者"
		facts.Operation = "対応は不要です（中止の指示どおり停止しました）"
	case TerminalProductionVerificationFailed:
		facts.Production = "変更済み（本番デプロイは完了、表示確認は失敗）"
	case TerminalProductionDeploymentUnverified:
		facts.Production = "不明（prod ブランチ反映済み、本番デプロイの完了は未確認）"
	}
	return facts
}

func randomLeaseToken() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", errors.New("lease token could not be generated")
	}
	return hex.EncodeToString(value), nil
}
