package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/worker"
)

// What a rebuilt instruction loses and what it keeps. The earlier round's
// objections go — a model that answered nothing is asked the same job
// shorter, not a different job — and the design, the working copy, the
// rules the applier is held to and the refused validation all stay. The
// last of those is the reason the round exists, not commentary on it.
func TestARebuiltApplyInstructionDropsOnlyTheEarlierObjections(t *testing.T) {
	workspace := t.TempDir()
	consumer := filepath.Join(workspace, "consumer.json")
	if err := os.WriteFile(consumer, []byte(`{"models":{"implementer":{"id":"author"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	pipeline := &Pipeline{Workspace: workspace}
	pipeline.Config.ConsumerConfigPath = consumer
	seedDesignedRound(t, pipeline, workspace)

	if err := pipeline.RenderApplyInstruction(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	before := readInstruction(t, workspace)
	if !strings.Contains(before, "missing-check") {
		t.Fatalf("this test needs a round carrying an earlier objection:\n%s", before)
	}

	if err := WriteSeatRecord(workspace, SeatRecord{
		Seat: "author", Stage: runtime.StageApply, Round: 2,
		PromptRebuilt: worker.PromptRebuildShorten, At: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.RenderApplyInstruction(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	after := readInstruction(t, workspace)
	if len(after) >= len(before) {
		t.Fatalf("the rebuilt instruction is %d bytes against %d; it has to be the shorter ask", len(after), len(before))
	}
	if strings.Contains(after, "missing-check") {
		t.Fatal("the rebuilt instruction still carries the earlier round's objections")
	}
	for _, kept := range []string{"You apply an approved design", "Change the label.", "検証が通らなかった"} {
		if !strings.Contains(after, kept) {
			t.Fatalf("the rebuilt instruction dropped %q", kept)
		}
	}
}

// seedDesignedRound puts an approved design, a decided first round that
// objected, and that round's refused validation where the applier's
// instruction reads them.
func seedDesignedRound(t *testing.T, pipeline *Pipeline, workspace string) {
	t.Helper()
	design := filepath.Join(workspace, "history", "design-1")
	first := filepath.Join(workspace, "history", "stage-1")
	for _, directory := range []string{design, first, filepath.Join(workspace, "target-repo")} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, content := range map[string]string{
		filepath.Join(design, "investigation.json"): `{"round":1}`,
		filepath.Join(design, "decision.json"):      `{"outcome":"approved"}`,
		filepath.Join(design, "design.json"):        `{"files":[{"path":"client/src/label.ts"}]}`,
		filepath.Join(design, "DESIGN.md"):          "# The design\n\nChange the label.\n",
		filepath.Join(first, "decision.json"):       `{"outcome":"revise"}`,
		filepath.Join(first, "review-a.json"): `{"reviewer_id":"review-a","verdict":"revise","findings":` +
			`[{"code":"missing-check","path":"client/src/label.ts","message":"the label is not read anywhere"}]}`,
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pipeline.SealValidationFailure(1, "run-validation", "the tests printed this")
	if _, sealed := ReadValidationFailure(workspace, 1); !sealed {
		t.Fatal("the refused validation did not seal")
	}
}

func readInstruction(t *testing.T, workspace string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(workspace, "INSTRUCTION.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
