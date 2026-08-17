package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"automation.internal/ticket-ingress/internal/githubapi"
	"automation.internal/ticket-ingress/internal/releaseproof"
	"automation.internal/ticket-ingress/internal/worker"
)

const (
	controllerArtifactSchemaVersion = 1
	controllerArtifactMaxBytes      = worker.MaxArtifactJSONBytes

	kindBaseline             = "m1-baseline"
	kindFeature              = "m1-published-feature"
	kindFeaturePR            = "m1-feature-pull-request"
	kindFeatureChecks        = "m1-feature-checks"
	kindFeatureMerge         = "m1-feature-merge"
	kindStaging              = "m1-staging-deployment"
	kindPromotion            = "m1-promotion-pull-request"
	kindProductionReflection = "m1-production-reflection"
	kindPromotionMerge       = "m1-promotion-merge"
	kindProduction           = "m1-production-deployment"
	controllerConfigPath     = "config/m1-consumer.json"
)

type baselineArtifact struct {
	SchemaVersion  int                `json:"schema_version"`
	Kind           string             `json:"kind"`
	ConfigSHA256   string             `json:"config_sha256"`
	Repository     string             `json:"repository"`
	Baseline       githubapi.Baseline `json:"baseline"`
	ArtifactSHA256 string             `json:"artifact_sha256"`
}

type deliveryBinding struct {
	DeliveryID       string   `json:"delivery_id"`
	InputSHA256      string   `json:"input_sha256"`
	ConfigSHA256     string   `json:"config_sha256"`
	ToolSHA          string   `json:"tool_sha"`
	IssueKey         string   `json:"issue_key"`
	Repository       string   `json:"repository"`
	SourceSHA256     string   `json:"source_sha256"`
	CandidateSHA256  string   `json:"candidate_sha256"`
	DecisionSHA256   string   `json:"decision_sha256"`
	ValidationSHA256 string   `json:"validation_sha256"`
	ProductPaths     []string `json:"product_paths"`
}

type deliveryArtifact[T any] struct {
	SchemaVersion  int             `json:"schema_version"`
	Kind           string          `json:"kind"`
	Binding        deliveryBinding `json:"binding"`
	Payload        T               `json:"payload"`
	ArtifactSHA256 string          `json:"artifact_sha256"`
}

type featurePRPayload struct {
	Feature     githubapi.PublishedFeature `json:"feature"`
	PullRequest githubapi.PullRequest      `json:"pull_request"`
}

type featureChecksPayload struct {
	Feature     githubapi.PublishedFeature `json:"feature"`
	PullRequest githubapi.PullRequest      `json:"pull_request"`
	Checks      githubapi.CheckEvidence    `json:"checks"`
}

type featureMergePayload struct {
	Feature     githubapi.PublishedFeature `json:"feature"`
	PullRequest githubapi.PullRequest      `json:"pull_request"`
	Checks      githubapi.CheckEvidence    `json:"checks"`
	Merge       githubapi.MergeResult      `json:"merge"`
}

type promotionPayload struct {
	Release     releaseproof.StagingProof `json:"release"`
	Proof       githubapi.PromotionProof  `json:"proof"`
	PullRequest githubapi.PullRequest     `json:"pull_request"`
}

type promotionMergePayload struct {
	Release     releaseproof.StagingProof `json:"release"`
	Proof       githubapi.PromotionProof  `json:"proof"`
	PullRequest githubapi.PullRequest     `json:"pull_request"`
	Merge       githubapi.MergeResult     `json:"merge"`
}

type productionReflectionPayload struct {
	Release     releaseproof.StagingProof `json:"release"`
	Proof       githubapi.PromotionProof  `json:"proof"`
	PullRequest githubapi.PullRequest     `json:"pull_request"`
	Reflection  githubapi.MergeReflection `json:"reflection"`
}

type productionPayload struct {
	Staging    releaseproof.StagingProof    `json:"staging"`
	Production releaseproof.ProductionProof `json:"production"`
}

func newBaselineArtifact(config worker.Config, consumer worker.ConsumerConfig, baseline githubapi.Baseline) (baselineArtifact, error) {
	configSHA, err := config.SHA256()
	if err != nil {
		return baselineArtifact{}, err
	}
	artifact := baselineArtifact{
		SchemaVersion: controllerArtifactSchemaVersion,
		Kind:          kindBaseline,
		ConfigSHA256:  configSHA,
		Repository:    consumer.Repository,
		Baseline:      baseline,
	}
	artifact.ArtifactSHA256, err = digestBaselineArtifact(artifact)
	if err != nil {
		return baselineArtifact{}, err
	}
	return artifact, nil
}

func (a baselineArtifact) validate(config worker.Config) error {
	configSHA, err := config.SHA256()
	if err != nil || a.SchemaVersion != controllerArtifactSchemaVersion || a.Kind != kindBaseline ||
		a.ConfigSHA256 != configSHA || !validSHA256(a.ArtifactSHA256) {
		return errors.New("baseline artifact is invalid")
	}
	consumer, err := config.ConsumerFor(a.Repository)
	if err != nil || !validBaseline(a.Baseline, consumer) {
		return errors.New("baseline artifact is invalid")
	}
	digest, err := digestBaselineArtifact(a)
	if err != nil || digest != a.ArtifactSHA256 {
		return errors.New("baseline artifact digest is invalid")
	}
	return nil
}

func newDeliveryArtifact[T any](kind string, binding deliveryBinding, payload T) (deliveryArtifact[T], error) {
	artifact := deliveryArtifact[T]{
		SchemaVersion: controllerArtifactSchemaVersion,
		Kind:          kind,
		Binding:       binding,
		Payload:       payload,
	}
	digest, err := digestDeliveryArtifact(artifact)
	if err != nil {
		return deliveryArtifact[T]{}, err
	}
	artifact.ArtifactSHA256 = digest
	return artifact, nil
}

func (a deliveryArtifact[T]) validateEnvelope(kind string, request worker.TicketRequest, config worker.Config) error {
	if a.SchemaVersion != controllerArtifactSchemaVersion || a.Kind != kind || !validSHA256(a.ArtifactSHA256) ||
		a.Binding.validate(request, config) != nil {
		return errors.New("delivery artifact is invalid")
	}
	digest, err := digestDeliveryArtifact(a)
	if err != nil || digest != a.ArtifactSHA256 {
		return errors.New("delivery artifact digest is invalid")
	}
	return nil
}

func newDeliveryBinding(
	request worker.TicketRequest,
	source worker.SourceSnapshot,
	candidate worker.Candidate,
	decision worker.StageDecision,
	validation worker.ValidationEvidence,
) deliveryBinding {
	paths := make([]string, len(candidate.Files))
	for index, file := range candidate.Files {
		paths[index] = file.Path
	}
	return deliveryBinding{
		DeliveryID: request.DeliveryID, InputSHA256: request.InputSHA256,
		ConfigSHA256: request.ConfigSHA256, ToolSHA: request.ToolSHA, IssueKey: request.IssueKey,
		Repository:   request.Repository,
		SourceSHA256: source.SourceSHA256, CandidateSHA256: candidate.CandidateSHA256,
		DecisionSHA256: decision.DecisionSHA256, ValidationSHA256: validation.ValidationSHA256,
		ProductPaths: paths,
	}
}

func (b deliveryBinding) validate(request worker.TicketRequest, config worker.Config) error {
	if request.Validate(config) != nil || b.DeliveryID != request.DeliveryID || b.InputSHA256 != request.InputSHA256 ||
		b.ConfigSHA256 != request.ConfigSHA256 || b.ToolSHA != request.ToolSHA || b.IssueKey != request.IssueKey ||
		b.Repository != request.Repository || !slices.Equal(b.ProductPaths, request.TargetFiles) {
		return errors.New("delivery binding identity is invalid")
	}
	return b.basicValidate()
}

func (b deliveryBinding) basicValidate() error {
	if _, err := loadedConfig.ConsumerFor(b.Repository); err != nil {
		return errors.New("delivery binding repository is invalid")
	}
	if b.DeliveryID == "" || !validSHA256(b.InputSHA256) || !validSHA256(b.ConfigSHA256) || !validObjectID(b.ToolSHA) ||
		b.IssueKey == "" || !validSHA256(b.SourceSHA256) || !validSHA256(b.CandidateSHA256) ||
		!validSHA256(b.DecisionSHA256) || !validSHA256(b.ValidationSHA256) ||
		len(b.ProductPaths) == 0 || !slices.IsSorted(b.ProductPaths) {
		return errors.New("delivery binding is invalid")
	}
	for index, path := range b.ProductPaths {
		if path == "" || strings.ContainsAny(path, "\x00\r\n\\") || strings.HasPrefix(path, "/") ||
			(index > 0 && path == b.ProductPaths[index-1]) {
			return errors.New("delivery product paths are invalid")
		}
	}
	return nil
}

func (b deliveryBinding) equal(other deliveryBinding) bool {
	return b.DeliveryID == other.DeliveryID && b.InputSHA256 == other.InputSHA256 &&
		b.ConfigSHA256 == other.ConfigSHA256 && b.ToolSHA == other.ToolSHA && b.IssueKey == other.IssueKey &&
		b.Repository == other.Repository &&
		b.SourceSHA256 == other.SourceSHA256 && b.CandidateSHA256 == other.CandidateSHA256 &&
		b.DecisionSHA256 == other.DecisionSHA256 && b.ValidationSHA256 == other.ValidationSHA256 &&
		slices.Equal(b.ProductPaths, other.ProductPaths)
}

func bindingContract(binding deliveryBinding) (githubapi.Contract, bool) {
	consumer, err := loadedConfig.ConsumerFor(binding.Repository)
	if err != nil {
		return githubapi.Contract{}, false
	}
	return consumer.Contract(), true
}

// validBaseline checks the observed branch heads. The two delivery branches
// must exist and be well-formed for every delivery; requiring them to carry
// the same tree — nothing half-promoted — only guards deliveries that will
// merge and promote, so a pull-request-only delivery does not demand it.
func validBaseline(baseline githubapi.Baseline, consumer worker.ConsumerConfig) bool {
	contract, known := consumer.Contract(), true
	_ = known
	if !known {
		return false
	}
	wellFormed := baseline.Integration.Branch == contract.IntegrationBranch && baseline.Release.Branch == contract.ReleaseBranch &&
		validObjectID(baseline.Integration.SHA) && validObjectID(baseline.Integration.TreeSHA) &&
		validObjectID(baseline.Release.SHA) && validObjectID(baseline.Release.TreeSHA) &&
		validObjectID(baseline.MergeBaseSHA) && validObjectID(baseline.MergeBaseTreeSHA)
	if !wellFormed {
		return false
	}
	if consumer.Delivery.ReachesIntegration() {
		return baseline.Integration.TreeSHA == baseline.Release.TreeSHA && baseline.MergeBaseTreeSHA == baseline.Integration.TreeSHA
	}
	return true
}

func validPublishedFeature(feature githubapi.PublishedFeature, binding deliveryBinding) bool {
	contract, known := bindingContract(binding)
	if !known {
		return false
	}
	return feature.Base.Branch == contract.IntegrationBranch && validObjectID(feature.Base.SHA) &&
		validObjectID(feature.Base.TreeSHA) && feature.Branch == featureBranch(binding) &&
		validObjectID(feature.HeadSHA) && validObjectID(feature.TreeSHA) && feature.TreeSHA != feature.Base.TreeSHA &&
		slices.Equal(feature.Paths, binding.ProductPaths)
}

func validFeaturePullRequest(pull githubapi.PullRequest, binding deliveryBinding) bool {
	contract, known := bindingContract(binding)
	if !known {
		return false
	}
	// The digest chain must open the body verbatim; what follows it — the
	// requester-facing trail, or notes a reviewing person adds later — is
	// deliberately unpinned, because the sealed chain is the part machines
	// verify and a human-edited description must not fail the delivery.
	return pull.Number > 0 && pull.Title == "[Codex] "+binding.IssueKey &&
		strings.HasPrefix(pull.Body, digestBody(binding, "")) &&
		pull.HeadRef == featureBranch(binding) && pull.BaseRef == contract.IntegrationBranch &&
		validObjectID(pull.HeadSHA) && validObjectID(pull.BaseSHA) &&
		strings.EqualFold(pull.HeadFullName, binding.Repository) &&
		!pull.CreatedAt.IsZero()
}

func validFeaturePRPayload(payload featurePRPayload, binding deliveryBinding) bool {
	return validPublishedFeature(payload.Feature, binding) && validFeaturePullRequest(payload.PullRequest, binding) &&
		payload.PullRequest.HeadSHA == payload.Feature.HeadSHA && payload.PullRequest.BaseSHA == payload.Feature.Base.SHA
}

func validFeatureChecksPayload(payload featureChecksPayload, binding deliveryBinding) bool {
	return validFeaturePRPayload(featurePRPayload{Feature: payload.Feature, PullRequest: payload.PullRequest}, binding) &&
		validFeatureChecks(payload.Checks, payload.PullRequest, binding)
}

func validFeatureMergePayload(payload featureMergePayload, binding deliveryBinding) bool {
	return validFeatureChecksPayload(featureChecksPayload{
		Feature: payload.Feature, PullRequest: payload.PullRequest, Checks: payload.Checks,
	}, binding) && validFeatureMerge(payload.Merge, binding) &&
		payload.Merge.PullRequestNumber == payload.PullRequest.Number && payload.Merge.BaseSHA == payload.Feature.Base.SHA &&
		payload.Merge.HeadSHA == payload.Feature.HeadSHA && payload.Merge.TreeSHA == payload.Feature.TreeSHA
}

func validFeatureChecks(checks githubapi.CheckEvidence, pull githubapi.PullRequest, binding deliveryBinding) bool {
	contract, known := bindingContract(binding)
	if !known {
		return false
	}
	requiredJobs := 0
	for _, workflow := range contract.FeatureWorkflows {
		requiredJobs += len(workflow.RequiredJobs)
	}
	return checks.PullRequestNumber == pull.Number && checks.HeadSHA == pull.HeadSHA &&
		len(checks.WorkflowRunIDs) == len(contract.FeatureWorkflows) && len(checks.WorkflowJobIDs) == requiredJobs &&
		positiveUnique(checks.WorkflowRunIDs) && positiveUnique(checks.WorkflowJobIDs) &&
		len(checks.CheckRunIDs) == 0 && len(checks.StatusIDs) == 0
}

func validFeatureMerge(merge githubapi.MergeResult, binding deliveryBinding) bool {
	contract, known := bindingContract(binding)
	if !known {
		return false
	}
	return merge.PullRequestNumber > 0 && merge.BaseBranch == contract.IntegrationBranch &&
		merge.HeadBranch == featureBranch(binding) && validMergeSHAs(merge)
}

func validPromotionMerge(merge githubapi.MergeResult, binding deliveryBinding) bool {
	contract, known := bindingContract(binding)
	if !known {
		return false
	}
	return merge.PullRequestNumber > 0 && merge.BaseBranch == contract.ReleaseBranch &&
		merge.HeadBranch == contract.IntegrationBranch && validMergeSHAs(merge)
}

func validMergeSHAs(merge githubapi.MergeResult) bool {
	return validObjectID(merge.BaseSHA) && validObjectID(merge.HeadSHA) &&
		validObjectID(merge.MergeSHA) && validObjectID(merge.TreeSHA)
}

func validStagingDeployment(deployment githubapi.DeploymentResult, binding deliveryBinding) bool {
	contract, known := bindingContract(binding)
	if !known {
		return false
	}
	policy := stagingDigestPolicyFor(binding.Repository)
	return validFeatureMerge(deployment.Merge, binding) && len(deployment.WorkflowRuns) == 1 &&
		validWorkflowRun(deployment.WorkflowRuns[0], contract.StagingWorkflow, contract.IntegrationBranch, deployment.Merge.MergeSHA) &&
		validObjectID(deployment.BranchHeadSHA) && deployment.BranchHeadSHA == deployment.DigestCommitSHA &&
		slices.Equal(deployment.DigestPaths, policy.ExactPaths)
}

func validProductionDeployment(deployment githubapi.DeploymentResult, binding deliveryBinding) bool {
	contract, known := bindingContract(binding)
	if !known {
		return false
	}
	if !validPromotionMerge(deployment.Merge, binding) || len(deployment.WorkflowRuns) != len(contract.ProductionWorkflows) ||
		deployment.BranchHeadSHA != deployment.Merge.MergeSHA || deployment.DigestCommitSHA != "" || len(deployment.DigestPaths) != 0 {
		return false
	}
	for index, expected := range contract.ProductionWorkflows {
		if !validWorkflowRun(deployment.WorkflowRuns[index], expected, contract.ReleaseBranch, deployment.Merge.MergeSHA) {
			return false
		}
	}
	return true
}

func validWorkflowRun(run githubapi.WorkflowRun, expected githubapi.WorkflowContract, branch, sha string) bool {
	return run.ID > 0 && run.WorkflowID == expected.ID && run.Name == expected.Name && workflowRunPathMatches(run.Path, expected.Path) &&
		run.HeadBranch == branch && run.HeadSHA == sha && run.Event == "push" && run.Status == "completed" &&
		run.Conclusion == "success" && run.Attempt > 0 && !run.CreatedAt.IsZero() && !run.UpdatedAt.IsZero() &&
		run.CreatedAt.Location() == time.UTC && run.UpdatedAt.Location() == time.UTC &&
		!run.UpdatedAt.Before(run.CreatedAt)
}

func workflowRunPathMatches(actual, expected string) bool {
	return actual == expected || strings.HasPrefix(actual, expected+"@")
}

func validPromotionPayload(payload promotionPayload, binding deliveryBinding) bool {
	proof := payload.Proof
	if !stagingProofMatchesBinding(payload.Release, binding) || !validPromotionBaseline(proof.Baseline, binding) ||
		!validStagingDeployment(proof.Staging, binding) || proof.Baseline != payload.Release.Baseline ||
		!equalDeploymentResult(proof.Staging, payload.Release.StagingDeployment) ||
		!slices.Equal(proof.ProductPaths, binding.ProductPaths) || !validSHA256(proof.AcceptanceEvidenceSHA256) {
		return false
	}
	pull := payload.PullRequest
	spec := promotionPullRequestSpec(binding, proof.AcceptanceEvidenceSHA256)
	return pull.Number > 0 && pull.Title == spec.Title && pull.Body == spec.Body &&
		pull.HeadRef == promotionContractIntegration(binding) && pull.HeadSHA == proof.Staging.BranchHeadSHA &&
		pull.BaseRef == promotionContractRelease(binding) && pull.BaseSHA == proof.Baseline.Release.SHA &&
		strings.EqualFold(pull.HeadFullName, binding.Repository) && !pull.CreatedAt.IsZero()
}

func validPromotionMergePayload(payload promotionMergePayload, binding deliveryBinding) bool {
	promotion := promotionPayload{Release: payload.Release, Proof: payload.Proof, PullRequest: payload.PullRequest}
	return validPromotionPayload(promotion, binding) && validPromotionMerge(payload.Merge, binding) &&
		payload.Merge.PullRequestNumber == payload.PullRequest.Number && payload.Merge.BaseSHA == payload.PullRequest.BaseSHA &&
		payload.Merge.HeadSHA == payload.PullRequest.HeadSHA
}

func validProductionReflectionPayload(payload productionReflectionPayload, binding deliveryBinding) bool {
	promotion := promotionPayload{Release: payload.Release, Proof: payload.Proof, PullRequest: payload.PullRequest}
	reflection := payload.Reflection
	return validPromotionPayload(promotion, binding) && reflection.PullRequestNumber == payload.PullRequest.Number &&
		reflection.BaseBranch == promotionContractRelease(binding) && reflection.BaseSHA == payload.PullRequest.BaseSHA &&
		reflection.HeadBranch == promotionContractIntegration(binding) && reflection.HeadSHA == payload.PullRequest.HeadSHA &&
		validObjectID(reflection.MergeSHA)
}

func reflectionMatchesMerge(reflection githubapi.MergeReflection, merge githubapi.MergeResult) bool {
	return reflection.PullRequestNumber == merge.PullRequestNumber && reflection.BaseBranch == merge.BaseBranch &&
		reflection.BaseSHA == merge.BaseSHA && reflection.HeadBranch == merge.HeadBranch &&
		reflection.HeadSHA == merge.HeadSHA && reflection.MergeSHA == merge.MergeSHA
}

func stagingProofMatchesBinding(proof releaseproof.StagingProof, binding deliveryBinding) bool {
	return proof.SchemaVersion == releaseproof.SchemaVersion && proof.DeliveryID == binding.DeliveryID &&
		proof.IssueKey == binding.IssueKey && proof.InputSHA256 == binding.InputSHA256 &&
		proof.ConfigSHA256 == binding.ConfigSHA256 && proof.ToolSHA == binding.ToolSHA &&
		proof.SourceSHA256 == binding.SourceSHA256 && proof.CandidateSHA256 == binding.CandidateSHA256 &&
		proof.DecisionSHA256 == binding.DecisionSHA256 && proof.ValidationSHA256 == binding.ValidationSHA256 &&
		slices.Equal(proof.ProductPaths, binding.ProductPaths) && validSHA256(proof.ProofSHA256)
}

func equalPublishedFeature(left, right githubapi.PublishedFeature) bool {
	return left.Base == right.Base && left.Branch == right.Branch && left.HeadSHA == right.HeadSHA &&
		left.TreeSHA == right.TreeSHA && slices.Equal(left.Paths, right.Paths)
}

func equalFeaturePRPayload(left, right featurePRPayload) bool {
	return equalPublishedFeature(left.Feature, right.Feature) && left.PullRequest == right.PullRequest
}

func equalDeploymentResult(left, right githubapi.DeploymentResult) bool {
	return left.Merge == right.Merge && slices.Equal(left.WorkflowRuns, right.WorkflowRuns) &&
		left.BranchHeadSHA == right.BranchHeadSHA && left.DigestCommitSHA == right.DigestCommitSHA &&
		slices.Equal(left.DigestPaths, right.DigestPaths)
}

func digestBaselineArtifact(artifact baselineArtifact) (string, error) {
	artifact.ArtifactSHA256 = ""
	return digestJSON(artifact)
}

func digestDeliveryArtifact[T any](artifact deliveryArtifact[T]) (string, error) {
	artifact.ArtifactSHA256 = ""
	return digestJSON(artifact)
}

func digestJSON(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func validSHA256(value string) bool {
	return len(value) == 64 && validLowerHex(value)
}

func validObjectID(value string) bool {
	return len(value) == 40 && validLowerHex(value)
}

func validLowerHex(value string) bool {
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func positiveUnique(values []int64) bool {
	seen := make(map[int64]struct{}, len(values))
	for _, value := range values {
		if value <= 0 {
			return false
		}
		if _, exists := seen[value]; exists {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

// promotionContractIntegration and promotionContractRelease resolve branch
// names for promotion checks; an unknown repository yields an impossible
// branch so every comparison fails closed.
func promotionContractIntegration(binding deliveryBinding) string {
	contract, known := bindingContract(binding)
	if !known {
		return "\x00"
	}
	return contract.IntegrationBranch
}

func promotionContractRelease(binding deliveryBinding) string {
	contract, known := bindingContract(binding)
	if !known {
		return "\x00"
	}
	return contract.ReleaseBranch
}

// validPromotionBaseline is the promotion-time baseline check: a promotion
// always merges, so the converged-tree requirement applies regardless of the
// configured delivery stop.
func validPromotionBaseline(baseline githubapi.Baseline, binding deliveryBinding) bool {
	contract, known := bindingContract(binding)
	if !known {
		return false
	}
	return baseline.Integration.Branch == contract.IntegrationBranch && baseline.Release.Branch == contract.ReleaseBranch &&
		validObjectID(baseline.Integration.SHA) && validObjectID(baseline.Integration.TreeSHA) &&
		validObjectID(baseline.Release.SHA) && validObjectID(baseline.Release.TreeSHA) &&
		validObjectID(baseline.MergeBaseSHA) && validObjectID(baseline.MergeBaseTreeSHA) &&
		baseline.Integration.TreeSHA == baseline.Release.TreeSHA && baseline.MergeBaseTreeSHA == baseline.Integration.TreeSHA
}
