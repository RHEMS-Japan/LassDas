package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/runtime"
)

// The instruction the implementer receives must carry what the reception
// decided this ticket changes. The runner passes it only when the run has
// one, so an orchestration without a derivation is unchanged.
func TestImplementInstructionPassesTheDerivationWhenThereIsOne(t *testing.T) {
	record := filepath.Join(t.TempDir(), "argv.txt")
	script := filepath.Join(t.TempDir(), "worker")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+record+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	consumerPath := filepath.Join(workspace, "consumer.json")
	if err := os.WriteFile(consumerPath, []byte(`{"models":{"reviewers":[{"id":"review-a"},{"id":"review-b"}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	config := runtime.Config{WorkerBin: script, ConsumerConfigPath: consumerPath}
	config.Identity.EngineSHA = strings.Repeat("ab", 20)
	pipeline := &Pipeline{Config: config, Workspace: workspace, Logger: trailTestLogger{}}

	_ = pipeline.RenderImplementInstruction(context.Background(), 1)
	first, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("the worker was not run: %v", err)
	}
	if strings.Contains(string(first), "--derivation") {
		t.Fatalf("a run with no derivation passed one: %s", first)
	}

	if err := os.WriteFile(pipeline.path("derivation.json"), []byte(`{"target_files":["README.md"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = pipeline.RenderImplementInstruction(context.Background(), 1)
	second, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(second), "--derivation "+pipeline.path("derivation.json")) {
		t.Fatalf("the derivation did not reach the instruction: %s", second)
	}
}
