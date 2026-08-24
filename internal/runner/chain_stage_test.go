package runner

import (
	"context"
	"encoding/json"
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
