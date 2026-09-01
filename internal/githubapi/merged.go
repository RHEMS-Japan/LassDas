package githubapi

import (
	"context"
	"net/http"
	"strconv"
	"time"
)

// AwaitFeatureMerge waits for a HUMAN to merge the delivered pull request
// and returns the merge the way MergeFeaturePullRequest would have — the
// debug role runs after the human decision, so the merge is observed here,
// never performed. A pull request closed without merging fails honestly,
// and only the contracted merge-commit shape (two parents, the delivered
// head second) is accepted: anything else means the branch protection or
// merge settings changed under the run, and the observation must not guess.
func (c *Controller) AwaitFeatureMerge(parent context.Context, pull PullRequest, wait WaitOptions) (MergeResult, error) {
	if err := c.client.requireVerified(); err != nil {
		return MergeResult{}, err
	}
	if pull.Number <= 0 || !validObjectID(pull.HeadSHA) {
		return MergeResult{}, invariant("invalid_pull_request")
	}
	ctx, cancel, err := waitContext(parent, wait)
	if err != nil {
		return MergeResult{}, err
	}
	defer cancel()
	for {
		result, found, err := c.probeFeatureMerge(ctx, pull)
		if err != nil {
			return MergeResult{}, err
		}
		if found {
			return result, nil
		}
		select {
		case <-ctx.Done():
			return MergeResult{}, invariant("merge_wait_timeout")
		case <-time.After(wait.PollInterval):
		}
	}
}

// probeFeatureMerge takes one read-only look at the pull request. Transient
// read failures — the package's own isTransientReadError set (5xx, dropped
// connections, rate limits), which every other wait loop retries on —
// return no error at all: a wait measured in days must outlive them, and
// the deadline is the bound. Everything else stops the wait immediately:
// invariant violations (closed unmerged, a merge shape outside the
// contract) and permanent API failures (credential loss, a vanished pull
// request) never heal on their own, and 72 hours of silent polling would
// bury the actual cause.
func (c *Controller) probeFeatureMerge(ctx context.Context, pull PullRequest) (MergeResult, bool, error) {
	response, err := c.readPullByNumber(ctx, pull.Number)
	if err != nil {
		if isTransientReadError(err) {
			return MergeResult{}, false, nil
		}
		return MergeResult{}, false, err
	}
	if response.State == "closed" && !response.Merged {
		return MergeResult{}, false, invariant("pull_request_closed_unmerged")
	}
	if !response.Merged {
		return MergeResult{}, false, nil
	}
	if !validObjectID(response.MergeCommitSHA) {
		return MergeResult{}, false, invariant("invalid_merge_response")
	}
	commit, err := c.getGitCommit(ctx, response.MergeCommitSHA)
	if err != nil {
		// A 404 here is NOT permanent: the merge API can name a commit the
		// git commits API does not serve yet, the same lag waitCommit and
		// the other merge readers tolerate. The pull request itself proved
		// readable one request ago, so keep polling.
		if isTransientReadError(err) || isStatus(err, http.StatusNotFound) {
			return MergeResult{}, false, nil
		}
		return MergeResult{}, false, err
	}
	if len(commit.Parents) != 2 || commit.Parents[1] != pull.HeadSHA {
		return MergeResult{}, false, invariant("unsupported_merge_shape")
	}
	return MergeResult{
		PullRequestNumber: pull.Number,
		BaseBranch:        pull.BaseRef, BaseSHA: commit.Parents[0],
		HeadBranch: pull.HeadRef, HeadSHA: pull.HeadSHA,
		MergeSHA: response.MergeCommitSHA, TreeSHA: commit.TreeSHA,
	}, true, nil
}

// PullMergedState is one read-only look at whether a pull request merged —
// the honest-reporting primitive for "the merge verb failed, but did the
// merge itself land?".
type PullMergedState struct {
	State          string `json:"state"`
	Merged         bool   `json:"merged"`
	MergeCommitSHA string `json:"merge_commit_sha,omitempty"`
}

// ReadPullMerged reads the pull request's merged state, nothing else. It
// never waits and never mutates.
func (c *Controller) ReadPullMerged(ctx context.Context, number int64) (PullMergedState, error) {
	if err := c.client.requireVerified(); err != nil {
		return PullMergedState{}, err
	}
	if number <= 0 {
		return PullMergedState{}, invariant("invalid_pull_request")
	}
	response, err := c.readPullByNumber(ctx, number)
	if err != nil {
		return PullMergedState{}, err
	}
	return PullMergedState{State: response.State, Merged: response.Merged, MergeCommitSHA: response.MergeCommitSHA}, nil
}

func (c *Controller) readPullByNumber(ctx context.Context, number int64) (pullResponse, error) {
	var response pullResponse
	if err := c.client.get(ctx, c.client.repositoryPath("/pulls/"+strconv.FormatInt(number, 10)), &response); err != nil {
		return pullResponse{}, err
	}
	if response.Number != number {
		return pullResponse{}, invariant("invalid_pull_response")
	}
	return response, nil
}
