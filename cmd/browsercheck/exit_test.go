package main

import (
	"errors"
	"fmt"
	"testing"

	"automation.internal/ticket-ingress/internal/visiblecheck"
)

// The runner waits out a page's refusal and nothing else, so the exit
// codes must keep the three outcomes apart.
func TestExitCodesTellTheRefusalsApart(t *testing.T) {
	if exitCode(errEvidenceRejected) != visiblecheck.ExitEvidenceRejected || exitCode(fmt.Errorf("wrapped: %w", errEvidenceRejected)) != visiblecheck.ExitEvidenceRejected {
		t.Fatal("a refused page is exit 3")
	}
	if exitCode(errSignInRefused) != visiblecheck.ExitSignInRefused {
		t.Fatal("a refused login is exit 4")
	}
	if exitCode(errors.New("browser ticket is invalid")) != 1 || exitCode(errors.New("browser screenshot could not be written")) != 1 {
		t.Fatal("every other failure is exit 1")
	}
}
