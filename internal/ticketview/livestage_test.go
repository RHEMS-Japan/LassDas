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
// written at the call site.
var stepCall = regexp.MustCompile(`p\.(?:worker|step|controller)\(ctx,\s*(.+)$`)

// TestEveryRunnerStepHasAStage measures the stage table against the runner's
// own call sites. The first table was written from remembered step names and
// three of the engine's stages showed a requester nothing while their output
// sat in the run directory. A table maintained by hand drifts the moment a
// step is added; this fails the suite instead (live 2026-09-17).
func TestEveryRunnerStepHasAStage(t *testing.T) {
	names, prefixes := runnerStepNames(t)
	if len(names) < 30 {
		t.Fatalf("only %d step names were found; the scan is looking in the wrong place", len(names))
	}
	for _, name := range names {
		if stage := LiveStage(runner.LiveLogName(name)); stage == "" {
			t.Errorf("step %q belongs to no stage, so its live output is offered under no part of the rail", name)
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

// TestLiveStageNamesAreWholeNames pins the shadowing the prefix table
// allowed: the reception's own decision was filed under 審査 because it
// begins with the review stage's "decide".
func TestLiveStageNamesAreWholeNames(t *testing.T) {
	for name, want := range map[string]string{
		"decide-readiness":        "intake",
		"decide-design":           "design",
		"decide":                  "review",
		"agent-design-review":     "design",
		"agent-review":            "review",
		"design-impasse-question": "design",
		"impasse-question":        "intake",
		"run-instruction":         "implement",
		"run-validation":          "checks",
		"git-checkout":            "intake",
		"browsercheck-staging":    "staging",
		"browsercheck-production": "production",
	} {
		if stage := LiveStage(name); stage != want {
			t.Errorf("LiveStage(%q) = %q, want %q", name, stage, want)
		}
	}
	if stage := LiveStage("a-step-nobody-wrote"); stage != "" {
		t.Errorf("an unknown step claimed stage %q", stage)
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
		for _, line := range strings.Split(string(body), "\n") {
			match := stepCall.FindStringSubmatch(line)
			if match == nil {
				continue
			}
			argument := strings.TrimSpace(match[1])
			if !strings.HasPrefix(argument, `"`) {
				// The forwarding wrappers pass the name they were given;
				// anything else is a step this scan cannot see.
				if field := strings.FieldsFunc(argument, func(r rune) bool { return r == ',' || r == ' ' }); len(field) > 0 && field[0] == "name" {
					continue
				}
				t.Errorf("%s: a step is started with %q, which this scan cannot resolve to a name", entry.Name(), argument)
				continue
			}
			end := strings.Index(argument[1:], `"`)
			if end < 0 {
				t.Errorf("%s: a step name is not closed: %q", entry.Name(), argument)
				continue
			}
			literal := argument[1 : end+1]
			if strings.HasPrefix(strings.TrimSpace(argument[end+2:]), "+") {
				prefixes = append(prefixes, literal)
				continue
			}
			names = append(names, literal)
		}
	}
	return names, prefixes
}
