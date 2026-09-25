package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/hook"
)

// A reviewer opening the pull request wants what the change is for and what
// was decided without anybody being asked. Those go above the round-by-round
// record, which answers a different question — and below the digest header,
// which the delivery's own verification requires the body to open with.
func TestThePullRequestBodyOpensWithTheOutcomeUnderItsDigestHeader(t *testing.T) {
	outcome := "## この変更でできるようになること\n注文履歴を月ごとに絞り込めます\n\n" +
		"## 確認せずに本体が決めたこと\n- 絞り込みの初期値は今月とする\n\n"
	record := "### 実装とレビューの経過\n- 1 周目: 収束\n"
	directory := t.TempDir()
	outcomePath := filepath.Join(directory, "m1-outcome.txt")
	if err := os.WriteFile(outcomePath, []byte(outcome), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded := readOutcomeFile(outcomePath)
	if loaded != outcome {
		t.Fatalf("readOutcomeFile returned %d of %d bytes", len(loaded), len(outcome))
	}
	binding := trailBodyBinding()
	body := featurePullRequestSpec(binding, loaded+record).Body

	header := strings.Index(body, "Issue: TICKET-123")
	opening := strings.Index(body, "この変更でできるようになること")
	decided := strings.Index(body, "絞り込みの初期値は今月とする")
	trail := strings.Index(body, "実装とレビューの経過")
	if header != 0 {
		t.Fatalf("the body does not open with the digest header the delivery verifies: %d", header)
	}
	// The delivery verifies its own pull request by the digest chain opening
	// the body verbatim; everything after it is deliberately unpinned.
	if !strings.HasPrefix(body, digestBody(binding, "")) {
		t.Fatalf("the body no longer opens with the digest chain the delivery verifies:\n%s", body)
	}
	for _, step := range []struct {
		name          string
		before, after int
	}{
		{"the header above the opening", header, opening},
		{"the opening above what was decided", opening, decided},
		{"what was decided above the record", decided, trail},
	} {
		if step.before < 0 || step.after < 0 || step.before >= step.after {
			t.Errorf("%s: %d is not above %d\n%s", step.name, step.before, step.after, body)
		}
	}
}

// Every way of failing to read the opening answers the same way: without it.
// A description missing its opening is worth more than a delivery refused
// over one.
func TestAnUnreadableOpeningLeavesTheDescriptionAsItWas(t *testing.T) {
	directory := t.TempDir()
	oversized := filepath.Join(directory, "too-long.txt")
	if err := os.WriteFile(oversized, []byte(strings.Repeat("x", outcomePreambleMaxBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	carriageReturn := filepath.Join(directory, "not-plain.txt")
	if err := os.WriteFile(carriageReturn, []byte("できること\r\n一覧"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(directory, "empty.txt")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"no file at all":       filepath.Join(directory, "absent.txt"),
		"no path given":        "",
		"an empty file":        empty,
		"longer than allowed":  oversized,
		"not plain text":       carriageReturn,
		"a directory, not one": directory,
	} {
		if got := readOutcomeFile(path); got != "" {
			t.Errorf("%s produced %d bytes of opening", name, len(got))
		}
	}
}

// The opening and the record share the record's budget, and the record is
// the half that gives way: it is the account of how the work went, and the
// opening is what the work was for. Without that, a record already at its
// own bound plus an opening makes a body the client refuses, and the
// delivery fails at its last step with the work already pushed.
func TestTheRecordGivesWayToTheOpeningInsideTheBodyBudget(t *testing.T) {
	fixture := newDeliveryFixture(t)
	transport := newDeliveryTransport(fixture)
	record := strings.Repeat("あ", hook.MaxTrailRecordBytes/3)
	if err := os.WriteFile(fixture.trailPath, []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}
	opening := strings.Repeat("決めたこと。", 300)
	outcomePath := filepath.Join(t.TempDir(), "m1-outcome.txt")
	if err := os.WriteFile(outcomePath, []byte(opening), 0o600); err != nil {
		t.Fatal(err)
	}

	featurePath := fixture.output("feature.json")
	if err := run(context.Background(), fixture.publishArguments(featurePath), deliveryEnvironment, transport); err != nil {
		t.Fatalf("publish-feature: %v", err)
	}
	pullPath := fixture.output("feature-pr.json")
	arguments := append(fixture.createPullRequestArguments(featurePath, pullPath), "--outcome", outcomePath)
	if err := run(context.Background(), arguments, deliveryEnvironment, transport); err != nil {
		t.Fatalf("create-feature-pr: %v", err)
	}
	pull, err := readDeliveryArtifact[featurePRPayload](pullPath, kindFeaturePR, fixture.request, fixture.config)
	if err != nil {
		t.Fatalf("feature pull request artifact: %v", err)
	}
	body := pull.Payload.PullRequest.Body
	if len(body) > maxPullRequestBodyBytes {
		t.Fatalf("the body is %d bytes, over the client's %d", len(body), maxPullRequestBodyBytes)
	}
	if !strings.Contains(body, opening) {
		t.Errorf("the opening gave way to the record")
	}
	if !strings.Contains(body, hook.TrailShortenedNote) {
		t.Errorf("the record was cut without the description saying so")
	}
}

// The verb that opens the pull request puts the opening where the reviewer
// reads it: under the digest chain the delivery verifies, above the record.
// A body that is not the one the spec asked for is refused by the client, so
// this also proves the opening survives the round trip.
func TestCreateFeaturePRPutsTheOpeningAboveTheRecord(t *testing.T) {
	fixture := newDeliveryFixture(t)
	transport := newDeliveryTransport(fixture)
	outcomePath := filepath.Join(t.TempDir(), "m1-outcome.txt")
	opening := "## この変更でできるようになること\n注文履歴を月ごとに絞り込めます\n\n" +
		"## 確認せずに本体が決めたこと\n- 絞り込みの初期値は今月とする\n\n"
	if err := os.WriteFile(outcomePath, []byte(opening), 0o600); err != nil {
		t.Fatal(err)
	}

	featurePath := fixture.output("feature.json")
	if err := run(context.Background(), fixture.publishArguments(featurePath), deliveryEnvironment, transport); err != nil {
		t.Fatalf("publish-feature: %v", err)
	}
	pullPath := fixture.output("feature-pr.json")
	arguments := append(fixture.createPullRequestArguments(featurePath, pullPath), "--outcome", outcomePath)
	if err := run(context.Background(), arguments, deliveryEnvironment, transport); err != nil {
		t.Fatalf("create-feature-pr: %v", err)
	}
	pull, err := readDeliveryArtifact[featurePRPayload](pullPath, kindFeaturePR, fixture.request, fixture.config)
	if err != nil {
		t.Fatalf("feature pull request artifact: %v", err)
	}
	body := pull.Payload.PullRequest.Body
	if !strings.HasPrefix(body, digestBody(pull.Binding, "")) {
		t.Fatalf("the body no longer opens with the digest chain:\n%s", body)
	}
	opened := strings.Index(body, "この変更でできるようになること")
	record := strings.Index(body, "実装とレビューの経過")
	if opened < 0 {
		t.Fatalf("the opening never reached the description:\n%s", body)
	}
	if record < 0 || opened >= record {
		t.Errorf("the opening is not above the record: %d, %d\n%s", opened, record, body)
	}
}
