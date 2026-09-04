package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"time"

	"automation.internal/ticket-ingress/internal/githubapi"
	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/releaseproof"
	"automation.internal/ticket-ingress/internal/visiblecheck"
	"automation.internal/ticket-ingress/internal/worker"
)

// extractPublishOption removes one optional name/value pair from a pairwise
// argument list. The publish command grew host-side-only options after the
// shared parser froze its required/repeated contract; stripping them first
// keeps every other verb's argument contract byte-identical.
func extractPublishOption(args []string, name string) ([]string, string) {
	for index := 0; index+1 < len(args); index += 2 {
		if args[index] == name {
			trimmed := append(append(make([]string, 0, len(args)-2), args[:index]...), args[index+2:]...)
			return trimmed, args[index+1]
		}
	}
	return args, ""
}

var publishSourceBasePattern = regexp.MustCompile(`^[a-f0-9]{40}$`)

// writePublishFailure leaves a machine-readable failure reason for the
// runner, which retries exactly one class of refusal (the integration base
// advancing mid-run) and treats everything else as final. Best-effort by
// design: the exit code stays the authoritative failure signal, and the
// file carries invariant names only — never repository content or tokens.
func writePublishFailure(path, code string, err error) {
	if path == "" {
		return
	}
	invariantCode := ""
	var invariantError *githubapi.InvariantError
	if errors.As(err, &invariantError) {
		invariantCode = invariantError.Code
	}
	payload, marshalErr := json.Marshal(struct {
		Code      string `json:"code"`
		Invariant string `json:"invariant,omitempty"`
	}{Code: code, Invariant: invariantCode})
	if marshalErr != nil {
		return
	}
	_ = os.Remove(path)
	_ = os.WriteFile(path, payload, 0o600)
}

func runBaseline(ctx context.Context, args []string, getenv func(string) string, transport http.RoundTripper) error {
	arguments, err := parseCommandArguments(args, []string{"--config", "--draft", "--out"})
	if err != nil {
		return err
	}
	config, err := loadCommandConfig(arguments.one("--config"), arguments.one("--out"))
	if err != nil {
		return err
	}
	// The baseline runs before target files are known, so it binds through
	// the draft: the destination is already sealed there, the files are not.
	var draft worker.TicketDraft
	if err := worker.ReadJSONFile(arguments.one("--draft"), worker.MaxTicketJSONBytes, &draft); err != nil {
		return fail("ticket_artifact_invalid")
	}
	configSHA, err := config.SHA256()
	if err != nil || draft.ConfigSHA256 != configSHA {
		return fail("ticket_artifact_invalid")
	}
	runtime, err := prepareRuntime(ctx, config, draft.Repository, getenv, transport)
	if err != nil {
		return err
	}
	baseline, err := runtime.controller.VerifyBaseline(ctx)
	if err != nil {
		// failFrom prints the invariant name; without it every baseline
		// refusal reads the same in the run log.
		return failFrom("baseline_verify_failed", err)
	}
	artifact, err := newBaselineArtifact(runtime.config, runtime.consumer, baseline)
	if err != nil || artifact.validate(runtime.config) != nil {
		return fail("baseline_artifact_invalid")
	}
	if err := writeControllerArtifact(arguments.one("--out"), artifact); err != nil {
		return fail("baseline_artifact_write_failed")
	}
	return nil
}

func runPublishFeature(ctx context.Context, args []string, getenv func(string) string, transport http.RoundTripper) error {
	args, sourceBase := extractPublishOption(args, "--source-base")
	args, failureOut := extractPublishOption(args, "--failure-out")
	if sourceBase != "" && !publishSourceBasePattern.MatchString(sourceBase) {
		return fail("arguments_invalid")
	}
	arguments, err := parseCommandArguments(args, []string{
		"--config", "--ticket", "--source", "--candidate", "--decision", "--validation", "--baseline", "--out",
	}, "--review")
	if err != nil {
		return err
	}
	config, err := loadCommandConfig(arguments.one("--config"), arguments.one("--out"))
	if err != nil {
		return err
	}
	request, err := readTicket(arguments.one("--ticket"), config)
	if err != nil {
		return fail("ticket_artifact_invalid")
	}
	runtime, err := prepareRuntime(ctx, config, request.Repository, getenv, transport)
	if err != nil {
		return err
	}
	var source worker.SourceSnapshot
	if err := worker.ReadJSONFile(arguments.one("--source"), worker.MaxArtifactJSONBytes, &source); err != nil {
		return fail("source_artifact_invalid")
	}
	var candidate worker.Candidate
	if err := worker.ReadJSONFile(arguments.one("--candidate"), worker.MaxArtifactJSONBytes, &candidate); err != nil {
		return fail("candidate_artifact_invalid")
	}
	var decision worker.StageDecision
	if err := worker.ReadJSONFile(arguments.one("--decision"), worker.MaxDecisionJSONBytes, &decision); err != nil {
		return fail("decision_artifact_invalid")
	}
	var validation worker.ValidationEvidence
	if err := worker.ReadJSONFile(arguments.one("--validation"), worker.MaxValidationJSONBytes, &validation); err != nil {
		return fail("validation_artifact_invalid")
	}
	reviews, err := readReviews(arguments["--review"])
	if err != nil {
		return fail("review_artifact_invalid")
	}
	baseline, err := readBaselineArtifact(arguments.one("--baseline"), runtime.config)
	if err != nil {
		return fail("baseline_artifact_invalid")
	}
	// The source snapshot chains to the base it was recorded on. Normally
	// that must be the baseline being published against; on a base-advance
	// retry the runner supplies the ORIGINAL base via --source-base, has
	// re-validated the same candidate on the freshly advanced baseline, and
	// PublishFeature's per-file blob checks below hold every touched path to
	// the recorded blob against the advanced tree — a touched path that also
	// moved upstream refuses as source_blob_changed.
	expectedSourceBase := baseline.Baseline.Integration.SHA
	if sourceBase != "" {
		expectedSourceBase = sourceBase
	}
	if worker.ValidatePublishGate(decision, validation, candidate, reviews, source, request, runtime.config) != nil ||
		source.BaseSHA != expectedSourceBase {
		writePublishFailure(failureOut, "publish_gate_rejected", nil)
		return fail("publish_gate_rejected")
	}
	binding := newDeliveryBinding(request, source, candidate, decision, validation)
	if binding.validate(request, runtime.config) != nil {
		return fail("delivery_binding_invalid")
	}
	consumer, err := request.Consumer(runtime.config)
	if err != nil {
		return fail("delivery_binding_invalid")
	}
	files := make([]githubapi.FileUpdate, len(candidate.Files))
	for index, file := range candidate.Files {
		if file.Path != source.Files[index].Path {
			return fail("candidate_file_set_invalid")
		}
		files[index] = githubapi.FileUpdate{
			Path: file.Path, Content: []byte(file.Content), ExpectedBlobSHA: source.Files[index].GitBlobSHA,
			Created: source.Files[index].Created,
		}
	}
	spec := githubapi.FeatureSpec{
		Branch: featureBranch(binding), CommitMessage: featureCommitMessage(binding),
		AllowedPathPrefixes: slices.Clone(consumer.Mode.AllowedFilePrefixes), Files: files,
	}
	feature, err := runtime.controller.PublishFeature(ctx, baseline.Baseline, spec)
	if err != nil {
		writePublishFailure(failureOut, "feature_publish_failed", err)
		return failFrom("feature_publish_failed", err)
	}
	if !validPublishedFeature(feature, binding) {
		return fail("feature_publish_result_invalid")
	}
	artifact, err := newDeliveryArtifact(kindFeature, binding, feature)
	if err != nil {
		return fail("feature_artifact_invalid")
	}
	if err := writeControllerArtifact(arguments.one("--out"), artifact); err != nil {
		return fail("feature_artifact_write_failed")
	}
	return nil
}

func runCreateFeaturePR(ctx context.Context, args []string, getenv func(string) string, transport http.RoundTripper) error {
	arguments, err := parseCommandArguments(args, []string{"--config", "--ticket", "--feature", "--trail", "--out"})
	if err != nil {
		return err
	}
	trail, err := readTrailFile(arguments.one("--trail"))
	if err != nil {
		return fail("trail_invalid")
	}
	config, err := loadCommandConfig(arguments.one("--config"), arguments.one("--out"))
	if err != nil {
		return err
	}
	request, err := readTicket(arguments.one("--ticket"), config)
	if err != nil {
		return fail("ticket_artifact_invalid")
	}
	runtime, err := prepareRuntime(ctx, config, request.Repository, getenv, transport)
	if err != nil {
		return err
	}
	feature, err := readDeliveryArtifact[githubapi.PublishedFeature](arguments.one("--feature"), kindFeature, request, runtime.config)
	if err != nil || !validPublishedFeature(feature.Payload, feature.Binding) {
		return fail("feature_artifact_invalid")
	}
	pull, err := runtime.controller.CreateFeaturePullRequest(ctx, feature.Payload, featurePullRequestSpec(feature.Binding, trail))
	if err != nil {
		return failFrom("feature_pr_create_failed", err)
	}
	if !validFeaturePullRequest(pull, feature.Binding) || pull.HeadSHA != feature.Payload.HeadSHA || pull.BaseSHA != feature.Payload.Base.SHA {
		return fail("feature_pr_result_invalid")
	}
	payload := featurePRPayload{Feature: feature.Payload, PullRequest: pull}
	if !validFeaturePRPayload(payload, feature.Binding) {
		return fail("feature_pr_result_invalid")
	}
	artifact, err := newDeliveryArtifact(kindFeaturePR, feature.Binding, payload)
	if err != nil {
		return fail("feature_pr_artifact_invalid")
	}
	if err := writeControllerArtifact(arguments.one("--out"), artifact); err != nil {
		return fail("feature_pr_artifact_write_failed")
	}
	return nil
}

func runWaitFeature(ctx context.Context, args []string, getenv func(string) string, transport http.RoundTripper) error {
	arguments, err := parseCommandArguments(args, []string{"--config", "--ticket", "--feature-pr", "--out"})
	if err != nil {
		return err
	}
	config, err := loadCommandConfig(arguments.one("--config"), arguments.one("--out"))
	if err != nil {
		return err
	}
	request, err := readTicket(arguments.one("--ticket"), config)
	if err != nil {
		return fail("ticket_artifact_invalid")
	}
	runtime, err := prepareRuntime(ctx, config, request.Repository, getenv, transport)
	if err != nil {
		return err
	}
	pull, err := readDeliveryArtifact[featurePRPayload](arguments.one("--feature-pr"), kindFeaturePR, request, runtime.config)
	if err != nil || !validFeaturePRPayload(pull.Payload, pull.Binding) {
		return fail("feature_pr_artifact_invalid")
	}
	checks, err := runtime.controller.WaitForPullRequestChecks(ctx, pull.Payload.PullRequest, featureChecks(), waitOptions())
	if err != nil {
		return failFrom("feature_checks_failed", err)
	}
	if !validFeatureChecks(checks, pull.Payload.PullRequest, pull.Binding) {
		return fail("feature_checks_result_invalid")
	}
	payload := featureChecksPayload{Feature: pull.Payload.Feature, PullRequest: pull.Payload.PullRequest, Checks: checks}
	if !validFeatureChecksPayload(payload, pull.Binding) {
		return fail("feature_checks_result_invalid")
	}
	artifact, err := newDeliveryArtifact(kindFeatureChecks, pull.Binding, payload)
	if err != nil {
		return fail("feature_checks_artifact_invalid")
	}
	if err := writeControllerArtifact(arguments.one("--out"), artifact); err != nil {
		return fail("feature_checks_artifact_write_failed")
	}
	return nil
}

// runAwaitMergedStaging is the debug role's trigger: wait for a HUMAN to
// merge the delivered pull request, then for the staging deployment workflow
// of that merge commit to succeed (the digest commit included). It performs
// no mutation and writes a plain progress record — not a sealed proof: the
// pull_request delivery never builds the release-proof chain, and the E2E
// observation this gates is a courtesy report, not a promotion input.
func runAwaitMergedStaging(ctx context.Context, args []string, getenv func(string) string, transport http.RoundTripper) error {
	arguments, err := parseCommandArguments(args, []string{"--config", "--ticket", "--feature-pr", "--out"})
	if err != nil {
		return err
	}
	config, err := loadCommandConfig(arguments.one("--config"), arguments.one("--out"))
	if err != nil {
		return err
	}
	request, err := readTicket(arguments.one("--ticket"), config)
	if err != nil {
		return fail("ticket_artifact_invalid")
	}
	runtime, err := prepareRuntime(ctx, config, request.Repository, getenv, transport)
	if err != nil {
		return err
	}
	pull, err := readDeliveryArtifact[featurePRPayload](arguments.one("--feature-pr"), kindFeaturePR, request, runtime.config)
	if err != nil || !validFeaturePRPayload(pull.Payload, pull.Binding) {
		return fail("feature_pr_artifact_invalid")
	}
	merge, err := runtime.controller.AwaitFeatureMerge(ctx, pull.Payload.PullRequest, mergedStagingWait())
	if err != nil {
		return failFrom("feature_merge_wait_failed", err)
	}
	deployment, err := runtime.controller.AwaitStaging(ctx, merge, waitOptions(), runtime.consumer.StagingDigestCommitPolicy())
	if err != nil {
		return failFrom("staging_wait_failed", err)
	}
	record := struct {
		SchemaVersion int       `json:"schema_version"`
		DeliveryID    string    `json:"delivery_id"`
		MergedSHA     string    `json:"merged_sha"`
		StagingRunID  int64     `json:"staging_run_id"`
		BranchHeadSHA string    `json:"branch_head_sha"`
		ObservedAt    time.Time `json:"observed_at"`
	}{
		SchemaVersion: 1, DeliveryID: request.DeliveryID, MergedSHA: merge.MergeSHA,
		BranchHeadSHA: deployment.BranchHeadSHA, ObservedAt: time.Now().UTC(),
	}
	if len(deployment.WorkflowRuns) == 1 {
		record.StagingRunID = deployment.WorkflowRuns[0].ID
	}
	if err := worker.WriteJSONFileExclusive(arguments.one("--out"), record, controllerArtifactMaxBytes); err != nil {
		return fail("merged_staging_artifact_write_failed")
	}
	return nil
}

// mergedStagingWait bounds the human-merge wait. A merge decision is a
// human's, not a deployment's: hours are normal, so the poll is slow and
// the budget wide — the debug card's own wall is the final bound.
func mergedStagingWait() githubapi.WaitOptions {
	return githubapi.WaitOptions{PollInterval: time.Minute, Timeout: 72 * time.Hour}
}

// read-merged reads one pull request's merged state, nothing else. The
// delivery uses it AFTER a merge verb failed, to report honestly whether
// the merge itself landed — the merge verbs can fail after the merge did.
func runReadMerged(ctx context.Context, args []string, getenv func(string) string, transport http.RoundTripper) error {
	arguments, err := parseCommandArguments(args, []string{"--config", "--ticket", "--number", "--out"})
	if err != nil {
		return err
	}
	config, err := loadCommandConfig(arguments.one("--config"), arguments.one("--out"))
	if err != nil {
		return err
	}
	request, err := readTicket(arguments.one("--ticket"), config)
	if err != nil {
		return fail("ticket_artifact_invalid")
	}
	number, err := strconv.ParseInt(arguments.one("--number"), 10, 64)
	if err != nil || number <= 0 {
		return fail("arguments_invalid")
	}
	runtime, err := prepareRuntime(ctx, config, request.Repository, getenv, transport)
	if err != nil {
		return err
	}
	merged, err := runtime.controller.ReadPullMerged(ctx, number)
	if err != nil {
		return failFrom("read_merged_failed", err)
	}
	if err := worker.WriteJSONFileExclusive(arguments.one("--out"), merged, controllerArtifactMaxBytes); err != nil {
		return fail("read_merged_write_failed")
	}
	return nil
}

// promotion-delta reads what a stg→prod promotion would carry RIGHT NOW.
// The requester sees this list in the staging report before writing Go —
// the rail moves the whole branch, and Go approves the whole list.
func runPromotionDelta(ctx context.Context, args []string, getenv func(string) string, transport http.RoundTripper) error {
	arguments, err := parseCommandArguments(args, []string{"--config", "--ticket", "--out"})
	if err != nil {
		return err
	}
	config, err := loadCommandConfig(arguments.one("--config"), arguments.one("--out"))
	if err != nil {
		return err
	}
	request, err := readTicket(arguments.one("--ticket"), config)
	if err != nil {
		return fail("ticket_artifact_invalid")
	}
	runtime, err := prepareRuntime(ctx, config, request.Repository, getenv, transport)
	if err != nil {
		return err
	}
	delta, err := runtime.controller.ReadPromotionDelta(ctx)
	if err != nil {
		return failFrom("promotion_delta_failed", err)
	}
	if err := worker.WriteJSONFileExclusive(arguments.one("--out"), delta, controllerArtifactMaxBytes); err != nil {
		return fail("promotion_delta_write_failed")
	}
	return nil
}

func runMergeFeature(ctx context.Context, args []string, getenv func(string) string, transport http.RoundTripper) error {
	arguments, err := parseCommandArguments(args, []string{"--config", "--ticket", "--feature-pr", "--checks", "--out"})
	if err != nil {
		return err
	}
	config, err := loadCommandConfig(arguments.one("--config"), arguments.one("--out"))
	if err != nil {
		return err
	}
	request, err := readTicket(arguments.one("--ticket"), config)
	if err != nil {
		return fail("ticket_artifact_invalid")
	}
	runtime, err := prepareRuntime(ctx, config, request.Repository, getenv, transport)
	if err != nil {
		return err
	}
	pull, err := readDeliveryArtifact[featurePRPayload](arguments.one("--feature-pr"), kindFeaturePR, request, runtime.config)
	if err != nil || !validFeaturePRPayload(pull.Payload, pull.Binding) {
		return fail("feature_pr_artifact_invalid")
	}
	checks, err := readDeliveryArtifact[featureChecksPayload](arguments.one("--checks"), kindFeatureChecks, request, runtime.config)
	if err != nil || !checks.Binding.equal(pull.Binding) || !validFeatureChecksPayload(checks.Payload, checks.Binding) ||
		!equalFeaturePRPayload(featurePRPayload{Feature: checks.Payload.Feature, PullRequest: checks.Payload.PullRequest}, pull.Payload) {
		return fail("feature_checks_artifact_invalid")
	}
	merge, err := runtime.controller.MergeFeaturePullRequest(
		ctx, pull.Payload.PullRequest, checks.Payload.Checks, featureMergeSpec(pull.Binding), waitOptions(),
	)
	if err != nil {
		return failFrom("feature_merge_failed", err)
	}
	if !validFeatureMerge(merge, pull.Binding) || merge.PullRequestNumber != pull.Payload.PullRequest.Number ||
		merge.BaseSHA != pull.Payload.PullRequest.BaseSHA || merge.HeadSHA != pull.Payload.PullRequest.HeadSHA ||
		merge.TreeSHA != pull.Payload.Feature.TreeSHA {
		return fail("feature_merge_result_invalid")
	}
	payload := featureMergePayload{
		Feature: pull.Payload.Feature, PullRequest: pull.Payload.PullRequest, Checks: checks.Payload.Checks, Merge: merge,
	}
	if !validFeatureMergePayload(payload, pull.Binding) {
		return fail("feature_merge_result_invalid")
	}
	artifact, err := newDeliveryArtifact(kindFeatureMerge, pull.Binding, payload)
	if err != nil {
		return fail("feature_merge_artifact_invalid")
	}
	if err := writeControllerArtifact(arguments.one("--out"), artifact); err != nil {
		return fail("feature_merge_artifact_write_failed")
	}
	return nil
}

func runAwaitStaging(ctx context.Context, args []string, getenv func(string) string, transport http.RoundTripper) error {
	arguments, err := parseCommandArguments(args, []string{
		"--config", "--ticket", "--source", "--candidate", "--decision", "--validation", "--baseline", "--feature-merge", "--out",
	}, "--review")
	if err != nil {
		return err
	}
	config, err := loadCommandConfig(arguments.one("--config"), arguments.one("--out"))
	if err != nil {
		return err
	}
	request, err := readTicket(arguments.one("--ticket"), config)
	if err != nil {
		return fail("ticket_artifact_invalid")
	}
	runtime, err := prepareRuntime(ctx, config, request.Repository, getenv, transport)
	if err != nil {
		return err
	}
	gate, err := readGateArtifacts(arguments, request, runtime.config)
	if err != nil {
		return err
	}
	baseline, err := readBaselineArtifact(arguments.one("--baseline"), runtime.config)
	if err != nil {
		return fail("baseline_artifact_invalid")
	}
	merge, err := readDeliveryArtifact[featureMergePayload](arguments.one("--feature-merge"), kindFeatureMerge, request, runtime.config)
	// The published base must be the recorded baseline. The SOURCE base may
	// legitimately be older — the integration branch advanced mid-run and
	// the publish gate re-validated on the new base — and source identity
	// itself is pinned by the binding's artifact digests below.
	if err != nil || !validFeatureMergePayload(merge.Payload, merge.Binding) ||
		gate.decision.DecisionSHA256 != merge.Binding.DecisionSHA256 ||
		!merge.Binding.matchesArtifacts(gate.source, gate.candidate, gate.validation) ||
		merge.Payload.Feature.Base.SHA != baseline.Baseline.Integration.SHA {
		return fail("feature_merge_artifact_invalid")
	}
	deployment, err := runtime.controller.AwaitStaging(ctx, merge.Payload.Merge, waitOptions(), stagingDigestPolicyFor(merge.Binding.Repository))
	if err != nil {
		return failFrom("staging_deployment_failed", err)
	}
	if !validStagingDeployment(deployment, merge.Binding) {
		return fail("staging_deployment_result_invalid")
	}
	proof, err := releaseproof.NewStagingProof(stagingInputsFromChain(
		request, runtime.config, gate, baseline.Baseline, merge.Payload, deployment,
	))
	if err != nil {
		return fail("release_proof_invalid")
	}
	artifact, err := newDeliveryArtifact(kindStaging, merge.Binding, proof)
	if err != nil {
		return fail("staging_artifact_invalid")
	}
	if err := writeControllerArtifact(arguments.one("--out"), artifact); err != nil {
		return fail("staging_artifact_write_failed")
	}
	return nil
}

func runCreatePromotionPR(ctx context.Context, args []string, getenv func(string) string, transport http.RoundTripper) error {
	arguments, err := parseCommandArguments(args, []string{
		"--config", "--ticket", "--source", "--candidate", "--decision", "--validation", "--baseline", "--staging", "--visible", "--screenshot", "--out",
	}, "--review")
	if err != nil {
		return err
	}
	config, err := loadCommandConfig(arguments.one("--config"), arguments.one("--out"))
	if err != nil {
		return err
	}
	request, err := readTicket(arguments.one("--ticket"), config)
	if err != nil {
		return fail("ticket_artifact_invalid")
	}
	runtime, err := prepareRuntime(ctx, config, request.Repository, getenv, transport)
	if err != nil {
		return err
	}
	gate, err := readGateArtifacts(arguments, request, runtime.config)
	if err != nil {
		return err
	}
	baseline, err := readBaselineArtifact(arguments.one("--baseline"), runtime.config)
	if err != nil {
		return fail("baseline_artifact_invalid")
	}
	staging, err := readDeliveryArtifact[releaseproof.StagingProof](arguments.one("--staging"), kindStaging, request, runtime.config)
	if err != nil || !stagingProofMatchesBinding(staging.Payload, staging.Binding) {
		return fail("staging_artifact_invalid")
	}
	stagingInputs := stagingInputsFromProof(request, runtime.config, gate, staging.Payload)
	if staging.Payload.Baseline != baseline.Baseline || staging.Payload.Validate(stagingInputs) != nil {
		return fail("promotion_binding_invalid")
	}
	var visible visiblecheck.Evidence
	if err := worker.ReadJSONFile(arguments.one("--visible"), worker.MaxArtifactJSONBytes, &visible); err != nil {
		return fail("visible_evidence_invalid")
	}
	screenshot, err := readBoundedRegularArtifact(arguments.one("--screenshot"), int64(visiblecheck.MaxScreenshotBytes))
	if err != nil {
		return fail("visible_screenshot_invalid")
	}
	if visible.Environment != "staging" || visible.ValidateStaging(staging.Payload, stagingInputs) != nil {
		return fail("visible_evidence_rejected")
	}
	if visible.ValidateScreenshot(screenshot) != nil {
		return fail("visible_screenshot_rejected")
	}
	proof := githubapi.PromotionProof{
		Baseline: baseline.Baseline, Staging: staging.Payload.StagingDeployment,
		ProductPaths: slices.Clone(staging.Binding.ProductPaths), AcceptanceEvidenceSHA256: visible.EvidenceSHA256,
	}
	pull, err := runtime.controller.CreatePromotionPullRequest(
		ctx, proof, stagingDigestPolicyFor(staging.Binding.Repository), promotionPullRequestSpec(staging.Binding, visible.EvidenceSHA256),
	)
	if err != nil {
		return failFrom("promotion_pr_create_failed", err)
	}
	if !promotionFollowsVisibleEvidence(pull, visible) {
		return fail("promotion_pr_result_invalid")
	}
	payload := promotionPayload{Release: staging.Payload, Proof: proof, PullRequest: pull}
	if !validPromotionPayload(payload, staging.Binding) {
		return fail("promotion_pr_result_invalid")
	}
	artifact, err := newDeliveryArtifact(kindPromotion, staging.Binding, payload)
	if err != nil {
		return fail("promotion_artifact_invalid")
	}
	if err := writeControllerArtifact(arguments.one("--out"), artifact); err != nil {
		return fail("promotion_artifact_write_failed")
	}
	return nil
}

func promotionFollowsVisibleEvidence(pull githubapi.PullRequest, visible visiblecheck.Evidence) bool {
	return !pull.CreatedAt.IsZero() && pull.CreatedAt.Location() == time.UTC &&
		!visible.ObservedAt.IsZero() && visible.ObservedAt.Location() == time.UTC &&
		!pull.CreatedAt.Before(visible.ObservedAt.Truncate(time.Second))
}

func runMergePromotion(ctx context.Context, args []string, getenv func(string) string, transport http.RoundTripper) error {
	arguments, err := parseCommandArguments(args, []string{
		"--config", "--ticket", "--source", "--candidate", "--decision", "--validation", "--baseline", "--promotion", "--reflection-out", "--out",
	}, "--review")
	if err != nil {
		return err
	}
	reflectionOutput := arguments.one("--reflection-out")
	if validateOutputDestination(reflectionOutput) != nil ||
		!distinctOutputDestinations(arguments.one("--out"), reflectionOutput) {
		return fail("production_reflection_path_invalid")
	}
	config, err := loadCommandConfig(arguments.one("--config"), arguments.one("--out"))
	if err != nil {
		return err
	}
	request, err := readTicket(arguments.one("--ticket"), config)
	if err != nil {
		return fail("ticket_artifact_invalid")
	}
	runtime, err := prepareRuntime(ctx, config, request.Repository, getenv, transport)
	if err != nil {
		return err
	}
	gate, err := readGateArtifacts(arguments, request, runtime.config)
	if err != nil {
		return err
	}
	baseline, err := readBaselineArtifact(arguments.one("--baseline"), runtime.config)
	if err != nil {
		return fail("baseline_artifact_invalid")
	}
	promotion, err := readDeliveryArtifact[promotionPayload](arguments.one("--promotion"), kindPromotion, request, runtime.config)
	if err != nil || !validPromotionPayload(promotion.Payload, promotion.Binding) ||
		promotion.Payload.Release.Baseline != baseline.Baseline ||
		promotion.Payload.Release.Validate(stagingInputsFromProof(request, runtime.config, gate, promotion.Payload.Release)) != nil {
		return fail("promotion_artifact_invalid")
	}
	var recordedReflection githubapi.MergeReflection
	var reflectionFailure error
	recordReflection := func(reflection githubapi.MergeReflection) error {
		reflectionFailure = writeProductionReflectionArtifact(reflectionOutput, promotion, reflection)
		if reflectionFailure != nil {
			return reflectionFailure
		}
		recordedReflection = reflection
		return nil
	}
	merge, err := runtime.controller.MergePromotionPullRequest(
		ctx, promotion.Payload.PullRequest, githubapi.CheckEvidence{}, promotion.Payload.Proof, stagingDigestPolicyFor(promotion.Binding.Repository),
		promotionMergeSpec(promotion.Binding, promotion.Payload.Proof.AcceptanceEvidenceSHA256), waitOptions(), recordReflection,
	)
	if reflectionFailure != nil {
		return reflectionFailure
	}
	if err != nil {
		return failFrom("promotion_merge_failed", err)
	}
	if !validPromotionMerge(merge, promotion.Binding) || merge.PullRequestNumber != promotion.Payload.PullRequest.Number ||
		merge.BaseSHA != promotion.Payload.PullRequest.BaseSHA || merge.HeadSHA != promotion.Payload.PullRequest.HeadSHA ||
		!reflectionMatchesMerge(recordedReflection, merge) {
		return fail("promotion_merge_result_invalid")
	}
	payload := promotionMergePayload{
		Release: promotion.Payload.Release, Proof: promotion.Payload.Proof,
		PullRequest: promotion.Payload.PullRequest, Merge: merge,
	}
	if !validPromotionMergePayload(payload, promotion.Binding) {
		return fail("promotion_merge_result_invalid")
	}
	artifact, err := newDeliveryArtifact(kindPromotionMerge, promotion.Binding, payload)
	if err != nil {
		return fail("promotion_merge_artifact_invalid")
	}
	if err := writeControllerArtifact(arguments.one("--out"), artifact); err != nil {
		return fail("promotion_merge_artifact_write_failed")
	}
	return nil
}

func writeProductionReflectionArtifact(
	filename string,
	promotion deliveryArtifact[promotionPayload],
	reflection githubapi.MergeReflection,
) error {
	payload := productionReflectionPayload{
		Release: promotion.Payload.Release, Proof: promotion.Payload.Proof,
		PullRequest: promotion.Payload.PullRequest, Reflection: reflection,
	}
	if !validProductionReflectionPayload(payload, promotion.Binding) {
		return fail("production_reflection_result_invalid")
	}
	artifact, err := newDeliveryArtifact(kindProductionReflection, promotion.Binding, payload)
	if err != nil {
		return fail("production_reflection_artifact_invalid")
	}
	if err := writeControllerArtifact(filename, artifact); err != nil {
		return fail("production_reflection_artifact_write_failed")
	}
	return nil
}

func runAwaitProduction(ctx context.Context, args []string, getenv func(string) string, transport http.RoundTripper) error {
	arguments, err := parseCommandArguments(args, []string{
		"--config", "--ticket", "--source", "--candidate", "--decision", "--validation", "--baseline", "--promotion-merge", "--out",
	}, "--review")
	if err != nil {
		return err
	}
	config, err := loadCommandConfig(arguments.one("--config"), arguments.one("--out"))
	if err != nil {
		return err
	}
	request, err := readTicket(arguments.one("--ticket"), config)
	if err != nil {
		return fail("ticket_artifact_invalid")
	}
	runtime, err := prepareRuntime(ctx, config, request.Repository, getenv, transport)
	if err != nil {
		return err
	}
	gate, err := readGateArtifacts(arguments, request, runtime.config)
	if err != nil {
		return err
	}
	baseline, err := readBaselineArtifact(arguments.one("--baseline"), runtime.config)
	if err != nil {
		return fail("baseline_artifact_invalid")
	}
	merge, err := readDeliveryArtifact[promotionMergePayload](arguments.one("--promotion-merge"), kindPromotionMerge, request, runtime.config)
	if err != nil || !validPromotionMergePayload(merge.Payload, merge.Binding) ||
		merge.Payload.Release.Baseline != baseline.Baseline ||
		merge.Payload.Release.Validate(stagingInputsFromProof(request, runtime.config, gate, merge.Payload.Release)) != nil {
		return fail("promotion_merge_artifact_invalid")
	}
	deployment, err := runtime.controller.AwaitProduction(ctx, merge.Payload.Merge, waitOptions(), productionDigestPolicy())
	if err != nil {
		return failFrom("production_deployment_failed", err)
	}
	if !validProductionDeployment(deployment, merge.Binding) {
		return fail("production_deployment_result_invalid")
	}
	production, err := releaseproof.NewProductionProof(
		merge.Payload.Release, runtime.consumer, merge.Payload.Proof.AcceptanceEvidenceSHA256,
		merge.Payload.PullRequest, merge.Payload.Merge, deployment,
	)
	if err != nil || production.Validate(merge.Payload.Release, runtime.consumer) != nil {
		return fail("production_proof_invalid")
	}
	payload := productionPayload{Staging: merge.Payload.Release, Production: production}
	artifact, err := newDeliveryArtifact(kindProduction, merge.Binding, payload)
	if err != nil {
		return fail("production_artifact_invalid")
	}
	if err := writeControllerArtifact(arguments.one("--out"), artifact); err != nil {
		return fail("production_artifact_write_failed")
	}
	return nil
}

type gateArtifacts struct {
	source     worker.SourceSnapshot
	candidate  worker.Candidate
	reviews    []worker.Review
	decision   worker.StageDecision
	validation worker.ValidationEvidence
}

func readGateArtifacts(arguments commandArguments, request worker.TicketRequest, config worker.Config) (gateArtifacts, error) {
	var artifacts gateArtifacts
	if err := worker.ReadJSONFile(arguments.one("--source"), worker.MaxArtifactJSONBytes, &artifacts.source); err != nil {
		return gateArtifacts{}, fail("source_artifact_invalid")
	}
	if err := worker.ReadJSONFile(arguments.one("--candidate"), worker.MaxArtifactJSONBytes, &artifacts.candidate); err != nil {
		return gateArtifacts{}, fail("candidate_artifact_invalid")
	}
	if err := worker.ReadJSONFile(arguments.one("--decision"), worker.MaxDecisionJSONBytes, &artifacts.decision); err != nil {
		return gateArtifacts{}, fail("decision_artifact_invalid")
	}
	if err := worker.ReadJSONFile(arguments.one("--validation"), worker.MaxValidationJSONBytes, &artifacts.validation); err != nil {
		return gateArtifacts{}, fail("validation_artifact_invalid")
	}
	reviews, err := readReviews(arguments["--review"])
	if err != nil {
		return gateArtifacts{}, fail("review_artifact_invalid")
	}
	artifacts.reviews = reviews
	if worker.ValidatePublishGate(
		artifacts.decision, artifacts.validation, artifacts.candidate, artifacts.reviews,
		artifacts.source, request, config,
	) != nil {
		return gateArtifacts{}, fail("publish_gate_rejected")
	}
	return artifacts, nil
}

func stagingInputsFromChain(
	request worker.TicketRequest,
	config worker.Config,
	gate gateArtifacts,
	baseline githubapi.Baseline,
	chain featureMergePayload,
	staging githubapi.DeploymentResult,
) releaseproof.StagingInputs {
	return releaseproof.StagingInputs{
		Request: request, Config: config, Source: gate.source, Candidate: gate.candidate,
		Reviews: gate.reviews, Decision: gate.decision, Validation: gate.validation,
		Baseline: baseline, PublishedFeature: chain.Feature, FeaturePullRequest: chain.PullRequest,
		FeatureChecks: chain.Checks, FeatureMerge: chain.Merge, StagingDeployment: staging,
	}
}

func stagingInputsFromProof(
	request worker.TicketRequest,
	config worker.Config,
	gate gateArtifacts,
	proof releaseproof.StagingProof,
) releaseproof.StagingInputs {
	return releaseproof.StagingInputs{
		Request: request, Config: config, Source: gate.source, Candidate: gate.candidate,
		Reviews: gate.reviews, Decision: gate.decision, Validation: gate.validation,
		Baseline: proof.Baseline, PublishedFeature: proof.PublishedFeature,
		FeaturePullRequest: proof.FeaturePullRequest, FeatureChecks: proof.FeatureChecks,
		FeatureMerge: proof.FeatureMerge, StagingDeployment: proof.StagingDeployment,
	}
}

func readTicket(filename string, config worker.Config) (worker.TicketRequest, error) {
	var request worker.TicketRequest
	if err := worker.ReadJSONFile(filename, worker.MaxTicketJSONBytes, &request); err != nil || request.Validate(config) != nil {
		return worker.TicketRequest{}, fail("ticket_artifact_invalid")
	}
	return request, nil
}

func readReviews(filenames []string) ([]worker.Review, error) {
	reviews := make([]worker.Review, len(filenames))
	for index, filename := range filenames {
		if err := worker.ReadJSONFile(filename, worker.MaxReviewJSONBytes, &reviews[index]); err != nil {
			return nil, fail("review_artifact_invalid")
		}
	}
	return reviews, nil
}

func readBaselineArtifact(filename string, config worker.Config) (baselineArtifact, error) {
	var artifact baselineArtifact
	if err := worker.ReadJSONFile(filename, controllerArtifactMaxBytes, &artifact); err != nil || artifact.validate(config) != nil {
		return baselineArtifact{}, fail("baseline_artifact_invalid")
	}
	return artifact, nil
}

func readDeliveryArtifact[T any](filename, kind string, request worker.TicketRequest, config worker.Config) (deliveryArtifact[T], error) {
	var artifact deliveryArtifact[T]
	if err := worker.ReadJSONFile(filename, controllerArtifactMaxBytes, &artifact); err != nil || artifact.validateEnvelope(kind, request, config) != nil {
		return deliveryArtifact[T]{}, fail("delivery_artifact_invalid")
	}
	return artifact, nil
}

func writeControllerArtifact(filename string, value any) error {
	return worker.WriteJSONFileExclusive(filename, value, controllerArtifactMaxBytes)
}

func (binding deliveryBinding) matchesArtifacts(
	source worker.SourceSnapshot,
	candidate worker.Candidate,
	validation worker.ValidationEvidence,
) bool {
	paths := make([]string, len(candidate.Files))
	for index, file := range candidate.Files {
		paths[index] = file.Path
	}
	return binding.SourceSHA256 == source.SourceSHA256 && binding.CandidateSHA256 == candidate.CandidateSHA256 &&
		binding.ValidationSHA256 == validation.ValidationSHA256 && slices.Equal(binding.ProductPaths, paths)
}

// readTrailFile loads the requester-facing run record composed by the worker.
// It is held to the same plain-text bounds as the terminal report's copy.
func readTrailFile(filename string) (string, error) {
	encoded, err := os.ReadFile(filename)
	if err != nil || len(encoded) == 0 || len(encoded) > hook.MaxTerminalTrailBytes {
		return "", errors.New("trail file is invalid")
	}
	if hook.ValidateTrailText(string(encoded)) != nil {
		return "", errors.New("trail file is invalid")
	}
	return string(encoded), nil
}
