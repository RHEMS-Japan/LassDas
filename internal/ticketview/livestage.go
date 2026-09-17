package ticketview

import "strings"

// LiveStage names the board stage a step's live output belongs to, so a
// reader hovering one stage of the rail sees that stage's work and not
// another's. The step names are the runner's own; a step nobody mapped
// belongs to no stage and is offered under its own name only.
func LiveStage(step string) string {
	for _, rule := range liveStages {
		for _, prefix := range rule.prefixes {
			if step == prefix || strings.HasPrefix(step, prefix+"-") {
				return rule.stage
			}
		}
	}
	return ""
}

var liveStages = []struct {
	stage    string
	prefixes []string
}{
	{"investigate", []string{"investigate", "investigation"}},
	{"design", []string{"design"}},
	{"review", []string{"agent-review", "review", "decide"}},
	{"checks", []string{"validate", "validation"}},
	{"implement", []string{"implement", "run-instruction", "seal-candidate", "apply", "applier"}},
	{"production", []string{"deliver-production", "browsercheck-production"}},
	{"staging", []string{"deliver-staging", "browsercheck-staging", "publish", "deliver"}},
	{"intake", []string{
		"parse-ticket", "read-ticket", "read-contract", "build-draft", "baseline", "git",
		"list-candidates", "derive-contract", "snapshot", "assess-readiness", "check-readiness",
		"decide-readiness", "check-ticket", "locate-target", "impasse-question",
	}},
}
