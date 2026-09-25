package initwizard

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/localrun"
	runtimeconfig "automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/worker"
)

var modelRoles = []string{"implementer", "review-a", "review-b", "readiness-assessor", "readiness-checker", "designer", "applier"}

const (
	modelKeysShared   = "shared"
	modelKeysSeparate = "separate"
)

// ModelKeysShared and ModelKeysSeparate are the two key modes a setup
// records: one OpenRouter key for every role, or one per role.
const (
	ModelKeysShared   = modelKeysShared
	ModelKeysSeparate = modelKeysSeparate
)

// KeyName is the environment variable a role's own key lives in.
func KeyName(role string) string { return keyName(role) }

// ModelRoles lists the roles a setup names a model for.
func ModelRoles() []string { return append([]string(nil), modelRoles...) }

func keyName(role string) string {
	return "LASSDAS_" + strings.ToUpper(strings.ReplaceAll(role, "-", "_")) + "_KEY"
}
func modelName(role string) string { return strings.TrimSuffix(keyName(role), "_KEY") + "_MODEL" }
func allRoles(s *State) []string {
	roles := append([]string(nil), modelRoles...)
	if s.SeparateDesignReviews {
		roles = append(roles, "design-review-a", "design-review-b")
	}
	return roles
}

// Consumer is the destination this installation delivers to.
//
// The design path is off. It is written here and nowhere else: apply
// regenerates this file on every run, so the shipped example's own design
// block never reaches an installed project and editing the installed file by
// hand lasts until the next apply. Measured on one repository and one ticket
// text, 2026-09-17: without the design path two deliveries finished, in 6.5
// and 36 minutes; with it one took about two hours and one delivered nothing
// in 76 minutes across six design rounds.
func Consumer(s *State) worker.ConsumerConfig {
	consumer := worker.ConsumerConfig{Kind: "cli", Repository: s.Repository, RepositoryID: s.RepositoryID, Description: "CLI application", DeliveryBranch: s.Branch, IntegrationBranch: s.Branch, Delivery: worker.DeliverPullRequest, Design: &worker.DesignConfig{Default: "off"}, GitHub: worker.ConsumerGitHubContract{DefaultBranch: s.DefaultBranch}, Mode: s.Mode}
	if s.Means != nil {
		consumer.Infrastructure = s.Means.Infrastructure
	}
	return consumer
}

func agent(role string, timeout int) worker.AgentConfig {
	profile := "lassdas-" + role
	return worker.AgentConfig{
		ID: role, Command: "sh",
		// Hermes one-shot tools need the working copy explicitly. The worker
		// appends its prompt as an argument, never as part of the shell script.
		Args:           []string{"-c", `export TERMINAL_CWD="$PWD"; exec hermes "$@"`, "hermes", "--profile", profile, "-z"},
		Profile:        profile,
		SecretEnv:      map[string]string{keyName(role): keyName(role)},
		TimeoutSeconds: timeout,
	}
}

// Generate uses the same answers for direct endpoints, agent identities and
// entrypoint profiles; no credential values enter either JSON configuration.
func Generate(s *State, secrets Secrets) (worker.Config, runtimeconfig.Config, Secrets, error) {
	env := Secrets{}
	for k, v := range secrets {
		env[k] = v
	}
	delete(env, "LASSDAS_BOARD_USER")
	delete(env, "LASSDAS_BOARD_PASS")
	env["LASSDAS_BOARD_AUTH"] = "local"
	for _, role := range allRoles(s) {
		endpoint, ok := s.Models[role]
		if !ok {
			return worker.Config{}, runtimeconfig.Config{}, nil, fmt.Errorf("モデル未設定: %s", role)
		}
		endpoint.ID = role
		endpoint.BaseURL = s.BaseURL
		endpoint.APIKeyEnv = keyName(role)
		s.Models[role] = endpoint
		env[modelName(role)] = endpoint.Model
	}
	endpoint := func(role string) worker.ModelEndpoint { return s.Models[role] }
	implementer := endpoint("implementer")
	// The implementer endpoint is not what implements here: the implement
	// card launches the implementing agent, which reads its own key. This
	// endpoint is the identity sealed into a candidate, and no
	// model is called through it, so it keeps its own key name and the
	// agent's key is read for spend through the launch.
	implementer.APIKeyEnv = "LASSDAS_INTAKE_TARGET_KEY"
	designer := endpoint("designer")
	applier := agent("applier", 900)
	consumer := Consumer(s)
	// What the depth question answered reaches the configuration only where
	// there is somewhere for it to reach.
	//
	// This version installs command-line destinations, and the loader
	// refuses one that carries a delivery continuation or an observation at
	// all (internal/runtime/config.go's report destination check, and the
	// destination's own validateCLI). Writing the answers here would
	// produce an instance that does not start — which is a worse answer
	// than a shallower delivery, and a silent one. They stay in the journal
	// instead, and the interview says so while it is asking.
	carriesDelivery := consumer.EffectiveKind() != "cli"
	deliver := runtimeconfig.DeliverConfig{}
	if carriesDelivery {
		deliver = s.Deliver
		consumer.StagingLoginURL, consumer.ProductionLoginURL = s.StagingLoginURL, s.ProductionLoginURL
		consumer.ObservationLanguage = s.ObservationLanguage
		if s.Delivery != "" {
			consumer.Delivery = worker.Delivery(s.Delivery)
		}
	}
	config := worker.Config{SchemaVersion: worker.ConfigSchemaVersion, Consumers: []worker.ConsumerConfig{consumer}, MaxStages: 3, Models: worker.ModelConfig{Implementer: implementer, Reviewers: []worker.ModelEndpoint{endpoint("review-a"), endpoint("review-b")}, Readiness: worker.ReadinessModels{Assessor: endpoint("readiness-assessor"), Checker: endpoint("readiness-checker")}, Designer: &designer}, Agents: worker.AgentSet{Implementer: agent("implementer", 3600), Reviewer: agent("reviewer-unused", 3600), Applier: &applier}}
	for _, suffix := range []string{"a", "b"} {
		role := "review-" + suffix
		config.Agents.ReviewerAgents = append(config.Agents.ReviewerAgents, worker.ReviewerAgent{ReviewerID: role, Agent: agent(role, 3600)})
		designRole := "design-review-" + suffix
		source := role
		if s.SeparateDesignReviews {
			source = designRole
			judge := endpoint(designRole)
			judge.ID = role
			config.Models.DesignReviewers = append(config.Models.DesignReviewers, judge)
			config.Agents.DesignReviewerAgents = append(config.Agents.DesignReviewerAgents, worker.ReviewerAgent{ReviewerID: role, Agent: agent(designRole, 3600)})
		}
		env[modelName(designRole)] = s.Models[source].Model
		env[strings.TrimSuffix(keyName(designRole), "_KEY")+"_KEY_VAR"] = keyName(source)
	}
	if err := config.Validate(); err != nil {
		return config, runtimeconfig.Config{}, nil, fmt.Errorf("生成設定: %w", err)
	}
	if err := ValidateModelKeys(s, secrets); err != nil {
		return config, runtimeconfig.Config{}, nil, err
	}
	board := "local-" + s.Project
	runtime := runtimeconfig.Config{LedgerPath: "/data/ledger.db", ConsumerConfigPath: "/etc/lassdas/config/m1-consumer.json", KnowledgeRoot: "/data/instance", Tracker: s.Tracker, Identity: runtimeconfig.IdentityConfig{RepositoryID: s.EngineRepositoryID, Repository: s.EngineRepository, WorkflowRef: s.EngineRepository + "/local-runtime@" + s.EngineSHA, EngineSHA: s.EngineSHA}, AutomationRunID: s.AutomationRunID, ReportDestinations: []hook.ReportDestination{{Kind: "cli", Repository: s.Repository, Delivery: "pull_request"}}, WorkerBin: "/usr/local/bin/worker", ControllerBin: "/usr/local/bin/controller", WorkerSHA256: s.Pins["worker"], ControllerSHA256: s.Pins["controller"], HermesBin: "/usr/local/bin/hermes", HermesBoard: board, Orchestration: "cards", Chain: runtimeconfig.ChainConfig{RunsRoot: "/data/runs", TargetTokenPath: "/data/secrets/target-token", Deliver: deliver, Profiles: runtimeconfig.ChainProfiles{Implementer: "lassdas-implementer", ReviewA: "lassdas-review-a", ReviewB: "lassdas-review-b", Validate: "lassdas-validate", Publish: "lassdas-publish", Investigate: "lassdas-investigate", DesignReviewA: "lassdas-design-review-a", DesignReviewB: "lassdas-design-review-b", DesignDecide: "lassdas-design-decide", Applier: "lassdas-applier"}}}
	if s.Means != nil {
		runtime.Chain.Credentials = s.Means.Credentials
	}
	for k, v := range map[string]string{"LASSDAS_RUNTIME_CONFIG": "/etc/lassdas/config/runtime.json", "LASSDAS_STATE_DIR": "/data", "HERMES_KANBAN_DB": "/data/kanban.db", "LASSDAS_AGENT_TREE_ROOT": "/data/runs", "HERMES_KANBAN_BOARD": board, "LASSDAS_GATEWAY_BASE_URL": s.BaseURL, "LASSDAS_GUARDED_FILES": runtime.Chain.GuardedFiles(localrun.EngineGuardedFiles...)} {
		env[k] = v
	}
	if env["TARGET_GITHUB_TOKEN"] == "" || env["BACKLOG_API_KEY"] == "" {
		return config, runtime, nil, errors.New("本体の常用鍵が未設定です")
	}
	return config, runtime, env, nil
}

func ValidateModelKeys(s *State, secrets Secrets) error {
	if s.ModelKeyMode != "" && s.ModelKeyMode != modelKeysShared && s.ModelKeyMode != modelKeysSeparate {
		return errors.New("モデルの鍵の設定は shared または separate です")
	}
	names := []string{"LASSDAS_INTAKE_TARGET_KEY"}
	for _, role := range allRoles(s) {
		names = append(names, keyName(role))
	}
	seen := map[string]string{}
	for _, name := range names {
		value := secrets[name]
		if value == "" || strings.ContainsAny(value, "\r\n\x00") {
			return fmt.Errorf("%s の鍵を入力してください", name)
		}
		if s.ModelKeyMode == modelKeysShared {
			if value != secrets["LASSDAS_INTAKE_TARGET_KEY"] {
				return errors.New("共通キーの設定と役の鍵が一致しません。init の models 段をやり直してください")
			}
			continue
		}
		// Journals written before key modes existed used separate keys.
		if prior, ok := seen[value]; ok {
			return fmt.Errorf("別身元の鍵が同じです: %s / %s", prior, name)
		}
		seen[value] = name
	}
	return nil
}

func writeConfigs(dir string, consumer worker.Config, runtime runtimeconfig.Config) error {
	configDir := filepath.Join(dir, "config")
	if err := secureDir(configDir, 0755); err != nil {
		return err
	}
	legacyPath := filepath.Join(configDir, "consumer.json")
	if info, err := os.Lstat(legacyPath); err == nil && !info.Mode().IsRegular() {
		return errors.New("旧生成設定 consumer.json は通常ファイルである必要があります")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	for name, value := range map[string]any{"m1-consumer.json": consumer, "runtime.json": runtime} {
		raw, err := marshal(value)
		if err != nil {
			return err
		}
		if err := atomicWrite(filepath.Join(configDir, name), raw, 0644); err != nil {
			return err
		}
	}
	// Earlier init versions used a basename the controller cannot load. This
	// is a generated config, retired only after the instance has been stopped
	// and both replacement configs have been written. Runtime state is retained.
	if err := os.Remove(legacyPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
