package worker

import (
	"encoding/json"
	"strings"
	"testing"
)

func cliTestConfig() Config {
	c := validTestConfig()
	c.Consumers[0] = ConsumerConfig{Kind: "cli", Repository: "example/consumer", RepositoryID: 101, Delivery: DeliverPullRequest, IntegrationBranch: "develop", GitHub: ConsumerGitHubContract{DefaultBranch: "main"}, Mode: c.Consumers[0].Mode}
	return c
}

func TestCLIConsumerUsesTheSharedPRContract(t *testing.T) {
	c := cliTestConfig()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := c.Consumers[0].Contract().Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*ConsumerConfig){
		"kind":                    func(c *ConsumerConfig) { c.Kind = "native" },
		"delivery":                func(c *ConsumerConfig) { c.Delivery = DeliverIntegration },
		"release branch":          func(c *ConsumerConfig) { c.ReleaseBranch = "prod" },
		"origin":                  func(c *ConsumerConfig) { c.StagingOrigin = "https://stg.example.com" },
		"login":                   func(c *ConsumerConfig) { c.ProductionLoginURL = "https://example.com/login" },
		"language":                func(c *ConsumerConfig) { c.ObservationLanguage = "en" },
		"workflow":                func(c *ConsumerConfig) { c.StagingWorkflow = "deploy.yml" },
		"merge settings":          func(c *ConsumerConfig) { c.GitHub.MergeSettings.AllowMergeCommit = true },
		"feature workflows":       func(c *ConsumerConfig) { c.GitHub.FeatureWorkflows = []ConsumerWorkflow{{ID: 1}} },
		"staging workflow":        func(c *ConsumerConfig) { c.GitHub.StagingWorkflow.Path = ".github/workflows/deploy.yml" },
		"production workflow":     func(c *ConsumerConfig) { c.GitHub.ProductionWorkflows = []ConsumerWorkflow{{ID: 1}} },
		"digest policy":           func(c *ConsumerConfig) { c.GitHub.StagingDigestCommit = &ConsumerDigestCommit{} },
		"repository identity":     func(c *ConsumerConfig) { c.RepositoryID = 0 },
		"integration branch":      func(c *ConsumerConfig) { c.IntegrationBranch = "../develop" },
		"observed default branch": func(c *ConsumerConfig) { c.GitHub.DefaultBranch = "" },
		"mode limits":             func(c *ConsumerConfig) { c.Mode.MaxFiles = 0 },
		"verification":            func(c *ConsumerConfig) { c.Mode.VerifyCommands = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			config := cliTestConfig()
			mutate(&config.Consumers[0])
			if err := config.Validate(); err == nil {
				t.Fatal("accepted incompatible CLI config")
			}
		})
	}
}

func TestWebConsumerKindRemainsCompatible(t *testing.T) {
	config := validTestConfig()
	encoded, err := json.Marshal(config.Consumers[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"kind"`) {
		t.Fatal("absent kind changed the sealed config encoding")
	}
	for _, kind := range []string{"", "web"} {
		config.Consumers[0].Kind = kind
		if err := config.Validate(); err != nil {
			t.Fatal(err)
		}
		if config.Consumers[0].EffectiveKind() != "web" {
			t.Fatal("missing web default")
		}
	}
	config.Consumers[0].StagingOrigin = ""
	if err := config.Validate(); err == nil {
		t.Fatal("web consumer omitted a required origin")
	}
}
