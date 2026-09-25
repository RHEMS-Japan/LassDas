package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/cardsecret"
	"automation.internal/ticket-ingress/internal/hook"
	runtimecfg "automation.internal/ticket-ingress/internal/runtime"
)

// scanFixture writes a runtime configuration naming the credentials given,
// with each one's file provisioned, and points this process at it the way a
// dispatched card is pointed at it.
func scanFixture(t *testing.T, credentials ...runtimecfg.Credential) (string, map[string]string) {
	t.Helper()
	directory := t.TempDir()
	consumerPath := filepath.Join(directory, "consumer.json")
	writeTestJSON(t, consumerPath, cliTestConfig())
	contents := map[string]string{}
	for index, credential := range credentials {
		path := filepath.Join(directory, credential.Name)
		value := "value-of-" + credential.Name + "-is-long-enough"
		if err := os.WriteFile(path, []byte(value+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		credentials[index].Path = path
		contents[credential.Name] = value
	}
	config := runtimecfg.Config{
		LedgerPath: filepath.Join(directory, "ledger.db"), ConsumerConfigPath: consumerPath,
		KnowledgeRoot: filepath.Join(directory, "knowledge"), WorkerBin: "/usr/local/bin/worker", ControllerBin: "/usr/local/bin/controller",
		Identity:           runtimecfg.IdentityConfig{RepositoryID: 1, Repository: "example/engine", WorkflowRef: "example/engine/local-runtime@fixture", EngineSHA: strings.Repeat("a", 40)},
		AutomationRunID:    "run_20260908_" + strings.Repeat("a", 24),
		Tracker:            runtimecfg.TrackerConfig{Origin: "https://example.backlog.com", SpaceKey: "example", ProjectID: 100, ProjectKey: "TKT", AllowedCreatorID: 7, AllowedActivityType: 1},
		ReportDestinations: []hook.ReportDestination{{Repository: "example/consumer", Delivery: "pull_request", StagingOrigin: "https://stg.example.com", ProductionOrigin: "https://example.com"}},
		Orchestration:      "cards",
		Chain: runtimecfg.ChainConfig{
			RunsRoot: filepath.Join(directory, "runs"), TargetTokenPath: filepath.Join(directory, "target-token"),
			Profiles: runtimecfg.ChainProfiles{
				Implementer: "lassdas-implementer", ReviewA: "lassdas-review-a", ReviewB: "lassdas-review-b",
				Validate: "lassdas-validate", Publish: "lassdas-publish",
			},
			Credentials: credentials,
		},
	}
	path := filepath.Join(directory, "runtime.json")
	writeTestJSON(t, path, config)
	t.Setenv("LASSDAS_RUNTIME_CONFIG", path)
	return path, contents
}

// The card that seals a round is not the card a credential is usually
// handed to: the ordinary configuration gives one to the card that writes
// the change. A check that could only see what its own card received would
// never look at the ordinary case.
func TestTheSealChecksACredentialHandedToAnotherCard(t *testing.T) {
	cardsecret.Forget()
	t.Cleanup(cardsecret.Forget)
	_, contents := scanFixture(t, runtimecfg.Credential{
		Name: "warehouse", Env: runtimecfg.EnvNames{"DATABASE_URL"},
		// Handed to the card that writes the change, and to no other.
		Stages: []string{runtimecfg.StageImplement},
	})
	if err := registerCredentialsForScan(); err != nil {
		t.Fatalf("registerCredentialsForScan: %v", err)
	}
	if got := cardsecret.VariableIn("const dsn = \"" + contents["warehouse"] + "\""); got != "DATABASE_URL" {
		t.Fatalf("a change carrying the value was not recognised: %q", got)
	}
}

// Reading them for the comparison must widen nothing else: this process
// was handed none of them, so none reaches an agent's environment, a
// validation sandbox or a mask.
func TestReadingCredentialsForTheScanWidensNothingElse(t *testing.T) {
	cardsecret.Forget()
	t.Cleanup(cardsecret.Forget)
	_, contents := scanFixture(t, runtimecfg.Credential{
		Name: "warehouse", Env: runtimecfg.EnvNames{"DATABASE_URL"}, Stages: []string{runtimecfg.StageImplement},
	})
	if err := registerCredentialsForScan(); err != nil {
		t.Fatal(err)
	}
	if names := cardsecret.Names(); len(names) != 0 {
		t.Fatalf("the reviewer's agent environment gained %v", names)
	}
	if literals := cardsecret.Literals(); len(literals) != 0 {
		t.Fatalf("the masking changed: %v", literals)
	}
	if got := cardsecret.Redact("psql: " + contents["warehouse"]); !strings.Contains(got, contents["warehouse"]) {
		t.Fatalf("a value this card was never handed was masked out of its logs: %q", got)
	}
}

// A credential the operator provisioned and the engine cannot read leaves
// the change uncompared. Sealing it anyway is the silence the gate exists
// to end, so the seal refuses and the reason names the variable.
func TestAnUnreadableCredentialRefusesTheSeal(t *testing.T) {
	cardsecret.Forget()
	t.Cleanup(cardsecret.Forget)
	_, _ = scanFixture(t, runtimecfg.Credential{
		Name: "warehouse", Env: runtimecfg.EnvNames{"DATABASE_URL"}, Stages: []string{runtimecfg.StageImplement},
	})
	config, err := runtimecfg.Load(os.Getenv("LASSDAS_RUNTIME_CONFIG"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(config.Chain.Credentials[0].Path); err != nil {
		t.Fatal(err)
	}
	err = registerCredentialsForScan()
	if err == nil {
		t.Fatal("a change was sealed without being compared against a configured credential")
	}
	if !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("the refusal does not name the variable: %v", err)
	}
}

// A process nothing dispatched as a card has no runtime configuration and
// nothing to compare against; it seals as it always did.
func TestAVerbRunByHandHasNothingToCompareAgainst(t *testing.T) {
	cardsecret.Forget()
	t.Cleanup(cardsecret.Forget)
	t.Setenv("LASSDAS_RUNTIME_CONFIG", "")
	if err := registerCredentialsForScan(); err != nil {
		t.Fatalf("registerCredentialsForScan: %v", err)
	}
}

// The whole path, through the verb that seals: a credential handed to the
// card that wrote the change, a change carrying its value, and the seal
// refusing before anything downstream sees it.
func TestSealCandidateRefusesAChangeCarryingAHandedCredential(t *testing.T) {
	cardsecret.Forget()
	t.Cleanup(cardsecret.Forget)
	fixture := newAgentFixture(t, "true", "true")
	_, contents := scanFixture(t, runtimecfg.Credential{
		Name: "warehouse", Env: runtimecfg.EnvNames{"DATABASE_URL"},
		// Handed to the card that writes the change, never to this one.
		Stages: []string{runtimecfg.StageImplement},
	})
	target := filepath.Join(fixture.repoRoot, "client", "src", "label.ts")
	body := "export const label = 'Updated label';\nconst dsn = '" + contents["warehouse"] + "';\n"
	if err := os.WriteFile(target, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	err := fixture.sealCandidate(t)
	if err == nil {
		t.Fatal("a change carrying a handed credential was sealed")
	}
	if !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("the refusal does not name the variable: %v", err)
	}
	if strings.Contains(err.Error(), contents["warehouse"]) {
		t.Fatalf("the refusal published the value: %v", err)
	}
	if _, statErr := os.Stat(fixture.path("candidate.json")); statErr == nil {
		t.Fatal("the change was sealed anyway")
	}
}

// And the same change without the value seals as it always did.
func TestSealCandidateIsUnchangedForAChangeCarryingNothing(t *testing.T) {
	cardsecret.Forget()
	t.Cleanup(cardsecret.Forget)
	fixture := newAgentFixture(t, "true", "true")
	scanFixture(t, runtimecfg.Credential{
		Name: "warehouse", Env: runtimecfg.EnvNames{"DATABASE_URL"}, Stages: []string{runtimecfg.StageImplement},
	})
	target := filepath.Join(fixture.repoRoot, "client", "src", "label.ts")
	if err := os.WriteFile(target, []byte("export const label = 'Updated label';\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fixture.sealCandidate(t); err != nil {
		t.Fatalf("an ordinary change was refused: %v", err)
	}
}
