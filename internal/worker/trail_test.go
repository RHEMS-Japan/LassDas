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
	decision, err := DecideStage(candidate, reviews, source, request, config)
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
		"実装とレビューの経過 (1 周で収束せず)",
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
