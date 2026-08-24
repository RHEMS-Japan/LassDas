package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/githubapi"
	"automation.internal/ticket-ingress/internal/releaseproof"
	"automation.internal/ticket-ingress/internal/visiblecheck"
	"automation.internal/ticket-ingress/internal/worker"
)

const fakeGitHubToken = "github_pat_test_controller_token"

type fakeGitHubTransport struct {
	mu           sync.Mutex
	requests     []*http.Request
	baseline     bool
	status       int
	responseBody string
}

func (transport *fakeGitHubTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.mu.Lock()
	transport.requests = append(transport.requests, request.Clone(request.Context()))
	transport.mu.Unlock()
	if request.URL.Scheme != "https" || request.URL.Host != "api.github.com" ||
		request.Header.Get("Authorization") != "Bearer "+fakeGitHubToken {
		return jsonResponse(http.StatusBadRequest, map[string]string{"message": "invalid request"}), nil
	}
	if transport.status != 0 {
		return rawResponse(transport.status, transport.responseBody), nil
	}
	if request.Method != http.MethodGet {
		return jsonResponse(http.StatusMethodNotAllowed, map[string]string{"message": "mutation denied by test"}), nil
	}
	path := request.URL.Path
	if path == "/repos/example/consumer" {
		return jsonResponse(http.StatusOK, consumerRepositoryPayload()), nil
	}
	for _, workflow := range allWorkflowContracts() {
		if path == "/repos/example/consumer/actions/workflows/"+decimal(workflow.ID) {
			return jsonResponse(http.StatusOK, workflowContractPayload(workflow)), nil
		}
	}
	if transport.baseline {
		return transport.baselineResponse(path)
	}
	return jsonResponse(http.StatusNotFound, map[string]string{"message": "not found"}), nil
}

func (transport *fakeGitHubTransport) baselineResponse(path string) (*http.Response, error) {
	stagingSHA := strings.Repeat("a", 40)
	releaseSHA := strings.Repeat("b", 40)
	treeSHA := strings.Repeat("c", 40)
	switch path {
	case "/repos/example/consumer/git/ref/heads/stg":
		return jsonResponse(http.StatusOK, map[string]any{
			"ref": "refs/heads/stg", "object": map[string]any{"type": "commit", "sha": stagingSHA},
		}), nil
	case "/repos/example/consumer/git/ref/heads/prod":
		return jsonResponse(http.StatusOK, map[string]any{
			"ref": "refs/heads/prod", "object": map[string]any{"type": "commit", "sha": releaseSHA},
		}), nil
	case "/repos/example/consumer/git/commits/" + stagingSHA:
		return jsonResponse(http.StatusOK, map[string]any{"sha": stagingSHA, "tree": map[string]string{"sha": treeSHA}, "parents": []any{}}), nil
	case "/repos/example/consumer/git/commits/" + releaseSHA:
		return jsonResponse(http.StatusOK, map[string]any{
			"sha": releaseSHA, "tree": map[string]string{"sha": treeSHA},
			"parents": []map[string]string{{"sha": stagingSHA}},
		}), nil
	case "/repos/example/consumer/compare/" + releaseSHA + "..." + stagingSHA:
		return jsonResponse(http.StatusOK, map[string]any{
			"status": "behind", "ahead_by": 0, "behind_by": 1, "total_commits": 0,
			"base_commit": map[string]any{
				"sha": releaseSHA, "commit": map[string]any{"tree": map[string]string{"sha": treeSHA}},
			},
			"merge_base_commit": map[string]any{
				"sha": stagingSHA, "commit": map[string]any{"tree": map[string]string{"sha": treeSHA}},
			},
			"files": []any{},
		}), nil
	default:
		return jsonResponse(http.StatusNotFound, map[string]string{"message": "not found"}), nil
	}
}

func (transport *fakeGitHubTransport) requestSnapshot() []*http.Request {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return append([]*http.Request(nil), transport.requests...)
}

// writeControllerDraft writes a draft naming the first configured consumer,
// for the baseline command that resolves its destination before target files
// exist.
func writeControllerDraft(t *testing.T, config worker.Config) string {
	t.Helper()
	configSHA, err := config.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	request := worker.TicketDraft{
		SchemaVersion: 1, DeliveryID: "delivery_0123456789abcdef0123456789abcdef",
		InputSHA256: strings.Repeat("1", 64), ConfigSHA256: configSHA, ToolSHA: strings.Repeat("2", 40),
		IssueKey: "TICKET-123", RunID: "run_20260806_sample",
		Repository: config.Consumers[0].Repository, Mode: config.Consumers[0].Mode.ID,
		Summary: "Baseline resolution ticket",
		Request: "Resolve the delivery destination for the baseline.",
	}
	filename := filepath.Join(t.TempDir(), "draft.json")
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return filename
}

func TestRunBaselineUsesFixedReadOnlyContractAndWritesSealedArtifact(t *testing.T) {
	enterRepositoryRoot(t)
	output := filepath.Join(t.TempDir(), "baseline.json")
	transport := &fakeGitHubTransport{baseline: true}
	config, err := loadFixedConfig(controllerConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	err = run(context.Background(), []string{
		"baseline", "--config", controllerConfigPath, "--draft", writeControllerDraft(t, config), "--out", output,
	}, func(name string) string {
		if name == githubTokenEnvironment {
			return fakeGitHubToken
		}
		return ""
	}, transport)
	if err != nil {
		t.Fatalf("run baseline: %v", err)
	}
	artifact, err := readBaselineArtifact(output, config)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Baseline.Integration.Branch != "stg" || artifact.Baseline.Release.Branch != "prod" {
		t.Fatalf("baseline branches = %+v", artifact.Baseline)
	}
	info, err := os.Stat(output)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("artifact mode = %v, error = %v", info, err)
	}
	requests := transport.requestSnapshot()
	if len(requests) != 12 {
		t.Fatalf("request count = %d", len(requests))
	}
	for _, request := range requests {
		if request.Method != http.MethodGet {
			t.Fatalf("unexpected mutation: %s %s", request.Method, request.URL)
		}
	}
}

// Config integrity is per run, not per binary: the loader accepts any valid
// configuration, and a swapped one fails the run it was swapped into because
// every sealed ticket binds the canonical digest of the configuration the
// intake saw. The engine carries no customer digest of its own — a baked-in
// digest is what once kept a customer's values inside the engine binary.
func TestConfigDriftFailsTheRunThroughTheTicketBinding(t *testing.T) {
	enterRepositoryRoot(t)
	absolute, err := filepath.Abs(controllerConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	original, err := loadFixedConfig(absolute)
	if err != nil {
		t.Fatalf("verified absolute config was rejected: %v", err)
	}
	encoded, err := os.ReadFile(controllerConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Replace(encoded, []byte(`"max_files": 8`), []byte(`"max_files": 7`), 1)
	if bytes.Equal(tampered, encoded) {
		t.Fatal("test fixture did not change the config")
	}
	directory := t.TempDir()
	tamperedPath := filepath.Join(directory, filepath.Base(controllerConfigPath))
	if err := os.WriteFile(tamperedPath, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	// The loader accepts the modified file: it is a valid configuration.
	if _, err := loadFixedConfig(tamperedPath); err != nil {
		t.Fatalf("loader rejected a valid configuration: %v", err)
	}
	// A run whose ticket was sealed against the original configuration fails
	// against the swapped one before anything reaches GitHub.
	transport := &fakeGitHubTransport{baseline: true}
	err = run(context.Background(), []string{
		"baseline", "--config", tamperedPath, "--draft", writeControllerDraft(t, original),
		"--out", filepath.Join(t.TempDir(), "baseline.json"),
	}, func(name string) string {
		if name == githubTokenEnvironment {
			return fakeGitHubToken
		}
		return ""
	}, transport)
	if failureCode(err) != "ticket_artifact_invalid" {
		t.Fatalf("error = %v", err)
	}
	if requests := transport.requestSnapshot(); len(requests) != 0 {
		t.Fatalf("request count before ticket rejection = %d", len(requests))
	}
	wrongName := filepath.Join(directory, "other.json")
	if err := os.WriteFile(wrongName, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadFixedConfig(wrongName); err == nil {
		t.Fatal("controller accepted a non-fixed config filename")
	}
}

func TestRunVerifiesRepositoryBeforeReadingDeliveryArtifact(t *testing.T) {
	enterRepositoryRoot(t)
	transport := &fakeGitHubTransport{}
	trailPath := filepath.Join(t.TempDir(), "trail.txt")
	if err := os.WriteFile(trailPath, []byte("### 実装とレビューの経過\n- 1 周目: 収束\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := run(context.Background(), []string{
		"create-feature-pr", "--config", controllerConfigPath,
		"--ticket", filepath.Join(t.TempDir(), "missing-ticket.json"),
		"--feature", filepath.Join(t.TempDir(), "missing-feature.json"),
		"--trail", trailPath,
		"--out", filepath.Join(t.TempDir(), "out.json"),
	}, func(string) string { return fakeGitHubToken }, transport)
	if failureCode(err) != "ticket_artifact_invalid" {
		t.Fatalf("error = %v", err)
	}
	// The destination is resolved from the ticket, so nothing may reach
	// GitHub before the ticket itself is read and accepted.
	if requests := transport.requestSnapshot(); len(requests) != 0 {
		t.Fatalf("request count before ticket acceptance = %d", len(requests))
	}
}

func TestRunDoesNotLeakTokenOrRemoteBody(t *testing.T) {
	enterRepositoryRoot(t)
	remoteSecret := "remote-secret-response"
	transport := &fakeGitHubTransport{status: http.StatusInternalServerError, responseBody: remoteSecret}
	config, err := loadFixedConfig(controllerConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	err = run(context.Background(), []string{
		"baseline", "--config", controllerConfigPath, "--draft", writeControllerDraft(t, config), "--out", filepath.Join(t.TempDir(), "out.json"),
	}, func(string) string { return fakeGitHubToken }, transport)
	if failureCode(err) != "github_verify_failed" {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), fakeGitHubToken) || strings.Contains(err.Error(), remoteSecret) {
		t.Fatalf("error leaked remote data: %q", err)
	}
}

func TestControllerArtifactsRejectUnknownAndDuplicateJSON(t *testing.T) {
	enterRepositoryRoot(t)
	config, err := loadFixedConfig(controllerConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := newBaselineArtifact(config, config.Consumers[0], githubapi.Baseline{
		Integration:  githubapi.Snapshot{Branch: "stg", SHA: strings.Repeat("a", 40), TreeSHA: strings.Repeat("c", 40)},
		Release:      githubapi.Snapshot{Branch: "prod", SHA: strings.Repeat("b", 40), TreeSHA: strings.Repeat("c", 40)},
		MergeBaseSHA: strings.Repeat("a", 40), MergeBaseTreeSHA: strings.Repeat("c", 40),
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(artifact)
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string][]byte{
		"unknown":   append(append([]byte(nil), encoded[:len(encoded)-1]...), []byte(`,"unexpected":true}`)...),
		"duplicate": bytes.Replace(encoded, []byte(`"kind":"m1-baseline"`), []byte(`"kind":"m1-baseline","kind":"m1-baseline"`), 1),
	}
	for name, value := range tests {
		t.Run(name, func(t *testing.T) {
			filename := filepath.Join(t.TempDir(), "artifact.json")
			if err := os.WriteFile(filename, value, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := readBaselineArtifact(filename, config); err == nil {
				t.Fatal("invalid JSON was accepted")
			}
		})
	}
}

func TestReadBoundedRegularArtifactRejectsSymlinkAndOversize(t *testing.T) {
	directory := t.TempDir()
	regular := filepath.Join(directory, "screenshot.png")
	content := bytes.Repeat([]byte{0x42}, 128)
	if err := os.WriteFile(regular, content, 0o600); err != nil {
		t.Fatal(err)
	}
	read, err := readBoundedRegularArtifact(regular, int64(len(content)))
	if err != nil || !bytes.Equal(read, content) {
		t.Fatalf("read regular artifact: %v", err)
	}
	if _, err := readBoundedRegularArtifact(regular, int64(len(content)-1)); err == nil {
		t.Fatal("oversized artifact was accepted")
	}
	link := filepath.Join(directory, "screenshot-link.png")
	if err := os.Symlink(regular, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readBoundedRegularArtifact(link, int64(len(content))); err == nil {
		t.Fatal("symlink artifact was accepted")
	}
}

func TestValidateOutputDestinationRejectsExistingArtifact(t *testing.T) {
	directory := t.TempDir()
	output := filepath.Join(directory, "output.json")
	if err := validateOutputDestination(output); err != nil {
		t.Fatalf("new output path: %v", err)
	}
	if err := os.WriteFile(output, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateOutputDestination(output); err == nil {
		t.Fatal("existing output path was accepted")
	}
}

func TestArgumentsAndGeneratedMetadataAreClosed(t *testing.T) {
	if _, err := parseCommandArguments([]string{"--config", controllerConfigPath, "--config", controllerConfigPath}, []string{"--config"}); err == nil {
		t.Fatal("duplicate flag was accepted")
	}
	if _, err := parseCommandArguments([]string{"--unknown", "value"}, []string{"--config"}); err == nil {
		t.Fatal("unknown flag was accepted")
	}
	binding := deliveryBinding{
		DeliveryID: "delivery_0123456789abcdef0123456789abcdef", IssueKey: "TICKET-123",
		InputSHA256: strings.Repeat("1", 64), ConfigSHA256: strings.Repeat("2", 64), ToolSHA: strings.Repeat("3", 40),
		SourceSHA256: strings.Repeat("4", 64), CandidateSHA256: strings.Repeat("5", 64),
		DecisionSHA256: strings.Repeat("6", 64), ValidationSHA256: strings.Repeat("7", 64),
		ProductPaths: []string{"client/src/example.tsx"},
	}
	if branch := featureBranch(binding); branch != "automation/ticket-123-456789abcdef" {
		t.Fatalf("feature branch = %q", branch)
	}
	if !strings.HasPrefix(featureCommitMessage(binding), "Codex:") {
		t.Fatalf("commit message = %q", featureCommitMessage(binding))
	}
	featureSpec := featurePullRequestSpec(binding, "### 実装とレビューの経過\n- 1 周目: 収束")
	promotionSpec := promotionPullRequestSpec(binding, strings.Repeat("8", 64))
	for _, spec := range []githubapi.PullRequestSpec{featureSpec, promotionSpec} {
		if !strings.HasPrefix(spec.Title, "[Codex]") || strings.Contains(spec.Body, "arbitrary request") ||
			!strings.Contains(spec.Body, "Issue: TICKET-123") || strings.Contains(spec.Body, "Summary:") {
			t.Fatalf("PR metadata = %+v", spec)
		}
	}
	if !strings.Contains(featureSpec.Body, "実装とレビューの経過") {
		t.Fatalf("feature PR body lacks the trail: %q", featureSpec.Body)
	}
}

func TestStagingProofMustMatchDeliveryBinding(t *testing.T) {
	binding := deliveryBinding{
		DeliveryID:  "delivery_0123456789abcdef0123456789abcdef",
		InputSHA256: strings.Repeat("1", 64), ConfigSHA256: strings.Repeat("2", 64),
		ToolSHA: strings.Repeat("3", 40), IssueKey: "TICKET-123",
		SourceSHA256: strings.Repeat("3", 64), CandidateSHA256: strings.Repeat("4", 64),
		DecisionSHA256: strings.Repeat("5", 64), ValidationSHA256: strings.Repeat("6", 64),
		ProductPaths: []string{"client/src/example.tsx"},
	}
	proof := releaseproof.StagingProof{
		SchemaVersion: releaseproof.SchemaVersion, DeliveryID: binding.DeliveryID, IssueKey: binding.IssueKey,
		InputSHA256: binding.InputSHA256, ConfigSHA256: binding.ConfigSHA256, ToolSHA: binding.ToolSHA,
		SourceSHA256: binding.SourceSHA256, CandidateSHA256: binding.CandidateSHA256,
		DecisionSHA256: binding.DecisionSHA256, ValidationSHA256: binding.ValidationSHA256,
		ProductPaths: append([]string(nil), binding.ProductPaths...), ProofSHA256: strings.Repeat("7", 64),
	}
	if !stagingProofMatchesBinding(proof, binding) {
		t.Fatal("matching staging proof was rejected")
	}
	tampered := proof
	tampered.CandidateSHA256 = strings.Repeat("9", 64)
	if stagingProofMatchesBinding(tampered, binding) {
		t.Fatal("mismatched staging proof was accepted")
	}
}

func TestPromotionMustNotPredateVisibleEvidence(t *testing.T) {
	observed := time.Date(2026, 8, 3, 4, 5, 6, 900_000_000, time.UTC)
	visible := visiblecheck.Evidence{ObservedAt: observed}
	if !promotionFollowsVisibleEvidence(githubapi.PullRequest{CreatedAt: observed.Truncate(time.Second)}, visible) ||
		!promotionFollowsVisibleEvidence(githubapi.PullRequest{CreatedAt: observed.Add(time.Second)}, visible) {
		t.Fatal("valid promotion chronology was rejected")
	}
	if promotionFollowsVisibleEvidence(githubapi.PullRequest{CreatedAt: observed.Add(-time.Second)}, visible) ||
		promotionFollowsVisibleEvidence(githubapi.PullRequest{}, visible) {
		t.Fatal("promotion predating visible evidence was accepted")
	}
}

func enterRepositoryRoot(t *testing.T) {
	t.Helper()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs(filepath.Join(previous, "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(previous); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})
}

// testPrimaryConsumer is the first destination of the checked-in
// configuration; the fake GitHub echoes whatever it says, so these tests are
// bound to the configuration file and not to any fixed table.
func testPrimaryConsumer() worker.ConsumerConfig {
	config, err := loadFixedConfig(controllerConfigPath)
	if err != nil {
		panic(err)
	}
	return config.Consumers[0]
}

func allWorkflowContracts() []githubapi.WorkflowContract {
	contract := testPrimaryConsumer().Contract()
	workflows := append([]githubapi.WorkflowContract(nil), contract.FeatureWorkflows...)
	workflows = append(workflows, contract.StagingWorkflow)
	workflows = append(workflows, contract.ProductionWorkflows...)
	return workflows
}

func decimal(value int64) string {
	const digits = "0123456789"
	if value <= 0 {
		return ""
	}
	var buffer [20]byte
	index := len(buffer)
	for value > 0 {
		index--
		buffer[index] = digits[value%10]
		value /= 10
	}
	return string(buffer[index:])
}

func jsonResponse(status int, value any) *http.Response {
	encoded, _ := json.Marshal(value)
	return rawResponse(status, string(encoded))
}

func rawResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
