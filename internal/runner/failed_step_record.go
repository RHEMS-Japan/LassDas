package runner

import (
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

// recordFailedStep writes the step for a later attempt at the same report.
// Best-effort by design: the report must never be blocked by it.
func recordFailedStep(runDir string, evidence map[string]string) {
	step := evidence["failed_step"]
	if !UsableStepName(step) {
		return
	}
	_ = os.WriteFile(filepath.Join(runDir, FailedStepFile), []byte(step), 0o600)
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
	return map[string]string{"failed_step": step}
}

// UsableStepName reports whether a name is one the report will accept: the
// same bound and the same shape rule, checked before it can block a run.
func UsableStepName(step string) bool {
	return step != "" && len(step) <= hook.MaxFailedStepBytes &&
		utf8.ValidString(step) && !strings.ContainsAny(step, "\x00\r\n")
}
