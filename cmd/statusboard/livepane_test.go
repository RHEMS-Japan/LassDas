package main

import (
	"os/exec"
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
	if passed < 17 {
		t.Fatalf("only %d checks ran; the harness is not checking what it claims to\n%s", passed, output)
	}
}
