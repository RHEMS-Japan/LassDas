package main

import (
	"context"
	"errors"
	"fmt"

	"ticket-runner/internal/chain"
	"ticket-runner/internal/tracker"
)

var trackerEnvironment = []string{"TASK_TRACKER_URL", "TASK_TRACKER_KEY", "TASK_TRACKER_CERT", "TASK_TRACKER_ISSUE"}

// Validate operator configuration, never the wording of a model report.
func roleAccess(cfg config, issue string) (func(context.Context, chain.Process) (chain.Process, func(), error), error) {
	enabled := false
	for _, role := range cfg.Roles {
		for _, process := range role.Processes {
			switch process.TrackerAccess {
			case "":
				continue
			case "read", "comment", "every-comment":
				enabled = true
			default:
				return nil, errors.New("tracker_access must be read, comment, every-comment, or absent")
			}
			for _, name := range trackerEnvironment {
				_, env := process.Env[name]
				_, secret := process.Secrets[name]
				if env || secret || process.ModelEnv == name {
					return nil, fmt.Errorf("scoped tracker owns the %s environment variable", name)
				}
			}
		}
	}
	if !enabled {
		return nil, nil
	}
	if issue == "" {
		return nil, errors.New("scoped tracker access needs an operator-assigned issue, not an identity inferred from request prose")
	}
	source := cfg.source()
	if cfg.Backlog.BaseURL == "" || source.CredentialEnv() == "" {
		return nil, errors.New("scoped tracker access needs the controller tracker configuration")
	}
	for _, role := range cfg.Roles {
		for _, process := range role.Processes {
			for _, secret := range process.Secrets {
				if secret == source.CredentialEnv() {
					return nil, errors.New("do not also pass the controller tracker credential to a role")
				}
			}
		}
	}
	return func(ctx context.Context, process chain.Process) (chain.Process, func(), error) {
		if process.TrackerAccess == "" {
			return process, nil, nil
		}
		// "comment" leaves one comment per launch: when the launch is over,
		// the comments it stored before its last one are removed.
		// "every-comment" keeps every post. Anything else cannot post.
		var options []func(*tracker.IssueScope)
		if process.TrackerAccess == "comment" {
			options = append(options, tracker.KeepLatestPost)
		}
		mayPost := process.TrackerAccess == "comment" || process.TrackerAccess == "every-comment"
		access, err := tracker.ServeIssue(ctx, source, issue, mayPost, options...)
		if err != nil {
			return process, nil, err
		}
		env := make(map[string]string, len(process.Env)+3)
		for name, value := range process.Env {
			env[name] = value
		}
		env["TASK_TRACKER_URL"], env["TASK_TRACKER_CERT"], env["TASK_TRACKER_ISSUE"] = access.URL, access.Certificate, issue
		process.Env = env
		process.Credentials = map[string]string{"TASK_TRACKER_KEY": access.Key}
		return process, access.Close, nil
	}, nil
}
