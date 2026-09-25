package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/ticketview"
	"automation.internal/ticket-ingress/internal/worker"
)

func runnerFixtureConfig(t *testing.T, count, rounds int, bound bool) worker.Config {
	t.Helper()
	c, err := worker.LoadConfig("../../config/m1-consumer.json")
	if err != nil {
		t.Fatal(err)
	}
	c.MaxStages = rounds
	c.Consumers[0].Delivery = "pull_request"
	c.Consumers[0].Mode.Toolchain = []worker.ToolRequirement{{Binary: "go", Version: "1.26.8"}}
	if bound {
		c.Models.Reviewers = nil
		c.Agents.ReviewerAgents = nil
		for i := 0; i < count; i++ {
			id := fmt.Sprintf("judge-%d", i+1)
			key := fmt.Sprintf("RUNNER_TEST_KEY_%d", i+1)
			c.Models.Reviewers = append(c.Models.Reviewers, worker.ModelEndpoint{ID: id, Vendor: id, Model: id, BaseURL: "https://gateway.example.com/api/v1", APIKeyEnv: key, Lens: "adversarial", MaxOutputTokens: 4096})
			c.Agents.ReviewerAgents = append(c.Agents.ReviewerAgents, worker.ReviewerAgent{ReviewerID: id, Agent: worker.AgentConfig{ID: id + "-agent", Command: "hermes", Args: []string{"--profile", id, "chat"}, Profile: id, SecretEnv: map[string]string{"HERMES_API_KEY": key}, TimeoutSeconds: 900}})
		}
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	return c
}

func writeRunnerConfig(t *testing.T, c worker.Config) string {
	t.Helper()
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "consumer.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Every step is real; only the external worker/controller binaries and the
// remote clone are substituted.
func configuredRunner(t *testing.T, c worker.Config, finalOutcome string) (*Pipeline, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MODEL_API_KEY_IMPLEMENTER", "")
	t.Setenv("MODEL_API_KEY_REVIEWER", "")
	repo, sha := gitBaseRepo(t)
	log := filepath.Join(t.TempDir(), "calls.log")
	t.Setenv("RUNNER_TEST_CALL_LOG", log)
	t.Setenv("RUNNER_TEST_REPOSITORY", c.Consumers[0].Repository)
	t.Setenv("RUNNER_TEST_BASE_SHA", sha)
	t.Setenv("RUNNER_TEST_ROUNDS", fmt.Sprint(c.MaxStages))
	t.Setenv("RUNNER_TEST_OUTCOME", finalOutcome)
	bin := filepath.Join(t.TempDir(), "steps")
	writeExecutable(t, bin, `#!/bin/sh
verb="$1"
printf '%s\n' "$*" >> "$RUNNER_TEST_CALL_LOG"
out=""; stage=""; prev=""
for arg in "$@"; do
  [ "$prev" = "--out" ] && out="$arg"
  [ "$prev" = "--stage" ] && stage="$arg"
  prev="$arg"
done
case "$verb" in
  read-contract) printf '{"gaps":[]}' > "$out" ;;
  build-draft) printf '{"repository":"%s"}' "$RUNNER_TEST_REPOSITORY" > "$out" ;;
  baseline) printf '{"baseline":{"Integration":{"SHA":"%s"}}}' "$RUNNER_TEST_BASE_SHA" > "$out" ;;
  check-readiness) printf '{"verdict":"pass"}' > "$out" ;;
  decide-readiness) printf '{"outcome":"ready"}' > "$out" ;;
  decide)
    case "$out" in
      */stage-"$RUNNER_TEST_ROUNDS"/*) printf '{"outcome":"%s"}' "$RUNNER_TEST_OUTCOME" > "$out" ;;
      *) printf '{"outcome":"revise"}' > "$out" ;;
    esac ;;
  compose-trail) printf 'Reviewed change\n' > "$out" ;;
  create-feature-pr) printf '{"payload":{"pull_request":{"HTMLURL":"https://github.com/example/consumer/pull/7"}}}' > "$out" ;;
  *) [ -z "$out" ] || printf '{}' > "$out" ;;
esac
exit 0
`)
	p := &Pipeline{Config: runtime.Config{ConsumerConfigPath: writeRunnerConfig(t, c), WorkerBin: bin, ControllerBin: bin}, Workspace: t.TempDir(), Logger: baseAdvanceLogger{}}
	p.Config.Identity.EngineSHA = strings.Repeat("ab", 20)
	t.Cleanup(func() {
		if err := forceRemoveAll(p.path("target-base")); err != nil {
			t.Error(err)
		}
	})
	p.cloneTarget = func(ctx context.Context, destination string) error {
		return exec.CommandContext(ctx, "git", "clone", "-q", repo, destination).Run()
	}
	return p, log
}

func TestAgentSetupUsesLaunchConfigurationNotReviewerName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := runnerFixtureConfig(t, 2, 3, true)
	// Even the old identity can now be backed by a Hermes profile.
	c.Models.Reviewers[1].ID = "codex-adversarial"
	c.Agents.ReviewerAgents[1].ReviewerID = "codex-adversarial"
	p := &Pipeline{Config: runtime.Config{ConsumerConfigPath: writeRunnerConfig(t, c), Orchestration: "cards"}, Workspace: t.TempDir()}
	stale := filepath.Join(os.Getenv("HOME"), ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(stale), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("old model"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.writeAgentConfigs(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("unused provider file remains")
	}
	if _, err := os.Stat(p.path("agent-mcp.json")); err != nil {
		t.Fatal(err)
	}
}

// A card that is dispatched again finds its own sealed review and leaves
// it alone: redoing it would double the judge's spend.
func TestAResumedCardKeepsItsSealedReview(t *testing.T) {
	p := chainStagePipeline(t)
	sealStageFiles(t, p, 1, "")
	path := p.path("history/stage-1/judge-a.json")
	if err := os.WriteFile(path, []byte(`{"verdict":"pass"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.chainReviewSealed(context.Background(), []string{"judge-a"}, 0, p.path("repo"), strings.Repeat("a", 40), 1); err != nil {
		t.Fatalf("existing card resume behavior changed: %v", err)
	}
}

// A run whose agents cannot be configured stops before the implement card
// exists, and the reason reaches the requester's trail and the board.
func TestSetupFailureReachesRequesterAndBoard(t *testing.T) {
	c := runnerFixtureConfig(t, 2, 1, false)
	c.Models.Reviewers[1].Effort = "" // Valid endpoint, insufficient for the legacy CLI provider file.
	p, log := configuredRunner(t, c, "converged")
	_, outcome, err := p.PrepareChainRun(context.Background())
	if err == nil || outcome.Code != hook.TerminalInternalFailed || outcome.Evidence["failed_step"] == "" {
		t.Fatalf("PrepareChainRun = %+v, %v", outcome, err)
	}
	trail := readReceptionTrail(t, p)
	if !strings.Contains(trail, "推論設定") || !p.trailWritten {
		t.Fatalf("setup reason was discarded: %s", trail)
	}
	if strings.Contains(trail, p.Workspace) || strings.Contains(trail, os.Getenv("HOME")) {
		t.Fatal("private path in requester trail")
	}
	if err := hook.ValidateTrailText(trail); err != nil {
		t.Fatal(err)
	}
	comment := hook.TerminalCommentContent(hook.TerminalReportRequest{Code: outcome.Code, TrailText: trail}, strings.Repeat("a", 64))
	if !strings.Contains(comment, "推論設定") {
		t.Fatalf("comment lost cause: %s", comment)
	}
	recordFailedStep(p.Workspace, outcome.Evidence)
	view, viewErr := ticketview.Build(p.Workspace)
	if viewErr != nil || view.Failure == nil || !strings.Contains(view.Failure.Detail, "推論設定") {
		t.Fatalf("board lost setup cause: %+v, %v", view.Failure, viewErr)
	}
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "implement ") {
		t.Fatal("implementation started despite setup failure")
	}
}
