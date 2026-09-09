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
	design, err := investigate.SealDesignDigest(investigate.Design{
		Files: []investigate.FileChange{{Path: "docs/OPERATIONS.md"}, {Path: "client/src/label.ts"}},
	})
	if err != nil {
		t.Fatal(err)
	}
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

// A working copy with no absolute path is refused where the instruction is
// rendered: printing "write with absolute paths" above a list of relative
// ones is the failure this section exists to remove.
func TestTheApplyInstructionRefusesAWorkingCopyThatIsNotAbsolute(t *testing.T) {
	absolute := t.TempDir()
	relative, err := filepath.Rel(mustGetwd(t), absolute)
	if err != nil {
		t.Skip("the temporary directory has no relative form here")
	}
	p := &Pipeline{Workspace: relative}
	if err := os.MkdirAll(p.designRoundDir(1), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.designRoundDir(1), "DESIGN.md"), []byte("# Design — round 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Everything the rendering needs is in place, so only the root's shape
	// can refuse it.
	if err := p.RenderApplyInstruction(nil, 1); err == nil {
		t.Fatal("a relative working copy was accepted")
	} else if !strings.Contains(err.Error(), "absolute path") {
		t.Fatalf("refused for another reason: %v", err)
	}
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	working, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return working
}

// A design record that does not match its own digest names nothing: the
// section falls back to the root alone rather than listing paths from a
// record the readers would refuse.
func TestTheApplyInstructionIgnoresADesignThatDoesNotMatchItsDigest(t *testing.T) {
	p := &Pipeline{Workspace: t.TempDir()}
	if err := os.MkdirAll(p.designRoundDir(1), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.designRoundDir(1), "DESIGN.md"), []byte("# Design\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(investigate.Design{Files: []investigate.FileChange{{Path: "docs/TAMPERED.md"}}})
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
	if containsString(string(instruction), "docs/TAMPERED.md") {
		t.Error("paths from an unsealed record reached the instruction")
	}
	if !containsString(string(instruction), p.path("target-repo")) {
		t.Error("the section lost the root as well")
	}
}

// Both halves of the design stage are handed what the requester decided.
// Without it the designer wrote designs that contradicted the answers and
// the design reviewers approved them, while the implementation reviewers —
// who have always had the answers — sent the design back every round citing
// them. The designer could not act on that, because it could not see what
// it was wrong about. Live, 2026-09-09: four design rounds approved, three
// implementation rounds refused, and the answers appearing nowhere in what
// the designer was given.
func TestTheDesignStageIsHandedTheRequestersAnswers(t *testing.T) {
	argvOf := func(t *testing.T, withAnswers bool) (string, string) {
		t.Helper()
		pipeline := chainStagePipeline(t)
		record := filepath.Join(t.TempDir(), "worker.log")
		fake := filepath.Join(t.TempDir(), "fake-worker")
		if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+record+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		pipeline.Config.WorkerBin = fake
		pipeline.Config.ConsumerConfigPath = "/etc/consumer.json"
		pipeline.Config.Identity.EngineSHA = strings.Repeat("a", 40)
		if err := os.MkdirAll(pipeline.designRoundDir(1), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{
			filepath.Join(pipeline.designRoundDir(1), "investigation.json"),
			pipeline.path("readiness-ticket.json"),
			pipeline.path("ticket-draft.json"),
		} {
			if err := os.WriteFile(name, []byte(`{}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if withAnswers {
			if err := os.WriteFile(pipeline.path("clarification.json"), []byte(`{}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		_ = pipeline.chainDesignReview(context.Background(), []string{"review-a", "review-b"}, 0,
			pipeline.path("target-repo"), strings.Repeat("b", 40))
		reviewArgv, _ := os.ReadFile(record)
		_ = os.Remove(record)
		// The designer's own call: a round it has not sealed yet, and a
		// readiness decision that asks for a design.
		if err := os.MkdirAll(filepath.Join(pipeline.path("history"), "readiness"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pipeline.path("history"), "readiness", "decision.json"),
			[]byte(`{"request_kind":"change","needs_design":true}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(pipeline.designRoundDir(1), "investigation.json")); err != nil {
			t.Fatal(err)
		}
		_ = pipeline.chainInvestigate(context.Background(), pipeline.path("target-repo"), strings.Repeat("b", 40))
		designerArgv, _ := os.ReadFile(record)
		return string(designerArgv), string(reviewArgv)
	}

	designer, reviewer := argvOf(t, true)
	for name, argv := range map[string]string{"the designer": designer, "the design reviewer": reviewer} {
		if !strings.Contains(argv, "--clarification ") {
			t.Errorf("%s was not handed the requester's answers: %q", name, argv)
		}
	}
	// A run that was never asked anything passes no empty flag.
	designer, reviewer = argvOf(t, false)
	for name, argv := range map[string]string{"the designer": designer, "the design reviewer": reviewer} {
		if strings.Contains(argv, "--clarification") {
			t.Errorf("%s was handed an answers flag for a run with no answers: %q", name, argv)
		}
	}
}
