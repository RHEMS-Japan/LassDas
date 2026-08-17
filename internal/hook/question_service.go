package hook

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

type QuestionCommentClient interface {
	FindExactComment(context.Context, int64, string) (int64, bool, error)
	AddCommentNotifying(context.Context, int64, string, []int64) (int64, error)
}

type QuestionReportProcessor interface {
	ProcessQuestionReport(context.Context, QuestionReportRequest) Result
}

// QuestionReportService posts the clarification question exactly once and
// moves the run into awaiting_answer. It mirrors TerminalReportService: begin
// seals the record under a lease, the deterministic comment is posted with a
// notification to the fixed answerer, exact-content lookup repairs a lost
// response, and complete binds the observed comment.
type QuestionReportService struct {
	config  ReportRouteConfig
	store   QuestionStore
	backlog QuestionCommentClient
	logger  *slog.Logger
	board   BoardProjector
	now     func() time.Time
	token   func() (string, error)
}

// UseBoard mirrors the answer wait onto the board humans watch.
func (s *QuestionReportService) UseBoard(board BoardProjector) { s.board = board }

func NewQuestionReportService(config ReportRouteConfig, store QuestionStore, backlog QuestionCommentClient, logger *slog.Logger) (*QuestionReportService, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if store == nil || backlog == nil || logger == nil {
		return nil, errors.New("question report dependencies must not be nil")
	}
	config.HMACKey = append([]byte(nil), config.HMACKey...)
	return &QuestionReportService{
		config: config, store: store, backlog: backlog, logger: logger,
		now: time.Now, token: randomLeaseToken,
	}, nil
}

func (s *QuestionReportService) ProcessQuestionReport(ctx context.Context, request QuestionReportRequest) Result {
	record := request.Record
	// The run is named by the ticket, so the route follows the record rather
	// than one value configured for the deployment. The store still refuses a
	// record whose sealed envelope and claim owner do not match that run.
	route := s.config
	route.ExpectedRunID = record.AutomationRunID
	if err := record.ValidateRoute(route); err != nil {
		return s.result(DecisionInvalid, "question_report_invalid", record.DeliveryID)
	}
	now := s.now().UTC()
	if request.IssuedAt.Before(now.Add(-s.config.ClockSkew)) || request.IssuedAt.After(now.Add(s.config.ClockSkew)) {
		return s.result(DecisionInvalid, "question_report_timestamp_invalid", record.DeliveryID)
	}
	encoded, err := MarshalQuestionRecord(record)
	if err != nil {
		return s.result(DecisionInvalid, "question_report_invalid", record.DeliveryID)
	}
	recordDigest := TerminalReportDigest(encoded)
	content, err := QuestionCommentContent(record)
	if err != nil {
		return s.result(DecisionInvalid, "question_report_invalid", record.DeliveryID)
	}
	leaseToken, err := s.token()
	if err != nil {
		return s.result(DecisionInternal, "question_report_token_failed", record.DeliveryID)
	}
	binding, disposition, err := s.store.BeginQuestion(ctx, QuestionBeginRequest{
		Record: record, RecordJSON: string(encoded), RecordSHA256: recordDigest, Route: route,
		StartedAt: now, LeaseUntil: now.Add(s.config.LeaseDuration), LeaseToken: leaseToken,
	})
	if err != nil {
		return s.failure(DecisionRetryRequested, "question_report_begin", err, record.DeliveryID)
	}
	switch disposition {
	case QuestionBeginBusy:
		return s.result(DecisionRetryRequested, "question_report_pending", record.DeliveryID)
	case QuestionBeginComplete:
		return s.result(DecisionAccepted, "question_report_already_recorded", record.DeliveryID)
	case QuestionBeginConflict:
		return s.result(DecisionInvalid, "question_report_conflict", record.DeliveryID)
	case QuestionBeginAcquired:
	default:
		return s.result(DecisionInternal, "question_report_state_invalid", record.DeliveryID)
	}
	commentID, found, err := s.backlog.FindExactComment(ctx, binding.IssueID, content)
	if err != nil {
		return s.failure(DecisionRetryRequested, "question_comment_lookup", err, record.DeliveryID)
	}
	if !found {
		commentID, err = s.backlog.AddCommentNotifying(ctx, binding.IssueID, content, []int64{s.config.AllowedCreatorID})
		if err != nil {
			return s.failure(DecisionRetryRequested, "question_comment_add", err, record.DeliveryID)
		}
	}
	if commentID <= 0 {
		return s.result(DecisionInternal, "question_comment_id_invalid", record.DeliveryID)
	}
	complete, err := s.store.CompleteQuestion(ctx, QuestionCompleteRequest{
		Record: record, RecordJSON: string(encoded), RecordSHA256: recordDigest, Route: route,
		LeaseToken: leaseToken, CommentID: commentID, PostedAt: s.now().UTC(),
	})
	if err != nil {
		return s.failure(DecisionRetryRequested, "question_report_complete", err, record.DeliveryID)
	}
	switch complete {
	case QuestionCompleted, QuestionAlreadyComplete:
		projectBoard(ctx, s.board, s.logger, binding.IssueID, BoardAwaitingAnswer)
		return s.result(DecisionAccepted, "question_report_recorded", record.DeliveryID)
	case QuestionCompleteConflict:
		return s.result(DecisionInvalid, "question_report_conflict", record.DeliveryID)
	default:
		return s.result(DecisionInternal, "question_report_state_invalid", record.DeliveryID)
	}
}

// OverrideClock replaces the wall clock and lease-token source so tests can
// drive the schedule deterministically. Production wiring never calls this.
func (s *QuestionReportService) OverrideClock(now func() time.Time, token func() (string, error)) {
	s.now, s.token = now, token
}

func (s *QuestionReportService) failure(retry Decision, operation string, err error, deliveryID string) Result {
	class, _ := FailureDetails(err)
	if class == FailureRejected {
		return s.result(DecisionInvalid, operation+"_rejected", deliveryID)
	}
	return s.result(retry, operation+"_failed", deliveryID)
}

func (s *QuestionReportService) result(decision Decision, code, deliveryID string) Result {
	s.logger.Info("question report decision", "decision", decision, "code", code, "delivery_id", deliveryID)
	return Result{Decision: decision, Code: code, DeliveryID: deliveryID}
}

// QuestionWaitSnapshot is the sealed waiting state the tick operates on.
// Posting marks a run whose question was sealed but whose comment was never
// bound (the poster died mid-flight); QuestionCommentID is zero then.
type QuestionWaitSnapshot struct {
	Record              QuestionRecord
	RecordJSON          string
	RecordSHA256        string
	QuestionCommentID   int64
	IssueID             int64
	ClarificationJSON   string
	ClarificationSHA256 string
	Posting             bool
}

type QuestionWaitStore interface {
	LoadQuestionWait(context.Context, ReportRouteConfig) (QuestionWaitSnapshot, bool, error)
}

type QuestionTickStore interface {
	QuestionWaitStore
	QuestionStore
	NotifyStore
	ReplyStore
	ResumeStore
	RunCommentStore
	RunNoticeStore
	IngestCursorStore
}

// RunNoticeSnapshot is the run-level view the tick uses for the acceptance
// and receipt notices and for the ingest completion gate.
type RunNoticeSnapshot struct {
	Exists            bool
	Terminal          bool
	IssueID           int64
	Snapshot          TicketSnapshot
	ClarificationJSON string
}

type RunNoticeStore interface {
	LoadRunNotice(context.Context, ReportRouteConfig) (RunNoticeSnapshot, error)
}

type IngestCursorStore interface {
	LoadIngestCursor(context.Context, ReportRouteConfig) (int64, error)
	StoreIngestCursor(context.Context, ReportRouteConfig, int64) error
}

type RecentActivityClient interface {
	ProjectRecentUpdates(context.Context, int64, int64) ([]WebhookHint, error)
}

type QuestionTickCommentClient interface {
	ListComments(context.Context, int64, int64) ([]BacklogComment, error)
	RecentActivityClient
	QuestionCommentClient
}

type QuestionTickProcessor interface {
	ProcessQuestionTick(context.Context, QuestionTickRequest) Result
}

// QuestionTickService is the 5-minute wake-up for a waiting question. One
// tick does, in contract order: adopt a valid answer (resume) or an explicit
// cancel (terminate), otherwise expire past the deadline, otherwise send owed
// intake replies and the due renotification. Every external post goes through
// its own exactly-once marker, so repeating a tick is always harmless.
type QuestionTickService struct {
	config   ReportRouteConfig
	store    QuestionTickStore
	backlog  QuestionTickCommentClient
	reporter TerminalReportProcessor
	ingest   HookProcessor
	logger   *slog.Logger
	board    BoardProjector
	now      func() time.Time
	token    func() (string, error)
}

// UseBoard mirrors a recovered posting and an adopted answer onto the board
// humans watch. Endings are projected by the terminal reporter, not here.
func (s *QuestionTickService) UseBoard(board BoardProjector) { s.board = board }

func NewQuestionTickService(config ReportRouteConfig, store QuestionTickStore, backlog QuestionTickCommentClient, reporter TerminalReportProcessor, ingest HookProcessor, logger *slog.Logger) (*QuestionTickService, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if store == nil || backlog == nil || reporter == nil || ingest == nil || logger == nil {
		return nil, errors.New("question tick dependencies must not be nil")
	}
	config.HMACKey = append([]byte(nil), config.HMACKey...)
	return &QuestionTickService{
		config: config, store: store, backlog: backlog, reporter: reporter, ingest: ingest, logger: logger,
		now: time.Now, token: randomLeaseToken,
	}, nil
}

func (s *QuestionTickService) ProcessQuestionTick(ctx context.Context, request QuestionTickRequest) Result {
	if request.Protocol != QuestionTickProtocol || request.AutomationRunID != s.config.ExpectedRunID {
		return s.result(DecisionInvalid, "question_tick_invalid", "")
	}
	now := s.now().UTC()
	if request.IssuedAt.Before(now.Add(-s.config.ClockSkew)) || request.IssuedAt.After(now.Add(s.config.ClockSkew)) {
		return s.result(DecisionInvalid, "question_tick_timestamp_invalid", "")
	}
	notice, err := s.store.LoadRunNotice(ctx, s.config)
	if err != nil {
		return s.failure("question_tick_load", err, "")
	}
	if !notice.Exists {
		// No active run: this wake-up may complete a lost webhook instead
		// (README: 既存 5 分 schedule は active run がない場合に限り照合する).
		return s.completeLostIngest(ctx)
	}
	// A notice that could not be posted must never hold back the answer
	// adoption or the deadline: a stuck acknowledgement would otherwise mean
	// the run waits past its expiry with no ending at all. The unfinished
	// notice is carried to the end and reported only when nothing more
	// decisive happened on this wake-up.
	var noticeResult Result
	noticePending := false
	if !notice.Terminal {
		if result, blocked := s.postRunNotices(ctx, notice); blocked {
			noticeResult, noticePending = result, true
		}
	}
	snapshot, waiting, err := s.store.LoadQuestionWait(ctx, s.config)
	if err != nil {
		return s.failure("question_tick_load", err, "")
	}
	if !waiting {
		if noticePending {
			return noticeResult
		}
		return s.result(DecisionAccepted, "question_tick_idle", "")
	}
	deliveryID := snapshot.Record.DeliveryID
	// A sealed question whose comment was never bound means the poster died
	// between begin and complete: without recovery the requester would see
	// neither the question nor any ending. Finish the posting here, from the
	// sealed record, through the same lease and repair path.
	if snapshot.Posting {
		return s.recoverPosting(ctx, snapshot, deliveryID)
	}
	comments, err := s.backlog.ListComments(ctx, snapshot.IssueID, snapshot.QuestionCommentID)
	if err != nil {
		return s.failure("question_tick_comments", err, deliveryID)
	}
	guidanceSent, err := s.store.ReplyState(ctx, s.config, snapshot.Record, ReplyGuidance, 0)
	if err != nil {
		return s.failure("question_tick_reply_state", err, deliveryID)
	}
	handled := map[int64]bool{}
	for _, comment := range comments {
		if comment.UserID != s.config.AllowedCreatorID || comment.CommentID <= snapshot.QuestionCommentID {
			continue
		}
		replied, err := s.store.ReplyState(ctx, s.config, snapshot.Record, ReplyShortfall, comment.CommentID)
		if err != nil {
			return s.failure("question_tick_reply_state", err, deliveryID)
		}
		if replied {
			handled[comment.CommentID] = true
		}
	}
	decision, err := EvaluateAnswerIntake(AnswerIntakeInput{
		Question:          snapshot.Record,
		QuestionCommentID: snapshot.QuestionCommentID,
		AnswererID:        s.config.AllowedCreatorID,
		GuidanceSent:      guidanceSent,
		HandledCommentIDs: handled,
		Comments:          comments,
	})
	if err != nil {
		return s.result(DecisionInvalid, "question_tick_intake_invalid", deliveryID)
	}
	if decision.Cancel != nil {
		return s.terminate(ctx, snapshot.Record, TerminalCancelled, now, deliveryID, "question_tick_cancelled")
	}
	if decision.Adopted != nil {
		return s.resume(ctx, snapshot, *decision.Adopted, now, deliveryID)
	}
	action := DecideQuestionTick(snapshot.Record, now.UnixMilli())
	if action.Kind == QuestionTickExpire {
		return s.terminate(ctx, snapshot.Record, TerminalClarificationExpired, now, deliveryID, "question_tick_expired")
	}
	for _, reply := range decision.Replies {
		if result, done := s.postReply(ctx, snapshot, reply, deliveryID); !done {
			return result
		}
	}
	if action.Kind == QuestionTickNotify {
		return s.postNotify(ctx, snapshot, action.NotifyIndex, deliveryID)
	}
	if len(decision.Replies) > 0 {
		return s.result(DecisionAccepted, "question_tick_replied", deliveryID)
	}
	if noticePending {
		return noticeResult
	}
	return s.result(DecisionAccepted, "question_tick_waiting", deliveryID)
}

func (s *QuestionTickService) recoverPosting(ctx context.Context, snapshot QuestionWaitSnapshot, deliveryID string) Result {
	content, err := QuestionCommentContent(snapshot.Record)
	if err != nil {
		return s.result(DecisionInvalid, "question_tick_posting_invalid", deliveryID)
	}
	now := s.now().UTC()
	leaseToken, err := s.token()
	if err != nil {
		return s.result(DecisionInternal, "question_tick_token_failed", deliveryID)
	}
	binding, disposition, err := s.store.BeginQuestion(ctx, QuestionBeginRequest{
		Record: snapshot.Record, RecordJSON: snapshot.RecordJSON, RecordSHA256: snapshot.RecordSHA256, Route: s.config,
		StartedAt: now, LeaseUntil: now.Add(s.config.LeaseDuration), LeaseToken: leaseToken,
	})
	if err != nil {
		return s.failure("question_tick_posting_begin", err, deliveryID)
	}
	switch disposition {
	case QuestionBeginComplete:
		return s.result(DecisionAccepted, "question_tick_waiting", deliveryID)
	case QuestionBeginBusy:
		return s.result(DecisionRetryRequested, "question_tick_posting_pending", deliveryID)
	case QuestionBeginConflict:
		return s.result(DecisionInvalid, "question_tick_posting_conflict", deliveryID)
	case QuestionBeginAcquired:
	default:
		return s.result(DecisionInternal, "question_tick_posting_invalid", deliveryID)
	}
	commentID, found, err := s.backlog.FindExactComment(ctx, binding.IssueID, content)
	if err != nil {
		return s.failure("question_tick_posting_lookup", err, deliveryID)
	}
	if !found {
		commentID, err = s.backlog.AddCommentNotifying(ctx, binding.IssueID, content, []int64{s.config.AllowedCreatorID})
		if err != nil {
			return s.failure("question_tick_posting_add", err, deliveryID)
		}
	}
	complete, err := s.store.CompleteQuestion(ctx, QuestionCompleteRequest{
		Record: snapshot.Record, RecordJSON: snapshot.RecordJSON, RecordSHA256: snapshot.RecordSHA256, Route: s.config,
		LeaseToken: leaseToken, CommentID: commentID, PostedAt: s.now().UTC(),
	})
	if err != nil {
		return s.failure("question_tick_posting_complete", err, deliveryID)
	}
	if complete == QuestionCompleted || complete == QuestionAlreadyComplete {
		projectBoard(ctx, s.board, s.logger, binding.IssueID, BoardAwaitingAnswer)
		return s.result(DecisionAccepted, "question_tick_question_posted", deliveryID)
	}
	return s.result(DecisionInvalid, "question_tick_posting_conflict", deliveryID)
}

// terminate synthesizes the terminal report for the waiting run from its own
// sealed question record — the identity, workflow owner and run URL are all
// in the record, so the report matches the stored claim exactly.
func (s *QuestionTickService) terminate(ctx context.Context, record QuestionRecord, code TerminalCode, now time.Time, deliveryID, okCode string) Result {
	report := TerminalReportRequest{
		Protocol:          TerminalReportProtocolVersion,
		DeliveryID:        record.DeliveryID,
		InputSHA256:       record.InputSHA256,
		RepositoryID:      record.RepositoryID,
		RepositorySHA256:  record.RepositorySHA256,
		WorkflowRefSHA256: record.WorkflowRefSHA256,
		WorkflowSHA:       record.WorkflowSHA,
		WorkflowRunID:     record.WorkflowRunID,
		RunAttempt:        record.RunAttempt,
		AutomationRunID:   record.AutomationRunID,
		Code:              code,
		RunURL:            record.RunURL,
		IssuedAt:          now,
	}
	result := s.reporter.ProcessTerminalReport(ctx, report)
	if result.Decision == DecisionAccepted {
		return s.result(DecisionAccepted, okCode, deliveryID)
	}
	return result
}

func (s *QuestionTickService) resume(ctx context.Context, snapshot QuestionWaitSnapshot, adopted AdoptedAnswerDecision, now time.Time, deliveryID string) Result {
	record := snapshot.Record
	rounds := []ClarificationRound{}
	if snapshot.ClarificationJSON != "" {
		previous, err := DecodeClarificationRecord([]byte(snapshot.ClarificationJSON))
		if err != nil {
			return s.result(DecisionInvalid, "question_tick_resume_invalid", deliveryID)
		}
		rounds = append(rounds, previous.Rounds...)
	}
	rounds = append(rounds, ClarificationRound{
		QuestionRecordJSON:   snapshot.RecordJSON,
		QuestionRecordSHA256: snapshot.RecordSHA256,
		QuestionCommentID:    snapshot.QuestionCommentID,
		AnswerCommentID:      adopted.CommentID,
		AnswererID:           s.config.AllowedCreatorID,
		AnswerPostedAt:       adopted.PostedAt,
		AnswerBodySHA256:     adopted.BodySHA256,
		AnswersJSON:          adopted.AnswersJSON,
		AnswersSHA256:        TerminalReportDigest([]byte(adopted.AnswersJSON)),
	})
	clarification := ClarificationRecord{
		Protocol:          ClarificationProtocolVersion,
		DeliveryID:        record.DeliveryID,
		InputSHA256:       record.InputSHA256,
		RepositoryID:      record.RepositoryID,
		RepositorySHA256:  record.RepositorySHA256,
		WorkflowRefSHA256: record.WorkflowRefSHA256,
		AutomationRunID:   record.AutomationRunID,
		InputRevision:     record.QuestionRevision + 1,
		Rounds:            rounds,
	}
	encoded, err := MarshalClarificationRecord(clarification)
	if err != nil {
		return s.result(DecisionInvalid, "question_tick_resume_invalid", deliveryID)
	}
	disposition, err := s.store.ResumeWithAnswer(ctx, ResumeRequest{
		Record: clarification, RecordJSON: string(encoded), RecordSHA256: TerminalReportDigest(encoded),
		PreviousRecordJSON: snapshot.ClarificationJSON, PreviousRecordSHA256: snapshot.ClarificationSHA256,
		Route: s.config, ResumedAt: now,
	})
	if err != nil {
		return s.failure("question_tick_resume", err, deliveryID)
	}
	switch disposition {
	case ResumeCompleted, ResumeAlreadyComplete:
		projectBoard(ctx, s.board, s.logger, snapshot.IssueID, BoardRunning)
		return s.result(DecisionAccepted, "question_tick_resumed", deliveryID)
	default:
		return s.result(DecisionInvalid, "question_tick_resume_conflict", deliveryID)
	}
}

func (s *QuestionTickService) postReply(ctx context.Context, snapshot QuestionWaitSnapshot, reply AnswerReply, deliveryID string) (Result, bool) {
	var content string
	var err error
	kind := ReplyGuidance
	if reply.Kind == AnswerReplyShortfall {
		kind = ReplyShortfall
		content, err = ShortfallCommentContent(snapshot.Record, reply.CommentID, reply.MissingQuestionIDs)
	} else {
		content = GuidanceCommentContent(snapshot.Record)
	}
	if err != nil {
		return s.result(DecisionInvalid, "question_tick_reply_invalid", deliveryID), false
	}
	now := s.now().UTC()
	leaseToken, err := s.token()
	if err != nil {
		return s.result(DecisionInternal, "question_tick_token_failed", deliveryID), false
	}
	binding, disposition, err := s.store.BeginReply(ctx, ReplyBeginRequest{
		Record: snapshot.Record, RecordJSON: snapshot.RecordJSON, RecordSHA256: snapshot.RecordSHA256, Route: s.config,
		Kind: kind, TriggerCommentID: reply.CommentID, ContentSHA256: TerminalReportDigest([]byte(content)),
		StartedAt: now, LeaseUntil: now.Add(s.config.LeaseDuration), LeaseToken: leaseToken,
	})
	if err != nil {
		return s.failure("question_tick_reply_begin", err, deliveryID), false
	}
	switch disposition {
	case ReplyBeginComplete:
		return Result{}, true
	case ReplyBeginBusy:
		return s.result(DecisionRetryRequested, "question_tick_reply_pending", deliveryID), false
	case ReplyBeginConflict:
		return s.result(DecisionInvalid, "question_tick_reply_conflict", deliveryID), false
	case ReplyBeginAcquired:
	default:
		return s.result(DecisionInternal, "question_tick_reply_invalid", deliveryID), false
	}
	commentID, found, err := s.backlog.FindExactComment(ctx, binding.IssueID, content)
	if err != nil {
		return s.failure("question_tick_reply_lookup", err, deliveryID), false
	}
	if !found {
		commentID, err = s.backlog.AddCommentNotifying(ctx, binding.IssueID, content, []int64{s.config.AllowedCreatorID})
		if err != nil {
			return s.failure("question_tick_reply_add", err, deliveryID), false
		}
	}
	complete, err := s.store.CompleteReply(ctx, ReplyCompleteRequest{
		Record: snapshot.Record, RecordJSON: snapshot.RecordJSON, RecordSHA256: snapshot.RecordSHA256, Route: s.config,
		Kind: kind, TriggerCommentID: reply.CommentID, ContentSHA256: TerminalReportDigest([]byte(content)),
		LeaseToken: leaseToken, CommentID: commentID, PostedAt: s.now().UTC(),
	})
	if err != nil {
		return s.failure("question_tick_reply_complete", err, deliveryID), false
	}
	if complete == ReplyCompleted || complete == ReplyAlreadyComplete {
		return Result{}, true
	}
	return s.result(DecisionInvalid, "question_tick_reply_conflict", deliveryID), false
}

func (s *QuestionTickService) postNotify(ctx context.Context, snapshot QuestionWaitSnapshot, index int, deliveryID string) Result {
	content, err := NotifyCommentContent(snapshot.Record, index)
	if err != nil {
		return s.result(DecisionInvalid, "question_tick_notify_invalid", deliveryID)
	}
	now := s.now().UTC()
	leaseToken, err := s.token()
	if err != nil {
		return s.result(DecisionInternal, "question_tick_token_failed", deliveryID)
	}
	binding, disposition, err := s.store.BeginNotify(ctx, NotifyBeginRequest{
		Record: snapshot.Record, RecordJSON: snapshot.RecordJSON, RecordSHA256: snapshot.RecordSHA256, Route: s.config,
		Index: index, StartedAt: now, LeaseUntil: now.Add(s.config.LeaseDuration), LeaseToken: leaseToken,
	})
	if err != nil {
		return s.failure("question_tick_notify_begin", err, deliveryID)
	}
	switch disposition {
	case NotifyBeginComplete:
		return s.result(DecisionAccepted, "question_tick_waiting", deliveryID)
	case NotifyBeginBusy:
		return s.result(DecisionRetryRequested, "question_tick_notify_pending", deliveryID)
	case NotifyBeginConflict:
		return s.result(DecisionInvalid, "question_tick_notify_conflict", deliveryID)
	case NotifyBeginAcquired:
	default:
		return s.result(DecisionInternal, "question_tick_notify_invalid", deliveryID)
	}
	commentID, found, err := s.backlog.FindExactComment(ctx, binding.IssueID, content)
	if err != nil {
		return s.failure("question_tick_notify_lookup", err, deliveryID)
	}
	if !found {
		commentID, err = s.backlog.AddCommentNotifying(ctx, binding.IssueID, content, []int64{s.config.AllowedCreatorID})
		if err != nil {
			return s.failure("question_tick_notify_add", err, deliveryID)
		}
	}
	complete, err := s.store.CompleteNotify(ctx, NotifyCompleteRequest{
		Record: snapshot.Record, RecordJSON: snapshot.RecordJSON, RecordSHA256: snapshot.RecordSHA256, Route: s.config,
		Index: index, LeaseToken: leaseToken, CommentID: commentID, PostedAt: s.now().UTC(),
	})
	if err != nil {
		return s.failure("question_tick_notify_complete", err, deliveryID)
	}
	if complete == NotifyCompleted || complete == NotifyAlreadyComplete {
		return s.result(DecisionAccepted, "question_tick_notified", deliveryID)
	}
	return s.result(DecisionInvalid, "question_tick_notify_conflict", deliveryID)
}

// OverrideClock replaces the wall clock and lease-token source so tests can
// drive the schedule deterministically. Production wiring never calls this.
func (s *QuestionTickService) OverrideClock(now func() time.Time, token func() (string, error)) {
	s.now, s.token = now, token
}

func (s *QuestionTickService) failure(operation string, err error, deliveryID string) Result {
	class, _ := FailureDetails(err)
	if class == FailureRejected {
		return s.result(DecisionInvalid, operation+"_rejected", deliveryID)
	}
	return s.result(DecisionRetryRequested, operation+"_failed", deliveryID)
}

func (s *QuestionTickService) result(decision Decision, code, deliveryID string) Result {
	s.logger.Info("question tick decision", "decision", decision, "code", code, "delivery_id", deliveryID)
	return Result{Decision: decision, Code: code, DeliveryID: deliveryID}
}

// postRunNotices posts the owed acceptance and answer-receipt notices. A
// transient failure returns (result, true) so the tick retries next wake-up;
// otherwise processing continues.
func (s *QuestionTickService) postRunNotices(ctx context.Context, notice RunNoticeSnapshot) (Result, bool) {
	deliveryID := notice.Snapshot.DeliveryID
	posted, err := s.store.RunCommentState(ctx, s.config, RunCommentAck, "")
	if err != nil {
		return s.failure("question_tick_notice_state", err, deliveryID), true
	}
	if !posted {
		content := AckCommentContent(notice.Snapshot)
		if result, ok := s.postRunComment(ctx, RunCommentAck, "", content, deliveryID); !ok {
			return result, true
		}
	}
	if notice.ClarificationJSON == "" {
		return Result{}, false
	}
	record, err := DecodeClarificationRecord([]byte(notice.ClarificationJSON))
	if err != nil {
		return s.result(DecisionInvalid, "question_tick_notice_invalid", deliveryID), true
	}
	lastRound := record.Rounds[len(record.Rounds)-1]
	question, err := DecodeQuestionRecord([]byte(lastRound.QuestionRecordJSON))
	if err != nil {
		return s.result(DecisionInvalid, "question_tick_notice_invalid", deliveryID), true
	}
	qualifier := questionRevisionTag(question.QuestionRevision) + ":" + fmt.Sprintf("%d", lastRound.AnswerCommentID)
	posted, err = s.store.RunCommentState(ctx, s.config, RunCommentReceipt, qualifier)
	if err != nil {
		return s.failure("question_tick_notice_state", err, deliveryID), true
	}
	if !posted {
		content, err := ReceiptCommentContent(question, lastRound.AnswerCommentID)
		if err != nil {
			return s.result(DecisionInvalid, "question_tick_notice_invalid", deliveryID), true
		}
		if result, ok := s.postRunComment(ctx, RunCommentReceipt, qualifier, content, deliveryID); !ok {
			return result, true
		}
	}
	return Result{}, false
}

func (s *QuestionTickService) postRunComment(ctx context.Context, kind RunCommentKind, qualifier, content, deliveryID string) (Result, bool) {
	now := s.now().UTC()
	leaseToken, err := s.token()
	if err != nil {
		return s.result(DecisionInternal, "question_tick_token_failed", deliveryID), false
	}
	binding, disposition, err := s.store.BeginRunComment(ctx, RunCommentBeginRequest{
		Route: s.config, Kind: kind, Qualifier: qualifier, ContentSHA256: TerminalReportDigest([]byte(content)),
		StartedAt: now, LeaseUntil: now.Add(s.config.LeaseDuration), LeaseToken: leaseToken,
	})
	if err != nil {
		return s.failure("question_tick_notice_begin", err, deliveryID), false
	}
	switch disposition {
	case ReplyBeginComplete:
		return Result{}, true
	case ReplyBeginBusy:
		return s.result(DecisionRetryRequested, "question_tick_notice_pending", deliveryID), false
	case ReplyBeginConflict:
		return s.result(DecisionInvalid, "question_tick_notice_conflict", deliveryID), false
	case ReplyBeginAcquired:
	default:
		return s.result(DecisionInternal, "question_tick_notice_invalid", deliveryID), false
	}
	commentID, found, err := s.backlog.FindExactComment(ctx, binding.IssueID, content)
	if err != nil {
		return s.failure("question_tick_notice_lookup", err, deliveryID), false
	}
	if !found {
		commentID, err = s.backlog.AddCommentNotifying(ctx, binding.IssueID, content, []int64{s.config.AllowedCreatorID})
		if err != nil {
			return s.failure("question_tick_notice_add", err, deliveryID), false
		}
	}
	complete, err := s.store.CompleteRunComment(ctx, RunCommentCompleteRequest{
		Route: s.config, Kind: kind, Qualifier: qualifier, ContentSHA256: TerminalReportDigest([]byte(content)),
		LeaseToken: leaseToken, CommentID: commentID, PostedAt: s.now().UTC(),
	})
	if err != nil {
		return s.failure("question_tick_notice_complete", err, deliveryID), false
	}
	if complete == ReplyCompleted || complete == ReplyAlreadyComplete {
		return Result{}, true
	}
	return s.result(DecisionInvalid, "question_tick_notice_conflict", deliveryID), false
}

// completeLostIngest reads the project's recent updates past the stored
// cursor and feeds matching issue-created activities through the same webhook
// processor, so a lost webhook cannot silence an accepted ticket forever.
func (s *QuestionTickService) completeLostIngest(ctx context.Context) Result {
	cursor, err := s.store.LoadIngestCursor(ctx, s.config)
	if err != nil {
		return s.failure("question_tick_ingest_cursor", err, "")
	}
	hints, err := s.backlog.ProjectRecentUpdates(ctx, s.config.ProjectID, cursor)
	if err != nil {
		return s.failure("question_tick_ingest_list", err, "")
	}
	advanced := cursor
	matched := 0
	stalled := false
	for _, hint := range hints {
		if hint.ActivityID <= advanced {
			continue
		}
		if hint.ActivityType != s.config.AllowedActivityType || hint.ProjectID != s.config.ProjectID ||
			hint.CreatorID != s.config.AllowedCreatorID {
			// Conclusively not a ticket this automation owns.
			advanced = hint.ActivityID
			continue
		}
		matched++
		result := s.ingest.Process(ctx, hint)
		s.logger.Info("question tick ingest", "activity_id", hint.ActivityID, "decision", result.Decision, "code", result.Code)
		if !ingestOutcomeConclusive(result.Decision) {
			// Leave the cursor before this activity. Advancing past an
			// unresolved outcome would skip the ticket on every later scan,
			// and this scan is the only net under a lost webhook: the
			// requester would wait forever with no acknowledgement, no
			// question and no final report.
			stalled = true
			break
		}
		advanced = hint.ActivityID
	}
	if advanced > cursor {
		if err := s.store.StoreIngestCursor(ctx, s.config, advanced); err != nil {
			return s.failure("question_tick_ingest_cursor", err, "")
		}
	}
	if stalled {
		return s.result(DecisionRetryRequested, "question_tick_ingest_incomplete", "")
	}
	if matched > 0 {
		return s.result(DecisionAccepted, "question_tick_ingested", "")
	}
	return s.result(DecisionAccepted, "question_tick_idle", "")
}

// ingestOutcomeConclusive reports whether the ingest decision settled the
// activity for good. Accepted (queued or already known), ignored (not ours)
// and invalid (malformed beyond retry) are final; everything else means the
// activity still needs another attempt.
func ingestOutcomeConclusive(decision Decision) bool {
	return decision == DecisionAccepted || decision == DecisionIgnored || decision == DecisionInvalid
}
