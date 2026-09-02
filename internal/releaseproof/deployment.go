package releaseproof

import (
	"errors"
	"strings"
	"time"

	"automation.internal/ticket-ingress/internal/githubapi"
	"automation.internal/ticket-ingress/internal/worker"
)

// ValidateStagingDeployment validates the complete fixed staging workflow set
// and returns its latest completion time.
func ValidateStagingDeployment(deployment githubapi.DeploymentResult, consumer worker.ConsumerConfig) (time.Time, error) {
	if !validMerge(deployment.Merge) || deployment.Merge.BaseBranch != consumer.IntegrationBranch ||
		!strings.HasPrefix(deployment.Merge.HeadBranch, "automation/") ||
		deployment.Merge.HeadBranch == consumer.IntegrationBranch || deployment.Merge.HeadBranch == consumer.ReleaseBranch ||
		!validObjectID(deployment.BranchHeadSHA) {
		return time.Time{}, errors.New("staging deployment is invalid")
	}
	policy := consumer.StagingDigestCommitPolicy()
	if !policy.Required {
		// No digest policy: the staging deploy pushes nothing back, so the
		// branch must still sit on the deployed merge and the observation must
		// not claim a digest commit. The first gateway delivery to reach this
		// gate was refused by the digest equality below, which only a consumer
		// with a digest policy can satisfy.
		if deployment.BranchHeadSHA != deployment.Merge.MergeSHA || deployment.DigestCommitSHA != "" ||
			len(deployment.DigestPaths) != 0 {
			return time.Time{}, errors.New("staging deployment is invalid")
		}
	} else if deployment.BranchHeadSHA != deployment.DigestCommitSHA ||
		deployment.DigestCommitSHA == deployment.Merge.MergeSHA ||
		!exactStringSet(deployment.DigestPaths, policy.ExactPaths) {
		return time.Time{}, errors.New("staging deployment is invalid")
	}
	return validateWorkflowSet(
		deployment.WorkflowRuns,
		[]githubapi.WorkflowContract{consumer.Contract().StagingWorkflow},
		consumer.IntegrationBranch,
		deployment.Merge.MergeSHA,
	)
}

// ValidateProductionDeployment validates the complete fixed production
// workflow set and returns its latest completion time.
func ValidateProductionDeployment(deployment githubapi.DeploymentResult, consumer worker.ConsumerConfig) (time.Time, error) {
	if !validMerge(deployment.Merge) || deployment.Merge.BaseBranch != consumer.ReleaseBranch ||
		deployment.Merge.HeadBranch != consumer.IntegrationBranch ||
		deployment.BranchHeadSHA != deployment.Merge.MergeSHA || deployment.DigestCommitSHA != "" ||
		len(deployment.DigestPaths) != 0 {
		return time.Time{}, errors.New("production deployment is invalid")
	}
	return validateWorkflowSet(
		deployment.WorkflowRuns,
		consumer.Contract().ProductionWorkflows,
		consumer.ReleaseBranch,
		deployment.Merge.MergeSHA,
	)
}

func validateWorkflowSet(
	runs []githubapi.WorkflowRun,
	expected []githubapi.WorkflowContract,
	branch string,
	headSHA string,
) (time.Time, error) {
	if len(runs) != len(expected) || !validObjectID(headSHA) {
		return time.Time{}, errors.New("deployment workflow set is invalid")
	}
	byID := make(map[int64]githubapi.WorkflowContract, len(expected))
	for _, workflow := range expected {
		if _, duplicate := byID[workflow.ID]; duplicate {
			return time.Time{}, errors.New("deployment workflow contract is invalid")
		}
		byID[workflow.ID] = workflow
	}
	seenWorkflows := make(map[int64]struct{}, len(runs))
	seenRuns := make(map[int64]struct{}, len(runs))
	latest := time.Time{}
	for _, run := range runs {
		workflow, known := byID[run.WorkflowID]
		if !known {
			return time.Time{}, errors.New("deployment workflow is unknown")
		}
		if _, duplicate := seenWorkflows[run.WorkflowID]; duplicate {
			return time.Time{}, errors.New("deployment workflow is duplicated")
		}
		if run.ID <= 0 {
			return time.Time{}, errors.New("deployment workflow run id is invalid")
		}
		if _, duplicate := seenRuns[run.ID]; duplicate {
			return time.Time{}, errors.New("deployment workflow run is duplicated")
		}
		if run.Name != workflow.Name || run.Path != workflow.Path || run.HeadBranch != branch || run.HeadSHA != headSHA ||
			run.Event != "push" || run.Status != "completed" || run.Conclusion != "success" || run.Attempt <= 0 ||
			run.CreatedAt.IsZero() || run.UpdatedAt.IsZero() || run.CreatedAt.Location() != time.UTC ||
			run.UpdatedAt.Location() != time.UTC || run.UpdatedAt.Before(run.CreatedAt) {
			return time.Time{}, errors.New("deployment workflow evidence is invalid")
		}
		seenWorkflows[run.WorkflowID] = struct{}{}
		seenRuns[run.ID] = struct{}{}
		if latest.IsZero() || run.UpdatedAt.After(latest) {
			latest = run.UpdatedAt
		}
	}
	if len(seenWorkflows) != len(expected) || latest.IsZero() {
		return time.Time{}, errors.New("deployment workflow set is incomplete")
	}
	return latest, nil
}

func canonicalWorkflowOrder(runs []githubapi.WorkflowRun, expected []githubapi.WorkflowContract) bool {
	if len(runs) != len(expected) {
		return false
	}
	for index, workflow := range expected {
		if runs[index].WorkflowID != workflow.ID {
			return false
		}
	}
	return true
}

func exactStringSet(actual, expected []string) bool {
	if len(actual) != len(expected) {
		return false
	}
	want := make(map[string]struct{}, len(expected))
	for _, value := range expected {
		if _, duplicate := want[value]; duplicate {
			return false
		}
		want[value] = struct{}{}
	}
	seen := make(map[string]struct{}, len(actual))
	for _, value := range actual {
		if _, known := want[value]; !known {
			return false
		}
		if _, duplicate := seen[value]; duplicate {
			return false
		}
		seen[value] = struct{}{}
	}
	return len(seen) == len(want)
}
