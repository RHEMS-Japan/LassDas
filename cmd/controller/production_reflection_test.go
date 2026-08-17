package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/githubapi"
)

func TestWriteProductionReflectionArtifactSealsAcceptedMergeFact(t *testing.T) {
	enterRepositoryRoot(t)
	fixture, promotion, reflection, merge := newProductionReflectionFixture(t)
	filename := filepath.Join(t.TempDir(), "production-reflection.json")

	if err := writeProductionReflectionArtifact(filename, promotion, reflection); err != nil {
		t.Fatalf("writeProductionReflectionArtifact() error = %v", err)
	}
	config, err := loadFixedConfig(controllerConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := readDeliveryArtifact[productionReflectionPayload](
		filename, kindProductionReflection, fixture.request, config,
	)
	if err != nil || !validProductionReflectionPayload(artifact.Payload, artifact.Binding) {
		t.Fatalf("production reflection artifact is invalid: %v", err)
	}
	if artifact.Payload.Reflection != reflection || !reflectionMatchesMerge(reflection, merge) {
		t.Fatalf("reflection = %+v; merge = %+v", artifact.Payload.Reflection, merge)
	}
}

func TestProductionReflectionRejectsIdentityDrift(t *testing.T) {
	enterRepositoryRoot(t)
	_, promotion, exact, _ := newProductionReflectionFixture(t)
	tests := []struct {
		name   string
		mutate func(*githubapi.MergeReflection)
	}{
		{name: "pull request", mutate: func(value *githubapi.MergeReflection) { value.PullRequestNumber++ }},
		{name: "base branch", mutate: func(value *githubapi.MergeReflection) { value.BaseBranch = "main" }},
		{name: "base sha", mutate: func(value *githubapi.MergeReflection) { value.BaseSHA = promotionObjectID("c") }},
		{name: "head branch", mutate: func(value *githubapi.MergeReflection) { value.HeadBranch = "feature" }},
		{name: "head sha", mutate: func(value *githubapi.MergeReflection) { value.HeadSHA = promotionObjectID("d") }},
		{name: "merge sha", mutate: func(value *githubapi.MergeReflection) { value.MergeSHA = "invalid" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reflection := exact
			test.mutate(&reflection)
			payload := productionReflectionPayload{
				Release: promotion.Payload.Release, Proof: promotion.Payload.Proof,
				PullRequest: promotion.Payload.PullRequest, Reflection: reflection,
			}
			if validProductionReflectionPayload(payload, promotion.Binding) {
				t.Fatal("validProductionReflectionPayload() accepted drift")
			}
		})
	}
}

func TestProductionReflectionOutputMustBeSeparateAndUnused(t *testing.T) {
	directory := t.TempDir()
	full := filepath.Join(directory, "promotion-merge.json")
	receipt := filepath.Join(directory, "production-reflection.json")
	if !distinctOutputDestinations(full, receipt) || distinctOutputDestinations(full, full) {
		t.Fatal("distinctOutputDestinations() did not enforce separate paths")
	}
	if validateOutputDestination(full) != nil || validateOutputDestination(receipt) != nil {
		t.Fatal("unused output destination was rejected")
	}
	if err := os.WriteFile(receipt, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	if validateOutputDestination(receipt) == nil {
		t.Fatal("validateOutputDestination() accepted an existing receipt")
	}
	realParent := filepath.Join(directory, "real", "outputs")
	if err := os.MkdirAll(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(directory, "alias")
	if err := os.Symlink(filepath.Join(directory, "real"), alias); err != nil {
		t.Fatal(err)
	}
	if distinctOutputDestinations(
		filepath.Join(realParent, "same.json"), filepath.Join(alias, "outputs", "same.json"),
	) {
		t.Fatal("distinctOutputDestinations() accepted symlink aliases")
	}
}

func TestMergePromotionRejectsSharedOutputBeforeGitHub(t *testing.T) {
	directory := t.TempDir()
	shared := filepath.Join(directory, "shared.json")
	arguments := []string{
		"merge-promotion",
		"--config", controllerConfigPath,
		"--ticket", "ticket.json",
		"--source", "source.json",
		"--candidate", "candidate.json",
		"--review", "review.json",
		"--decision", "decision.json",
		"--validation", "validation.json",
		"--baseline", "baseline.json",
		"--promotion", "promotion.json",
		"--reflection-out", shared,
		"--out", shared,
	}
	transport := &fakeGitHubTransport{}
	err := run(context.Background(), arguments, func(string) string { return fakeGitHubToken }, transport)
	if failureCode(err) != "production_reflection_path_invalid" {
		t.Fatalf("failureCode() = %q; error = %v", failureCode(err), err)
	}
	if requests := transport.requestSnapshot(); len(requests) != 0 {
		t.Fatalf("GitHub requests before output validation = %d", len(requests))
	}
}

func TestWorkflowRoutesFailedReflectedMergeToUnverified(t *testing.T) {
	enterRepositoryRoot(t)
	encoded, err := os.ReadFile(".github/workflows/m1-worker.yml")
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(encoded)
	required := []string{
		"production_reflected: ${{ steps.reflection.outputs.production_reflected }}",
		"MERGE_OUTCOME: ${{ steps.merge.outcome }}",
		`elif [[ "$MERGE_OUTCOME" == skipped ]]; then`,
		`printf 'production_reflected=unknown\n' >> "$GITHUB_OUTPUT"`,
		"if: ${{ !cancelled() && steps.reflection.outputs.production_reflected == 'true' }}",
		"needs.promotion-merge.result != 'success' && needs.promotion-merge.outputs.production_reflected == 'true'",
		"PROMOTION_MERGE_RESULT: ${{ needs.promotion-merge.result }}",
		"PRODUCTION_REFLECTED: ${{ needs.promotion-merge.outputs.production_reflected }}",
		`"$PRODUCTION_REFLECTED" != true && "$PRODUCTION_REFLECTED" != false`,
		`"$PROMOTION_MERGE_RESULT" == cancelled && "$PRODUCTION_REFLECTED" != true`,
		`elif [[ "$PROMOTION_MERGE_RESULT" == success || "$PRODUCTION_REFLECTED" == true ]]; then`,
		"code=production_deployment_unverified",
		`select(.kind == "m1-production-reflection")`,
	}
	for _, fragment := range required {
		if !strings.Contains(workflow, fragment) {
			t.Fatalf("workflow is missing reflection contract %q", fragment)
		}
	}
	reflectedIndex := strings.Index(workflow, `elif [[ "$PROMOTION_MERGE_RESULT" == success || "$PRODUCTION_REFLECTED" == true ]]; then`)
	if reflectedIndex < 0 {
		t.Fatal("production reflection selector is missing")
	}
	releaseFailedIndex := strings.Index(workflow[reflectedIndex:], "code=release_failed")
	if releaseFailedIndex < 0 {
		t.Fatal("production reflection is not selected before release_failed")
	}
}

func newProductionReflectionFixture(t *testing.T) (
	promotionRejectionFixture,
	deliveryArtifact[promotionPayload],
	githubapi.MergeReflection,
	githubapi.MergeResult,
) {
	t.Helper()
	fixture := newPromotionRejectionFixture(t)
	binding := fixture.staging.Binding
	proof := githubapi.PromotionProof{
		Baseline: fixture.baseline.Baseline, Staging: fixture.staging.Payload.StagingDeployment,
		ProductPaths: slices.Clone(binding.ProductPaths), AcceptanceEvidenceSHA256: fixture.visible.EvidenceSHA256,
	}
	spec := promotionPullRequestSpec(binding, proof.AcceptanceEvidenceSHA256)
	pull := githubapi.PullRequest{
		Number: 51, HTMLURL: "https://github.com/example/consumer/pull/51",
		Title: spec.Title, Body: spec.Body, CreatedAt: fixture.visible.ObservedAt.Add(time.Second),
		HeadRef: testPrimaryConsumer().IntegrationBranch, HeadSHA: proof.Staging.BranchHeadSHA,
		BaseRef: testPrimaryConsumer().ReleaseBranch, BaseSHA: proof.Baseline.Release.SHA,
		HeadFullName: testPrimaryConsumer().Repository,
	}
	promotion := deliveryArtifact[promotionPayload]{
		Binding: binding,
		Payload: promotionPayload{Release: fixture.staging.Payload, Proof: proof, PullRequest: pull},
	}
	if !validPromotionPayload(promotion.Payload, promotion.Binding) {
		t.Fatal("promotion fixture is invalid")
	}
	reflection := githubapi.MergeReflection{
		PullRequestNumber: pull.Number, BaseBranch: pull.BaseRef, BaseSHA: pull.BaseSHA,
		HeadBranch: pull.HeadRef, HeadSHA: pull.HeadSHA, MergeSHA: promotionObjectID("b"),
	}
	merge := githubapi.MergeResult{
		PullRequestNumber: reflection.PullRequestNumber,
		BaseBranch:        reflection.BaseBranch, BaseSHA: reflection.BaseSHA,
		HeadBranch: reflection.HeadBranch, HeadSHA: reflection.HeadSHA,
		MergeSHA: reflection.MergeSHA, TreeSHA: promotionObjectID("e"),
	}
	return fixture, promotion, reflection, merge
}
