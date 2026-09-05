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
		MeasurementsCount: 3, AttachedCount: 3, AttachmentsOmitted: 1, EndsHere: true,
	})
	for _, want := range []string{"【調査報告】", "調査のみのため、ここで完了", "2 巡目", "Where is the label?", "実測 m-0002", "（推測）", "Whether the page caches it", "次の一手: Replace the label.", "実測は 3 件", "1 件は省略", CommentMarker("investigation", "run-1")} {
		if !strings.Contains(content, want) {
			t.Errorf("comment lacks %q:\n%s", want, content)
		}
	}
	if strings.Contains(content, "kubectl") || !strings.Contains(content, "添付ファイル 3 件で確認できます") || !strings.Contains(content, "measurements-index.jsonl") {
		t.Errorf("comment wording: %s", content)
	}
	none := InvestigationCommentContent("run-1", InvestigationFacts{Questions: []string{"q"}, Next: "n", MeasurementsCount: 2})
	if strings.Contains(none, "添付ファイル") || !strings.Contains(none, "添付できなかった") {
		t.Errorf("zero attachments claimed as attached: %s", none)
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

func TestPlanHeadlineFollowsTheRequestKind(t *testing.T) {
	investigation := PlanCommentContent("run-1", PlanFacts{Request: "r", RequestKind: "investigation", TargetFiles: []string{"web/a"}})
	if !strings.Contains(investigation, "【調査方針】") || strings.Contains(investigation, "実装を開始します") {
		t.Errorf("investigation headline: %s", investigation)
	}
	if strings.Contains(investigation, "触る予定の範囲") || strings.Contains(investigation, "Pull Request をマージしない") || !strings.Contains(investigation, "調査を止めたい場合") {
		t.Errorf("investigation notice talks about implementation: %s", investigation)
	}
	design := PlanCommentContent("run-1", PlanFacts{Request: "r", RequestKind: "change", NeedsDesign: true, DesignReason: "default"})
	if !strings.Contains(design, "設計書にまとめ") || strings.Contains(design, "次の方針で実装を開始します") {
		t.Errorf("design headline: %s", design)
	}
	plain := PlanCommentContent("run-1", PlanFacts{Request: "r", RequestKind: "change"})
	if !strings.Contains(plain, "次の方針で実装を開始します") {
		t.Errorf("plain headline: %s", plain)
	}
}

func TestDeliverCommentsCarryTheMeasurement(t *testing.T) {
	line := &MeasurementLine{Probe: "http.timing", Metric: "time_total", Threshold: 3, Value: 0.412, Pass: true}
	staging := DeliverStagingContent("run-1", DeliverStagingReport{Verdict: "pass", ScreenChecked: true, GoDeadlineDays: 3, Measurement: line})
	if !strings.Contains(staging, "反映後の計測: http.timing の time_total = 0.412（閾値 3 以下）→ 合格") {
		t.Errorf("staging pass lacks the measurement line: %s", staging)
	}
	failed := DeliverStagingContent("run-1", DeliverStagingReport{Verdict: "measure_failed", Measurement: &MeasurementLine{Probe: "http.timing", Metric: "time_total", Threshold: 3, Value: 4.2, Detail: "time_total = 4.2 が閾値 3 を超えています"}})
	for _, want := range []string{"設計書が約束した計測が閾値を満たしませんでした", "→ 不合格", "計測が不合格"} {
		if !strings.Contains(failed, want) {
			t.Errorf("staging measure_failed lacks %q: %s", want, failed)
		}
	}
	if strings.Contains(failed, "工程が結果を残さず終了") {
		t.Error("measure_failed fell into the default headline")
	}
	release := DeliverReleaseContent("run-1", DeliverReleaseReport{Verdict: "measure_failed", Measurement: &MeasurementLine{Probe: "http.timing", Metric: "time_total", Threshold: 3, Value: 5}})
	if !strings.Contains(release, "計測は閾値超過") || strings.Contains(release, "工程が結果を残さず終了") {
		t.Errorf("release measure_failed: %s", release)
	}
}

func TestDesignCommentReusesThePlanStopSentence(t *testing.T) {
	content := DesignCommentContent("run-1", DesignFacts{Round: 2, Cause: "c", Approach: "a", Files: []string{"web/x"}, Verification: "v", BlastRadius: []string{"b"}})
	if !strings.Contains(content, planStopSentence) || strings.Contains(content, "カードが始まる前までに") {
		t.Errorf("design comment promises its own stop gate: %s", content)
	}
	if !strings.Contains(content, "設計 2 巡目") {
		t.Errorf("design comment does not name its round: %s", content)
	}
}
