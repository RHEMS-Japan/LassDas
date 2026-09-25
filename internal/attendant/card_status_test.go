package attendant

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
)

// The cards supply the coarse stage; the step file in the delivery's own
// run directory only says which step of it is under way. Nothing else
// names a directory: a board row cannot send the display at one.
func TestCardsKeepTheirStageWhileShowingTheCurrentReceptionStep(t *testing.T) {
	config := runtime.Config{Orchestration: "cards"}
	config.Chain.RunsRoot = t.TempDir()
	run := state.RunOverview{DeliveryID: "delivery-example", State: "claimed"}
	dir := runDirectory(config, run.DeliveryID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"step": "assess-readiness", "started_at": time.Now()})
	if err := os.WriteFile(filepath.Join(dir, "current-step.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	tasks := []runtime.BoardTask{{IdempotencyKey: run.DeliveryID}}
	got := classifyRun(config, run, tasks)
	if got.Step != "intake" || got.Running == nil || got.Running.Step != "assess-readiness" {
		t.Fatalf("%+v", got)
	}
	// A step sealed hours ago is not a step that is running now.
	stale, _ := json.Marshal(map[string]any{"step": "assess-readiness", "started_at": time.Now().Add(-3 * time.Hour)})
	if err := os.WriteFile(filepath.Join(dir, "current-step.json"), stale, 0600); err != nil {
		t.Fatal(err)
	}
	if got := classifyRun(config, run, tasks); got.Running != nil {
		t.Fatal("a stale step is shown as running")
	}
}
