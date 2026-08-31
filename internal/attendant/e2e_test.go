package attendant

import (
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
)

// The observation card's key must stay OUTSIDE the chain namespace: the
// five-stage machinery filters the board by ParseChainCardKey, and a key it
// parses would drag the card into round archiving and regeneration.
func TestE2ECardKeyStaysOutsideTheChainNamespace(t *testing.T) {
	if _, _, _, ok := runtime.ParseChainCardKey(e2eCardKey("delivery_0123456789abcdef0123456789abcdef")); ok {
		t.Fatal("the e2e card key parses as a chain card key")
	}
}

// Enabling the role must NOT reach back through the ledger's past
// successes: only runs claimed after the operator's cut-off are observed,
// and every unparsable or degenerate input fails closed.
func TestE2EObservableHonoursTheCutOff(t *testing.T) {
	cutOff := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	chain := runtime.ChainConfig{E2EProfile: "lassdas-e2e", E2EEnabledAfter: cutOff.Format(time.RFC3339)}
	after := state.RunOverview{TerminalCode: "success", ClaimedAt: cutOff.UnixMilli() + 1}
	cases := map[string]struct {
		chain runtime.ChainConfig
		run   state.RunOverview
		want  bool
	}{
		"claimed after the cut-off":     {chain, after, true},
		"claimed before the cut-off":    {chain, state.RunOverview{TerminalCode: "success", ClaimedAt: cutOff.UnixMilli() - 1}, false},
		"never claimed":                 {chain, state.RunOverview{TerminalCode: "success"}, false},
		"role off":                      {runtime.ChainConfig{E2EEnabledAfter: chain.E2EEnabledAfter}, after, false},
		"run did not succeed":           {chain, state.RunOverview{TerminalCode: "cancelled", ClaimedAt: after.ClaimedAt}, false},
		"cut-off missing (fail closed)": {runtime.ChainConfig{E2EProfile: "lassdas-e2e"}, after, false},
	}
	for name, c := range cases {
		if got := e2eObservable(c.chain, c.run); got != c.want {
			t.Errorf("%s: e2eObservable() = %v, want %v", name, got, c.want)
		}
	}
}
