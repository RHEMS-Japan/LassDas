package visiblecheck

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/githubapi"
	"automation.internal/ticket-ingress/internal/releaseproof"
	"automation.internal/ticket-ingress/internal/worker"
)

type releaseBindingFixture struct {
	base              time.Time
	now               time.Time
	input             releaseproof.StagingInputs
	staging           releaseproof.StagingProof
	stagingBinding    releaseBinding
	stagingEvidence   Evidence
	stagingScreenshot []byte
}

func TestStagingReleaseBindingSealsAndReplaysWithoutBrowser(t *testing.T) {
	fixture := newReleaseBindingFixture(t)
	workflow := fixture.staging.StagingDeployment.WorkflowRuns[0]

	if fixture.stagingBinding.environment != "staging" ||
		fixture.stagingBinding.proofSHA256 != fixture.staging.ProofSHA256 ||
		fixture.stagingBinding.primaryRun.ID != workflow.ID ||
		!fixture.stagingBinding.completedAt.Equal(workflow.UpdatedAt) {
		t.Fatalf("stagingBinding() = %+v", fixture.stagingBinding)
	}
	if err := fixture.stagingEvidence.ValidateStaging(fixture.staging, fixture.input); err != nil {
		t.Fatalf("ValidateStaging() error = %v", err)
	}
	if err := fixture.stagingEvidence.ValidateScreenshot(fixture.stagingScreenshot); err != nil {
		t.Fatalf("ValidateScreenshot() error = %v", err)
	}

	tampered := cloneBindingStagingProof(t, fixture.staging)
	tampered.StagingDeployment.WorkflowRuns[0].ID++
	if _, err := stagingBinding(tampered, fixture.input); err == nil {
		t.Fatal("stagingBinding() accepted a tampered release proof")
	}
	if err := fixture.stagingEvidence.ValidateStaging(tampered, fixture.input); err == nil {
		t.Fatal("ValidateStaging() accepted a tampered release proof")
	}
}

func TestProductionReleaseBindingUsesBothWorkflowsLatestCompletion(t *testing.T) {
	fixture := newReleaseBindingFixture(t)
	proof := newBindingProductionProof(t, fixture, fixture.base.Add(7*time.Minute))
	binding, err := productionBinding(
		proof, fixture.staging, fixture.stagingEvidence, fixture.stagingScreenshot, fixture.input, fixture.now,
	)
	if err != nil {
		t.Fatal(err)
	}
	runs := proof.ProductionDeployment.WorkflowRuns
	if len(runs) != 2 {
		t.Fatalf("production workflow count = %d", len(runs))
	}
	if binding.primaryRun.ID != runs[0].ID {
		t.Fatalf("primary run id = %d; want %d", binding.primaryRun.ID, runs[0].ID)
	}
	if !binding.completedAt.Equal(runs[0].UpdatedAt) || !runs[0].UpdatedAt.After(runs[1].UpdatedAt) {
		t.Fatalf("completedAt = %v; workflow updates = %v, %v", binding.completedAt, runs[0].UpdatedAt, runs[1].UpdatedAt)
	}

	productionScreenshot := bindingPNG(t, color.RGBA{R: 0x20, G: 0xa0, B: 0x60, A: 0xff})
	beforeLatestWorkflow := bindingCapture(
		fixture.input.Config.Consumers[0].ProductionOrigin,
		fixture.input.Request,
		runs[1].UpdatedAt.Add(time.Second),
		productionScreenshot,
	)
	if _, err := seal(beforeLatestWorkflow, binding, fixture.input, fixture.now); err == nil {
		t.Fatal("seal() accepted an observation made before the later production workflow completed")
	}

	observed := bindingCapture(
		fixture.input.Config.Consumers[0].ProductionOrigin,
		fixture.input.Request,
		binding.completedAt.Add(time.Minute),
		productionScreenshot,
	)
	evidence, err := seal(observed, binding, fixture.input, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	if err := evidence.ValidateProduction(
		proof, fixture.staging, fixture.stagingEvidence, fixture.stagingScreenshot, fixture.input,
	); err != nil {
		t.Fatalf("ValidateProduction() error = %v", err)
	}
	if err := evidence.ValidateScreenshot(productionScreenshot); err != nil {
		t.Fatalf("ValidateScreenshot() error = %v", err)
	}

	missingGuard := cloneBindingProductionProof(t, proof)
	missingGuard.ProductionDeployment.WorkflowRuns = missingGuard.ProductionDeployment.WorkflowRuns[:1]
	if _, err := productionBinding(
		missingGuard, fixture.staging, fixture.stagingEvidence, fixture.stagingScreenshot, fixture.input, fixture.now,
	); err == nil {
		t.Fatal("productionBinding() accepted a one-workflow production proof")
	}
}

func TestProductionReleaseBindingRejectsParentAndReleaseTampering(t *testing.T) {
	fixture := newReleaseBindingFixture(t)
	proof := newBindingProductionProof(t, fixture, fixture.base.Add(7*time.Minute))
	binding, err := productionBinding(
		proof, fixture.staging, fixture.stagingEvidence, fixture.stagingScreenshot, fixture.input, fixture.now,
	)
	if err != nil {
		t.Fatal(err)
	}
	productionScreenshot := bindingPNG(t, color.RGBA{R: 0xc0, G: 0x40, B: 0x20, A: 0xff})
	productionEvidence, err := seal(
		bindingCapture(
			fixture.input.Config.Consumers[0].ProductionOrigin,
			fixture.input.Request,
			binding.completedAt.Add(time.Minute),
			productionScreenshot,
		),
		binding,
		fixture.input,
		fixture.now,
	)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("parent evidence digest", func(t *testing.T) {
		parent := fixture.stagingEvidence
		parent.ObservedAt = parent.ObservedAt.Add(time.Second)
		parent.EvidenceSHA256, err = evidenceDigest(parent)
		if err != nil {
			t.Fatal(err)
		}
		if err := parent.ValidateStagingAt(fixture.staging, fixture.input, fixture.now); err != nil {
			t.Fatalf("resealed parent fixture is invalid: %v", err)
		}
		if _, err := productionBinding(
			proof, fixture.staging, parent, fixture.stagingScreenshot, fixture.input, fixture.now,
		); err == nil {
			t.Fatal("productionBinding() accepted a different parent evidence digest")
		}
	})

	t.Run("raw staging screenshot", func(t *testing.T) {
		screenshot := append([]byte(nil), fixture.stagingScreenshot...)
		screenshot[len(screenshot)/2] ^= 0xff
		if _, err := productionBinding(
			proof, fixture.staging, fixture.stagingEvidence, screenshot, fixture.input, fixture.now,
		); err == nil {
			t.Fatal("productionBinding() accepted tampered raw staging screenshot bytes")
		}
	})

	t.Run("production release proof", func(t *testing.T) {
		tampered := cloneBindingProductionProof(t, proof)
		tampered.ProductionDeployment.WorkflowRuns[1].ID++
		if _, err := productionBinding(
			tampered, fixture.staging, fixture.stagingEvidence, fixture.stagingScreenshot, fixture.input, fixture.now,
		); err == nil {
			t.Fatal("productionBinding() accepted a tampered production release proof")
		}
		if err := productionEvidence.ValidateProduction(
			tampered, fixture.staging, fixture.stagingEvidence, fixture.stagingScreenshot, fixture.input,
		); err == nil {
			t.Fatal("ValidateProduction() accepted a tampered production release proof")
		}
	})

	t.Run("staging release proof", func(t *testing.T) {
		tampered := cloneBindingStagingProof(t, fixture.staging)
		tampered.FeatureMerge.TreeSHA = bindingObjectID("d")
		if _, err := productionBinding(
			proof, tampered, fixture.stagingEvidence, fixture.stagingScreenshot, fixture.input, fixture.now,
		); err == nil {
			t.Fatal("productionBinding() accepted a tampered staging release proof")
		}
		if err := productionEvidence.ValidateProduction(
			proof, tampered, fixture.stagingEvidence, fixture.stagingScreenshot, fixture.input,
		); err == nil {
			t.Fatal("ValidateProduction() accepted a tampered staging release proof")
		}
	})
}

func TestProductionPromotionMustFollowSealedStagingObservation(t *testing.T) {
	fixture := newReleaseBindingFixture(t)
	createdAt := fixture.stagingEvidence.ObservedAt.Add(-time.Second)
	if createdAt.Before(fixture.stagingBinding.completedAt) {
		t.Fatal("chronology fixture predates the staging deployment")
	}
	proof := newBindingProductionProof(t, fixture, createdAt)
	if err := proof.Validate(fixture.staging, fixtureConsumer()); err != nil {
		t.Fatalf("release-only proof should be valid before visible chronology is applied: %v", err)
	}
	if _, err := productionBinding(
		proof, fixture.staging, fixture.stagingEvidence, fixture.stagingScreenshot, fixture.input, fixture.now,
	); err == nil {
		t.Fatal("productionBinding() accepted promotion created before the sealed staging observation")
	}
}

func newReleaseBindingFixture(t *testing.T) releaseBindingFixture {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	base := now.Add(-2 * time.Hour)
	input := bindingStagingInput(t, base)
	staging, err := releaseproof.NewStagingProof(input)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := stagingBinding(staging, input)
	if err != nil {
		t.Fatal(err)
	}
	screenshot := bindingPNG(t, color.RGBA{R: 0x40, G: 0x80, B: 0xc0, A: 0xff})
	evidence, err := seal(
		bindingCapture(input.Config.Consumers[0].StagingOrigin, input.Request, binding.completedAt.Add(time.Minute), screenshot),
		binding,
		input,
		now,
	)
	if err != nil {
		t.Fatal(err)
	}
	return releaseBindingFixture{
		base: base, now: now, input: input, staging: staging, stagingBinding: binding,
		stagingEvidence: evidence, stagingScreenshot: screenshot,
	}
}

func bindingStagingInput(t *testing.T, base time.Time) releaseproof.StagingInputs {
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
		SchemaVersion:    1,
		DeliveryID:       "delivery_0123456789abcdef0123456789abcdef",
		InputSHA256:      strings.Repeat("1", 64),
		ConfigSHA256:     configSHA,
		ToolSHA:          bindingObjectID("2"),
		IssueKey:         "TICKET-123",
		RunID:            "run_20260803_sample",
		Repository:       config.Consumers[0].Repository,
		Mode:             config.Consumers[0].Mode.ID,
		Summary:          "Update one visible label",
		TargetFiles:      []string{"client/src/components/Example.tsx"},
		VerificationPath: "/settings",
		ExpectedText:     "Updated label",
		AbsentText:       "Previous label",
		Request:          "Replace the visible label while preserving all surrounding behavior.",
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
	source, err := worker.ReadSourceSnapshot(root, bindingObjectID("3"), request, config)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := worker.NewCandidate(
		1,
		worker.ModelCandidateOutput{
			Files: []worker.ModelCandidateFile{{
				Path: request.TargetFiles[0], Content: "export const label = 'Updated label';\n",
			}},
			Rationale: "Update the requested visible label.",
		},
		source,
		request,
		config,
		bindingInvocation(config.Models.Implementer, "implementer"),
		base,
	)
	if err != nil {
		t.Fatal(err)
	}
	reviews := make([]worker.Review, 0, len(config.Models.Reviewers))
	for index, endpoint := range config.Models.Reviewers {
		review, err := worker.NewReview(
			1,
			endpoint,
			worker.ModelReviewOutput{Verdict: "pass", Findings: []worker.ModelFinding{}},
			candidate,
			source,
			request,
			config,
			bindingInvocation(endpoint, "review-"+string(rune('a'+index))),
			base.Add(time.Minute),
		)
		if err != nil {
			t.Fatal(err)
		}
		reviews = append(reviews, review)
	}
	decision, err := worker.DecideStage(candidate, reviews, source, request, config)
	if err != nil {
		t.Fatal(err)
	}
	validation := bindingValidationEvidence(t, base, config, request, source, candidate)

	baselineTree := bindingObjectID("4")
	baseline := githubapi.Baseline{
		Integration: githubapi.Snapshot{
			Branch: "stg", SHA: source.BaseSHA, TreeSHA: baselineTree,
		},
		Release: githubapi.Snapshot{
			Branch: "prod", SHA: bindingObjectID("5"), TreeSHA: baselineTree,
		},
		MergeBaseSHA: bindingObjectID("6"), MergeBaseTreeSHA: baselineTree,
	}
	identity := releaseproof.StagingProof{
		DeliveryID: request.DeliveryID, IssueKey: request.IssueKey,
		InputSHA256: request.InputSHA256, ConfigSHA256: request.ConfigSHA256, ToolSHA: request.ToolSHA,
		SourceSHA256: source.SourceSHA256, CandidateSHA256: candidate.CandidateSHA256,
		DecisionSHA256: decision.DecisionSHA256, ValidationSHA256: validation.ValidationSHA256,
	}
	feature := githubapi.PublishedFeature{
		Base: baseline.Integration, Branch: bindingFeatureBranch(request.DeliveryID, request.IssueKey),
		HeadSHA: bindingObjectID("7"), TreeSHA: bindingObjectID("8"), Paths: slices.Clone(request.TargetFiles),
	}
	pull := githubapi.PullRequest{
		Number: 41, HTMLURL: "https://github.com/example/consumer/pull/41",
		Title: "[Codex] " + request.IssueKey, Body: bindingDigestBody(identity, ""),
		CreatedAt: base.Add(3 * time.Minute), HeadRef: feature.Branch, HeadSHA: feature.HeadSHA,
		BaseRef: "stg", BaseSHA: feature.Base.SHA,
		HeadFullName: "example/consumer",
	}
	checks := githubapi.CheckEvidence{
		PullRequestNumber: pull.Number, HeadSHA: pull.HeadSHA,
		WorkflowRunIDs: []int64{1001}, WorkflowJobIDs: []int64{2001},
	}
	merge := githubapi.MergeResult{
		PullRequestNumber: pull.Number, BaseBranch: "stg", BaseSHA: pull.BaseSHA,
		HeadBranch: pull.HeadRef, HeadSHA: pull.HeadSHA, MergeSHA: bindingObjectID("9"), TreeSHA: feature.TreeSHA,
	}
	contract := fixtureConsumer().Contract()
	run := bindingWorkflowRun(
		3001, contract.StagingWorkflow, "stg", merge.MergeSHA, base.Add(4*time.Minute),
	)
	staging := githubapi.DeploymentResult{
		Merge: merge, WorkflowRuns: []githubapi.WorkflowRun{run},
		BranchHeadSHA: bindingObjectID("a"), DigestCommitSHA: bindingObjectID("a"),
		DigestPaths: slices.Clone(fixtureConsumer().StagingDigestCommitPolicy().ExactPaths),
	}
	return releaseproof.StagingInputs{
		Request: request, Config: config, Source: source, Candidate: candidate, Reviews: reviews,
		Decision: decision, Validation: validation, Baseline: baseline, PublishedFeature: feature,
		FeaturePullRequest: pull, FeatureChecks: checks, FeatureMerge: merge, StagingDeployment: staging,
	}
}

func newBindingProductionProof(
	t *testing.T,
	fixture releaseBindingFixture,
	promotionCreatedAt time.Time,
) releaseproof.ProductionProof {
	t.Helper()
	pull := githubapi.PullRequest{
		Number: 42, HTMLURL: "https://github.com/example/consumer/pull/42",
		Title:     "[Codex] " + fixture.staging.IssueKey + " promote",
		Body:      bindingDigestBody(fixture.staging, fixture.stagingEvidence.EvidenceSHA256),
		CreatedAt: promotionCreatedAt, HeadRef: "stg",
		HeadSHA: fixture.staging.StagingDeployment.BranchHeadSHA, BaseRef: "prod",
		BaseSHA:      fixture.staging.Baseline.Release.SHA,
		HeadFullName: "example/consumer",
	}
	merge := githubapi.MergeResult{
		PullRequestNumber: pull.Number, BaseBranch: "prod", BaseSHA: pull.BaseSHA,
		HeadBranch: "stg", HeadSHA: pull.HeadSHA,
		MergeSHA: bindingObjectID("b"), TreeSHA: bindingObjectID("c"),
	}
	contract := fixtureConsumer().Contract()
	runs := []githubapi.WorkflowRun{
		bindingWorkflowRun(4001, contract.ProductionWorkflows[0], "prod", merge.MergeSHA, fixture.base.Add(10*time.Minute)),
		bindingWorkflowRun(4002, contract.ProductionWorkflows[1], "prod", merge.MergeSHA, fixture.base.Add(9*time.Minute)),
	}
	deployment := githubapi.DeploymentResult{Merge: merge, WorkflowRuns: runs, BranchHeadSHA: merge.MergeSHA}
	proof, err := releaseproof.NewProductionProof(
		fixture.staging,
		fixtureConsumer(),
		fixture.stagingEvidence.EvidenceSHA256,
		pull,
		merge,
		deployment,
	)
	if err != nil {
		t.Fatal(err)
	}
	return proof
}

func bindingValidationEvidence(
	t *testing.T,
	base time.Time,
	config worker.Config,
	request worker.TicketRequest,
	source worker.SourceSnapshot,
	candidate worker.Candidate,
) worker.ValidationEvidence {
	t.Helper()
	commands := make([][]string, 0, 1+len(config.Consumers[0].Mode.VerifyCommands))
	commands = append(commands, slices.Clone(config.Consumers[0].Mode.InstallCommand))
	for _, command := range config.Consumers[0].Mode.VerifyCommands {
		commands = append(commands, slices.Clone(command))
	}
	files := make([]worker.ValidationFileEvidence, len(candidate.Files))
	for index, file := range candidate.Files {
		files[index] = worker.ValidationFileEvidence{Path: file.Path, SHA256: digest([]byte(file.Content))}
	}
	evidence := worker.ValidationEvidence{
		SchemaVersion: worker.ValidationEvidenceSchemaVersion,
		DeliveryID:    request.DeliveryID, InputSHA256: request.InputSHA256,
		ConfigSHA256: request.ConfigSHA256, ToolSHA: request.ToolSHA,
		SourceSHA256: source.SourceSHA256, CandidateSHA256: candidate.CandidateSHA256,
		BaseSHA: source.BaseSHA, Stage: candidate.Stage,
		Tools: []worker.ObservedTool{{Binary: "node", Version: "22.12.0"}, {Binary: "pnpm", Version: "9.15.4"}}, Commands: commands, Files: files,
		StartedAt: base.Add(90 * time.Second), CompletedAt: base.Add(2 * time.Minute),
	}
	encoded, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	evidence.ValidationSHA256 = digest(encoded)
	if err := evidence.Validate(candidate, source, request, config); err != nil {
		t.Fatal(err)
	}
	return evidence
}

func bindingInvocation(endpoint worker.ModelEndpoint, suffix string) worker.InvocationUsage {
	return worker.InvocationUsage{
		RequestedModel: endpoint.Model, RequestID: "request-" + suffix, StopReason: worker.ChatFinishStop,
		InputTokens: 20, OutputTokens: 10, TotalTokens: 30, LatencyMillis: 50,
	}
}

func bindingWorkflowRun(
	id int64,
	workflow githubapi.WorkflowContract,
	branch string,
	head string,
	created time.Time,
) githubapi.WorkflowRun {
	return githubapi.WorkflowRun{
		ID: id, WorkflowID: workflow.ID, Name: workflow.Name, Path: workflow.Path,
		HeadBranch: branch, HeadSHA: head, Event: "push", Status: "completed", Conclusion: "success",
		Attempt: 1, CreatedAt: created, UpdatedAt: created.Add(time.Minute),
	}
}

func bindingCapture(origin string, request worker.TicketRequest, observedAt time.Time, screenshot []byte) capture {
	url := origin + request.VerificationPath
	return capture{
		requestedURL: url, finalURL: url, statusCode: 200,
		visibleText:   "Settings\n" + request.ExpectedText,
		screenshotPNG: append([]byte(nil), screenshot...),
		browser:       "chrome", BrowserVersion: "140.0.7339.41", observedAt: observedAt,
	}
}

func bindingPNG(t *testing.T, pixel color.RGBA) []byte {
	t.Helper()
	value := image.NewRGBA(image.Rect(0, 0, 4, 3))
	value.Set(1, 1, pixel)
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, value); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func bindingFeatureBranch(deliveryID, issueKey string) string {
	delivery := strings.TrimPrefix(deliveryID, "delivery_")
	return "automation/" + strings.ToLower(issueKey) + "-" + delivery[len(delivery)-12:]
}

func bindingDigestBody(proof releaseproof.StagingProof, evidenceSHA string) string {
	lines := []string{
		"Issue: " + proof.IssueKey,
		"Input-SHA256: " + proof.InputSHA256,
		"Config-SHA256: " + proof.ConfigSHA256,
		"Tool-SHA: " + proof.ToolSHA,
		"Source-SHA256: " + proof.SourceSHA256,
		"Candidate-SHA256: " + proof.CandidateSHA256,
		"Decision-SHA256: " + proof.DecisionSHA256,
		"Validation-SHA256: " + proof.ValidationSHA256,
	}
	if evidenceSHA != "" {
		lines = append(lines, "Evidence-SHA256: "+evidenceSHA)
	}
	return strings.Join(lines, "\n")
}

func bindingObjectID(character string) string { return strings.Repeat(character, 40) }

func cloneBindingStagingProof(t *testing.T, proof releaseproof.StagingProof) releaseproof.StagingProof {
	t.Helper()
	encoded, err := json.Marshal(proof)
	if err != nil {
		t.Fatal(err)
	}
	var cloned releaseproof.StagingProof
	if err := json.Unmarshal(encoded, &cloned); err != nil {
		t.Fatal(err)
	}
	return cloned
}

func cloneBindingProductionProof(t *testing.T, proof releaseproof.ProductionProof) releaseproof.ProductionProof {
	t.Helper()
	encoded, err := json.Marshal(proof)
	if err != nil {
		t.Fatal(err)
	}
	var cloned releaseproof.ProductionProof
	if err := json.Unmarshal(encoded, &cloned); err != nil {
		t.Fatal(err)
	}
	return cloned
}

// fixtureConfig mirrors the proof fixtures: a complete, self-contained
// configuration so this package's tests read no customer's file.
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
