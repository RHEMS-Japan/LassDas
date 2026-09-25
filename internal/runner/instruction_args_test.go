package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/runtime"
)

// The implement card's instruction is rendered without a list of files the
// agent must keep to. The reception contract still exists - the readiness
// stages are bound to it - but it names no files, and nothing turns it into
// a bound on the change. A guessed list is how a live run ended having
// changed nothing: the request named the files it wanted created, the
// reception picked two existing ones instead, and the implementer refused to
// work outside them (2026-09-25).
func TestTheImplementCardsInstructionCarriesNoPerRequestFileList(t *testing.T) {
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
	// A reception contract is present, as it is on every real run.
	if err := os.WriteFile(pipeline.path("readiness-ticket.json"), []byte(`{"target_files":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := pipeline.RenderImplementInstruction(context.Background(), 1); err != nil {
		t.Fatalf("RenderImplementInstruction: %v", err)
	}
	argv, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("the worker was not run: %v", err)
	}
	if !strings.Contains(string(argv), "implement-instruction") {
		t.Fatalf("the instruction was not rendered: %s", argv)
	}
	if strings.Contains(string(argv), "--targets") {
		t.Fatalf("a per-request file list reached the implementer: %s", argv)
	}
}

// The launching path must be covered too, not only the rendering of the
// instruction file: the runner mode starts the implementing agent itself,
// and a file list reaching it there would bind the change just as tightly.
// This goes when the runner mode does.
func TestTheRunnerModesImplementLaunchCarriesNoPerRequestFileList(t *testing.T) {
	record := filepath.Join(t.TempDir(), "argv.txt")
	script := filepath.Join(t.TempDir(), "worker")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+record+"\nexit 9\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	config := runtime.Config{WorkerBin: script, ConsumerConfigPath: writeRunnerConfig(t, runnerFixtureConfig(t, 2, 3, true))}
	config.Identity.EngineSHA = strings.Repeat("ab", 20)
	pipeline := &Pipeline{Config: config, Workspace: workspace, Logger: trailTestLogger{}}
	if err := os.WriteFile(pipeline.path("readiness-ticket.json"), []byte(`{"target_files":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// The real path, not a hand-built argument list. The fake worker fails,
	// so the round ends at its first step; what it was called with is the
	// point.
	_, _ = pipeline.implementRounds(context.Background(), filepath.Join(workspace, "target-repo"), filepath.Join(workspace, "target-base"), strings.Repeat("c", 40))
	argv, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("the worker was not run: %v", err)
	}
	if !strings.Contains(string(argv), "implement ") {
		t.Fatalf("the implement launch did not run: %s", argv)
	}
	if strings.Contains(string(argv), "--targets") {
		t.Fatalf("a per-request file list reached the implementer: %s", argv)
	}
}
