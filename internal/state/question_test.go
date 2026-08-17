package state

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
)

const testQuestionsJSON = `[{"id":"Q1","dimension":"user_visible_behavior","question":"一覧の並び順はどちらにしますか","why_blocking":"利用者に見える並びが変わる","choices":[{"id":"a","label":"新着順","effect":"新しい項目が先頭に出る"},{"id":"b","label":"名前順","effect":"五十音順に並ぶ"}]}]`

func testQuestionRecord(t *testing.T, envelope hook.DispatchEnvelope) hook.QuestionRecord {
	t.Helper()
	pull := testPullRequest(t)
	base := testQueuedAt.UnixMilli()
	return hook.QuestionRecord{
		Protocol:          hook.QuestionProtocolVersion,
		DeliveryID:        envelope.DeliveryID,
		InputSHA256:       envelope.Snapshot.InputSHA256,
		RepositoryID:      pull.Owner.RepositoryID,
		RepositorySHA256:  pull.Owner.RepositorySHA256,
		WorkflowRefSHA256: pull.Owner.WorkflowRefSHA256,
		WorkflowSHA:       pull.Owner.WorkflowSHA,
		WorkflowRunID:     pull.Owner.WorkflowRunID,
		RunAttempt:        pull.Owner.RunAttempt,
		AutomationRunID:   pull.RunID,
		RunURL:            "https://github.com/example/automation-receiver/actions/runs/" + strconv.FormatInt(pull.Owner.WorkflowRunID, 10) + "/attempts/" + strconv.Itoa(pull.Owner.RunAttempt),
		QuestionRevision:  1,
		QuestionsJSON:     testQuestionsJSON,
		QuestionsSHA256:   hook.TerminalReportDigest([]byte(testQuestionsJSON)),
		DecisionSHA256:    strings.Repeat("c", 64),
		AnswerDeadlineAt:  base + 125*time.Hour.Milliseconds(),
		NotifyAt: [3]int64{
			base + 24*time.Hour.Milliseconds(),
			base + 72*time.Hour.Milliseconds(),
			base + 120*time.Hour.Milliseconds(),
		},
	}
}

func testQuestionBegin(t *testing.T, envelope hook.DispatchEnvelope, startedAt time.Time, token string) hook.QuestionBeginRequest {
	t.Helper()
	record := testQuestionRecord(t, envelope)
	body, err := hook.MarshalQuestionRecord(record)
	if err != nil {
		t.Fatalf("MarshalQuestionRecord() error = %v", err)
	}
	route := testTerminalRoute(t)
	return hook.QuestionBeginRequest{
		Record: record, RecordJSON: string(body), RecordSHA256: hook.TerminalReportDigest(body), Route: route,
		StartedAt: startedAt, LeaseUntil: startedAt.Add(route.LeaseDuration), LeaseToken: token,
	}
}

func testQuestionComplete(t *testing.T, envelope hook.DispatchEnvelope, token string, commentID int64, postedAt time.Time) hook.QuestionCompleteRequest {
	t.Helper()
	record := testQuestionRecord(t, envelope)
	body, err := hook.MarshalQuestionRecord(record)
	if err != nil {
		t.Fatalf("MarshalQuestionRecord() error = %v", err)
	}
	return hook.QuestionCompleteRequest{
		Record: record, RecordJSON: string(body), RecordSHA256: hook.TerminalReportDigest(body),
		Route: testTerminalRoute(t), LeaseToken: token, CommentID: commentID, PostedAt: postedAt,
	}
}

func awaitForTest(t *testing.T, store *DynamoStore) hook.DispatchEnvelope {
	t.Helper()
	envelope := claimForTerminal(t, store)
	begin := testQuestionBegin(t, envelope, testQueuedAt.Add(3*time.Second), strings.Repeat("a", 32))
	if _, disposition, err := store.BeginQuestion(context.Background(), begin); err != nil || disposition != hook.QuestionBeginAcquired {
		t.Fatalf("BeginQuestion() disposition = %s, err = %v", disposition, err)
	}
	complete := testQuestionComplete(t, envelope, strings.Repeat("a", 32), 6001, testQueuedAt.Add(4*time.Second))
	if disposition, err := store.CompleteQuestion(context.Background(), complete); err != nil || disposition != hook.QuestionCompleted {
		t.Fatalf("CompleteQuestion() disposition = %s, err = %v", disposition, err)
	}
	return envelope
}

func TestBeginQuestionSealsRecordUnderLeaseFromClaimedOnly(t *testing.T) {
	api := newMemoryDynamo()
	store := testStore(t, api)
	envelope := enqueueForTest(t, store)

	// A queued run has no claim owner yet: the question begin must not seal.
	begin := testQuestionBegin(t, envelope, testQueuedAt.Add(3*time.Second), strings.Repeat("a", 32))
	if _, disposition, err := store.BeginQuestion(context.Background(), begin); err != nil || disposition != hook.QuestionBeginConflict {
		t.Fatalf("BeginQuestion() on queued = %s, err = %v", disposition, err)
	}

	if _, disposition, err := store.Pull(context.Background(), testPullRequest(t)); err != nil || disposition != hook.PullAcquired {
		t.Fatalf("Pull() disposition = %s, err = %v", disposition, err)
	}
	if _, disposition, err := store.BeginQuestion(context.Background(), begin); err != nil || disposition != hook.QuestionBeginAcquired {
		t.Fatalf("BeginQuestion() disposition = %s, err = %v", disposition, err)
	}
	assertNoUnusedExpressionBindings(t, api.lastTransaction.TransactItems[1].Update)
	_, runKey := itemKeys(envelope)
	item := api.items[runKey]
	if state, _ := attributeString(item, "state"); state != stateQuestionPending {
		t.Fatalf("state = %s, want %s", state, stateQuestionPending)
	}
	if !questionRecordMatches(item, begin.RecordSHA256) {
		t.Fatal("question record digest was not sealed")
	}
	if stored, _ := attributeString(item, "question_record_json"); stored != begin.RecordJSON {
		t.Fatal("question record json was not sealed")
	}
	if _, exists := item["question_comment_id"]; exists {
		t.Fatal("comment id must not exist before completion")
	}

	// The same seal under a live lease is busy for any other poster.
	other := testQuestionBegin(t, envelope, testQueuedAt.Add(4*time.Second), strings.Repeat("b", 32))
	if _, disposition, err := store.BeginQuestion(context.Background(), other); err != nil || disposition != hook.QuestionBeginBusy {
		t.Fatalf("BeginQuestion() during lease = %s, err = %v", disposition, err)
	}
}

func TestCompleteQuestionBindsCommentExactlyOnce(t *testing.T) {
	api := newMemoryDynamo()
	store := testStore(t, api)
	envelope := claimForTerminal(t, store)
	token := strings.Repeat("a", 32)
	begin := testQuestionBegin(t, envelope, testQueuedAt.Add(3*time.Second), token)
	if _, disposition, err := store.BeginQuestion(context.Background(), begin); err != nil || disposition != hook.QuestionBeginAcquired {
		t.Fatalf("BeginQuestion() disposition = %s, err = %v", disposition, err)
	}

	complete := testQuestionComplete(t, envelope, token, 6001, testQueuedAt.Add(4*time.Second))
	if disposition, err := store.CompleteQuestion(context.Background(), complete); err != nil || disposition != hook.QuestionCompleted {
		t.Fatalf("CompleteQuestion() disposition = %s, err = %v", disposition, err)
	}
	assertNoUnusedExpressionBindings(t, api.lastTransaction.TransactItems[1].Update)
	_, runKey := itemKeys(envelope)
	item := api.items[runKey]
	if state, _ := attributeString(item, "state"); state != stateAwaitingAnswer {
		t.Fatalf("state = %s, want %s", state, stateAwaitingAnswer)
	}
	if !attributeInt64Equals(item, "question_comment_id", 6001) {
		t.Fatal("comment id was not bound")
	}
	if _, exists := item["question_lease_token"]; exists {
		t.Fatal("lease token must be removed after completion")
	}

	// Same seal, same comment: resolved as already complete, never duplicated.
	if disposition, err := store.CompleteQuestion(context.Background(), complete); err != nil || disposition != hook.QuestionAlreadyComplete {
		t.Fatalf("duplicate CompleteQuestion() = %s, err = %v", disposition, err)
	}
	// A different observed comment for the same seal is a conflict, not a rebind.
	rebind := testQuestionComplete(t, envelope, token, 6002, testQueuedAt.Add(5*time.Second))
	if disposition, err := store.CompleteQuestion(context.Background(), rebind); err != nil || disposition != hook.QuestionCompleteConflict {
		t.Fatalf("rebinding CompleteQuestion() = %s, err = %v", disposition, err)
	}
	if !attributeInt64Equals(api.items[runKey], "question_comment_id", 6001) {
		t.Fatal("bound comment id was mutated")
	}

	// Re-begin with the identical sealed record resolves to complete.
	if _, disposition, err := store.BeginQuestion(context.Background(), begin); err != nil || disposition != hook.QuestionBeginComplete {
		t.Fatalf("BeginQuestion() after completion = %s, err = %v", disposition, err)
	}
}

func TestBeginQuestionRetryReacquiresExpiredLeaseWithoutRewritingSeal(t *testing.T) {
	api := newMemoryDynamo()
	store := testStore(t, api)
	envelope := claimForTerminal(t, store)
	first := testQuestionBegin(t, envelope, testQueuedAt.Add(3*time.Second), strings.Repeat("a", 32))
	if _, disposition, err := store.BeginQuestion(context.Background(), first); err != nil || disposition != hook.QuestionBeginAcquired {
		t.Fatalf("BeginQuestion() disposition = %s, err = %v", disposition, err)
	}

	// After the lease expires the retry takes over with a new token.
	retry := testQuestionBegin(t, envelope, testQueuedAt.Add(10*time.Minute), strings.Repeat("b", 32))
	if _, disposition, err := store.BeginQuestion(context.Background(), retry); err != nil || disposition != hook.QuestionBeginAcquired {
		t.Fatalf("reacquiring BeginQuestion() = %s, err = %v", disposition, err)
	}
	assertNoUnusedExpressionBindings(t, api.lastTransaction.TransactItems[1].Update)
	_, runKey := itemKeys(envelope)
	item := api.items[runKey]
	if !questionRecordMatches(item, first.RecordSHA256) {
		t.Fatal("reacquire changed the sealed record")
	}
	if !attributeStringEquals(item, "question_lease_token", strings.Repeat("b", 32)) {
		t.Fatal("reacquire did not rotate the lease token")
	}

	// The first poster's stale token can no longer bind a comment.
	stale := testQuestionComplete(t, envelope, strings.Repeat("a", 32), 6001, testQueuedAt.Add(11*time.Minute))
	if disposition, err := store.CompleteQuestion(context.Background(), stale); err != nil || disposition != hook.QuestionCompleteConflict {
		t.Fatalf("stale-token CompleteQuestion() = %s, err = %v", disposition, err)
	}
	// The reacquirer completes normally.
	fresh := testQuestionComplete(t, envelope, strings.Repeat("b", 32), 6001, testQueuedAt.Add(12*time.Minute))
	if disposition, err := store.CompleteQuestion(context.Background(), fresh); err != nil || disposition != hook.QuestionCompleted {
		t.Fatalf("fresh-token CompleteQuestion() = %s, err = %v", disposition, err)
	}
}

func TestQuestionStatesAreOwnedForPullAndEnqueue(t *testing.T) {
	api := newMemoryDynamo()
	store := testStore(t, api)
	envelope := claimForTerminal(t, store)
	begin := testQuestionBegin(t, envelope, testQueuedAt.Add(3*time.Second), strings.Repeat("a", 32))
	if _, disposition, err := store.BeginQuestion(context.Background(), begin); err != nil || disposition != hook.QuestionBeginAcquired {
		t.Fatalf("BeginQuestion() disposition = %s, err = %v", disposition, err)
	}

	// question_report_pending: not re-claimable, not re-queueable.
	if _, disposition, err := store.Pull(context.Background(), testPullRequest(t)); err != nil || disposition != hook.PullClaimed {
		t.Fatalf("Pull() on %s = %s, err = %v", stateQuestionPending, disposition, err)
	}
	if disposition, err := store.Enqueue(context.Background(), testQueueRequest(t)); err != nil || disposition != hook.QueueClaimed {
		t.Fatalf("Enqueue() on %s = %s, err = %v", stateQuestionPending, disposition, err)
	}

	complete := testQuestionComplete(t, envelope, strings.Repeat("a", 32), 6001, testQueuedAt.Add(4*time.Second))
	if disposition, err := store.CompleteQuestion(context.Background(), complete); err != nil || disposition != hook.QuestionCompleted {
		t.Fatalf("CompleteQuestion() disposition = %s, err = %v", disposition, err)
	}

	// awaiting_answer: same ownership rules.
	if _, disposition, err := store.Pull(context.Background(), testPullRequest(t)); err != nil || disposition != hook.PullClaimed {
		t.Fatalf("Pull() on %s = %s, err = %v", stateAwaitingAnswer, disposition, err)
	}
	if disposition, err := store.Enqueue(context.Background(), testQueueRequest(t)); err != nil || disposition != hook.QueueClaimed {
		t.Fatalf("Enqueue() on %s = %s, err = %v", stateAwaitingAnswer, disposition, err)
	}
}

func TestBeginTerminalFromAwaitingAllowsOnlyExpiryAndCancel(t *testing.T) {
	for _, run := range []struct {
		name        string
		code        hook.TerminalCode
		disposition hook.TerminalBeginDisposition
	}{
		{name: "expiry ends the wait", code: hook.TerminalClarificationExpired, disposition: hook.TerminalBeginAcquired},
		{name: "requester cancel ends the wait", code: hook.TerminalCancelled, disposition: hook.TerminalBeginAcquired},
		{name: "readiness rejection cannot be claimed while waiting", code: hook.TerminalReadinessRejected, disposition: hook.TerminalBeginConflict},
		{name: "model failure cannot be claimed while waiting", code: hook.TerminalModelFailed, disposition: hook.TerminalBeginConflict},
		{name: "clarification_required cannot re-terminalize the wait", code: hook.TerminalClarificationRequired, disposition: hook.TerminalBeginConflict},
	} {
		t.Run(run.name, func(t *testing.T) {
			api := newMemoryDynamo()
			store := testStore(t, api)
			envelope := awaitForTest(t, store)
			begin := testTerminalBegin(t, envelope, run.code, testQueuedAt.Add(6*time.Second), strings.Repeat("e", 32))
			_, disposition, err := store.BeginTerminal(context.Background(), begin)
			if err != nil || disposition != run.disposition {
				t.Fatalf("BeginTerminal(%s) = %s, err = %v, want %s", run.code, disposition, err, run.disposition)
			}
		})
	}
}

func TestAwaitingRunTerminatesThroughExistingReportFlowKeepingQuestionEvidence(t *testing.T) {
	api := newMemoryDynamo()
	store := testStore(t, api)
	envelope := awaitForTest(t, store)
	token := strings.Repeat("e", 32)
	begin := testTerminalBegin(t, envelope, hook.TerminalClarificationExpired, testQueuedAt.Add(6*time.Second), token)
	if _, disposition, err := store.BeginTerminal(context.Background(), begin); err != nil || disposition != hook.TerminalBeginAcquired {
		t.Fatalf("BeginTerminal() disposition = %s, err = %v", disposition, err)
	}
	assertNoUnusedExpressionBindings(t, api.lastTransaction.TransactItems[1].Update)
	complete := hook.TerminalCompleteRequest{
		Report: begin.Report, ReportJSON: begin.ReportJSON, ReportSHA256: begin.ReportSHA256, Route: begin.Route,
		LeaseToken: token, CommentID: 6100, CompletedAt: testQueuedAt.Add(7 * time.Second),
	}
	if disposition, err := store.CompleteTerminal(context.Background(), complete); err != nil || disposition != hook.TerminalCompleted {
		t.Fatalf("CompleteTerminal() disposition = %s, err = %v", disposition, err)
	}
	_, runKey := itemKeys(envelope)
	item := api.items[runKey]
	if state, _ := attributeString(item, "state"); state != stateTerminal {
		t.Fatalf("state = %s, want %s", state, stateTerminal)
	}
	if code, _ := attributeString(item, "terminal_code"); code != string(hook.TerminalClarificationExpired) {
		t.Fatalf("terminal_code = %s, want %s", code, hook.TerminalClarificationExpired)
	}
	// The question evidence survives termination for audit.
	if !validStoredQuestionCommentID(item) {
		t.Fatal("question comment evidence was dropped at termination")
	}
	if _, ok := attributeString(item, "question_record_json"); !ok {
		t.Fatal("question record evidence was dropped at termination")
	}

	// A completed wait can never be asked or completed again.
	reask := testQuestionBegin(t, envelope, testQueuedAt.Add(8*time.Second), strings.Repeat("f", 32))
	if _, disposition, err := store.BeginQuestion(context.Background(), reask); err != nil || disposition != hook.QuestionBeginConflict {
		t.Fatalf("BeginQuestion() after terminal = %s, err = %v", disposition, err)
	}
	replay := testQuestionComplete(t, envelope, token, 6001, testQueuedAt.Add(8*time.Second))
	if disposition, err := store.CompleteQuestion(context.Background(), replay); err != nil || disposition != hook.QuestionCompleteConflict {
		t.Fatalf("CompleteQuestion() after terminal = %s, err = %v", disposition, err)
	}
}

func TestTerminalCodeSourceMatrixFromClaimed(t *testing.T) {
	// clarification_expired only makes sense for a run that actually waited.
	api := newMemoryDynamo()
	store := testStore(t, api)
	envelope := claimForTerminal(t, store)
	expired := testTerminalBegin(t, envelope, hook.TerminalClarificationExpired, testQueuedAt.Add(3*time.Second), strings.Repeat("e", 32))
	if _, disposition, err := store.BeginTerminal(context.Background(), expired); err != nil || disposition != hook.TerminalBeginConflict {
		t.Fatalf("BeginTerminal(expired) from claimed = %s, err = %v", disposition, err)
	}
	// A pre-write cancel of a working run is allowed.
	cancelled := testTerminalBegin(t, envelope, hook.TerminalCancelled, testQueuedAt.Add(3*time.Second), strings.Repeat("e", 32))
	if _, disposition, err := store.BeginTerminal(context.Background(), cancelled); err != nil || disposition != hook.TerminalBeginAcquired {
		t.Fatalf("BeginTerminal(cancelled) from claimed = %s, err = %v", disposition, err)
	}
}

func TestBeginTerminalConflictsWhileQuestionPostIsInFlight(t *testing.T) {
	api := newMemoryDynamo()
	store := testStore(t, api)
	envelope := claimForTerminal(t, store)
	begin := testQuestionBegin(t, envelope, testQueuedAt.Add(3*time.Second), strings.Repeat("a", 32))
	if _, disposition, err := store.BeginQuestion(context.Background(), begin); err != nil || disposition != hook.QuestionBeginAcquired {
		t.Fatalf("BeginQuestion() disposition = %s, err = %v", disposition, err)
	}
	cancelled := testTerminalBegin(t, envelope, hook.TerminalCancelled, testQueuedAt.Add(4*time.Second), strings.Repeat("e", 32))
	if _, disposition, err := store.BeginTerminal(context.Background(), cancelled); err != nil || disposition != hook.TerminalBeginConflict {
		t.Fatalf("BeginTerminal(cancelled) during question post = %s, err = %v", disposition, err)
	}
}

func TestBeginQuestionValidatesSealBeforeAnyWrite(t *testing.T) {
	api := newMemoryDynamo()
	store := testStore(t, api)
	envelope := claimForTerminal(t, store)
	valid := testQuestionBegin(t, envelope, testQueuedAt.Add(3*time.Second), strings.Repeat("a", 32))
	writesBefore := api.lastTransaction

	for _, run := range []struct {
		name   string
		mutate func(request *hook.QuestionBeginRequest)
	}{
		{name: "record json is not canonical", mutate: func(request *hook.QuestionBeginRequest) {
			request.RecordJSON = request.RecordJSON + " "
		}},
		{name: "record digest mismatch", mutate: func(request *hook.QuestionBeginRequest) {
			request.RecordSHA256 = strings.Repeat("0", 64)
		}},
		{name: "lease token malformed", mutate: func(request *hook.QuestionBeginRequest) {
			request.LeaseToken = "not-a-token"
		}},
		{name: "lease until detached from route duration", mutate: func(request *hook.QuestionBeginRequest) {
			request.LeaseUntil = request.LeaseUntil.Add(time.Second)
		}},
		{name: "question revision out of contract", mutate: func(request *hook.QuestionBeginRequest) {
			request.Record.QuestionRevision = 3
		}},
		{name: "empty question set", mutate: func(request *hook.QuestionBeginRequest) {
			request.Record.QuestionsJSON = "[]"
			request.Record.QuestionsSHA256 = hook.TerminalReportDigest([]byte("[]"))
		}},
		{name: "record escapes beyond the seal bound", mutate: func(request *hook.QuestionBeginRequest) {
			// Within the questions-JSON bound, but JSON string escaping while
			// embedding expands the sealed record past its own bound. The raw
			// re-encode bypasses the marshal guard so the store validator must
			// reject it before any write.
			record := request.Record
			record.QuestionsJSON = `[{"id":"Q1","question":"` + strings.Repeat("<", 5000) + `"}]`
			record.QuestionsSHA256 = hook.TerminalReportDigest([]byte(record.QuestionsJSON))
			raw, err := json.Marshal(record)
			if err != nil {
				panic(err)
			}
			request.Record = record
			request.RecordJSON = string(raw)
			request.RecordSHA256 = hook.TerminalReportDigest(raw)
		}},
	} {
		t.Run(run.name, func(t *testing.T) {
			request := valid
			run.mutate(&request)
			_, disposition, err := store.BeginQuestion(context.Background(), request)
			class, code := hook.FailureDetails(err)
			if disposition != "" || class != hook.FailureRejected || code != "invalid_question_begin" {
				t.Fatalf("BeginQuestion() = %s, class = %s, code = %s", disposition, class, code)
			}
		})
	}
	if api.lastTransaction != writesBefore {
		t.Fatal("an invalid begin request reached the store")
	}
}

func TestConcurrentQuestionBeginHasOneLeaseOwner(t *testing.T) {
	api := newMemoryDynamo()
	store := testStore(t, api)
	envelope := claimForTerminal(t, store)
	first := testQuestionBegin(t, envelope, testQueuedAt.Add(3*time.Second), strings.Repeat("a", 32))
	second := testQuestionBegin(t, envelope, testQueuedAt.Add(3*time.Second), strings.Repeat("b", 32))
	_, firstDisposition, firstErr := store.BeginQuestion(context.Background(), first)
	_, secondDisposition, secondErr := store.BeginQuestion(context.Background(), second)
	if firstErr != nil || secondErr != nil {
		t.Fatalf("BeginQuestion() errors = %v, %v", firstErr, secondErr)
	}
	if firstDisposition != hook.QuestionBeginAcquired || secondDisposition != hook.QuestionBeginBusy {
		t.Fatalf("dispositions = %s, %s; want acquired then busy", firstDisposition, secondDisposition)
	}
	_, runKey := itemKeys(envelope)
	if !attributeStringEquals(api.items[runKey], "question_lease_token", strings.Repeat("a", 32)) {
		t.Fatal("second begin stole the live lease")
	}
}

func TestCorruptedQuestionSealFailsClosedForTermination(t *testing.T) {
	api := newMemoryDynamo()
	store := testStore(t, api)
	envelope := awaitForTest(t, store)
	_, runKey := itemKeys(envelope)
	// Corrupt the sealed record on the stored item: the wait may no longer be
	// terminated because the stored shape can no longer be trusted.
	api.items[runKey]["question_record_json"] = stringValue(`{"forged":true}`)
	begin := testTerminalBegin(t, envelope, hook.TerminalClarificationExpired, testQueuedAt.Add(6*time.Second), strings.Repeat("e", 32))
	if _, disposition, err := store.BeginTerminal(context.Background(), begin); err != nil || disposition != hook.TerminalBeginConflict {
		t.Fatalf("BeginTerminal() on corrupted seal = %s, err = %v", disposition, err)
	}
	reask := testQuestionBegin(t, envelope, testQueuedAt.Add(6*time.Second), strings.Repeat("f", 32))
	if _, disposition, err := store.BeginQuestion(context.Background(), reask); err != nil || disposition != hook.QuestionBeginConflict {
		t.Fatalf("BeginQuestion() on corrupted seal = %s, err = %v", disposition, err)
	}
}

func TestResolvePullWriteTreatsQuestionStatesAsClaimed(t *testing.T) {
	api := newMemoryDynamo()
	store := testStore(t, api)
	envelope := claimForTerminal(t, store)
	begin := testQuestionBegin(t, envelope, testQueuedAt.Add(3*time.Second), strings.Repeat("a", 32))
	if _, disposition, err := store.BeginQuestion(context.Background(), begin); err != nil || disposition != hook.QuestionBeginAcquired {
		t.Fatalf("BeginQuestion() disposition = %s, err = %v", disposition, err)
	}
	eventKey, runKey := itemKeys(envelope)
	if _, disposition, err := store.resolvePullWrite(context.Background(), testPullRequest(t), envelope, eventKey, runKey); err != nil || disposition != hook.PullClaimed {
		t.Fatalf("resolvePullWrite() on %s = %s, err = %v", stateQuestionPending, disposition, err)
	}
	complete := testQuestionComplete(t, envelope, strings.Repeat("a", 32), 6001, testQueuedAt.Add(4*time.Second))
	if disposition, err := store.CompleteQuestion(context.Background(), complete); err != nil || disposition != hook.QuestionCompleted {
		t.Fatalf("CompleteQuestion() disposition = %s, err = %v", disposition, err)
	}
	if _, disposition, err := store.resolvePullWrite(context.Background(), testPullRequest(t), envelope, eventKey, runKey); err != nil || disposition != hook.PullClaimed {
		t.Fatalf("resolvePullWrite() on %s = %s, err = %v", stateAwaitingAnswer, disposition, err)
	}
}

func TestAwaitingTerminalBeginWriteFailureIsRetryableWhenStateUnmoved(t *testing.T) {
	api := newMemoryDynamo()
	store := testStore(t, api)
	envelope := awaitForTest(t, store)
	api.transactionErr = transactionCanceled()
	begin := testTerminalBegin(t, envelope, hook.TerminalClarificationExpired, testQueuedAt.Add(6*time.Second), strings.Repeat("e", 32))
	_, disposition, err := store.BeginTerminal(context.Background(), begin)
	class, code := hook.FailureDetails(err)
	if disposition != "" || class != hook.FailureRetryable || code != "terminal_begin_write_failed" {
		t.Fatalf("BeginTerminal() = %s, class = %s, code = %s", disposition, class, code)
	}
	// Once the transient failure clears, the same request succeeds.
	api.transactionErr = nil
	if _, disposition, err := store.BeginTerminal(context.Background(), begin); err != nil || disposition != hook.TerminalBeginAcquired {
		t.Fatalf("retried BeginTerminal() = %s, err = %v", disposition, err)
	}
}
