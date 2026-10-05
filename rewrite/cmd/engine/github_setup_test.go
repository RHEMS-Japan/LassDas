package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGitHubSetupMistakesCannotStartIntakeOrExposeTheInput(t *testing.T) {
	tests := []struct{ name, field, value string }{}
	for _, prefix := range []string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_"} {
		tests = append(tests, struct{ name, field, value string }{prefix, "github.key_env", prefix + strings.Repeat("synthetic", 8)})
	}
	for _, repository := range []string{"REPLACE_WITH_OWNER/REPLACE_WITH_REPOSITORY", "example/REPLACE_WITH_REPOSITORY", "REPLACE_WITH_OWNER/project"} {
		tests = append(tests, struct{ name, field, value string }{repository, "github.repository", repository})
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := githubConfiguration(t)
			if test.field == "github.key_env" {
				cfg.GitHub.KeyEnv = test.value
			} else {
				cfg.GitHub.Repository = test.value
			}
			root := t.TempDir()
			configPath := filepath.Join(root, "operator.json")
			if err := os.WriteFile(configPath, configurationJSON(t, cfg), 0600); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				return nil, fmt.Errorf("unfinished setup attempted an external request")
			})
			for _, mode := range []string{"--check", "--watch"} {
				ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
				queue := filepath.Join(root, "must-not-start")
				var output, log bytes.Buffer
				args := []string{"--config", configPath, mode}
				if mode == "--watch" {
					args = append(args, "--run-dir", queue)
				}
				err := run(ctx, args, &output, &log)
				cancel()
				if err == nil || !strings.Contains(err.Error(), test.field) || calls.Load() != 0 {
					t.Errorf("%s did not refuse the setting before communication: err=%v calls=%d", mode, err, calls.Load())
				}
				if test.field == "github.key_env" && strings.Contains(fmt.Sprint(err)+output.String()+log.String(), test.value) {
					t.Error("the mistaken credential value was exposed")
				}
				if _, err := os.Stat(queue); !os.IsNotExist(err) {
					t.Errorf("%s created a queue for an unfinished setup", mode)
				}
			}
		})
	}
}

func TestGitHubSetupCheckLeavesGuidanceAndEnvironmentNamesAlone(t *testing.T) {
	cfg := githubConfiguration(t)
	cfg.GitHub.KeyEnv = "GITHUB_PAT_FOR_TRACKER"
	cfg.Instructions = "The project's guide explains why the example uses REPLACE_WITH_OWNER/REPLACE_WITH_REPOSITORY."
	if _, _, _, _, err := watchSettings(&cfg, filepath.Join(t.TempDir(), "queue")); err != nil {
		t.Fatal(err)
	}
}

func TestGitHubOrderedSetupScriptProducesACheckableConfiguration(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("requires Python for the documented setup command")
	}
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := os.ReadFile(filepath.Join(root, "deploy", "ticket-engine", "SETUP.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, section, found := strings.Cut(string(doc), "### GitHub: make the ordered copy")
	if !found {
		t.Fatal("documented conversion is missing")
	}
	_, script, found := strings.Cut(section, "<<'PY'\n")
	if !found {
		t.Fatal("documented Python conversion is missing")
	}
	script, _, found = strings.Cut(script, "\nPY\n")
	if !found {
		t.Fatal("documented conversion has no closing delimiter")
	}
	configPath := filepath.Join(t.TempDir(), "operator.json")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, python, "-B", "-", configPath, "example/requests", "example/project", "master")
	command.Dir = root
	command.Stdin = strings.NewReader(script)
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "PYTHONDONTWRITEBYTECODE=1"}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("documented conversion failed: %v: %s", err, output)
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	// The next documented step supplies project guidance, not tracker fields.
	fields["instructions"] = json.RawMessage(`"Read the project's CONTRIBUTING.md before changing its source."`)
	raw, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		t.Errorf("the local setup check attempted a request: %s", r.URL)
		return nil, http.ErrNotSupported
	})
	var output, log bytes.Buffer
	if err := run(ctx, []string{"--config", configPath, "--check"}, &output, &log); err != nil {
		t.Fatalf("converted configuration was refused: %v: %s", err, log.String())
	}
	if !strings.Contains(output.String(), "example/requests") || !strings.Contains(output.String(), "nothing was started") {
		t.Fatalf("check did not report the selected intake: %s", output.String())
	}
}
