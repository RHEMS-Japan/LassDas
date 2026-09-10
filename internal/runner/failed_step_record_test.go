package runner

import (
	"automation.internal/ticket-ingress/internal/hook"
	"os"
	"path/filepath"
	"testing"
)

func TestFailedStepPreservesBudgetReasonButDegradesBrokenCompanions(t *testing.T) {
	dir := t.TempDir()
	step := "AI による設計のレビュー（レビュー役 A）"
	evidence := map[string]string{"failed_step": step, "model_failure_reason": hook.ModelFailureBudgetExhausted}
	recordFailedStep(dir, evidence)
	if got := RecordedFailedStep(dir); got["model_failure_reason"] != hook.ModelFailureBudgetExhausted || got["failed_step"] != step {
		t.Fatalf("lost explanation: %v", got)
	}
	for _, broken := range []string{
		`{`,
		`{"failed_step":"another step","model_failure_reason":"budget_exhausted"}`,
		`{"failed_step":"` + step + `","model_failure_reason":"other"}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, ModelFailureFile), []byte(broken), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := RecordedFailedStep(dir); got["model_failure_reason"] != "" || got["failed_step"] != step {
			t.Fatalf("broken explanation blocked or changed step: %v", got)
		}
	}
	recordFailedStep(dir, evidence)
	recordFailedStep(dir, map[string]string{"failed_step": step})
	if got := RecordedFailedStep(dir); got["model_failure_reason"] != "" || got["failed_step"] != step {
		t.Fatalf("stale explanation survived: %v", got)
	}
}
