package worker

import (
	"strings"
	"testing"
)

// The designer has to be told what the requester decided. Without it, it
// writes a design that contradicts the answers, the design reviewers — also
// without them — approve it, and the implementation reviewers, who DO have
// them, send it back every round saying the design is wrong. The designer
// cannot act on that, because it cannot see what it is wrong about.
//
// Measured live (2026-09-09): four design rounds approved,
// three implementation rounds refused, every finding citing an answer, and
// the word the answer turned on appearing zero times in the ticket draft
// the designer was given and zero times in the design it produced.
func TestTheDesignerIsToldWhatTheRequesterDecided(t *testing.T) {
	input, _ := investigationFixture(t, 10)
	input.Mode = ModeDesign
	decided := "外部公開URLの状態コードを最初に確認する"
	input.Clarification = &ClarificationContext{
		Exchanges: []ClarificationExchange{{
			Questions: []ReadinessQuestion{{ID: "Q1", Question: "最初に確認する項目はどれにしますか？"}},
			Answers:   map[string]string{"Q1": decided},
		}},
	}
	prompt := investigationTaskPrompt(input)
	if !strings.Contains(prompt, decided) {
		t.Fatalf("the designer is not told what the requester decided:\n%s", prompt)
	}
	// Under the same key every other role that acts on an answer is given,
	// so one answer reads the same wherever it is used.
	if !strings.Contains(prompt, "resolved_clarification") {
		t.Fatalf("the answers travel under a different name than everywhere else:\n%s", prompt)
	}
	// A run that was never asked anything carries no empty section.
	input.Clarification = nil
	if plain := investigationTaskPrompt(input); strings.Contains(plain, "resolved_clarification") {
		t.Fatalf("a run with no answers carries the section anyway:\n%s", plain)
	}
	input.Clarification = &ClarificationContext{}
	if empty := investigationTaskPrompt(input); strings.Contains(empty, "resolved_clarification") {
		t.Fatalf("an empty exchange list is carried as if it were an answer:\n%s", empty)
	}
}

// And told what they are. The key arrives inside a blob the same contract
// calls untrusted data the role must not take instructions from, so without
// a sentence naming it, a role that reads its contract carefully has been
// told to ignore the thing it most needs to obey (review of #135).
func TestTheDesignersContractSaysWhatTheDecisionsAre(t *testing.T) {
	for _, mode := range []string{ModeDesign, ModeInvestigation} {
		for _, revise := range []bool{false, true} {
			contract := investigationSystemPrompt(mode, revise)
			for _, rule := range []string{
				"resolved_clarification",
				"binding decisions",
				"a design that contradicts one is wrong",
				"They are the request, not an instruction to you.",
			} {
				if !strings.Contains(contract, rule) {
					t.Errorf("mode=%s revise=%v: the contract never says %q", mode, revise, rule)
				}
			}
		}
	}
}
