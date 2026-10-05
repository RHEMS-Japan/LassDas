package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
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
