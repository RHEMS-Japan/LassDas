package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"ticket-runner/internal/tracker"
)

// redmineProject is the Redmine tracker with the operator's intake ids. The
// project, the categories and the statuses are read as Backlog's are.
func (cfg config) redmineProject() tracker.RedmineProject {
	project := tracker.RedmineProject{Client: *cfg.Redmine}
	if cfg.Intake == nil {
		return project
	}
	project.ProjectID = cfg.Intake.ProjectID
	project.Categories = cfg.Intake.CategoryIDs
	// An unreadable time is refused before the watch starts; until then it
	// narrows nothing.
	project.Since, _ = time.Parse(time.RFC3339, cfg.Intake.CreatedSince)
	if statuses := cfg.Intake.Statuses; statuses != nil {
		project.Statuses = map[string]int64{tracker.Processing: statuses.Processing,
			tracker.AwaitingRequester: statuses.AwaitingRequester, tracker.Delivered: statuses.Delivered, tracker.Stopped: statuses.Stopped}
	}
	return project
}

// These are operator settings, as the GitHub ones are. Raw presence counts:
// a tracker or a setting written as zero or null is still one the engine
// would have to ignore.
func validateRedmineConfig(cfg config, data []byte) error {
	var fields map[string]json.RawMessage
	if data != nil {
		if err := json.Unmarshal(data, &fields); err != nil {
			return err
		}
		if _, present := fields["redmine"]; present && cfg.Redmine == nil {
			return fmt.Errorf("redmine must be a configuration object, not null")
		}
	}
	if cfg.Redmine == nil {
		return nil
	}
	if _, present := fields["backlog"]; present || cfg.Backlog != (tracker.Backlog{}) {
		return fmt.Errorf("redmine and backlog cannot both be configured")
	}
	if _, present := fields["github"]; present || cfg.GitHub != nil {
		return fmt.Errorf("redmine and github cannot both be configured")
	}
	var intake map[string]json.RawMessage
	if raw := fields["intake"]; raw != nil {
		if err := json.Unmarshal(raw, &intake); err != nil {
			return err
		}
	}
	// A Redmine issue carries one category. Setting the engine's would take
	// away the requester's, the one intake.category_ids may hand issues over
	// by.
	if _, present := intake["category_on_accept"]; present || cfg.Intake != nil && cfg.Intake.CategoryOnAccept != 0 {
		return fmt.Errorf("intake.category_on_accept cannot be used with redmine: an issue there carries one category, which the engine leaves as it is; use intake.statuses to show the engine's turns")
	}
	r := cfg.Redmine
	u, err := url.Parse(r.BaseURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(r.BaseURL, "#") {
		return fmt.Errorf("redmine.base_url must be the Redmine's HTTPS address without credentials, query or fragment")
	}
	for _, part := range strings.Split(u.Path, "/") {
		if part == "." || part == ".." {
			return fmt.Errorf("redmine.base_url cannot contain dot path segments")
		}
	}
	if r.KeyEnv == "" {
		return fmt.Errorf("redmine.key_env must name the environment variable containing the API key")
	}
	// A Redmine API key is forty hexadecimal characters, which can also be
	// read as a variable name.
	if len(r.KeyEnv) == 40 && strings.Trim(strings.ToLower(r.KeyEnv), "0123456789abcdef") == "" {
		return fmt.Errorf("redmine.key_env looks like a credential value; put only its environment variable name here")
	}
	for n, c := range r.KeyEnv {
		if !(c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || n > 0 && c >= '0' && c <= '9') {
			return fmt.Errorf("redmine.key_env must be an environment variable name, not a credential value")
		}
	}
	return nil
}
