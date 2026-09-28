package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"ticket-runner/internal/chain"
	"ticket-runner/internal/tracker"
)

// Intake settings are operator scope, not a format required of requesters.
// The explicit timestamp prevents quietly starting every historical issue.
type intakeConfig struct {
	ProjectID           int64   `json:"project_id"`
	CreatedSince        string  `json:"created_since"`
	PollIntervalSeconds int     `json:"poll_interval_seconds,omitempty"`
	MaxRunning          int     `json:"max_running,omitempty"`
	StopUserIDs         []int64 `json:"stop_user_ids,omitempty"`
	StopReportRole      string  `json:"stop_report_role,omitempty"`
	QuestionRole        string  `json:"question_role,omitempty"`
	IssueIDs            []int64 `json:"issue_ids,omitempty"`
	// MinModelCredit is the USD balance below which the shared model key can no
	// longer carry the work. Absent or zero asks the provider nothing.
	MinModelCredit float64 `json:"min_model_credit,omitempty"`
	// ModelCreditURL overrides where that balance is read, so a gateway can
	// answer instead of the provider.
	ModelCreditURL string `json:"model_credit_url,omitempty"`
	// StallNoticeMinutes is how long a request may go without completing a step
	// before the requester is told. Absent means 90 minutes; zero says nothing.
	StallNoticeMinutes *int         `json:"stall_notice_minutes,omitempty"`
	Client             *http.Client `json:"-"`
}

type sourceIssue struct {
	ID        int64              `json:"id"`
	Key       string             `json:"issueKey"`
	ProjectID int64              `json:"projectId"`
	Created   time.Time          `json:"created"`
	Creator   struct{ ID int64 } `json:"createdUser"`
}

type serialLog struct {
	mu     sync.Mutex
	writer io.Writer
}

func (w *serialLog) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writer.Write(p)
}

func watchRequests(ctx context.Context, cfg config, root string, log io.Writer) error {
	if cfg.Intake == nil || cfg.Intake.ProjectID <= 0 {
		return errors.New("watch requires an explicit intake.project_id")
	}
	if err := validateStopReporter(cfg); err != nil {
		return err
	}
	if err := validateQuestionRole(cfg); err != nil {
		return err
	}
	if err := validateNotices(cfg); err != nil {
		return err
	}
	since, err := time.Parse(time.RFC3339, cfg.Intake.CreatedSince)
	if err != nil {
		return errors.New("watch requires intake.created_since as an explicit RFC3339 timestamp")
	}
	delay, capacity := cfg.Intake.PollIntervalSeconds, cfg.Intake.MaxRunning
	if delay < 0 || time.Duration(delay) > time.Duration(1<<63-1)/time.Second || capacity < 0 {
		return errors.New("intake interval and capacity must be positive")
	}
	for _, id := range cfg.Intake.StopUserIDs {
		if id <= 0 {
			return errors.New("intake.stop_user_ids must contain positive user ids")
		}
	}
	for _, id := range cfg.Intake.IssueIDs {
		if id <= 0 {
			return errors.New("intake.issue_ids must contain positive issue ids")
		}
	}
	if delay == 0 {
		delay = 30
	}
	if capacity == 0 {
		capacity = 1
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return err
	}
	// Validate binding before discovering/accepting any work.
	if _, err := bindRequestConfig(cfg, filepath.Join(root, "jobs", "0"), "example"); err != nil {
		return err
	}
	w := &serialLog{writer: log}
	observe := func(message string) { fmt.Fprintln(w, message) }
	identity := fmt.Sprintf("Issue intake: %s\nProject: %d\nCreated since: %s", cfg.Backlog.BaseURL, cfg.Intake.ProjectID, since.UTC().Format(time.RFC3339))
	// Reuse the existing exclusive, durable runtime store for ownership of this
	// queue. Its identity is not a verdict or completion mark for any issue.
	jobs := filepath.Join(root, "jobs")
	owner, err := acquireRequest(ctx, root, func(context.Context) (string, error) {
		if err := os.MkdirAll(jobs, 0700); err != nil {
			return "", err
		}
		return identity, nil
	}, 10*time.Second, observe)
	if err != nil {
		return err
	}
	defer owner.Close()
	return pollRequests(ctx, cfg, jobs, since, time.Duration(delay)*time.Second, capacity, w)
}

// One collector owns the queue. Job histories, not child exit codes or prose,
// tell it which already-run engines chose done. Pending histories are resumed
// by the same engine; the collector does not make a new completion judgment.
func pollRequests(ctx context.Context, cfg config, jobs string, since time.Time, interval time.Duration, capacity int, log io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	finished := make(chan string, capacity)
	slots := make(chan struct{}, capacity)
	collected := make(chan struct{}, 1)
	active := map[string]bool{}
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	observe := func(message string) { fmt.Fprintln(log, message) }
	launch := func(name string, work func() error) {
		active[name] = true
		workers.Add(1)
		go func() {
			defer workers.Done()
			if err := work(); err != nil {
				observe("request " + name + " remains unfinished: " + err.Error())
			}
			select {
			case finished <- name:
			case <-ctx.Done():
			}
		}()
	}
	// Discovery must not hold up cancellation or local recovery of already
	// accepted work. Its only writes are immutable native issue snapshots.
	workers.Add(1)
	go func() {
		defer workers.Done()
		collectIssues(ctx, cfg, jobs, since, interval, collected, observe)
	}()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Already accepted work remains runnable even when discovery is down or
		// the issue later disappears from the remote list.
		entries, err := os.ReadDir(jobs)
		if err != nil {
			observe("reading accepted requests: " + err.Error())
		}
		sort.Slice(entries, func(i, j int) bool {
			a, _ := strconv.ParseInt(entries[i].Name(), 10, 64)
			b, _ := strconv.ParseInt(entries[j].Name(), 10, 64)
			return a < b
		})
		// One shared model key serves the whole queue, so ask once per tick
		// rather than once per waiting request.
		creditLow, creditKnown := modelCreditHold(ctx, cfg, observe)
		for _, entry := range entries {
			if ctx.Err() != nil {
				break
			}
			id, err := strconv.ParseInt(entry.Name(), 10, 64)
			if err != nil || id <= 0 || !entry.IsDir() || active[entry.Name()] {
				continue
			}
			directory := filepath.Join(jobs, entry.Name())
			raw, err := os.ReadFile(filepath.Join(directory, "issue.json"))
			if err != nil {
				observe("reading accepted issue: " + err.Error())
				continue
			}
			var issue sourceIssue
			if err := json.Unmarshal(raw, &issue); err != nil || issue.ID != id || issue.ProjectID != cfg.Intake.ProjectID || issue.Key == "" {
				observe("accepted issue record could not be read for its source; no replacement used")
				continue
			}
			if stopped, err := savedStop(directory, issue, cfg.Intake.StopUserIDs); err != nil {
				observe("request " + entry.Name() + " held: " + err.Error())
				continue
			} else if stopped {
				if cfg.Intake.StopReportRole != "" {
					if done, err := stoppedReportDone(directory); err != nil {
						observe("reading stopped report: " + err.Error())
					} else if !done {
						launch(entry.Name(), func() error { return reportStoppedRequest(ctx, cfg, issue, directory, slots, log) })
					}
				}
				continue
			}
			request, err := tracker.RequestText(raw)
			if err != nil {
				observe(err.Error())
				continue
			}
			store, err := chain.Open(filepath.Join(directory, "run"), request)
			if err != nil {
				observe("opening accepted request: " + err.Error())
				continue
			}
			state, err := store.Load()
			store.Close()
			if err != nil {
				observe("reading accepted history: " + err.Error())
				continue
			}
			if state.Done {
				continue
			}
			if state.Waiting {
				// This request put a question to the person who filed it. Only
				// an authorized stop, or their reply after that question, makes
				// it runnable again; anything else leaves it untouched.
				resume, err := resumeWaitingRequest(ctx, cfg, issue, directory, request, state, interval)
				if err != nil {
					observe("request " + entry.Name() + " waits for the requester: " + err.Error())
					continue
				}
				if !resume {
					continue
				}
			}
			// Nothing else tells the requester why an accepted request sits
			// still. These are the controller's own fixed words, posted at most
			// once per condition, and none of them ends the request.
			notice := requestNotices(cfg, issue, directory)
			if creditKnown {
				if err := applyBudgetNotice(ctx, notice, creditLow); err != nil {
					observe("request " + entry.Name() + ": budget notice not confirmed: " + err.Error())
				}
				if creditLow {
					continue
				}
			}
			bound, err := bindRequestConfig(cfg, directory, issue.Key)
			if err != nil {
				observe(err.Error())
				continue
			}
			if err := os.MkdirAll(filepath.Join(directory, "workspace"), 0700); err != nil {
				observe(err.Error())
				continue
			}
			data, err := json.Marshal(bound)
			if err != nil {
				observe(err.Error())
				continue
			}
			configPath, requestPath := filepath.Join(directory, "engine.json"), filepath.Join(directory, "request.txt")
			if err := writeRuntimeFile(configPath, data); err != nil {
				observe(err.Error())
				continue
			}
			if err := writeRuntimeFile(requestPath, []byte(request)); err != nil {
				observe(err.Error())
				continue
			}
			// An interrupted action or an unfinished recovery means the work is
			// picked up again, not started afresh. Say so before it runs, so
			// the requester is not left reading a silent gap in the night.
			if state.Pending != nil || state.Recovering {
				if err := notice.post(ctx, resumeNotice, resumeNoticeText); err != nil {
					observe("request " + entry.Name() + ": restart notice not confirmed: " + err.Error())
				}
			}
			launch(entry.Name(), func() error {
				return runWatchedRequest(ctx, cfg, issue, directory, configPath, requestPath, interval, slots, log)
			})
		}
		// Launch failures are retried on the next tick, not in a busy loop.
		for waiting := true; waiting; {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case name := <-finished:
				delete(active, name)
			case <-collected:
				waiting = false
			case <-tick.C:
				waiting = false
			}
		}
	}
}

func collectIssues(ctx context.Context, cfg config, jobs string, since time.Time, interval time.Duration, collected chan<- struct{}, observe func(string)) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for ctx.Err() == nil {
		rows, err := cfg.Backlog.Issues(ctx, cfg.Intake.ProjectID)
		changed := false
		if err != nil {
			observe("issue discovery unavailable: " + err.Error())
		} else {
			for _, raw := range rows {
				if ctx.Err() != nil {
					return
				}
				var issue sourceIssue
				if err := json.Unmarshal(raw, &issue); err != nil || issue.Created.IsZero() {
					observe("issue discovery returned no readable creation time; it was not accepted")
					continue
				}
				if issue.Created.Before(since) {
					continue
				}
				// An optional operator allowlist narrows discovery only. Already
				// accepted requests still resume from their durable queue records.
				if len(cfg.Intake.IssueIDs) > 0 {
					allowed := false
					for _, id := range cfg.Intake.IssueIDs {
						allowed = allowed || issue.ID == id
					}
					if !allowed {
						continue
					}
				}
				directory := filepath.Join(jobs, strconv.FormatInt(issue.ID, 10))
				path := filepath.Join(directory, "issue.json")
				if _, err := os.Stat(path); err == nil {
					continue
				} else if !errors.Is(err, os.ErrNotExist) {
					observe("reading issue intake: " + err.Error())
					continue
				}
				if err := os.MkdirAll(directory, 0700); err != nil {
					observe("creating request directory: " + err.Error())
					continue
				}
				if err := writeRuntimeFile(path, raw); err != nil {
					observe("saving original issue: " + err.Error())
				} else {
					changed = true
				}
			}
		}
		if changed {
			select {
			case collected <- struct{}{}:
			default:
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// Logical directories and explicit harness context are not a filesystem or
// network sandbox. The configured launcher must enforce actual permissions.
func bindRequestConfig(cfg config, directory, issue string) (config, error) {
	bound := cfg
	bound.AssignedIssue = issue
	if _, err := roleAccess(bound, issue); err != nil {
		return config{}, err
	}
	bound.Roles = make([]chain.Role, len(cfg.Roles))
	workspace := filepath.Join(directory, "workspace")
	for i, role := range cfg.Roles {
		bound.Roles[i] = role
		bound.Roles[i].Processes = append([]chain.Process(nil), role.Processes...)
		for j, process := range role.Processes {
			if process.Directory != "" && !filepath.IsLocal(process.Directory) {
				return config{}, errors.New("watch role directories must be relative to each request workspace")
			}
			process.Directory = filepath.Join(workspace, process.Directory)
			process.Env = make(map[string]string, len(role.Processes[j].Env)+3)
			for key, value := range role.Processes[j].Env {
				process.Env[key] = value
			}
			for key, value := range map[string]string{"TASK_WORKSPACE": workspace, "TASK_HOME": filepath.Join(directory, "homes", fmt.Sprintf("%d-%d", i, j)), "TASK_ISSUE": issue} {
				if process.ModelEnv == key {
					return config{}, fmt.Errorf("watch environment %s overlaps model selection", key)
				}
				if _, exists := process.Env[key]; exists {
					return config{}, fmt.Errorf("watch owns the %s environment variable", key)
				}
				if _, secret := process.Secrets[key]; secret {
					return config{}, fmt.Errorf("watch environment %s overlaps a credential", key)
				}
				process.Env[key] = value
			}
			bound.Roles[i].Processes[j] = process
		}
	}
	return bound, nil
}

func writeRuntimeFile(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".intake-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
