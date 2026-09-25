package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The digest the fixture's run recorded, distinct from every other repeated
// digit in these tests so a value travelling the wrong way is visible.
const recordedRunDigest = "2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b"

// The continuation and the observation both run after the run itself has
// ended, and the destination's configuration may have been changed in
// between. They name the digest the run recorded, so the delivery binary
// holds the run's sealed records to it rather than to the live
// configuration — which is what stopped a merged pull request from ever
// being noticed (live 2026-09-24).
func TestPostTerminalVerbsNameTheDigestTheRunRecorded(t *testing.T) {
	tests := map[string]func(*Pipeline) error{
		"deliver": func(p *Pipeline) error { return p.RunDeliver(context.Background(), DeliverUntilChecks) },
		"observe": func(p *Pipeline) error { return p.RunE2ECheck(context.Background()) },
	}
	for name, drive := range tests {
		t.Run(name, func(t *testing.T) {
			pipeline := deliverPipeline(t)
			sealRounds(t, pipeline, 1)
			writeDeliveredPullRequest(t, pipeline, recordedRunDigest)
			record := recordingController(t, pipeline)

			if err := drive(pipeline); err != nil {
				t.Fatalf("driving the card: %v", err)
			}

			if digest := argumentValue(t, record, "--config-sha256"); digest != recordedRunDigest {
				t.Fatalf("--config-sha256 = %q, want the digest the run recorded", digest)
			}
		})
	}
}

// A delivered pull request artifact that records no digest leaves the verb
// exactly as it was: bound to the live configuration, and no worse than
// before. Naming an empty or malformed value would be refused outright.
func TestAPullRequestArtifactWithoutADigestNamesNone(t *testing.T) {
	pipeline := deliverPipeline(t)
	sealRounds(t, pipeline, 1)
	if err := os.WriteFile(pipeline.path("feature-pr.json"), []byte(`{"payload":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	record := recordingController(t, pipeline)

	if err := pipeline.RunDeliver(context.Background(), DeliverUntilChecks); err != nil {
		t.Fatalf("driving the card: %v", err)
	}
	if digest := argumentValue(t, record, "--config-sha256"); digest != "" {
		t.Fatalf("--config-sha256 = %q, want none", digest)
	}
}

// writeDeliveredPullRequest seals the artifact the publication left behind,
// carrying the digest the run was sealed under.
func writeDeliveredPullRequest(t *testing.T, pipeline *Pipeline, recorded string) {
	t.Helper()
	artifact := `{"schema_version":1,"kind":"m1-feature-pull-request",` +
		`"binding":{"config_sha256":"` + recorded + `"},` +
		`"payload":{"pull_request":{"Number":76}}}`
	if err := os.WriteFile(pipeline.path("feature-pr.json"), []byte(artifact), 0o600); err != nil {
		t.Fatal(err)
	}
}

// recordingController stands in for the delivery binary: it writes down the
// arguments it was called with and refuses, which every verb under test
// treats as a sealed result rather than broken plumbing.
func recordingController(t *testing.T, pipeline *Pipeline) string {
	t.Helper()
	record := filepath.Join(t.TempDir(), "argv.txt")
	script := filepath.Join(t.TempDir(), "controller.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+record+"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	pipeline.Config.ControllerBin = script
	return record
}

// argumentValue answers what the recorded call passed for one option, or the
// empty string when it passed the option at all.
func argumentValue(t *testing.T, record, name string) string {
	t.Helper()
	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	arguments := strings.Split(strings.TrimSpace(string(raw)), "\n")
	for index, argument := range arguments {
		if argument == name && index+1 < len(arguments) {
			return arguments[index+1]
		}
	}
	return ""
}
