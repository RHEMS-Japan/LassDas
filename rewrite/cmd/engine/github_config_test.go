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

func githubConfiguration(t *testing.T) config {
	t.Helper()
	cfg := watchConfiguration(t)
	cfg.Backlog = tracker.Backlog{}
	cfg.Intake.ProjectID = 0
	// Separate fixture API bases keep the tracker's cached responses and write
	// spacing local to each test. Only synthetic credentials are installed.
	cfg.GitHub = &tracker.GitHub{APIURL: fmt.Sprintf("https://github-tracker.example/test-%x", sha256.Sum256([]byte(t.Name()))),
		Repository: "example/project", KeyEnv: "WATCH_TEST_KEY", IntakeLabel: "automation"}
	return cfg
}

func configurationJSON(t *testing.T, cfg config) []byte {
	t.Helper()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestGitHubConfigurationRefusesMixedOrUnusableSettingsBeforeSending(t *testing.T) {
	for _, test := range []struct {
		key   string
		value any
	}{
		{"backlog", map[string]any{}}, {"backlog", nil},
		{"intake.project_id", 0}, {"intake.project_id", 17}, {"intake.project_id", nil},
		{"intake.category_ids", []any{}}, {"intake.category_ids", []int{1}}, {"intake.category_ids", nil},
		{"intake.category_on_accept", 0}, {"intake.statuses", nil}, {"intake.statuses", map[string]any{}},
		{"github", nil}, {"github.repository", ""}, {"github.repository", "owner"},
		{"github.repository", "owner/../repo"}, {"github.repository", "owner/%2e"},
		{"github.repository", "owner/space name"}, {"github.repository", "owner/name\u0001"},
		{"github.key_env", ""}, {"github.key_env", "not a name"}, {"github.key_env", "1KEY"},
		{"github.intake_label", ""}, {"github.intake_label", "one,two"}, {"github.intake_label", " automation"},
		{"github.api_url", "http://tracker.example"}, {"github.api_url", "https:///api"},
		{"github.api_url", "https://user:synthetic@tracker.example/api"},
		{"github.api_url", "https://tracker.example/api?"}, {"github.api_url", "https://tracker.example/api#"},
		{"github.api_url", "https://tracker.example/a/%2e%2e/b"},
		{"github.labels.processing", ""}, {"github.labels.processing", nil},
		{"github.labels.processing", "."}, {"github.labels.processing", ".."},
		{"github.labels.processing", "a/../b"}, {"github.labels.processing", "./x"},
		{"github.labels.processing", "x/."}, {"github.labels.processing", "AuToMaTiOn"},
		{"github.labels.processing", " working"}, {"github.labels.processing", "working "},
		{"github.labels.processing", "working\n"},
		{"github.labels.delivered", ""}, {"github.labels.accepted", ""},
		{"github.labels.awaiting_requester", ""}, {"github.labels.stopped", ""},
	} {
		t.Run(fmt.Sprintf("%s=%v", test.key, test.value), func(t *testing.T) {
			cfg := githubConfiguration(t)
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
			for _, args := range [][]string{
				{"--config", path, "--check"},
				{"--config", path, "--watch", "--run-dir", filepath.Join(t.TempDir(), "queue")},
			} {
				err := run(context.Background(), args, io.Discard, io.Discard)
				if err == nil || !strings.Contains(err.Error(), test.key) || calls != 0 {
					t.Fatalf("error=%v external calls=%d; expected key %s", err, calls, test.key)
				}
			}
		})
	}
}

func TestGitHubConfigurationKeepsStrictKeysAndOptionalLabels(t *testing.T) {
	cfg := githubConfiguration(t)
	cfg.GitHub.Labels = tracker.GitHubLabels{Processing: "stage/in progress", Delivered: "結果 ✓"}
	data := configurationJSON(t, cfg)
	for _, text := range []string{
		strings.Replace(string(data), `"intake_label":`, `"Intake_label":`, 1),
		strings.Replace(string(data), `"repository":`, `"unknown":true,"repository":`, 1),
		strings.Replace(string(data), `"repository":`, `"repository":"other/project","repository":`, 1),
		strings.Replace(string(data), `"labels":{`, `"labels":{"unknown":"word",`, 1),
	} {
		if _, err := readConfig([]byte(text)); err == nil {
			t.Fatalf("accepted unknown or repeated key: %s", text)
		}
	}
	for _, endpoint := range []string{"", "https://api.github.com", "https://tracker.example:8443/api/v3/"} {
		cfg.GitHub.APIURL = endpoint
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
		if _, ok := reloaded.source().(tracker.GitHub); !ok {
			t.Fatal("configuration selected another tracker")
		}
		if reloaded.AssignedIssue != "11" || reloaded.GitHub.Labels.Processing != "stage/in progress" {
			t.Fatal("job lost its issue or labels")
		}
	}
	// Existing Backlog files keep their interpretation, including explicit zero
	// settings: the GitHub-only rules must not be applied to them.
	if _, err := readConfig(configurationJSON(t, watchConfiguration(t))); err != nil {
		t.Fatal(err)
	}
}

func TestGitHubCheckNamesItsRepositoryAndIntakeLabelWithoutCallingAnything(t *testing.T) {
	cfg := githubConfiguration(t)
	cfg.Intake.IssueIDs = []int64{11}
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
	for _, part := range []string{"example/project", `label "automation"`, "pull requests are excluded", "issue numbers [11]"} {
		if !strings.Contains(output.String(), part) {
			t.Fatalf("missing %q: %s", part, &output)
		}
	}
	if strings.Contains(output.String(), "project 0") || strings.Contains(output.String(), "every such issue") {
		t.Fatal(output.String())
	}
}

func TestGitHubCannotOpenABacklogQueue(t *testing.T) {
	root := t.TempDir()
	old := watchConfiguration(t).source().Identity()
	data, err := json.Marshal(map[string]any{"request": old, "history": []any{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "history.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) { calls++; return nil, fmt.Errorf("unexpected") })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = watchRequests(ctx, githubConfiguration(t), root, io.Discard)
	if err == nil || calls != 0 {
		t.Fatalf("different tracker reused queue: %v calls=%d", err, calls)
	}
	left, err := os.ReadFile(filepath.Join(root, "history.json"))
	if err != nil || !bytes.Equal(left, data) {
		t.Fatalf("old queue was rewritten: %s %v", left, err)
	}
}

func TestGitHubRoleCannotReceiveTheControllerCredential(t *testing.T) {
	cfg := githubConfiguration(t)
	cfg.Roles[0].Processes[0].TrackerAccess = "comment"
	cfg.Roles[0].Processes[0].Secrets = map[string]string{"BROAD_KEY": cfg.GitHub.KeyEnv}
	if _, err := roleAccess(cfg, "11"); err == nil || !strings.Contains(err.Error(), "controller tracker credential") {
		t.Fatalf("%v", err)
	}
}
