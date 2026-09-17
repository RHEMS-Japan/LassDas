package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The live pane is JavaScript, and three defects lived in it at once with
// no test able to see any of them. This runs the page's own code against a
// small DOM and checks what a reader would experience. It needs node; where
// there is none the check says so rather than passing quietly.
func TestLivePaneBehaviour(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; the live pane's behaviour was not checked")
	}
	output, err := exec.Command(node, "testdata/livepane_check.mjs", "board.html").CombinedOutput()
	passed := 0
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		switch {
		case strings.HasPrefix(line, "FAIL"):
			t.Error(line)
		case strings.HasPrefix(line, "PASS"):
			passed++
		}
	}
	if err != nil && !strings.Contains(string(output), "FAIL") {
		t.Fatalf("the check could not run: %v\n%s", err, output)
	}
	// A harness that stops checking says nothing and leaves no FAIL line,
	// so the count is what says it ran: replacing the whole file with one
	// empty print passed before this (review of #200).
	if passed < 20 {
		t.Fatalf("only %d checks ran; the harness is not checking what it claims to\n%s", passed, output)
	}
}

// Every card entry point clears the record that says which step is
// running. Only the one-process mode did, so in the cards the record
// outlived the card and the ticket page pulsed "いま動いています" beside a
// finished run until the two-hour bound expired (review of #200).
func TestEveryCardEntryPointClearsTheRunningStep(t *testing.T) {
	for _, file := range []string{"chain_stage.go", "e2e.go", "deliver.go"} {
		body, err := os.ReadFile(filepath.Join("..", "runner", file))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), "ClearCurrentStep(workspace)") {
			t.Errorf("cmd/runner/%s runs a card and never clears the running step", file)
		}
	}
}
