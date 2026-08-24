package attendant

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runtime"
)

func TestChainOwnerRunIDIsStableAndPositive(t *testing.T) {
	first := chainOwnerRunID("delivery_abc")
	if first <= 0 {
		t.Fatalf("owner id = %d", first)
	}
	if chainOwnerRunID("delivery_abc") != first {
		t.Fatal("owner id is not stable across derivations")
	}
	if chainOwnerRunID("delivery_other") == first {
		t.Fatal("distinct deliveries derived one owner id")
	}
}

func TestChainViewForPicksTheCurrentRound(t *testing.T) {
	tasks := []runtime.BoardTask{
		{ID: "r1-impl", Status: "done", IdempotencyKey: runtime.ChainCardKey("delivery_x", runtime.StageImplement, 1)},
		{ID: "r1-val", Status: "archived", IdempotencyKey: runtime.ChainCardKey("delivery_x", runtime.StageValidate, 1)},
		{ID: "r2-impl", Status: "running", IdempotencyKey: runtime.ChainCardKey("delivery_x", runtime.StageImplement, 2)},
		{ID: "other", Status: "running", IdempotencyKey: runtime.ChainCardKey("delivery_y", runtime.StageImplement, 5)},
		{ID: "foreign", Status: "running", IdempotencyKey: "delivery_x"},
	}
	view := chainViewFor(tasks, "delivery_x")
	if view.round != 2 {
		t.Fatalf("round = %d", view.round)
	}
	if view.cards[runtime.StageImplement].ID != "r2-impl" {
		t.Fatalf("current implement card = %+v", view.cards[runtime.StageImplement])
	}
	// The archived round-1 validate is out of the living set; the done
	// round-1 implement stays (retirement sweeps need it).
	if len(view.all) != 2 {
		t.Fatalf("living chain cards = %+v", view.all)
	}
}

func TestClassifyChainFailure(t *testing.T) {
	decided := func(outcome string) func() (string, error) {
		return func() (string, error) { return outcome, nil }
	}
	undecided := func() (string, error) { return "", errors.New("missing") }

	action, code := classifyChainFailure(runtime.StageImplement, undecided, undecided)
	if action != actionReport || code != hook.TerminalModelFailed {
		t.Fatalf("implement failure = %v %v", action, code)
	}
	action, code = classifyChainFailure(runtime.StagePublish, undecided, undecided)
	if action != actionReport || code != hook.TerminalReleaseFailed {
		t.Fatalf("publish failure = %v %v", action, code)
	}
	action, _ = classifyChainFailure(runtime.StageValidate, decided("revise"), undecided)
	if action != actionRegenerate {
		t.Fatalf("revise = %v", action)
	}
	action, code = classifyChainFailure(runtime.StageValidate, decided("nonconverged"), decided("clarification_required"))
	if action != actionAskQuestion || code != hook.TerminalNonconverged {
		t.Fatalf("nonconverged with question = %v %v", action, code)
	}
	action, code = classifyChainFailure(runtime.StageValidate, decided("nonconverged"), undecided)
	if action != actionReport || code != hook.TerminalNonconverged {
		t.Fatalf("nonconverged without question = %v %v", action, code)
	}
	action, code = classifyChainFailure(runtime.StageValidate, decided("converged"), undecided)
	if action != actionReport || code != hook.TerminalValidationFailed {
		t.Fatalf("converged but failed = %v %v", action, code)
	}
	action, code = classifyChainFailure(runtime.StageValidate, undecided, undecided)
	if action != actionReport || code != hook.TerminalModelFailed {
		t.Fatalf("undecided validate = %v %v", action, code)
	}
	action, code = classifyChainFailure(runtime.StageValidate, decided("elsewhere"), undecided)
	if action != actionReport || code != hook.TerminalModelFailed {
		t.Fatalf("unknown decision = %v %v", action, code)
	}
}

func TestReadFieldWalksNestedObjects(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "baseline.json"),
		[]byte(`{"baseline":{"Integration":{"SHA":"abc"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	value, err := readField(directory, "baseline.json", "baseline", "Integration", "SHA")
	if err != nil || value != "abc" {
		t.Fatalf("readField = %q, %v", value, err)
	}
	if _, err := readField(directory, "baseline.json", "baseline", "absent"); err == nil {
		t.Fatal("a missing field was read")
	}
	if _, err := readField(directory, "absent.json", "x"); err == nil {
		t.Fatal("a missing file was read")
	}
}

func TestReadEnvelopeRefusesAnotherDelivery(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "ticket-envelope.json"),
		[]byte(`{"delivery_id":"delivery_a"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readEnvelope(directory, "delivery_b"); err == nil {
		t.Fatal("an envelope for another delivery was accepted")
	}
	envelope, err := readEnvelope(directory, "delivery_a")
	if err != nil || envelope.DeliveryID != "delivery_a" {
		t.Fatalf("readEnvelope = %+v, %v", envelope, err)
	}
}

func TestConsumerMaxStagesReadsTheKernelLimit(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "consumer.json")
	if err := os.WriteFile(path, []byte(`{"max_stages":3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	limit, err := consumerMaxStages(path)
	if err != nil || limit != 3 {
		t.Fatalf("consumerMaxStages = %d, %v", limit, err)
	}
	if err := os.WriteFile(path, []byte(`{"max_stages":9}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := consumerMaxStages(path); err == nil {
		t.Fatal("an out-of-range limit was accepted")
	}
}
