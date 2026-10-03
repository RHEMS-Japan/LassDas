package main

import "ticket-runner/internal/tracker"

// source is the tracker the configuration names, with the operator's intake
// ids. It is made from the configuration wherever it is wanted rather than
// kept beside it, so every copy of a configuration, and every change made to
// one, reaches it.
func (cfg config) source() tracker.Tracker {
	if cfg.GitHub != nil {
		return *cfg.GitHub
	}
	project := tracker.BacklogProject{Client: cfg.Backlog}
	if cfg.Intake == nil {
		return project
	}
	project.ProjectID = cfg.Intake.ProjectID
	project.Categories = cfg.Intake.CategoryIDs
	project.OnAccept = cfg.Intake.CategoryOnAccept
	if statuses := cfg.Intake.Statuses; statuses != nil {
		project.Statuses = map[string]int64{tracker.Processing: statuses.Processing,
			tracker.AwaitingRequester: statuses.AwaitingRequester, tracker.Delivered: statuses.Delivered, tracker.Stopped: statuses.Stopped}
	}
	return project
}
