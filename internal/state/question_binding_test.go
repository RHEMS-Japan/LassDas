package state

import (
	"context"
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
func askSecondRound(t *testing.T, harness *flowHarness, envelope hook.DispatchEnvelope) (hook.QuestionRecord, int64) {
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
	return second, harness.backlog.comments[len(harness.backlog.comments)-1].CommentID
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
	_, questionCommentID := askSecondRound(t, harness, envelope)
	reader.bodies = nil

	// Written now, but for the round that is already over.
	harness.clock = harness.clock.Add(time.Minute)
	harness.backlog.post(harness.route.AllowedCreatorID, "回答 C1 Q1:b")
	if result := harness.tick(t); result.Code != "question_tick_waiting" {
		t.Fatalf("an answer for C1 was taken while C2 was open: %+v", result)
	}

	// Written before the open question existed, and only numbered after it.
	// Ids say it is newer; the clock says it cannot be an answer to a
	// question nobody had seen.
	posted := harness.clock
	harness.clock = harness.clock.Add(-10 * time.Minute)
	staleID := harness.backlog.post(harness.route.AllowedCreatorID, "Q1: b")
	harness.clock = posted
	if staleID <= questionCommentID {
		t.Fatalf("the stale comment id %d is not after the question's %d; the test proves nothing", staleID, questionCommentID)
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
