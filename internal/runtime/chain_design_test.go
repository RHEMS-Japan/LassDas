package runtime

import (
	"context"
	"strings"
	"testing"
)

func designChainConfig() ChainConfig {
	chain := chainTestConfig()
	chain.Profiles.Investigate = "lassdas-investigate"
	chain.Profiles.DesignReviewA = "lassdas-design-review-a"
	chain.Profiles.DesignReviewB = "lassdas-design-review-b"
	chain.Profiles.DesignDecide = "lassdas-design-decide"
	chain.Profiles.Applier = "lassdas-applier"
	return chain
}

func stageNames(stages []ChainStage) string {
	names := make([]string, 0, len(stages))
	for _, stage := range stages {
		names = append(names, stage.Name)
	}
	return strings.Join(names, " ")
}

func TestChainStagesForShapes(t *testing.T) {
	chain := designChainConfig()
	if got := stageNames(ChainStagesFor(chain, ChainPlan{Shape: ShapeImplement})); got != "implement review-a review-b validate publish" {
		t.Errorf("implement shape = %q", got)
	}
	if got := stageNames(ChainStagesFor(chain, ChainPlan{Shape: ShapeInvestigation, ReviewInvestigation: true})); got != "investigate design-review-a design-decide" {
		t.Errorf("investigation shape = %q", got)
	}
	if got := stageNames(ChainStagesFor(chain, ChainPlan{Shape: ShapeInvestigation})); got != "investigate" {
		t.Errorf("investigation shape without review = %q", got)
	}
	design := ChainStagesFor(chain, ChainPlan{Shape: ShapeDesign})
	if got := stageNames(design); got != "investigate design-review-a design-review-b design-decide apply review-a review-b validate publish" {
		t.Errorf("design shape = %q", got)
	}
	walls := map[string]int{}
	profiles := map[string]string{}
	for _, stage := range design {
		walls[stage.Name] = stage.MaxRuntimeSeconds
		profiles[stage.Name] = stage.Profile
	}
	// The investigate wall outlasts the role's own 1,800-second budget; the
	// apply wall outlasts 40 turns at the measured pace; design-decide is a
	// kernel process.
	if walls[StageInvestigate] <= 1800 || walls[StageApply] < 1200 || walls[StageDesignDecide] != 300 || walls[StageDesignReviewA] != walls[StageReviewA] {
		t.Errorf("walls = %v", walls)
	}
	if profiles[StageInvestigate] != "lassdas-investigate" || profiles[StageApply] != "lassdas-applier" || profiles[StageDesignReviewB] != "lassdas-design-review-b" || profiles[StageReviewA] != "lassdas-review-a" {
		t.Errorf("profiles = %v", profiles)
	}
}

func TestChainCardKeysDistinguishDesignRounds(t *testing.T) {
	for _, tc := range []struct {
		stage string
		round int
		want  string
	}{
		{StageInvestigate, 2, "delivery-1:investigate:d2"},
		{StageDesignReviewB, 1, "delivery-1:design-review-b:d1"},
		{StageDesignDecide, 3, "delivery-1:design-decide:d3"},
		{StageApply, 1, "delivery-1:apply:r1"},
		{StageImplement, 4, "delivery-1:implement:r4"},
		{StagePublish, 1, "delivery-1:publish:r1"},
	} {
		key := ChainCardKey("delivery-1", tc.stage, tc.round)
		if key != tc.want {
			t.Errorf("key(%s, %d) = %q, want %q", tc.stage, tc.round, key, tc.want)
		}
		delivery, stage, round, ok := ParseChainCardKey(key)
		if !ok || delivery != "delivery-1" || stage != tc.stage || round != tc.round {
			t.Errorf("parse(%q) = %q %q %d %v", key, delivery, stage, round, ok)
		}
	}
	for _, bad := range []string{
		"delivery-1:investigate:r1", // a design stage in an implementation round
		"delivery-1:apply:d1",       // an implementation stage in a design round
		"delivery-1:investigate:d0",
		"delivery-1:investigate:d01",
		"delivery-1:design:d1",
		"delivery-1:investigate:x1",
		"delivery-1:investigate",
	} {
		if _, _, _, ok := ParseChainCardKey(bad); ok {
			t.Errorf("parse(%q) accepted", bad)
		}
	}
	// A second design round never collides with the first round's cards.
	if ChainCardKey("d", StageInvestigate, 1) == ChainCardKey("d", StageInvestigate, 2) {
		t.Error("design rounds share a key")
	}
}

func TestEnsureChainForDesignShapeKeysDesignAndImplementRoundsApart(t *testing.T) {
	hermes, callLog := stubChainHermes(t)
	chain := designChainConfig()
	terminal, err := EnsureChainFor(context.Background(), hermes, chain, ChainPlan{Shape: ShapeDesign}, nil, "delivery-1", "run-1", "", ChainRounds{Design: 2, Implement: 1})
	if err != nil {
		t.Fatal(err)
	}
	records := createRecords(t, callLog)
	if len(records) != 9 || terminal != "t_9" {
		t.Fatalf("created %d cards, terminal %q", len(records), terminal)
	}
	wantKeys := []string{
		"delivery-1:investigate:d2", "delivery-1:design-review-a:d2", "delivery-1:design-review-b:d2", "delivery-1:design-decide:d2",
		"delivery-1:apply:r1", "delivery-1:review-a:r1", "delivery-1:review-b:r1", "delivery-1:validate:r1", "delivery-1:publish:r1",
	}
	for index, record := range records {
		if got := recordFlag(record, "--idempotency-key"); got != wantKeys[index] {
			t.Errorf("card %d key = %q, want %q", index, got, wantKeys[index])
		}
		if index > 0 && recordFlag(record, "--parent") != "t_"+strings.TrimPrefix(recordFlag(records[index-1], "--idempotency-key"), "") && recordFlag(record, "--parent") == "" {
			t.Errorf("card %d has no parent", index)
		}
	}
	if !strings.Contains(records[4], "INSTRUCTION.md") {
		t.Errorf("apply card body does not point at the instruction file: %s", records[4])
	}
	if !strings.Contains(records[0], "run-1 investigate d2") || !strings.Contains(records[4], "run-1 apply r1") {
		t.Errorf("titles do not carry the round letters: %s / %s", recordFlag(records[0], "create"), records[4])
	}
	// A shape without implementation stages needs no implementation round.
	hermes2, callLog2 := stubChainHermes(t)
	if _, err := EnsureChainFor(context.Background(), hermes2, chain, ChainPlan{Shape: ShapeInvestigation, ReviewInvestigation: true}, nil, "delivery-2", "run-2", "", ChainRounds{Design: 1}); err != nil {
		t.Fatal(err)
	}
	if got := len(createRecords(t, callLog2)); got != 3 {
		t.Errorf("investigation shape created %d cards", got)
	}
	if _, err := EnsureChainFor(context.Background(), hermes2, chain, ChainPlan{Shape: ShapeDesign}, nil, "delivery-3", "run-3", "", ChainRounds{Design: 1}); err == nil {
		t.Error("design shape accepted without an implementation round")
	}
}

func TestDesignProfilesAreSetTogether(t *testing.T) {
	config := Config{Orchestration: "cards", HermesProfile: "lassdas-runner", Chain: chainTestConfig()}
	if err := config.validateOrchestration(); err != nil {
		t.Fatalf("original five profiles refused: %v", err)
	}
	config.Chain.Profiles.Investigate = "lassdas-investigate"
	if err := config.validateOrchestration(); err == nil || !strings.Contains(err.Error(), "set together") {
		t.Errorf("partial design profiles: %v", err)
	}
	config.Chain = designChainConfig()
	if err := config.validateOrchestration(); err != nil {
		t.Errorf("all design profiles refused: %v", err)
	}
	config.Chain.Profiles.Applier = "lassdas-implementer"
	if err := config.validateOrchestration(); err == nil || !strings.Contains(err.Error(), "reuse") {
		t.Errorf("reused profile accepted: %v", err)
	}
	if !designChainConfig().Profiles.DesignEnabled() || chainTestConfig().Profiles.DesignEnabled() {
		t.Error("DesignEnabled disagrees with the configuration")
	}
}
