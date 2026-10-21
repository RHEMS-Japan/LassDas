package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ticket-runner/internal/chain"
	"ticket-runner/internal/tracker"
)

// The engine against a real Redmine, end to end: intake by project, start
// and category, a question as a note, the requester's answer read as the
// answer, statuses and assignees at each turn, the report as a note, and the
// shipped report check finding it; and a stop. It runs only when pointed at a
// Redmine set up for it, as internal/tracker's live check describes:
// REDMINE_LIVE_URL and REDMINE_LIVE_KEYS. The models are the test's own.
type redmineLiveServer struct {
	t      *testing.T
	target *url.URL
	keys   map[string]json.RawMessage
}

func redmineLive(t *testing.T) (*redmineLiveServer, config) {
	t.Helper()
	address, keys := os.Getenv("REDMINE_LIVE_URL"), os.Getenv("REDMINE_LIVE_KEYS")
	if address == "" || keys == "" {
		t.Skip("set REDMINE_LIVE_URL and REDMINE_LIVE_KEYS to run the engine against a real Redmine")
	}
	target, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(keys)
	if err != nil {
		t.Fatal(err)
	}
	live := &redmineLiveServer{t: t, target: target}
	if err := json.Unmarshal(data, &live.keys); err != nil {
		t.Fatal(err)
	}
	cfg := watchConfiguration(t)
	t.Setenv("REDMINE_LIVE_ENGINE_KEY", live.account("engine-bot").Key)
	cfg.Backlog = tracker.Backlog{}
	cfg.Redmine = &tracker.Redmine{BaseURL: "https://redmine-live.test", KeyEnv: "REDMINE_LIVE_ENGINE_KEY"}
	var project struct{ ID int64 }
	json.Unmarshal(live.keys["project"], &project)
	cfg.Intake.ProjectID = project.ID
	// Redmine keeps whole seconds; the start is the second before this run.
	cfg.Intake.CreatedSince = time.Now().UTC().Truncate(time.Second).Add(-time.Second).Format(time.RFC3339)
	cfg.Intake.Statuses = &statusConfig{Processing: 2, AwaitingRequester: 4, Delivered: 3, Stopped: 5}
	cfg.Intake.Assign = true
	cfg.Intake.Announce = true
	return live, cfg
}

func (l *redmineLiveServer) account(name string) struct {
	ID  int64
	Key string
} {
	var entry struct {
		ID  int64
		Key string
	}
	json.Unmarshal(l.keys[name], &entry)
	return entry
}

func (l *redmineLiveServer) category(name string) int64 {
	var categories map[string]int64
	json.Unmarshal(l.keys["categories"], &categories)
	return categories[name]
}

// transport carries the engine's HTTPS requests for the Redmine to it, and
// answers the decisions with route.
func (l *redmineLiveServer) transport(t *testing.T, route func(chain.State) string) {
	network := http.DefaultTransport
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Host {
		case "redmine-live.test":
			moved := r.Clone(r.Context())
			moved.URL.Scheme, moved.URL.Host, moved.Host = l.target.Scheme, l.target.Host, l.target.Host
			return network.RoundTrip(moved)
		case "watch-model.example":
			var input struct{ State chain.State }
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				return nil, err
			}
			return selectionReply(r, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": route(input.State)}}}), nil
		}
		return nil, fmt.Errorf("only the Redmine and the test's models answer here, not %s", r.URL.Host)
	})
}

func (l *redmineLiveServer) as(account, method, path string, body any) []byte {
	l.t.Helper()
	var reader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reader = bytes.NewReader(data)
	}
	request, err := http.NewRequest(method, l.target.String()+path, reader)
	if err != nil {
		l.t.Fatal(err)
	}
	request.Header.Set("X-Redmine-API-Key", l.account(account).Key)
	request.Header.Set("Content-Type", "application/json")
	// A Redmine that stops answering fails the check rather than holding it.
	response, err := (&http.Client{Transport: &http.Transport{}, Timeout: 30 * time.Second}).Do(request)
	if err != nil {
		l.t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	if response.StatusCode >= 300 {
		l.t.Fatalf("%s %s answered %d: %.300s", method, path, response.StatusCode, data)
	}
	return data
}

func (l *redmineLiveServer) file(category int64, subject string) int64 {
	l.t.Helper()
	var project struct{ ID int64 }
	json.Unmarshal(l.keys["project"], &project)
	data := l.as("requester", http.MethodPost, "/issues.json", map[string]any{"issue": map[string]any{
		"project_id": project.ID, "category_id": category, "subject": subject, "description": "Live engine check\nkeep this line"}})
	var created struct{ Issue struct{ ID int64 } }
	json.Unmarshal(data, &created)
	return created.Issue.ID
}

// startAt opens the intake at the creation time of the given issue, by
// Redmine's own clock, which need not agree with this machine's.
func (l *redmineLiveServer) startAt(cfg *config, issue int64) {
	l.t.Helper()
	var record struct {
		Issue struct {
			Created string `json:"created_on"`
		}
	}
	if json.Unmarshal(l.as("admin", http.MethodGet, "/issues/"+strconv.FormatInt(issue, 10)+".json", nil), &record) != nil || record.Issue.Created == "" {
		l.t.Fatal("the issue's creation time could not be read")
	}
	cfg.Intake.CreatedSince = record.Issue.Created
}

type redmineLiveJournal struct {
	ID      int64
	User    struct{ ID int64 }
	Notes   string
	Details []struct {
		Name     string
		NewValue string `json:"new_value"`
	}
}

func (l *redmineLiveServer) journals(issue int64) (status, assignee int64, journals []redmineLiveJournal) {
	var record struct {
		Issue struct {
			Status     struct{ ID int64 }
			AssignedTo *struct{ ID int64 } `json:"assigned_to"`
			Journals   []redmineLiveJournal
		}
	}
	json.Unmarshal(l.as("admin", http.MethodGet, "/issues/"+strconv.FormatInt(issue, 10)+".json?include=journals", nil), &record)
	if record.Issue.AssignedTo != nil {
		assignee = record.Issue.AssignedTo.ID
	}
	return record.Issue.Status.ID, assignee, record.Issue.Journals
}

func (l *lockedLog) said() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.text.String()
}

func waitLive(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("waited 90 seconds for %s", what)
}

const (
	redmineLiveQuestion = "どちらにしますか？ (a) この範囲 (b) 別の範囲"
	redmineLiveReport   = "## 報告\n変更を納品しました。\n- 確かめたこと: 手元の試験"
)

// A role of the live check: the test binary, run as the engine launches a
// role, using the role-facing client through the engine's issue scope.
func TestRedmineLiveRole(t *testing.T) {
	role := os.Getenv("REDMINE_LIVE_ROLE")
	if role == "" {
		return
	}
	if os.Getenv("REDMINE_LIVE_ENGINE_KEY") != "" {
		t.Fatal("controller credential reached the role")
	}
	client, err := tracker.CertificateClient(os.Getenv("TASK_TRACKER_CERT"))
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	issue := os.Getenv("TASK_TRACKER_ISSUE")
	source := tracker.Backlog{BaseURL: os.Getenv("TASK_TRACKER_URL"), KeyEnv: "TASK_TRACKER_KEY", Client: client}
	ctx := context.Background()
	if original, err := source.Request(ctx, issue); err != nil || !strings.Contains(original, "Live engine check\r\nkeep this line") {
		t.Fatalf("%q %v", original, err)
	}
	text := redmineLiveQuestion
	if role == "report" {
		text = redmineLiveReport
		report := filepath.Join(os.Getenv("TASK_WORKSPACE"), "report")
		if err := os.MkdirAll(report, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(report, "result.md"), []byte(text+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		// A trial line first: the launch leaves only its last comment.
		if _, err := source.AddComment(ctx, issue, "trial line of the report"); err != nil {
			t.Fatal(err)
		}
	}
	receipt, err := source.AddComment(ctx, issue, text)
	if err != nil {
		t.Fatal(err)
	}
	var posted struct{ ID int64 }
	if err := json.Unmarshal(receipt, &posted); err != nil || posted.ID <= 0 {
		t.Fatalf("unreadable receipt: %s %v", receipt, err)
	}
	fmt.Printf("Posted the %s as note %d.\n", role, posted.ID)
}

// confirmReportScript is the shipped report check, from the operator
// scripts' template.
func confirmReportScript(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("../../../deploy/ticket-engine/operator-scripts-configmap.yaml.example")
	if err != nil {
		t.Fatal(err)
	}
	block := regexp.MustCompile(`(?m)^  confirm-report: \|\n((?:    [^\n]*(?:\n|$)|\n)+)`).FindSubmatch(data)
	if block == nil {
		t.Fatal("the shipped confirm-report script is missing")
	}
	var lines []string
	for _, line := range strings.Split(string(block[1]), "\n") {
		lines = append(lines, strings.TrimPrefix(line, "    "))
	}
	script := filepath.Join(t.TempDir(), "confirm-report")
	if err := os.WriteFile(script, []byte(strings.Join(lines, "\n")), 0700); err != nil {
		t.Fatal(err)
	}
	return script
}

func TestRedmineLiveEngineAsksWaitsWorksReportsAndConfirms(t *testing.T) {
	live, cfg := redmineLive(t)
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("the shipped report check needs python3")
	}
	tools := t.TempDir()
	build := exec.Command("go", "build", "-o", filepath.Join(tools, "ticket-tracker"), "../tracker")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the role-facing tracker command: %v %s", err, output)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Intake.CategoryIDs = []int64{live.category("engine")}
	cfg.Intake.QuestionRole = "ask"
	role := func(name, access string) chain.Role {
		return chain.Role{Name: name, Processes: []chain.Process{{Name: name + "-process", TrackerAccess: access,
			Command: []string{binary, "-test.run=^TestRedmineLiveRole$"}, Env: map[string]string{"REDMINE_LIVE_ROLE": name}}}}
	}
	cfg.Roles = append(cfg.Roles, role("ask", "comment"), role("report", "comment"), chain.Role{Name: "confirm_report",
		Processes: []chain.Process{{Name: "stored-comment-check", TrackerAccess: "read", Command: []string{python, "-B", confirmReportScript(t)},
			Env: map[string]string{"TRACKER_TOOL": filepath.Join(tools, "ticket-tracker")}}}})
	cfg, err = readConfig(configurationJSON(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var confirmed string
	live.transport(t, func(state chain.State) string {
		mu.Lock()
		defer mu.Unlock()
		answered := slices.ContainsFunc(state.History, func(r chain.Result) bool { return r.Speaker == "requester" })
		if !answered {
			return "ask"
		}
		last := state.History[len(state.History)-1]
		switch last.Role {
		case "implement":
			return "report"
		case "report":
			return "confirm_report"
		case "confirm_report":
			confirmed = last.Output + last.Error
			return "done"
		}
		return "implement"
	})
	other := live.file(live.category("other"), "Live engine other category")
	issue := live.file(live.category("engine"), "Live engine request")
	live.startAt(&cfg, other)
	root := t.TempDir()
	var log lockedLog
	finish := startStopQueue(t, cfg, root, 200*time.Millisecond, &log)
	defer func() {
		if t.Failed() {
			t.Logf("engine log:\n%s", log.said())
		}
	}()
	engine, requester := live.account("engine-bot").ID, live.account("requester").ID
	waitLive(t, "the question as a note and the issue handed to the requester", func() bool {
		status, assignee, journals := live.journals(issue)
		asked := slices.ContainsFunc(journals, func(j redmineLiveJournal) bool { return j.Notes == redmineLiveQuestion && j.User.ID == engine })
		return asked && status == 4 && assignee == requester
	})
	live.as("operator", http.MethodPut, "/issues/"+strconv.FormatInt(issue, 10)+".json", map[string]any{"issue": map[string]any{"notes": "an operator's remark, not an answer"}})
	live.as("requester", http.MethodPut, "/issues/"+strconv.FormatInt(issue, 10)+".json", map[string]any{"issue": map[string]any{"notes": requesterAnswer}})
	waitLive(t, "the request to finish", func() bool {
		state, err := loadWatchState(root, int(issue))
		return err == nil && state.Done
	})
	waitLive(t, "the delivered status and the hand-back to the requester", func() bool {
		status, assignee, _ := live.journals(issue)
		return status == 3 && assignee == requester
	})
	finish()
	state, _ := loadWatchState(root, int(issue))
	answers := slices.IndexFunc(state.History, func(r chain.Result) bool { return r.Speaker == "requester" })
	if answers < 0 || state.History[answers].Output != requesterAnswer {
		t.Fatalf("the requester's answer was not the one taken: %+v", state.History)
	}
	if !strings.Contains(confirmed, "a stored comment matches report/result.md") {
		t.Fatalf("the shipped report check did not find the report: %q", confirmed)
	}
	if _, err := os.Stat(filepath.Join(root, "jobs", strconv.FormatInt(other, 10))); !os.IsNotExist(err) {
		t.Fatal("an issue without the intake category was accepted")
	}
	_, _, journals := live.journals(issue)
	var statuses, assignees []string
	notes := map[string]int{}
	for _, journal := range journals {
		for _, detail := range journal.Details {
			switch detail.Name {
			case "status_id":
				statuses = append(statuses, detail.NewValue)
			case "assigned_to_id":
				assignees = append(assignees, detail.NewValue)
			}
		}
		if journal.User.ID == engine && journal.Notes != "" {
			notes[journal.Notes]++
		}
	}
	if !slices.Equal(statuses, []string{"2", "4", "2", "3"}) {
		t.Errorf("statuses set: %v", statuses)
	}
	want := []string{fmt.Sprint(engine), fmt.Sprint(requester), fmt.Sprint(engine), fmt.Sprint(requester)}
	if !slices.Equal(assignees, want) {
		t.Errorf("assignees set: %v, want %v", assignees, want)
	}
	if notes[redmineLiveQuestion] != 1 || notes[redmineLiveReport] != 1 || notes["trial line of the report"] != 0 {
		t.Errorf("the engine's notes: %q", notes)
	}
	records := readNotices(t, filepath.Join(root, "jobs", strconv.FormatInt(issue, 10))).Notices
	if len(records) == 0 {
		t.Fatal("no notice was posted")
	}
	for _, record := range records {
		if record.PostedAt == nil || record.CommentID <= 0 || notes[record.Text] != 1 {
			t.Errorf("a notice was not confirmed by its note's id, or posted twice: %+v (%d)", record, notes[record.Text])
		}
	}
	t.Logf("issue %d: %d notes by the engine, statuses %v, assignees %v, %d notices; check said %q",
		issue, len(notes), statuses, assignees, len(records), strings.TrimSpace(confirmed))
}

func TestRedmineLiveStopWhileWorkIsRunning(t *testing.T) {
	live, cfg := redmineLive(t)
	cfg.Intake.Announce = false
	cfg.Roles[0].Processes[0].Command = []string{"/bin/sh", "-c", `cat > received.txt; printf '%s' "$$" > child-pid; exec sleep 60`}
	live.transport(t, func(chain.State) string { return "implement" })
	cfg.Intake.CategoryIDs = []int64{live.category("engine")}
	issue := live.file(live.category("engine"), "Live engine stop")
	live.startAt(&cfg, issue)
	root := t.TempDir()
	var log lockedLog
	finish := startStopQueue(t, cfg, root, 200*time.Millisecond, &log)
	defer func() {
		if t.Failed() {
			t.Logf("engine log:\n%s", log.said())
		}
	}()
	waitLive(t, "the work to start", func() bool {
		data, _ := os.ReadFile(filepath.Join(root, "jobs", strconv.FormatInt(issue, 10), "workspace", "child-pid"))
		return len(data) > 0
	})
	live.as("requester", http.MethodPut, "/issues/"+strconv.FormatInt(issue, 10)+".json", map[string]any{"issue": map[string]any{"notes": "停止\n別の方法にします"}})
	waitLive(t, "the stop to be kept and the stopped status", func() bool {
		_, err := os.Stat(filepath.Join(root, "jobs", strconv.FormatInt(issue, 10), "stop-request.json"))
		status, assignee, _ := live.journals(issue)
		return err == nil && status == 5 && assignee == live.account("requester").ID
	})
	finish()
	if state, err := loadWatchState(root, int(issue)); err != nil || state.Done {
		t.Fatalf("stop claimed delivery: %+v %v", state, err)
	}
}
