package attendant

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/worker"
)

// settingsFixture is a delivery past its pull request, with a sealed plan
// naming the workflow files the round was told to build.
func settingsFixture(t *testing.T, handed bool, workflows, delivered []string) (runtime.Config, string) {
	t.Helper()
	config, _, runDir := releasePathFixture(t, `{"repository":"`+gapRepository+`"}`, true)
	config.ConsumerConfigPath = liveConsumerConfig(t, handed)
	if len(workflows) > 0 {
		plan := worker.ReleasePathPlan{
			SchemaVersion: worker.ReleasePathSchemaVersion, Repository: gapRepository,
			Configured: "production", WorkflowFiles: workflows,
			DecidedAt: time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC),
			Items: []worker.ReleasePathItem{{
				Name: workflows[0], Kind: worker.ReleasePathWorkflow, Detail: "build it",
			}},
		}
		if err := plan.Seal(); err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(plan)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(runDir, worker.ReleasePathFile), encoded, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	binding, err := json.Marshal(map[string]any{
		"binding": map[string]any{"product_paths": delivered},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "feature-pr.json"), binding, 0o600); err != nil {
		t.Fatal(err)
	}
	return config, runDir
}

// liveConsumerConfig is the engine's own sample destination configuration,
// copied somewhere writable, asking for production with its digest-commit
// policy not yet written — and, when the means is handed, carrying the
// content policy that lets the engine author the workflow.
func liveConsumerConfig(t *testing.T, handed bool) string {
	t.Helper()
	config, err := worker.LoadConfig("../../config/m1-consumer.json")
	if err != nil {
		t.Fatal(err)
	}
	config.Consumers[0].Delivery = worker.DeliverProduction
	config.Consumers[0].GitHub.StagingDigestCommit = nil
	if handed {
		config.Consumers[0].Infrastructure = &worker.InfrastructureConfig{
			Provider: "aws",
			DeployWorkflows: &worker.DeployWorkflowPolicy{
				Paths:       []string{"deploy-staging.yml"},
				Triggers:    []string{"workflow_dispatch"},
				Permissions: map[string]string{"contents": "read"},
				Runners:     []string{"ubuntu-latest"},
			},
		}
	}
	encoded, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "consumer.json")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// digestCommitOf reads back what the destination's configuration now says
// about the commit that records what landed on staging.
func digestCommitOf(t *testing.T, path string) *worker.ConsumerDigestCommit {
	t.Helper()
	config, err := worker.LoadConfig(path)
	if err != nil {
		t.Fatalf("the configuration no longer loads: %v", err)
	}
	return config.Consumers[0].GitHub.StagingDigestCommit
}

// Where the engine wrote the workflow that makes the commit, the shape of
// that commit is not something to observe but something it chose — so it
// writes it into the destination's own configuration rather than naming it
// in a report for somebody else to write.
func TestTheEngineWritesThePolicyItNowOwns(t *testing.T) {
	workflows := []string{".github/workflows/deploy-staging.yml"}
	config, runDir := settingsFixture(t, true, workflows, workflows)

	writeReleaseSettings(config, runDir, &recordingLogger{})

	policy := digestCommitOf(t, config.ConsumerConfigPath)
	if policy == nil {
		t.Fatal("the engine wrote no digest-commit policy for a workflow it authored")
	}
	if policy.ExactMessagePrefix != digestCommitMessagePrefix || policy.ActorLogin != digestCommitActor ||
		len(policy.ExactPaths) != 1 || policy.ExactPaths[0] != "deploy-staging.yml" {
		t.Fatalf("the policy the engine wrote is not the one its own workflow makes: %+v", policy)
	}
	// Written where the report reads everything else the engine settled
	// without asking.
	decisions, err := os.ReadFile(filepath.Join(runDir, "history", "assumptions.jsonl"))
	if err != nil || !strings.Contains(string(decisions), "deploy-staging.yml") {
		t.Fatalf("the write was not recorded as a decision: %v / %s", err, decisions)
	}
	// A second tick does not write again.
	before, err := os.ReadFile(config.ConsumerConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	writeReleaseSettings(config, runDir, &recordingLogger{})
	after, err := os.ReadFile(config.ConsumerConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("a second tick wrote the configuration again")
	}
}

// Nothing is written for a delivery whose workflow somebody else wrote: the
// destination's own policy is not the engine's to invent.
func TestNothingIsWrittenWhenTheWorkflowWasNotEngineAuthored(t *testing.T) {
	for name, fixture := range map[string]struct {
		handed               bool
		workflows, delivered []string
	}{
		"no plan at all": {false, nil, []string{"client/src/app.ts"}},
		"a plan the round did not build": {
			true,
			[]string{".github/workflows/deploy-staging.yml"},
			[]string{"client/src/app.ts"},
		},
	} {
		config, runDir := settingsFixture(t, fixture.handed, fixture.workflows, fixture.delivered)
		writeReleaseSettings(config, runDir, &recordingLogger{})
		if policy := digestCommitOf(t, config.ConsumerConfigPath); policy != nil {
			t.Fatalf("%s: the engine wrote a policy it does not own: %+v", name, policy)
		}
	}
}

// The write moves the destination's digest, which is what restarts a run
// whose configuration changed underneath it — and a delivery past its pull
// request is exempt from that, which is the only reason this write is safe.
func TestTheWriteMovesTheDigestAndRestartsNoDeliveryPastItsPullRequest(t *testing.T) {
	workflows := []string{".github/workflows/deploy-staging.yml"}
	config, runDir := settingsFixture(t, true, workflows, workflows)
	before, err := worker.LoadConfig(config.ConsumerConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeDigest, err := before.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	// The delivery sealed its draft under the digest as it was.
	if err := os.WriteFile(filepath.Join(runDir, "ticket-draft.json"),
		[]byte(`{"repository":"`+gapRepository+`","config_sha256":"`+beforeDigest+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	writeReleaseSettings(config, runDir, &recordingLogger{})

	after, err := worker.LoadConfig(config.ConsumerConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	afterDigest, err := after.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	if afterDigest == beforeDigest {
		t.Fatal("the write left the destination's digest where it was")
	}
	if settingsChangedUnderRun(config, runDir) {
		t.Fatal("a delivery past its pull request was restarted by the engine's own write")
	}
	// The same moved digest does restart a delivery that has not got there.
	if err := os.Remove(filepath.Join(runDir, "feature-pr.json")); err != nil {
		t.Fatal(err)
	}
	if !settingsChangedUnderRun(config, runDir) {
		t.Fatal("a delivery before its pull request ignored a configuration that moved")
	}
}
