package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/cardsecret"
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

// Anything a step prints goes to the live log, which is what the board's
// live pane serves. A credential printed there is published to everyone who
// can open the board, and nothing about it has a shape the general masker
// would recognise.
func TestACredentialPrintedByAStepDoesNotReachTheLiveLog(t *testing.T) {
	cardsecret.Forget()
	t.Cleanup(cardsecret.Forget)
	secret := "postgres://warehouse.invalid/orders?password=hunter2hunter2"
	cardsecret.Register([]cardsecret.Entry{{Name: "DATABASE_URL", Secret: secret}})

	script := filepath.Join(t.TempDir(), "worker")
	// Once on each stream: the board serves both.
	body := "#!/bin/sh\necho \"connecting to $DATABASE_URL\"\necho \"psql: $DATABASE_URL refused\" >&2\nexit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	pipeline := chainStagePipeline(t)
	pipeline.Config.WorkerBin = script
	pipeline.Config.ConsumerConfigPath = writeChainConsumerConfig(t, []string{"review-a", "review-b"})
	pipeline.StageCredentials = []string{"DATABASE_URL=" + secret}
	sealStageFiles(t, pipeline, 1, "")
	if err := pipeline.chainReview(context.Background(), []string{"review-a", "review-b"}, 0, pipeline.path("target-repo"), strings.Repeat("ab", 20)); err != nil {
		t.Fatalf("chainReview: %v", err)
	}
	// The step names its own live file; the board serves what is in it.
	shown, err := os.ReadFile(LiveLogPath(pipeline.Workspace, "agent-review"))
	if err != nil {
		t.Fatalf("the live log was not written: %v", err)
	}
	if strings.Contains(string(shown), "hunter2hunter2") {
		t.Fatalf("the board would show the credential:\n%s", shown)
	}
	if !strings.Contains(string(shown), "秘密") {
		t.Fatalf("the line was dropped without saying why:\n%s", shown)
	}
}

// The tail the engine keeps is the last so many bytes of what a step said.
// A value straddling the cut would survive as its own last characters,
// which no whole-value replacement finds: one byte off the front of a token
// is still the token.
func TestACredentialIsNeverSplitByTheTailTheEngineKeeps(t *testing.T) {
	cardsecret.Forget()
	t.Cleanup(cardsecret.Forget)
	secret := strings.Repeat("s3cr3t", 10)
	cardsecret.Register([]cardsecret.Entry{{Name: "API_TOKEN", Secret: secret}})

	// The bound is small enough that the cut falls inside the value's own
	// line, which is the case a byte offset cannot survive.
	buffer := &tailBuffer{limit: 40}
	_, _ = buffer.Write([]byte("a\ntoken " + secret + "\n"))
	_, _ = buffer.Write([]byte("later output line\n"))
	// Read raw, before any replacement: what is asserted here is that the
	// tail never holds a part of the value in the first place. A fragment
	// is not the value, so no whole-value replacement would find it.
	kept := buffer.String()
	if strings.Contains(kept, "s3cr3t") {
		t.Fatalf("part of the value survived the tail: %q", kept)
	}
	if !strings.Contains(kept, "later output line") {
		t.Fatalf("the tail lost what a reader needs: %q", kept)
	}
	// And the whole value, when it fits, is still replaced on the way out.
	whole := &tailBuffer{limit: 4096}
	_, _ = whole.Write([]byte("token " + secret + "\n"))
	if got := cardsecret.Redact(whole.String()); strings.Contains(got, "s3cr3t") {
		t.Fatalf("a value that fits was kept: %q", got)
	}
}

// Every line of a credentials file is a secret in its own right: a tool
// prints the profile line it could not use, and the record keeps it.
func TestOneLineOfAMultiLineCredentialDoesNotSurviveAStep(t *testing.T) {
	cardsecret.Forget()
	t.Cleanup(cardsecret.Forget)
	file := "[dev]\naws_access_key_id = AKIAEXAMPLEEXAMPLE\naws_secret_access_key = wJalrXUtnFEMIexampleKEY\n"
	cardsecret.Register([]cardsecret.Entry{{Name: "AWS_SHARED_CREDENTIALS_FILE", Secret: file}})

	script := filepath.Join(t.TempDir(), "worker")
	body := "#!/bin/sh\necho 'profile load failed: aws_secret_access_key = wJalrXUtnFEMIexampleKEY' >&2\nexit 5\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	pipeline := chainStagePipeline(t)
	pipeline.Config.WorkerBin = script
	pipeline.Config.ConsumerConfigPath = writeChainConsumerConfig(t, []string{"review-a", "review-b"})
	pipeline.StageCredentials = []string{"AWS_SHARED_CREDENTIALS_FILE=" + file}
	sealStageFiles(t, pipeline, 1, "")
	err := pipeline.chainReview(context.Background(), []string{"review-a", "review-b"}, 0, pipeline.path("target-repo"), strings.Repeat("ab", 20))
	if err == nil {
		t.Fatal("the failing step was reported as a success")
	}
	if strings.Contains(pipeline.lastStepStderr, "wJalrXUtnFEMIexampleKEY") {
		t.Fatalf("a line of the credentials file survived: %q", pipeline.lastStepStderr)
	}
	if !strings.Contains(pipeline.lastStepStderr, "profile load failed") {
		t.Fatalf("the reason the card failed was lost with it: %q", pipeline.lastStepStderr)
	}
}

// The pipeline registers what it hands over, as a backstop for the card's
// own registration. A variable holding a file's name must not be registered
// as a secret there either: every build line that mentions the file would
// disappear from the live log, and a path-mode credential exists precisely
// so a tool can be told which file to read.
func TestThePipelineDoesNotRegisterAFileNameAsASecret(t *testing.T) {
	cardsecret.Forget()
	t.Cleanup(cardsecret.Forget)
	// The card's entry registered the mode and the contents when it read
	// the file; the pipeline sees only the assignment.
	path := filepath.Join(t.TempDir(), "cloud-credentials")
	cardsecret.Register([]cardsecret.Entry{{
		Name: "AWS_SHARED_CREDENTIALS_FILE", Path: true,
		Secret: "[dev]\naws_secret_access_key = wJalrXUtnFEMIexampleKEY\n",
	}})

	pipeline := chainStagePipeline(t)
	pipeline.Config.WorkerBin = "true"
	pipeline.Config.ConsumerConfigPath = writeChainConsumerConfig(t, []string{"review-a", "review-b"})
	pipeline.StageCredentials = []string{"AWS_SHARED_CREDENTIALS_FILE=" + path}
	sealStageFiles(t, pipeline, 1, "")
	if err := pipeline.chainReview(context.Background(), []string{"review-a", "review-b"}, 0, pipeline.path("target-repo"), strings.Repeat("ab", 20)); err != nil {
		t.Fatalf("chainReview: %v", err)
	}
	if got := cardsecret.Redact("aws: reading " + path); !strings.Contains(got, path) {
		t.Fatalf("the file name was masked out of the logs: %q", got)
	}
	// And what is in the file is still secret.
	if got := cardsecret.Redact("profile load failed: aws_secret_access_key = wJalrXUtnFEMIexampleKEY"); strings.Contains(got, "wJalrXUtnFEMIexampleKEY") {
		t.Fatalf("the contents behind the path stopped being secret: %q", got)
	}
}
