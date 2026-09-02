package releaseproof

import (
	"slices"
	"testing"

	"automation.internal/ticket-ingress/internal/githubapi"
)

func TestDeploymentValidatorsUseExactWorkflowSets(t *testing.T) {
	input := stagingFixture(t)
	if _, err := ValidateStagingDeployment(input.StagingDeployment, fixtureConsumer()); err != nil {
		t.Fatal(err)
	}

	staging, err := NewStagingProof(input)
	if err != nil {
		t.Fatal(err)
	}
	_, _, production := productionFixture(staging, sha256Hex([]byte("visible")))
	expectedLatest := production.WorkflowRuns[1].UpdatedAt
	production.WorkflowRuns[0], production.WorkflowRuns[1] = production.WorkflowRuns[1], production.WorkflowRuns[0]
	latest, err := ValidateProductionDeployment(production, fixtureConsumer())
	if err != nil || !latest.Equal(expectedLatest) {
		t.Fatalf("permuted exact set latest = %v; error = %v", latest, err)
	}

	tests := map[string]func(*githubapi.DeploymentResult){
		"unknown workflow": func(value *githubapi.DeploymentResult) { value.WorkflowRuns[0].WorkflowID++ },
		"duplicate workflow": func(value *githubapi.DeploymentResult) {
			value.WorkflowRuns[1].WorkflowID = value.WorkflowRuns[0].WorkflowID
		},
		"duplicate run": func(value *githubapi.DeploymentResult) { value.WorkflowRuns[1].ID = value.WorkflowRuns[0].ID },
		"name":          func(value *githubapi.DeploymentResult) { value.WorkflowRuns[0].Name += " changed" },
		"path":          func(value *githubapi.DeploymentResult) { value.WorkflowRuns[0].Path += "@refs/heads/prod" },
		"branch":        func(value *githubapi.DeploymentResult) { value.WorkflowRuns[0].HeadBranch = "stg" },
		"head":          func(value *githubapi.DeploymentResult) { value.WorkflowRuns[0].HeadSHA = objectID("d") },
		"status":        func(value *githubapi.DeploymentResult) { value.WorkflowRuns[0].Status = "in_progress" },
		"conclusion":    func(value *githubapi.DeploymentResult) { value.WorkflowRuns[0].Conclusion = "failure" },
		"attempt":       func(value *githubapi.DeploymentResult) { value.WorkflowRuns[0].Attempt = 0 },
		"created time": func(value *githubapi.DeploymentResult) {
			value.WorkflowRuns[0].CreatedAt = value.WorkflowRuns[0].UpdatedAt.AddDate(0, 0, 1)
		},
		"updated time": func(value *githubapi.DeploymentResult) {
			value.WorkflowRuns[0].UpdatedAt = value.WorkflowRuns[0].CreatedAt.Add(-1)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			candidate := cloneDeployment(production)
			mutate(&candidate)
			if _, err := ValidateProductionDeployment(candidate, fixtureConsumer()); err == nil {
				t.Fatal("ValidateProductionDeployment() accepted invalid workflow evidence")
			}
		})
	}
}

func TestStagingDeploymentRejectsUnknownDuplicateAndMissingDigestPaths(t *testing.T) {
	base := stagingFixture(t).StagingDeployment
	tests := map[string]func(*githubapi.DeploymentResult){
		"unknown workflow":      func(value *githubapi.DeploymentResult) { value.WorkflowRuns[0].WorkflowID++ },
		"unknown digest path":   func(value *githubapi.DeploymentResult) { value.DigestPaths[0] = "unexpected" },
		"duplicate digest path": func(value *githubapi.DeploymentResult) { value.DigestPaths[1] = value.DigestPaths[0] },
		"missing digest commit": func(value *githubapi.DeploymentResult) { value.DigestCommitSHA = "" },
		"source merge as digest": func(value *githubapi.DeploymentResult) {
			value.BranchHeadSHA = value.Merge.MergeSHA
			value.DigestCommitSHA = value.Merge.MergeSHA
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			candidate := cloneDeployment(base)
			mutate(&candidate)
			if _, err := ValidateStagingDeployment(candidate, fixtureConsumer()); err == nil {
				t.Fatal("ValidateStagingDeployment() accepted invalid evidence")
			}
		})
	}

	permuted := cloneDeployment(base)
	slices.Reverse(permuted.DigestPaths)
	if _, err := ValidateStagingDeployment(permuted, fixtureConsumer()); err != nil {
		t.Fatalf("ValidateStagingDeployment() rejected an exact unordered path set: %v", err)
	}
}

func TestProductionDeploymentRejectsAnyDigestCommit(t *testing.T) {
	input := stagingFixture(t)
	staging, err := NewStagingProof(input)
	if err != nil {
		t.Fatal(err)
	}
	_, _, deployment := productionFixture(staging, sha256Hex([]byte("visible")))
	deployment.DigestCommitSHA = objectID("d")
	deployment.DigestPaths = slices.Clone(fixtureConsumer().StagingDigestCommitPolicy().ExactPaths)
	if _, err := ValidateProductionDeployment(deployment, fixtureConsumer()); err == nil {
		t.Fatal("ValidateProductionDeployment() accepted a production digest commit")
	}
}

// A consumer whose staging deploy pushes no digest commit is validated
// against the merge itself: the branch head is the deployed merge, and no
// digest commit or path is claimed. The first gateway delivery to reach
// staging was refused because the digest equality was demanded of it
// regardless of the consumer having no digest policy.
func TestStagingDeploymentAcceptsAConsumerWithoutADigestPolicy(t *testing.T) {
	consumer := fixtureConsumer()
	consumer.GitHub.StagingDigestCommit = nil
	base := cloneDeployment(stagingFixture(t).StagingDeployment)
	base.BranchHeadSHA = base.Merge.MergeSHA
	base.DigestCommitSHA = ""
	base.DigestPaths = nil
	if _, err := ValidateStagingDeployment(base, consumer); err != nil {
		t.Fatalf("a digest-free consumer's deployment was refused: %v", err)
	}
	tests := map[string]func(*githubapi.DeploymentResult){
		"branch moved past the merge": func(value *githubapi.DeploymentResult) { value.BranchHeadSHA = objectID("e") },
		"digest commit claimed":       func(value *githubapi.DeploymentResult) { value.DigestCommitSHA = value.Merge.MergeSHA },
		"digest paths claimed": func(value *githubapi.DeploymentResult) {
			value.DigestPaths = []string{"k8s/overlays/stg/kustomization.yaml"}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			candidate := cloneDeployment(base)
			mutate(&candidate)
			if _, err := ValidateStagingDeployment(candidate, consumer); err == nil {
				t.Fatal("ValidateStagingDeployment() accepted invalid evidence for a digest-free consumer")
			}
		})
	}
	// The same evidence stays refused for a consumer that does have a policy.
	if _, err := ValidateStagingDeployment(base, fixtureConsumer()); err == nil {
		t.Fatal("a digest-free deployment was accepted under a digest policy")
	}
}
