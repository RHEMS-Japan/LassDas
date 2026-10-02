// Command status serves a read-only page about one runtime's queue: where each
// accepted request is, what every role was told and what it answered, what is
// running at this moment and what the runtime itself observed. It holds no
// credential, writes nothing under the queue and shows what is on disk as it
// is; nothing here decides or changes the course of a request.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
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
	"ticket-runner/internal/stagename"
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
		"time": s.formatTime, "ago": s.ago, "pretty": prettyJSON, "t": translate, "st": localize, "sn": stageName, "cls": cssClass,
		"pct": func(index, count int) int {
			if count <= 0 {
				return 0
			}
			return index * 100 / count
		},
		"head": func(lang, title string, refresh int) headData {
			return headData{Title: translate(lang, title), Refresh: refresh, Lang: lang}
		},
		"column": func(p page, c column) columnData { return columnData{Page: p, Lang: p.Lang, Col: c} },
		"card": func(p page, j *job) cardData {
			return cardData{Lang: p.Lang, ID: j.ID, Key: j.Key, Title: j.Title, Lane: j.Lane, Status: j.Status,
				Position: j.Position, StageIndex: j.StageIndex, StageCount: j.StageCount, Model: j.Model, Attention: j.Attention, Failure: j.Failure,
				Elapsed: j.Elapsed, Updated: j.Updated, Requester: j.Requester}
		},
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
	mux.HandleFunc("GET /lang/{lang}", s.auth(s.languagePage))
	return mux
}

func (s *server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if s.user != "" {
			user, password, ok := r.BasicAuth()
			userHash, wantUser := sha256.Sum256([]byte(user)), sha256.Sum256([]byte(s.user))
			passwordHash, wantPassword := sha256.Sum256([]byte(password)), sha256.Sum256([]byte(s.password))
			matched := subtle.ConstantTimeCompare(userHash[:], wantUser[:]) & subtle.ConstantTimeCompare(passwordHash[:], wantPassword[:])
			if !ok || matched != 1 {
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
	ID          string
	Key         string
	Title       string
	Requester   string
	Created     string
	Link        string
	Request     string
	State       *chain.State
	Records     []record
	Launches    []launch
	LaunchCount int
	Answers     []namedText
	Live        []liveEntry
	Notices     []map[string]any
	Report      string
	Reviews     []namedText
	Notes       []string
	Status      string
	Position    string
	Started     time.Time
	Updated     time.Time
	Elapsed     string
	Workspace   *workspace
	Receipt     string
	Homes       []homeLogs
	Stages      []stageTime
	Refresh     int
	Lane        string
	Dots        string
	Attention   string
	Trail       []stageMark
	Current     string
	Stage       string
	Failure     string
	Stopped     bool
	Reported    bool
	StopBroken  bool
	NoRecord    bool
	StageIndex  int
	StageCount  int
	Model       string
}

// stageMark is one stage on a card: passed, current or ahead.
type stageMark struct {
	Name  string
	State string
}

// lane is one column of the board: the requests in one of four situations
// the operator reads at a glance, in the order the old board used.
// lane is one of the four situations a request can be in; the board shows it
// as the colour and badge of a card, and the summary strip counts them.
type lane struct {
	Key   string
	Title string
	Count int
}

var laneOrder = []lane{{Key: "queued", Title: "Queued"}, {Key: "running", Title: "Running"}, {Key: "awaiting", Title: "Awaiting answer"},
	{Key: "attention", Title: "Needs attention"}, {Key: "delivered", Title: "Delivered"}, {Key: "unchanged", Title: "Done without a change"},
	{Key: "unmerged", Title: "Done with the pull request open"}, {Key: "closed", Title: "Done, the pull request closed unmerged"},
	{Key: "stopped", Title: "Stopped"}}

func lanes(jobs []*job) []lane {
	result := make([]lane, len(laneOrder))
	copy(result, laneOrder)
	for _, j := range jobs {
		for i := range result {
			if result[i].Key == j.Lane {
				result[i].Count++
			}
		}
	}
	return result
}

// column is one stage of the configured run on the board, holding the
// requests that are at that stage now; the last column holds the delivered.
type column struct {
	Key  string
	Jobs []*job
	Done bool
}

func columns(stages []string, jobs []*job) []column {
	result := make([]column, 0, len(stages)+2)
	index := map[string]int{}
	for _, name := range stages {
		index[name] = len(result)
		result = append(result, column{Key: name})
	}
	other := -1
	for _, j := range jobs {
		place := j.Stage
		if place == "" {
			place = j.Current
		}
		// A queued request has no record of its own to place it by; it will
		// begin at the board's first stage.
		if place == "" && j.Lane == "queued" && len(stages) > 0 {
			place = stages[0]
		}
		switch at, known := index[place]; {
		case j.State != nil && j.State.Done:
			continue
		case known:
			result[at].Jobs = append(result[at].Jobs, j)
		default:
			if other < 0 {
				other = len(result)
				result = append(result, column{Key: "other"})
			}
			result[other].Jobs = append(result[other].Jobs, j)
		}
	}
	done := column{Key: "done", Done: true}
	for _, j := range jobs {
		if j.State != nil && j.State.Done {
			done.Jobs = append(done.Jobs, j)
		}
	}
	return append(result, done)
}

// stopState reads what the runtime leaves for a request the requester
// stopped: the saved instruction, and the report's own record once the
// report has been posted and read back.
func stopState(dir string) (stopped, reported, broken bool) {
	raw, err := os.ReadFile(filepath.Join(dir, "stop-request.json"))
	if err != nil {
		return false, false, false
	}
	var instruction map[string]any
	if json.Unmarshal(raw, &instruction) != nil || instruction == nil {
		return false, false, true
	}
	raw, err = os.ReadFile(filepath.Join(dir, "stop-report", "history.json"))
	if err != nil {
		return true, false, false
	}
	var report struct {
		Done bool `json:"done"`
	}
	return true, json.Unmarshal(raw, &report) == nil && report.Done, false
}

// cssClass turns an outcome's words into one class name.
func cssClass(text string) string {
	return strings.ReplaceAll(strings.ToLower(text), " ", "-")
}

func firstLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

func isStage(stages []chain.Stage, name string) bool {
	for _, stage := range stages {
		if stage.Name == name {
			return true
		}
	}
	return false
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

type homeFile struct {
	Name string
	Link bool
}

// homeLogs is what a role's own private directory holds of its native agent's
// log: the steps it took, as the agent itself wrote them.
type homeLogs struct {
	Name       string
	AgentLog   string
	ErrorsLog  string
	Files      []homeFile
	Calls      int
	TokensIn   int
	TokensOut  int
	UsageNote  string
	Transcript string
	MoreFiles  int
}

// homeFileLimit bounds how many of a role's files the request page names; a
// toolchain cache in a role's directory holds thousands, all under files.
const homeFileLimit = 200

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

// loadJob reads one request. With detail, everything on disk about it is
// read; without, only what a card on the board needs (the issue, the run
// record, the live copy and the notices), so the overview stays cheap.
func (s *server) loadJob(id string, now time.Time, detail bool) *job {
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
	if raw, err := os.ReadFile(historyPath); errors.Is(err, os.ErrNotExist) {
		// Accepted on this tick; the runtime writes the record on the next.
		j.NoRecord = true
	} else if err != nil {
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
			j.Launches = groupLaunches(j.Records)
			for _, entry := range j.Launches {
				if len(entry.Workers) > 0 && entry.Outcome != answered {
					j.LaunchCount += entry.Count
				}
			}
		}
		touch(historyPath)
	}
	j.Stopped, j.Reported, j.StopBroken = stopState(dir)
	if !detail {
		if raw, err := os.ReadFile(filepath.Join(dir, "notices.json")); err == nil {
			var log struct {
				Notices []map[string]any `json:"notices"`
			}
			if json.Unmarshal(raw, &log) == nil {
				j.Notices = log.Notices
			}
		}
		if names, _ := filepath.Glob(filepath.Join(dir, "live", "*.json")); len(names) > 0 {
			for _, name := range names {
				if raw, err := os.ReadFile(name); err == nil {
					var entry struct {
						Role string `json:"role"`
					}
					if json.Unmarshal(raw, &entry) == nil {
						j.Live = append(j.Live, liveEntry{Role: entry.Role})
					}
				}
				touch(name)
			}
		}
		// A finished request's card says whether anything was delivered,
		// which only the delivery's receipt records.
		if j.State != nil && j.State.Done {
			if raw, err := os.ReadFile(filepath.Join(dir, "workspace", ".git", "ticket-engine", "delivery.json")); err == nil {
				j.Receipt = string(raw)
			}
		}
		j.derive(now)
		return j
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
					if log, err := s.readIn(filepath.Join(rel, "logs", "agent.log"), 20000); err == nil {
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
			homeRel := filepath.Join("jobs", id, "homes", home.Name())
			if text, err := s.readIn(filepath.Join(homeRel, "logs", "agent.log"), 20000); err == nil {
				logs.AgentLog = text
				if size, err := s.sizeIn(filepath.Join(homeRel, "logs", "agent.log")); err == nil && size <= usageLimit {
					if whole, err := s.readIn(filepath.Join(homeRel, "logs", "agent.log"), 0); err == nil {
						logs.Calls, logs.TokensIn, logs.TokensOut = agentUsage(whole)
					}
				} else {
					logs.UsageNote = "the agent's log is larger than the page scans for its counts"
				}
			}
			if size, err := s.sizeIn(filepath.Join(homeRel, "transcript.json")); err == nil {
				if size <= usageLimit {
					if text, err := s.readIn(filepath.Join(homeRel, "transcript.json"), 0); err == nil {
						var indented bytes.Buffer
						if json.Indent(&indented, []byte(text), "", "  ") == nil {
							logs.Transcript = indented.String()
						} else {
							logs.Transcript = text
						}
					}
				} else {
					logs.UsageNote += " the transcript is larger than the page shows inline; it is under files"
				}
			}
			if text, err := s.readIn(filepath.Join(homeRel, "logs", "errors.log"), 20000); err == nil {
				logs.ErrorsLog = text
			}
			filepath.WalkDir(filepath.Join(dir, "homes", home.Name()), func(path string, entry os.DirEntry, err error) error {
				if err == nil && !entry.IsDir() {
					if len(logs.Files) >= homeFileLimit {
						logs.MoreFiles++
						return nil
					}
					if rel, err := filepath.Rel(filepath.Join(dir, "homes", home.Name()), path); err == nil {
						logs.Files = append(logs.Files, homeFile{Name: filepath.ToSlash(rel), Link: entry.Type()&fs.ModeSymlink != 0})
					}
				}
				return nil
			})
			j.Homes = append(j.Homes, logs)
		}
	}
	j.derive(now)
	j.Workspace = s.readWorkspace(id)
	return j
}

// endedUnchanged says whether the delivery's own receipt records an ending
// with no change: nothing committed, pushed or merged. It is the delivery
// process's record, read for that one field; no role's words decide it.
func (j *job) endedUnchanged() bool {
	var receipt struct {
		Unchanged bool `json:"unchanged"`
	}
	return j.Receipt != "" && json.Unmarshal([]byte(j.Receipt), &receipt) == nil && receipt.Unchanged
}

// endedClosed says whether the delivery's own receipt records that a person
// closed the pull request without merging it, so nothing was delivered.
func (j *job) endedClosed() bool {
	var receipt struct {
		Closed bool `json:"closed_unmerged"`
	}
	return j.Receipt != "" && json.Unmarshal([]byte(j.Receipt), &receipt) == nil && receipt.Closed
}

// endedUnmerged says whether the delivery's own receipt records a pull request
// left open for a person to merge and no merge yet: nothing has reached the
// integration branch.
func (j *job) endedUnmerged() bool {
	var receipt struct {
		LeftToPerson bool   `json:"merge_left_to_person"`
		MergeSHA     string `json:"merge_sha"`
	}
	return j.Receipt != "" && json.Unmarshal([]byte(j.Receipt), &receipt) == nil && receipt.LeftToPerson && receipt.MergeSHA == ""
}

func (j *job) derive(now time.Time) {
	end := now
	if state := j.State; state != nil {
		var last time.Time
		for _, result := range state.History {
			started := result.StartedAt
			if started.IsZero() {
				started = result.FinishedAt
			}
			if !started.IsZero() && (j.Started.IsZero() || started.Before(j.Started)) {
				j.Started = started
			}
			if result.FinishedAt.After(last) {
				last = result.FinishedAt
			}
		}
		switch {
		case state.Done && j.endedUnchanged():
			j.Status = "done without a change; nothing was delivered"
			if !last.IsZero() {
				end = last
			}
		case state.Done && j.endedClosed():
			j.Status = "done; the pull request was closed without being merged, so nothing was delivered"
			if !last.IsZero() {
				end = last
			}
		case state.Done && j.endedUnmerged():
			j.Status = "done; the pull request is open, its merge left to a person"
			if !last.IsZero() {
				end = last
			}
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
		// The runtime marks a step it takes up again: after a launch that
		// failed, or after its own process was interrupted with the action
		// pending. The card says which, and names the failure it retries.
		if state.Recovering {
			prefix := "retrying after a failure; "
			if n := len(state.History); n > 0 && strings.HasPrefix(state.History[n-1].Error, "The process stopped while this action was pending") {
				prefix = "taking up an interrupted step; "
			} else {
				// The latest launch is one role's process records behind the
				// runtime's notes; any of them may be the one that failed.
				role := ""
				for i := len(state.History) - 1; i >= 0; i-- {
					record := state.History[i]
					if record.Speaker == "runtime" {
						if role != "" {
							break
						}
						continue
					}
					if role != "" && record.Role != role {
						break
					}
					role = record.Role
					if record.Error != "" && j.Failure == "" {
						j.Failure = firstLine(record.Error)
					}
				}
			}
			j.Status = prefix + j.Status
		}
		// The position is the stage running now when one is, else the last
		// stage the record reached: the record's step moves only after a
		// launch returns, and a card must not name the previous stage while
		// the next one visibly runs.
		current := state.Step
		if len(j.Live) > 0 {
			current = j.Live[0].Role
		} else if state.Pending != nil && !state.Done {
			current = state.Pending.Role
		}
		j.Current = current
		if state.Workflow != nil && len(state.Workflow.Stages) > 0 && current != "" {
			j.Position = current
			j.StageCount = len(state.Workflow.Stages)
			// A role outside the sequence (a question to the requester, a
			// stop report) is a side step of the stage that reached it, so the
			// card keeps that stage's place and progress.
			stage := current
			if !isStage(state.Workflow.Stages, stage) {
				for i := len(state.History) - 1; i >= 0; i-- {
					if isStage(state.Workflow.Stages, state.History[i].Role) {
						stage = state.History[i].Role
						break
					}
				}
			}
			for i, candidate := range state.Workflow.Stages {
				if candidate.Name == stage {
					j.Stage = stage
					j.StageIndex = i + 1
					j.Position = fmt.Sprintf("step %d of %d: %s", i+1, len(state.Workflow.Stages), stage)
				}
			}
			if state.Done {
				j.StageIndex = j.StageCount
			}
		} else if current != "" {
			j.Position = current
		}
		for _, entry := range j.Live {
			if entry.Model != "" {
				j.Model = entry.Model
			}
		}
		if j.Model == "" {
			for i := len(state.History) - 1; i >= 0; i-- {
				if state.History[i].Model != "" {
					j.Model = state.History[i].ModelPrefix + state.History[i].Model
					break
				}
			}
		}
	} else {
		if !j.NoRecord || j.Key == "" {
			j.Status = "no run record yet"
		}
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
		// Only a request the runtime accepted, with its issue on disk, is
		// queued; a directory left by an interrupted acceptance is not.
		if j.NoRecord && j.Key != "" {
			j.Lane, j.Status = "queued", "queued: waiting for a free execution slot"
		}
		for _, note := range j.Notes {
			if strings.Contains(note, "history.json could not be decoded") {
				j.Lane, j.Attention = "attention", note
			}
		}
	case state.Done && j.endedUnchanged():
		j.Lane = "unchanged"
	case state.Done && j.endedClosed():
		j.Lane = "closed"
	case state.Done && j.endedUnmerged():
		j.Lane = "unmerged"
	case state.Done:
		j.Lane = "delivered"
	case state.Waiting:
		j.Lane = "awaiting"
	case state.Step == "" && len(state.History) == 0 && state.Pending == nil && len(j.Live) == 0:
		// Nothing has been launched. The record written at acceptance holds
		// no run definition; the run writes it as it starts, before it
		// chooses its first stage. So a record without one is a request
		// waiting for a free execution slot, and a record with one is a
		// request that has the slot and is choosing where to begin.
		if state.Workflow == nil {
			j.Lane, j.Status = "queued", "queued: waiting for a free execution slot"
		} else if len(state.Workflow.Stages) > 0 {
			j.Status, j.Stage = "starting: choosing the first stage", state.Workflow.Stages[0].Name
		} else {
			j.Status = "starting: choosing the first role"
		}
	default:
		var last time.Time
		if n := len(state.History); n > 0 {
			last = state.History[n-1].FinishedAt
			record := state.History[n-1]
			// The runtime's own failure (a router it could not reach) needs a
			// person; its note that a stopped action is being taken up again
			// after a restart is the run going on, not a call for attention.
			if record.Speaker == "runtime" && record.Error != "" && len(j.Live) == 0 &&
				!strings.HasPrefix(record.Error, "The process stopped while this action was pending") {
				j.Lane, j.Attention = "attention", record.Error
			}
		}
		// A budget pause holds the request until the runtime says the budget
		// is back, so the later of the two lines is the one that stands; a
		// no-progress notice stands until a step completes after it. "Later"
		// is the order in the file: the runtime only appends to notices.json,
		// stamping each line as it is written.
		var pause, stall string
		for _, notice := range j.Notices {
			written, err := time.Parse(time.RFC3339Nano, stringOf(notice["written_at"]))
			if err != nil || !written.After(last) {
				continue
			}
			switch stringOf(notice["kind"]) {
			case "budget-paused":
				pause = stringOf(notice["text"])
			case "budget-restored":
				pause = ""
			case "stall":
				stall = stringOf(notice["text"])
			}
		}
		if pause != "" {
			j.Lane, j.Attention = "attention", pause
		} else if stall != "" {
			j.Lane, j.Attention = "attention", stall
		}
	}
	// A request the requester stopped is over: nothing runs, nothing needs a
	// person, and the only thing left to say is whether its report went out.
	// A delivered request stays delivered: a stop after that changed nothing.
	// A stop record the runtime cannot read holds the work, so it needs one.
	switch {
	case j.StopBroken && (j.State == nil || !j.State.Done):
		j.Lane, j.Attention = "attention", "the saved stop instruction is unreadable; the work is held"
		j.Status = "held: the saved stop instruction is unreadable"
	case j.Stopped && (j.State == nil || !j.State.Done):
		j.Lane, j.Attention = "stopped", ""
		j.Status = "stopped by the requester; report pending"
		if j.Reported {
			j.Status = "stopped by the requester; report posted"
		}
	}
	// The trail of stages on the card: passed, current and ahead; a finished
	// request has passed them all, a waiting one is still at its stage.
	if state := j.State; state != nil && state.Workflow != nil && len(state.Workflow.Stages) > 0 {
		current := state.Step
		if len(j.Live) > 0 {
			current = j.Live[0].Role
		} else if state.Pending != nil && !state.Done {
			current = state.Pending.Role
		}
		var dots strings.Builder
		reached := false
		for _, stage := range state.Workflow.Stages {
			switch {
			case state.Done:
				dots.WriteString("●")
				j.Trail = append(j.Trail, stageMark{Name: stage.Name, State: "passed"})
			case stage.Name == current:
				dots.WriteString("◉")
				j.Trail = append(j.Trail, stageMark{Name: stage.Name, State: "current"})
				reached = true
			case reached:
				dots.WriteString("○")
				j.Trail = append(j.Trail, stageMark{Name: stage.Name, State: "ahead"})
			default:
				dots.WriteString("●")
				j.Trail = append(j.Trail, stageMark{Name: stage.Name, State: "passed"})
			}
		}
		j.Dots = dots.String()
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
	NotShown  int
	Log       string
	Note      string
}

const untrackedLimit = 200000

// untrackedFiles and untrackedTotal bound what the request page shows of new
// files inline; the rest is named and left to the file browser.
const untrackedFiles, untrackedTotal = 50, 2 << 20

// gitRead runs one git query in a checkout the roles write. Nothing the
// checkout's own configuration names is executed (hooks, the file monitor,
// external diff and textconv drivers are all switched off), and the process
// receives none of the page's environment, so a credential the page holds
// cannot reach a program named there.
func gitRead(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "--no-optional-locks",
		"-c", "core.hooksPath=" + os.DevNull, "-c", "core.fsmonitor=false", "-c", "diff.external=",
		"-c", "core.quotePath=false", "-c", "core.pager=cat"}, args...)...)
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.TempDir(), "GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_CONFIG_SYSTEM=" + os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "LANG=C.UTF-8"}
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

func (s *server) readWorkspace(id string) *workspace {
	rel := filepath.Join("jobs", id, "workspace")
	dir := filepath.Join(s.runDir, rel)
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
	// Staged and unstaged changes alike: the delivery program stages before
	// it commits, and a page read between the two must not show a name with
	// no content. A checkout without a commit yet has no HEAD to diff against.
	diff, err := gitRead(dir, "diff", "--no-color", "--no-ext-diff", "--no-textconv", "HEAD")
	if err != nil {
		diff, err = gitRead(dir, "diff", "--no-color", "--no-ext-diff", "--no-textconv")
	}
	if err != nil {
		w.Note = "git diff could not be read: " + err.Error()
	} else {
		w.Diff = diff
	}
	shown, total := 0, 0
	for _, line := range strings.Split(status, "\n") {
		if !strings.HasPrefix(line, "?? ") {
			continue
		}
		name := strings.TrimPrefix(line, "?? ")
		if shown >= untrackedFiles || total >= untrackedTotal {
			w.NotShown++
			continue
		}
		if info, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(name))); err == nil && info.Mode()&fs.ModeSymlink != 0 {
			w.Untracked = append(w.Untracked, namedText{Name: name, Text: "(symbolic link; not followed, the page stays inside the queue)"})
			continue
		}
		text, err := s.headIn(filepath.Join(rel, filepath.FromSlash(name)), untrackedLimit)
		if err != nil {
			w.Untracked = append(w.Untracked, namedText{Name: name, Text: "could not be read: " + err.Error()})
			continue
		}
		w.Untracked = append(w.Untracked, namedText{Name: name, Text: text})
		shown++
		total += len(text)
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

func (s *server) ago(lang string, t time.Time) string {
	if t.IsZero() {
		return ""
	}
	if lang == "ja" {
		return humanDuration(time.Since(t)) + " 前"
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

// readIn reads a file by its path relative to the queue through the queue's
// root, so a symbolic link a role planted in its own directory cannot lead the
// page outside the queue. With a limit, only the last limit bytes are read
// (seeking, not loading the file) and a marker says how much lies before them.
func (s *server) readIn(rel string, limit int64) (string, error) {
	root, err := os.OpenRoot(s.runDir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	file, err := root.Open(filepath.ToSlash(rel))
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory", rel)
	}
	if limit > 0 && info.Size() > limit {
		if _, err := file.Seek(info.Size()-limit, io.SeekStart); err != nil {
			return "", err
		}
		raw, err := io.ReadAll(io.LimitReader(file, limit))
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("[%d earlier bytes not shown; the whole file is under files]\n", info.Size()-limit) + string(raw), nil
	}
	raw, err := io.ReadAll(file)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// usageLimit bounds how much of a native agent's log is scanned for its own
// call and token counts, and how large a transcript is shown inline; a larger
// file is left to the file browser and the page says so.
const usageLimit = 8 << 20

// headIn reads at most limit bytes from the start of a file through the
// queue's root, and says how much of the file lies beyond them, without ever
// loading the rest.
func (s *server) headIn(rel string, limit int64) (string, error) {
	root, err := os.OpenRoot(s.runDir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	file, err := root.Open(filepath.ToSlash(rel))
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory", rel)
	}
	raw, err := io.ReadAll(io.LimitReader(file, limit))
	if err != nil {
		return "", err
	}
	if info.Size() > limit {
		return string(raw) + fmt.Sprintf("\n[cut here: %d more bytes on disk]\n", info.Size()-limit), nil
	}
	return string(raw), nil
}

// sizeIn reports a file's size through the queue's root without reading it.
func (s *server) sizeIn(rel string) (int64, error) {
	root, err := os.OpenRoot(s.runDir)
	if err != nil {
		return 0, err
	}
	defer root.Close()
	info, err := root.Stat(filepath.ToSlash(rel))
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

type page struct {
	Lang      string
	Now       string
	RunDir    string
	Path      string
	Base      string
	Files     []fileEntry
	Jobs      []*job
	Lanes     []lane
	Columns   []column
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
	data := page{Lang: language(r), Now: s.formatTime(now), RunDir: s.runDir, Config: s.configRaw != nil}
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
	if len(data.Stages) == 0 {
		// Without a configuration, the stages a request recorded for itself.
		for _, j := range jobs {
			if j.State != nil && j.State.Workflow != nil && len(j.State.Workflow.Stages) > 0 {
				for _, stage := range j.State.Workflow.Stages {
					data.Stages = append(data.Stages, stage.Name)
				}
				break
			}
		}
	}
	data.Columns = columns(data.Stages, jobs)
	if log, err := s.readIn("engine.log", 20000); err == nil {
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
	s.render(w, "job", page{Lang: language(r), Now: s.formatTime(now), RunDir: s.runDir, Job: s.loadJob(id, now, true), Config: s.configRaw != nil})
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
	workspace := s.readWorkspace(id)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if workspace.Note != "" {
		fmt.Fprintln(w, workspace.Note)
	}
	fmt.Fprintf(w, "== git status --porcelain --untracked-files=all\n%s\n== git diff\n%s", workspace.Status, workspace.Diff)
	for _, file := range workspace.Untracked {
		fmt.Fprintf(w, "\n== untracked: %s\n%s", file.Name, file.Text)
	}
	if workspace.NotShown > 0 {
		fmt.Fprintf(w, "\n== %d more new files are not shown here; they are under /files/\n", workspace.NotShown)
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
		item := fileEntry{Name: entry.Name(), Dir: entry.IsDir(), Link: entry.Type()&fs.ModeSymlink != 0,
			Href: url.PathEscape(entry.Name())}
		if info, err := entry.Info(); err == nil {
			item.Size, item.Modified = info.Size(), info.ModTime()
		}
		listing = append(listing, item)
	}
	base := "/files/"
	if rel != "." {
		for _, segment := range strings.Split(rel, "/") {
			base += url.PathEscape(segment) + "/"
		}
	}
	s.render(w, "files", page{Lang: language(r), Now: s.formatTime(time.Now()), RunDir: s.runDir, Path: rel, Base: base, Files: listing})
}

// cardData is one request as a card on the board, with the viewer's language.
// columnData is one column of the board with the page it belongs to, so
// the column template can hand each card the page.
type columnData struct {
	Page page
	Lang string
	Col  column
}

type cardData struct {
	Lang       string
	ID         string
	Key        string
	Title      string
	Lane       string
	Status     string
	Position   string
	StageIndex int
	StageCount int
	Model      string
	Attention  string
	Failure    string
	Elapsed    string
	Updated    time.Time
	Requester  string
}

type headData struct {
	Title   string
	Refresh int
	Lang    string
}

// language is the viewer's choice of labels, kept in a cookie; the content of
// the queue is shown as it is in either language.
func language(r *http.Request) string {
	if cookie, err := r.Cookie("lang"); err == nil && cookie.Value == "ja" {
		return "ja"
	}
	return "en"
}

func (s *server) languagePage(w http.ResponseWriter, r *http.Request) {
	lang := r.PathValue("lang")
	if lang != "ja" && lang != "en" {
		http.NotFound(w, r)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "lang", Value: lang, Path: "/", MaxAge: 365 * 24 * 3600, SameSite: http.SameSiteLaxMode})
	back := "/"
	if referer, err := url.Parse(r.Referer()); err == nil && strings.HasPrefix(referer.Path, "/") &&
		!strings.HasPrefix(referer.Path, "//") && !strings.HasPrefix(referer.Path, "/\\") && !strings.ContainsAny(referer.Path, "\\") {
		back = referer.Path
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

var japanese = map[string]string{
	"ticket engine status": "自動処理の状態", "skip to content": "本文へ", "overview": "一覧", "configuration as read": "読み込まれた設定", "runtime log": "本体のログ",
	"every file of the queue": "queue の全ファイル", "every file of this request": "この依頼の全ファイル", "Requests": "依頼",
	"Queue": "queue", "read at": "読み取り時刻", "No request has been accepted into this queue yet.": "この queue に受け付けた依頼はまだありません。",
	"Running": "実行中", "Awaiting answer": "返事待ち", "Needs attention": "要対応", "Delivered": "納品済み", "Stopped": "停止", "Queued": "順番待ち", "none": "なし",
	"queued: waiting for a free execution slot": "順番待ち (実行枠が空くのを待っています)", "starting: choosing the first stage": "開始中 (最初の工程を決めています)", "starting: choosing the first role": "開始中 (最初の担当を決めています)",
	"stopped by the requester; report posted": "依頼者が停止。報告済み", "stopped by the requester; report pending": "依頼者が停止。報告を準備中",
	"the saved stop instruction is unreadable; the work is held": "保存された停止指示が読めないため、作業を保留中です",
	"held: the saved stop instruction is unreadable":             "保留中: 保存された停止指示が読めません",
	"elapsed": "経過", "last change": "最終更新", "last failure": "直近の失敗", "Intake, as configured": "受付の設定", "Stages of the run": "工程の並び",
	"Decision and models": "判断とモデル", "Runtime log (tail)": "本体のログ (末尾)", "(nothing yet)": "(まだ何もない)", "the whole log": "ログ全文",
	"Rendered": "表示時刻", "this page reloads by itself (every 10 seconds while a process runs, otherwise every 30) and shows the queue as it is on disk. Read only.": "この画面は自動で更新され (工程の実行中は 10 秒ごと、それ以外は 30 秒ごと)、ディスク上の queue をそのまま表示します。読み取り専用。",
	"status": "状態", "Requested by": "依頼者", "at": "起票", "open the issue": "チケットを開く", "started": "開始",
	"waiting for the requester": "依頼者の返事待ち", "pending:": "実行待ち:", "instruction of the pending action": "実行待ちの工程への指示", "Raw files:": "生のファイル:",
	"workspace changes as text": "作業場所の変更 (テキスト)", "Running now:": "実行中:", "since": "開始", "instruction handed to it": "渡した指示",
	"output so far": "ここまでの出力", "diagnostics so far": "ここまでの stderr", "the native agent's own log so far": "agent 自身のログ (ここまで)", "whole file": "全文",
	"Stages:": "工程:", "workflow as recorded for this request": "この依頼に記録された工程定義", "Time by stage": "工程別の時間", "Stage": "工程", "Launches": "回数",
	"Failed": "失敗", "Total time (sum over process runs; parallel processes add up)": "合計時間 (プロセスごとの合算。並列は足し合わせ)", "First started": "最初の開始", "Last finished": "最後の終了",
	"Request": "依頼の原文", "Notices posted by the runtime": "本体が投稿した通知", "Record": "記録", "entries": "件", "after the previous record": "前の記録から",
	"Error": "失敗", "instruction": "本体が担当に渡した説明", "Output": "出力", "diagnostics (stderr)": "stderr", "Requester answers consumed": "取り込んだ依頼者の回答",
	"What happened, launch by launch": "起きたこと (起動ごと)", "launch": "起動", "launches": "回の起動", "raw records": "生の記録", "the same failure, repeated": "同じ失敗の繰り返し", "times": "回",
	"returned": "完了", "failed": "失敗", "could not start": "起動できず", "interrupted": "中断", "answer from the requester": "依頼者の返答", "note by the runtime": "本体の記録",
	"why it ended so": "理由", "what the worker was handed": "担当 (LLM) に渡したもの", "what the worker wrote": "担当がしたこと・書いたこと", "what the runtime observed": "本体が観察したこと",
	"written by": "書いた者", "the requester": "依頼者", "the runtime": "本体", "the worker": "担当", "the command": "コマンド", "the operator's settings": "運用者の設定",
	"your ticket text, as written at the tracker (shown above as the request)": "あなたが Backlog に書いた本文 (上の「依頼の原文」)", "the runtime's own words about this stage and the role": "本体が添えた説明 (工程と役)",
	"the record up to this launch, as shown on this page": "それまでの記録 (このページの前の起動)", "no output: the process could not start": "出力なし: 起動できず", "the worker's own stderr": "担当の stderr",
	"the process ended with an error": "プロセスが失敗で終わった",
	"no output":                       "出力なし", "what the requester wrote": "依頼者が書いたこと",
	"the runtime (how it ended), then the worker's stderr": "本体 (終了状態)、続く行は担当の stderr", "the worker, with the runtime's notes": "担当 (本体の注記を含む)",
	"the role's purpose and the operator's instructions": "役の説明と運用者の指示", "the record up to this launch, collapsed: identical failures as one entry, at most the latest sixty": "それまでの記録 (同じ失敗は 1 件に畳み、直近 60 件まで)",
	"Review findings kept by the review command": "レビューコマンドが残した指摘", "Report written in the workspace": "作業場所に書かれた報告",
	"Delivery receipt written by the delivery program": "納品プログラムが書いた receipt", "What each role's native agent logged in its own directory": "各役の agent が自分のディレクトリに残したもの",
	"home": "home", "files:": "ファイル:", "as the agent logged it:": "agent の記録では:", "model calls": "回のモデル呼び出し", "input tokens": "入力トークン", "output tokens": "出力トークン",
	"agent.log (tail)": "agent.log (末尾)", "errors.log (tail)": "errors.log (末尾)", "the whole conversation the agent had (messages, tool calls and their results)": "agent の会話の全記録 (メッセージ、ツール呼び出しと結果)",
	"Workspace changes": "作業場所の変更", "no change in the checkout": "checkout に変更なし", "recent commits in the checkout": "checkout の直近の commit", "diff of tracked files": "追跡ファイルの差分",
	"new file:": "新規ファイル:", "(symbolic link; not followed, the page stays inside the queue)": "(シンボリックリンク。たどらない。画面は queue の中だけを見せる)",
	"Name": "名前", "Size": "サイズ", "Modified": "更新", "files": "ファイル",
	"done": "完了", "waiting for the requester's reply": "依頼者の返事待ち", "between steps": "工程の切れ目", "no run record yet": "実行記録なし",
	"Done without a change": "変更なしで完了", "done without a change; nothing was delivered": "変更なしで完了 (何も納品していません)",
	"Done with the pull request open": "PR を開いて完了", "Done, the pull request closed unmerged": "PR が閉じられて終了",
	"done; the pull request was closed without being merged, so nothing was delivered": "完了 (PR はマージされずに閉じられたので、何も納品していません)", "done; the pull request is open, its merge left to a person": "完了 (PR は開いたまま。merge は人に任せています)",
	"no checkout yet": "checkout はまだない", "more under files": "件は files 配下", "Position": "工程の位置", "model": "モデル", "links": "リンク",
	"The process stopped while this action was pending. Available reports may be partial, and the action may have taken effect. Inspect the working tree and external state before repeating it.": "この工程の実行中に本体が止まりました。報告は途中までの可能性があり、操作は既に反映されているかもしれません。作業場所と外部の状態を確認してから繰り返します。", "more new files are not shown here; they are under files": "件の新規ファイルはここには出していない (files 配下にある)",
}

// The names of the stages of the shipped runs in Japanese are shared with the
// runtime, which says the same stage by the same name in its own comments on
// the tracker; a stage an operator named differently is shown as named.
func stageName(lang, name string) string {
	if lang == "ja" {
		if label, known := stagename.Japanese(name); known {
			return label
		}
	}
	if name == "done" {
		return "Done"
	}
	if name == "other" {
		return "Other"
	}
	return name
}

func translate(lang, text string) string {
	if lang == "ja" {
		if ja, known := japanese[text]; known {
			return ja
		}
	}
	return text
}

// localize renders the runtime's own status phrases, which name roles and
// stages, in the viewer's language.
func localize(lang, status string) string {
	if lang != "ja" {
		return status
	}
	prefix := ""
	if rest, found := strings.CutPrefix(status, "retrying after a failure; "); found {
		prefix, status = "失敗後の再試行中。", rest
	} else if rest, found := strings.CutPrefix(status, "taking up an interrupted step; "); found {
		prefix, status = "中断した工程の再開中。", rest
	}
	switch {
	case strings.HasPrefix(status, "running "):
		return prefix + "実行中: " + stageName(lang, strings.TrimPrefix(status, "running "))
	case strings.HasPrefix(status, "assigned to ") && strings.HasSuffix(status, ", no process output yet"):
		return prefix + "割り当て済み (出力はまだ): " + stageName(lang, strings.TrimSuffix(strings.TrimPrefix(status, "assigned to "), ", no process output yet"))
	case strings.HasPrefix(status, "step "):
		var i, n int
		var name string
		if _, err := fmt.Sscanf(status, "step %d of %d: %s", &i, &n, &name); err == nil {
			return prefix + fmt.Sprintf("工程 %d/%d: %s", i, n, stageName(lang, name))
		}
	}
	return prefix + translate(lang, status)
}

type fileEntry struct {
	Name     string
	Dir      bool
	Link     bool
	Href     string
	Size     int64
	Modified time.Time
}
