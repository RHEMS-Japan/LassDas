package attendant

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// What the reception is given before it asks is sealed where it can read
// it, and only for a destination that is missing something.
func TestThePreparationSealsWhatTheReceptionNeedsToKnow(t *testing.T) {
	config, run, runDir := releasePathFixture(t, withoutWorkflowMeans, true)

	if err := prepareReleasePath(config, run, runDir, &recordingLogger{})(); err != nil {
		t.Fatalf("prepareReleasePath: %v", err)
	}
	plan, ok := readReleasePathPlan(runDir)
	if !ok {
		t.Fatal("the reception was given nothing to read")
	}
	if !contains(plan.UnappliedNames(), ".github/workflows/deploy-staging.yml") {
		t.Fatalf("the sealed plan does not carry the missing means: %v", plan.UnappliedNames())
	}

	// A destination whose path is complete seals nothing, so the reception
	// has nothing to ask about.
	complete, run, completeDir := releasePathFixture(t, completeReleasePath, true)
	for _, name := range []string{"ops/deploy-staging.yml", "ops/deploy-production.yml"} {
		path := filepath.Join(completeDir, "target-repo", filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("on: push\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := prepareReleasePath(complete, run, completeDir, &recordingLogger{})(); err != nil {
		t.Fatalf("prepareReleasePath: %v", err)
	}
	if _, sealed := readReleasePathPlan(completeDir); sealed {
		t.Fatal("a destination whose path is complete was given a plan")
	}
	// A destination this engine cannot read at all reports the failure
	// rather than sealing a plan built on nothing.
	broken, run, brokenDir := releasePathFixture(t, withoutWorkflowMeans, true)
	if err := os.Remove(filepath.Join(brokenDir, "ticket-draft.json")); err != nil {
		t.Fatal(err)
	}
	if err := prepareReleasePath(broken, run, brokenDir, &recordingLogger{})(); err == nil {
		t.Fatal("an unreadable destination was prepared as if it had been read")
	}
	if _, sealed := readReleasePathPlan(brokenDir); sealed {
		t.Fatal("a plan was sealed for a destination that could not be read")
	}
}

// The preparation is handed to the run's preparation, not run beside it.
//
// Read out of the source because that is where the mistake lives and
// nowhere else can see it: passing nil here compiles, every test stays
// green, the delivery still finishes, and the one question the reception
// would have asked about a means this engine was not handed silently stops
// existing. The seam itself — that what is handed runs after the clone and
// before the reception — is pinned on the other side, in the runner.
func TestThePreparationIsHandedToTheRunsPreparation(t *testing.T) {
	source, err := os.ReadFile("chains.go")
	if err != nil {
		t.Fatal(err)
	}
	call := regexp.MustCompile(`PrepareChainRun\(ctx, ([^)]*)\)`).FindSubmatch(source)
	if call == nil {
		t.Fatal("the run's preparation is no longer called from here")
	}
	handed := strings.TrimSpace(string(call[1]))
	if !strings.HasPrefix(handed, "prepareReleasePath(") {
		t.Fatalf("the reception is prepared with %q rather than the release path", handed)
	}
}
