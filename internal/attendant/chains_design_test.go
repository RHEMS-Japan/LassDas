package attendant

import (
	"os"
	"path/filepath"
	"testing"

	"automation.internal/ticket-ingress/internal/runtime"
)

func TestChainViewForSeparatesDesignRounds(t *testing.T) {
	tasks := []runtime.BoardTask{
		{ID: "t1", Status: "done", IdempotencyKey: "delivery-1:investigate:d1"},
		{ID: "t2", Status: "done", IdempotencyKey: "delivery-1:design-review-a:d1"},
		{ID: "t3", Status: "failed", IdempotencyKey: "delivery-1:design-decide:d1"},
		{ID: "t4", Status: "todo", IdempotencyKey: "delivery-1:investigate:d2"},
		{ID: "t5", Status: "todo", IdempotencyKey: "delivery-1:apply:r1"},
		{ID: "t6", Status: "todo", IdempotencyKey: "delivery-1:review-a:r1"},
		{ID: "t7", Status: "archived", IdempotencyKey: "delivery-1:publish:r1"},
		{ID: "t8", Status: "todo", IdempotencyKey: "delivery-2:investigate:d1"},
	}
	view := chainViewFor(tasks, "delivery-1")
	if view.designRound != 2 || view.round != 1 || !view.hasChain() {
		t.Fatalf("rounds: design %d implement %d", view.designRound, view.round)
	}
	if len(view.designCards) != 1 || view.designCards[runtime.StageInvestigate].ID != "t4" {
		t.Errorf("design cards of the newest round: %+v", view.designCards)
	}
	if len(view.cards) != 2 || view.cards[runtime.StageApply].ID != "t5" {
		t.Errorf("implementation cards: %+v", view.cards)
	}
	if len(view.all) != 6 {
		t.Errorf("live cards = %d", len(view.all))
	}
	keys := view.existingKeys("delivery-1")
	for _, want := range []string{"delivery-1:investigate:d2", "delivery-1:apply:r1", "delivery-1:review-a:r1"} {
		if _, ok := keys[want]; !ok {
			t.Errorf("existing keys lack %s: %v", want, keys)
		}
	}
	if task, ok := view.card(runtime.StageInvestigate); !ok || task.ID != "t4" {
		t.Errorf("card(investigate) = %+v %v", task, ok)
	}
	if rounds := view.rounds(); rounds.Design != 2 || rounds.Implement != 1 {
		t.Errorf("rounds() = %+v", rounds)
	}
	if chainViewFor(nil, "delivery-1").hasChain() {
		t.Error("empty board has a chain")
	}
}

func TestConsumerDesignMaxRoundsReadsTheLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "consumer.json")
	if err := os.WriteFile(path, []byte(`{"max_stages":3,"design_max_rounds":2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := consumerDesignMaxRounds(path); got != 2 {
		t.Errorf("limit = %d", got)
	}
	if err := os.WriteFile(path, []byte(`{"max_stages":3}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := consumerDesignMaxRounds(path); got != defaultDesignMaxRounds {
		t.Errorf("default = %d", got)
	}
	if got := consumerDesignMaxRounds(filepath.Join(t.TempDir(), "missing.json")); got != defaultDesignMaxRounds {
		t.Errorf("missing file = %d", got)
	}
}

func TestDesignObjectionRecorded(t *testing.T) {
	runDir := t.TempDir()
	if objected, _ := designObjectionRecorded(runDir, 1); objected {
		t.Error("objection reported with no record")
	}
	if err := os.MkdirAll(filepath.Join(runDir, "history", "stage-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "history", "stage-1", "design-objection.json"), []byte(`{"reason":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if objected, _ := designObjectionRecorded(runDir, 1); !objected {
		t.Error("sealed objection not seen")
	}
	if objected, _ := designObjectionRecorded(runDir, 2); objected {
		t.Error("objection of another round reported")
	}
}
