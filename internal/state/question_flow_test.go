package state

import (
	"context"
	"log/slog"
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
}

func (f *fakeBacklog) FindExactComment(_ context.Context, _ int64, content string) (int64, bool, error) {
	for _, comment := range f.comments {
		if comment.Body == content {
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
	ticker, err := hook.NewQuestionTickService(route, store, harness.backlog, reporter, harness.ingest, logger)
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

	// A malformed answer earns the one-time guidance; the next tick does not
	// repeat it.
	harness.clock = harness.clock.Add(time.Minute)
	harness.backlog.post(harness.route.AllowedCreatorID, "回答 C1 Q1:z")
	if result := harness.tick(t); result.Code != "question_tick_replied" {
		t.Fatalf("tick with malformed answer = %+v", result)
	}
	guidanceCount := 0
	for _, comment := range harness.backlog.comments {
		if strings.Contains(comment.Body, "回答書式のご案内") {
			guidanceCount++
		}
	}
	if guidanceCount != 1 {
		t.Fatalf("guidance comments = %d, want 1", guidanceCount)
	}
	if result := harness.tick(t); result.Code != "question_tick_waiting" {
		t.Fatalf("tick after guidance = %+v", result)
	}
	for _, comment := range harness.backlog.comments {
		if strings.Count(comment.Body, "回答書式のご案内") > 1 {
			t.Fatal("guidance was repeated")
		}
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

func TestQuestionFlowCompletesALostWebhookOnlyWhenNoRunExists(t *testing.T) {
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

	// Once a run exists, the completion scan stops.
	envelope := claimForTerminal(t, harness.store)
	harness.backlog.activities = append(harness.backlog.activities, hook.WebhookHint{
		ActivityID: 43, ActivityType: harness.route.AllowedActivityType, ProjectID: harness.route.ProjectID,
		ProjectKey: harness.route.ProjectKey, CreatorID: harness.route.AllowedCreatorID, IssueID: envelope.Snapshot.IssueID, IssueKeyID: 501,
	})
	if result := harness.tick(t); result.Decision != hook.DecisionAccepted {
		t.Fatalf("tick with active run = %+v", result)
	}
	if harness.ingest.calls != 1 {
		t.Fatal("the completion scan ran while a run was active")
	}
}

// TestQuestionFlowNeverSkipsATicketWhoseIngestFailed pins the compensation's
// reason for existing: a transient failure while taking in a ticket whose
// webhook was lost must not make that ticket invisible to every later scan.
// Advancing the cursor past it would leave the requester with no
// acknowledgement, no question and no final report, forever.
func TestQuestionFlowNeverSkipsATicketWhoseIngestFailed(t *testing.T) {
	api := newMemoryDynamo()
	harness := newFlowHarness(t, api)
	harness.backlog.activities = []hook.WebhookHint{
		{ActivityID: 40, ActivityType: 99, ProjectID: harness.route.ProjectID, ProjectKey: harness.route.ProjectKey, CreatorID: harness.route.AllowedCreatorID, IssueID: 8000, IssueKeyID: 500},
		{ActivityID: 41, ActivityType: harness.route.AllowedActivityType, ProjectID: harness.route.ProjectID, ProjectKey: harness.route.ProjectKey, CreatorID: harness.route.AllowedCreatorID, IssueID: 8001, IssueKeyID: 501},
		{ActivityID: 42, ActivityType: harness.route.AllowedActivityType, ProjectID: harness.route.ProjectID, ProjectKey: harness.route.ProjectKey, CreatorID: harness.route.AllowedCreatorID, IssueID: 8002, IssueKeyID: 502},
	}
	harness.ingest.failWith = hook.DecisionRetryRequested

	// The first scan hits the transient failure on 41 and stops there.
	if result := harness.tick(t); result.Code != "question_tick_ingest_incomplete" || result.Decision != hook.DecisionRetryRequested {
		t.Fatalf("tick with a failing ingest = %+v", result)
	}
	if len(harness.ingest.seen) != 1 || harness.ingest.seen[0] != 41 {
		t.Fatalf("processed = %v, want only 41", harness.ingest.seen)
	}

	// The next scan retries the very ticket that failed, then continues.
	if result := harness.tick(t); result.Code != "question_tick_ingested" {
		t.Fatalf("retry tick = %+v", result)
	}
	if len(harness.ingest.seen) != 3 || harness.ingest.seen[1] != 41 || harness.ingest.seen[2] != 42 {
		t.Fatalf("processed = %v, want 41 retried then 42", harness.ingest.seen)
	}
	if result := harness.tick(t); result.Code != "question_tick_idle" {
		t.Fatalf("settled tick = %+v", result)
	}
	if len(harness.ingest.seen) != 3 {
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
