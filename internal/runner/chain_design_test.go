package runner

import (
	"os"
	"path/filepath"
	"testing"

	"automation.internal/ticket-ingress/internal/runtime"
)

func writeDecision(t *testing.T, runDir, body string) {
	t.Helper()
	dir := filepath.Join(runDir, "history", "readiness")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "decision.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestChainPlanFromDecision(t *testing.T) {
	runDir := t.TempDir()
	consumer := filepath.Join(t.TempDir(), "consumer.json")
	if err := os.WriteFile(consumer, []byte(`{"consumers":[{"design":{"review_investigation":true}}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ChainPlanFromDecision(runDir, consumer); err == nil {
		t.Error("missing decision accepted")
	}
	writeDecision(t, runDir, `{"outcome":"ready","request_kind":"change","needs_design":true}`)
	if plan, err := ChainPlanFromDecision(runDir, consumer); err != nil || plan.Shape != runtime.ShapeDesign {
		t.Errorf("needs_design: %+v %v", plan, err)
	}
	writeDecision(t, runDir, `{"outcome":"ready","request_kind":"change","needs_design":false}`)
	if plan, err := ChainPlanFromDecision(runDir, consumer); err != nil || plan.Shape != runtime.ShapeImplement {
		t.Errorf("design skipped: %+v %v", plan, err)
	}
	// A decision without the field (an older shape) runs the original chain.
	writeDecision(t, runDir, `{"outcome":"ready"}`)
	if plan, err := ChainPlanFromDecision(runDir, consumer); err != nil || plan.Shape != runtime.ShapeImplement {
		t.Errorf("old decision: %+v %v", plan, err)
	}
	writeDecision(t, runDir, `{"outcome":"ready","request_kind":"investigation","needs_design":false}`)
	if plan, err := ChainPlanFromDecision(runDir, consumer); err != nil || plan.Shape != runtime.ShapeInvestigation || !plan.ReviewInvestigation {
		t.Errorf("investigation with review: %+v %v", plan, err)
	}
	if err := os.WriteFile(consumer, []byte(`{"consumers":[{"design":{"review_investigation":false}}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if plan, err := ChainPlanFromDecision(runDir, consumer); err != nil || plan.Shape != runtime.ShapeInvestigation || plan.ReviewInvestigation {
		t.Errorf("investigation without review: %+v %v", plan, err)
	}
	if err := os.WriteFile(consumer, []byte(`{"consumers":[{}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if plan, err := ChainPlanFromDecision(runDir, consumer); err != nil || !plan.ReviewInvestigation {
		t.Errorf("review defaults on: %+v %v", plan, err)
	}
}

func TestDesignRoundsAreCountedFromSealedDecisions(t *testing.T) {
	p := &Pipeline{Workspace: t.TempDir()}
	if p.currentDesignRound() != 1 || p.LatestDesignRound() != 0 {
		t.Fatalf("empty run dir: current %d latest %d", p.currentDesignRound(), p.LatestDesignRound())
	}
	for _, name := range []string{"investigation.json", "decision.json"} {
		dir := p.designRoundDir(1)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if p.currentDesignRound() != 2 || p.LatestDesignRound() != 1 {
		t.Errorf("after round 1 decided: current %d latest %d", p.currentDesignRound(), p.LatestDesignRound())
	}
	if err := os.MkdirAll(p.designRoundDir(2), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.designRoundDir(2), "investigation.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if p.currentDesignRound() != 2 || p.LatestDesignRound() != 2 {
		t.Errorf("round 2 in progress: current %d latest %d", p.currentDesignRound(), p.LatestDesignRound())
	}
	if err := os.WriteFile(filepath.Join(p.designRoundDir(2), "DESIGN.md"), []byte("# Design — round 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := p.RenderApplyInstruction(nil, 2); err != nil {
		t.Fatal(err)
	}
	instruction, _ := os.ReadFile(p.path("INSTRUCTION.md"))
	for _, want := range []string{"You apply an approved design", "# Design — round 2", "revise-design.json", "Change only the files the design lists"} {
		if !containsString(string(instruction), want) {
			t.Errorf("instruction lacks %q", want)
		}
	}
}

func containsString(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
