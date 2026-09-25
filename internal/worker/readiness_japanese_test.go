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

// The readiness gate judges the writable scope, not whatever files happen to
// be in front of it. The first two live verdicts rejected legitimate tickets
// because the fix lived in other files inside the same scope (measured
// 2026-08-07 on two live tickets); this pins the reframing that ended that.
// An ordinary change request is now shown no file at all, so the same
// rejection would be the default state rather than an edge case.
func TestReadinessPromptCarriesTheWholeWritableScope(t *testing.T) {
	config, request, source := validArtifactFixture(t)
	prompt, err := readinessPrompt(source, request, config, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, `"writable_scope":["client/src/"]`) {
		t.Fatalf("the prompt does not carry the writable scope: %s", prompt[:200])
	}
	system := readinessSystemPrompt()
	for _, must := range []string{
		"writable_scope",
		// An ordinary change request is shown no file, and the contract says
		// so rather than describing a set the assessor never received.
		"USER_DATA_JSON.source.files is empty for an ordinary change request",
		// That emptiness is not a boundary, and not a reason to refuse.
		"it is not the implementation boundary",
		"Judge readiness against that whole scope",
		"never reject a ticket because no file is shown to you",
		// What the repository answers is not the requester's to answer, so
		// an empty file set must not turn every detail into a question.
		"anything that can be found there is not a requester's decision and is not a question",
	} {
		if !strings.Contains(system, must) {
			t.Fatalf("the assessor instruction lost %q", must)
		}
	}
}
