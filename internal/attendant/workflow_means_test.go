package attendant

import (
	"strings"
	"testing"
)

// handedWorkflowMeans is the destination of completeReleasePath with its
// deploy workflows missing from the repository and the means to write them
// handed: a content policy naming the two files and what they may contain.
const handedWorkflowMeans = `{"repository":"` + gapRepository + `","delivery":"production",` +
	`"integration_branch":"develop","release_branch":"main",` +
	`"staging_origin":"https://staging.example.test","production_origin":"https://www.example.test",` +
	`"staging_workflow":"deploy-staging.yml","production_workflow":"deploy-production.yml",` +
	`"production_login_url":"https://www.example.test/login","observation_language":"ja",` +
	`"github_contract":{"staging_workflow":{"path":".github/workflows/deploy-staging.yml"},` +
	`"production_workflows":[{"path":".github/workflows/deploy-production.yml"}]},` +
	`"mode":{"allowed_file_prefixes":["ops/","src/"]},` +
	`"infrastructure":{"provider":"aws","deploy_workflows":{` +
	`"paths":["deploy-staging.yml","deploy-production.yml"],` +
	`"triggers":["workflow_dispatch","push"],` +
	`"permissions":{"contents":"read","id-token":"write"},` +
	`"secrets":["DEPLOY_ROLE_ARN"],` +
	`"actions":["actions/checkout@8f4b7f84864484a7bf31766abe9204da3cbe65b3"],` +
	`"runners":["ubuntu-latest"]}}}`

// withoutWorkflowMeans is the same destination with the policy taken away.
var withoutWorkflowMeans = strings.Replace(handedWorkflowMeans,
	`,"infrastructure":{"provider":"aws","deploy_workflows":{`+
		`"paths":["deploy-staging.yml","deploy-production.yml"],`+
		`"triggers":["workflow_dispatch","push"],`+
		`"permissions":{"contents":"read","id-token":"write"},`+
		`"secrets":["DEPLOY_ROLE_ARN"],`+
		`"actions":["actions/checkout@8f4b7f84864484a7bf31766abe9204da3cbe65b3"],`+
		`"runners":["ubuntu-latest"]}}`, "", 1)

// The one file the engine could never write becomes a file it writes, and
// the round is told which names it may take and every rule the file will be
// held to. A round told less than the rule it is measured against is a
// round refused for something nobody mentioned.
func TestTheHandedMeansPutsTheWorkflowFilesInTheRoundsInstruction(t *testing.T) {
	config, run, runDir := releasePathFixture(t, handedWorkflowMeans, true)

	plan, err := detectReleasePathGap(config, run, runDir)
	if err != nil {
		t.Fatalf("detectReleasePathGap: %v", err)
	}
	want := []string{".github/workflows/deploy-staging.yml", ".github/workflows/deploy-production.yml"}
	for _, file := range want {
		if !contains(plan.WorkflowFiles, file) {
			t.Fatalf("the plan does not name %q: %v", file, plan.WorkflowFiles)
		}
		if !strings.Contains(plan.Instruction, file) {
			t.Fatalf("the instruction does not name %q: %s", file, plan.Instruction)
		}
	}
	// The policy itself, quoted: the events, the ceiling, the one secret,
	// the pinned action and the runner.
	for _, quoted := range []string{
		"workflow_dispatch", "push", "contents: read", "id-token: write",
		"secrets.DEPLOY_ROLE_ARN", "actions/checkout@8f4b7f84864484a7bf31766abe9204da3cbe65b3",
		"ubuntu-latest", "main", "develop",
	} {
		if !strings.Contains(plan.Instruction, quoted) {
			t.Fatalf("the instruction does not quote %q: %s", quoted, plan.Instruction)
		}
	}
	// The settings the engine still cannot make true are reported by name,
	// handed means or not. The digest-commit policy describes a commit
	// nothing asks any round to make, so writing it would leave this
	// destination unable to reach production at all.
	if unapplied := releasePathUnapplied(plan); !contains(unapplied, "github_contract.staging_digest_commit") {
		t.Fatalf("the report does not name the digest-commit policy: %v", unapplied)
	}
	// And the instance's own execution settings, where they are missing.
	noCards, run, runDir := releasePathFixture(t, handedWorkflowMeans, false)
	without, err := detectReleasePathGap(noCards, run, runDir)
	if err != nil {
		t.Fatalf("detectReleasePathGap: %v", err)
	}
	for _, key := range []string{
		"chain.deliver.checks_profile", "chain.deliver.integrate_profile", "chain.deliver.promote_profile",
	} {
		if !contains(releasePathUnapplied(without), key) {
			t.Fatalf("the report does not name %q: %v", key, releasePathUnapplied(without))
		}
	}
}

// Without the means nothing changes: the workflow file is out of reach, the
// round is not told to write one, and the report names the settings by the
// keys an operator knows them by.
func TestWithoutTheMeansNoWorkflowIsNamedAndTheReportSaysSo(t *testing.T) {
	config, run, runDir := releasePathFixture(t, withoutWorkflowMeans, true)

	plan, err := detectReleasePathGap(config, run, runDir)
	if err != nil {
		t.Fatalf("detectReleasePathGap: %v", err)
	}
	if len(plan.WorkflowFiles) != 0 {
		t.Fatalf("a destination that handed no means got workflow files: %v", plan.WorkflowFiles)
	}
	for _, file := range []string{"deploy-staging.yml", "deploy-production.yml"} {
		if strings.Contains(plan.Instruction, ".github/workflows/"+file) {
			t.Fatalf("the round was told to write %q with no means handed: %s", file, plan.Instruction)
		}
	}
	unapplied := releasePathUnapplied(plan)
	for _, key := range []string{
		".github/workflows/deploy-staging.yml",
		".github/workflows/deploy-production.yml",
		"github_contract.staging_digest_commit",
	} {
		if !contains(unapplied, key) {
			t.Fatalf("the report does not name %q: %v", key, unapplied)
		}
	}
	// Named, never asked for: the instruction the round is given carries
	// nothing about them.
	for _, key := range unapplied {
		if strings.Contains(plan.Instruction, key) {
			t.Fatalf("an unapplied setting reached the round's instruction: %q", key)
		}
	}
}

// A policy the configuration would refuse opens nothing. The lenient read
// this file does must not be the way a refused policy takes effect.
func TestAPolicyTheConfigurationWouldRefuseHandsNothing(t *testing.T) {
	refused := strings.Replace(handedWorkflowMeans,
		`"runners":["ubuntu-latest"]`, `"runners":["self-hosted"]`, 1)
	config, run, runDir := releasePathFixture(t, refused, true)

	plan, err := detectReleasePathGap(config, run, runDir)
	if err != nil {
		t.Fatalf("detectReleasePathGap: %v", err)
	}
	if len(plan.WorkflowFiles) != 0 {
		t.Fatalf("a refused policy named workflow files: %v", plan.WorkflowFiles)
	}
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
