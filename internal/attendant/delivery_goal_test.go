package attendant

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
)

func deliveryMustStillBeOpen(t *testing.T, h *depthHarness) {
	t.Helper()
	if row := h.runRow(); row.State != "claimed" || row.TerminalCode != "" {
		t.Fatalf("an unfinished delivery was closed: state=%s code=%s; log=%v", row.State, row.TerminalCode, h.logger.lines)
	}
}

func finishProductionAfterRecovery(t *testing.T, h *depthHarness) {
	t.Helper()
	h.tick()
	if !strings.Contains(h.calls(), "deliver:checks") {
		t.Fatalf("the existing delivery did not resume its checks: %s", h.calls())
	}
	h.setBoard(h.card(deliverStageChecks, "done", 1))
	h.write(runner.DeliverChecksFile, `{"ok":true}`)
	h.tick()
	if !strings.Contains(h.calls(), "deliver:integrate") {
		t.Fatal("integration did not resume")
	}
	h.setBoard(h.card(deliverStageChecks, "done", 1), h.card(deliverStageIntegrate, "done", 1))
	h.sealPhase(runner.DeliverStagingReportFile, h.stagingPass())
	h.tick()
	h.tick()
	deliveryMustStillBeOpen(t, h)
	if !strings.Contains(h.calls(), "deliver:promote") {
		t.Fatalf("production did not resume: %s", h.calls())
	}
	finishRecoveredPromotion(t, h)
}

func finishRecoveredPromotion(t *testing.T, h *depthHarness) {
	t.Helper()
	h.setBoard(h.card(deliverStageChecks, "done", 1), h.card(deliverStageIntegrate, "done", 1), h.card(deliverStagePromote, "done", 1))
	h.sealPhase(runner.DeliverProductionReportFile, h.productionPass())
	h.tick()
	h.tick()
	if row := h.runRow(); row.State != "terminal" || row.TerminalCode != string(hook.TerminalSuccess) {
		t.Fatalf("the recovered delivery did not finish: %+v; %v", row, h.logger.lines)
	}
	if !strings.Contains(strings.Join(*h.posted, "\n"), depthProdHost+"/feature") {
		t.Fatal("the final account carries no production evidence")
	}
}

func TestDeliveryGoalSurvivesUnavailableConfiguration(t *testing.T) {
	for _, damage := range []string{"absent", "malformed"} {
		t.Run(damage, func(t *testing.T) {
			h := newDepthHarness(t, "production", true, "")
			original, err := os.ReadFile(h.config.ConsumerConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			if damage == "absent" {
				err = os.Remove(h.config.ConsumerConfigPath)
			} else {
				err = os.WriteFile(h.config.ConsumerConfigPath, []byte(`{"consumers":[`), 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			for range 3 {
				h.tick()
				deliveryMustStillBeOpen(t, h)
			}
			if strings.Contains(h.calls(), ":deliver:") || len(*h.posted) != 0 {
				t.Fatal("an unreadable goal dispatched work or reported completion")
			}
			if err := os.WriteFile(h.config.ConsumerConfigPath, original, 0o600); err != nil {
				t.Fatal(err)
			}
			finishProductionAfterRecovery(t, h)
		})
	}
}

func TestDeliveryGoalSurvivesMissingProfiles(t *testing.T) {
	h := newDepthHarness(t, "production", false, "")
	for range 3 {
		h.tick()
		deliveryMustStillBeOpen(t, h)
	}
	if strings.Contains(h.calls(), ":deliver:") {
		t.Fatal("delivery used profiles the operator had not configured")
	}
	// Simulate the operator restoring the existing release route. This is
	// not engine-written configuration and does not change the goal.
	h.config.Chain.Deliver = runtime.DeliverConfig{ChecksProfile: "checks", IntegrateProfile: "integrate", PromoteProfile: "promote", EnabledAfter: "2026-09-01T00:00:00Z"}
	finishProductionAfterRecovery(t, h)
}

func TestDeliveryGoalDoesNotShrinkDuringRecovery(t *testing.T) {
	h := newDepthHarness(t, "production", false, "")
	original, err := os.ReadFile(h.config.ConsumerConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	h.tick()
	deliveryMustStillBeOpen(t, h)
	changed := strings.Replace(string(original), `"delivery":"production"`, `"delivery":"pull_request"`, 1)
	if changed == string(original) {
		t.Fatal("the fixture did not change the destination")
	}
	if err := os.WriteFile(h.config.ConsumerConfigPath, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	h.tick()
	deliveryMustStillBeOpen(t, h)
	if got := depthPlanFor(t, h.runDir).Configured; got != "production" {
		t.Fatalf("the accepted delivery goal shrank to %q", got)
	}
	if err := os.WriteFile(h.config.ConsumerConfigPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	h.config.Chain.Deliver = runtime.DeliverConfig{ChecksProfile: "checks", IntegrateProfile: "integrate", PromoteProfile: "promote", EnabledAfter: "2026-09-01T00:00:00Z"}
	finishProductionAfterRecovery(t, h)
}

func TestDeliveryGoalBindsItsRepositoryAndKeepsItsRecord(t *testing.T) {
	h := newDepthHarness(t, "production", false, "")
	h.tick()
	file := filepath.Join(h.runDir, deliveryDepthFile)
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	h.tick()
	still, err := os.Stat(file)
	if err != nil || !os.SameFile(info, still) {
		t.Fatal("an unchanged capability rewrote the saved goal")
	}
	plan := depthPlanFor(t, h.runDir)
	plan.Repository = "example/another"
	if err := writeDepthRecord(h.runDir, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := currentDeliveryPlan(h.config, h.runRow(), h.runDir); err == nil {
		t.Fatal("a saved goal for another repository was accepted")
	}
	if got := depthPlanFor(t, h.runDir).Repository; got != plan.Repository {
		t.Fatalf("a mismatched record was overwritten: %s", got)
	}
}

func TestDeliveryGoalIsRequiredForANewSuccess(t *testing.T) {
	for _, record := range []string{"absent", "another repository"} {
		t.Run(record, func(t *testing.T) {
			h := newDepthHarness(t, "pull_request", false, "")
			if record == "another repository" {
				plan := depthPlan{SchemaVersion: depthSchemaVersion, Repository: "example/another", Configured: "pull_request", Reached: "pull_request"}
				if err := writeDepthRecord(h.runDir, plan); err != nil {
					t.Fatal(err)
				}
			}
			run := h.runRow()
			envelope, err := pendingEnvelope(h.runDir, run)
			if err != nil {
				t.Fatal(err)
			}
			if err := reportChainSuccess(context.Background(), h.config, h.services, envelope, run, h.logger); err == nil {
				t.Fatal("a new success skipped the saved goal")
			}
			deliveryMustStillBeOpen(t, h)
			if len(*h.posted) != 0 {
				t.Fatal("an unbound goal posted a success comment")
			}
		})
	}
}

func TestProductionGoalResumesAfterItsReleasePathReturns(t *testing.T) {
	h := newDepthHarness(t, "production", true, "")
	workflow := filepath.Join(h.runDir, "target-repo", ".github/workflows/deploy-production.yml")
	original, err := os.ReadFile(workflow)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(workflow); err != nil {
		t.Fatal(err)
	}
	h.setBoard(h.card(deliverStageChecks, "done", 1), h.card(deliverStageIntegrate, "done", 1))
	h.write(runner.DeliverChecksFile, `{"ok":true}`)
	h.sealPhase(runner.DeliverStagingReportFile, h.stagingPass())
	h.tick()
	h.tick()
	deliveryMustStillBeOpen(t, h)
	if strings.Contains(h.calls(), "deliver:promote") {
		t.Fatal("production was attempted without its verified path")
	}
	if err := os.WriteFile(workflow, original, 0o600); err != nil {
		t.Fatal(err)
	}
	h.tick()
	if !strings.Contains(h.calls(), "deliver:promote") {
		t.Fatalf("the restored release route was not retried: %v", h.logger.lines)
	}
	finishRecoveredPromotion(t, h)
}

func TestDeliveryGoalKeepsItsCutoff(t *testing.T) {
	h := newDepthHarness(t, "production", true, "")
	h.config.Chain.Deliver.EnabledAfter = time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	for range 2 {
		h.tick()
		deliveryMustStillBeOpen(t, h)
	}
	if strings.Contains(h.calls(), ":deliver:") || len(*h.posted) != 0 {
		t.Fatal("an older delivery bypassed the operator's cutoff")
	}
	if !strings.Contains(strings.Join(depthPlanFor(t, h.runDir).Missing, " "), "enabled_after") {
		t.Fatal("the cutoff is missing from the recovery record")
	}
}

func TestDeliveryGoalDoesNotTreatAReleaseNoticeAsProduction(t *testing.T) {
	for _, missing := range []string{"expired Go", "deployment not applicable"} {
		t.Run(missing, func(t *testing.T) {
			h := newDepthHarness(t, "production", true, "required")
			h.config.Chain.Deliver.GoWaitSeconds = 1
			h.setBoard(h.card(deliverStageChecks, "done", 1), h.card(deliverStageIntegrate, "done", 1))
			h.write(runner.DeliverChecksFile, `{"ok":true}`)
			staging := h.stagingPass()
			staging.ObservedAt = time.Now().UTC().Add(-time.Minute)
			if missing == "deployment not applicable" {
				staging.Verdict = "deploy_not_applicable"
			}
			h.sealPhase(runner.DeliverStagingReportFile, staging)
			for range 3 {
				h.tick()
				deliveryMustStillBeOpen(t, h)
			}
			if strings.Contains(h.calls(), "deliver:promote") {
				t.Fatal("an expired or inapplicable release was promoted")
			}
			if strings.Contains(strings.Join(*h.posted, "\n"), hook.TerminalMarkerPrefix(depthRunID)) {
				t.Fatal("a release notice was treated as a completed production delivery")
			}
			if missing == "expired Go" {
				if posted, err := h.services.Tick.ReleaseReportPosted(context.Background(), depthRunID); err != nil || !posted {
					t.Fatalf("the expiry notice was not exercised: %v, %v", posted, err)
				}
			}
		})
	}
}

func TestDeliveryGoalDoesNotOverwriteAnUnreadableRecord(t *testing.T) {
	for _, damage := range []string{"malformed", "oversize", "symlink", "directory", "invalid goal", "inflated capability"} {
		t.Run(damage, func(t *testing.T) {
			h := newDepthHarness(t, "production", true, "")
			file := filepath.Join(h.runDir, deliveryDepthFile)
			body := []byte(`{"schema_version":1`)
			switch damage {
			case "oversize":
				body = []byte(strings.Repeat(" ", maxDepthRecordBytes+1))
			case "invalid goal":
				body = []byte(`{"schema_version":1,"repository":"example/consumer","configured":"unknown","reached":"production"}`)
			case "inflated capability":
				body = []byte(`{"schema_version":1,"repository":"example/consumer","configured":"integration","reached":"production"}`)
			case "symlink":
				body = []byte(`{"schema_version":1,"repository":"example/consumer","configured":"production","reached":"production"}`)
				outside := filepath.Join(t.TempDir(), "untouched.json")
				if err := os.WriteFile(outside, body, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, file); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(file, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if damage != "symlink" && damage != "directory" {
				if err := os.WriteFile(file, body, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				h.tick()
				deliveryMustStillBeOpen(t, h)
			}
			if strings.Contains(h.calls(), ":deliver:") {
				t.Fatal("an unreadable saved goal authorized delivery work")
			}
			if damage != "directory" {
				got, err := os.ReadFile(file)
				if err != nil || string(got) != string(body) {
					t.Fatalf("the saved record was overwritten: %q, %v", got, err)
				}
			}
		})
	}
}

func TestDeliveryGoalCanStillBeStopped(t *testing.T) {
	for _, unavailable := range []string{"profiles", "configuration", "saved record"} {
		t.Run(unavailable, func(t *testing.T) {
			h := newDepthHarness(t, "production", unavailable != "profiles", "")
			switch unavailable {
			case "configuration":
				if err := os.Remove(h.config.ConsumerConfigPath); err != nil {
					t.Fatal(err)
				}
			case "saved record":
				if err := os.Mkdir(filepath.Join(h.runDir, deliveryDepthFile), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			*h.comments = append(*h.comments, ticketComment(stopCommentID, depthRequester, "停止"))
			h.tick()
			if row := h.runRow(); row.State != "terminal" || row.TerminalCode != string(hook.TerminalCancelled) {
				t.Fatalf("the recovery prevented a stop: %+v; %v", row, h.logger.lines)
			}
			if stopAcknowledgements(*h.posted) != 1 || strings.Contains(h.calls(), ":deliver:") {
				t.Fatal("the stop was not acknowledged without new delivery work")
			}
		})
	}
}

func TestDeliveryGoalRecordIsAtomicAndBounded(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, deliveryDepthFile)
	outside := filepath.Join(t.TempDir(), "untouched.json")
	if err := os.WriteFile(outside, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, file); err != nil {
		t.Fatal(err)
	}
	plan := depthPlan{SchemaVersion: depthSchemaVersion, Repository: depthRepository, Configured: "production", Reached: "production"}
	if err := writeDepthRecord(dir, plan); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(outside); err != nil || string(raw) != "untouched" {
		t.Fatal("the record write followed a symlink")
	}
	if info, err := os.Lstat(file); err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("the record is not a private regular file: %v, %v", info, err)
	}
	original, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	plan.Missing = []string{strings.Repeat("x", maxDepthRecordBytes)}
	if err := writeDepthRecord(dir, plan); err == nil {
		t.Fatal("an oversized goal record was accepted")
	}
	if raw, err := os.ReadFile(file); err != nil || string(raw) != string(original) {
		t.Fatal("a refused write damaged the saved goal")
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(file, 0o700); err != nil {
		t.Fatal(err)
	}
	plan.Missing = nil
	if err := writeDepthRecord(dir, plan); err == nil {
		t.Fatal("a failed replacement was reported as persisted")
	}
	if temporary, err := filepath.Glob(filepath.Join(dir, ".delivery-depth-*")); err != nil || len(temporary) != 0 {
		t.Fatalf("record write left temporary files: %v, %v", temporary, err)
	}
}

func TestDeliveryGoalPreservesLegacySuccessResubmission(t *testing.T) {
	for _, recorded := range []bool{false, true} {
		t.Run(map[bool]string{false: "before depth records", true: "short delivery already ended"}[recorded], func(t *testing.T) {
			f := newPendingFixture(t, depthRepository)
			f.writeRunDir(t, depthRepository)
			dir := runDirectory(f.config, f.deliveryID)
			outcome := runner.ChainOutcome{Stage: 1, Evidence: map[string]string{"pull_request_url": "https://github.com/example/consumer/pull/9"}}
			encoded, err := json.Marshal(outcome)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, runner.ChainOutcomeFile), encoded, 0o600); err != nil {
				t.Fatal(err)
			}
			evidence := outcome.Evidence
			if recorded {
				plan := depthPlan{SchemaVersion: depthSchemaVersion, Repository: depthRepository, Configured: "production", Reached: "pull_request", Missing: []string{"chain.deliver.checks_profile"}}
				if err := writeDepthRecord(dir, plan); err != nil {
					t.Fatal(err)
				}
				_, evidence, _ = deliveryOutcome(dir, depthRepository, plan, evidence)
			}
			envelope, err := pendingEnvelope(dir, f.run)
			if err != nil {
				t.Fatal(err)
			}
			logger := &pendingTestLogger{}
			terminal := runner.NewTerminal(f.config, f.services, envelope, chainOwnerRunID(f.deliveryID), dir, logger)
			digest, err := terminal.ReportDigest(context.Background(), hook.TerminalSuccess, runner.Outcome{Stage: 1, Evidence: evidence}, depthRepository)
			if err != nil {
				t.Fatal(err)
			}
			f.run.TerminalCode, f.run.TerminalReportSHA256, f.store.expected = string(hook.TerminalSuccess), digest, digest
			for range 2 {
				if err := reportChainSuccess(context.Background(), f.config, f.services, envelope, f.run, logger); err != nil {
					t.Fatal(err)
				}
			}
			if len(f.comments.posted) != 1 || f.store.completes != 2 {
				t.Fatalf("legacy report was not retried exactly: comments=%d completions=%d", len(f.comments.posted), f.store.completes)
			}
		})
	}
}
