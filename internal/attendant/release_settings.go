package attendant

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/worker"
)

// The one setting the engine now knows the answer to.
//
// A destination that asks for production needs a policy saying which commit
// records what landed on staging: the promotion reads it to prove that what
// it is about to push to production is the build a screen was judged on.
// Until now the engine could not write that policy and said so, by name, in
// its report — the shape of the commit was somebody else's workflow's, and
// the engine had no way to observe a message prefix or an author.
//
// Where the engine wrote the workflow itself, that stops being true. The
// commit is made by the file it just authored, so its shape is not
// something to observe but something the engine chose. It writes it down.
//
// Timing is the whole safety of this. A destination's configuration moving
// under a delivery restarts that delivery (chains.go), which would make a
// write during the rounds an engine that restarted itself for ever. Past
// the pull request the cards re-verify their records under the digest the
// pull request recorded, so a configuration that moves afterwards cannot
// make them unreadable — and that is the only window this write happens in.

// releaseSettingsRecord is what was written, kept beside the run so the
// report can say it and a second tick can see it was already done.
type releaseSettingsRecord struct {
	Repository string   `json:"repository"`
	Workflows  []string `json:"workflows"`
	Written    []string `json:"written"`
}

// releaseSettingsFile is that record's name inside a delivery's directory.
const releaseSettingsFile = "release-settings.json"

// digestCommitMessagePrefix is the first words of the commit an
// engine-authored deploy workflow makes to record what it deployed. The
// instruction asks for exactly this line, and the policy written here is
// what the promotion matches it against, so the two are one constant.
const digestCommitMessagePrefix = "Deployed build"

// digestCommitActor is who that commit is authored by: the platform's own
// automation identity, which is what a workflow commits as when it uses the
// token the job was given rather than a person's credential.
const digestCommitActor = "github-actions[bot]"

// writeReleaseSettings records, in the destination's own configuration, the
// policy the engine now owns because it wrote the workflow that produces
// it.
//
// Best-effort and idempotent. A configuration that cannot be written — a
// read-only mount is the ordinary reason — leaves the delivery exactly as
// it was: the promotion then checks the path and stops at staging with a
// reason, which is a worse outcome than writing but a truthful one. Nothing
// here can fail a delivery.
func writeReleaseSettings(config runtime.Config, runDir string, logger Logger) {
	if _, recorded := readReleaseSettings(runDir); recorded {
		return
	}
	plan, ok := readReleasePathPlan(runDir)
	if !ok || len(plan.WorkflowFiles) == 0 {
		// Nothing was authored under the means, so nothing here is the
		// engine's to decide. A destination whose workflow somebody else
		// wrote keeps its own policy, and the report keeps naming it.
		return
	}
	delivered := deliveredProductPaths(runDir)
	built := make([]string, 0, len(plan.WorkflowFiles))
	for _, file := range plan.WorkflowFiles {
		if slices.Contains(delivered, file) {
			built = append(built, file)
		}
	}
	if len(built) == 0 {
		// The plan said it would build them and the pull request does not
		// carry them. The round did something else, and a policy written
		// for a file that does not exist would be a policy the promotion
		// checks against nothing.
		return
	}
	live, err := worker.LoadConfig(config.ConsumerConfigPath)
	if err != nil {
		logger.Error("the destination configuration could not be read; the release policy was not written",
			"repository", plan.Repository, "error", err.Error())
		return
	}
	updated, written := withDigestCommitPolicy(live, plan.Repository, built)
	if len(written) == 0 {
		// Already says what this would say. Recorded all the same, so the
		// report tells the requester which settings this delivery owns.
		sealReleaseSettings(runDir, releaseSettingsRecord{
			Repository: plan.Repository, Workflows: built,
		}, logger)
		return
	}
	if err := writeConsumerConfig(config.ConsumerConfigPath, updated); err != nil {
		logger.Error("the release policy could not be written to the destination configuration",
			"repository", plan.Repository, "error", err.Error())
		return
	}
	logger.Info("the engine wrote the release policy it now owns",
		"repository", plan.Repository, "settings", strings.Join(written, ","))
	sealReleaseSettings(runDir, releaseSettingsRecord{
		Repository: plan.Repository, Workflows: built, Written: written,
	}, logger)
	recordReleaseSettingsDecision(runDir, built, written)
}

// withDigestCommitPolicy fills in the staging digest-commit policy for one
// destination, and says which settings it wrote. An empty answer means the
// configuration already said this.
func withDigestCommitPolicy(config worker.Config, repository string, built []string) (worker.Config, []string) {
	for index := range config.Consumers {
		if config.Consumers[index].Repository != repository {
			continue
		}
		if config.Consumers[index].GitHub.StagingDigestCommit != nil {
			return config, nil
		}
		paths := make([]string, 0, len(built))
		for _, file := range built {
			paths = append(paths, path.Base(file))
		}
		sortedPaths := slices.Clone(paths)
		slices.Sort(sortedPaths)
		config.Consumers[index].GitHub.StagingDigestCommit = &worker.ConsumerDigestCommit{
			RequireDigestOnly:  true,
			ExactMessagePrefix: digestCommitMessagePrefix,
			ExactPaths:         sortedPaths,
			ActorLogin:         digestCommitActor,
		}
		return config, []string{"github_contract.staging_digest_commit"}
	}
	return config, nil
}

// writeConsumerConfig replaces the destination configuration with one the
// engine has already validated, atomically and in the canonical form the
// digest is taken over. Written through a temporary and renamed, so a
// reader between the two sees one whole file or the other.
func writeConsumerConfig(filename string, config worker.Config) error {
	if _, err := config.SHA256(); err != nil {
		// SHA256 validates. A configuration this refuses is one the engine
		// has just composed and would make every card fail to load, so it
		// does not reach the file.
		return err
	}
	encoded, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	temporary := filename + ".lassdas-tmp"
	if err := os.Remove(temporary); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.WriteFile(temporary, encoded, 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporary, filename); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}

// deliveredProductPaths are the files the delivered pull request carries,
// read from the record the publish card sealed. It is what tells a plan
// that said it would build a file from a round that actually did.
func deliveredProductPaths(runDir string) []string {
	raw, err := os.ReadFile(filepath.Join(runDir, "feature-pr.json"))
	if err != nil {
		return nil
	}
	var wrapper struct {
		Binding struct {
			ProductPaths []string `json:"product_paths"`
		} `json:"binding"`
	}
	if json.Unmarshal(raw, &wrapper) != nil {
		return nil
	}
	return wrapper.Binding.ProductPaths
}

// recordReleaseSettingsDecision puts the write where the report reads
// everything else the engine settled without asking.
func recordReleaseSettingsDecision(runDir string, built, written []string) {
	entry := struct {
		Kind      string `json:"kind"`
		Statement string `json:"statement"`
		Evidence  string `json:"evidence"`
	}{
		Kind: "defensible_default",
		Statement: "この実行で本体が書いた deploy の workflow (" + strings.Join(built, " / ") +
			") に合わせて、納品先の設定 " + strings.Join(written, " / ") + " を本体が書きました。",
		Evidence: "反映を記録するコミットの形は、本体が書いた workflow が作るものなので、" +
			"観測して決めるものではなく本体が決めた値です。",
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		return
	}
	path := filepath.Join(runDir, "history", "assumptions.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = file.Write(append(encoded, '\n'))
}

// sealReleaseSettings writes the record of what was written.
func sealReleaseSettings(runDir string, record releaseSettingsRecord, logger Logger) {
	encoded, err := json.Marshal(record)
	if err != nil {
		return
	}
	name := filepath.Join(runDir, releaseSettingsFile)
	// Removed before the write for the reason every other record here is: a
	// link left at the path must not carry the write somewhere else.
	if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
		return
	}
	if err := os.WriteFile(name, encoded, 0o600); err != nil {
		logger.Error("the release settings record could not be written; the delivery continues",
			"repository", record.Repository, "error", err.Error())
	}
}

// readReleaseSettings reads that record back, and says whether this
// delivery has already done its write.
func readReleaseSettings(runDir string) (releaseSettingsRecord, bool) {
	raw, err := os.ReadFile(filepath.Join(runDir, releaseSettingsFile))
	if err != nil {
		return releaseSettingsRecord{}, false
	}
	var record releaseSettingsRecord
	if json.Unmarshal(raw, &record) != nil {
		return releaseSettingsRecord{}, false
	}
	return record, record.Repository != ""
}
