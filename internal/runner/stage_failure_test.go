package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/worker"
)

// A volume with no room left arrives wrapped in whatever the pipeline was
// doing at the time, so the class has to come out of the error chain rather
// than the outermost sentence.
func TestClassifyStageFailureReadsAFullVolumeThroughItsWrappers(t *testing.T) {
	wrapped := fmt.Errorf("the implemented change could not be sealed: %w",
		&verbFailure{verb: "seal-candidate", code: -1,
			err: fmt.Errorf("step seal-candidate could not run: %w", syscall.ENOSPC)})
	if class := classifyStageFailure(wrapped); class != FailureClassDisk {
		t.Fatalf("classifyStageFailure(ENOSPC) = %q", class)
	}
	// The same fact told in words, which is how a child process reports it.
	spoken := &verbFailure{verb: "run-instruction", code: 1,
		stderr: "cp: write error: No space left on device\n"}
	if class := classifyStageFailure(spoken); class != FailureClassDisk {
		t.Fatalf("classifyStageFailure(no space left) = %q", class)
	}
}

// A binary that is not there is a different remedy from a model that will not
// answer, and the two used to arrive as the same sentence.
func TestClassifyStageFailureReadsAMissingBinaryAsATool(t *testing.T) {
	wrapped := fmt.Errorf("review by review-a did not finish: %w",
		&verbFailure{verb: "agent-review", code: -1,
			err: fmt.Errorf("step agent-review could not run: %w", exec.ErrNotFound)})
	if class := classifyStageFailure(wrapped); class != FailureClassTool {
		t.Fatalf("classifyStageFailure(ErrNotFound) = %q", class)
	}
}

// A verb whose whole job is a model turn, exiting non-zero with nothing
// readable behind it, is still a model failure.
func TestClassifyStageFailureReadsAModelVerbsNonZeroExitAsModel(t *testing.T) {
	for _, verb := range []string{"agent-review", "investigate", "run-instruction", "agent-design-review"} {
		failure := fmt.Errorf("the round stopped: %w", &verbFailure{verb: verb, code: 1})
		if class := classifyStageFailure(failure); class != FailureClassModel {
			t.Fatalf("classifyStageFailure(%s exit 1) = %q", verb, class)
		}
	}
	// A deterministic verb exiting non-zero is the machinery's own breakdown,
	// not the model's; calling it "model" would send the remedy elsewhere.
	if class := classifyStageFailure(&verbFailure{verb: "decide", code: 1}); class != FailureClassUnknown {
		t.Fatalf("classifyStageFailure(decide exit 1) = %q", class)
	}
}

// The worker's own machine-readable account of a turn that ended badly is a
// stronger statement than any word in the text around it — including the
// gateway's own 5xx, whose remedy is a different provider, not a different
// route.
func TestClassifyStageFailureBelievesTheWorkersOwnModelAccount(t *testing.T) {
	encoded, err := json.Marshal(worker.ModelFailureDetail{
		Phrase: "model invocation failed with status 503", Model: "m", Calls: 2,
		ProviderErrors: 2, LastHTTPStatus: 503,
	})
	if err != nil {
		t.Fatal(err)
	}
	failure := &verbFailure{verb: "decide", code: 1,
		stderr: worker.FailureDetailLinePrefix + string(encoded) + "\n"}
	if class := classifyStageFailure(failure); class != FailureClassModel {
		t.Fatalf("classifyStageFailure(model detail) = %q", class)
	}
}

// Anything the vocabulary does not recognise says so, rather than borrowing
// the nearest class.
func TestClassifyStageFailureSaysUnknownRatherThanGuessing(t *testing.T) {
	for _, failure := range []error{
		errors.New("no sealed candidate to review"),
		errors.New("baseline sha invalid"),
		fmt.Errorf("the decision outcome %q is not one this chain knows", "elsewhere"),
		&verbFailure{verb: "seal-candidate", code: 2},
	} {
		if class := classifyStageFailure(failure); class != FailureClassUnknown {
			t.Fatalf("classifyStageFailure(%v) = %q", failure, class)
		}
	}
	if class := classifyStageFailure(nil); class != FailureClassUnknown {
		t.Fatalf("classifyStageFailure(nil) = %q", class)
	}
}

// The validate card's own refusal is the branch it took, not the words the
// consumer's tests printed on the way out.
func TestClassifyStageFailureKeepsAConsumersRedBuildAValidationFailure(t *testing.T) {
	if class := classifyStageFailure(ErrValidationRejected); class != FailureClassValidation {
		t.Fatalf("classifyStageFailure(rejected) = %q", class)
	}
	noisy := fmt.Errorf("%w (connection refused, no space left on device)", ErrValidationRejected)
	if class := classifyStageFailure(noisy); class != FailureClassValidation {
		t.Fatalf("classifyStageFailure(noisy rejection) = %q", class)
	}
}

// Something that could not be reached is its own class: a different route is
// the remedy, and neither a reclaim nor a different seat would help.
func TestClassifyStageFailureReadsAnUnreachableThingAsNetwork(t *testing.T) {
	for _, said := range []string{
		"fatal: unable to access 'https://example.invalid/': Could not resolve host: example.invalid",
		"dial tcp 10.0.0.1:443: connect: connection refused",
		"x509: certificate signed by unknown authority",
		"the registry returned an error for this tag",
	} {
		failure := &verbFailure{verb: "seal-candidate", code: 1, stderr: said}
		if class := classifyStageFailure(failure); class != FailureClassNetwork {
			t.Fatalf("classifyStageFailure(%q) = %q", said, class)
		}
	}
}

func stageFailurePipeline(t *testing.T) *Pipeline {
	t.Helper()
	pipeline := &Pipeline{Workspace: t.TempDir(), Logger: trailTestLogger{}}
	pipeline.Config.Identity.EngineSHA = strings.Repeat("a", 40)
	draft := map[string]any{
		"delivery_id": "d-1", "input_sha256": strings.Repeat("b", 64),
		"config_sha256": strings.Repeat("c", 64),
	}
	encoded, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pipeline.Workspace, "ticket-draft.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return pipeline
}

// The publish card can only exit non-zero. Which ending the delivery named is
// the difference between the destination refusing the change and the
// machinery breaking; the code used to be collapsed into a sentence on one
// arm and dropped outright on the other, so from outside the process the two
// were the same failure.
func TestPublishKeepsItsDeliveryCodeApart(t *testing.T) {
	for _, situation := range []struct {
		name    string
		arrange func(t *testing.T, pipeline *Pipeline)
		want    hook.TerminalCode
	}{
		{
			name:    "the destination refused the change",
			arrange: func(*testing.T, *Pipeline) {},
			want:    hook.TerminalReleaseFailed,
		},
		{
			name: "the machinery could not clear its own workspace",
			arrange: func(t *testing.T, pipeline *Pipeline) {
				// The refusal record's path is not a file it can replace, so
				// the publish never starts. That is the machinery's own
				// breakdown and it reports a different ending.
				squatted := pipeline.path(publishFailureFile)
				if err := os.MkdirAll(filepath.Join(squatted, "occupied"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want: hook.TerminalInternalFailed,
		},
	} {
		t.Run(situation.name, func(t *testing.T) {
			pipeline := cardPipeline(t, writeFakeWorker(t, "exit 0\n"))
			pipeline.Config.ControllerBin = writeFakeWorker(t, "exit 1\n")
			if err := os.WriteFile(pipeline.path("history/stage-1/decision.json"),
				[]byte(`{"outcome":"converged"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			situation.arrange(t, pipeline)
			if err := pipeline.RunChainStage(context.Background(), runtime.StagePublish); err == nil {
				t.Fatal("the publish card did not fail")
			}
			record, ok := ReadStageFailure(pipeline.Workspace, runtime.StagePublish, 1)
			if !ok {
				t.Fatal("the publish card sealed no account of its failure")
			}
			if record.TerminalCode != string(situation.want) {
				t.Fatalf("record terminal code = %q, want %q (error %q)",
					record.TerminalCode, situation.want, record.Error)
			}
			if record.DeliveryID != "d-1" || record.ToolSHA != strings.Repeat("a", 40) {
				t.Fatalf("record lost its binding: %+v", record)
			}
		})
	}
}

// The account goes to the round the stage's own records go to, and to the
// design half for the stages that belong to it.
func TestSealStageFailureWritesBesideTheRoundsOtherRecords(t *testing.T) {
	pipeline := stageFailurePipeline(t)
	for _, directory := range []string{"history/stage-1", "history/design-1"} {
		if err := os.MkdirAll(filepath.Join(pipeline.Workspace, directory), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	pipeline.SealStageFailure(runtime.StageReviewA, ErrValidationRejected)
	pipeline.SealStageFailure(runtime.StageInvestigate, &verbFailure{verb: "investigate", code: 1})
	implementation, ok := ReadStageFailure(pipeline.Workspace, runtime.StageReviewA, 1)
	if !ok || implementation.Class != FailureClassValidation {
		t.Fatalf("implementation record = %+v, %v", implementation, ok)
	}
	design, ok := ReadStageFailure(pipeline.Workspace, runtime.StageInvestigate, 1)
	if !ok || design.Class != FailureClassModel {
		t.Fatalf("design record = %+v, %v", design, ok)
	}
	if _, err := os.Stat(filepath.Join(pipeline.Workspace, "history", "design-1", "investigate-failure.json")); err != nil {
		t.Fatalf("design account not beside its round: %v", err)
	}
}

// A pod being replaced cancels whatever every card was doing, and for a
// verb that spends a model turn the class reads that as a model failure —
// from inside the process it is one, because the turn did not finish. The
// record says which it was, so a reader deciding the model will not answer
// does not count a rolling restart towards it.
func TestSealStageFailureSaysWhenTheCardWasStoppedRatherThanFailed(t *testing.T) {
	pipeline := stageFailurePipeline(t)
	if err := os.MkdirAll(filepath.Join(pipeline.Workspace, "history", "stage-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name        string
		stage       string
		failure     error
		interrupted bool
	}{
		{"the pod was replaced mid-turn", runtime.StageReviewA,
			&verbFailure{verb: "agent-review", err: context.Canceled}, true},
		{"the card met its own wall", runtime.StageReviewB,
			&verbFailure{verb: "agent-review", err: context.DeadlineExceeded}, true},
		{"the provider gave up", runtime.StageValidate,
			&verbFailure{verb: "agent-review", code: 1}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pipeline.SealStageFailure(tc.stage, tc.failure)
			record, ok := ReadStageFailure(pipeline.Workspace, tc.stage, 1)
			if !ok {
				t.Fatal("nothing was sealed")
			}
			if record.Interrupted != tc.interrupted {
				t.Fatalf("interrupted = %v, want %v (class %q)", record.Interrupted, tc.interrupted, record.Class)
			}
			// The class still says what kind of thing it was, which is what
			// makes this a second fact rather than a replacement for one.
			if record.Class != FailureClassModel {
				t.Fatalf("class = %q, want the model verb still read as a model failure", record.Class)
			}
		})
	}
	// And the flag is inside the digest: an account altered after the fact
	// is refused, not read.
	path := StageFailureFile(pipeline.Workspace, runtime.StageValidate, 1)
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	forged := strings.Replace(string(encoded), `"error":`, `"interrupted":true,"error":`, 1)
	if forged == string(encoded) {
		t.Fatal("the record could not be altered; the test measures nothing")
	}
	if err := os.WriteFile(path, []byte(forged), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadStageFailure(pipeline.Workspace, runtime.StageValidate, 1); ok {
		t.Fatal("an account given the flag after it was sealed was read as sealed")
	}
}

// The same thing, measured through a real child process rather than a
// hand-built failure.
//
// It is worth its own test because the two disagreed. A step killed by a
// signal comes back from Cmd.Run as an exit error — Wait prefers the
// process's own ending over the context's — so the step returns an exit code
// with no error beside it, and every hand-built check above passed while the
// only thing that actually runs recorded a rolling restart as a provider
// that would not answer.
func TestAStepKilledByItsContextIsSealedAsInterrupted(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stage   string
		context func() (context.Context, context.CancelFunc)
	}{
		{"the pod was replaced mid-step", runtime.StageReviewA, func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(75*time.Millisecond, cancel)
			return ctx, cancel
		}},
		{"the step met its own wall", runtime.StageReviewB, func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 75*time.Millisecond)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pipeline := stageFailurePipeline(t)
			pipeline.Config.WorkerBin = sleepingBinary(t)
			ctx, cancel := tc.context()
			defer cancel()
			err := pipeline.runVerb(ctx, "agent-review", []string{"agent-review"})
			if err == nil {
				t.Fatal("a step killed mid-run returned no failure")
			}
			pipeline.SealStageFailure(tc.stage, err)
			record, ok := ReadStageFailure(pipeline.Workspace, tc.stage, 1)
			if !ok {
				t.Fatal("nothing was sealed")
			}
			if !record.Interrupted {
				t.Fatalf("interrupted = false for a step its context killed (class %q, error %q)", record.Class, record.Error)
			}
			// The class is untouched: what kind of thing it was is still
			// worth knowing about a card that was stopped.
			if record.Class != FailureClassModel {
				t.Fatalf("class = %q, want the model verb still read as a model failure", record.Class)
			}
		})
	}
	// A step that ran to a non-zero ending of its own, under a context that
	// is still good, is not interrupted. Without this the fix could be "say
	// interrupted whenever the exit code is negative" and pass.
	pipeline := stageFailurePipeline(t)
	pipeline.Config.WorkerBin = refusingBinary(t)
	err := pipeline.runVerb(context.Background(), "agent-review", []string{"agent-review"})
	if err == nil {
		t.Fatal("a step that exited non-zero returned no failure")
	}
	pipeline.SealStageFailure(runtime.StageValidate, err)
	record, ok := ReadStageFailure(pipeline.Workspace, runtime.StageValidate, 1)
	if !ok || record.Interrupted {
		t.Fatalf("an ordinary non-zero exit was sealed as interrupted: %+v", record)
	}
}

// sleepingBinary outlives any wait this test has patience for, so the only
// thing that ends it is its context.
func sleepingBinary(t *testing.T) string {
	t.Helper()
	return executableScript(t, "sleeping", "#!/bin/sh\nsleep 30\n")
}

// refusingBinary ends by itself, promptly, non-zero.
func refusingBinary(t *testing.T) string {
	t.Helper()
	return executableScript(t, "refusing", "#!/bin/sh\nexit 3\n")
}

func executableScript(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// A run directory outlives its cards. A record that does not name the stage
// and round it was asked for, or that was cut short, explains nothing and is
// refused rather than read.
func TestReadStageFailureRefusesAnAccountThatDoesNotBind(t *testing.T) {
	pipeline := stageFailurePipeline(t)
	if err := os.MkdirAll(filepath.Join(pipeline.Workspace, "history", "stage-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	pipeline.SealStageFailure(runtime.StageReviewA, errors.New("nothing in particular"))
	if _, ok := ReadStageFailure(pipeline.Workspace, runtime.StageReviewB, 1); ok {
		t.Fatal("another stage's account was read as this one's")
	}
	if _, ok := ReadStageFailure(pipeline.Workspace, runtime.StageReviewA, 2); ok {
		t.Fatal("another round's account was read as this one's")
	}
	path := StageFailureFile(pipeline.Workspace, runtime.StageReviewA, 1)
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(encoded), `"unknown"`, `"disk"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadStageFailure(pipeline.Workspace, runtime.StageReviewA, 1); ok {
		t.Fatal("an altered account was read as sealed")
	}
}

// A stage name is part of a path. One that is not the chain's own never
// reaches the filesystem.
func TestSealStageFailureRefusesAStageTheChainDoesNotRun(t *testing.T) {
	pipeline := stageFailurePipeline(t)
	pipeline.SealStageFailure("../../escape", errors.New("nothing in particular"))
	entries, err := os.ReadDir(pipeline.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "ticket-draft.json" {
			t.Fatalf("an unknown stage wrote %q", entry.Name())
		}
	}
}

// The error text is kept, bounded, and the failed step's own output is not:
// upstream text belongs in no record.
func TestSealStageFailureKeepsTheSentenceBoundedAndTheOutputOut(t *testing.T) {
	pipeline := stageFailurePipeline(t)
	if err := os.MkdirAll(filepath.Join(pipeline.Workspace, "history", "stage-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	secretish := "sk-" + strings.Repeat("z", 64)
	pipeline.SealStageFailure(runtime.StageImplement, fmt.Errorf("%s: %w", strings.Repeat("長", 2000),
		&verbFailure{verb: "run-instruction", code: 1, stderr: "authorization: Bearer " + secretish}))
	record, ok := ReadStageFailure(pipeline.Workspace, runtime.StageImplement, 1)
	if !ok {
		t.Fatal("no record sealed")
	}
	if len(record.Error) > maxStageFailureErrorBytes {
		t.Fatalf("error text is %d bytes", len(record.Error))
	}
	if !strings.Contains(record.Error, "長") || strings.Contains(record.Error, secretish) {
		t.Fatalf("record kept the wrong text: %q", record.Error)
	}
}

// cardPipeline is a chain card's whole world: a consumer whose delivery the
// pod runtime ships, a baseline, a draft that points at it, and one sealed
// round for the review card to work on.
func cardPipeline(t *testing.T, workerBin string) *Pipeline {
	t.Helper()
	pipeline := stageFailurePipeline(t)
	pipeline.Config.WorkerBin = workerBin
	consumer, err := json.Marshal(map[string]any{
		"max_stages": 3,
		"consumers": []map[string]any{
			{"repository": "example/consumer", "delivery": "pull_request"},
		},
		"models": map[string]any{"reviewers": []map[string]any{{"id": "review-a"}, {"id": "review-b"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	pipeline.Config.ConsumerConfigPath = filepath.Join(t.TempDir(), "consumer.json")
	if err := os.WriteFile(pipeline.Config.ConsumerConfigPath, consumer, 0o600); err != nil {
		t.Fatal(err)
	}
	draft, err := json.Marshal(map[string]any{
		"delivery_id": "d-1", "repository": "example/consumer",
		"input_sha256": strings.Repeat("b", 64), "config_sha256": strings.Repeat("c", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pipeline.path("ticket-draft.json"), draft, 0o600); err != nil {
		t.Fatal(err)
	}
	baseline := `{"baseline":{"Integration":{"SHA":"` + strings.Repeat("d", 40) + `"}}}`
	if err := os.WriteFile(pipeline.path("baseline.json"), []byte(baseline), 0o600); err != nil {
		t.Fatal(err)
	}
	stageDir := pipeline.path("history/stage-1")
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stageDir, "candidate.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return pipeline
}

func writeFakeWorker(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-worker")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// The card itself writes the account, on the way out, from what the step it
// ran actually said. Nothing above it has to be asked.
func TestRunChainStageSealsWhyItFailed(t *testing.T) {
	detail, err := json.Marshal(worker.ModelFailureDetail{
		Phrase: "the model answered nothing usable", Model: "m", Calls: 3, Malformed: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, situation := range []struct {
		name   string
		script string
		want   FailureClass
	}{
		{"a full volume", "echo 'fatal: write error: No space left on device' >&2\nexit 1\n", FailureClassDisk},
		{"an unreachable host", "echo 'dial tcp: connect: connection refused' >&2\nexit 1\n", FailureClassNetwork},
		{"a model that answered nothing usable",
			"echo '" + worker.FailureDetailLinePrefix + string(detail) + "' >&2\nexit 1\n", FailureClassModel},
		{"a model verb that said nothing at all", "exit 1\n", FailureClassModel},
	} {
		t.Run(situation.name, func(t *testing.T) {
			pipeline := cardPipeline(t, writeFakeWorker(t, situation.script))
			if err := pipeline.RunChainStage(context.Background(), runtime.StageReviewB); err == nil {
				t.Fatal("the card did not fail")
			}
			record, ok := ReadStageFailure(pipeline.Workspace, runtime.StageReviewB, 1)
			if !ok {
				t.Fatal("the card sealed no account of its failure")
			}
			if record.Class != situation.want {
				t.Fatalf("class = %q, want %q (error %q)", record.Class, situation.want, record.Error)
			}
			if record.DeliveryID != "d-1" || record.Round != 1 || record.Stage != runtime.StageReviewB {
				t.Fatalf("record does not bind to its round: %+v", record)
			}
		})
	}
	// A worker binary that is not there at all: the step never starts, and
	// the cause used to be dropped at the call site.
	pipeline := cardPipeline(t, filepath.Join(t.TempDir(), "absent-worker"))
	if err := pipeline.RunChainStage(context.Background(), runtime.StageReviewB); err == nil {
		t.Fatal("the card did not fail")
	}
	record, ok := ReadStageFailure(pipeline.Workspace, runtime.StageReviewB, 1)
	if !ok || record.Class != FailureClassTool {
		t.Fatalf("missing binary = %+v, %v", record, ok)
	}
}

// The decide card seals its round's decision and then fails on what it says.
// The account has to stay in the round that was decided: by then the first
// undecided design round is the next one, and nothing looking at the failed
// card would think to look there.
func TestDesignDecideSealsItsAccountInTheRoundItDecided(t *testing.T) {
	pipeline := stageFailurePipeline(t)
	roundDir := pipeline.path("history/design-1")
	if err := os.MkdirAll(roundDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"investigation.json": `{}`,
		"decision.json":      `{"outcome":"revise"}`,
	} {
		if err := os.WriteFile(filepath.Join(roundDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if pipeline.currentDesignRound() != 2 {
		t.Fatalf("the decided round is still the current one: %d", pipeline.currentDesignRound())
	}
	pipeline.SealStageFailure(runtime.StageDesignDecide, errors.New("the design was sent back for revision"))
	if _, ok := ReadStageFailure(pipeline.Workspace, runtime.StageDesignDecide, 1); !ok {
		t.Fatal("the decided round has no account of the card that failed on it")
	}
}

// A key that has reached its spending limit refuses like a provider error and
// answers to none of the same remedies: every seat behind that key meets the
// same wall, and only a person raising the limit moves it.
func TestClassifyStageFailureReadsASpentKeyAsCredit(t *testing.T) {
	said := "model invocation failed with status 402: {\"error\":{\"code\":402," +
		"\"message\":\"Insufficient credits to run this request.\"}}"
	if class := classifyStageFailure(&verbFailure{verb: "agent-review", code: 1, stderr: said}); class != FailureClassCredit {
		t.Fatalf("classifyStageFailure(402, insufficient credits) = %q", class)
	}
	for _, refusal := range []string{
		"402 Payment Required",
		"model invocation failed with status 402",
		"the gateway answered http 402",
		"{\"error\":{\"code\":402,\"message\":\"see your dashboard\"}}",
		"your key limit has been reached",
		"monthly quota exceeded for this account",
		"billing: this organisation has no active payment method",
	} {
		failure := fmt.Errorf("review by review-a did not finish: %w",
			&verbFailure{verb: "agent-review", code: 1, stderr: refusal})
		if class := classifyStageFailure(failure); class != FailureClassCredit {
			t.Fatalf("classifyStageFailure(%q) = %q", refusal, class)
		}
	}
	// The status the worker parsed out of the answer decides on its own, with
	// no words to read at all. Through a sealed record the number is restated
	// in the record's own text, so the two halves are only separable here.
	if !spendingLimitReached("model invocation failed", worker.ModelFailureDetail{LastHTTPStatus: 402}) {
		t.Fatal("a parsed 402 did not read as a spending limit on its own")
	}
	// The status the worker parsed out of the answer says it on its own, with
	// no words to read.
	encoded, err := json.Marshal(worker.ModelFailureDetail{
		Phrase: "model invocation failed with status 402", Model: "m", Calls: 1,
		ProviderErrors: 1, LastHTTPStatus: 402,
	})
	if err != nil {
		t.Fatal(err)
	}
	structured := &verbFailure{verb: "agent-review", code: 1,
		stderr: worker.FailureDetailLinePrefix + string(encoded) + "\n"}
	if class := classifyStageFailure(structured); class != FailureClassCredit {
		t.Fatalf("classifyStageFailure(status 402) = %q", class)
	}
	// A volume out of room says "disk quota exceeded" and is still a volume:
	// the reclaim is the remedy, and it is read before any of this.
	full := &verbFailure{verb: "run-instruction", code: 1, stderr: "write failed: disk quota exceeded"}
	if class := classifyStageFailure(full); class != FailureClassDisk {
		t.Fatalf("classifyStageFailure(disk quota) = %q", class)
	}
	// A provider asking for a pause is not a provider asking for money, and
	// it says so in words that would otherwise read as one.
	for _, paused := range []string{
		"429 Too Many Requests: rate limit exceeded for this model",
		"rate_limit_error: quota exceeded for requests per minute",
	} {
		failure := &verbFailure{verb: "agent-review", code: 1, stderr: paused}
		if class := classifyStageFailure(failure); class == FailureClassCredit {
			t.Fatalf("classifyStageFailure(%q) = credit", paused)
		}
	}
	// The dangerous one: a burst refused in the very words a spent key uses,
	// printed where words are read. The status the worker parsed is the only
	// thing that tells them apart.
	throttled, err := json.Marshal(worker.ModelFailureDetail{
		Phrase: worker.TransportFailedPhrase, Model: "m", Calls: 2,
		ProviderErrors: 2, LastHTTPStatus: 429,
	})
	if err != nil {
		t.Fatal(err)
	}
	paused := &verbFailure{verb: "agent-review", code: 1,
		stderr: "quota exceeded for requests per minute\n" +
			worker.FailureDetailLinePrefix + string(throttled) + "\n"}
	if class := classifyStageFailure(paused); class != FailureClassModel {
		t.Fatalf("classifyStageFailure(status 429) = %q", class)
	}
	// The numbers a turn reports sit in the same line as the status it got,
	// and several of them can honestly be 402. Each of these is a model
	// answer that went wrong; read as a key out of money, the ladder would
	// stop and wait for a person on a run nothing was stopping.
	for _, honest := range []worker.ModelFailureDetail{
		{Phrase: worker.AnswerUnusablePhrase, Malformed: 1, MaxOutputTokens: 402},
		{Phrase: worker.AnswerUnusablePhrase, Malformed: 1, MaxOutputTokens: 8402, FinalMaxOutputTokens: 8402},
		{Phrase: worker.AnswerUnusablePhrase, Malformed: 1, LastCompletionTokens: 402},
		{Phrase: worker.AnswerUnusablePhrase, Malformed: 1, LastRequestID: "gen-x402y"},
	} {
		honest.Model = "m"
		honest.Calls = 1
		encoded, err := json.Marshal(honest)
		if err != nil {
			t.Fatal(err)
		}
		failure := &verbFailure{verb: "agent-review", code: 1,
			stderr: worker.FailureDetailLinePrefix + string(encoded) + "\n"}
		if class := classifyStageFailure(failure); class != FailureClassModel {
			t.Fatalf("classifyStageFailure(%s) = %q", string(encoded), class)
		}
	}
	// A plain non-zero exit from a model verb, with none of the words and no
	// number at all, is a model failure and not a refusal over money.
	if class := classifyStageFailure(&verbFailure{verb: "agent-review", code: 1}); class != FailureClassModel {
		t.Fatalf("classifyStageFailure(bare model exit) = %q", class)
	}
	// A turn that spent its own allowance is still a model failure: the
	// phrase for it names no limit that a payment lifts.
	spent, err := json.Marshal(worker.ModelFailureDetail{
		Phrase: worker.TransportFailedPhrase + ": " + worker.SpentAllowancePhrase,
		Model:  "m", Calls: 4, AllowanceSpent: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	allowance := &verbFailure{verb: "agent-review", code: 1,
		stderr: worker.FailureDetailLinePrefix + string(spent) + "\n"}
	if class := classifyStageFailure(allowance); class != FailureClassModel {
		t.Fatalf("classifyStageFailure(allowance spent) = %q", class)
	}
}

// A word on its own is not a route that could not be reached. Narrowed after
// review: "registry" alone appears in changes that have nothing to do with
// one, and a network class sends the remedy looking for another endpoint.
func TestClassifyStageFailureNeedsMoreThanTheWordRegistry(t *testing.T) {
	mentioned := &verbFailure{verb: "seal-candidate", code: 1,
		stderr: "the plugin registry entry was rewritten and the fixture no longer matches"}
	if class := classifyStageFailure(mentioned); class != FailureClassUnknown {
		t.Fatalf("classifyStageFailure(a mentioned registry) = %q", class)
	}
}

// The publish card reaches the destination through commands of its own. A
// delivery that could not get there used to arrive with its code and nothing
// else — a completed non-zero run returns no error — so every one of them
// sealed as "unknown" whatever had happened.
func TestPublishSealsWhyTheDestinationWasNotReached(t *testing.T) {
	pipeline := cardPipeline(t, writeFakeWorker(t, "exit 0\n"))
	pipeline.Config.ControllerBin = writeFakeWorker(t,
		"echo \"fatal: unable to access 'https://example.invalid/': "+
			"Failed to connect to example.invalid port 443: Connection refused\" >&2\nexit 1\n")
	if err := os.WriteFile(pipeline.path("history/stage-1/decision.json"),
		[]byte(`{"outcome":"converged"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.RunChainStage(context.Background(), runtime.StagePublish); err == nil {
		t.Fatal("the publish card did not fail")
	}
	record, ok := ReadStageFailure(pipeline.Workspace, runtime.StagePublish, 1)
	if !ok {
		t.Fatal("the publish card sealed no account of its failure")
	}
	if record.Class != FailureClassNetwork {
		t.Fatalf("class = %q, want network (error %q)", record.Class, record.Error)
	}
	// And the delivery's own ending is still in the record beside it.
	if record.TerminalCode != string(hook.TerminalReleaseFailed) {
		t.Fatalf("record terminal code = %q", record.TerminalCode)
	}
}

// A record is refused unless it names the stage and round it was found under.
// A run directory outlives its cards and holds every round side by side, so a
// neighbour's account sitting at the wrong name explains the wrong failure.
func TestReadStageFailureRefusesANeighboursAccountUnderTheWrongName(t *testing.T) {
	pipeline := stageFailurePipeline(t)
	if err := os.MkdirAll(filepath.Join(pipeline.Workspace, "history", "stage-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	pipeline.SealStageFailure(runtime.StageReviewA, ErrValidationRejected)
	sealed, err := os.ReadFile(StageFailureFile(pipeline.Workspace, runtime.StageReviewA, 1))
	if err != nil {
		t.Fatal(err)
	}
	// The same bytes, digest and all, under the next reviewer's name.
	if err := os.WriteFile(StageFailureFile(pipeline.Workspace, runtime.StageReviewB, 1), sealed, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadStageFailure(pipeline.Workspace, runtime.StageReviewB, 1); ok {
		t.Fatal("one card's account was read as another card's")
	}
	// And under another round of its own stage.
	if err := os.MkdirAll(filepath.Join(pipeline.Workspace, "history", "stage-2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(StageFailureFile(pipeline.Workspace, runtime.StageReviewA, 2), sealed, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadStageFailure(pipeline.Workspace, runtime.StageReviewA, 2); ok {
		t.Fatal("one round's account was read as another round's")
	}
	// The one it does name still reads.
	if _, ok := ReadStageFailure(pipeline.Workspace, runtime.StageReviewA, 1); !ok {
		t.Fatal("the account was refused under its own name")
	}
}

// The seal writes into a run directory the preparation made. Where there is
// none, it writes nothing: a card that failed because its workspace is gone
// would otherwise build one on the way out, holding a single file that
// explains a run nothing else knows about.
func TestSealStageFailureBuildsNoRunDirectoryOfItsOwn(t *testing.T) {
	parent := t.TempDir()
	pipeline := &Pipeline{Workspace: filepath.Join(parent, "gone"), Logger: trailTestLogger{}}
	pipeline.SealStageFailure(runtime.StageReviewA, errors.New("nothing in particular"))
	if _, err := os.Stat(pipeline.Workspace); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the seal made a workspace: %v", err)
	}
	// Nor where the workspace is something other than a directory.
	file := filepath.Join(parent, "a-file")
	if err := os.WriteFile(file, []byte("not a run directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	notADirectory := &Pipeline{Workspace: file, Logger: trailTestLogger{}}
	notADirectory.SealStageFailure(runtime.StageReviewA, errors.New("nothing in particular"))
	body, err := os.ReadFile(file)
	if err != nil || string(body) != "not a run directory" {
		t.Fatalf("the seal wrote through a workspace that is not one: %q %v", body, err)
	}
}

// A design round is made 0o700 by the card that owns it. A seal that gets
// there first stands in for that card and must not leave a wider directory
// behind than it would have.
func TestSealStageFailureLeavesTheRoundTheModeItsCardWouldHave(t *testing.T) {
	pipeline := stageFailurePipeline(t)
	pipeline.SealStageFailure(runtime.StageInvestigate, errors.New("nothing in particular"))
	info, err := os.Stat(filepath.Join(pipeline.Workspace, "history", "design-1"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("design round mode = %v", info.Mode().Perm())
	}
}

// The sentence is cut to a character, the way the report's own trail is.
func TestBoundedFailureTextCutsOnARune(t *testing.T) {
	// A multi-byte rune straddling the bound: the cut backs off onto it
	// rather than leaving half of it behind.
	text := strings.Repeat("a", maxStageFailureErrorBytes-1) + "長"
	cut := boundedFailureText(text)
	if len(cut) > maxStageFailureErrorBytes || !utf8.ValidString(cut) {
		t.Fatalf("cut is %d bytes, valid=%v", len(cut), utf8.ValidString(cut))
	}
	if strings.HasSuffix(cut, "長") {
		t.Fatal("a rune the bound cannot hold was kept whole")
	}
	if cut != strings.Repeat("a", maxStageFailureErrorBytes-1) {
		t.Fatalf("the cut lost more than the rune it could not keep: %d bytes", len(cut))
	}
}

// The validate card owns one failure: the deterministic verification ran and
// would not pass the round. That is the branch the card took, never the words
// the consumer's own checks printed on the way out — a repository whose tests
// say "connection refused" must not turn its own red build into a route that
// could not be reached.
func TestValidationsRefusalStaysItsOwnAnswer(t *testing.T) {
	repository, baseSHA := gitBaseRepo(t)
	steps := writeFakeWorker(t, `
verb="$1"
if [ "$verb" = "run-validation" ]; then
  echo "dial tcp 10.0.0.1:443: connect: connection refused" >&2
  exit 1
fi
exit 0
`)
	pipeline := cardPipeline(t, steps)
	baseline := `{"baseline":{"Integration":{"SHA":"` + baseSHA + `"}}}`
	if err := os.WriteFile(pipeline.path("baseline.json"), []byte(baseline), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pipeline.path("history/stage-1/decision.json"),
		[]byte(`{"outcome":"converged"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	pipeline.cloneTarget = func(ctx context.Context, destination string) error {
		return exec.CommandContext(ctx, "git", "clone", "-q", repository, destination).Run()
	}
	t.Cleanup(func() {
		if err := forceRemoveAll(pipeline.path("validation-target")); err != nil {
			t.Error(err)
		}
	})
	err := pipeline.RunChainStage(context.Background(), runtime.StageValidate)
	if !errors.Is(err, ErrValidationRejected) {
		t.Fatalf("RunChainStage(validate) = %v", err)
	}
	record, ok := ReadStageFailure(pipeline.Workspace, runtime.StageValidate, 1)
	if !ok {
		t.Fatal("the validate card sealed no account of its failure")
	}
	if record.Class != FailureClassValidation {
		t.Fatalf("class = %q, want validation (error %q)", record.Class, record.Error)
	}
}

// The worker's account of a turn carries the head of the model's last answer,
// and a request reaches that answer: a page about payments, a branch about
// quotas, a balance in a billing file. Read for words, that line lets an
// answer name its own failure class — and this class stops the run and waits
// for a person. The line is read through its parsed fields only.
func TestClassifyStageFailureWillNotLetAnAnswerNameItsOwnClass(t *testing.T) {
	for _, objection := range []string{
		"model response content is invalid (answer 3 of 3, request req-1, " +
			"began: the credit balance check in billing.go is wrong)",
		"model response content is invalid (answer 3 of 3, request req-1, " +
			"began: the quota exceeded branch is never taken)",
		"model response content is invalid (answer 3 of 3, request req-1, " +
			"began: findings about the payment required page)",
	} {
		encoded, err := json.Marshal(worker.ModelFailureDetail{
			Phrase: worker.AnswerUnusablePhrase, Model: "m", Calls: 3, Malformed: 3,
			LastHTTPStatus: 200, Objection: objection,
		})
		if err != nil {
			t.Fatal(err)
		}
		failure := &verbFailure{verb: "agent-review", code: 1,
			stderr: worker.FailureDetailLinePrefix + string(encoded) + "\n"}
		if class := classifyStageFailure(failure); class != FailureClassModel {
			t.Fatalf("classifyStageFailure(objection %q) = %q", objection, class)
		}
	}
	// And the numbers a stage prints are its own. An applier working on an
	// order or a delivery writes them to its output, where they are neither a
	// status nor a refusal.
	for _, written := range []string{
		`{"status_code":402,"unit":"parcels awaiting pickup"}`,
		`{"order_code": 402, "state": "awaiting pickup"}`,
	} {
		failure := fmt.Errorf("the applier did not finish: %w",
			&verbFailure{verb: "run-instruction", code: 1, stderr: written})
		if class := classifyStageFailure(failure); class != FailureClassModel {
			t.Fatalf("classifyStageFailure(%s) = %q", written, class)
		}
	}
	// A provider's own body is not that line, so a refusal still reads.
	body := &verbFailure{verb: "agent-review", code: 1,
		stderr: `{"error":{"code":402,"message":"Insufficient credits"}}`}
	if class := classifyStageFailure(body); class != FailureClassCredit {
		t.Fatalf("classifyStageFailure(a provider body) = %q", class)
	}
}
