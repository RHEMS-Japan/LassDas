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

	changed := func() bool { return false }
	reported := func() bool { return true }

	action, code := classifyChainFailure(runtime.StageImplement, undecided, changed)
	if action != actionReport || code != hook.TerminalModelFailed {
		t.Fatalf("implement failure = %v %v", action, code)
	}
	// An implement card whose agent reported instead of changing is not an
	// ending at all: the engine decides what the report asked about and
	// starts the same round again. The code travels for the case where the
	// engine will not answer the return any more, and it is the ladder's —
	// a round handed back past the answers the engine makes is the seat's
	// model refusing the work.
	action, code = classifyChainFailure(runtime.StageImplement, undecided, reported)
	if action != actionAnswerReturn || code != hook.TerminalModelFailed {
		t.Fatalf("implement report = %v %v", action, code)
	}
	// The applier of a designed request can do exactly the same thing, and
	// its empty working copy used to go to the review and end as a model
	// failure with the report nowhere.
	action, code = classifyChainFailure(runtime.StageApply, undecided, reported)
	if action != actionAnswerReturn || code != hook.TerminalModelFailed {
		t.Fatalf("apply report = %v %v", action, code)
	}
	action, code = classifyChainFailure(runtime.StageApply, undecided, changed)
	if action != actionReport || code != hook.TerminalModelFailed {
		t.Fatalf("apply failure = %v %v", action, code)
	}
	action, code = classifyChainFailure(runtime.StagePublish, undecided, reported)
	if action != actionReport || code != hook.TerminalReleaseFailed {
		t.Fatalf("publish failure = %v %v", action, code)
	}
	// The code a revise carries is not inert, and it is no longer a model
	// failure. An operator who configured a round limit ends a run out of
	// the regenerate arm under whatever the classification carried, and on
	// this path both seats answered — what the requester reads is that the
	// reviews did not agree within the rounds that operator paid for.
	action, code = classifyChainFailure(runtime.StageValidate, decided("revise"), changed)
	if action != actionRegenerate || code != hook.TerminalNonconverged {
		t.Fatalf("revise = %v %v", action, code)
	}
	// A record this engine can no longer seal, kept because an upgrade can
	// find one in flight. Nothing asks the requester about it.
	action, code = classifyChainFailure(runtime.StageValidate, decided("nonconverged"), changed)
	if action != actionReport || code != hook.TerminalNonconverged {
		t.Fatalf("nonconverged = %v %v", action, code)
	}
	// A converged round the deterministic validation refused starts another
	// round instead of ending the delivery. The code travels with it for the
	// one path that still ends a run out of that arm, an operator's own
	// round limit.
	action, code = classifyChainFailure(runtime.StageValidate, decided("converged"), changed)
	if action != actionRegenerate || code != hook.TerminalValidationFailed {
		t.Fatalf("converged but failed = %v %v", action, code)
	}
	action, code = classifyChainFailure(runtime.StageValidate, undecided, changed)
	if action != actionReport || code != hook.TerminalModelFailed {
		t.Fatalf("undecided validate = %v %v", action, code)
	}
	action, code = classifyChainFailure(runtime.StageValidate, decided("elsewhere"), changed)
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

// The round limit is the operator's own and the default is none. A
// destination that declares three stages — which is every destination
// written before this — gets a delivery that is not stopped by that number.
func TestConsumerRoundLimitDefaultsToUnbounded(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "consumer.json")
	if err := os.WriteFile(path, []byte(`{"max_stages":3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	limit, err := consumerRoundLimit(path)
	if err != nil || limit != 0 {
		t.Fatalf("consumerRoundLimit = %d, %v; want unbounded", limit, err)
	}
	if err := os.WriteFile(path, []byte(`{"max_stages":3,"max_rounds":4}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if limit, err := consumerRoundLimit(path); err != nil || limit != 4 {
		t.Fatalf("consumerRoundLimit = %d, %v; want 4", limit, err)
	}
	if err := os.WriteFile(path, []byte(`{"max_rounds":999}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := consumerRoundLimit(path); err == nil {
		t.Fatal("a limit above the record ceiling was accepted")
	}
}
