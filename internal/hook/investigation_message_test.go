package hook

import (
	"strings"
	"testing"
)

func TestInvestigationCommentContentShowsStandingAndAttachments(t *testing.T) {
	content := InvestigationCommentContent("run-1", InvestigationFacts{
		Round:     2,
		Questions: []string{"Where is the label?"},
		Findings: []InvestigationFindingFact{
			{Claim: "The label lives in the template", Measured: true, Evidence: []string{"m-0002"}},
			{Claim: "Nothing else references it", Measured: false},
		},
		Unknowns: []string{"Whether the page caches it"}, Next: "Replace the label.",
		MeasurementsCount: 3, AttachmentsOmitted: 1, EndsHere: true,
	})
	for _, want := range []string{"【調査報告】", "調査のみのため、ここで完了", "2 巡目", "Where is the label?", "実測 m-0002", "（推測）", "Whether the page caches it", "次の一手: Replace the label.", "実測は 3 件", "1 件は省略", CommentMarker("investigation", "run-1")} {
		if !strings.Contains(content, want) {
			t.Errorf("comment lacks %q:\n%s", want, content)
		}
	}
	if strings.Contains(content, "kubectl") || strings.Contains(content, "measurements.jsonl と") == false {
		t.Errorf("comment wording: %s", content)
	}
	continuing := InvestigationCommentContent("run-1", InvestigationFacts{Questions: []string{"q"}, Next: "n", MeasurementsCount: 1})
	if !strings.Contains(continuing, "設計書を作り") || strings.Contains(continuing, "ここで完了") {
		t.Errorf("continuing report wording: %s", continuing)
	}
	long := InvestigationCommentContent("run-1", InvestigationFacts{Questions: []string{strings.Repeat("あ", 5000)}, Next: strings.Repeat("い", 5000), MeasurementsCount: 1})
	if len(long) > investigationBodyMaxBytes+2048 {
		t.Errorf("comment not capped: %d bytes", len(long))
	}
}

func TestDesignCommentContentSummarisesTheDesign(t *testing.T) {
	content := DesignCommentContent("run-1", DesignFacts{Round: 1, Cause: "The label is hard-coded", Approach: "Replace the label",
		Files: []string{"web/page.tmpl"}, Verification: "画面 /page に「New」が表示される", BlastRadius: []string{"the page header"}, NotDoing: []string{"renaming the route"}})
	for _, want := range []string{"【設計】", "原因: The label is hard-coded", "直し方: Replace the label", "web/page.tmpl", "これ以外は触りません", "反映後の確認: 画面 /page", "the page header", "renaming the route", "「停止」", CommentMarker("design", "run-1")} {
		if !strings.Contains(content, want) {
			t.Errorf("comment lacks %q:\n%s", want, content)
		}
	}
}

func TestInvestigatedIsATerminalCode(t *testing.T) {
	for _, code := range []TerminalCode{TerminalInvestigated, TerminalInvestigationIncomplete, TerminalInvestigationNonconverged, TerminalDesignNonconverged} {
		if !code.valid() {
			t.Errorf("%s is not a valid terminal code", code)
		}
		comment := fixedTerminalComment(TerminalReportRequest{Code: code, AutomationRunID: "run-1"}, strings.Repeat("0", 64))
		if !strings.Contains(comment, "変更していません") && !strings.Contains(comment, "変更せず停止しました") {
			t.Errorf("%s has no requester-facing text: %s", code, comment)
		}
	}
	if terminalBoardPhase(TerminalInvestigated) != BoardDelivered || terminalBoardPhase(TerminalDesignNonconverged) != BoardNeedsAttention {
		t.Error("board phases for the new codes are wrong")
	}
}
