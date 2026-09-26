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
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/worker"
)

func arbitrationCardFixture(t *testing.T) *Pipeline {
	t.Helper()
	p := cardPipeline(t, "")
	write := func(path string, value any) {
		t.Helper()
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, encoded, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for round := 1; round <= 2; round++ {
		sealStageFiles(t, p, round, "revise")
		dir := p.path(fmt.Sprintf("history/stage-%d", round))
		write(filepath.Join(dir, "candidate.json"), worker.Candidate{Stage: round, Files: []worker.CandidateFile{{Path: "README.md", Content: fmt.Sprint(round)}}})
		for _, id := range []string{"review-a", "review-b"} {
			write(filepath.Join(dir, id+".json"), worker.Review{Stage: round, ReviewerID: id, Verdict: "revise",
				Findings: []worker.ModelFinding{{Code: "missing-behaviour", Path: "README.md", Message: "Required behaviour is missing."}}})
		}
	}
	write(p.path("baseline.json"), map[string]any{"baseline": map[string]any{"Integration": map[string]string{"SHA": strings.Repeat("a", 40)}}})
	p.Config.WorkerBin = filepath.Join(t.TempDir(), "worker")
	script := `#!/bin/sh
printf '%s\n' "$*" >> '` + p.Config.WorkerBin + `.log'
out=''
prev=''
for arg in "$@"; do
  [ "$prev" = '--out' ] && out="$arg"
  prev="$arg"
done
case "$1" in
  arbitrate) printf '%s' '{"stage":2,"ruling":"instruct_implementer","instruction":"Implement the missing behaviour.","ruling_sha256":"fixture-ruling"}' > "$out" ;;
  *) exit 91 ;;
esac
`
	if err := os.WriteFile(p.Config.WorkerBin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestArbitrationRunsInsideTheValidationCard(t *testing.T) {
	p := arbitrationCardFixture(t)
	if err := p.RunChainStage(context.Background(), runtime.StageValidate); err == nil || !strings.Contains(err.Error(), "sent back for revision") {
		t.Fatalf("an instruction ruling must still send the change back: %v", err)
	}
	ruling, err := ReadRuling(p.Workspace, 2)
	if err != nil || ruling == nil || ruling.Instruction != "Implement the missing behaviour." {
		t.Fatalf("the validation card did not leave its ruling: %+v, %v", ruling, err)
	}
	if err := p.RunChainStage(context.Background(), runtime.StageValidate); err == nil {
		t.Fatal("re-dispatch lost the revision")
	}
	calls, err := os.ReadFile(p.Config.WorkerBin + ".log")
	if err != nil || strings.Count(string(calls), "arbitrate ") != 1 {
		t.Fatalf("re-dispatch repeated or skipped the model call: %v\n%s", err, calls)
	}
}

func TestTheArbitrationCardPassesItsSelectedSeatToTheWorker(t *testing.T) {
	p := arbitrationCardFixture(t)
	raw, err := os.ReadFile(p.Config.ConsumerConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	config["models"].(map[string]any)["arbiter"] = map[string]string{"id": "arbiter"}
	raw, err = json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Config.ConsumerConfigPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteSeatRecord(p.Workspace, SeatRecord{Seat: "arbiter", Stage: runtime.StageValidate, Round: 2, Candidate: 1}); err != nil {
		t.Fatal(err)
	}
	if err := p.RunChainStage(t.Context(), runtime.StageValidate); err == nil || !strings.Contains(err.Error(), "sent back for revision") {
		t.Fatalf("the fixture ruling was not executed: %v", err)
	}
	calls, err := os.ReadFile(p.Config.WorkerBin + ".log")
	if err != nil || !strings.Contains(string(calls), "--seat-candidate 1") {
		t.Fatalf("the selected occupant never reached the worker: %v\n%s", err, calls)
	}
}

func editArbitrationWorker(t *testing.T, p *Pipeline, edit func(string) string) string {
	t.Helper()
	raw, err := os.ReadFile(p.Config.WorkerBin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Config.WorkerBin, []byte(edit(string(raw))), 0o700); err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestArbitrationCardFailureKeepsItsCauseAndResumesTheRuling(t *testing.T) {
	p := arbitrationCardFixture(t)
	original := editArbitrationWorker(t, p, func(script string) string {
		return strings.Replace(script, "  arbitrate)", "  arbitrate) echo 'insufficient credits' >&2; exit 1 ;;\n  unused)", 1)
	})
	if err := p.RunChainStage(context.Background(), runtime.StageValidate); err == nil {
		t.Fatal("a refused arbiter passed validation")
	}
	failure, found := ReadStageFailure(p.Workspace, runtime.StageValidate, 2)
	if !found || failure.Step != "arbitrate" || failure.Class != FailureClassCredit || failure.Interrupted {
		t.Fatalf("the unfinished operation or provider refusal was lost: %+v, %v", failure, found)
	}
	editArbitrationWorker(t, p, func(string) string { return original })
	if err := p.RunChainStage(context.Background(), runtime.StageValidate); err == nil || !strings.Contains(err.Error(), "sent back for revision") {
		t.Fatalf("a recovered instruction must revise the change: %v", err)
	}
	failure, found = ReadStageFailure(p.Workspace, runtime.StageValidate, 2)
	if !found || failure.Step != "" {
		t.Fatalf("the completed ruling left an unfinished-arbitration marker: %+v", failure)
	}
	calls, err := os.ReadFile(p.Config.WorkerBin + ".log")
	if err != nil || strings.Count(string(calls), "arbitrate ") != 2 || strings.Contains(string(calls), "run-instruction") {
		t.Fatalf("the retry did not resume only arbitration: %v\n%s", err, calls)
	}
}

func TestArbitrationCardCancellationKeepsAnInterruptedRuling(t *testing.T) {
	p := arbitrationCardFixture(t)
	editArbitrationWorker(t, p, func(script string) string {
		return strings.Replace(script, "  arbitrate)", "  arbitrate) exec sleep 30 ;;\n  unused)", 1)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- p.RunChainStage(ctx, runtime.StageValidate) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if calls, _ := os.ReadFile(p.Config.WorkerBin + ".log"); strings.Contains(string(calls), "arbitrate ") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the card never started the blocked arbiter")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the card lost cancellation: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the arbiter did not stop with its card")
	}
	failure, found := ReadStageFailure(p.Workspace, runtime.StageValidate, 2)
	if !found || !failure.Interrupted || failure.Step != "arbitrate" {
		t.Fatalf("an interrupted ruling became a completed revision: %+v, %v", failure, found)
	}
}

func TestOverrulingInTheCardIsRecountedButCannotSkipValidation(t *testing.T) {
	p := arbitrationCardFixture(t)
	editArbitrationWorker(t, p, func(script string) string {
		script = strings.Replace(script, `"ruling":"instruct_implementer"`, `"ruling":"overrule_reviewer"`, 1)
		return strings.Replace(script, "  *) exit 91 ;;", `  decide) printf '{"outcome":"converged","ruling":%s}' "$(cat "${out%/*}/ruling.json")" > "$out" ;;
  run-validation) echo 'required behaviour still fails' >&2; exit 1 ;;
  *) exit 0 ;;`, 1)
	})
	repository, baseSHA := gitBaseRepo(t)
	if err := os.WriteFile(p.path("baseline.json"), []byte(`{"baseline":{"Integration":{"SHA":"`+baseSHA+`"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p.cloneTarget = func(ctx context.Context, destination string) error {
		return exec.CommandContext(ctx, "git", "clone", "-q", repository, destination).Run()
	}
	for pass := 0; pass < 2; pass++ {
		if err := p.RunChainStage(context.Background(), runtime.StageValidate); !errors.Is(err, ErrValidationRejected) {
			t.Fatalf("pass %d skipped the still-failing deterministic check: %v", pass, err)
		}
	}
	ruling, err := ReadRuling(p.Workspace, 2)
	if err != nil {
		t.Fatal(err)
	}
	if applied, err := RulingApplied(p.Workspace, 2, ruling); !applied || err != nil {
		t.Fatalf("the decision never counted the overruling: %v, %v", applied, err)
	}
	calls, err := os.ReadFile(p.Config.WorkerBin + ".log")
	if err != nil || strings.Count(string(calls), "arbitrate ") != 1 || strings.Count(string(calls), "decide ") != 1 || !strings.Contains(string(calls), "run-validation ") {
		t.Fatalf("the ruling was repeated or the gate skipped: %v\n%s", err, calls)
	}
	for _, line := range strings.Split(string(calls), "\n") {
		if strings.HasPrefix(line, "decide ") && !strings.Contains(line, "--ruling "+RulingFile(p.Workspace, 2)) {
			t.Fatalf("the worker that counts the decision was not given its ruling: %s", line)
		}
	}
	if failure, found := ReadStageFailure(p.Workspace, runtime.StageValidate, 2); !found || failure.Class != FailureClassValidation || failure.Step != "" {
		t.Fatalf("a real failed check was called unfinished arbitration: %+v", failure)
	}
}

func TestAnInterruptedOverrulingIsCountedBeforeAnyNewModelCall(t *testing.T) {
	for _, previousDigest := range []string{"", "different-ruling"} {
		t.Run("previous="+previousDigest, func(t *testing.T) {
			testInterruptedOverruling(t, previousDigest)
		})
	}
}

func testInterruptedOverruling(t *testing.T, previousDigest string) {
	t.Helper()
	p := arbitrationCardFixture(t)
	if previousDigest != "" {
		decision := `{"outcome":"revise","ruling":{"ruling_sha256":"` + previousDigest + `"}}`
		if err := os.WriteFile(p.path("history/stage-2/decision.json"), []byte(decision), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(RulingFile(p.Workspace, 2), []byte(`{"stage":2,"ruling":"overrule_reviewer","ruling_sha256":"fixture-ruling"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	editArbitrationWorker(t, p, func(script string) string {
		return strings.Replace(script, "  *) exit 91 ;;", `  decide) printf '{"outcome":"revise","ruling":%s}' "$(cat "${out%/*}/ruling.json")" > "$out" ;;
  *) exit 91 ;;`, 1)
	})
	for pass := 0; pass < 2; pass++ {
		if err := p.RunChainStage(context.Background(), runtime.StageValidate); err == nil || !strings.Contains(err.Error(), "sent back for revision") {
			t.Fatalf("pass %d: %v", pass, err)
		}
	}
	calls, err := os.ReadFile(p.Config.WorkerBin + ".log")
	if err != nil || strings.Contains(string(calls), "arbitrate ") || strings.Count(string(calls), "decide ") != 1 {
		t.Fatalf("the sealed ruling was not resumed exactly once: %v\n%s", err, calls)
	}
}

func TestAnUnreadableRulingIsKeptForRecoveryNotReplaced(t *testing.T) {
	p := arbitrationCardFixture(t)
	broken := []byte(`{"stage":2,"ruling":`)
	if err := os.WriteFile(RulingFile(p.Workspace, 2), broken, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.RunChainStage(context.Background(), runtime.StageValidate); err == nil {
		t.Fatal("an unreadable ruling passed the card")
	}
	if failure, found := ReadStageFailure(p.Workspace, runtime.StageValidate, 2); !found || failure.Step != "arbitrate" {
		t.Fatalf("an unreadable ruling lost its recovery marker: %+v, %v", failure, found)
	}
	if body, err := os.ReadFile(RulingFile(p.Workspace, 2)); err != nil || string(body) != string(broken) {
		t.Fatalf("the unreadable ruling was discarded: %q, %v", body, err)
	}
	if calls, _ := os.ReadFile(p.Config.WorkerBin + ".log"); len(calls) != 0 {
		t.Fatalf("the unreadable ruling was silently replaced by another model call: %s", calls)
	}
}
