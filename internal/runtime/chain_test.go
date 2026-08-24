package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubChainHermes is the chain tests' fake CLI: like stubHermes it records
// every argv record, but create answers with an incrementing id so parent
// links between freshly created cards are observable.
func stubChainHermes(t *testing.T) (hermes *Hermes, callLog string) {
	t.Helper()
	directory := t.TempDir()
	callLog = filepath.Join(directory, "calls.log")
	counter := filepath.Join(directory, "count")
	bin := filepath.Join(directory, "hermes")
	script := `#!/bin/sh
{ printf '%s|' "$@"; echo; } >> "` + callLog + `"
case "$2" in
  create)
    n=$(cat "` + counter + `" 2>/dev/null || echo 0)
    n=$((n+1))
    echo "$n" > "` + counter + `"
    printf '{"id":"t_%s"}\n' "$n"
    ;;
  *) : ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return NewHermes(Config{HermesBin: bin, HermesBoard: "lassdas"}), callLog
}

func chainTestConfig() ChainConfig {
	return ChainConfig{
		RunsRoot:        "/var/lassdas/runs",
		TargetTokenPath: "/var/lassdas/secrets/target-token",
		Profiles: ChainProfiles{
			Implementer: "lassdas-implementer", ReviewA: "lassdas-review-a", ReviewB: "lassdas-review-b",
			Validate: "lassdas-validate", Publish: "lassdas-publish",
		},
	}
}

func createRecords(t *testing.T, callLog string) []string {
	t.Helper()
	records := make([]string, 0, 5)
	for _, record := range calls(t, callLog) {
		if strings.HasPrefix(record, "kanban|create|") {
			records = append(records, record)
		}
	}
	return records
}

func recordFlag(record, flag string) string {
	parts := strings.Split(record, "|")
	for index, part := range parts {
		if part == flag && index+1 < len(parts) {
			return parts[index+1]
		}
	}
	return ""
}

func TestEnsureChainCreatesFiveLinkedCards(t *testing.T) {
	hermes, callLog := stubChainHermes(t)
	chain := chainTestConfig()
	terminal, err := EnsureChain(context.Background(), hermes, chain, nil, "delivery_x", "run_x", "reword the label", 1)
	if err != nil {
		t.Fatalf("EnsureChain() error = %v", err)
	}
	if terminal != "t_5" {
		t.Fatalf("terminal card = %q", terminal)
	}
	records := createRecords(t, callLog)
	if len(records) != 5 {
		t.Fatalf("create calls = %d: %v", len(records), records)
	}
	stages := ChainStages(chain)
	for index, record := range records {
		stage := stages[index]
		if got := recordFlag(record, "--idempotency-key"); got != ChainCardKey("delivery_x", stage.Name, 1) {
			t.Fatalf("stage %s idempotency key = %q", stage.Name, got)
		}
		if got := recordFlag(record, "--assignee"); got != stage.Profile {
			t.Fatalf("stage %s assignee = %q", stage.Name, got)
		}
		if got := recordFlag(record, "--workspace"); got != "dir:/var/lassdas/runs/delivery_x" {
			t.Fatalf("stage %s workspace = %q", stage.Name, got)
		}
		parent := recordFlag(record, "--parent")
		if index == 0 && parent != "" {
			t.Fatalf("the chain head has a parent: %q", parent)
		}
		if index > 0 && parent != "t_"+string(rune('0'+index)) {
			t.Fatalf("stage %s parent = %q", stage.Name, parent)
		}
	}
}

// A chain whose creation died halfway is healed: existing stages are kept
// and the missing ones hang off the last existing card.
func TestEnsureChainHealsAPartialChain(t *testing.T) {
	hermes, callLog := stubChainHermes(t)
	chain := chainTestConfig()
	existing := map[string]BoardTask{
		ChainCardKey("delivery_x", StageImplement, 1): {ID: "kept_1", Status: "done"},
		ChainCardKey("delivery_x", StageReviewA, 1):   {ID: "kept_2", Status: "ready"},
	}
	terminal, err := EnsureChain(context.Background(), hermes, chain, existing, "delivery_x", "run_x", "", 1)
	if err != nil {
		t.Fatalf("EnsureChain() error = %v", err)
	}
	records := createRecords(t, callLog)
	if len(records) != 3 {
		t.Fatalf("create calls = %d: %v", len(records), records)
	}
	if got := recordFlag(records[0], "--parent"); got != "kept_2" {
		t.Fatalf("the first recreated stage hangs off %q, want the last kept card", got)
	}
	if terminal != "t_3" {
		t.Fatalf("terminal card = %q", terminal)
	}
}

func TestEnsureChainIsIdempotentWhenComplete(t *testing.T) {
	hermes, callLog := stubChainHermes(t)
	chain := chainTestConfig()
	existing := map[string]BoardTask{}
	for index, stage := range ChainStages(chain) {
		existing[ChainCardKey("delivery_x", stage.Name, 2)] = BoardTask{ID: "kept_" + stage.Name, Status: "todo", IdempotencyKey: ChainCardKey("delivery_x", stage.Name, 2)}
		_ = index
	}
	terminal, err := EnsureChain(context.Background(), hermes, chain, existing, "delivery_x", "run_x", "", 2)
	if err != nil {
		t.Fatalf("EnsureChain() error = %v", err)
	}
	if len(createRecords(t, callLog)) != 0 {
		t.Fatal("a complete chain was recreated")
	}
	if terminal != "kept_"+StagePublish {
		t.Fatalf("terminal card = %q", terminal)
	}
}

// An archived stage card does not count as existing: archiving released its
// idempotency key, and the healed chain recreates that stage.
func TestEnsureChainRecreatesArchivedStages(t *testing.T) {
	hermes, callLog := stubChainHermes(t)
	chain := chainTestConfig()
	existing := map[string]BoardTask{
		ChainCardKey("delivery_x", StageImplement, 1): {ID: "gone", Status: "archived"},
	}
	if _, err := EnsureChain(context.Background(), hermes, chain, existing, "delivery_x", "run_x", "", 1); err != nil {
		t.Fatalf("EnsureChain() error = %v", err)
	}
	if len(createRecords(t, callLog)) != 5 {
		t.Fatal("an archived stage was treated as alive")
	}
}

// An archived stage with a living successor is satisfied, not recreatable:
// the kanban's gating treats an archived parent as done, so a recreated
// earlier stage would dispatch immediately and run in parallel with the
// living remainder on the shared workspace.
func TestEnsureChainLeavesArchivedStagesBeforeALivingOne(t *testing.T) {
	hermes, callLog := stubChainHermes(t)
	chain := chainTestConfig()
	existing := map[string]BoardTask{
		ChainCardKey("delivery_x", StageImplement, 1): {ID: "gone", Status: "archived"},
		ChainCardKey("delivery_x", StageReviewA, 1):   {ID: "alive", Status: "running"},
	}
	terminal, err := EnsureChain(context.Background(), hermes, chain, existing, "delivery_x", "run_x", "", 1)
	if err != nil {
		t.Fatalf("EnsureChain() error = %v", err)
	}
	records := createRecords(t, callLog)
	if len(records) != 3 {
		t.Fatalf("create calls = %d: %v", len(records), records)
	}
	if got := recordFlag(records[0], "--idempotency-key"); got != ChainCardKey("delivery_x", StageReviewB, 1) {
		t.Fatalf("the first recreated stage = %q, want review-b", got)
	}
	if got := recordFlag(records[0], "--parent"); got != "alive" {
		t.Fatalf("review-b parent = %q, want the living review-a card", got)
	}
	if terminal != "t_3" {
		t.Fatalf("terminal card = %q", terminal)
	}
}

func TestChainCardKeyRoundTrips(t *testing.T) {
	key := ChainCardKey("delivery_0123", StageValidate, 3)
	deliveryID, stage, round, ok := ParseChainCardKey(key)
	if !ok || deliveryID != "delivery_0123" || stage != StageValidate || round != 3 {
		t.Fatalf("ParseChainCardKey(%q) = %q %q %d %v", key, deliveryID, stage, round, ok)
	}
	for _, invalid := range []string{
		"delivery_0123", "delivery_0123:implement", "delivery_0123:implement:r0", "delivery_0123:elsewhere:r1", ":implement:r1",
		// Non-canonical spellings Sscanf alone would accept.
		"delivery_0123:implement:r01", "delivery_0123:implement:r1x", "delivery_0123:implement:r+5", "delivery_0123:implement:r 7",
	} {
		if _, _, _, ok := ParseChainCardKey(invalid); ok {
			t.Fatalf("ParseChainCardKey(%q) accepted a foreign key", invalid)
		}
	}
}

func TestOrchestrationValidation(t *testing.T) {
	base := Config{Orchestration: "cards", Chain: chainTestConfig()}
	if err := base.validateOrchestration(); err != nil {
		t.Fatalf("validateOrchestration() error = %v", err)
	}
	if !base.OrchestrationCards() {
		t.Fatal("cards orchestration did not report itself")
	}
	withoutRoot := chainTestConfig()
	withoutRoot.RunsRoot = ""
	withoutToken := chainTestConfig()
	withoutToken.TargetTokenPath = ""
	cases := map[string]Config{
		"unknown mode":         {Orchestration: "swarm"},
		"missing profile":      {Orchestration: "cards", Chain: ChainConfig{RunsRoot: "/r", TargetTokenPath: "/t", Profiles: ChainProfiles{Implementer: "a", ReviewA: "b", ReviewB: "c", Validate: "d"}}},
		"duplicate names":      {Orchestration: "cards", Chain: ChainConfig{RunsRoot: "/r", TargetTokenPath: "/t", Profiles: ChainProfiles{Implementer: "a", ReviewA: "a", ReviewB: "c", Validate: "d", Publish: "e"}}},
		"missing runs dir":     {Orchestration: "cards", Chain: withoutRoot},
		"missing token path":   {Orchestration: "cards", Chain: withoutToken},
		"runner profile reuse": {Orchestration: "cards", HermesProfile: "lassdas-implementer", Chain: chainTestConfig()},
	}
	for name, config := range cases {
		if err := config.validateOrchestration(); err == nil {
			t.Errorf("validateOrchestration() accepted %s", name)
		}
	}
	for _, mode := range []string{"", "runner"} {
		config := Config{Orchestration: mode}
		if err := config.validateOrchestration(); err != nil {
			t.Errorf("validateOrchestration() rejected mode %q: %v", mode, err)
		}
	}
}
