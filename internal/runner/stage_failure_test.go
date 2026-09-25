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
		"registry returned 503 service unavailable",
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
