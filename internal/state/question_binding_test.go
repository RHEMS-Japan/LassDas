package state

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// silentReading is a model that makes nothing of anything. It stands for the
// model being unreachable, or simply not recognizing what a person wrote:
// what the question comment itself prescribes must work without it.
type silentReading struct{}

func (silentReading) ReadAnswer(_ context.Context, _, _ string) (hook.AnswerReading, error) {
	return hook.AnswerReading{Kind: hook.AnswerReadingUnrelated}, nil
}

// countingReading records what the model was asked to read. A comment that
// cannot be this question's is not worth a model call, and counting is how
// a test sees that it was never made.
type countingReading struct{ bodies []string }

func (c *countingReading) ReadAnswer(ctx context.Context, questions, body string) (hook.AnswerReading, error) {
	c.bodies = append(c.bodies, body)
	return readingStub{}.ReadAnswer(ctx, questions, body)
}

// askSecondRound answers the open question, lets the run resume, claims it
// again and opens a second question, without a tick in between - the
// sequence a run really goes through, and the one that put a closed
// ticket's old answer in front of a requester who had just asked to stop
// (live 2026-09-25).
func askSecondRound(t *testing.T, harness *flowHarness, envelope hook.DispatchEnvelope) (hook.QuestionRecord, hook.BacklogComment) {
	t.Helper()
	harness.clock = harness.clock.Add(time.Minute)
	harness.backlog.post(harness.route.AllowedCreatorID, "回答 C1 Q1:a")
	if result := harness.tick(t); result.Code != "question_tick_resumed" {
		t.Fatalf("the first answer did not resume the run: %+v", result)
	}
	pull := testPullRequest(t)
	harness.clock = harness.clock.Add(time.Minute)
	pull.IssuedAt, pull.ClaimedAt = harness.clock, harness.clock.Add(time.Second)
	pulled, disposition, err := harness.store.Pull(context.Background(), pull)
	if err != nil || disposition != hook.PullAcquired {
		t.Fatalf("Pull() after resume = %s, err = %v", disposition, err)
	}
	second := testQuestionRecord(t, envelope)
	second.QuestionRevision = 2
	second.ClarificationSHA256 = hook.TerminalReportDigest([]byte(pulled.ClarificationJSON))
	harness.clock = harness.clock.Add(time.Minute)
	report := hook.QuestionReportRequest{Record: second, IssuedAt: harness.clock.UTC()}
	if result := harness.questioner.ProcessQuestionReport(context.Background(), report); result.Code != "question_report_recorded" {
		t.Fatalf("the second question was not posted: %+v", result)
	}
	return second, harness.backlog.comments[len(harness.backlog.comments)-1]
}

// An answer belongs to the question it was written for. The run that is
// waiting on C2 must not be resumed by a comment written for C1 - it is on
// the same ticket, by the same requester, and reads as an answer, which is
// exactly why it was taken and announced while the open question went
// unanswered (live 2026-09-25).
func TestAnAnswerForAnEarlierRoundNeverAnswersTheOpenQuestion(t *testing.T) {
	api := newMemoryDynamo()
	reader := &countingReading{}
	harness := newFlowHarnessReading(t, api, reader)
	envelope := claimForTerminal(t, harness.store)
	first := testQuestionRecord(t, envelope)
	if result := harness.questioner.ProcessQuestionReport(context.Background(),
		hook.QuestionReportRequest{Record: first, IssuedAt: harness.clock.UTC()}); result.Code != "question_report_recorded" {
		t.Fatalf("ProcessQuestionReport() = %+v", result)
	}
	firstQuestion := harness.backlog.comments[len(harness.backlog.comments)-1]
	_, secondQuestion := askSecondRound(t, harness, envelope)
	reader.bodies = nil

	// Written now, but for the round that is already over.
	harness.clock = harness.clock.Add(time.Minute)
	harness.backlog.post(harness.route.AllowedCreatorID, "回答 C1 Q1:b")
	if result := harness.tick(t); result.Code != "question_tick_waiting" {
		t.Fatalf("an answer for C1 was taken while C2 was open: %+v", result)
	}

	// Written after the first question and before the second, and only
	// numbered after both. Ids say it is newer than the open question; the
	// clock says it was written when nobody had seen that question yet, so
	// it cannot be its answer - and the instant it is held to has to be the
	// open question's, not the one before it.
	posted := harness.clock
	between := time.UnixMilli((firstQuestion.PostedAt + secondQuestion.PostedAt) / 2)
	if !between.After(time.UnixMilli(firstQuestion.PostedAt)) || !between.Before(time.UnixMilli(secondQuestion.PostedAt)) {
		t.Fatal("the two questions are not far enough apart for the test to say anything")
	}
	harness.clock = between
	staleID := harness.backlog.post(harness.route.AllowedCreatorID, "Q1: b")
	harness.clock = posted
	if staleID <= secondQuestion.CommentID {
		t.Fatalf("the stale comment id %d is not after the question's %d; the test proves nothing", staleID, secondQuestion.CommentID)
	}
	if result := harness.tick(t); result.Code != "question_tick_waiting" {
		t.Fatalf("a comment older than the question was taken as its answer: %+v", result)
	}
	_, runKey := itemKeys(envelope)
	if state, _ := attributeString(api.items[runKey], "state"); state != stateAwaitingAnswer {
		t.Fatalf("state = %s, want the run still waiting on its open question", state)
	}

	// Neither comment was worth a model call: what round a comment names and
	// when it was written are this engine's to decide, and deciding them
	// first is what keeps the reading from being asked a question it has no
	// way to answer.
	for _, body := range reader.bodies {
		if strings.Contains(body, "回答 C1") || body == "Q1: b" {
			t.Fatalf("the model was asked to read a comment that cannot be this question's: %q", body)
		}
	}

	// The answer to the question that is open resumes the run.
	harness.clock = harness.clock.Add(time.Minute)
	harness.backlog.post(harness.route.AllowedCreatorID, "回答 C2 Q1:b")
	if result := harness.tick(t); result.Code != "question_tick_resumed" {
		t.Fatalf("the answer to the open question did not resume the run: %+v", result)
	}
	if len(reader.bodies) == 0 {
		t.Fatal("the model was never asked to read anything at all; the check above proves nothing")
	}
}

// The question comment tells the requester to write 「中止 C1」 to stop. That
// phrase is the engine's own, so it ends the run whether or not a model is
// there to read it - a phrase the automation asks for and then ignores is
// worse than never offering it (live 2026-09-25).
func TestThePrescribedCancelEndsTheRunWithNoModelReadingIt(t *testing.T) {
	api := newMemoryDynamo()
	harness := newFlowHarnessReading(t, api, silentReading{})
	envelope := claimForTerminal(t, harness.store)
	record := testQuestionRecord(t, envelope)
	if result := harness.questioner.ProcessQuestionReport(context.Background(),
		hook.QuestionReportRequest{Record: record, IssuedAt: harness.clock.UTC()}); result.Code != "question_report_recorded" {
		t.Fatalf("ProcessQuestionReport() = %+v", result)
	}
	harness.clock = harness.clock.Add(time.Minute)
	harness.backlog.post(harness.route.AllowedCreatorID, "中止 C1")
	if result := harness.tick(t); result.Code != "question_tick_cancelled" {
		t.Fatalf("the prescribed cancellation was ignored: %+v", result)
	}
	_, runKey := itemKeys(envelope)
	if state, _ := attributeString(api.items[runKey], "state"); state == stateAwaitingAnswer {
		t.Fatal("the run is still waiting for an answer after the requester cancelled")
	}
	// A cancellation naming a round that is not open is not this question's.
	other := newFlowHarnessReading(t, newMemoryDynamo(), silentReading{})
	otherEnvelope := claimForTerminal(t, other.store)
	otherRecord := testQuestionRecord(t, otherEnvelope)
	if result := other.questioner.ProcessQuestionReport(context.Background(),
		hook.QuestionReportRequest{Record: otherRecord, IssuedAt: other.clock.UTC()}); result.Code != "question_report_recorded" {
		t.Fatalf("ProcessQuestionReport() = %+v", result)
	}
	other.clock = other.clock.Add(time.Minute)
	other.backlog.post(other.route.AllowedCreatorID, "中止 C2")
	if result := other.tick(t); result.Code != "question_tick_waiting" {
		t.Fatalf("a cancellation for another round ended this one: %+v", result)
	}
}

// 「停止」 stops the run wherever it sits, and the answer wait is no
// exception. A stop written in the seconds before the question was posted
// used to be invisible to the wait, which listed only what came after the
// question: the requester had said stop, the automation asked anyway, and
// the ticket sat waiting (live 2026-09-25).
func TestAStopWrittenBeforeTheQuestionStillEndsTheRun(t *testing.T) {
	api := newMemoryDynamo()
	harness := newFlowHarnessReading(t, api, silentReading{})
	envelope := claimForTerminal(t, harness.store)
	harness.backlog.post(harness.route.AllowedCreatorID, "停止")
	harness.clock = harness.clock.Add(time.Second)
	record := testQuestionRecord(t, envelope)
	if result := harness.questioner.ProcessQuestionReport(context.Background(),
		hook.QuestionReportRequest{Record: record, IssuedAt: harness.clock.UTC()}); result.Code != "question_report_recorded" {
		t.Fatalf("ProcessQuestionReport() = %+v", result)
	}
	harness.clock = harness.clock.Add(time.Minute)
	if result := harness.tick(t); result.Code != "question_tick_stopped" {
		t.Fatalf("a stop written before the question was never read: %+v", result)
	}
	_, runKey := itemKeys(envelope)
	if state, _ := attributeString(api.items[runKey], "state"); state == stateAwaitingAnswer {
		t.Fatal("the run is still waiting for an answer after the requester stopped it")
	}
	// The word inside an ordinary sentence is not a stop, and neither is
	// anyone else's comment.
	quiet := newFlowHarnessReading(t, newMemoryDynamo(), silentReading{})
	quietEnvelope := claimForTerminal(t, quiet.store)
	quiet.backlog.post(quiet.route.AllowedCreatorID, "レビュー後に停止も検討します")
	quiet.backlog.post(quiet.route.AllowedCreatorID+1, "停止")
	quiet.clock = quiet.clock.Add(time.Second)
	if result := quiet.questioner.ProcessQuestionReport(context.Background(),
		hook.QuestionReportRequest{Record: testQuestionRecord(t, quietEnvelope), IssuedAt: quiet.clock.UTC()}); result.Code != "question_report_recorded" {
		t.Fatalf("ProcessQuestionReport() = %+v", result)
	}
	quiet.clock = quiet.clock.Add(time.Minute)
	if result := quiet.tick(t); result.Code != "question_tick_waiting" {
		t.Fatalf("a mention of the word, or someone else's comment, stopped the run: %+v", result)
	}
}

// The receipt says an answer was taken and the run went on with it. While a
// question is open that is not true, and a requester reading it looks away
// from a question that is waiting on them (live 2026-09-25: a receipt for
// the previous day's answer, posted while the new run waited on C2).
func TestNoReceiptClaimsTheRunResumedWhileItIsWaiting(t *testing.T) {
	api := newMemoryDynamo()
	harness := newFlowHarness(t, api)
	envelope := claimForTerminal(t, harness.store)
	first := testQuestionRecord(t, envelope)
	if result := harness.questioner.ProcessQuestionReport(context.Background(),
		hook.QuestionReportRequest{Record: first, IssuedAt: harness.clock.UTC()}); result.Code != "question_report_recorded" {
		t.Fatalf("ProcessQuestionReport() = %+v", result)
	}
	askSecondRound(t, harness, envelope)
	harness.clock = harness.clock.Add(time.Minute)
	if result := harness.tick(t); result.Code != "question_tick_waiting" {
		t.Fatalf("tick with the second question open = %+v", result)
	}
	if body, found := harnessComment(harness, "【回答受領"); found {
		t.Fatalf("a receipt claimed the run resumed while it waits for an answer:\n%s", body)
	}

	// Once the run really has resumed, the receipt is owed and posted: the
	// rule above suppresses a false claim, not the notice itself.
	harness.clock = harness.clock.Add(time.Minute)
	harness.backlog.post(harness.route.AllowedCreatorID, "回答 C2 Q1:b")
	if result := harness.tick(t); result.Code != "question_tick_resumed" {
		t.Fatalf("the answer to the open question did not resume the run: %+v", result)
	}
	harness.clock = harness.clock.Add(time.Minute)
	harness.tick(t)
	if _, found := harnessComment(harness, "【回答受領"); !found {
		t.Fatal("the receipt was never posted after the run resumed")
	}
}

// A run that was handed someone else's answers as input did not receive
// them. Announcing them announces an event that belongs to another run, and
// the per-run marker on the comment cannot see the one that run already
// posted - which is how a ticket closed the day before was re-received and
// answered its requester with a receipt for yesterday (live 2026-09-25).
func TestNoReceiptAnnouncesAnotherRunsAnswer(t *testing.T) {
	api := newMemoryDynamo()
	harness := newFlowHarness(t, api)
	envelope := claimForTerminal(t, harness.store)
	record := testQuestionRecord(t, envelope)
	if result := harness.questioner.ProcessQuestionReport(context.Background(),
		hook.QuestionReportRequest{Record: record, IssuedAt: harness.clock.UTC()}); result.Code != "question_report_recorded" {
		t.Fatalf("ProcessQuestionReport() = %+v", result)
	}
	harness.clock = harness.clock.Add(time.Minute)
	harness.backlog.post(harness.route.AllowedCreatorID, "回答 C1 Q1:a")
	if result := harness.tick(t); result.Code != "question_tick_resumed" {
		t.Fatalf("tick with a valid answer = %+v", result)
	}
	_, runKey := itemKeys(envelope)
	rewriteClarificationRunID(t, api.items[runKey], "run_20260101_ffffffffffffffff00000000")

	harness.clock = harness.clock.Add(time.Minute)
	harness.tick(t)
	if body, found := harnessComment(harness, "【回答受領"); found {
		t.Fatalf("a receipt announced an answer another run was given:\n%s", body)
	}
}

// harnessComment finds the first posted comment containing text.
func harnessComment(harness *flowHarness, text string) (string, bool) {
	for _, comment := range harness.backlog.comments {
		if strings.Contains(comment.Body, text) {
			return comment.Body, true
		}
	}
	return "", false
}

// rewriteClarificationRunID reseals the row's clarification under a
// different run, the way a re-received ticket carries the answers an earlier
// run was given.
func rewriteClarificationRunID(t *testing.T, item map[string]types.AttributeValue, runID string) {
	t.Helper()
	encoded, ok := attributeString(item, "clarification_json")
	if !ok {
		t.Fatal("the resumed run carries no clarification to reseal")
	}
	record, err := hook.DecodeClarificationRecord([]byte(encoded))
	if err != nil {
		t.Fatalf("DecodeClarificationRecord() error = %v", err)
	}
	record.AutomationRunID = runID
	for index, round := range record.Rounds {
		question, err := hook.DecodeQuestionRecord([]byte(round.QuestionRecordJSON))
		if err != nil {
			t.Fatalf("DecodeQuestionRecord() error = %v", err)
		}
		question.AutomationRunID = runID
		questionJSON, err := hook.MarshalQuestionRecord(question)
		if err != nil {
			t.Fatalf("MarshalQuestionRecord() error = %v", err)
		}
		record.Rounds[index].QuestionRecordJSON = string(questionJSON)
		record.Rounds[index].QuestionRecordSHA256 = hook.TerminalReportDigest(questionJSON)
	}
	resealed, err := hook.MarshalClarificationRecord(record)
	if err != nil {
		t.Fatalf("MarshalClarificationRecord() error = %v", err)
	}
	item["clarification_json"] = &types.AttributeValueMemberS{Value: string(resealed)}
	item["clarification_sha256"] = &types.AttributeValueMemberS{Value: hook.TerminalReportDigest(resealed)}
}

// Reading the thread whole is what lets a stop written before the question
// be heard, and nothing can be added before the question once it is out. So
// the wait reads that half once and then only the tail: a ticket with
// hundreds of comments costs one request a minute instead of one for every
// hundred comments on it, for as many days as the requester takes to answer.
func TestTheWaitReadsTheHistoryOnceAndThenOnlyTheTail(t *testing.T) {
	api := newMemoryDynamo()
	harness := newFlowHarnessReading(t, api, silentReading{})
	envelope := claimForTerminal(t, harness.store)
	for index := 0; index < 250; index++ {
		harness.backlog.post(harness.route.AllowedCreatorID, "これまでのやり取り")
	}
	harness.clock = harness.clock.Add(time.Second)
	if result := harness.questioner.ProcessQuestionReport(context.Background(),
		hook.QuestionReportRequest{Record: testQuestionRecord(t, envelope), IssuedAt: harness.clock.UTC()}); result.Code != "question_report_recorded" {
		t.Fatalf("ProcessQuestionReport() = %+v", result)
	}
	questionCommentID := harness.backlog.comments[len(harness.backlog.comments)-1].CommentID
	harness.backlog.listedFrom = nil

	harness.clock = harness.clock.Add(time.Minute)
	if result := harness.tick(t); result.Code != "question_tick_waiting" {
		t.Fatalf("first tick = %+v", result)
	}
	harness.clock = harness.clock.Add(time.Minute)
	if result := harness.tick(t); result.Code != "question_tick_waiting" {
		t.Fatalf("second tick = %+v", result)
	}
	if len(harness.backlog.listedFrom) != 2 {
		t.Fatalf("two ticks made %d listings: %v", len(harness.backlog.listedFrom), harness.backlog.listedFrom)
	}
	if harness.backlog.listedFrom[0] != 0 {
		t.Fatalf("the first tick read from %d, not the start of the thread", harness.backlog.listedFrom[0])
	}
	// One before the question, so the question's own comment is in every
	// listing: when it was posted is what keeps an older comment from being
	// read as its answer.
	if harness.backlog.listedFrom[1] != questionCommentID-1 {
		t.Fatalf("the second tick read from %d, want the question at %d - it is re-reading %d comments of settled history every minute",
			harness.backlog.listedFrom[1], questionCommentID-1, questionCommentID-1-harness.backlog.listedFrom[1])
	}
	// The position is on the run, not in this process: a restart reads the
	// tail too.
	_, runKey := itemKeys(envelope)
	if through, ok := attributeInt64(api.items[runKey], "question_scan_through"); !ok || through < questionCommentID {
		t.Fatalf("the read position on the run is %d (recorded: %v), want at least the question at %d", through, ok, questionCommentID)
	}

	// Reading only the tail loses nothing: a stop written now is in the
	// tail, and the run ends on the next wake-up.
	harness.clock = harness.clock.Add(time.Minute)
	harness.backlog.post(harness.route.AllowedCreatorID, "停止")
	if result := harness.tick(t); result.Code != "question_tick_stopped" {
		t.Fatalf("a stop written after the question was missed: %+v", result)
	}
}

// The one read of the history is what carries the stop written before the
// question, however deep in the thread it is.
func TestAStopDeepInTheHistoryIsFoundByTheOneFullRead(t *testing.T) {
	api := newMemoryDynamo()
	harness := newFlowHarnessReading(t, api, silentReading{})
	envelope := claimForTerminal(t, harness.store)
	for index := 0; index < 120; index++ {
		harness.backlog.post(harness.route.AllowedCreatorID, "これまでのやり取り")
	}
	harness.backlog.post(harness.route.AllowedCreatorID, "停止")
	for index := 0; index < 120; index++ {
		harness.backlog.post(harness.route.AllowedCreatorID, "これまでのやり取り")
	}
	harness.clock = harness.clock.Add(time.Second)
	if result := harness.questioner.ProcessQuestionReport(context.Background(),
		hook.QuestionReportRequest{Record: testQuestionRecord(t, envelope), IssuedAt: harness.clock.UTC()}); result.Code != "question_report_recorded" {
		t.Fatalf("ProcessQuestionReport() = %+v", result)
	}
	harness.clock = harness.clock.Add(time.Minute)
	if result := harness.tick(t); result.Code != "question_tick_stopped" {
		t.Fatalf("a stop 120 comments back was never read: %+v", result)
	}
}

// A ticket can outgrow what one listing can hold. Reading it whole is an
// extra - it is what catches a stop written before the question - and the
// wait runs on the tail, so a ticket too long to read whole is read from the
// question on instead of being retried forever. Before this, every wake-up
// failed on the listing and returned before the deadline was even looked at:
// the run could not be answered, cancelled or expired, and never ended.
func TestATicketTooLongToReadWholeIsStillAnsweredAndStillExpires(t *testing.T) {
	api := newMemoryDynamo()
	harness := newFlowHarness(t, api)
	envelope := claimForTerminal(t, harness.store)
	record := testQuestionRecord(t, envelope)
	if result := harness.questioner.ProcessQuestionReport(context.Background(),
		hook.QuestionReportRequest{Record: record, IssuedAt: harness.clock.UTC()}); result.Code != "question_report_recorded" {
		t.Fatalf("ProcessQuestionReport() = %+v", result)
	}
	questionCommentID := harness.backlog.comments[len(harness.backlog.comments)-1].CommentID
	harness.backlog.tooLongFromStart = true
	harness.backlog.listedFrom = nil

	harness.clock = harness.clock.Add(time.Minute)
	if result := harness.tick(t); result.Code != "question_tick_waiting" {
		t.Fatalf("a ticket too long to read whole stalled the wait: %+v", result)
	}
	if len(harness.backlog.listedFrom) != 2 || harness.backlog.listedFrom[0] != 0 ||
		harness.backlog.listedFrom[1] != questionCommentID-1 {
		t.Fatalf("listings = %v, want the whole thread tried once and then the tail from %d",
			harness.backlog.listedFrom, questionCommentID-1)
	}

	// Nothing claims the history was read. Only a read that reached the
	// start of the thread may move the position, so a wait that gave the
	// history up tries again next time rather than recording a stop it
	// never looked for as absent.
	_, runKey := itemKeys(envelope)
	if through, recorded := attributeInt64(api.items[runKey], "question_scan_through"); recorded && through >= questionCommentID {
		t.Fatalf("the read position moved to %d on a read that never saw the start of the thread", through)
	}
	harness.clock = harness.clock.Add(time.Minute)
	if result := harness.tick(t); result.Code != "question_tick_waiting" {
		t.Fatalf("second tick on a very long ticket = %+v", result)
	}
	if harness.backlog.listedFrom[2] != 0 {
		t.Fatalf("the next wake-up read from %d; the history it gave up is never tried again", harness.backlog.listedFrom[2])
	}

	// The answer still arrives and is still taken.
	harness.clock = harness.clock.Add(time.Minute)
	harness.backlog.post(harness.route.AllowedCreatorID, "回答 C1 Q1:a")
	if result := harness.tick(t); result.Code != "question_tick_resumed" {
		t.Fatalf("an answer on a very long ticket was not taken: %+v", result)
	}
}

// And when the listing cannot be had at all, the clock still ends the wait:
// a run whose ticket cannot be read must not sit in the answer wait for
// good.
func TestAWaitThatCannotBeReadStillEndsAtItsDeadline(t *testing.T) {
	api := newMemoryDynamo()
	harness := newFlowHarness(t, api)
	envelope := claimForTerminal(t, harness.store)
	record := testQuestionRecord(t, envelope)
	if result := harness.questioner.ProcessQuestionReport(context.Background(),
		hook.QuestionReportRequest{Record: record, IssuedAt: harness.clock.UTC()}); result.Code != "question_report_recorded" {
		t.Fatalf("ProcessQuestionReport() = %+v", result)
	}
	harness.backlog.listErr = errors.New("the tracker cannot be reached")

	harness.clock = harness.clock.Add(time.Minute)
	if result := harness.tick(t); result.Code != "question_tick_comments_failed" {
		t.Fatalf("an unreadable ticket before the deadline = %+v, want the failure reported", result)
	}
	_, runKey := itemKeys(envelope)
	if state, _ := attributeString(api.items[runKey], "state"); state != stateAwaitingAnswer {
		t.Fatalf("state = %s, want the run still waiting before its deadline", state)
	}

	harness.clock = time.UnixMilli(record.AnswerDeadlineAt).Add(time.Minute)
	if result := harness.tick(t); result.Code != "question_tick_expired" {
		t.Fatalf("an unreadable ticket past its deadline = %+v, want it ended", result)
	}
	if state, _ := attributeString(api.items[runKey], "state"); state == stateAwaitingAnswer {
		t.Fatal("the run is still waiting for an answer past its deadline")
	}
}
