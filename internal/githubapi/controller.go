package githubapi

import (
	"context"
	"encoding/base64"
	"net/http"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type Controller struct {
	client          *Client
	contract        Contract
	promotesChanges bool
}

const (
	mutationReconcileAttempts     = 5
	mutationReconcilePollInterval = time.Second
)

func NewController(client *Client, contract Contract) (*Controller, error) {
	if client == nil {
		return nil, invariant("nil_client")
	}
	if err := contract.validate(); err != nil {
		return nil, err
	}
	return &Controller{client: client, contract: contract, promotesChanges: true}, nil
}

// NewProposalController verifies only what a run that proposes a pull request
// relies on. Such a run never merges, and its read-scoped token is not shown
// the merge settings at all.
func NewProposalController(client *Client, contract Contract) (*Controller, error) {
	controller, err := NewController(client, contract)
	if err != nil {
		return nil, err
	}
	controller.promotesChanges = false
	return controller, nil
}

func (c *Controller) Verify(ctx context.Context) (VerifiedRepository, error) {
	// The merge settings decide how an automated merge behaves, so they are
	// verified when this run may merge. A read-scoped token is not shown them
	// at all (measured 2026-08-06: the fields are absent from the response),
	// so a run that only proposes a pull request must not demand them — and a
	// run that merges must fail loudly when they are invisible rather than
	// compare absent fields as false.
	var repositoryResponse struct {
		ID                        int64   `json:"id"`
		FullName                  string  `json:"full_name"`
		DefaultBranch             string  `json:"default_branch"`
		Archived                  bool    `json:"archived"`
		Disabled                  bool    `json:"disabled"`
		AllowMergeCommit          *bool   `json:"allow_merge_commit"`
		AllowSquashMerge          *bool   `json:"allow_squash_merge"`
		AllowRebaseMerge          *bool   `json:"allow_rebase_merge"`
		AllowAutoMerge            *bool   `json:"allow_auto_merge"`
		AllowUpdateBranch         *bool   `json:"allow_update_branch"`
		DeleteBranchOnMerge       *bool   `json:"delete_branch_on_merge"`
		UseSquashPRTitleAsDefault *bool   `json:"use_squash_pr_title_as_default"`
		SquashMergeCommitTitle    *string `json:"squash_merge_commit_title"`
		SquashMergeCommitMessage  *string `json:"squash_merge_commit_message"`
		MergeCommitTitle          *string `json:"merge_commit_title"`
		MergeCommitMessage        *string `json:"merge_commit_message"`
		WebCommitSignoffRequired  *bool   `json:"web_commit_signoff_required"`
	}
	if err := c.client.get(ctx, c.client.repositoryPath(""), &repositoryResponse); err != nil {
		return VerifiedRepository{}, err
	}
	expectedFullName := c.client.config.Owner + "/" + c.client.config.Repository
	if repositoryResponse.ID != c.client.config.RepositoryID || !strings.EqualFold(repositoryResponse.FullName, expectedFullName) {
		return VerifiedRepository{}, invariant("repository_identity_mismatch")
	}
	if repositoryResponse.Archived || repositoryResponse.Disabled {
		return VerifiedRepository{}, invariant("repository_not_active")
	}
	if repositoryResponse.DefaultBranch != c.contract.DefaultBranch {
		return VerifiedRepository{}, invariant("repository_settings_mismatch")
	}
	observed := observedMergeSettings{
		AllowMergeCommit: repositoryResponse.AllowMergeCommit, AllowSquashMerge: repositoryResponse.AllowSquashMerge,
		AllowRebaseMerge: repositoryResponse.AllowRebaseMerge, AllowAutoMerge: repositoryResponse.AllowAutoMerge,
		AllowUpdateBranch: repositoryResponse.AllowUpdateBranch, DeleteBranchOnMerge: repositoryResponse.DeleteBranchOnMerge,
		UseSquashPRTitleAsDefault: repositoryResponse.UseSquashPRTitleAsDefault,
		SquashMergeCommitTitle:    repositoryResponse.SquashMergeCommitTitle,
		SquashMergeCommitMessage:  repositoryResponse.SquashMergeCommitMessage,
		MergeCommitTitle:          repositoryResponse.MergeCommitTitle,
		MergeCommitMessage:        repositoryResponse.MergeCommitMessage,
		WebCommitSignoffRequired:  repositoryResponse.WebCommitSignoffRequired,
	}
	if c.promotesChanges {
		if err := observed.verifyAgainst(c.contract.MergeSettings); err != nil {
			return VerifiedRepository{}, err
		}
	}
	features := make([]Workflow, 0, len(c.contract.FeatureWorkflows))
	for _, expected := range c.contract.FeatureWorkflows {
		actual, err := c.verifyWorkflow(ctx, expected)
		if err != nil {
			return VerifiedRepository{}, err
		}
		features = append(features, actual)
	}
	staging, err := c.verifyWorkflow(ctx, c.contract.StagingWorkflow)
	if err != nil {
		return VerifiedRepository{}, err
	}
	production := make([]Workflow, 0, len(c.contract.ProductionWorkflows))
	for _, expected := range c.contract.ProductionWorkflows {
		actual, err := c.verifyWorkflow(ctx, expected)
		if err != nil {
			return VerifiedRepository{}, err
		}
		production = append(production, actual)
	}
	c.client.markVerified()
	return VerifiedRepository{
		Repository: Repository{
			ID: repositoryResponse.ID, FullName: repositoryResponse.FullName, DefaultBranch: repositoryResponse.DefaultBranch,
			Archived: repositoryResponse.Archived, Disabled: repositoryResponse.Disabled,
			AllowMergeCommit: boolOrFalse(repositoryResponse.AllowMergeCommit), AllowSquashMerge: boolOrFalse(repositoryResponse.AllowSquashMerge),
			AllowRebaseMerge: boolOrFalse(repositoryResponse.AllowRebaseMerge), AllowAutoMerge: boolOrFalse(repositoryResponse.AllowAutoMerge),
			AllowUpdateBranch: boolOrFalse(repositoryResponse.AllowUpdateBranch), DeleteBranchOnMerge: boolOrFalse(repositoryResponse.DeleteBranchOnMerge),
			UseSquashPRTitleAsDefault: boolOrFalse(repositoryResponse.UseSquashPRTitleAsDefault),
			SquashMergeCommitTitle:    stringOrEmpty(repositoryResponse.SquashMergeCommitTitle), SquashMergeCommitMessage: stringOrEmpty(repositoryResponse.SquashMergeCommitMessage),
			MergeCommitTitle: stringOrEmpty(repositoryResponse.MergeCommitTitle), MergeCommitMessage: stringOrEmpty(repositoryResponse.MergeCommitMessage),
			WebCommitSignoffRequired: boolOrFalse(repositoryResponse.WebCommitSignoffRequired),
		},
		FeatureWorkflows: features, StagingWorkflow: staging, ProductionWorkflows: production,
	}, nil
}

func (c *Controller) verifyWorkflow(ctx context.Context, expected WorkflowContract) (Workflow, error) {
	var response struct {
		ID    int64  `json:"id"`
		Name  string `json:"name"`
		Path  string `json:"path"`
		State string `json:"state"`
	}
	endpoint := c.client.repositoryPath("/actions/workflows/" + formatInt(expected.ID))
	if err := c.client.get(ctx, endpoint, &response); err != nil {
		return Workflow{}, err
	}
	if response.ID != expected.ID || response.Name != expected.Name || response.Path != expected.Path || response.State != expected.State {
		return Workflow{}, invariant("deployment_workflow_mismatch")
	}
	return Workflow{ID: response.ID, Name: response.Name, Path: response.Path, State: response.State}, nil
}

func (c *Controller) VerifyBaseline(ctx context.Context) (Baseline, error) {
	if err := c.client.requireVerified(); err != nil {
		return Baseline{}, err
	}
	integration, err := c.snapshotBranch(ctx, c.contract.IntegrationBranch)
	if err != nil {
		return Baseline{}, err
	}
	release, err := c.snapshotBranch(ctx, c.contract.ReleaseBranch)
	if err != nil {
		return Baseline{}, err
	}
	// A run that promotes work relies on staging holding nothing unpromoted:
	// its merges land on a baseline where staging and release agree. A run
	// that only proposes a pull request relies on neither — staging routinely
	// runs ahead of release between promotion batches (measured 2026-08-07 on
	// the second live ticket: three unpromoted commits, which is this
	// repository's normal state), so demanding equality there would refuse
	// almost every proposal for no protection in return.
	if c.promotesChanges && integration.TreeSHA != release.TreeSHA {
		return Baseline{}, invariant("baseline_trees_differ")
	}
	comparison, err := c.readComparison(ctx, release.SHA, integration.SHA)
	if err != nil {
		return Baseline{}, err
	}
	if comparison.BaseSHA != release.SHA || comparison.BaseTreeSHA != release.TreeSHA || !validObjectID(comparison.MergeBaseSHA) || !validObjectID(comparison.MergeBaseTreeSHA) {
		return Baseline{}, invariant("baseline_compare_mismatch")
	}
	if c.promotesChanges && len(comparison.Files) != 0 {
		return Baseline{}, invariant("baseline_compare_mismatch")
	}
	mergeBase, err := c.getGitCommit(ctx, comparison.MergeBaseSHA)
	if err != nil {
		return Baseline{}, err
	}
	if mergeBase.TreeSHA != comparison.MergeBaseTreeSHA {
		return Baseline{}, invariant("baseline_merge_base_mismatch")
	}
	return Baseline{
		Integration: integration, Release: release,
		MergeBaseSHA: comparison.MergeBaseSHA, MergeBaseTreeSHA: comparison.MergeBaseTreeSHA,
	}, nil
}

type comparison struct {
	Status           string
	AheadBy          int
	BehindBy         int
	TotalCommits     int
	BaseSHA          string
	BaseTreeSHA      string
	MergeBaseSHA     string
	MergeBaseTreeSHA string
	Files            []comparisonFile
}

type comparisonFile struct {
	Filename string `json:"filename"`
	Status   string `json:"status"`
	Patch    string `json:"patch"`
}

func (c *Controller) readComparison(ctx context.Context, baseSHA, headSHA string) (comparison, error) {
	var response struct {
		Status       string `json:"status"`
		AheadBy      int    `json:"ahead_by"`
		BehindBy     int    `json:"behind_by"`
		TotalCommits int    `json:"total_commits"`
		BaseCommit   struct {
			SHA    string `json:"sha"`
			Commit struct {
				Tree struct {
					SHA string `json:"sha"`
				} `json:"tree"`
			} `json:"commit"`
		} `json:"base_commit"`
		MergeBaseCommit struct {
			SHA    string `json:"sha"`
			Commit struct {
				Tree struct {
					SHA string `json:"sha"`
				} `json:"tree"`
			} `json:"commit"`
		} `json:"merge_base_commit"`
		Files []struct {
			Filename string `json:"filename"`
			Status   string `json:"status"`
			Patch    string `json:"patch"`
		} `json:"files"`
	}
	endpoint := c.client.repositoryPath("/compare/" + baseSHA + "..." + headSHA)
	if err := c.client.get(ctx, endpoint, &response); err != nil {
		return comparison{}, err
	}
	if response.Status != "ahead" && response.Status != "behind" && response.Status != "diverged" && response.Status != "identical" {
		return comparison{}, invariant("invalid_compare_response")
	}
	result := comparison{
		Status: response.Status, AheadBy: response.AheadBy, BehindBy: response.BehindBy, TotalCommits: response.TotalCommits,
		BaseSHA: response.BaseCommit.SHA, BaseTreeSHA: response.BaseCommit.Commit.Tree.SHA,
		MergeBaseSHA: response.MergeBaseCommit.SHA, MergeBaseTreeSHA: response.MergeBaseCommit.Commit.Tree.SHA,
		Files: make([]comparisonFile, len(response.Files)),
	}
	for index, file := range response.Files {
		result.Files[index] = comparisonFile{Filename: file.Filename, Status: file.Status, Patch: file.Patch}
	}
	return result, nil
}

func (c *Controller) SnapshotIntegration(ctx context.Context) (Snapshot, error) {
	return c.snapshotBranch(ctx, c.contract.IntegrationBranch)
}

func (c *Controller) SnapshotRelease(ctx context.Context) (Snapshot, error) {
	return c.snapshotBranch(ctx, c.contract.ReleaseBranch)
}

func (c *Controller) snapshotBranch(ctx context.Context, branch string) (Snapshot, error) {
	if err := c.client.requireVerified(); err != nil {
		return Snapshot{}, err
	}
	sha, err := c.getRef(ctx, branch)
	if err != nil {
		return Snapshot{}, err
	}
	commit, err := c.getGitCommit(ctx, sha)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Branch: branch, SHA: sha, TreeSHA: commit.TreeSHA}, nil
}

func (c *Controller) getRef(ctx context.Context, branch string) (string, error) {
	var response struct {
		Ref    string `json:"ref"`
		Object struct {
			Type string `json:"type"`
			SHA  string `json:"sha"`
		} `json:"object"`
	}
	endpoint := c.client.repositoryPath("/git/ref/heads/" + escapePath(branch))
	if err := c.client.get(ctx, endpoint, &response); err != nil {
		return "", err
	}
	if response.Ref != "refs/heads/"+branch || response.Object.Type != "commit" || !validObjectID(response.Object.SHA) {
		return "", invariant("invalid_ref_response")
	}
	return response.Object.SHA, nil
}

type gitCommit struct {
	SHA     string
	TreeSHA string
	Parents []string
}

func (c *Controller) getGitCommit(ctx context.Context, sha string) (gitCommit, error) {
	if !validObjectID(sha) {
		return gitCommit{}, invariant("invalid_commit_sha")
	}
	var response struct {
		SHA  string `json:"sha"`
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
		Parents []struct {
			SHA string `json:"sha"`
		} `json:"parents"`
	}
	if err := c.client.get(ctx, c.client.repositoryPath("/git/commits/"+sha), &response); err != nil {
		return gitCommit{}, err
	}
	if response.SHA != sha || !validObjectID(response.Tree.SHA) {
		return gitCommit{}, invariant("invalid_commit_response")
	}
	commit := gitCommit{SHA: response.SHA, TreeSHA: response.Tree.SHA, Parents: make([]string, len(response.Parents))}
	for index, parent := range response.Parents {
		if !validObjectID(parent.SHA) {
			return gitCommit{}, invariant("invalid_commit_response")
		}
		commit.Parents[index] = parent.SHA
	}
	return commit, nil
}

func (c *Controller) PublishFeature(ctx context.Context, baseline Baseline, spec FeatureSpec) (PublishedFeature, error) {
	if err := c.client.requireVerified(); err != nil {
		return PublishedFeature{}, err
	}
	if err := c.validateBaseline(baseline); err != nil {
		return PublishedFeature{}, err
	}
	base := baseline.Integration
	paths, err := validateFeatureSpec(spec, c.contract)
	if err != nil {
		return PublishedFeature{}, err
	}
	// The release pin matters where release is a parent of the work: a
	// promotion merges onto it. A proposal's artifact carries nothing from
	// the release branch, and the integration pin below guards its base on
	// its own — release moving mid-run (another ticket promoted) does not
	// change what this proposal says, so pinning it here would kill the
	// run for no protection, the same shape as the tree-equality demand.
	if c.promotesChanges {
		currentRelease, err := c.getRef(ctx, c.contract.ReleaseBranch)
		if err != nil {
			return PublishedFeature{}, err
		}
		if currentRelease != baseline.Release.SHA {
			return PublishedFeature{}, invariant("release_base_changed")
		}
	}
	current, err := c.snapshotBranch(ctx, c.contract.IntegrationBranch)
	if err != nil {
		return PublishedFeature{}, err
	}
	if current.SHA != base.SHA || current.TreeSHA != base.TreeSHA {
		return PublishedFeature{}, invariant("integration_base_changed")
	}

	baseEntries, err := c.getBaseTreeEntries(ctx, base.TreeSHA)
	if err != nil {
		return PublishedFeature{}, err
	}
	treeEntries := make([]map[string]string, 0, len(spec.Files))
	for _, file := range spec.Files {
		metadata, exists := baseEntries[file.Path]
		if file.Created {
			// A created file's precondition is the mirror image: the base
			// must not carry the path.
			if exists {
				return PublishedFeature{}, invariant("created_file_already_exists")
			}
		} else if !exists || metadata.Type != "blob" || metadata.Mode != "100644" || metadata.SHA != file.ExpectedBlobSHA {
			return PublishedFeature{}, invariant("source_blob_changed")
		}
		var blobResponse struct {
			SHA string `json:"sha"`
		}
		body := map[string]string{
			"content":  base64.StdEncoding.EncodeToString(file.Content),
			"encoding": "base64",
		}
		if err := c.client.mutate(ctx, http.MethodPost, c.client.repositoryPath("/git/blobs"), body, &blobResponse, http.StatusCreated); err != nil {
			return PublishedFeature{}, err
		}
		if !validObjectID(blobResponse.SHA) {
			return PublishedFeature{}, invariant("invalid_blob_response")
		}
		treeEntries = append(treeEntries, map[string]string{
			"path": file.Path,
			"mode": "100644",
			"type": "blob",
			"sha":  blobResponse.SHA,
		})
	}

	var treeResponse struct {
		SHA string `json:"sha"`
	}
	treeBody := struct {
		BaseTree string              `json:"base_tree"`
		Tree     []map[string]string `json:"tree"`
	}{BaseTree: base.TreeSHA, Tree: treeEntries}
	if err := c.client.mutate(ctx, http.MethodPost, c.client.repositoryPath("/git/trees"), treeBody, &treeResponse, http.StatusCreated); err != nil {
		return PublishedFeature{}, err
	}
	if !validObjectID(treeResponse.SHA) || treeResponse.SHA == base.TreeSHA {
		return PublishedFeature{}, invariant("invalid_candidate_tree")
	}

	var commitResponse struct {
		SHA string `json:"sha"`
	}
	commitBody := struct {
		Message string   `json:"message"`
		Tree    string   `json:"tree"`
		Parents []string `json:"parents"`
	}{Message: spec.CommitMessage, Tree: treeResponse.SHA, Parents: []string{base.SHA}}
	if err := c.client.mutate(ctx, http.MethodPost, c.client.repositoryPath("/git/commits"), commitBody, &commitResponse, http.StatusCreated); err != nil {
		return PublishedFeature{}, err
	}
	if !validObjectID(commitResponse.SHA) {
		return PublishedFeature{}, invariant("invalid_candidate_commit")
	}
	createdCommit, err := c.getGitCommit(ctx, commitResponse.SHA)
	if err != nil {
		return PublishedFeature{}, err
	}
	if createdCommit.TreeSHA != treeResponse.SHA || !slices.Equal(createdCommit.Parents, []string{base.SHA}) {
		return PublishedFeature{}, invariant("candidate_commit_mismatch")
	}
	if err := c.verifyChangedPaths(ctx, base.SHA, commitResponse.SHA, paths); err != nil {
		return PublishedFeature{}, err
	}
	currentSHA, err := c.getRef(ctx, c.contract.IntegrationBranch)
	if err != nil {
		return PublishedFeature{}, err
	}
	if currentSHA != base.SHA {
		return PublishedFeature{}, invariant("integration_base_changed")
	}
	if c.promotesChanges {
		currentReleaseSHA, err := c.getRef(ctx, c.contract.ReleaseBranch)
		if err != nil {
			return PublishedFeature{}, err
		}
		if currentReleaseSHA != baseline.Release.SHA {
			return PublishedFeature{}, invariant("release_base_changed")
		}
	}
	if err := c.createExactBranchRef(ctx, spec.Branch, commitResponse.SHA); err != nil {
		return PublishedFeature{}, err
	}
	return PublishedFeature{
		Base:    base,
		Branch:  spec.Branch,
		HeadSHA: commitResponse.SHA,
		TreeSHA: treeResponse.SHA,
		Paths:   paths,
	}, nil
}

func (c *Controller) createExactBranchRef(ctx context.Context, branch, sha string) error {
	refBody := map[string]string{"ref": "refs/heads/" + branch, "sha": sha}
	var refResponse struct {
		Ref    string `json:"ref"`
		Object struct {
			Type string `json:"type"`
			SHA  string `json:"sha"`
		} `json:"object"`
	}
	if err := c.client.mutate(ctx, http.MethodPost, c.client.repositoryPath("/git/refs"), refBody, &refResponse, http.StatusCreated); err != nil {
		if !isAmbiguousMutationError(err) && !isExistingResourceMutationError(err) {
			return err
		}
		return c.reconcileExactBranchRef(ctx, branch, sha, err)
	}
	if refResponse.Ref != "refs/heads/"+branch || refResponse.Object.Type != "commit" || refResponse.Object.SHA != sha {
		return c.reconcileExactBranchRef(ctx, branch, sha, invariant("feature_ref_mismatch"))
	}
	return nil
}

func (c *Controller) reconcileExactBranchRef(ctx context.Context, branch, sha string, ambiguityErr error) error {
	for attempt := 0; attempt < mutationReconcileAttempts; attempt++ {
		actual, err := c.getRef(ctx, branch)
		if err == nil {
			if actual == sha {
				return nil
			}
			return ambiguityErr
		}
		if !isTransientReadError(err) && !isStatus(err, http.StatusNotFound) {
			return ambiguityErr
		}
		if attempt+1 == mutationReconcileAttempts || c.client.sleep(ctx, mutationReconcilePollInterval) != nil {
			return ambiguityErr
		}
	}
	return ambiguityErr
}

type treeEntry struct {
	Type string
	Mode string
	SHA  string
}

func (c *Controller) getBaseTreeEntries(ctx context.Context, treeSHA string) (map[string]treeEntry, error) {
	var response struct {
		SHA       string `json:"sha"`
		Truncated bool   `json:"truncated"`
		Tree      []struct {
			Path string `json:"path"`
			Mode string `json:"mode"`
			Type string `json:"type"`
			SHA  string `json:"sha"`
		} `json:"tree"`
	}
	endpoint := c.client.repositoryPath("/git/trees/"+treeSHA) + "?recursive=1"
	if err := c.client.get(ctx, endpoint, &response); err != nil {
		return nil, err
	}
	if response.SHA != treeSHA || response.Truncated {
		return nil, invariant("base_tree_unavailable")
	}
	entries := make(map[string]treeEntry, len(response.Tree))
	for _, item := range response.Tree {
		if _, exists := entries[item.Path]; exists || !validObjectID(item.SHA) {
			return nil, invariant("invalid_base_tree")
		}
		entries[item.Path] = treeEntry{Type: item.Type, Mode: item.Mode, SHA: item.SHA}
	}
	return entries, nil
}

func (c *Controller) validateBaseline(baseline Baseline) error {
	if baseline.Integration.Branch != c.contract.IntegrationBranch || baseline.Release.Branch != c.contract.ReleaseBranch ||
		!validObjectID(baseline.Integration.SHA) || !validObjectID(baseline.Release.SHA) ||
		!validObjectID(baseline.Integration.TreeSHA) || !validObjectID(baseline.Release.TreeSHA) ||
		!validObjectID(baseline.MergeBaseSHA) || !validObjectID(baseline.MergeBaseTreeSHA) {
		return invariant("invalid_baseline")
	}
	// The same condition VerifyBaseline stamps the baseline under: a run
	// that promotes lands its merges where staging and release agree, but a
	// run that only proposes a pull request tolerates staging running ahead
	// between promotion batches. Demanding equality here for a proposal
	// rejected the very baseline this engine had issued — every proposal
	// after an unpromoted merge died at publish, ten minutes after the
	// mismatch was knowable (measured 2026-08-13/14 on two consecutive live proposals).
	if c.promotesChanges && baseline.Integration.TreeSHA != baseline.Release.TreeSHA {
		return invariant("invalid_baseline")
	}
	return nil
}

func (c *Controller) verifyChangedPaths(ctx context.Context, baseSHA, headSHA string, expected []string) error {
	var response struct {
		Status       string `json:"status"`
		AheadBy      int    `json:"ahead_by"`
		BehindBy     int    `json:"behind_by"`
		TotalCommits int    `json:"total_commits"`
		Files        []struct {
			Filename string `json:"filename"`
			Status   string `json:"status"`
		} `json:"files"`
	}
	endpoint := c.client.repositoryPath("/compare/" + baseSHA + "..." + headSHA)
	if err := c.client.get(ctx, endpoint, &response); err != nil {
		return err
	}
	if response.Status != "ahead" || response.AheadBy != 1 || response.BehindBy != 0 || response.TotalCommits != 1 || len(response.Files) != len(expected) {
		return invariant("candidate_diff_mismatch")
	}
	actual := make([]string, 0, len(response.Files))
	for _, file := range response.Files {
		// A candidate carries full file contents, so its commit can modify
		// an existing file or add a new one — nothing else. "added" was
		// missing here until RFDEV-622: the first live delivery to create
		// files (a SQL migration and three new modules) passed both reviews
		// and the deterministic validation, then died at this line.
		if file.Status != "modified" && file.Status != "added" {
			return invariant("candidate_diff_mismatch")
		}
		actual = append(actual, file.Filename)
	}
	sort.Strings(actual)
	if !slices.Equal(actual, expected) {
		return invariant("candidate_diff_mismatch")
	}
	return nil
}

func validateFeatureSpec(spec FeatureSpec, contract Contract) ([]string, error) {
	if err := validateBranch(spec.Branch); err != nil || spec.Branch == contract.IntegrationBranch || spec.Branch == contract.ReleaseBranch {
		return nil, invariant("invalid_feature_branch")
	}
	if err := validateText(spec.CommitMessage, 512, "invalid_commit_message"); err != nil {
		return nil, err
	}
	if len(spec.Files) == 0 || len(spec.Files) > 32 || len(spec.AllowedPathPrefixes) == 0 || len(spec.AllowedPathPrefixes) > 16 {
		return nil, invariant("invalid_feature_file_set")
	}
	for _, prefix := range spec.AllowedPathPrefixes {
		if err := validateRepositoryPath(prefix, true); err != nil {
			return nil, invariant("invalid_allowed_path_prefix")
		}
	}
	paths := make([]string, 0, len(spec.Files))
	seen := make(map[string]struct{}, len(spec.Files))
	totalBytes := 0
	for _, file := range spec.Files {
		if err := validateRepositoryPath(file.Path, false); err != nil || !validObjectID(file.ExpectedBlobSHA) {
			return nil, invariant("invalid_feature_file")
		}
		allowed := false
		for _, prefix := range spec.AllowedPathPrefixes {
			if strings.HasPrefix(file.Path, prefix) {
				allowed = true
				break
			}
		}
		if !allowed {
			return nil, invariant("feature_path_not_allowed")
		}
		if len(file.Content) > 1024*1024 {
			return nil, invariant("feature_file_too_large")
		}
		if !utf8.Valid(file.Content) || slices.Contains(file.Content, byte(0)) {
			return nil, invariant("feature_file_not_text")
		}
		totalBytes += len(file.Content)
		if totalBytes > 4*1024*1024 {
			return nil, invariant("feature_set_too_large")
		}
		if _, exists := seen[file.Path]; exists {
			return nil, invariant("duplicate_feature_path")
		}
		seen[file.Path] = struct{}{}
		paths = append(paths, file.Path)
	}
	sort.Strings(paths)
	return paths, nil
}

func validateRepositoryPath(value string, directoryPrefix bool) error {
	if value == "" || len(value) > 512 || !utf8.ValidString(value) || strings.ContainsAny(value, "\x00\r\n\\") || strings.HasPrefix(value, "/") {
		return invariant("invalid_repository_path")
	}
	if directoryPrefix != strings.HasSuffix(value, "/") {
		return invariant("invalid_repository_path")
	}
	cleaned := path.Clean(strings.TrimSuffix(value, "/"))
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || cleaned != strings.TrimSuffix(value, "/") {
		return invariant("invalid_repository_path")
	}
	return nil
}

func (c *Controller) CreateFeaturePullRequest(ctx context.Context, feature PublishedFeature, spec PullRequestSpec) (PullRequest, error) {
	if feature.Base.Branch != c.contract.IntegrationBranch || feature.Base.SHA == "" || feature.HeadSHA == "" {
		return PullRequest{}, invariant("invalid_published_feature")
	}
	if err := c.validatePullRequestSpec(spec); err != nil {
		return PullRequest{}, err
	}
	currentBase, err := c.getRef(ctx, c.contract.IntegrationBranch)
	if err != nil {
		return PullRequest{}, err
	}
	currentHead, err := c.getRef(ctx, feature.Branch)
	if err != nil {
		return PullRequest{}, err
	}
	if currentBase != feature.Base.SHA || currentHead != feature.HeadSHA {
		return PullRequest{}, invariant("feature_pr_refs_changed")
	}
	body := map[string]any{
		"title": spec.Title,
		"body":  spec.Body,
		"head":  feature.Branch,
		"base":  c.contract.IntegrationBranch,
		"draft": false,
	}
	var response pullResponse
	if err := c.client.mutate(ctx, http.MethodPost, c.client.repositoryPath("/pulls"), body, &response, http.StatusCreated); err != nil {
		if !isAmbiguousMutationError(err) && !isExistingResourceMutationError(err) {
			return PullRequest{}, err
		}
		return c.reconcileCreatedPullRequest(ctx, err, spec.Title, spec.Body, feature.Branch, feature.HeadSHA, c.contract.IntegrationBranch, feature.Base.SHA)
	}
	pull, err := c.validatePullResponse(response, spec.Title, spec.Body, feature.Branch, feature.HeadSHA, c.contract.IntegrationBranch, feature.Base.SHA)
	if err != nil {
		return c.reconcileCreatedPullRequest(ctx, err, spec.Title, spec.Body, feature.Branch, feature.HeadSHA, c.contract.IntegrationBranch, feature.Base.SHA)
	}
	return pull, nil
}

func (c *Controller) CreatePromotionPullRequest(ctx context.Context, proof PromotionProof, spec PullRequestSpec) (PullRequest, error) {
	if err := c.client.requireVerified(); err != nil {
		return PullRequest{}, err
	}
	if err := c.validatePullRequestSpec(spec); err != nil {
		return PullRequest{}, err
	}
	if !strings.Contains(spec.Body, proof.AcceptanceEvidenceSHA256) {
		return PullRequest{}, invariant("promotion_pr_missing_acceptance_evidence")
	}
	if err := c.verifyPromotionProof(ctx, proof); err != nil {
		return PullRequest{}, err
	}
	body := map[string]any{
		"title": spec.Title,
		"body":  spec.Body,
		"head":  c.contract.IntegrationBranch,
		"base":  c.contract.ReleaseBranch,
		"draft": false,
	}
	var response pullResponse
	if err := c.client.mutate(ctx, http.MethodPost, c.client.repositoryPath("/pulls"), body, &response, http.StatusCreated); err != nil {
		if !isAmbiguousMutationError(err) && !isExistingResourceMutationError(err) {
			return PullRequest{}, err
		}
		return c.reconcileCreatedPullRequest(ctx, err, spec.Title, spec.Body, c.contract.IntegrationBranch, proof.Staging.BranchHeadSHA, c.contract.ReleaseBranch, proof.Baseline.Release.SHA)
	}
	pull, err := c.validatePullResponse(response, spec.Title, spec.Body, c.contract.IntegrationBranch, proof.Staging.BranchHeadSHA, c.contract.ReleaseBranch, proof.Baseline.Release.SHA)
	if err != nil {
		return c.reconcileCreatedPullRequest(ctx, err, spec.Title, spec.Body, c.contract.IntegrationBranch, proof.Staging.BranchHeadSHA, c.contract.ReleaseBranch, proof.Baseline.Release.SHA)
	}
	return pull, nil
}

func (c *Controller) reconcileCreatedPullRequest(ctx context.Context, mutationErr error, title, body, headRef, headSHA, baseRef, baseSHA string) (PullRequest, error) {
	for attempt := 0; attempt < mutationReconcileAttempts; attempt++ {
		responses, err := c.readOpenPulls(ctx, headRef, baseRef)
		if err == nil {
			switch len(responses) {
			case 0:
				// GitHub can make an accepted create visible just after the response is lost.
			case 1:
				pull, validationErr := c.validatePullResponse(responses[0], title, body, headRef, headSHA, baseRef, baseSHA)
				if validationErr != nil {
					return PullRequest{}, mutationErr
				}
				return pull, nil
			default:
				return PullRequest{}, mutationErr
			}
		} else if !isTransientReadError(err) {
			return PullRequest{}, mutationErr
		}
		if attempt+1 == mutationReconcileAttempts || c.client.sleep(ctx, mutationReconcilePollInterval) != nil {
			return PullRequest{}, mutationErr
		}
	}
	return PullRequest{}, mutationErr
}

func (c *Controller) verifyPromotionProof(ctx context.Context, proof PromotionProof) error {
	productPaths, promotionPaths, err := c.validatePromotionProof(proof)
	if err != nil {
		return err
	}
	currentIntegration, err := c.getRef(ctx, c.contract.IntegrationBranch)
	if err != nil {
		return err
	}
	currentRelease, err := c.getRef(ctx, c.contract.ReleaseBranch)
	if err != nil {
		return err
	}
	if currentIntegration != proof.Staging.BranchHeadSHA || currentRelease != proof.Baseline.Release.SHA {
		return invariant("promotion_refs_changed")
	}
	stagingRun, err := c.readWorkflowRunByID(ctx, proof.Staging.WorkflowRuns[0].ID)
	if err != nil {
		return err
	}
	if !deploymentRunMatches(stagingRun, c.contract.StagingWorkflow, c.contract.IntegrationBranch, proof.Staging.Merge.MergeSHA) {
		return invariant("staging_workflow_proof_mismatch")
	}
	if err := c.verifyMergeResult(ctx, proof.Staging.Merge); err != nil {
		return err
	}
	digestCommit, err := c.getGitCommit(ctx, proof.Staging.DigestCommitSHA)
	if err != nil {
		return err
	}
	if !slices.Equal(digestCommit.Parents, []string{proof.Staging.Merge.MergeSHA}) {
		return invariant("promotion_graph_mismatch")
	}
	if err := c.verifyPathSet(ctx, proof.Baseline.Integration.SHA, proof.Baseline.Integration.TreeSHA, proof.Staging.Merge.MergeSHA, productPaths, proof.Baseline.Integration.SHA, proof.Baseline.Integration.TreeSHA); err != nil {
		return invariant("promotion_product_diff_mismatch")
	}
	if err := c.verifyPathSet(ctx, proof.Baseline.Release.SHA, proof.Baseline.Release.TreeSHA, proof.Staging.BranchHeadSHA, promotionPaths, proof.Baseline.MergeBaseSHA, proof.Baseline.MergeBaseTreeSHA); err != nil {
		return invariant("promotion_full_diff_mismatch")
	}
	latestIntegration, err := c.getRef(ctx, c.contract.IntegrationBranch)
	if err != nil {
		return err
	}
	latestRelease, err := c.getRef(ctx, c.contract.ReleaseBranch)
	if err != nil {
		return err
	}
	if latestIntegration != currentIntegration || latestRelease != currentRelease {
		return invariant("promotion_refs_changed")
	}
	return nil
}

func (c *Controller) validatePromotionProof(proof PromotionProof) ([]string, []string, error) {
	if err := c.validateBaseline(proof.Baseline); err != nil {
		return nil, nil, err
	}
	staging := proof.Staging
	if staging.Merge.BaseBranch != c.contract.IntegrationBranch || staging.Merge.BaseSHA != proof.Baseline.Integration.SHA ||
		staging.Merge.HeadBranch == "" || staging.Merge.HeadBranch == c.contract.IntegrationBranch || staging.Merge.HeadBranch == c.contract.ReleaseBranch || staging.Merge.MergeSHA == "" || staging.BranchHeadSHA == "" ||
		staging.DigestCommitSHA != staging.BranchHeadSHA || len(staging.WorkflowRuns) != 1 ||
		!deploymentRunMatches(staging.WorkflowRuns[0], c.contract.StagingWorkflow, c.contract.IntegrationBranch, staging.Merge.MergeSHA) || len(staging.DigestPaths) == 0 ||
		len(proof.AcceptanceEvidenceSHA256) != 64 || !validObjectID(proof.AcceptanceEvidenceSHA256) {
		return nil, nil, invariant("invalid_promotion_proof")
	}
	productPaths, err := normalizeExactPaths(proof.ProductPaths)
	if err != nil || len(productPaths) == 0 {
		return nil, nil, invariant("invalid_promotion_product_paths")
	}
	digestPaths, err := normalizeExactPaths(staging.DigestPaths)
	if err != nil {
		return nil, nil, invariant("invalid_promotion_digest_paths")
	}
	promotionPaths := append(slices.Clone(productPaths), digestPaths...)
	sort.Strings(promotionPaths)
	for index := 1; index < len(promotionPaths); index++ {
		if promotionPaths[index] == promotionPaths[index-1] {
			return nil, nil, invariant("overlapping_promotion_paths")
		}
	}
	return productPaths, promotionPaths, nil
}

func deploymentRunMatches(run WorkflowRun, workflow WorkflowContract, branch, sha string) bool {
	return run.ID > 0 && run.WorkflowID == workflow.ID && run.Name == workflow.Name && workflowPathMatches(run.Path, workflow.Path) &&
		run.HeadBranch == branch && run.HeadSHA == sha && run.Event == "push" && run.Status == "completed" && run.Conclusion == "success" && run.Attempt > 0
}

func normalizeExactPaths(values []string) ([]string, error) {
	if len(values) == 0 || len(values) > 48 {
		return nil, invariant("invalid_path_set")
	}
	result := slices.Clone(values)
	sort.Strings(result)
	for index, filename := range result {
		if err := validateRepositoryPath(filename, false); err != nil {
			return nil, err
		}
		if index > 0 && result[index-1] == filename {
			return nil, invariant("duplicate_path")
		}
	}
	return result, nil
}

func (c *Controller) verifyPathSet(ctx context.Context, baseSHA, baseTreeSHA, headSHA string, expected []string, expectedMergeBaseSHA, expectedMergeBaseTreeSHA string) error {
	comparison, err := c.readComparison(ctx, baseSHA, headSHA)
	if err != nil {
		return err
	}
	if (comparison.Status != "ahead" && comparison.Status != "diverged") || comparison.AheadBy <= 0 || comparison.BaseSHA != baseSHA || comparison.BaseTreeSHA != baseTreeSHA || comparison.MergeBaseSHA != expectedMergeBaseSHA || comparison.MergeBaseTreeSHA != expectedMergeBaseTreeSHA || len(comparison.Files) != len(expected) {
		return invariant("comparison_graph_mismatch")
	}
	actual := make([]string, 0, len(comparison.Files))
	for _, file := range comparison.Files {
		if file.Status != "modified" {
			return invariant("comparison_path_mismatch")
		}
		actual = append(actual, file.Filename)
	}
	sort.Strings(actual)
	if !slices.Equal(actual, expected) {
		return invariant("comparison_path_mismatch")
	}
	return nil
}

func (c *Controller) validatePullRequestSpec(spec PullRequestSpec) error {
	if err := validateText(spec.Title, 256, "invalid_pr_title"); err != nil || strings.Contains(spec.Title, "\n") {
		return invariant("invalid_pr_title")
	}
	if len(spec.Body) > 64*1024 || strings.ContainsAny(spec.Body, "\x00\r") {
		return invariant("invalid_pr_body")
	}
	return nil
}

type pullResponse struct {
	Number         int64  `json:"number"`
	HTMLURL        string `json:"html_url"`
	Title          string `json:"title"`
	Body           string `json:"body"`
	CreatedAt      string `json:"created_at"`
	State          string `json:"state"`
	Draft          bool   `json:"draft"`
	Merged         bool   `json:"merged"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	Mergeable      *bool  `json:"mergeable"`
	Head           struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"base"`
}

func (c *Controller) getPullRequest(ctx context.Context, number int64) (pullResponse, error) {
	if number <= 0 {
		return pullResponse{}, invariant("invalid_pr_number")
	}
	var response pullResponse
	if err := c.client.get(ctx, c.client.repositoryPath("/pulls/"+formatInt(number)), &response); err != nil {
		return pullResponse{}, err
	}
	return response, nil
}

func (c *Controller) validatePullResponse(response pullResponse, title, body, headRef, headSHA, baseRef, baseSHA string) (PullRequest, error) {
	expectedFullName := c.client.config.Owner + "/" + c.client.config.Repository
	createdAt, err := time.Parse(time.RFC3339, response.CreatedAt)
	if err != nil || response.Number <= 0 || response.HTMLURL != c.pullHTMLURL(response.Number) || response.Title != title || response.Body != body || response.State != "open" || response.Draft || response.Merged || response.Head.Ref != headRef || response.Head.SHA != headSHA || response.Base.Ref != baseRef || response.Base.SHA != baseSHA || !strings.EqualFold(response.Head.Repo.FullName, expectedFullName) {
		return PullRequest{}, invariant("pull_request_mismatch")
	}
	return PullRequest{
		Number:       response.Number,
		HTMLURL:      response.HTMLURL,
		Title:        response.Title,
		Body:         response.Body,
		CreatedAt:    createdAt,
		HeadRef:      response.Head.Ref,
		HeadSHA:      response.Head.SHA,
		BaseRef:      response.Base.Ref,
		BaseSHA:      response.Base.SHA,
		HeadFullName: response.Head.Repo.FullName,
	}, nil
}

func (c *Controller) pullHTMLURL(number int64) string {
	return "https://github.com/" + c.client.config.Owner + "/" + c.client.config.Repository + "/pull/" + formatInt(number)
}

func formatInt(value int64) string {
	return strconv.FormatInt(value, 10)
}

// observedMergeSettings carries what the repository read actually returned.
// A nil field was not in the response: the token is not allowed to see it,
// which is different from the setting being off.
type observedMergeSettings struct {
	AllowMergeCommit          *bool
	AllowSquashMerge          *bool
	AllowRebaseMerge          *bool
	AllowAutoMerge            *bool
	AllowUpdateBranch         *bool
	DeleteBranchOnMerge       *bool
	UseSquashPRTitleAsDefault *bool
	SquashMergeCommitTitle    *string
	SquashMergeCommitMessage  *string
	MergeCommitTitle          *string
	MergeCommitMessage        *string
	WebCommitSignoffRequired  *bool
}

func (o observedMergeSettings) verifyAgainst(settings MergeSettings) error {
	bools := []*bool{
		o.AllowMergeCommit, o.AllowSquashMerge, o.AllowRebaseMerge, o.AllowAutoMerge,
		o.AllowUpdateBranch, o.DeleteBranchOnMerge, o.UseSquashPRTitleAsDefault, o.WebCommitSignoffRequired,
	}
	strs := []*string{o.SquashMergeCommitTitle, o.SquashMergeCommitMessage, o.MergeCommitTitle, o.MergeCommitMessage}
	for _, field := range bools {
		if field == nil {
			return invariant("merge_settings_invisible_to_token")
		}
	}
	for _, field := range strs {
		if field == nil {
			return invariant("merge_settings_invisible_to_token")
		}
	}
	if *o.AllowMergeCommit != settings.AllowMergeCommit ||
		*o.AllowSquashMerge != settings.AllowSquashMerge ||
		*o.AllowRebaseMerge != settings.AllowRebaseMerge ||
		*o.AllowAutoMerge != settings.AllowAutoMerge ||
		*o.AllowUpdateBranch != settings.AllowUpdateBranch ||
		*o.DeleteBranchOnMerge != settings.DeleteBranchOnMerge ||
		*o.UseSquashPRTitleAsDefault != settings.UseSquashPRTitleAsDefault ||
		*o.SquashMergeCommitTitle != settings.SquashMergeCommitTitle ||
		*o.SquashMergeCommitMessage != settings.SquashMergeCommitMessage ||
		*o.MergeCommitTitle != settings.MergeCommitTitle ||
		*o.MergeCommitMessage != settings.MergeCommitMessage ||
		*o.WebCommitSignoffRequired != settings.WebCommitSignoffRequired {
		return invariant("repository_settings_mismatch")
	}
	return nil
}

func boolOrFalse(value *bool) bool {
	return value != nil && *value
}

func stringOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
