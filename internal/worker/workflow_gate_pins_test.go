package worker

import (
	"strings"
	"testing"
	"time"
)

// Four checks that a sibling check happens to cover for the inputs the
// other tests use, so removing any one of them alone left the whole module
// green. Each is measured here on an input only it refuses.

// The path pattern and the writable declaration are two different
// questions, and the answers differ: a dotted path that IS inside a
// declared prefix passes the declaration and must still be refused by the
// pattern. Nothing else in the engine produces such a path, which is
// exactly why the pattern has to be the thing that refuses it.
func TestThePathPatternRefusesADottedPathInsideADeclaredPrefix(t *testing.T) {
	inside := ".github/workflows/deploy-staging.yml"
	prefixes := []string{".github/workflows/"}
	if !allowedPath(inside, prefixes) {
		t.Fatal("the declaration in this test does not cover the path, so it measures nothing")
	}
	if validRelativePathWithin(inside, WorkflowAllowance{}) {
		t.Fatal("the path pattern admitted a dotted path with no plan behind it")
	}
	if !validRelativePathWithin(inside, NewWorkflowAllowance([]string{inside})) {
		t.Fatal("the path pattern refused the file a plan named")
	}
	// The floor itself is untouched: no destination can declare such a
	// prefix in the first place.
	if validFilePrefix(".github/workflows/") {
		t.Fatal("a destination can now declare the workflow directory writable")
	}
}

// The changed-file tally asks two questions of every path, and only the
// second refuses a hidden file whose FIRST component is not hidden.
//
// The path pattern reads the first character and nothing else, so
// "ops/.env" passes it, sits inside an ordinary declared scope, and is a
// credential file an agent left behind. What keeps it out of the delivery
// is the dotted-component check beside the pattern, and only that.
func TestTheChangedFileTallyKeepsItsOwnDottedFloor(t *testing.T) {
	hidden := "ops/.env"
	if !validRelativePath(hidden) {
		t.Fatal("the path pattern already refuses this, so the test measures nothing")
	}
	root := workflowRepository(t, hidden)
	_, err := ChangedFilesUnder(root, []string{"ops/"}, nil, WorkflowAllowance{})
	if err == nil {
		t.Fatal("the tally carried a hidden file from inside the declared scope")
	}
	if !strings.Contains(err.Error(), "not addressable") {
		t.Fatalf("the refusal came from somewhere else: %v", err)
	}
	// The plan-named workflow file still travels, so the floor above it is
	// the check rather than a blanket refusal.
	planned := workflowRepository(t, plannedWorkflow)
	changed, err := ChangedFilesUnder(planned, []string{"ops/"}, nil, NewWorkflowAllowance([]string{plannedWorkflow}))
	if err != nil || len(changed) != 1 || changed[0] != plannedWorkflow {
		t.Fatalf("the plan's own workflow file was refused: %v / %v", changed, err)
	}
}

// The contract needs the destination's own handed policy, not just a plan.
// A plan is a file on a volume; the policy is what an operator wrote. The
// other tests give a destination with a policy, so the refusal for one
// without is measured here.
func TestTheContractRefusesAPlanNoDestinationHanded(t *testing.T) {
	bare := validTestConfig()
	draft := workflowDraft(t, bare)
	// Nothing else in this contract is wrong: the same call against a
	// destination that handed the means is accepted.
	handed := configWithMeans(t)
	handedDraft := workflowDraft(t, handed)
	if _, err := handedDraft.WithTargetFilesBuilding(
		[]string{plannedWorkflow}, []string{plannedWorkflow}, handed); err != nil {
		t.Fatalf("the reference contract is not valid: %v", err)
	}
	if _, err := draft.WithTargetFilesBuilding(
		[]string{plannedWorkflow}, []string{plannedWorkflow}, bare); err == nil {
		t.Fatal("a contract naming a workflow file was accepted with no means handed")
	}
}

// A sealed plan is a record read back from a volume, and the files it names
// open the one hole in the path vocabulary. A record edited to name
// something else has to read as not ours rather than have that name quietly
// dropped by whatever reads it next.
func TestASealedPlanNamingSomethingElseIsNotOurs(t *testing.T) {
	plan := ReleasePathPlan{
		SchemaVersion: ReleasePathSchemaVersion, Repository: "example/consumer",
		Configured: "production", DecidedAt: time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC),
		Items: []ReleasePathItem{{Name: plannedWorkflow, Kind: ReleasePathWorkflow, Detail: "build it"}},
	}
	for name, files := range map[string][]string{
		"a path that escapes the directory": {".github/workflows/../../.ssh/config"},
		"another dotted file":               {".github/dependabot.yml"},
		"a file that is not a workflow":     {".github/workflows/deploy.sh"},
	} {
		forged := plan
		forged.WorkflowFiles = files
		if err := forged.Seal(); err != nil {
			t.Fatal(err)
		}
		if forged.Bound() {
			t.Fatalf("%s read back as one of ours", name)
		}
	}
	// The plain workflow file it is meant to carry still reads back.
	plan.WorkflowFiles = []string{plannedWorkflow}
	if err := plan.Seal(); err != nil {
		t.Fatal(err)
	}
	if !plan.Bound() {
		t.Fatal("a plan naming an ordinary workflow file does not read back")
	}
}
