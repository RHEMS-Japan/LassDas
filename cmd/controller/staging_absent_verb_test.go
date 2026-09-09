package main

import (
	"context"
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
}
