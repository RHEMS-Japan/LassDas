package attendant

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/runner"
)

// A delivery whose sealed readiness decision cannot be read does not end.
//
// It used to: the tick could not derive the chain's shape, and the run went
// terminal as internal_failed with a comment telling the requester an
// internal error had happened and an operator would look. Nothing about
// that file is a decision that the request cannot be carried out — the
// reception derived it from the ticket and can derive it again — so the
// delivery goes back to the queue and does exactly that.
func TestAnUnreadableReadinessDecisionRunsTheReceptionAgain(t *testing.T) {
	h := newDepthHarness(t, "production", true, "")
	h.write("history/readiness/decision.json", "{broken")

	h.tick()

	row := h.runRow()
	if row.State != "queued" {
		t.Fatalf("run = %s / %s, want it back in the queue (log: %v)", row.State, row.TerminalCode, h.logger.lines)
	}
	if len(*h.posted) != 0 {
		t.Fatalf("the requester was told a delivery had failed: %v", *h.posted)
	}
	if !strings.Contains(h.calls(), "archive") {
		t.Fatalf("the round's cards were left on the board: %s", h.calls())
	}
	// The record the reception will replace is gone, so a rebuild that dies
	// before it gets that far cannot leave the same unreadable file to be
	// read and ended on.
	if _, err := os.Stat(filepath.Join(h.runDir, "history", "readiness", "decision.json")); !os.IsNotExist(err) {
		t.Fatalf("the unreadable decision is still on the volume: %v", err)
	}
	var record receptionAgainRecord
	raw, err := os.ReadFile(filepath.Join(h.runDir, runner.ReceptionAgainFile))
	if err != nil {
		t.Fatalf("the regeneration left no note: %v", err)
	}
	if err := json.Unmarshal(raw, &record); err != nil || record.SchemaVersion != receptionAgainSchemaVersion {
		t.Fatalf("note = %q (%v)", raw, err)
	}
	if record.At.IsZero() || record.Reason == "" {
		t.Fatalf("note = %+v", record)
	}
}

// Once. A delivery already carrying the note ends honestly rather than
// asking the reception a third time: the second unreadable decision is the
// reception sealing something unreadable, not a file that got corrupted,
// and a regeneration with no bound is the unbounded retry this engine is
// meant not to have.
func TestASecondUnreadableReadinessDecisionEndsTheDelivery(t *testing.T) {
	h := newDepthHarness(t, "production", true, "")
	if err := sealReceptionAgain(h.runDir, "an earlier one", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	h.write("history/readiness/decision.json", "{broken")

	h.tick()

	row := h.runRow()
	if row.State != "terminal" || row.TerminalCode != "internal_failed" {
		t.Fatalf("run = %s / %s, want a terminal internal_failed (log: %v)", row.State, row.TerminalCode, h.logger.lines)
	}
	if len(*h.posted) != 1 {
		t.Fatalf("comments = %v, want the one ending", *h.posted)
	}
}

// A shape the instance is not equipped for is not the same thing, and is
// not regenerated. The pod has no design profiles now and will have none on
// the next tick either; running the reception again would spend two models
// to seal the same decision and end at the same place.
func TestAMissingDesignProfileIsNotRegenerated(t *testing.T) {
	h := newDepthHarness(t, "production", true, "")
	h.write("history/readiness/decision.json", `{"request_kind":"change","needs_design":true}`)

	h.tick()

	row := h.runRow()
	if row.State != "terminal" || row.TerminalCode != "internal_failed" {
		t.Fatalf("run = %s / %s, want a terminal internal_failed (log: %v)", row.State, row.TerminalCode, h.logger.lines)
	}
	if _, err := os.Stat(filepath.Join(h.runDir, runner.ReceptionAgainFile)); !os.IsNotExist(err) {
		t.Fatalf("a configuration gap asked the reception to run again: %v", err)
	}
}

// What the second reception decided is what the delivery is built on, and
// nobody was asked whether the first one had decided the same. That is an
// assumption, and it is shown where the reception's other assumptions are.
func TestTheRegeneratedReceptionIsShownAsAnAssumption(t *testing.T) {
	runDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(runDir, "history", "readiness"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := loadPlanFacts(runDir).Assumptions; len(got) != 0 {
		t.Fatalf("assumptions without a regeneration = %v", got)
	}
	if err := sealReceptionAgain(runDir, "not readable as a decision", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	assumptions := loadPlanFacts(runDir).Assumptions
	if len(assumptions) != 1 || !strings.Contains(assumptions[0], "受付をもう一度実行し") {
		t.Fatalf("assumptions = %v", assumptions)
	}
}
