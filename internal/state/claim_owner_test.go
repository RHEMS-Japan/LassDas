package state

import (
	"context"
	"testing"
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

	owner, found, err := store.ClaimOwner(ctx, envelope.Snapshot.RunID, route)
	if err != nil || !found {
		t.Fatalf("ClaimOwner() = %+v, %v, %v", owner, found, err)
	}
	if owner != pull.Owner {
		t.Fatalf("claim owner = %+v, want the pull's owner %+v", owner, pull.Owner)
	}
	if _, found, err := store.ClaimOwner(ctx, route.ProjectKey+"-999999", route); err != nil || found {
		t.Fatalf("an unknown run answered %v, %v", found, err)
	}
}
