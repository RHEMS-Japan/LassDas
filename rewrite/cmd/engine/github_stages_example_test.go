package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ticket-runner/internal/tracker"
)

func githubStagesExample(t *testing.T) config {
	t.Helper()
	data, err := os.ReadFile("../../examples/operator-github-stages.json")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := readConfig(data)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// Compare raw objects as well as validating them: an explicitly present
// zero-valued Backlog setting is not a supported GitHub setting either.
func TestGitHubStagesExampleChangesOnlyTheTracker(t *testing.T) {
	read := func(name string) map[string]any {
		t.Helper()
		data, err := os.ReadFile("../../examples/" + name + ".json")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := readConfig(data); err != nil {
			t.Fatal(err)
		}
		var object map[string]any
		if err := json.Unmarshal(data, &object); err != nil {
			t.Fatal(err)
		}
		return object
	}
	want, got := read("operator-stages"), read("operator-github-stages")
	if _, present := got["backlog"]; present {
		t.Fatal("the GitHub example retains a Backlog object")
	}
	intake := got["intake"].(map[string]any)
	for _, name := range []string{"project_id", "category_ids", "category_on_accept", "statuses"} {
		if _, present := intake[name]; present {
			t.Fatalf("the GitHub example retains intake.%s", name)
		}
	}
	delete(want, "backlog")
	delete(want["intake"].(map[string]any), "project_id")
	delete(got, "github")
	if !reflect.DeepEqual(got, want) {
		t.Fatal("the GitHub example changed the ordered stages, roles, permissions or instructions")
	}
	wantGitHub := tracker.GitHub{
		APIURL: "https://tracker.example.invalid/api/v3", Repository: "REPLACE_WITH_OWNER/REPLACE_WITH_REPOSITORY",
		KeyEnv: "TRACKER_API_KEY", IntakeLabel: "automation",
		Labels: tracker.GitHubLabels{Accepted: "automation-accepted", Processing: "automation-working",
			AwaitingRequester: "automation-awaiting-requester", Delivered: "automation-delivered", Stopped: "automation-stopped"},
	}
	if got := githubStagesExample(t).GitHub; got == nil || !reflect.DeepEqual(*got, wantGitHub) {
		t.Fatal("the example changed its placeholder endpoint, repository, credential name or stage labels")
	}
}

func configuredGitHubStagesExample(t *testing.T) config {
	t.Helper()
	cfg := githubStagesExample(t)
	cfg.GitHub.APIURL = ""
	cfg.GitHub.Repository = "sample-owner/intake"
	cfg.Intake.CreatedSince = "2100-01-01T00:00:00Z"
	cfg.Instructions = "Approved project guidance and knowledge destination supplied by the operator."
	data := strings.NewReplacer(
		"https://repository.example.invalid/example-owner/example-repository.git", "/var/lib/ticket-automation/mirror/sample-owner/delivery.git",
		"example-owner/example-repository", "sample-owner/delivery",
		"example-integration-branch", "integration",
	).Replace(string(configurationJSON(t, cfg)))
	for _, placeholder := range []string{"REPLACE_WITH_", "example.invalid", "example-owner", "example-repository", "example-integration-branch"} {
		if strings.Contains(data, placeholder) {
			t.Fatalf("the configured fixture retains %s", placeholder)
		}
	}
	cfg, err := readConfig([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestUnfinishedGitHubStagesExampleDoesNotContactServicesOrCreateAQueue(t *testing.T) {
	for _, variant := range []string{"unedited", "date-only", "guidance", "repository", "api-url", "source"} {
		for _, mode := range []string{"--check", "--watch"} {
			t.Run(variant+"/"+mode, func(t *testing.T) {
				t.Setenv("TRACKER_API_KEY", "fixture-tracker-key")
				t.Setenv("MODEL_API_KEY", "fixture-model-key")
				cfg := configuredGitHubStagesExample(t)
				wantError := ""
				switch variant {
				case "unedited":
					cfg = githubStagesExample(t)
					wantError = "intake.created_since"
				case "date-only":
					cfg = githubStagesExample(t)
					cfg.Intake.CreatedSince = "2100-01-01T00:00:00Z"
					wantError = "instructions"
				case "guidance":
					cfg.Instructions = githubStagesExample(t).Instructions
					wantError = "instructions"
				case "repository":
					cfg.GitHub.Repository = githubStagesExample(t).GitHub.Repository
					wantError = "github.repository"
				case "api-url":
					cfg.GitHub.APIURL = githubStagesExample(t).GitHub.APIURL
					wantError = "github.api_url"
				case "source":
					cfg.Roles[0].Processes[0].Env["TASK_REPOSITORY"] = "https://repository.example.invalid/example-owner/example-repository.git"
					wantError = "TASK_REPOSITORY"
				}
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				var calls atomic.Int64
				useCatalogTransport(t, func(*http.Request) (*http.Response, error) {
					calls.Add(1)
					cancel()
					return nil, fmt.Errorf("unfinished example attempted a service request")
				})
				root := t.TempDir()
				configPath, queue := filepath.Join(root, "operator.json"), filepath.Join(root, "must-not-start")
				if err := os.WriteFile(configPath, configurationJSON(t, cfg), 0600); err != nil {
					t.Fatal(err)
				}
				args := []string{"--config", configPath, mode}
				if mode == "--watch" {
					args = append(args, "--run-dir", queue)
				}
				err := run(ctx, args, io.Discard, io.Discard)
				if err == nil || !strings.Contains(err.Error(), wantError) || calls.Load() != 0 {
					t.Fatalf("unfinished configuration: err=%v want=%q calls=%d", err, wantError, calls.Load())
				}
				if _, err := os.Stat(queue); !os.IsNotExist(err) {
					t.Fatal("unfinished example created a queue")
				}
			})
		}
	}
}

func TestConfiguredGitHubStagesExamplePassesAnOfflineCheck(t *testing.T) {
	cfg := configuredGitHubStagesExample(t)
	useCatalogTransport(t, func(*http.Request) (*http.Response, error) {
		t.Error("the offline check attempted a service request")
		return nil, fmt.Errorf("unexpected service request")
	})
	root := t.TempDir()
	configPath, queue := filepath.Join(root, "operator.json"), filepath.Join(root, "must-not-start")
	if err := os.WriteFile(configPath, configurationJSON(t, cfg), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var output bytes.Buffer
	if err := run(ctx, []string{"--config", configPath, "--check"}, &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, phrase := range []string{"sample-owner/intake", "automation", "2100-01-01T00:00:00Z", "the configuration is accepted; nothing was started"} {
		if !strings.Contains(output.String(), phrase) {
			t.Errorf("offline check omitted %q", phrase)
		}
	}
	if _, err := os.Stat(queue); !os.IsNotExist(err) {
		t.Fatal("offline check created a queue")
	}
}
