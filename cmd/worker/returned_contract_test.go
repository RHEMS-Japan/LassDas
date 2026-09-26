package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/worker"
)

func TestAnImmediateRetryPreservesConstraints(t *testing.T) {
	for _, withDesign := range []bool{false, true} {
		note := emptyResultRetryNote(withDesign)
		for _, falseClaim := range []string{"Your previous answer reported the work as done.", "Nothing you described exists.", "Make the changes now with your tools"} {
			if strings.Contains(note, falseClaim) {
				t.Errorf("withDesign=%v: an unchanged tree does not justify %q", withDesign, falseClaim)
			}
		}
		for _, boundary := range []string{"original request", "no-change condition", "not proof of completion"} {
			if !strings.Contains(note, boundary) {
				t.Errorf("withDesign=%v: retry lost %q", withDesign, boundary)
			}
		}
	}
}

// Observe the actual second process's prompt, not just the note helper. A
// report with no edits remains returned work, not a fabricated completion.
func TestAnImmediateReturnRetryCarriesTheOriginalConstraint(t *testing.T) {
	for _, role := range []string{"implementer", "applier"} {
		t.Run(role, func(t *testing.T) {
			promptPath := filepath.Join(t.TempDir(), "seen-prompt")
			marker := filepath.Join(t.TempDir(), "launches")
			body := `printf x >> ` + marker + `; for a in "$@"; do last="$a"; done; printf '%s' "$last" > ` + promptPath + `; echo 'The reference is absent. No changes made as instructed.'`
			fixture := newTunedAgentFixture(t, body, "true", func(binaries string, config *worker.Config) {
				writeStandInAgent(t, binaries, "stand-in-applier", body)
				applier := config.Agents.Implementer
				applier.ID = "apply-agent"
				applier.Command = "stand-in-applier"
				config.Agents.Applier = &applier
			})
			const original = "If the required reference is absent, do not infer its contents or modify files."
			instruction := fixture.path("INSTRUCTION.md")
			if err := os.WriteFile(instruction, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
			record := fixture.path("run.json")
			err := run(t.Context(), []string{
				"run-instruction", "--role", role, "--config", fixture.configPath,
				"--tool-sha", cliToolSHA, "--draft", fixture.draftPath, "--instruction", instruction,
				"--repo-root", fixture.repoRoot, "--base-sha", fixture.baseSHA, "--stage", "1", "--out", record,
			})
			if err == nil || !strings.Contains(err.Error(), "the round is answered and run again") {
				t.Fatalf("an unchanged report bypassed recovery: %v", err)
			}
			attempts, err := os.ReadFile(marker)
			if err != nil || string(attempts) != "xx" {
				t.Fatalf("expected two actual launches, got %q, %v", attempts, err)
			}
			prompt, err := os.ReadFile(promptPath)
			if err != nil {
				t.Fatal(err)
			}
			for _, required := range []string{original, "no-change condition", "not proof of completion"} {
				if !strings.Contains(string(prompt), required) {
					t.Errorf("the second process did not receive %q", required)
				}
			}
			if strings.Contains(string(prompt), "Nothing you described exists.") {
				t.Fatal("the second process was falsely accused of claiming edits")
			}
			var sealed worker.AgentRun
			if err := worker.ReadJSONFile(record, worker.MaxArtifactJSONBytes, &sealed); err != nil {
				t.Fatal(err)
			}
			if len(sealed.ChangedFiles) != 0 || sealed.EmptyAttempts != 1 || !worker.IsSendBack(sealed) {
				t.Fatalf("the measured no-change result was lost: %+v", sealed)
			}
		})
	}
}
