package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// consumerWithDeployScope rewrites the pipeline's consumer config with the
// given staging workflow and production workflows (JSON), keeping the two
// reviewers the delivery needs.
func consumerWithDeployScope(t *testing.T, pipeline *Pipeline, staging, production string) {
	t.Helper()
	content := `{"models":{"reviewers":[{"id":"lassdas-review-a"},{"id":"lassdas-review-b"}]},` +
		`"consumers":[{"repository":"example/one","staging_origin":"https://one.example.invalid",` +
		`"github_contract":{"staging_workflow":` + staging + `,"production_workflows":` + production + `}}]}`
	if err := os.WriteFile(pipeline.Config.ConsumerConfigPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// absentController records every verb and answers the named wait verb with
// the destination-created-no-run ending; every other verb writes {} to
// --out and succeeds.
func absentController(t *testing.T, pipeline *Pipeline, verb, stderr string) string {
	t.Helper()
	record := filepath.Join(t.TempDir(), "argv.txt")
	script := filepath.Join(t.TempDir(), "controller.sh")
	body := "#!/bin/sh\n" +
		"echo \"$@\" >> " + record + "\n" +
		"verb=\"$1\"\n" +
		"out=\"\"; prev=\"\"\nfor a in \"$@\"; do [ \"$prev\" = \"--out\" ] && out=\"$a\"; prev=\"$a\"; done\n" +
		"if [ \"$verb\" = \"" + verb + "\" ]; then\n" +
		"  printf '%s\\n' \"" + stderr + "\" >&2\n  exit 1\nfi\n" +
		"[ -n \"$out\" ] && echo '{}' > \"$out\"\nexit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	pipeline.Config.ControllerBin = script
	return record
}

func controllerVerbsCalled(t *testing.T, record string) string {
	t.Helper()
	raw, err := os.ReadFile(record)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(raw)
}

// A docs-only change against a deployment that declares it reacts to src/
// only is not a deployment that never started: nothing was going to start.
// The runner says so before waiting, and the delivery ends without an
// operator (three such deliveries sat ten hours each in the attention
// column, 2026-09-10). Everything the declaration does not settle — no
// declaration, one path inside — waits exactly as before.
func TestAChangeOutsideTheDeclaredStagingScopeCompletesWithoutWaiting(t *testing.T) {
	const declared = `{"path":".github/workflows/stg.yml","deploy_paths":["src/","app/"]}`
	for _, tc := range []struct {
		name        string
		staging     string
		paths       string
		want        string
		awaitCalled bool
	}{
		{"docs only, scope declared", declared, `["docs/README.md","docs/guide.md"]`, "deploy_not_applicable", false},
		{"one path inside the scope", declared, `["docs/README.md","src/main.go"]`, "deploy_absent", true},
		{"no scope declared", `{"path":".github/workflows/stg.yml"}`, `["docs/README.md"]`, "deploy_absent", true},
		{"a glob covers the path", `{"path":"s.yml","deploy_paths":["*.go"]}`, `["main.go"]`, "deploy_absent", true},
		{"a prefix does not cover a sibling directory", `{"path":"s.yml","deploy_paths":["docs"]}`, `["docs2/x.md"]`, "deploy_not_applicable", false},
		{"no path list at all", declared, `[]`, "deploy_absent", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pipeline := deliverPipeline(t)
			consumerWithDeployScope(t, pipeline, tc.staging, `[]`)
			sealRounds(t, pipeline, 1)
			featurePR := `{"binding":{"repository":"example/one","product_paths":` + tc.paths + `},"payload":{"pull_request":{"Number":41}}}`
			if err := os.WriteFile(pipeline.path("feature-pr.json"), []byte(featurePR), 0o600); err != nil {
				t.Fatal(err)
			}
			record := absentController(t, pipeline, "await-staging", "controller: staging_deployment_absent")
			if err := pipeline.RunDeliver(context.Background(), DeliverUntilStaging); err != nil {
				t.Fatalf("RunDeliver() error = %v", err)
			}
			report := readSealedDeliverReport(t, pipeline, DeliverStagingReportFile)
			if report.Verdict != tc.want {
				t.Fatalf("verdict = %q, want %q", report.Verdict, tc.want)
			}
			if called := strings.Contains(controllerVerbsCalled(t, record), "await-staging"); called != tc.awaitCalled {
				t.Fatalf("await-staging called = %v, want %v", called, tc.awaitCalled)
			}
			if tc.want == "deploy_not_applicable" && !strings.Contains(report.Detail, "対象範囲") {
				t.Fatalf("the report does not say why nothing was waited for: %q", report.Detail)
			}
		})
	}
}

// The production phase keeps the same rule, with the stricter reading a
// guard workflow demands: the scope is known only when EVERY production
// workflow declared one.
func TestAChangeOutsideEveryProductionScopeCompletesWithoutWaiting(t *testing.T) {
	for _, tc := range []struct {
		name        string
		production  string
		want        string
		awaitCalled bool
	}{
		{"every production workflow declares a scope", `[{"path":"p1.yml","deploy_paths":["src/"]},{"path":"p2.yml","deploy_paths":["app/"]}]`, "deploy_not_applicable", false},
		{"one production workflow declares nothing", `[{"path":"p1.yml","deploy_paths":["src/"]},{"path":"p2.yml"}]`, "deploy_absent", true},
		{"no production workflow at all", `[]`, "deploy_absent", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pipeline := deliverPipeline(t)
			consumerWithDeployScope(t, pipeline, `{"path":"s.yml","deploy_paths":["src/","app/","docs/"]}`, tc.production)
			sealRounds(t, pipeline, 1)
			for name, content := range map[string]string{
				"feature-pr.json":         `{"binding":{"repository":"example/one","product_paths":["docs/README.md"]},"payload":{"pull_request":{"Number":41}}}`,
				DeliverStagingReportFile:  `{"phase":"staging","verdict":"pass"}`,
				DeliverStagingProofFile:   `{"payload":{}}`,
				DeliverStagingVisibleFile: `{}`,
				DeliverPromotionFile:      `{"payload":{"pull_request":{"Number":52,"HTMLURL":"https://example.invalid/pr/52"}}}`,
				DeliverPromotionMergeFile: `{}`,
				DeliverReflectionFile:     `{}`,
			} {
				if err := os.WriteFile(pipeline.path(name), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			record := absentController(t, pipeline, "await-production", "controller: production_deployment_absent")
			if err := pipeline.RunDeliver(context.Background(), DeliverUntilProduction); err != nil {
				t.Fatalf("RunDeliver() error = %v", err)
			}
			report := readSealedDeliverReport(t, pipeline, DeliverProductionReportFile)
			if report.Verdict != tc.want {
				t.Fatalf("verdict = %q, want %q", report.Verdict, tc.want)
			}
			if called := strings.Contains(controllerVerbsCalled(t, record), "await-production"); called != tc.awaitCalled {
				t.Fatalf("await-production called = %v, want %v", called, tc.awaitCalled)
			}
			if tc.want == "deploy_not_applicable" && report.PullRequestURL != "https://example.invalid/pr/52" {
				t.Fatalf("the report lost the promotion PR: %q", report.PullRequestURL)
			}
		})
	}
}

// The promotion carries the CI digest files the staging deployment
// committed, not the product paths alone. A production scope that names
// the manifests those digests live in is reacting to exactly that commit,
// so the runner must wait for the production run — and must not, once the
// digest policy is gone.
func TestAProductionScopeSeesTheDigestFilesThePromotionCarries(t *testing.T) {
	for _, tc := range []struct {
		name        string
		digest      string
		want        string
		awaitCalled bool
	}{
		{"digest files inside the production scope", `,"staging_digest_commit":{"exact_message_prefix":"ci: digest","exact_paths":["deploy/prod/kustomization.yaml"],"actor_login":"ci-bot"}`, "deploy_absent", true},
		{"no digest policy", ``, "deploy_not_applicable", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pipeline := deliverPipeline(t)
			content := `{"models":{"reviewers":[{"id":"lassdas-review-a"},{"id":"lassdas-review-b"}]},` +
				`"consumers":[{"repository":"example/one","staging_origin":"https://one.example.invalid",` +
				`"github_contract":{"staging_workflow":{"path":"s.yml","deploy_paths":["src/"]},` +
				`"production_workflows":[{"path":"p.yml","deploy_paths":["deploy/prod/"]}]` + tc.digest + `}}]}`
			if err := os.WriteFile(pipeline.Config.ConsumerConfigPath, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			sealRounds(t, pipeline, 1)
			for name, content := range map[string]string{
				"feature-pr.json":         `{"binding":{"repository":"example/one","product_paths":["src/main.go"]},"payload":{"pull_request":{"Number":41}}}`,
				DeliverStagingReportFile:  `{"phase":"staging","verdict":"pass"}`,
				DeliverStagingProofFile:   `{"payload":{}}`,
				DeliverStagingVisibleFile: `{}`,
				DeliverPromotionFile:      `{"payload":{"pull_request":{"Number":52}}}`,
				DeliverPromotionMergeFile: `{}`,
				DeliverReflectionFile:     `{}`,
			} {
				if err := os.WriteFile(pipeline.path(name), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			record := absentController(t, pipeline, "await-production", "controller: production_deployment_absent")
			if err := pipeline.RunDeliver(context.Background(), DeliverUntilProduction); err != nil {
				t.Fatalf("RunDeliver() error = %v", err)
			}
			report := readSealedDeliverReport(t, pipeline, DeliverProductionReportFile)
			if report.Verdict != tc.want {
				t.Fatalf("verdict = %q, want %q", report.Verdict, tc.want)
			}
			if called := strings.Contains(controllerVerbsCalled(t, record), "await-production"); called != tc.awaitCalled {
				t.Fatalf("await-production called = %v, want %v", called, tc.awaitCalled)
			}
		})
	}
}

// A consumer config the scope reader cannot use is not a declaration. The
// runner waits, as it did before the field existed, instead of ending the
// card with an error nothing on the ticket explains.
func TestAnUnreadableConsumerConfigIsNotADeclaration(t *testing.T) {
	pipeline := deliverPipeline(t)
	if err := os.WriteFile(pipeline.path("feature-pr.json"), []byte(`{"binding":{"repository":"example/one","product_paths":["docs/x.md"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, config := range map[string]string{"missing": filepath.Join(t.TempDir(), "none.json"), "not json": pipeline.path("feature-pr.json")} {
		pipeline.Config.ConsumerConfigPath = config
		skip, _, err := pipeline.deployNotApplicable("staging")
		if err != nil || skip {
			t.Fatalf("%s config: skip = %v err = %v, want wait with no error", name, skip, err)
		}
	}
}

// The digest-commit policy is declared under github_contract — that is
// where the config type puts it and where the shipped config writes it.
// Read from anywhere else, the promotion sees every digest file as a
// foreign change.
func TestConsumerDigestPathsReadTheContractLevelPolicy(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "m1-consumer.json"))
	if err != nil {
		t.Skipf("shipped config unavailable: %v", err)
	}
	var shipped struct {
		Consumers []struct {
			Repository string `json:"repository"`
			GitHub     struct {
				StagingDigestCommit struct {
					ExactPaths []string `json:"exact_paths"`
				} `json:"staging_digest_commit"`
			} `json:"github_contract"`
		} `json:"consumers"`
	}
	if err := json.Unmarshal(raw, &shipped); err != nil || len(shipped.Consumers) == 0 {
		t.Fatalf("shipped config unreadable: %v", err)
	}
	want := shipped.Consumers[0].GitHub.StagingDigestCommit.ExactPaths
	if len(want) == 0 {
		t.Skip("the shipped config declares no digest policy")
	}
	pipeline := deliverPipeline(t)
	pipeline.Config.ConsumerConfigPath = filepath.Join("..", "..", "config", "m1-consumer.json")
	if err := os.WriteFile(pipeline.path("feature-pr.json"), []byte(`{"binding":{"repository":"`+shipped.Consumers[0].Repository+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got := pipeline.consumerDigestPaths()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("consumerDigestPaths() = %q, want %q", got, want)
	}
}

func TestDeployPathCoveredReadsPrefixesAndGlobs(t *testing.T) {
	for _, tc := range []struct {
		patterns []string
		path     string
		want     bool
	}{
		{[]string{"docs/"}, "docs/README.md", true},
		{[]string{"docs"}, "docs/README.md", true},
		{[]string{"docs"}, "docs", true},
		{[]string{"docs/"}, "docs2/x.md", false},
		{[]string{"src/", "app/"}, "README.md", false},
		{[]string{"*.go"}, "main.go", true},
		{[]string{"*.go"}, "cmd/main.go", false},
		{[]string{"cmd/*/main.go"}, "cmd/x/main.go", true},
		// "**" spans segments, as the workflow's own paths filter reads it.
		{[]string{"docs/**"}, "docs/guide/intro.md", true},
		{[]string{"docs/**"}, "docs/README.md", true},
		{[]string{"docs/**"}, "docs2/x.md", false},
		{[]string{"**"}, "src/pkg/main.go", true},
		{[]string{"**/*.go"}, "cmd/app/main.go", true},
		{[]string{"**/*.go"}, "main.go", true},
		{[]string{"src/**/*.go"}, "src/a/b/c.go", true},
		{[]string{"src/**/*.go"}, "src/c.go", true},
		{[]string{"src/**/*.go"}, "src/a/b/c.md", false},
		{[]string{"docs/*.md"}, "docs/a/b.md", false},
		{[]string{""}, "anything", false},
		{nil, "anything", false},
	} {
		if got := deployPathCovered(tc.patterns, tc.path); got != tc.want {
			t.Errorf("deployPathCovered(%q, %q) = %v, want %v", tc.patterns, tc.path, got, tc.want)
		}
	}
}
