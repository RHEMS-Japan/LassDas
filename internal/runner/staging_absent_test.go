package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// A destination that started no staging deployment for the merge is not a
// deployment that failed. Told as one, the requester reads "the deploy did
// not finish" about a deploy that was never going to run, and an operator
// goes looking for a broken deployment that is working exactly as
// configured (live 2026-09-09: two deliveries of docs-only changes waited
// fifty-one minutes each for a workflow whose paths do not include docs).
func TestAStagingDeployThatNeverStartedIsNotToldAsOneThatFailed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stderr string
		want   string
	}{
		{"the destination created no run", "controller: staging_deployment_absent", "deploy_absent"},
		{"the deployment did not complete", "controller: staging_deployment_failed", "deploy_failed"},
		// The code carries its meaning only as the verb's own ending line.
		// The same text inside a wrapped error is the detail underneath some
		// other failure, and reading it as the ending would tell a requester
		// nothing deployed when a deployment ran and broke.
		{"the code appears only inside another failure's text",
			"controller: staging_deployment_failed: staging_deployment_absent was considered\ncontroller: staging_deployment_failed", "deploy_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pipeline := deliverPipeline(t)
			sealRounds(t, pipeline, 1)
			if err := os.WriteFile(pipeline.path("feature-pr.json"), []byte(`{"payload":{"pull_request":{"Number":41}}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			script := filepath.Join(t.TempDir(), "controller.sh")
			body := "#!/bin/sh\n" +
				"verb=\"$1\"\n" +
				"out=\"\"; prev=\"\"\nfor a in \"$@\"; do [ \"$prev\" = \"--out\" ] && out=\"$a\"; prev=\"$a\"; done\n" +
				"if [ \"$verb\" = \"await-staging\" ]; then\n" +
				"  printf '%s\\n' \"" + tc.stderr + "\" >&2\n  exit 1\nfi\n" +
				"[ -n \"$out\" ] && echo '{}' > \"$out\"\nexit 0\n"
			if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
			pipeline.Config.ControllerBin = script

			if err := pipeline.RunDeliver(context.Background(), DeliverUntilStaging); err != nil {
				t.Fatalf("RunDeliver() error = %v", err)
			}
			report := readSealedDeliverReport(t, pipeline, DeliverStagingReportFile)
			if report.Verdict != tc.want {
				t.Fatalf("verdict = %q, want %q (stderr was %q)", report.Verdict, tc.want, tc.stderr)
			}
			if tc.want == "deploy_absent" && report.Detail == "" {
				t.Fatal("the report says nothing about what happened")
			}
		})
	}
}

// The promotion carries the higher-stakes version of the same sentence, and
// the shipped contract names two production workflows — a guard workflow
// among them is exactly the kind that is filtered on paths. Adding the arm
// without a test left it free to delete with everything green, which is the
// same asymmetry twice (review of #134).
func TestAProductionDeployThatNeverStartedIsNotToldAsOneThatFailed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stderr string
		want   string
	}{
		{"the destination created no run", "controller: production_deployment_absent", "deploy_absent"},
		{"the deployment did not complete", "controller: production_deployment_failed", "deploy_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pipeline := deliverPipeline(t)
			sealRounds(t, pipeline, 1)
			for name, content := range map[string]string{
				"feature-pr.json":         `{"payload":{"pull_request":{"Number":41}}}`,
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
			script := filepath.Join(t.TempDir(), "controller.sh")
			body := "#!/bin/sh\n" +
				"verb=\"$1\"\n" +
				"out=\"\"; prev=\"\"\nfor a in \"$@\"; do [ \"$prev\" = \"--out\" ] && out=\"$a\"; prev=\"$a\"; done\n" +
				"if [ \"$verb\" = \"await-production\" ]; then\n" +
				"  printf '%s\\n' \"" + tc.stderr + "\" >&2\n  exit 1\nfi\n" +
				"[ -n \"$out\" ] && echo '{}' > \"$out\"\nexit 0\n"
			if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
			pipeline.Config.ControllerBin = script

			if err := pipeline.RunDeliver(context.Background(), DeliverUntilProduction); err != nil {
				t.Fatalf("RunDeliver() error = %v", err)
			}
			report := readSealedDeliverReport(t, pipeline, DeliverProductionReportFile)
			if report.Verdict != tc.want {
				t.Fatalf("verdict = %q, want %q (stderr was %q)", report.Verdict, tc.want, tc.stderr)
			}
		})
	}
}
