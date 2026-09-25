package hook

import (
	"strings"
	"testing"
)

// outcomeReportSample is a finished delivery whose run composed both of the
// sections the closing comment now leads with.
func outcomeReportSample() TerminalReportRequest {
	report := terminalTestRequest(TerminalSuccess)
	report.ReachedDelivery = DeliverProduction
	report.OutcomeText = "## この依頼でできるようになったこと\n" +
		"注文履歴の画面から、過去の注文を月ごとに絞り込めるようになりました。\n\n" +
		"## どこで見られるか\n" +
		"production確認先: " + report.ProductionEvidenceURL + "\n" +
		"本番の画面で「月で絞り込む」が表示されているのを確認しました。"
	report.AssumptionsText = "## 確認せずに本体が決めたこと\n" +
		"- 絞り込みの初期値は今月としました（根拠: 既存の一覧が今月から始まるため）"
	report.SpendText = "この実行の費用は 1.23 USD でした。"
	report.TrailText = "### 実装とレビューの経過 (1 周で合意)\n- 1 周目: 合意\n"
	return report
}

// The person who filed the ticket came for the result, so the comment opens
// with it: what they can now do, then where to look at it. The pull request
// and the run record answer a different question and come after.
func TestTheClosingCommentOpensWithTheOutcomeAndWhereToSeeIt(t *testing.T) {
	report := outcomeReportSample()
	comment := TerminalCommentContent(report, strings.Repeat("a", 64))

	outcome := strings.Index(comment, "この依頼でできるようになったこと")
	where := strings.Index(comment, "どこで見られるか")
	production := strings.Index(comment, report.ProductionEvidenceURL)
	decided := strings.Index(comment, "確認せずに本体が決めたこと")
	cost := strings.Index(comment, "この依頼にかかった費用")
	pull := strings.Index(comment, "Pull Request: ")
	record := strings.Index(comment, "証跡 (自動処理の実行記録)")
	for name, index := range map[string]int{
		"the outcome": outcome, "where to see it": where, "the confirmed screen": production,
		"what was decided": decided, "the cost": cost, "the pull request": pull, "the record": record,
	} {
		if index < 0 {
			t.Fatalf("%s is missing from the comment:\n%s", name, comment)
		}
	}
	// The order is the requester's, top to bottom.
	for _, step := range []struct {
		name          string
		before, after int
	}{
		{"the outcome above where to see it", outcome, where},
		{"where to see it above what was decided", where, decided},
		{"what was decided above the cost", decided, cost},
		{"the cost above the pull request", cost, pull},
		{"the pull request above the record", pull, record},
	} {
		if step.before >= step.after {
			t.Errorf("%s: %d is not above %d\n%s", step.name, step.before, step.after, comment)
		}
	}

	// The screen the delivery was confirmed on is named once, inside the
	// outcome, not repeated among the links below it.
	if count := strings.Count(comment, report.ProductionEvidenceURL); count != 1 {
		t.Errorf("the confirmed screen appears %d times, want once:\n%s", count, comment)
	}
	if strings.Contains(comment, "\nproduction確認先: "+report.ProductionEvidenceURL+"\nstaging確認先") {
		t.Errorf("the links below still carry the confirmation screens:\n%s", comment)
	}
}

// A report from an engine that composed no outcome reads exactly as it did
// before: the places to look stay among the links, because nothing else on
// the comment says where the delivery landed.
func TestAReportWithoutAnOutcomeStillSaysWhereItLanded(t *testing.T) {
	report := outcomeReportSample()
	report.OutcomeText, report.AssumptionsText = "", ""
	comment := TerminalCommentContent(report, strings.Repeat("a", 64))
	for _, want := range []string{
		"staging確認先: " + report.StagingEvidenceURL,
		"production確認先: " + report.ProductionEvidenceURL,
	} {
		if !strings.Contains(comment, want) {
			t.Errorf("an older report lost %q:\n%s", want, comment)
		}
	}
}

// Two parts of this comment can be long. They give way in the order they are
// worth least to the reader — the run record first, then what was decided —
// and the outcome, the place to look, the cost and the footer never do.
func TestTheRecordGivesWayBeforeAnythingTheRequesterCameFor(t *testing.T) {
	report := outcomeReportSample()
	report.TrailText = strings.Repeat("記録の行\n", MaxTrackerCommentBytes/10)
	comment := TerminalCommentContent(report, strings.Repeat("a", 64))
	if len(comment) > MaxTrackerCommentBytes {
		t.Fatalf("the comment is %d bytes, over the tracker's %d", len(comment), MaxTrackerCommentBytes)
	}
	for name, want := range map[string]string{
		"the outcome":       "注文履歴の画面から、過去の注文を月ごとに絞り込めるようになりました。",
		"where to see it":   report.ProductionEvidenceURL,
		"what was decided":  "絞り込みの初期値は今月としました",
		"the cost":          report.SpendText,
		"the footer marker": CommentMarker("terminal", report.AutomationRunID, string(report.Code), strings.Repeat("a", 64)),
	} {
		if !strings.Contains(comment, want) {
			t.Errorf("%s was dropped to make room:\n%s", name, comment)
		}
	}
	if !strings.Contains(comment, TrailShortenedNote) && !strings.Contains(comment, terminalTrailElsewhere) {
		t.Errorf("the record was cut without the comment saying so:\n%s", comment)
	}

	// A delivery that decided so much that the list alone will not fit: the
	// list gives way next, and says where the whole of it is, while the
	// outcome and the cost are still there.
	crowded := outcomeReportSample()
	crowded.TrailText = ""
	crowded.AssumptionsText = strings.Repeat("- 決めたこと\n", MaxTrackerCommentBytes/8)
	crowded.SpendText = "費用の行 1.23 USD"
	crowded.OutcomeText = "成果の行: 一覧が出ます"
	body := TerminalCommentContent(crowded, strings.Repeat("a", 64))
	if len(body) > MaxTrackerCommentBytes {
		t.Fatalf("the comment is %d bytes, over the tracker's %d", len(body), MaxTrackerCommentBytes)
	}
	if !strings.Contains(body, terminalAssumptionsElsewhere) {
		t.Errorf("what was decided was dropped without the comment saying where it is:\n%s", body)
	}
	if !strings.Contains(body, crowded.OutcomeText) || !strings.Contains(body, crowded.SpendText) {
		t.Errorf("the outcome or the cost gave way before the list did:\n%s", body)
	}
}

// Both sections are prose the run composed, so they are held to the record's
// own discipline: bounded, valid text, no carriage returns.
func TestTheComposedSectionsAreHeldToTheReportsPlainTextDiscipline(t *testing.T) {
	config := terminalTestConfig()
	cases := map[string]struct {
		mutate  func(*TerminalReportRequest)
		wantErr bool
	}{
		"an ordinary pair": {func(r *TerminalReportRequest) {
			r.OutcomeText, r.AssumptionsText = "## できたこと\n一覧が出ます", "## 決めたこと\n- 既定は今月"
		}, false},
		"neither, as every report before this": {func(r *TerminalReportRequest) {}, false},
		"an outcome over its bound": {func(r *TerminalReportRequest) {
			r.OutcomeText = strings.Repeat("x", MaxOutcomeTextBytes+1)
		}, true},
		"a list over its bound": {func(r *TerminalReportRequest) {
			r.AssumptionsText = strings.Repeat("x", MaxAssumptionsTextBytes+1)
		}, true},
		"an outcome carrying a carriage return": {func(r *TerminalReportRequest) {
			r.OutcomeText = "できたこと\r\n一覧"
		}, true},
		"a list carrying a NUL": {func(r *TerminalReportRequest) {
			r.AssumptionsText = "決めたこと\x00"
		}, true},
	}
	for name, c := range cases {
		request := terminalTestRequest(TerminalSuccess)
		c.mutate(&request)
		err := request.ValidateRoute(config)
		if c.wantErr && err == nil {
			t.Errorf("%s: accepted", name)
		}
		if !c.wantErr && err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
}

// Neither section is part of the sealed record, so a report begun by an
// earlier engine and re-sent by this one is re-sent under the digest it was
// begun with.
func TestTheComposedSectionsDoNotMoveTheReportDigest(t *testing.T) {
	plain := terminalTestRequest(TerminalSuccess)
	before, err := MarshalTerminalReportRecord(plain)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	composed := plain
	composed.OutcomeText = "## この依頼でできるようになったこと\n一覧が出ます"
	composed.AssumptionsText = "## 確認せずに本体が決めたこと\n- 既定は今月"
	after, err := MarshalTerminalReportRecord(composed)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if TerminalReportDigest(before) != TerminalReportDigest(after) {
		t.Errorf("the digest moved when the report composed its outcome")
	}
}
