package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var testInvocationTime = time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)

func validTestInvocation(endpoint ModelEndpoint) InvocationUsage {
	return InvocationUsage{
		RequestedModel: endpoint.Model, RequestID: "request-" + endpoint.ID, StopReason: ChatFinishStop,
		InputTokens: 10, OutputTokens: 5, TotalTokens: 15, LatencyMillis: 25,
	}
}

func validTestConfig() Config {
	return Config{
		SchemaVersion: ConfigSchemaVersion,
		Consumers: []ConsumerConfig{{
			Repository: "example/consumer", RepositoryID: 101,
			Delivery: DeliverPullRequest, IntegrationBranch: "stg", ReleaseBranch: "prod",
			StagingOrigin: "https://stg.example.com", ProductionOrigin: "https://example.com",
			StagingWorkflow: "deploy-stg.yml", ProductionWorkflow: "deploy.yml",
			GitHub: testConsumerGitHubContract(),
			Mode: ModeConfig{
				ID: "client-visible-change", AllowedFilePrefixes: []string{"client/src/"},
				ForbiddenCandidateText: []string{"forbidden-project-name"},
				MaxFiles:               3, MaxFileBytes: 256 * 1024, MaxTotalBytes: 512 * 1024,
				MaxChangedLines: 200, MaxChangedBytes: 64 * 1024,
				Toolchain:              []ToolRequirement{{Binary: "node", Version: "22", StripVPrefix: true}, {Binary: "pnpm", Version: "9.15.4"}},
				VerifyWorkingDirectory: "client",
				InstallCommand:         []string{"pnpm", "install", "--frozen-lockfile"},
				VerifyCommands:         [][]string{{"pnpm", "exec", "tsc", "--noEmit"}, {"pnpm", "build"}},
			},
		}},
		Models: ModelConfig{
			Implementer: ModelEndpoint{ID: "author", Vendor: "Vendor A", Model: "model-a", BaseURL: "https://gateway.example.com/api/v1", APIKeyEnv: "TEST_MODEL_KEY_A", Effort: "low", MaxOutputTokens: 4096},
			Reviewers: []ModelEndpoint{
				{ID: "review-a", Vendor: "Vendor A", Model: "model-a", BaseURL: "https://gateway.example.com/api/v1", APIKeyEnv: "TEST_MODEL_KEY_A", Lens: "correctness", MaxOutputTokens: 2048},
				{ID: "review-b", Vendor: "Vendor B", Model: "model-b", BaseURL: "https://gateway.example.com/api/v1", APIKeyEnv: "TEST_MODEL_KEY_B", Lens: "adversarial", StructuredOutput: true, MaxOutputTokens: 2048},
			},
			Readiness: ReadinessModels{
				Assessor: ModelEndpoint{ID: "readiness-assessor", Vendor: "Vendor A", Model: "model-a", BaseURL: "https://gateway.example.com/api/v1", APIKeyEnv: "TEST_MODEL_KEY_A", Effort: "high", MaxOutputTokens: 4096},
				Checker:  ModelEndpoint{ID: "readiness-checker", Vendor: "Vendor B", Model: "model-b", BaseURL: "https://gateway.example.com/api/v1", APIKeyEnv: "TEST_MODEL_KEY_B", Lens: "readiness adversarial", StructuredOutput: true, MaxOutputTokens: 2048},
			},
		},
		Agents: AgentSet{
			Implementer: AgentConfig{
				ID: "author-agent", Command: "true",
				Args:           []string{"--print"},
				Env:            map[string]string{"AGENT_BASE_URL": "https://gateway.example.com/api"},
				SecretEnv:      map[string]string{"AGENT_TOKEN": "TEST_MODEL_KEY_A"},
				TimeoutSeconds: 900,
			},
			Reviewer: AgentConfig{
				ID: "review-agent", Command: "false",
				Args:           []string{"exec"},
				Env:            map[string]string{"REVIEW_BASE_URL": "https://gateway.example.com/api/v1"},
				SecretEnv:      map[string]string{"REVIEW_TOKEN": "TEST_MODEL_KEY_B"},
				TimeoutSeconds: 900,
			},
		},
		MaxStages: 3,
	}
}

func TestConfigValidate(t *testing.T) {
	if err := validTestConfig().Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestConfigRejectsSingleReviewerVendor(t *testing.T) {
	config := validTestConfig()
	config.Models.Reviewers[1].Vendor = config.Models.Reviewers[0].Vendor
	if err := config.Validate(); err == nil {
		t.Fatal("Validate() accepted one reviewer vendor")
	}
}

func TestConfigRejectsDuplicateImplementerAndReviewerID(t *testing.T) {
	config := validTestConfig()
	config.Models.Reviewers[0].ID = config.Models.Implementer.ID
	if err := config.Validate(); err == nil {
		t.Fatal("Validate() accepted a duplicate implementer and reviewer id")
	}
}

func TestConfigRejectsDuplicateReviewerModel(t *testing.T) {
	config := validTestConfig()
	config.Models.Reviewers[0].Model = config.Models.Reviewers[1].Model
	config.Models.Reviewers[0].BaseURL = config.Models.Reviewers[1].BaseURL
	err := config.Validate()
	if err == nil {
		t.Fatal("Validate() accepted duplicate reviewer model paths")
	}
	if !strings.Contains(err.Error(), "duplicates") {
		t.Fatalf("Validate() error = %v, want the duplicate refusal", err)
	}
}

func TestConfigRejectsSecondReviewerOnImplementerEndpoint(t *testing.T) {
	config := validTestConfig()
	config.Models.Reviewers[1].Model = config.Models.Implementer.Model
	config.Models.Reviewers[1].BaseURL = config.Models.Implementer.BaseURL
	err := config.Validate()
	if err == nil {
		t.Fatal("Validate() accepted two reviewers on the implementer endpoint and model")
	}
	if !strings.Contains(err.Error(), "share the implementer") {
		t.Fatalf("Validate() error = %v, want the implementer-sharing refusal", err)
	}
}

func TestConfigAcceptsVendorHostTable(t *testing.T) {
	config := validTestConfig()
	config.Models.VendorHosts = map[string][]string{
		"vendor a": {"gateway.example.com"},
		"vendor b": {"gateway.example.com", "other.example.com"},
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestConfigRejectsVendorMissingFromHostTable(t *testing.T) {
	config := validTestConfig()
	config.Models.VendorHosts = map[string][]string{"vendor a": {"gateway.example.com"}}
	err := config.Validate()
	if err == nil {
		t.Fatal("Validate() accepted a vendor absent from the vendor host table")
	}
	if !strings.Contains(err.Error(), "no vendor host table entry") {
		t.Fatalf("Validate() error = %v, want the missing-entry refusal", err)
	}
}

func TestConfigRejectsBaseURLOutsideVendorHosts(t *testing.T) {
	config := validTestConfig()
	config.Models.VendorHosts = map[string][]string{
		"vendor a": {"gateway.example.com"},
		"vendor b": {"elsewhere.example.com"},
	}
	err := config.Validate()
	if err == nil {
		t.Fatal("Validate() accepted a base url host not registered for its vendor")
	}
	if !strings.Contains(err.Error(), "not registered for its vendor") {
		t.Fatalf("Validate() error = %v, want the unregistered-host refusal", err)
	}
}

func TestConfigRejectsInvalidVendorHostTable(t *testing.T) {
	oversizedTable := make(map[string][]string, 17)
	oversizedTable["vendor a"] = []string{"gateway.example.com"}
	oversizedTable["vendor b"] = []string{"gateway.example.com"}
	for index := 0; index < 15; index++ {
		oversizedTable["vendor-"+string(rune('c'+index))] = []string{"gateway.example.com"}
	}
	tooManyHosts := make([]string, 9)
	for index := range tooManyHosts {
		tooManyHosts[index] = "host-" + string(rune('a'+index)) + ".example.com"
	}
	cases := map[string]map[string][]string{
		"uppercase vendor key": {"Vendor A": {"gateway.example.com"}, "vendor b": {"gateway.example.com"}},
		"empty host list":      {"vendor a": {}, "vendor b": {"gateway.example.com"}},
		"duplicate hosts":      {"vendor a": {"gateway.example.com", "gateway.example.com"}, "vendor b": {"gateway.example.com"}},
		"invalid host":         {"vendor a": {"gateway.example.com/api"}, "vendor b": {"gateway.example.com"}},
		"dotted host":          {"vendor a": {"gateway..example.com"}, "vendor b": {"gateway.example.com"}},
		"empty table":          {},
		"too many vendors":     oversizedTable,
		"too many hosts":       {"vendor a": tooManyHosts, "vendor b": {"gateway.example.com"}},
	}
	for name, table := range cases {
		config := validTestConfig()
		config.Models.VendorHosts = table
		if err := config.Validate(); err == nil {
			t.Errorf("Validate() accepted vendor host table with %s", name)
		}
	}
}

func TestConfigRejectsInvalidModelBaseURL(t *testing.T) {
	// The port and dot-segment spellings are the measured bypass: a second
	// spelling of one endpoint defeats the string-compared duplicate rules.
	for _, value := range []string{"", "http://gateway.example.com/api/v1", "https://gateway.example.com/api/v1/", "https://Gateway.example.com", "https://gateway.example.com/api//v1", "https://gateway.example.com/api/v1?x=1", "https://gateway.example.com:443/api/v1", "https://gateway.example.com:/api/v1", "https://gateway.example.com/api/./v1", "https://gateway.example.com/api/../api/v1"} {
		config := validTestConfig()
		config.Models.Implementer.BaseURL = value
		if err := config.Validate(); err == nil {
			t.Errorf("Validate() accepted base url %q", value)
		}
	}
}

func TestConfigRejectsInvalidAPIKeyEnv(t *testing.T) {
	for _, value := range []string{"", "lower_case", "1STARTS_WITH_DIGIT", "HAS-HYPHEN"} {
		config := validTestConfig()
		config.Models.Implementer.APIKeyEnv = value
		if err := config.Validate(); err == nil {
			t.Errorf("Validate() accepted api key env %q", value)
		}
	}
}

func TestConfigDigestIsCanonicalAndSensitive(t *testing.T) {
	config := validTestConfig()
	first, err := config.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	second, err := config.SHA256()
	if err != nil || first != second || !sha256Pattern.MatchString(first) {
		t.Fatalf("config digests = %q, %q; error = %v", first, second, err)
	}
	config.Consumers[0].Mode.MaxChangedBytes--
	changed, err := config.SHA256()
	if err != nil || changed == first {
		t.Fatalf("changed digest = %q; error = %v", changed, err)
	}
}

func TestConfigRejectsUnsafePaths(t *testing.T) {
	config := validTestConfig()
	config.Consumers[0].Mode.AllowedFilePrefixes = []string{"../client/"}
	if err := config.Validate(); err == nil {
		t.Fatal("Validate() accepted unsafe prefix")
	}
}

func TestLoadConfigRejectsUnknownFields(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(filename, []byte(`{"schema_version":1,"unknown":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(filename); err == nil {
		t.Fatal("LoadConfig() accepted unknown field")
	}
}

func TestLoadM1ConsumerConfig(t *testing.T) {
	config, err := LoadConfig(filepath.Join("..", "..", "config", "m1-consumer.json"))
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if config.Consumers[0].Repository != "example/consumer" || config.Consumers[0].IntegrationBranch != "stg" || config.Consumers[0].ReleaseBranch != "prod" {
		t.Fatalf("consumer = %+v", config.Consumers[0])
	}
	if len(config.Models.Reviewers) != 2 || config.Models.Reviewers[0].Vendor == config.Models.Reviewers[1].Vendor {
		t.Fatalf("reviewers = %+v", config.Models.Reviewers)
	}
}

func TestValidBranch(t *testing.T) {
	for _, value := range []string{"stg", "feature/TICKET-500", "release-2026.08"} {
		if !validBranch(value) {
			t.Errorf("validBranch(%q) = false", value)
		}
	}
	for _, value := range []string{"", "/stg", "stg/", "a..b", "a//b", "branch.lock", "branch."} {
		if validBranch(value) {
			t.Errorf("validBranch(%q) = true", value)
		}
	}
}

// testConsumerGitHubContract is a complete, internally consistent destination
// contract for fixtures. Values are invented; only their shape matters.
func testConsumerGitHubContract() ConsumerGitHubContract {
	return ConsumerGitHubContract{
		DefaultBranch: "prod",
		MergeSettings: ConsumerMergeSettings{
			AllowMergeCommit:         true,
			SquashMergeCommitTitle:   "COMMIT_OR_PR_TITLE",
			SquashMergeCommitMessage: "COMMIT_MESSAGES",
			MergeCommitTitle:         "MERGE_MESSAGE",
			MergeCommitMessage:       "PR_TITLE",
		},
		StagingWorkflow: ConsumerWorkflow{ID: 101101, Name: "Deploy (stg)", Path: ".github/workflows/deploy-stg.yml"},
		ProductionWorkflows: []ConsumerWorkflow{
			{ID: 101102, Name: "Deploy", Path: ".github/workflows/deploy.yml"},
		},
	}
}
