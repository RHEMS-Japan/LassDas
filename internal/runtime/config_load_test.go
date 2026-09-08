package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/worker"
)

// validRuntimeConfigMap is a complete runtime.json in map form, so each test
// can break exactly one thing.
func validRuntimeConfigMap() map[string]any {
	return map[string]any{
		"ledger_path": "/data/ledger.db",
		"tracker": map[string]any{
			"origin": "https://example.backlog.com", "space_key": "example",
			"project_id": 100, "project_key": "TKT",
			"allowed_creator_id": 7, "allowed_activity_type": 1,
			"required_category_id": 0,
			"board_statuses":       map[string]any{"running": 1, "awaiting_answer": 2, "delivered": 3, "needs_attention": 4},
		},
		"identity": map[string]any{
			"repository_id": 1, "repository": "example/consumer",
			"workflow_ref": "example/consumer/pod@main",
			"engine_sha":   strings.Repeat("ab", 20),
		},
		"automation_run_id": "run_20260802_" + strings.Repeat("ab", 12),
		"report_destinations": []any{map[string]any{
			"repository": "example/consumer", "delivery": "pull_request",
			"staging_origin": "https://stg.example.com", "production_origin": "https://example.com",
		}},
		"consumer_config_path": "/etc/lassdas/config/m1-consumer.json",
		"knowledge_root":       "/etc/lassdas/config/knowledge",
		"worker_bin":           "/usr/local/bin/worker",
		"controller_bin":       "/usr/local/bin/controller",
		"browsercheck_bin":     "",
		"hermes_bin":           "/usr/local/bin/hermes",
		"hermes_board":         "lassdas",
		"hermes_profile":       "lassdas-runner",
	}
}

func writeRuntimeConfig(t *testing.T, config map[string]any) string {
	t.Helper()
	if config["consumer_config_path"] == "/etc/lassdas/config/m1-consumer.json" {
		fixture, err := worker.LoadConfig("../../config/m1-consumer.json")
		if err != nil {
			t.Fatal(err)
		}
		fixture.Consumers = fixture.Consumers[:1]
		encoded, err := json.Marshal(fixture)
		if err != nil {
			t.Fatal(err)
		}
		filename := filepath.Join(t.TempDir(), "consumer.json")
		if err := os.WriteFile(filename, encoded, 0600); err != nil {
			t.Fatal(err)
		}
		config["consumer_config_path"] = filename
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "runtime.json")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadAcceptsACompleteConfig(t *testing.T) {
	config, err := Load(writeRuntimeConfig(t, validRuntimeConfigMap()))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if config.OrchestrationCards() {
		t.Fatal("a config without orchestration reported cards mode")
	}
}

func cardsChainMap() map[string]any {
	return map[string]any{
		"runs_root":         "/data/runs",
		"target_token_path": "/data/secrets/target-token",
		"profiles": map[string]any{
			"implementer": "lassdas-implementer", "review_a": "lassdas-review-a",
			"review_b": "lassdas-review-b", "validate": "lassdas-validate", "publish": "lassdas-publish",
		},
	}
}

func TestLoadAcceptsTheCardsOrchestration(t *testing.T) {
	raw := validRuntimeConfigMap()
	raw["orchestration"] = "cards"
	raw["chain"] = cardsChainMap()
	config, err := Load(writeRuntimeConfig(t, raw))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !config.OrchestrationCards() {
		t.Fatal("the cards orchestration did not report itself")
	}
}

func deliverChainMap() map[string]any {
	return map[string]any{
		"checks_profile": "lassdas-checks", "integrate_profile": "lassdas-integrate",
		"promote_profile": "lassdas-promote", "enabled_after": "2026-09-01T00:00:00Z",
	}
}

func TestLoadAcceptsTheDeliverConfiguration(t *testing.T) {
	raw := validRuntimeConfigMap()
	raw["orchestration"] = "cards"
	raw["browsercheck_bin"] = "/usr/local/bin/browsercheck"
	chain := cardsChainMap()
	chain["deliver"] = deliverChainMap()
	raw["chain"] = chain
	config, err := Load(writeRuntimeConfig(t, raw))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	deliver := config.Chain.Deliver
	if !deliver.Enabled() {
		t.Fatal("a fully configured deliver did not report itself enabled")
	}
	if deliver.GoWait() != 7*24*time.Hour {
		t.Fatalf("GoWait() = %v, want the 7-day default", deliver.GoWait())
	}
	if deliver.ChecksWallSeconds() != 3*60*60 || deliver.IntegrateWallSeconds() != 3*60*60 || deliver.PromoteWallSeconds() != 3*60*60 {
		t.Fatal("the card walls did not default to 3 hours")
	}
	if _, err := deliver.EnabledAfterTime(); err != nil {
		t.Fatalf("EnabledAfterTime() error = %v", err)
	}
}

func TestLoadAcceptsTheDebugRole(t *testing.T) {
	raw := validRuntimeConfigMap()
	raw["orchestration"] = "cards"
	chain := cardsChainMap()
	chain["e2e_profile"] = "lassdas-e2e"
	chain["e2e_enabled_after"] = "2026-08-30T12:00:00Z"
	raw["chain"] = chain
	config, err := Load(writeRuntimeConfig(t, raw))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if config.Chain.E2EWallSeconds() != 76*60*60 {
		t.Fatalf("E2EWallSeconds() = %d, want the 76-hour default", config.Chain.E2EWallSeconds())
	}
	if _, err := config.Chain.E2EEnabledAfterTime(); err != nil {
		t.Fatalf("E2EEnabledAfterTime() error = %v", err)
	}
}

func TestLoadRejectsBrokenConfigs(t *testing.T) {
	mutations := map[string]func(map[string]any){
		"missing ledger path":   func(m map[string]any) { m["ledger_path"] = "" },
		"missing worker bin":    func(m map[string]any) { m["worker_bin"] = "" },
		"missing tracker key":   func(m map[string]any) { m["tracker"].(map[string]any)["space_key"] = "" },
		"wrong activity type":   func(m map[string]any) { m["tracker"].(map[string]any)["allowed_activity_type"] = 2 },
		"bad repository":        func(m map[string]any) { m["identity"].(map[string]any)["repository"] = "no-slash" },
		"short engine sha":      func(m map[string]any) { m["identity"].(map[string]any)["engine_sha"] = "abc" },
		"bad automation id":     func(m map[string]any) { m["automation_run_id"] = "run" },
		"no destinations":       func(m map[string]any) { m["report_destinations"] = []any{} },
		"bad binary pin":        func(m map[string]any) { m["worker_sha256"] = "zz" },
		"cards without chain":   func(m map[string]any) { m["orchestration"] = "cards" },
		"unknown field":         func(m map[string]any) { m["surprise"] = true },
		"unknown orchestration": func(m map[string]any) { m["orchestration"] = "swarm" },
		"e2e profile without cut-off": func(m map[string]any) {
			m["orchestration"] = "cards"
			chain := cardsChainMap()
			chain["e2e_profile"] = "lassdas-e2e"
			m["chain"] = chain
		},
		"e2e profile reusing a stage profile": func(m map[string]any) {
			m["orchestration"] = "cards"
			chain := cardsChainMap()
			chain["e2e_profile"] = "lassdas-validate"
			chain["e2e_enabled_after"] = "2026-08-30T12:00:00Z"
			m["chain"] = chain
		},
		"deliver partially configured": func(m map[string]any) {
			m["orchestration"] = "cards"
			m["browsercheck_bin"] = "/usr/local/bin/browsercheck"
			chain := cardsChainMap()
			chain["deliver"] = map[string]any{"checks_profile": "lassdas-checks"}
			m["chain"] = chain
		},
		"deliver without cut-off": func(m map[string]any) {
			m["orchestration"] = "cards"
			m["browsercheck_bin"] = "/usr/local/bin/browsercheck"
			chain := cardsChainMap()
			chain["deliver"] = deliverChainMap()
			delete(chain["deliver"].(map[string]any), "enabled_after")
			m["chain"] = chain
		},
		"deliver together with the debug role": func(m map[string]any) {
			m["orchestration"] = "cards"
			m["browsercheck_bin"] = "/usr/local/bin/browsercheck"
			chain := cardsChainMap()
			chain["e2e_profile"] = "lassdas-e2e"
			chain["e2e_enabled_after"] = "2026-08-30T12:00:00Z"
			chain["deliver"] = deliverChainMap()
			m["chain"] = chain
		},
		"deliver reusing a stage profile": func(m map[string]any) {
			m["orchestration"] = "cards"
			m["browsercheck_bin"] = "/usr/local/bin/browsercheck"
			chain := cardsChainMap()
			deliver := deliverChainMap()
			deliver["promote_profile"] = "lassdas-validate"
			chain["deliver"] = deliver
			m["chain"] = chain
		},
		"deliver without the browsercheck binary": func(m map[string]any) {
			m["orchestration"] = "cards"
			chain := cardsChainMap()
			chain["deliver"] = deliverChainMap()
			m["chain"] = chain
		},
	}
	for name, mutate := range mutations {
		raw := validRuntimeConfigMap()
		mutate(raw)
		if _, err := Load(writeRuntimeConfig(t, raw)); err == nil {
			t.Errorf("Load() accepted a config with %s", name)
		}
	}
	if _, err := Load(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Error("Load() accepted a missing file")
	}
}

func TestListBoardTasksReadsEveryAssignee(t *testing.T) {
	bin, callLog, tasksFile := stubHermes(t)
	setTasks(t, tasksFile, []BoardTask{{ID: "t1", Status: "blocked", IdempotencyKey: "k1", BlockKind: "needs_input"}})
	hermes := NewHermes(Config{HermesBin: bin, HermesBoard: "lassdas"})
	tasks, err := hermes.ListBoardTasks(context.Background())
	if err != nil || len(tasks) != 1 || tasks[0].BlockKind != "needs_input" {
		t.Fatalf("ListBoardTasks() = %+v, %v", tasks, err)
	}
	records := calls(t, callLog)
	if len(records) != 1 || strings.Contains(records[0], "--assignee") {
		t.Fatalf("board listing was assignee-scoped: %v", records)
	}
}

func cliRuntimeConfigMap(t *testing.T) map[string]any {
	t.Helper()
	consumers, err := worker.LoadConfig("../../config/m1-consumer.json")
	if err != nil {
		t.Fatal(err)
	}
	consumers.Consumers = consumers.Consumers[:1]
	original := consumers.Consumers[0]
	consumers.Consumers[0] = worker.ConsumerConfig{Kind: "cli", Repository: original.Repository, RepositoryID: original.RepositoryID, Delivery: worker.DeliverPullRequest, IntegrationBranch: "develop", GitHub: worker.ConsumerGitHubContract{DefaultBranch: "main"}, Mode: original.Mode}
	encoded, err := json.Marshal(consumers)
	if err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(t.TempDir(), "consumer.json")
	if err := os.WriteFile(filename, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	raw := validRuntimeConfigMap()
	raw["ledger_path"] = filepath.Join(t.TempDir(), "ledger.db")
	raw["consumer_config_path"] = filename
	raw["report_destinations"] = []any{map[string]any{"kind": "cli", "repository": original.Repository, "delivery": "pull_request"}}
	return raw
}

func TestCLIConfigAndBootBindConsumerToReportDestination(t *testing.T) {
	t.Setenv("BACKLOG_API_KEY", "test-key")
	config, err := Load(writeRuntimeConfig(t, cliRuntimeConfigMap(t)))
	if err != nil {
		t.Fatal(err)
	}
	services, err := BuildServices(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	if services.Route.Destinations[0].EffectiveKind() != "cli" {
		t.Fatal("CLI report route was not wired")
	}
	if err := services.Close(); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"kind", "repository", "delivery"} {
		t.Run(field, func(t *testing.T) {
			raw := cliRuntimeConfigMap(t)
			destination := raw["report_destinations"].([]any)[0].(map[string]any)
			switch field {
			case "kind":
				destination["kind"] = "web"
				destination["staging_origin"] = "https://stg.example.com"
				destination["production_origin"] = "https://example.com"
			case "repository":
				destination[field] = "example/other"
			case "delivery":
				destination[field] = "integration"
			}
			if _, err := Load(writeRuntimeConfig(t, raw)); err == nil {
				t.Fatal("accepted consumer/report mismatch")
			}
		})
	}
	for _, field := range []string{"e2e_profile", "e2e_enabled_after", "e2e_max_runtime_seconds", "deliver"} {
		t.Run(field, func(t *testing.T) {
			raw := cliRuntimeConfigMap(t)
			chain := map[string]any{}
			switch field {
			case "e2e_profile":
				chain[field] = "observer"
			case "e2e_enabled_after":
				chain[field] = "2026-09-08T00:00:00Z"
			case "e2e_max_runtime_seconds":
				chain[field] = 1
			case "deliver":
				chain[field] = map[string]any{"go_wait_seconds": 1}
			}
			raw["chain"] = chain
			if _, err := Load(writeRuntimeConfig(t, raw)); err == nil {
				t.Fatal("accepted CLI web stage settings")
			}
		})
	}
}

func TestBootRechecksConsumerBeforeCreatingState(t *testing.T) {
	raw := cliRuntimeConfigMap(t)
	config, err := Load(writeRuntimeConfig(t, raw))
	if err != nil {
		t.Fatal(err)
	}
	consumers, err := worker.LoadConfig(config.ConsumerConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	consumers.Consumers[0].Repository = "example/other"
	encoded, err := json.Marshal(consumers)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.ConsumerConfigPath, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildServices(config, nil); err == nil {
		t.Fatal("boot accepted a replaced consumer destination")
	}
	for _, path := range []string{config.LedgerPath, filepath.Join(filepath.Dir(config.LedgerPath), "route.key")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("mismatched boot created %s", path)
		}
	}
}
