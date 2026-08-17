package githubapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

const testToken = "GITHUB-TOKEN-SENTINEL"

var (
	shaA = strings.Repeat("a", 40)
	shaB = strings.Repeat("b", 40)
	shaC = strings.Repeat("c", 40)
	shaD = strings.Repeat("d", 40)
	shaE = strings.Repeat("e", 40)
	shaF = strings.Repeat("f", 40)
	sha1 = strings.Repeat("1", 40)
	sha2 = strings.Repeat("2", 40)
	sha3 = strings.Repeat("3", 40)
	sha4 = strings.Repeat("4", 40)
	sha5 = strings.Repeat("5", 40)
	sha6 = strings.Repeat("6", 40)
)

type requestStep struct {
	method  string
	path    string
	query   string
	status  int
	body    string
	err     error
	readErr error
	check   func(*http.Request, []byte)
}

type errorReadCloser struct{ err error }

func (r errorReadCloser) Read([]byte) (int, error) { return 0, r.err }
func (errorReadCloser) Close() error               { return nil }

type scriptedTransport struct {
	t     *testing.T
	steps []requestStep
	index int
}

func (s *scriptedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	s.t.Helper()
	if s.index >= len(s.steps) {
		s.t.Fatalf("unexpected request %s %s", request.Method, request.URL.String())
	}
	step := s.steps[s.index]
	s.index++
	if request.Method != step.method || request.URL.Path != step.path || request.URL.RawQuery != step.query {
		s.t.Fatalf("request[%d] = %s %s?%s, want %s %s?%s", s.index-1, request.Method, request.URL.Path, request.URL.RawQuery, step.method, step.path, step.query)
	}
	if request.URL.Scheme != "https" || request.URL.Host != "api.github.com" {
		s.t.Fatalf("request escaped fixed GitHub origin: %s", request.URL.String())
	}
	if request.Header.Get("Authorization") != "Bearer "+testToken {
		s.t.Fatal("installation token was not sent")
	}
	var body []byte
	if request.Body != nil {
		var err error
		body, err = io.ReadAll(request.Body)
		if err != nil {
			s.t.Fatalf("read request body: %v", err)
		}
	}
	if step.check != nil {
		step.check(request, body)
	}
	if step.err != nil {
		return nil, step.err
	}
	status := step.status
	if status == 0 {
		status = http.StatusOK
	}
	responseBody := io.ReadCloser(io.NopCloser(strings.NewReader(step.body)))
	if step.readErr != nil {
		responseBody = errorReadCloser{err: step.readErr}
	}
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       responseBody,
	}, nil
}

func (s *scriptedTransport) done() {
	s.t.Helper()
	if s.index != len(s.steps) {
		s.t.Fatalf("used %d/%d scripted requests", s.index, len(s.steps))
	}
}

func newTestController(t *testing.T, steps []requestStep, markVerified bool) (*Controller, *scriptedTransport) {
	t.Helper()
	transport := &scriptedTransport{t: t, steps: steps}
	client, err := NewClient(Config{
		Owner:            "example",
		Repository:       "consumer",
		RepositoryID:     1101796955,
		Token:            testToken,
		Timeout:          time.Second,
		MaxResponseBytes: 2 * 1024 * 1024,
	}, transport)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	client.sleep = func(context.Context, time.Duration) error { return nil }
	if markVerified {
		client.markVerified()
	}
	controller, err := NewController(client, testContract())
	if err != nil {
		t.Fatalf("NewController() error = %v", err)
	}
	return controller, transport
}

func testContract() Contract {
	return Contract{
		IntegrationBranch: "stg",
		ReleaseBranch:     "prod",
		DefaultBranch:     "prod",
		MergeSettings: MergeSettings{
			AllowMergeCommit: true, AllowAutoMerge: true,
			SquashMergeCommitTitle: "COMMIT_OR_PR_TITLE", SquashMergeCommitMessage: "COMMIT_MESSAGES",
			MergeCommitTitle: "MERGE_MESSAGE", MergeCommitMessage: "PR_TITLE",
		},
		FeatureWorkflows: []WorkflowContract{
			{ID: 234224542, Name: "codex-review", Path: ".github/workflows/codex-review.yml", State: "active", RequiredJobs: []string{"gate"}},
			{ID: 317831412, Name: "QA Gates", Path: ".github/workflows/qa-gates.yml", State: "active", RequiredJobs: []string{"E1 migration target 追随 (blocking)", "E5 認可ファイルのテスト同伴 (warn only)"}},
		},
		StagingWorkflow: WorkflowContract{ID: 262913062, Name: "Deploy API/Client (stg)", Path: ".github/workflows/deploy-stg.yml", State: "active"},
		ProductionWorkflows: []WorkflowContract{
			{ID: 231779190, Name: "Deploy API/Client", Path: ".github/workflows/deploy.yml", State: "active"},
			{ID: 307681769, Name: "prod-overlay-guard", Path: ".github/workflows/prod-overlay-guard.yml", State: "active"},
		},
	}
}

func refJSON(branch, sha string) string {
	return `{"ref":"refs/heads/` + branch + `","object":{"type":"commit","sha":"` + sha + `"}}`
}

func gitCommitJSON(sha, tree string, parents ...string) string {
	values := make([]string, len(parents))
	for index, parent := range parents {
		values[index] = `{"sha":"` + parent + `"}`
	}
	return `{"sha":"` + sha + `","tree":{"sha":"` + tree + `"},"parents":[` + strings.Join(values, ",") + `]}`
}

func pullJSON(number int64, state, headRef, headSHA, baseRef, baseSHA string, mergeable *bool) string {
	mergeableJSON := "null"
	if mergeable != nil {
		if *mergeable {
			mergeableJSON = "true"
		} else {
			mergeableJSON = "false"
		}
	}
	title := "sample"
	body := "ticket"
	if number == 9 {
		title = "promote sample"
		body = "verified staging evidence " + strings.Repeat("9", 64)
	}
	return `{"number":` + formatInt(number) + `,"html_url":"https://github.com/example/consumer/pull/` + formatInt(number) + `","title":"` + title + `","body":"` + body + `","created_at":"2026-08-03T00:00:00Z","state":"` + state + `","draft":false,"mergeable":` + mergeableJSON + `,"head":{"ref":"` + headRef + `","sha":"` + headSHA + `","repo":{"full_name":"example/consumer"}},"base":{"ref":"` + baseRef + `","sha":"` + baseSHA + `"}}`
}

func boolPointer(value bool) *bool { return &value }

func TestVerifyPinsRepositoryIdentityAndExistingWorkflows(t *testing.T) {
	steps := []requestStep{
		{method: http.MethodGet, path: "/repos/example/consumer", body: `{"id":1101796955,"full_name":"example/consumer","default_branch":"prod","archived":false,"disabled":false,"allow_merge_commit":true,"allow_squash_merge":false,"allow_rebase_merge":false,"allow_auto_merge":true,"allow_update_branch":false,"delete_branch_on_merge":false,"use_squash_pr_title_as_default":false,"squash_merge_commit_title":"COMMIT_OR_PR_TITLE","squash_merge_commit_message":"COMMIT_MESSAGES","merge_commit_title":"MERGE_MESSAGE","merge_commit_message":"PR_TITLE","web_commit_signoff_required":false}`},
		{method: http.MethodGet, path: "/repos/example/consumer/actions/workflows/234224542", body: `{"id":234224542,"name":"codex-review","path":".github/workflows/codex-review.yml","state":"active"}`},
		{method: http.MethodGet, path: "/repos/example/consumer/actions/workflows/317831412", body: `{"id":317831412,"name":"QA Gates","path":".github/workflows/qa-gates.yml","state":"active"}`},
		{method: http.MethodGet, path: "/repos/example/consumer/actions/workflows/262913062", body: `{"id":262913062,"name":"Deploy API/Client (stg)","path":".github/workflows/deploy-stg.yml","state":"active"}`},
		{method: http.MethodGet, path: "/repos/example/consumer/actions/workflows/231779190", body: `{"id":231779190,"name":"Deploy API/Client","path":".github/workflows/deploy.yml","state":"active"}`},
		{method: http.MethodGet, path: "/repos/example/consumer/actions/workflows/307681769", body: `{"id":307681769,"name":"prod-overlay-guard","path":".github/workflows/prod-overlay-guard.yml","state":"active"}`},
	}
	controller, transport := newTestController(t, steps, false)

	verified, err := controller.Verify(context.Background())
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if verified.Repository.ID != 1101796955 || verified.Repository.DefaultBranch != "prod" || verified.StagingWorkflow.ID != 262913062 || len(verified.FeatureWorkflows) != 2 || len(verified.ProductionWorkflows) != 2 {
		t.Fatalf("verified = %+v", verified)
	}
	if err := controller.client.requireVerified(); err != nil {
		t.Fatalf("client not marked verified: %v", err)
	}
	transport.done()
}

func TestVerifyRejectsTransferredOrWrongRepositoryBeforeMutation(t *testing.T) {
	steps := []requestStep{{method: http.MethodGet, path: "/repos/example/consumer", body: `{"id":999,"full_name":"attacker/consumer"}`}}
	controller, transport := newTestController(t, steps, false)

	_, err := controller.Verify(context.Background())
	if !IsInvariant(err, "repository_identity_mismatch") {
		t.Fatalf("Verify() error = %v", err)
	}
	if err := controller.client.requireVerified(); !IsInvariant(err, "repository_not_verified") {
		t.Fatalf("requireVerified() error = %v", err)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatal("error leaked installation token")
	}
	transport.done()
}

func TestVerifyRejectsRepositoryMergeSettingDrift(t *testing.T) {
	steps := []requestStep{{method: http.MethodGet, path: "/repos/example/consumer", body: `{"id":1101796955,"full_name":"example/consumer","default_branch":"prod","archived":false,"disabled":false,"allow_merge_commit":true,"allow_squash_merge":true,"allow_rebase_merge":false,"allow_auto_merge":true,"allow_update_branch":false,"delete_branch_on_merge":false,"use_squash_pr_title_as_default":false,"squash_merge_commit_title":"COMMIT_OR_PR_TITLE","squash_merge_commit_message":"COMMIT_MESSAGES","merge_commit_title":"MERGE_MESSAGE","merge_commit_message":"PR_TITLE","web_commit_signoff_required":false}`}}
	controller, transport := newTestController(t, steps, false)

	_, err := controller.Verify(context.Background())
	if !IsInvariant(err, "repository_settings_mismatch") {
		t.Fatalf("Verify() error = %v", err)
	}
	transport.done()
}

// A read-scoped token is not shown the merge settings at all (measured
// 2026-08-06 against the live API). A merging run must say so by name instead
// of comparing absent fields as false.
func TestVerifyNamesInvisibleMergeSettingsInsteadOfMisreadingThem(t *testing.T) {
	steps := []requestStep{{method: http.MethodGet, path: "/repos/example/consumer", body: `{"id":1101796955,"full_name":"example/consumer","default_branch":"prod","archived":false,"disabled":false}`}}
	controller, transport := newTestController(t, steps, false)

	_, err := controller.Verify(context.Background())
	if !IsInvariant(err, "merge_settings_invisible_to_token") {
		t.Fatalf("Verify() error = %v", err)
	}
	transport.done()
}

// A run that only proposes a pull request never merges, so the same invisible
// fields must not stop it; identity, liveness and the workflows still hold.
func TestProposalVerifyPassesWithoutTheMergeSettingsAReadTokenCannotSee(t *testing.T) {
	steps := []requestStep{
		{method: http.MethodGet, path: "/repos/example/consumer", body: `{"id":1101796955,"full_name":"example/consumer","default_branch":"prod","archived":false,"disabled":false}`},
		{method: http.MethodGet, path: "/repos/example/consumer/actions/workflows/234224542", body: `{"id":234224542,"name":"codex-review","path":".github/workflows/codex-review.yml","state":"active"}`},
		{method: http.MethodGet, path: "/repos/example/consumer/actions/workflows/317831412", body: `{"id":317831412,"name":"QA Gates","path":".github/workflows/qa-gates.yml","state":"active"}`},
		{method: http.MethodGet, path: "/repos/example/consumer/actions/workflows/262913062", body: `{"id":262913062,"name":"Deploy API/Client (stg)","path":".github/workflows/deploy-stg.yml","state":"active"}`},
		{method: http.MethodGet, path: "/repos/example/consumer/actions/workflows/231779190", body: `{"id":231779190,"name":"Deploy API/Client","path":".github/workflows/deploy.yml","state":"active"}`},
		{method: http.MethodGet, path: "/repos/example/consumer/actions/workflows/307681769", body: `{"id":307681769,"name":"prod-overlay-guard","path":".github/workflows/prod-overlay-guard.yml","state":"active"}`},
	}
	controller, transport := newTestController(t, steps, false)
	controller.promotesChanges = false

	verified, err := controller.Verify(context.Background())
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if verified.Repository.ID != 1101796955 || verified.StagingWorkflow.ID != 262913062 {
		t.Fatalf("verified = %+v", verified)
	}
	transport.done()
}

func TestVerifyBaselineRequiresEqualContentTreesAndConsistentMergeBase(t *testing.T) {
	steps := []requestStep{
		{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/stg", body: refJSON("stg", shaA)},
		{method: http.MethodGet, path: "/repos/example/consumer/git/commits/" + shaA, body: gitCommitJSON(shaA, shaB, sha1)},
		{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/prod", body: refJSON("prod", sha2)},
		{method: http.MethodGet, path: "/repos/example/consumer/git/commits/" + sha2, body: gitCommitJSON(sha2, shaB, sha1)},
		{method: http.MethodGet, path: "/repos/example/consumer/compare/" + sha2 + "..." + shaA, body: compareJSON("diverged", sha2, shaB, sha1, shaB, nil)},
		{method: http.MethodGet, path: "/repos/example/consumer/git/commits/" + sha1, body: gitCommitJSON(sha1, shaB)},
	}
	controller, transport := newTestController(t, steps, true)

	baseline, err := controller.VerifyBaseline(context.Background())
	if err != nil {
		t.Fatalf("VerifyBaseline() error = %v", err)
	}
	if baseline.Integration.SHA != shaA || baseline.Release.SHA != sha2 || baseline.Integration.TreeSHA != baseline.Release.TreeSHA || baseline.MergeBaseSHA != sha1 {
		t.Fatalf("baseline = %+v", baseline)
	}
	transport.done()
}

// A proposal-only run works from staging as it stands; staging running ahead
// of release is this rail's normal state between promotion batches, and a
// proposal must not be refused for it (measured 2026-08-07 on a live ticket).
func TestProposalBaselineAcceptsAStagingBranchAheadOfRelease(t *testing.T) {
	treeC := strings.Repeat("d", 40)
	steps := []requestStep{
		{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/stg", body: refJSON("stg", shaC)},
		{method: http.MethodGet, path: "/repos/example/consumer/git/commits/" + shaC, body: gitCommitJSON(shaC, treeC, sha2)},
		{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/prod", body: refJSON("prod", sha2)},
		{method: http.MethodGet, path: "/repos/example/consumer/git/commits/" + sha2, body: gitCommitJSON(sha2, shaB, sha1)},
		{method: http.MethodGet, path: "/repos/example/consumer/compare/" + sha2 + "..." + shaC, body: compareJSON("ahead", sha2, shaB, sha2, shaB, []comparisonFile{{Filename: "internal/x.go"}})},
		{method: http.MethodGet, path: "/repos/example/consumer/git/commits/" + sha2, body: gitCommitJSON(sha2, shaB, sha1)},
	}
	controller, transport := newTestController(t, steps, true)
	controller.promotesChanges = false

	baseline, err := controller.VerifyBaseline(context.Background())
	if err != nil {
		t.Fatalf("VerifyBaseline() error = %v", err)
	}
	if baseline.Integration.SHA != shaC || baseline.Release.SHA != sha2 || baseline.Integration.TreeSHA == baseline.Release.TreeSHA {
		t.Fatalf("baseline = %+v", baseline)
	}
	transport.done()
}

func TestVerifyBaselineStopsWhileStagingContainsUnpromotedContent(t *testing.T) {
	steps := []requestStep{
		{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/stg", body: refJSON("stg", shaA)},
		{method: http.MethodGet, path: "/repos/example/consumer/git/commits/" + shaA, body: gitCommitJSON(shaA, shaB, sha1)},
		{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/prod", body: refJSON("prod", sha2)},
		{method: http.MethodGet, path: "/repos/example/consumer/git/commits/" + sha2, body: gitCommitJSON(sha2, shaC, sha1)},
	}
	controller, transport := newTestController(t, steps, true)

	_, err := controller.VerifyBaseline(context.Background())
	if !IsInvariant(err, "baseline_trees_differ") {
		t.Fatalf("VerifyBaseline() error = %v", err)
	}
	transport.done()
}

func compareJSON(status, baseSHA, baseTree, mergeBaseSHA, mergeBaseTree string, files []comparisonFile) string {
	encodedFiles, _ := json.Marshal(files)
	aheadBy, behindBy, totalCommits := "1", "0", "1"
	if status == "diverged" {
		behindBy, totalCommits = "1", "2"
	} else if status == "behind" {
		aheadBy, behindBy = "0", "1"
	} else if status == "identical" {
		aheadBy, totalCommits = "0", "0"
	}
	return `{"status":"` + status + `","ahead_by":` + aheadBy + `,"behind_by":` + behindBy + `,"total_commits":` + totalCommits + `,"base_commit":{"sha":"` + baseSHA + `","commit":{"tree":{"sha":"` + baseTree + `"}}},"merge_base_commit":{"sha":"` + mergeBaseSHA + `","commit":{"tree":{"sha":"` + mergeBaseTree + `"}}},"files":` + string(encodedFiles) + `}`
}

func TestPublishFeatureBuildsOneParentCommitAndOnlyCandidatePaths(t *testing.T) {
	steps := []requestStep{
		{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/prod", body: refJSON("prod", sha2)},
		{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/stg", body: refJSON("stg", shaA)},
		{method: http.MethodGet, path: "/repos/example/consumer/git/commits/" + shaA, body: gitCommitJSON(shaA, shaB, sha1)},
		{method: http.MethodGet, path: "/repos/example/consumer/git/trees/" + shaB, query: "recursive=1", body: `{"sha":"` + shaB + `","truncated":false,"tree":[{"path":"client/src/page.tsx","mode":"100644","type":"blob","sha":"` + shaC + `"}]}`},
		{method: http.MethodPost, path: "/repos/example/consumer/git/blobs", status: http.StatusCreated, body: `{"sha":"` + shaD + `"}`, check: func(_ *http.Request, body []byte) {
			var payload map[string]string
			if json.Unmarshal(body, &payload) != nil || payload["encoding"] != "base64" || payload["content"] != "bmV3IGNvbnRlbnQ=" {
				t.Fatalf("blob body = %s", body)
			}
		}},
		{method: http.MethodPost, path: "/repos/example/consumer/git/trees", status: http.StatusCreated, body: `{"sha":"` + shaE + `"}`, check: func(_ *http.Request, body []byte) {
			var payload struct {
				BaseTree string `json:"base_tree"`
				Tree     []struct {
					Path string `json:"path"`
					Mode string `json:"mode"`
					Type string `json:"type"`
					SHA  string `json:"sha"`
				} `json:"tree"`
			}
			if json.Unmarshal(body, &payload) != nil || payload.BaseTree != shaB || len(payload.Tree) != 1 || payload.Tree[0].Path != "client/src/page.tsx" || payload.Tree[0].Mode != "100644" || payload.Tree[0].Type != "blob" || payload.Tree[0].SHA != shaD {
				t.Fatalf("tree body = %s", body)
			}
		}},
		{method: http.MethodPost, path: "/repos/example/consumer/git/commits", status: http.StatusCreated, body: `{"sha":"` + shaF + `"}`, check: func(_ *http.Request, body []byte) {
			var payload struct {
				Message string   `json:"message"`
				Tree    string   `json:"tree"`
				Parents []string `json:"parents"`
			}
			if json.Unmarshal(body, &payload) != nil || payload.Message != "sample ticket" || payload.Tree != shaE || len(payload.Parents) != 1 || payload.Parents[0] != shaA {
				t.Fatalf("commit body = %s", body)
			}
		}},
		{method: http.MethodGet, path: "/repos/example/consumer/git/commits/" + shaF, body: gitCommitJSON(shaF, shaE, shaA)},
		{method: http.MethodGet, path: "/repos/example/consumer/compare/" + shaA + "..." + shaF, body: `{"status":"ahead","ahead_by":1,"behind_by":0,"total_commits":1,"files":[{"filename":"client/src/page.tsx","status":"modified"}]}`},
		{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/stg", body: refJSON("stg", shaA)},
		{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/prod", body: refJSON("prod", sha2)},
		{method: http.MethodPost, path: "/repos/example/consumer/git/refs", status: http.StatusCreated, body: `{"ref":"refs/heads/automation/sample","object":{"type":"commit","sha":"` + shaF + `"}}`, check: func(_ *http.Request, body []byte) {
			var payload map[string]string
			if json.Unmarshal(body, &payload) != nil || payload["ref"] != "refs/heads/automation/sample" || payload["sha"] != shaF {
				t.Fatalf("ref body = %s", body)
			}
		}},
	}
	controller, transport := newTestController(t, steps, true)
	baseline := Baseline{
		Integration:  Snapshot{Branch: "stg", SHA: shaA, TreeSHA: shaB},
		Release:      Snapshot{Branch: "prod", SHA: sha2, TreeSHA: shaB},
		MergeBaseSHA: sha1, MergeBaseTreeSHA: shaB,
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
		t.Fatalf("PublishFeature() error = %v", err)
	}
	if published.HeadSHA != shaF || published.TreeSHA != shaE || len(published.Paths) != 1 || published.Paths[0] != "client/src/page.tsx" {
		t.Fatalf("published = %+v", published)
	}
	transport.done()
}

func TestPublishFeatureRejectsPathOutsideExplicitProductPrefixWithoutHTTP(t *testing.T) {
	controller, transport := newTestController(t, nil, true)
	baseline := Baseline{Integration: Snapshot{Branch: "stg", SHA: shaA, TreeSHA: shaB}, Release: Snapshot{Branch: "prod", SHA: sha2, TreeSHA: shaB}, MergeBaseSHA: sha1, MergeBaseTreeSHA: shaB}
	_, err := controller.PublishFeature(context.Background(), baseline, FeatureSpec{
		Branch:              "automation/sample",
		CommitMessage:       "sample ticket",
		AllowedPathPrefixes: []string{"client/src/"},
		Files:               []FileUpdate{{Path: ".github/workflows/new.yml", Content: []byte("x"), ExpectedBlobSHA: shaC}},
	})
	if !IsInvariant(err, "feature_path_not_allowed") {
		t.Fatalf("PublishFeature() error = %v", err)
	}
	transport.done()
}

func TestPublishFeatureRejectsSymlinkOrNonRegularModeBeforeCreatingObjects(t *testing.T) {
	steps := []requestStep{
		{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/prod", body: refJSON("prod", sha2)},
		{method: http.MethodGet, path: "/repos/example/consumer/git/ref/heads/stg", body: refJSON("stg", shaA)},
		{method: http.MethodGet, path: "/repos/example/consumer/git/commits/" + shaA, body: gitCommitJSON(shaA, shaB, sha1)},
		{method: http.MethodGet, path: "/repos/example/consumer/git/trees/" + shaB, query: "recursive=1", body: `{"sha":"` + shaB + `","truncated":false,"tree":[{"path":"client/src/page.tsx","mode":"120000","type":"blob","sha":"` + shaC + `"}]}`},
	}
	controller, transport := newTestController(t, steps, true)
	baseline := Baseline{Integration: Snapshot{Branch: "stg", SHA: shaA, TreeSHA: shaB}, Release: Snapshot{Branch: "prod", SHA: sha2, TreeSHA: shaB}, MergeBaseSHA: sha1, MergeBaseTreeSHA: shaB}

	_, err := controller.PublishFeature(context.Background(), baseline, FeatureSpec{
		Branch: "automation/sample", CommitMessage: "sample ticket", AllowedPathPrefixes: []string{"client/src/"},
		Files: []FileUpdate{{Path: "client/src/page.tsx", Content: []byte("new"), ExpectedBlobSHA: shaC}},
	})
	if !IsInvariant(err, "source_blob_changed") {
		t.Fatalf("PublishFeature() error = %v", err)
	}
	transport.done()
}
