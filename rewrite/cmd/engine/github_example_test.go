package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"ticket-runner/internal/tracker"
)

func githubExample(t *testing.T) config {
	t.Helper()
	data, err := os.ReadFile("../../examples/operator-github.json")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := readConfig(data)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestGitHubExampleChangesOnlyTheTrackerNotTheWork(t *testing.T) {
	want := operatorExample(t)
	got := githubExample(t)
	if got.GitHub == nil || got.GitHub.KeyEnv != "TRACKER_API_KEY" || got.GitHub.IntakeLabel != "automation" || got.Backlog != (tracker.Backlog{}) || got.Intake.ProjectID != 0 {
		t.Fatal("example lost its explicit GitHub configuration or kept Backlog settings")
	}
	if got.GitHub.Labels != (tracker.GitHubLabels{Accepted: "automation-accepted", Processing: "automation-working",
		AwaitingRequester: "automation-awaiting-requester", Delivered: "automation-delivered", Stopped: "automation-stopped"}) {
		t.Fatal("example lost a stage label or changed its declared meaning")
	}
	got.GitHub = nil
	want.Backlog = tracker.Backlog{}
	want.Intake.ProjectID = 0
	if !reflect.DeepEqual(got, want) {
		t.Fatal("the GitHub example changed a role, model, instruction or workflow beyond tracker selection")
	}
}

func TestUneditedGitHubExampleAndPartialEditsSendNothing(t *testing.T) {
	for _, edit := range []string{"none", "date", "guidance"} {
		t.Run(edit, func(t *testing.T) {
			cfg := githubExample(t)
			if edit != "none" {
				cfg.Intake.CreatedSince = "2099-01-01T00:00:00Z"
			}
			if edit == "guidance" {
				cfg.Instructions = "Project guidance supplied by the operator."
			}
			calls := 0
			useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
				calls++
				return nil, fmt.Errorf("an unfinished example attempted an external request")
			})
			root := t.TempDir()
			configPath := filepath.Join(root, "operator.json")
			if err := os.WriteFile(configPath, configurationJSON(t, cfg), 0600); err != nil {
				t.Fatal(err)
			}
			for _, mode := range []string{"--watch", "--check"} {
				queue := filepath.Join(root, "must-not-start")
				args := []string{"--config", configPath, mode}
				if mode == "--watch" {
					args = append(args, "--run-dir", queue)
				}
				if err := run(exampleCheckContext(t), args, io.Discard, io.Discard); err == nil || errors.Is(err, context.DeadlineExceeded) || calls != 0 {
					t.Fatalf("unfinished %s configuration: err=%v calls=%d", edit, err, calls)
				}
				if _, err := os.Stat(queue); !os.IsNotExist(err) {
					t.Fatal("unfinished example created a queue")
				}
			}
		})
	}
}
