package githubapi

import (
	"context"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

var digestLinePattern = regexp.MustCompile(`^digest: sha256:[0-9a-f]{64}$`)

func (c *Controller) WaitForPullRequestChecks(parent context.Context, pull PullRequest, requirements CheckRequirements, wait WaitOptions) (CheckEvidence, error) {
	if err := c.client.requireVerified(); err != nil {
		return CheckEvidence{}, err
	}
	if err := validateCheckRequirements(requirements); err != nil {
		return CheckEvidence{}, err
	}
	ctx, cancel, err := waitContext(parent, wait)
	if err != nil {
		return CheckEvidence{}, err
	}
	defer cancel()
	if err := c.assertPullRefs(ctx, pull); err != nil {
		return CheckEvidence{}, err
	}
	if pull.CreatedAt.IsZero() {
		return CheckEvidence{}, invariant("missing_pull_request_time")
	}
	if err := c.assertUniqueOpenPull(ctx, pull); err != nil {
		return CheckEvidence{}, err
	}
	workflowRunIDs := make([]int64, 0, len(c.contract.FeatureWorkflows))
	workflowJobIDs := make([]int64, 0)
	for _, workflow := range c.contract.FeatureWorkflows {
		runID, jobIDs, err := c.waitPullRequestWorkflow(ctx, pull, workflow, wait.PollInterval)
		if err != nil {
			return CheckEvidence{}, err
		}
		workflowRunIDs = append(workflowRunIDs, runID)
		workflowJobIDs = append(workflowJobIDs, jobIDs...)
	}
	for {
		checkIDs, checkReady, err := c.readRequiredCheckRuns(ctx, pull.HeadSHA, requirements.CheckRuns)
		if err != nil {
			return CheckEvidence{}, err
		}
		statusIDs, statusReady, err := c.readRequiredStatuses(ctx, pull.HeadSHA, requirements.Statuses)
		if err != nil {
			return CheckEvidence{}, err
		}
		if checkReady && statusReady {
			if err := c.assertPullRefs(ctx, pull); err != nil {
				return CheckEvidence{}, err
			}
			if err := c.assertUniqueOpenPull(ctx, pull); err != nil {
				return CheckEvidence{}, err
			}
			return CheckEvidence{
				PullRequestNumber: pull.Number,
				HeadSHA:           pull.HeadSHA,
				WorkflowRunIDs:    workflowRunIDs,
				WorkflowJobIDs:    workflowJobIDs,
				CheckRunIDs:       checkIDs,
				StatusIDs:         statusIDs,
			}, nil
		}
		if err := sleepOrTimeout(ctx, c.client, wait.PollInterval, "checks_timeout"); err != nil {
			return CheckEvidence{}, err
		}
	}
}

func (c *Controller) assertUniqueOpenPull(ctx context.Context, pull PullRequest) error {
	response, err := c.readUniqueOpenPull(ctx, pull.HeadRef, pull.BaseRef)
	if err != nil {
		return err
	}
	return c.matchPullResponse(response, pull)
}

func (c *Controller) readUniqueOpenPull(ctx context.Context, headRef, baseRef string) (pullResponse, error) {
	response, err := c.readOpenPulls(ctx, headRef, baseRef)
	if err != nil {
		return pullResponse{}, err
	}
	if len(response) != 1 || len(response) >= 100 {
		return pullResponse{}, invariant("pull_request_not_unique")
	}
	return response[0], nil
}

func (c *Controller) readOpenPulls(ctx context.Context, headRef, baseRef string) ([]pullResponse, error) {
	query := url.Values{}
	query.Set("base", baseRef)
	query.Set("head", c.client.config.Owner+":"+headRef)
	query.Set("per_page", "100")
	query.Set("state", "open")
	var response []pullResponse
	if err := c.client.get(ctx, c.client.repositoryPath("/pulls")+"?"+query.Encode(), &response); err != nil {
		return nil, err
	}
	return response, nil
}

func validateCheckRequirements(requirements CheckRequirements) error {
	if len(requirements.CheckRuns) > 32 || len(requirements.Statuses) > 32 {
		return invariant("invalid_check_requirements")
	}
	seenRuns := make(map[string]struct{}, len(requirements.CheckRuns))
	for _, required := range requirements.CheckRuns {
		if err := validateText(required.Name, 256, "invalid_check_run"); err != nil || len(required.AppSlug) > 100 || strings.ContainsAny(required.AppSlug, "\x00\r\n") {
			return invariant("invalid_check_run")
		}
		key := required.Name + "\x00" + required.AppSlug
		if _, exists := seenRuns[key]; exists {
			return invariant("duplicate_check_run")
		}
		seenRuns[key] = struct{}{}
	}
	seenStatuses := make(map[string]struct{}, len(requirements.Statuses))
	for _, required := range requirements.Statuses {
		if err := validateText(required, 256, "invalid_status_context"); err != nil {
			return err
		}
		if _, exists := seenStatuses[required]; exists {
			return invariant("duplicate_status_context")
		}
		seenStatuses[required] = struct{}{}
	}
	return nil
}

func (c *Controller) waitPullRequestWorkflow(ctx context.Context, pull PullRequest, workflow WorkflowContract, interval time.Duration) (int64, []int64, error) {
	for {
		var response struct {
			TotalCount   int `json:"total_count"`
			WorkflowRuns []struct {
				ID             int64  `json:"id"`
				WorkflowID     int64  `json:"workflow_id"`
				Name           string `json:"name"`
				HeadBranch     string `json:"head_branch"`
				HeadSHA        string `json:"head_sha"`
				Event          string `json:"event"`
				Status         string `json:"status"`
				Conclusion     string `json:"conclusion"`
				Path           string `json:"path"`
				RunAttempt     int    `json:"run_attempt"`
				CreatedAt      string `json:"created_at"`
				DisplayTitle   string `json:"display_title"`
				HeadRepository struct {
					FullName string `json:"full_name"`
				} `json:"head_repository"`
				Repository struct {
					FullName string `json:"full_name"`
				} `json:"repository"`
				Pulls []struct {
					Number int64 `json:"number"`
					Head   struct {
						Ref string `json:"ref"`
						SHA string `json:"sha"`
					} `json:"head"`
					Base struct {
						Ref string `json:"ref"`
						SHA string `json:"sha"`
					} `json:"base"`
				} `json:"pull_requests"`
			} `json:"workflow_runs"`
		}
		query := url.Values{}
		query.Set("branch", pull.HeadRef)
		query.Set("event", "pull_request")
		query.Set("head_sha", pull.HeadSHA)
		query.Set("per_page", "100")
		endpoint := c.client.repositoryPath("/actions/workflows/"+formatInt(workflow.ID)+"/runs") + "?" + query.Encode()
		if err := c.client.get(ctx, endpoint, &response); err != nil {
			return 0, nil, err
		}
		if response.TotalCount != len(response.WorkflowRuns) || len(response.WorkflowRuns) > 100 {
			return 0, nil, invariant("feature_workflow_runs_truncated")
		}
		matches := make([]int, 0, 1)
		for index, run := range response.WorkflowRuns {
			createdAt, createdErr := time.Parse(time.RFC3339, run.CreatedAt)
			expectedRepository := c.client.config.Owner + "/" + c.client.config.Repository
			if createdErr != nil || createdAt.Before(pull.CreatedAt) || run.ID <= 0 || run.WorkflowID != workflow.ID || run.Name != workflow.Name || run.DisplayTitle != pull.Title || run.HeadBranch != pull.HeadRef || run.HeadSHA != pull.HeadSHA || run.Event != "pull_request" || !workflowPathMatches(run.Path, workflow.Path) || !sameRepository(run.HeadRepository.FullName, expectedRepository) || !sameRepository(run.Repository.FullName, expectedRepository) {
				continue
			}
			if len(run.Pulls) > 0 {
				associated := false
				for _, linked := range run.Pulls {
					if linked.Number == pull.Number && linked.Head.Ref == pull.HeadRef && linked.Head.SHA == pull.HeadSHA && linked.Base.Ref == pull.BaseRef && linked.Base.SHA == pull.BaseSHA {
						associated = true
						break
					}
				}
				if !associated {
					continue
				}
			}
			matches = append(matches, index)
		}
		if len(matches) > 1 {
			return 0, nil, invariant("ambiguous_feature_workflow_run")
		}
		if len(matches) == 1 {
			run := response.WorkflowRuns[matches[0]]
			if run.RunAttempt <= 0 {
				return 0, nil, invariant("invalid_feature_workflow_run")
			}
			switch run.Status {
			case "queued", "in_progress", "pending", "requested", "waiting":
			case "completed":
				if run.Conclusion != "success" {
					return 0, nil, invariant("feature_workflow_failed")
				}
				jobIDs, ready, err := c.readExactWorkflowJobs(ctx, run.ID, workflow.RequiredJobs)
				if err != nil {
					return 0, nil, err
				}
				if ready {
					return run.ID, jobIDs, nil
				}
			default:
				return 0, nil, invariant("unknown_feature_workflow_status")
			}
		}
		if err := sleepOrTimeout(ctx, c.client, interval, "feature_workflow_timeout"); err != nil {
			return 0, nil, err
		}
	}
}

func (c *Controller) readExactWorkflowJobs(ctx context.Context, runID int64, expected []string) ([]int64, bool, error) {
	var response struct {
		TotalCount int `json:"total_count"`
		Jobs       []struct {
			ID         int64  `json:"id"`
			RunID      int64  `json:"run_id"`
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"jobs"`
	}
	endpoint := c.client.repositoryPath("/actions/runs/"+formatInt(runID)+"/jobs") + "?filter=latest&per_page=100"
	if err := c.client.get(ctx, endpoint, &response); err != nil {
		return nil, false, err
	}
	if response.TotalCount != len(response.Jobs) || len(response.Jobs) != len(expected) {
		return nil, false, invariant("workflow_job_set_mismatch")
	}
	expectedSet := make(map[string]struct{}, len(expected))
	for _, name := range expected {
		expectedSet[name] = struct{}{}
	}
	ids := make([]int64, 0, len(expected))
	for _, job := range response.Jobs {
		if job.ID <= 0 || job.RunID != runID {
			return nil, false, invariant("invalid_workflow_job")
		}
		if _, exists := expectedSet[job.Name]; !exists {
			return nil, false, invariant("workflow_job_set_mismatch")
		}
		delete(expectedSet, job.Name)
		switch job.Status {
		case "queued", "in_progress", "pending", "requested", "waiting":
			return nil, false, nil
		case "completed":
			if job.Conclusion != "success" {
				return nil, false, invariant("required_workflow_job_failed")
			}
			ids = append(ids, job.ID)
		default:
			return nil, false, invariant("unknown_workflow_job_status")
		}
	}
	if len(expectedSet) != 0 {
		return nil, false, invariant("workflow_job_set_mismatch")
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, true, nil
}

func workflowPathMatches(actual, expected string) bool {
	return actual == expected || strings.HasPrefix(actual, expected+"@")
}

func (c *Controller) readWorkflowRunByID(ctx context.Context, runID int64) (WorkflowRun, error) {
	if runID <= 0 {
		return WorkflowRun{}, invariant("invalid_workflow_run_id")
	}
	var response struct {
		ID           int64  `json:"id"`
		WorkflowID   int64  `json:"workflow_id"`
		Name         string `json:"name"`
		DisplayTitle string `json:"display_title"`
		HTMLURL      string `json:"html_url"`
		HeadBranch   string `json:"head_branch"`
		HeadSHA      string `json:"head_sha"`
		Event        string `json:"event"`
		Status       string `json:"status"`
		Conclusion   string `json:"conclusion"`
		Path         string `json:"path"`
		RunAttempt   int    `json:"run_attempt"`
		CreatedAt    string `json:"created_at"`
		UpdatedAt    string `json:"updated_at"`
		Repository   struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		HeadRepository struct {
			FullName string `json:"full_name"`
		} `json:"head_repository"`
	}
	if err := c.client.get(ctx, c.client.repositoryPath("/actions/runs/"+formatInt(runID)), &response); err != nil {
		return WorkflowRun{}, err
	}
	createdAt, createdErr := time.Parse(time.RFC3339, response.CreatedAt)
	updatedAt, updatedErr := time.Parse(time.RFC3339, response.UpdatedAt)
	expectedRepository := c.client.config.Owner + "/" + c.client.config.Repository
	if response.ID != runID || response.WorkflowID <= 0 || response.RunAttempt <= 0 || !validObjectID(response.HeadSHA) || createdErr != nil || updatedErr != nil || updatedAt.Before(createdAt) || !sameRepository(response.Repository.FullName, expectedRepository) || !sameRepository(response.HeadRepository.FullName, expectedRepository) {
		return WorkflowRun{}, invariant("invalid_workflow_run")
	}
	return WorkflowRun{
		ID: response.ID, WorkflowID: response.WorkflowID, Name: response.Name, DisplayTitle: response.DisplayTitle,
		HTMLURL: response.HTMLURL, HeadBranch: response.HeadBranch, HeadSHA: response.HeadSHA,
		Event: response.Event, Status: response.Status, Conclusion: response.Conclusion, Path: response.Path,
		Attempt: response.RunAttempt, CreatedAt: createdAt, UpdatedAt: updatedAt,
		RepositoryFullName: response.Repository.FullName, HeadRepositoryFullName: response.HeadRepository.FullName,
	}, nil
}

func (c *Controller) readRequiredCheckRuns(ctx context.Context, sha string, required []RequiredCheckRun) ([]int64, bool, error) {
	if len(required) == 0 {
		return nil, true, nil
	}
	var response struct {
		TotalCount int `json:"total_count"`
		CheckRuns  []struct {
			ID         int64  `json:"id"`
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			App        struct {
				Slug string `json:"slug"`
			} `json:"app"`
		} `json:"check_runs"`
	}
	endpoint := c.client.repositoryPath("/commits/"+sha+"/check-runs") + "?filter=latest&per_page=100"
	if err := c.client.get(ctx, endpoint, &response); err != nil {
		return nil, false, err
	}
	if response.TotalCount != len(response.CheckRuns) || len(response.CheckRuns) > 100 {
		return nil, false, invariant("check_runs_truncated")
	}
	ids := make([]int64, 0, len(required))
	ready := true
	for _, expected := range required {
		matches := make([]int, 0, 1)
		for index, actual := range response.CheckRuns {
			if actual.Name == expected.Name && (expected.AppSlug == "" || actual.App.Slug == expected.AppSlug) {
				matches = append(matches, index)
			}
		}
		if len(matches) == 0 {
			ready = false
			continue
		}
		if len(matches) != 1 {
			return nil, false, invariant("ambiguous_check_run")
		}
		actual := response.CheckRuns[matches[0]]
		if actual.ID <= 0 {
			return nil, false, invariant("invalid_check_run_response")
		}
		switch actual.Status {
		case "queued", "in_progress", "pending", "requested", "waiting":
			ready = false
		case "completed":
			if actual.Conclusion != "success" {
				return nil, false, invariant("required_check_run_failed")
			}
			ids = append(ids, actual.ID)
		default:
			return nil, false, invariant("unknown_check_run_status")
		}
	}
	return ids, ready, nil
}

func (c *Controller) readRequiredStatuses(ctx context.Context, sha string, required []string) ([]int64, bool, error) {
	if len(required) == 0 {
		return nil, true, nil
	}
	var response struct {
		TotalCount int `json:"total_count"`
		Statuses   []struct {
			ID      int64  `json:"id"`
			Context string `json:"context"`
			State   string `json:"state"`
		} `json:"statuses"`
	}
	endpoint := c.client.repositoryPath("/commits/"+sha+"/status") + "?per_page=100"
	if err := c.client.get(ctx, endpoint, &response); err != nil {
		return nil, false, err
	}
	if response.TotalCount != len(response.Statuses) || len(response.Statuses) > 100 {
		return nil, false, invariant("statuses_truncated")
	}
	ids := make([]int64, 0, len(required))
	ready := true
	for _, expected := range required {
		var latest *struct {
			ID      int64  `json:"id"`
			Context string `json:"context"`
			State   string `json:"state"`
		}
		for index := range response.Statuses {
			if response.Statuses[index].Context == expected {
				latest = &response.Statuses[index]
				break
			}
		}
		if latest == nil {
			ready = false
			continue
		}
		if latest.ID <= 0 {
			return nil, false, invariant("invalid_status_response")
		}
		switch latest.State {
		case "pending":
			ready = false
		case "success":
			ids = append(ids, latest.ID)
		case "error", "failure":
			return nil, false, invariant("required_status_failed")
		default:
			return nil, false, invariant("unknown_status_state")
		}
	}
	return ids, ready, nil
}

func (c *Controller) AwaitStaging(parent context.Context, merge MergeResult, wait WaitOptions, digestPolicy DigestCommitPolicy) (DeploymentResult, error) {
	if err := c.client.requireVerified(); err != nil {
		return DeploymentResult{}, err
	}
	if merge.BaseBranch != c.contract.IntegrationBranch {
		return DeploymentResult{}, invariant("invalid_staging_merge")
	}
	ctx, cancel, err := waitContext(parent, wait)
	if err != nil {
		return DeploymentResult{}, err
	}
	defer cancel()
	if err := c.verifyMergeResult(ctx, merge); err != nil {
		return DeploymentResult{}, err
	}
	run, err := c.waitWorkflowRun(ctx, c.contract.StagingWorkflow, c.contract.IntegrationBranch, merge.MergeSHA, wait)
	if err != nil {
		return DeploymentResult{}, err
	}
	branchHead, digestSHA, err := c.waitDigestCommit(ctx, c.contract.IntegrationBranch, merge.MergeSHA, digestPolicy, wait)
	if err != nil {
		return DeploymentResult{}, err
	}
	return DeploymentResult{Merge: merge, WorkflowRuns: []WorkflowRun{run}, BranchHeadSHA: branchHead, DigestCommitSHA: digestSHA, DigestPaths: slices.Clone(digestPolicy.ExactPaths)}, nil
}

func (c *Controller) AwaitProduction(parent context.Context, merge MergeResult, wait WaitOptions, digestPolicy DigestCommitPolicy) (DeploymentResult, error) {
	if err := c.client.requireVerified(); err != nil {
		return DeploymentResult{}, err
	}
	if merge.BaseBranch != c.contract.ReleaseBranch || merge.HeadBranch != c.contract.IntegrationBranch {
		return DeploymentResult{}, invariant("invalid_production_merge")
	}
	ctx, cancel, err := waitContext(parent, wait)
	if err != nil {
		return DeploymentResult{}, err
	}
	defer cancel()
	if err := c.verifyMergeResult(ctx, merge); err != nil {
		return DeploymentResult{}, err
	}
	runs := make([]WorkflowRun, 0, len(c.contract.ProductionWorkflows))
	for _, workflow := range c.contract.ProductionWorkflows {
		run, err := c.waitWorkflowRun(ctx, workflow, c.contract.ReleaseBranch, merge.MergeSHA, wait)
		if err != nil {
			return DeploymentResult{}, err
		}
		runs = append(runs, run)
	}
	branchHead, digestSHA, err := c.waitDigestCommit(ctx, c.contract.ReleaseBranch, merge.MergeSHA, digestPolicy, wait)
	if err != nil {
		return DeploymentResult{}, err
	}
	return DeploymentResult{Merge: merge, WorkflowRuns: runs, BranchHeadSHA: branchHead, DigestCommitSHA: digestSHA, DigestPaths: slices.Clone(digestPolicy.ExactPaths)}, nil
}

func (c *Controller) verifyMergeResult(ctx context.Context, merge MergeResult) error {
	if !validObjectID(merge.BaseSHA) || !validObjectID(merge.HeadSHA) || !validObjectID(merge.MergeSHA) || !validObjectID(merge.TreeSHA) {
		return invariant("invalid_merge_result")
	}
	commit, err := c.getGitCommit(ctx, merge.MergeSHA)
	if err != nil {
		return err
	}
	if !slices.Equal(commit.Parents, []string{merge.BaseSHA, merge.HeadSHA}) || commit.TreeSHA != merge.TreeSHA {
		return invariant("merge_result_changed")
	}
	return nil
}

func (c *Controller) waitWorkflowRun(parent context.Context, workflow WorkflowContract, branch, sha string, wait WaitOptions) (WorkflowRun, error) {
	ctx, cancel, err := waitContext(parent, wait)
	if err != nil {
		return WorkflowRun{}, err
	}
	defer cancel()
	for {
		var response struct {
			TotalCount   int `json:"total_count"`
			WorkflowRuns []struct {
				ID         int64  `json:"id"`
				WorkflowID int64  `json:"workflow_id"`
				Name       string `json:"name"`
				HTMLURL    string `json:"html_url"`
				HeadBranch string `json:"head_branch"`
				HeadSHA    string `json:"head_sha"`
				Event      string `json:"event"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
				Path       string `json:"path"`
				RunAttempt int    `json:"run_attempt"`
				CreatedAt  string `json:"created_at"`
				UpdatedAt  string `json:"updated_at"`
			} `json:"workflow_runs"`
		}
		query := url.Values{}
		query.Set("branch", branch)
		query.Set("event", "push")
		query.Set("head_sha", sha)
		query.Set("per_page", "100")
		endpoint := c.client.repositoryPath("/actions/workflows/"+formatInt(workflow.ID)+"/runs") + "?" + query.Encode()
		if err := c.client.get(ctx, endpoint, &response); err != nil {
			return WorkflowRun{}, err
		}
		if response.TotalCount != len(response.WorkflowRuns) || len(response.WorkflowRuns) > 100 {
			return WorkflowRun{}, invariant("workflow_runs_truncated")
		}
		matches := make([]int, 0, 1)
		for index, run := range response.WorkflowRuns {
			if run.WorkflowID == workflow.ID && run.Name == workflow.Name && run.HeadBranch == branch && run.HeadSHA == sha && run.Event == "push" && workflowPathMatches(run.Path, workflow.Path) {
				matches = append(matches, index)
			}
		}
		if len(matches) > 1 {
			return WorkflowRun{}, invariant("ambiguous_workflow_run")
		}
		if len(matches) == 1 {
			actual := response.WorkflowRuns[matches[0]]
			createdAt, createdErr := time.Parse(time.RFC3339, actual.CreatedAt)
			updatedAt, updatedErr := time.Parse(time.RFC3339, actual.UpdatedAt)
			if actual.ID <= 0 || actual.RunAttempt <= 0 || createdErr != nil || updatedErr != nil || updatedAt.Before(createdAt) {
				return WorkflowRun{}, invariant("invalid_workflow_run")
			}
			run := WorkflowRun{
				ID: actual.ID, WorkflowID: actual.WorkflowID, Name: actual.Name, HTMLURL: actual.HTMLURL, HeadBranch: actual.HeadBranch, HeadSHA: actual.HeadSHA,
				Event: actual.Event, Status: actual.Status, Conclusion: actual.Conclusion, Path: actual.Path,
				Attempt: actual.RunAttempt, CreatedAt: createdAt, UpdatedAt: updatedAt,
			}
			switch run.Status {
			case "queued", "in_progress", "pending", "requested", "waiting":
			case "completed":
				if run.Conclusion != "success" {
					return WorkflowRun{}, invariant("deployment_workflow_failed")
				}
				return run, nil
			default:
				return WorkflowRun{}, invariant("unknown_workflow_status")
			}
		}
		if err := sleepOrTimeout(ctx, c.client, wait.PollInterval, "workflow_run_timeout"); err != nil {
			return WorkflowRun{}, err
		}
	}
}

func (c *Controller) waitDigestCommit(parent context.Context, branch, sourceSHA string, policy DigestCommitPolicy, wait WaitOptions) (string, string, error) {
	if err := validateDigestPolicy(policy); err != nil {
		return "", "", err
	}
	ctx, cancel, err := waitContext(parent, wait)
	if err != nil {
		return "", "", err
	}
	defer cancel()
	for {
		headSHA, err := c.getRef(ctx, branch)
		if err != nil {
			return "", "", err
		}
		if headSHA == sourceSHA {
			if !policy.Required {
				return headSHA, "", nil
			}
			if err := sleepOrTimeout(ctx, c.client, wait.PollInterval, "digest_commit_timeout"); err != nil {
				return "", "", err
			}
			continue
		}
		var response struct {
			SHA    string `json:"sha"`
			Commit struct {
				Message string `json:"message"`
			} `json:"commit"`
			Author *struct {
				Login string `json:"login"`
			} `json:"author"`
			Committer *struct {
				Login string `json:"login"`
			} `json:"committer"`
			Parents []struct {
				SHA string `json:"sha"`
			} `json:"parents"`
		}
		if err := c.client.get(ctx, c.client.repositoryPath("/commits/"+headSHA), &response); err != nil {
			return "", "", err
		}
		if response.SHA != headSHA || len(response.Parents) != 1 || response.Parents[0].SHA != sourceSHA || response.Commit.Message != policy.ExactMessagePrefix+sourceSHA {
			return "", "", invariant("unexpected_branch_advance")
		}
		if policy.ActorLogin != "" && (response.Author == nil || response.Committer == nil || response.Author.Login != policy.ActorLogin || response.Committer.Login != policy.ActorLogin) {
			return "", "", invariant("digest_commit_actor_mismatch")
		}
		expectedPaths := slices.Clone(policy.ExactPaths)
		sort.Strings(expectedPaths)
		if err := c.verifyDigestChangedPaths(ctx, sourceSHA, headSHA, expectedPaths, policy.RequireDigestOnly); err != nil {
			return "", "", invariant("digest_commit_diff_mismatch")
		}
		return headSHA, headSHA, nil
	}
}

func (c *Controller) verifyDigestChangedPaths(ctx context.Context, sourceSHA, headSHA string, expected []string, digestOnly bool) error {
	comparison, err := c.readComparison(ctx, sourceSHA, headSHA)
	if err != nil {
		return err
	}
	if comparison.Status != "ahead" || comparison.AheadBy != 1 || comparison.BehindBy != 0 || comparison.TotalCommits != 1 || comparison.BaseSHA != sourceSHA || comparison.MergeBaseSHA != sourceSHA || len(comparison.Files) != len(expected) {
		return invariant("digest_comparison_mismatch")
	}
	actual := make([]string, 0, len(comparison.Files))
	for _, file := range comparison.Files {
		if file.Status != "modified" {
			return invariant("digest_comparison_mismatch")
		}
		if digestOnly && !patchChangesOnlyDigests(file.Patch) {
			return invariant("digest_patch_mismatch")
		}
		actual = append(actual, file.Filename)
	}
	sort.Strings(actual)
	if !slices.Equal(actual, expected) {
		return invariant("digest_comparison_mismatch")
	}
	return nil
}

func patchChangesOnlyDigests(patch string) bool {
	additions, deletions := 0, 0
	for _, line := range strings.Split(patch, "\n") {
		if len(line) == 0 || (line[0] != '+' && line[0] != '-') || strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---") {
			continue
		}
		if line[0] == '+' {
			additions++
		} else {
			deletions++
		}
		if !digestLinePattern.MatchString(strings.TrimSpace(line[1:])) {
			return false
		}
	}
	return additions > 0 && additions == deletions
}

func validateDigestPolicy(policy DigestCommitPolicy) error {
	if !policy.Required && !policy.RequireDigestOnly && policy.ExactMessagePrefix == "" && len(policy.ExactPaths) == 0 && policy.ActorLogin == "" {
		return nil
	}
	if policy.Required && !policy.RequireDigestOnly {
		return invariant("digest_only_proof_required")
	}
	if policy.Required && policy.ActorLogin == "" {
		return invariant("digest_actor_proof_required")
	}
	if err := validateText(policy.ExactMessagePrefix, 256, "invalid_digest_message_prefix"); err != nil || strings.Contains(policy.ExactMessagePrefix, "\n") {
		return invariant("invalid_digest_message_prefix")
	}
	if len(policy.ExactPaths) == 0 || len(policy.ExactPaths) > 16 {
		return invariant("invalid_digest_paths")
	}
	seen := make(map[string]struct{}, len(policy.ExactPaths))
	for _, filename := range policy.ExactPaths {
		if err := validateRepositoryPath(filename, false); err != nil {
			return invariant("invalid_digest_paths")
		}
		if _, exists := seen[filename]; exists {
			return invariant("invalid_digest_paths")
		}
		seen[filename] = struct{}{}
	}
	if len(policy.ActorLogin) > 100 || strings.ContainsAny(policy.ActorLogin, "\x00\r\n") {
		return invariant("invalid_digest_actor")
	}
	return nil
}
