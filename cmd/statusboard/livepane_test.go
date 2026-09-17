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
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if strings.HasPrefix(line, "FAIL") {
			t.Error(line)
		}
	}
	if err != nil && !strings.Contains(string(output), "FAIL") {
		t.Fatalf("the check could not run: %v\n%s", err, output)
	}
}
