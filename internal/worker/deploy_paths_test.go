package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A declared scope must be able to mean what it says: an absolute path or a
// ".." segment never matches a delivered path, a padded or empty entry
// covers nothing while looking declared, and a glob the matcher rejects
// would silently cover nothing.
// A feature workflow's scope is read by nothing — its CI is waited for by
// required jobs and never skipped — so the field must be refused there
// rather than accepted as a declaration that does something.
func TestAFeatureWorkflowRefusesADeployScope(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "m1-consumer.json"))
	if err != nil {
		t.Skipf("shipped config unavailable: %v", err)
	}
	var config map[string]any
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	consumer := config["consumers"].([]any)[0].(map[string]any)
	contract := consumer["github_contract"].(map[string]any)
	features, _ := contract["feature_workflows"].([]any)
	if len(features) == 0 {
		features = []any{map[string]any{"id": 1, "name": "ci", "path": ".github/workflows/ci.yml"}}
	}
	features[0].(map[string]any)["deploy_paths"] = []any{"docs/"}
	contract["feature_workflows"] = features
	mutated, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(t.TempDir(), "consumer.json")
	if err := os.WriteFile(tmp, mutated, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(tmp); err == nil || !strings.Contains(err.Error(), "feature workflow deploy_paths") {
		t.Fatalf("LoadConfig() error = %v, want the feature workflow scope refused", err)
	}
}

func TestDeployPathsRefuseWhatCannotMatchADeliveredPath(t *testing.T) {
	for _, bad := range [][]string{{""}, {" docs/"}, {"docs/ "}, {"/docs"}, {"../docs"}, {"docs/../src"}, {"."}, {".."}, {"./docs"}, {"["},
		{"!docs/"}, {"docs//x"}, {"docs/./x"}, {"docs/."}, {"docs/\\*"}, {"docs/[/x"}} {
		if err := (ConsumerWorkflow{DeployPaths: bad}).validateDeployPaths(); err == nil {
			t.Errorf("deploy_paths %q accepted", bad)
		}
	}
	for _, good := range [][]string{{"docs/"}, {"src", "app/"}, {"*.go"}, {"cmd/*/main.go"}, {"docs/**"}, {"**/*.go"}, {"**"}, nil} {
		if err := (ConsumerWorkflow{DeployPaths: good}).validateDeployPaths(); err != nil {
			t.Errorf("deploy_paths %q refused: %v", good, err)
		}
	}
}
