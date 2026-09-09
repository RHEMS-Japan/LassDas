package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeChainConsumerConfig(t *testing.T, reviewerIDs []string) string {
	t.Helper()
	reviewers := make([]map[string]any, 0, len(reviewerIDs))
	for _, id := range reviewerIDs {
		reviewers = append(reviewers, map[string]any{"id": id})
	}
	encoded, err := json.Marshal(map[string]any{
		"max_stages": 3,
		"models":     map[string]any{"reviewers": reviewers},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "consumer.json")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The chain runs exactly two review cards; any other reviewer count would
// make every decide refuse its review set, so it refuses to start instead.
func TestChainReviewersRequiresExactlyTwo(t *testing.T) {
	reviewers, err := chainReviewers(writeChainConsumerConfig(t, []string{"review-a", "review-b"}))
	if err != nil || len(reviewers) != 2 || reviewers[0] != "review-a" {
		t.Fatalf("chainReviewers() = %v, %v", reviewers, err)
	}
	for _, ids := range [][]string{{"only-one"}, {"a", "b", "c"}} {
		if _, err := chainReviewers(writeChainConsumerConfig(t, ids)); err == nil {
			t.Fatalf("chainReviewers() accepted %d reviewers", len(ids))
		}
	}
}

func chainStagePipeline(t *testing.T) *Pipeline {
	t.Helper()
	return &Pipeline{Workspace: t.TempDir(), Logger: trailTestLogger{}}
}

func sealStageFiles(t *testing.T, pipeline *Pipeline, round int, decisionOutcome string) {
	t.Helper()
	stageDir := filepath.Join(pipeline.Workspace, "history", "stage-"+string(rune('0'+round)))
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stageDir, "candidate.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if decisionOutcome != "" {
		encoded := []byte(`{"outcome":"` + decisionOutcome + `"}`)
		if err := os.WriteFile(filepath.Join(stageDir, "decision.json"), encoded, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// currentRound moves past a decided round; latestCandidateRound does not —
// that difference is what keeps a re-dispatched validate card on its own
// round instead of an empty next one.
func TestRoundDerivationsDisagreeExactlyAtTheDecision(t *testing.T) {
	pipeline := chainStagePipeline(t)
	if pipeline.currentRound() != 1 || pipeline.latestCandidateRound() != 0 {
		t.Fatalf("empty workspace rounds = %d, %d", pipeline.currentRound(), pipeline.latestCandidateRound())
	}
	sealStageFiles(t, pipeline, 1, "converged")
	sealStageFiles(t, pipeline, 2, "")
	if pipeline.currentRound() != 2 || pipeline.latestCandidateRound() != 2 {
		t.Fatalf("mid-round = %d, %d", pipeline.currentRound(), pipeline.latestCandidateRound())
	}
	// The decision lands: the seal/review round advances, the validate and
	// publish round stays.
	sealStageFiles(t, pipeline, 2, "converged")
	if pipeline.currentRound() != 3 || pipeline.latestCandidateRound() != 2 {
		t.Fatalf("post-decision = %d, %d", pipeline.currentRound(), pipeline.latestCandidateRound())
	}
}

// A re-dispatched validate card resumes behind its own sealed decision: the
// decide verb must not run again (the worker binary here would fail loudly
// if invoked), and a sealed revise stays the same honest non-zero exit.
func TestChainValidateResumesBehindItsOwnDecision(t *testing.T) {
	pipeline := chainStagePipeline(t)
	pipeline.Config.WorkerBin = "false"
	sealStageFiles(t, pipeline, 1, "revise")
	err := pipeline.chainValidate(context.Background(), []string{"review-a", "review-b"})
	if err == nil || !strings.Contains(err.Error(), "sent back for revision") {
		t.Fatalf("chainValidate() = %v", err)
	}
}

// The implement and apply cards are direct commands: the stage hands the
// rendered instruction to the worker's run-instruction, which starts the
// agent through the launcher under the agent user, and records the run in
// the round the seal will complete. Without an instruction it refuses.
func TestChainImplementRunsTheInstructionThroughTheWorker(t *testing.T) {
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
	repoRoot := pipeline.path("target-repo")
	baseSHA := strings.Repeat("b", 40)
	err := pipeline.chainRunInstruction(context.Background(), "implementer", repoRoot, baseSHA)
	if err == nil || !strings.Contains(err.Error(), "no instruction") {
		t.Fatalf("without an instruction: %v", err)
	}
	if err := os.WriteFile(pipeline.path("INSTRUCTION.md"), []byte("do this\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.chainRunInstruction(context.Background(), "implementer", repoRoot, baseSHA); err != nil {
		t.Fatalf("chainRunInstruction: %v", err)
	}
	sealStageFiles(t, pipeline, 1, "revise")
	if err := pipeline.chainRunInstruction(context.Background(), "applier", repoRoot, baseSHA); err != nil {
		t.Fatalf("chainRunInstruction round 2: %v", err)
	}
	logged, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(logged)), "\n")
	want := []string{
		"run-instruction --config /etc/consumer.json --tool-sha " + strings.Repeat("a", 40) + " --draft " + pipeline.path("ticket-draft.json") +
			" --role implementer --instruction " + pipeline.path("INSTRUCTION.md") + " --repo-root " + repoRoot + " --base-sha " + baseSHA +
			" --stage 1 --knowledge-root /knowledge --out " + pipeline.path("history/stage-1/implementer-run.json"),
		"run-instruction --config /etc/consumer.json --tool-sha " + strings.Repeat("a", 40) + " --draft " + pipeline.path("ticket-draft.json") +
			" --role applier --instruction " + pipeline.path("INSTRUCTION.md") + " --repo-root " + repoRoot + " --base-sha " + baseSHA +
			" --stage 2 --knowledge-root /knowledge --out " + pipeline.path("history/stage-2/applier-run.json"),
	}
	if len(lines) != 2 || lines[0] != want[0] || lines[1] != want[1] {
		t.Fatalf("worker calls = %q, want %q", lines, want)
	}
}

// The implementer's instruction is rendered with the working copy's
// absolute path: the command refuses a root that is not absolute, and
// without the argument the card fails at render time rather than writing
// into the agent's home.
func TestTheImplementInstructionCardCarriesTheWorkingCopyRoot(t *testing.T) {
	pipeline := chainStagePipeline(t)
	record := filepath.Join(t.TempDir(), "worker.log")
	fake := filepath.Join(t.TempDir(), "fake-worker")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+record+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	pipeline.Config.WorkerBin = fake
	consumer := filepath.Join(t.TempDir(), "consumer.json")
	if err := os.WriteFile(consumer, []byte(`{"models":{"reviewers":[{"id":"review-a"},{"id":"review-b"}]},"consumers":[{}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	pipeline.Config.ConsumerConfigPath = consumer
	if err := pipeline.RenderImplementInstruction(context.Background(), 1); err != nil {
		t.Fatalf("RenderImplementInstruction: %v", err)
	}
	logged, _ := os.ReadFile(record)
	if !strings.Contains(string(logged), "--repo-root "+pipeline.path("target-repo")) {
		t.Fatalf("the card does not carry the working copy root: %q", strings.TrimSpace(string(logged)))
	}
}

// The applier's card carries the approved design and where the design round's
// objection record goes, so the run-instruction command can seal an objection
// the applier wrote at the root of its working copy (issue #103). The
// implementer's card, and an applier card of a run that never designed, carry
// neither.
func TestChainApplyCardCarriesTheDesignAndTheObjectionDestination(t *testing.T) {
	pipeline := chainStagePipeline(t)
	record := filepath.Join(t.TempDir(), "worker.log")
	fake := filepath.Join(t.TempDir(), "fake-worker")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+record+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	consumer := filepath.Join(t.TempDir(), "consumer.json")
	if err := os.WriteFile(consumer, []byte(`{"consumers":[{"design":{"review_investigation":true}}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	pipeline.Config.WorkerBin = fake
	pipeline.Config.ConsumerConfigPath = consumer
	writeDecision(t, pipeline.Workspace, `{"outcome":"ready","request_kind":"change","needs_design":true}`)
	if err := os.MkdirAll(pipeline.designRoundDir(1), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"investigation.json": `{}`, "decision.json": `{"outcome":"approved"}`, "design.json": `{}`} {
		if err := os.WriteFile(filepath.Join(pipeline.designRoundDir(1), name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(pipeline.path("INSTRUCTION.md"), []byte("apply this\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repoRoot := pipeline.path("target-repo")
	baseSHA := strings.Repeat("b", 40)
	if err := pipeline.chainRunInstruction(context.Background(), "applier", repoRoot, baseSHA); err != nil {
		t.Fatalf("applier card: %v", err)
	}
	logged, _ := os.ReadFile(record)
	line := strings.TrimSpace(string(logged))
	wantTail := " --design " + filepath.Join(pipeline.designRoundDir(1), "design.json") + " --objection-out " + filepath.Join(pipeline.designRoundDir(1), "objection.json")
	if !strings.Contains(line, "--role applier") || !strings.HasSuffix(line, wantTail) {
		t.Fatalf("applier call = %q, want it to end with %q", line, wantTail)
	}
	// The implementer's card never carries a design, even on a run that designed.
	if err := os.Remove(record); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.chainRunInstruction(context.Background(), "implementer", repoRoot, baseSHA); err != nil {
		t.Fatalf("implementer card: %v", err)
	}
	logged, _ = os.ReadFile(record)
	if strings.Contains(string(logged), "--design") {
		t.Fatalf("implementer call carries a design: %q", strings.TrimSpace(string(logged)))
	}
	// A design-shaped run whose approved design is gone fails closed rather
	// than running the applier without the binding.
	if err := os.Remove(filepath.Join(pipeline.designRoundDir(1), "design.json")); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.chainRunInstruction(context.Background(), "applier", repoRoot, baseSHA); err == nil || !errors.Is(err, ErrNoApprovedDesign) {
		t.Fatalf("applier without its design: %v", err)
	}
}
