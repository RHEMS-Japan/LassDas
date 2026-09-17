package state

import (
	"testing"

	"automation.internal/ticket-ingress/internal/hook"
)

// The ledger and the hook must agree on the terminal codes: a code the hook
// accepts but the ledger refuses leaves a run in terminal_report_pending
// after its comment was posted (the investigating designer's four endings
// were the first to show it).
func TestLedgerAcceptsEveryTerminalCodeTheHookAccepts(t *testing.T) {
	// Every ending, from the one list, so a code added later is checked
	// here without anybody remembering to add it (review of #201).
	for _, code := range hook.AllTerminalCodes() {
		if !code.Valid() {
			t.Errorf("hook refuses %s", code)
		}
		if !validTerminalCode(string(code)) {
			t.Errorf("ledger refuses %s", code)
		}
	}
	if validTerminalCode("not_a_code") || hook.TerminalCode("not_a_code").Valid() {
		t.Error("an unknown code was accepted")
	}
}
