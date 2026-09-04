package runtime

import "testing"

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
