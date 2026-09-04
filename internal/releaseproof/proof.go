// Package releaseproof seals the fixed first-consumer release chain. It does
// not discover repositories or workflows and it does not perform live GitHub
// reads; the controller must revalidate every retained GitHub identifier before
// mutating remote state.
package releaseproof

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"automation.internal/ticket-ingress/internal/githubapi"
	"automation.internal/ticket-ingress/internal/worker"
)

const (
	SchemaVersion       = 1
	featureBranchSuffix = 12
)

var (
	sha256Pattern       = regexp.MustCompile(`^[a-f0-9]{64}$`)
	objectIDPattern     = regexp.MustCompile(`^[a-f0-9]{40}$`)
	deliveryPattern     = regexp.MustCompile(`^delivery_[a-f0-9]{32}$`)
	issueKeyPattern     = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,99}-[1-9][0-9]*$`)
	relativePathPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,511}$`)
)

// StagingInputs are the complete inputs needed to prove the worker gate and
// the feature-to-staging GitHub chain. Reviews are deliberately inputs rather
// than proof fields: the decision digest binds their canonical digests and the
// worker gate independently validates the full set.
type StagingInputs struct {
	Request            worker.TicketRequest
	Config             worker.Config
	Source             worker.SourceSnapshot
	Candidate          worker.Candidate
	Reviews            []worker.Review
	Decision           worker.StageDecision
	Validation         worker.ValidationEvidence
	Baseline           githubapi.Baseline
	PublishedFeature   githubapi.PublishedFeature
	FeaturePullRequest githubapi.PullRequest
	FeatureChecks      githubapi.CheckEvidence
	FeatureMerge       githubapi.MergeResult
	StagingDeployment  githubapi.DeploymentResult
}

// StagingProof binds the deterministic worker publication gate to the exact
// target-repository objects that reached staging.
type StagingProof struct {
	SchemaVersion      int                        `json:"schema_version"`
	DeliveryID         string                     `json:"delivery_id"`
	IssueKey           string                     `json:"issue_key"`
	Repository         string                     `json:"repository"`
	InputSHA256        string                     `json:"input_sha256"`
	ConfigSHA256       string                     `json:"config_sha256"`
	ToolSHA            string                     `json:"tool_sha"`
	SourceSHA256       string                     `json:"source_sha256"`
	CandidateSHA256    string                     `json:"candidate_sha256"`
	DecisionSHA256     string                     `json:"decision_sha256"`
	ValidationSHA256   string                     `json:"validation_sha256"`
	SourceBaseSHA      string                     `json:"source_base_sha"`
	Baseline           githubapi.Baseline         `json:"baseline"`
	PublishedFeature   githubapi.PublishedFeature `json:"published_feature"`
	FeaturePullRequest githubapi.PullRequest      `json:"feature_pull_request"`
	FeatureChecks      githubapi.CheckEvidence    `json:"feature_checks"`
	FeatureMerge       githubapi.MergeResult      `json:"feature_merge"`
	StagingDeployment  githubapi.DeploymentResult `json:"staging_deployment"`
	ProductPaths       []string                   `json:"product_paths"`
	ProofSHA256        string                     `json:"proof_sha256"`
}

// ProductionProof extends one already sealed staging proof through the exact
// promotion and production deployment. The visible evidence is intentionally
// represented only by its canonical digest to avoid a package cycle.
type ProductionProof struct {
	SchemaVersion                int                        `json:"schema_version"`
	StagingProofSHA256           string                     `json:"staging_proof_sha256"`
	StagingVisibleEvidenceSHA256 string                     `json:"staging_visible_evidence_sha256"`
	PromotionPullRequest         githubapi.PullRequest      `json:"promotion_pull_request"`
	PromotionMerge               githubapi.MergeResult      `json:"promotion_merge"`
	ProductionDeployment         githubapi.DeploymentResult `json:"production_deployment"`
	ProofSHA256                  string                     `json:"proof_sha256"`
}

// NewStagingProof validates the full worker publication gate before sealing
// the target-fixed GitHub evidence.
func NewStagingProof(input StagingInputs) (StagingProof, error) {
	if err := validateStagingInput(input); err != nil {
		return StagingProof{}, err
	}
	proof := StagingProof{
		SchemaVersion: SchemaVersion,
		DeliveryID:    input.Request.DeliveryID, IssueKey: input.Request.IssueKey,
		Repository:  input.Request.Repository,
		InputSHA256: input.Request.InputSHA256, ConfigSHA256: input.Request.ConfigSHA256,
		ToolSHA: input.Request.ToolSHA, SourceSHA256: input.Source.SourceSHA256,
		CandidateSHA256: input.Candidate.CandidateSHA256, DecisionSHA256: input.Decision.DecisionSHA256,
		ValidationSHA256: input.Validation.ValidationSHA256, SourceBaseSHA: input.Source.BaseSHA,
		Baseline: input.Baseline, PublishedFeature: clonePublishedFeature(input.PublishedFeature),
		FeaturePullRequest: input.FeaturePullRequest, FeatureChecks: cloneCheckEvidence(input.FeatureChecks),
		FeatureMerge: input.FeatureMerge, StagingDeployment: cloneDeployment(input.StagingDeployment),
		ProductPaths: candidatePaths(input.Candidate),
	}
	var err error
	proof.ProofSHA256, err = stagingProofDigest(proof)
	if err != nil {
		return StagingProof{}, errors.New("staging proof could not be sealed")
	}
	if err := proof.Validate(input); err != nil {
		return StagingProof{}, err
	}
	return proof, nil
}

// Validate replays both the worker publication gate and every local
// target-repository relationship captured by a staging proof.
func (proof StagingProof) Validate(input StagingInputs) error {
	if err := validateStagingInput(input); err != nil {
		return err
	}
	if proof.Repository != input.Request.Repository ||
		proof.DeliveryID != input.Request.DeliveryID || proof.IssueKey != input.Request.IssueKey ||
		proof.InputSHA256 != input.Request.InputSHA256 || proof.ConfigSHA256 != input.Request.ConfigSHA256 ||
		proof.ToolSHA != input.Request.ToolSHA || proof.SourceSHA256 != input.Source.SourceSHA256 ||
		proof.CandidateSHA256 != input.Candidate.CandidateSHA256 || proof.DecisionSHA256 != input.Decision.DecisionSHA256 ||
		proof.ValidationSHA256 != input.Validation.ValidationSHA256 || proof.SourceBaseSHA != input.Source.BaseSHA ||
		proof.Baseline != input.Baseline || !equalPublishedFeature(proof.PublishedFeature, input.PublishedFeature) ||
		proof.FeaturePullRequest != input.FeaturePullRequest || !equalCheckEvidence(proof.FeatureChecks, input.FeatureChecks) ||
		proof.FeatureMerge != input.FeatureMerge || !equalDeployment(proof.StagingDeployment, input.StagingDeployment) ||
		!slices.Equal(proof.ProductPaths, candidatePaths(input.Candidate)) {
		return errors.New("staging proof input binding is invalid")
	}
	consumer, err := input.Config.ConsumerFor(input.Request.Repository)
	if err != nil {
		return errors.New("staging proof repository is not a configured consumer")
	}
	return proof.validateStatic(consumer)
}

// NewProductionProof seals the promotion of one canonical staging proof. The
// caller remains responsible for calling StagingProof.Validate with the worker
// artifacts when loading an existing proof.
func NewProductionProof(
	staging StagingProof,
	consumer worker.ConsumerConfig,
	stagingVisibleEvidenceSHA256 string,
	pull githubapi.PullRequest,
	merge githubapi.MergeResult,
	deployment githubapi.DeploymentResult,
) (ProductionProof, error) {
	if err := staging.validateStatic(consumer); err != nil {
		return ProductionProof{}, errors.New("staging proof is invalid")
	}
	proof := ProductionProof{
		SchemaVersion: SchemaVersion, StagingProofSHA256: staging.ProofSHA256,
		StagingVisibleEvidenceSHA256: stagingVisibleEvidenceSHA256,
		PromotionPullRequest:         pull, PromotionMerge: merge,
		ProductionDeployment: cloneDeployment(deployment),
	}
	var err error
	proof.ProofSHA256, err = productionProofDigest(proof)
	if err != nil {
		return ProductionProof{}, errors.New("production proof could not be sealed")
	}
	if err := proof.Validate(staging, consumer); err != nil {
		return ProductionProof{}, err
	}
	return proof, nil
}

// Validate binds production evidence to the exact canonical staging proof.
func (proof ProductionProof) Validate(staging StagingProof, consumer worker.ConsumerConfig) error {
	if err := staging.validateStatic(consumer); err != nil {
		return errors.New("staging proof is invalid")
	}
	if proof.SchemaVersion != SchemaVersion || proof.StagingProofSHA256 != staging.ProofSHA256 ||
		!validSHA256(proof.StagingProofSHA256) || !validSHA256(proof.StagingVisibleEvidenceSHA256) ||
		!validSHA256(proof.ProofSHA256) {
		return errors.New("production proof identity is invalid")
	}
	if err := validatePromotionPullRequest(proof.PromotionPullRequest, staging, consumer, proof.StagingVisibleEvidenceSHA256); err != nil {
		return err
	}
	if err := validatePromotionMerge(proof.PromotionMerge, proof.PromotionPullRequest, consumer); err != nil {
		return err
	}
	if !equalMerge(proof.ProductionDeployment.Merge, proof.PromotionMerge) {
		return errors.New("production deployment merge binding is invalid")
	}
	if proof.ProductionDeployment.DigestPaths != nil {
		return errors.New("production proof digest paths are not canonical")
	}
	if !canonicalWorkflowOrder(proof.ProductionDeployment.WorkflowRuns, consumer.Contract().ProductionWorkflows) {
		return errors.New("production workflow order is not canonical")
	}
	if _, err := ValidateProductionDeployment(proof.ProductionDeployment, consumer); err != nil {
		return err
	}
	for _, run := range proof.ProductionDeployment.WorkflowRuns {
		if run.CreatedAt.Before(proof.PromotionPullRequest.CreatedAt) {
			return errors.New("production workflow predates promotion pull request")
		}
	}
	digest, err := productionProofDigest(proof)
	if err != nil || digest != proof.ProofSHA256 {
		return errors.New("production proof digest is invalid")
	}
	return nil
}

func validateStagingInput(input StagingInputs) error {
	if err := validateTarget(input.Request, input.Config); err != nil {
		return err
	}
	if err := worker.ValidatePublishGate(
		input.Decision, input.Validation, input.Candidate, input.Reviews,
		input.Source, input.Request, input.Config,
	); err != nil {
		return errors.New("worker publish gate is invalid")
	}
	// The source base may be older than the baseline: the publish gate
	// re-validates on an advanced integration base (per-file blob checks)
	// and publishes there. validatePublishedFeature pins the PUBLISHED base
	// to the baseline; the source base stays recorded for audit only. The
	// blob checks cover the touched files; UNTOUCHED files are covered by
	// the deterministic re-validation on the advanced base, so an advanced
	// proof must carry that run's checkout — recorded, not assumed.
	if input.Source.BaseSHA != input.Baseline.Integration.SHA &&
		input.Validation.CheckedOutSHA != input.Baseline.Integration.SHA {
		return errors.New("advanced-base validation binding is invalid")
	}
	paths := candidatePaths(input.Candidate)
	if !slices.Equal(paths, input.Request.TargetFiles) || !sourcePathsEqual(input.Source, paths) ||
		!validationPathsEqual(input.Validation, paths) || !slices.Equal(input.PublishedFeature.Paths, paths) {
		return errors.New("product path chain is invalid")
	}
	return nil
}

func (proof StagingProof) validateStatic(consumer worker.ConsumerConfig) error {
	if proof.Repository != consumer.Repository {
		return errors.New("staging proof repository does not match the consumer")
	}
	if proof.SchemaVersion != SchemaVersion || !deliveryPattern.MatchString(proof.DeliveryID) ||
		!issueKeyPattern.MatchString(proof.IssueKey) || !validSHA256(proof.InputSHA256) ||
		!validSHA256(proof.ConfigSHA256) || !worker.ValidToolSHA(proof.ToolSHA) ||
		!validSHA256(proof.SourceSHA256) || !validSHA256(proof.CandidateSHA256) ||
		!validSHA256(proof.DecisionSHA256) || !validSHA256(proof.ValidationSHA256) ||
		!validObjectID(proof.SourceBaseSHA) || !validSHA256(proof.ProofSHA256) ||
		!validProductPaths(proof.ProductPaths) {
		return errors.New("staging proof identity is invalid")
	}
	// SourceBaseSHA is the base the implementation READ; the baseline is the
	// base the publication USED. They differ whenever the integration branch
	// advanced mid-run and the publish gate re-validated on the new base
	// (its per-file blob checks are what prove the source still applies), so
	// equality must not be required here — validatePublishedFeature already
	// pins the published base to this baseline exactly.
	if err := validateBaseline(proof.Baseline, consumer); err != nil {
		return errors.New("staging baseline is invalid")
	}
	if err := validatePublishedFeature(proof.PublishedFeature, proof); err != nil {
		return err
	}
	if err := validateFeaturePullRequest(proof.FeaturePullRequest, proof, consumer); err != nil {
		return err
	}
	if err := validateFeatureChecks(proof.FeatureChecks, proof.FeaturePullRequest, consumer); err != nil {
		return err
	}
	if err := validateFeatureMerge(proof.FeatureMerge, proof.FeaturePullRequest, proof.PublishedFeature, consumer); err != nil {
		return err
	}
	if !equalMerge(proof.StagingDeployment.Merge, proof.FeatureMerge) {
		return errors.New("staging deployment merge binding is invalid")
	}
	if !slices.Equal(proof.StagingDeployment.DigestPaths, consumer.StagingDigestCommitPolicy().ExactPaths) {
		return errors.New("staging digest path order is not canonical")
	}
	if !canonicalWorkflowOrder(proof.StagingDeployment.WorkflowRuns, []githubapi.WorkflowContract{consumer.Contract().StagingWorkflow}) {
		return errors.New("staging workflow order is not canonical")
	}
	if _, err := ValidateStagingDeployment(proof.StagingDeployment, consumer); err != nil {
		return err
	}
	for _, run := range proof.StagingDeployment.WorkflowRuns {
		if run.CreatedAt.Before(proof.FeaturePullRequest.CreatedAt) {
			return errors.New("staging workflow predates feature pull request")
		}
	}
	digest, err := stagingProofDigest(proof)
	if err != nil || digest != proof.ProofSHA256 {
		return errors.New("staging proof digest is invalid")
	}
	return nil
}

// validateTarget confirms the ticket names a configured destination. The
// configuration is the single source of the destination contract; a fixed
// in-code copy used to be cross-checked here, which kept a customer's name
// inside the engine.
func validateTarget(request worker.TicketRequest, config worker.Config) error {
	if err := config.Validate(); err != nil {
		return errors.New("worker configuration is invalid")
	}
	if _, err := request.Consumer(config); err != nil {
		return errors.New("release target contract is invalid")
	}
	return nil
}

func validateBaseline(baseline githubapi.Baseline, consumer worker.ConsumerConfig) error {
	// The integration branch legitimately runs ahead of the release branch:
	// deliveries land on staging continuously and promote only on the
	// requester's Go, which approves the WHOLE branch with the delta spelled
	// out in the staging report. Requiring identical trees here encoded the
	// old promote-every-delivery rhythm and would refuse every delivery
	// made while anything is awaiting its Go.
	if baseline.Integration.Branch != consumer.IntegrationBranch || baseline.Release.Branch != consumer.ReleaseBranch ||
		!validObjectID(baseline.Integration.SHA) || !validObjectID(baseline.Integration.TreeSHA) ||
		!validObjectID(baseline.Release.SHA) || !validObjectID(baseline.Release.TreeSHA) ||
		!validObjectID(baseline.MergeBaseSHA) || !validObjectID(baseline.MergeBaseTreeSHA) {
		return errors.New("baseline is invalid")
	}
	return nil
}

func validatePublishedFeature(feature githubapi.PublishedFeature, proof StagingProof) error {
	expectedBranch := featureBranch(proof.DeliveryID, proof.IssueKey)
	if feature.Base != proof.Baseline.Integration || feature.Branch != expectedBranch ||
		!validObjectID(feature.HeadSHA) || !validObjectID(feature.TreeSHA) ||
		feature.HeadSHA == feature.Base.SHA || feature.TreeSHA == feature.Base.TreeSHA ||
		!slices.Equal(feature.Paths, proof.ProductPaths) {
		return errors.New("published feature is invalid")
	}
	return nil
}

func validateFeaturePullRequest(pull githubapi.PullRequest, proof StagingProof, consumer worker.ConsumerConfig) error {
	if err := validatePullIdentity(pull, consumer); err != nil || pull.Title != "[Codex] "+proof.IssueKey ||
		!strings.HasPrefix(pull.Body, digestBody(proof, "")) || pull.HeadRef != proof.PublishedFeature.Branch ||
		pull.HeadSHA != proof.PublishedFeature.HeadSHA || pull.BaseRef != consumer.IntegrationBranch ||
		pull.BaseSHA != proof.PublishedFeature.Base.SHA {
		return errors.New("feature pull request is invalid")
	}
	return nil
}

func validateFeatureChecks(checks githubapi.CheckEvidence, pull githubapi.PullRequest, consumer worker.ConsumerConfig) error {
	contract := consumer.Contract()
	expectedJobs := 0
	for _, workflow := range contract.FeatureWorkflows {
		expectedJobs += len(workflow.RequiredJobs)
	}
	if checks.PullRequestNumber != pull.Number || checks.HeadSHA != pull.HeadSHA ||
		len(checks.WorkflowRunIDs) != len(contract.FeatureWorkflows) || len(checks.WorkflowJobIDs) != expectedJobs ||
		!positiveUnique(checks.WorkflowRunIDs) || !positiveUnique(checks.WorkflowJobIDs) ||
		checks.CheckRunIDs != nil || checks.StatusIDs != nil {
		return errors.New("feature check evidence is invalid")
	}
	return nil
}

func validateFeatureMerge(merge githubapi.MergeResult, pull githubapi.PullRequest, feature githubapi.PublishedFeature, consumer worker.ConsumerConfig) error {
	if !validMerge(merge) || merge.PullRequestNumber != pull.Number || merge.BaseBranch != consumer.IntegrationBranch ||
		merge.BaseSHA != pull.BaseSHA || merge.HeadBranch != pull.HeadRef || merge.HeadSHA != pull.HeadSHA ||
		merge.TreeSHA != feature.TreeSHA {
		return errors.New("feature merge is invalid")
	}
	return nil
}

func validatePromotionPullRequest(pull githubapi.PullRequest, staging StagingProof, consumer worker.ConsumerConfig, evidenceSHA string) error {
	stagingCompletedAt, err := ValidateStagingDeployment(staging.StagingDeployment, consumer)
	if err != nil {
		return errors.New("staging deployment is invalid")
	}
	if err := validatePullIdentity(pull, consumer); err != nil || pull.Title != "[Codex] "+staging.IssueKey+" promote" ||
		pull.Body != digestBody(staging, evidenceSHA) || pull.HeadRef != consumer.IntegrationBranch ||
		pull.HeadSHA != staging.StagingDeployment.BranchHeadSHA || pull.BaseRef != consumer.ReleaseBranch ||
		pull.BaseSHA != staging.Baseline.Release.SHA || pull.CreatedAt.Before(stagingCompletedAt) {
		return errors.New("promotion pull request is invalid")
	}
	return nil
}

func validatePromotionMerge(merge githubapi.MergeResult, pull githubapi.PullRequest, consumer worker.ConsumerConfig) error {
	if !validMerge(merge) || merge.PullRequestNumber != pull.Number || merge.BaseBranch != consumer.ReleaseBranch ||
		merge.BaseSHA != pull.BaseSHA || merge.HeadBranch != consumer.IntegrationBranch || merge.HeadSHA != pull.HeadSHA {
		return errors.New("promotion merge is invalid")
	}
	return nil
}

func validatePullIdentity(pull githubapi.PullRequest, consumer worker.ConsumerConfig) error {
	expectedURL := fmt.Sprintf("https://github.com/%s/pull/%d", consumer.Repository, pull.Number)
	if pull.Number <= 0 || pull.HTMLURL != expectedURL ||
		!strings.EqualFold(pull.HeadFullName, consumer.Repository) ||
		pull.CreatedAt.IsZero() || pull.CreatedAt.Location() != time.UTC ||
		!validObjectID(pull.HeadSHA) || !validObjectID(pull.BaseSHA) {
		return errors.New("pull request identity is invalid")
	}
	return nil
}

func validMerge(merge githubapi.MergeResult) bool {
	return merge.PullRequestNumber > 0 && validObjectID(merge.BaseSHA) && validObjectID(merge.HeadSHA) &&
		validObjectID(merge.MergeSHA) && validObjectID(merge.TreeSHA) && merge.MergeSHA != merge.BaseSHA &&
		merge.MergeSHA != merge.HeadSHA
}

func featureBranch(deliveryID, issueKey string) string {
	if !deliveryPattern.MatchString(deliveryID) || !issueKeyPattern.MatchString(issueKey) {
		return ""
	}
	delivery := strings.TrimPrefix(deliveryID, "delivery_")
	return "automation/" + strings.ToLower(issueKey) + "-" + delivery[len(delivery)-featureBranchSuffix:]
}

func digestBody(proof StagingProof, evidenceSHA string) string {
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

func sourcePathsEqual(source worker.SourceSnapshot, paths []string) bool {
	if len(source.Files) != len(paths) {
		return false
	}
	for index, path := range paths {
		if source.Files[index].Path != path || !validObjectID(source.Files[index].GitBlobSHA) {
			return false
		}
	}
	return true
}

func validationPathsEqual(validation worker.ValidationEvidence, paths []string) bool {
	if len(validation.Files) != len(paths) {
		return false
	}
	for index, path := range paths {
		if validation.Files[index].Path != path {
			return false
		}
	}
	return true
}

func candidatePaths(candidate worker.Candidate) []string {
	paths := make([]string, len(candidate.Files))
	for index, file := range candidate.Files {
		paths[index] = file.Path
	}
	return paths
}

func validProductPaths(paths []string) bool {
	if len(paths) == 0 || !slices.IsSorted(paths) {
		return false
	}
	for index, productPath := range paths {
		if !relativePathPattern.MatchString(productPath) || strings.HasPrefix(productPath, "/") ||
			path.Clean(productPath) != productPath ||
			(index > 0 && productPath == paths[index-1]) {
			return false
		}
	}
	return true
}

func workflowFilename(path string) string {
	return strings.TrimPrefix(path, ".github/workflows/")
}

func stagingProofDigest(proof StagingProof) (string, error) {
	proof.ProofSHA256 = ""
	return digestJSON(proof)
}

func productionProofDigest(proof ProductionProof) (string, error) {
	proof.ProofSHA256 = ""
	return digestJSON(proof)
}

func digestJSON(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func validSHA256(value string) bool { return sha256Pattern.MatchString(value) }

func validObjectID(value string) bool { return objectIDPattern.MatchString(value) }

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

func clonePublishedFeature(feature githubapi.PublishedFeature) githubapi.PublishedFeature {
	feature.Paths = slices.Clone(feature.Paths)
	return feature
}

func cloneCheckEvidence(evidence githubapi.CheckEvidence) githubapi.CheckEvidence {
	evidence.WorkflowRunIDs = slices.Clone(evidence.WorkflowRunIDs)
	evidence.WorkflowJobIDs = slices.Clone(evidence.WorkflowJobIDs)
	// These two sets must be absent for the fixed workflow-only feature gate.
	evidence.CheckRunIDs = nil
	evidence.StatusIDs = nil
	return evidence
}

func cloneDeployment(deployment githubapi.DeploymentResult) githubapi.DeploymentResult {
	deployment.WorkflowRuns = slices.Clone(deployment.WorkflowRuns)
	deployment.DigestPaths = slices.Clone(deployment.DigestPaths)
	if len(deployment.DigestPaths) == 0 {
		deployment.DigestPaths = nil
	}
	return deployment
}

func equalPublishedFeature(left, right githubapi.PublishedFeature) bool {
	return left.Base == right.Base && left.Branch == right.Branch && left.HeadSHA == right.HeadSHA &&
		left.TreeSHA == right.TreeSHA && slices.Equal(left.Paths, right.Paths)
}

func equalCheckEvidence(left, right githubapi.CheckEvidence) bool {
	return left.PullRequestNumber == right.PullRequestNumber && left.HeadSHA == right.HeadSHA &&
		slices.Equal(left.WorkflowRunIDs, right.WorkflowRunIDs) && slices.Equal(left.WorkflowJobIDs, right.WorkflowJobIDs) &&
		slices.Equal(left.CheckRunIDs, right.CheckRunIDs) && slices.Equal(left.StatusIDs, right.StatusIDs)
}

func equalMerge(left, right githubapi.MergeResult) bool { return left == right }

func equalDeployment(left, right githubapi.DeploymentResult) bool {
	return equalMerge(left.Merge, right.Merge) && slices.Equal(left.WorkflowRuns, right.WorkflowRuns) &&
		left.BranchHeadSHA == right.BranchHeadSHA && left.DigestCommitSHA == right.DigestCommitSHA &&
		slices.Equal(left.DigestPaths, right.DigestPaths)
}
