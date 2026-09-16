package main

import (
	"os"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/initwizard"
)

// The instruction the agent reads names every answer the wizard asks for,
// so the two cannot drift apart silently.
func TestSetupInstructionNamesEveryAnswer(t *testing.T) {
	raw, err := os.ReadFile("../../docs/SETUP.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, requirement := range append(initwizard.RequiredAnswers(), initwizard.OptionalAnswers()...) {
		if !strings.Contains(text, "`"+requirement.ID+"`") {
			t.Errorf("docs/SETUP.md does not name the answer %q", requirement.ID)
		}
	}
	for _, want := range []string{"lassdas setup check", "lassdas setup secrets", "lassdas setup apply", "lassdas setup smoke", ".lassdas/", "agreement.md", "progress.md"} {
		if !strings.Contains(text, want) {
			t.Errorf("docs/SETUP.md should mention %q", want)
		}
	}
}
