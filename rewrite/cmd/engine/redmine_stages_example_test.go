package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ticket-runner/internal/tracker"
)

func redmineStagesExample(t *testing.T) config {
	t.Helper()
	data, err := os.ReadFile("../../examples/operator-redmine-stages.json")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := readConfig(data)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// Compare raw objects as well as validating them: the Redmine example is the
// Backlog one with its tracker exchanged, and nothing else.
func TestRedmineStagesExampleChangesOnlyTheTracker(t *testing.T) {
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
	want, got := read("operator-stages"), read("operator-redmine-stages")
	if _, present := got["backlog"]; present {
		t.Fatal("the Redmine example retains a Backlog object")
	}
	if _, present := got["intake"].(map[string]any)["category_on_accept"]; present {
		t.Fatal("the Redmine example sets a category on acceptance")
	}
	delete(want, "backlog")
	delete(got, "redmine")
	if !reflect.DeepEqual(got, want) {
		t.Fatal("the Redmine example changed the ordered stages, roles, permissions, intake or instructions")
	}
	if got := redmineStagesExample(t).Redmine; got == nil || *got != (tracker.Redmine{BaseURL: "https://tracker.example.invalid", KeyEnv: "TRACKER_API_KEY"}) {
		t.Fatal("the example changed its placeholder address or credential name")
	}
}

func configuredRedmineStagesExample(t *testing.T) config {
	t.Helper()
	cfg := redmineStagesExample(t)
	cfg.Redmine.BaseURL = "https://redmine.sample-owner.test"
	cfg.Intake.ProjectID = 17
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

func TestUnfinishedRedmineStagesExampleDoesNotContactServicesOrCreateAQueue(t *testing.T) {
	for _, variant := range []string{"unedited", "date-only", "guidance", "base-url", "project", "source"} {
		for _, mode := range []string{"--check", "--watch"} {
			t.Run(variant+"/"+mode, func(t *testing.T) {
				t.Setenv("TRACKER_API_KEY", "fixture-tracker-key")
				t.Setenv("MODEL_API_KEY", "fixture-model-key")
				cfg := configuredRedmineStagesExample(t)
				wantError := ""
				switch variant {
				case "unedited":
					cfg = redmineStagesExample(t)
					wantError = "intake.project_id"
				case "date-only":
					cfg = redmineStagesExample(t)
					cfg.Intake.ProjectID = 17
					cfg.Intake.CreatedSince = "2100-01-01T00:00:00Z"
					wantError = "instructions"
				case "guidance":
					cfg.Instructions = redmineStagesExample(t).Instructions
					wantError = "instructions"
				case "base-url":
					cfg.Redmine.BaseURL = redmineStagesExample(t).Redmine.BaseURL
					wantError = "redmine.base_url"
				case "project":
					cfg.Intake.ProjectID = 0
					wantError = "intake.project_id"
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

func TestConfiguredRedmineStagesExamplePassesAnOfflineCheck(t *testing.T) {
	cfg := configuredRedmineStagesExample(t)
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
	for _, phrase := range []string{"Redmine project 17 without its subprojects", "2100-01-01T00:00:00Z", "the configuration is accepted; nothing was started"} {
		if !strings.Contains(output.String(), phrase) {
			t.Errorf("offline check omitted %q", phrase)
		}
	}
	if _, err := os.Stat(queue); !os.IsNotExist(err) {
		t.Fatal("offline check created a queue")
	}
}

// The setup guide's copy command for Redmine, run as it is written with its
// placeholders filled, makes a configuration the check accepts.
func TestRedmineCopyCommandInTheSetupGuideMakesACheckableConfiguration(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	guide, err := os.ReadFile(filepath.Join(root, "deploy", "ticket-engine", "SETUP.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, section, found := strings.Cut(string(guide), "### Redmine: make your copy with one command")
	if !found {
		t.Fatal("the documented Redmine copy is missing")
	}
	_, command, found := strings.Cut(section, "```sh\n")
	if !found {
		t.Fatal("the documented Redmine copy has no command")
	}
	command, _, found = strings.Cut(command, "\n```")
	if !found {
		t.Fatal("the documented Redmine copy does not end")
	}
	configPath := filepath.Join(t.TempDir(), "operator.json")
	command = strings.NewReplacer(
		"<absolute path outside the repository>/operator.json", configPath,
		"<your Redmine address>", "redmine.sample-owner.test/tracker",
		"<project-id>", "17",
		"<owner>/<repository-name>", "sample-owner/delivery",
		"<integration-branch>", "integration",
	).Replace(command)
	if strings.Contains(command, "<") {
		t.Fatalf("a placeholder of the documented command is not filled: %s", command)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	shell := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	shell.Dir = root
	if output, err := shell.CombinedOutput(); err != nil {
		t.Fatalf("documented copy failed: %v: %s", err, output)
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, placeholder := range []string{"REPLACE_WITH_", "example.invalid", "example-owner", "example-repository", "example-integration-branch"} {
		if strings.Contains(string(raw), placeholder) {
			t.Fatalf("the copy retains %s", placeholder)
		}
	}
	cfg, err := readConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Redmine == nil || cfg.Redmine.BaseURL != "https://redmine.sample-owner.test/tracker" || cfg.Intake.ProjectID != 17 || cfg.Intake.CreatedSince != "2100-01-01T00:00:00Z" {
		t.Fatalf("the copy's tracker or intake: %+v %+v", cfg.Redmine, cfg.Intake)
	}
	// The next documented step supplies project guidance, not tracker fields.
	cfg.Instructions = "Read the project's CONTRIBUTING.md before changing its source."
	if err := os.WriteFile(configPath, configurationJSON(t, cfg), 0600); err != nil {
		t.Fatal(err)
	}
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		t.Errorf("the local setup check attempted a request: %s", r.URL)
		return nil, http.ErrNotSupported
	})
	var output bytes.Buffer
	if err := run(ctx, []string{"--config", configPath, "--check"}, &output, io.Discard); err != nil {
		t.Fatalf("the copied configuration was refused: %v", err)
	}
	if !strings.Contains(output.String(), "Redmine project 17 without its subprojects") || !strings.Contains(output.String(), "nothing was started") {
		t.Fatalf("check did not report the selected intake: %s", output.String())
	}
}
