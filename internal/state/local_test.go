package state

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
)

// ledgerStore is the union of every store interface the hook package
// consumes — both implementations satisfy it, which is what lets one
// scenario drive both and demand identical answers.
type ledgerStore interface {
	hook.QueueStore
	hook.TerminalReportStore
	hook.QuestionStore
	hook.QuestionWaitStore
	hook.NotifyStore
	hook.ReplyStore
	hook.RunCommentStore
	hook.ResumeStore
	hook.RunNoticeStore
	hook.IngestCursorStore
}

func newLocalForTest(t *testing.T) *LocalStore {
	t.Helper()
	store, err := NewLocalStore(t.TempDir() + "/ledger.db")
	if err != nil {
		t.Fatalf("NewLocalStore() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// runLedgerScenario drives one full ticket life — enqueue, claim, question,
// notify, reply, run comments, resume, re-claim with the adopted answer,
// terminal — and records every disposition. The trace is the observable
// semantics; the Dynamo store's trace is the specification.
func runLedgerScenario(t *testing.T, store ledgerStore) []string {
	t.Helper()
	ctx := context.Background()
	trace := []string{}
	step := func(name string, value any, err error) {
		trace = append(trace, fmt.Sprintf("%s=%v err=%t", name, value, err != nil))
	}
	tokenA := strings.Repeat("a", 32)
	tokenB := strings.Repeat("b", 32)

	_, pullDisposition, err := store.Pull(ctx, testPullRequest(t))
	step("pull-empty", pullDisposition, err)

	queue := testQueueRequest(t)
	d1, err := store.Enqueue(ctx, queue)
	step("enqueue", d1, err)
	d2, err := store.Enqueue(ctx, queue)
	step("enqueue-again", d2, err)

	envelope, pd, err := store.Pull(ctx, testPullRequest(t))
	step("pull", fmt.Sprintf("%s clar=%t", pd, envelope.ClarificationJSON != ""), err)
	_, pd2, err := store.Pull(ctx, testPullRequest(t))
	step("pull-same-owner", pd2, err)
	d3, err := store.Enqueue(ctx, queue)
	step("enqueue-claimed", d3, err)

	begin := testQuestionBegin(t, queue.Envelope, testQueuedAt.Add(3*time.Second), tokenA)
	_, qb, err := store.BeginQuestion(ctx, begin)
	step("q-begin", qb, err)
	other := testQuestionBegin(t, queue.Envelope, testQueuedAt.Add(4*time.Second), tokenB)
	_, qb2, err := store.BeginQuestion(ctx, other)
	step("q-begin-busy", qb2, err)
	complete := testQuestionComplete(t, queue.Envelope, tokenA, 6001, testQueuedAt.Add(4*time.Second))
	qc, err := store.CompleteQuestion(ctx, complete)
	step("q-complete", qc, err)
	qc2, err := store.CompleteQuestion(ctx, complete)
	step("q-complete-again", qc2, err)
	_, qb3, err := store.BeginQuestion(ctx, begin)
	step("q-begin-after", qb3, err)

	wait, found, err := store.LoadQuestionWait(ctx, testTerminalRoute(t))
	step("q-wait", fmt.Sprintf("found=%t comment=%d posting=%t", found, wait.QuestionCommentID, wait.Posting), err)

	notifyBegin := testNotifyBegin(t, queue.Envelope, 1, time.UnixMilli(testQuestionRecord(t, queue.Envelope).NotifyAt[0]).UTC(), tokenA)
	_, nb, err := store.BeginNotify(ctx, notifyBegin)
	step("n-begin", nb, err)
	notifyComplete := testNotifyComplete(t, queue.Envelope, 1, tokenA, 6002, time.UnixMilli(testQuestionRecord(t, queue.Envelope).NotifyAt[0]).UTC().Add(time.Second))
	nc, err := store.CompleteNotify(ctx, notifyComplete)
	step("n-complete", nc, err)
	_, nb2, err := store.BeginNotify(ctx, notifyBegin)
	step("n-begin-after", nb2, err)

	replyBegin := testReplyBegin(t, queue.Envelope, hook.ReplyShortfall, 6003, testQueuedAt.Add(6*time.Second), tokenA)
	_, rb, err := store.BeginReply(ctx, replyBegin)
	step("r-begin", rb, err)
	handled, err := store.ReplyState(ctx, testTerminalRoute(t), testQuestionRecord(t, queue.Envelope), hook.ReplyShortfall, 6003)
	step("r-state-before", handled, err)
	replyComplete := replyCompleteFromBegin(replyBegin, 6101, testQueuedAt.Add(7*time.Second))
	rc, err := store.CompleteReply(ctx, replyComplete)
	step("r-complete", rc, err)
	handled2, err := store.ReplyState(ctx, testTerminalRoute(t), testQuestionRecord(t, queue.Envelope), hook.ReplyShortfall, 6003)
	step("r-state-after", handled2, err)

	rcBegin := hook.RunCommentBeginRequest{
		Route: testTerminalRoute(t), Kind: hook.RunCommentAck, Qualifier: "",
		ContentSHA256: strings.Repeat("e", 64), LeaseToken: tokenA,
		StartedAt:  testQueuedAt.Add(8 * time.Second),
		LeaseUntil: testQueuedAt.Add(8 * time.Second).Add(testTerminalRoute(t).LeaseDuration),
	}
	_, cb, err := store.BeginRunComment(ctx, rcBegin)
	step("c-begin", cb, err)
	ccReq := hook.RunCommentCompleteRequest{
		Route: testTerminalRoute(t), Kind: hook.RunCommentAck, Qualifier: "",
		ContentSHA256: strings.Repeat("e", 64), LeaseToken: tokenA,
		CommentID: 6201, PostedAt: testQueuedAt.Add(9 * time.Second),
	}
	cc, err := store.CompleteRunComment(ctx, ccReq)
	step("c-complete", cc, err)
	seen, err := store.RunCommentState(ctx, testTerminalRoute(t), hook.RunCommentAck, "")
	step("c-state", seen, err)

	record := testClarificationRecord(t, queue.Envelope, 6001, 6301)
	resume := testResumeRequest(t, record, testQueuedAt.Add(10*time.Second))
	rd, err := store.ResumeWithAnswer(ctx, resume)
	step("resume", rd, err)
	rd2, err := store.ResumeWithAnswer(ctx, resume)
	step("resume-again", rd2, err)

	pullAfterResume := testPullRequest(t)
	pullAfterResume.IssuedAt = testQueuedAt.Add(11 * time.Second)
	pullAfterResume.ClaimedAt = testQueuedAt.Add(11 * time.Second)
	envelope2, pd3, err := store.Pull(ctx, pullAfterResume)
	step("pull-resumed", fmt.Sprintf("%s clar=%t", pd3, envelope2.ClarificationJSON != ""), err)

	notice, err := store.LoadRunNotice(ctx, testTerminalRoute(t))
	step("notice-live", fmt.Sprintf("exists=%t terminal=%t", notice.Exists, notice.Terminal), err)

	tb := testTerminalBegin(t, envelope2, hook.TerminalValidationFailed, testQueuedAt.Add(11*time.Second), tokenA)
	_, td, err := store.BeginTerminal(ctx, tb)
	step("t-begin", td, err)
	tc := hook.TerminalCompleteRequest{
		Report: tb.Report, ReportJSON: tb.ReportJSON, ReportSHA256: tb.ReportSHA256, Route: tb.Route,
		LeaseToken: tokenA, CommentID: 6401, CompletedAt: testQueuedAt.Add(12 * time.Second),
	}
	tcd, err := store.CompleteTerminal(ctx, tc)
	step("t-complete", tcd, err)
	tcd2, err := store.CompleteTerminal(ctx, tc)
	step("t-complete-again", tcd2, err)
	_, td2, err := store.BeginTerminal(ctx, tb)
	step("t-begin-after", td2, err)

	notice2, err := store.LoadRunNotice(ctx, testTerminalRoute(t))
	step("notice-terminal", fmt.Sprintf("exists=%t terminal=%t", notice2.Exists, notice2.Terminal), err)

	cursor0, err := store.LoadIngestCursor(ctx, testTerminalRoute(t))
	step("cursor-empty", cursor0, err)
	err = store.StoreIngestCursor(ctx, testTerminalRoute(t), 10)
	step("cursor-store", "ok", err)
	err = store.StoreIngestCursor(ctx, testTerminalRoute(t), 5)
	step("cursor-regress", "ok", err)
	cursor1, err := store.LoadIngestCursor(ctx, testTerminalRoute(t))
	step("cursor-load", cursor1, err)
	err = store.StoreIngestCursor(ctx, testTerminalRoute(t), 20)
	step("cursor-advance", "ok", err)
	cursor2, err := store.LoadIngestCursor(ctx, testTerminalRoute(t))
	step("cursor-load2", cursor2, err)

	return trace
}

// TestLocalStoreMatchesDynamoStoreSemantics runs the identical full-life
// scenario against both implementations and demands identical dispositions
// at every step. The Dynamo store is the specification; any divergence in
// the local store is a transcription bug.
func TestLocalStoreMatchesDynamoStoreSemantics(t *testing.T) {
	dynamoTrace := runLedgerScenario(t, testStore(t, newMemoryDynamo()))
	localTrace := runLedgerScenario(t, newLocalForTest(t))
	if len(dynamoTrace) != len(localTrace) {
		t.Fatalf("trace lengths differ: dynamo=%d local=%d\ndynamo=%v\nlocal=%v",
			len(dynamoTrace), len(localTrace), dynamoTrace, localTrace)
	}
	for index := range dynamoTrace {
		if dynamoTrace[index] != localTrace[index] {
			t.Errorf("step %d diverges:\n  dynamo: %s\n  local:  %s", index, dynamoTrace[index], localTrace[index])
		}
	}
}

// TestLocalStoreSurvivesReopen closes and reopens the file between the two
// halves of the life cycle: the ledger must be durable, not connection state.
func TestLocalStoreSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/ledger.db"
	store, err := NewLocalStore(path)
	if err != nil {
		t.Fatalf("NewLocalStore() error = %v", err)
	}
	queue := testQueueRequest(t)
	if d, err := store.Enqueue(ctx, queue); err != nil || d != hook.QueueCreated {
		t.Fatalf("Enqueue() = %s, err = %v", d, err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	reopened, err := NewLocalStore(path)
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	defer func() { _ = reopened.Close() }()
	if d, err := reopened.Enqueue(ctx, queue); err != nil || d != hook.QueueDuplicate {
		t.Fatalf("Enqueue() after reopen = %s, err = %v (want duplicate)", d, err)
	}
	if _, d, err := reopened.Pull(ctx, testPullRequest(t)); err != nil || d != hook.PullAcquired {
		t.Fatalf("Pull() after reopen = %s, err = %v", d, err)
	}
}
