package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ticket-runner/internal/tracker"
)

// Direct callers need the same checks even without readConfig in front of them.
func TestDirectWatchAndRoleAccessRejectInvalidGitHubSettings(t *testing.T) {
	for _, variant := range []string{"repository", "key_env", "intake_label", "api_url", "mixed trackers", "project_id"} {
		t.Run(variant, func(t *testing.T) {
			cfg := githubConfiguration(t)
			want := "github." + variant
			switch variant {
			case "repository":
				cfg.GitHub.Repository = "not-a-pair"
			case "key_env":
				cfg.GitHub.KeyEnv = ""
			case "intake_label":
				cfg.GitHub.IntakeLabel = ""
			case "api_url":
				cfg.GitHub.APIURL = "http://tracker.example"
			case "mixed trackers":
				cfg.Backlog = tracker.Backlog{BaseURL: "https://tracker.example"}
				want = "cannot both be configured"
			case "project_id":
				cfg.Intake.ProjectID = 17
				want = "intake.project_id"
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
