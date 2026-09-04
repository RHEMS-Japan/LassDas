package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/githubapi"
)

func TestExtractPublishOptionRemovesOnePairAndKeepsTheRest(t *testing.T) {
	args := []string{"--config", "c.json", "--source-base", strings.Repeat("a", 40), "--out", "o.json"}
	trimmed, value := extractPublishOption(args, "--source-base")
	if value != strings.Repeat("a", 40) {
		t.Fatalf("value = %q", value)
	}
	if !slices.Equal(trimmed, []string{"--config", "c.json", "--out", "o.json"}) {
		t.Fatalf("trimmed = %v", trimmed)
	}
	same, missing := extractPublishOption(trimmed, "--failure-out")
	if missing != "" || !slices.Equal(same, trimmed) {
		t.Fatalf("absent option changed the arguments: %v %q", same, missing)
	}
}

func TestWritePublishFailureCarriesTheInvariantName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "failure.json")
	writePublishFailure(path, "feature_publish_failed", &githubapi.InvariantError{Code: "integration_base_changed"})
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var failure struct {
		Code      string `json:"code"`
		Invariant string `json:"invariant"`
	}
	if json.Unmarshal(encoded, &failure) != nil ||
		failure.Code != "feature_publish_failed" || failure.Invariant != "integration_base_changed" {
		t.Fatalf("failure file = %s", encoded)
	}
	writePublishFailure(path, "publish_gate_rejected", nil)
	encoded, _ = os.ReadFile(path)
	if !strings.Contains(string(encoded), "publish_gate_rejected") || strings.Contains(string(encoded), "invariant") {
		t.Fatalf("gate failure file = %s", encoded)
	}
	// An empty path is a no-op, never an error.
	writePublishFailure("", "feature_publish_failed", errors.New("x"))
}

func TestRunPublishFeatureRejectsAMalformedSourceBase(t *testing.T) {
	err := run(context.Background(), []string{"publish-feature", "--source-base", "not-a-sha"}, func(string) string { return "" }, nil)
	if failureCode(err) != "arguments_invalid" {
		t.Fatalf("error = %v", err)
	}
}

// The integration branch advancing mid-run refuses the publish and leaves
// the machine-readable reason the runner keys its retry on.
func TestRunPublishFeatureReportsTheAdvancedBase(t *testing.T) {
	fixture := newDeliveryFixture(t)
	transport := newDeliveryTransport(fixture)
	advanced := strings.Repeat("7", 40)
	transport.integrationSHA = advanced
	transport.pullBaseSHA = advanced

	failurePath := fixture.output("publish-failure.json")
	arguments := append(fixture.publishArguments(fixture.output("feature.json")), "--failure-out", failurePath)
	err := run(context.Background(), arguments, deliveryEnvironment, transport)
	if failureCode(err) != "feature_publish_failed" {
		t.Fatalf("error = %v", err)
	}
	encoded, readErr := os.ReadFile(failurePath)
	if readErr != nil || !strings.Contains(string(encoded), `"invariant":"integration_base_changed"`) {
		t.Fatalf("failure file = %s, %v", encoded, readErr)
	}
}

// The base-advance retry: the runner publishes against a freshly advanced
// baseline and pins the original base with --source-base. The per-file blob
// checks then hold every touched path against the advanced tree, and the
// publish succeeds without weakening any other invariant.
func TestRunPublishFeaturePublishesAgainstAnAdvancedBase(t *testing.T) {
	fixture := newDeliveryFixture(t)
	transport := newDeliveryTransport(fixture)
	original := fixture.baseline.Baseline.Integration.SHA
	advanced := strings.Repeat("7", 40)
	transport.integrationSHA = advanced
	transport.pullBaseSHA = advanced

	advancedBaseline := fixture.baseline.Baseline
	advancedBaseline.Integration.SHA = advanced
	artifact, err := newBaselineArtifact(fixture.config, fixture.consumer, advancedBaseline)
	if err != nil {
		t.Fatal(err)
	}
	advancedPath := fixture.output("baseline-advanced.json")
	if err := writeControllerArtifact(advancedPath, artifact); err != nil {
		t.Fatal(err)
	}

	featurePath := fixture.output("feature.json")
	arguments := fixture.publishArguments(featurePath)
	for index, argument := range arguments {
		if argument == "--baseline" {
			arguments[index+1] = advancedPath
		}
	}
	arguments = append(arguments, "--source-base", original, "--failure-out", fixture.output("publish-failure.json"))
	if err := run(context.Background(), arguments, deliveryEnvironment, transport); err != nil {
		t.Fatalf("publish-feature against the advanced base: %v", err)
	}
	feature, err := readDeliveryArtifact[githubapi.PublishedFeature](featurePath, kindFeature, fixture.request, fixture.config)
	if err != nil || !validPublishedFeature(feature.Payload, feature.Binding) {
		t.Fatalf("published feature artifact is invalid: %v", err)
	}
	if _, err := os.Stat(fixture.output("publish-failure.json")); err == nil {
		t.Fatal("a successful publish left a failure file")
	}
}

// The safety cornerstone of the relaxation: when the base advanced AND a
// touched file also moved upstream, the publish keeps refusing even with
// --source-base — the per-file blob checks hold every touched path to the
// blob the candidate was recorded on, against the advanced tree.
func TestRunPublishFeatureRefusesAConflictingAdvancedBase(t *testing.T) {
	fixture := newDeliveryFixture(t)
	transport := newDeliveryTransport(fixture)
	original := fixture.baseline.Baseline.Integration.SHA
	advanced := strings.Repeat("7", 40)
	transport.integrationSHA = advanced
	transport.pullBaseSHA = advanced
	// The advanced tree serves a DIFFERENT blob for the touched path.
	transport.sourceBlobSHA = strings.Repeat("9", 40)

	advancedBaseline := fixture.baseline.Baseline
	advancedBaseline.Integration.SHA = advanced
	artifact, err := newBaselineArtifact(fixture.config, fixture.consumer, advancedBaseline)
	if err != nil {
		t.Fatal(err)
	}
	advancedPath := fixture.output("baseline-advanced.json")
	if err := writeControllerArtifact(advancedPath, artifact); err != nil {
		t.Fatal(err)
	}
	failurePath := fixture.output("publish-failure.json")
	arguments := fixture.publishArguments(fixture.output("feature.json"))
	for index, argument := range arguments {
		if argument == "--baseline" {
			arguments[index+1] = advancedPath
		}
	}
	arguments = append(arguments, "--source-base", original, "--failure-out", failurePath)
	if failureCode(run(context.Background(), arguments, deliveryEnvironment, transport)) != "feature_publish_failed" {
		t.Fatal("a conflicting advanced base was accepted")
	}
	encoded, readErr := os.ReadFile(failurePath)
	if readErr != nil || !strings.Contains(string(encoded), `"invariant":"source_blob_changed"`) {
		t.Fatalf("failure file = %s, %v", encoded, readErr)
	}
}

// Without --source-base an advanced baseline artifact must keep refusing:
// the source snapshot does not chain to it.
func TestRunPublishFeatureStillChainsTheSourceWithoutSourceBase(t *testing.T) {
	fixture := newDeliveryFixture(t)
	transport := newDeliveryTransport(fixture)
	advanced := strings.Repeat("7", 40)
	transport.integrationSHA = advanced
	transport.pullBaseSHA = advanced

	advancedBaseline := fixture.baseline.Baseline
	advancedBaseline.Integration.SHA = advanced
	artifact, err := newBaselineArtifact(fixture.config, fixture.consumer, advancedBaseline)
	if err != nil {
		t.Fatal(err)
	}
	advancedPath := fixture.output("baseline-advanced.json")
	if err := writeControllerArtifact(advancedPath, artifact); err != nil {
		t.Fatal(err)
	}
	arguments := fixture.publishArguments(fixture.output("feature.json"))
	for index, argument := range arguments {
		if argument == "--baseline" {
			arguments[index+1] = advancedPath
		}
	}
	if failureCode(run(context.Background(), arguments, deliveryEnvironment, transport)) != "publish_gate_rejected" {
		t.Fatal("an unchained advanced baseline was accepted")
	}
}
