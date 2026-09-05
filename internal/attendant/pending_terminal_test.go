package attendant

import (
	"testing"

	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
)

// A run whose terminal report stayed pending is re-submitted only when the
// pieces the report is rebuilt from are still there; otherwise it is left to
// a person — never requeued, which would rerun the whole delivery.
func TestPendingTerminalIsResubmittedOnlyWithItsCodeAndCards(t *testing.T) {
	cards := chainViewFor([]runtime.BoardTask{
		{ID: "t_i1", Status: "blocked", IdempotencyKey: runtime.ChainCardKey("delivery-1", runtime.StageInvestigate, 1)},
	}, "delivery-1")
	if action, _ := classifyPendingTerminal(state.RunOverview{DeliveryID: "delivery-1", TerminalCode: "model_failed"}, cards); action != pendingTerminalResubmit {
		t.Fatalf("with a code and cards: %s", action)
	}
	if action, reason := classifyPendingTerminal(state.RunOverview{DeliveryID: "delivery-1", TerminalCode: "model_failed"}, chainViewFor(nil, "delivery-1")); action != pendingTerminalNeedsOperator || reason == "" {
		t.Fatalf("without cards: %s %q", action, reason)
	}
	if action, _ := classifyPendingTerminal(state.RunOverview{DeliveryID: "delivery-1"}, cards); action != pendingTerminalNeedsOperator {
		t.Fatalf("without a code: %s", action)
	}
	if action, _ := classifyPendingTerminal(state.RunOverview{DeliveryID: "delivery-1", TerminalCode: "not-a-code"}, cards); action != pendingTerminalNeedsOperator {
		t.Fatalf("with an unknown code: %s", action)
	}
}
