package worker

import (
	"strings"
	"testing"
)

// Every rule about a run: step reads it as a POSIX shell script. A workflow
// that names a different interpreter would be measured in one language and
// executed in another: under defaults.run.shell: python, the check for a
// step that prints the environment is reading Python as if it were bash and
// sees nothing at all.
func TestTheContentGateRefusesAWorkflowThatChangesTheLanguage(t *testing.T) {
	for name, content := range map[string]string{
		"a default shell for the whole file": strings.Replace(passingWorkflow,
			"jobs:\n", "defaults:\n  run:\n    shell: python\njobs:\n", 1),
		"a default shell for one job": strings.Replace(passingWorkflow,
			"    runs-on: ubuntu-latest", "    defaults:\n      run:\n        shell: python\n    runs-on: ubuntu-latest", 1),
		"a step naming its own": strings.Replace(passingWorkflow,
			"      - run: ./ops/deploy.sh", "      - run: ./ops/deploy.sh\n        shell: python", 1),
	} {
		if err := checkWorkflow(t, content); err == nil {
			t.Fatalf("%s was sealed", name)
		}
	}
	// The two shells the rules can actually read are ordinary.
	for _, shell := range []string{"bash", "sh"} {
		content := strings.Replace(passingWorkflow,
			"      - run: ./ops/deploy.sh", "      - run: ./ops/deploy.sh\n        shell: "+shell, 1)
		if err := checkWorkflow(t, content); err != nil {
			t.Fatalf("a step running under %s was refused: %v", shell, err)
		}
	}
}

// An if: takes a bare expression with no ${{ }} around it, so the scan that
// reads those spans never sees one — and asking whether a secret is set is
// a perfectly ordinary thing to write there.
func TestTheContentGateReadsABareConditionToo(t *testing.T) {
	for name, content := range map[string]string{
		"on a step": strings.Replace(passingWorkflow,
			"      - run: ./ops/deploy.sh", "      - run: ./ops/deploy.sh\n        if: secrets.OTHER_TOKEN != ''", 1),
		"on a job": strings.Replace(passingWorkflow,
			"    runs-on: ubuntu-latest", "    if: toJSON(secrets) != ''\n    runs-on: ubuntu-latest", 1),
	} {
		if err := checkWorkflow(t, content); err == nil {
			t.Fatalf("%s was sealed", name)
		} else if !strings.Contains(err.Error(), workflowRuleSecrets) {
			t.Fatalf("%s was refused without naming the secrets rule: %v", name, err)
		}
	}
	// A condition about something other than a secret is ordinary.
	ordinary := strings.Replace(passingWorkflow,
		"    runs-on: ubuntu-latest", "    if: github.ref == 'refs/heads/prod'\n    runs-on: ubuntu-latest", 1)
	if err := checkWorkflow(t, ordinary); err != nil {
		t.Fatalf("an ordinary condition was refused: %v", err)
	}
}

// A commit message, a branch name and a pull request title are written by
// whoever opened them. Pasted into a run: step they stop being text and
// become commands, on the destination's account, with whatever the job's
// token can do.
func TestTheContentGateRefusesSomebodyElsesTextBecomingCommands(t *testing.T) {
	for name, script := range map[string]string{
		"a commit message":     "echo ${{ github.event.head_commit.message }}",
		"a branch name":        "./ops/deploy.sh --ref ${{ github.head_ref }}",
		"a pull request title": "./ops/deploy.sh ${{ github.event.pull_request.title }}",
	} {
		content := strings.Replace(passingWorkflow, "./ops/deploy.sh", script, 1)
		if err := checkWorkflow(t, content); err == nil {
			t.Fatalf("%s was sealed", name)
		} else if !strings.Contains(err.Error(), workflowRuleRunStep) {
			t.Fatalf("%s was refused without naming the run rule: %v", name, err)
		}
	}
	// The same value carried as an environment variable is how it is done.
	carried := strings.Replace(passingWorkflow,
		"          ROLE: ${{ secrets.DEPLOY_ROLE_ARN }}",
		"          ROLE: ${{ secrets.DEPLOY_ROLE_ARN }}\n          REF: ${{ github.head_ref }}", 1)
	if err := checkWorkflow(t, carried); err != nil {
		t.Fatalf("a value carried through env: was refused: %v", err)
	}
}

// A policy may only name the three events whose start this gate can bound.
// Each of the others begins a job from something outside the branch filter
// — a release being published, another workflow finishing.
func TestAPolicyNamesOnlyTheThreeBoundedTriggers(t *testing.T) {
	for _, event := range []string{"workflow_run", "workflow_call", "release", "deployment", "deployment_status", "issue_comment"} {
		policy := handedPolicy()
		policy.Triggers = []string{event}
		if err := ValidateDeployWorkflowPolicy(*policy); err == nil {
			t.Fatalf("a policy allowing %s loaded", event)
		}
	}
	for _, event := range []string{WorkflowTriggerPush, WorkflowTriggerPullRequest, WorkflowTriggerDispatch} {
		policy := handedPolicy()
		policy.Triggers = []string{event}
		if err := ValidateDeployWorkflowPolicy(*policy); err != nil {
			t.Fatalf("a policy allowing %s was refused: %v", event, err)
		}
	}
}

// Two refusals that were right for the wrong reason, and one that was
// simply too wide.
func TestTheContentGateSaysWhatIsActuallyWrong(t *testing.T) {
	// A plan-named file the round deleted is the round taking the release
	// path away, not a file with a syntax error in it.
	err := checkWorkflow(t, "")
	if err == nil {
		t.Fatal("a plan-named workflow that was emptied was sealed")
	}
	if strings.Contains(err.Error(), "YAML として読めません") {
		t.Fatalf("the objection sends the next round looking for a syntax error: %v", err)
	}
	if !strings.Contains(err.Error(), "計画に載っています") {
		t.Fatalf("the objection does not say the plan names this file: %v", err)
	}
	// paths only narrows what branches already settled, so it may sit
	// beside it; anything that could widen may not.
	narrowed := strings.Replace(passingWorkflow,
		"    branches: [prod]", "    branches: [prod]\n    paths: ['ops/**']", 1)
	if err := checkWorkflow(t, narrowed); err != nil {
		t.Fatalf("a push narrowed further by paths was refused: %v", err)
	}
	widened := strings.Replace(passingWorkflow,
		"    branches: [prod]", "    branches: [prod]\n    tags: ['v*']", 1)
	if err := checkWorkflow(t, widened); err == nil {
		t.Fatal("a push that also starts on a tag was sealed")
	}
}

// An expression the gate cannot take apart is refused, which is right — and
// arithmetic is not one of them. A legitimate expression refused costs the
// round that would have fixed nothing.
func TestTheGateCanReadAnOrdinaryArithmeticExpression(t *testing.T) {
	for _, expression := range []string{
		"${{ secrets.DEPLOY_ROLE_ARN }}",
		"${{ github.run_number + 1 }}",
		"${{ (github.run_attempt - 1) * 2 }}",
		"${{ github.run_number % 4 }}",
	} {
		content := strings.Replace(passingWorkflow, "${{ secrets.DEPLOY_ROLE_ARN }}", expression, 1)
		if err := checkWorkflow(t, content); err != nil {
			t.Fatalf("%s was refused: %v", expression, err)
		}
	}
}
