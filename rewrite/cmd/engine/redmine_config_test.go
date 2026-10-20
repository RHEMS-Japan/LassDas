package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ticket-runner/internal/tracker"
)

func redmineConfiguration(t *testing.T) config {
	t.Helper()
	cfg := watchConfiguration(t)
	cfg.Backlog = tracker.Backlog{}
	// Separate fixture hosts keep each test's requests its own. Only synthetic
	// credentials are installed.
	cfg.Redmine = &tracker.Redmine{BaseURL: fmt.Sprintf("https://redmine-tracker.example/test-%x", sha256.Sum256([]byte(t.Name()))), KeyEnv: "WATCH_TEST_KEY"}
	return cfg
}

func TestRedmineConfigurationRefusesMixedOrUnusableSettingsBeforeSending(t *testing.T) {
	for _, test := range []struct {
		key   string
		value any
	}{
		{"backlog", map[string]any{}}, {"backlog", nil},
		{"github", map[string]any{"repository": "example/project", "key_env": "WATCH_TEST_KEY", "intake_label": "automation"}},
		{"intake.category_on_accept", 0}, {"intake.category_on_accept", 3}, {"intake.category_on_accept", nil},
		{"redmine", nil}, {"redmine.base_url", ""}, {"redmine.base_url", "http://redmine.example"},
		{"redmine.base_url", "https:///redmine"}, {"redmine.base_url", "https://user:synthetic@redmine.example"},
		{"redmine.base_url", "https://redmine.example/?"}, {"redmine.base_url", "https://redmine.example/#"},
		{"redmine.base_url", "https://redmine.example/a/%2e%2e/b"}, {"redmine.base_url", "https://redmine.example/a/../b"},
		{"redmine.key_env", ""}, {"redmine.key_env", "not a name"}, {"redmine.key_env", "1KEY"},
		// The shape of a Redmine key, assembled here so that no file holds one.
		{"redmine.key_env", strings.Repeat("a0", 20)},
	} {
		t.Run(fmt.Sprintf("%s=%v", test.key, test.value), func(t *testing.T) {
			cfg := redmineConfiguration(t)
			var object map[string]any
			if err := json.Unmarshal(configurationJSON(t, cfg), &object); err != nil {
				t.Fatal(err)
			}
			parts := strings.Split(test.key, ".")
			parent := object
			for _, part := range parts[:len(parts)-1] {
				parent = parent[part].(map[string]any)
			}
			parent[parts[len(parts)-1]] = test.value
			data, err := json.Marshal(object)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "operator.json")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			calls := 0
			useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
				calls++
				return nil, fmt.Errorf("unexpected external request")
			})
			want := test.key
			if test.key == "backlog" || test.key == "github" {
				want = "redmine and " + test.key + " cannot both be configured"
			}
			for _, args := range [][]string{
				{"--config", path, "--check"},
				{"--config", path, "--watch", "--run-dir", filepath.Join(t.TempDir(), "queue")},
			} {
				err := run(context.Background(), args, io.Discard, io.Discard)
				if err == nil || !strings.Contains(err.Error(), want) || calls != 0 {
					t.Fatalf("error=%v external calls=%d; expected %s", err, calls, want)
				}
				if strings.Contains(fmt.Sprint(err), strings.Repeat("a0", 20)) {
					t.Fatal("the mistaken credential value was repeated")
				}
			}
		})
	}
}

func TestDirectWatchAndRoleAccessRejectInvalidRedmineSettings(t *testing.T) {
	for _, variant := range []string{"base_url", "key_env", "mixed trackers", "category_on_accept"} {
		t.Run(variant, func(t *testing.T) {
			cfg := redmineConfiguration(t)
			want := "redmine." + variant
			switch variant {
			case "base_url":
				cfg.Redmine.BaseURL = "http://redmine.example"
			case "key_env":
				cfg.Redmine.KeyEnv = ""
			case "mixed trackers":
				cfg.Backlog = tracker.Backlog{BaseURL: "https://tracker.example"}
				want = "cannot both be configured"
			case "category_on_accept":
				cfg.Intake.CategoryOnAccept = 3
				want = "intake.category_on_accept"
			}
			calls := 0
			useCatalogTransport(t, func(*http.Request) (*http.Response, error) {
				calls++
				return nil, fmt.Errorf("invalid configuration attempted a request")
			})
			root := filepath.Join(t.TempDir(), "not-created")
			if _, _, _, _, err := watchSettings(&cfg, root); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("direct watch settings: %v; want %s", err, want)
			}
			if prepare, err := roleAccess(cfg, "7"); err == nil || prepare != nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("direct role settings: %v; want %s", err, want)
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) || calls != 0 {
				t.Fatalf("configuration check created a queue or sent requests: stat=%v calls=%d", err, calls)
			}
		})
	}
}

func TestRedmineConfigurationKeepsStrictKeysAndTheIntakeIDs(t *testing.T) {
	cfg := redmineConfiguration(t)
	cfg.Intake.CategoryIDs = []int64{4}
	cfg.Intake.Statuses = &statusConfig{Processing: 2, AwaitingRequester: 4, Delivered: 3, Stopped: 5}
	data := configurationJSON(t, cfg)
	for _, text := range []string{
		strings.Replace(string(data), `"key_env":"WATCH_TEST_KEY"}`, `"Key_env":"WATCH_TEST_KEY"}`, 1),
		strings.Replace(string(data), `"redmine":{`, `"redmine":{"unknown":true,`, 1),
		strings.Replace(string(data), `"redmine":{`, `"redmine":{"base_url":"https://other.example",`, 1),
	} {
		if _, err := readConfig([]byte(text)); err == nil {
			t.Fatalf("accepted unknown or repeated key: %s", text)
		}
	}
	for _, base := range []string{"https://redmine.example", "https://host.example:8443/redmine/"} {
		cfg.Redmine.BaseURL = base
		if _, _, _, _, err := watchSettings(&cfg, t.TempDir()); err != nil {
			t.Fatal(err)
		}
		bound, err := bindRequestConfig(cfg, filepath.Join(t.TempDir(), "jobs", "11"), "11")
		if err != nil {
			t.Fatal(err)
		}
		reloaded, err := readConfig(configurationJSON(t, bound))
		if err != nil {
			t.Fatalf("generated job configuration cannot be read: %v", err)
		}
		source, ok := reloaded.source().(tracker.RedmineProject)
		if !ok || reloaded.AssignedIssue != "11" || source.ProjectID != 17 || fmt.Sprint(source.Categories) != "[4]" ||
			source.Statuses[tracker.Processing] != 2 || source.Statuses[tracker.Stopped] != 5 ||
			!source.Since.Equal(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)) || source.Client.BaseURL != base {
			t.Fatalf("job lost its tracker or its intake ids: %+v", reloaded.source())
		}
	}
	// Existing Backlog and GitHub files keep their interpretation.
	if _, err := readConfig(configurationJSON(t, watchConfiguration(t))); err != nil {
		t.Fatal(err)
	}
	if _, err := readConfig(configurationJSON(t, githubConfiguration(t))); err != nil {
		t.Fatal(err)
	}
}

func TestRedmineWatchNeedsItsProjectID(t *testing.T) {
	cfg := redmineConfiguration(t)
	cfg.Intake.ProjectID = 0
	if _, _, _, _, err := watchSettings(&cfg, filepath.Join(t.TempDir(), "queue")); err == nil || !strings.Contains(err.Error(), "intake.project_id") {
		t.Fatalf("watched Redmine without a project: %v", err)
	}
}

func TestRedmineCheckNamesItsProjectWithoutCallingAnything(t *testing.T) {
	cfg := redmineConfiguration(t)
	cfg.Intake.IssueIDs = []int64{11}
	cfg.Intake.CategoryIDs = []int64{4}
	path := filepath.Join(t.TempDir(), "operator.json")
	if err := os.WriteFile(path, configurationJSON(t, cfg), 0600); err != nil {
		t.Fatal(err)
	}
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		t.Error("--check called outside")
		return nil, fmt.Errorf("unexpected")
	})
	var output bytes.Buffer
	if err := run(context.Background(), []string{"--config", path, "--check"}, &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"Redmine project 17 without its subprojects", "open issues created at or after 2026-01-02T00:00:00Z",
		"only issue ids [11], and of those only the ones carrying one of the categories [4]", "nothing was started"} {
		if !strings.Contains(output.String(), part) {
			t.Fatalf("missing %q: %s", part, &output)
		}
	}
}

func TestRedmineCannotOpenABacklogOrGitHubQueue(t *testing.T) {
	for _, earlier := range []config{watchConfiguration(t), githubConfiguration(t)} {
		root := t.TempDir()
		data, err := json.Marshal(map[string]any{"request": earlier.source().Identity(), "history": []any{}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "history.json"), data, 0600); err != nil {
			t.Fatal(err)
		}
		calls := 0
		useCatalogTransport(t, func(r *http.Request) (*http.Response, error) { calls++; return nil, fmt.Errorf("unexpected") })
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err = watchRequests(ctx, redmineConfiguration(t), root, io.Discard)
		cancel()
		if err == nil || calls != 0 {
			t.Fatalf("different tracker reused queue: %v calls=%d", err, calls)
		}
		left, err := os.ReadFile(filepath.Join(root, "history.json"))
		if err != nil || !bytes.Equal(left, data) {
			t.Fatalf("old queue was rewritten: %s %v", left, err)
		}
	}
}

func TestRedmineRoleCannotReceiveTheControllerCredential(t *testing.T) {
	cfg := redmineConfiguration(t)
	cfg.Roles[0].Processes[0].TrackerAccess = "comment"
	cfg.Roles[0].Processes[0].Secrets = map[string]string{"BROAD_KEY": cfg.Redmine.KeyEnv}
	if _, err := roleAccess(cfg, "11"); err == nil || !strings.Contains(err.Error(), "controller tracker credential") {
		t.Fatalf("%v", err)
	}
}
