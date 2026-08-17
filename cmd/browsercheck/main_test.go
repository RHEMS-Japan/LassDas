package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseCommandRequiresAllAbsoluteUniqueArguments(t *testing.T) {
	root := t.TempDir()
	args := []string{
		"--config", filepath.Join(root, "config.json"),
		"--ticket", filepath.Join(root, "ticket.json"),
		"--source", filepath.Join(root, "source.json"),
		"--candidate", filepath.Join(root, "candidate.json"),
		"--decision", filepath.Join(root, "decision.json"),
		"--validation", filepath.Join(root, "validation.json"),
		"--review", filepath.Join(root, "claude-review.json"),
		"--review", filepath.Join(root, "codex-review.json"),
		"--staging-proof", filepath.Join(root, "staging-proof.json"),
		"--environment", "staging",
		"--tool-sha", strings.Repeat("a", 40),
		"--evidence-out", filepath.Join(root, "evidence.json"),
		"--screenshot-out", filepath.Join(root, "screen.png"),
	}
	command, err := parseCommand(args)
	if err != nil || command.environment != "staging" {
		t.Fatalf("parseCommand() = %+v, %v", command, err)
	}
	for name, mutate := range map[string]func([]string){
		"missing":         func(values []string) { values[1] = "" },
		"relative":        func(values []string) { values[1] = "config.json" },
		"bad environment": func(values []string) { values[19] = "preview" },
		"bad tool sha":    func(values []string) { values[21] = "main" },
		"extra":           func(values []string) { values[0] = "--unknown" },
	} {
		t.Run(name, func(t *testing.T) {
			values := append([]string(nil), args...)
			mutate(values)
			if _, err := parseCommand(values); err == nil {
				t.Fatal("parseCommand() accepted invalid arguments")
			}
		})
	}
}

func TestParseProductionRequiresPriorEvidenceChain(t *testing.T) {
	root := t.TempDir()
	args := []string{
		"--config", filepath.Join(root, "config.json"), "--ticket", filepath.Join(root, "ticket.json"),
		"--source", filepath.Join(root, "source.json"), "--candidate", filepath.Join(root, "candidate.json"),
		"--decision", filepath.Join(root, "decision.json"), "--validation", filepath.Join(root, "validation.json"),
		"--review", filepath.Join(root, "claude.json"), "--review", filepath.Join(root, "codex.json"),
		"--staging-proof", filepath.Join(root, "staging.json"),
		"--production-proof", filepath.Join(root, "production.json"),
		"--prior-evidence", filepath.Join(root, "staging-visible.json"),
		"--prior-screenshot", filepath.Join(root, "staging.png"),
		"--environment", "production", "--tool-sha", strings.Repeat("b", 40),
		"--evidence-out", filepath.Join(root, "production-visible.json"),
		"--screenshot-out", filepath.Join(root, "production.png"),
	}
	if _, err := parseCommand(args); err != nil {
		t.Fatal(err)
	}
	if _, err := parseCommand(args[:len(args)-6]); err == nil {
		t.Fatal("parseCommand() accepted an incomplete production chain")
	}
}

func TestWriteScreenshotExclusiveNeverReplaces(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "screen.png")
	content := []byte(strings.Repeat("x", 64))
	if err := writeScreenshotExclusive(filename, content); err != nil {
		t.Fatal(err)
	}
	if err := writeScreenshotExclusive(filename, []byte(strings.Repeat("y", 64))); err == nil {
		t.Fatal("writeScreenshotExclusive() replaced an existing file")
	}
	actual, err := os.ReadFile(filename)
	if err != nil || string(actual) != string(content) {
		t.Fatalf("content = %q, %v", actual, err)
	}
	info, err := os.Stat(filename)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, %v", info.Mode().Perm(), err)
	}
}
