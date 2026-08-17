package githubapi

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestAwaitStagingPinsWorkflowRunAndExactDigestCommit(t *testing.T) {
	merge := MergeResult{
		PullRequestNumber: 7,
		BaseBranch:        "stg",
		BaseSHA:           shaA,
		HeadBranch:        "automation/sample",
		HeadSHA:           shaF,
		MergeSHA:          sha4,
		TreeSHA:           sha3,
	}
	steps := []requestStep{
		{method: http.MethodGet, path: "/repos/example/consumer/git/commits/" + sha4, body: gitCommitJSON(sha4, sha3, shaA, shaF)},
		{method: http.MethodGet, path: "/repos/example/consumer/actions/workflows/262913062/runs", query: "branch=stg&event=push&head_sha=" + sha4 + "&per_page=100", body: workflowRunsJSON(41, 262913062, "Deploy API/Client (stg)", ".github/workflows/deploy-stg.yml", "stg", sha4, "completed", "success")},
		{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/stg", body: refJSON("stg", sha1)},
		{method: http.MethodGet, path: "/repos/example/consumer/commits/" + sha1, body: digestCommitJSON(sha1, sha4, "[skip ci] update stg image digests for deploy "+sha4)},
		{method: http.MethodGet, path: "/repos/example/consumer/compare/" + sha4 + "..." + sha1, body: digestCompareJSON(sha4, sha3, "k8s/overlays/stg/kustomization.yaml")},
	}
	controller, transport := newTestController(t, steps, true)

	result, err := controller.AwaitStaging(context.Background(), merge,
		WaitOptions{PollInterval: time.Millisecond, Timeout: time.Second},
		DigestCommitPolicy{
			Required:           true,
			RequireDigestOnly:  true,
			ExactMessagePrefix: "[skip ci] update stg image digests for deploy ",
			ExactPaths:         []string{"k8s/overlays/stg/kustomization.yaml"},
			ActorLogin:         "github-actions[bot]",
		},
	)
	if err != nil {
		t.Fatalf("AwaitStaging() error = %v", err)
	}
	if len(result.WorkflowRuns) != 1 || result.WorkflowRuns[0].ID != 41 || result.WorkflowRuns[0].HeadSHA != sha4 || result.BranchHeadSHA != sha1 || result.DigestCommitSHA != sha1 {
		t.Fatalf("result = %+v", result)
	}
	transport.done()
}

func TestAwaitStagingStopsWhenAnotherCommitEnteredTheRail(t *testing.T) {
	merge := MergeResult{BaseBranch: "stg", BaseSHA: shaA, HeadBranch: "automation/sample", HeadSHA: shaF, MergeSHA: sha4, TreeSHA: sha3}
	steps := []requestStep{
		{method: http.MethodGet, path: "/repos/example/consumer/git/commits/" + sha4, body: gitCommitJSON(sha4, sha3, shaA, shaF)},
		{method: http.MethodGet, path: "/repos/example/consumer/actions/workflows/262913062/runs", query: "branch=stg&event=push&head_sha=" + sha4 + "&per_page=100", body: workflowRunsJSON(41, 262913062, "Deploy API/Client (stg)", ".github/workflows/deploy-stg.yml", "stg", sha4, "completed", "success")},
		{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/stg", body: refJSON("stg", sha1)},
		{method: http.MethodGet, path: "/repos/example/consumer/commits/" + sha1, body: digestCommitJSON(sha1, shaB, "unrelated commit")},
	}
	controller, transport := newTestController(t, steps, true)

	_, err := controller.AwaitStaging(context.Background(), merge,
		WaitOptions{PollInterval: time.Millisecond, Timeout: time.Second},
		DigestCommitPolicy{Required: true, RequireDigestOnly: true, ExactMessagePrefix: "[skip ci] update stg image digests for deploy ", ExactPaths: []string{"k8s/overlays/stg/kustomization.yaml"}, ActorLogin: "github-actions[bot]"},
	)
	if !IsInvariant(err, "unexpected_branch_advance") {
		t.Fatalf("AwaitStaging() error = %v", err)
	}
	transport.done()
}

func TestAwaitProductionReverifiesExactPromotionAndExistingWorkflow(t *testing.T) {
	merge := MergeResult{
		PullRequestNumber: 9,
		BaseBranch:        "prod",
		BaseSHA:           shaA,
		HeadBranch:        "stg",
		HeadSHA:           shaF,
		MergeSHA:          sha4,
		TreeSHA:           sha3,
	}
	steps := []requestStep{
		{method: http.MethodGet, path: "/repos/example/consumer/git/commits/" + sha4, body: gitCommitJSON(sha4, sha3, shaA, shaF)},
		{method: http.MethodGet, path: "/repos/example/consumer/actions/workflows/231779190/runs", query: "branch=prod&event=push&head_sha=" + sha4 + "&per_page=100", body: workflowRunsJSON(42, 231779190, "Deploy API/Client", ".github/workflows/deploy.yml", "prod", sha4, "completed", "success")},
		{method: http.MethodGet, path: "/repos/example/consumer/actions/workflows/307681769/runs", query: "branch=prod&event=push&head_sha=" + sha4 + "&per_page=100", body: workflowRunsJSON(43, 307681769, "prod-overlay-guard", ".github/workflows/prod-overlay-guard.yml", "prod", sha4, "completed", "success")},
		{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/prod", body: refJSON("prod", sha1)},
		{method: http.MethodGet, path: "/repos/example/consumer/commits/" + sha1, body: digestCommitJSON(sha1, sha4, "[skip ci] update image digests for deploy "+sha4)},
		{method: http.MethodGet, path: "/repos/example/consumer/compare/" + sha4 + "..." + sha1, body: digestCompareJSON(sha4, sha3, "k8s/overlays/prod/kustomization.yaml")},
	}
	controller, transport := newTestController(t, steps, true)

	result, err := controller.AwaitProduction(context.Background(), merge,
		WaitOptions{PollInterval: time.Millisecond, Timeout: time.Second},
		DigestCommitPolicy{
			Required:           true,
			RequireDigestOnly:  true,
			ExactMessagePrefix: "[skip ci] update image digests for deploy ",
			ExactPaths:         []string{"k8s/overlays/prod/kustomization.yaml"},
			ActorLogin:         "github-actions[bot]",
		},
	)
	if err != nil {
		t.Fatalf("AwaitProduction() error = %v", err)
	}
	if len(result.WorkflowRuns) != 2 || result.WorkflowRuns[0].ID != 42 || result.WorkflowRuns[0].Path != ".github/workflows/deploy.yml" || result.WorkflowRuns[1].ID != 43 || result.BranchHeadSHA != sha1 {
		t.Fatalf("result = %+v", result)
	}
	transport.done()
}

func TestAwaitDeploymentStopsOnWorkflowFailureBeforeInspectingBranch(t *testing.T) {
	merge := MergeResult{BaseBranch: "stg", BaseSHA: shaA, HeadBranch: "automation/sample", HeadSHA: shaF, MergeSHA: sha4, TreeSHA: sha3}
	steps := []requestStep{
		{method: http.MethodGet, path: "/repos/example/consumer/git/commits/" + sha4, body: gitCommitJSON(sha4, sha3, shaA, shaF)},
		{method: http.MethodGet, path: "/repos/example/consumer/actions/workflows/262913062/runs", query: "branch=stg&event=push&head_sha=" + sha4 + "&per_page=100", body: workflowRunsJSON(41, 262913062, "Deploy API/Client (stg)", ".github/workflows/deploy-stg.yml", "stg", sha4, "completed", "failure")},
	}
	controller, transport := newTestController(t, steps, true)

	_, err := controller.AwaitStaging(context.Background(), merge,
		WaitOptions{PollInterval: time.Millisecond, Timeout: time.Second},
		DigestCommitPolicy{Required: true, RequireDigestOnly: true, ExactMessagePrefix: "prefix ", ExactPaths: []string{"k8s/overlays/stg/kustomization.yaml"}, ActorLogin: "github-actions[bot]"},
	)
	if !IsInvariant(err, "deployment_workflow_failed") {
		t.Fatalf("AwaitStaging() error = %v", err)
	}
	transport.done()
}

func workflowRunsJSON(id, workflowID int64, name, path, branch, sha, status, conclusion string) string {
	return `{"total_count":1,"workflow_runs":[{"id":` + formatInt(id) + `,"workflow_id":` + formatInt(workflowID) + `,"name":"` + name + `","html_url":"https://github.example/run","head_branch":"` + branch + `","head_sha":"` + sha + `","event":"push","status":"` + status + `","conclusion":"` + conclusion + `","path":"` + path + `","run_attempt":1,"created_at":"2026-08-03T01:00:00Z","updated_at":"2026-08-03T01:10:00Z"}]}`
}

func digestCommitJSON(sha, parent, message string) string {
	return `{"sha":"` + sha + `","commit":{"message":"` + message + `"},"author":{"login":"github-actions[bot]"},"committer":{"login":"github-actions[bot]"},"parents":[{"sha":"` + parent + `"}]}`
}

func digestCompareJSON(sourceSHA, sourceTree, filename string) string {
	patch := "@@ -1,2 +1,2 @@\n-  digest: sha256:" + strings.Repeat("a", 64) + "\n+  digest: sha256:" + strings.Repeat("b", 64)
	return compareJSON("ahead", sourceSHA, sourceTree, sourceSHA, sourceTree, []comparisonFile{{Filename: filename, Status: "modified", Patch: patch}})
}
