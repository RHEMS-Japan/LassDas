package state

import (
	"context"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
)

// A ticket outlives the engine's revisions. It is claimed under one, asked a
// question, and answered later - possibly after the body has been replaced.
// The tick must still find that run: comparing the whole delivery target
// bound the ticket to the revision that ingested it, so an upgrade stranded
// every waiting ticket, and the answer was never adopted (live 2026-09-17,
// measured on the instance's own ledger).
func TestAWaitingTicketSurvivesAnEngineUpgrade(t *testing.T) {
	store := newLocalForTest(t)
	ctx := context.Background()
	envelope := underscoreEnvelope(t, "TICKET")

	if _, err := store.Enqueue(ctx, hook.QueueRequest{Envelope: envelope, QueuedAt: testQueuedAt}); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	pull := testPullRequest(t)
	pull.ProjectKey, pull.RunID = envelope.Snapshot.ProjectKey, envelope.Snapshot.RunID
	pull.SpaceKey, pull.ProjectID = envelope.Snapshot.SpaceKey, envelope.Snapshot.ProjectID
	if _, _, err := store.Pull(ctx, pull); err != nil {
		t.Fatalf("Pull() error = %v", err)
	}
	route := underscoreRoute(t, envelope, envelope.Snapshot.RunID)
	record := testQuestionRecord(t, envelope)
	record.AutomationRunID = envelope.Snapshot.RunID
	body, err := hook.MarshalQuestionRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("a", 32)
	startedAt := testQueuedAt.Add(3 * time.Second)
	if _, _, err := store.BeginQuestion(ctx, hook.QuestionBeginRequest{
		Record: record, RecordJSON: string(body), RecordSHA256: hook.TerminalReportDigest(body), Route: route,
		StartedAt: startedAt, LeaseUntil: startedAt.Add(route.LeaseDuration), LeaseToken: token,
	}); err != nil {
		t.Fatalf("BeginQuestion() error = %v", err)
	}
	if _, err := store.CompleteQuestion(ctx, hook.QuestionCompleteRequest{
		Record: record, RecordJSON: string(body), RecordSHA256: hook.TerminalReportDigest(body), Route: route,
		LeaseToken: token, CommentID: 6001, PostedAt: startedAt.Add(time.Second),
	}); err != nil {
		t.Fatalf("CompleteQuestion() error = %v", err)
	}

	// The body is replaced: same delivery repository, a new revision, so a
	// new workflow reference.
	upgraded := route
	upgraded.WorkflowRefSHA256 = strings.Repeat("e", 64)
	upgraded.Target.WorkflowRefSHA256 = upgraded.WorkflowRefSHA256

	notice, err := store.LoadRunNotice(ctx, upgraded)
	if err != nil || !notice.Exists {
		t.Fatalf("LoadRunNotice() exists=%v err=%v; the tick stops here", notice.Exists, err)
	}
	wait, waiting, err := store.LoadQuestionWait(ctx, upgraded)
	if err != nil || !waiting || wait.QuestionCommentID != 6001 {
		t.Fatalf("LoadQuestionWait() waiting=%v comment=%d err=%v; the posted answer is never adopted", waiting, wait.QuestionCommentID, err)
	}

	// Another delivery repository is still another delivery.
	elsewhere := upgraded
	elsewhere.Target.RepositoryID++
	if notice, err := store.LoadRunNotice(ctx, elsewhere); err == nil && notice.Exists {
		t.Fatal("a route for another repository reached this ticket")
	}
}
