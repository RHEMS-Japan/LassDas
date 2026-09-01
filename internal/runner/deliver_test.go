package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
