package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/hook"
	runtimecfg "automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/worker"
)

func TestCheckConsumerCLIUsesSingleConsumerAndExclusiveTrialRecord(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runCLITestGit(t, root, "init", "--initial-branch=stg")
	runCLITestGit(t, root, "add", "README.md")
	runCLITestGit(t, root, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "base")
	sha := strings.TrimSpace(runCLITestGit(t, root, "rev-parse", "HEAD"))
	consumer := cliTestConfig().Consumers[0]
	consumer.Mode.VerifyWorkingDirectory = "."
	consumer.Mode.Toolchain = nil
	consumer.Mode.InstallCommand = []string{"true"}
	consumer.Mode.VerifyCommands = [][]string{{"true"}}
	directory := t.TempDir()
	path := filepath.Join(directory, "consumer.json")
	out := filepath.Join(directory, "trial.json")
	writeTestJSON(t, path, consumer)
	args := []string{"check-consumer", "--consumer", path, "--repo-root", root, "--base-sha", sha, "--out", out}
	if err := run(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	var result worker.ConsumerCheck
	if err := worker.ReadJSONFile(out, worker.MaxArtifactJSONBytes, &result); err != nil || result.BaseSHA != sha {
		t.Fatalf("trial record: %+v %v", result, err)
	}
	raw, _ := os.ReadFile(out)
	var fields map[string]any
	_ = json.Unmarshal(raw, &fields)
	for _, key := range []string{"delivery_id", "validation_sha256", "candidate_sha256"} {
		if _, exists := fields[key]; exists {
			t.Fatalf("trial claimed publication field %s", key)
		}
	}
	if err := run(context.Background(), args); err == nil {
		t.Fatal("existing trial evidence was overwritten")
	}
}

type noRuntimeNetwork struct{ t *testing.T }

func (n noRuntimeNetwork) RoundTrip(*http.Request) (*http.Response, error) {
	n.t.Fatal("runtime check attempted an external request")
	return nil, nil
}

func TestCheckRuntimeCLIOpensLocalServicesWithoutPolling(t *testing.T) {
	directory := t.TempDir()
	consumerPath := filepath.Join(directory, "consumer.json")
	writeTestJSON(t, consumerPath, cliTestConfig())
	config := runtimecfg.Config{
		LedgerPath: filepath.Join(directory, "ledger.db"), ConsumerConfigPath: consumerPath,
		KnowledgeRoot: filepath.Join(directory, "knowledge"), WorkerBin: "/usr/local/bin/worker", ControllerBin: "/usr/local/bin/controller",
		Identity:           runtimecfg.IdentityConfig{RepositoryID: 1, Repository: "example/engine", WorkflowRef: "example/engine/local-runtime@fixture", EngineSHA: strings.Repeat("a", 40)},
		AutomationRunID:    "run_20260908_" + strings.Repeat("a", 24),
		Tracker:            runtimecfg.TrackerConfig{Origin: "https://example.backlog.com", SpaceKey: "example", ProjectID: 100, ProjectKey: "TKT", AllowedCreatorID: 7, AllowedActivityType: 1},
		ReportDestinations: []hook.ReportDestination{{Repository: "example/consumer", Delivery: "pull_request", StagingOrigin: "https://stg.example.com", ProductionOrigin: "https://example.com"}},
	}
	path := filepath.Join(directory, "runtime.json")
	writeTestJSON(t, path, config)
	t.Setenv("BACKLOG_API_KEY", "fixture-local-only")
	original := http.DefaultTransport
	http.DefaultTransport = noRuntimeNetwork{t}
	t.Cleanup(func() { http.DefaultTransport = original })
	args := []string{"check-runtime", "--config", path}
	if err := run(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	key, err := os.ReadFile(filepath.Join(directory, "route.key"))
	if err != nil || len(key) != 64 {
		t.Fatalf("local route key missing: %v", err)
	}
	if err := run(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	resumed, _ := os.ReadFile(filepath.Join(directory, "route.key"))
	if string(resumed) != string(key) {
		t.Fatal("runtime resume replaced its route key")
	}
}
