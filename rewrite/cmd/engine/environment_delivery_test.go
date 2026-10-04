package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"ticket-runner/internal/chain"
	"ticket-runner/internal/tracker"
)

// Real child processes, real Git, the existing verifier and shipped report
// check exercise the existing chain together. Models and remote services are
// explicit stand-ins. Passing this test certifies no real deployment service.
func TestEnvironmentDeliveryComposition(t *testing.T) {
	trackerTool, confirm := environmentTools(t)
	for _, scenario := range []struct {
		name, failed string
		attempts     int
	}{
		{"integrated", "", 1},
		{"already-applied", "", 0},
		{"response-lost", "", 1},
		{"confirm-fails-once", "", 1},
		{"base-moves-after-check", "", 1},
		{"open-pr", "inspect", 0},
		{"closed-pr", "inspect", 0},
		{"changed-pr", "inspect", 0},
		{"unconfirmed-commit", "inspect", 0},
		{"other-request", "inspect", 0},
		{"other-repository", "inspect", 0},
		{"other-base", "inspect", 0},
		{"malformed", "inspect", 0},
		{"old-artifact", "apply", 0},
		{"other-commit", "apply", 0},
		{"unverified", "apply", 0},
		{"missing-artifact", "apply", 0},
		{"other-environment", "apply", 0},
		{"base-moves-before-check", "verify", 0},
		{"missing-receipt", "verify", 0},
		{"malformed-receipt", "verify", 0},
		{"stale-healthy", "observe", 1},
		{"readback-fails", "observe", 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			root := t.TempDir()
			workspace, remote := filepath.Join(root, "workspace"), filepath.Join(root, "remote.git")
			environmentWrite(t, filepath.Join(workspace, "application.txt"), []byte("initial application\n"))
			environmentGit(t, workspace, "init", "-q", "-b", "integration")
			environmentGit(t, workspace, "add", "application.txt")
			environmentGit(t, workspace, "commit", "-qm", "Initial fixture")
			environmentGit(t, workspace, "checkout", "-qb", "ticket/"+environmentIssue)
			environmentWrite(t, filepath.Join(workspace, "application.txt"), []byte(environmentSource))
			environmentGit(t, workspace, "commit", "-qam", "Requested fixture")
			head := environmentGit(t, workspace, "rev-parse", "HEAD")
			environmentGit(t, workspace, "checkout", "-q", "integration")
			environmentGit(t, workspace, "merge", "--no-ff", "-qm", "Integrate fixture", "ticket/"+environmentIssue)
			commit := environmentGit(t, workspace, "rev-parse", "HEAD")
			environmentGit(t, root, "clone", "--bare", "--", workspace, remote)
			// The role's checkout is deliberately not the source verified remotely.
			environmentWrite(t, filepath.Join(workspace, "application.txt"), []byte("unverified local edit\n"))
			receipt := map[string]any{"repository": "fixture/application", "base": "integration", "issue": environmentIssue, "head": head, "branch": "ticket/" + environmentIssue, "pull_request": 7, "merge_sha": commit}
			// A local success cannot override the provider's actual open PR.
			if scenario.name == "closed-pr" {
				delete(receipt, "merge_sha")
				receipt["closed_unmerged"] = true
			}
			if scenario.name == "changed-pr" {
				receipt["changed_by_person"] = true
			}
			receiptPath := filepath.Join(workspace, ".git", "ticket-engine", "delivery.json")
			if scenario.name != "missing-receipt" {
				environmentJSON(t, receiptPath, receipt)
			}
			if scenario.name == "malformed-receipt" {
				environmentWrite(t, receiptPath, []byte("not JSON"))
			}
			p := &environmentProvider{mode: scenario.name, runDir: filepath.Join(root, "run"), comments: []map[string]any{}, integration: environmentIntegration{Issue: environmentIssue, Repository: "fixture/application", Base: "integration", Commit: commit, Environment: "evaluation", Merged: true}}
			switch scenario.name {
			case "open-pr", "closed-pr", "changed-pr":
				p.integration.Merged = false
			case "unconfirmed-commit":
				p.integration.Commit = "unknown"
			case "other-request":
				p.integration.Issue = "EXAMPLE-42"
			case "other-repository":
				p.integration.Repository = "fixture/other"
			case "other-base":
				p.integration.Base = "other"
			case "other-environment":
				p.integration.Environment = "another-environment"
			case "already-applied":
				p.live = environmentLive{Environment: "evaluation", Digest: environmentDigest(environmentSource), Content: environmentSource, Healthy: true}
			}
			advance := func() {
				clone := filepath.Join(root, "later")
				environmentGit(t, root, "clone", "--", remote, clone)
				environmentWrite(t, filepath.Join(clone, "application.txt"), []byte("later unverified application\n"))
				environmentGit(t, clone, "commit", "-qam", "Later fixture")
				environmentGit(t, clone, "push", "origin", "integration")
			}
			if scenario.name == "base-moves-before-check" {
				advance()
			}
			if scenario.name == "base-moves-after-check" {
				p.advance = advance
			}
			provider := environmentServer(t, p)
			t.Setenv("ENVIRONMENT_TRACKER_SOURCE", "synthetic-tracker-key")
			t.Setenv("ENVIRONMENT_APPLY_SOURCE", "synthetic-apply-key")
			t.Setenv("ENVIRONMENT_GIT_SOURCE", "synthetic-git-key")
			t.Setenv("ENVIRONMENT_PARENT_ONLY", "must-not-be-inherited")
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			verifier, err := filepath.Abs("../../harnesses/verify_merged.py")
			if err != nil {
				t.Fatal(err)
			}
			cfg := config{AssignedIssue: environmentIssue, ModelSelection: &selectionConfig{Fixed: "fixture/stand-in"}, Backlog: tracker.Backlog{BaseURL: "https://tracker.example.invalid/api/v2", KeyEnv: "ENVIRONMENT_TRACKER_SOURCE"}}
			cfg.Router.Mode = "stages"
			cfg.Workflow = &chain.Workflow{Stages: []chain.Stage{
				{Name: "elicit", Kind: chain.ModelStage},
				{Name: "inspect", Kind: chain.CommandStage, OnFailure: "elicit"},
				{Name: "verify", Kind: chain.CommandStage, OnFailure: "elicit"},
				{Name: "apply", Kind: chain.CommandStage, OnFailure: "elicit"},
				{Name: "observe", Kind: chain.CommandStage, OnFailure: "elicit"},
				{Name: "report", Kind: chain.ModelStage},
				{Name: "confirm", Kind: chain.CommandStage, OnFailure: "report"},
			}}
			for _, stage := range cfg.Workflow.Stages {
				env := map[string]string{
					"ENVIRONMENT_ACTION": stage.Name, "ENVIRONMENT_FIXTURE_URL": provider, "ENVIRONMENT_TARGET": "evaluation", "ENVIRONMENT_COMMIT": commit,
					"ENVIRONMENT_VERIFY": verifier, "ENVIRONMENT_CONFIRM": confirm, "TRACKER_TOOL": trackerTool,
					"TASK_WORKSPACE": workspace, "TASK_HOME": filepath.Join(root, "homes", stage.Name),
					"DELIVERY_REPOSITORY": "fixture/application", "DELIVERY_BASE_BRANCH": "integration", "DELIVERY_REMOTE_URL": remote,
					"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.DevNull,
					"VERIFY_COMMANDS": fmt.Sprintf("env ENVIRONMENT_ACTION=build %s -test.run=^TestEnvironmentDeliveryHelper$", quoteEnvironmentArg(binary)),
				}
				process := chain.Process{Name: stage.Name, Directory: workspace, Command: []string{binary, "-test.run=^TestEnvironmentDeliveryHelper$"}, Env: env, TimeoutMinutes: 1}
				if stage.Kind == chain.ModelStage {
					process.ModelEnv = "FIXTURE_MODEL"
				}
				if stage.Name == "report" {
					process.TrackerAccess = "comment"
				}
				if stage.Name == "confirm" {
					process.TrackerAccess = "read"
				}
				if stage.Name == "apply" {
					process.Secrets = map[string]string{"ENVIRONMENT_APPLY_KEY": "ENVIRONMENT_APPLY_SOURCE"}
				}
				if stage.Name == "verify" {
					process.Secrets = map[string]string{"GITHUB_TOKEN": "ENVIRONMENT_GIT_SOURCE"}
				}
				cfg.Roles = append(cfg.Roles, chain.Role{Name: stage.Name, Purpose: "Synthetic composition stage", Processes: []chain.Process{process}})
			}
			configPath, requestPath := filepath.Join(root, "config.json"), filepath.Join(root, "request.txt")
			environmentJSON(t, configPath, cfg)
			environmentWrite(t, requestPath, []byte("Check the requested application in evaluation; no other environment is authorized."))
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			if scenario.failed != "" {
				p.cancel = cancel
			}
			var log bytes.Buffer
			err = run(ctx, []string{"--config", configPath, "--request", requestPath, "--run-dir", p.runDir}, io.Discard, &log)
			store, openErr := chain.Open(p.runDir, "Check the requested application in evaluation; no other environment is authorized.")
			if openErr != nil {
				t.Fatalf("open history: %v; run=%v\n%s", openErr, err, log.String())
			}
			defer store.Close()
			state, loadErr := store.Load()
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			p.mu.Lock()
			defer p.mu.Unlock()
			roles := []string{}
			for _, event := range p.events {
				roles = append(roles, event.Role)
			}
			if p.prematureDone || p.deployAttempts != scenario.attempts || p.accepted != scenario.attempts {
				t.Fatalf("premature=%v attempts=%d accepted=%d roles=%v run=%v\n%s", p.prematureDone, p.deployAttempts, p.accepted, roles, err, log.String())
			}
			if scenario.failed != "" {
				if state.Done || err == nil || len(roles) < 2 || roles[len(roles)-1] != "elicit" || roles[len(roles)-2] != scenario.failed || slices.Contains(roles, "report") {
					t.Fatalf("failed stage did not return to elicitation: done=%v roles=%v run=%v\n%s", state.Done, roles, err, log.String())
				}
				found := false
				for _, result := range state.History {
					found = found || result.Role == scenario.failed && result.Speaker != "runtime" && result.Error != ""
				}
				if !found || !strings.Contains(p.events[len(p.events)-1].Prompt, "did not exit 0") {
					t.Fatalf("recovery lost the observed failure: %+v", state.History)
				}
				return
			}
			if err != nil || !state.Done || state.Step != "confirm" || p.confirms == 0 || p.observations == 0 || len(p.comments) != 1 {
				t.Fatalf("unfinished composition done=%v confirms=%d observations=%d comments=%d roles=%v run=%v\n%s", state.Done, p.confirms, p.observations, len(p.comments), roles, err, log.String())
			}
			if p.live.Environment != "evaluation" || p.live.Content != environmentSource || p.live.Digest != environmentDigest(environmentSource) || p.artifact.Commit != commit {
				t.Fatalf("wrong observed artifact: %+v %+v", p.live, p.artifact)
			}
			report, readErr := os.ReadFile(filepath.Join(workspace, "report", "result.md"))
			if readErr != nil || string(report) != p.comments[0]["content"] {
				t.Fatal("stored report differs", readErr)
			}
			if scenario.name == "response-lost" && !slices.Equal(roles, []string{"elicit", "inspect", "verify", "apply", "elicit", "inspect", "verify", "apply", "observe", "report", "confirm"}) {
				t.Fatalf("response loss did not take the configured recovery: %v", roles)
			}
			if scenario.name == "confirm-fails-once" && (!p.confirmFailure || p.confirms != 2 || !slices.Equal(roles[len(roles)-4:], []string{"report", "confirm", "report", "confirm"})) {
				t.Fatalf("confirm did not retry via report: %v", roles)
			}
		})
	}
}

func quoteEnvironmentArg(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func runEnvironmentVerifier(ctx context.Context, workspace string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "python3", "-B", "../../harnesses/verify_merged.py")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "TASK_WORKSPACE=" + workspace}
	return cmd.CombinedOutput()
}

// A zero status from these existing verifier endings explicitly means no
// verification. It must not substitute for the environment command's checks.
func TestEnvironmentDeliveryVerifierNoCheckIsNotPermission(t *testing.T) {
	for _, ending := range []string{"closed_unmerged", "changed_by_person"} {
		t.Run(ending, func(t *testing.T) {
			root := t.TempDir()
			environmentJSON(t, filepath.Join(root, ".git", "ticket-engine", "delivery.json"), map[string]any{ending: true})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			output, err := runEnvironmentVerifier(ctx, root)
			if err != nil || !strings.Contains(string(output), "configured verification commands were not run") {
				t.Fatalf("verifier no-check result: %v\n%s", err, output)
			}
		})
	}
}
