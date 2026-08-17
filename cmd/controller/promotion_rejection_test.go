package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/githubapi"
	"automation.internal/ticket-ingress/internal/releaseproof"
	"automation.internal/ticket-ingress/internal/visiblecheck"
	"automation.internal/ticket-ingress/internal/worker"
)

type promotionRejectionFixture struct {
	request           worker.TicketRequest
	source            worker.SourceSnapshot
	candidate         worker.Candidate
	reviews           []worker.Review
	decision          worker.StageDecision
	validation        worker.ValidationEvidence
	baseline          baselineArtifact
	staging           deliveryArtifact[releaseproof.StagingProof]
	visible           visiblecheck.Evidence
	stagingScreenshot []byte
}

func TestRunCreatePromotionPRRejectsTamperedScreenshotBeforeMutation(t *testing.T) {
	enterRepositoryRoot(t)
	fixture := newPromotionRejectionFixture(t)
	directory := t.TempDir()

	ticketPath := writePromotionFixtureJSON(t, directory, "ticket.json", fixture.request)
	sourcePath := writePromotionFixtureJSON(t, directory, "source.json", fixture.source)
	candidatePath := writePromotionFixtureJSON(t, directory, "candidate.json", fixture.candidate)
	decisionPath := writePromotionFixtureJSON(t, directory, "decision.json", fixture.decision)
	validationPath := writePromotionFixtureJSON(t, directory, "validation.json", fixture.validation)
	baselinePath := writePromotionFixtureJSON(t, directory, "baseline.json", fixture.baseline)
	stagingPath := writePromotionFixtureJSON(t, directory, "staging.json", fixture.staging)
	visiblePath := writePromotionFixtureJSON(t, directory, "visible.json", fixture.visible)
	reviewPaths := make([]string, len(fixture.reviews))
	for index, review := range fixture.reviews {
		reviewPaths[index] = writePromotionFixtureJSON(t, directory, "review-"+string(rune('a'+index))+".json", review)
	}

	tamperedScreenshot := append([]byte(nil), fixture.stagingScreenshot...)
	tamperedScreenshot[len(tamperedScreenshot)/2] ^= 0xff
	screenshotPath := filepath.Join(directory, "staging.png")
	if err := os.WriteFile(screenshotPath, tamperedScreenshot, 0o600); err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(directory, "promotion.json")
	arguments := []string{
		"create-promotion-pr",
		"--config", controllerConfigPath,
		"--ticket", ticketPath,
		"--source", sourcePath,
		"--candidate", candidatePath,
		"--decision", decisionPath,
		"--validation", validationPath,
		"--baseline", baselinePath,
		"--staging", stagingPath,
		"--visible", visiblePath,
		"--screenshot", screenshotPath,
		"--out", outputPath,
	}
	for _, reviewPath := range reviewPaths {
		arguments = append(arguments, "--review", reviewPath)
	}

	transport := &fakeGitHubTransport{}
	err := run(context.Background(), arguments, func(name string) string {
		if name == githubTokenEnvironment {
			return fakeGitHubToken
		}
		return ""
	}, transport)
	if failureCode(err) != "visible_screenshot_rejected" {
		t.Fatalf("failureCode() = %q; error = %v", failureCode(err), err)
	}
	if _, err := os.Lstat(outputPath); !os.IsNotExist(err) {
		t.Fatalf("promotion output exists after rejection: %v", err)
	}

	requests := transport.requestSnapshot()
	mutationCounts := map[string]int{
		http.MethodPost:  0,
		http.MethodPatch: 0,
		http.MethodPut:   0,
	}
	for _, request := range requests {
		if _, tracked := mutationCounts[request.Method]; tracked {
			mutationCounts[request.Method]++
		}
		if request.Method != http.MethodGet {
			t.Errorf("non-read-only request = %s %s", request.Method, request.URL.Path)
		}
	}
	if mutationCounts[http.MethodPost] != 0 || mutationCounts[http.MethodPatch] != 0 || mutationCounts[http.MethodPut] != 0 {
		t.Fatalf("GitHub mutation counts = %+v", mutationCounts)
	}
	if len(requests) != 6 {
		t.Fatalf("read-only Verify request count = %d; want 6", len(requests))
	}
}

func newPromotionRejectionFixture(t *testing.T) promotionRejectionFixture {
	t.Helper()
	base := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	config, err := loadFixedConfig(controllerConfigPath)
	if err != nil {
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
		ToolSHA:          promotionObjectID("2"),
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

	sourceRoot := t.TempDir()
	sourcePath := filepath.Join(sourceRoot, filepath.FromSlash(request.TargetFiles[0]))
	if err := os.MkdirAll(filepath.Dir(sourcePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourcePath, []byte("export const label = 'Previous label';\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := worker.ReadSourceSnapshot(sourceRoot, promotionObjectID("3"), request, config)
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
		promotionInvocation(config.Models.Implementer, "implementer"),
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
			promotionInvocation(endpoint, "review-"+string(rune('a'+index))),
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
	validation := promotionValidationEvidence(t, base, config, request, source, candidate)
	binding := newDeliveryBinding(request, source, candidate, decision, validation)
	if err := binding.validate(request, config); err != nil {
		t.Fatal(err)
	}

	baselineTree := promotionObjectID("4")
	baseline := githubapi.Baseline{
		Integration: githubapi.Snapshot{
			Branch: testPrimaryConsumer().IntegrationBranch, SHA: source.BaseSHA, TreeSHA: baselineTree,
		},
		Release: githubapi.Snapshot{
			Branch: testPrimaryConsumer().ReleaseBranch, SHA: promotionObjectID("5"), TreeSHA: baselineTree,
		},
		MergeBaseSHA: promotionObjectID("6"), MergeBaseTreeSHA: baselineTree,
	}
	baselineEnvelope, err := newBaselineArtifact(config, config.Consumers[0], baseline)
	if err != nil || baselineEnvelope.validate(config) != nil {
		t.Fatalf("baseline fixture is invalid: %v", err)
	}
	feature := githubapi.PublishedFeature{
		Base: baseline.Integration, Branch: featureBranch(binding),
		HeadSHA: promotionObjectID("7"), TreeSHA: promotionObjectID("8"), Paths: slices.Clone(request.TargetFiles),
	}
	pull := githubapi.PullRequest{
		Number: 41, HTMLURL: "https://github.com/example/consumer/pull/41",
		Title: "[Codex] " + request.IssueKey, Body: digestBody(binding, ""),
		CreatedAt: base.Add(3 * time.Minute), HeadRef: feature.Branch, HeadSHA: feature.HeadSHA,
		BaseRef: testPrimaryConsumer().IntegrationBranch, BaseSHA: feature.Base.SHA,
		HeadFullName: testPrimaryConsumer().Repository,
	}
	checks := githubapi.CheckEvidence{
		PullRequestNumber: pull.Number, HeadSHA: pull.HeadSHA,
		WorkflowRunIDs: []int64{1001, 1002}, WorkflowJobIDs: []int64{2001, 2002, 2003},
	}
	merge := githubapi.MergeResult{
		PullRequestNumber: pull.Number, BaseBranch: testPrimaryConsumer().IntegrationBranch, BaseSHA: pull.BaseSHA,
		HeadBranch: pull.HeadRef, HeadSHA: pull.HeadSHA, MergeSHA: promotionObjectID("9"), TreeSHA: feature.TreeSHA,
	}
	workflow := testPrimaryConsumer().Contract().StagingWorkflow
	run := githubapi.WorkflowRun{
		ID: 3001, WorkflowID: workflow.ID, Name: workflow.Name, Path: workflow.Path,
		HeadBranch: testPrimaryConsumer().IntegrationBranch, HeadSHA: merge.MergeSHA,
		Event: "push", Status: "completed", Conclusion: "success", Attempt: 1,
		CreatedAt: base.Add(4 * time.Minute), UpdatedAt: base.Add(5 * time.Minute),
	}
	deployment := githubapi.DeploymentResult{
		Merge: merge, WorkflowRuns: []githubapi.WorkflowRun{run},
		BranchHeadSHA: promotionObjectID("a"), DigestCommitSHA: promotionObjectID("a"),
		DigestPaths: slices.Clone(testPrimaryConsumer().StagingDigestCommitPolicy().ExactPaths),
	}
	stagingInputs := releaseproof.StagingInputs{
		Request: request, Config: config, Source: source, Candidate: candidate, Reviews: reviews,
		Decision: decision, Validation: validation, Baseline: baseline, PublishedFeature: feature,
		FeaturePullRequest: pull, FeatureChecks: checks, FeatureMerge: merge, StagingDeployment: deployment,
	}
	stagingProof, err := releaseproof.NewStagingProof(stagingInputs)
	if err != nil {
		t.Fatal(err)
	}
	stagingEnvelope, err := newDeliveryArtifact(kindStaging, binding, stagingProof)
	if err != nil || stagingEnvelope.validateEnvelope(kindStaging, request, config) != nil {
		t.Fatalf("staging fixture is invalid: %v", err)
	}

	screenshot := promotionScreenshot(t)
	visibleText := "Settings\n" + request.ExpectedText
	visible := visiblecheck.Evidence{
		SchemaVersion: visiblecheck.SchemaVersion,
		DeliveryID:    request.DeliveryID, InputSHA256: request.InputSHA256,
		ConfigSHA256: request.ConfigSHA256, ToolSHA: request.ToolSHA, IssueKey: request.IssueKey,
		CandidateSHA256: candidate.CandidateSHA256, ValidationSHA256: validation.ValidationSHA256,
		Environment: "staging", ReleaseProofSHA256: stagingProof.ProofSHA256,
		ReleaseSHA:           stagingProof.StagingDeployment.Merge.MergeSHA,
		DeploymentWorkflowID: run.WorkflowID, DeploymentRunID: run.ID,
		DeploymentCompletedAt: run.UpdatedAt, BranchHeadSHA: stagingProof.StagingDeployment.BranchHeadSHA,
		RequestedURL: config.Consumers[0].StagingOrigin + request.VerificationPath,
		FinalURL:     config.Consumers[0].StagingOrigin + request.VerificationPath,
		HTTPStatus:   200, Browser: "chrome", BrowserVersion: "140.0.7339.41",
		ObservedAt: base.Add(6 * time.Minute), VisibleTextBytes: len(visibleText),
		VisibleTextSHA256: promotionSHA256([]byte(visibleText)),
		ScreenshotBytes:   len(screenshot), ScreenshotSHA256: promotionSHA256(screenshot),
		ExpectedTextSHA256:  promotionSHA256([]byte(request.ExpectedText)),
		AbsentTextSHA256:    promotionSHA256([]byte(request.AbsentText)),
		ExpectedTextVisible: true, AbsentTextNotVisible: true,
	}
	visible.EvidenceSHA256, err = promotionEvidenceDigest(visible)
	if err != nil {
		t.Fatal(err)
	}
	if err := visible.ValidateStaging(stagingProof, stagingInputs); err != nil {
		t.Fatalf("visible fixture is invalid: %v", err)
	}
	if err := visible.ValidateScreenshot(screenshot); err != nil {
		t.Fatalf("screenshot fixture is invalid: %v", err)
	}

	return promotionRejectionFixture{
		request: request, source: source, candidate: candidate, reviews: reviews,
		decision: decision, validation: validation, baseline: baselineEnvelope,
		staging: stagingEnvelope, visible: visible, stagingScreenshot: screenshot,
	}
}

func promotionValidationEvidence(
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
		files[index] = worker.ValidationFileEvidence{Path: file.Path, SHA256: promotionSHA256([]byte(file.Content))}
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
	evidence.ValidationSHA256 = promotionSHA256(encoded)
	if err := evidence.Validate(candidate, source, request, config); err != nil {
		t.Fatal(err)
	}
	return evidence
}

func promotionInvocation(endpoint worker.ModelEndpoint, suffix string) worker.InvocationUsage {
	return worker.InvocationUsage{
		RequestedModel: endpoint.Model, RequestID: "request-" + suffix, StopReason: worker.ChatFinishStop,
		InputTokens: 20, OutputTokens: 10, TotalTokens: 30, LatencyMillis: 50,
	}
}

func promotionScreenshot(t *testing.T) []byte {
	t.Helper()
	value := image.NewRGBA(image.Rect(0, 0, 4, 3))
	value.Set(1, 1, color.RGBA{R: 0x40, G: 0x80, B: 0xc0, A: 0xff})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, value); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func promotionEvidenceDigest(evidence visiblecheck.Evidence) (string, error) {
	evidence.EvidenceSHA256 = ""
	encoded, err := json.Marshal(evidence)
	if err != nil {
		return "", err
	}
	return promotionSHA256(encoded), nil
}

func promotionSHA256(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func promotionObjectID(character string) string { return strings.Repeat(character, 40) }

func writePromotionFixtureJSON(t *testing.T, directory, name string, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(directory, name)
	if err := os.WriteFile(filename, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return filename
}
