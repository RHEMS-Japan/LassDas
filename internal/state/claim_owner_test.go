package state

import (
	"context"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
)

// A run's reports are bound to the identity it was claimed under. ClaimOwner
// returns exactly the owner the pull wrote into the run row, so an engine of
// a later revision can end a run its predecessor claimed (the ending was
// refused as terminal_report_conflict for ever, live 2026-09-05).
func TestClaimOwnerIsTheIdentityTheRunWasClaimedUnder(t *testing.T) {
	store := newLocalForTest(t)
	ctx := context.Background()
	envelope := localClaim(t, store)
	pull := testPullRequest(t)
	route := testTerminalRoute(t)
	route.ExpectedRunID = envelope.Snapshot.RunID

	owner, found, err := store.ClaimOwner(ctx, route)
	if err != nil || !found {
		t.Fatalf("ClaimOwner() = %+v, %v, %v", owner, found, err)
	}
	if owner != pull.Owner {
		t.Fatalf("claim owner = %+v, want the pull's owner %+v", owner, pull.Owner)
	}
	unknown := route
	unknown.ExpectedRunID = route.ProjectKey + "-999999"
	if _, found, err := store.ClaimOwner(ctx, unknown); err != nil || found {
		t.Fatalf("an unknown run answered %v, %v", found, err)
	}

	// The defect: a report under another engine revision is a conflict.
	startedAt := testQueuedAt.Add(2 * time.Minute)
	other := testTerminalBegin(t, envelope, hook.TerminalModelFailed, startedAt, strings.Repeat("a", 32))
	other.Report.WorkflowSHA = strings.Repeat("e", 40)
	other.Report.IssuedAt = startedAt
	body, err := hook.MarshalTerminalReportRecord(other.Report)
	if err != nil {
		t.Fatal(err)
	}
	other.ReportJSON, other.ReportSHA256 = string(body), hook.TerminalReportDigest(body)
	if _, disposition, err := store.BeginTerminal(ctx, other); err != nil || disposition != hook.TerminalBeginConflict {
		t.Fatalf("a report under another engine revision: %s, %v (want conflict)", disposition, err)
	}

	// The fix: a report built from ClaimOwner is acquired.
	mine := testTerminalBegin(t, envelope, hook.TerminalModelFailed, startedAt, strings.Repeat("b", 32))
	mine.Report.RepositoryID, mine.Report.RepositorySHA256, mine.Report.WorkflowRefSHA256 = owner.RepositoryID, owner.RepositorySHA256, owner.WorkflowRefSHA256
	mine.Report.WorkflowSHA, mine.Report.WorkflowRunID, mine.Report.RunAttempt = owner.WorkflowSHA, owner.WorkflowRunID, owner.RunAttempt
	mine.Report.IssuedAt = startedAt
	body, err = hook.MarshalTerminalReportRecord(mine.Report)
	if err != nil {
		t.Fatal(err)
	}
	mine.ReportJSON, mine.ReportSHA256 = string(body), hook.TerminalReportDigest(body)
	if _, disposition, err := store.BeginTerminal(ctx, mine); err != nil || disposition != hook.TerminalBeginAcquired {
		t.Fatalf("a report under the claim owner: %s, %v (want acquired)", disposition, err)
	}
	if owner.WorkflowRunID != 7663335643410923834 {
		t.Fatalf("workflow run id lost precision: %d", owner.WorkflowRunID)
	}
}

// The DynamoDB store answers the same owner from its run item.
func TestDynamoClaimOwnerIsTheIdentityTheRunWasClaimedUnder(t *testing.T) {
	ctx := context.Background()
	store, err := NewDynamoStore("ticket-automation-state", newMemoryDynamo())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Enqueue(ctx, testQueueRequest(t)); err != nil {
		t.Fatal(err)
	}
	pull := testPullRequest(t)
	envelope, disposition, err := store.Pull(ctx, pull)
	if err != nil || disposition != hook.PullAcquired {
		t.Fatalf("Pull() = %s, %v", disposition, err)
	}
	route := testTerminalRoute(t)
	route.ExpectedRunID = envelope.Snapshot.RunID
	owner, found, err := store.ClaimOwner(ctx, route)
	if err != nil || !found || owner != pull.Owner {
		t.Fatalf("ClaimOwner() = %+v, %v, %v; want %+v", owner, found, err, pull.Owner)
	}
}
