package worker

import (
	"strings"
	"testing"
)

// The prose caps are byte budgets, and Japanese runs three bytes a character.
// The first live Japanese ticket produced a well-formed assessment whose
// 306-character Japanese assumption breached the old 500-byte cap, so a
// correct model answer died as "assessment failed" (measured 2026-08-07 on a
// live request). The caps must hold the same sentence length in Japanese
// that they held in English.
func TestReadinessOutputValidationHoldsJapaneseProse(t *testing.T) {
	statement := strings.Repeat("抑制状態の判定は既に実装済みで、", 20) // ~300 字 ≈ 900 バイト
	if len(statement) < 800 {
		t.Fatalf("fixture is not exercising the byte budget: %d", len(statement))
	}
	output := ModelReadinessOutput{
		Decision: "reject", RejectCode: "out-of-scope",
		Assumptions: []ReadinessAssumption{{
			Kind: "repository_convention", Statement: statement, Evidence: statement,
		}},
	}
	if err := validateModelReadinessOutput(output); err != nil {
		t.Fatalf("a Japanese assumption of ordinary length must validate: %v", err)
	}
}

// The readiness gate judges the writable scope, not the provisional file
// anchor. The first two live verdicts rejected legitimate tickets because the
// fix lived in other files inside the same scope (measured 2026-08-07 on two
// live tickets); this pins the reframing that ended that.
func TestReadinessPromptCarriesTheWholeWritableScope(t *testing.T) {
	config, request, source := validArtifactFixture(t)
	prompt, err := readinessPrompt(source, request, config, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, `"writable_scope":["client/src/"]`) {
		t.Fatalf("the prompt does not carry the writable scope: %s", prompt[:200])
	}
	system := readinessSystemPrompt()
	for _, must := range []string{
		"writable_scope",
		"preliminary reading anchor",
		"never reject a ticket merely because the provided files alone could not satisfy it",
	} {
		if !strings.Contains(system, must) {
			t.Fatalf("the assessor instruction lost %q", must)
		}
	}
}
