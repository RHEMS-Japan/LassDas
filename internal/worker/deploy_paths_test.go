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

// The entries are read the way a workflow's paths filter reads them (the
// cheat-sheet rows), plus the two documented departures.
func TestDeployPathsAreReadLikeAWorkflowPathsFilter(t *testing.T) {
	for _, tc := range []struct {
		patterns []string
		path     string
		want     bool
	}{
		{[]string{"docs/"}, "docs/README.md", true},
		{[]string{"docs"}, "docs/a/b.md", true},
		{[]string{"docs"}, "docs", true},
		{[]string{"docs/"}, "docs", false},
		{[]string{"docs/"}, "docs2/x.md", false},
		{[]string{"*"}, "README.md", true},
		{[]string{"*"}, "docs/README.md", false},
		{[]string{"**"}, "docs/a/b.md", true},
		{[]string{"**.js"}, "index.js", true},
		{[]string{"**.js"}, "js/index.js", true},
		{[]string{"**.js"}, "src/js/app.js", true},
		{[]string{"docs/**.md"}, "docs/a/b.md", true},
		{[]string{"docs/**"}, "docs/guide/intro.md", true},
		{[]string{"docs/**"}, "docs2/x.md", false},
		{[]string{"**/*.go"}, "cmd/app/main.go", true},
		{[]string{"**/*.go"}, "main.go", true},
		{[]string{"src/**/*.go"}, "src/a/b/c.go", true},
		{[]string{"src/**/*.go"}, "src/c.go", true},
		{[]string{"src/**/*.go"}, "src/a/b/c.md", false},
		{[]string{"**/migrate-*.sql"}, "migrate-10.sql", true},
		{[]string{"**/migrate-*.sql"}, "db/sept/migrate-v1.sql", true},
		{[]string{"docs/*.md"}, "docs/a/b.md", false},
		{[]string{"src/**/"}, "src/main.go", true},
		{[]string{"src/**/"}, "src", false},
		{[]string{"src/*/"}, "src/x/y.go", true},
		{[]string{"src/*/"}, "src/x", false},
		{[]string{"cmd/*/main.go"}, "cmd/x/main.go", true},
		{[]string{"a/**/b"}, "a/b", true},
		{[]string{"a/**/b"}, "a/x/y/b", true},
		{[]string{"a/**/b"}, "a/xb", false},
		{[]string{"docs/[ab].md"}, "docs/a.md", true},
		{[]string{"docs/[!ab].md"}, "docs/a.md", false},
		{[]string{"*.jsx?"}, "page.jsx1", true}, // "?" is exactly one character here
		{[]string{"*.jsx?"}, "page.jsx", false}, // (GitHub reads it as optional)
		{[]string{"*.jsx?"}, "page.js", false},
		{[]string{"Docs/"}, "docs/a.md", false},
		{[]string{""}, "anything", false},
		{nil, "anything", false},
	} {
		if got := DeployPathCovered(tc.patterns, tc.path); got != tc.want {
			t.Errorf("DeployPathCovered(%q, %q) = %v, want %v", tc.patterns, tc.path, got, tc.want)
		}
	}
}

func TestDeployPathsRefuseWhatCannotMatchADeliveredPath(t *testing.T) {
	for _, bad := range [][]string{{""}, {" docs/"}, {"docs/ "}, {"/docs"}, {"../docs"}, {"docs/../src"}, {"."}, {".."}, {"./docs"}, {"["},
		{"!docs/"}, {"docs//x"}, {"docs/./x"}, {"docs/."}, {"docs/\\*"}, {"docs/[/x"}, {"docs/[a/b]"}, {"docs/[]"},
		// Characters no delivered path can carry: accepted, such an entry
		// would be a declared scope that covers nothing (review, round 3).
		{"a b"}, {"a\tb"}, {"src/+x"}, {"a(b)"}, {"a$b"}, {"a|b"}, {"a{2}"}, {"ドキュメント/*.md"}, {"a^b"}, {strings.Repeat("a", 513)},
		// A class that no delivered byte satisfies (review, round 4).
		{"[*]"}, {"[?]"}, {"docs/[*?]"}, {"[?-?]"}, {"[!A-Za-z0-9._-]"}, {"docs/[^A-Za-z0-9._-]*"}} {
		if err := (ConsumerWorkflow{DeployPaths: bad}).validateDeployPaths(); err == nil {
			t.Errorf("deploy_paths %q accepted", bad)
		}
	}
	for _, good := range [][]string{{"docs/"}, {"src", "app/"}, {"*.go"}, {"cmd/*/main.go"}, {"docs/**"}, {"**/*.go"}, {"**"}, {"**.js"}, {"src/**/"}, {"docs/[ab].md"}, {"[!ab]*"}, {"[a-z]*"}, {"[.]*"}, {"[-a]*"}, nil} {
		if err := (ConsumerWorkflow{DeployPaths: good}).validateDeployPaths(); err != nil {
			t.Errorf("deploy_paths %q refused: %v", good, err)
		}
	}
}
