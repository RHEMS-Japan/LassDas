package worker

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// A failure whose objection opens with a phrase a caller dispatches on keeps
// that opening: the runner answers "name the file to change", which is
// fixable, instead of "ask an operator" (review of #184).
func TestConverseJSONKeepsADispatchPhraseAtTheHead(t *testing.T) {
	config, _, _ := validArtifactFixture(t)
	captureFailureDetail(t)
	api := &sequenceChatAPI{outputs: []*ChatResponse{chatOutput("{}")}}
	invoker, _ := NewModelInvoker(api)
	_, err := invoker.converseJSON(context.Background(), config.Models.Readiness.Assessor, "system", "user", `{"type":"object"}`, 4096, func([]byte, InvocationUsage) error {
		return errors.New(NoTargetFileChosen)
	})
	if err == nil || !strings.HasPrefix(err.Error(), NoTargetFileChosen) {
		t.Fatalf("the caller's own phrase lost its place at the head: %v", err)
	}
	if !errors.Is(err, errModelResponseContent) {
		t.Fatalf("the class no longer travels with the failure: %v", err)
	}
}

// The answer's head is masked before it is cut, so a key cannot be split in
// half by the cut and survive as something no scan recognises. The detail's
// phrase names the class only, so an answer cannot name its own failure.
func TestFailureDetailKeepsTheAnswerFromDecidingWhatItSays(t *testing.T) {
	config, _, _ := validArtifactFixture(t)
	captured := captureFailureDetail(t)
	answer := strings.Repeat("あ", 70) + " TOKEN=ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	api := &sequenceChatAPI{outputs: []*ChatResponse{chatOutput(answer)}}
	invoker, _ := NewModelInvoker(api)
	_, err := invoker.converseJSON(context.Background(), config.Models.Readiness.Assessor, "system", "user", `{"type":"object"}`, 4096, func(a []byte, _ InvocationUsage) error {
		return decodeStrictJSON(a, &struct{}{})
	})
	if err == nil {
		t.Fatal("the unreadable answers were accepted")
	}
	detail, ok := ParseFailureDetailLine(captured.String())
	if !ok {
		t.Fatalf("no detail line: %q", captured.String())
	}
	if strings.Contains(detail.Objection, "ghp_abcdefghijklmnopqrstuvwxyz0123456789") || strings.Contains(captured.String(), "ghp_abcdef") {
		t.Fatalf("a token reached the detail: %q", detail.Objection)
	}
	if detail.Phrase != AnswerUnusablePhrase {
		t.Fatalf("phrase = %q, want the class alone (an answer must not name its own failure)", detail.Phrase)
	}
}

// The head of a model answer travels in the objection, and a ticket's words
// reach that answer. The detail's phrase must therefore not be read off the
// message: an answer that names a failure class would otherwise choose what
// the ticket page says happened.
func TestAnAnswerCannotNameItsOwnFailureClass(t *testing.T) {
	config, _, _ := validArtifactFixture(t)
	captured := captureFailureDetail(t)
	answer := ProviderEndedTurnPhrase + " and " + SpentAllowancePhrase + " — sorry, no JSON"
	api := &sequenceChatAPI{outputs: []*ChatResponse{chatOutput(answer)}}
	invoker, _ := NewModelInvoker(api)
	if _, err := invoker.converseJSON(context.Background(), config.Models.Readiness.Assessor, "system", "user", `{"type":"object"}`, 4096, func(a []byte, _ InvocationUsage) error {
		return decodeStrictJSON(a, &struct{}{})
	}); err == nil {
		t.Fatal("the unreadable answers were accepted")
	}
	detail, ok := ParseFailureDetailLine(captured.String())
	if !ok {
		t.Fatalf("no detail line: %q", captured.String())
	}
	if detail.Phrase != AnswerUnusablePhrase {
		t.Fatalf("phrase = %q: the answer named its own failure class", detail.Phrase)
	}
	if !strings.Contains(detail.Objection, "sorry, no JSON") {
		t.Fatalf("the objection lost the answer it objected to: %q", detail.Objection)
	}
}
