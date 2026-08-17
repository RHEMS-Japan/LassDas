package state

import (
	"context"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
)

func testReplyBegin(t *testing.T, envelope hook.DispatchEnvelope, kind hook.ReplyKind, trigger int64, startedAt time.Time, token string) hook.ReplyBeginRequest {
	t.Helper()
	record := testQuestionRecord(t, envelope)
	body, err := hook.MarshalQuestionRecord(record)
	if err != nil {
		t.Fatalf("MarshalQuestionRecord() error = %v", err)
	}
	route := testTerminalRoute(t)
	return hook.ReplyBeginRequest{
		Record: record, RecordJSON: string(body), RecordSHA256: hook.TerminalReportDigest(body), Route: route,
		Kind: kind, TriggerCommentID: trigger, ContentSHA256: strings.Repeat("d", 64),
		StartedAt: startedAt, LeaseUntil: startedAt.Add(route.LeaseDuration), LeaseToken: token,
	}
}

func replyCompleteFromBegin(begin hook.ReplyBeginRequest, commentID int64, postedAt time.Time) hook.ReplyCompleteRequest {
	return hook.ReplyCompleteRequest{
		Record: begin.Record, RecordJSON: begin.RecordJSON, RecordSHA256: begin.RecordSHA256, Route: begin.Route,
		Kind: begin.Kind, TriggerCommentID: begin.TriggerCommentID, ContentSHA256: begin.ContentSHA256,
		LeaseToken: begin.LeaseToken, CommentID: commentID, PostedAt: postedAt,
	}
}

func TestReplyMarkersBindOnceAndFeedIntakeState(t *testing.T) {
	api := newMemoryDynamo()
	store := testStore(t, api)
	envelope := awaitForTest(t, store)
	route := testTerminalRoute(t)
	record := testQuestionRecord(t, envelope)

	// The shortfall marker is per incomplete comment and binds exactly once.
	begin := testReplyBegin(t, envelope, hook.ReplyShortfall, 6002, testQueuedAt.Add(6*time.Second), strings.Repeat("a", 32))
	if _, disposition, err := store.BeginReply(context.Background(), begin); err != nil || disposition != hook.ReplyBeginAcquired {
		t.Fatalf("BeginReply() disposition = %s, err = %v", disposition, err)
	}
	if handled, err := store.ReplyState(context.Background(), route, record, hook.ReplyShortfall, 6002); err != nil || handled {
		t.Fatalf("ReplyState() before completion = %v, err = %v", handled, err)
	}
	complete := replyCompleteFromBegin(begin, 6101, testQueuedAt.Add(7*time.Second))
	if disposition, err := store.CompleteReply(context.Background(), complete); err != nil || disposition != hook.ReplyCompleted {
		t.Fatalf("CompleteReply() disposition = %s, err = %v", disposition, err)
	}
	assertNoUnusedExpressionBindings(t, api.lastTransaction.TransactItems[0].Update)
	if disposition, err := store.CompleteReply(context.Background(), complete); err != nil || disposition != hook.ReplyAlreadyComplete {
		t.Fatalf("duplicate CompleteReply() = %s, err = %v", disposition, err)
	}
	if _, disposition, err := store.BeginReply(context.Background(), begin); err != nil || disposition != hook.ReplyBeginComplete {
		t.Fatalf("BeginReply() after completion = %s, err = %v", disposition, err)
	}
	if handled, err := store.ReplyState(context.Background(), route, record, hook.ReplyShortfall, 6002); err != nil || !handled {
		t.Fatalf("ReplyState() after completion = %v, err = %v", handled, err)
	}
	// A different incomplete comment owns an independent marker.
	other := testReplyBegin(t, envelope, hook.ReplyShortfall, 6003, testQueuedAt.Add(8*time.Second), strings.Repeat("b", 32))
	if _, disposition, err := store.BeginReply(context.Background(), other); err != nil || disposition != hook.ReplyBeginAcquired {
		t.Fatalf("BeginReply() for another trigger = %s, err = %v", disposition, err)
	}
}

func TestGuidanceMarkerIsSingularPerRevision(t *testing.T) {
	api := newMemoryDynamo()
	store := testStore(t, api)
	envelope := awaitForTest(t, store)
	route := testTerminalRoute(t)
	record := testQuestionRecord(t, envelope)

	first := testReplyBegin(t, envelope, hook.ReplyGuidance, 6002, testQueuedAt.Add(6*time.Second), strings.Repeat("a", 32))
	if _, disposition, err := store.BeginReply(context.Background(), first); err != nil || disposition != hook.ReplyBeginAcquired {
		t.Fatalf("BeginReply() disposition = %s, err = %v", disposition, err)
	}
	// Guidance for a different trigger comment lands on the same singular
	// marker: busy while leased, complete after binding — never a second post.
	otherTrigger := testReplyBegin(t, envelope, hook.ReplyGuidance, 6003, testQueuedAt.Add(7*time.Second), strings.Repeat("b", 32))
	if _, disposition, err := store.BeginReply(context.Background(), otherTrigger); err != nil || disposition != hook.ReplyBeginBusy {
		t.Fatalf("BeginReply() during guidance lease = %s, err = %v", disposition, err)
	}
	complete := replyCompleteFromBegin(first, 6102, testQueuedAt.Add(8*time.Second))
	if disposition, err := store.CompleteReply(context.Background(), complete); err != nil || disposition != hook.ReplyCompleted {
		t.Fatalf("CompleteReply() disposition = %s, err = %v", disposition, err)
	}
	if _, disposition, err := store.BeginReply(context.Background(), otherTrigger); err != nil || disposition != hook.ReplyBeginComplete {
		t.Fatalf("BeginReply() after guidance bound = %s, err = %v", disposition, err)
	}
	if sent, err := store.ReplyState(context.Background(), route, record, hook.ReplyGuidance, 0); err != nil || !sent {
		t.Fatalf("ReplyState() guidance = %v, err = %v", sent, err)
	}
}

func TestReplyRefusesRunsThatAreNotWaiting(t *testing.T) {
	api := newMemoryDynamo()
	store := testStore(t, api)
	envelope := awaitForTest(t, store)
	expiry := testTerminalBegin(t, envelope, hook.TerminalClarificationExpired, testQueuedAt.Add(6*time.Second), strings.Repeat("e", 32))
	if _, disposition, err := store.BeginTerminal(context.Background(), expiry); err != nil || disposition != hook.TerminalBeginAcquired {
		t.Fatalf("BeginTerminal() disposition = %s, err = %v", disposition, err)
	}
	begin := testReplyBegin(t, envelope, hook.ReplyShortfall, 6002, testQueuedAt.Add(7*time.Second), strings.Repeat("a", 32))
	if _, disposition, err := store.BeginReply(context.Background(), begin); err != nil || disposition != hook.ReplyBeginConflict {
		t.Fatalf("BeginReply() after expiry begin = %s, err = %v", disposition, err)
	}
}

func TestReplyValidatesRequestBeforeAnyWrite(t *testing.T) {
	api := newMemoryDynamo()
	store := testStore(t, api)
	envelope := awaitForTest(t, store)
	valid := testReplyBegin(t, envelope, hook.ReplyShortfall, 6002, testQueuedAt.Add(6*time.Second), strings.Repeat("a", 32))
	writesBefore := api.lastTransaction
	for _, run := range []struct {
		name   string
		mutate func(request *hook.ReplyBeginRequest)
	}{
		{name: "unknown kind", mutate: func(request *hook.ReplyBeginRequest) { request.Kind = "note" }},
		{name: "missing trigger", mutate: func(request *hook.ReplyBeginRequest) { request.TriggerCommentID = 0 }},
		{name: "content digest malformed", mutate: func(request *hook.ReplyBeginRequest) { request.ContentSHA256 = "not-a-digest" }},
		{name: "record digest mismatch", mutate: func(request *hook.ReplyBeginRequest) { request.RecordSHA256 = strings.Repeat("0", 64) }},
	} {
		t.Run(run.name, func(t *testing.T) {
			request := valid
			run.mutate(&request)
			_, disposition, err := store.BeginReply(context.Background(), request)
			class, code := hook.FailureDetails(err)
			if disposition != "" || class != hook.FailureRejected || code != "invalid_reply_begin" {
				t.Fatalf("BeginReply() = %s, class = %s, code = %s", disposition, class, code)
			}
		})
	}
	if api.lastTransaction != writesBefore {
		t.Fatal("an invalid reply begin reached the store")
	}
}
