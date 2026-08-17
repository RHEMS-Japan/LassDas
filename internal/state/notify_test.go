package state

import (
	"context"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
)

func notifyBeginFromRecord(t *testing.T, record hook.QuestionRecord, index int, startedAt time.Time, token string) hook.NotifyBeginRequest {
	t.Helper()
	body, err := hook.MarshalQuestionRecord(record)
	if err != nil {
		t.Fatalf("MarshalQuestionRecord() error = %v", err)
	}
	route := testTerminalRoute(t)
	return hook.NotifyBeginRequest{
		Record: record, RecordJSON: string(body), RecordSHA256: hook.TerminalReportDigest(body), Route: route,
		Index: index, StartedAt: startedAt, LeaseUntil: startedAt.Add(route.LeaseDuration), LeaseToken: token,
	}
}

func testNotifyBegin(t *testing.T, envelope hook.DispatchEnvelope, index int, startedAt time.Time, token string) hook.NotifyBeginRequest {
	t.Helper()
	return notifyBeginFromRecord(t, testQuestionRecord(t, envelope), index, startedAt, token)
}

// notifyDueAt is the sealed slot time of notification index for the test
// question record.
func notifyDueAt(t *testing.T, envelope hook.DispatchEnvelope, index int) time.Time {
	t.Helper()
	record := testQuestionRecord(t, envelope)
	return time.UnixMilli(record.NotifyAt[index-1]).UTC()
}

func testNotifyComplete(t *testing.T, envelope hook.DispatchEnvelope, index int, token string, commentID int64, postedAt time.Time) hook.NotifyCompleteRequest {
	t.Helper()
	record := testQuestionRecord(t, envelope)
	body, err := hook.MarshalQuestionRecord(record)
	if err != nil {
		t.Fatalf("MarshalQuestionRecord() error = %v", err)
	}
	return hook.NotifyCompleteRequest{
		Record: record, RecordJSON: string(body), RecordSHA256: hook.TerminalReportDigest(body),
		Route: testTerminalRoute(t), Index: index, LeaseToken: token, CommentID: commentID, PostedAt: postedAt,
	}
}

func TestBeginNotifyAcquiresOneMarkerPerIndex(t *testing.T) {
	api := newMemoryDynamo()
	store := testStore(t, api)
	envelope := awaitForTest(t, store)
	begin := testNotifyBegin(t, envelope, 1, notifyDueAt(t, envelope, 1).Add(6*time.Second), strings.Repeat("a", 32))
	if _, disposition, err := store.BeginNotify(context.Background(), begin); err != nil || disposition != hook.NotifyBeginAcquired {
		t.Fatalf("BeginNotify() disposition = %s, err = %v", disposition, err)
	}
	_, runKey := itemKeys(envelope)
	marker := api.items[notifyMarkerKey(runKey, 1, 1)]
	if marker == nil {
		t.Fatal("notify marker was not written")
	}
	if !attributeStringEquals(marker, "question_record_sha256", begin.RecordSHA256) ||
		!attributeInt64Equals(marker, "notify_index", 1) ||
		!attributeInt64Equals(marker, "notify_at", begin.Record.NotifyAt[0]) {
		t.Fatalf("marker binding = %+v", marker)
	}

	// The same index under a live lease is busy for any other sender; a
	// different index is independent.
	other := testNotifyBegin(t, envelope, 1, notifyDueAt(t, envelope, 1).Add(7*time.Second), strings.Repeat("b", 32))
	if _, disposition, err := store.BeginNotify(context.Background(), other); err != nil || disposition != hook.NotifyBeginBusy {
		t.Fatalf("BeginNotify() during lease = %s, err = %v", disposition, err)
	}
	second := testNotifyBegin(t, envelope, 2, notifyDueAt(t, envelope, 2).Add(7*time.Second), strings.Repeat("c", 32))
	if _, disposition, err := store.BeginNotify(context.Background(), second); err != nil || disposition != hook.NotifyBeginAcquired {
		t.Fatalf("BeginNotify() for index 2 = %s, err = %v", disposition, err)
	}
}

func TestCompleteNotifyBindsCommentExactlyOnce(t *testing.T) {
	api := newMemoryDynamo()
	store := testStore(t, api)
	envelope := awaitForTest(t, store)
	token := strings.Repeat("a", 32)
	begin := testNotifyBegin(t, envelope, 1, notifyDueAt(t, envelope, 1).Add(6*time.Second), token)
	if _, disposition, err := store.BeginNotify(context.Background(), begin); err != nil || disposition != hook.NotifyBeginAcquired {
		t.Fatalf("BeginNotify() disposition = %s, err = %v", disposition, err)
	}
	complete := testNotifyComplete(t, envelope, 1, token, 6101, notifyDueAt(t, envelope, 1).Add(7*time.Second))
	if disposition, err := store.CompleteNotify(context.Background(), complete); err != nil || disposition != hook.NotifyCompleted {
		t.Fatalf("CompleteNotify() disposition = %s, err = %v", disposition, err)
	}
	assertNoUnusedExpressionBindings(t, api.lastTransaction.TransactItems[0].Update)
	if disposition, err := store.CompleteNotify(context.Background(), complete); err != nil || disposition != hook.NotifyAlreadyComplete {
		t.Fatalf("duplicate CompleteNotify() = %s, err = %v", disposition, err)
	}
	rebound := testNotifyComplete(t, envelope, 1, token, 6102, notifyDueAt(t, envelope, 1).Add(8*time.Second))
	if disposition, err := store.CompleteNotify(context.Background(), rebound); err != nil || disposition != hook.NotifyCompleteConflict {
		t.Fatalf("rebinding CompleteNotify() = %s, err = %v", disposition, err)
	}
	if _, disposition, err := store.BeginNotify(context.Background(), begin); err != nil || disposition != hook.NotifyBeginComplete {
		t.Fatalf("BeginNotify() after completion = %s, err = %v", disposition, err)
	}
	_, runKey := itemKeys(envelope)
	marker := api.items[notifyMarkerKey(runKey, 1, 1)]
	if !attributeInt64Equals(marker, "notify_comment_id", 6101) {
		t.Fatal("marker does not hold the bound comment")
	}
	if _, exists := marker["notify_lease_token"]; exists {
		t.Fatal("lease survived completion")
	}
}

func TestBeginNotifyRefusesRunsThatAreNotWaiting(t *testing.T) {
	// A run that never posted the question cannot be renotified.
	api := newMemoryDynamo()
	store := testStore(t, api)
	envelope := claimForTerminal(t, store)
	begin := testNotifyBegin(t, envelope, 1, notifyDueAt(t, envelope, 1).Add(6*time.Second), strings.Repeat("a", 32))
	if _, disposition, err := store.BeginNotify(context.Background(), begin); err != nil || disposition != hook.NotifyBeginConflict {
		t.Fatalf("BeginNotify() on claimed = %s, err = %v", disposition, err)
	}

	// After the expiry begins, no further reminder goes out.
	api = newMemoryDynamo()
	store = testStore(t, api)
	envelope = awaitForTest(t, store)
	expiry := testTerminalBegin(t, envelope, hook.TerminalClarificationExpired, testQueuedAt.Add(6*time.Second), strings.Repeat("e", 32))
	if _, disposition, err := store.BeginTerminal(context.Background(), expiry); err != nil || disposition != hook.TerminalBeginAcquired {
		t.Fatalf("BeginTerminal() disposition = %s, err = %v", disposition, err)
	}
	begin = testNotifyBegin(t, envelope, 1, notifyDueAt(t, envelope, 1).Add(7*time.Second), strings.Repeat("a", 32))
	if _, disposition, err := store.BeginNotify(context.Background(), begin); err != nil || disposition != hook.NotifyBeginConflict {
		t.Fatalf("BeginNotify() after expiry begin = %s, err = %v", disposition, err)
	}

	// After the adopted answer resumed the run, a reminder is a conflict too.
	api = newMemoryDynamo()
	store = testStore(t, api)
	envelope, _ = resumeForTest(t, store)
	begin = testNotifyBegin(t, envelope, 1, notifyDueAt(t, envelope, 1).Add(8*time.Second), strings.Repeat("a", 32))
	if _, disposition, err := store.BeginNotify(context.Background(), begin); err != nil || disposition != hook.NotifyBeginConflict {
		t.Fatalf("BeginNotify() after resume = %s, err = %v", disposition, err)
	}
}

func TestBeginNotifyRetryReacquiresExpiredLease(t *testing.T) {
	api := newMemoryDynamo()
	store := testStore(t, api)
	envelope := awaitForTest(t, store)
	route := testTerminalRoute(t)
	first := testNotifyBegin(t, envelope, 1, notifyDueAt(t, envelope, 1).Add(6*time.Second), strings.Repeat("a", 32))
	if _, disposition, err := store.BeginNotify(context.Background(), first); err != nil || disposition != hook.NotifyBeginAcquired {
		t.Fatalf("BeginNotify() disposition = %s, err = %v", disposition, err)
	}
	// After the lease runs out a new sender may take over; the stale token
	// can no longer complete.
	retry := testNotifyBegin(t, envelope, 1, notifyDueAt(t, envelope, 1).Add(6*time.Second).Add(route.LeaseDuration).Add(time.Second), strings.Repeat("b", 32))
	if _, disposition, err := store.BeginNotify(context.Background(), retry); err != nil || disposition != hook.NotifyBeginAcquired {
		t.Fatalf("BeginNotify() after lease expiry = %s, err = %v", disposition, err)
	}
	assertNoUnusedExpressionBindings(t, api.lastTransaction.TransactItems[2].Update)
	stale := testNotifyComplete(t, envelope, 1, strings.Repeat("a", 32), 6101, retry.StartedAt.Add(time.Second))
	if disposition, err := store.CompleteNotify(context.Background(), stale); err != nil || disposition != hook.NotifyCompleteConflict {
		t.Fatalf("stale CompleteNotify() = %s, err = %v", disposition, err)
	}
	fresh := testNotifyComplete(t, envelope, 1, strings.Repeat("b", 32), 6101, retry.StartedAt.Add(time.Second))
	if disposition, err := store.CompleteNotify(context.Background(), fresh); err != nil || disposition != hook.NotifyCompleted {
		t.Fatalf("fresh CompleteNotify() = %s, err = %v", disposition, err)
	}
}

func TestBeginNotifyValidatesRequestBeforeAnyWrite(t *testing.T) {
	api := newMemoryDynamo()
	store := testStore(t, api)
	envelope := awaitForTest(t, store)
	valid := testNotifyBegin(t, envelope, 1, notifyDueAt(t, envelope, 1).Add(6*time.Second), strings.Repeat("a", 32))
	writesBefore := api.lastTransaction
	for _, run := range []struct {
		name   string
		mutate func(request *hook.NotifyBeginRequest)
	}{
		{name: "index below range", mutate: func(request *hook.NotifyBeginRequest) { request.Index = 0 }},
		{name: "index above range", mutate: func(request *hook.NotifyBeginRequest) { request.Index = hook.QuestionNotifyCount + 1 }},
		{name: "record json is not canonical", mutate: func(request *hook.NotifyBeginRequest) { request.RecordJSON += " " }},
		{name: "record digest mismatch", mutate: func(request *hook.NotifyBeginRequest) { request.RecordSHA256 = strings.Repeat("0", 64) }},
		{name: "lease token malformed", mutate: func(request *hook.NotifyBeginRequest) { request.LeaseToken = "not-a-token" }},
		{name: "lease detached from route duration", mutate: func(request *hook.NotifyBeginRequest) { request.LeaseUntil = request.LeaseUntil.Add(time.Second) }},
		{name: "begin before the sealed slot", mutate: func(request *hook.NotifyBeginRequest) {
			request.StartedAt = request.StartedAt.Add(-25 * time.Hour)
			request.LeaseUntil = request.LeaseUntil.Add(-25 * time.Hour)
		}},
	} {
		t.Run(run.name, func(t *testing.T) {
			request := valid
			run.mutate(&request)
			_, disposition, err := store.BeginNotify(context.Background(), request)
			class, code := hook.FailureDetails(err)
			if disposition != "" || class != hook.FailureRejected || code != "invalid_notify_begin" {
				t.Fatalf("BeginNotify() = %s, class = %s, code = %s", disposition, class, code)
			}
		})
	}
	if api.lastTransaction != writesBefore {
		t.Fatal("an invalid notify begin reached the store")
	}
}

func TestConcurrentNotifyBeginHasOneLeaseOwner(t *testing.T) {
	api := newMemoryDynamo()
	store := testStore(t, api)
	envelope := awaitForTest(t, store)
	first := testNotifyBegin(t, envelope, 1, notifyDueAt(t, envelope, 1).Add(6*time.Second), strings.Repeat("a", 32))
	second := testNotifyBegin(t, envelope, 1, notifyDueAt(t, envelope, 1).Add(6*time.Second), strings.Repeat("b", 32))
	_, firstDisposition, firstErr := store.BeginNotify(context.Background(), first)
	_, secondDisposition, secondErr := store.BeginNotify(context.Background(), second)
	if firstErr != nil || secondErr != nil {
		t.Fatalf("BeginNotify() errors = %v, %v", firstErr, secondErr)
	}
	if firstDisposition != hook.NotifyBeginAcquired || secondDisposition != hook.NotifyBeginBusy {
		t.Fatalf("dispositions = %s, %s; want acquired then busy", firstDisposition, secondDisposition)
	}
	_, runKey := itemKeys(envelope)
	if !attributeStringEquals(api.items[notifyMarkerKey(runKey, 1, 1)], "notify_lease_token", strings.Repeat("a", 32)) {
		t.Fatal("second begin stole the live lease")
	}
}

func TestBeginNotifyResolvesDeadLeasedMarkerToConflict(t *testing.T) {
	api := newMemoryDynamo()
	store := testStore(t, api)
	envelope := awaitForTest(t, store)
	first := testNotifyBegin(t, envelope, 1, notifyDueAt(t, envelope, 1).Add(6*time.Second), strings.Repeat("a", 32))
	if _, disposition, err := store.BeginNotify(context.Background(), first); err != nil || disposition != hook.NotifyBeginAcquired {
		t.Fatalf("BeginNotify() disposition = %s, err = %v", disposition, err)
	}
	// The run expires while the notification is still leased: the marker is a
	// dead notification now and every later begin must say stop (conflict),
	// not retry-later (busy).
	expiry := testTerminalBegin(t, envelope, hook.TerminalClarificationExpired, testQueuedAt.Add(6*time.Second), strings.Repeat("e", 32))
	if _, disposition, err := store.BeginTerminal(context.Background(), expiry); err != nil || disposition != hook.TerminalBeginAcquired {
		t.Fatalf("BeginTerminal() disposition = %s, err = %v", disposition, err)
	}
	during := testNotifyBegin(t, envelope, 1, notifyDueAt(t, envelope, 1).Add(7*time.Second), strings.Repeat("b", 32))
	if _, disposition, err := store.BeginNotify(context.Background(), during); err != nil || disposition != hook.NotifyBeginConflict {
		t.Fatalf("BeginNotify() on dead leased marker = %s, err = %v", disposition, err)
	}
	route := testTerminalRoute(t)
	after := testNotifyBegin(t, envelope, 1, notifyDueAt(t, envelope, 1).Add(6*time.Second).Add(route.LeaseDuration).Add(time.Second), strings.Repeat("c", 32))
	if _, disposition, err := store.BeginNotify(context.Background(), after); err != nil || disposition != hook.NotifyBeginConflict {
		t.Fatalf("BeginNotify() after dead lease expiry = %s, err = %v", disposition, err)
	}
}

func TestNotifyMarkersOfTheSecondRoundAreIndependent(t *testing.T) {
	api := newMemoryDynamo()
	store := testStore(t, api)
	envelope, _, roundTwoQuestion := awaitRoundTwoForTest(t, store)
	dueAt := time.UnixMilli(roundTwoQuestion.NotifyAt[0]).UTC()
	begin := notifyBeginFromRecord(t, roundTwoQuestion, 1, dueAt.Add(6*time.Second), strings.Repeat("a", 32))
	if _, disposition, err := store.BeginNotify(context.Background(), begin); err != nil || disposition != hook.NotifyBeginAcquired {
		t.Fatalf("BeginNotify() for round 2 = %s, err = %v", disposition, err)
	}
	_, runKey := itemKeys(envelope)
	if api.items[notifyMarkerKey(runKey, 2, 1)] == nil {
		t.Fatal("round-2 marker was not written under its own revision key")
	}
	if api.items[notifyMarkerKey(runKey, 1, 1)] != nil {
		t.Fatal("round-2 begin touched the round-1 key space")
	}
	// A stale round-1 reminder cannot fire against the round-2 wait.
	stale := testNotifyBegin(t, envelope, 1, dueAt.Add(7*time.Second), strings.Repeat("b", 32))
	if _, disposition, err := store.BeginNotify(context.Background(), stale); err != nil || disposition != hook.NotifyBeginConflict {
		t.Fatalf("BeginNotify() with the round-1 record = %s, err = %v", disposition, err)
	}
}

func TestNotifyBeginWriteFailureIsRetryableWhenStateUnmoved(t *testing.T) {
	api := newMemoryDynamo()
	store := testStore(t, api)
	envelope := awaitForTest(t, store)
	api.transactionErr = transactionCanceled()
	begin := testNotifyBegin(t, envelope, 1, notifyDueAt(t, envelope, 1).Add(6*time.Second), strings.Repeat("a", 32))
	_, disposition, err := store.BeginNotify(context.Background(), begin)
	class, code := hook.FailureDetails(err)
	if disposition != "" || class != hook.FailureRetryable || code != "notify_begin_write_failed" {
		t.Fatalf("BeginNotify() = %s, class = %s, code = %s", disposition, class, code)
	}
	api.transactionErr = nil
	if _, disposition, err := store.BeginNotify(context.Background(), begin); err != nil || disposition != hook.NotifyBeginAcquired {
		t.Fatalf("retried BeginNotify() = %s, err = %v", disposition, err)
	}
}
