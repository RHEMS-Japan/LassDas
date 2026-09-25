package runtime

import (
	"context"
	"testing"
	"time"
)

// The publish card's wall must cover what the base-advance retry can
// legally spend: the same deterministic validation the validate card runs.
// A publish wall below the validate wall reintroduces the failure the retry
// exists to prevent — the kanban kills the catch-up mid-validation.
func TestPublishWallCoversTheBaseAdvanceRevalidation(t *testing.T) {
	stages := ChainStages(ChainConfig{})
	walls := map[string]int{}
	for _, stage := range stages {
		walls[stage.Name] = stage.MaxRuntimeSeconds
	}
	if walls[StagePublish] < walls[StageValidate] {
		t.Fatalf("publish wall %ds is below the validate wall %ds", walls[StagePublish], walls[StageValidate])
	}
}

// The wall a card is dispatched with is the wall the process inside it
// holds itself to, less the margin. Both are read from one place so the
// number the kanban is given and the number the process stops at cannot
// drift apart.
func TestTheProcessStopsItselfInsideTheWallItsCardWasGiven(t *testing.T) {
	chain := ChainConfig{Deliver: DeliverConfig{
		ChecksProfile: "c", IntegrateProfile: "i", PromoteProfile: "p", EnabledAfter: "2026-09-01T00:00:00Z"}}
	// Every card the runner is dispatched for knows its own wall, whichever
	// shape it belongs to. A stage missing from this would be bounded by
	// nothing but the supervisor's signal, which is the shape that gets
	// replayed for free.
	for _, stage := range DispatchedStages() {
		if CardWall(chain, stage) <= 0 {
			t.Fatalf("%s has no wall of its own", stage)
		}
	}
	if CardWall(chain, "not-a-card") != 0 {
		t.Fatal("a name no card carries was given a wall")
	}

	for name, tc := range map[string]struct{ wall, within time.Duration }{
		"a review's seventy minutes": {70 * time.Minute, 69 * time.Minute},
		"exactly the margin":         {time.Minute, 30 * time.Second},
		"shorter than the margin":    {30 * time.Second, 15 * time.Second},
		"no wall at all":             {0, 0},
	} {
		t.Run(name, func(t *testing.T) {
			before := time.Now()
			ctx, cancel := WithWall(context.Background(), tc.wall)
			defer cancel()
			deadline, ok := ctx.Deadline()
			if tc.within == 0 {
				if ok {
					t.Fatalf("a card with no wall was given a deadline at %v", deadline)
				}
				return
			}
			if !ok {
				t.Fatal("the card was given no deadline")
			}
			// The margin never exceeds the wall: the deadline is always in
			// front of the caller and always behind the supervisor's.
			if got := deadline.Sub(before); got > tc.within+time.Second || got < tc.within-time.Second {
				t.Fatalf("deadline in %v, want about %v", got, tc.within)
			}
			if tc.within >= tc.wall {
				t.Fatalf("the process would stop at %v, no earlier than its card's %v", tc.within, tc.wall)
			}
		})
	}
}
