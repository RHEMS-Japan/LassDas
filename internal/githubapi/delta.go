package githubapi

import (
	"context"
	"strings"
)

// The promotion rail moves the WHOLE integration branch, never one ticket.
// PromotionDelta is what the requester reads next to the staging evidence
// before writing Go: every commit the release branch would receive.

// PromotionCommit is one commit a promotion would carry.
type PromotionCommit struct {
	SHA   string `json:"sha"`
	Title string `json:"title"`
}

// PromotionDelta describes the release→integration gap at one instant.
type PromotionDelta struct {
	ReleaseSHA     string            `json:"release_sha"`
	IntegrationSHA string            `json:"integration_sha"`
	Status         string            `json:"status"` // identical | ahead | behind | diverged
	AheadBy        int               `json:"ahead_by"`
	Commits        []PromotionCommit `json:"commits"`
	// CommitsTruncated is set when the compare API capped the commit list
	// (250); AheadBy stays exact.
	CommitsTruncated bool `json:"commits_truncated,omitempty"`
	// Files is every path the promotion would change. The promotion gate
	// (verifyPathSet) enforces that this set is exactly the delivery's own
	// files plus the CI digest files — one delivery per promotion — so the
	// attendant reads this to decide whether asking for a Go can succeed at
	// all. Capped by the compare API at 300; FilesTruncated marks the cap.
	Files          []string `json:"files,omitempty"`
	FilesTruncated bool     `json:"files_truncated,omitempty"`
}

// ReadPromotionDelta compares the release branch to the integration branch.
// Read-only; "identical" simply means a promotion would carry nothing.
func (c *Controller) ReadPromotionDelta(ctx context.Context) (PromotionDelta, error) {
	if err := c.client.requireVerified(); err != nil {
		return PromotionDelta{}, err
	}
	release, err := c.SnapshotRelease(ctx)
	if err != nil {
		return PromotionDelta{}, err
	}
	integration, err := c.SnapshotIntegration(ctx)
	if err != nil {
		return PromotionDelta{}, err
	}
	var response struct {
		Status       string `json:"status"`
		AheadBy      int    `json:"ahead_by"`
		TotalCommits int    `json:"total_commits"`
		Commits      []struct {
			SHA    string `json:"sha"`
			Commit struct {
				Message string `json:"message"`
			} `json:"commit"`
		} `json:"commits"`
		Files []struct {
			Filename string `json:"filename"`
		} `json:"files"`
	}
	endpoint := c.client.repositoryPath("/compare/" + release.SHA + "..." + integration.SHA)
	if err := c.client.get(ctx, endpoint, &response); err != nil {
		return PromotionDelta{}, err
	}
	if response.Status != "ahead" && response.Status != "behind" && response.Status != "diverged" && response.Status != "identical" {
		return PromotionDelta{}, invariant("invalid_compare_response")
	}
	delta := PromotionDelta{
		ReleaseSHA: release.SHA, IntegrationSHA: integration.SHA,
		Status: response.Status, AheadBy: response.AheadBy,
		Commits:          make([]PromotionCommit, 0, len(response.Commits)),
		CommitsTruncated: len(response.Commits) < response.TotalCommits,
	}
	for _, commit := range response.Commits {
		if !validObjectID(commit.SHA) {
			return PromotionDelta{}, invariant("invalid_compare_response")
		}
		title, _, _ := strings.Cut(commit.Commit.Message, "\n")
		delta.Commits = append(delta.Commits, PromotionCommit{SHA: commit.SHA, Title: strings.TrimSpace(title)})
	}
	for _, file := range response.Files {
		delta.Files = append(delta.Files, file.Filename)
	}
	delta.FilesTruncated = len(response.Files) >= 300
	return delta, nil
}
