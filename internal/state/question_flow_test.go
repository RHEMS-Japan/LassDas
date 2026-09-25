package state

import (
	"context"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// fakeBacklog is a deterministic Backlog double for the service-level flow:
// comments are appended with ascending IDs and can be listed back with author
// and server timestamp.
type fakeBacklog struct {
	nextID     int64
	now        func() time.Time
	botID      int64
	comments   []hook.BacklogComment
	activities []hook.WebhookHint
	// listedFrom records the id each listing started after, so a test can
	// see how much of the thread a caller asked the tracker for.
	listedFrom []int64
	// tooLongFromStart makes a listing that starts at the beginning fail the
	// way the real client fails on a ticket with more comments than one
	// listing window holds.
	tooLongFromStart bool
	// listErr makes every listing fail, the way an unreachable tracker does.
	listErr error
}

func (f *fakeBacklog) FindExactComment(_ context.Context, _ int64, content string) (int64, bool, error) {
	for _, comment := range f.comments {
		if comment.Body == content {
			return comment.CommentID, true, nil
		}
	}
	return 0, false, nil
}

func (f *fakeBacklog) FindCommentWithMarker(_ context.Context, _ int64, marker string) (int64, bool, error) {
	for _, comment := range f.comments {
		if strings.Contains(comment.Body, marker) {
			return comment.CommentID, true, nil
		}
	}
	return 0, false, nil
}

func (f *fakeBacklog) AddCommentNotifying(_ context.Context, _ int64, content string, _ []int64) (int64, error) {
	f.nextID++
	f.comments = append(f.comments, hook.BacklogComment{
		CommentID: f.nextID, UserID: f.botID, Body: content, PostedAt: f.now().UnixMilli(),
	})
	return f.nextID, nil
}

func (f *fakeBacklog) AddComment(ctx context.Context, issueID int64, content string) (int64, error) {
	return f.AddCommentNotifying(ctx, issueID, content, nil)
}

func (f *fakeBacklog) ListComments(_ context.Context, _ int64, minCommentID int64) ([]hook.BacklogComment, error) {
	f.listedFrom = append(f.listedFrom, minCommentID)
	if f.listErr != nil {
		return nil, f.listErr
	}
	if f.tooLongFromStart && minCommentID == 0 {
		return nil, hook.NewExternalFailure("backlog", hook.FailureRetryable, "comment_window_exhausted")
	}
	result := []hook.BacklogComment{}
	for _, comment := range f.comments {
		if comment.CommentID > minCommentID {
			result = append(result, comment)
		}
	}
	return result, nil
}

func (f *fakeBacklog) ProjectRecentUpdates(_ context.Context, _ int64, minActivityID int64) ([]hook.WebhookHint, error) {
	result := []hook.WebhookHint{}
	for _, hint := range f.activities {
		if hint.ActivityID > minActivityID {
			result = append(result, hint)
		}
	}
	return result, nil
}

func (f *fakeBacklog) post(userID int64, body string) int64 {
	f.nextID++
	f.comments = append(f.comments, hook.BacklogComment{
		CommentID: f.nextID, UserID: userID, Body: body, PostedAt: f.now().UnixMilli(),
	})
	return f.nextID
}

type flowHarness struct {
	store      *DynamoStore
	backlog    *fakeBacklog
	questioner *hook.QuestionReportService
	ticker     *hook.QuestionTickService
	ingest     *ingestStub
	route      hook.ReportRouteConfig
	clock      time.Time
}

func newFlowHarness(t *testing.T, api *memoryDynamo) *flowHarness {
	t.Helper()
	return newFlowHarnessReading(t, api, readingStub{})
}

// newFlowHarnessReading is newFlowHarness with the reading substituted, so a
// test can put the engine in front of a model that makes nothing of what the
// requester wrote.
func newFlowHarnessReading(t *testing.T, api *memoryDynamo, reader hook.AnswerReader) *flowHarness {
	t.Helper()
	store := testStore(t, api)
	route := testTerminalRoute(t)
	logger := slog.New(slog.DiscardHandler)
	harness := &flowHarness{store: store, route: route, clock: testQueuedAt.Add(5 * time.Second)}
	harness.backlog = &fakeBacklog{nextID: 6000, botID: 999, now: func() time.Time { return harness.clock }}
	reporter, err := hook.NewTerminalReportService(route, store, harness.backlog, logger)
	if err != nil {
		t.Fatalf("NewTerminalReportService() error = %v", err)
	}
	questioner, err := hook.NewQuestionReportService(route, store, harness.backlog, logger)
	if err != nil {
		t.Fatalf("NewQuestionReportService() error = %v", err)
	}
	harness.ingest = &ingestStub{}
	ticker, err := hook.NewQuestionTickService(route, store, harness.backlog, reporter, harness.ingest, reader, logger)
	if err != nil {
		t.Fatalf("NewQuestionTickService() error = %v", err)
	}
	harness.questioner = questioner
	harness.ticker = ticker
	token := 0
	clockFn := func() time.Time { return harness.clock.UTC() }
	tokenFn := func() (string, error) {
		token++
		return strings.Repeat(strconv.Itoa(token%10), 32), nil
	}
	questioner.OverrideClock(clockFn, tokenFn)
	ticker.OverrideClock(clockFn, tokenFn)
	reporter.OverrideClock(clockFn, tokenFn)
	return harness
}

func (h *flowHarness) tick(t *testing.T) hook.Result {
	t.Helper()
	return h.ticker.ProcessQuestionTick(context.Background(), hook.QuestionTickRequest{
		Protocol: hook.QuestionTickProtocol, AutomationRunID: h.route.ExpectedRunID, IssuedAt: h.clock.UTC(),
	})
}

func TestQuestionFlowPostsAnswersAndResumesEndToEnd(t *testing.T) {
	api := newMemoryDynamo()
	harness := newFlowHarness(t, api)
	envelope := claimForTerminal(t, harness.store)

	// The clarification decision posts the question exactly once, with the
	// copy-paste lines, and the run starts waiting.
	record := testQuestionRecord(t, envelope)
	report := hook.QuestionReportRequest{Record: record, IssuedAt: harness.clock.UTC()}
	if result := harness.questioner.ProcessQuestionReport(context.Background(), report); result.Code != "question_report_recorded" {
		t.Fatalf("ProcessQuestionReport() = %+v", result)
	}
	if result := harness.questioner.ProcessQuestionReport(context.Background(), report); result.Code != "question_report_already_recorded" {
		t.Fatalf("duplicate ProcessQuestionReport() = %+v", result)
	}
	questionComment := harness.backlog.comments[len(harness.backlog.comments)-1]
	if !strings.Contains(questionComment.Body, "回答 C1 Q1:a") {
		t.Fatalf("question comment lacks the copy-paste line:\n%s", questionComment.Body)
	}

	// An idle tick before any slot does nothing.
	if result := harness.tick(t); result.Code != "question_tick_waiting" {
		t.Fatalf("tick before answers = %+v", result)
	}

	// A comment that says nothing the questions asked about is left alone.
	// The automation used to reply with the format the person should have
	// used, and that courtesy stopped a live delivery when it could not be
	// posted (2026-09-17).
	harness.clock = harness.clock.Add(time.Minute)
	harness.backlog.post(harness.route.AllowedCreatorID, "ありがとうございます、確認します")
	before := len(harness.backlog.comments)
	if result := harness.tick(t); result.Code != "question_tick_waiting" {
		t.Fatalf("tick with an unrelated comment = %+v", result)
	}
	if len(harness.backlog.comments) != before {
		t.Fatalf("the automation answered back: %d comments, was %d", len(harness.backlog.comments), before)
	}

	// The pasted answer resumes the same run with the sealed clarification.
	harness.clock = harness.clock.Add(time.Minute)
	answerID := harness.backlog.post(harness.route.AllowedCreatorID, "回答 C1 Q1:a")
	if result := harness.tick(t); result.Code != "question_tick_resumed" {
		t.Fatalf("tick with valid answer = %+v", result)
	}
	_, runKey := itemKeys(envelope)
	item := api.items[runKey]
	if state, _ := attributeString(item, "state"); state != stateQueued {
		t.Fatalf("state after resume = %s, want %s", state, stateQueued)
	}
	clarificationJSON, _ := attributeString(item, "clarification_json")
	clarification, err := hook.DecodeClarificationRecord([]byte(clarificationJSON))
	if err != nil || len(clarification.Rounds) != 1 || clarification.Rounds[0].AnswerCommentID != answerID {
		t.Fatalf("sealed clarification = %+v, err = %v", clarification, err)
	}
	// A later tick on the resumed (no longer waiting) run is idle.
	if result := harness.tick(t); result.Code != "question_tick_idle" {
		t.Fatalf("tick after resume = %+v", result)
	}

	// Pulling the resumed run delivers the adopted answers with the original
	// ticket, and the combined envelope still validates end to end.
	pull := testPullRequest(t)
	pull.IssuedAt = harness.clock.Add(time.Second)
	pull.ClaimedAt = harness.clock.Add(2 * time.Second)
	pulled, disposition, err := harness.store.Pull(context.Background(), pull)
	if err != nil || disposition != hook.PullAcquired {
		t.Fatalf("Pull() after resume = %s, err = %v", disposition, err)
	}
	if pulled.ClarificationJSON != clarificationJSON {
		t.Fatal("pulled envelope does not carry the sealed clarification")
	}
	if err := hook.ValidateEnvelope(pulled); err != nil {
		t.Fatalf("pulled envelope does not validate: %v", err)
	}
}

func TestQuestionFlowNotifiesOnScheduleAndExpiresAtTheDeadline(t *testing.T) {
	api := newMemoryDynamo()
	harness := newFlowHarness(t, api)
	envelope := claimForTerminal(t, harness.store)
	record := testQuestionRecord(t, envelope)
	report := hook.QuestionReportRequest{Record: record, IssuedAt: harness.clock.UTC()}
	if result := harness.questioner.ProcessQuestionReport(context.Background(), report); result.Code != "question_report_recorded" {
		t.Fatalf("ProcessQuestionReport() = %+v", result)
	}

	// At the second slot (the first was skipped by an outage) exactly one
	// current reminder goes out, and a repeated tick does not send it again.
	harness.clock = time.UnixMilli(record.NotifyAt[1]).Add(time.Minute)
	if result := harness.tick(t); result.Code != "question_tick_notified" {
		t.Fatalf("tick at slot 2 = %+v", result)
	}
	if result := harness.tick(t); result.Code != "question_tick_waiting" {
		t.Fatalf("repeated tick at slot 2 = %+v", result)
	}
	notifyCount := 0
	for _, comment := range harness.backlog.comments {
		if strings.Contains(comment.Body, "再通知") {
			notifyCount++
		}
	}
	if notifyCount != 1 {
		t.Fatalf("reminder comments = %d, want exactly 1", notifyCount)
	}

	// Past the deadline the run expires with the sealed terminal message and
	// no further reminder.
	harness.clock = time.UnixMilli(record.AnswerDeadlineAt).Add(time.Minute)
	if result := harness.tick(t); result.Code != "question_tick_expired" {
		t.Fatalf("tick past deadline = %+v", result)
	}
	_, runKey := itemKeys(envelope)
	if state, _ := attributeString(api.items[runKey], "state"); state != stateTerminal {
		t.Fatalf("state after expiry = %s, want %s", state, stateTerminal)
	}
	if code, _ := attributeString(api.items[runKey], "terminal_code"); code != string(hook.TerminalClarificationExpired) {
		t.Fatalf("terminal code = %s", code)
	}
	if result := harness.tick(t); result.Code != "question_tick_idle" {
		t.Fatalf("tick after expiry = %+v", result)
	}
}

func TestQuestionFlowRecoversAHalfPostedQuestion(t *testing.T) {
	api := newMemoryDynamo()
	harness := newFlowHarness(t, api)
	envelope := claimForTerminal(t, harness.store)

	// The poster sealed the question but died before posting the comment: the
	// requester has seen nothing yet.
	begin := testQuestionBegin(t, envelope, harness.clock.UTC(), strings.Repeat("f", 32))
	if _, disposition, err := harness.store.BeginQuestion(context.Background(), begin); err != nil || disposition != hook.QuestionBeginAcquired {
		t.Fatalf("BeginQuestion() disposition = %s, err = %v", disposition, err)
	}
	// While the poster's lease is still alive the tick stays hands-off on
	// the question (the acceptance notice may post meanwhile).
	if result := harness.tick(t); result.Code != "question_tick_posting_pending" {
		t.Fatalf("tick during live posting lease = %+v", result)
	}
	commentsBefore := len(harness.backlog.comments)
	// After the lease expires the tick finishes the posting itself.
	harness.clock = harness.clock.Add(harness.route.LeaseDuration).Add(time.Second)
	if result := harness.tick(t); result.Code != "question_tick_question_posted" {
		t.Fatalf("tick after poster crash = %+v", result)
	}
	if len(harness.backlog.comments) != commentsBefore+1 ||
		!strings.Contains(harness.backlog.comments[len(harness.backlog.comments)-1].Body, "回答 C1 Q1:a") {
		t.Fatal("recovered posting did not publish the question comment")
	}
	_, runKey := itemKeys(envelope)
	if state, _ := attributeString(api.items[runKey], "state"); state != stateAwaitingAnswer {
		t.Fatalf("state after recovery = %s, want %s", state, stateAwaitingAnswer)
	}
	// The next tick treats the run as a normal wait and posts nothing new.
	if result := harness.tick(t); result.Code != "question_tick_waiting" {
		t.Fatalf("tick after recovery = %+v", result)
	}
	if len(harness.backlog.comments) != commentsBefore+1 {
		t.Fatal("recovery was repeated")
	}
}

func TestQuestionFlowCancelBeatsAnswerInTheSameSnapshot(t *testing.T) {
	api := newMemoryDynamo()
	harness := newFlowHarness(t, api)
	envelope := claimForTerminal(t, harness.store)
	record := testQuestionRecord(t, envelope)
	report := hook.QuestionReportRequest{Record: record, IssuedAt: harness.clock.UTC()}
	if result := harness.questioner.ProcessQuestionReport(context.Background(), report); result.Code != "question_report_recorded" {
		t.Fatalf("ProcessQuestionReport() = %+v", result)
	}
	harness.clock = harness.clock.Add(time.Minute)
	harness.backlog.post(harness.route.AllowedCreatorID, "回答 C1 Q1:a")
	harness.backlog.post(harness.route.AllowedCreatorID, "中止 C1\nよろしくお願いします")
	if result := harness.tick(t); result.Code != "question_tick_cancelled" {
		t.Fatalf("tick with cancel = %+v", result)
	}
	_, runKey := itemKeys(envelope)
	if code, _ := attributeString(api.items[runKey], "terminal_code"); code != string(hook.TerminalCancelled) {
		t.Fatalf("terminal code = %s", code)
	}
}

type ingestStub struct {
	calls    int
	seen     []int64
	failWith hook.Decision
}

func (s *ingestStub) Process(_ context.Context, hint hook.WebhookHint) hook.Result {
	s.calls++
	s.seen = append(s.seen, hint.ActivityID)
	if s.failWith != "" {
		decision := s.failWith
		s.failWith = ""
		return hook.Result{Decision: decision, Code: "queue_failed"}
	}
	return hook.Result{Decision: hook.DecisionAccepted, Code: "queued"}
}

func flowCountMarker(h *flowHarness, marker string) int {
	count := 0
	for _, comment := range h.backlog.comments {
		if strings.Contains(comment.Body, marker) {
			count++
		}
	}
	return count
}

func TestQuestionFlowPostsAcceptanceAndReceiptExactlyOnce(t *testing.T) {
	api := newMemoryDynamo()
	harness := newFlowHarness(t, api)
	envelope := claimForTerminal(t, harness.store)
	ackMarker := hook.CommentMarker("ack", envelope.Snapshot.RunID)

	// The first wake-up after acceptance posts the acknowledgement once.
	if result := harness.tick(t); result.Decision != hook.DecisionAccepted {
		t.Fatalf("tick = %+v", result)
	}
	if flowCountMarker(harness, ackMarker) != 1 {
		t.Fatalf("ack comments = %d, want 1", flowCountMarker(harness, ackMarker))
	}
	harness.clock = harness.clock.Add(time.Minute)
	if result := harness.tick(t); result.Decision != hook.DecisionAccepted {
		t.Fatalf("tick = %+v", result)
	}
	if flowCountMarker(harness, ackMarker) != 1 {
		t.Fatal("acknowledgement was reposted")
	}

	// Question, answer, resume: the receipt names the adopted comment and is
	// posted exactly once across repeated ticks.
	record := testQuestionRecord(t, envelope)
	report := hook.QuestionReportRequest{Record: record, IssuedAt: harness.clock.UTC()}
	if result := harness.questioner.ProcessQuestionReport(context.Background(), report); result.Code != "question_report_recorded" {
		t.Fatalf("ProcessQuestionReport() = %+v", result)
	}
	harness.clock = harness.clock.Add(time.Minute)
	answerID := harness.backlog.post(harness.route.AllowedCreatorID, "回答 C1 Q1:a")
	if result := harness.tick(t); result.Code != "question_tick_resumed" {
		t.Fatalf("tick with answer = %+v", result)
	}
	receiptMarker := hook.CommentMarker("answer-receipt", envelope.Snapshot.RunID, "C1", strconv.FormatInt(answerID, 10))
	if result := harness.tick(t); result.Code != "question_tick_idle" {
		t.Fatalf("tick after resume = %+v", result)
	}
	if flowCountMarker(harness, receiptMarker) != 1 {
		t.Fatalf("receipt comments = %d, want 1", flowCountMarker(harness, receiptMarker))
	}
	found := false
	for _, comment := range harness.backlog.comments {
		if strings.Contains(comment.Body, receiptMarker) && strings.Contains(comment.Body, strconv.FormatInt(answerID, 10)) {
			found = true
		}
	}
	if !found {
		t.Fatal("receipt does not name the adopted answer comment")
	}
	harness.clock = harness.clock.Add(time.Minute)
	if result := harness.tick(t); result.Decision != hook.DecisionAccepted {
		t.Fatalf("tick = %+v", result)
	}
	if flowCountMarker(harness, receiptMarker) != 1 {
		t.Fatal("receipt was reposted")
	}
}

func TestQuestionFlowCompletesALostWebhookAndKeepsScanningWithARun(t *testing.T) {
	api := newMemoryDynamo()
	harness := newFlowHarness(t, api)
	harness.backlog.activities = []hook.WebhookHint{
		{ActivityID: 41, ActivityType: 99, ProjectID: harness.route.ProjectID, ProjectKey: harness.route.ProjectKey, CreatorID: harness.route.AllowedCreatorID, IssueID: 8001, IssueKeyID: 501},
		{ActivityID: 42, ActivityType: harness.route.AllowedActivityType, ProjectID: harness.route.ProjectID, ProjectKey: harness.route.ProjectKey, CreatorID: harness.route.AllowedCreatorID, IssueID: 8001, IssueKeyID: 501},
	}

	// No run exists: the wake-up completes the lost webhook through the same
	// ingest path, and the cursor never re-feeds the same activity.
	if result := harness.tick(t); result.Code != "question_tick_ingested" {
		t.Fatalf("tick without a run = %+v", result)
	}
	if harness.ingest.calls != 1 {
		t.Fatalf("ingest calls = %d, want 1 (type-filtered)", harness.ingest.calls)
	}
	if result := harness.tick(t); result.Code != "question_tick_idle" {
		t.Fatalf("second tick = %+v", result)
	}
	if harness.ingest.calls != 1 {
		t.Fatal("the same activity was fed twice")
	}

	// A run coming alive must NOT stop the completion scan: runs proceed
	// in parallel under the cards orchestration, and a new ticket arriving
	// mid-run still has to be taken in.
	envelope := claimForTerminal(t, harness.store)
	harness.backlog.activities = append(harness.backlog.activities, hook.WebhookHint{
		ActivityID: 43, ActivityType: harness.route.AllowedActivityType, ProjectID: harness.route.ProjectID,
		ProjectKey: harness.route.ProjectKey, CreatorID: harness.route.AllowedCreatorID, IssueID: envelope.Snapshot.IssueID, IssueKeyID: 501,
	})
	if result := harness.tick(t); result.Decision != hook.DecisionAccepted {
		t.Fatalf("tick with active run = %+v", result)
	}
	if harness.ingest.calls != 2 {
		t.Fatalf("ingest calls = %d, want the mid-run activity taken in", harness.ingest.calls)
	}
}

// TestQuestionFlowNeverSkipsATicketWhoseIngestFailed pins the compensation's
// reason for existing: a transient failure while taking in a ticket whose
// webhook was lost must not make that ticket invisible to every later scan.
// Advancing the cursor past it would leave the requester with no
// acknowledgement, no question and no final report, forever.
// TestQuestionFlowIngestsNewTicketsWhileARunIsActive pins the parallel-runs
// contract: an active run must not starve the intake. The old gate ("scan
// only when no run is active") came from the single-run world and left the
// second of two simultaneous tickets unread for as long as the first one
// lived — found live on the board when ticket #2 never appeared.
func TestQuestionFlowIngestsNewTicketsWhileARunIsActive(t *testing.T) {
	api := newMemoryDynamo()
	harness := newFlowHarness(t, api)
	claimForTerminal(t, harness.store) // an active run: its notice exists
	harness.backlog.activities = []hook.WebhookHint{
		{ActivityID: 50, ActivityType: harness.route.AllowedActivityType, ProjectID: harness.route.ProjectID, ProjectKey: harness.route.ProjectKey, CreatorID: harness.route.AllowedCreatorID, IssueID: 9000, IssueKeyID: 600},
	}
	result := harness.tick(t)
	if len(harness.ingest.seen) != 1 || harness.ingest.seen[0] != 50 {
		t.Fatalf("processed = %v (tick=%+v), want the new ticket ingested while the run is active", harness.ingest.seen, result)
	}
	// And the settled state stays quiet — no re-feeding.
	if result := harness.tick(t); result.Code != "question_tick_idle" {
		t.Fatalf("settled tick = %+v", result)
	}
	if len(harness.ingest.seen) != 1 {
		t.Fatalf("processed = %v, want no duplicates", harness.ingest.seen)
	}
}

func TestQuestionFlowNeverSkipsATicketWhoseIngestFailed(t *testing.T) {
	api := newMemoryDynamo()
	harness := newFlowHarness(t, api)
	harness.backlog.activities = []hook.WebhookHint{
		{ActivityID: 40, ActivityType: 99, ProjectID: harness.route.ProjectID, ProjectKey: harness.route.ProjectKey, CreatorID: harness.route.AllowedCreatorID, IssueID: 8000, IssueKeyID: 500},
		{ActivityID: 41, ActivityType: harness.route.AllowedActivityType, ProjectID: harness.route.ProjectID, ProjectKey: harness.route.ProjectKey, CreatorID: harness.route.AllowedCreatorID, IssueID: 8001, IssueKeyID: 501},
		{ActivityID: 42, ActivityType: harness.route.AllowedActivityType, ProjectID: harness.route.ProjectID, ProjectKey: harness.route.ProjectKey, CreatorID: harness.route.AllowedCreatorID, IssueID: 8002, IssueKeyID: 502},
	}
	harness.ingest.failWith = hook.DecisionRetryRequested

	// The first scan hits the transient failure on 41 and reads 42 anyway.
	// Stopping at 41 meant a ticket waiting its turn kept every ticket
	// filed after it from being read at all - measured live: a ticket filed
	// three minutes after a stuck one produced not one line of log for as
	// long as the stall lasted (完遂率を最優先、発注者指示 2026-09-17).
	if result := harness.tick(t); result.Code != "question_tick_ingest_incomplete" || result.Decision != hook.DecisionRetryRequested {
		t.Fatalf("tick with a failing ingest = %+v", result)
	}
	if len(harness.ingest.seen) != 2 || harness.ingest.seen[0] != 41 || harness.ingest.seen[1] != 42 {
		t.Fatalf("processed = %v, want 41 then 42", harness.ingest.seen)
	}

	// And the cursor did not move past 41, so the next scan starts there.
	if result := harness.tick(t); result.Code != "question_tick_ingested" {
		t.Fatalf("retry tick = %+v", result)
	}
	if len(harness.ingest.seen) != 4 || harness.ingest.seen[2] != 41 || harness.ingest.seen[3] != 42 {
		t.Fatalf("processed = %v, want 41 retried then 42", harness.ingest.seen)
	}
	if result := harness.tick(t); result.Code != "question_tick_idle" {
		t.Fatalf("settled tick = %+v", result)
	}
	if len(harness.ingest.seen) != 4 {
		t.Fatalf("processed = %v, want no re-feeding once settled", harness.ingest.seen)
	}
}

// TestQuestionFlowExpiresEvenWhileANoticeIsStuck pins that an unpostable
// notice cannot hold the run past its deadline: the requester must still be
// told the automation stopped.
func TestQuestionFlowExpiresEvenWhileANoticeIsStuck(t *testing.T) {
	api := newMemoryDynamo()
	harness := newFlowHarness(t, api)
	envelope := claimForTerminal(t, harness.store)
	record := testQuestionRecord(t, envelope)
	report := hook.QuestionReportRequest{Record: record, IssuedAt: harness.clock.UTC()}
	if result := harness.questioner.ProcessQuestionReport(context.Background(), report); result.Code != "question_report_recorded" {
		t.Fatalf("ProcessQuestionReport() = %+v", result)
	}
	// Hold the acknowledgement marker under a live lease owned by nobody
	// else, so the tick always finds it busy.
	_, runKey := itemKeys(envelope)
	api.items[runKey+"#comment#ack"] = map[string]types.AttributeValue{
		"pk":                stringValue(runKey + "#comment#ack"),
		"record_type":       stringValue("run_comment"),
		"run_key":           stringValue(runKey),
		"reply_kind":        stringValue(string(hook.RunCommentAck)),
		"content_sha256":    stringValue(strings.Repeat("d", 64)),
		"reply_started_at":  numberValue(record.AnswerDeadlineAt),
		"reply_lease_until": numberValue(record.AnswerDeadlineAt + 4*time.Hour.Milliseconds()),
		"reply_lease_token": stringValue(strings.Repeat("e", 32)),
	}

	harness.clock = time.UnixMilli(record.AnswerDeadlineAt).Add(time.Minute)
	if result := harness.tick(t); result.Code != "question_tick_expired" {
		t.Fatalf("tick past the deadline with a stuck notice = %+v", result)
	}
	if state, _ := attributeString(api.items[runKey], "state"); state != stateTerminal {
		t.Fatalf("state = %s, want %s", state, stateTerminal)
	}
}

// A ticket waiting its turn is not passed over because something else
// happened afterwards. The scan reads only what is newer than its cursor,
// so a cursor carried past a waiting ticket loses it for good: filed,
// acknowledged nowhere, never started. Measured live - a comment on
// another issue moved the cursor past a ticket that had been waiting
// twenty-seven minutes for the delivery before it (完遂率を最優先、発注者
// 指示 2026-09-17).
func TestAWaitingTicketIsNotPassedOverByWhatComesAfterIt(t *testing.T) {
	api := newMemoryDynamo()
	harness := newFlowHarness(t, api)
	harness.backlog.activities = []hook.WebhookHint{
		// Ours, and waiting for the delivery before it.
		{ActivityID: 41, ActivityType: harness.route.AllowedActivityType, ProjectID: harness.route.ProjectID, ProjectKey: harness.route.ProjectKey, CreatorID: harness.route.AllowedCreatorID, IssueID: 8001, IssueKeyID: 501},
		// Not ours: a comment on another issue, filed afterwards.
		{ActivityID: 99, ActivityType: 99, ProjectID: harness.route.ProjectID, ProjectKey: harness.route.ProjectKey, CreatorID: harness.route.AllowedCreatorID, IssueID: 8009, IssueKeyID: 509},
	}
	harness.ingest.failWith = hook.DecisionRetryRequested

	if result := harness.tick(t); result.Code != "question_tick_ingest_incomplete" {
		t.Fatalf("tick = %+v", result)
	}
	// The next scan must read 41 again. If the cursor moved past it, the
	// ticket is gone.
	harness.ingest.failWith = ""
	if result := harness.tick(t); result.Code != "question_tick_ingested" {
		t.Fatalf("retry tick = %+v", result)
	}
	seen := 0
	for _, id := range harness.ingest.seen {
		if id == 41 {
			seen++
		}
	}
	if seen < 2 {
		t.Fatalf("the waiting ticket was read %d time(s); the cursor was carried past it: %v", seen, harness.ingest.seen)
	}
}

// readingStub stands in for the model that reads a requester's comment. It
// is deliberately crude: these tests are about the flow around a reading,
// not about the reading.
type readingStub struct{}

var stubAnswerPair = regexp.MustCompile(`[Qq]([0-9]+)[ 	]*:[ 	]*([A-Za-z0-9]+)`)

func (readingStub) ReadAnswer(_ context.Context, _, body string) (hook.AnswerReading, error) {
	if strings.Contains(body, "中止") {
		return hook.AnswerReading{Kind: hook.AnswerReadingCancel}, nil
	}
	answers := map[string]string{}
	for _, match := range stubAnswerPair.FindAllStringSubmatch(body, -1) {
		answers["Q"+match[1]] = match[2]
	}
	if len(answers) == 0 {
		return hook.AnswerReading{Kind: hook.AnswerReadingUnrelated}, nil
	}
	return hook.AnswerReading{Kind: hook.AnswerReadingAnswer, Answers: answers}, nil
}

// flowCommentWithMarker returns the body of the comment carrying the marker
// and how many comments carry it.
func flowCommentWithMarker(h *flowHarness, marker string) (string, int) {
	body, count := "", 0
	for _, comment := range h.backlog.comments {
		if strings.Contains(comment.Body, marker) {
			body, count = comment.Body, count+1
		}
	}
	return body, count
}

// assertAckFrame holds every acceptance notice to the lines the reception's
// state does not move, whichever reading it got.
func assertAckFrame(t *testing.T, body, marker string) {
	t.Helper()
	for _, line := range []string{
		"処理の所有者: 自動処理（結果はこのチケットのコメントでお知らせします）",
		"最終報告の目安: 受付から 2 時間以内（質問への回答待ちの期間は除きます）",
		"状態: 受付済み・自動処理中",
		"次回通知・期限: 最終結果または確認事項を、受付から 2 時間以内を目安に通知",
		"本番の状態: 未変更",
		"自動再試行: なし（webhook 未達時は 5 分周期の照合で受付を補完）",
	} {
		if !strings.Contains(body, "\n"+line+"\n") {
			t.Fatalf("the acceptance notice lost the line %q:\n%s", line, body)
		}
	}
	if !strings.HasPrefix(body, "【受付】このチケットの自動処理を受け付けました。\n") {
		t.Fatalf("the acceptance notice does not open with the headline:\n%s", body)
	}
	if got := hook.ExtractCommentMarker(body); got != marker {
		t.Fatalf("marker line = %q, want %q", got, marker)
	}
}

const (
	ackPendingRequest    = "ご対応のお願い: いまは何もありません。受付の確認が終わるまでお待ちください。依頼者にしか決められない点があれば、受付の時点でまとめて質問します。受付の質問は設定で許された回数（既定は 1 回）までで、それ以降は質問しません。"
	ackProceededRequest  = "ご対応のお願い: ありません。受付の確認は完了しており、受付からの質問はこれ以上ありません。方針が違う場合は、方針コメントにある停止の方法をご利用ください。"
	ackQuestionedRequest = "ご対応のお願い: 上の質問への回答だけです。受付の質問は設定で許された回数（既定は 1 回）までで、それ以降は質問しません。"
)

// A run is in flight from the claim, and the reception happens some way
// after it: a budget hold, a sign-in hold, a target token that cannot be
// read or a restart all leave a claimed run whose reception has not begun,
// and the claim then goes back to the queue for the reception to run later.
// The acceptance notice is posted on the first wake-up that finds the run in
// flight — so it can precede the decision — and it is posted once per run and
// never revised. A notice that told such a requester nothing would be asked
// would be contradicted by the question that follows, with nothing left to
// correct it.
func TestAcceptanceNoticeBeforeTheReceptionDecidesPromisesNoSilence(t *testing.T) {
	api := newMemoryDynamo()
	harness := newFlowHarness(t, api)
	envelope := claimForTerminal(t, harness.store)
	marker := hook.CommentMarker("ack", envelope.Snapshot.RunID)

	// Claimed, held, and not yet received: one notice pass.
	if result := harness.tick(t); result.Decision != hook.DecisionAccepted {
		t.Fatalf("tick = %+v", result)
	}
	body, count := flowCommentWithMarker(harness, marker)
	if count != 1 {
		t.Fatalf("acceptance notices = %d, want 1", count)
	}
	if !strings.Contains(body, "\n"+ackPendingRequest+"\n") {
		t.Fatalf("the notice does not say the reception is unfinished:\n%s", body)
	}
	if strings.Contains(body, "これ以上ありません") || strings.Contains(body, "上の質問") {
		t.Fatalf("the notice settled a reception that had not happened:\n%s", body)
	}
	assertAckFrame(t, body, marker)

	// The reception then runs and asks. The notice already on the ticket must
	// not contradict it, and nothing reposts or rewrites it.
	harness.clock = harness.clock.Add(time.Minute)
	record := testQuestionRecord(t, envelope)
	if result := harness.questioner.ProcessQuestionReport(context.Background(),
		hook.QuestionReportRequest{Record: record, IssuedAt: harness.clock.UTC()}); result.Code != "question_report_recorded" {
		t.Fatalf("ProcessQuestionReport() = %+v", result)
	}
	harness.clock = harness.clock.Add(time.Minute)
	if result := harness.tick(t); result.Code != "question_tick_waiting" {
		t.Fatalf("tick while waiting = %+v", result)
	}
	after, count := flowCommentWithMarker(harness, marker)
	if count != 1 || after != body {
		t.Fatalf("the acceptance notice changed: count=%d\n%s", count, after)
	}
	if flowCountMarker(harness, hook.CommentMarker("question", envelope.Snapshot.RunID, "C1")) != 1 {
		t.Fatal("the reception's question is not on the ticket")
	}
}

// The reception concluded and went on: its plan notice is the run's own
// record of that, and only then does the acceptance notice say the asking is
// over and point at the comment the stop method is written in.
func TestAcceptanceNoticeAfterThePlanNoticeSaysTheAskingIsOver(t *testing.T) {
	api := newMemoryDynamo()
	harness := newFlowHarness(t, api)
	envelope := claimForTerminal(t, harness.store)
	marker := hook.CommentMarker("ack", envelope.Snapshot.RunID)

	if posted, reason := harness.ticker.PostPlanComment(context.Background(), envelope.DeliveryID,
		hook.PlanCommentContent(envelope.Snapshot.RunID, hook.PlanFacts{Request: "一覧の取得失敗時に再試行の導線を出す"})); !posted {
		t.Fatalf("plan notice not posted: %s", reason)
	}
	harness.clock = harness.clock.Add(time.Minute)
	if result := harness.tick(t); result.Decision != hook.DecisionAccepted {
		t.Fatalf("tick = %+v", result)
	}
	body, count := flowCommentWithMarker(harness, marker)
	if count != 1 {
		t.Fatalf("acceptance notices = %d, want 1", count)
	}
	if !strings.Contains(body, "\n"+ackProceededRequest+"\n") {
		t.Fatalf("the notice does not close the asking:\n%s", body)
	}
	if !strings.Contains(body, "\n次に行動する人: 自動処理\n") || !strings.Contains(body, "\n操作: 利用者操作なし\n") {
		t.Fatalf("the footer asks for something nothing needs:\n%s", body)
	}
	assertAckFrame(t, body, marker)
}

// The reception asked: the notice names the answer that is owed and its
// footer sends the requester to the same place as the question comment.
func TestAcceptanceNoticeDuringAnAnswerWaitNamesTheAnswer(t *testing.T) {
	api := newMemoryDynamo()
	harness := newFlowHarness(t, api)
	envelope := claimForTerminal(t, harness.store)
	marker := hook.CommentMarker("ack", envelope.Snapshot.RunID)

	record := testQuestionRecord(t, envelope)
	if result := harness.questioner.ProcessQuestionReport(context.Background(),
		hook.QuestionReportRequest{Record: record, IssuedAt: harness.clock.UTC()}); result.Code != "question_report_recorded" {
		t.Fatalf("ProcessQuestionReport() = %+v", result)
	}
	harness.clock = harness.clock.Add(time.Minute)
	if result := harness.tick(t); result.Code != "question_tick_waiting" {
		t.Fatalf("tick while waiting = %+v", result)
	}
	body, count := flowCommentWithMarker(harness, marker)
	if count != 1 {
		t.Fatalf("acceptance notices = %d, want 1", count)
	}
	if !strings.Contains(body, "\n"+ackQuestionedRequest+"\n") {
		t.Fatalf("the notice does not name the open question:\n%s", body)
	}
	if strings.Contains(body, "操作: 利用者操作なし") {
		t.Fatalf("the footer contradicts the open question:\n%s", body)
	}
	if !strings.Contains(body, "\n次に行動する人: 起票者（回答者）\n") {
		t.Fatalf("the footer does not name the answerer:\n%s", body)
	}
	assertAckFrame(t, body, marker)
}
