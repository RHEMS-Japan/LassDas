package main

import (
	"fmt"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/worker"
)

func promptFixtureFiles(fileCount, lineCount int) (worker.Candidate, worker.SourceSnapshot) {
	var candidate worker.Candidate
	var source worker.SourceSnapshot
	for f := 0; f < fileCount; f++ {
		before := make([]string, 0, lineCount)
		after := make([]string, 0, lineCount)
		for l := 0; l < lineCount; l++ {
			line := fmt.Sprintf("file-%d line-%d original-payload", f, l)
			before = append(before, line)
			if l%2 == 1 {
				line = fmt.Sprintf("file-%d line-%d changed-payload", f, l)
			}
			after = append(after, line)
		}
		path := fmt.Sprintf("client/src/file-%d.ts", f)
		source.Files = append(source.Files, worker.SourceFile{Path: path, Content: strings.Join(before, "\n")})
		candidate.Files = append(candidate.Files, worker.CandidateFile{Path: path, Content: strings.Join(after, "\n")})
	}
	return candidate, source
}

func promptFixtureRequest() worker.TicketRequest {
	return worker.TicketRequest{IssueKey: "TEST-1", Summary: "件名", Request: "本文"}
}

// A small change travels with its full patches, exactly as before.
func TestReviewAgentPromptEmbedsThePatchesWhenTheyFit(t *testing.T) {
	candidate, source := promptFixtureFiles(1, 40)
	prompt, err := reviewAgentPrompt(candidate, source, promptFixtureRequest(),
		worker.ModelEndpoint{Lens: "correctness"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "機械抽出の差分") || !strings.Contains(prompt, "changed-payload") {
		t.Fatal("a small change lost its embedded patch")
	}
}

// When the full patches outgrow the instruction budget, the reviewer still
// gets an instruction: the changed line ranges without their content. The
// first oversized live candidate (15 files, ~550KB of change) died as
// "instruction is too large" with no review at all; this pins the fallback.
func TestReviewAgentPromptFallsBackToOutlinesWhenPatchesOvergrow(t *testing.T) {
	candidate, source := promptFixtureFiles(40, 120)
	prompt, err := reviewAgentPrompt(candidate, source, promptFixtureRequest(),
		worker.ModelEndpoint{Lens: "correctness"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(prompt) > worker.MaxAgentPromptBytes {
		t.Fatalf("the fallback still exceeds the budget: %d bytes", len(prompt))
	}
	if !strings.Contains(prompt, "変更位置の一覧") {
		t.Fatal("the oversized change did not switch to the outline form")
	}
	if !strings.Contains(prompt, "@@ 変更前") {
		t.Fatal("the outline lost the changed line ranges")
	}
	if strings.Contains(prompt, "changed-payload") {
		t.Fatal("the outline still carries patch content")
	}
	if !strings.Contains(prompt, "client/src/file-39.ts") {
		t.Fatal("the outline dropped a changed file")
	}
}
