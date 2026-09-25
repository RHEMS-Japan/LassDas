package worker

import (
	"strings"
	"testing"
)

// passingWorkflow is what this destination's policy allows: started by
// hand or by a push to the release branch, a bounded token, one pinned
// action, a hosted runner, and the one secret the policy names.
const passingWorkflow = `name: Deploy staging
on:
  workflow_dispatch:
  push:
    branches: [prod]
permissions:
  contents: read
  id-token: write
jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@8f4b7f84864484a7bf31766abe9204da3cbe65b3
      - run: ./ops/deploy.sh
        env:
          ROLE: ${{ secrets.DEPLOY_ROLE_ARN }}
`

// checkWorkflow is the gate as the seal calls it.
func checkWorkflow(t *testing.T, content string) error {
	t.Helper()
	config := configWithMeans(t)
	return CheckDeployWorkflows(
		[]CandidateFile{{Path: plannedWorkflow, Content: content}}, config.Consumers[0])
}

// The destination's own release branch is the only thing a push may be
// filtered to, and a push with no filter at all runs on the branch this
// engine pushes to open its own pull request — with the repository's
// secrets, before anybody has read a line of it.
func TestTheContentGateHoldsTriggersToTheReleaseBranch(t *testing.T) {
	if err := checkWorkflow(t, passingWorkflow); err != nil {
		t.Fatalf("a workflow inside the policy was refused: %v", err)
	}
	for name, content := range map[string]string{
		"a push with no filter":     strings.Replace(passingWorkflow, "  push:\n    branches: [prod]\n", "  push:\n", 1),
		"a push filtered to a list": strings.Replace(passingWorkflow, "on:\n  workflow_dispatch:\n  push:\n    branches: [prod]\n", "on: [push]\n", 1),
		"another branch":            strings.Replace(passingWorkflow, "branches: [prod]", "branches: [feature/x]", 1),
		"every branch":              strings.Replace(passingWorkflow, "branches: [prod]", `branches: ["**"]`, 1),
		"branches-ignore instead":   strings.Replace(passingWorkflow, "branches: [prod]", "branches-ignore: [nothing]", 1),
		"an event nobody allowed":   strings.Replace(passingWorkflow, "  workflow_dispatch:\n", "  issue_comment:\n", 1),
	} {
		if err := checkWorkflow(t, content); err == nil {
			t.Fatalf("%s was sealed", name)
		} else if !strings.Contains(err.Error(), workflowRuleTrigger) &&
			!strings.Contains(err.Error(), workflowRuleShape) {
			t.Fatalf("%s was refused without naming the rule: %v", name, err)
		}
	}
}

// A workflow with no permissions: of its own gets whatever the repository
// defaults to, which this engine neither sets nor can read — so absent is
// "unknown" rather than "nothing", and unknown does not pass a ceiling.
func TestTheContentGateHoldsPermissionsToTheCeiling(t *testing.T) {
	for name, content := range map[string]string{
		"write-all":                strings.Replace(passingWorkflow, "permissions:\n  contents: read\n  id-token: write\n", "permissions: write-all\n", 1),
		"read-all":                 strings.Replace(passingWorkflow, "permissions:\n  contents: read\n  id-token: write\n", "permissions: read-all\n", 1),
		"no permissions at all":    strings.Replace(passingWorkflow, "permissions:\n  contents: read\n  id-token: write\n", "", 1),
		"above the ceiling":        strings.Replace(passingWorkflow, "contents: read", "contents: write", 1),
		"a scope nobody allowed":   strings.Replace(passingWorkflow, "contents: read", "packages: write", 1),
		"above the ceiling in job": strings.Replace(passingWorkflow, "    runs-on: ubuntu-latest", "    permissions:\n      contents: write\n    runs-on: ubuntu-latest", 1),
	} {
		if err := checkWorkflow(t, content); err == nil {
			t.Fatalf("%s was sealed", name)
		} else if !strings.Contains(err.Error(), workflowRulePermissions) {
			t.Fatalf("%s was refused without naming the permissions rule: %v", name, err)
		}
	}
	// Asking for less than the ceiling is ordinary, and asking for nothing
	// is the most bounded request there is.
	if err := checkWorkflow(t, strings.Replace(passingWorkflow,
		"permissions:\n  contents: read\n  id-token: write\n", "permissions: {}\n", 1)); err != nil {
		t.Fatalf("a workflow asking for no permissions at all was refused: %v", err)
	}
}

// The secrets rule reads the parsed expression, not the text. Searching for
// "secrets.NAME" catches what somebody writes by hand and misses every
// other way to reach the same place.
func TestTheContentGateReadsEveryExpressionThatTouchesSecrets(t *testing.T) {
	for name, expression := range map[string]string{
		"a secret nobody allowed":  "${{ secrets.OTHER_TOKEN }}",
		"the whole set as JSON":    "${{ toJSON(secrets) }}",
		"a name built at run time": "${{ secrets[format('{0}', github.event.inputs.which)] }}",
		"indexed by a literal":     "${{ secrets['DEPLOY_ROLE_ARN'] }}",
		"the set passed along":     "${{ secrets }}",
		"the engine's own token":   "${{ secrets.TARGET_GITHUB_TOKEN }}",
	} {
		content := strings.Replace(passingWorkflow, "${{ secrets.DEPLOY_ROLE_ARN }}", expression, 1)
		if err := checkWorkflow(t, content); err == nil {
			t.Fatalf("%s was sealed", name)
		} else if !strings.Contains(err.Error(), workflowRuleSecrets) {
			t.Fatalf("%s was refused without naming the secrets rule: %v", name, err)
		}
	}
	// An env: block one level up is the same reference under another name,
	// and it is read by the same walk.
	inherited := strings.Replace(passingWorkflow,
		"jobs:\n", "env:\n  CARRIED: ${{ toJSON(secrets) }}\njobs:\n", 1)
	if err := checkWorkflow(t, inherited); err == nil {
		t.Fatal("a top-level env: carrying every secret was sealed")
	}
}

// A policy may not name the engine's own credential for this destination,
// and a workflow may not read it even if one did: the token that pushes
// branches and opens pull requests is the token that gets them merged.
func TestThePolicyItselfRefusesTheEnginesOwnToken(t *testing.T) {
	policy := handedPolicy()
	policy.Secrets = []string{"TARGET_GITHUB_TOKEN"}
	if err := ValidateDeployWorkflowPolicy(*policy); err == nil {
		t.Fatal("a policy naming the engine's own token loaded")
	}
	// And the gate refuses the reference even where a policy carrying it
	// somehow reached a run.
	forced := handedPolicy()
	forced.Secrets = append(forced.Secrets, "TARGET_GITHUB_TOKEN")
	consumer := validTestConfig().Consumers[0]
	consumer.Infrastructure = &InfrastructureConfig{Provider: "aws", DeployWorkflows: forced}
	content := strings.Replace(passingWorkflow, "secrets.DEPLOY_ROLE_ARN", "secrets.TARGET_GITHUB_TOKEN", 1)
	if err := CheckDeployWorkflows([]CandidateFile{{Path: plannedWorkflow, Content: content}}, consumer); err == nil {
		t.Fatal("a workflow reading the engine's own token was sealed")
	}
}

// A third-party action sees the job's whole environment, so allowing one is
// allowing its author — at one exact commit and no other.
func TestTheContentGateHoldsActionsToThePinnedAllowList(t *testing.T) {
	for name, uses := range map[string]string{
		"unpinned":       "actions/checkout@v4",
		"another commit": "actions/checkout@0000000000000000000000000000000000000000",
		"another action": "someone/deploy@8f4b7f84864484a7bf31766abe9204da3cbe65b3",
		"an image":       "dock" + "er://alpine:3",
	} {
		content := strings.Replace(passingWorkflow,
			"actions/checkout@8f4b7f84864484a7bf31766abe9204da3cbe65b3", uses, 1)
		if err := checkWorkflow(t, content); err == nil {
			t.Fatalf("%s was sealed", name)
		} else if !strings.Contains(err.Error(), workflowRuleAction) {
			t.Fatalf("%s was refused without naming the action rule: %v", name, err)
		}
	}
	// A policy naming an unpinned action does not load at all.
	policy := handedPolicy()
	policy.Actions = []string{"actions/checkout@v4"}
	if err := ValidateDeployWorkflowPolicy(*policy); err == nil {
		t.Fatal("a policy naming an unpinned action loaded")
	}
}

// A run: step an AI wrote, executing on a machine the destination owns, is
// code execution inside the destination's own infrastructure.
func TestTheContentGateRefusesEveryRunnerThatIsNotHosted(t *testing.T) {
	for name, runsOn := range map[string]string{
		"self-hosted":       "self-hosted",
		"a label of theirs": "deploy-box",
		"a list with one":   "[self-hosted, linux]",
		"a group":           "{ group: theirs }",
		"another hosted":    "windows-latest",
	} {
		content := strings.Replace(passingWorkflow, "runs-on: ubuntu-latest", "runs-on: "+runsOn, 1)
		if err := checkWorkflow(t, content); err == nil {
			t.Fatalf("%s was sealed", name)
		} else if !strings.Contains(err.Error(), workflowRuleRunner) {
			t.Fatalf("%s was refused without naming the runner rule: %v", name, err)
		}
	}
	policy := handedPolicy()
	policy.Runners = []string{"self-hosted"}
	if err := ValidateDeployWorkflowPolicy(*policy); err == nil {
		t.Fatal("a policy allowing a self-hosted runner loaded")
	}
}

// A step that prints the environment has put every secret the job holds
// into a log anybody who can read the repository can read.
func TestTheContentGateRefusesAStepThatPrintsTheEnvironment(t *testing.T) {
	for name, script := range map[string]string{
		"env alone": "env",
		"printenv":  "printenv | sort",
		"set alone": "set",
		"export -p": "export -p > dump.txt",
	} {
		content := strings.Replace(passingWorkflow, "./ops/deploy.sh", script, 1)
		if err := checkWorkflow(t, content); err == nil {
			t.Fatalf("%s was sealed", name)
		} else if !strings.Contains(err.Error(), workflowRuleRunStep) {
			t.Fatalf("%s was refused without naming the run rule: %v", name, err)
		}
	}
	// A block that guards the shell and then prints everything is the
	// same dump on its second line.
	if err := checkWorkflow(t, strings.Replace(passingWorkflow,
		"- run: ./ops/deploy.sh", "- run: |\n          set -euo pipefail\n          env", 1)); err == nil {
		t.Fatal("a multi-line script that prints the environment was sealed")
	}
	// The line half the scripts ever written start with prints nothing.
	if err := checkWorkflow(t, strings.Replace(passingWorkflow,
		"- run: ./ops/deploy.sh", "- run: |\n          set -euo pipefail\n          ./ops/deploy.sh", 1)); err != nil {
		t.Fatalf("an ordinary shell guard was refused: %v", err)
	}
}

// Anything the rules cannot classify is refused rather than skipped: an
// alias is a second name for a node somewhere else, so a document could
// pass every check reading one copy and run another.
func TestTheContentGateRefusesWhatItCannotClassify(t *testing.T) {
	for name, content := range map[string]string{
		"an alias": strings.Replace(passingWorkflow,
			"permissions:\n  contents: read\n  id-token: write\n",
			"permissions: &p\n  contents: read\n  id-token: write\nx-alias: *p\n", 1),
		"a custom tag":       strings.Replace(passingWorkflow, "runs-on: ubuntu-latest", "runs-on: !!python/object ubuntu-latest", 1),
		"a key nobody reads": strings.Replace(passingWorkflow, "jobs:\n", "on-failure: rm -rf /\njobs:\n", 1),
		"a job key nobody reads": strings.Replace(passingWorkflow,
			"    runs-on: ubuntu-latest", "    container: alpine:3\n    runs-on: ubuntu-latest", 1),
		"a step key nobody reads": strings.Replace(passingWorkflow,
			"      - run: ./ops/deploy.sh", "      - run: ./ops/deploy.sh\n        post: whatever", 1),
		"the same key twice": strings.Replace(passingWorkflow, "jobs:\n", "permissions:\n  contents: read\njobs:\n", 1),
		"two documents":      passingWorkflow + "---\nname: second\n",
		"not YAML at all":    "name: [unclosed\n",
		"an unreadable expression": strings.Replace(passingWorkflow,
			"${{ secrets.DEPLOY_ROLE_ARN }}", "${{ secrets.DEPLOY_ROLE_ARN # }}", 1),
	} {
		if err := checkWorkflow(t, content); err == nil {
			t.Fatalf("%s was sealed", name)
		}
	}
}

// A file the policy does not name is refused by the gate as well as by the
// path gates: this function is the last thing between a workflow file and a
// pushed branch, and it does not rely on an earlier check having run.
func TestTheContentGateRefusesAFileNoPolicyNames(t *testing.T) {
	config := configWithMeans(t)
	if err := CheckDeployWorkflows(
		[]CandidateFile{{Path: siblingWorkflow, Content: passingWorkflow}}, config.Consumers[0]); err == nil {
		t.Fatal("a workflow file outside the policy was sealed")
	}
	bare := validTestConfig()
	if err := CheckDeployWorkflows(
		[]CandidateFile{{Path: plannedWorkflow, Content: passingWorkflow}}, bare.Consumers[0]); err == nil {
		t.Fatal("a workflow file was sealed for a destination that handed no means")
	}
	// Files that are not workflow files are not its business.
	if err := CheckDeployWorkflows(
		[]CandidateFile{{Path: "client/src/App.tsx", Content: "anything at all"}}, bare.Consumers[0]); err != nil {
		t.Fatalf("an ordinary file was refused by the workflow gate: %v", err)
	}
}

// A policy with nothing in it is a boolean wearing a policy's clothes: it
// would hand the means and bound nothing.
func TestAnEmptyPolicyDoesNotLoad(t *testing.T) {
	if err := ValidateDeployWorkflowPolicy(DeployWorkflowPolicy{}); err == nil {
		t.Fatal("an empty policy loaded")
	}
	for name, change := range map[string]func(*DeployWorkflowPolicy){
		"no files":       func(p *DeployWorkflowPolicy) { p.Paths = nil },
		"no triggers":    func(p *DeployWorkflowPolicy) { p.Triggers = nil },
		"no runners":     func(p *DeployWorkflowPolicy) { p.Runners = nil },
		"no ceiling":     func(p *DeployWorkflowPolicy) { p.Permissions = nil },
		"a path in name": func(p *DeployWorkflowPolicy) { p.Paths = []string{"nested/deploy.yml"} },
		"not YAML":       func(p *DeployWorkflowPolicy) { p.Paths = []string{"deploy.txt"} },
	} {
		policy := handedPolicy()
		change(policy)
		if err := ValidateDeployWorkflowPolicy(*policy); err == nil {
			t.Fatalf("a policy with %s loaded", name)
		}
	}
}
