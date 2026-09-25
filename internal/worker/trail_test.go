package worker

import (
	"strings"
	"testing"
)

// The trail is the requester's durable record, so the render must carry the
// round outcomes, the reviewers' objections, the changed files, the adopted
// decisions and the validation verdict — in the requester's language.
func TestComposeTrailRendersTheRunRecord(t *testing.T) {
	config, request, source, candidate, reviews := nonconvergedFixture(t)
	decision, err := DecideStage(candidate, reviews, source, request, config, nil)
	if err != nil {
		t.Fatal(err)
	}
	stages := []trailStage{{
		Stage: 1, Candidate: candidate, Reviews: reviews, Decision: decision,
		Source: source, Request: request,
	}}
	clarification := &ClarificationContext{
		SHA256: strings.Repeat("ab", 32), Revision: 2,
		DeliveryID: request.DeliveryID, InputSHA256: request.InputSHA256,
		Exchanges: []ClarificationExchange{{
			Questions: []ReadinessQuestion{{
				ID: "Q1", Dimension: "user_visible_behavior",
				Question: "どちらを優先しますか。",
				Choices: []ReadinessChoice{
					{ID: "a", Label: "抑制を優先", Effect: "取りこぼしうる"},
					{ID: "b", Label: "通知を優先", Effect: "鳴りうる"},
				},
			}},
			Answers: map[string]string{"Q1": "a"},
		}},
	}

	trail := ComposeTrail(stages, clarification, true)
	for _, expected := range []string{
		// A round the seats did not agree on is a round to do again. It
		// used to read "did not converge" here, because the round budget
		// rewrote a final-round objection into the ending the delivery
		// stopped on; nothing rewrites it now.
		"実装とレビューの経過 (1 周でやり直し)",
		"指摘 1 件",
		"missed-escalation",
		request.TargetFiles[0],
		"どちらを優先しますか。",
		"a: 抑制を優先",
		"ビルドとテストを通過",
	} {
		if !strings.Contains(trail, expected) {
			t.Fatalf("trail lacks %q:\n%s", expected, trail)
		}
	}
	if len(trail) > MaxTrailBytes {
		t.Fatalf("trail exceeds the bound: %d bytes", len(trail))
	}

	unvalidated := ComposeTrail(stages, nil, false)
	if !strings.Contains(unvalidated, "未実施または未通過") {
		t.Fatalf("trail claims validation it did not have:\n%s", unvalidated)
	}
}

func TestTrailSummaryUsesTheLatestValidatedCycle(t *testing.T) {
	config, request, source, candidate, reviews := nonconvergedFixture(t)
	decision, err := DecideStage(candidate, reviews, source, request, config, nil)
	if err != nil {
		t.Fatal(err)
	}
	summary := summarizeTrailStages([]trailStage{{
		Stage: 2, Candidate: candidate, Reviews: reviews, Decision: decision,
		Source: source, Request: request,
	}})
	if len(summary.Cycles) != 1 || summary.Cycles[0].Number != 2 || summary.Decision != decision.Outcome || summary.Cycles[0].Findings != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	if len(summary.Cycles[0].Reviews) != len(reviews) || summary.Cycles[0].Reviews[0].Verdict != reviews[0].Verdict {
		t.Fatalf("reviews = %v", summary.Cycles[0].Reviews)
	}
}

// The implementer's final report is its own account of what it did, and a
// requester's acceptance criterion is routinely answered inside it, so the
// trail carries all of it. A live run (2026-09-25) was asked for a
// configuration example and a verification command in the pull request
// description, wrote both in its report, and the clip at seven hundred runes
// dropped both before the requester ever saw them.
func TestComposeTrailCarriesTheWholeImplementerReport(t *testing.T) {
	config, request, source, candidate, reviews := nonconvergedFixture(t)
	decision, err := DecideStage(candidate, reviews, source, request, config, nil)
	if err != nil {
		t.Fatal(err)
	}
	report := "有効化の設定例:\n" + strings.Repeat("設定の行を書く。\n", 120) + "確認手順: 反映後に一覧画面を開いて件数を数える。"
	if len([]rune(report)) < 900 || len(report) > 4096 {
		t.Fatalf("fixture report is %d runes / %d bytes; it must be longer than the old clip and inside the candidate bound", len([]rune(report)), len(report))
	}
	candidate.Rationale = report
	trail := ComposeTrail([]trailStage{{
		Stage: 1, Candidate: candidate, Reviews: reviews, Decision: decision,
		Source: source, Request: request,
	}}, nil, true)
	if !strings.Contains(trail, "実装者の説明 (要点): ") {
		t.Fatalf("trail lacks the report's heading:\n%s", trail)
	}
	if !strings.Contains(trail, report) {
		t.Fatalf("trail carries only part of the report:\n%s", trail)
	}
	if strings.Contains(trail, "確認手順: 反映後に一覧画面を開いて件数を数える。…") {
		t.Fatalf("the report was clipped:\n%s", trail)
	}
	if len(trail) > MaxTrailBytes {
		t.Fatalf("trail exceeds the bound: %d bytes", len(trail))
	}
}
