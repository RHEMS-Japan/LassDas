// Command status serves a read-only page about one runtime's queue: where each
// accepted request is, what every role was told and what it answered, what is
// running at this moment and what the runtime itself observed. It holds no
// credential, writes nothing under the queue and shows what is on disk as it
// is; nothing here decides or changes the course of a request.
package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"ticket-runner/internal/chain"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, log io.Writer) error {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(log)
	runDir := flags.String("run-dir", "", "the runtime's queue directory, read only")
	configPath := flags.String("config", "", "the runtime's configuration file, shown as it is read; optional")
	listen := flags.String("listen", "127.0.0.1:9200", "address to serve on")
	userEnv := flags.String("auth-user-env", "", "environment variable holding the user name for HTTP basic authentication; empty serves without one")
	passwordEnv := flags.String("auth-password-env", "", "environment variable holding the password for HTTP basic authentication")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *runDir == "" || flags.NArg() != 0 {
		return errors.New("provide --run-dir and optionally --config, --listen, --auth-user-env and --auth-password-env")
	}
	s, err := newServer(*runDir, *configPath, *userEnv, *passwordEnv)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: s.handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(stop)
	}()
	fmt.Fprintf(log, "status page on %s reading %s\n", listener.Addr(), *runDir)
	if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

type server struct {
	runDir    string
	configRaw []byte
	config    map[string]any
	issueBase string
	user      string
	password  string
	location  *time.Location
	templates *template.Template
}

func newServer(runDir, configPath, userEnv, passwordEnv string) (*server, error) {
	s := &server{runDir: runDir, location: time.Local}
	if configPath != "" {
		raw, err := os.ReadFile(configPath)
		if err != nil {
			return nil, fmt.Errorf("reading --config: %w", err)
		}
		s.configRaw = raw
		if err := json.Unmarshal(raw, &s.config); err != nil {
			s.config = nil
		}
		if backlog, ok := s.config["backlog"].(map[string]any); ok {
			if base, ok := backlog["base_url"].(string); ok {
				s.issueBase = issueLinkBase(base)
			}
		}
	}
	if userEnv != "" {
		s.user, s.password = os.Getenv(userEnv), os.Getenv(passwordEnv)
		if s.user == "" || s.password == "" {
			return nil, errors.New("basic authentication was requested but the named environment variables are empty")
		}
	}
	s.templates = template.Must(template.New("").Funcs(template.FuncMap{
		"time": s.formatTime, "ago": s.ago, "pretty": prettyJSON,
		"head": func(title string, refresh int) headData { return headData{Title: title, Refresh: refresh} },
	}).Parse(pageTemplates))
	return s, nil
}

// issueLinkBase turns a tracker API base such as https://space.example/api/v2
// into the prefix of a browser link for one issue key.
func issueLinkBase(base string) string {
	base = strings.TrimSuffix(strings.TrimSuffix(base, "/"), "/api/v2")
	if base == "" {
		return ""
	}
	return base + "/view/"
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	mux.HandleFunc("GET /{$}", s.auth(s.overview))
	mux.HandleFunc("GET /jobs/{id}", s.auth(s.jobPage))
	mux.HandleFunc("GET /jobs/{id}/raw/{name}", s.auth(s.rawFile))
	mux.HandleFunc("GET /jobs/{id}/workspace", s.auth(s.workspacePage))
	mux.HandleFunc("GET /config", s.auth(s.configPage))
	mux.HandleFunc("GET /log", s.auth(s.logPage))
	mux.HandleFunc("GET /files", s.auth(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/files/", http.StatusMovedPermanently)
	}))
	mux.HandleFunc("GET /files/{path...}", s.auth(s.filesPage))
	return mux
}

func (s *server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.user != "" {
			user, password, ok := r.BasicAuth()
			if !ok || subtle.ConstantTimeCompare([]byte(user), []byte(s.user)) != 1 ||
				subtle.ConstantTimeCompare([]byte(password), []byte(s.password)) != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="ticket engine status"`)
				http.Error(w, "authentication required", http.StatusUnauthorized)
				return
			}
		}
		next(w, r)
	}
}

var jobName = regexp.MustCompile(`^[0-9]+$`)
var answerName = regexp.MustCompile(`^answer-[0-9]+\.json$`)

// job is everything on disk about one accepted request, read as it is. Every
// file that cannot be read or decoded is reported in Notes instead of hidden.
type job struct {
	ID        string
	Key       string
	Title     string
	Requester string
	Created   string
	Link      string
	Request   string
	State     *chain.State
	Records   []record
	Answers   []namedText
	Live      []liveEntry
	Notices   []map[string]any
	Report    string
	Reviews   []namedText
	Notes     []string
	Status    string
	Position  string
	Started   time.Time
	Updated   time.Time
	Elapsed   string
	Workspace *workspace
	Receipt   string
	Homes     []homeLogs
	Stages    []stageTime
	Refresh   int
	Lane      string
	Dots      string
	Attention string
}

// lane is one column of the board: the requests in one of four situations
// the operator reads at a glance, in the order the old board used.
type lane struct {
	Key   string
	Title string
	Jobs  []*job
}

var laneOrder = []lane{{Key: "running", Title: "Running"}, {Key: "awaiting", Title: "Awaiting answer"},
	{Key: "attention", Title: "Needs attention"}, {Key: "delivered", Title: "Delivered"}}

func lanes(jobs []*job) []lane {
	result := make([]lane, len(laneOrder))
	copy(result, laneOrder)
	for _, j := range jobs {
		for i := range result {
			if result[i].Key == j.Lane {
				result[i].Jobs = append(result[i].Jobs, j)
			}
		}
	}
	return result
}

// stageTime is the review view of one stage or role: how often it ran, how
// long it took in all, and when it first started and last finished.
type stageTime struct {
	Name     string
	Launches int
	Failures int
	Total    string
	First    time.Time
	Last     time.Time
}

// homeLogs is what a role's own private directory holds of its native agent's
// log: the steps it took, as the agent itself wrote them.
type homeLogs struct {
	Name       string
	AgentLog   string
	ErrorsLog  string
	Files      []string
	Calls      int
	TokensIn   int
	TokensOut  int
	Transcript string
}

type record struct {
	Index       int
	Role        string
	Speaker     string
	Model       string
	Started     time.Time
	Finished    time.Time
	Duration    string
	Output      string
	Diagnostics string
	Instruction string
	Error       string
	Runtime     bool
	Person      bool
	Gap         string
}

type namedText struct {
	Name string
	Text string
}

type liveEntry struct {
	Role        string
	Speaker     string
	Model       string
	Instruction string
	Started     time.Time
	Stdout      string
	Stderr      string
	Home        string
	AgentLog    string
}

func (s *server) jobs(now time.Time) ([]*job, error) {
	entries, err := os.ReadDir(filepath.Join(s.runDir, "jobs"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var jobs []*job
	for _, entry := range entries {
		if entry.IsDir() && jobName.MatchString(entry.Name()) {
			jobs = append(jobs, s.loadJob(entry.Name(), now, false))
		}
	}
	sort.SliceStable(jobs, func(i, j int) bool { return jobs[i].Updated.After(jobs[j].Updated) })
	return jobs, nil
}

func stringOf(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}

func (s *server) loadJob(id string, now time.Time, withWorkspace bool) *job {
	dir := filepath.Join(s.runDir, "jobs", id)
	j := &job{ID: id}
	note := func(format string, args ...any) { j.Notes = append(j.Notes, fmt.Sprintf(format, args...)) }
	touch := func(path string) {
		if info, err := os.Stat(path); err == nil && info.ModTime().After(j.Updated) {
			j.Updated = info.ModTime()
		}
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "issue.json")); err != nil {
		note("issue.json could not be read: %v", err)
	} else {
		var issue map[string]any
		if err := json.Unmarshal(raw, &issue); err != nil {
			note("issue.json could not be decoded: %v", err)
		} else {
			j.Key, j.Title, j.Created = stringOf(issue["issueKey"]), stringOf(issue["summary"]), stringOf(issue["created"])
			if user, ok := issue["createdUser"].(map[string]any); ok {
				j.Requester = stringOf(user["name"])
			}
			if s.issueBase != "" && j.Key != "" {
				j.Link = s.issueBase + j.Key
			}
		}
		touch(filepath.Join(dir, "issue.json"))
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "request.txt")); err != nil {
		note("request.txt could not be read: %v", err)
	} else {
		j.Request = string(raw)
	}
	historyPath := filepath.Join(dir, "run", "history.json")
	if raw, err := os.ReadFile(historyPath); err != nil {
		note("run/history.json could not be read: %v", err)
	} else {
		var state chain.State
		if err := json.Unmarshal(raw, &state); err != nil {
			note("run/history.json could not be decoded: %v", err)
		} else {
			j.State = &state
			var previous time.Time
			for i, result := range state.History {
				entry := record{Index: i + 1, Role: result.Role, Speaker: result.Speaker,
					Model: result.ModelPrefix + result.Model, Started: result.StartedAt, Finished: result.FinishedAt,
					Duration: humanDuration(result.FinishedAt.Sub(result.StartedAt)), Output: result.Output,
					Diagnostics: result.Diagnostics, Instruction: result.Instruction, Error: result.Error,
					Runtime: result.Speaker == "runtime", Person: result.Speaker == "requester"}
				if !previous.IsZero() && result.StartedAt.Sub(previous) >= time.Second {
					entry.Gap = humanDuration(result.StartedAt.Sub(previous))
				}
				if result.FinishedAt.After(previous) {
					previous = result.FinishedAt
				}
				j.Records = append(j.Records, entry)
			}
		}
		touch(historyPath)
	}
	if names, _ := filepath.Glob(filepath.Join(dir, "answer-*.json")); len(names) > 0 {
		sort.Strings(names)
		for _, name := range names {
			raw, err := os.ReadFile(name)
			if err != nil {
				note("%s could not be read: %v", filepath.Base(name), err)
				continue
			}
			j.Answers = append(j.Answers, namedText{Name: filepath.Base(name), Text: string(raw)})
			touch(name)
		}
	}
	if names, _ := filepath.Glob(filepath.Join(dir, "live", "*.json")); len(names) > 0 {
		sort.Strings(names)
		for _, name := range names {
			raw, err := os.ReadFile(name)
			if err != nil {
				continue // gone between the listing and the read: the process just ended
			}
			var entry struct {
				Role        string    `json:"role"`
				Speaker     string    `json:"speaker"`
				Model       string    `json:"model"`
				Instruction string    `json:"instruction"`
				StartedAt   time.Time `json:"started_at"`
				Home        string    `json:"home"`
			}
			if err := json.Unmarshal(raw, &entry); err != nil {
				note("%s could not be decoded: %v", filepath.Base(name), err)
				continue
			}
			base := strings.TrimSuffix(name, ".json")
			stdout, _ := os.ReadFile(base + ".stdout")
			stderr, _ := os.ReadFile(base + ".stderr")
			live := liveEntry{Role: entry.Role, Speaker: entry.Speaker, Model: entry.Model,
				Instruction: entry.Instruction, Started: entry.StartedAt, Stdout: string(stdout), Stderr: string(stderr)}
			if entry.Home != "" {
				if rel, err := filepath.Rel(s.runDir, entry.Home); err == nil && filepath.IsLocal(rel) {
					live.Home = filepath.ToSlash(rel)
					if log, err := tail(filepath.Join(entry.Home, "logs", "agent.log"), 20000); err == nil {
						live.AgentLog = log
					}
				}
			}
			j.Live = append(j.Live, live)
			touch(base + ".stdout")
			touch(base + ".stderr")
		}
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "notices.json")); err == nil {
		var log struct {
			Notices []map[string]any `json:"notices"`
		}
		if err := json.Unmarshal(raw, &log); err != nil {
			note("notices.json could not be decoded: %v", err)
		} else {
			j.Notices = log.Notices
		}
		touch(filepath.Join(dir, "notices.json"))
	} else if !errors.Is(err, os.ErrNotExist) {
		note("notices.json could not be read: %v", err)
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "workspace", "report", "result.md")); err == nil {
		j.Report = string(raw)
	}
	if names, _ := filepath.Glob(filepath.Join(dir, "homes", "*", "review.md")); len(names) > 0 {
		sort.Strings(names)
		for _, name := range names {
			if raw, err := os.ReadFile(name); err == nil {
				j.Reviews = append(j.Reviews, namedText{Name: filepath.Base(filepath.Dir(name)), Text: string(raw)})
			}
		}
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "workspace", ".git", "ticket-engine", "delivery.json")); err == nil {
		j.Receipt = string(raw)
	}
	if homes, err := os.ReadDir(filepath.Join(dir, "homes")); err == nil {
		for _, home := range homes {
			if !home.IsDir() {
				continue
			}
			logs := homeLogs{Name: home.Name()}
			if raw, err := os.ReadFile(filepath.Join(dir, "homes", home.Name(), "logs", "agent.log")); err == nil {
				logs.Calls, logs.TokensIn, logs.TokensOut = agentUsage(string(raw))
				if len(raw) > 20000 {
					logs.AgentLog = fmt.Sprintf("[%d earlier bytes not shown; the whole file is under files]\n", len(raw)-20000) + string(raw[len(raw)-20000:])
				} else {
					logs.AgentLog = string(raw)
				}
			}
			if raw, err := os.ReadFile(filepath.Join(dir, "homes", home.Name(), "transcript.json")); err == nil {
				var indented bytes.Buffer
				if json.Indent(&indented, raw, "", "  ") == nil {
					logs.Transcript = indented.String()
				} else {
					logs.Transcript = string(raw)
				}
			}
			if text, err := tail(filepath.Join(dir, "homes", home.Name(), "logs", "errors.log"), 20000); err == nil {
				logs.ErrorsLog = text
			}
			filepath.WalkDir(filepath.Join(dir, "homes", home.Name()), func(path string, entry os.DirEntry, err error) error {
				if err == nil && !entry.IsDir() {
					if rel, err := filepath.Rel(filepath.Join(dir, "homes", home.Name()), path); err == nil {
						logs.Files = append(logs.Files, filepath.ToSlash(rel))
					}
				}
				return nil
			})
			j.Homes = append(j.Homes, logs)
		}
	}
	j.derive(now)
	if withWorkspace {
		j.Workspace = readWorkspace(filepath.Join(dir, "workspace"))
	}
	return j
}

func (j *job) derive(now time.Time) {
	end := now
	if state := j.State; state != nil {
		var last time.Time
		for _, result := range state.History {
			if j.Started.IsZero() || result.StartedAt.Before(j.Started) {
				j.Started = result.StartedAt
			}
			if result.FinishedAt.After(last) {
				last = result.FinishedAt
			}
		}
		switch {
		case state.Done:
			j.Status = "done"
			if !last.IsZero() {
				end = last
			}
		case state.Waiting:
			j.Status = "waiting for the requester's reply"
		case len(j.Live) > 0:
			var roles []string
			for _, entry := range j.Live {
				roles = append(roles, entry.Role)
			}
			j.Status = "running " + strings.Join(roles, ", ")
		case state.Pending != nil:
			j.Status = "assigned to " + state.Pending.Role + ", no process output yet"
		default:
			j.Status = "between steps"
		}
		if state.Recovering {
			j.Status = "recovering after a restart; " + j.Status
		}
		if state.Workflow != nil && len(state.Workflow.Stages) > 0 && state.Step != "" {
			j.Position = state.Step
			for i, stage := range state.Workflow.Stages {
				if stage.Name == state.Step {
					j.Position = fmt.Sprintf("step %d of %d: %s", i+1, len(state.Workflow.Stages), state.Step)
				}
			}
		} else if state.Step != "" {
			j.Position = state.Step
		}
	} else {
		j.Status = "no run record yet"
	}
	if j.Started.IsZero() {
		j.Started = j.Updated
	}
	if !j.Started.IsZero() {
		j.Elapsed = humanDuration(end.Sub(j.Started))
	}
	j.Lane = "running"
	switch state := j.State; {
	case state == nil:
		for _, note := range j.Notes {
			if strings.Contains(note, "history.json could not be decoded") {
				j.Lane, j.Attention = "attention", note
			}
		}
	case state.Done:
		j.Lane = "delivered"
	case state.Waiting:
		j.Lane = "awaiting"
	default:
		var last time.Time
		if n := len(state.History); n > 0 {
			last = state.History[n-1].FinishedAt
			if record := state.History[n-1]; record.Speaker == "runtime" && record.Error != "" && len(j.Live) == 0 {
				j.Lane, j.Attention = "attention", record.Error
			}
		}
		for _, notice := range j.Notices {
			kind := stringOf(notice["kind"])
			if kind != "budget-paused" && kind != "stall" {
				continue
			}
			if written, err := time.Parse(time.RFC3339Nano, stringOf(notice["written_at"])); err == nil && written.After(last) {
				j.Lane, j.Attention = "attention", stringOf(notice["text"])
			}
		}
		if state.Workflow != nil && len(state.Workflow.Stages) > 0 {
			var dots strings.Builder
			reached := false
			for _, stage := range state.Workflow.Stages {
				switch {
				case stage.Name == state.Step:
					dots.WriteString("◉")
					reached = true
				case reached:
					dots.WriteString("○")
				default:
					dots.WriteString("●")
				}
			}
			j.Dots = dots.String()
		}
	}
	j.Refresh = 30
	if len(j.Live) > 0 {
		j.Refresh = 10
	}
	if j.State != nil {
		totals := map[string]*stageTime{}
		var order []string
		if j.State.Workflow != nil {
			for _, stage := range j.State.Workflow.Stages {
				order = append(order, stage.Name)
				totals[stage.Name] = &stageTime{Name: stage.Name}
			}
		}
		var durations = map[string]time.Duration{}
		for _, result := range j.State.History {
			if result.Speaker == "runtime" || result.Speaker == "requester" {
				continue
			}
			entry, known := totals[result.Role]
			if !known {
				entry = &stageTime{Name: result.Role}
				totals[result.Role] = entry
				order = append(order, result.Role)
			}
			entry.Launches++
			if result.Error != "" {
				entry.Failures++
			}
			durations[result.Role] += result.FinishedAt.Sub(result.StartedAt)
			if entry.First.IsZero() || result.StartedAt.Before(entry.First) {
				entry.First = result.StartedAt
			}
			if result.FinishedAt.After(entry.Last) {
				entry.Last = result.FinishedAt
			}
		}
		for _, name := range order {
			entry := totals[name]
			entry.Total = humanDuration(durations[name])
			j.Stages = append(j.Stages, *entry)
		}
	}
}

// workspace is what the roles have changed so far in the request's checkout,
// read with git in a mode that writes nothing.
type workspace struct {
	Status    string
	Diff      string
	Untracked []namedText
	Log       string
	Note      string
}

const untrackedLimit = 200000

func gitRead(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "--no-optional-locks",
		"-c", "core.hooksPath=" + os.DevNull, "-c", "core.fsmonitor=false"}, args...)...)
	command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull, "GIT_TERMINAL_PROMPT=0")
	var out, errs bytes.Buffer
	command.Stdout, command.Stderr = &out, &errs
	if err := command.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 && strings.TrimSpace(errs.String()) == "" {
			return out.String(), nil // git diff reports a difference with 1
		}
		return out.String(), fmt.Errorf("%v: %s", err, strings.TrimSpace(errs.String()))
	}
	return out.String(), nil
}

func readWorkspace(dir string) *workspace {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return &workspace{Note: "no checkout yet"}
	}
	w := &workspace{}
	status, err := gitRead(dir, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		w.Note = "git status could not be read: " + err.Error()
		return w
	}
	w.Status = status
	if log, err := gitRead(dir, "log", "--no-color", "-5", "--format=%H %ci %s"); err == nil {
		w.Log = log
	}
	if diff, err := gitRead(dir, "diff", "--no-color"); err != nil {
		w.Note = "git diff could not be read: " + err.Error()
	} else {
		w.Diff = diff
	}
	for _, line := range strings.Split(status, "\n") {
		if !strings.HasPrefix(line, "?? ") {
			continue
		}
		name := strings.TrimPrefix(line, "?? ")
		raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			w.Untracked = append(w.Untracked, namedText{Name: name, Text: "could not be read: " + err.Error()})
			continue
		}
		text := string(raw)
		if len(text) > untrackedLimit {
			text = text[:untrackedLimit] + fmt.Sprintf("\n[cut here: %d more bytes on disk]\n", len(raw)-untrackedLimit)
		}
		w.Untracked = append(w.Untracked, namedText{Name: name, Text: text})
	}
	return w
}

var apiCallLine = regexp.MustCompile(`API call #[0-9]+:.*\bin=([0-9]+) out=([0-9]+)`)

// agentUsage sums what the native agent logged about its own model calls:
// the count and the input and output tokens, as the agent wrote them.
func agentUsage(log string) (calls, in, out int) {
	for _, match := range apiCallLine.FindAllStringSubmatch(log, -1) {
		calls++
		var i, o int
		fmt.Sscan(match[1], &i)
		fmt.Sscan(match[2], &o)
		in, out = in+i, out+o
	}
	return calls, in, out
}

func humanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	hours, minutes, seconds := int(d/time.Hour), int(d/time.Minute)%60, int(d/time.Second)%60
	switch {
	case hours > 0:
		return fmt.Sprintf("%dh%02dm%02ds", hours, minutes, seconds)
	case minutes > 0:
		return fmt.Sprintf("%dm%02ds", minutes, seconds)
	}
	return fmt.Sprintf("%ds", seconds)
}

func (s *server) formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.In(s.location).Format("2006-01-02 15:04:05 MST")
}

func (s *server) ago(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return humanDuration(time.Since(t)) + " ago"
}

func prettyJSON(value any) string {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(data)
}

func tail(path string, limit int) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if len(raw) > limit {
		return fmt.Sprintf("[%d earlier bytes not shown; the whole file is on disk]\n", len(raw)-limit) + string(raw[len(raw)-limit:]), nil
	}
	return string(raw), nil
}

type page struct {
	Now       string
	RunDir    string
	Path      string
	Base      string
	Files     []fileEntry
	Jobs      []*job
	Lanes     []lane
	Job       *job
	Intake    any
	Stages    []string
	Router    any
	Selection any
	Config    bool
	Log       string
	Notes     []string
}

func (s *server) render(w http.ResponseWriter, name string, data page) {
	var buffer bytes.Buffer
	if err := s.templates.ExecuteTemplate(&buffer, name, data); err != nil {
		http.Error(w, "the page could not be rendered: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(buffer.Bytes())
}

func (s *server) overview(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	data := page{Now: s.formatTime(now), RunDir: s.runDir, Config: s.configRaw != nil}
	jobs, err := s.jobs(now)
	if err != nil {
		data.Notes = append(data.Notes, "the jobs directory could not be listed: "+err.Error())
	}
	data.Jobs = jobs
	data.Lanes = lanes(jobs)
	if s.config != nil {
		data.Intake, data.Router, data.Selection = s.config["intake"], s.config["router"], s.config["model_selection"]
		if workflow, ok := s.config["workflow"].(map[string]any); ok {
			if stages, ok := workflow["stages"].([]any); ok {
				for _, stage := range stages {
					if m, ok := stage.(map[string]any); ok {
						data.Stages = append(data.Stages, stringOf(m["name"]))
					}
				}
			}
		}
	} else if s.configRaw != nil {
		data.Notes = append(data.Notes, "the configuration file is not valid JSON; see /config")
	}
	if log, err := tail(filepath.Join(s.runDir, "engine.log"), 20000); err == nil {
		data.Log = log
	} else if !errors.Is(err, os.ErrNotExist) {
		data.Notes = append(data.Notes, "engine.log could not be read: "+err.Error())
	} else {
		data.Notes = append(data.Notes, "engine.log is not present: the runtime was started without --log-file, so its own observations are only in its standard error")
	}
	s.render(w, "overview", data)
}

func (s *server) jobID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !jobName.MatchString(id) {
		http.NotFound(w, r)
		return "", false
	}
	if _, err := os.Stat(filepath.Join(s.runDir, "jobs", id)); err != nil {
		http.NotFound(w, r)
		return "", false
	}
	return id, true
}

func (s *server) jobPage(w http.ResponseWriter, r *http.Request) {
	id, ok := s.jobID(w, r)
	if !ok {
		return
	}
	now := time.Now()
	s.render(w, "job", page{Now: s.formatTime(now), RunDir: s.runDir, Job: s.loadJob(id, now, true), Config: s.configRaw != nil})
}

func (s *server) rawFile(w http.ResponseWriter, r *http.Request) {
	id, ok := s.jobID(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	var path string
	switch {
	case name == "issue.json" || name == "request.txt" || name == "engine.json" || name == "notices.json" || answerName.MatchString(name):
		path = filepath.Join(s.runDir, "jobs", id, name)
	case name == "history.json":
		path = filepath.Join(s.runDir, "jobs", id, "run", "history.json")
	default:
		http.NotFound(w, r)
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		http.Error(w, "could not be read: "+err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(raw)
}

func (s *server) workspacePage(w http.ResponseWriter, r *http.Request) {
	id, ok := s.jobID(w, r)
	if !ok {
		return
	}
	workspace := readWorkspace(filepath.Join(s.runDir, "jobs", id, "workspace"))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if workspace.Note != "" {
		fmt.Fprintln(w, workspace.Note)
	}
	fmt.Fprintf(w, "== git status --porcelain --untracked-files=all\n%s\n== git diff\n%s", workspace.Status, workspace.Diff)
	for _, file := range workspace.Untracked {
		fmt.Fprintf(w, "\n== untracked: %s\n%s", file.Name, file.Text)
	}
}

func (s *server) configPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if s.configRaw == nil {
		io.WriteString(w, "the status page was started without --config\n")
		return
	}
	var indented bytes.Buffer
	if err := json.Indent(&indented, s.configRaw, "", "  "); err != nil {
		w.Write(s.configRaw)
		return
	}
	w.Write(indented.Bytes())
}

func (s *server) logPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	file, err := os.Open(filepath.Join(s.runDir, "engine.log"))
	if err != nil {
		fmt.Fprintf(w, "engine.log could not be read: %v\n", err)
		return
	}
	defer file.Close()
	io.Copy(w, file)
}

// filesPage serves every file under the queue directory as it is, and lists
// every directory, so nothing the runtime wrote is out of reach of the page.
// The root refuses any path that leaves the queue, symbolic links included.
func (s *server) filesPage(w http.ResponseWriter, r *http.Request) {
	rel := strings.Trim(r.PathValue("path"), "/")
	if rel == "" {
		rel = "."
	}
	root, err := os.OpenRoot(s.runDir)
	if err != nil {
		http.Error(w, "the queue directory could not be opened: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer root.Close()
	file, err := root.Open(rel)
	if err != nil {
		http.Error(w, "could not be read: "+err.Error(), http.StatusNotFound)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		http.Error(w, "could not be read: "+err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if !info.IsDir() {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.Copy(w, file)
		return
	}
	entries, err := file.ReadDir(-1)
	if err != nil {
		http.Error(w, "could not be listed: "+err.Error(), http.StatusNotFound)
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var listing []fileEntry
	for _, entry := range entries {
		item := fileEntry{Name: entry.Name(), Dir: entry.IsDir()}
		if info, err := entry.Info(); err == nil {
			item.Size, item.Modified = info.Size(), info.ModTime()
		}
		listing = append(listing, item)
	}
	base := "/files/"
	if rel != "." {
		base += rel + "/"
	}
	s.render(w, "files", page{Now: s.formatTime(time.Now()), RunDir: s.runDir, Path: rel, Base: base, Files: listing})
}

type headData struct {
	Title   string
	Refresh int
}

type fileEntry struct {
	Name     string
	Dir      bool
	Size     int64
	Modified time.Time
}

const pageTemplates = `
{{define "head"}}<!DOCTYPE html><html><head><meta charset="utf-8"><meta http-equiv="refresh" content="{{.Refresh}}"><title>{{.Title}}</title>
<style>
body{font-family:system-ui,sans-serif;margin:1.2em;color:#222;background:#fafafa}
h1,h2,h3{margin:.8em 0 .3em}
table{border-collapse:collapse;width:100%;background:#fff}
th,td{border:1px solid #ccc;padding:.3em .5em;text-align:left;vertical-align:top;font-size:.95em}
pre{white-space:pre-wrap;word-break:break-word;background:#fff;border:1px solid #ddd;padding:.6em;margin:.3em 0;font-size:.9em}
.rec{border:1px solid #bbb;background:#fff;padding:.5em .8em;margin:.6em 0}
.rec.runtime{background:#f3f3f3;border-style:dashed}
.rec.person{background:#fff8e6;border-color:#c90}
.rec.error{border-color:#c33}
.err{color:#a00;font-weight:bold}
.live{border:2px solid #2a7;background:#f4fff8;padding:.5em .8em;margin:.6em 0}
.meta{color:#555;font-size:.9em}
details>summary{cursor:pointer;color:#246}
a{color:#246}
.status{font-weight:bold}
nav a{margin-right:1em}
.board{display:flex;gap:1em;align-items:flex-start;overflow-x:auto;padding-bottom:.5em}
.lane{flex:1 1 0;min-width:17em;background:#eceff3;border-radius:8px;padding:.5em .6em}
.lane h2{margin:.2em 0 .5em;font-size:1em;padding-left:.4em;border-left:6px solid #888}
.lane.running h2{border-color:#2a7}.lane.awaiting h2{border-color:#d90}.lane.attention h2{border-color:#c33}.lane.delivered h2{border-color:#46a}
.card{background:#fff;border-radius:6px;box-shadow:0 1px 2px rgba(0,0,0,.18);padding:.6em .8em;margin:.5em 0;border-left:5px solid #888}
.lane.running .card{border-color:#2a7}.lane.awaiting .card{border-color:#d90}.lane.attention .card{border-color:#c33}.lane.delivered .card{border-color:#46a}
.card .key{font-weight:bold}.card .title{margin:.2em 0 .4em}.card .line{color:#444;font-size:.92em;margin:.15em 0}.card .dots{letter-spacing:.15em;color:#357}
.card .attn{color:#a00;font-size:.9em;white-space:pre-wrap}
.empty{color:#888;font-size:.9em;padding:.4em}
</style></head><body>{{end}}

{{define "foot"}}<p class="meta">Rendered {{.Now}}; this page reloads by itself (every 10 seconds while a process runs, otherwise every 30) and shows the queue as it is on disk. Read only.</p></body></html>{{end}}

{{define "overview"}}{{template "head" (head "ticket engine status" 30)}}
<nav><a href="/">overview</a><a href="/config">configuration as read</a><a href="/log">runtime log</a><a href="/files/">every file of the queue</a></nav>
<h1>Requests</h1>
<p class="meta">Queue {{.RunDir}}, read at {{.Now}}.</p>
{{range .Notes}}<p class="err">{{.}}</p>{{end}}
{{if not .Jobs}}<p>No request has been accepted into this queue yet.</p>{{end}}
<div class="board">{{range .Lanes}}<div class="lane {{.Key}}"><h2>{{.Title}} ({{len .Jobs}})</h2>
{{range .Jobs}}<div class="card"><div class="key"><a href="/jobs/{{.ID}}">{{if .Key}}{{.Key}}{{else}}job {{.ID}}{{end}}</a></div><div class="title">{{.Title}}</div>
<div class="line status">{{.Status}}</div>{{if .Position}}<div class="line">{{.Position}} <span class="dots">{{.Dots}}</span></div>{{end}}
{{if .Attention}}<div class="attn">{{.Attention}}</div>{{end}}
<div class="line meta">elapsed {{.Elapsed}} &middot; last change {{ago .Updated}}{{if .Requester}} &middot; {{.Requester}}{{end}}</div></div>{{else}}<div class="empty">none</div>{{end}}</div>{{end}}</div>
{{if .Config}}<h2>Intake, as configured</h2><pre>{{pretty .Intake}}</pre>
{{if .Stages}}<h2>Stages of the run</h2><p>{{range $i, $s := .Stages}}{{if $i}} &rarr; {{end}}{{$s}}{{end}}</p>{{end}}
<h2>Decision and models</h2><pre>{{pretty .Router}}</pre><pre>{{pretty .Selection}}</pre>{{end}}
<h2>Runtime log (tail)</h2>{{if .Log}}<pre>{{.Log}}</pre><p class="meta"><a href="/log">the whole log</a></p>{{else}}<p class="meta">(nothing yet)</p>{{end}}
{{template "foot" .}}{{end}}

{{define "files"}}{{template "head" (head (printf "files: %s" .Path) 30)}}
<nav><a href="/">overview</a><a href="/config">configuration as read</a><a href="/log">runtime log</a><a href="/files/">every file of the queue</a></nav>
<h1>{{.RunDir}}{{if ne .Path "."}}/{{.Path}}{{end}}</h1>
<table><tr><th>Name</th><th>Size</th><th>Modified</th></tr>
{{if ne .Path "."}}<tr><td><a href="{{.Base}}../">../</a></td><td></td><td></td></tr>{{end}}
{{range .Files}}<tr><td><a href="{{$.Base}}{{.Name}}{{if .Dir}}/{{end}}">{{.Name}}{{if .Dir}}/{{end}}</a></td><td>{{if not .Dir}}{{.Size}}{{end}}</td><td>{{time .Modified}}</td></tr>{{end}}
</table>
{{template "foot" .}}{{end}}

{{define "job"}}{{with .Job}}{{template "head" (head (printf "%s status" .Key) .Refresh)}}
<nav><a href="/">overview</a><a href="/config">configuration as read</a><a href="/log">runtime log</a><a href="/files/jobs/{{.ID}}/">every file of this request</a></nav>
<h1>{{if .Key}}{{.Key}}{{else}}job {{.ID}}{{end}} {{.Title}}</h1>
<p><span class="status">{{.Status}}</span>{{if .Position}} &middot; {{.Position}}{{end}} &middot; elapsed {{.Elapsed}}</p>
<p class="meta">Requested by {{.Requester}} at {{.Created}}{{if .Link}} &middot; <a href="{{.Link}}">open the issue</a>{{end}} &middot; started {{time .Started}} &middot; last change {{time .Updated}} ({{ago .Updated}})
{{if .State}}{{if .State.Recovering}} &middot; <b>recovering</b>{{end}}{{if .State.Waiting}} &middot; <b>waiting for the requester</b>{{end}}{{if .State.Pending}} &middot; pending: {{.State.Pending.Role}}{{end}}{{end}}</p>
{{if .State}}{{if .State.Pending}}{{if .State.Pending.Instruction}}<details><summary>instruction of the pending action ({{.State.Pending.Role}})</summary><pre>{{.State.Pending.Instruction}}</pre></details>{{end}}{{end}}{{end}}
<p class="meta">Raw files: <a href="/jobs/{{.ID}}/raw/issue.json">issue.json</a> <a href="/jobs/{{.ID}}/raw/request.txt">request.txt</a> <a href="/jobs/{{.ID}}/raw/history.json">history.json</a> <a href="/jobs/{{.ID}}/raw/engine.json">engine.json</a> <a href="/jobs/{{.ID}}/raw/notices.json">notices.json</a> <a href="/jobs/{{.ID}}/workspace">workspace changes as text</a></p>
{{range .Notes}}<p class="err">{{.}}</p>{{end}}
{{range .Live}}<div class="live"><h2>Running now: {{.Role}} ({{.Speaker}}{{if .Model}}, {{.Model}}{{end}}) since {{time .Started}}, {{ago .Started}}</h2>
<details><summary>instruction handed to it</summary><pre>{{.Instruction}}</pre></details>
<h3>output so far</h3><pre>{{.Stdout}}</pre>{{if .Stderr}}<h3>diagnostics so far</h3><pre>{{.Stderr}}</pre>{{end}}
{{if .AgentLog}}<h3>the native agent's own log so far{{if .Home}} (<a href="/files/{{.Home}}/logs/agent.log">whole file</a>){{end}}</h3><pre>{{.AgentLog}}</pre>{{end}}</div>{{end}}
{{if .State}}{{if .State.Workflow}}{{if .State.Workflow.Stages}}<p class="meta">Stages: {{range $i, $s := .State.Workflow.Stages}}{{if $i}} &rarr; {{end}}{{if eq $s.Name $.Job.State.Step}}<b>{{$s.Name}}</b>{{else}}{{$s.Name}}{{end}}{{end}}</p>{{end}}{{end}}{{end}}
{{if .State}}{{if .State.Workflow}}<details><summary>workflow as recorded for this request</summary><pre>{{pretty .State.Workflow}}</pre></details>{{end}}{{end}}
{{if .Stages}}<h2>Time by stage</h2><table><tr><th>Stage</th><th>Launches</th><th>Failed</th><th>Total time</th><th>First started</th><th>Last finished</th></tr>
{{range .Stages}}<tr><td>{{.Name}}</td><td>{{.Launches}}</td><td>{{.Failures}}</td><td>{{.Total}}</td><td>{{time .First}}</td><td>{{time .Last}}</td></tr>{{end}}</table>{{end}}
<h2>Request</h2><pre>{{.Request}}</pre>
{{if .Notices}}<h2>Notices posted by the runtime</h2>{{range .Notices}}<pre>{{pretty .}}</pre>{{end}}{{end}}
<h2>Record ({{len .Records}} entries)</h2>
{{range .Records}}<div class="rec{{if .Runtime}} runtime{{end}}{{if .Person}} person{{end}}{{if .Error}} error{{end}}"><p><b>{{.Index}}. {{.Role}}</b> &middot; {{.Speaker}}{{if .Model}} &middot; {{.Model}}{{end}} &middot; {{time .Started}} &rarr; {{time .Finished}} ({{.Duration}}){{if .Gap}} &middot; <span class="meta">{{.Gap}} after the previous record</span>{{end}}</p>
{{if .Error}}<p class="err">Error</p><pre>{{.Error}}</pre>{{end}}
<details><summary>instruction</summary><pre>{{.Instruction}}</pre></details>
<p>Output</p><pre>{{.Output}}</pre>
{{if .Diagnostics}}<details><summary>diagnostics (stderr)</summary><pre>{{.Diagnostics}}</pre></details>{{end}}</div>{{end}}
{{if .Answers}}<h2>Requester answers consumed</h2>{{range .Answers}}<p class="meta">{{.Name}}</p><pre>{{.Text}}</pre>{{end}}{{end}}
{{if .Reviews}}<h2>Review findings kept by the review command</h2>{{range .Reviews}}<p class="meta">{{.Name}}</p><pre>{{.Text}}</pre>{{end}}{{end}}
{{if .Report}}<h2>Report written in the workspace</h2><pre>{{.Report}}</pre>{{end}}
{{if .Receipt}}<h2>Delivery receipt written by the delivery program</h2><pre>{{.Receipt}}</pre>{{end}}
{{if .Homes}}<h2>What each role's native agent logged in its own directory</h2>{{range .Homes}}{{$h := .}}<div class="rec"><p><b>home {{$h.Name}}</b> &middot; files: {{range $i, $f := $h.Files}}{{if $i}}, {{end}}<a href="/files/jobs/{{$.Job.ID}}/homes/{{$h.Name}}/{{$f}}">{{$f}}</a>{{end}}</p>
{{if $h.Calls}}<p class="meta">as the agent logged it: {{$h.Calls}} model calls, {{$h.TokensIn}} input tokens, {{$h.TokensOut}} output tokens</p>{{end}}
{{if $h.AgentLog}}<details><summary>agent.log (tail)</summary><pre>{{$h.AgentLog}}</pre></details>{{end}}{{if $h.ErrorsLog}}<details><summary>errors.log (tail)</summary><pre>{{$h.ErrorsLog}}</pre></details>{{end}}
{{if $h.Transcript}}<details><summary>the whole conversation the agent had (messages, tool calls and their results)</summary><pre>{{$h.Transcript}}</pre></details>{{end}}</div>{{end}}{{end}}
<h2>Workspace changes</h2>{{with .Workspace}}{{if .Note}}<p class="meta">{{.Note}}</p>{{end}}
{{if .Status}}<pre>{{.Status}}</pre>{{else}}{{if not .Note}}<p class="meta">no change in the checkout</p>{{end}}{{end}}
{{if .Log}}<details><summary>recent commits in the checkout</summary><pre>{{.Log}}</pre></details>{{end}}
{{if .Diff}}<details open><summary>diff of tracked files</summary><pre>{{.Diff}}</pre></details>{{end}}
{{range .Untracked}}<details><summary>new file: {{.Name}}</summary><pre>{{.Text}}</pre></details>{{end}}{{end}}
{{template "foot" $}}{{end}}{{end}}
`
