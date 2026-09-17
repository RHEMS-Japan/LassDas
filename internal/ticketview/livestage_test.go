package ticketview

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/runner"
	"sort"
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
	"read-merged-feature": "staging", "promotion-delta": "staging", "browsercheck-staging": "staging",

	"await-merged-staging": "confirm",

	"create-promotion-pr": "production", "merge-promotion": "production",
	"await-production": "production", "browsercheck-production": "production",
	"read-merged-promotion": "production",
}

// TestEveryRunnerStepHasAStage measures the stage table against the runner's
// own call sites, in both directions: every step the engine starts has a
// stage, and it is the stage pinned above. A table maintained by hand drifts
// the moment a step is added or moved; this fails the suite instead (live
// 2026-09-17).
func TestEveryRunnerStepHasAStage(t *testing.T) {
	names, prefixes := runnerStepNames(t)
	// The engine starts this many steps. A scan that finds fewer is not a
	// smaller engine, it is a scan that stopped seeing call sites, or an
	// engine that lost one - and a lost step is how a rail stage went empty
	// (review of #200).
	if len(names) < 45 {
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
	// And the other way: every pinned step is one the scan still finds in
	// the engine. Counting names caught a step that stopped being
	// collected only while nothing else was added in its place; this
	// catches it either way (review of #200).
	names, prefixes := runnerStepNames(t)
	found := map[string]bool{}
	for _, name := range names {
		found[runner.LiveLogName(name)] = true
	}
	for step := range pinnedStages {
		if found[step] {
			continue
		}
		covered := false
		for _, prefix := range prefixes {
			if strings.HasPrefix(step, runner.LiveLogName(prefix + "x")[:len(runner.LiveLogName(prefix+"x"))-1]) {
				covered = true
				break
			}
		}
		if !covered {
			t.Errorf("%q is pinned to a stage and the scan no longer finds the engine starting it", step)
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
	// Counted from the steps the engine actually starts, not from the
	// table's own entries: counting the table meant the engine could lose
	// its only 確認 step and every check stayed green - the regression this
	// change fixed, restorable in silence (review of #200).
	names, prefixes := runnerStepNames(t)
	counted := map[string]int{}
	for _, name := range names {
		counted[LiveStage(runner.LiveLogName(name))]++
	}
	for _, prefix := range prefixes {
		counted[LiveStage(runner.LiveLogName(prefix+"anything"))]++
	}
	for _, stage := range []string{
		"intake", "investigate", "design", "implement", "review", "checks", "staging", "confirm", "production",
	} {
		if counted[stage] == 0 {
			t.Errorf("rail stage %q has no step at all", stage)
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
	helpers := forwardedStepNames(t)
	for helper, literals := range helpers {
		if len(literals) == 0 {
			t.Errorf("%s is handed the step it starts and no caller names one", helper)
		}
		names = append(names, literals...)
	}
	for file, source := range runnerSources(t) {
		for fn, body := range runnerFunctions(source) {
			for _, at := range stepCall.FindAllStringIndex(body, -1) {
				argument := strings.TrimLeft(body[at[1]:], " \t\n")
				if !strings.HasPrefix(argument, `"`) {
					// A name this function was handed. Excused only inside
					// the function that was handed it, and only for the
					// parameter it was handed: keyed on the name alone,
					// any local variable called "step" hid a step from the
					// scan entirely (review of #200).
					field := strings.FieldsFunc(argument, func(r rune) bool {
						return r == ',' || r == ' ' || r == '\n' || r == '\t'
					})
					if len(field) > 0 && field[0] == forwardedParam[fn] {
						continue
					}
					head := argument
					if len(head) > 40 {
						head = head[:40]
					}
					t.Errorf("%s: %s starts a step with %q, which this scan cannot resolve to a name", file, fn, head)
					continue
				}
				close := strings.Index(argument[1:], `"`)
				if close < 0 {
					t.Errorf("%s: a step name is not closed", file)
					continue
				}
				literal := argument[1 : close+1]
				if strings.HasPrefix(strings.TrimLeft(argument[close+2:], " \t\n"), "+") {
					prefixes = append(prefixes, literal)
					continue
				}
				names = append(names, literal)
			}
		}
	}
	return names, prefixes
}

// forwardedParam is the parameter each helper is handed its step in, filled
// by forwardedStepNames.
var forwardedParam = map[string]string{}

// scannedWrappers are the three the scan matches at their own call sites, so
// their callers' names are already collected once. Collecting them again as
// forwarded names doubled every ordinary step and left the floor on how many
// the scan expects unable to fire (review of #200).
var scannedWrappers = map[string]bool{"worker": true, "step": true, "controller": true}

// cardRailStage is where the attendant puts the rail while one chain card
// runs (internal/attendant/status.go). Nine lines, and the only thing this
// derivation takes on trust; everything else below is read out of the
// engine.
//
// The publish card is absent on purpose: the attendant places "reporting"
// for it, which the board's rail does not draw at all, so a step reached
// only from there has no lit stage to belong to. That gap is older than
// this table and is named in it.
// It is an approximation: status.go does not hold a per-card table, it
// picks from the cards that are open. These nine hold while one card is
// open and nothing is failing or waiting. They stop holding when a card
// has failed (the rail says 実装 whatever the card was), when a card is
// waiting for a person (the rail says attention), and while the implement
// card is still open (it is chosen before review and validate).
var cardRailStage = map[string]string{
	"investigate":     "investigate",
	"design-review-a": "design",
	"design-review-b": "design",
	"design-decide":   "design",
	"implement":       "implement",
	"apply":           "implement",
	"review-a":        "review",
	"review-b":        "review",
	"validate":        "checks",
}

// TestTheTableAgreesWithTheCardThatRunsEachStep is the rule measured rather
// than restated. pinnedStages is a second copy of the same judgement and
// moves with the table; this walks the engine instead — the card
// orchestration's own switch says which function runs a card, the call
// graph says which steps that function can reach, and the table has to
// agree. A step moved to the wrong stage fails here even if both copies
// were changed together (review of #200).
func TestTheTableAgreesWithTheCardThatRunsEachStep(t *testing.T) {
	calls, steps := runnerCallGraph(t)
	entries := cardEntryFunctions(t)
	if len(entries) < 9 {
		t.Fatalf("only %d cards were found in the orchestration's switch", len(entries))
	}
	// Which cards can reach each step, and which functions start it.
	reached := map[string]map[string]bool{}
	underACard := map[string]bool{}
	for card, entry := range entries {
		for _, fn := range reachableFrom(entry, calls) {
			underACard[fn] = true
			for _, step := range steps[fn] {
				if reached[step] == nil {
					reached[step] = map[string]bool{}
				}
				reached[step][card] = true
			}
		}
	}
	// The reception is not a card, and its work is most of what a reader
	// watches under 受付, so a step it starts is the table's call. Only the
	// reception: the one-process mode runs the same stages without cards,
	// and letting that abstain took the review steps out of the derivation
	// entirely - agent-review could be moved anywhere and stay green
	// (review of #200).
	reception := map[string]bool{}
	for _, entry := range []string{"pretrip", "readinessGate"} {
		for _, fn := range reachableFrom(entry, calls) {
			reception[fn] = true
		}
	}
	startedOutside := map[string]bool{}
	for fn, started := range steps {
		if !reception[fn] {
			continue
		}
		for _, step := range started {
			startedOutside[step] = true
		}
	}
	// Which steps the reception starts, pinned: the entry points are named
	// by literal, and reachableFrom answers about a name it does not know
	// with a set of one rather than an error - so renaming one emptied the
	// abstention in silence (review of #200).
	var abstained []string
	for step := range startedOutside {
		abstained = append(abstained, step)
	}
	sort.Strings(abstained)
	if strings.Join(abstained, " ") != strings.Join(receptionSteps, " ") {
		t.Errorf("the reception starts %v; it started %v when this was written", abstained, receptionSteps)
	}
	if len(reached) < 15 {
		t.Fatalf("only %d steps were reached from the cards; the walk is not working", len(reached))
	}
	decided := []string{}
	for step, cards := range reached {
		if startedOutside[step] {
			continue
		}
		want := map[string]bool{}
		for card := range cards {
			if rail, known := cardRailStage[card]; known {
				want[rail] = true
			}
		}
		if len(want) == 0 {
			// Every card that reaches it lights no rail stage; the table
			// decides, and its comment says so.
			continue
		}
		decided = append(decided, step)
		if got := LiveStage(runnerLogName(step)); !want[got] {
			t.Errorf("step %q is offered under %q, but the cards that run it put the rail at %v",
				step, got, keysOf(want))
		}
	}
	// Which steps this decides, not how many: one entering and one leaving
	// left the count where it was while a step slipped back to being
	// checked by nothing but a second hand-written copy (review of #200).
	//
	// What is missing from this list is decided by hand and says why in the
	// table: the reception is not a card, the publish card lights no rail
	// stage, the delivery and confirmation cards are not in the
	// orchestration's switch, and a name the engine builds from what it
	// acts on is a family rather than a step.
	sort.Strings(decided)
	if strings.Join(decided, " ") != strings.Join(derivedSteps, " ") {
		t.Errorf("the walk decides %v; it decided %v when it was written", decided, derivedSteps)
	}
}

// derivedSteps is every step whose stage the walk settles. Adding to it is
// coverage growing; removing from it is coverage going away, and either has
// to be a deliberate edit here.
// receptionSteps is what the reception starts. The walk leaves these to
// the table, so the set has to be a deliberate edit rather than a rename
// nobody noticed.
var receptionSteps = []string{
	"assess-readiness", "baseline", "build-draft", "check-readiness", "decide-readiness",
	"derive-contract", "list-candidates", "locate-target",
	"read-contract", "read-ticket", "snapshot",
}

var derivedSteps = []string{
	"agent-design-review", "agent-review", "apply", "decide", "decide-design",
	"impasse-question", "investigate", "run-instruction", "run-validation",
	"seal-candidate", "verify-applied", "verify-publish-gate",
}

// cardEntryFunctions reads the card orchestration's own switch: which
// function runs which card.
func cardEntryFunctions(t *testing.T) map[string]string {
	t.Helper()
	body, err := os.ReadFile("../runner/chain_stage.go")
	if err != nil {
		t.Fatal(err)
	}
	// case runtime.StageX:\n\t\treturn p.fnName(
	pattern := regexp.MustCompile(`case runtime\.(Stage\w+):\s*\n\s*return p\.(\w+)\(`)
	names := stageConstantValues(t)
	entries := map[string]string{}
	for _, match := range pattern.FindAllStringSubmatch(string(body), -1) {
		value, known := names[match[1]]
		if !known {
			t.Errorf("the orchestration names %s, which internal/runtime does not define", match[1])
			continue
		}
		entries[value] = match[2]
	}
	return entries
}

// stageConstantValues reads the card names themselves out of the runtime.
func stageConstantValues(t *testing.T) map[string]string {
	t.Helper()
	body, err := os.ReadFile("../runtime/chain.go")
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for _, match := range regexp.MustCompile(`(Stage\w+)\s*=\s*"([a-z-]+)"`).FindAllStringSubmatch(string(body), -1) {
		values[match[1]] = match[2]
	}
	return values
}

// runnerCallGraph reads every function in the runner: which functions on
// the pipeline it calls, and which steps it starts itself.
func runnerCallGraph(t *testing.T) (calls map[string][]string, steps map[string][]string) {
	t.Helper()
	calls, steps = map[string][]string{}, map[string][]string{}
	define := regexp.MustCompile(`(?m)^func (?:\(p \*Pipeline\) )?(\w+)`)
	call := regexp.MustCompile(`p\.(\w+)\(`)
	for _, source := range runnerSources(t) {
		bounds := define.FindAllStringSubmatchIndex(source, -1)
		for index, at := range bounds {
			name := source[at[2]:at[3]]
			end := len(source)
			if index+1 < len(bounds) {
				end = bounds[index+1][0]
			}
			fnBody := source[at[1]:end]
			for _, c := range call.FindAllStringSubmatch(fnBody, -1) {
				calls[name] = append(calls[name], c[1])
			}
			for _, at := range stepCall.FindAllStringIndex(fnBody, -1) {
				argument := strings.TrimLeft(fnBody[at[1]:], " \t\n")
				if !strings.HasPrefix(argument, `"`) {
					continue
				}
				close := strings.Index(argument[1:], `"`)
				if close < 0 {
					continue
				}
				if strings.HasPrefix(strings.TrimLeft(argument[close+2:], " \t\n"), "+") {
					// A name the engine builds from what the step acts on
					// ("git ", "browsercheck-"). It is a family, not a
					// step, and the table covers each family with its own
					// rule and its own reason.
					continue
				}
				steps[name] = append(steps[name], argument[1:close+1])
			}
		}
	}
	return calls, steps
}

// forwardedStepNames finds the helpers that are handed the step they
// start, and reads the names their callers hand them. Without this a
// helper shared by two phases could only start one step under one name,
// and both phases' output landed in one file under one stage (review of
// #200).
func forwardedStepNames(t *testing.T) map[string][]string {
	t.Helper()
	sources := runnerSources(t)
	byHelper := map[string]string{}
	define := regexp.MustCompile(`(?m)^func \(p \*Pipeline\) (\w+)\(ctx context\.Context, (\w+)[ ,]`)
	for _, source := range sources {
		for _, at := range define.FindAllStringSubmatchIndex(source, -1) {
			fn, param := source[at[2]:at[3]], source[at[4]:at[5]]
			if scannedWrappers[fn] {
				continue
			}
			end := len(source)
			if next := regexp.MustCompile(`(?m)^func `).FindStringIndex(source[at[1]:]); next != nil {
				end = at[1] + next[0]
			}
			// Only when the helper actually starts a step with that name.
			// The same shape the step scan uses, because gofmt wraps a
			// long call and a pattern that needed "(ctx," on one line
			// quietly stopped seeing it (review of #200).
			starts := regexp.MustCompile(`(?s)p\.(?:worker|step|controller)\(\s*ctx\s*,\s*` + param + `\b`)
			if starts.MatchString(source[at[1]:end]) {
				byHelper[fn] = param
				forwardedParam[fn] = param
			}
		}
	}
	// The wrappers are handed "name" and are scanned at their own call
	// sites, so the scan excuses that one word inside them without
	// collecting anything.
	for wrapper := range scannedWrappers {
		forwardedParam[wrapper] = "name"
	}
	found := map[string][]string{}
	for helper := range byHelper {
		found[helper] = nil
		call := regexp.MustCompile(`(?s)p\.` + helper + `\(\s*ctx\s*,\s*"([^"]+)"`)
		for _, source := range sources {
			for _, match := range call.FindAllStringSubmatch(source, -1) {
				found[helper] = append(found[helper], match[1])
			}
		}
	}
	return found
}

// runnerFunctions splits one source into its function bodies by name.
func runnerFunctions(source string) map[string]string {
	define := regexp.MustCompile(`(?m)^func (?:\([^)]*\) )?(\w+)`)
	bodies := map[string]string{}
	bounds := define.FindAllStringSubmatchIndex(source, -1)
	for index, at := range bounds {
		end := len(source)
		if index+1 < len(bounds) {
			end = bounds[index+1][0]
		}
		bodies[source[at[2]:at[3]]] += source[at[1]:end]
	}
	return bodies
}

// runnerSources reads every non-test source of the runner.
func runnerSources(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir("../runner")
	if err != nil {
		t.Fatalf("the runner's sources could not be read: %v", err)
	}
	sources := map[string]string{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		body, err := os.ReadFile(filepath.Join("../runner", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		sources[entry.Name()] = string(body)
	}
	return sources
}

// reachableFrom walks the call graph from one entry function.
func reachableFrom(entry string, calls map[string][]string) []string {
	seen, queue := map[string]bool{entry: true}, []string{entry}
	for len(queue) > 0 {
		fn := queue[0]
		queue = queue[1:]
		for _, next := range calls[fn] {
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
	out := make([]string, 0, len(seen))
	for fn := range seen {
		out = append(out, fn)
	}
	return out
}

// runnerLogName is the step's name as its live file is named, repeating
// runner.LiveLogName's rule for the one case that differs (a space).
func runnerLogName(step string) string { return runner.LiveLogName(step) }

func keysOf(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
