package attendant

import (
	"testing"

	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
)

// A run whose terminal report stayed pending is re-submitted from its row:
// only a success or an investigated ending is rebuilt from the chain cards,
// so only those are left to a person when the cards are gone. Nothing is
// requeued, which would rerun the whole delivery.
func TestPendingTerminalIsResubmittedWithItsCodeAndNeedsCardsOnlyForRebuiltReports(t *testing.T) {
	cards := chainViewFor([]runtime.BoardTask{
		{ID: "t_i1", Status: "blocked", IdempotencyKey: runtime.ChainCardKey("delivery-1", runtime.StageInvestigate, 1)},
	}, "delivery-1")
	none := chainViewFor(nil, "delivery-1")
	if action, _ := classifyPendingTerminal(state.RunOverview{DeliveryID: "delivery-1", TerminalCode: "model_failed"}, cards); action != pendingTerminalResubmit {
		t.Fatalf("with a code and cards: %s", action)
	}
	for _, code := range []string{"model_failed", "internal_failed", "readiness_rejected", "input_rejected", "cancelled"} {
		if action, reason := classifyPendingTerminal(state.RunOverview{DeliveryID: "delivery-1", TerminalCode: code}, none); action != pendingTerminalResubmit {
			t.Fatalf("%s without cards: %s %q, want a resubmit from the row", code, action, reason)
		}
	}
	for _, code := range []string{"success", "investigated"} {
		if action, reason := classifyPendingTerminal(state.RunOverview{DeliveryID: "delivery-1", TerminalCode: code}, none); action != pendingTerminalNeedsOperator || reason == "" {
			t.Fatalf("%s without cards: %s %q, want an operator", code, action, reason)
		}
		if action, _ := classifyPendingTerminal(state.RunOverview{DeliveryID: "delivery-1", TerminalCode: code}, cards); action != pendingTerminalResubmit {
			t.Fatalf("%s with cards: %s", code, action)
		}
	}
	if action, _ := classifyPendingTerminal(state.RunOverview{DeliveryID: "delivery-1"}, cards); action != pendingTerminalNeedsOperator {
		t.Fatalf("without a code: %s", action)
	}
	if action, _ := classifyPendingTerminal(state.RunOverview{DeliveryID: "delivery-1", TerminalCode: "not-a-code"}, cards); action != pendingTerminalNeedsOperator {
		t.Fatalf("with an unknown code: %s", action)
	}
}
