package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	plannedWorkflow = ".github/workflows/deploy-staging.yml"
	siblingWorkflow = ".github/workflows/other.yml"
	siblingDotted   = ".github/dependabot.yml"
)

// handedPolicy is a destination that lets the engine author one workflow
// file, with a policy saying what may be in it.
func handedPolicy() *DeployWorkflowPolicy {
	return &DeployWorkflowPolicy{
		Paths:       []string{"deploy-staging.yml"},
		Triggers:    []string{WorkflowTriggerDispatch, WorkflowTriggerPush},
		Permissions: map[string]string{"contents": "read", "id-token": "write"},
		Secrets:     []string{"DEPLOY_ROLE_ARN"},
		Actions:     []string{"actions/checkout@8f4b7f84864484a7bf31766abe9204da3cbe65b3"},
		Runners:     []string{"ubuntu-latest"},
	}
}

// configWithMeans is the ordinary test configuration with the means handed.
func configWithMeans(t *testing.T) Config {
	t.Helper()
	config := validTestConfig()
	config.Consumers[0].Infrastructure = &InfrastructureConfig{
		Provider: "aws", DeployWorkflows: handedPolicy(),
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("a destination with the means handed does not load: %v", err)
	}
	return config
}

// Gate one and two, through the contract every later reader goes by: the
// pattern that refuses a dotted path, and the writable declaration a
// dotted path is never inside. A file the plan named passes both; a
// dotted sibling does not; and neither does the same file in a delivery
// whose contract names no workflow at all.
func TestTheTicketAdmitsOnlyThePlansOwnWorkflowFile(t *testing.T) {
	config := configWithMeans(t)
	draft := workflowDraft(t, config)

	if _, err := draft.WithTargetFilesBuilding(
		[]string{plannedWorkflow, "client/src/App.tsx"}, []string{plannedWorkflow}, config); err != nil {
		t.Fatalf("the contract refused the file its own plan named: %v", err)
	}
	for _, refused := range []string{siblingWorkflow, siblingDotted, ".github/workflows/../../etc/passwd"} {
		if _, err := draft.WithTargetFilesBuilding(
			[]string{refused}, []string{plannedWorkflow}, config); err == nil {
			t.Fatalf("the contract admitted %q, which no plan named", refused)
		}
	}
	if _, err := draft.WithTargetFilesBuilding([]string{plannedWorkflow}, nil, config); err == nil {
		t.Fatal("the contract admitted a workflow file with no plan behind it")
	}
	// A contract naming a file the destination handed no means for is
	// refused whatever the plan said: the two have to agree.
	bare := validTestConfig()
	if _, err := draft.WithTargetFilesBuilding(
		[]string{plannedWorkflow}, []string{plannedWorkflow}, bare); err == nil {
		t.Fatal("the contract admitted a workflow file the destination handed no means for")
	}
	// And a file this destination's own policy does not name is refused
	// even where the policy exists: the plan may only name what an
	// operator wrote down.
	if _, err := draft.WithTargetFilesBuilding(
		[]string{siblingWorkflow}, []string{siblingWorkflow}, config); err == nil {
		t.Fatal("the contract admitted a workflow file outside the destination's policy")
	}
}

// Gate three: the scan that reads what the agent changed.
func TestTheChangedFileTallyAdmitsOnlyThePlansOwnWorkflowFile(t *testing.T) {
	config := configWithMeans(t)
	prefixes := config.Consumers[0].Mode.AllowedFilePrefixes
	root := workflowRepository(t, plannedWorkflow)

	changed, err := ChangedFilesUnder(root, prefixes, nil, NewWorkflowAllowance([]string{plannedWorkflow}))
	if err != nil || len(changed) != 1 || changed[0] != plannedWorkflow {
		t.Fatalf("the plan's own workflow file was not seen: %v / %v", changed, err)
	}
	if _, err := ChangedFilesUnder(root, prefixes, nil, WorkflowAllowance{}); err == nil {
		t.Fatal("a workflow file was accepted with no plan behind it")
	}
	sibling := workflowRepository(t, siblingWorkflow)
	if _, err := ChangedFilesUnder(sibling, prefixes, nil, NewWorkflowAllowance([]string{plannedWorkflow})); err == nil {
		t.Fatal("a dotted sibling of the plan's file was accepted")
	}
}

// Gate four: the guard that refuses to let a deliverable vanish into the
// repository's own ignore rules. A workflow file the plan named is a
// deliverable, and a dotted path used to walk straight past this.
func TestTheIgnoredByproductGuardCoversThePlansOwnWorkflowFile(t *testing.T) {
	config := configWithMeans(t)
	prefixes := config.Consumers[0].Mode.AllowedFilePrefixes
	root := workflowRepository(t, plannedWorkflow)
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".github/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, root, "add", ".gitignore")
	runTestGit(t, root, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "ignore")

	_, err := ChangedFilesUnder(root, prefixes, nil, NewWorkflowAllowance([]string{plannedWorkflow}))
	if err == nil {
		t.Fatal("a plan-named workflow file the repository ignores was passed over in silence")
	}
	if !strings.Contains(err.Error(), "ignores") {
		t.Fatalf("the refusal does not say the repository ignores it: %v", err)
	}
	// The same directory, with nothing planned, is an ordinary byproduct
	// and stays tolerated: this guard is not a way to refuse dotted files.
	if _, err := ChangedFilesUnder(root, prefixes, nil, WorkflowAllowance{}); err != nil {
		t.Fatalf("an ignored dotted directory stopped a run with no plan: %v", err)
	}
}

// Gate five: the sealing of a model-authored candidate.
func TestTheCandidateSealAdmitsOnlyThePlansOwnWorkflowFile(t *testing.T) {
	config := configWithMeans(t)
	draft := workflowDraft(t, config)
	request, err := draft.WithTargetFilesBuilding([]string{plannedWorkflow}, []string{plannedWorkflow}, config)
	if err != nil {
		t.Fatal(err)
	}
	passing := ModelCandidateOutput{
		Rationale: "Build the deploy workflow this destination has no path without.",
		Files:     []ModelCandidateFile{{Path: plannedWorkflow, Content: passingWorkflow}},
	}
	if err := validateModelCandidateOutput(passing, request, config); err != nil {
		t.Fatalf("the seal refused the file its own plan named: %v", err)
	}
	// Without a plan behind it the same bytes are not sealable.
	bare, err := draft.WithTargetFilesBuilding([]string{"client/src/App.tsx"}, nil, config)
	if err != nil {
		t.Fatal(err)
	}
	unplanned := ModelCandidateOutput{Rationale: "x", Files: []ModelCandidateFile{{Path: plannedWorkflow, Content: passingWorkflow}}}
	if err := validateModelCandidateOutput(unplanned, bare, config); err == nil {
		t.Fatal("the seal admitted a workflow file with no plan behind it")
	}
}

// workflowDraft is a ticket draft for the destination with the means.
func workflowDraft(t *testing.T, config Config) TicketDraft {
	t.Helper()
	request, err := ParseTicket(validTicketEnvelope(t, validTicketDescription()), config)
	if err != nil {
		t.Fatal(err)
	}
	return TicketDraft{
		SchemaVersion: request.SchemaVersion, DeliveryID: request.DeliveryID,
		InputSHA256: request.InputSHA256, ConfigSHA256: request.ConfigSHA256, ToolSHA: request.ToolSHA,
		IssueKey: request.IssueKey, RunID: request.RunID, Repository: request.Repository,
		Mode: request.Mode, Summary: request.Summary, Request: request.Request,
		VerificationPath: request.VerificationPath, ExpectedText: request.ExpectedText,
		AbsentText: request.AbsentText,
	}
}

// workflowRepository is a committed repository with one workflow file
// written into the working copy and not committed.
func workflowRepository(t *testing.T, name string) string {
	t.Helper()
	root := t.TempDir()
	runTestGit(t, root, "init")
	if err := os.WriteFile(filepath.Join(root, "seed.txt"), []byte("seed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, root, "add", "seed.txt")
	runTestGit(t, root, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "initial")
	full := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(passingWorkflow), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}
