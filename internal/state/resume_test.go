package state

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
)

const testAnswersJSON = `{"Q1":"a"}`

func testClarificationRound(t *testing.T, question hook.QuestionRecord, questionCommentID, answerCommentID int64) hook.ClarificationRound {
	t.Helper()
	route := testTerminalRoute(t)
	body, err := hook.MarshalQuestionRecord(question)
	if err != nil {
		t.Fatalf("MarshalQuestionRecord() error = %v", err)
	}
	return hook.ClarificationRound{
		QuestionRecordJSON:   string(body),
		QuestionRecordSHA256: hook.TerminalReportDigest(body),
		QuestionCommentID:    questionCommentID,
		AnswerCommentID:      answerCommentID,
		AnswererID:           route.AllowedCreatorID,
		AnswerPostedAt:       question.AnswerDeadlineAt - 1000,
		AnswerBodySHA256:     strings.Repeat("b", 64),
		AnswersJSON:          testAnswersJSON,
		AnswersSHA256:        hook.TerminalReportDigest([]byte(testAnswersJSON)),
	}
}

func testClarificationRecord(t *testing.T, envelope hook.DispatchEnvelope, questionCommentID, answerCommentID int64) hook.ClarificationRecord {
	t.Helper()
	route := testTerminalRoute(t)
	return hook.ClarificationRecord{
		Protocol:          hook.ClarificationProtocolVersion,
		DeliveryID:        envelope.DeliveryID,
		InputSHA256:       envelope.Snapshot.InputSHA256,
		RepositoryID:      route.RepositoryID,
		RepositorySHA256:  route.RepositorySHA256,
		WorkflowRefSHA256: route.WorkflowRefSHA256,
		AutomationRunID:   route.ExpectedRunID,
		InputRevision:     2,
		Rounds:            []hook.ClarificationRound{testClarificationRound(t, testQuestionRecord(t, envelope), questionCommentID, answerCommentID)},
	}
}

func testResumeRequest(t *testing.T, record hook.ClarificationRecord, resumedAt time.Time) hook.ResumeRequest {
	t.Helper()
	body, err := hook.MarshalClarificationRecord(record)
	if err != nil {
		t.Fatalf("MarshalClarificationRecord() error = %v", err)
	}
	return hook.ResumeRequest{
		Record: record, RecordJSON: string(body), RecordSHA256: hook.TerminalReportDigest(body),
		Route: testTerminalRoute(t), ResumedAt: resumedAt,
	}
}

// resumeForTest drives a run to awaiting_answer (question comment 6001) and
// resumes it with the adopted answer comment 6002.
func resumeForTest(t *testing.T, store *DynamoStore) (hook.DispatchEnvelope, hook.ResumeRequest) {
	t.Helper()
	envelope := awaitForTest(t, store)
	request := testResumeRequest(t, testClarificationRecord(t, envelope, 6001, 6002), testQueuedAt.Add(6*time.Second))
	if disposition, err := store.ResumeWithAnswer(context.Background(), request); err != nil || disposition != hook.ResumeCompleted {
		t.Fatalf("ResumeWithAnswer() disposition = %s, err = %v", disposition, err)
	}
	return envelope, request
}

func TestResumeWithAnswerReturnsRunToQueueExactlyOnce(t *testing.T) {
	api := newMemoryDynamo()
	store := testStore(t, api)
	envelope, request := resumeForTest(t, store)
	assertNoUnusedExpressionBindings(t, api.lastTransaction.TransactItems[2].Update)

	_, runKey := itemKeys(envelope)
	item := api.items[runKey]
	if state, _ := attributeString(item, "state"); state != stateQueued {
		t.Fatalf("state = %s, want %s", state, stateQueued)
	}
	if !attributeStringEquals(item, "clarification_sha256", request.RecordSHA256) {
		t.Fatal("clarification record digest was not sealed")
	}
	if stored, _ := attributeString(item, "clarification_json"); stored != request.RecordJSON {
		t.Fatal("clarification record json was not sealed")
	}
	if revision, _ := attributeInt64(item, "input_revision"); revision != 2 {
		t.Fatalf("input_revision = %d, want 2", revision)
	}
	if queuedAt, _ := attributeInt64(item, "queued_at"); queuedAt != request.ResumedAt.UnixMilli() {
		t.Fatal("queued_at was not moved to the resume time")
	}
	// The original envelope is untouched and every claim and question field is
	// gone from the run item.
	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if !attributeStringEquals(item, "envelope_json", string(encoded)) {
		t.Fatal("original envelope was rewritten")
	}
	for _, name := range []string{
		"question_record_sha256", "question_record_json", "question_started_at",
		"question_comment_id", "question_posted_at", "claimed_at",
		"repository_id", "repository_sha256", "workflow_ref_sha256",
		"workflow_sha", "workflow_run_id", "run_attempt",
	} {
		if _, exists := item[name]; exists {
			t.Fatalf("field %s must not remain on the resumed item", name)
		}
	}
	// The question evidence is archived in the same transaction.
	archive := api.items[clarificationArchiveKey(runKey, 2)]
	if archive == nil {
		t.Fatal("clarification archive item was not written")
	}
	if !attributeStringEquals(archive, "record_json", request.RecordJSON) {
		t.Fatal("archive does not hold the sealed record")
	}
	if _, ok := attributeInt64(archive, "question_posted_at"); !ok {
		t.Fatal("archive does not hold the question posting evidence")
	}

	// The same adoption retried is idempotent; a different answer can no
	// longer win after the resume is decided.
	if disposition, err := store.ResumeWithAnswer(context.Background(), request); err != nil || disposition != hook.ResumeAlreadyComplete {
		t.Fatalf("duplicate ResumeWithAnswer() = %s, err = %v", disposition, err)
	}
	other := testResumeRequest(t, testClarificationRecord(t, envelope, 6001, 6003), testQueuedAt.Add(7*time.Second))
	if disposition, err := store.ResumeWithAnswer(context.Background(), other); err != nil || disposition != hook.ResumeConflict {
		t.Fatalf("late different answer ResumeWithAnswer() = %s, err = %v", disposition, err)
	}
}

func TestResumeRequiresAWaitingRunWithTheExactQuestion(t *testing.T) {
	api := newMemoryDynamo()
	store := testStore(t, api)
	envelope := claimForTerminal(t, store)
	// No question was ever posted: there is nothing to resume.
	request := testResumeRequest(t, testClarificationRecord(t, envelope, 6001, 6002), testQueuedAt.Add(6*time.Second))
	if disposition, err := store.ResumeWithAnswer(context.Background(), request); err != nil || disposition != hook.ResumeConflict {
		t.Fatalf("ResumeWithAnswer() on claimed = %s, err = %v", disposition, err)
	}

	begin := testQuestionBegin(t, envelope, testQueuedAt.Add(3*time.Second), strings.Repeat("a", 32))
	if _, disposition, err := store.BeginQuestion(context.Background(), begin); err != nil || disposition != hook.QuestionBeginAcquired {
		t.Fatalf("BeginQuestion() disposition = %s, err = %v", disposition, err)
	}
	// The question post is still in flight: adopting an answer now would race
	// the comment binding.
	if disposition, err := store.ResumeWithAnswer(context.Background(), request); err != nil || disposition != hook.ResumeConflict {
		t.Fatalf("ResumeWithAnswer() on %s = %s, err = %v", stateQuestionPending, disposition, err)
	}
	complete := testQuestionComplete(t, envelope, strings.Repeat("a", 32), 6001, testQueuedAt.Add(4*time.Second))
	if disposition, err := store.CompleteQuestion(context.Background(), complete); err != nil || disposition != hook.QuestionCompleted {
		t.Fatalf("CompleteQuestion() disposition = %s, err = %v", disposition, err)
	}
	// The record must reference the posted question comment, not another one.
	wrongComment := testResumeRequest(t, testClarificationRecord(t, envelope, 6009, 6010), testQueuedAt.Add(6*time.Second))
	if disposition, err := store.ResumeWithAnswer(context.Background(), wrongComment); err != nil || disposition != hook.ResumeConflict {
		t.Fatalf("ResumeWithAnswer() with wrong question comment = %s, err = %v", disposition, err)
	}
	if disposition, err := store.ResumeWithAnswer(context.Background(), request); err != nil || disposition != hook.ResumeCompleted {
		t.Fatalf("ResumeWithAnswer() disposition = %s, err = %v", disposition, err)
	}
}

func TestResumeAndExpiryDecideExactlyOne(t *testing.T) {
	// Expiry first: the adopted answer can no longer resume the run.
	api := newMemoryDynamo()
	store := testStore(t, api)
	envelope := awaitForTest(t, store)
	expiry := testTerminalBegin(t, envelope, hook.TerminalClarificationExpired, testQueuedAt.Add(6*time.Second), strings.Repeat("e", 32))
	if _, disposition, err := store.BeginTerminal(context.Background(), expiry); err != nil || disposition != hook.TerminalBeginAcquired {
		t.Fatalf("BeginTerminal() disposition = %s, err = %v", disposition, err)
	}
	request := testResumeRequest(t, testClarificationRecord(t, envelope, 6001, 6002), testQueuedAt.Add(7*time.Second))
	if disposition, err := store.ResumeWithAnswer(context.Background(), request); err != nil || disposition != hook.ResumeConflict {
		t.Fatalf("ResumeWithAnswer() after expiry begin = %s, err = %v", disposition, err)
	}

	// Resume first: the expiry can no longer terminate the run.
	api = newMemoryDynamo()
	store = testStore(t, api)
	envelope, _ = resumeForTest(t, store)
	expiry = testTerminalBegin(t, envelope, hook.TerminalClarificationExpired, testQueuedAt.Add(8*time.Second), strings.Repeat("e", 32))
	if _, disposition, err := store.BeginTerminal(context.Background(), expiry); err != nil || disposition != hook.TerminalBeginConflict {
		t.Fatalf("BeginTerminal() after resume = %s, err = %v", disposition, err)
	}
}

func TestResumedRunIsClaimableAndAcceptsOnlyTheChainedSecondQuestion(t *testing.T) {
	api := newMemoryDynamo()
	store := testStore(t, api)
	envelope, first := resumeForTest(t, store)

	// The resumed run is queued again for the standard 5-minute pull, with a
	// pull issued after the resume moment.
	pull := testPullRequest(t)
	pull.IssuedAt = first.ResumedAt.Add(time.Second)
	pull.ClaimedAt = first.ResumedAt.Add(2 * time.Second)
	pulled, disposition, err := store.Pull(context.Background(), pull)
	if err != nil || disposition != hook.PullAcquired {
		t.Fatalf("Pull() after resume = %s, err = %v", disposition, err)
	}
	if pulled.DeliveryID != envelope.DeliveryID {
		t.Fatal("resumed pull did not return the original envelope")
	}
	// Also the duplicate-enqueue path keeps treating the resumed run as known.
	if disposition, err := store.Enqueue(context.Background(), testQueueRequest(t)); err != nil || disposition != hook.QueueClaimed {
		t.Fatalf("Enqueue() after re-claim = %s, err = %v", disposition, err)
	}

	// A stale round-1 question can never reopen a resumed run.
	staleBegin := testQuestionBegin(t, envelope, first.ResumedAt.Add(3*time.Second), strings.Repeat("a", 32))
	if _, disposition, err := store.BeginQuestion(context.Background(), staleBegin); err != nil || disposition != hook.QuestionBeginConflict {
		t.Fatalf("BeginQuestion() with revision 1 = %s, err = %v", disposition, err)
	}
	// A round-2 question chained to a different record is rejected.
	forged := testQuestionBegin(t, envelope, first.ResumedAt.Add(3*time.Second), strings.Repeat("a", 32))
	forged.Record.QuestionRevision = 2
	forged.Record.ClarificationSHA256 = strings.Repeat("0", 64)
	forgedBody, err := hook.MarshalQuestionRecord(forged.Record)
	if err != nil {
		t.Fatalf("MarshalQuestionRecord() error = %v", err)
	}
	forged.RecordJSON = string(forgedBody)
	forged.RecordSHA256 = hook.TerminalReportDigest(forgedBody)
	if _, disposition, err := store.BeginQuestion(context.Background(), forged); err != nil || disposition != hook.QuestionBeginConflict {
		t.Fatalf("BeginQuestion() with forged chain = %s, err = %v", disposition, err)
	}
	// The question chained to the sealed resume record is accepted.
	chained := forged
	chained.Record.ClarificationSHA256 = first.RecordSHA256
	chainedBody, err := hook.MarshalQuestionRecord(chained.Record)
	if err != nil {
		t.Fatalf("MarshalQuestionRecord() error = %v", err)
	}
	chained.RecordJSON = string(chainedBody)
	chained.RecordSHA256 = hook.TerminalReportDigest(chainedBody)
	if _, disposition, err := store.BeginQuestion(context.Background(), chained); err != nil || disposition != hook.QuestionBeginAcquired {
		t.Fatalf("BeginQuestion() with revision 2 = %s, err = %v", disposition, err)
	}
	assertNoUnusedExpressionBindings(t, api.lastTransaction.TransactItems[1].Update)
}

// awaitRoundTwoForTest drives a run through resume, re-claim and the posted
// round-2 question (comment 7001).
func awaitRoundTwoForTest(t *testing.T, store *DynamoStore) (hook.DispatchEnvelope, hook.ResumeRequest, hook.QuestionRecord) {
	t.Helper()
	envelope, first := resumeForTest(t, store)
	pull := testPullRequest(t)
	pull.IssuedAt = first.ResumedAt.Add(time.Second)
	pull.ClaimedAt = first.ResumedAt.Add(2 * time.Second)
	if _, disposition, err := store.Pull(context.Background(), pull); err != nil || disposition != hook.PullAcquired {
		t.Fatalf("Pull() after resume = %s, err = %v", disposition, err)
	}
	begin := testQuestionBegin(t, envelope, first.ResumedAt.Add(3*time.Second), strings.Repeat("a", 32))
	begin.Record.QuestionRevision = 2
	begin.Record.ClarificationSHA256 = first.RecordSHA256
	body, err := hook.MarshalQuestionRecord(begin.Record)
	if err != nil {
		t.Fatalf("MarshalQuestionRecord() error = %v", err)
	}
	begin.RecordJSON = string(body)
	begin.RecordSHA256 = hook.TerminalReportDigest(body)
	if _, disposition, err := store.BeginQuestion(context.Background(), begin); err != nil || disposition != hook.QuestionBeginAcquired {
		t.Fatalf("BeginQuestion() round 2 = %s, err = %v", disposition, err)
	}
	complete := testQuestionComplete(t, envelope, strings.Repeat("a", 32), 7001, first.ResumedAt.Add(4*time.Second))
	complete.Record = begin.Record
	complete.RecordJSON = begin.RecordJSON
	complete.RecordSHA256 = begin.RecordSHA256
	if disposition, err := store.CompleteQuestion(context.Background(), complete); err != nil || disposition != hook.QuestionCompleted {
		t.Fatalf("CompleteQuestion() round 2 = %s, err = %v", disposition, err)
	}
	return envelope, first, begin.Record
}

func TestSecondResumeExtendsTheSealedChainVerbatim(t *testing.T) {
	api := newMemoryDynamo()
	store := testStore(t, api)
	envelope, first, roundTwoQuestion := awaitRoundTwoForTest(t, store)

	second := testClarificationRecord(t, envelope, 6001, 6002)
	second.InputRevision = 3
	second.Rounds = append(second.Rounds, testClarificationRound(t, roundTwoQuestion, 7001, 7002))
	request := testResumeRequest(t, second, first.ResumedAt.Add(6*time.Second))
	request.PreviousRecordJSON = first.RecordJSON
	request.PreviousRecordSHA256 = first.RecordSHA256
	if disposition, err := store.ResumeWithAnswer(context.Background(), request); err != nil || disposition != hook.ResumeCompleted {
		t.Fatalf("second ResumeWithAnswer() = %s, err = %v", disposition, err)
	}
	assertNoUnusedExpressionBindings(t, api.lastTransaction.TransactItems[2].Update)
	_, runKey := itemKeys(envelope)
	if revision, _ := attributeInt64(api.items[runKey], "input_revision"); revision != 3 {
		t.Fatalf("input_revision = %d, want 3", revision)
	}
	if api.items[clarificationArchiveKey(runKey, 3)] == nil {
		t.Fatal("second clarification archive item was not written")
	}
	if disposition, err := store.ResumeWithAnswer(context.Background(), request); err != nil || disposition != hook.ResumeAlreadyComplete {
		t.Fatalf("duplicate second ResumeWithAnswer() = %s, err = %v", disposition, err)
	}

	// A second resume that does not present the sealed previous record is
	// rejected before any write.
	broken := request
	broken.PreviousRecordJSON = ""
	broken.PreviousRecordSHA256 = ""
	_, code := hook.FailureDetails(func() error {
		_, err := store.ResumeWithAnswer(context.Background(), broken)
		return err
	}())
	if code != "invalid_resume_request" {
		t.Fatalf("code = %s, want invalid_resume_request", code)
	}
}

func TestResumeValidatesRequestBeforeAnyWrite(t *testing.T) {
	api := newMemoryDynamo()
	store := testStore(t, api)
	envelope := awaitForTest(t, store)
	valid := testResumeRequest(t, testClarificationRecord(t, envelope, 6001, 6002), testQueuedAt.Add(6*time.Second))
	writesBefore := api.lastTransaction

	for _, run := range []struct {
		name   string
		mutate func(request *hook.ResumeRequest)
	}{
		{name: "record json is not canonical", mutate: func(request *hook.ResumeRequest) {
			request.RecordJSON = request.RecordJSON + " "
		}},
		{name: "record digest mismatch", mutate: func(request *hook.ResumeRequest) {
			request.RecordSHA256 = strings.Repeat("0", 64)
		}},
		{name: "resumed at unset", mutate: func(request *hook.ResumeRequest) {
			request.ResumedAt = time.Time{}
		}},
		{name: "first resume with a previous record", mutate: func(request *hook.ResumeRequest) {
			request.PreviousRecordJSON = request.RecordJSON
			request.PreviousRecordSHA256 = request.RecordSHA256
		}},
		{name: "answerer outside the allowlist", mutate: func(request *hook.ResumeRequest) {
			record := request.Record
			record.Rounds = append([]hook.ClarificationRound{}, record.Rounds...)
			record.Rounds[0].AnswererID++
			raw, err := json.Marshal(record)
			if err != nil {
				panic(err)
			}
			request.Record = record
			request.RecordJSON = string(raw)
			request.RecordSHA256 = hook.TerminalReportDigest(raw)
		}},
		{name: "answer after the sealed deadline", mutate: func(request *hook.ResumeRequest) {
			record := request.Record
			record.Rounds = append([]hook.ClarificationRound{}, record.Rounds...)
			record.Rounds[0].AnswerPostedAt = testQuestionRecord(t, envelope).AnswerDeadlineAt
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
			disposition, err := store.ResumeWithAnswer(context.Background(), request)
			class, code := hook.FailureDetails(err)
			if disposition != "" || class != hook.FailureRejected || code != "invalid_resume_request" {
				t.Fatalf("ResumeWithAnswer() = %s, class = %s, code = %s", disposition, class, code)
			}
		})
	}
	if api.lastTransaction != writesBefore {
		t.Fatal("an invalid resume request reached the store")
	}
}

func TestResumeWriteFailureIsRetryableWhenStateUnmoved(t *testing.T) {
	api := newMemoryDynamo()
	store := testStore(t, api)
	envelope := awaitForTest(t, store)
	request := testResumeRequest(t, testClarificationRecord(t, envelope, 6001, 6002), testQueuedAt.Add(6*time.Second))
	api.transactionErr = transactionCanceled()
	disposition, err := store.ResumeWithAnswer(context.Background(), request)
	class, code := hook.FailureDetails(err)
	if disposition != "" || class != hook.FailureRetryable || code != "resume_write_failed" {
		t.Fatalf("ResumeWithAnswer() = %s, class = %s, code = %s", disposition, class, code)
	}
	api.transactionErr = nil
	if disposition, err := store.ResumeWithAnswer(context.Background(), request); err != nil || disposition != hook.ResumeCompleted {
		t.Fatalf("retried ResumeWithAnswer() = %s, err = %v", disposition, err)
	}
}

func TestConcurrentResumeHasOneWinner(t *testing.T) {
	api := newMemoryDynamo()
	store := testStore(t, api)
	envelope := awaitForTest(t, store)
	requests := []hook.ResumeRequest{
		testResumeRequest(t, testClarificationRecord(t, envelope, 6001, 6002), testQueuedAt.Add(6*time.Second)),
		testResumeRequest(t, testClarificationRecord(t, envelope, 6001, 6003), testQueuedAt.Add(6*time.Second)),
	}
	dispositions := make([]hook.ResumeDisposition, len(requests))
	var group sync.WaitGroup
	for index := range requests {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			disposition, err := store.ResumeWithAnswer(context.Background(), requests[index])
			if err != nil {
				panic(err)
			}
			dispositions[index] = disposition
		}(index)
	}
	group.Wait()
	completed := 0
	winner := -1
	for index, disposition := range dispositions {
		if disposition == hook.ResumeCompleted {
			completed++
			winner = index
		} else if disposition != hook.ResumeConflict {
			t.Fatalf("disposition[%d] = %s", index, disposition)
		}
	}
	if completed != 1 {
		t.Fatalf("completed = %d, want exactly one winner", completed)
	}
	_, runKey := itemKeys(envelope)
	if !attributeStringEquals(api.items[runKey], "clarification_sha256", requests[winner].RecordSHA256) {
		t.Fatal("stored record does not belong to the winner")
	}
}

func TestResumeFailsClosedWhenStoredOwnerIsCorrupted(t *testing.T) {
	api := newMemoryDynamo()
	store := testStore(t, api)
	envelope := awaitForTest(t, store)
	_, runKey := itemKeys(envelope)
	// A waiting item whose stored owner binding no longer matches the sealed
	// route must resolve to a conflict, not to an endless retry loop.
	api.items[runKey]["repository_sha256"] = stringValue(strings.Repeat("f", 64))
	request := testResumeRequest(t, testClarificationRecord(t, envelope, 6001, 6002), testQueuedAt.Add(6*time.Second))
	if disposition, err := store.ResumeWithAnswer(context.Background(), request); err != nil || disposition != hook.ResumeConflict {
		t.Fatalf("ResumeWithAnswer() on corrupted owner = %s, err = %v", disposition, err)
	}
}

func TestCorruptedClarificationSealFailsClosed(t *testing.T) {
	api := newMemoryDynamo()
	store := testStore(t, api)
	envelope, first := resumeForTest(t, store)
	_, runKey := itemKeys(envelope)
	pull := testPullRequest(t)
	pull.IssuedAt = first.ResumedAt.Add(time.Second)
	pull.ClaimedAt = first.ResumedAt.Add(2 * time.Second)

	// A corrupted seal stops the pull itself: a resumed run must never be
	// dispatched with a revision-1 view of its input.
	api.items[runKey]["clarification_json"] = stringValue(`{"forged":true}`)
	if _, disposition, err := store.Pull(context.Background(), pull); disposition != "" || err == nil {
		t.Fatalf("Pull() on corrupted seal = %s, err = %v", disposition, err)
	} else if _, code := hook.FailureDetails(err); code != "pull_binding_invalid" {
		t.Fatalf("Pull() code = %s", code)
	}
	// The duplicate adoption can no longer be confirmed either.
	if disposition, err := store.ResumeWithAnswer(context.Background(), first); err != nil || disposition != hook.ResumeConflict {
		t.Fatalf("ResumeWithAnswer() on corrupted seal = %s, err = %v", disposition, err)
	}

	// Corruption after a clean re-claim still blocks the next question.
	api.items[runKey]["clarification_json"] = stringValue(first.RecordJSON)
	if _, disposition, err := store.Pull(context.Background(), pull); err != nil || disposition != hook.PullAcquired {
		t.Fatalf("Pull() disposition = %s, err = %v", disposition, err)
	}
	api.items[runKey]["clarification_json"] = stringValue(`{"forged":true}`)
	chained := testQuestionBegin(t, envelope, first.ResumedAt.Add(3*time.Second), strings.Repeat("a", 32))
	chained.Record.QuestionRevision = 2
	chained.Record.ClarificationSHA256 = first.RecordSHA256
	body, err := hook.MarshalQuestionRecord(chained.Record)
	if err != nil {
		t.Fatalf("MarshalQuestionRecord() error = %v", err)
	}
	chained.RecordJSON = string(body)
	chained.RecordSHA256 = hook.TerminalReportDigest(body)
	if _, disposition, err := store.BeginQuestion(context.Background(), chained); err != nil || disposition != hook.QuestionBeginConflict {
		t.Fatalf("BeginQuestion() on corrupted seal = %s, err = %v", disposition, err)
	}
}
