package attendant

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
)

// Unrecoverable records are not permission to reset already accepted work.
// In particular the old "already retried" note cannot turn them into a
// failure ending. Recovery may need storage to become available, but it
// preserves the evidence and the same running cards while it cannot act.
func TestAnUnavailableReceptionKeepsAcceptedWork(t *testing.T) {
	for _, retried := range []bool{false, true} {
		name := "first damage"
		if retried {
			name = "old engine already restarted it"
		}
		t.Run(name, func(t *testing.T) {
			h := newDepthHarness(t, "production", true, "")
			runningChainCard(t, h, runtime.StageImplement)
			h.write("history/readiness/decision.json", "{broken")
			if retried {
				h.write(runner.ReceptionAgainFile, `{"schema_version":1,"reason":"earlier damage"}`)
			}
			for range 3 {
				h.tick()
			}
			if row := h.runRow(); row.State != "claimed" || row.TerminalCode != "" {
				t.Fatalf("unavailable reception ended or restarted accepted work: %+v; %v", row, h.logger.lines)
			}
			if len(*h.posted) != 0 {
				t.Fatalf("a question or ending was posted: %v", *h.posted)
			}
			if calls := h.calls(); strings.Contains(calls, "|archive|") || strings.Contains(calls, "|create|") {
				t.Fatalf("the running cards were discarded: %s", calls)
			}
			raw, err := os.ReadFile(filepath.Join(h.runDir, "history/readiness/decision.json"))
			if err != nil || string(raw) != "{broken" {
				t.Fatalf("unrecoverable evidence was discarded: %q %v", raw, err)
			}
			if !strings.Contains(strings.Join(h.logger.lines, "\n"), "recovery is pending") {
				t.Fatalf("the unavailable recovery reason was lost: %v", h.logger.lines)
			}
		})
	}
}

// This separate configuration gap is not repaired by repeating reception.
func TestAMissingDesignProfileIsNotRegenerated(t *testing.T) {
	h := newDepthHarness(t, "production", true, "")
	h.write("history/readiness/decision.json", `{"request_kind":"change","needs_design":true}`)
	h.tick()
	if row := h.runRow(); row.State != "terminal" || row.TerminalCode != "internal_failed" {
		t.Fatalf("configuration gap = %s / %s; %v", row.State, row.TerminalCode, h.logger.lines)
	}
	if _, err := os.Stat(filepath.Join(h.runDir, runner.ReceptionAgainFile)); !os.IsNotExist(err) {
		t.Fatalf("a configuration gap restarted the reception: %v", err)
	}
}

// Historical notes still explain what that older engine really did.
func TestTheRegeneratedReceptionIsShownAsAnAssumption(t *testing.T) {
	runDir := t.TempDir()
	if got := loadPlanFacts(runDir).Assumptions; len(got) != 0 {
		t.Fatalf("assumptions without a regeneration = %v", got)
	}
	encoded, err := json.Marshal(receptionAgainRecord{SchemaVersion: receptionAgainSchemaVersion,
		Reason: "not readable as a decision", At: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, runner.ReceptionAgainFile), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	assumptions := loadPlanFacts(runDir).Assumptions
	if len(assumptions) != 1 || !strings.Contains(assumptions[0], "受付をもう一度実行し") {
		t.Fatalf("assumptions = %v", assumptions)
	}
}
