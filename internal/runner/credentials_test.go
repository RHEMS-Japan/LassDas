package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/runtime"
)

// envDumpingWorker stands in for the worker binary and writes its whole
// environment to a file, so a test can say exactly what a card's child
// process could see.
func envDumpingWorker(t *testing.T, dump string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "worker")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nenv > "+dump+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return script
}

// A card named in a credential really receives it: every process that card
// starts, the agent that writes the change included, finds the variable in
// its environment. This is the whole point of handing the engine its means
// — a change that has to reach a service cannot be written by something
// that was given no way in.
func TestACardsChildProcessesSeeTheCredentialsThatCardWasNamedIn(t *testing.T) {
	dump := filepath.Join(t.TempDir(), "env.txt")
	pipeline := chainStagePipeline(t)
	pipeline.Config.WorkerBin = envDumpingWorker(t, dump)
	pipeline.Config.ConsumerConfigPath = writeChainConsumerConfig(t, []string{"review-a", "review-b"})
	pipeline.StageCredentials = []string{"DATABASE_URL=postgres://warehouse.invalid/orders"}
	sealStageFiles(t, pipeline, 1, "")
	if err := pipeline.chainReview(context.Background(), []string{"review-a", "review-b"}, 0, pipeline.path("target-repo"), strings.Repeat("ab", 20)); err != nil {
		t.Fatalf("chainReview: %v", err)
	}
	environment, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("the worker did not run: %v", err)
	}
	if !strings.Contains(string(environment), "DATABASE_URL=postgres://warehouse.invalid/orders") {
		t.Fatalf("the card's child did not receive the credential:\n%s", environment)
	}
}

// And a card named in none receives none. The two entries are read by the
// card's own entry point, so a card that was not named never reads the file
// at all.
func TestACardNamedInNoCredentialReceivesNothing(t *testing.T) {
	dump := filepath.Join(t.TempDir(), "env.txt")
	pipeline := chainStagePipeline(t)
	pipeline.Config.WorkerBin = envDumpingWorker(t, dump)
	pipeline.Config.ConsumerConfigPath = writeChainConsumerConfig(t, []string{"review-a", "review-b"})
	sealStageFiles(t, pipeline, 1, "")
	if err := pipeline.chainReview(context.Background(), []string{"review-a", "review-b"}, 0, pipeline.path("target-repo"), strings.Repeat("ab", 20)); err != nil {
		t.Fatalf("chainReview: %v", err)
	}
	environment, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("the worker did not run: %v", err)
	}
	if strings.Contains(string(environment), "DATABASE_URL") {
		t.Fatalf("an unnamed card received a credential:\n%s", environment)
	}
}

// A credential a failing command echoed must not survive the card. The
// tail of a step's output travels into the round's failure record, into the
// next round's instruction and onto the ticket; each of those outlives the
// card, and a ticket is the least private place the engine writes.
func TestACredentialEchoedByAFailingStepIsNotKept(t *testing.T) {
	secret := "postgres://warehouse.invalid/orders?password=hunter2hunter2"
	script := filepath.Join(t.TempDir(), "worker")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"psql: could not connect to $DATABASE_URL\" >&2\nexit 3\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	pipeline := chainStagePipeline(t)
	pipeline.Config.WorkerBin = script
	pipeline.Config.ConsumerConfigPath = writeChainConsumerConfig(t, []string{"review-a", "review-b"})
	pipeline.StageCredentials = []string{"DATABASE_URL=" + secret}
	sealStageFiles(t, pipeline, 1, "")
	err := pipeline.chainReview(context.Background(), []string{"review-a", "review-b"}, 0, pipeline.path("target-repo"), strings.Repeat("ab", 20))
	if err == nil {
		t.Fatal("the failing step was reported as a success")
	}
	if strings.Contains(pipeline.lastStepStderr, secret) || strings.Contains(err.Error(), secret) {
		t.Fatalf("the credential survived the card: %q / %v", pipeline.lastStepStderr, err)
	}
	if !strings.Contains(pipeline.lastStepStderr, "could not connect to") {
		t.Fatalf("the reason the card failed was lost with it: %q", pipeline.lastStepStderr)
	}
	// The stage's sealed record is what the ladder and the report read.
	pipeline.SealStageFailure(runtime.StageReviewA, err)
	sealed, readErr := os.ReadFile(pipeline.path("history/stage-1/review-a-failure.json"))
	if readErr != nil {
		t.Fatalf("the failure was not sealed: %v", readErr)
	}
	if strings.Contains(string(sealed), secret) {
		t.Fatalf("the credential reached the round's record:\n%s", sealed)
	}
}
