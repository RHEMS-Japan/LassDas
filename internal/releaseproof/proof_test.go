package releaseproof

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/githubapi"
	"automation.internal/ticket-ingress/internal/worker"
)

var fixtureTime = time.Date(2026, 8, 3, 1, 0, 0, 0, time.UTC)

func TestStagingProofCanonicalRoundTrip(t *testing.T) {
	input := stagingFixture(t)
	first, err := NewStagingProof(input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewStagingProof(input)
	if err != nil {
		t.Fatal(err)
	}
	if first.ProofSHA256 != second.ProofSHA256 {
		t.Fatalf("proof digests differ: %q != %q", first.ProofSHA256, second.ProofSHA256)
	}
	if first.ProofSHA256 != "354d2ee5309f201eb326168f38c779e3afc5bcd59de23bcea67209068dc0623d" {
		t.Fatalf("canonical staging proof digest changed: %q", first.ProofSHA256)
	}
	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Fatal("canonical proof JSON differs for identical input")
	}
	var decoded StagingProof
	if err := json.Unmarshal(firstJSON, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := decoded.Validate(input); err != nil {
		t.Fatalf("decoded.Validate() error = %v", err)
	}
	latest, err := ValidateStagingDeployment(first.StagingDeployment, fixtureConsumer())
	if err != nil || !latest.Equal(input.StagingDeployment.WorkflowRuns[0].UpdatedAt) {
		t.Fatalf("latest = %v; error = %v", latest, err)
	}

	// Constructor-owned slices must not alias mutable controller results.
	input.PublishedFeature.Paths[0] = "client/src/changed-after-seal.tsx"
	input.FeatureChecks.WorkflowRunIDs[0] = 999
	input.StagingDeployment.DigestPaths[0] = "unexpected"
	if first.PublishedFeature.Paths[0] == input.PublishedFeature.Paths[0] ||
		first.FeatureChecks.WorkflowRunIDs[0] == input.FeatureChecks.WorkflowRunIDs[0] ||
		first.StagingDeployment.DigestPaths[0] == input.StagingDeployment.DigestPaths[0] {
		t.Fatal("sealed proof aliases mutable input slices")
	}
}

func TestStagingProofRejectsResealedSemanticTampering(t *testing.T) {
	input := stagingFixture(t)
	proof, err := NewStagingProof(input)
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]func(*StagingProof){
		"ticket digest":  func(value *StagingProof) { value.InputSHA256 = strings.Repeat("f", 64) },
		"baseline":       func(value *StagingProof) { value.Baseline.Integration.TreeSHA = objectID("b") },
		"published path": func(value *StagingProof) { value.PublishedFeature.Paths[0] = "client/src/other.tsx" },
		"pull body":      func(value *StagingProof) { value.FeaturePullRequest.Body += "\nInjected: true" },
		"check ids": func(value *StagingProof) {
			value.FeatureChecks.WorkflowRunIDs[0] = 0
		},
		"merge tree": func(value *StagingProof) { value.FeatureMerge.TreeSHA = objectID("b") },
		"workflow":   func(value *StagingProof) { value.StagingDeployment.WorkflowRuns[0].WorkflowID++ },
		"workflow chronology": func(value *StagingProof) {
			value.StagingDeployment.WorkflowRuns[0].CreatedAt = value.FeaturePullRequest.CreatedAt.Add(-2 * time.Minute)
			value.StagingDeployment.WorkflowRuns[0].UpdatedAt = value.FeaturePullRequest.CreatedAt.Add(-time.Minute)
		},
		"digest paths": func(value *StagingProof) {
			value.StagingDeployment.DigestPaths[0], value.StagingDeployment.DigestPaths[1] =
				value.StagingDeployment.DigestPaths[1], value.StagingDeployment.DigestPaths[0]
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			tampered := cloneStagingProof(t, proof)
			mutate(&tampered)
			tampered.ProofSHA256, err = stagingProofDigest(tampered)
			if err != nil {
				t.Fatal(err)
			}
			if err := tampered.Validate(input); err == nil {
				t.Fatal("Validate() accepted a resealed tampered staging proof")
			}
		})
	}
}

func TestProductionProofCanonicalChainAndTamperRejection(t *testing.T) {
	input := stagingFixture(t)
	staging, err := NewStagingProof(input)
	if err != nil {
		t.Fatal(err)
	}
	visibleSHA := strings.Repeat("e", 64)
	pull, merge, deployment := productionFixture(staging, visibleSHA)
	proof, err := NewProductionProof(staging, fixtureConsumer(), visibleSHA, pull, merge, deployment)
	if err != nil {
		t.Fatal(err)
	}
	if err := proof.Validate(staging, fixtureConsumer()); err != nil {
		t.Fatal(err)
	}
	if proof.ProofSHA256 != "17fb4976348cd8bdf9b2fde2da9d388cfa93984228e6ca40d8773ce08301aec0" {
		t.Fatalf("canonical production proof digest changed: %q", proof.ProofSHA256)
	}
	second, err := NewProductionProof(staging, fixtureConsumer(), visibleSHA, pull, merge, deployment)
	if err != nil || second.ProofSHA256 != proof.ProofSHA256 {
		t.Fatalf("second proof = %+v; error = %v", second, err)
	}
	latest, err := ValidateProductionDeployment(proof.ProductionDeployment, fixtureConsumer())
	if err != nil || !latest.Equal(deployment.WorkflowRuns[1].UpdatedAt) {
		t.Fatalf("latest = %v; error = %v", latest, err)
	}

	tests := map[string]func(*ProductionProof){
		"staging digest": func(value *ProductionProof) { value.StagingProofSHA256 = strings.Repeat("d", 64) },
		"visible digest": func(value *ProductionProof) { value.StagingVisibleEvidenceSHA256 = strings.Repeat("c", 64) },
		"pull body":      func(value *ProductionProof) { value.PromotionPullRequest.Body += "\nInjected: true" },
		"promotion chronology": func(value *ProductionProof) {
			value.PromotionPullRequest.CreatedAt = staging.StagingDeployment.WorkflowRuns[0].UpdatedAt.Add(-time.Minute)
		},
		"merge head": func(value *ProductionProof) { value.PromotionMerge.HeadSHA = objectID("b") },
		"workflow":   func(value *ProductionProof) { value.ProductionDeployment.WorkflowRuns[1].WorkflowID++ },
		"workflow chronology": func(value *ProductionProof) {
			value.ProductionDeployment.WorkflowRuns[0].CreatedAt = value.PromotionPullRequest.CreatedAt.Add(-2 * time.Minute)
			value.ProductionDeployment.WorkflowRuns[0].UpdatedAt = value.PromotionPullRequest.CreatedAt.Add(-time.Minute)
		},
		"workflow order":    func(value *ProductionProof) { slices.Reverse(value.ProductionDeployment.WorkflowRuns) },
		"production digest": func(value *ProductionProof) { value.ProductionDeployment.DigestCommitSHA = objectID("c") },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			tampered := cloneProductionProof(t, proof)
			mutate(&tampered)
			tampered.ProofSHA256, err = productionProofDigest(tampered)
			if err != nil {
				t.Fatal(err)
			}
			if err := tampered.Validate(staging, fixtureConsumer()); err == nil {
				t.Fatal("Validate() accepted a resealed tampered production proof")
			}
		})
	}
}

func TestStagingProofRequiresWorkerPublishGateAndFixedTarget(t *testing.T) {
	input := stagingFixture(t)
	input.Decision.Outcome = "revise"
	if _, err := NewStagingProof(input); err == nil {
		t.Fatal("NewStagingProof() accepted a non-converged worker decision")
	}

	input = stagingFixture(t)
	input.Config.Consumers[0].Repository = "attacker/repository"
	if _, err := NewStagingProof(input); err == nil {
		t.Fatal("NewStagingProof() accepted a different target")
	}
}

// fixtureConfig is a complete, self-contained configuration for proof
// fixtures. The engine's tests must not read any customer's configuration:
// doing so both re-pinned the canonical digests to customer edits and kept a
// customer name inside the engine's test suite.
func fixtureConfig() worker.Config {
	return worker.Config{
		SchemaVersion: worker.ConfigSchemaVersion,
		Consumers: []worker.ConsumerConfig{{
			Repository: "example/consumer", RepositoryID: 101,
			Delivery: worker.DeliverProduction, IntegrationBranch: "stg", ReleaseBranch: "prod",
			StagingOrigin: "https://stg.example.com", ProductionOrigin: "https://example.com",
			StagingWorkflow: "deploy-stg.yml", ProductionWorkflow: "deploy.yml",
			GitHub: worker.ConsumerGitHubContract{
				DefaultBranch: "prod",
				MergeSettings: worker.ConsumerMergeSettings{
					AllowMergeCommit:         true,
					SquashMergeCommitTitle:   "COMMIT_OR_PR_TITLE",
					SquashMergeCommitMessage: "COMMIT_MESSAGES",
					MergeCommitTitle:         "MERGE_MESSAGE",
					MergeCommitMessage:       "PR_TITLE",
				},
				FeatureWorkflows: []worker.ConsumerWorkflow{
					{ID: 201201, Name: "feature-gate", Path: ".github/workflows/feature-gate.yml", RequiredJobs: []string{"gate"}},
				},
				StagingWorkflow: worker.ConsumerWorkflow{ID: 201202, Name: "Deploy (stg)", Path: ".github/workflows/deploy-stg.yml"},
				ProductionWorkflows: []worker.ConsumerWorkflow{
					{ID: 201203, Name: "Deploy", Path: ".github/workflows/deploy.yml"},
					{ID: 201204, Name: "release-guard", Path: ".github/workflows/release-guard.yml"},
				},
				StagingDigestCommit: &worker.ConsumerDigestCommit{
					RequireDigestOnly:  true,
					ExactMessagePrefix: "[skip ci] update stg image digests for deploy ",
					ExactPaths:         []string{"deploy/prod/kustomization.yaml", "deploy/stg/kustomization.yaml"},
					ActorLogin:         "github-actions[bot]",
				},
			},
			Mode: worker.ModeConfig{
				ID: "client-visible-change", AllowedFilePrefixes: []string{"client/src/"},
				ForbiddenCandidateText: []string{"forbidden-project-name"},
				MaxFiles:               3, MaxFileBytes: 256 * 1024, MaxTotalBytes: 512 * 1024,
				MaxChangedLines: 200, MaxChangedBytes: 64 * 1024,
				Toolchain:              []worker.ToolRequirement{{Binary: "node", Version: "22", StripVPrefix: true}, {Binary: "pnpm", Version: "9.15.4"}},
				VerifyWorkingDirectory: "client",
				InstallCommand:         []string{"pnpm", "install", "--frozen-lockfile"},
				VerifyCommands:         [][]string{{"pnpm", "exec", "tsc", "--noEmit"}, {"pnpm", "build"}},
			},
		}},
		Models: worker.ModelConfig{
			Implementer: worker.ModelEndpoint{ID: "author", Vendor: "Vendor A", Model: "model-a", BaseURL: "https://gateway.example.com/api/v1", APIKeyEnv: "TEST_MODEL_KEY_A", Effort: "low", MaxOutputTokens: 4096},
			Reviewers: []worker.ModelEndpoint{
				{ID: "review-a", Vendor: "Vendor A", Model: "model-a", BaseURL: "https://gateway.example.com/api/v1", APIKeyEnv: "TEST_MODEL_KEY_A", Lens: "correctness", MaxOutputTokens: 2048},
				{ID: "review-b", Vendor: "Vendor B", Model: "model-b", BaseURL: "https://gateway.example.com/api/v1", APIKeyEnv: "TEST_MODEL_KEY_B", Lens: "adversarial", StructuredOutput: true, MaxOutputTokens: 2048},
			},
			Readiness: worker.ReadinessModels{
				Assessor: worker.ModelEndpoint{ID: "readiness-assessor", Vendor: "Vendor A", Model: "model-a", BaseURL: "https://gateway.example.com/api/v1", APIKeyEnv: "TEST_MODEL_KEY_A", Effort: "high", MaxOutputTokens: 4096},
				Checker:  worker.ModelEndpoint{ID: "readiness-checker", Vendor: "Vendor B", Model: "model-b", BaseURL: "https://gateway.example.com/api/v1", APIKeyEnv: "TEST_MODEL_KEY_B", Lens: "readiness adversarial", StructuredOutput: true, MaxOutputTokens: 2048},
			},
		},
		Agents: worker.AgentSet{
			Implementer: worker.AgentConfig{ID: "author-agent", Command: "true", TimeoutSeconds: 900},
			Reviewer:    worker.AgentConfig{ID: "review-agent", Command: "false", TimeoutSeconds: 900},
		},
		MaxStages: 3,
	}
}

func fixtureConsumer() worker.ConsumerConfig {
	return fixtureConfig().Consumers[0]
}

func stagingFixture(t *testing.T) StagingInputs {
	t.Helper()
	config := fixtureConfig()
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	configSHA, err := config.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	request := worker.TicketRequest{
		SchemaVersion: 1, DeliveryID: "delivery_0123456789abcdef0123456789abcdef",
		InputSHA256: strings.Repeat("1", 64), ConfigSHA256: configSHA, ToolSHA: objectID("2"),
		IssueKey: "TICKET-123", RunID: "run_20260803_sample", Repository: config.Consumers[0].Repository, Mode: config.Consumers[0].Mode.ID,
		Summary: "Update one visible label", TargetFiles: []string{"client/src/components/Example.tsx"},
		VerificationPath: "/settings", ExpectedText: "Updated label", AbsentText: "Previous label",
		Request: "Replace the visible label while preserving all surrounding behavior.",
	}
	if err := request.Validate(config); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	filename := filepath.Join(root, filepath.FromSlash(request.TargetFiles[0]))
	if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte("export const label = 'Previous label';\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := worker.ReadSourceSnapshot(root, objectID("3"), request, config)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := worker.NewCandidate(1, worker.ModelCandidateOutput{
		Files:     []worker.ModelCandidateFile{{Path: request.TargetFiles[0], Content: "export const label = 'Updated label';\n"}},
		Rationale: "Update the requested visible label.",
	}, source, request, config, invocation(config.Models.Implementer, "implementer"), fixtureTime)
	if err != nil {
		t.Fatal(err)
	}
	reviews := make([]worker.Review, 0, len(config.Models.Reviewers))
	for index, endpoint := range config.Models.Reviewers {
		review, err := worker.NewReview(1, endpoint, worker.ModelReviewOutput{Verdict: "pass", Findings: []worker.ModelFinding{}},
			candidate, source, request, config, invocation(endpoint, "review-"+string(rune('a'+index))), fixtureTime.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		reviews = append(reviews, review)
	}
	decision, err := worker.DecideStage(candidate, reviews, source, request, config)
	if err != nil {
		t.Fatal(err)
	}
	validation := validationEvidence(t, config, request, source, candidate)

	baselineTree := objectID("4")
	baseline := githubapi.Baseline{
		Integration:  githubapi.Snapshot{Branch: "stg", SHA: source.BaseSHA, TreeSHA: baselineTree},
		Release:      githubapi.Snapshot{Branch: "prod", SHA: objectID("5"), TreeSHA: baselineTree},
		MergeBaseSHA: objectID("6"), MergeBaseTreeSHA: baselineTree,
	}
	identity := StagingProof{
		DeliveryID: request.DeliveryID, IssueKey: request.IssueKey,
		InputSHA256: request.InputSHA256, ConfigSHA256: request.ConfigSHA256, ToolSHA: request.ToolSHA,
		SourceSHA256: source.SourceSHA256, CandidateSHA256: candidate.CandidateSHA256,
		DecisionSHA256: decision.DecisionSHA256, ValidationSHA256: validation.ValidationSHA256,
	}
	feature := githubapi.PublishedFeature{
		Base: baseline.Integration, Branch: featureBranch(request.DeliveryID, request.IssueKey),
		HeadSHA: objectID("7"), TreeSHA: objectID("8"), Paths: slices.Clone(request.TargetFiles),
	}
	pull := githubapi.PullRequest{
		Number: 41, HTMLURL: "https://github.com/example/consumer/pull/41",
		Title: "[Codex] " + request.IssueKey, Body: digestBody(identity, ""),
		CreatedAt: fixtureTime.Add(2 * time.Minute), HeadRef: feature.Branch, HeadSHA: feature.HeadSHA,
		BaseRef: "stg", BaseSHA: feature.Base.SHA,
		HeadFullName: "example/consumer",
	}
	checks := githubapi.CheckEvidence{
		PullRequestNumber: pull.Number, HeadSHA: pull.HeadSHA,
		WorkflowRunIDs: []int64{1001}, WorkflowJobIDs: []int64{2001},
	}
	merge := githubapi.MergeResult{
		PullRequestNumber: pull.Number, BaseBranch: "stg", BaseSHA: pull.BaseSHA,
		HeadBranch: pull.HeadRef, HeadSHA: pull.HeadSHA, MergeSHA: objectID("9"), TreeSHA: feature.TreeSHA,
	}
	contract := fixtureConsumer().Contract()
	run := workflowRun(3001, contract.StagingWorkflow, "stg", merge.MergeSHA, fixtureTime.Add(3*time.Minute))
	staging := githubapi.DeploymentResult{
		Merge: merge, WorkflowRuns: []githubapi.WorkflowRun{run},
		BranchHeadSHA: objectID("a"), DigestCommitSHA: objectID("a"),
		DigestPaths: slices.Clone(fixtureConsumer().StagingDigestCommitPolicy().ExactPaths),
	}
	return StagingInputs{
		Request: request, Config: config, Source: source, Candidate: candidate, Reviews: reviews,
		Decision: decision, Validation: validation, Baseline: baseline, PublishedFeature: feature,
		FeaturePullRequest: pull, FeatureChecks: checks, FeatureMerge: merge, StagingDeployment: staging,
	}
}

func productionFixture(staging StagingProof, visibleSHA string) (githubapi.PullRequest, githubapi.MergeResult, githubapi.DeploymentResult) {
	pull := githubapi.PullRequest{
		Number: 42, HTMLURL: "https://github.com/example/consumer/pull/42",
		Title: "[Codex] " + staging.IssueKey + " promote", Body: digestBody(staging, visibleSHA),
		CreatedAt: fixtureTime.Add(6 * time.Minute), HeadRef: "stg",
		HeadSHA: staging.StagingDeployment.BranchHeadSHA, BaseRef: "prod",
		BaseSHA:      staging.Baseline.Release.SHA,
		HeadFullName: "example/consumer",
	}
	merge := githubapi.MergeResult{
		PullRequestNumber: pull.Number, BaseBranch: "prod", BaseSHA: pull.BaseSHA,
		HeadBranch: "stg", HeadSHA: pull.HeadSHA,
		MergeSHA: objectID("b"), TreeSHA: objectID("c"),
	}
	contract := fixtureConsumer().Contract()
	runs := make([]githubapi.WorkflowRun, len(contract.ProductionWorkflows))
	for index, workflow := range contract.ProductionWorkflows {
		runs[index] = workflowRun(int64(4001+index), workflow, "prod", merge.MergeSHA,
			fixtureTime.Add(time.Duration(7+index)*time.Minute))
	}
	return pull, merge, githubapi.DeploymentResult{Merge: merge, WorkflowRuns: runs, BranchHeadSHA: merge.MergeSHA}
}

func validationEvidence(t *testing.T, config worker.Config, request worker.TicketRequest, source worker.SourceSnapshot, candidate worker.Candidate) worker.ValidationEvidence {
	t.Helper()
	commands := make([][]string, 0, 1+len(config.Consumers[0].Mode.VerifyCommands))
	commands = append(commands, slices.Clone(config.Consumers[0].Mode.InstallCommand))
	for _, command := range config.Consumers[0].Mode.VerifyCommands {
		commands = append(commands, slices.Clone(command))
	}
	files := make([]worker.ValidationFileEvidence, len(candidate.Files))
	for index, file := range candidate.Files {
		files[index] = worker.ValidationFileEvidence{Path: file.Path, SHA256: sha256Hex([]byte(file.Content))}
	}
	evidence := worker.ValidationEvidence{
		SchemaVersion: worker.ValidationEvidenceSchemaVersion,
		DeliveryID:    request.DeliveryID, InputSHA256: request.InputSHA256,
		ConfigSHA256: request.ConfigSHA256, ToolSHA: request.ToolSHA,
		SourceSHA256: source.SourceSHA256, CandidateSHA256: candidate.CandidateSHA256,
		BaseSHA: source.BaseSHA, Stage: candidate.Stage,
		Tools: []worker.ObservedTool{{Binary: "node", Version: "22.12.0"}, {Binary: "pnpm", Version: "9.15.4"}}, Commands: commands, Files: files,
		StartedAt: fixtureTime.Add(90 * time.Second), CompletedAt: fixtureTime.Add(2 * time.Minute),
	}
	encoded, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	evidence.ValidationSHA256 = sha256Hex(encoded)
	if err := evidence.Validate(candidate, source, request, config); err != nil {
		t.Fatal(err)
	}
	return evidence
}

func invocation(endpoint worker.ModelEndpoint, suffix string) worker.InvocationUsage {
	return worker.InvocationUsage{
		RequestedModel: endpoint.Model, RequestID: "request-" + suffix, StopReason: worker.ChatFinishStop,
		InputTokens: 20, OutputTokens: 10, TotalTokens: 30, LatencyMillis: 50,
	}
}

func workflowRun(id int64, workflow githubapi.WorkflowContract, branch, head string, created time.Time) githubapi.WorkflowRun {
	return githubapi.WorkflowRun{
		ID: id, WorkflowID: workflow.ID, Name: workflow.Name, Path: workflow.Path,
		HeadBranch: branch, HeadSHA: head, Event: "push", Status: "completed", Conclusion: "success",
		Attempt: 1, CreatedAt: created, UpdatedAt: created.Add(time.Minute),
	}
}

func objectID(character string) string { return strings.Repeat(character, 40) }

func sha256Hex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func cloneStagingProof(t *testing.T, proof StagingProof) StagingProof {
	t.Helper()
	encoded, err := json.Marshal(proof)
	if err != nil {
		t.Fatal(err)
	}
	var cloned StagingProof
	if err := json.Unmarshal(encoded, &cloned); err != nil {
		t.Fatal(err)
	}
	return cloned
}

func cloneProductionProof(t *testing.T, proof ProductionProof) ProductionProof {
	t.Helper()
	encoded, err := json.Marshal(proof)
	if err != nil {
		t.Fatal(err)
	}
	var cloned ProductionProof
	if err := json.Unmarshal(encoded, &cloned); err != nil {
		t.Fatal(err)
	}
	return cloned
}
