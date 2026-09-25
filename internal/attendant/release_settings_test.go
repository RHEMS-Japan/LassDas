package attendant

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/worker"
)

// The engine does not write the destination's configuration, and the one
// setting it looked able to write is the one it must not.
//
// The staging digest-commit policy says which files the deployment's own
// commit modifies, with which exact message and by whom, and the promotion
// holds the real commit to it to the file (internal/githubapi/waits.go).
// Nothing asks any round to make such a commit, so a policy written from
// the workflow's own file name would describe a commit that never happens
// — and a destination carrying it would stop short of production for ever
// after. It is reported by name instead.
func TestTheEngineWritesNoSettingIntoTheDestinationsConfiguration(t *testing.T) {
	harness := newDepthHarness(t, "production", true, "")
	// The means handed, the plan naming a workflow file, and a pull request
	// carrying it: the exact delivery a write would have happened on.
	handed := workflowMeansConfig(t, harness.config.ConsumerConfigPath)
	plan := worker.ReleasePathPlan{
		SchemaVersion: worker.ReleasePathSchemaVersion, Repository: depthRepository,
		Configured: "production", WorkflowFiles: []string{".github/workflows/deploy-staging.yml"},
		DecidedAt: time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC),
		Items: []worker.ReleasePathItem{{
			Name: ".github/workflows/deploy-staging.yml", Kind: worker.ReleasePathWorkflow, Detail: "build it",
		}},
	}
	if err := plan.Seal(); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	harness.write(worker.ReleasePathFile, string(encoded))
	harness.write("feature-pr.json",
		`{"payload":{"pull_request":{"HTMLURL":"https://github.com/example/consumer/pull/9"}},`+
			`"binding":{"product_paths":[".github/workflows/deploy-staging.yml"]}}`)
	// The delivery as the proven sequence leaves it: both earlier cards
	// done, the checks record sealed, staging passed. This is the tick that
	// reads the plan and decides about the promotion, and it is the one a
	// write would have happened on.
	harness.setBoard(harness.card(deliverStageChecks, "done", 1), harness.card(deliverStageIntegrate, "done", 1))
	harness.write(runner.DeliverChecksFile, `{"ok":true}`)
	harness.sealPhase(runner.DeliverStagingReportFile, harness.stagingPass())

	harness.tick()
	harness.tick()

	after, err := os.ReadFile(harness.config.ConsumerConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != handed {
		t.Fatalf("the delivery wrote the destination's configuration:\nbefore %s\nafter  %s", handed, after)
	}
	// And no OTHER delivery was restarted by it. A destination's
	// configuration is shared: moving its digest restarts every run still
	// before its pull request, not only the one that moved it, and only the
	// writing run is ever exempt. A second delivery, still short of its
	// pull request, is held to the digest it sealed under.
	other := filepath.Join(filepath.Dir(harness.runDir), "delivery_other")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	live, err := worker.LoadConfig(harness.config.ConsumerConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := live.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "ticket-draft.json"),
		[]byte(`{"repository":"`+depthRepository+`","config_sha256":"`+digest+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if settingsChangedUnderRun(harness.config, other) {
		t.Fatal("another delivery, still short of its pull request, was restarted by this one")
	}

	// And the setting it did not write is named in the plan the report
	// reads, by the key an operator knows it by.
	sealed, ok := readReleasePathPlan(harness.runDir)
	if !ok {
		t.Fatal("the plan is no longer readable after the tick")
	}
	if sealed.RecordSHA256 != plan.RecordSHA256 {
		t.Fatal("the delivery rewrote the sealed plan")
	}
}

// workflowMeansConfig replaces the harness's destination with a complete,
// loadable one that handed the means, and returns the bytes it left.
//
// Loadable matters: anything that would write this file reads it first, so
// a stand-in configuration no reader accepts would make the test pass by
// making the write impossible rather than by nobody attempting it.
func workflowMeansConfig(t *testing.T, path string) string {
	t.Helper()
	config, err := worker.LoadConfig("../../config/m1-consumer.json")
	if err != nil {
		t.Fatal(err)
	}
	config.Consumers = config.Consumers[:1]
	consumer := &config.Consumers[0]
	if consumer.Repository != depthRepository {
		t.Fatalf("the sample destination is %q, not the one this harness delivers to", consumer.Repository)
	}
	consumer.Delivery = worker.DeliverProduction
	consumer.GitHub.StagingDigestCommit = nil
	consumer.StagingOrigin, consumer.ProductionOrigin = depthStagingHost, depthProdHost
	consumer.StagingWorkflow, consumer.ProductionWorkflow = "deploy-staging.yml", "deploy-production.yml"
	consumer.GitHub.StagingWorkflow.Path = ".github/workflows/deploy-staging.yml"
	consumer.GitHub.ProductionWorkflows = consumer.GitHub.ProductionWorkflows[:1]
	consumer.GitHub.ProductionWorkflows[0].Path = ".github/workflows/deploy-production.yml"
	consumer.Infrastructure = &worker.InfrastructureConfig{
		Provider: "aws",
		DeployWorkflows: &worker.DeployWorkflowPolicy{
			Paths:       []string{"deploy-staging.yml"},
			Triggers:    []string{worker.WorkflowTriggerDispatch},
			Permissions: map[string]string{"contents": "read"},
			Runners:     []string{"ubuntu-latest"},
		},
	}
	if _, err := config.SHA256(); err != nil {
		t.Fatalf("the destination this test delivers to does not load: %v", err)
	}
	encoded, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
