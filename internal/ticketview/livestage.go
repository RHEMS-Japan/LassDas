package ticketview

import "strings"

// LiveStage names the board stage a step's live output belongs to, so a
// reader hovering one stage of the rail sees that stage's work and not
// another's.
//
// The argument is the live file's own name — runner.LiveLogName of the step,
// which is the step's name with everything outside [A-Za-z0-9._-] replaced
// ("git checkout" is the file "git-checkout"). Names are matched whole. The
// first table was written from guessed names matched by prefix, and guessing
// cost both ways: the design stage's own reviewer ("agent-design-review")
// matched nothing and showed a requester an empty pane while its output sat
// in the run directory, and "decide-readiness" matched the review stage's
// "decide" and filed the reception's work under 審査. Whole names cannot
// shadow one another, and TestEveryRunnerStepHasAStage measures the table
// against the runner's actual call sites so a new step cannot be added
// without one (live 2026-09-17).
func LiveStage(step string) string {
	if stage, found := liveStages[step]; found {
		return stage
	}
	for _, rule := range liveStagePrefixes {
		if strings.HasPrefix(step, rule.prefix) {
			return rule.stage
		}
	}
	return ""
}

// liveStages is every step the runner names, by the stage a reader would
// look for it under.
var liveStages = map[string]string{
	// 受付 — reading the ticket, deriving the contract, binding the source.
	"read-ticket":      "intake",
	"read-contract":    "intake",
	"build-draft":      "intake",
	"derive-contract":  "intake",
	"list-candidates":  "intake",
	"locate-target":    "intake",
	"baseline":         "intake",
	"snapshot":         "intake",
	"assess-readiness": "intake",
	"check-readiness":  "intake",
	"decide-readiness": "intake",
	"impasse-question": "intake",

	// 調査 — the investigating designer's measurements.
	"investigate": "investigate",

	// 設計 — the plan, its judges, and the question a plan nobody passed asks.
	"agent-design-review":     "design",
	"decide-design":           "design",
	"design-impasse-question": "design",

	// 実装 — the instruction and the agent that carries it out.
	"implement":             "implement",
	"implement-instruction": "implement",
	"run-instruction":       "implement",
	"apply":                 "implement",
	"seal-candidate":        "implement",

	// 審査 — the reviewers and the verdict.
	"agent-review": "review",
	"review":       "review",
	"decide":       "review",

	// 検査 — the project's own checks against what was written.
	"run-validation": "checks",
	"verify-applied": "checks",

	// STG — publishing the branch and waiting for the deployment.
	"create-feature-pr":    "staging",
	"publish-feature":      "staging",
	"merge-feature":        "staging",
	"wait-feature":         "staging",
	"await-staging":        "staging",
	"await-merged-staging": "staging",
	"read-merged":          "staging",
	"verify-publish-gate":  "staging",
	"compose-trail":        "staging",

	// 本番 — the promotion and its deployment.
	"create-promotion-pr": "production",
	"merge-promotion":     "production",
	"promotion-delta":     "production",
	"await-production":    "production",
}

// liveStagePrefixes covers the two steps the runner names for what they act
// on: the git subcommand it runs, and the environment it checks in a
// browser. Longest prefix first, so the production check is not read as a
// staging one.
var liveStagePrefixes = []struct{ prefix, stage string }{
	{"browsercheck-production", "production"},
	{"browsercheck-", "staging"},
	{"git-", "intake"},
}
