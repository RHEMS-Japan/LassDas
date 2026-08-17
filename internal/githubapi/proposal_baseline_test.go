package githubapi

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// A proposal run tolerates staging running ahead of release between promotion
// batches — VerifyBaseline stamps such a baseline on purpose. Publish must
// accept the baseline it was issued, or every proposal after an unpromoted
// merge dies at publish, ten minutes after the mismatch was knowable
// (measured 2026-08-13/14 on two consecutive live proposals). The release
// branch is not read at all: nothing of it enters a proposal's artifact, so
// there is nothing to pin.
func TestPublishFeatureAcceptsAheadOfReleaseBaselineForAProposal(t *testing.T) {
	steps := []requestStep{
		{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/stg", body: refJSON("stg", shaA)},
		{method: http.MethodGet, path: "/repos/example/consumer/git/commits/" + shaA, body: gitCommitJSON(shaA, shaB, sha2)},
		{method: http.MethodGet, path: "/repos/example/consumer/git/trees/" + shaB, query: "recursive=1", body: `{"sha":"` + shaB + `","truncated":false,"tree":[{"path":"client/src/page.tsx","mode":"100644","type":"blob","sha":"` + shaC + `"}]}`},
		{method: http.MethodPost, path: "/repos/example/consumer/git/blobs", status: http.StatusCreated, body: `{"sha":"` + shaD + `"}`},
		{method: http.MethodPost, path: "/repos/example/consumer/git/trees", status: http.StatusCreated, body: `{"sha":"` + shaE + `"}`},
		{method: http.MethodPost, path: "/repos/example/consumer/git/commits", status: http.StatusCreated, body: `{"sha":"` + shaF + `"}`},
		{method: http.MethodGet, path: "/repos/example/consumer/git/commits/" + shaF, body: gitCommitJSON(shaF, shaE, shaA)},
		{method: http.MethodGet, path: "/repos/example/consumer/compare/" + shaA + "..." + shaF, body: `{"status":"ahead","ahead_by":1,"behind_by":0,"total_commits":1,"files":[{"filename":"client/src/page.tsx","status":"modified"}]}`},
		{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/stg", body: refJSON("stg", shaA)},
		{method: http.MethodPost, path: "/repos/example/consumer/git/refs", status: http.StatusCreated, body: `{"ref":"refs/heads/automation/sample","object":{"type":"commit","sha":"` + shaF + `"}}`},
	}
	controller, transport := newTestController(t, steps, true)
	controller.promotesChanges = false
	// Staging one merge ahead of release: the merge base is the release head
	// itself, carrying the release tree.
	baseline := Baseline{
		Integration:  Snapshot{Branch: "stg", SHA: shaA, TreeSHA: shaB},
		Release:      Snapshot{Branch: "prod", SHA: sha2, TreeSHA: sha3},
		MergeBaseSHA: sha2, MergeBaseTreeSHA: sha3,
	}

	published, err := controller.PublishFeature(context.Background(), baseline, FeatureSpec{
		Branch:              "automation/sample",
		CommitMessage:       "sample ticket",
		AllowedPathPrefixes: []string{"client/src/"},
		Files: []FileUpdate{{
			Path:            "client/src/page.tsx",
			Content:         []byte("new content"),
			ExpectedBlobSHA: shaC,
		}},
	})
	if err != nil {
		t.Fatalf("a proposal must accept the baseline it was issued, got %v", err)
	}
	if published.HeadSHA != shaF {
		t.Fatalf("published = %+v", published)
	}
	transport.done()
}

// A run that promotes still refuses the same baseline, before any API call:
// its merges land where staging and release must agree.
func TestPublishFeatureStillRejectsAheadOfReleaseBaselineWhenPromoting(t *testing.T) {
	controller, transport := newTestController(t, nil, true)
	baseline := Baseline{
		Integration:  Snapshot{Branch: "stg", SHA: shaA, TreeSHA: shaB},
		Release:      Snapshot{Branch: "prod", SHA: sha2, TreeSHA: sha3},
		MergeBaseSHA: sha2, MergeBaseTreeSHA: sha3,
	}

	_, err := controller.PublishFeature(context.Background(), baseline, FeatureSpec{
		Branch:              "automation/sample",
		CommitMessage:       "sample ticket",
		AllowedPathPrefixes: []string{"client/src/"},
		Files:               []FileUpdate{{Path: "client/src/page.tsx", Content: []byte("x"), ExpectedBlobSHA: shaC}},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid_baseline") {
		t.Fatalf("a promoting run must reject differing trees, got %v", err)
	}
	transport.done()
}

// A malformed release tree id is refused in both modes, before any API call:
// dropping the equality demand for proposals must not drop well-formedness
// with it.
func TestPublishFeatureRejectsAMalformedReleaseTree(t *testing.T) {
	for name, promotes := range map[string]bool{"proposal": false, "promote": true} {
		controller, transport := newTestController(t, nil, true)
		controller.promotesChanges = promotes
		baseline := Baseline{
			Integration:  Snapshot{Branch: "stg", SHA: shaA, TreeSHA: shaB},
			Release:      Snapshot{Branch: "prod", SHA: sha2, TreeSHA: "not-a-tree"},
			MergeBaseSHA: sha2, MergeBaseTreeSHA: sha3,
		}

		_, err := controller.PublishFeature(context.Background(), baseline, FeatureSpec{
			Branch:              "automation/sample",
			CommitMessage:       "sample ticket",
			AllowedPathPrefixes: []string{"client/src/"},
			Files:               []FileUpdate{{Path: "client/src/page.tsx", Content: []byte("x"), ExpectedBlobSHA: shaC}},
		})
		if err == nil || !strings.Contains(err.Error(), "invalid_baseline") {
			t.Fatalf("%s: a malformed release tree must be refused, got %v", name, err)
		}
		transport.done()
	}
}
