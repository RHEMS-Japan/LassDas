package state

import (
	"testing"

	"automation.internal/ticket-ingress/internal/hook"
)

// Every one-shot run comment kind the hook protocol defines must be one the
// store accepts, or the attendant cannot ask whether that comment was posted
// and the run it belongs to never ends (the investigation report, live
// 2026-09-05: RunCommentState answered invalid_run_comment_state every tick).
func TestEveryRunCommentKindTheHookDefinesIsAcceptedByTheStore(t *testing.T) {
	kinds := []hook.RunCommentKind{
		hook.RunCommentAck, hook.RunCommentReceipt, hook.RunCommentPlan,
		hook.RunCommentInvestigation, hook.RunCommentDesign,
		hook.RunCommentE2E, hook.RunCommentStagingReport, hook.RunCommentReleaseReport,
	}
	for _, kind := range kinds {
		if !validRunCommentKind(kind) {
			t.Errorf("run comment kind %q is defined by the hook protocol but refused by the store", kind)
		}
	}
	if validRunCommentKind(hook.RunCommentKind("not-a-kind")) {
		t.Error("an undefined kind was accepted")
	}
}
