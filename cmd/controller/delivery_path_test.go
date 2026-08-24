package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/githubapi"
	"automation.internal/ticket-ingress/internal/worker"
)

const (
	deliveryPullNumber   = int64(41)
	deliveryFeatureTrail = "### 実装とレビューの経過\n- 1 周目: 収束\n"
)

// The objects the fake GitHub hands back for one delivery. They are distinct
// from the repeated-digit identifiers the sealed worker fixtures already use,
// so a value that travels the wrong way is visible in a failure message.
var (
	deliveryBlobSHA        = strings.Repeat("1a", 20)
	deliveryFeatureTreeSHA = strings.Repeat("2b", 20)
	deliveryFeatureHeadSHA = strings.Repeat("3c", 20)
	deliveryPreviewSHA     = strings.Repeat("4d", 20)
	deliveryMergeSHA       = strings.Repeat("5e", 20)
	deliveryPullCreatedAt  = time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
)

// deliveryFixture is one accepted delivery: the sealed worker artifacts, the
// baseline they were built on, and the files the four feature commands read.
type deliveryFixture struct {
	config     worker.Config
	consumer   worker.ConsumerConfig
	request    worker.TicketRequest
	source     worker.SourceSnapshot
	candidate  worker.Candidate
	reviews    []worker.Review
	decision   worker.StageDecision
	validation worker.ValidationEvidence
	baseline   baselineArtifact
	binding    deliveryBinding
	trail      string

	directory      string
	outputs        string
	ticketPath     string
	sourcePath     string
	candidatePath  string
	decisionPath   string
	validationPath string
	baselinePath   string
	trailPath      string
	reviewPaths    []string
}

func newDeliveryFixture(t *testing.T) deliveryFixture {
	t.Helper()
	enterRepositoryRoot(t)
	sealed := newPromotionRejectionFixture(t)
	config, err := loadFixedConfig(controllerConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	binding := newDeliveryBinding(sealed.request, sealed.source, sealed.candidate, sealed.decision, sealed.validation)
	if err := binding.validate(sealed.request, config); err != nil {
		t.Fatalf("delivery binding fixture is invalid: %v", err)
	}
	directory := t.TempDir()
	outputs := filepath.Join(directory, "outputs")
	if err := os.MkdirAll(outputs, 0o700); err != nil {
		t.Fatal(err)
	}
	trailPath := filepath.Join(directory, "trail.txt")
	if err := os.WriteFile(trailPath, []byte(deliveryFeatureTrail), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture := deliveryFixture{
		config: config, consumer: config.Consumers[0], request: sealed.request, source: sealed.source,
		candidate: sealed.candidate, reviews: sealed.reviews, decision: sealed.decision,
		validation: sealed.validation, baseline: sealed.baseline, binding: binding,
		trail: deliveryFeatureTrail, directory: directory, outputs: outputs, trailPath: trailPath,
	}
	fixture.ticketPath = writePromotionFixtureJSON(t, directory, "ticket.json", fixture.request)
	fixture.sourcePath = writePromotionFixtureJSON(t, directory, "source.json", fixture.source)
	fixture.candidatePath = writePromotionFixtureJSON(t, directory, "candidate.json", fixture.candidate)
	fixture.decisionPath = writePromotionFixtureJSON(t, directory, "decision.json", fixture.decision)
	fixture.validationPath = writePromotionFixtureJSON(t, directory, "validation.json", fixture.validation)
	fixture.baselinePath = writePromotionFixtureJSON(t, directory, "baseline.json", fixture.baseline)
	fixture.reviewPaths = make([]string, len(fixture.reviews))
	for index, review := range fixture.reviews {
		fixture.reviewPaths[index] = writePromotionFixtureJSON(t, directory, "review-"+string(rune('a'+index))+".json", review)
	}
	return fixture
}

// output names a destination inside the fixture directory that no command has
// written yet; the controller refuses to overwrite an existing artifact.
func (f deliveryFixture) output(name string) string {
	return filepath.Join(f.outputs, name)
}

func (f deliveryFixture) gateFlags() []string {
	flags := []string{
		"--source", f.sourcePath, "--candidate", f.candidatePath,
		"--decision", f.decisionPath, "--validation", f.validationPath,
	}
	for _, path := range f.reviewPaths {
		flags = append(flags, "--review", path)
	}
	return flags
}

func (f deliveryFixture) publishArguments(output string) []string {
	arguments := []string{"publish-feature", "--config", controllerConfigPath, "--ticket", f.ticketPath}
	arguments = append(arguments, f.gateFlags()...)
	return append(arguments, "--baseline", f.baselinePath, "--out", output)
}

func (f deliveryFixture) createPullRequestArguments(feature, output string) []string {
	return []string{
		"create-feature-pr", "--config", controllerConfigPath, "--ticket", f.ticketPath,
		"--feature", feature, "--trail", f.trailPath, "--out", output,
	}
}

func (f deliveryFixture) waitArguments(featurePR, output string) []string {
	return []string{
		"wait-feature", "--config", controllerConfigPath, "--ticket", f.ticketPath,
		"--feature-pr", featurePR, "--out", output,
	}
}

func (f deliveryFixture) mergeArguments(featurePR, checks, output string) []string {
	return []string{
		"merge-feature", "--config", controllerConfigPath, "--ticket", f.ticketPath,
		"--feature-pr", featurePR, "--checks", checks, "--out", output,
	}
}

// publishedFeature is the branch the fake GitHub reports after a publish, so a
// command can be exercised on its own without replaying the whole chain.
func (f deliveryFixture) publishedFeature() githubapi.PublishedFeature {
	return githubapi.PublishedFeature{
		Base:    f.baseline.Baseline.Integration,
		Branch:  featureBranch(f.binding),
		HeadSHA: deliveryFeatureHeadSHA,
		TreeSHA: deliveryFeatureTreeSHA,
		Paths:   slices.Clone(f.binding.ProductPaths),
	}
}

func (f deliveryFixture) pullRequest() githubapi.PullRequest {
	return githubapi.PullRequest{
		Number:       deliveryPullNumber,
		HTMLURL:      "https://github.com/" + f.binding.Repository + "/pull/" + decimal(deliveryPullNumber),
		Title:        "[Codex] " + f.binding.IssueKey,
		Body:         featurePullRequestSpec(f.binding, f.trail).Body,
		CreatedAt:    deliveryPullCreatedAt,
		HeadRef:      featureBranch(f.binding),
		HeadSHA:      deliveryFeatureHeadSHA,
		BaseRef:      f.consumer.IntegrationBranch,
		BaseSHA:      f.baseline.Baseline.Integration.SHA,
		HeadFullName: f.binding.Repository,
	}
}

func (f deliveryFixture) checkEvidence() githubapi.CheckEvidence {
	runIDs, jobIDs := deliveryCheckIdentifiers()
	flattened := make([]int64, 0, len(jobIDs))
	for _, ids := range jobIDs {
		flattened = append(flattened, ids...)
	}
	return githubapi.CheckEvidence{
		PullRequestNumber: deliveryPullNumber, HeadSHA: deliveryFeatureHeadSHA,
		WorkflowRunIDs: runIDs, WorkflowJobIDs: flattened,
	}
}

// writeDeliveryArtifact seals one controller artifact the way the preceding
// command would have written it.
func writeDeliveryArtifact[T any](t *testing.T, fixture deliveryFixture, name, kind string, binding deliveryBinding, payload T) string {
	t.Helper()
	artifact, err := newDeliveryArtifact(kind, binding, payload)
	if err != nil {
		t.Fatal(err)
	}
	return writePromotionFixtureJSON(t, fixture.directory, name, artifact)
}

// deliveryCheckIdentifiers assigns the workflow run and job identifiers the
// fake GitHub reports, in the order the controller records them: one run per
// feature workflow, and that workflow's required jobs in ascending order.
func deliveryCheckIdentifiers() ([]int64, [][]int64) {
	workflows := testPrimaryConsumer().Contract().FeatureWorkflows
	runIDs := make([]int64, 0, len(workflows))
	jobIDs := make([][]int64, 0, len(workflows))
	next := int64(2001)
	for index, workflow := range workflows {
		runIDs = append(runIDs, 1001+int64(index))
		ids := make([]int64, 0, len(workflow.RequiredJobs))
		for range workflow.RequiredJobs {
			ids = append(ids, next)
			next++
		}
		jobIDs = append(jobIDs, ids)
	}
	return runIDs, jobIDs
}

func deliveryEnvironment(name string) string {
	if name == githubTokenEnvironment {
		return fakeGitHubToken
	}
	return ""
}

type recordedCall struct {
	Method string
	Path   string
	Query  url.Values
	Body   map[string]any
}

// deliveryTransport answers the GitHub calls the four feature commands make
// for one delivery and advances the repository as they mutate it: the feature
// ref appears once it is created, and the integration branch moves to the
// merge commit once the merge is accepted.
type deliveryTransport struct {
	mu    sync.Mutex
	calls []recordedCall

	repository        string
	repositoryPath    string
	integrationBranch string
	releaseBranch     string

	integrationSHA string
	baseTreeSHA    string
	sourcePath     string
	sourceBlobSHA  string

	featureBranch    string
	featureRefExists bool

	pullBaseSHA string
	pullTitle   string
	pullBody    string
	pullCreated string

	previewTreeSHA string
	merged         bool

	runIDs             []int64
	jobIDs             [][]int64
	runCreated         string
	runUpdated         string
	workflowConclusion string

	// failures forces a status for one "METHOD path", standing in for a
	// GitHub refusal the command must report rather than paper over.
	failures map[string]int
}

func newDeliveryTransport(fixture deliveryFixture) *deliveryTransport {
	runIDs, jobIDs := deliveryCheckIdentifiers()
	consumer := fixture.consumer
	return &deliveryTransport{
		repository:         consumer.Repository,
		repositoryPath:     "/repos/" + consumer.Repository,
		integrationBranch:  consumer.IntegrationBranch,
		releaseBranch:      consumer.ReleaseBranch,
		integrationSHA:     fixture.baseline.Baseline.Integration.SHA,
		baseTreeSHA:        fixture.baseline.Baseline.Integration.TreeSHA,
		sourcePath:         fixture.source.Files[0].Path,
		sourceBlobSHA:      fixture.source.Files[0].GitBlobSHA,
		featureBranch:      featureBranch(fixture.binding),
		pullBaseSHA:        fixture.baseline.Baseline.Integration.SHA,
		pullTitle:          "[Codex] " + fixture.binding.IssueKey,
		pullBody:           featurePullRequestSpec(fixture.binding, fixture.trail).Body,
		pullCreated:        deliveryPullCreatedAt.Format(time.RFC3339),
		previewTreeSHA:     deliveryFeatureTreeSHA,
		runIDs:             runIDs,
		jobIDs:             jobIDs,
		runCreated:         deliveryPullCreatedAt.Add(time.Minute).Format(time.RFC3339),
		runUpdated:         deliveryPullCreatedAt.Add(2 * time.Minute).Format(time.RFC3339),
		workflowConclusion: "success",
		failures:           make(map[string]int),
	}
}

func (transport *deliveryTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme != "https" || request.URL.Host != "api.github.com" ||
		request.Header.Get("Authorization") != "Bearer "+fakeGitHubToken {
		return jsonResponse(http.StatusBadRequest, map[string]string{"message": "invalid request"}), nil
	}
	call := recordedCall{
		Method: request.Method, Path: request.URL.Path,
		Query: request.URL.Query(), Body: decodeRequestBody(request),
	}
	transport.mu.Lock()
	defer transport.mu.Unlock()
	transport.calls = append(transport.calls, call)
	if status, forced := transport.failures[call.Method+" "+call.Path]; forced {
		return jsonResponse(status, map[string]string{"message": "refused by test"}), nil
	}
	if !strings.HasPrefix(call.Path, transport.repositoryPath) {
		return deliveryNotFound(), nil
	}
	route := strings.TrimPrefix(call.Path, transport.repositoryPath)
	switch call.Method {
	case http.MethodGet:
		return transport.read(route)
	case http.MethodPost:
		return transport.create(route)
	case http.MethodPut:
		return transport.replace(route)
	default:
		return jsonResponse(http.StatusMethodNotAllowed, map[string]string{"message": "unexpected method"}), nil
	}
}

func (transport *deliveryTransport) read(route string) (*http.Response, error) {
	switch {
	case route == "":
		return jsonResponse(http.StatusOK, consumerRepositoryPayload()), nil
	case strings.HasPrefix(route, "/actions/workflows/") && strings.HasSuffix(route, "/runs"):
		identifier := strings.TrimSuffix(strings.TrimPrefix(route, "/actions/workflows/"), "/runs")
		return transport.workflowRuns(identifier)
	case strings.HasPrefix(route, "/actions/workflows/"):
		workflow, known := workflowContractByID(strings.TrimPrefix(route, "/actions/workflows/"))
		if !known {
			return deliveryNotFound(), nil
		}
		return jsonResponse(http.StatusOK, workflowContractPayload(workflow)), nil
	case strings.HasPrefix(route, "/actions/runs/") && strings.HasSuffix(route, "/jobs"):
		return transport.workflowJobs(strings.TrimSuffix(strings.TrimPrefix(route, "/actions/runs/"), "/jobs"))
	case strings.HasPrefix(route, "/actions/runs/"):
		return transport.workflowRun(strings.TrimPrefix(route, "/actions/runs/"))
	case strings.HasPrefix(route, "/git/ref/heads/"):
		return transport.branchRef(strings.TrimPrefix(route, "/git/ref/heads/"))
	case route == "/git/ref/pull/"+decimal(deliveryPullNumber)+"/merge":
		return jsonResponse(http.StatusOK, map[string]any{
			"ref":    "refs/pull/" + decimal(deliveryPullNumber) + "/merge",
			"object": map[string]any{"type": "commit", "sha": deliveryPreviewSHA},
		}), nil
	case strings.HasPrefix(route, "/git/commits/"):
		return transport.commit(strings.TrimPrefix(route, "/git/commits/"))
	case strings.HasPrefix(route, "/git/trees/"):
		return transport.tree(strings.TrimPrefix(route, "/git/trees/"))
	case strings.HasPrefix(route, "/compare/"):
		return transport.comparison(strings.TrimPrefix(route, "/compare/"))
	case route == "/pulls":
		if transport.merged {
			return jsonResponse(http.StatusOK, []any{}), nil
		}
		return jsonResponse(http.StatusOK, []any{transport.pullPayload()}), nil
	case route == "/pulls/"+decimal(deliveryPullNumber):
		return jsonResponse(http.StatusOK, transport.pullPayload()), nil
	default:
		return deliveryNotFound(), nil
	}
}

func (transport *deliveryTransport) create(route string) (*http.Response, error) {
	switch route {
	case "/git/blobs":
		return jsonResponse(http.StatusCreated, map[string]any{"sha": deliveryBlobSHA}), nil
	case "/git/trees":
		return jsonResponse(http.StatusCreated, map[string]any{"sha": deliveryFeatureTreeSHA}), nil
	case "/git/commits":
		return jsonResponse(http.StatusCreated, map[string]any{"sha": deliveryFeatureHeadSHA}), nil
	case "/git/refs":
		transport.featureRefExists = true
		return jsonResponse(http.StatusCreated, map[string]any{
			"ref":    "refs/heads/" + transport.featureBranch,
			"object": map[string]any{"type": "commit", "sha": deliveryFeatureHeadSHA},
		}), nil
	case "/pulls":
		created := transport.calls[len(transport.calls)-1].Body
		title, _ := created["title"].(string)
		body, _ := created["body"].(string)
		transport.pullTitle, transport.pullBody = title, body
		return jsonResponse(http.StatusCreated, transport.pullPayload()), nil
	default:
		return deliveryNotFound(), nil
	}
}

func (transport *deliveryTransport) replace(route string) (*http.Response, error) {
	if route != "/pulls/"+decimal(deliveryPullNumber)+"/merge" {
		return deliveryNotFound(), nil
	}
	transport.merged = true
	transport.integrationSHA = deliveryMergeSHA
	return jsonResponse(http.StatusOK, map[string]any{"sha": deliveryMergeSHA, "merged": true}), nil
}

func (transport *deliveryTransport) branchRef(branch string) (*http.Response, error) {
	switch branch {
	case transport.integrationBranch:
		return deliveryRefPayload(branch, transport.integrationSHA), nil
	case transport.releaseBranch:
		return deliveryRefPayload(branch, strings.Repeat("5", 40)), nil
	case transport.featureBranch:
		if !transport.featureRefExists {
			return deliveryNotFound(), nil
		}
		return deliveryRefPayload(branch, deliveryFeatureHeadSHA), nil
	default:
		return deliveryNotFound(), nil
	}
}

func deliveryRefPayload(branch, sha string) *http.Response {
	return jsonResponse(http.StatusOK, map[string]any{
		"ref": "refs/heads/" + branch, "object": map[string]any{"type": "commit", "sha": sha},
	})
}

func (transport *deliveryTransport) commit(sha string) (*http.Response, error) {
	switch sha {
	// The pre-merge integration head is named on its own: once the merge
	// lands the branch points at the merge commit, which is a different
	// commit with different parents.
	case transport.pullBaseSHA:
		return deliveryCommitPayload(sha, transport.baseTreeSHA, nil), nil
	case deliveryFeatureHeadSHA:
		return deliveryCommitPayload(sha, deliveryFeatureTreeSHA, []string{transport.pullBaseSHA}), nil
	case deliveryPreviewSHA, deliveryMergeSHA:
		return deliveryCommitPayload(sha, transport.previewTreeSHA, []string{transport.pullBaseSHA, deliveryFeatureHeadSHA}), nil
	default:
		return deliveryNotFound(), nil
	}
}

func deliveryCommitPayload(sha, tree string, parents []string) *http.Response {
	encoded := make([]map[string]any, 0, len(parents))
	for _, parent := range parents {
		encoded = append(encoded, map[string]any{"sha": parent})
	}
	return jsonResponse(http.StatusOK, map[string]any{
		"sha": sha, "tree": map[string]any{"sha": tree}, "parents": encoded,
	})
}

func (transport *deliveryTransport) tree(treeSHA string) (*http.Response, error) {
	if treeSHA != transport.baseTreeSHA {
		return deliveryNotFound(), nil
	}
	return jsonResponse(http.StatusOK, map[string]any{
		"sha": treeSHA, "truncated": false,
		"tree": []map[string]any{{
			"path": transport.sourcePath, "mode": "100644", "type": "blob", "sha": transport.sourceBlobSHA,
		}},
	}), nil
}

func (transport *deliveryTransport) comparison(pair string) (*http.Response, error) {
	base, head, found := strings.Cut(pair, "...")
	if !found || base != transport.pullBaseSHA || head != deliveryFeatureHeadSHA {
		return deliveryNotFound(), nil
	}
	return jsonResponse(http.StatusOK, map[string]any{
		"status": "ahead", "ahead_by": 1, "behind_by": 0, "total_commits": 1,
		"files": []map[string]any{{"filename": transport.sourcePath, "status": "modified"}},
	}), nil
}

func (transport *deliveryTransport) pullPayload() map[string]any {
	state, mergeCommit := "open", ""
	if transport.merged {
		state, mergeCommit = "closed", deliveryMergeSHA
	}
	return map[string]any{
		"number": deliveryPullNumber, "state": state, "draft": false, "merged": transport.merged,
		"merge_commit_sha": mergeCommit, "mergeable": true,
		"html_url": "https://github.com/" + transport.repository + "/pull/" + decimal(deliveryPullNumber),
		"title":    transport.pullTitle, "body": transport.pullBody, "created_at": transport.pullCreated,
		"head": map[string]any{
			"ref": transport.featureBranch, "sha": deliveryFeatureHeadSHA,
			"repo": map[string]any{"full_name": transport.repository},
		},
		"base": map[string]any{"ref": transport.integrationBranch, "sha": transport.pullBaseSHA},
	}
}

func (transport *deliveryTransport) workflowRuns(identifier string) (*http.Response, error) {
	index := transport.featureWorkflowIndex(identifier)
	if index < 0 {
		return deliveryNotFound(), nil
	}
	workflow := testPrimaryConsumer().Contract().FeatureWorkflows[index]
	return jsonResponse(http.StatusOK, map[string]any{
		"total_count":   1,
		"workflow_runs": []map[string]any{transport.workflowRunPayload(workflow, transport.runIDs[index])},
	}), nil
}

func (transport *deliveryTransport) workflowRun(identifier string) (*http.Response, error) {
	for index, runID := range transport.runIDs {
		if decimal(runID) != identifier {
			continue
		}
		workflow := testPrimaryConsumer().Contract().FeatureWorkflows[index]
		return jsonResponse(http.StatusOK, transport.workflowRunPayload(workflow, runID)), nil
	}
	return deliveryNotFound(), nil
}

func (transport *deliveryTransport) workflowRunPayload(workflow githubapi.WorkflowContract, runID int64) map[string]any {
	return map[string]any{
		"id": runID, "workflow_id": workflow.ID, "name": workflow.Name, "path": workflow.Path,
		"display_title": transport.pullTitle, "head_branch": transport.featureBranch,
		"head_sha": deliveryFeatureHeadSHA, "event": "pull_request", "status": "completed",
		"conclusion": transport.workflowConclusion, "run_attempt": 1,
		"created_at": transport.runCreated, "updated_at": transport.runUpdated,
		"html_url":        "https://github.com/" + transport.repository + "/actions/runs/" + decimal(runID),
		"repository":      map[string]any{"full_name": transport.repository},
		"head_repository": map[string]any{"full_name": transport.repository},
		"pull_requests": []map[string]any{{
			"number": deliveryPullNumber,
			"head":   map[string]any{"ref": transport.featureBranch, "sha": deliveryFeatureHeadSHA},
			"base":   map[string]any{"ref": transport.integrationBranch, "sha": transport.pullBaseSHA},
		}},
	}
}

func (transport *deliveryTransport) workflowJobs(identifier string) (*http.Response, error) {
	for index, runID := range transport.runIDs {
		if decimal(runID) != identifier {
			continue
		}
		workflow := testPrimaryConsumer().Contract().FeatureWorkflows[index]
		jobs := make([]map[string]any, 0, len(workflow.RequiredJobs))
		for position, name := range workflow.RequiredJobs {
			jobs = append(jobs, map[string]any{
				"id": transport.jobIDs[index][position], "run_id": runID, "name": name,
				"status": "completed", "conclusion": "success",
			})
		}
		return jsonResponse(http.StatusOK, map[string]any{"total_count": len(jobs), "jobs": jobs}), nil
	}
	return deliveryNotFound(), nil
}

func (transport *deliveryTransport) featureWorkflowIndex(identifier string) int {
	for index, workflow := range testPrimaryConsumer().Contract().FeatureWorkflows {
		if decimal(workflow.ID) == identifier {
			return index
		}
	}
	return -1
}

// mutations lists the writes that reached GitHub, so a rejection test can show
// that nothing was created before the refusal.
func (transport *deliveryTransport) mutations() []string {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	var mutations []string
	for _, call := range transport.calls {
		if call.Method != http.MethodGet {
			mutations = append(mutations, call.Method+" "+call.Path)
		}
	}
	return mutations
}

func deliveryNotFound() *http.Response {
	return jsonResponse(http.StatusNotFound, map[string]string{"message": "not found"})
}

func decodeRequestBody(request *http.Request) map[string]any {
	if request.Body == nil {
		return nil
	}
	defer request.Body.Close()
	encoded, err := io.ReadAll(request.Body)
	if err != nil || len(encoded) == 0 {
		return nil
	}
	var decoded map[string]any
	if json.Unmarshal(encoded, &decoded) != nil {
		return nil
	}
	return decoded
}

func workflowContractByID(identifier string) (githubapi.WorkflowContract, bool) {
	for _, workflow := range allWorkflowContracts() {
		if decimal(workflow.ID) == identifier {
			return workflow, true
		}
	}
	return githubapi.WorkflowContract{}, false
}

// TestFeatureDeliveryPublishesReviewsAndMergesOneChain runs the four feature
// commands in order against one repository, each reading only what the
// previous command sealed.
func TestFeatureDeliveryPublishesReviewsAndMergesOneChain(t *testing.T) {
	fixture := newDeliveryFixture(t)
	transport := newDeliveryTransport(fixture)

	featurePath := fixture.output("feature.json")
	if err := run(context.Background(), fixture.publishArguments(featurePath), deliveryEnvironment, transport); err != nil {
		t.Fatalf("publish-feature: %v", err)
	}
	feature, err := readDeliveryArtifact[githubapi.PublishedFeature](featurePath, kindFeature, fixture.request, fixture.config)
	if err != nil || !validPublishedFeature(feature.Payload, feature.Binding) {
		t.Fatalf("published feature artifact is invalid: %v", err)
	}
	if feature.Payload.HeadSHA != deliveryFeatureHeadSHA || feature.Payload.TreeSHA != deliveryFeatureTreeSHA ||
		feature.Payload.Branch != featureBranch(fixture.binding) {
		t.Fatalf("published feature = %+v", feature.Payload)
	}

	pullPath := fixture.output("feature-pr.json")
	if err := run(context.Background(), fixture.createPullRequestArguments(featurePath, pullPath), deliveryEnvironment, transport); err != nil {
		t.Fatalf("create-feature-pr: %v", err)
	}
	pull, err := readDeliveryArtifact[featurePRPayload](pullPath, kindFeaturePR, fixture.request, fixture.config)
	if err != nil || !validFeaturePRPayload(pull.Payload, pull.Binding) {
		t.Fatalf("feature pull request artifact is invalid: %v", err)
	}
	if pull.Payload.PullRequest.Number != deliveryPullNumber ||
		!strings.Contains(pull.Payload.PullRequest.Body, "実装とレビューの経過") {
		t.Fatalf("feature pull request = %+v", pull.Payload.PullRequest)
	}

	checksPath := fixture.output("feature-checks.json")
	if err := run(context.Background(), fixture.waitArguments(pullPath, checksPath), deliveryEnvironment, transport); err != nil {
		t.Fatalf("wait-feature: %v", err)
	}
	checks, err := readDeliveryArtifact[featureChecksPayload](checksPath, kindFeatureChecks, fixture.request, fixture.config)
	if err != nil || !validFeatureChecksPayload(checks.Payload, checks.Binding) {
		t.Fatalf("feature checks artifact is invalid: %v", err)
	}
	expectedRuns, expectedJobs := deliveryCheckIdentifiers()
	flattened := make([]int64, 0)
	for _, ids := range expectedJobs {
		flattened = append(flattened, ids...)
	}
	if !slices.Equal(checks.Payload.Checks.WorkflowRunIDs, expectedRuns) ||
		!slices.Equal(checks.Payload.Checks.WorkflowJobIDs, flattened) {
		t.Fatalf("recorded checks = %+v", checks.Payload.Checks)
	}

	mergePath := fixture.output("feature-merge.json")
	if err := run(context.Background(), fixture.mergeArguments(pullPath, checksPath, mergePath), deliveryEnvironment, transport); err != nil {
		t.Fatalf("merge-feature: %v", err)
	}
	merge, err := readDeliveryArtifact[featureMergePayload](mergePath, kindFeatureMerge, fixture.request, fixture.config)
	if err != nil || !validFeatureMergePayload(merge.Payload, merge.Binding) {
		t.Fatalf("feature merge artifact is invalid: %v", err)
	}
	if merge.Payload.Merge.MergeSHA != deliveryMergeSHA || merge.Payload.Merge.TreeSHA != deliveryFeatureTreeSHA ||
		merge.Payload.Merge.BaseBranch != fixture.consumer.IntegrationBranch {
		t.Fatalf("feature merge = %+v", merge.Payload.Merge)
	}

	// The delivery writes exactly the objects it needs: the blob, tree and
	// commit of the candidate, its branch, the pull request, and the merge.
	expected := []string{
		"POST /repos/example/consumer/git/blobs",
		"POST /repos/example/consumer/git/trees",
		"POST /repos/example/consumer/git/commits",
		"POST /repos/example/consumer/git/refs",
		"POST /repos/example/consumer/pulls",
		"PUT /repos/example/consumer/pulls/" + decimal(deliveryPullNumber) + "/merge",
	}
	if !slices.Equal(transport.mutations(), expected) {
		t.Fatalf("mutations = %v", transport.mutations())
	}
	for _, path := range []string{featurePath, pullPath, checksPath, mergePath} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("artifact %s mode = %v, error = %v", path, info, err)
		}
	}
}

// TestRunPublishFeatureRejectsUnusableInputsBeforeMutation covers the refusals
// that must happen before anything is written to the destination repository.
func TestRunPublishFeatureRejectsUnusableInputsBeforeMutation(t *testing.T) {
	fixture := newDeliveryFixture(t)
	missing := filepath.Join(fixture.directory, "missing.json")
	tamperedCandidate := fixture.candidate
	tamperedCandidate.CandidateSHA256 = strings.Repeat("9", 64)
	tamperedPath := writePromotionFixtureJSON(t, fixture.directory, "tampered-candidate.json", tamperedCandidate)

	movedBaseline, err := newBaselineArtifact(fixture.config, fixture.consumer, githubapi.Baseline{
		Integration: githubapi.Snapshot{
			Branch: fixture.consumer.IntegrationBranch, SHA: promotionObjectID("e"),
			TreeSHA: fixture.baseline.Baseline.Integration.TreeSHA,
		},
		Release:      fixture.baseline.Baseline.Release,
		MergeBaseSHA: fixture.baseline.Baseline.MergeBaseSHA, MergeBaseTreeSHA: fixture.baseline.Baseline.MergeBaseTreeSHA,
	})
	if err != nil || movedBaseline.validate(fixture.config) != nil {
		t.Fatalf("moved baseline fixture is invalid: %v", err)
	}
	movedBaselinePath := writePromotionFixtureJSON(t, fixture.directory, "moved-baseline.json", movedBaseline)

	tests := []struct {
		name      string
		arguments func(output string) []string
		code      string
	}{
		{
			name:      "no arguments",
			arguments: func(string) []string { return []string{"publish-feature"} },
			code:      "arguments_invalid",
		},
		{
			name: "unreadable ticket",
			arguments: func(output string) []string {
				return replaceFlag(fixture.publishArguments(output), "--ticket", missing)
			},
			code: "ticket_artifact_invalid",
		},
		{
			name: "unreadable source",
			arguments: func(output string) []string {
				return replaceFlag(fixture.publishArguments(output), "--source", missing)
			},
			code: "source_artifact_invalid",
		},
		{
			name: "unreadable candidate",
			arguments: func(output string) []string {
				return replaceFlag(fixture.publishArguments(output), "--candidate", missing)
			},
			code: "candidate_artifact_invalid",
		},
		{
			name: "unreadable decision",
			arguments: func(output string) []string {
				return replaceFlag(fixture.publishArguments(output), "--decision", missing)
			},
			code: "decision_artifact_invalid",
		},
		{
			name: "unreadable validation",
			arguments: func(output string) []string {
				return replaceFlag(fixture.publishArguments(output), "--validation", missing)
			},
			code: "validation_artifact_invalid",
		},
		{
			name: "unreadable review",
			arguments: func(output string) []string {
				return replaceFlag(fixture.publishArguments(output), "--review", missing)
			},
			code: "review_artifact_invalid",
		},
		{
			name: "unreadable baseline",
			arguments: func(output string) []string {
				return replaceFlag(fixture.publishArguments(output), "--baseline", missing)
			},
			code: "baseline_artifact_invalid",
		},
		{
			name: "candidate outside the sealed chain",
			arguments: func(output string) []string {
				return replaceFlag(fixture.publishArguments(output), "--candidate", tamperedPath)
			},
			code: "publish_gate_rejected",
		},
		{
			name: "baseline that moved away from the source",
			arguments: func(output string) []string {
				return replaceFlag(fixture.publishArguments(output), "--baseline", movedBaselinePath)
			},
			code: "publish_gate_rejected",
		},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transport := newDeliveryTransport(fixture)
			output := fixture.output("publish-" + decimal(int64(index+1)) + ".json")
			err := run(context.Background(), test.arguments(output), deliveryEnvironment, transport)
			if failureCode(err) != test.code {
				t.Fatalf("failureCode() = %q; want %q; error = %v", failureCode(err), test.code, err)
			}
			if mutations := transport.mutations(); len(mutations) != 0 {
				t.Fatalf("GitHub mutations before rejection = %v", mutations)
			}
			if _, err := os.Lstat(output); !os.IsNotExist(err) {
				t.Fatalf("output exists after rejection: %v", err)
			}
		})
	}
}

// TestRunPublishFeatureRejectsRunWithoutToken stops before GitHub is contacted
// at all when the credential is absent.
func TestRunPublishFeatureRejectsRunWithoutToken(t *testing.T) {
	fixture := newDeliveryFixture(t)
	transport := newDeliveryTransport(fixture)
	err := run(context.Background(), fixture.publishArguments(fixture.output("feature.json")),
		func(string) string { return "" }, transport)
	if failureCode(err) != "github_token_invalid" {
		t.Fatalf("failureCode() = %q; error = %v", failureCode(err), err)
	}
	if len(transport.calls) != 0 {
		t.Fatalf("GitHub calls without a token = %d", len(transport.calls))
	}
}

// TestRunPublishFeatureReportsRefusedObjectWrite keeps the fixed failure code
// when GitHub refuses to store the candidate.
func TestRunPublishFeatureReportsRefusedObjectWrite(t *testing.T) {
	fixture := newDeliveryFixture(t)
	transport := newDeliveryTransport(fixture)
	transport.failures["POST /repos/example/consumer/git/blobs"] = http.StatusForbidden
	output := fixture.output("feature.json")
	err := run(context.Background(), fixture.publishArguments(output), deliveryEnvironment, transport)
	if failureCode(err) != "feature_publish_failed" {
		t.Fatalf("failureCode() = %q; error = %v", failureCode(err), err)
	}
	if strings.Contains(err.Error(), fakeGitHubToken) {
		t.Fatalf("failure leaked the credential: %v", err)
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Fatalf("output exists after refusal: %v", err)
	}
}

// TestRunCreateFeaturePRRejectsUnusableInputs covers the refusals that happen
// before the pull request is opened.
func TestRunCreateFeaturePRRejectsUnusableInputs(t *testing.T) {
	fixture := newDeliveryFixture(t)
	missing := filepath.Join(fixture.directory, "missing.json")
	featurePath := writeDeliveryArtifact(t, fixture, "feature.json", kindFeature, fixture.binding, fixture.publishedFeature())

	foreign := fixture.publishedFeature()
	foreign.Paths = []string{"client/src/Other.tsx"}
	foreignPath := writeDeliveryArtifact(t, fixture, "foreign-feature.json", kindFeature, fixture.binding, foreign)

	emptyTrail := filepath.Join(fixture.directory, "empty-trail.txt")
	if err := os.WriteFile(emptyTrail, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		arguments func(output string) []string
		code      string
	}{
		{
			name: "empty trail",
			arguments: func(output string) []string {
				return replaceFlag(fixture.createPullRequestArguments(featurePath, output), "--trail", emptyTrail)
			},
			code: "trail_invalid",
		},
		{
			name: "unreadable feature artifact",
			arguments: func(output string) []string {
				return fixture.createPullRequestArguments(missing, output)
			},
			code: "feature_artifact_invalid",
		},
		{
			name: "feature artifact naming another file set",
			arguments: func(output string) []string {
				return fixture.createPullRequestArguments(foreignPath, output)
			},
			code: "feature_artifact_invalid",
		},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transport := newDeliveryTransport(fixture)
			transport.featureRefExists = true
			output := fixture.output("pr-" + decimal(int64(index+1)) + ".json")
			err := run(context.Background(), test.arguments(output), deliveryEnvironment, transport)
			if failureCode(err) != test.code {
				t.Fatalf("failureCode() = %q; want %q; error = %v", failureCode(err), test.code, err)
			}
			if mutations := transport.mutations(); len(mutations) != 0 {
				t.Fatalf("GitHub mutations before rejection = %v", mutations)
			}
		})
	}
}

// TestRunCreateFeaturePRReportsRefusedPullRequest keeps the fixed failure code
// when GitHub refuses to open the pull request.
func TestRunCreateFeaturePRReportsRefusedPullRequest(t *testing.T) {
	fixture := newDeliveryFixture(t)
	transport := newDeliveryTransport(fixture)
	transport.featureRefExists = true
	transport.failures["POST /repos/example/consumer/pulls"] = http.StatusForbidden
	featurePath := writeDeliveryArtifact(t, fixture, "feature.json", kindFeature, fixture.binding, fixture.publishedFeature())
	err := run(context.Background(), fixture.createPullRequestArguments(featurePath, fixture.output("pr.json")),
		deliveryEnvironment, transport)
	if failureCode(err) != "feature_pr_create_failed" {
		t.Fatalf("failureCode() = %q; error = %v", failureCode(err), err)
	}
}

// TestRunCreateFeaturePRRejectsPullRequestWithoutCreationTime refuses a pull
// request that carries no creation time: the promotion chronology later reads
// that field, and a zero value would silently pass every ordering check.
func TestRunCreateFeaturePRRejectsPullRequestWithoutCreationTime(t *testing.T) {
	fixture := newDeliveryFixture(t)
	transport := newDeliveryTransport(fixture)
	transport.featureRefExists = true
	transport.pullCreated = time.Time{}.UTC().Format(time.RFC3339)
	featurePath := writeDeliveryArtifact(t, fixture, "feature.json", kindFeature, fixture.binding, fixture.publishedFeature())
	output := fixture.output("pr.json")
	err := run(context.Background(), fixture.createPullRequestArguments(featurePath, output), deliveryEnvironment, transport)
	if failureCode(err) != "feature_pr_result_invalid" {
		t.Fatalf("failureCode() = %q; error = %v", failureCode(err), err)
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Fatalf("output exists after rejection: %v", err)
	}
}

// TestRunWaitFeatureRejectsUnusableFeaturePullRequest refuses a checks run
// whose pull request artifact does not describe the sealed delivery.
func TestRunWaitFeatureRejectsUnusableFeaturePullRequest(t *testing.T) {
	fixture := newDeliveryFixture(t)
	drifted := featurePRPayload{Feature: fixture.publishedFeature(), PullRequest: fixture.pullRequest()}
	drifted.PullRequest.HeadSHA = promotionObjectID("d")
	driftedPath := writeDeliveryArtifact(t, fixture, "drifted-pr.json", kindFeaturePR, fixture.binding, drifted)
	missing := filepath.Join(fixture.directory, "missing.json")

	for name, featurePR := range map[string]string{"unreadable": missing, "drifted head": driftedPath} {
		t.Run(name, func(t *testing.T) {
			transport := newDeliveryTransport(fixture)
			transport.featureRefExists = true
			err := run(context.Background(), fixture.waitArguments(featurePR, fixture.output("checks-"+name+".json")),
				deliveryEnvironment, transport)
			if failureCode(err) != "feature_pr_artifact_invalid" {
				t.Fatalf("failureCode() = %q; error = %v", failureCode(err), err)
			}
		})
	}
}

// TestRunWaitFeatureReportsFailedFeatureWorkflow refuses to seal check
// evidence when a required workflow concluded in failure.
func TestRunWaitFeatureReportsFailedFeatureWorkflow(t *testing.T) {
	fixture := newDeliveryFixture(t)
	transport := newDeliveryTransport(fixture)
	transport.featureRefExists = true
	transport.workflowConclusion = "failure"
	pullPath := writeDeliveryArtifact(t, fixture, "feature-pr.json", kindFeaturePR, fixture.binding,
		featurePRPayload{Feature: fixture.publishedFeature(), PullRequest: fixture.pullRequest()})
	output := fixture.output("checks.json")
	err := run(context.Background(), fixture.waitArguments(pullPath, output), deliveryEnvironment, transport)
	if failureCode(err) != "feature_checks_failed" {
		t.Fatalf("failureCode() = %q; error = %v", failureCode(err), err)
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Fatalf("output exists after a failed workflow: %v", err)
	}
}

// TestFeatureCommandsRefuseMalformedInvocation covers the refusals the later
// feature commands share: a malformed command line, an output that is already
// occupied, an unusable ticket, and a missing credential each stop the run
// before GitHub is asked for anything at all.
func TestFeatureCommandsRefuseMalformedInvocation(t *testing.T) {
	fixture := newDeliveryFixture(t)
	featurePath := writeDeliveryArtifact(t, fixture, "feature.json", kindFeature, fixture.binding, fixture.publishedFeature())
	pullPath := writeDeliveryArtifact(t, fixture, "feature-pr.json", kindFeaturePR, fixture.binding,
		featurePRPayload{Feature: fixture.publishedFeature(), PullRequest: fixture.pullRequest()})
	checksPath := writeDeliveryArtifact(t, fixture, "feature-checks.json", kindFeatureChecks, fixture.binding,
		featureChecksPayload{
			Feature: fixture.publishedFeature(), PullRequest: fixture.pullRequest(), Checks: fixture.checkEvidence(),
		})
	occupied := fixture.output("occupied.json")
	if err := os.WriteFile(occupied, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(fixture.directory, "missing.json")

	commands := map[string]func(output string) []string{
		"create-feature-pr": func(output string) []string {
			return fixture.createPullRequestArguments(featurePath, output)
		},
		"wait-feature":  func(output string) []string { return fixture.waitArguments(pullPath, output) },
		"merge-feature": func(output string) []string { return fixture.mergeArguments(pullPath, checksPath, output) },
	}
	for command, build := range commands {
		t.Run(command, func(t *testing.T) {
			tests := []struct {
				name        string
				arguments   []string
				environment func(string) string
				code        string
			}{
				{
					name: "no arguments", arguments: []string{command},
					environment: deliveryEnvironment, code: "arguments_invalid",
				},
				{
					name: "occupied output", arguments: build(occupied),
					environment: deliveryEnvironment, code: "output_path_invalid",
				},
				{
					name:        "unreadable ticket",
					arguments:   replaceFlag(build(fixture.output(command+"-ticket.json")), "--ticket", missing),
					environment: deliveryEnvironment, code: "ticket_artifact_invalid",
				},
				{
					name: "missing credential", arguments: build(fixture.output(command + "-token.json")),
					environment: func(string) string { return "" }, code: "github_token_invalid",
				},
			}
			for _, test := range tests {
				t.Run(test.name, func(t *testing.T) {
					transport := newDeliveryTransport(fixture)
					transport.featureRefExists = true
					err := run(context.Background(), test.arguments, test.environment, transport)
					if failureCode(err) != test.code {
						t.Fatalf("failureCode() = %q; want %q; error = %v", failureCode(err), test.code, err)
					}
					if len(transport.calls) != 0 {
						t.Fatalf("GitHub calls before the refusal = %d", len(transport.calls))
					}
				})
			}
		})
	}
}

// TestRunMergeFeatureRejectsUnusableFeaturePullRequest refuses a merge whose
// pull request artifact cannot be read at all.
func TestRunMergeFeatureRejectsUnusableFeaturePullRequest(t *testing.T) {
	fixture := newDeliveryFixture(t)
	transport := newDeliveryTransport(fixture)
	transport.featureRefExists = true
	checksPath := writeDeliveryArtifact(t, fixture, "feature-checks.json", kindFeatureChecks, fixture.binding,
		featureChecksPayload{
			Feature: fixture.publishedFeature(), PullRequest: fixture.pullRequest(), Checks: fixture.checkEvidence(),
		})
	missing := filepath.Join(fixture.directory, "missing.json")
	err := run(context.Background(), fixture.mergeArguments(missing, checksPath, fixture.output("merge.json")),
		deliveryEnvironment, transport)
	if failureCode(err) != "feature_pr_artifact_invalid" {
		t.Fatalf("failureCode() = %q; error = %v", failureCode(err), err)
	}
	if mutations := transport.mutations(); len(mutations) != 0 {
		t.Fatalf("GitHub mutations before rejection = %v", mutations)
	}
}

// TestRunMergeFeatureRejectsEvidenceFromAnotherDelivery refuses check evidence
// whose binding does not match the pull request it claims to cover.
func TestRunMergeFeatureRejectsEvidenceFromAnotherDelivery(t *testing.T) {
	fixture := newDeliveryFixture(t)
	pullPath := writeDeliveryArtifact(t, fixture, "feature-pr.json", kindFeaturePR, fixture.binding,
		featurePRPayload{Feature: fixture.publishedFeature(), PullRequest: fixture.pullRequest()})
	otherBinding := fixture.binding
	otherBinding.CandidateSHA256 = strings.Repeat("9", 64)
	otherChecks := writeDeliveryArtifact(t, fixture, "other-checks.json", kindFeatureChecks, otherBinding,
		featureChecksPayload{
			Feature: fixture.publishedFeature(), PullRequest: fixture.pullRequest(), Checks: fixture.checkEvidence(),
		})
	missing := filepath.Join(fixture.directory, "missing.json")

	for name, checks := range map[string]string{"unreadable": missing, "another binding": otherChecks} {
		t.Run(name, func(t *testing.T) {
			transport := newDeliveryTransport(fixture)
			transport.featureRefExists = true
			err := run(context.Background(), fixture.mergeArguments(pullPath, checks, fixture.output("merge-"+name+".json")),
				deliveryEnvironment, transport)
			if failureCode(err) != "feature_checks_artifact_invalid" {
				t.Fatalf("failureCode() = %q; error = %v", failureCode(err), err)
			}
			if mutations := transport.mutations(); len(mutations) != 0 {
				t.Fatalf("GitHub mutations before rejection = %v", mutations)
			}
		})
	}
}

// TestRunMergeFeatureReportsRefusedMerge keeps the fixed failure code when
// GitHub refuses the merge itself.
func TestRunMergeFeatureReportsRefusedMerge(t *testing.T) {
	fixture := newDeliveryFixture(t)
	transport := newDeliveryTransport(fixture)
	transport.featureRefExists = true
	transport.failures["PUT /repos/example/consumer/pulls/"+decimal(deliveryPullNumber)+"/merge"] = http.StatusForbidden
	pullPath := writeDeliveryArtifact(t, fixture, "feature-pr.json", kindFeaturePR, fixture.binding,
		featurePRPayload{Feature: fixture.publishedFeature(), PullRequest: fixture.pullRequest()})
	checksPath := writeDeliveryArtifact(t, fixture, "feature-checks.json", kindFeatureChecks, fixture.binding,
		featureChecksPayload{
			Feature: fixture.publishedFeature(), PullRequest: fixture.pullRequest(), Checks: fixture.checkEvidence(),
		})
	err := run(context.Background(), fixture.mergeArguments(pullPath, checksPath, fixture.output("merge.json")),
		deliveryEnvironment, transport)
	if failureCode(err) != "feature_merge_failed" {
		t.Fatalf("failureCode() = %q; error = %v", failureCode(err), err)
	}
}

// TestRunMergeFeatureRejectsMergeThatChangedTheTree refuses a merge whose
// resulting tree is not the tree that was reviewed and validated.
func TestRunMergeFeatureRejectsMergeThatChangedTheTree(t *testing.T) {
	fixture := newDeliveryFixture(t)
	transport := newDeliveryTransport(fixture)
	transport.featureRefExists = true
	transport.previewTreeSHA = promotionObjectID("d")
	pullPath := writeDeliveryArtifact(t, fixture, "feature-pr.json", kindFeaturePR, fixture.binding,
		featurePRPayload{Feature: fixture.publishedFeature(), PullRequest: fixture.pullRequest()})
	checksPath := writeDeliveryArtifact(t, fixture, "feature-checks.json", kindFeatureChecks, fixture.binding,
		featureChecksPayload{
			Feature: fixture.publishedFeature(), PullRequest: fixture.pullRequest(), Checks: fixture.checkEvidence(),
		})
	output := fixture.output("merge.json")
	err := run(context.Background(), fixture.mergeArguments(pullPath, checksPath, output), deliveryEnvironment, transport)
	if failureCode(err) != "feature_merge_result_invalid" {
		t.Fatalf("failureCode() = %q; error = %v", failureCode(err), err)
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Fatalf("output exists after a rejected merge: %v", err)
	}
}

// TestReadGateArtifactsNamesTheArtifactThatFailed reads the shared publish
// gate directly: every command downstream of the publish reads the same five
// artifacts, and each unusable one must be named on its own.
func TestReadGateArtifactsNamesTheArtifactThatFailed(t *testing.T) {
	fixture := newDeliveryFixture(t)
	missing := filepath.Join(fixture.directory, "missing.json")
	tampered := fixture.candidate
	tampered.CandidateSHA256 = strings.Repeat("9", 64)
	tamperedPath := writePromotionFixtureJSON(t, fixture.directory, "gate-candidate.json", tampered)

	complete := commandArguments{
		"--source": {fixture.sourcePath}, "--candidate": {fixture.candidatePath},
		"--decision": {fixture.decisionPath}, "--validation": {fixture.validationPath},
		"--review": slices.Clone(fixture.reviewPaths),
	}
	gate, err := readGateArtifacts(complete, fixture.request, fixture.config)
	if err != nil {
		t.Fatalf("readGateArtifacts() error = %v", err)
	}
	if gate.source.SourceSHA256 != fixture.source.SourceSHA256 ||
		gate.candidate.CandidateSHA256 != fixture.candidate.CandidateSHA256 ||
		gate.decision.DecisionSHA256 != fixture.decision.DecisionSHA256 ||
		gate.validation.ValidationSHA256 != fixture.validation.ValidationSHA256 ||
		len(gate.reviews) != len(fixture.reviews) {
		t.Fatalf("gate artifacts = %+v", gate)
	}

	tests := []struct {
		name  string
		flag  string
		value string
		code  string
	}{
		{name: "source", flag: "--source", value: missing, code: "source_artifact_invalid"},
		{name: "candidate", flag: "--candidate", value: missing, code: "candidate_artifact_invalid"},
		{name: "decision", flag: "--decision", value: missing, code: "decision_artifact_invalid"},
		{name: "validation", flag: "--validation", value: missing, code: "validation_artifact_invalid"},
		{name: "review", flag: "--review", value: missing, code: "review_artifact_invalid"},
		{name: "candidate outside the chain", flag: "--candidate", value: tamperedPath, code: "publish_gate_rejected"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			arguments := commandArguments{}
			for flag, values := range complete {
				arguments[flag] = slices.Clone(values)
			}
			arguments[test.flag] = []string{test.value}
			artifacts, err := readGateArtifacts(arguments, fixture.request, fixture.config)
			if failureCode(err) != test.code {
				t.Fatalf("failureCode() = %q; want %q; error = %v", failureCode(err), test.code, err)
			}
			if artifacts.candidate.CandidateSHA256 != "" || len(artifacts.reviews) != 0 {
				t.Fatalf("rejected gate returned artifacts: %+v", artifacts)
			}
		})
	}
}

// TestDeliveryBindingMatchesArtifacts checks the binding against the sealed
// artifacts on every dimension it pins, so a swapped artifact cannot ride a
// binding that was sealed for a different one.
func TestDeliveryBindingMatchesArtifacts(t *testing.T) {
	fixture := newDeliveryFixture(t)
	if !fixture.binding.matchesArtifacts(fixture.source, fixture.candidate, fixture.validation) {
		t.Fatal("matchesArtifacts() rejected the sealed chain")
	}
	tests := map[string]func(*deliveryBinding){
		"source":     func(binding *deliveryBinding) { binding.SourceSHA256 = strings.Repeat("9", 64) },
		"candidate":  func(binding *deliveryBinding) { binding.CandidateSHA256 = strings.Repeat("9", 64) },
		"validation": func(binding *deliveryBinding) { binding.ValidationSHA256 = strings.Repeat("9", 64) },
		"paths":      func(binding *deliveryBinding) { binding.ProductPaths = []string{"client/src/Other.tsx"} },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			binding := fixture.binding
			binding.ProductPaths = slices.Clone(fixture.binding.ProductPaths)
			mutate(&binding)
			if binding.matchesArtifacts(fixture.source, fixture.candidate, fixture.validation) {
				t.Fatal("matchesArtifacts() accepted a binding sealed for other artifacts")
			}
		})
	}
}

// replaceFlag swaps the value of the first occurrence of one flag, leaving
// every other flag as the command would normally receive it. The argument list
// opens with the command name, so the flags sit at the odd positions.
func replaceFlag(arguments []string, flag, value string) []string {
	replaced := slices.Clone(arguments)
	for index := 1; index+1 < len(replaced); index += 2 {
		if replaced[index] == flag {
			replaced[index+1] = value
			return replaced
		}
	}
	return replaced
}

// consumerRepositoryPayload echoes the checked-in configuration back as the
// repository read, so the fake GitHub is bound to the configuration file
// rather than to a fixed table of its own.
func consumerRepositoryPayload() map[string]any {
	consumer := testPrimaryConsumer()
	settings := consumer.Contract().MergeSettings
	return map[string]any{
		"id": consumer.RepositoryID, "full_name": consumer.Repository, "default_branch": consumer.GitHub.DefaultBranch,
		"archived": false, "disabled": false,
		"allow_merge_commit": settings.AllowMergeCommit, "allow_squash_merge": settings.AllowSquashMerge,
		"allow_rebase_merge": settings.AllowRebaseMerge, "allow_auto_merge": settings.AllowAutoMerge,
		"allow_update_branch": settings.AllowUpdateBranch, "delete_branch_on_merge": settings.DeleteBranchOnMerge,
		"use_squash_pr_title_as_default": settings.UseSquashPRTitleAsDefault,
		"squash_merge_commit_title":      settings.SquashMergeCommitTitle,
		"squash_merge_commit_message":    settings.SquashMergeCommitMessage,
		"merge_commit_title":             settings.MergeCommitTitle, "merge_commit_message": settings.MergeCommitMessage,
		"web_commit_signoff_required": settings.WebCommitSignoffRequired,
	}
}

func workflowContractPayload(workflow githubapi.WorkflowContract) map[string]any {
	return map[string]any{"id": workflow.ID, "name": workflow.Name, "path": workflow.Path, "state": workflow.State}
}
