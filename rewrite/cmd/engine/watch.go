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
	"strings"
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
	StallNoticeMinutes *int `json:"stall_notice_minutes,omitempty"`
	// Statuses names, by id, where the issue moves at each turn of the work.
	Statuses *statusConfig `json:"statuses,omitempty"`
	// CategoryOnAccept is added to an accepted issue, so the tracker's own
	// board shows which requests the runtime handles.
	CategoryOnAccept int64 `json:"category_on_accept,omitempty"`
	// Assign hands the issue to the requester while a question or the result
	// waits for them, and back to the runtime's own account while it works,
	// and records the hours the work took.
	Assign bool `json:"assign,omitempty"`
	// StatusPage is where the status page serves request pages, so the
	// acceptance comment can point at this request's own.
	StatusPage string `json:"status_page,omitempty"`
	// Announce posts the runtime's own fixed comments when a request is
	// accepted, when its work starts and when it resumes after an answer, and
	// the operator's sentence for a stage that announces itself. Off, the
	// runtime posts only its notices.
	Announce bool `json:"announce,omitempty"`
	// DeclareModels says, at each launch of a stage that chose a model,
	// which model that is: at the stage's first launch, and at a later one
	// whose models differ from those last said. A stage whose sentence is
	// announced says its first launch in that sentence alone. The request's
	// own files are looked at every few seconds for it, and once more when
	// the run returns, so neither waits for a poll.
	DeclareModels bool         `json:"declare_models,omitempty"`
	Client        *http.Client `json:"-"`
}

type sourceIssue struct {
	ID        int64                `json:"id"`
	Key       string               `json:"issueKey"`
	ProjectID int64                `json:"projectId"`
	Created   time.Time            `json:"created"`
	Creator   struct{ ID int64 }   `json:"createdUser"`
	Category  []struct{ ID int64 } `json:"category"`
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
	if err := prepareStages(&cfg); err != nil {
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
	// The queue belongs to one tracker and one project. The intake window
	// and the issue allowlist are filters an operator changes while the
	// queue lives on, so they are not part of its identity.
	identity := fmt.Sprintf("Issue intake: %s\nProject: %d", cfg.Backlog.BaseURL, cfg.Intake.ProjectID)
	// Reuse the existing exclusive, durable runtime store for ownership of this
	// queue. Its identity is not a verdict or completion mark for any issue.
	jobs := filepath.Join(root, "jobs")
	owner, err := acquireRequest(ctx, root, func(context.Context) (string, error) {
		if err := os.MkdirAll(jobs, 0700); err != nil {
			return "", err
		}
		if err := adoptEarlierIdentity(root, identity); err != nil {
			observe("the queue's earlier identity was not adopted: " + err.Error())
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
	if cfg.Intake != nil && cfg.Intake.Assign {
		// The runtime's own account, to hand an issue back to itself. Without
		// it the work still runs; only the hand-overs wait for a later start.
		if me, err := cfg.Backlog.Myself(ctx); err != nil {
			fmt.Fprintln(log, "the runtime's own tracker account is unknown; issues are not handed over either way: "+err.Error())
			cfg.Intake.Assign = false
		} else {
			cfg.runtimeUser = me
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	finished := make(chan string, capacity)
	turns := newTurnstile(capacity)
	collected := make(chan struct{}, 1)
	active := map[string]bool{}
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	observe := func(message string) { fmt.Fprintln(log, message) }
	// A finished request whose caches could not be removed is retried each
	// tick and said once per reason, not once per tick.
	trimTrouble := map[string]string{}
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
	// The queue learns which kinds of notice this engine posts before anything
	// is accepted or said, so a request accepted from here on is not older
	// than its kinds, and one delivered or begun before is not news now.
	if err := startNoticeKinds(filepath.Dir(jobs), cfg, time.Now(), observe); err != nil {
		observe("the queue's notice kinds were not brought up to date; a kind it lacks starts when first posted: " + err.Error())
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
			say := func(message string) { observe("request " + entry.Name() + ": " + message) }
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
				applyStatus(ctx, cfg, issue, directory, stoppedStatus, say)
				assignTurn(ctx, cfg, issue, directory, "requester", say)
				if cfg.Intake.StopReportRole != "" {
					if done, err := stoppedReportDone(directory); err != nil {
						observe("reading stopped report: " + err.Error())
					} else if !done {
						launch(entry.Name(), func() error { return reportStoppedRequest(ctx, cfg, issue, directory, turns, log) })
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
				applyStatus(ctx, cfg, issue, directory, deliveredStatus, say)
				assignTurn(ctx, cfg, issue, directory, "requester", say)
				if info, err := os.Stat(filepath.Join(directory, "issue.json")); err == nil && len(state.History) > 0 {
					hoursTurn(ctx, cfg, issue, directory, info.ModTime(), state.History[len(state.History)-1].FinishedAt, say)
				}
				modelsTurn(ctx, cfg, issue, directory, state, say)
				if err := trimFinished(directory); err != nil {
					reason := err.Error()
					if reason == "" {
						reason = "(no reason given)"
					}
					if trimTrouble[entry.Name()] != reason {
						say("finished caches not removed, retried each tick: " + reason)
						trimTrouble[entry.Name()] = reason
					}
				} else if trimTrouble[entry.Name()] != "" {
					say("finished caches removed")
					delete(trimTrouble, entry.Name())
				}
				continue
			}
			stopping := false
			if state.Waiting {
				// This request put a question to the person who filed it. Only
				// an authorized stop, or their reply after that question, makes
				// it runnable again; anything else leaves it untouched.
				applyStatus(ctx, cfg, issue, directory, awaitingStatus, say)
				assignTurn(ctx, cfg, issue, directory, "requester", say)
				resume, answered, err := resumeWaitingRequest(ctx, cfg, issue, directory, request, state, interval)
				if err != nil {
					observe("request " + entry.Name() + " waits for the requester: " + err.Error())
					continue
				}
				if !resume {
					continue
				}
				// A stop is not an answer. The request runs again only for the
				// stop to be recorded and reported, so the requester is not told
				// that their reply was received and the work goes on, and the
				// issue does not pass through the working status and the
				// runtime's hands on its way to stopped.
				stopping = !answered
				if answered {
					resumeTurn(ctx, cfg, issue, directory, say)
				}
			}
			if !stopping {
				applyStatus(ctx, cfg, issue, directory, processingStatus, say)
				acceptTurn(ctx, cfg, issue, directory, unfinishedBefore(jobs, entries, id), say)
			}
			// Nothing else tells the requester why an accepted request sits
			// still. These are the controller's own fixed words, posted at most
			// once per condition, and none of them ends the request.
			notice := requestNotices(cfg, issue, directory)
			// A stop launches no model, so it does not wait for the budget and
			// is not told about it: the requester asked for the work to end,
			// not to hear that it is paused and will carry on. A request held
			// here has no watcher reading its comments, so the stop is looked
			// for on its behalf.
			if creditKnown && creditLow && !stopping {
				stopping = stopWritten(ctx, cfg, issue, interval)
			}
			if creditKnown && !stopping {
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
				if err := notice.post(ctx, resumeNotice, resumeNoticeText, time.Now().UTC()); err != nil {
					observe("request " + entry.Name() + ": restart notice not confirmed: " + err.Error())
				}
			}
			// No process of this request can be running before it is launched
			// here, so a live copy left by a hard stop is stale by definition.
			if err := os.RemoveAll(filepath.Join(directory, "live")); err != nil {
				observe("request " + entry.Name() + ": stale live copy not removed: " + err.Error())
			}
			turns.enter(id)
			launch(entry.Name(), func() error {
				return runWatchedRequest(ctx, cfg, issue, directory, configPath, requestPath, interval, turns, log)
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

// stopWritten reports whether an authorized stop stands at the issue. A read
// that fails says no: the request stays as it is and is asked again on the
// next tick.
func stopWritten(ctx context.Context, cfg config, issue sourceIssue, interval time.Duration) bool {
	readCtx, release := context.WithTimeout(ctx, interval)
	defer release()
	rows, err := cfg.Backlog.Comments(readCtx, issue.Key, 0)
	if err != nil {
		return false
	}
	stop, err := stopInstruction(rows, issue, cfg.Intake.StopUserIDs)
	return err == nil && stop != nil
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
					observe("accepted " + issue.Key + " as request " + strconv.FormatInt(issue.ID, 10))
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
			// The running copy of each process's output, shown by the status page.
			process.Live = filepath.Join(directory, "live")
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

// adoptEarlierIdentity rewrites the queue's identity file when it was written
// by a runtime that still counted the intake window as part of the identity,
// so an operator who opens the window is not locked out of the queue. Only a
// file that holds no history and names the same tracker and project is
// rewritten; anything else is left for the ordinary check to refuse.
func adoptEarlierIdentity(root, identity string) error {
	path := filepath.Join(root, "history.json")
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var state chain.State
	if err := json.Unmarshal(raw, &state); err != nil {
		return err
	}
	if state.Request == identity || len(state.History) > 0 || state.Done || !strings.HasPrefix(state.Request, identity+"\nCreated since: ") {
		return nil
	}
	state.Request = identity
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return writeRuntimeFile(path, data)
}

// unfinishedBefore counts the accepted requests filed before the given one
// that are neither delivered nor stopped: the ones in line ahead of it,
// whether their watchers are running or not, compared by id as the line is.
func unfinishedBefore(jobs string, entries []os.DirEntry, id int64) int {
	ahead := 0
	for _, entry := range entries {
		other, err := strconv.ParseInt(entry.Name(), 10, 64)
		if err != nil || !entry.IsDir() || other >= id {
			continue
		}
		directory := filepath.Join(jobs, entry.Name())
		if _, err := os.Stat(filepath.Join(directory, "stop-request.json")); err == nil {
			continue
		}
		if state, err := savedHistory(directory); err == nil && state.Done {
			continue
		}
		ahead++
	}
	return ahead
}
