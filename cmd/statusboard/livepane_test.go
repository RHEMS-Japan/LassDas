package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	if ran < 47 {
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

// The page's styling and its script have to agree about the classes they
// share: the rule that keeps a pinned card's own rail readable named a
// descendant relationship that does not exist, so while a pane was pinned
// the one rail a reader had to click stopped looking clickable - and no
// DOM check could see it, because a stylesheet is not a behaviour (review
// of #200).
func TestTheStylesheetAndTheScriptAgreeOnTheirClasses(t *testing.T) {
	body, err := os.ReadFile("board.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(body)
	for _, class := range []string{"livePinned", "pinnedHost"} {
		styled := strings.Contains(page, "."+class+" ")
		set := strings.Contains(page, `classList.toggle("`+class+`"`)
		if styled && !set {
			t.Errorf("the styling uses .%s and nothing in the script ever sets it", class)
		}
		if set && !styled {
			t.Errorf("the script sets %s and nothing in the styling uses it", class)
		}
	}
	// The exception that keeps the pinned card's rail readable has to be
	// written against the card, because the rail and the pane are siblings.
	if !strings.Contains(page, "body.livePinned .card.pinnedHost .node.hasLive .nname") {
		t.Error("the pinned card's own rail has no rule keeping it readable")
	}
	if strings.Contains(page, "body.livePinned .livepane.on .node.hasLive .nname") {
		t.Error("the exception is written as a descendant of the pane, which the rail is not")
	}
}

// Every stage the attendant can place has somewhere to go on the rail.
// The board draws nine, and one the attendant placed was not among them,
// so the whole rail went dark while a pull request was being published
// (review of #200).
var undrawnByDesign = map[string]string{
	"reporting": "報告を書いている間だけ。札の本文が「報告を作成中」と言う",
}

func TestEveryPlacedStageIsDrawnOnTheRail(t *testing.T) {
	status, err := os.ReadFile(filepath.Join("..", "..", "internal", "attendant", "status.go"))
	if err != nil {
		t.Fatal(err)
	}
	page, err := os.ReadFile("board.html")
	if err != nil {
		t.Fatal(err)
	}
	drawn := map[string]bool{}
	for _, match := range regexp.MustCompile(`\["([a-z]+)", "[^"]+"\]`).FindAllStringSubmatch(string(page), -1) {
		drawn[match[1]] = true
	}
	if len(drawn) < 9 {
		t.Fatalf("only %d rail stages were found in the page; this check is looking in the wrong place", len(drawn))
	}
	// Resting and waiting states are not rail positions; the rail shows
	// where a run that is working has got to.
	resting := map[string]bool{"done": true, "failed": true, "stopped": true, "question": true, "attention": true}
	placed := map[string]bool{}
	for _, match := range regexp.MustCompile(`place(?:At)?\("([a-z_]+)"`).FindAllStringSubmatch(string(status), -1) {
		placed[match[1]] = true
	}
	if len(placed) < 9 {
		t.Fatalf("only %d placed stages were found; this check is looking in the wrong place", len(placed))
	}
	for stage := range placed {
		if resting[stage] || drawn[stage] {
			continue
		}
		// One exception, named and argued: while a run writes its report
		// it is no longer working, and the card's own line says so. The
		// rail is dark for those seconds. Marking every stage done instead
		// would have told a requester whose delivery failed in実装 that it
		// had reached 本番 (review of #203).
		if undrawnByDesign[stage] != "" {
			continue
		}
		t.Errorf("the attendant places %q and the rail does not draw it, so the rail goes dark", stage)
	}
}
