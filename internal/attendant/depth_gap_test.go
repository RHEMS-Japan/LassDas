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
	if means := workflowMeans(".github/workflows/deploy.yml", []string{"ops/"}, nil); !strings.Contains(means, "ドット") {
		t.Fatalf("a dotted directory was explained as %q", means)
	}
	if means := workflowMeans("ops/deploy.yml", []string{"src/"}, nil); !strings.Contains(means, "allowed_file_prefixes") {
		t.Fatalf("a path outside the writable scope was explained as %q", means)
	}
	if means := workflowMeans("ops/deploy.yml", []string{"ops/"}, nil); means != "" {
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

// dropFromTree takes something out of the destination's checked-out tree,
// which is what the promotion reads to see whether the path it is about to
// use is actually there. The harness puts a whole tree there, because every
// live delivery has one; a test says which part of it is missing.
func (h *depthHarness) dropFromTree(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		if err := os.RemoveAll(filepath.Join(h.runDir, "target-repo", filepath.FromSlash(name))); err != nil {
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
		drop     []string
		proof    bool
		promotes bool
		says     string
	}{
		{
			name:     "the built path reaches production",
			proof:    true,
			promotes: true,
		},
		{
			// One half of the path built and not the other.
			name:  "the workflow production needs is not in the repository",
			drop:  []string{".github/workflows/deploy-production.yml"},
			proof: true,
			says:  "deploy-production.yml",
		},
		{
			// A staging pass with no deployment behind it verified
			// nothing, and promoting on it would carry an unverified path
			// into production.
			name:  "nothing recorded that staging was actually deployed",
			proof: false,
			says:  "デプロイが実際に動いた記録",
		},
		{
			// The working copy itself is gone. Nothing rebuilds it for a
			// live delivery, so the path cannot be checked at all — and
			// the promotion merges into the release branch before any
			// deployment is looked at, so proceeding would release a
			// change whose route nothing verified.
			name:  "the working copy this delivery was checked out into is gone",
			drop:  []string{"."},
			proof: true,
			says:  "作業コピーが残っていない",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			h := newDepthHarness(t, "production", true, "")
			h.dropFromTree(t, testCase.drop...)
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

// The probe the reviewer ran: a destination whose path was complete when
// the delivery was claimed gets no plan, and something about it stopped
// being true before the promotion. The reason has to be the real one.
//
// Without a plan on the volume the hold was dropped and the report fell
// through to its last sentence, which says the promotion is waiting for an
// operator to look. Nothing was waiting: the requester was sent to approve
// a promotion that would never have been attempted, while the engine's own
// log said the workflow file was missing.
func TestARefusedPromotionWithNoPlanStillSaysTheRealReason(t *testing.T) {
	h := newDepthHarness(t, "production", true, "")
	// The staging half of the path is there and production's is not, and
	// no plan is sealed: this destination looked complete at reception.
	h.dropFromTree(t, ".github/workflows/deploy-production.yml")
	h.setBoard(h.card(deliverStageChecks, "done", 1), h.card(deliverStageIntegrate, "done", 1))
	h.write(runner.DeliverChecksFile, `{"ok":true}`)
	if _, sealed := readReleasePathPlan(h.runDir); sealed {
		t.Fatal("the fixture sealed a plan")
	}
	h.sealPhase(runner.DeliverStagingReportFile, h.stagingPass())
	h.tick() // posts the staging report
	h.tick() // would promote

	if strings.Contains(h.calls(), "deliver:promote") {
		t.Fatalf("an incomplete path was promoted on: %s", h.calls())
	}
	_, _, shortfall := deliveryOutcome(h.runDir, depthRepository, depthPlanFor(t, h.runDir),
		map[string]string{"pull_request_url": "https://github.com/example/consumer/pull/9"})
	if !strings.Contains(shortfall, "deploy-production.yml") {
		t.Fatalf("the requester was given a reason that is not the one: %q", shortfall)
	}
	if strings.Contains(shortfall, "運用担当者の確認") {
		t.Fatalf("the requester was told to approve a promotion nothing was waiting on: %q", shortfall)
	}
}

// A destination edited out of the configuration mid-delivery stops the
// promotion. It is the one kind of not-knowing here that is somebody's
// decision rather than a fault, and this delivery gets no other warning:
// past its pull request it is exempt from the restart that a changed
// configuration otherwise causes.
func TestADestinationEditedOutOfTheConfigurationIsNotPromotedTo(t *testing.T) {
	h := newDepthHarness(t, "production", true, "")
	h.setBoard(h.card(deliverStageChecks, "done", 1), h.card(deliverStageIntegrate, "done", 1))
	h.write(runner.DeliverChecksFile, `{"ok":true}`)
	h.sealPhase(runner.DeliverStagingReportFile, h.stagingPass())
	h.tick() // posts the staging report
	if err := os.WriteFile(h.config.ConsumerConfigPath,
		[]byte(`{"max_stages":3,"consumers":[{"repository":"example/elsewhere","delivery":"production"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	h.tick() // would promote

	if strings.Contains(h.calls(), "deliver:promote") {
		t.Fatalf("a destination that is no longer configured was promoted to: %s", h.calls())
	}
	if reason := releasePathHold(h.runDir); !strings.Contains(reason, "設定から外れている") {
		t.Fatalf("the reason does not say the destination is gone: %q", reason)
	}
}

// A configuration that cannot be read at all is a fault rather than an
// answer, and the promotion goes ahead: every card of the delivery reads
// the same file and fails on its own terms if it is really broken.
func TestAnUnreadableConfigurationDoesNotStopThePromotion(t *testing.T) {
	h := newDepthHarness(t, "production", true, "")
	h.setBoard(h.card(deliverStageChecks, "done", 1), h.card(deliverStageIntegrate, "done", 1))
	h.write(runner.DeliverChecksFile, `{"ok":true}`)
	h.sealPhase(runner.DeliverStagingReportFile, h.stagingPass())
	h.tick() // posts the staging report
	if err := os.WriteFile(h.config.ConsumerConfigPath, []byte(`{"consumers":[`), 0o600); err != nil {
		t.Fatal(err)
	}
	h.tick() // promotes

	if !strings.Contains(h.calls(), "deliver:promote") {
		t.Fatalf("an unlucky read stopped a healthy delivery: %s (log: %v)", h.calls(), h.logger.lines)
	}
}

// The screen check reads the record the caller already parsed, not the file
// again.
//
// Read again it was a third read of one file, and an unlucky one had to
// fail open on ScreenChecked — the only field this check needs and the one
// the caller never looks at. So the single case the check exists for was
// also the one case it could silently skip. Here the file on the volume
// says the opposite of the record passed in, and the record wins both ways.
func TestTheScreenCheckReadsTheRecordItWasGiven(t *testing.T) {
	config, _, runDir := releasePathFixture(t, completeReleasePath, true)
	for _, name := range []string{"ops/deploy-staging.yml", "ops/deploy-production.yml"} {
		path := filepath.Join(runDir, "target-repo", filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("on: push\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(runDir, runner.DeliverStagingProofFile), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// The file claims no screen was promised; the caller's record says one
	// was, and no screen was sealed.
	if err := os.WriteFile(filepath.Join(runDir, runner.DeliverStagingReportFile),
		[]byte(`{"schema_version":1,"phase":"staging","verdict":"pass"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	reason, built := verifyBuiltPath(config, runDir, runner.DeliverReport{Verdict: "pass", ScreenChecked: true})
	if built || !strings.Contains(reason, "画面を確かめた記録") {
		t.Fatalf("a promised screen with nothing sealed was promoted on: %q / %v", reason, built)
	}
	// And the other way: the record says no screen was promised, so the
	// absent seal is not a shortfall.
	if _, built := verifyBuiltPath(config, runDir, runner.DeliverReport{Verdict: "pass"}); !built {
		t.Fatal("a delivery that promised no screen was refused for want of one")
	}
	// With the screen sealed, a promised screen passes.
	if err := os.WriteFile(filepath.Join(runDir, runner.DeliverStagingVisibleFile), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, built := verifyBuiltPath(config, runDir, runner.DeliverReport{Verdict: "pass", ScreenChecked: true}); !built {
		t.Fatal("a sealed screen was not accepted")
	}
}
