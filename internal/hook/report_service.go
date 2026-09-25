package hook

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"
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
	// ClaimedAtMillis comes from the bound run, never from the report sender.
	ClaimedAtMillis int64
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
	config                 ReportRouteConfig
	store                  TerminalReportStore
	backlog                TerminalCommentClient
	logger                 *slog.Logger
	board                  BoardProjector
	automaticDeliveryAfter time.Time
	now                    func() time.Time
	token                  func() (string, error)
}

// UseBoard mirrors endings onto the board humans watch: a delivery in its own
// column, everything else where a person picks it up.
func (s *TerminalReportService) UseBoard(board BoardProjector) { s.board = board }

// UseAutomaticDeliveryAfter mirrors the existing attendant's delivery cut-off
// for notification wording; it does not enable or schedule delivery.
func (s *TerminalReportService) UseAutomaticDeliveryAfter(after time.Time) {
	s.automaticDeliveryAfter = after
}

func (s *TerminalReportService) deliveryContinues(report TerminalReportRequest, binding TerminalBinding) bool {
	// A report that names the depth it reached was written by a run that
	// carried the depth itself: whatever was going to happen has happened,
	// and there is nothing after the report to promise. Only a report from
	// before the depth moved inside the run — which is what an empty value
	// means — can still be followed by a continuation.
	return report.ReachedDelivery == "" &&
		!s.automaticDeliveryAfter.IsZero() && binding.ClaimedAtMillis > 0 &&
		binding.ClaimedAtMillis >= s.automaticDeliveryAfter.UnixMilli() &&
		report.Code == TerminalSuccess && report.PullRequestURL != "" &&
		report.StagingEvidenceURL == "" && report.ProductionEvidenceURL == ""
}

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

	deliveryContinues := s.deliveryContinues(report, binding)
	comment := terminalCommentContent(report, reportDigest, deliveryContinues)
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
		phase := terminalBoardPhase(report.Code)
		if deliveryContinues {
			phase = BoardRunning
		}
		projectBoard(ctx, s.board, s.logger, binding.IssueID, phase)
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
//
// A delivery that stopped short says so once and says it the same way
// everywhere. Two sentences used to disagree about who takes it the rest of
// the way -- this one promised the engine would, "automatically, once the
// settings are complete", while the footer told the requester a person does
// it -- and the promise was untrue whichever of them the reader believed:
// the run is terminal, and nothing in it ever issues another promotion
// card (deliver.go refuses to carry a delivery past its ending). It was
// untrue in its reason as well. Every shortfall was described as settings
// that had not been written, including the one that is nothing of the kind:
// a production branch carrying changes staging does not have.
//
// So neither this sentence nor the footer names a cause any more. The
// shortfall line the comment already carries -- 「ここまでで止まった理由: 」 --
// is the one place the reason is stated, written by whatever actually held
// the delivery, and what is said here is only what is true of every one of
// them: this run is over, production was not reached, and reaching it takes
// a later request or a person.
func successMessage(report TerminalReportRequest) string {
	if report.ProductionEvidenceURL != "" {
		return "自動処理が完了し、本番環境への反映と確認が完了しました。"
	}
	if report.StagingEvidenceURL != "" {
		if report.DeliveryShortfall != "" {
			return "自動処理が完了し、staging への反映と確認まで完了しました。" +
				"この実行はここで終わりで、本番へは届いていません（理由は下に書いています）。" +
				"本番へ届くのは、その理由が解消したあとの新しい依頼か、人の操作によってです。" +
				"この実行が後から自動で本番へ反映することはありません。"
		}
		return "自動処理が完了し、staging への反映と確認まで完了しました。本番への反映は人が行います。"
	}
	if report.DeliveryShortfall != "" {
		// The delivery was configured to go further and this instance could
		// not take it there. Saying "the rest is done by a person" would
		// hand back work the engine is meant to do; what the ticket needs is
		// what the engine is waiting on, which the shortfall line below
		// names.
		return "自動処理が完了し、取り込み用の Pull Request の作成まで完了しました。本番環境は変更していません。" +
			"この納品先はもっと先まで届ける設定ですが、下に書いた理由でここまでとしています。"
	}
	return "自動処理が完了し、取り込み用の Pull Request の作成まで完了しました。マージと以後の反映は人が行います。本番環境は変更していません。"
}

// cancelledMessage says what the stop stopped, which is not always nothing.
//
// The fixed sentence this replaces said the repository and production were
// unchanged. That was true while a delivery was over the moment its pull
// request existed; now the requester can stop one whose change is already
// merged, deployed and looked at, and telling them nothing had moved would
// send them away from an environment they need to see. Nothing is ever
// rolled back automatically — that is the other half the ticket is owed.
func cancelledMessage(report TerminalReportRequest) string {
	switch {
	case report.ProductionEvidenceURL != "":
		return "起票者による中止の指示を確認したため、ここで停止しました。中止を受け取った時点で本番環境への反映と表示の確認までが完了しています。" +
			"自動での巻き戻しは行っていません。戻す場合は運用の手順で戻してください。"
	case report.StagingEvidenceURL != "":
		return "起票者による中止の指示を確認したため、ここで停止しました。中止を受け取った時点で staging への反映と表示の確認までが完了しています。" +
			"本番環境は変更していません。自動での巻き戻しは行っていません。"
	case report.PullRequestURL != "":
		return "起票者による中止の指示を確認したため、ここで停止しました。取り込み用の Pull Request は作成済みで、マージは行っていません。本番環境は変更していません。"
	}
	return "起票者による中止の指示を確認したため、対象リポジトリと本番環境は変更せず停止しました。このチケットでの自動処理は終了しています。"
}

// TerminalCommentContent is the comment a finished run leaves on its
// ticket. Exported like every other comment this package builds: the
// wording is the product, and the one builder that was not reachable from
// outside was the one nothing outside could hold to its sentences.
func TerminalCommentContent(report TerminalReportRequest, reportDigest string) string {
	return terminalCommentContent(report, reportDigest, false)
}

// The endings that come from a round count say which count.
//
// Three of them used to say 「最大回数内」 and 「規定回数内」, from a contract
// where a destination declared how many rounds it would pay for and the
// third one ended the delivery. There is no such default any more: rounds
// run while they make progress, a round that repeats itself is ruled on,
// and a delivery that gets here has been round either fifty times — the
// highest round number any record can carry — or as many times as an
// operator asked for. A requester reading 「最大回数」 would look for a
// setting that stopped their delivery and find none.
func terminalCommentContent(report TerminalReportRequest, reportDigest string, deliveryContinues bool) string {
	message := map[TerminalCode]string{
		TerminalSuccess:                        successMessage(report),
		TerminalInputRejected:                  "入力が許可された形式または範囲に一致しなかったため、変更していません。",
		TerminalReadinessRejected:              "チケットの内容が自動処理の受付条件を満たさなかったため、対象リポジトリと本番環境は変更していません。詳細は運用担当者が確認し、このチケットのコメントでお知らせします。",
		TerminalClarificationRequired:          "実装に着手する前に、依頼者にしか決められない確認事項が見つかったため、対象リポジトリと本番環境は変更せず停止しました。確認事項は運用担当者が確認し、必要に応じてこのチケットのコメントでお知らせします。同じチケットの再投入は不要です。",
		TerminalReadinessUnresolved:            "着手可否の自動判定が規定回数内に確定しなかったため、対象リポジトリと本番環境は変更せず停止しました。運用担当者が内容を確認します。同じチケットの再投入は不要です。",
		TerminalClarificationExpired:           "確認事項への回答が期限までに得られなかったため、対象リポジトリと本番環境は変更せず停止しました。このチケットでの自動処理は終了しています。再度依頼する場合は、確認事項への回答内容を反映した新しいチケットとして起票してください。",
		TerminalCancelled:                      cancelledMessage(report),
		TerminalModelFailed:                    modelFailedMessage(report),
		TerminalNonconverged:                   "自動レビューが、記録の上限（50 巡）または運用担当者が設定した巡数に達しても収束しなかったため、本番環境には反映していません。",
		TerminalValidationFailed:               "生成した変更が検証を通過しなかったため、本番環境には反映していません。",
		TerminalReleaseFailed:                  "既存のリリース経路で処理を完了できなかったため、本番環境への反映は完了していません。",
		TerminalProductionDeploymentUnverified: "prodブランチへの反映は完了しましたが、既存の本番デプロイが完了したことを確認できませんでした。自動的な追加変更やロールバックは行っていません。",
		TerminalProductionVerificationFailed:   "本番デプロイは完了しましたが、利用者目線の表示確認に失敗しました。自動的な追加変更やロールバックは行っていません。",
		TerminalInternalFailed:                 "自動処理中に内部エラーが発生し、依頼を完了できませんでした。",
		TerminalInvestigated:                   "調査のみの依頼として、稼働環境とリポジトリを読み取りだけで計った報告をこのチケットに掲示しました。コードの変更と Pull Request はなく、対象リポジトリと本番環境は変更していません。このチケットでの自動処理は終了しています。",
		TerminalInvestigationIncomplete:        incompleteMessage(report),
		TerminalInvestigationNonconverged:      "調査報告が、記録の上限（50 巡）に達しても根拠のレビューを通らなかったため、対象リポジトリと本番環境は変更せず停止しました。運用担当者が内容を確認します。",
		TerminalDesignNonconverged:             "直し方の設計が、記録の上限（50 巡）に達してもレビューの合意に至らなかったため、コードは変更せず停止しました。争点は運用担当者が確認し、必要に応じてこのチケットでお知らせします。",
		TerminalDesignRoundsSpent:              "直し方の設計は合意できましたが、その設計で作業に入った後、「設計そのものを変えるべき」という判断になりました。設計をやり直せる回数を使い切っていたため、リポジトリは変更せず停止しました。争点は運用担当者が確認し、必要に応じてこのチケットでお知らせします。",
		TerminalImplementationReturned:         "実装役が、変更を加えずに理由を報告して作業を返しました。対象リポジトリと本番環境は変更していません。報告の全文は下の実行の記録に載せています。どう進めるかは依頼者の判断です。内容を確認のうえ、必要な情報を書き足して起票し直してください。",
	}[report.Code]
	if message == "" {
		message = "自動処理は終了しました。詳細は実行履歴を参照してください。"
	}
	heading := "自動処理の最終結果: " + string(report.Code)
	facts := terminalCommentFacts(report, reportDigest)
	if deliveryContinues {
		heading = "進行状況: Pull Request 作成済み・自動処理を継続"
		message = "Pull Request を作成しました。続いて CI の結果を確認し、通過後に staging への取り込みと確認を進めます。本番環境は変更していません。"
		facts.State = "Pull Request 作成済み（CI 確認・staging 取り込みへ進行中）"
		facts.NextActor = "自動処理"
		facts.Operation = "いまは利用者の操作は不要です。自動処理の結果をお待ちください"
		facts.NextEvent = "staging の確認結果、または処理を進められない理由と必要な操作を、このチケットでお知らせします"
	}
	// The order is the requester's. What they can now do and where to look
	// at it come first; what was decided on their behalf comes next,
	// because with nothing asking them anything after the reception it is
	// their only sight of those decisions; then the cost. The pull request
	// and the record of how the work went follow, because they answer a
	// different question -- how it was built -- and a comment that opens
	// with the account of the building makes the reader hunt for the result.
	head := heading + "\n" + message
	if outcome := strings.TrimSpace(report.OutcomeText); outcome != "" {
		head += "\n\n" + outcome
	}
	// What a deeper delivery would have needed, on the ticket rather than in
	// a log. A destination asked for production and the change stopped at
	// its pull request: the requester is owed the reason on the same comment
	// that tells them where it stopped, which is why it sits with the
	// outcome rather than down with the links.
	if report.DeliveryShortfall != "" {
		head += "\n\nここまでで止まった理由: " + report.DeliveryShortfall
	}
	lines := []string{"実行履歴: " + report.RunURL}
	if report.PullRequestURL != "" {
		lines = append(lines, "Pull Request: "+report.PullRequestURL)
	}
	if report.CommitURL != "" {
		lines = append(lines, "反映commit: "+report.CommitSHA+" "+report.CommitURL)
	}
	// A report that composed its own outcome named the places to look
	// inside it, beside what was actually seen there. One from an engine
	// that composed none keeps them here, so an older report still says
	// where its delivery landed.
	if report.OutcomeText == "" {
		if report.StagingEvidenceURL != "" {
			lines = append(lines, "staging確認先: "+report.StagingEvidenceURL)
		}
		if report.ProductionEvidenceURL != "" {
			lines = append(lines, "production確認先: "+report.ProductionEvidenceURL)
		}
	}
	footer := facts.render()
	links := "\n\n" + strings.Join(lines, "\n")
	cost := ""
	if report.SpendText != "" {
		cost = "\n\n## この依頼にかかった費用\n" + report.SpendText
	}
	// Two parts of this comment can be long, and they give way in the order
	// they are worth least to the person reading: the run record first, then
	// what was decided. Never the outcome, never the places to look, never
	// the cost line the requester is owed, and never the footer, whose final
	// line is the marker the exactly-once machinery anchors on. Whatever
	// gives way says so and says where the whole of it is, so nothing is
	// dropped without the ticket admitting it.
	fixed := len(head) + len(cost) + len(links) + len(footer)
	assumptions := ""
	if decided := strings.TrimSpace(report.AssumptionsText); decided != "" {
		switch {
		case fixed+len("\n\n")+len(decided) <= MaxTrackerCommentBytes:
			assumptions = "\n\n" + decided
		case fixed+len("\n\n")+len(assumptionsElsewhere(report)) <= MaxTrackerCommentBytes:
			assumptions = "\n\n" + assumptionsElsewhere(report)
		}
	}
	body := head + assumptions + cost + links
	if report.TrailText == "" {
		return fitCommentWithin(body, footer)
	}
	room := MaxTrackerCommentBytes - len(body) - len(terminalTrailHeading) - len(footer)
	trail := ShortenTrailForComment(report.TrailText, room)
	if trail == "" {
		// The sentence saying where the record is costs bytes of its own,
		// and the room left may have none: appending it unmeasured pushed
		// the comment past the tracker's limit, which loses the whole
		// comment rather than the record it was standing in for.
		elsewhere := "\n\n" + terminalTrailElsewhere
		if len(body)+len(elsewhere)+len(footer) > MaxTrackerCommentBytes {
			return fitCommentWithin(body, footer)
		}
		return fitCommentWithin(body+elsewhere, footer)
	}
	return fitCommentWithin(body+terminalTrailHeading+trail, footer)
}

const (
	terminalTrailHeading = "\n\n## 証跡 (自動処理の実行記録)\n"
	// terminalTrailElsewhere stands in for the record when the rest of the
	// comment leaves it no room at all, so the ticket still says the record
	// exists and where to read it.
	terminalTrailElsewhere = "この実行の記録はこのコメントに収まらないため、上の実行履歴をご確認ください。"
	// commentBodyCutNote ends a comment held back from the tracker's limit.
	commentBodyCutNote = "\n…（このコメントに収まらないため、ここまでを掲示しています）\n"
)

// assumptionsElsewhere stands in for what the engine decided on its own when
// the comment has no room for the list, and it has to name a place that
// really holds the rest.
//
// The pull request description does: it is written after the last round that
// can decide anything and it carries the whole list. The run record does
// not — it is an account of the rounds, with no decisions section in it — so
// with no pull request the honest answer is the run's own records, which an
// operator can read. Sending a requester to a page that does not hold what
// they were sent for is worse than telling them it is not here.
func assumptionsElsewhere(report TerminalReportRequest) string {
	if report.PullRequestURL != "" {
		return "この依頼で本体が確認せずに決めたことの一覧は、このコメントに収まらないため、" +
			"Pull Request の説明に全文を載せています: " + report.PullRequestURL
	}
	return "この依頼で本体が確認せずに決めたことの一覧は、このコメントに収まらないため、" +
		"運用担当者が保管しているこの実行の記録に残してあります。"
}

// fitCommentWithin holds the whole comment to the tracker's limit without
// losing the footer, whose final line is the marker the exactly-once
// machinery anchors on.
//
// Both halves of that matter. A comment over the limit is refused by the
// client before it leaves this process, so the requester sees no report at
// all; a comment that posted without its marker is one the next attempt
// cannot recognise, so it gets posted again. The parts above gave way in
// order and this is the last guard, for the case where even what never
// gives way does not fit.
func fitCommentWithin(body, footer string) string {
	if len(body)+len(footer) <= MaxTrackerCommentBytes {
		return body + footer
	}
	room := MaxTrackerCommentBytes - len(footer) - len(commentBodyCutNote)
	if room <= 0 {
		return footer
	}
	clipped := body[:room]
	// Back off to a rune boundary, then off the rune that boundary begins,
	// so the comment never ends on half a character.
	for len(clipped) > 0 && !utf8.RuneStart(clipped[len(clipped)-1]) {
		clipped = clipped[:len(clipped)-1]
	}
	if len(clipped) > 0 && clipped[len(clipped)-1] >= utf8.RuneSelf {
		clipped = clipped[:len(clipped)-1]
	}
	return clipped + commentBodyCutNote + footer
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
			// Two instructions for one delivery is one too many. A staging
			// stop that was asked for ends with a person taking it on; a
			// staging stop that was held ends with nobody taking it on from
			// this run, and saying 「人が行います」 there sent the requester
			// to ask someone to do what the held condition still forbids.
			facts.Operation = "staging の表示をご確認ください（本番への反映は人が行います）"
			if report.DeliveryShortfall != "" {
				facts.Operation = "staging の表示をご確認ください（この実行は本番へ届いておらず、ここで終了しています）"
			}
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
		if incompleteAnswersRefused(report.IncompleteReason) {
			facts.Operation = "起票者の操作は不要です（運用担当者が規則と答えを確認し、必要ならこのチケットでお知らせします）"
		} else {
			facts.NextActor = "起票者"
			facts.Operation = "調査の範囲を絞って再度起票すると、改めて調査します"
		}
	case TerminalClarificationExpired:
		facts.NextActor = "起票者"
		facts.Operation = "再度依頼する場合は、確認事項への回答内容を反映した新しいチケットとして起票してください"
	case TerminalCancelled:
		facts.NextActor = "起票者"
		facts.Operation = "対応は不要です（中止の指示どおり停止しました）"
		// What the stop left behind. The default line below says production
		// is untouched, which is the truth for a stop before anything left
		// the pod and a falsehood for one after the promotion landed.
		switch {
		case report.ProductionEvidenceURL != "":
			facts.Production = "反映済み（中止の時点で本番への反映と確認が完了しています。自動の巻き戻しは行いません）"
		case report.StagingEvidenceURL != "":
			facts.Production = "未変更（staging までは反映済み。自動の巻き戻しは行いません）"
		case report.PullRequestURL != "":
			facts.Production = "未変更（Pull Request 作成まで。マージは行っていません）"
		}
	case TerminalImplementationReturned:
		// The implementer answered and the answer is on the ticket, so the
		// next move is the requester's. The default line — an operator will
		// look and the requester need do nothing — would send the one
		// person who can act on the report away from it.
		facts.NextActor = "起票者"
		facts.Operation = "下の実行の記録にある実装役の報告をご確認のうえ、進めるかどうかをご判断ください（進める場合は、報告をふまえて書き直した新しいチケットとして起票してください）"
	case TerminalProductionVerificationFailed:
		facts.Production = "変更済み（本番デプロイは完了、表示確認は失敗）"
	case TerminalProductionDeploymentUnverified:
		facts.Production = "不明（prod ブランチ反映済み、本番デプロイの完了は未確認）"
	}
	return facts
}

// incompleteMessage says why an investigation round sealed nothing. The
// budget and the wall are the requester's lever (a narrower request fits);
// a streak of answers the contract refused is not — narrowing changes
// nothing, the operator reads the rule and the answer — so the comment
// carries the last objection verbatim instead of the narrowing advice.
// A report without a reason (an older engine's) keeps the budget text.
func incompleteMessage(report TerminalReportRequest) string {
	if !incompleteAnswersRefused(report.IncompleteReason) {
		return "調査に使える回数と時間の上限に達し、報告をまとめられなかったため、対象リポジトリと本番環境は変更せず停止しました。依頼の範囲を絞って再度起票すると、改めて調査します。"
	}
	text := "AI が作った調査報告・設計が自動検査の規則に合わず、規定回数内に通らなかったため、対象リポジトリと本番環境は変更せず停止しました。依頼の範囲を絞っても同じ結果になるため、再起票は不要です。運用担当者が規則と答えを確認します。"
	if objection := singleLineBounded(report.IncompleteObjection, maxIncompleteObjectionBytes); objection != "" {
		text += "\n最後に拒否された点 (規則の原文): " + objection
	}
	return text
}

// maxIncompleteObjectionBytes bounds the objection a comment quotes.
const maxIncompleteObjectionBytes = 600

// modelFailedMessage says which step of the work could not be completed.
// model_failed is the most common way a run ends, and the sentence without
// the step is the same for every one of them: the requester cannot tell a
// stop before anything was written from one after the change was already
// made and reviewed, and the operator has to go to the container log to
// find out — which the next release erases. A report that carries no step
// is an older engine's, and keeps the older sentence.
func modelFailedMessage(report TerminalReportRequest) string {
	step := singleLineBounded(report.FailedStep, MaxFailedStepBytes)
	if report.ModelFailureReason == ModelFailureBudgetExhausted {
		if step == "" {
			step = "AI による処理"
		}
		return step + "でモデル利用枠の上限超過が報告されたため、処理を終了しました。本番環境には反映していません。\n" +
			BudgetFailureAction + "\nこの試行は利用枠が回復しても自動では再実行しません。運用担当者が再開方法をこのチケットで案内します。"
	}
	if step == "" {
		return "AIによる成果物の生成またはレビューを完了できなかったため、本番環境には反映していません。"
	}
	// The step carries "AI による" itself where a model actually ran. Three
	// of the steps ask no model anything, and a frame that asserts one made
	// the sentence a specific false claim about them.
	return step + "を完了できなかったため、本番環境には反映していません。"
}

// incompleteAnswersRefused tells a round that ended on the role's answers
// (refused by the contract, unreadable, or asking for what the round no
// longer gives) from one that ran out of budget or time.
func incompleteAnswersRefused(reason string) bool {
	if reason == "" {
		return false
	}
	// The budget and wall reasons are fixed sentences the round writes
	// itself; every other reason ends in text the model produced, so the
	// match is on the prefix, never on a substring a model could plant.
	for _, spent := range []string{"the probe budget is spent", "the read budget is spent", "the wall ended"} {
		if strings.HasPrefix(reason, spent) {
			return false
		}
	}
	return true
}

// singleLineBounded folds a text onto one line and cuts it at a byte bound
// on a character boundary.
func singleLineBounded(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "…"
}

func randomLeaseToken() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", errors.New("lease token could not be generated")
	}
	return hex.EncodeToString(value), nil
}
