package ticketview

import "strings"

// LiveStage names the board stage a step's live output belongs to, so a
// reader hovering one stage of the rail sees that stage's work and not
// another's.
//
// One rule decides every entry: a step belongs to the stage of the card that
// runs it, because that is the stage the rail lights up while the step is
// working, and the lit stage is the one a reader hovers. Written from
// remembered names instead, the table was wrong in both directions — the
// design stage's own reviewer matched nothing while its output sat in the
// run directory; the reception's decision matched the review stage's
// "decide"; and the long wait a reader watches at 確認 was filed under STG,
// a stage the rail already marks done (live 2026-09-17, review of #200).
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

// liveStages is every step the runner names, under the stage of the card
// that runs it. The comment on each group names that card.
var liveStages = map[string]string{
	// 受付 — the reception (pretrip, readinessGate): reading the ticket,
	// deriving the contract, binding the source, judging readiness.
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

	// 調査 — the investigate card.
	"investigate": "investigate",

	// 設計 — the design review and decide cards, and the question a plan
	// nobody passed puts to its requester.
	"agent-design-review":     "design",
	"decide-design":           "design",
	"design-impasse-question": "design",

	// 実装 — the implement and apply cards: the instruction and the agent
	// that carries it out.
	"implement":             "implement",
	"implement-instruction": "implement",
	"run-instruction":       "implement",

	// 審査 — the review cards. The seal runs on the first of them, which is
	// why it is here and not with the implementation it records.
	"agent-review":   "review",
	"review":         "review",
	"seal-candidate": "review",

	// 検査 — the validate card and the checks card: the round's verdict,
	// the candidate applied into a sandbox, the project's own checks, the
	// wait for the branch's CI, and the question asked when the reviews
	// never agreed.
	"decide":              "checks",
	"wait-feature":        "checks",
	"apply":               "checks",
	"run-validation":      "checks",
	"verify-applied":      "checks",
	"verify-publish-gate": "checks",
	"impasse-question":    "checks",

	// STG — the publish card: the branch, the merge, and the wait for the
	// staging deployment.
	//
	// Four of these run while the attendant places "reporting", which the
	// rail does not draw at all, so the rail is dark then and a reader has
	// no lit stage to hover: create-feature-pr, publish-feature,
	// compose-trail and the re-baseline. They sit here because STG is what
	// they are doing. The missing rail stage is older than this table.
	"create-feature-pr": "staging",
	"publish-feature":   "staging",
	"compose-trail":     "staging",
	"merge-feature":     "staging",
	"await-staging":     "staging",
	// read-merged is asked twice, once for the staging branch and once for
	// the promotion. The live file is one per step name and appended to, so
	// both readings land in the staging pane; the promotion's own steps are
	// below.
	"read-merged":     "staging",
	"promotion-delta": "staging",

	// 確認 — the e2e card: the wait for a person to merge and for staging
	// to carry the change. It is the one stage a reader watches for a long
	// time, and its only step.
	"await-merged-staging": "confirm",

	// 本番 — the production delivery.
	"create-promotion-pr": "production",
	"merge-promotion":     "production",
	"await-production":    "production",
}

// liveStagePrefixes covers the two steps the runner names for what they act
// on: the git subcommand it runs, and the environment it checks in a
// browser. Longest prefix first, so the production check is not read as a
// staging one.
var liveStagePrefixes = []struct{ prefix, stage string }{
	{"browsercheck-production", "production"},
	{"browsercheck-", "staging"},
	// The reception clones and checks out, and so does the validate card
	// before it applies a candidate. Both append to one file, offered where
	// the reception's own work is.
	{"git-", "intake"},
}
