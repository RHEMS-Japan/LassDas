package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"automation.internal/ticket-ingress/internal/hook"
)

// FailedStepFile keeps the step a run ended on, so a report that has to be
// posted again carries it too. The first attempt's comment can be the thing
// that failed — the row then stays pending and a later tick posts it — and
// the stage that failed is in neither the run row nor the board by then.
//
// Nothing here removes a stale one: what keeps it fresh is Pipeline.Prepare,
// which clears the run directory at the start of every attempt, and run
// directories are keyed by delivery id so no two deliveries share one.
const FailedStepFile = "failed-step.txt"

// The companion keeps the legacy step file readable by older engines. The
// step binding prevents a partial update from explaining a different failure.
const ModelFailureFile = "model-failure.json"

type modelFailureRecord struct {
	Step   string `json:"failed_step"`
	Reason string `json:"model_failure_reason"`
}

// recordFailedStep writes the step for a later attempt at the same report.
// Best-effort by design: the report must never be blocked by it.
func recordFailedStep(runDir string, evidence map[string]string) {
	step := evidence["failed_step"]
	if !UsableStepName(step) {
		return
	}
	_ = os.Remove(filepath.Join(runDir, ModelFailureFile))
	if err := os.WriteFile(filepath.Join(runDir, FailedStepFile), []byte(step), 0o600); err != nil {
		return
	}
	if evidence["model_failure_reason"] == hook.ModelFailureBudgetExhausted {
		encoded, _ := json.Marshal(modelFailureRecord{Step: step, Reason: hook.ModelFailureBudgetExhausted})
		_ = os.WriteFile(filepath.Join(runDir, ModelFailureFile), encoded, 0o600)
	}
}

// RecordedFailedStep reads back the step a run ended on, as report evidence,
// or nothing when there is none to read.
//
// Held to the same shape the report will demand, and for a stronger reason
// than a hardcoded name is: this is the one source that can be malformed.
// The write is not atomic, and the situation it exists for is a pod that
// stopped — so a file cut mid-rune is exactly the file this will find. A
// name the report refuses does not cost the requester a vaguer sentence,
// it costs them the comment: the report is rejected, nothing is posted, and
// the row retries for ever (review of #132).
func RecordedFailedStep(runDir string) map[string]string {
	encoded, err := os.ReadFile(filepath.Join(runDir, FailedStepFile))
	if err != nil {
		return nil
	}
	step := string(encoded)
	if !UsableStepName(step) {
		return nil
	}
	evidence := map[string]string{"failed_step": step}
	var failure modelFailureRecord
	encoded, err = os.ReadFile(filepath.Join(runDir, ModelFailureFile))
	if err == nil && len(encoded) <= 1024 && json.Unmarshal(encoded, &failure) == nil &&
		failure.Step == step && failure.Reason == hook.ModelFailureBudgetExhausted {
		evidence["model_failure_reason"] = failure.Reason
	}
	return evidence
}

// UsableStepName reports whether a name is one the report will accept: the
// same bound and the same shape rule, checked before it can block a run.
func UsableStepName(step string) bool {
	return step != "" && len(step) <= hook.MaxFailedStepBytes &&
		utf8.ValidString(step) && !strings.ContainsAny(step, "\x00\r\n")
}
