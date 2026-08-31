package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runtime"
)

type baseAdvanceLogger struct{}

func (baseAdvanceLogger) Info(string, ...any)  {}
func (baseAdvanceLogger) Error(string, ...any) {}

func writeExecutable(t *testing.T, path, script string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}

// standInWorkerScript answers every worker verb the delivery path uses:
// compose-trail and run-validation write their --out, everything else just
// succeeds.
func standInWorkerScript() string {
	return `#!/bin/sh
verb="$1"; shift
out=""
prev=""
for a in "$@"; do
  [ "$prev" = "--out" ] && out="$a"
  prev="$a"
done
case "$verb" in
  compose-trail) printf 'TRAIL\n' > "$out" ;;
  run-validation) printf '{}' > "$out" ;;
esac
exit 0
`
}

// gitBaseRepo builds the destination repository the retry re-validates
// against: one commit, whose SHA stands in for the advanced base.
func gitBaseRepo(t *testing.T) (string, string) {
	t.Helper()
	directory := t.TempDir()
	execGit := func(args ...string) string {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", directory}, args...)...)
		command.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	execGit("init", "-q")
	if err := os.WriteFile(filepath.Join(directory, "a.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	execGit("add", ".")
	execGit("commit", "-qm", "base")
	return directory, execGit("rev-parse", "HEAD")
}

const baseAdvanceOriginalSHA = "0000000000000000000000000000000000000001"

func baseAdvancePipeline(t *testing.T, controllerScript string, cloneFrom string) (*Pipeline, string) {
	t.Helper()
	state := t.TempDir()
	workerBin := filepath.Join(state, "worker")
	writeExecutable(t, workerBin, standInWorkerScript())
	controllerBin := filepath.Join(state, "controller")
	writeExecutable(t, controllerBin, controllerScript)
	config := runtime.Config{WorkerBin: workerBin, ControllerBin: controllerBin, ConsumerConfigPath: "consumer.json"}
	config.Identity.EngineSHA = strings.Repeat("ab", 20)
	pipeline := &Pipeline{Config: config, Workspace: t.TempDir(), Logger: baseAdvanceLogger{}}
	pipeline.delivery = "pull_request"
	pipeline.cloneTarget = func(_ context.Context, destination string) error {
		return exec.Command("git", "clone", "-q", cloneFrom, destination).Run()
	}
	if err := os.MkdirAll(pipeline.path("history/stage-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"baseline.json":     fmt.Sprintf(`{"baseline":{"Integration":{"SHA":"%s"}}}`, baseAdvanceOriginalSHA),
		"ticket-draft.json": `{"repository":"example/consumer"}`,
		"validation.json":   `{}`,
	}
	for _, name := range []string{"ticket.json", "source.json", "candidate.json", "decision.json", "review-a.json", "review-b.json"} {
		files["history/stage-1/"+name] = `{}`
	}
	for name, content := range files {
		if err := os.WriteFile(pipeline.path(name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return pipeline, state
}

// controllerScriptShared is the arg-scanning prelude every stand-in
// controller uses: it records the call, and resolves --out/--failure-out.
const controllerScriptShared = `#!/bin/sh
verb="$1"; shift
echo "$verb $*" >> "%[1]s/calls.log"
out=""
fail=""
prev=""
for a in "$@"; do
  [ "$prev" = "--out" ] && out="$a"
  [ "$prev" = "--failure-out" ] && fail="$a"
  prev="$a"
done
`

// The core promise: a publish refused only because the integration branch
// advanced re-snapshots the base, re-validates on it, and publishes against
// the advanced baseline with the original base pinned as --source-base.
func TestPublishRetriesWhenTheIntegrationBaseAdvances(t *testing.T) {
	repo, advancedSHA := gitBaseRepo(t)
	pipeline, state := baseAdvancePipeline(t, "", repo)
	script := fmt.Sprintf(controllerScriptShared+`case "$verb" in
  publish-feature)
    count=$(cat "%[1]s/publish-count" 2>/dev/null || echo 0)
    count=$((count+1))
    echo $count > "%[1]s/publish-count"
    echo "$*" >> "%[1]s/publish-args"
    if [ $count -eq 1 ]; then
      printf '{"code":"feature_publish_failed","invariant":"integration_base_changed"}' > "$fail"
      exit 1
    fi
    printf '{}' > "$out"
    exit 0 ;;
  baseline)
    printf '{"baseline":{"Integration":{"SHA":"%[2]s"}}}' > "$out"
    exit 0 ;;
  create-feature-pr)
    printf '{"payload":{"pull_request":{"HTMLURL":"https://example.invalid/pr/9"}}}' > "$out"
    exit 0 ;;
esac
exit 1
`, state, advancedSHA)
	writeExecutable(t, pipeline.Config.ControllerBin, script)

	outcome, err := pipeline.deliveryStage(context.Background(), 1, []string{"review-a", "review-b"})
	if err != nil || outcome.Code != "" || outcome.Stage != 1 {
		t.Fatalf("deliveryStage() = %+v, %v", outcome, err)
	}
	if outcome.Evidence["pull_request_url"] != "https://example.invalid/pr/9" {
		t.Fatalf("evidence = %+v", outcome.Evidence)
	}
	countRaw, err := os.ReadFile(filepath.Join(state, "publish-count"))
	if err != nil || strings.TrimSpace(string(countRaw)) != "2" {
		t.Fatalf("publish attempts = %q, %v", countRaw, err)
	}
	argsRaw, err := os.ReadFile(filepath.Join(state, "publish-args"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(argsRaw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("publish invocations = %d", len(lines))
	}
	if strings.Contains(lines[0], "--source-base") {
		t.Fatalf("first publish already carried --source-base: %s", lines[0])
	}
	if !strings.Contains(lines[1], "--source-base "+baseAdvanceOriginalSHA) ||
		!strings.Contains(lines[1], advancedBaselineFile) {
		t.Fatalf("second publish is not pinned to the advanced base: %s", lines[1])
	}
}

// Every refusal that is not the base advancing stays final: one attempt, no
// baseline snapshot, release_failed.
func TestPublishDoesNotRetryOtherRefusals(t *testing.T) {
	repo, _ := gitBaseRepo(t)
	pipeline, state := baseAdvancePipeline(t, "", repo)
	script := fmt.Sprintf(controllerScriptShared+`case "$verb" in
  publish-feature)
    printf '{"code":"feature_publish_failed","invariant":"source_blob_changed"}' > "$fail"
    exit 1 ;;
esac
exit 1
`, state)
	writeExecutable(t, pipeline.Config.ControllerBin, script)

	outcome, _ := pipeline.deliveryStage(context.Background(), 1, []string{"review-a", "review-b"})
	if outcome.Code != hook.TerminalReleaseFailed {
		t.Fatalf("outcome = %+v", outcome)
	}
	calls, err := os.ReadFile(filepath.Join(state, "calls.log"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(calls), "\n") {
		if strings.HasPrefix(line, "baseline ") {
			t.Fatalf("a non-retryable refusal still snapshotted the base:\n%s", calls)
		}
	}
}

// A branch that keeps advancing is chased at most publishAttempts times, and
// the trail says why the delivery stopped.
func TestPublishStopsChasingAtTheAttemptBound(t *testing.T) {
	repo, advancedSHA := gitBaseRepo(t)
	pipeline, state := baseAdvancePipeline(t, "", repo)
	script := fmt.Sprintf(controllerScriptShared+`case "$verb" in
  publish-feature)
    count=$(cat "%[1]s/publish-count" 2>/dev/null || echo 0)
    count=$((count+1))
    echo $count > "%[1]s/publish-count"
    printf '{"code":"feature_publish_failed","invariant":"integration_base_changed"}' > "$fail"
    exit 1 ;;
  baseline)
    printf '{"baseline":{"Integration":{"SHA":"%[2]s"}}}' > "$out"
    exit 0 ;;
esac
exit 1
`, state, advancedSHA)
	writeExecutable(t, pipeline.Config.ControllerBin, script)

	outcome, _ := pipeline.deliveryStage(context.Background(), 1, []string{"review-a", "review-b"})
	if outcome.Code != hook.TerminalReleaseFailed {
		t.Fatalf("outcome = %+v", outcome)
	}
	countRaw, err := os.ReadFile(filepath.Join(state, "publish-count"))
	if err != nil || strings.TrimSpace(string(countRaw)) != fmt.Sprint(publishAttempts) {
		t.Fatalf("publish attempts = %q, want %d", countRaw, publishAttempts)
	}
	reason, err := os.ReadFile(pipeline.path(deliveryStopReasonFile))
	if err != nil || !strings.Contains(string(reason), "追随の上限") {
		t.Fatalf("stop reason lacks the bound: %q, %v", reason, err)
	}
	// The attendant recomposes the trail in its own process and must fold
	// the recorded reason in — a fresh pipeline stands in for it here.
	attendant := &Pipeline{Config: pipeline.Config, Workspace: pipeline.Workspace, Logger: baseAdvanceLogger{}}
	if err := attendant.EnsureTrail(context.Background()); err != nil {
		t.Fatalf("EnsureTrail() error = %v", err)
	}
	attendant.AttachDeliveryStopReason()
	trail, err := os.ReadFile(pipeline.path("m1-trail.txt"))
	if err != nil || !strings.Contains(string(trail), "追随の上限") {
		t.Fatalf("recomposed trail lacks the stop reason: %q, %v", trail, err)
	}
}

func TestReadPublishInvariantToleratesGarbage(t *testing.T) {
	pipeline := &Pipeline{Workspace: t.TempDir()}
	if got := pipeline.readPublishInvariant(); got != "" {
		t.Fatalf("missing file produced %q", got)
	}
	if err := os.WriteFile(pipeline.path(publishFailureFile), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := pipeline.readPublishInvariant(); got != "" {
		t.Fatalf("broken file produced %q", got)
	}
}

// The trail note never grows the trail past the report bound, and never
// touches a trail this run did not compose.
func TestAppendTrailNoteStaysInsideTheReportBound(t *testing.T) {
	pipeline := &Pipeline{Workspace: t.TempDir()}
	pipeline.appendTrailNote("note")
	if _, err := os.Stat(pipeline.path("m1-trail.txt")); err == nil {
		t.Fatal("a note was written without a composed trail")
	}
	pipeline.trailWritten = true
	if err := os.WriteFile(pipeline.path("m1-trail.txt"), []byte(strings.Repeat("a", 6*1024-4)), 0o600); err != nil {
		t.Fatal(err)
	}
	pipeline.appendTrailNote("この行は入らない")
	content, err := os.ReadFile(pipeline.path("m1-trail.txt"))
	if err != nil || strings.Contains(string(content), "入らない") {
		t.Fatalf("an oversized note was appended: %v", err)
	}
	if err := os.WriteFile(pipeline.path("m1-trail.txt"), []byte("TRAIL\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pipeline.appendTrailNote("理由")
	content, err = os.ReadFile(pipeline.path("m1-trail.txt"))
	if err != nil || !strings.Contains(string(content), "理由") {
		t.Fatalf("a small note was not appended: %q, %v", content, err)
	}
}
