package main

import (
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/worker"
)

// The ladder's last hand before anybody waits: the same judge, asked a
// different way. A model that answered nothing is not asked the identical
// question again — that is the loop the ladder replaces — so the earlier
// rounds' objections come out and the change travels as a map of where to
// look rather than as its own patches.
//
// What the judge is being asked to do is untouched: the scope, the lens
// and the shape of the answer are the same words, which is what makes this
// a shorter ask rather than a different job.
func TestARebuiltReviewInstructionIsShorterAndStillTheSameJob(t *testing.T) {
	candidate, source := promptFixtureFiles(2, 60)
	findings := []worker.ModelFinding{{Code: "earlier-objection", Path: "client/src/file-0.ts", Message: strings.Repeat("あ", 400)}}
	endpoint := worker.ModelEndpoint{Lens: "correctness"}
	full, err := reviewAgentPrompt(candidate, source, promptFixtureRequest(), endpoint, nil, findings, "", "/tmp/review repo", false)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := reviewAgentPrompt(candidate, source, promptFixtureRequest(), endpoint, nil, nil, "", "/tmp/review repo", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(rebuilt) >= len(full) {
		t.Fatalf("the rebuilt instruction is %d bytes against the original's %d; it has to be the shorter ask", len(rebuilt), len(full))
	}
	if rebuilt == full {
		t.Fatal("the rebuilt instruction is the instruction that was not answered")
	}
	if strings.Contains(rebuilt, "機械抽出の差分") {
		t.Fatal("the rebuilt instruction still carries the patches")
	}
	if !strings.Contains(rebuilt, "変更位置の一覧") {
		t.Fatal("the rebuilt instruction does not say where to look")
	}
	// The job, unchanged.
	for _, kept := range []string{"## 評決の対象", "## 見る観点", endpoint.Lens, "## 答え方 (最後にこの形の JSON だけを出力する)"} {
		if !strings.Contains(rebuilt, kept) {
			t.Fatalf("the rebuilt instruction dropped %q", kept)
		}
	}
}

// A rebuild this engine does not know is refused rather than ignored: a
// card asking for one that silently did not happen would spend the
// ladder's one different hand on the same instruction as before.
func TestAnUnknownRebuildIsRefused(t *testing.T) {
	if !validRebuild("") || !validRebuild(worker.PromptRebuildShorten) {
		t.Fatal("the rebuilds this engine plays were refused")
	}
	for _, unknown := range []string{"split", "structured-off", "SHORTEN", "shorten "} {
		if validRebuild(unknown) {
			t.Fatalf("validRebuild(%q) accepted a rebuild nothing implements", unknown)
		}
	}
}
