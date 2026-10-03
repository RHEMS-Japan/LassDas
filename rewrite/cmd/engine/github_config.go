package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"unicode"

	"ticket-runner/internal/tracker"
)

// These are operator settings, not requirements on an issue or a role's prose.
// Check raw presence too: a Backlog-only key explicitly set to zero or null is
// still a setting that GitHub cannot honor. Generated per-request settings omit
// these keys, so they pass the same checks when the engine reads them again.
func validateGitHubConfig(cfg config, data []byte) error {
	var fields map[string]json.RawMessage
	if data != nil {
		if err := json.Unmarshal(data, &fields); err != nil {
			return err
		}
		if _, present := fields["github"]; present && cfg.GitHub == nil {
			return fmt.Errorf("github must be a configuration object, not null")
		}
	}
	if cfg.GitHub == nil {
		return nil
	}
	if _, present := fields["backlog"]; present || cfg.Backlog != (tracker.Backlog{}) {
		return fmt.Errorf("github and backlog cannot both be configured")
	}
	var intake map[string]json.RawMessage
	if raw := fields["intake"]; raw != nil {
		if err := json.Unmarshal(raw, &intake); err != nil {
			return err
		}
	}
	i := cfg.Intake
	for _, setting := range []struct {
		name string
		set  bool
	}{
		{"project_id", i != nil && i.ProjectID != 0},
		{"category_ids", i != nil && i.CategoryIDs != nil},
		{"category_on_accept", i != nil && i.CategoryOnAccept != 0},
		{"statuses", i != nil && i.Statuses != nil},
	} {
		if _, present := intake[setting.name]; present || setting.set {
			return fmt.Errorf("intake.%s is a Backlog setting and cannot be used with github", setting.name)
		}
	}
	g := cfg.GitHub
	parts := strings.Split(g.Repository, "/")
	if len(parts) != 2 {
		return fmt.Errorf("github.repository must name owner/repository")
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "\\?#%") || strings.ContainsFunc(part, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
			return fmt.Errorf("github.repository must name owner/repository without whitespace or path escapes")
		}
	}
	if g.KeyEnv == "" {
		return fmt.Errorf("github.key_env must name the environment variable containing the PAT")
	}
	for _, prefix := range []string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_"} {
		if strings.HasPrefix(g.KeyEnv, prefix) {
			return fmt.Errorf("github.key_env looks like a credential value; put only its environment variable name here")
		}
	}
	for n, c := range g.KeyEnv {
		if !(c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || n > 0 && c >= '0' && c <= '9') {
			return fmt.Errorf("github.key_env must be an environment variable name, not a credential value")
		}
	}
	if g.APIURL != "" {
		u, err := url.Parse(g.APIURL)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(g.APIURL, "#") {
			return fmt.Errorf("github.api_url must be an HTTPS API base without credentials, query or fragment")
		}
		for _, part := range strings.Split(u.Path, "/") {
			if part == "." || part == ".." {
				return fmt.Errorf("github.api_url cannot contain dot path segments")
			}
		}
	}
	if err := githubLabelSetting("github.intake_label", g.IntakeLabel); err != nil {
		return err
	}
	if strings.Contains(g.IntakeLabel, ",") {
		return fmt.Errorf("github.intake_label must name one label, without a comma")
	}
	var rawGitHub struct {
		Labels map[string]json.RawMessage `json:"labels"`
	}
	if raw := fields["github"]; raw != nil {
		if err := json.Unmarshal(raw, &rawGitHub); err != nil {
			return err
		}
	}
	for _, label := range []struct{ key, name string }{
		{"accepted", g.Labels.Accepted}, {"processing", g.Labels.Processing},
		{"awaiting_requester", g.Labels.AwaitingRequester}, {"delivered", g.Labels.Delivered}, {"stopped", g.Labels.Stopped},
	} {
		if _, present := rawGitHub.Labels[label.key]; !present && label.name == "" {
			continue // An omitted turn leaves the issue's labels alone.
		}
		key := "github.labels." + label.key
		if err := githubLabelSetting(key, label.name); err != nil {
			return err
		}
		if strings.EqualFold(label.name, g.IntakeLabel) {
			return fmt.Errorf("%s cannot equal github.intake_label: changing a stage must not remove the intake label", key)
		}
	}
	return nil
}

func githubLabelSetting(key, name string) error {
	if name == "" || strings.TrimSpace(name) != name || strings.ContainsFunc(name, unicode.IsControl) {
		return fmt.Errorf("%s must be a nonempty label without surrounding whitespace or control characters", key)
	}
	for _, part := range strings.Split(name, "/") {
		if part == "." || part == ".." {
			return fmt.Errorf("%s label %q cannot contain dot path segments", key, name)
		}
	}
	return nil
}
