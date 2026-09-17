package ticketview

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/runner"
)

// stepCall finds every step the runner starts, with the step name as it is
// written at the call site. It reads the whole file rather than one line at
// a time: gofmt wraps a long call, and a scan that required the name on the
// same physical line as the call let a wrapped one through unseen - the
// drift this check exists to stop (review of #200).
var stepCall = regexp.MustCompile(`(?s)p\.(?:worker|step|controller)\(\s*ctx\s*,\s*`)

// pinnedStages is what every step's stage is, written out a second time so
// moving one in the table is a deliberate act with a failing test behind it.
// The first version of this check only asked "does this step have a stage",
// which left 30 of the entries free to move anywhere with the suite green
// (review of #200).
var pinnedStages = map[string]string{
	"read-ticket": "intake", "read-contract": "intake", "build-draft": "intake",
	"derive-contract": "intake", "list-candidates": "intake", "locate-target": "intake",
	"baseline": "intake", "snapshot": "intake", "assess-readiness": "intake",
	"check-readiness": "intake", "decide-readiness": "intake", "git-checkout": "intake",

	"investigate": "investigate",

	"agent-design-review": "design", "decide-design": "design", "design-impasse-question": "design",

	"implement": "implement", "implement-instruction": "implement", "run-instruction": "implement",

	"agent-review": "review", "review": "review", "seal-candidate": "review",

	"decide": "checks", "apply": "checks", "run-validation": "checks",
	"verify-applied": "checks", "verify-publish-gate": "checks", "impasse-question": "checks",
	"wait-feature": "checks",

	"create-feature-pr": "staging", "publish-feature": "staging", "compose-trail": "staging",
	"merge-feature": "staging", "await-staging": "staging",
	"read-merged": "staging", "promotion-delta": "staging", "browsercheck-staging": "staging",

	"await-merged-staging": "confirm",

	"create-promotion-pr": "production", "merge-promotion": "production",
	"await-production": "production", "browsercheck-production": "production",
}

// TestEveryRunnerStepHasAStage measures the stage table against the runner's
// own call sites, in both directions: every step the engine starts has a
// stage, and it is the stage pinned above. A table maintained by hand drifts
// the moment a step is added or moved; this fails the suite instead (live
// 2026-09-17).
func TestEveryRunnerStepHasAStage(t *testing.T) {
	names, prefixes := runnerStepNames(t)
	// The engine starts more than forty steps. A scan that suddenly finds
	// far fewer is not a smaller engine, it is a scan that stopped seeing
	// call sites (review of #200).
	if len(names) < 40 {
		t.Fatalf("only %d step names were found; the scan is looking in the wrong place", len(names))
	}
	for _, name := range names {
		file := runner.LiveLogName(name)
		want, pinned := pinnedStages[file]
		if !pinned {
			t.Errorf("step %q (file %q) is in no pinned stage; add it to pinnedStages and to the table", name, file)
			continue
		}
		if stage := LiveStage(file); stage != want {
			t.Errorf("step %q is offered under %q, pinned as %q", name, stage, want)
		}
	}
	for _, prefix := range prefixes {
		// The step is named for what it acts on, so the table must cover
		// the whole family, not one remembered member of it.
		if stage := LiveStage(runner.LiveLogName(prefix + "anything")); stage == "" {
			t.Errorf("steps named %q… belong to no stage", prefix)
		}
	}
}

// TestPinnedStagesAndTheTableAgree reads the pins from the other side: an
// entry moved in the table, or one the pins never knew about, fails here.
func TestPinnedStagesAndTheTableAgree(t *testing.T) {
	for step, want := range pinnedStages {
		if stage := LiveStage(step); stage != want {
			t.Errorf("LiveStage(%q) = %q, want %q", step, stage, want)
		}
	}
	for step, stage := range liveStages {
		if _, pinned := pinnedStages[step]; !pinned {
			t.Errorf("the table files %q under %q, and nothing pins it", step, stage)
		}
	}
	if stage := LiveStage("a-step-nobody-wrote"); stage != "" {
		t.Errorf("an unknown step claimed stage %q", stage)
	}
}

// Every rail stage has at least one step. A stage with none is a stage a
// reader hovers and is told there is nothing, for ever — which is how 確認
// behaved while the wait it shows ran under a step filed at STG.
func TestEveryRailStageHasItsSteps(t *testing.T) {
	counted := map[string]int{}
	for _, stage := range liveStages {
		counted[stage]++
	}
	for _, rule := range liveStagePrefixes {
		counted[rule.stage]++
	}
	for _, stage := range []string{
		"intake", "investigate", "design", "implement", "review", "checks", "staging", "confirm", "production",
	} {
		if counted[stage] == 0 {
			t.Errorf("rail stage %q has no step at all", stage)
		}
		if StageName(stage) == "" {
			t.Errorf("rail stage %q has no name a requester reads", stage)
		}
	}
}

// runnerStepNames reads the runner's sources and returns the step names it
// starts: the whole names, and the prefixes of the two it builds from what
// they act on. A call site whose name is neither fails the test rather than
// passing unnoticed, which is the point — an unscanned call site is exactly
// how a stage goes missing.
func runnerStepNames(t *testing.T) (names []string, prefixes []string) {
	t.Helper()
	entries, err := os.ReadDir("../runner")
	if err != nil {
		t.Fatalf("the runner's sources could not be read: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		body, err := os.ReadFile(filepath.Join("../runner", entry.Name()))
		if err != nil {
			t.Fatalf("%s could not be read: %v", entry.Name(), err)
		}
		source := string(body)
		for _, at := range stepCall.FindAllStringIndex(source, -1) {
			argument := strings.TrimLeft(source[at[1]:], " \t\n")
			if !strings.HasPrefix(argument, `"`) {
				// The forwarding wrappers pass the name they were given;
				// anything else is a step this scan cannot see.
				if field := strings.FieldsFunc(argument, func(r rune) bool {
					return r == ',' || r == ' ' || r == '\n' || r == '\t'
				}); len(field) > 0 && field[0] == "name" {
					continue
				}
				head := argument
				if len(head) > 40 {
					head = head[:40]
				}
				t.Errorf("%s: a step is started with %q, which this scan cannot resolve to a name", entry.Name(), head)
				continue
			}
			end := strings.Index(argument[1:], `"`)
			if end < 0 {
				t.Errorf("%s: a step name is not closed", entry.Name())
				continue
			}
			literal := argument[1 : end+1]
			if strings.HasPrefix(strings.TrimLeft(argument[end+2:], " \t\n"), "+") {
				prefixes = append(prefixes, literal)
				continue
			}
			names = append(names, literal)
		}
	}
	return names, prefixes
}

// The copy of the file-naming rule in this package has to agree with the
// engine's, or a stage is looked up under a name no file ever has.
func TestLiveLogNamesAgree(t *testing.T) {
	for _, step := range []string{
		"git checkout", "agent-design-review", "run-instruction", "browsercheck-production",
		"", "...", strings.Repeat("x", 100), "a b/c:d", "decide", "実装",
	} {
		if mine, theirs := liveLogName(step), runner.LiveLogName(step); mine != theirs {
			t.Errorf("%q: this package names the file %q, the engine names it %q", step, mine, theirs)
		}
	}
}
