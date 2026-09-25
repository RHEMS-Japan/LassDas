package hook

import (
	"strings"
	"testing"
	"unicode/utf8"
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
	if len(long) > MaxTrackerCommentBytes {
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
		comment := TerminalCommentContent(TerminalReportRequest{Code: code, AutomationRunID: "run-1"}, strings.Repeat("0", 64))
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

// A round that ended on refused answers must not tell the requester to
// narrow the request (live: a design refused three times ended with the
// budget text and the narrowing advice); it names the refusal instead. A
// round that ran out of budget, and a report without a reason, keep the
// narrowing advice.
func TestIncompleteCommentNamesRefusedAnswers(t *testing.T) {
	digest := strings.Repeat("0", 64)
	refused := TerminalReportRequest{Code: TerminalInvestigationIncomplete, AutomationRunID: "run-1",
		IncompleteReason:    "the model's design kept failing the checks: no design file contains the wording promised to disappear",
		IncompleteObjection: "the design was refused: design file \"docs/page.md\" change 12 is 304 bytes (limit 300)\nsecond line"}
	comment := TerminalCommentContent(refused, digest)
	for _, want := range []string{"自動検査の規則に合わず", "再起票は不要です", "最後に拒否された点 (規則の原文): the design was refused: design file \"docs/page.md\" change 12 is 304 bytes (limit 300) second line", "運用担当者が規則と答えを確認"} {
		if !strings.Contains(comment, want) {
			t.Errorf("refused comment lacks %q:\n%s", want, comment)
		}
	}
	if strings.Contains(comment, "範囲を絞って再度起票") {
		t.Errorf("refused comment still advises narrowing:\n%s", comment)
	}
	if facts := terminalCommentFacts(refused, digest); facts.NextActor != "運用担当者" || !strings.Contains(facts.Operation, "起票者の操作は不要") {
		t.Errorf("refused facts: %+v", facts)
	}
	for name, report := range map[string]TerminalReportRequest{
		"budget":    {Code: TerminalInvestigationIncomplete, AutomationRunID: "run-1", IncompleteReason: "the probe budget is spent and the model asked for another measurement"},
		"wall":      {Code: TerminalInvestigationIncomplete, AutomationRunID: "run-1", IncompleteReason: "the wall ended the round before a record was sealed"},
		"no reason": {Code: TerminalInvestigationIncomplete, AutomationRunID: "run-1"},
	} {
		comment := TerminalCommentContent(report, digest)
		if !strings.Contains(comment, "範囲を絞って再度起票") || strings.Contains(comment, "最後に拒否された点") {
			t.Errorf("%s comment: %s", name, comment)
		}
		if facts := terminalCommentFacts(report, digest); facts.NextActor != "起票者" {
			t.Errorf("%s facts: %+v", name, facts)
		}
	}
	// 600 is a multiple of 3, so a leading ASCII byte is what makes the
	// bound land inside a character: the cut steps back to the boundary.
	long := TerminalReportRequest{Code: TerminalInvestigationIncomplete, AutomationRunID: "run-1", IncompleteReason: "the model's report kept failing the checks: x", IncompleteObjection: "a" + strings.Repeat("あ", 400)}
	if comment := TerminalCommentContent(long, digest); !strings.Contains(comment, "a"+strings.Repeat("あ", 199)+"…") || strings.Contains(comment, strings.Repeat("あ", 200)) || !utf8.ValidString(comment) {
		t.Errorf("long objection not cut on a character boundary at %d bytes", maxIncompleteObjectionBytes)
	}
	// A reason is classified by its fixed prefix, not by words a model may
	// have planted in the objection it ends with.
	planted := TerminalReportRequest{Code: TerminalInvestigationIncomplete, AutomationRunID: "run-1",
		IncompleteReason: "the model's design kept failing the checks: design cause cites the wall ended, which no measured finding of the investigation carries"}
	if comment := TerminalCommentContent(planted, digest); strings.Contains(comment, "範囲を絞って再度起票") {
		t.Errorf("a planted budget phrase changed the classification:\n%s", comment)
	}
}

// investigationFixture is a report with more findings and unknowns than the
// old dozen-item cap admitted, each line longer than the old per-item clip.
func investigationFixture(claimBytes, unknownBytes int) InvestigationFacts {
	facts := InvestigationFacts{
		Round: 1, Questions: []string{"Where does the count come from?"},
		Next: "Change the query.", MeasurementsCount: 20, AttachedCount: 3, EndsHere: true,
	}
	for index := 0; index < 20; index++ {
		claim := "finding-" + string(rune('a'+index)) + " " + strings.Repeat("x", claimBytes)
		facts.Findings = append(facts.Findings, InvestigationFindingFact{
			Claim: claim, Measured: index%2 == 0, Evidence: []string{"m-0001"}})
		facts.Unknowns = append(facts.Unknowns, "unknown-"+string(rune('a'+index))+" "+strings.Repeat("y", unknownBytes))
	}
	return facts
}

// An investigation-only request ends on the ticket: the comment is the
// requester's only copy of what was measured. A live report (2026-09-25)
// carried twenty findings and the ticket showed ten, several of them cut
// mid-sentence, with the rest as "他 8 件" and the whole text left in a run
// directory the requester cannot open.
func TestInvestigationCommentCarriesEveryFinding(t *testing.T) {
	facts := investigationFixture(250, 250)
	content := InvestigationCommentContent("run-1", facts)
	for _, finding := range facts.Findings {
		if !strings.Contains(content, finding.Claim) {
			t.Fatalf("comment lacks a finding whole: %q", finding.Claim[:20])
		}
	}
	for _, unknown := range facts.Unknowns {
		if !strings.Contains(content, unknown) {
			t.Fatalf("comment lacks an unknown whole: %q", unknown[:20])
		}
	}
	if strings.Contains(content, "他 ") || strings.Contains(content, "…（以下略）") || strings.Contains(content, "収まらない") {
		t.Fatalf("a report that fits was shortened anyway:\n%s", content)
	}
	if overflow := InvestigationReportOverflow("run-1", facts); overflow != nil {
		t.Fatalf("a report that fits produced a %d byte attachment", len(overflow))
	}
	// Whether the report fits is asked before the attachments exist and
	// answered again after, with the sentence counting them longer the
	// second time. The two answers must agree, or the comment cuts a report
	// with nothing attached behind it.
	crowded := facts
	crowded.AttachedCount, crowded.AttachmentsOmitted = 10, 20
	if InvestigationReportOverflow("run-1", crowded) != nil {
		t.Fatal("the same report fits before the attachments and not after")
	}
	crowdedContent := InvestigationCommentContent("run-1", crowded)
	if !strings.Contains(crowdedContent, facts.Findings[19].Claim) || strings.Contains(crowdedContent, "収まらない") {
		t.Fatalf("a full attachment list shortened a report that fits:\n%s", crowdedContent)
	}
	if len(content) > MaxTrackerCommentBytes {
		t.Fatalf("comment is %d bytes; the tracker takes %d", len(content), MaxTrackerCommentBytes)
	}
	if ValidateCommentContract(content, CommentMarker("investigation", "run-1")) != nil {
		t.Fatalf("comment lost its contract footer:\n%s", content)
	}
}

// A report at the artifact schema's own limits does not fit one comment.
// What the comment cannot show travels whole as an attachment, and the
// comment says how much is there and where -- never an ellipsis, and never
// a silent drop.
func TestInvestigationReportTooLongForOneCommentIsAttachedWhole(t *testing.T) {
	facts := investigationFixture(600, 300)
	overflow := InvestigationReportOverflow("run-1", facts)
	if overflow == nil {
		t.Fatal("a report past the comment limit produced no attachment")
	}
	for _, finding := range facts.Findings {
		if !strings.Contains(string(overflow), finding.Claim) {
			t.Fatalf("attachment lacks a finding: %q", finding.Claim[:20])
		}
	}
	for _, unknown := range facts.Unknowns {
		if !strings.Contains(string(overflow), unknown) {
			t.Fatalf("attachment lacks an unknown: %q", unknown[:20])
		}
	}
	facts.ReportAttached, facts.AttachedCount = true, 4
	content := InvestigationCommentContent("run-1", facts)
	if len(content) > MaxTrackerCommentBytes {
		t.Fatalf("comment is %d bytes; the tracker takes %d", len(content), MaxTrackerCommentBytes)
	}
	if !strings.Contains(content, InvestigationReportFilename) || !strings.Contains(content, "ここでは省いています") {
		t.Fatalf("comment does not say where the rest of the report is:\n%s", content)
	}
	if !utf8.ValidString(content) {
		t.Fatal("comment is not valid UTF-8")
	}
	if ValidateCommentContract(content, CommentMarker("investigation", "run-1")) != nil {
		t.Fatalf("comment lost its contract footer:\n%s", content)
	}
	// The comment still shows what it could, and every line it shows is whole.
	if !strings.Contains(content, facts.Findings[0].Claim) {
		t.Fatalf("comment shows no finding at all:\n%s", content)
	}

	// An upload that failed leaves the report in the run record, and the
	// comment says that instead of naming a file that is not there.
	facts.ReportAttached = false
	unattached := InvestigationCommentContent("run-1", facts)
	if strings.Contains(unattached, InvestigationReportFilename) || !strings.Contains(unattached, "実行記録から取り出します") {
		t.Fatalf("comment names an attachment that does not exist:\n%s", unattached)
	}
	if len(unattached) > MaxTrackerCommentBytes {
		t.Fatalf("comment is %d bytes; the tracker takes %d", len(unattached), MaxTrackerCommentBytes)
	}
}
