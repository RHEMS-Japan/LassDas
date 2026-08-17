package githubapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestWaitForPullRequestChecksRequiresRealCheckRunAndStatusEvidence(t *testing.T) {
	pull := newFeaturePull()
	steps := appendPullRefChecks(nil, pull)
	steps = appendExactFeatureWorkflowSteps(steps, pull)
	steps = append(steps,
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/commits/" + shaF + "/check-runs", query: "filter=latest&per_page=100", body: `{"total_count":1,"check_runs":[{"id":501,"name":"gate","status":"completed","conclusion":"success","app":{"slug":"github-actions"}}]}`},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/commits/" + shaF + "/status", query: "per_page=100", body: `{"total_count":1,"statuses":[{"id":601,"context":"qa/user-visible","state":"success"}]}`},
	)
	steps = appendPullRefChecks(steps, pull)
	steps = appendUniquePullStep(steps, pull)
	controller, transport := newTestController(t, steps, true)

	evidence, err := controller.WaitForPullRequestChecks(context.Background(), pull, CheckRequirements{
		CheckRuns: []RequiredCheckRun{{Name: "gate", AppSlug: "github-actions"}},
		Statuses:  []string{"qa/user-visible"},
	}, WaitOptions{PollInterval: time.Millisecond, Timeout: time.Second})
	if err != nil {
		t.Fatalf("WaitForPullRequestChecks() error = %v", err)
	}
	if evidence.PullRequestNumber != 7 || evidence.HeadSHA != shaF || len(evidence.WorkflowRunIDs) != 2 || len(evidence.WorkflowJobIDs) != 3 || len(evidence.CheckRunIDs) != 1 || evidence.CheckRunIDs[0] != 501 || len(evidence.StatusIDs) != 1 || evidence.StatusIDs[0] != 601 {
		t.Fatalf("evidence = %+v", evidence)
	}
	transport.done()
}

func TestWaitForPullRequestChecksStopsOnFailedRequiredGate(t *testing.T) {
	pull := newFeaturePull()
	steps := appendPullRefChecks(nil, pull)
	steps = appendExactFeatureWorkflowSteps(steps, pull)
	steps = append(steps, requestStep{method: http.MethodGet, path: "/repos/example/consumer/commits/" + shaF + "/check-runs", query: "filter=latest&per_page=100", body: `{"total_count":1,"check_runs":[{"id":501,"name":"gate","status":"completed","conclusion":"failure","app":{"slug":"github-actions"}}]}`})
	controller, transport := newTestController(t, steps, true)

	_, err := controller.WaitForPullRequestChecks(context.Background(), pull, CheckRequirements{
		CheckRuns: []RequiredCheckRun{{Name: "gate", AppSlug: "github-actions"}},
	}, WaitOptions{PollInterval: time.Millisecond, Timeout: time.Second})
	if !IsInvariant(err, "required_check_run_failed") {
		t.Fatalf("WaitForPullRequestChecks() error = %v", err)
	}
	transport.done()
}

func TestWaitForPullRequestChecksRejectsUnexpectedWorkflowJobSet(t *testing.T) {
	pull := newFeaturePull()
	steps := appendPullRefChecks(nil, pull)
	steps = append(steps,
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/pulls", query: "base=stg&head=example%3Aautomation%2Fsample&per_page=100&state=open", body: `[` + pullJSON(7, "open", pull.HeadRef, pull.HeadSHA, pull.BaseRef, pull.BaseSHA, nil) + `]`},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/actions/workflows/234224542/runs", query: "branch=automation%2Fsample&event=pull_request&head_sha=" + pull.HeadSHA + "&per_page=100", body: pullWorkflowRunsJSON(701, 234224542, "codex-review", ".github/workflows/codex-review.yml", pull)},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/actions/runs/701/jobs", query: "filter=latest&per_page=100", body: `{"total_count":1,"jobs":[{"id":801,"run_id":701,"name":"unexpected-job","status":"completed","conclusion":"success"}]}`},
	)
	controller, transport := newTestController(t, steps, true)

	_, err := controller.WaitForPullRequestChecks(context.Background(), pull, CheckRequirements{}, WaitOptions{PollInterval: time.Millisecond, Timeout: time.Second})
	if !IsInvariant(err, "workflow_job_set_mismatch") {
		t.Fatalf("WaitForPullRequestChecks() error = %v", err)
	}
	transport.done()
}

func appendExactFeatureWorkflowSteps(steps []requestStep, pull PullRequest) []requestStep {
	query := "branch=automation%2Fsample&event=pull_request&head_sha=" + pull.HeadSHA + "&per_page=100"
	uniqueQuery := "base=stg&head=example%3Aautomation%2Fsample&per_page=100&state=open"
	if pull.HeadRef == "stg" {
		query = "branch=stg&event=pull_request&head_sha=" + pull.HeadSHA + "&per_page=100"
		uniqueQuery = "base=prod&head=example%3Astg&per_page=100&state=open"
	}
	return append(steps,
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/pulls", query: uniqueQuery, body: `[` + pullJSON(pull.Number, "open", pull.HeadRef, pull.HeadSHA, pull.BaseRef, pull.BaseSHA, nil) + `]`},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/actions/workflows/234224542/runs", query: query, body: pullWorkflowRunsJSON(701, 234224542, "codex-review", ".github/workflows/codex-review.yml", pull)},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/actions/runs/701/jobs", query: "filter=latest&per_page=100", body: `{"total_count":1,"jobs":[{"id":801,"run_id":701,"name":"gate","status":"completed","conclusion":"success"}]}`},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/actions/workflows/317831412/runs", query: query, body: pullWorkflowRunsJSON(702, 317831412, "QA Gates", ".github/workflows/qa-gates.yml", pull)},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/actions/runs/702/jobs", query: "filter=latest&per_page=100", body: `{"total_count":2,"jobs":[{"id":802,"run_id":702,"name":"E1 migration target 追随 (blocking)","status":"completed","conclusion":"success"},{"id":803,"run_id":702,"name":"E5 認可ファイルのテスト同伴 (warn only)","status":"completed","conclusion":"success"}]}`},
	)
}

func appendUniquePullStep(steps []requestStep, pull PullRequest) []requestStep {
	query := "base=stg&head=example%3Aautomation%2Fsample&per_page=100&state=open"
	if pull.HeadRef == "stg" {
		query = "base=prod&head=example%3Astg&per_page=100&state=open"
	}
	return append(steps, requestStep{method: http.MethodGet, path: "/repos/example/consumer/pulls", query: query, body: `[` + pullJSON(pull.Number, "open", pull.HeadRef, pull.HeadSHA, pull.BaseRef, pull.BaseSHA, nil) + `]`})
}

func pullWorkflowRunsJSON(runID, workflowID int64, name, path string, pull PullRequest) string {
	return `{"total_count":1,"workflow_runs":[{"id":` + formatInt(runID) + `,"workflow_id":` + formatInt(workflowID) + `,"name":"` + name + `","display_title":"` + pull.Title + `","head_branch":"` + pull.HeadRef + `","head_sha":"` + pull.HeadSHA + `","head_repository":{"full_name":"example/consumer"},"repository":{"full_name":"example/consumer"},"event":"pull_request","status":"completed","conclusion":"success","path":"` + path + `","run_attempt":1,"created_at":"2026-08-03T00:01:00Z","pull_requests":[]}]}`
}

func appendPullRefChecks(steps []requestStep, pull PullRequest) []requestStep {
	return append(steps,
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/pulls/" + formatInt(pull.Number), body: pullJSON(pull.Number, "open", pull.HeadRef, pull.HeadSHA, pull.BaseRef, pull.BaseSHA, nil)},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/" + pull.BaseRef, body: refJSON(pull.BaseRef, pull.BaseSHA)},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/" + pull.HeadRef, body: refJSON(pull.HeadRef, pull.HeadSHA)},
	)
}

func TestCreatePromotionPullRequestPinsBothLongLivedRefs(t *testing.T) {
	acceptanceHash := strings.Repeat("9", 64)
	steps := []requestStep{
		{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/stg", body: refJSON("stg", shaD)},
		{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/prod", body: refJSON("prod", sha2)},
		{method: http.MethodGet, path: "/repos/example/consumer/actions/runs/41", body: directWorkflowRunJSON(41, 262913062, "Deploy API/Client (stg)", "", ".github/workflows/deploy-stg.yml", "stg", sha4, "push")},
		{method: http.MethodGet, path: "/repos/example/consumer/git/commits/" + sha4, body: gitCommitJSON(sha4, sha3, shaA, shaF)},
		{method: http.MethodGet, path: "/repos/example/consumer/git/commits/" + shaD, body: gitCommitJSON(shaD, shaE, sha4)},
		{method: http.MethodGet, path: "/repos/example/consumer/compare/" + shaA + "..." + sha4, body: compareJSON("ahead", shaA, shaB, shaA, shaB, []comparisonFile{{Filename: "client/src/page.tsx", Status: "modified"}})},
		{method: http.MethodGet, path: "/repos/example/consumer/compare/" + sha2 + "..." + shaD, body: compareJSON("diverged", sha2, shaB, sha1, shaB, []comparisonFile{{Filename: "client/src/page.tsx", Status: "modified"}, {Filename: "k8s/overlays/stg/kustomization.yaml", Status: "modified"}})},
		{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/stg", body: refJSON("stg", shaD)},
		{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/prod", body: refJSON("prod", sha2)},
		{method: http.MethodPost, path: "/repos/example/consumer/pulls", status: http.StatusCreated, body: pullJSON(9, "open", "stg", shaD, "prod", sha2, nil), check: func(_ *http.Request, body []byte) {
			var payload struct {
				Head  string `json:"head"`
				Base  string `json:"base"`
				Draft bool   `json:"draft"`
			}
			if json.Unmarshal(body, &payload) != nil || payload.Head != "stg" || payload.Base != "prod" || payload.Draft {
				t.Fatalf("promotion PR body = %s", body)
			}
		}},
	}
	controller, transport := newTestController(t, steps, true)

	baseline := Baseline{
		Integration:  Snapshot{Branch: "stg", SHA: shaA, TreeSHA: shaB},
		Release:      Snapshot{Branch: "prod", SHA: sha2, TreeSHA: shaB},
		MergeBaseSHA: sha1, MergeBaseTreeSHA: shaB,
	}
	stagingMerge := MergeResult{BaseBranch: "stg", BaseSHA: shaA, HeadBranch: "automation/sample", HeadSHA: shaF, MergeSHA: sha4, TreeSHA: sha3}
	pull, err := controller.CreatePromotionPullRequest(context.Background(), PromotionProof{
		Baseline: baseline,
		Staging: DeploymentResult{
			Merge: stagingMerge, WorkflowRuns: []WorkflowRun{{ID: 41, WorkflowID: 262913062, Name: "Deploy API/Client (stg)", Path: ".github/workflows/deploy-stg.yml", HeadBranch: "stg", HeadSHA: sha4, Event: "push", Status: "completed", Conclusion: "success", Attempt: 1}},
			BranchHeadSHA: shaD, DigestCommitSHA: shaD, DigestPaths: []string{"k8s/overlays/stg/kustomization.yaml"},
		},
		ProductPaths: []string{"client/src/page.tsx"}, AcceptanceEvidenceSHA256: acceptanceHash,
	}, PullRequestSpec{Title: "promote sample", Body: "verified staging evidence " + acceptanceHash})
	if err != nil {
		t.Fatalf("CreatePromotionPullRequest() error = %v", err)
	}
	if pull.Number != 9 || pull.HeadRef != "stg" || pull.HeadSHA != shaD || pull.BaseRef != "prod" {
		t.Fatalf("pull = %+v", pull)
	}
	transport.done()
}

func TestMergeFeatureUsesPreviewTreeAndExactTwoParents(t *testing.T) {
	pull := newFeaturePull()
	steps := mergeRequestSteps(t, pull, []string{shaA, shaF})
	controller, transport := newTestController(t, steps, true)

	merged, err := controller.MergeFeaturePullRequest(context.Background(), pull,
		testGateEvidence(pull),
		MergeSpec{CommitTitle: "sample", CommitMessage: "ticket evidence"},
		WaitOptions{PollInterval: time.Millisecond, Timeout: time.Second},
	)
	if err != nil {
		t.Fatalf("MergeFeaturePullRequest() error = %v", err)
	}
	if merged.MergeSHA != sha4 || merged.TreeSHA != sha3 || merged.BaseSHA != shaA || merged.HeadSHA != shaF {
		t.Fatalf("merged = %+v", merged)
	}
	transport.done()
}

func TestMergeDetectsBaseRaceAfterGitHubMergeAPIAndStops(t *testing.T) {
	pull := newFeaturePull()
	steps := mergeRequestSteps(t, pull, []string{shaB, shaF})
	// The post-merge ref read must never be reached after the parent mismatch.
	steps = steps[:len(steps)-1]
	steps = append(steps,
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/pulls/7", body: mergedPullJSON(pull, sha4)},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/git/commits/" + sha4, body: gitCommitJSON(sha4, sha3, shaB, shaF)},
	)
	controller, transport := newTestController(t, steps, true)

	_, err := controller.MergeFeaturePullRequest(context.Background(), pull,
		testGateEvidence(pull),
		MergeSpec{CommitTitle: "sample"},
		WaitOptions{PollInterval: time.Millisecond, Timeout: time.Second},
	)
	if !IsInvariant(err, "merge_toctou_detected") {
		t.Fatalf("MergeFeaturePullRequest() error = %v", err)
	}
	transport.done()
}

func TestPromotionMergeRejectsNonEmptyCheckEvidenceBeforeHTTP(t *testing.T) {
	controller, transport := newTestController(t, nil, true)
	acceptanceHash := strings.Repeat("9", 64)
	pull := PullRequest{
		Number: 9, HTMLURL: "https://github.com/example/consumer/pull/9", Title: "promote sample", Body: "evidence " + acceptanceHash, CreatedAt: time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC),
		HeadRef: "stg", HeadSHA: shaD, BaseRef: "prod", BaseSHA: sha2,
	}

	_, err := controller.MergePromotionPullRequest(context.Background(), pull, CheckEvidence{CheckRunIDs: []int64{1}}, PromotionProof{}, MergeSpec{CommitTitle: "promote"}, WaitOptions{PollInterval: time.Millisecond, Timeout: time.Second}, discardMergeReflection)
	if !IsInvariant(err, "unexpected_promotion_checks") {
		t.Fatalf("MergePromotionPullRequest() error = %v", err)
	}
	transport.done()
}

func TestPromotionMergeRequiresReflectionRecorderBeforeHTTP(t *testing.T) {
	controller, transport := newTestController(t, nil, true)
	pull := PullRequest{
		Number: 9, HeadRef: "stg", HeadSHA: shaD, BaseRef: "prod", BaseSHA: sha2,
	}

	_, err := controller.MergePromotionPullRequest(
		context.Background(), pull, CheckEvidence{}, PromotionProof{}, MergeSpec{CommitTitle: "promote"},
		WaitOptions{PollInterval: time.Millisecond, Timeout: time.Second}, nil,
	)
	if !IsInvariant(err, "missing_merge_reflection_recorder") {
		t.Fatalf("MergePromotionPullRequest() error = %v", err)
	}
	transport.done()
}

func TestPromotionMergeTreatsEmptyChecksAsProofPathNotSuccessShortcut(t *testing.T) {
	controller, transport := newTestController(t, nil, true)
	acceptanceHash := strings.Repeat("9", 64)
	pull := PullRequest{
		Number: 9, HTMLURL: "https://github.com/example/consumer/pull/9", Title: "promote sample", Body: "evidence " + acceptanceHash, CreatedAt: time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC),
		HeadRef: "stg", HeadSHA: shaD, BaseRef: "prod", BaseSHA: sha2,
	}
	proof := PromotionProof{
		Baseline:                 Baseline{Release: Snapshot{SHA: sha2}},
		Staging:                  DeploymentResult{BranchHeadSHA: shaD},
		AcceptanceEvidenceSHA256: acceptanceHash,
	}

	_, err := controller.MergePromotionPullRequest(context.Background(), pull, CheckEvidence{}, proof, MergeSpec{CommitTitle: "promote"}, WaitOptions{PollInterval: time.Millisecond, Timeout: time.Second}, discardMergeReflection)
	if !IsInvariant(err, "invalid_baseline") {
		t.Fatalf("MergePromotionPullRequest() error = %v", err)
	}
	transport.done()
}

func TestPromotionMergeWithZeroChecksRevalidatesProofAndExactMerge(t *testing.T) {
	acceptanceHash := strings.Repeat("9", 64)
	pull := PullRequest{
		Number: 9, HTMLURL: "https://github.com/example/consumer/pull/9", Title: "promote sample", Body: "verified staging evidence " + acceptanceHash,
		CreatedAt: time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC),
		HeadRef:   "stg", HeadSHA: shaD, BaseRef: "prod", BaseSHA: sha2, HeadFullName: "example/consumer",
	}
	proof := testPromotionProof(acceptanceHash)
	previewRef := `{"ref":"refs/pull/9/merge","object":{"type":"commit","sha":"` + sha6 + `"}}`
	steps := promotionProofSteps()
	steps = append(steps,
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/pulls/9", body: pullJSON(9, "open", "stg", shaD, "prod", sha2, boolPointer(true))},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/git/ref/pull/9/merge", body: previewRef},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/git/commits/" + sha6, body: gitCommitJSON(sha6, shaE, sha2, shaD)},
	)
	steps = appendPullRefChecks(steps, pull)
	steps = append(steps,
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/git/ref/pull/9/merge", body: previewRef},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/git/commits/" + sha6, body: gitCommitJSON(sha6, shaE, sha2, shaD)},
		requestStep{method: http.MethodPut, path: "/repos/example/consumer/pulls/9/merge", body: `{"sha":"` + sha5 + `","merged":true,"message":"merged"}`},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/git/commits/" + sha5, body: gitCommitJSON(sha5, shaE, sha2, shaD)},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/prod", body: refJSON("prod", sha5)},
	)
	controller, transport := newTestController(t, steps, true)
	var reflections []MergeReflection

	merged, err := controller.MergePromotionPullRequest(context.Background(), pull, CheckEvidence{}, proof,
		MergeSpec{CommitTitle: "promote", CommitMessage: "acceptance " + acceptanceHash},
		WaitOptions{PollInterval: time.Millisecond, Timeout: time.Second},
		func(reflection MergeReflection) error {
			reflections = append(reflections, reflection)
			return nil
		})
	if err != nil {
		t.Fatalf("MergePromotionPullRequest() error = %v", err)
	}
	if merged.MergeSHA != sha5 || merged.TreeSHA != shaE || merged.BaseSHA != sha2 || merged.HeadSHA != shaD {
		t.Fatalf("merged = %+v", merged)
	}
	if len(reflections) != 1 || reflections[0] != reflectedMerge(pull, sha5) {
		t.Fatalf("reflections = %+v", reflections)
	}
	transport.done()
}

func testPromotionProof(acceptanceHash string) PromotionProof {
	baseline := Baseline{
		Integration:  Snapshot{Branch: "stg", SHA: shaA, TreeSHA: shaB},
		Release:      Snapshot{Branch: "prod", SHA: sha2, TreeSHA: shaB},
		MergeBaseSHA: sha1, MergeBaseTreeSHA: shaB,
	}
	stagingMerge := MergeResult{BaseBranch: "stg", BaseSHA: shaA, HeadBranch: "automation/sample", HeadSHA: shaF, MergeSHA: sha4, TreeSHA: sha3}
	return PromotionProof{
		Baseline: baseline,
		Staging: DeploymentResult{
			Merge: stagingMerge,
			WorkflowRuns: []WorkflowRun{{
				ID: 41, WorkflowID: 262913062, Name: "Deploy API/Client (stg)", Path: ".github/workflows/deploy-stg.yml",
				HeadBranch: "stg", HeadSHA: sha4, Event: "push", Status: "completed", Conclusion: "success", Attempt: 1,
			}},
			BranchHeadSHA: shaD, DigestCommitSHA: shaD, DigestPaths: []string{"k8s/overlays/stg/kustomization.yaml"},
		},
		ProductPaths: []string{"client/src/page.tsx"}, AcceptanceEvidenceSHA256: acceptanceHash,
	}
}

func discardMergeReflection(MergeReflection) error { return nil }

func promotionProofSteps() []requestStep {
	return []requestStep{
		{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/stg", body: refJSON("stg", shaD)},
		{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/prod", body: refJSON("prod", sha2)},
		{method: http.MethodGet, path: "/repos/example/consumer/actions/runs/41", body: directWorkflowRunJSON(41, 262913062, "Deploy API/Client (stg)", "", ".github/workflows/deploy-stg.yml", "stg", sha4, "push")},
		{method: http.MethodGet, path: "/repos/example/consumer/git/commits/" + sha4, body: gitCommitJSON(sha4, sha3, shaA, shaF)},
		{method: http.MethodGet, path: "/repos/example/consumer/git/commits/" + shaD, body: gitCommitJSON(shaD, shaE, sha4)},
		{method: http.MethodGet, path: "/repos/example/consumer/compare/" + shaA + "..." + sha4, body: compareJSON("ahead", shaA, shaB, shaA, shaB, []comparisonFile{{Filename: "client/src/page.tsx", Status: "modified"}})},
		{method: http.MethodGet, path: "/repos/example/consumer/compare/" + sha2 + "..." + shaD, body: compareJSON("diverged", sha2, shaB, sha1, shaB, []comparisonFile{{Filename: "client/src/page.tsx", Status: "modified"}, {Filename: "k8s/overlays/stg/kustomization.yaml", Status: "modified"}})},
		{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/stg", body: refJSON("stg", shaD)},
		{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/prod", body: refJSON("prod", sha2)},
	}
}

func testGateEvidence(pull PullRequest) CheckEvidence {
	return CheckEvidence{PullRequestNumber: pull.Number, HeadSHA: pull.HeadSHA, WorkflowRunIDs: []int64{701, 702}, WorkflowJobIDs: []int64{801, 802, 803}}
}

func newFeaturePull() PullRequest {
	return PullRequest{
		Number: 7, HTMLURL: "https://github.com/example/consumer/pull/7", Title: "sample", Body: "ticket", CreatedAt: time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC),
		HeadRef: "automation/sample", HeadSHA: shaF, BaseRef: "stg", BaseSHA: shaA, HeadFullName: "example/consumer",
	}
}

func mergeRequestSteps(t *testing.T, pull PullRequest, actualParents []string) []requestStep {
	t.Helper()
	previewRef := `{"ref":"refs/pull/` + formatInt(pull.Number) + `/merge","object":{"type":"commit","sha":"` + sha2 + `"}}`
	steps := appendUniquePullStep(nil, pull)
	steps = append(steps,
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/actions/runs/701", body: directWorkflowRunJSON(701, 234224542, "codex-review", pull.Title, ".github/workflows/codex-review.yml", pull.HeadRef, pull.HeadSHA, "pull_request")},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/actions/runs/701/jobs", query: "filter=latest&per_page=100", body: `{"total_count":1,"jobs":[{"id":801,"run_id":701,"name":"gate","status":"completed","conclusion":"success"}]}`},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/actions/runs/702", body: directWorkflowRunJSON(702, 317831412, "QA Gates", pull.Title, ".github/workflows/qa-gates.yml", pull.HeadRef, pull.HeadSHA, "pull_request")},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/actions/runs/702/jobs", query: "filter=latest&per_page=100", body: `{"total_count":2,"jobs":[{"id":802,"run_id":702,"name":"E1 migration target 追随 (blocking)","status":"completed","conclusion":"success"},{"id":803,"run_id":702,"name":"E5 認可ファイルのテスト同伴 (warn only)","status":"completed","conclusion":"success"}]}`},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/pulls/" + formatInt(pull.Number), body: pullJSON(pull.Number, "open", pull.HeadRef, pull.HeadSHA, pull.BaseRef, pull.BaseSHA, boolPointer(true))},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/git/ref/pull/" + formatInt(pull.Number) + "/merge", body: previewRef},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/git/commits/" + sha2, body: gitCommitJSON(sha2, sha3, pull.BaseSHA, pull.HeadSHA)},
	)
	steps = appendPullRefChecks(steps, pull)
	steps = append(steps,
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/git/ref/pull/" + formatInt(pull.Number) + "/merge", body: previewRef},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/git/commits/" + sha2, body: gitCommitJSON(sha2, sha3, pull.BaseSHA, pull.HeadSHA)},
		requestStep{method: http.MethodPut, path: "/repos/example/consumer/pulls/" + formatInt(pull.Number) + "/merge", body: `{"sha":"` + sha4 + `","merged":true,"message":"merged"}`, check: func(_ *http.Request, body []byte) {
			var payload map[string]string
			if json.Unmarshal(body, &payload) != nil || payload["sha"] != pull.HeadSHA || payload["merge_method"] != "merge" {
				t.Fatalf("merge body = %s", body)
			}
		}},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/git/commits/" + sha4, body: gitCommitJSON(sha4, sha3, actualParents...)},
		requestStep{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/" + pull.BaseRef, body: refJSON(pull.BaseRef, sha4)},
	)
	return steps
}

func directWorkflowRunJSON(runID, workflowID int64, name, displayTitle, path, branch, sha, event string) string {
	return `{"id":` + formatInt(runID) + `,"workflow_id":` + formatInt(workflowID) + `,"name":"` + name + `","display_title":"` + displayTitle + `","html_url":"https://github.example/run","head_branch":"` + branch + `","head_sha":"` + sha + `","event":"` + event + `","status":"completed","conclusion":"success","path":"` + path + `","run_attempt":1,"created_at":"2026-08-03T00:01:00Z","updated_at":"2026-08-03T00:02:00Z","repository":{"full_name":"example/consumer"},"head_repository":{"full_name":"example/consumer"}}`
}
