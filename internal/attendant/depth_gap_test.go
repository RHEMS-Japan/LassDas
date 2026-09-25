package attendant

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
	"automation.internal/ticket-ingress/internal/worker"
)

const gapRepository = "example/consumer"

// releasePathFixture is one destination and one delivery, as far as the gap
// detection reads them: a destination configuration, a run directory with
// the draft that names the destination, and the instance's own cards.
func releasePathFixture(t *testing.T, consumer string, cardsOn bool) (runtime.Config, state.RunOverview, string) {
	t.Helper()
	root := t.TempDir()
	consumerPath := filepath.Join(root, "consumer.json")
	if err := os.WriteFile(consumerPath, []byte(`{"max_stages":3,"consumers":[`+consumer+`]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(root, "runs", "d1")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "ticket-draft.json"),
		[]byte(`{"repository":"`+gapRepository+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	config := runtime.Config{ConsumerConfigPath: consumerPath,
		Chain: runtime.ChainConfig{RunsRoot: filepath.Join(root, "runs")}}
	if cardsOn {
		config.Chain.Deliver = runtime.DeliverConfig{ChecksProfile: "checks",
			IntegrateProfile: "integrate", PromoteProfile: "promote", EnabledAfter: "2026-09-01T00:00:00Z"}
	}
	run := state.RunOverview{RunID: "TICKET-1",
		ClaimedAt: time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC).UnixMilli()}
	return config, run, runDir
}

// completeReleasePath is a destination that can already be delivered to
// production: both environments answer somewhere, both deploy workflows are
// named, and the observation has a language to ask the screen in.
const completeReleasePath = `{"repository":"` + gapRepository + `","delivery":"production",` +
	`"staging_origin":"https://staging.example.test","production_origin":"https://www.example.test",` +
	`"staging_workflow":"deploy-staging.yml","production_workflow":"deploy-production.yml",` +
	`"production_login_url":"https://www.example.test/login","observation_language":"ja",` +
	`"github_contract":{"staging_workflow":{"path":"ops/deploy-staging.yml"},` +
	`"production_workflows":[{"path":"ops/deploy-production.yml"}],` +
	`"staging_digest_commit":{"exact_message_prefix":"deploy: "}},` +
	`"mode":{"allowed_file_prefixes":["ops/","src/"]}}`

// A destination that asks for production and has no way to get there does
// not stop at the proposal with a line asking somebody to configure one.
// The part of the path that lives in its own repository becomes work for
// the round that is about to run.
func TestAnUnconfiguredReleasePathBecomesWorkForTheRound(t *testing.T) {
	config, run, runDir := releasePathFixture(t,
		`{"repository":"`+gapRepository+`","delivery":"production","mode":{"allowed_file_prefixes":["src/"]}}`, false)

	plan, err := detectReleasePathGap(config, run, runDir)
	if err != nil {
		t.Fatalf("detectReleasePathGap: %v", err)
	}
	if plan.Empty() {
		t.Fatal("a destination with no release path got no plan")
	}
	built := map[string]bool{}
	for _, item := range plan.Buildable() {
		built[item.Name] = true
	}
	if !built[releasePathBuildDeployment] || !built[releasePathBuildObservation] {
		t.Fatalf("the round was not given the path to build: %v", plan.Items)
	}
	if plan.Instruction == "" {
		t.Fatal("the plan carries no instruction for the round")
	}
	for _, want := range []string{releasePathBuildDeployment, releasePathBuildObservation, "production"} {
		if !strings.Contains(plan.Instruction, want) {
			t.Fatalf("the instruction does not carry %q: %s", want, plan.Instruction)
		}
	}
}

// A destination whose path is complete is left exactly as it was: no plan,
// no extra work, nothing added to the round's instruction.
func TestACompleteReleasePathLeavesTheRoundUnchanged(t *testing.T) {
	config, run, runDir := releasePathFixture(t, completeReleasePath, true)
	// The workflows the settings name are in the destination's own tree,
	// which is the only thing that can say a path actually works.
	for _, name := range []string{"ops/deploy-staging.yml", "ops/deploy-production.yml"} {
		path := filepath.Join(runDir, "target-repo", filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("on: push\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := detectReleasePathGap(config, run, runDir)
	if err != nil {
		t.Fatalf("detectReleasePathGap: %v", err)
	}
	if !plan.Empty() || plan.Instruction != "" {
		t.Fatalf("a complete path produced work: %v / %s", plan.Items, plan.Instruction)
	}
}

// A destination that stops at the proposal has no path past it to be
// missing, and is never told to build one.
func TestAProposalOnlyDestinationIsNeverToldToBuildAPath(t *testing.T) {
	config, run, runDir := releasePathFixture(t,
		`{"repository":"`+gapRepository+`","delivery":"pull_request"}`, false)
	plan, err := detectReleasePathGap(config, run, runDir)
	if err != nil {
		t.Fatalf("detectReleasePathGap: %v", err)
	}
	if !plan.Empty() {
		t.Fatalf("a proposal-only destination was given a path to build: %v", plan.Items)
	}
}

// What the engine was not handed the means to apply is named, by the name
// an operator knows it by — and it is named nowhere the implementing round
// can see it, because a round cannot do anything about the instance's own
// settings and an instruction that carried them would turn the round into a
// report about configuration.
func TestUnappliedPartsAreNamedAndNeverAskedFor(t *testing.T) {
	config, run, runDir := releasePathFixture(t,
		`{"repository":"`+gapRepository+`","delivery":"production","staging_workflow":"deploy-staging.yml",`+
			`"production_workflow":"deploy-production.yml","mode":{"allowed_file_prefixes":["src/"]}}`, false)

	plan, err := detectReleasePathGap(config, run, runDir)
	if err != nil {
		t.Fatalf("detectReleasePathGap: %v", err)
	}
	names := releasePathUnapplied(plan)
	for _, want := range []string{
		".github/workflows/deploy-staging.yml", ".github/workflows/deploy-production.yml",
		"chain.deliver.checks_profile", "chain.deliver.integrate_profile", "chain.deliver.promote_profile",
		"production_origin", "github_contract.staging_digest_commit",
	} {
		if !containsName(names, want) {
			t.Fatalf("%q is not reported as unapplied: %v", want, names)
		}
	}
	for _, name := range names {
		if strings.Contains(plan.Instruction, name) {
			t.Fatalf("the round's instruction carries an unapplied part (%q): %s", name, plan.Instruction)
		}
	}
	// The instruction is work, never a request. Nothing in it asks anyone
	// to set, configure or hand over anything.
	for _, forbidden := range []string{"設定してください", "用意してもらって", "運用担当者に"} {
		if strings.Contains(plan.Instruction, forbidden) {
			t.Fatalf("the instruction asks a person for something (%q): %s", forbidden, plan.Instruction)
		}
	}
}

// The two reasons a workflow cannot be written are told apart, because the
// report names them to whoever can act on them: one is a limit of the
// engine everywhere, the other is one line of this destination's own
// configuration.
func TestAWorkflowOutOfReachSaysWhichLimitItIs(t *testing.T) {
	if means := workflowMeans(".github/workflows/deploy.yml", []string{"ops/"}); !strings.Contains(means, "ドット") {
		t.Fatalf("a dotted directory was explained as %q", means)
	}
	if means := workflowMeans("ops/deploy.yml", []string{"src/"}); !strings.Contains(means, "allowed_file_prefixes") {
		t.Fatalf("a path outside the writable scope was explained as %q", means)
	}
	if means := workflowMeans("ops/deploy.yml", []string{"ops/"}); means != "" {
		t.Fatalf("a path inside the writable scope was refused: %q", means)
	}
}

// The plan survives a round trip through the volume, and a record that is
// not ours reads as no plan rather than as an empty one.
func TestAReleasePathPlanIsReadBackOnlyWhenItIsOurs(t *testing.T) {
	config, run, runDir := releasePathFixture(t,
		`{"repository":"`+gapRepository+`","delivery":"production"}`, false)
	plan, err := detectReleasePathGap(config, run, runDir)
	if err != nil {
		t.Fatalf("detectReleasePathGap: %v", err)
	}
	sealReleasePathPlan(runDir, plan, &recordingLogger{})
	read, ok := readReleasePathPlan(runDir)
	if !ok || len(read.Items) != len(plan.Items) || read.Instruction != plan.Instruction {
		t.Fatalf("the plan did not read back: %v / %v", ok, read)
	}
	if err := os.WriteFile(releasePathPlanFile(runDir), []byte(`{"schema_version":1,"repository":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := readReleasePathPlan(runDir); ok {
		t.Fatal("a record with no digest read back as ours")
	}
}

// A destination configuration edited while a delivery was in flight used to
// leave that delivery failing its cards on "not bound to this run", every
// minute, for ever. It starts again instead — and one already past its pull
// request does not, because from there the delivery cards re-verify under
// the digest the pull request recorded.
func TestAConfigurationChangedUnderADeliveryStartsItAgain(t *testing.T) {
	root := t.TempDir()
	live, err := worker.LoadConfig("../../config/m1-consumer.json")
	if err != nil {
		t.Fatalf("the shipped destination configuration did not load: %v", err)
	}
	digest, err := live.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	config := runtime.Config{ConsumerConfigPath: "../../config/m1-consumer.json"}
	runDir := filepath.Join(root, "run")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	seal := func(sealed string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(runDir, "ticket-draft.json"),
			[]byte(`{"repository":"`+gapRepository+`","config_sha256":"`+sealed+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	seal(digest)
	if settingsChangedUnderRun(config, runDir) {
		t.Fatal("an unchanged configuration was read as changed")
	}
	seal(strings.Repeat("b", 64))
	if !settingsChangedUnderRun(config, runDir) {
		t.Fatal("a changed configuration was not noticed")
	}
	if err := os.WriteFile(filepath.Join(runDir, "feature-pr.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if settingsChangedUnderRun(config, runDir) {
		t.Fatal("a delivery past its pull request was restarted over a configuration change")
	}
}

func containsName(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}

// writeTree puts files into the destination's checked-out tree, which is
// what the promotion reads to see whether the path it is about to use is
// actually there.
func (h *depthHarness) writeTree(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		path := filepath.Join(h.runDir, "target-repo", filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("on: push\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// The path is checked before anything is promoted on it, and the check is
// read from the destination's own tree and records rather than from what
// the engine believes it built.
//
// Where the engine built the path, this is the first thing that ever asks
// whether what it built works. A piece missing does not fail the delivery —
// the change is merged, deployed and seen on staging — it stops the
// promotion, and the ticket is told which piece, instead of a promote card
// waiting hours for a deployment that was never going to start.
func TestTheBuiltPathIsCheckedBeforeProduction(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		tree     []string
		proof    bool
		promotes bool
		says     string
	}{
		{
			name:     "the built path reaches production",
			tree:     []string{".github/workflows/deploy-staging.yml", ".github/workflows/deploy-production.yml"},
			proof:    true,
			promotes: true,
		},
		{
			// One half of the path built and not the other.
			name:  "the workflow production needs is not in the repository",
			tree:  []string{".github/workflows/deploy-staging.yml"},
			proof: true,
			says:  "deploy-production.yml",
		},
		{
			// A staging pass with no deployment behind it verified
			// nothing, and promoting on it would carry an unverified path
			// into production.
			name:  "nothing recorded that staging was actually deployed",
			tree:  []string{".github/workflows/deploy-staging.yml", ".github/workflows/deploy-production.yml"},
			proof: false,
			says:  "デプロイが実際に動いた記録",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			h := newDepthHarness(t, "production", true, "")
			h.writeTree(t, testCase.tree...)
			h.setBoard(h.card(deliverStageChecks, "done", 1), h.card(deliverStageIntegrate, "done", 1))
			h.write(runner.DeliverChecksFile, `{"ok":true}`)
			plan := worker.ReleasePathPlan{SchemaVersion: worker.ReleasePathSchemaVersion,
				Repository: depthRepository, Configured: "production"}
			sealReleasePathPlan(h.runDir, plan, h.logger)
			h.sealPhase(runner.DeliverStagingReportFile, h.stagingPass())
			if !testCase.proof {
				if err := os.Remove(filepath.Join(h.runDir, runner.DeliverStagingProofFile)); err != nil {
					t.Fatal(err)
				}
			}
			h.tick() // posts the staging report
			h.tick() // promotes, or does not

			if promoted := strings.Contains(h.calls(), "deliver:promote"); promoted != testCase.promotes {
				t.Fatalf("promoted = %v, want %v (log: %v)", promoted, testCase.promotes, h.logger.lines)
			}
			if !testCase.promotes {
				row := h.runRow()
				if row.State != "terminal" || row.TerminalCode != string(hook.TerminalSuccess) {
					t.Fatalf("run = %s / %s (log: %v)", row.State, row.TerminalCode, h.logger.lines)
				}
				reached, _, shortfall := deliveryOutcome(h.runDir, depthRepository, depthPlanFor(t, h.runDir),
					map[string]string{"pull_request_url": "https://github.com/example/consumer/pull/9"})
				if reached != "integration" {
					t.Fatalf("reached = %q, want integration", reached)
				}
				if !strings.Contains(shortfall, testCase.says) {
					t.Fatalf("the shortfall does not say %q: %q", testCase.says, shortfall)
				}
				return
			}
			h.setBoard(h.card(deliverStageChecks, "done", 1), h.card(deliverStageIntegrate, "done", 1),
				h.card(deliverStagePromote, "done", 1))
			h.sealPhase(runner.DeliverProductionReportFile, h.productionPass())
			h.tick() // posts the release report
			h.tick() // reports the success
			reached, evidence, shortfall := deliveryOutcome(h.runDir, depthRepository, depthPlanFor(t, h.runDir),
				map[string]string{"pull_request_url": "https://github.com/example/consumer/pull/9"})
			if reached != "production" || shortfall != "" {
				t.Fatalf("reached = %q shortfall = %q (log: %v)", reached, shortfall, h.logger.lines)
			}
			if evidence["production_evidence_url"] == "" {
				t.Fatalf("the success carries no production evidence: %v", evidence)
			}
		})
	}
}
