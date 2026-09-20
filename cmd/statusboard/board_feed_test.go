package main

import (
	"os/exec"
	"strings"
	"testing"
)

func TestBoardFeedFreshnessBehaviour(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	output, err := exec.Command(node, "testdata/board_feed_check.mjs", "board.html").CombinedOutput()
	if err != nil {
		t.Fatalf("board feed behaviour: %v\n%s", err, output)
	}
	if strings.Count(string(output), "PASS ") != 10 {
		t.Fatalf("incomplete feed checks: %s", output)
	}
}
