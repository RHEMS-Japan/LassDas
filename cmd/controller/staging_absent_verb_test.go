package main

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The verb has to tell its caller which of the two happened, because the
// runner keys the requester's wording off the code and nothing else
// survives the process boundary. A merge that landed with no run created
// for it is not a deployment that failed.
func TestAwaitStagingSeparatesAMergeThatStartedNoDeployment(t *testing.T) {
	fixture := newDeliveryFixture(t)
	transport := newDeliveryTransport(fixture)

	featurePath := fixture.output("feature.json")
	if err := run(context.Background(), fixture.publishArguments(featurePath), deliveryEnvironment, transport); err != nil {
		t.Fatalf("publish-feature: %v", err)
	}
	pullPath := fixture.output("feature-pr.json")
	if err := run(context.Background(), fixture.createPullRequestArguments(featurePath, pullPath), deliveryEnvironment, transport); err != nil {
		t.Fatalf("create-feature-pr: %v", err)
	}
	checksPath := fixture.output("feature-checks.json")
	if err := run(context.Background(), fixture.waitArguments(pullPath, checksPath), deliveryEnvironment, transport); err != nil {
		t.Fatalf("wait-feature: %v", err)
	}
	mergePath := fixture.output("feature-merge.json")
	if err := run(context.Background(), fixture.mergeArguments(pullPath, checksPath, mergePath), deliveryEnvironment, transport); err != nil {
		t.Fatalf("merge-feature: %v", err)
	}

	arguments := []string{"await-staging", "--config", controllerConfigPath, "--ticket", fixture.ticketPath}
	arguments = append(arguments, fixture.gateFlags()...)
	arguments = append(arguments, "--baseline", fixture.baselinePath,
		"--feature-merge", mergePath, "--out", fixture.output("staging-proof.json"))

	// The destination's run list stays empty. The parent deadline closes the
	// window in seconds rather than the fifty minutes the verb waits for
	// real; the wait takes the earlier of the two.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := run(ctx, arguments, deliveryEnvironment, transport)
	if failureCode(err) != StagingDeploymentAbsentCode {
		t.Fatalf("failure code = %q (error %v), want %q", failureCode(err), err, StagingDeploymentAbsentCode)
	}

	// And the other direction, which matters more: a deployment that ran
	// and failed must NOT be announced as one that never started. Guarding
	// only the absent case let every failure report as absent with the whole
	// suite green (review of #134).
	failed := newDeliveryTransport(fixture)
	failedFeature := fixture.output("feature-2.json")
	if err := run(context.Background(), fixture.publishArguments(failedFeature), deliveryEnvironment, failed); err != nil {
		t.Fatalf("publish-feature: %v", err)
	}
	failedPull := fixture.output("feature-pr-2.json")
	if err := run(context.Background(), fixture.createPullRequestArguments(failedFeature, failedPull), deliveryEnvironment, failed); err != nil {
		t.Fatalf("create-feature-pr: %v", err)
	}
	failedChecks := fixture.output("feature-checks-2.json")
	if err := run(context.Background(), fixture.waitArguments(failedPull, failedChecks), deliveryEnvironment, failed); err != nil {
		t.Fatalf("wait-feature: %v", err)
	}
	failedMerge := fixture.output("feature-merge-2.json")
	if err := run(context.Background(), fixture.mergeArguments(failedPull, failedChecks, failedMerge), deliveryEnvironment, failed); err != nil {
		t.Fatalf("merge-feature: %v", err)
	}
	workflow := testPrimaryConsumer().Contract().StagingWorkflow
	failed.stagingRuns = []map[string]any{{
		"id": int64(9001), "workflow_id": workflow.ID, "name": workflow.Name,
		"html_url": "https://github.example/run", "head_branch": testPrimaryConsumer().IntegrationBranch,
		"head_sha": deliveryMergeSHA, "event": "push", "status": "completed", "conclusion": "failure",
		"path": workflow.Path, "run_attempt": 1,
		"created_at": "2026-08-03T01:00:00Z", "updated_at": "2026-08-03T01:10:00Z",
	}}
	failedArgs := []string{"await-staging", "--config", controllerConfigPath, "--ticket", fixture.ticketPath}
	failedArgs = append(failedArgs, fixture.gateFlags()...)
	failedArgs = append(failedArgs, "--baseline", fixture.baselinePath,
		"--feature-merge", failedMerge, "--out", fixture.output("staging-proof-2.json"))
	failedCtx, cancelFailed := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelFailed()
	if code := failureCode(run(failedCtx, failedArgs, deliveryEnvironment, failed)); code != "staging_deployment_failed" {
		t.Fatalf("a deployment that ran and failed was reported as %q", code)
	}
}

// The runner reads the verb's failure code off the last line of its stderr.
// Nothing else pins that shape: change main's Fprintln and the whole
// distinction goes dark with every test still green (review of #134).
func TestTheControllerEndsWithItsCodeOnALineOfItsOwn(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "controller")
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the controller: %v\n%s", err, out)
	}
	command := exec.Command(binary, "await-staging")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	_ = command.Run()
	lines := strings.Split(strings.TrimRight(stderr.String(), "\n"), "\n")
	ending := lines[len(lines)-1]
	if !strings.HasPrefix(ending, "controller: ") {
		t.Fatalf("the ending line does not open with the runner's prefix: %q", ending)
	}
	code := strings.TrimPrefix(ending, "controller: ")
	if code == "" || strings.ContainsAny(code, ": ") {
		t.Fatalf("the ending line is not a bare code: %q", ending)
	}
}
