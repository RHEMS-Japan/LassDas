package state

import (
	"context"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
)

// A tracker project key may carry an underscore. The run the engine keeps is
// named for the ticket (PROJECT_KEY-9), while the route a deployment holds
// names the deployment's own run id; the pending row is what ties the two
// together. When that rebinding refused underscores, a question asked on
// such a project could never be answered: the tick found no waiting run and
// said "idle" for ever (live 2026-09-17).
func TestQuestionWaitIsFoundWhenTheProjectKeyCarriesAnUnderscore(t *testing.T) {
	for _, projectKey := range []string{"TICKET", "RHEMS_TEST", "A"} {
		t.Run(projectKey, func(t *testing.T) {
			store := newLocalForTest(t)
			ctx := context.Background()
			envelope := underscoreEnvelope(t, projectKey)

			if _, err := store.Enqueue(ctx, hook.QueueRequest{Envelope: envelope, QueuedAt: testQueuedAt}); err != nil {
				t.Fatalf("Enqueue() error = %v", err)
			}
			pull := testPullRequest(t)
			pull.ProjectKey = envelope.Snapshot.ProjectKey
			pull.RunID = envelope.Snapshot.RunID
			pull.SpaceKey = envelope.Snapshot.SpaceKey
			pull.ProjectID = envelope.Snapshot.ProjectID
			if _, _, err := store.Pull(ctx, pull); err != nil {
				t.Fatalf("Pull() error = %v", err)
			}

			// The run's own route: a run is named for its ticket, so the
			// question is sealed under that name.
			ticketRoute := underscoreRoute(t, envelope, envelope.Snapshot.RunID)
			record := testQuestionRecord(t, envelope)
			record.AutomationRunID = envelope.Snapshot.RunID
			body, err := hook.MarshalQuestionRecord(record)
			if err != nil {
				t.Fatalf("MarshalQuestionRecord() error = %v", err)
			}
			token := strings.Repeat("a", 32)
			startedAt := testQueuedAt.Add(3 * time.Second)
			if _, _, err := store.BeginQuestion(ctx, hook.QuestionBeginRequest{
				Record: record, RecordJSON: string(body), RecordSHA256: hook.TerminalReportDigest(body), Route: ticketRoute,
				StartedAt: startedAt, LeaseUntil: startedAt.Add(ticketRoute.LeaseDuration), LeaseToken: token,
			}); err != nil {
				t.Fatalf("BeginQuestion() error = %v", err)
			}
			if _, err := store.CompleteQuestion(ctx, hook.QuestionCompleteRequest{
				Record: record, RecordJSON: string(body), RecordSHA256: hook.TerminalReportDigest(body), Route: ticketRoute,
				LeaseToken: token, CommentID: 6001, PostedAt: testQueuedAt.Add(4 * time.Second),
			}); err != nil {
				t.Fatalf("CompleteQuestion() error = %v", err)
			}

			// The route a running deployment holds: its own run id, not the
			// ticket's. Resolving it is what the tick depends on.
			route := underscoreRoute(t, envelope, "run_20260909_deployment")

			notice, err := store.LoadRunNotice(ctx, route)
			if err != nil || !notice.Exists {
				t.Fatalf("LoadRunNotice() exists=%v err=%v; the tick stops here when this is false", notice.Exists, err)
			}
			wait, waiting, err := store.LoadQuestionWait(ctx, route)
			if err != nil || !waiting {
				t.Fatalf("LoadQuestionWait() waiting=%v err=%v; the posted answer is never adopted when this is false", waiting, err)
			}
			if wait.QuestionCommentID != 6001 {
				t.Fatalf("question comment = %d, want the one the ledger holds", wait.QuestionCommentID)
			}
		})
	}
}

// underscoreEnvelope is the shared fixture with the project key and the
// ticket-shaped run id this test needs.
func underscoreEnvelope(t *testing.T, projectKey string) hook.DispatchEnvelope {
	t.Helper()
	snapshot := testEnvelope(t).Snapshot
	snapshot.DeliveryID, snapshot.InputSHA256 = "", ""
	snapshot.ProjectKey = projectKey
	// A ticket key must be at least eight characters (ValidateEnvelope), so
	// the number here is one a one-character project key can carry too.
	snapshot.IssueKeyID = 12345678
	snapshot.IssueKey = projectKey + "-12345678"
	snapshot.RunID = snapshot.IssueKey
	envelope, err := hook.SealSnapshot(snapshot)
	if err != nil {
		t.Fatalf("SealSnapshot() error = %v", err)
	}
	return envelope
}

// underscoreRoute is the shared route fixture pointed at one envelope, with
// the run id the caller names.
func underscoreRoute(t *testing.T, envelope hook.DispatchEnvelope, runID string) hook.ReportRouteConfig {
	t.Helper()
	route := testTerminalRoute(t)
	route.SpaceKey = envelope.Snapshot.SpaceKey
	route.ProjectID = envelope.Snapshot.ProjectID
	route.ProjectKey = envelope.Snapshot.ProjectKey
	route.AllowedCreatorID = envelope.Snapshot.CreatorID
	route.AllowedActivityType = envelope.Snapshot.ActivityType
	route.Target = envelope.Snapshot.Target
	route.ExpectedRunID = runID
	return route
}

// The gate both stores ask before they rebind a route. It admits exactly the
// ticket names of this project: everything before the single dash is pinned
// by the prefix, and the shape is the one the ingest can write.
func TestReboundRunIDAdmitsOnlyThisProjectsTickets(t *testing.T) {
	route := testTerminalRoute(t)
	route.ProjectKey = "TICKET"
	admitted := []string{"TICKET-1", "TICKET-501", "TICKET-999999999"}
	refused := []string{
		"", "TICKET", "TICKET-", "TICKET-0", "TICKET-09", "TICKET-1-EXTRA", "TICKET-1 ", "TICKET-x",
		"TICKET_QA-9",         // another project whose key begins the same way
		"OTHER-1", "ticket-1", // another project, and a shape the ingest never writes
		"TICKET-1234567890", // more digits than a tracker issue number
	}
	for _, runID := range admitted {
		if !reboundRunID(route, runID) {
			t.Errorf("refused a ticket of this project: %q", runID)
		}
	}
	for _, runID := range refused {
		if reboundRunID(route, runID) {
			t.Errorf("admitted %q", runID)
		}
	}
	// A project key that is a prefix of another must not reach its tickets.
	other := route
	other.ProjectKey = "TICKET_QA"
	if !reboundRunID(other, "TICKET_QA-9") || reboundRunID(other, "TICKET-9") {
		t.Fatal("the prefix no longer ties a run id to its project")
	}
	// The shape is the tracker's: keys are upper case, with no spaces. A
	// route naming anything else rebinds to nothing, whatever its own
	// prefix says.
	for _, odd := range []struct{ key, runID string }{
		{"ticket", "ticket-1"},
		{"TICK ET", "TICK ET-1"},
		{"TICKET.QA", "TICKET.QA-1"},
	} {
		wrong := route
		wrong.ProjectKey = odd.key
		if reboundRunID(wrong, odd.runID) {
			t.Errorf("admitted a run id of a shape the tracker never writes: %q", odd.runID)
		}
	}
	// A one-character key is a real tracker key, and a hundred-character one
	// is the bound.
	short := route
	short.ProjectKey = "A"
	if !reboundRunID(short, "A-12345678") {
		t.Fatal("a one-character project key was refused")
	}
	long := route
	long.ProjectKey = "B" + strings.Repeat("C", 99)
	if !reboundRunID(long, long.ProjectKey+"-1") {
		t.Fatal("a hundred-character project key was refused")
	}
	tooLong := route
	tooLong.ProjectKey = "B" + strings.Repeat("C", 100)
	if reboundRunID(tooLong, tooLong.ProjectKey+"-1") {
		t.Fatal("the length bound does not hold")
	}
}
