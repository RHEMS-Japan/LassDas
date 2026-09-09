package hook

import (
	"strings"
	"testing"
)

// model_failed is the most common way a run ends. The sentence has to say
// which step could not be completed, and it has to keep working for a report
// that carries no step — an engine older than the field still reports.
func TestTheModelFailureSentenceNamesTheStepWhenItHasOne(t *testing.T) {
	digest := strings.Repeat("0", 64)
	named := TerminalCommentContent(TerminalReportRequest{
		Code: TerminalModelFailed, AutomationRunID: "run-1", FailedStep: "AI による変更のレビュー",
	}, digest)
	if !strings.Contains(named, "AI による変更のレビューを完了できなかったため") {
		t.Fatalf("the sentence does not name the step:\n%s", named)
	}
	if strings.Contains(named, "成果物の生成またはレビュー") {
		t.Fatalf("the sentence that names no step is still there:\n%s", named)
	}
	older := TerminalCommentContent(TerminalReportRequest{
		Code: TerminalModelFailed, AutomationRunID: "run-1",
	}, digest)
	if !strings.Contains(older, "AIによる成果物の生成またはレビューを完了できなかったため") {
		t.Fatalf("a report with no step lost its sentence:\n%s", older)
	}
	if !strings.Contains(named, "本番環境には反映していません") || !strings.Contains(older, "本番環境には反映していません") {
		t.Fatalf("the claim about production changed:\n%s\n%s", named, older)
	}
}

// The step is requester-facing text arriving from another process, held to
// the same shape rule as the rest of it: one bounded line, no control bytes.
func TestAStepNameThatIsNotOneBoundedLineIsRefused(t *testing.T) {
	base := terminalTestRequest(TerminalModelFailed)
	for _, tc := range []struct {
		name string
		step string
	}{
		{"a newline", "変更の\nレビュー"},
		{"a carriage return", "変更の\rレビュー"},
		{"a null byte", "変更の\x00レビュー"},
		{"past the bound", strings.Repeat("あ", MaxFailedStepBytes)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := base
			report.FailedStep = tc.step
			if err := report.ValidateShape(); err == nil {
				t.Fatalf("a step of %q was accepted", tc.step)
			}
		})
	}
	accepted := base
	accepted.FailedStep = "稼働環境とリポジトリの調査"
	if err := accepted.ValidateShape(); err != nil {
		t.Fatalf("a well-formed step was refused: %v", err)
	}
}
