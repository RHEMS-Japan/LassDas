package worker

import (
	"strings"
	"testing"
)

// A sealed candidate proves what its bytes are, not that they were allowed.
//
// Every reader of one re-checks it — the reviews, the apply, the publish
// gate — and the workflow content rules are part of what they re-check.
// Without that, a candidate sealed by an older engine, by a card that
// skipped the seal's own check, or by anything that wrote the file
// directly, would carry a workflow nobody measured onto the branch that
// gets pushed.
func TestAReaderOfASealedCandidateChecksTheWorkflowAgain(t *testing.T) {
	config := configWithMeans(t)
	// No wording promise: a release path is machinery rather than a screen,
	// so the ticket that carries one promises nothing visible about the
	// file being built here.
	draft := workflowDraft(t, config)
	draft.VerificationPath, draft.ExpectedText, draft.AbsentText = "", "", ""
	request, err := draft.WithTargetFilesBuilding([]string{plannedWorkflow}, []string{plannedWorkflow}, config)
	if err != nil {
		t.Fatal(err)
	}
	source, err := ReadSourceSnapshot(t.TempDir(), strings.Repeat("a", 40), request, config)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := NewCandidate(1, ModelCandidateOutput{
		Rationale: "Build the deploy workflow this destination has no path without.",
		Files:     []ModelCandidateFile{{Path: plannedWorkflow, Content: passingWorkflow}},
	}, source, request, config, validTestInvocation(config.Models.Implementer), testInvocationTime)
	if err != nil {
		t.Fatalf("a workflow inside the policy could not be sealed: %v", err)
	}
	if err := candidate.Validate(source, request, config); err != nil {
		t.Fatalf("a sealed candidate inside the policy was refused: %v", err)
	}

	// The same candidate, its one file swapped for a workflow the policy
	// refuses and its digest recomputed, is what a reader has to catch.
	// Only the content changed: every binding the reader checks first — the
	// round, the delivery, the source, the digest — still holds.
	forged := candidate
	forged.Files = []CandidateFile{{
		Path:         plannedWorkflow,
		BeforeSHA256: candidate.Files[0].BeforeSHA256,
		Content:      strings.Replace(passingWorkflow, "runs-on: ubuntu-latest", "runs-on: self-hosted", 1),
	}}
	digest, err := candidateDigest(forged)
	if err != nil {
		t.Fatal(err)
	}
	forged.CandidateSHA256 = digest
	err = forged.Validate(source, request, config)
	if err == nil {
		t.Fatal("a reader accepted a sealed candidate whose workflow the policy refuses")
	}
	if !strings.Contains(err.Error(), workflowRuleRunner) {
		t.Fatalf("the refusal does not name the rule: %v", err)
	}
}
