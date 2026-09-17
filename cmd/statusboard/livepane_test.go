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
	ran := 0
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		switch {
		case strings.HasPrefix(line, "FAIL"):
			t.Error(line)
			ran++
		case strings.HasPrefix(line, "PASS"):
			ran++
		}
	}
	if err != nil && !strings.Contains(string(output), "FAIL") {
		t.Fatalf("the check could not run: %v\n%s", err, output)
	}
	// A harness that stops checking says nothing and leaves no FAIL line,
	// so the count is what says it ran: replacing the whole file with one
	// empty print passed before this. A failure counts as having run, or
	// one behaviour going wrong produced a second, untrue complaint that
	// the harness had stopped (review of #200).
	if ran < 21 {
		t.Fatalf("only %d checks ran; the harness is not checking what it claims to\n%s", ran, output)
	}
}

// Every card entry point clears the record that says which step is
// running. Only the one-process mode did, so in the cards the record
// outlived the card and the ticket page pulsed "いま動いています" beside a
// finished run until the two-hour bound expired (review of #200).
func TestEveryCardEntryPointClearsTheRunningStep(t *testing.T) {
	// Found rather than named: a fourth entry point added later would not
	// be in a list of three, and would keep the record alive again.
	entries, err := os.ReadDir(filepath.Join("..", "runner"))
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		body, err := os.ReadFile(filepath.Join("..", "runner", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		// A file that builds a Pipeline and runs something on it is an
		// entry point; main.go's one-process mode clears it already.
		source := string(body)
		if !strings.Contains(source, "&runner.Pipeline{") || !strings.Contains(source, "pipeline.Run") {
			continue
		}
		checked++
		if !strings.Contains(source, "ClearCurrentStep(workspace)") {
			t.Errorf("cmd/runner/%s runs a card and never clears the running step", entry.Name())
		}
	}
	if checked < 4 {
		t.Errorf("only %d entry points were found; this check is looking in the wrong place", checked)
	}
}
