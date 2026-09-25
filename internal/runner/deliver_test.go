package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runtime"
)

func deliverPipeline(t *testing.T) *Pipeline {
	t.Helper()
	pipeline := &Pipeline{Workspace: t.TempDir(), Logger: baseAdvanceLogger{}}
	consumerPath := filepath.Join(t.TempDir(), "consumer.json")
	content := `{"models":{"reviewers":[{"id":"lassdas-review-a"},{"id":"lassdas-review-b"}]},` +
		`"consumers":[{"repository":"example/one","staging_origin":"https://one.example.invalid"}]}`
	if err := os.WriteFile(consumerPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	pipeline.Config.ConsumerConfigPath = consumerPath
	return pipeline
}

// standInController records every invocation and writes {} to --out.
func standInController(t *testing.T, pipeline *Pipeline, exitCode string) string {
	t.Helper()
	record := filepath.Join(t.TempDir(), "argv.txt")
	script := filepath.Join(t.TempDir(), "controller.sh")
	body := "#!/bin/sh\necho \"$@\" >> " + record + "\nout=\"\"; prev=\"\"\nfor a in \"$@\"; do [ \"$prev\" = \"--out\" ] && out=\"$a\"; prev=\"$a\"; done\n[ -n \"$out\" ] && echo '{}' > \"$out\"\nexit " + exitCode + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	pipeline.Config.ControllerBin = script
	return record
}

func TestRunDeliverValidatesItsInputs(t *testing.T) {
	pipeline := deliverPipeline(t)
	if err := pipeline.RunDeliver(context.Background(), "sideways"); err == nil {
		t.Fatal("an unknown milestone was accepted")
	}
	if err := pipeline.RunDeliver(context.Background(), DeliverUntilChecks); err == nil {
		t.Fatal("a run without a delivered pull request was accepted")
	}
	if err := os.WriteFile(pipeline.path("feature-pr.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.RunDeliver(context.Background(), DeliverUntilChecks); err == nil {
		t.Fatal("a run without a sealed round was accepted")
	}
}

func TestRunDeliverIsIdempotentOncePhaseReportsExist(t *testing.T) {
	pipeline := deliverPipeline(t)
	sealRounds(t, pipeline, 1)
	for name, content := range map[string]string{
		"feature-pr.json":           `{}`,
		DeliverStagingReportFile:    `{"phase":"staging","verdict":"pass"}`,
		DeliverProductionReportFile: `{"phase":"production","verdict":"pass"}`,
	} {
		if err := os.WriteFile(pipeline.path(name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pipeline.Config.ControllerBin = "false"
	if err := pipeline.RunDeliver(context.Background(), DeliverUntilStaging); err != nil {
		t.Fatalf("staging resume error = %v", err)
	}
	if err := pipeline.RunDeliver(context.Background(), DeliverUntilProduction); err != nil {
		t.Fatalf("production resume error = %v", err)
	}
}

// A red CI gate is a sealed RESULT, not a blocked card, and it stops the
// delivery before staging.
func TestRunDeliverSealsChecksFailureHonestly(t *testing.T) {
	pipeline := deliverPipeline(t)
	sealRounds(t, pipeline, 1)
	if err := os.WriteFile(pipeline.path("feature-pr.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	standInController(t, pipeline, "1")
	if err := pipeline.RunDeliver(context.Background(), DeliverUntilStaging); err != nil {
		t.Fatalf("RunDeliver() error = %v", err)
	}
	report := readSealedDeliverReport(t, pipeline, DeliverStagingReportFile)
	if report.Verdict != "checks_failed" || report.Phase != "staging" {
		t.Fatalf("report = %+v", report)
	}
	if pipeline.exists(DeliverMergeFile) {
		t.Fatal("a failed CI gate still merged to staging")
	}
}

// The checks milestone stops BEFORE the merge — that gap is where the
// attendant re-checks for a stop comment.
func TestRunDeliverChecksMilestoneStopsBeforeTheMerge(t *testing.T) {
	pipeline := deliverPipeline(t)
	sealRounds(t, pipeline, 1)
	if err := os.WriteFile(pipeline.path("feature-pr.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	record := standInController(t, pipeline, "0")
	if err := pipeline.RunDeliver(context.Background(), DeliverUntilChecks); err != nil {
		t.Fatalf("RunDeliver() error = %v", err)
	}
	if !pipeline.exists(DeliverChecksFile) {
		t.Fatal("the checks artifact was not written")
	}
	if pipeline.exists(DeliverMergeFile) || pipeline.exists(DeliverStagingReportFile) {
		t.Fatal("the checks milestone went past the merge boundary")
	}
	argv, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if calls := strings.Count(string(argv), "\n"); calls != 1 || !strings.Contains(string(argv), "wait-feature") {
		t.Fatalf("controller calls = %d:\n%s", calls, argv)
	}
	if !strings.Contains(string(argv), "history/stage-1/ticket.json") {
		t.Fatalf("the sealed round ticket was not chained:\n%s", argv)
	}
}

// verbController fails the named verb and answers read-merged with the
// given body — the harness for the merge-honesty branches.
func verbController(t *testing.T, pipeline *Pipeline, failVerb, readMergedBody, readMergedExit string) {
	t.Helper()
	script := filepath.Join(t.TempDir(), "controller.sh")
	body := "#!/bin/sh\n" +
		"verb=\"$1\"\n" +
		"out=\"\"; prev=\"\"\nfor a in \"$@\"; do [ \"$prev\" = \"--out\" ] && out=\"$a\"; prev=\"$a\"; done\n" +
		"if [ \"$verb\" = \"" + failVerb + "\" ]; then exit 1; fi\n" +
		"if [ \"$verb\" = \"read-merged\" ]; then\n" +
		"  [ -n \"$out\" ] && printf '%s' '" + readMergedBody + "' > \"$out\"\n" +
		"  exit " + readMergedExit + "\nfi\n" +
		"[ -n \"$out\" ] && echo '{}' > \"$out\"\nexit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	pipeline.Config.ControllerBin = script
}

// A merge verb can fail AFTER the merge landed. The sealed verdict must
// track what GitHub says, never assume "not merged".
func TestRunDeliverMergeFailureVerdictTracksTheActualMergeState(t *testing.T) {
	cases := map[string]struct {
		readMergedBody string
		readMergedExit string
		wantVerdict    string
	}{
		"landed despite the failure":     {`{"state":"closed","merged":true,"merge_commit_sha":"x"}`, "0", "deploy_failed"},
		"really not merged":              {`{"state":"open","merged":false}`, "0", "merge_failed"},
		"state unreadable":               {`{}`, "1", "merge_unverified"},
		"probe succeeded but no verdict": {`{}`, "0", "merge_unverified"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			pipeline := deliverPipeline(t)
			sealRounds(t, pipeline, 1)
			if err := os.WriteFile(pipeline.path("feature-pr.json"), []byte(`{"payload":{"pull_request":{"Number":41}}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			verbController(t, pipeline, "merge-feature", c.readMergedBody, c.readMergedExit)
			if err := pipeline.RunDeliver(context.Background(), DeliverUntilStaging); err != nil {
				t.Fatalf("RunDeliver() error = %v", err)
			}
			report := readSealedDeliverReport(t, pipeline, DeliverStagingReportFile)
			if report.Verdict != c.wantVerdict {
				t.Fatalf("verdict = %q, want %q", report.Verdict, c.wantVerdict)
			}
		})
	}
}

// The promotion-merge failure trusts the reflection artifact first: it is
// written the moment the release branch moves.
func TestRunDeliverPromotionFailureTrustsTheReflection(t *testing.T) {
	pipeline := deliverPipeline(t)
	sealRounds(t, pipeline, 1)
	for name, content := range map[string]string{
		"feature-pr.json":         `{"payload":{"pull_request":{"Number":41}}}`,
		DeliverStagingReportFile:  `{"phase":"staging","verdict":"pass"}`,
		DeliverStagingProofFile:   `{"payload":{}}`,
		DeliverStagingVisibleFile: `{}`,
		DeliverPromotionFile:      `{"payload":{"pull_request":{"Number":52}}}`,
		DeliverReflectionFile:     `{}`,
	} {
		if err := os.WriteFile(pipeline.path(name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	verbController(t, pipeline, "merge-promotion", `{}`, "1")
	if err := pipeline.RunDeliver(context.Background(), DeliverUntilProduction); err != nil {
		t.Fatalf("RunDeliver() error = %v", err)
	}
	report := readSealedDeliverReport(t, pipeline, DeliverProductionReportFile)
	if report.Verdict != "deploy_failed" {
		t.Fatalf("verdict = %q, want deploy_failed (the reflection proves the branch moved)", report.Verdict)
	}
}

// The production phase refuses to start without the sealed staging
// observation — a Go cannot promote what was never proven on staging.
func TestRunDeliverProductionNeedsTheSealedStagingObservation(t *testing.T) {
	pipeline := deliverPipeline(t)
	sealRounds(t, pipeline, 1)
	if err := os.WriteFile(pipeline.path("feature-pr.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	pipeline.Config.ControllerBin = "false"
	if err := pipeline.RunDeliver(context.Background(), DeliverUntilProduction); err == nil {
		t.Fatal("production ran without the sealed staging observation")
	}
}

// The promotion gate moves exactly one delivery, so a Go must only be
// requested when the release→integration delta is this delivery's own
// files (plus the CI digest files). Everything else is an honest hold.
// A ticket without a visible-wording promise cannot pass or fail a screen
// check, and the promotion gates need the pass evidence this path cannot
// produce. The report must say exactly that: a deploy-verified pass, no
// verdict theater, and an honest promotion hold.
func TestReferenceStagingReportPassesWithAnHonestHold(t *testing.T) {
	pipeline := deliverPipeline(t)
	stageDir := "history/stage-1"
	if err := os.MkdirAll(pipeline.path(stageDir), 0o755); err != nil {
		t.Fatal(err)
	}
	// repository only — no verification_path, no expected_text.
	if err := os.WriteFile(pipeline.path(stageDir+"/ticket.json"), []byte(`{"repository":"example/one"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// A pre-existing delta skips the controller verb entirely.
	if err := os.WriteFile(pipeline.path(DeliverDeltaFile), []byte(`{"status":"ahead","files":["client/src/page.tsx"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.sealReferenceStagingReport(context.Background(), stageDir); err != nil {
		t.Fatalf("sealReferenceStagingReport() error = %v", err)
	}
	raw, err := os.ReadFile(pipeline.path(DeliverStagingReportFile))
	if err != nil {
		t.Fatalf("report unreadable: %v", err)
	}
	var report DeliverReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatalf("report is not valid JSON: %v", err)
	}
	if report.Verdict != "pass" {
		t.Fatalf("verdict = %q, want pass", report.Verdict)
	}
	if report.ScreenChecked {
		t.Fatal("screen_checked must stay false: no screen was verified")
	}
	if !strings.Contains(report.Detail, "画面の合否確認は行っていません") {
		t.Fatalf("detail = %q, want the no-verdict explanation", report.Detail)
	}
	if !report.Screenshot && strings.Contains(report.Detail, "写真を添付") {
		t.Fatalf("detail = %q promises a photo that was not captured", report.Detail)
	}
	if !strings.Contains(report.PromotionHold, "合格証拠を作れない") {
		t.Fatalf("promotion hold = %q, want the honest hold", report.PromotionHold)
	}
	if report.TargetURL == "" {
		t.Fatalf("target url should carry the staging origin for the reader")
	}
	// The requester-facing headline must not claim a screen check either —
	// render the real comment, not just the struct.
	content := hook.DeliverStagingContent("TKT-900", hook.DeliverStagingReport{
		Verdict: report.Verdict, Detail: report.Detail, PromotionHold: report.PromotionHold,
		ScreenChecked: report.ScreenChecked,
	})
	if strings.Contains(content, "画面を自動確認しました") {
		t.Fatalf("headline claims a screen check that did not happen:\n%s", content)
	}
	if !strings.Contains(content, "画面の合否確認は行っていません") {
		t.Fatalf("headline should state the no-check honestly:\n%s", content)
	}
}

func TestPromotionHoldTracksTheGateReality(t *testing.T) {
	build := func(t *testing.T, delta string) *Pipeline {
		pipeline := deliverPipeline(t)
		if err := os.WriteFile(pipeline.path("feature-pr.json"),
			[]byte(`{"binding":{"repository":"example/one","product_paths":["internal/gateway/budget.go"]}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if delta != "" {
			if err := os.WriteFile(pipeline.path(DeliverDeltaFile), []byte(delta), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return pipeline
	}
	cases := map[string]struct {
		delta    string
		wantHold string // substring; empty means promotable
	}{
		"own files only":         {`{"status":"ahead","files":["internal/gateway/budget.go"]}`, ""},
		"foreign file on stage":  {`{"status":"ahead","files":["internal/gateway/budget.go","internal/proxy/streaming.go"]}`, "以外の変更が滞留"},
		"diverged from release":  {`{"status":"diverged","files":["internal/gateway/budget.go"]}`, "分岐状態"},
		"nothing to promote":     {`{"status":"identical","files":[]}`, "同じ内容"},
		"delta unavailable":      {`{"status":"unavailable"}`, "確認できなかった"},
		"delta missing entirely": {"", "確認できなかった"},
		"file list truncated":    {`{"status":"ahead","files":["internal/gateway/budget.go"],"files_truncated":true}`, "大きすぎて"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			pipeline := build(t, c.delta)
			hold := pipeline.promotionHold()
			if c.wantHold == "" && hold != "" {
				t.Fatalf("promotionHold() = %q, want promotable", hold)
			}
			if c.wantHold != "" && !strings.Contains(hold, c.wantHold) {
				t.Fatalf("promotionHold() = %q, want a hold mentioning %q", hold, c.wantHold)
			}
		})
	}
	// Digest-commit files ride every promotion and must not trigger a hold.
	pipeline := deliverPipeline(t)
	consumer := `{"models":{"reviewers":[{"id":"a"},{"id":"b"}]},"consumers":[{"repository":"example/one","staging_origin":"https://one.example.invalid","github_contract":{"staging_digest_commit":{"exact_paths":["k8s/overlays/stg/kustomization.yaml"]}}}]}`
	if err := os.WriteFile(pipeline.Config.ConsumerConfigPath, []byte(consumer), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pipeline.path("feature-pr.json"),
		[]byte(`{"binding":{"repository":"example/one","product_paths":["internal/gateway/budget.go"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pipeline.path(DeliverDeltaFile),
		[]byte(`{"status":"ahead","files":["internal/gateway/budget.go","k8s/overlays/stg/kustomization.yaml"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if hold := pipeline.promotionHold(); hold != "" {
		t.Fatalf("digest files triggered a hold: %q", hold)
	}
}

func readSealedDeliverReport(t *testing.T, pipeline *Pipeline, name string) DeliverReport {
	t.Helper()
	raw, err := os.ReadFile(pipeline.path(name))
	if err != nil {
		t.Fatal(err)
	}
	var report DeliverReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatalf("report unreadable: %v", err)
	}
	if report.SchemaVersion != 1 || report.ObservedAt.IsZero() {
		t.Fatalf("report is not sealed: %s", raw)
	}
	return report
}

// A delivery phase holds itself to its card's wall too, and says so.
//
// Its own verbs read a killed process as a sealed verdict rather than as a
// failure that kept its cause, so the class this bound produces is not
// visible from here; what is visible, and what was missing, is the bound.
// Without it a phase killed at its wall arrives as a cancelled context,
// which is what a pod being replaced also looks like, and the ladder
// replays it for free.
func TestADeliveryPhaseHoldsItselfToItsCardsWall(t *testing.T) {
	pipeline := deliverPipeline(t)
	said := &wallLogger{}
	pipeline.Logger = said
	pipeline.Config.Chain.Deliver = runtime.DeliverConfig{
		ChecksProfile: "c", IntegrateProfile: "i", PromoteProfile: "p",
		EnabledAfter: "2026-09-01T00:00:00Z", ChecksMaxRuntimeSeconds: 600,
	}
	// It fails at once for want of a delivered pull request; what is
	// measured is the bound installed before any of that.
	_ = pipeline.RunDeliver(context.Background(), DeliverUntilChecks)
	if !said.saw("the card holds itself to its own wall") {
		t.Fatalf("the phase ran with no bound but the supervisor's signal: %v", said.lines)
	}
}

// deliverAtItsWall is a delivery card whose verb never answers, with the
// one wall an operator configures rather than the engine fixing.
func deliverAtItsWall(t *testing.T, wallSeconds int) *Pipeline {
	t.Helper()
	pipeline := deliverPipeline(t)
	sealRounds(t, pipeline, 1)
	if err := os.WriteFile(pipeline.path("feature-pr.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "waiting.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	pipeline.Config.ControllerBin = script
	pipeline.Config.Chain.Deliver = runtime.DeliverConfig{
		ChecksProfile: "c", IntegrateProfile: "i", PromoteProfile: "p",
		EnabledAfter: "2026-09-01T00:00:00Z", ChecksMaxRuntimeSeconds: wallSeconds,
	}
	return pipeline
}

// A delivery verb cut off by the card's own wall is the phase running out
// of its time, not the destination refusing.
//
// Every verb on this path reads a killed child as a result — exit code -1
// with no error beside it, the same shape as a verb that ran and refused —
// so the phase sealed "the CI never went green" and the ticket carried a
// red gate the requester never had. The ladder then replayed it for free,
// because a cancelled context is also what a pod being replaced looks like.
func TestADeliveryVerbCutOffAtItsWallSealsAsATimeout(t *testing.T) {
	pipeline := deliverAtItsWall(t, 1)

	err := pipeline.RunDeliver(context.Background(), DeliverUntilChecks)
	if err == nil {
		t.Fatal("a verb held past the card's wall returned no failure")
	}
	if _, sealed, _ := ReadDeliverReport(pipeline.Workspace, DeliverStagingReportFile); sealed {
		t.Fatal("a card that ran out of time sealed a verdict about the destination")
	}
	pipeline.SealStageFailure(DeliverStageOf(DeliverUntilChecks), err)
	record, ok := ReadStageFailure(pipeline.Workspace, DeliverStageOf(DeliverUntilChecks), runtime.DeliverRound)
	if !ok {
		t.Fatalf("nothing was sealed for a phase that used up its wall (error %v)", err)
	}
	if record.Class != FailureClassTimeout {
		t.Fatalf("class = %q, want the phase's own time running out (error %q)", record.Class, record.Error)
	}
	if record.Interrupted {
		t.Fatalf("a phase that used up its wall was sealed as a replacement from outside: %q", record.Error)
	}
	// The verb is named, because which step ran out is the first thing an
	// operator reading this record wants.
	if !strings.Contains(record.Error, "wait-feature") {
		t.Fatalf("the record does not name the verb that ran out: %q", record.Error)
	}
}

// A pod replaced mid-phase is still a replacement: nothing was learnt, and
// the ladder dispatches the card again without spending anything on it.
func TestADeliveryVerbStoppedByAReplacementStaysAnInterruption(t *testing.T) {
	pipeline := deliverAtItsWall(t, 600)
	ctx, replace := context.WithCancel(context.Background())
	defer replace()
	time.AfterFunc(100*time.Millisecond, replace)

	err := pipeline.RunDeliver(ctx, DeliverUntilChecks)
	if err == nil {
		t.Fatal("a verb stopped by the signal returned no failure")
	}
	pipeline.SealStageFailure(DeliverStageOf(DeliverUntilChecks), err)
	record, ok := ReadStageFailure(pipeline.Workspace, DeliverStageOf(DeliverUntilChecks), runtime.DeliverRound)
	if !ok {
		t.Fatalf("nothing was sealed for a phase whose pod was replaced (error %v)", err)
	}
	if !record.Interrupted {
		t.Fatalf("a pod being replaced was not sealed as a replacement (class %q, error %q)", record.Class, record.Error)
	}
	if record.Class == FailureClassTimeout {
		t.Fatal("a pod being replaced was sealed as the phase running out of its own time")
	}
}

// And under a live context every outcome is exactly what it was. A verb
// that ran and refused is the destination's answer about the change, and
// this wrapper has nothing to say about it.
func TestADeliveryVerbThatRefusedUnderALiveContextIsUnchanged(t *testing.T) {
	pipeline := deliverPipeline(t)
	sealRounds(t, pipeline, 1)
	if err := os.WriteFile(pipeline.path("feature-pr.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	standInController(t, pipeline, "1")
	pipeline.Config.Chain.Deliver = runtime.DeliverConfig{
		ChecksProfile: "c", IntegrateProfile: "i", PromoteProfile: "p",
		EnabledAfter: "2026-09-01T00:00:00Z", ChecksMaxRuntimeSeconds: 600,
	}

	if err := pipeline.RunDeliver(context.Background(), DeliverUntilStaging); err != nil {
		t.Fatalf("RunDeliver() error = %v", err)
	}

	// The sealed report, to the byte.
	sealed, err := os.ReadFile(pipeline.path(DeliverStagingReportFile))
	if err != nil {
		t.Fatal(err)
	}
	var report DeliverReport
	if err := json.Unmarshal(sealed, &report); err != nil {
		t.Fatal(err)
	}
	if report.Phase != "staging" || report.Verdict != "checks_failed" {
		t.Fatalf("report = %s, want the red gate it always sealed", sealed)
	}
	if report.Detail != "納品 PR の自動検査 (CI) が期限内に全部緑になりませんでした。ステージングへの反映は行っていません。" {
		t.Fatalf("the red gate's sentence moved: %q", report.Detail)
	}
	// And no failure record: a refusal is a result, not a card that broke.
	if _, ok := ReadStageFailure(pipeline.Workspace, DeliverStageOf(DeliverUntilStaging), runtime.DeliverRound); ok {
		t.Fatal("a red gate was sealed as the card failing")
	}
}
