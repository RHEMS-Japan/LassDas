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

func TestRunnerProgressUsesCanonicalWorkspaceAndLedgerEnd(t *testing.T) {
	config := runtime.Config{Orchestration: "runner"}
	config.Chain.RunsRoot = t.TempDir() // Deliberately not the runner's workspace.
	workspace := t.TempDir()
	run := state.RunOverview{DeliveryID: "delivery-example", State: "claimed"}
	tasks := []runtime.BoardTask{{IdempotencyKey: run.DeliveryID, Status: "running", WorkspacePath: workspace}}
	write := func(step string, at time.Time) {
		t.Helper()
		raw, err := json.Marshal(map[string]any{"step": step, "started_at": at})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(workspace, "current-step.json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct{ step, stage string }{{"read-contract", "intake"}, {"implement", "implement"}, {"agent-review", "review"}, {"run-validation", "checks"}} {
		write(test.step, time.Now())
		got := classifyRun(config, run, tasks)
		if got.Step != test.stage || got.Running == nil || got.Running.Step != test.step || got.WorkspacePath != workspace {
			t.Fatalf("%s: %+v", test.step, got)
		}
	}
	write("implement", time.Now().Add(-3*time.Hour))
	if got := classifyRun(config, run, tasks); got.Running != nil {
		t.Fatal("stale step is shown as running")
	}
	write("implement", time.Now())
	tasks[0].Status = "blocked"
	if got := classifyRun(config, run, tasks); got.Step != "attention" || got.Running != nil {
		t.Fatalf("blocked: %+v", got)
	}
	run.State, run.TerminalCode = "terminal", "internal_failed"
	if got := classifyRun(config, run, tasks); got.Step != "failed" || got.Running != nil || got.WorkspacePath != workspace {
		t.Fatalf("terminal: %+v", got)
	}
	run.State = "awaiting_answer"
	if got := classifyRun(config, run, tasks); got.Step != "question" || got.Running != nil {
		t.Fatalf("question: %+v", got)
	}
	tasks[0].IdempotencyKey = "another-delivery"
	if got := classifyRun(config, run, tasks); got.WorkspacePath != "" {
		t.Fatal("another delivery supplied the directory")
	}
}

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
	tasks := []runtime.BoardTask{{IdempotencyKey: run.DeliveryID, WorkspacePath: t.TempDir()}}
	got := classifyRun(config, run, tasks)
	if got.Step != "intake" || got.Running == nil || got.Running.Step != "assess-readiness" || got.WorkspacePath != "" {
		t.Fatalf("%+v", got)
	}
}
