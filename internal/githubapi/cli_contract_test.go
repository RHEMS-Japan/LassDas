package githubapi

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func cliController(t *testing.T, steps []requestStep, verified bool) (*Controller, *scriptedTransport) {
	t.Helper()
	existing, transport := newTestController(t, steps, verified)
	contract := Contract{Kind: "cli", IntegrationBranch: "develop", DefaultBranch: "main"}
	if _, err := NewController(existing.client, contract); err == nil {
		t.Fatal("CLI allowed a promoting controller")
	}
	controller, err := NewProposalController(existing.client, contract)
	if err != nil {
		t.Fatal(err)
	}
	return controller, transport
}

func cliProposalRequests() []requestStep {
	return []requestStep{
		{method: http.MethodGet, path: "/repos/example/consumer", body: `{"id":1101796955,"full_name":"example/consumer","default_branch":"main","archived":false,"disabled":false}`},
		{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/develop", body: refJSON("develop", shaA)},
		{method: http.MethodGet, path: "/repos/example/consumer/git/commits/" + shaA, body: gitCommitJSON(shaA, shaB, sha2)},
		{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/develop", body: refJSON("develop", shaA)},
		{method: http.MethodGet, path: "/repos/example/consumer/git/commits/" + shaA, body: gitCommitJSON(shaA, shaB, sha2)},
		{method: http.MethodGet, path: "/repos/example/consumer/git/trees/" + shaB, query: "recursive=1", body: `{"sha":"` + shaB + `","truncated":false,"tree":[{"path":"main.go","mode":"100644","type":"blob","sha":"` + shaC + `"}]}`},
		{method: http.MethodPost, path: "/repos/example/consumer/git/blobs", status: http.StatusCreated, body: `{"sha":"` + shaD + `"}`},
		{method: http.MethodPost, path: "/repos/example/consumer/git/trees", status: http.StatusCreated, body: `{"sha":"` + shaE + `"}`},
		{method: http.MethodPost, path: "/repos/example/consumer/git/commits", status: http.StatusCreated, body: `{"sha":"` + shaF + `"}`},
		{method: http.MethodGet, path: "/repos/example/consumer/git/commits/" + shaF, body: gitCommitJSON(shaF, shaE, shaA)},
		{method: http.MethodGet, path: "/repos/example/consumer/compare/" + shaA + "..." + shaF, body: `{"status":"ahead","ahead_by":1,"behind_by":0,"total_commits":1,"files":[{"filename":"main.go","status":"modified"}]}`},
		{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/develop", body: refJSON("develop", shaA)},
		{method: http.MethodPost, path: "/repos/example/consumer/git/refs", status: http.StatusCreated, body: `{"ref":"refs/heads/automation/sample","object":{"type":"commit","sha":"` + shaF + `"}}`},
		{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/develop", body: refJSON("develop", shaA)},
		{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/automation/sample", body: refJSON("automation/sample", shaF)},
		{method: http.MethodPost, path: "/repos/example/consumer/pulls", status: http.StatusCreated, body: pullJSON(8, "open", "automation/sample", shaF, "develop", shaA, nil)},
	}
}

func TestCLIProposalVerifiesPublishesAndCreatesPRWithoutWebRequests(t *testing.T) {
	controller, transport := cliController(t, cliProposalRequests(), false)
	ctx := context.Background()
	if _, err := controller.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	baseline, err := controller.VerifyBaseline(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if baseline.Release != (Snapshot{}) || baseline.MergeBaseSHA != "" || baseline.MergeBaseTreeSHA != "" {
		t.Fatalf("invented release evidence: %+v", baseline)
	}
	feature, err := controller.PublishFeature(ctx, baseline, FeatureSpec{Branch: "automation/sample", CommitMessage: "sample", AllowedPathPrefixes: []string{"main.go"}, Files: []FileUpdate{{Path: "main.go", Content: []byte("package main\n"), ExpectedBlobSHA: shaC}}})
	if err != nil {
		t.Fatal(err)
	}
	pull, err := controller.CreateFeaturePullRequest(ctx, feature, PullRequestSpec{Title: "sample", Body: "ticket"})
	if err != nil {
		t.Fatal(err)
	}
	if pull.HTMLURL != "https://github.com/example/consumer/pull/8" || pull.BaseRef != "develop" || pull.HeadSHA != shaF {
		t.Fatalf("PR not bound to candidate: %+v", pull)
	}
	transport.done()
}

func TestCLIRetainsIdentityActiveAndObservedDefaultChecks(t *testing.T) {
	original := `{"id":1101796955,"full_name":"example/consumer","default_branch":"main","archived":false,"disabled":false}`
	for name, pair := range map[string][2]string{
		"id": {"1101796955", "1101796956"}, "name": {"example/consumer", "example/other"}, "default": {"main", "develop"}, "archived": {`"archived":false`, `"archived":true`}, "disabled": {`"disabled":false`, `"disabled":true`},
	} {
		t.Run(name, func(t *testing.T) {
			c, tr := cliController(t, []requestStep{{method: http.MethodGet, path: "/repos/example/consumer", body: strings.ReplaceAll(original, pair[0], pair[1])}}, false)
			if _, err := c.Verify(context.Background()); err == nil {
				t.Fatal("accepted changed repository")
			}
			tr.done()
		})
	}
}

func TestCLIRefusesReleaseEvidenceAndWebStagesBeforeRequests(t *testing.T) {
	c, tr := cliController(t, nil, true)
	baseline := Baseline{Integration: Snapshot{Branch: "develop", SHA: shaA, TreeSHA: shaB}}
	if err := c.validateBaseline(baseline); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Baseline){
		func(b *Baseline) { b.Integration.Branch = "main" }, func(b *Baseline) { b.Integration.SHA = "invalid" }, func(b *Baseline) { b.Integration.TreeSHA = "invalid" },
		func(b *Baseline) { b.Release = Snapshot{Branch: "prod", SHA: shaA, TreeSHA: shaB} }, func(b *Baseline) { b.MergeBaseSHA = shaA }, func(b *Baseline) { b.MergeBaseTreeSHA = shaB },
	} {
		b := baseline
		mutate(&b)
		if err := c.validateBaseline(b); err == nil {
			t.Fatalf("accepted invalid baseline: %+v", b)
		}
	}
	ctx := context.Background()
	if _, err := c.MergeFeaturePullRequest(ctx, PullRequest{BaseRef: "develop", HeadRef: "automation/sample"}, CheckEvidence{}, MergeSpec{}, WaitOptions{}); !IsInvariant(err, "cli_delivery_stops_at_pull_request") {
		t.Fatalf("merge: %v", err)
	}
	if _, err := c.CreatePromotionPullRequest(ctx, PromotionProof{}, DigestCommitPolicy{}, PullRequestSpec{}); !IsInvariant(err, "cli_delivery_stops_at_pull_request") {
		t.Fatalf("promotion: %v", err)
	}
	if _, err := c.WaitForPullRequestChecks(ctx, PullRequest{}, CheckRequirements{}, WaitOptions{}); !IsInvariant(err, "cli_delivery_stops_at_pull_request") {
		t.Fatalf("checks: %v", err)
	}
	if _, err := c.AwaitStaging(ctx, MergeResult{}, WaitOptions{}, DigestCommitPolicy{}); !IsInvariant(err, "cli_delivery_stops_at_pull_request") {
		t.Fatalf("staging: %v", err)
	}
	if _, err := c.AwaitProduction(ctx, MergeResult{}, WaitOptions{}, DigestCommitPolicy{}); !IsInvariant(err, "cli_delivery_stops_at_pull_request") {
		t.Fatalf("production: %v", err)
	}
	tr.done()
}

func TestCLIProposalRetainsCandidateBindingGates(t *testing.T) {
	cases := []struct {
		name   string
		last   int
		mutate func([]requestStep)
		code   string
	}{
		{"source blob", 5, func(s []requestStep) { s[5].body = strings.ReplaceAll(s[5].body, shaC, shaD) }, "source_blob_changed"},
		{"unchanged candidate tree", 7, func(s []requestStep) { s[7].body = strings.ReplaceAll(s[7].body, shaE, shaB) }, "invalid_candidate_tree"},
		{"candidate parents", 9, func(s []requestStep) { s[9].body = gitCommitJSON(shaF, shaE, sha2) }, "candidate_commit_mismatch"},
		{"candidate diff", 10, func(s []requestStep) { s[10].body = strings.ReplaceAll(s[10].body, "main.go", "other.go") }, "candidate_diff_mismatch"},
		{"base moved before publish", 11, func(s []requestStep) { s[11].body = refJSON("develop", sha2) }, "integration_base_changed"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			steps := cliProposalRequests()[:test.last+1]
			test.mutate(steps)
			c, tr := cliController(t, steps, false)
			ctx := context.Background()
			if _, err := c.Verify(ctx); err != nil {
				t.Fatal(err)
			}
			baseline, err := c.VerifyBaseline(ctx)
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.PublishFeature(ctx, baseline, FeatureSpec{Branch: "automation/sample", CommitMessage: "sample", AllowedPathPrefixes: []string{"main.go"}, Files: []FileUpdate{{Path: "main.go", Content: []byte("package main\n"), ExpectedBlobSHA: shaC}}})
			if !IsInvariant(err, test.code) {
				t.Fatalf("got %v, want %s", err, test.code)
			}
			tr.done()
		})
	}
}
