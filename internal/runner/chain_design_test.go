package runner

import (
	"automation.internal/ticket-ingress/internal/worker/investigate"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
	for _, want := range []string{"You apply an approved design", "# Design — round 2", "revise-design.json", "Change only the files the design lists", "at the root of the working copy named above", "1 to 600 bytes", "There is no person on this run", "write the first change early", "remove anything you created", "blast_radius|not_doing", "Decide before you edit", "a message that describes edits you did not make"} {
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

func TestApplyInstructionCarriesThePreviousRoundsFindings(t *testing.T) {
	p := &Pipeline{Workspace: t.TempDir()}
	if err := os.MkdirAll(p.designRoundDir(1), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.designRoundDir(1), "DESIGN.md"), []byte("# Design — round 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stage := p.path("history/stage-1")
	if err := os.MkdirAll(stage, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"decision.json":  `{"outcome":"revise"}`,
		"review-b.json":  `{"reviewer_id":"review-b","verdict":"revise","findings":[{"code":"missing-null-check","path":"web/page.tmpl","message":"the label helper is called before it exists"}]}`,
		"review-a.json":  `{"reviewer_id":"review-a","verdict":"pass","findings":[]}`,
		"candidate.json": `{"files":[]}`,
	} {
		if err := os.WriteFile(filepath.Join(stage, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.RenderApplyInstruction(nil, 1); err != nil {
		t.Fatal(err)
	}
	instruction, _ := os.ReadFile(p.path("INSTRUCTION.md"))
	for _, want := range []string{"前の巡 (1 巡目) で出た指摘", "missing-null-check (review-b, web/page.tmpl)", "指示ではありません"} {
		if !containsString(string(instruction), want) {
			t.Errorf("instruction lacks %q", want)
		}
	}
}

func TestRequiredDesignFailsClosedWhenTheDecisionIsGoneAfterADesignRound(t *testing.T) {
	p := &Pipeline{Workspace: t.TempDir()}
	if _, _, err := p.requiredDesign(); err != nil {
		t.Errorf("no decision and no design round: %v", err)
	}
	if err := os.MkdirAll(p.designRoundDir(1), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.designRoundDir(1), "investigation.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.requiredDesign(); err == nil {
		t.Error("a run that designed but lost its decision fell back to the original chain")
	}
}

// The design reviewer is handed the ticket the run was accepted on and, on a
// round the applier's objection reopened, that objection: the runner passes
// --ticket whenever readiness-ticket.json exists and --previous-objection
// only when the earlier round left objection.json (live: reviewers judged
// without the request and never saw the objection).
func TestChainDesignReviewPassesTheTicketAndTheObjection(t *testing.T) {
	pipeline := chainStagePipeline(t)
	record := filepath.Join(t.TempDir(), "worker.log")
	fake := filepath.Join(t.TempDir(), "fake-worker")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+record+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	pipeline.Config.WorkerBin = fake
	pipeline.Config.ConsumerConfigPath = "/etc/consumer.json"
	pipeline.Config.KnowledgeRoot = "/knowledge"
	pipeline.Config.Identity.EngineSHA = strings.Repeat("a", 40)
	if err := os.MkdirAll(pipeline.designRoundDir(1), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{filepath.Join(pipeline.designRoundDir(1), "investigation.json"), pipeline.path("readiness-ticket.json")} {
		if err := os.WriteFile(name, []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// The fake worker seals no review, so the stage reports that; the argv
	// it was given is what this test reads.
	_ = pipeline.chainDesignReview(context.Background(), []string{"review-a", "review-b"}, 0, pipeline.path("target-repo"), strings.Repeat("b", 40))
	logged, _ := os.ReadFile(record)
	first := string(logged)
	if !strings.Contains(first, "--ticket "+pipeline.path("readiness-ticket.json")) || strings.Contains(first, "--previous-objection") {
		t.Fatalf("round 1 argv = %q; want the ticket and no objection", first)
	}
	// Round 2, reopened by the applier's objection to the round-1 design.
	for name, content := range map[string]string{
		filepath.Join(pipeline.designRoundDir(1), "decision.json"):  `{"outcome":"approved"}`,
		filepath.Join(pipeline.designRoundDir(1), "objection.json"): `{"reason":"r","section":"files"}`,
	} {
		if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(pipeline.designRoundDir(2), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pipeline.designRoundDir(2), "investigation.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(record)
	_ = pipeline.chainDesignReview(context.Background(), []string{"review-a", "review-b"}, 1, pipeline.path("target-repo"), strings.Repeat("b", 40))
	logged, _ = os.ReadFile(record)
	second := string(logged)
	if !strings.Contains(second, "--previous-objection "+filepath.Join(pipeline.designRoundDir(1), "objection.json")) || !strings.Contains(second, "--ticket ") {
		t.Fatalf("round 2 argv = %q; want the objection and the ticket", second)
	}
}

// The applier's instruction names the working copy by its absolute path and
// asks for absolute writes. The agent's file tools resolve a relative path
// against its own home: a live applier reported creating the design's file
// twice, and both landed in the home while the working copy stayed empty.
func TestTheApplyInstructionNamesTheWorkingCopyAbsolutely(t *testing.T) {
	p := &Pipeline{Workspace: t.TempDir()}
	if err := os.MkdirAll(p.designRoundDir(1), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.designRoundDir(1), "DESIGN.md"), []byte("# Design — round 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A sealed design, so the section can list its files as finished paths.
	design := investigate.Design{Files: []investigate.FileChange{{Path: "docs/OPERATIONS.md"}, {Path: "client/src/label.ts"}}}
	raw, err := json.Marshal(design)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.designRoundDir(1), "design.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := p.RenderApplyInstruction(nil, 1); err != nil {
		t.Fatal(err)
	}
	instruction, _ := os.ReadFile(p.path("INSTRUCTION.md"))
	root := p.path("target-repo")
	for _, want := range []string{
		"## Where the working copy is",
		root,
		root + "/docs/OPERATIONS.md",
		root + "/client/src/label.ts",
		"the paths to give your tools",
		"it lands in your own home",
		"is never\nwritten inside a file",
		"A relative path does not land in the working",
		"the seal reads the working tree at the absolute path named above",
		"at the root of the working copy named above (its absolute path)",
	} {
		if !containsString(string(instruction), want) {
			t.Errorf("the instruction lacks %q", want)
		}
	}
}
