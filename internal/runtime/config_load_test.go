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
		"orchestration": "cards",
		"chain":         cardsChainMap(),
		"ledger_path":   "/data/ledger.db",
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
	if config.Chain.RunsRoot != "/data/runs" {
		t.Fatalf("chain.runs_root = %q", config.Chain.RunsRoot)
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

// A pod that still asks for the retired single-card mode — or says nothing,
// which used to mean it — must be told what to change rather than started
// on a shape nobody configured. The same goes for the setting that mode
// took with it: the operator is told which line to delete, instead of the
// decoder's unknown-field message.
func TestLoadRefusesEverythingButTheCardChain(t *testing.T) {
	for _, orchestration := range []any{"runner", "", "swarm", nil} {
		raw := validRuntimeConfigMap()
		if orchestration == nil {
			delete(raw, "orchestration")
		} else {
			raw["orchestration"] = orchestration
		}
		_, err := Load(writeRuntimeConfig(t, raw))
		if err == nil || err.Error() != OrchestrationRefusal {
			t.Fatalf("orchestration %v: Load() error = %v, want %s", orchestration, err, OrchestrationRefusal)
		}
	}
	raw := validRuntimeConfigMap()
	raw["hermes_profile"] = "an-assignee-profile"
	_, err := Load(writeRuntimeConfig(t, raw))
	if err == nil || !strings.Contains(err.Error(), "hermes_profile") || !strings.Contains(err.Error(), "削除") {
		t.Fatalf("hermes_profile: Load() error = %v", err)
	}
	// The hold that stopped intake after several deliveries ended the same
	// way. A failed card is climbed away from rather than reported now, so
	// that run of identical endings cannot form and the number would hold
	// nothing back. Left in place it would read as a live safeguard, so the
	// file is refused by name — not read and ignored, and not refused with
	// the decoder's unknown-field message either.
	for _, value := range []any{3, 0} {
		streak := validRuntimeConfigMap()
		chain := cardsChainMap()
		chain["failure_streak_limit"] = value
		streak["chain"] = chain
		_, err := Load(writeRuntimeConfig(t, streak))
		if err == nil || !strings.Contains(err.Error(), "failure_streak_limit") || !strings.Contains(err.Error(), "削除") {
			t.Fatalf("failure_streak_limit %v: Load() error = %v", value, err)
		}
	}
	// A file carrying all of them names all of them. Found one at a time,
	// the next would only appear after the first was fixed and the pod
	// restarted.
	both := validRuntimeConfigMap()
	both["orchestration"] = "runner"
	both["hermes_profile"] = "an-assignee-profile"
	bothChain := cardsChainMap()
	bothChain["failure_streak_limit"] = 3
	both["chain"] = bothChain
	_, err = Load(writeRuntimeConfig(t, both))
	if err == nil || !strings.Contains(err.Error(), "orchestration") ||
		!strings.Contains(err.Error(), "hermes_profile") || !strings.Contains(err.Error(), "failure_streak_limit") {
		t.Fatalf("every retired setting: Load() error = %v", err)
	}
}

// The waits between attempts are refused rather than repaired when they
// make no sense: a negative wait has already passed, which turns the last
// rung of the ladder into a loop with no pause in it, and a first wait
// longer than the longest would be clamped down to it on the very first
// attempt.
func TestLoadRefusesRetrySettingsThatMakeNoSense(t *testing.T) {
	for name, settings := range map[string]map[string]any{
		"a negative first wait": {"retry_backoff_base_seconds": -1},
		"a negative longest":    {"retry_backoff_max_seconds": -30},
		"a negative limit":      {"retry_max_attempts": -1},
		"a negative notice":     {"retry_notice_attempts": -1},
		"a first wait too long": {"retry_backoff_base_seconds": 3600, "retry_backoff_max_seconds": 60},
	} {
		raw := validRuntimeConfigMap()
		chain := cardsChainMap()
		for key, value := range settings {
			chain[key] = value
		}
		raw["chain"] = chain
		if _, err := Load(writeRuntimeConfig(t, raw)); err == nil || !strings.Contains(err.Error(), "retry_") {
			t.Fatalf("%s: Load() error = %v", name, err)
		}
	}
	// Two wrong at once names the first of them, every time. Refused by
	// whichever one a map handed over first, an operator would fix a
	// different line on each restart.
	raw := validRuntimeConfigMap()
	chain := cardsChainMap()
	chain["retry_backoff_base_seconds"] = -1
	chain["retry_notice_attempts"] = -1
	raw["chain"] = chain
	path := writeRuntimeConfig(t, raw)
	for attempt := 0; attempt < 8; attempt++ {
		_, err := Load(path)
		if err == nil || !strings.Contains(err.Error(), "retry_backoff_base_seconds") {
			t.Fatalf("attempt %d named a different setting: %v", attempt, err)
		}
	}

	// And the settings that do make sense load.
	sane := validRuntimeConfigMap()
	saneChain := cardsChainMap()
	saneChain["retry_backoff_base_seconds"] = 30
	saneChain["retry_backoff_max_seconds"] = 600
	saneChain["retry_notice_attempts"] = 2
	sane["chain"] = saneChain
	config, err := Load(writeRuntimeConfig(t, sane))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if config.Chain.RetryBackoffBase() != 30*time.Second || config.Chain.RetryNoticeAttemptsValue() != 2 {
		t.Fatalf("the configured waits did not come back: %+v", config.Chain)
	}
}

// The chain has nowhere to work without a run directory root, and a card
// created with Hermes' scratch default loses its records on replacement.
func TestLoadRequiresTheRunsRoot(t *testing.T) {
	raw := validRuntimeConfigMap()
	chain := cardsChainMap()
	delete(chain, "runs_root")
	raw["chain"] = chain
	_, err := Load(writeRuntimeConfig(t, raw))
	if err == nil || !strings.Contains(err.Error(), "chain.runs_root") {
		t.Fatalf("Load() error = %v, want chain.runs_root required", err)
	}
	chain["runs_root"] = ""
	raw["chain"] = chain
	if _, err := Load(writeRuntimeConfig(t, raw)); err == nil {
		t.Fatal("Load() accepted an empty chain.runs_root")
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
		"bad automation id":     func(m map[string]any) { m["automation_run_id"] = "run id" },
		"no destinations":       func(m map[string]any) { m["report_destinations"] = []any{} },
		"bad binary pin":        func(m map[string]any) { m["worker_sha256"] = "zz" },
		"no chain at all":       func(m map[string]any) { delete(m, "chain") },
		"unknown field":         func(m map[string]any) { m["surprise"] = true },
		"unknown orchestration": func(m map[string]any) { m["orchestration"] = "swarm" },
		"no target token path": func(m map[string]any) {
			chain := cardsChainMap()
			delete(chain, "target_token_path")
			m["chain"] = chain
		},
		"a stage profile missing": func(m map[string]any) {
			chain := cardsChainMap()
			delete(chain["profiles"].(map[string]any), "publish")
			m["chain"] = chain
		},
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
			chain := cardsChainMap()
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
