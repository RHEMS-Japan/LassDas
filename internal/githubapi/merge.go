package githubapi

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"
)

type mergePreview struct {
	SHA     string
	TreeSHA string
}

func (c *Controller) MergeFeaturePullRequest(ctx context.Context, pull PullRequest, checks CheckEvidence, spec MergeSpec, wait WaitOptions) (MergeResult, error) {
	if pull.BaseRef != c.contract.IntegrationBranch || pull.HeadRef == c.contract.IntegrationBranch || pull.HeadRef == c.contract.ReleaseBranch {
		return MergeResult{}, invariant("invalid_feature_pull_request")
	}
	return c.mergePullRequest(ctx, pull, checks, spec, wait, true, nil)
}

func (c *Controller) MergePromotionPullRequest(ctx context.Context, pull PullRequest, checks CheckEvidence, proof PromotionProof, digestPolicy DigestCommitPolicy, spec MergeSpec, wait WaitOptions, recordReflection MergeReflectionRecorder) (MergeResult, error) {
	if pull.BaseRef != c.contract.ReleaseBranch || pull.HeadRef != c.contract.IntegrationBranch {
		return MergeResult{}, invariant("invalid_promotion_pull_request")
	}
	if !emptyCheckEvidence(checks) {
		return MergeResult{}, invariant("unexpected_promotion_checks")
	}
	if recordReflection == nil {
		return MergeResult{}, invariant("missing_merge_reflection_recorder")
	}
	if pull.HeadSHA != proof.Staging.BranchHeadSHA || pull.BaseSHA != proof.Baseline.Release.SHA || !strings.Contains(pull.Body, proof.AcceptanceEvidenceSHA256) {
		return MergeResult{}, invariant("promotion_pull_proof_mismatch")
	}
	if err := c.verifyPromotionProof(ctx, proof, digestPolicy); err != nil {
		return MergeResult{}, err
	}
	return c.mergePullRequest(ctx, pull, CheckEvidence{}, spec, wait, false, recordReflection)
}

func (c *Controller) mergePullRequest(parent context.Context, pull PullRequest, checks CheckEvidence, spec MergeSpec, wait WaitOptions, requireFeatureChecks bool, recordReflection MergeReflectionRecorder) (MergeResult, error) {
	if err := c.client.requireVerified(); err != nil {
		return MergeResult{}, err
	}
	if pull.Number <= 0 || !validObjectID(pull.BaseSHA) || !validObjectID(pull.HeadSHA) {
		return MergeResult{}, invariant("invalid_pull_request")
	}
	expectedJobs := 0
	for _, workflow := range c.contract.FeatureWorkflows {
		expectedJobs += len(workflow.RequiredJobs)
	}
	if requireFeatureChecks {
		if checks.PullRequestNumber != pull.Number || checks.HeadSHA != pull.HeadSHA || len(checks.WorkflowRunIDs) != len(c.contract.FeatureWorkflows) || len(checks.WorkflowJobIDs) != expectedJobs || !positiveUnique(checks.WorkflowRunIDs) || !positiveUnique(checks.WorkflowJobIDs) {
			return MergeResult{}, invariant("missing_check_evidence")
		}
		if err := c.revalidateFeatureEvidence(parent, pull, checks); err != nil {
			return MergeResult{}, err
		}
	}
	if err := validateText(spec.CommitTitle, 256, "invalid_merge_title"); err != nil {
		return MergeResult{}, err
	}
	if len(spec.CommitMessage) > 64*1024 || containsUnsafeText(spec.CommitMessage) {
		return MergeResult{}, invariant("invalid_merge_message")
	}
	ctx, cancel, err := waitContext(parent, wait)
	if err != nil {
		return MergeResult{}, err
	}
	defer cancel()

	preview, err := c.waitMergePreview(ctx, pull, wait.PollInterval)
	if err != nil {
		return MergeResult{}, err
	}
	if err := c.assertPullRefs(ctx, pull); err != nil {
		return MergeResult{}, err
	}
	latestPreview, err := c.readMergePreview(ctx, pull)
	if err != nil {
		return MergeResult{}, err
	}
	if latestPreview.SHA != preview.SHA || latestPreview.TreeSHA != preview.TreeSHA {
		return MergeResult{}, invariant("merge_preview_changed")
	}

	body := map[string]string{
		"commit_title":   spec.CommitTitle,
		"commit_message": spec.CommitMessage,
		"sha":            pull.HeadSHA,
		"merge_method":   "merge",
	}
	var response struct {
		SHA     string `json:"sha"`
		Merged  bool   `json:"merged"`
		Message string `json:"message"`
	}
	endpoint := c.client.repositoryPath("/pulls/" + formatInt(pull.Number) + "/merge")
	if err := c.client.mutate(ctx, http.MethodPut, endpoint, body, &response, http.StatusOK); err != nil {
		if !isAmbiguousMutationError(err) {
			return MergeResult{}, err
		}
		return c.reconcileMergedPullRequest(ctx, err, pull, preview, wait.PollInterval, recordReflection)
	}
	if !response.Merged || !validObjectID(response.SHA) {
		return c.reconcileMergedPullRequest(ctx, invariant("pull_request_not_merged"), pull, preview, wait.PollInterval, recordReflection)
	}
	reflection := reflectedMerge(pull, response.SHA)
	if err := recordMergeReflection(recordReflection, reflection); err != nil {
		return MergeResult{}, err
	}
	result, err := c.verifyExactMerge(ctx, pull, preview, response.SHA, wait.PollInterval)
	if err != nil {
		if IsInvariant(err, "merge_base_advanced") {
			return MergeResult{}, err
		}
		return c.reconcileMergedPullRequest(ctx, err, pull, preview, wait.PollInterval, nil)
	}
	return result, nil
}

func (c *Controller) reconcileMergedPullRequest(ctx context.Context, mutationErr error, pull PullRequest, preview mergePreview, interval time.Duration, recordReflection MergeReflectionRecorder) (MergeResult, error) {
	for attempt := 0; attempt < mutationReconcileAttempts; attempt++ {
		response, err := c.getPullRequest(ctx, pull.Number)
		if err == nil {
			if contractErr := c.matchPullContract(response, pull); contractErr != nil {
				return MergeResult{}, mutationErr
			}
			if response.State == "closed" && response.Merged && validObjectID(response.MergeCommitSHA) {
				reflection := reflectedMerge(pull, response.MergeCommitSHA)
				if err := recordMergeReflection(recordReflection, reflection); err != nil {
					return MergeResult{}, err
				}
				result, verifyErr := c.verifyExactMerge(ctx, pull, preview, response.MergeCommitSHA, interval)
				if verifyErr != nil {
					return MergeResult{}, mutationErr
				}
				return result, nil
			}
			if response.State != "open" || response.Merged {
				return MergeResult{}, mutationErr
			}
		} else if !isTransientReadError(err) && !isStatus(err, http.StatusNotFound) {
			return MergeResult{}, mutationErr
		}
		if attempt+1 == mutationReconcileAttempts || c.client.sleep(ctx, interval) != nil {
			return MergeResult{}, mutationErr
		}
	}
	return MergeResult{}, mutationErr
}

func (c *Controller) verifyExactMerge(ctx context.Context, pull PullRequest, preview mergePreview, mergeSHA string, interval time.Duration) (MergeResult, error) {
	actualCommit, err := c.waitCommit(ctx, mergeSHA, interval)
	if err != nil {
		return MergeResult{}, err
	}
	if !slices.Equal(actualCommit.Parents, []string{pull.BaseSHA, pull.HeadSHA}) || actualCommit.TreeSHA != preview.TreeSHA {
		return MergeResult{}, invariant("merge_toctou_detected")
	}
	if _, err := c.waitMergeBaseRef(ctx, pull.BaseRef, pull.BaseSHA, mergeSHA, interval); err != nil {
		return MergeResult{}, err
	}
	return MergeResult{
		PullRequestNumber: pull.Number,
		BaseBranch:        pull.BaseRef,
		BaseSHA:           pull.BaseSHA,
		HeadBranch:        pull.HeadRef,
		HeadSHA:           pull.HeadSHA,
		MergeSHA:          mergeSHA,
		TreeSHA:           actualCommit.TreeSHA,
	}, nil
}

func reflectedMerge(pull PullRequest, mergeSHA string) MergeReflection {
	return MergeReflection{
		PullRequestNumber: pull.Number,
		BaseBranch:        pull.BaseRef,
		BaseSHA:           pull.BaseSHA,
		HeadBranch:        pull.HeadRef,
		HeadSHA:           pull.HeadSHA,
		MergeSHA:          mergeSHA,
	}
}

func recordMergeReflection(recorder MergeReflectionRecorder, reflection MergeReflection) error {
	if recorder == nil {
		return nil
	}
	if !validMergeReflection(reflection) {
		return invariant("invalid_merge_reflection")
	}
	return recorder(reflection)
}

func validMergeReflection(result MergeReflection) bool {
	return result.PullRequestNumber > 0 && result.BaseBranch != "" && validObjectID(result.BaseSHA) &&
		result.HeadBranch != "" && validObjectID(result.HeadSHA) && validObjectID(result.MergeSHA)
}

func (c *Controller) revalidateFeatureEvidence(ctx context.Context, pull PullRequest, checks CheckEvidence) error {
	if err := c.assertUniqueOpenPull(ctx, pull); err != nil {
		return err
	}
	jobOffset := 0
	for index, workflow := range c.contract.FeatureWorkflows {
		run, err := c.readWorkflowRunByID(ctx, checks.WorkflowRunIDs[index])
		if err != nil {
			return err
		}
		if run.WorkflowID != workflow.ID || run.Name != workflow.Name || run.DisplayTitle != pull.Title || !workflowPathMatches(run.Path, workflow.Path) || run.HeadBranch != pull.HeadRef || run.HeadSHA != pull.HeadSHA || run.Event != "pull_request" || run.Status != "completed" || run.Conclusion != "success" || run.CreatedAt.Before(pull.CreatedAt) {
			return invariant("check_evidence_mismatch")
		}
		jobIDs, ready, err := c.readExactWorkflowJobs(ctx, run.ID, workflow.RequiredJobs)
		if err != nil {
			return err
		}
		if !ready || !slices.Equal(jobIDs, checks.WorkflowJobIDs[jobOffset:jobOffset+len(jobIDs)]) {
			return invariant("check_evidence_mismatch")
		}
		jobOffset += len(jobIDs)
	}
	return nil
}

func emptyCheckEvidence(checks CheckEvidence) bool {
	return checks.PullRequestNumber == 0 && checks.HeadSHA == "" && len(checks.WorkflowRunIDs) == 0 && len(checks.WorkflowJobIDs) == 0 && len(checks.CheckRunIDs) == 0 && len(checks.StatusIDs) == 0
}

func positiveUnique(values []int64) bool {
	seen := make(map[int64]struct{}, len(values))
	for _, value := range values {
		if value <= 0 {
			return false
		}
		if _, exists := seen[value]; exists {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

func (c *Controller) waitMergePreview(ctx context.Context, pull PullRequest, interval time.Duration) (mergePreview, error) {
	for {
		response, err := c.getPullRequest(ctx, pull.Number)
		if err != nil {
			return mergePreview{}, err
		}
		if err := c.matchPullResponse(response, pull); err != nil {
			return mergePreview{}, err
		}
		if response.Mergeable != nil && !*response.Mergeable {
			return mergePreview{}, invariant("pull_request_not_mergeable")
		}
		preview, err := c.readMergePreview(ctx, pull)
		if err == nil {
			return preview, nil
		}
		if !isStatus(err, http.StatusNotFound) {
			return mergePreview{}, err
		}
		if err := sleepOrTimeout(ctx, c.client, interval, "merge_preview_timeout"); err != nil {
			return mergePreview{}, err
		}
	}
}

func (c *Controller) readMergePreview(ctx context.Context, pull PullRequest) (mergePreview, error) {
	var response struct {
		Ref    string `json:"ref"`
		Object struct {
			Type string `json:"type"`
			SHA  string `json:"sha"`
		} `json:"object"`
	}
	endpoint := c.client.repositoryPath("/git/ref/pull/" + formatInt(pull.Number) + "/merge")
	if err := c.client.get(ctx, endpoint, &response); err != nil {
		return mergePreview{}, err
	}
	if response.Ref != "refs/pull/"+formatInt(pull.Number)+"/merge" || response.Object.Type != "commit" || !validObjectID(response.Object.SHA) {
		return mergePreview{}, invariant("invalid_merge_preview_ref")
	}
	commit, err := c.getGitCommit(ctx, response.Object.SHA)
	if err != nil {
		return mergePreview{}, err
	}
	if !slices.Equal(commit.Parents, []string{pull.BaseSHA, pull.HeadSHA}) {
		return mergePreview{}, invariant("merge_preview_parents_changed")
	}
	return mergePreview{SHA: commit.SHA, TreeSHA: commit.TreeSHA}, nil
}

func (c *Controller) assertPullRefs(ctx context.Context, pull PullRequest) error {
	response, err := c.getPullRequest(ctx, pull.Number)
	if err != nil {
		return err
	}
	if err := c.matchPullResponse(response, pull); err != nil {
		return err
	}
	base, err := c.getRef(ctx, pull.BaseRef)
	if err != nil {
		return err
	}
	head, err := c.getRef(ctx, pull.HeadRef)
	if err != nil {
		return err
	}
	if base != pull.BaseSHA || head != pull.HeadSHA {
		return invariant("merge_refs_changed")
	}
	return nil
}

func (c *Controller) matchPullResponse(response pullResponse, expected PullRequest) error {
	if err := c.matchPullContract(response, expected); err != nil {
		return err
	}
	if response.State != "open" || response.Merged {
		return invariant("pull_request_changed")
	}
	return nil
}

func (c *Controller) matchMergedPullResponse(response pullResponse, expected PullRequest) (string, error) {
	if err := c.matchPullContract(response, expected); err != nil {
		return "", err
	}
	if response.State != "closed" || !response.Merged || !validObjectID(response.MergeCommitSHA) {
		return "", invariant("pull_request_not_merged")
	}
	return response.MergeCommitSHA, nil
}

func (c *Controller) matchPullContract(response pullResponse, expected PullRequest) error {
	createdAt, err := time.Parse(time.RFC3339, response.CreatedAt)
	if err != nil || response.Number != expected.Number || response.HTMLURL != expected.HTMLURL || response.HTMLURL != c.pullHTMLURL(response.Number) || response.Title != expected.Title || response.Body != expected.Body || !createdAt.Equal(expected.CreatedAt) || response.Draft || response.Head.Ref != expected.HeadRef || response.Head.SHA != expected.HeadSHA || response.Base.Ref != expected.BaseRef || response.Base.SHA != expected.BaseSHA {
		return invariant("pull_request_changed")
	}
	if !sameRepository(response.Head.Repo.FullName, c.client.config.Owner+"/"+c.client.config.Repository) {
		return invariant("pull_request_repository_changed")
	}
	return nil
}

func (c *Controller) waitCommit(ctx context.Context, sha string, interval time.Duration) (gitCommit, error) {
	for {
		commit, err := c.getGitCommit(ctx, sha)
		if err == nil {
			return commit, nil
		}
		if !isStatus(err, http.StatusNotFound) && !isTransientReadError(err) {
			return gitCommit{}, err
		}
		if err := sleepOrTimeout(ctx, c.client, interval, "merge_commit_timeout"); err != nil {
			return gitCommit{}, err
		}
	}
}

func (c *Controller) waitMergeBaseRef(ctx context.Context, branch, previousSHA, mergeSHA string, interval time.Duration) (string, error) {
	for {
		actual, err := c.getRef(ctx, branch)
		if err == nil {
			switch actual {
			case mergeSHA:
				return actual, nil
			case previousSHA:
				// The merge response can become visible before the branch ref.
				// Wait only while the ref is still at the exact pre-merge SHA.
			default:
				return "", invariant("merge_base_advanced")
			}
		} else if !isStatus(err, http.StatusNotFound) && !isTransientReadError(err) {
			return "", err
		}
		if err := sleepOrTimeout(ctx, c.client, interval, "merge_base_ref_timeout"); err != nil {
			return "", err
		}
	}
}

func containsUnsafeText(value string) bool {
	for _, character := range value {
		if character == '\x00' || character == '\r' {
			return true
		}
	}
	return false
}

func sameRepository(actual, expected string) bool {
	return strings.EqualFold(actual, expected)
}
