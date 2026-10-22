package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"ticket-runner/internal/chain"
	"ticket-runner/internal/tracker"
)

func hasApproval(workflow *chain.Workflow) bool {
	if workflow != nil {
		for _, stage := range workflow.Stages {
			if stage.Approval != nil {
				return true
			}
		}
	}
	return false
}

func approvalStages(cfg config) []chain.Stage {
	if cfg.Workflow == nil {
		return nil
	}
	return cfg.Workflow.Stages
}

// Each numbered preflight is independent, including when called on a saved
// stage list with current role settings. No error includes credential material.
func validateApprovalPredecessor(cfg config) error {
	stages := approvalStages(cfg)
	for i, stage := range stages {
		if stage.Approval != nil && (i == 0 || stages[i-1].Kind != chain.CommandStage) {
			return fmt.Errorf("stage %q: approval needs an immediately preceding command stage", stage.Name)
		}
	}
	return nil
}

func validateApprovalFailure(cfg config) error {
	stages := approvalStages(cfg)
	for i, stage := range stages {
		if stage.Approval == nil {
			continue
		}
		valid := false
		for j, target := range stages {
			if target.Name == stage.OnFailure && target.Kind == chain.ModelStage && j < i-1 {
				valid = true
			}
		}
		if !valid {
			return fmt.Errorf("stage %q: approval on_failure must name a model before the preceding command", stage.Name)
		}
	}
	return nil
}

func approvalRoles(cfg config) map[string]bool {
	names := map[string]bool{}
	stages := approvalStages(cfg)
	for i, stage := range stages {
		if stage.Approval != nil {
			names[stage.Name] = true
			if i > 0 {
				names[stages[i-1].Name] = true
			}
		}
	}
	return names
}

func validateApprovalSecrets(cfg config) error {
	protected := approvalRoles(cfg)
	for _, role := range cfg.Roles {
		if !protected[role.Name] {
			continue
		}
		for _, process := range role.Processes {
			for _, source := range process.Secrets {
				value := os.Getenv(source)
				for _, other := range cfg.Roles {
					if other.Name == role.Name {
						continue
					}
					for _, p := range other.Processes {
						for _, otherSource := range p.Secrets {
							if source == otherSource || value != "" && value == os.Getenv(otherSource) {
								return fmt.Errorf("approval credentials must not be shared between roles %q and %q", role.Name, other.Name)
							}
						}
					}
				}
			}
		}
	}
	return nil
}

func validateApprovalStopUsers(cfg config) error {
	var allowed []int64
	if cfg.Intake != nil {
		allowed = cfg.Intake.StopUserIDs
	}
	for _, stage := range approvalStages(cfg) {
		if stage.Approval == nil {
			continue
		}
		for _, id := range stage.Approval.ApproverUserIDs {
			if !slices.Contains(allowed, id) {
				return fmt.Errorf("stage %q: approvers must belong to intake.stop_user_ids", stage.Name)
			}
		}
	}
	return nil
}

func validateApprovalTrackerAccess(cfg config) error {
	if !hasApproval(cfg.Workflow) {
		return nil
	}
	if cfg.Workflow.Question != "" || cfg.Intake != nil && cfg.Intake.QuestionRole != "" {
		return errors.New("approval stages cannot be combined with question_role")
	}
	stages := approvalStages(cfg)
	reachable := map[string]bool{}
	for i, stage := range stages {
		if stage.Approval != nil {
			for _, before := range stages[:i+1] {
				reachable[before.Name] = true
			}
		}
	}
	// Follow failures and subsequent stages too: a forward recovery target can
	// reach roles beyond itself before another attempt at the approval stage.
	changed := true
	for changed {
		changed = false
		for _, stage := range stages {
			if !reachable[stage.Name] || stage.OnFailure == "" {
				continue
			}
			for j, target := range stages {
				if target.Name != stage.OnFailure {
					continue
				}
				for _, next := range stages[j:] {
					if !reachable[next.Name] {
						reachable[next.Name], changed = true, true
					}
					if next.Approval != nil {
						break
					}
				}
			}
		}
	}
	for _, role := range cfg.Roles {
		if !reachable[role.Name] {
			continue
		}
		for _, process := range role.Processes {
			if process.TrackerAccess != "" {
				return fmt.Errorf("role %q can run before approval and must have no tracker_access", role.Name)
			}
		}
	}
	return nil
}

func validateApprovalStopReport(cfg config) error {
	if cfg.Intake != nil && approvalRoles(cfg)[cfg.Intake.StopReportRole] {
		return errors.New("stop_report_role must not name an approval stage or its preceding command")
	}
	return nil
}

func validateApprovalProcessCount(cfg config) error {
	for name := range approvalRoles(cfg) {
		count := 0
		for _, role := range cfg.Roles {
			if role.Name == name {
				count += len(role.Processes)
			}
		}
		if count != 1 {
			return fmt.Errorf("approval role %q must have exactly one process", name)
		}
	}
	return nil
}

func validateApprovalTracker(cfg config) error {
	if hasApproval(cfg.Workflow) && (cfg.GitHub != nil || cfg.Redmine != nil) {
		return errors.New("approval stages require Backlog")
	}
	return nil
}

func validateApprovalValues(cfg config) error {
	for _, stage := range approvalStages(cfg) {
		if err := stage.ValidateApproval(); err != nil {
			return err
		}
	}
	return nil
}

func validateApprovalLaterFailures(cfg config) error {
	stages := approvalStages(cfg)
	for i, stage := range stages {
		if stage.Approval == nil {
			continue
		}
		for _, later := range stages[i+1:] {
			if later.OnFailure == "" {
				continue
			}
			valid := false
			for j, target := range stages {
				if target.Name == later.OnFailure && j > i {
					valid = true
				}
			}
			if !valid {
				return fmt.Errorf("stage %q: on_failure must stay after approval stage %q", later.Name, stage.Name)
			}
		}
	}
	return nil
}

func validateApprovalStages(cfg config) error {
	for _, check := range []func(config) error{
		validateApprovalPredecessor, validateApprovalFailure, validateApprovalSecrets,
		validateApprovalStopUsers, validateApprovalTrackerAccess, validateApprovalStopReport,
		validateApprovalProcessCount, validateApprovalTracker, validateApprovalValues,
		validateApprovalLaterFailures,
	} {
		if err := check(cfg); err != nil {
			return err
		}
	}
	return rejectUnavailableApproval(cfg.Workflow)
}

// Removed only when the parent-side approval handling and end-to-end tests land.
func rejectUnavailableApproval(workflow *chain.Workflow) error {
	if hasApproval(workflow) {
		return errors.New("approval stages are not available yet")
	}
	return nil
}

func validateApprovalRun(cfg config, supervised bool) error {
	if hasApproval(cfg.Workflow) && !supervised {
		return errors.New("approval stages require --watch; a single run is not supported")
	}
	return nil
}

func startApprovalWatch(ctx context.Context, cfg *config, source tracker.Tracker) error {
	me, err := source.Myself(ctx)
	if err != nil {
		return fmt.Errorf("approval startup cannot read the runtime account: %w", err)
	}
	for _, stage := range approvalStages(*cfg) {
		if stage.Approval != nil && slices.Contains(stage.Approval.ApproverUserIDs, me.ID) {
			return errors.New("approval approvers must not include the runtime account")
		}
	}
	rule, err := source.TextFormattingRule(ctx)
	if err != nil {
		return fmt.Errorf("approval startup cannot read the project text formatting rule: %w", err)
	}
	if rule != "markdown" {
		return errors.New("approval stages require markdown project formatting")
	}
	cfg.runtimeUser = me
	return nil
}

func setRunCommentJobs(cfg *config, runDirectory string) {
	// --run-dir is jobs/<id>/run; both parent and child record beside that run.
	cfg.commentJobs = filepath.Dir(filepath.Dir(filepath.Clean(runDirectory)))
}

type roleAccessFactory func(config, string, ...func(*tracker.IssueScope)) (func(context.Context, chain.Process) (chain.Process, func(), error), error)

func prepareRunAccess(cfg *config, runDirectory, issue string, access roleAccessFactory) (func(context.Context, chain.Process) (chain.Process, func(), error), error) {
	// Bind before passing a configuration copy to roleAccess, which captures
	// its tracker. The caller also keeps this directory for runtime reads.
	setRunCommentJobs(cfg, runDirectory)
	return access(*cfg, issue)
}

func prepareStoredApprovals(cfg *config, store *chain.FileStore) error {
	// Ordinary runs leave history loading and retries entirely to Chain.Run.
	if !hasApproval(cfg.Workflow) {
		return nil
	}
	state, err := store.Load()
	if err != nil {
		return err
	}
	// Run has not taken its first copy yet.
	if state.Workflow == nil {
		return nil
	}
	if hasApproval(cfg.Workflow) && !hasApproval(state.Workflow) {
		return errors.New("saved workflow has no approval stages; a new request is required")
	}
	saved := *cfg
	saved.Workflow = state.Workflow
	// Shape checks 1, 2 and 10 already ran before the immutable copy was taken.
	for _, check := range []func(config) error{validateApprovalSecrets, validateApprovalTrackerAccess, validateApprovalStopReport, validateApprovalProcessCount} {
		if err := check(saved); err != nil {
			return err
		}
	}
	return rejectUnavailableApproval(state.Workflow)
}
