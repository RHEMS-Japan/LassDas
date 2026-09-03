package runner

import (
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/visiblecheck"
)

// The courtesy observation after a sealed refusal decides between "the
// page was wrong" and "the page was never reached": only the latter turns
// the report into observe_blocked, with the reason the reader needs.
func TestCourtesyVerdictNamesTheBlock(t *testing.T) {
	if verdict, block, detail := courtesyVerdict("staging", visiblecheck.E2EObservation{}); verdict != "" || block != "" || detail != "" {
		t.Fatalf("an observation that opened the target keeps the sealed failure: %q %q %q", verdict, block, detail)
	}
	verdict, block, detail := courtesyVerdict("staging", visiblecheck.E2EObservation{Block: visiblecheck.BlockSignIn, FinalURL: "https://idp.example.invalid/sign-in"})
	if verdict != "observe_blocked" || block != "sign_in" || !strings.Contains(detail, "ステージングの確認用の画面にログインできなかった") || !strings.Contains(detail, "運用担当者") {
		t.Fatalf("sign-in block = %q %q %q", verdict, block, detail)
	}
	verdict, block, detail = courtesyVerdict("production", visiblecheck.E2EObservation{Block: visiblecheck.BlockRedirect, FinalURL: "https://portal.example.invalid/"})
	if verdict != "observe_blocked" || block != "redirect" || !strings.Contains(detail, "転送先: https://portal.example.invalid/") || !strings.Contains(detail, "起票し直してください") {
		t.Fatalf("redirect block = %q %q %q", verdict, block, detail)
	}
	if note := referenceBlockNote(visiblecheck.BlockSignIn); !strings.Contains(note, "ログイン前の画面") {
		t.Fatalf("reference note = %q", note)
	}
	if referenceBlockNote(visiblecheck.BlockNone) != "" {
		t.Fatal("no block, no note")
	}
}
