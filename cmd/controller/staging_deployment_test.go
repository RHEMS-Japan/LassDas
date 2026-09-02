package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/githubapi"
	"automation.internal/ticket-ingress/internal/worker"
)

// A consumer whose staging deploy pushes no digest commit (the second
// destination of the checked-in configuration) is validated against the merge
// itself; the first destination, which has a digest policy, must not accept
// that shape. The live pod refused the first delivery of such a consumer at
// this gate with staging_deployment_result_invalid.
func TestStagingDeploymentValidationFollowsTheConsumerDigestPolicy(t *testing.T) {
	enterRepositoryRoot(t)
	config, err := loadFixedConfig(controllerConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Consumers) < 2 || config.Consumers[0].GitHub.StagingDigestCommit == nil || config.Consumers[1].GitHub.StagingDigestCommit != nil {
		t.Fatal("fixture: the first destination must carry a staging digest policy and the second must not")
	}

	edge := config.Consumers[1]
	edgeBinding := stagingTestBinding(edge, "b")
	edgeDeployment := stagingTestDeployment(edge, edgeBinding)
	if !validStagingDeployment(edgeDeployment, edgeBinding) {
		t.Fatal("a staging deployment sitting on its merge must be valid for a consumer without a digest policy")
	}
	mutations := map[string]func(*githubapi.DeploymentResult){
		"a digest commit claimed":   func(d *githubapi.DeploymentResult) { d.DigestCommitSHA = d.Merge.MergeSHA },
		"the branch past the merge": func(d *githubapi.DeploymentResult) { d.BranchHeadSHA = promotionObjectID("5") },
		"digest paths claimed":      func(d *githubapi.DeploymentResult) { d.DigestPaths = []string{"deploy/stg/kustomization.yaml"} },
	}
	for name, mutate := range mutations {
		mutated := edgeDeployment
		mutate(&mutated)
		if validStagingDeployment(mutated, edgeBinding) {
			t.Errorf("%s: accepted for a consumer without a digest policy", name)
		}
	}

	primary := config.Consumers[0]
	primaryBinding := stagingTestBinding(primary, "c")
	primaryDeployment := stagingTestDeployment(primary, primaryBinding)
	if validStagingDeployment(primaryDeployment, primaryBinding) {
		t.Fatal("a deployment without a digest commit must be refused for a consumer with a digest policy")
	}
	digested := primaryDeployment
	digested.BranchHeadSHA = promotionObjectID("6")
	digested.DigestCommitSHA = promotionObjectID("6")
	digested.DigestPaths = slices.Clone(primary.StagingDigestCommitPolicy().ExactPaths)
	if !validStagingDeployment(digested, primaryBinding) {
		t.Fatal("a deployment on its digest commit must be valid for a consumer with a digest policy")
	}
	digested.BranchHeadSHA = digested.Merge.MergeSHA
	digested.DigestCommitSHA = digested.Merge.MergeSHA
	if validStagingDeployment(digested, primaryBinding) {
		t.Fatal("the merge itself must not pass as the digest commit")
	}
}

func stagingTestBinding(consumer worker.ConsumerConfig, character string) deliveryBinding {
	return deliveryBinding{
		DeliveryID: "delivery_" + strings.Repeat(character, 32), IssueKey: "EX-1", Repository: consumer.Repository,
	}
}

// stagingTestDeployment is a staging observation whose branch sits on the
// deployed merge with no digest commit claimed.
func stagingTestDeployment(consumer worker.ConsumerConfig, binding deliveryBinding) githubapi.DeploymentResult {
	base := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	merge := githubapi.MergeResult{
		PullRequestNumber: 7, BaseBranch: consumer.IntegrationBranch, BaseSHA: promotionObjectID("1"),
		HeadBranch: featureBranch(binding), HeadSHA: promotionObjectID("2"),
		MergeSHA: promotionObjectID("3"), TreeSHA: promotionObjectID("4"),
	}
	workflow := consumer.Contract().StagingWorkflow
	run := githubapi.WorkflowRun{
		ID: 3001, WorkflowID: workflow.ID, Name: workflow.Name, Path: workflow.Path,
		HeadBranch: consumer.IntegrationBranch, HeadSHA: merge.MergeSHA,
		Event: "push", Status: "completed", Conclusion: "success", Attempt: 1,
		CreatedAt: base, UpdatedAt: base.Add(time.Minute),
	}
	return githubapi.DeploymentResult{Merge: merge, WorkflowRuns: []githubapi.WorkflowRun{run}, BranchHeadSHA: merge.MergeSHA}
}
