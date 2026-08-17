package main

import (
	"errors"
	"path/filepath"
	"strings"
	"time"

	"automation.internal/ticket-ingress/internal/githubapi"
	"automation.internal/ticket-ingress/internal/worker"
)

const deliveryBranchSuffix = 12

// loadedConfig is the configuration the single-shot command loaded. Binding
// validators resolve destination contracts through it; a controller process
// runs exactly one command, so the variable is written once before any
// validator can read it.
var loadedConfig worker.Config

func loadFixedConfig(filename string) (worker.Config, error) {
	if filename == "" || filepath.Clean(filename) != filename || filepath.Base(filename) != filepath.Base(controllerConfigPath) {
		return worker.Config{}, errors.New("fixed config path is invalid")
	}
	config, err := worker.LoadConfig(filename)
	if err != nil {
		return worker.Config{}, errors.New("fixed config is invalid")
	}
	// Integrity is per run, not per binary: every command validates its
	// sealed ticket against this configuration's canonical digest, so a
	// swapped configuration fails the run it was swapped into. The engine
	// carries no customer digest — or any customer value — of its own.
	loadedConfig = config
	return config, nil
}

// featureChecks is intentionally empty: the controller verifies the exact
// workflow IDs and job sets from the destination contract instead of
// trusting mutable check names.
func featureChecks() githubapi.CheckRequirements {
	return githubapi.CheckRequirements{}
}

func waitOptions() githubapi.WaitOptions {
	return githubapi.WaitOptions{PollInterval: 15 * time.Second, Timeout: 50 * time.Minute}
}

// productionDigestPolicy is empty for every destination: production does not
// create a second digest commit; both production workflows must complete for
// the exact promotion merge SHA.
func productionDigestPolicy() githubapi.DigestCommitPolicy {
	return githubapi.DigestCommitPolicy{}
}

// stagingDigestPolicyFor resolves the configured staging digest-commit policy
// of one destination. A destination without one fails those checks closed.
func stagingDigestPolicyFor(repository string) githubapi.DigestCommitPolicy {
	consumer, err := loadedConfig.ConsumerFor(repository)
	if err != nil {
		return githubapi.DigestCommitPolicy{}
	}
	return consumer.StagingDigestCommitPolicy()
}

func featureBranch(binding deliveryBinding) string {
	delivery := strings.TrimPrefix(binding.DeliveryID, "delivery_")
	if len(delivery) != 32 {
		return ""
	}
	return "automation/" + strings.ToLower(binding.IssueKey) + "-" + delivery[len(delivery)-deliveryBranchSuffix:]
}

func featureCommitMessage(binding deliveryBinding) string {
	return "Codex: deliver " + binding.IssueKey
}

// featurePullRequestSpec carries the digest chain for machine verification
// and the trail for the person deciding the merge — the digests prove what
// was built, the trail says what happened and why.
func featurePullRequestSpec(binding deliveryBinding, trail string) githubapi.PullRequestSpec {
	return githubapi.PullRequestSpec{
		Title: "[Codex] " + binding.IssueKey,
		Body:  digestBody(binding, "") + "\n\n" + trail,
	}
}

func promotionPullRequestSpec(binding deliveryBinding, evidenceSHA256 string) githubapi.PullRequestSpec {
	return githubapi.PullRequestSpec{
		Title: "[Codex] " + binding.IssueKey + " promote",
		Body:  digestBody(binding, evidenceSHA256),
	}
}

func featureMergeSpec(binding deliveryBinding) githubapi.MergeSpec {
	return githubapi.MergeSpec{
		CommitTitle:   "Codex: merge " + binding.IssueKey,
		CommitMessage: digestBody(binding, ""),
	}
}

func promotionMergeSpec(binding deliveryBinding, evidenceSHA256 string) githubapi.MergeSpec {
	return githubapi.MergeSpec{
		CommitTitle:   "Codex: promote " + binding.IssueKey,
		CommitMessage: digestBody(binding, evidenceSHA256),
	}
}

func digestBody(binding deliveryBinding, evidenceSHA256 string) string {
	lines := []string{
		"Issue: " + binding.IssueKey,
		"Input-SHA256: " + binding.InputSHA256,
		"Config-SHA256: " + binding.ConfigSHA256,
		"Tool-SHA: " + binding.ToolSHA,
		"Source-SHA256: " + binding.SourceSHA256,
		"Candidate-SHA256: " + binding.CandidateSHA256,
		"Decision-SHA256: " + binding.DecisionSHA256,
		"Validation-SHA256: " + binding.ValidationSHA256,
	}
	if evidenceSHA256 != "" {
		lines = append(lines, "Evidence-SHA256: "+evidenceSHA256)
	}
	return strings.Join(lines, "\n")
}

func controllerGitHubConfig(token string, consumer worker.ConsumerConfig) githubapi.Config {
	owner, name, _ := strings.Cut(consumer.Repository, "/")
	return githubapi.Config{
		Owner: owner, Repository: name,
		RepositoryID: consumer.RepositoryID, Token: token,
		Timeout: 30 * time.Second, MaxResponseBytes: 8 * 1024 * 1024,
	}
}
