package ticketview

import "testing"

func TestLiveStageNamesTheStageAStepBelongsTo(t *testing.T) {
	cases := map[string]string{
		"read-contract":           "intake",
		"derive-contract":         "intake",
		"git-checkout":            "intake",
		"assess-readiness":        "intake",
		"implement-instruction":   "implement",
		"run-instruction":         "implement",
		"seal-candidate":          "implement",
		"agent-review":            "review",
		"decide":                  "review",
		"validate":                "checks",
		"investigate-instruction": "investigate",
		"design-review-a":         "design",
		"deliver-staging":         "staging",
		"deliver-production":      "production",
		"browsercheck-production": "production",
		"something-new":           "",
		"":                        "",
	}
	for step, want := range cases {
		if got := LiveStage(step); got != want {
			t.Errorf("LiveStage(%q) = %q, want %q", step, got, want)
		}
	}
	// A prefix match never swallows an unrelated step that merely starts
	// with the same letters.
	if got := LiveStage("designated-survivor"); got != "" {
		t.Errorf("LiveStage(designated-survivor) = %q", got)
	}
}
