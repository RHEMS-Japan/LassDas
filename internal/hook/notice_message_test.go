package hook

import (
	"strings"
	"testing"
)

// The plan notice tells the requester what the reception decided about the
// design stage, in one line and in their terms: the skip and its reason, or
// the design and why it was kept. A run whose decision predates the stage
// says nothing about it.
func TestPlanCommentContentCarriesTheDesignDecisionLine(t *testing.T) {
	skipped := PlanCommentContent("run-42", PlanFacts{Request: "ラベルの文言を変える", DesignReason: "approach_in_ticket"})
	if !strings.Contains(skipped, "\n設計なし: 方針が本文にあるため設計を省略\n") {
		t.Fatalf("skipped design is not reported:\n%s", skipped)
	}
	kept := PlanCommentContent("run-42", PlanFacts{Request: "ラベルの文言を変える", NeedsDesign: true, DesignReason: "approach_not_in_ticket"})
	if !strings.Contains(kept, "\n設計あり: 本文に「どう直すか」が書かれていないため\n") {
		t.Fatalf("kept design is not reported:\n%s", kept)
	}
	// A reason this package has never heard of still shows the decision, with
	// a neutral sentence - never the machine code itself.
	future := PlanCommentContent("run-42", PlanFacts{NeedsDesign: true, DesignReason: "future_code"})
	if !strings.Contains(future, "\n設計あり: 理由は自動処理の記録に残しています\n") {
		t.Fatalf("unknown reason was dropped:\n%s", future)
	}
	if strings.Contains(future, "future_code") {
		t.Fatalf("a machine code reached the requester:\n%s", future)
	}
	if got := DesignDecisionLine(false, "future_code"); got != "設計なし: 理由は自動処理の記録に残しています" {
		t.Fatalf("DesignDecisionLine(unknown, skipped) = %q", got)
	}
	// No reason, no line: the notice never guesses.
	silent := PlanCommentContent("run-42", PlanFacts{Request: "ラベルの文言を変える"})
	if strings.Contains(silent, "設計あり") || strings.Contains(silent, "設計なし") {
		t.Fatalf("a run without a design decision reported one:\n%s", silent)
	}
	for _, content := range []string{skipped, kept, future, silent} {
		if err := ValidateCommentContract(content, CommentMarker("plan", "run-42")); err != nil {
			t.Fatalf("plan comment violates the contract: %v", err)
		}
	}
}

func TestDesignDecisionLineNamesEveryVerdict(t *testing.T) {
	if got := DesignDecisionLine(false, "investigation"); got != "設計なし: 調査の依頼のため設計は行わない" {
		t.Fatalf("DesignDecisionLine(investigation) = %q", got)
	}
	if got := DesignDecisionLine(true, "checker_disagreed"); !strings.HasPrefix(got, "設計あり: ") || !strings.Contains(got, "確認役") {
		t.Fatalf("DesignDecisionLine(checker_disagreed) = %q", got)
	}
	if _, known := DesignReasonPhrase("not-a-reason"); known {
		t.Fatal("an unknown reason was reported as known")
	}
}
