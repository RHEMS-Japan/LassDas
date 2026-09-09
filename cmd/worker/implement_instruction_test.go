package main

import (
	"context"
	"strings"
	"testing"
)

// The root the instruction names must be absolute: the agent's file tools
// resolve a relative path against their own home, so an instruction built
// on a relative root would send every write there.
func TestImplementInstructionRefusesARelativeRoot(t *testing.T) {
	fixture := newAgentFixture(t, "true", "true")
	err := run(context.Background(), []string{
		"implement-instruction", "--config", fixture.configPath, "--tool-sha", cliToolSHA,
		"--draft", fixture.draftPath, "--out", fixture.path("INSTRUCTION.md"),
		"--repo-root", "relative/target-repo",
	})
	if err == nil || !strings.Contains(err.Error(), "arguments are invalid") {
		t.Fatalf("a relative root was accepted: %v", err)
	}
}
