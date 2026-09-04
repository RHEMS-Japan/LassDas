package state

import (
	"testing"

	"automation.internal/ticket-ingress/internal/hook"
)

// The plan notice reuses the run-comment machinery; the kind gate must admit
// it and keep refusing unknown kinds.
func TestValidRunCommentKindAdmitsThePlanNotice(t *testing.T) {
	for _, kind := range []hook.RunCommentKind{hook.RunCommentAck, hook.RunCommentReceipt, hook.RunCommentPlan} {
		if !validRunCommentKind(kind) {
			t.Fatalf("kind %q refused", kind)
		}
	}
	if validRunCommentKind(hook.RunCommentKind("bogus")) {
		t.Fatal("unknown kind admitted")
	}
}
