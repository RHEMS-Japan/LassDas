package state

import (
	"testing"

	"automation.internal/ticket-ingress/internal/hook"
)

// Every kind hook.StoreRunCommentKinds names must be one the store accepts,
// or the attendant cannot ask whether that comment was posted and the run
// it belongs to never ends (the investigation report, live 2026-09-05:
// RunCommentState answered invalid_run_comment_state every tick). The list
// itself is checked against the protocol's constants in internal/hook.
func TestEveryStoreRunCommentKindIsAcceptedByTheStore(t *testing.T) {
	for _, kind := range hook.StoreRunCommentKinds() {
		if !validRunCommentKind(kind) {
			t.Errorf("run comment kind %q goes through the store but is refused by it", kind)
		}
	}
	for _, kind := range []hook.RunCommentKind{hook.RunCommentInvestigation, hook.RunCommentDesign} {
		if !validRunCommentKind(kind) {
			t.Errorf("run comment kind %q (the investigating designer's) is refused by the store", kind)
		}
	}
	if validRunCommentKind(hook.RunCommentKind("not-a-kind")) {
		t.Error("an undefined kind was accepted")
	}
}
