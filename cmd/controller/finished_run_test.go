package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The verbs that read a run AFTER it ended are handed the digest the run
// recorded, and are no longer held to the destination's configuration as it
// stands now. A delivery whose pull request had been merged sat on the board
// asking for that merge for more than ten hours because this reading was
// refused, and from the reception a refusal reads exactly like "not merged
// yet" (live 2026-09-24).
func TestReadMergedReadsAFinishedRunAfterTheConfigurationChanged(t *testing.T) {
	fixture := newDeliveryFixture(t)
	changed := changedConsumerConfig(t)
	transport := newDeliveryTransport(fixture)
	transport.merged = true

	// Held to the live configuration, the run's own ticket is refused, and
	// nothing reaches the destination at all.
	err := run(context.Background(), []string{
		"read-merged", "--config", changed, "--ticket", fixture.ticketPath,
		"--number", decimal(deliveryPullNumber), "--out", fixture.output("refused.json"),
	}, deliveryEnvironment, transport)
	if failureCode(err) != "ticket_artifact_invalid" {
		t.Fatalf("error = %v", err)
	}

	// Told which digest the run recorded, the same reading succeeds.
	output := fixture.output("merged.json")
	err = run(context.Background(), []string{
		"read-merged", "--config", changed, "--ticket", fixture.ticketPath,
		"--config-sha256", fixture.request.ConfigSHA256,
		"--number", decimal(deliveryPullNumber), "--out", output,
	}, deliveryEnvironment, transport)
	if err != nil {
		t.Fatalf("read-merged: %v", err)
	}
	var reading struct {
		State          string `json:"state"`
		Merged         bool   `json:"merged"`
		MergeCommitSHA string `json:"merge_commit_sha"`
	}
	raw, readErr := os.ReadFile(output)
	if readErr != nil || json.Unmarshal(raw, &reading) != nil {
		t.Fatalf("reading = %s, error = %v", raw, readErr)
	}
	if !reading.Merged || reading.MergeCommitSHA != deliveryMergeSHA {
		t.Fatalf("reading = %+v", reading)
	}

	// A digest no run recorded is still a refusal: the option names one run,
	// it does not switch the comparison off.
	err = run(context.Background(), []string{
		"read-merged", "--config", changed, "--ticket", fixture.ticketPath,
		"--config-sha256", strings.Repeat("9", 64),
		"--number", decimal(deliveryPullNumber), "--out", fixture.output("wrong.json"),
	}, deliveryEnvironment, transport)
	if failureCode(err) != "ticket_artifact_invalid" {
		t.Fatalf("a foreign digest was accepted: %v", err)
	}
	err = run(context.Background(), []string{
		"read-merged", "--config", changed, "--ticket", fixture.ticketPath,
		"--config-sha256", "not-a-digest",
		"--number", decimal(deliveryPullNumber), "--out", fixture.output("malformed.json"),
	}, deliveryEnvironment, transport)
	if failureCode(err) != "config_sha256_invalid" {
		t.Fatalf("a malformed digest was accepted: %v", err)
	}
}

// The delivery continuation also runs after the run has ended, and it reads
// far more than the ticket: the sealed publication gate, the baseline and the
// preceding step's artifact all have to come back readable. They chain their
// own digests to the ticket's, so the recorded digest has to reach the ticket
// for any of them to pass.
func TestTheDeliveryContinuationReadsAFinishedRunAfterTheConfigurationChanged(t *testing.T) {
	fixture := newDeliveryFixture(t)
	transport := newDeliveryTransport(fixture)

	// The run publishes under the configuration it was admitted with.
	featurePath := fixture.output("feature.json")
	if err := run(context.Background(), fixture.publishArguments(featurePath), deliveryEnvironment, transport); err != nil {
		t.Fatalf("publish-feature: %v", err)
	}
	pullPath := fixture.output("feature-pr.json")
	if err := run(context.Background(), fixture.createPullRequestArguments(featurePath, pullPath), deliveryEnvironment, transport); err != nil {
		t.Fatalf("create-feature-pr: %v", err)
	}

	// Then the destination's configuration is changed, and the continuation
	// carries the finished run the rest of the way.
	changed := changedConsumerConfig(t)
	recorded := fixture.request.ConfigSHA256

	refused := []string{
		"wait-feature", "--config", changed, "--ticket", fixture.ticketPath,
		"--feature-pr", pullPath, "--out", fixture.output("checks-refused.json"),
	}
	if code := failureCode(run(context.Background(), refused, deliveryEnvironment, transport)); code != "ticket_artifact_invalid" {
		t.Fatalf("the live configuration no longer refuses a finished run's ticket: %s", code)
	}

	checksPath := fixture.output("feature-checks.json")
	told := []string{
		"wait-feature", "--config", changed, "--ticket", fixture.ticketPath,
		"--config-sha256", recorded, "--feature-pr", pullPath, "--out", checksPath,
	}
	if err := run(context.Background(), told, deliveryEnvironment, transport); err != nil {
		t.Fatalf("wait-feature: %v", err)
	}
	mergePath := fixture.output("feature-merge.json")
	told = []string{
		"merge-feature", "--config", changed, "--ticket", fixture.ticketPath,
		"--config-sha256", recorded, "--feature-pr", pullPath, "--checks", checksPath, "--out", mergePath,
	}
	if err := run(context.Background(), told, deliveryEnvironment, transport); err != nil {
		t.Fatalf("merge-feature: %v", err)
	}

	// await-staging re-verifies the whole gate before it waits. The
	// destination's run list is empty in this fixture, so reaching the
	// "no deployment was created" answer is proof that every sealed record
	// passed its validator; the parent deadline closes the wait in seconds.
	staging := func(digest, output string) []string {
		arguments := []string{"await-staging", "--config", changed, "--ticket", fixture.ticketPath}
		if digest != "" {
			arguments = append(arguments, "--config-sha256", digest)
		}
		arguments = append(arguments, fixture.gateFlags()...)
		return append(arguments, "--baseline", fixture.baselinePath, "--feature-merge", mergePath, "--out", output)
	}
	if code := failureCode(run(context.Background(), staging("", fixture.output("staging-refused.json")), deliveryEnvironment, transport)); code != "ticket_artifact_invalid" {
		t.Fatalf("await-staging no longer refuses a finished run held to the live configuration: %s", code)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if code := failureCode(run(ctx, staging(recorded, fixture.output("staging-proof.json")), deliveryEnvironment, transport)); code != StagingDeploymentAbsentCode {
		t.Fatalf("a sealed record did not reach the destination: %s", code)
	}
}

// A verb that runs while the delivery is still in flight does not accept the
// option at all: a run whose configuration moves underneath it must still
// stop, and the surest way to keep that is for the argument not to exist.
func TestInFlightVerbsDoNotAcceptARecordedDigest(t *testing.T) {
	fixture := newDeliveryFixture(t)
	transport := newDeliveryTransport(fixture)
	inFlight := map[string][]string{
		"publish-feature": append(fixture.publishArguments(fixture.output("feature.json")),
			"--config-sha256", fixture.request.ConfigSHA256),
		"create-feature-pr": append(fixture.createPullRequestArguments(
			fixture.output("some-feature.json"), fixture.output("feature-pr.json")),
			"--config-sha256", fixture.request.ConfigSHA256),
		"baseline": {
			"baseline", "--config", controllerConfigPath, "--draft", writeControllerDraft(t, fixture.config),
			"--config-sha256", fixture.request.ConfigSHA256, "--out", fixture.output("baseline.json"),
		},
	}
	for verb, arguments := range inFlight {
		t.Run(verb, func(t *testing.T) {
			if code := failureCode(run(context.Background(), arguments, deliveryEnvironment, transport)); code != "arguments_invalid" {
				t.Fatalf("%s accepted a recorded digest: %s", verb, code)
			}
		})
	}
}

// changedConsumerConfig writes the destination's configuration as it stands
// after an operator changed it: a valid configuration the loader accepts,
// whose digest is no longer the one the fixture's run recorded.
func changedConsumerConfig(t *testing.T) string {
	t.Helper()
	encoded, err := os.ReadFile(controllerConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	changed := bytes.Replace(encoded, []byte(`"max_files": 8`), []byte(`"max_files": 7`), 1)
	if bytes.Equal(changed, encoded) {
		t.Fatal("test fixture did not change the configuration")
	}
	path := filepath.Join(t.TempDir(), filepath.Base(controllerConfigPath))
	if err := os.WriteFile(path, changed, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadFixedConfig(path); err != nil {
		t.Fatalf("the changed configuration is not a valid one: %v", err)
	}
	// Every command loads the configuration itself; leave the package's
	// loaded one as the fixture found it.
	if _, err := loadFixedConfig(controllerConfigPath); err != nil {
		t.Fatal(err)
	}
	return path
}
