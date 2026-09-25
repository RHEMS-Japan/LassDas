package attendant

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
)

// The implementing cards read their instruction from a file, written once
// when the round began. A card dispatched again reads exactly what it read
// the first time — so a rebuild that only wrote down its intention would
// be a hand spent on nothing, which is the shape of failure this whole
// ladder exists to stop producing.
//
// The hand is the instruction being written again. Here the file is taken
// away first, so its coming back is the render having actually run inside
// the climb rather than a leftover from before it.
func TestTheRebuildWritesTheApplyInstructionAgain(t *testing.T) {
	setup := newLadderSetup(t, runner.StageFailure{
		Stage: runtime.StageApply, Round: 1, Class: runner.FailureClassModel, Error: "the provider gave up",
	})
	seatTheConsumer(t, setup)
	seedApprovedDesign(t, setup)
	instruction := filepath.Join(setup.runDir, "INSTRUCTION.md")
	if err := os.Remove(instruction); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	if _, err := setup.climb(t); err != nil {
		t.Fatal(err)
	}
	record, rebuilt := seatRecord(t, setup, runtime.StageApply, "author")
	if !rebuilt || record.PromptRebuilt != "shorten" {
		t.Fatalf("the rebuild was not written down: %+v", record)
	}
	raw, err := os.ReadFile(instruction)
	if err != nil {
		t.Fatalf("the instruction was not written again; the card would read what it read before: %v", err)
	}
	for _, kept := range []string{"You apply an approved design", "Change the label."} {
		if !strings.Contains(string(raw), kept) {
			t.Fatalf("the rebuilt instruction dropped %q", kept)
		}
	}
}

// seedApprovedDesign puts an approved design where the applier's
// instruction reads it, with the working copy the instruction points at.
func seedApprovedDesign(t *testing.T, setup *ladderSetup) {
	t.Helper()
	design := filepath.Join(setup.runDir, "history", "design-1")
	if err := os.MkdirAll(design, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		// The round is found by its sealed investigation, approved by its
		// decision, and rendered from its design — the three files the
		// applier's instruction is built out of.
		"investigation.json": `{"round":1}`,
		"decision.json":      `{"outcome":"approved"}`,
		"design.json":        `{"files":[{"path":"client/src/label.ts"}]}`,
		"DESIGN.md":          "# The design\n\nChange the label.\n",
	} {
		if err := os.WriteFile(filepath.Join(design, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(setup.runDir, "target-repo"), 0o755); err != nil {
		t.Fatal(err)
	}
}
