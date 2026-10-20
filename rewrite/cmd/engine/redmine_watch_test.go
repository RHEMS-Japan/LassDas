package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"ticket-runner/internal/chain"
	"ticket-runner/internal/tracker"
)

func redmineIssueRow(id int, category int64) map[string]any {
	row := map[string]any{"id": id, "project": map[string]any{"id": 17, "name": "Requests"}, "subject": "Original title",
		"description": "Original conditions 日本語\r\n", "created_on": "2026-01-03T00:00:00Z",
		"author": map[string]any{"id": 55, "name": "Requester"}, "status": map[string]any{"id": 1, "name": "New"}}
	if category > 0 {
		row["category"] = map[string]any{"id": category, "name": "automation"}
	}
	return row
}

// redmineJournalRow is a journal as the engine keeps it: Redmine's record,
// placed on its issue.
func redmineJournalRow(issue int, id, author int64, notes string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"issue": map[string]any{"id": issue}, "id": id, "user": map[string]any{"id": author, "name": "fixture user"},
		"notes": notes, "created_on": "2026-01-03T00:00:00Z", "private_notes": false, "details": []any{}})
	return raw
}

// redmineQueueFake answers as Redmine does for one project: a note or a
// change answers 204 without a body, and the account behind the key is 99.
type redmineQueueFake struct {
	t        *testing.T
	base     string
	mu       sync.Mutex
	issues   []map[string]any
	journals map[string][]map[string]any
	next     int64
	puts     []string
	reads    int
	// listed is called with each list query; nil lists every issue.
	listed func(query map[string][]string) []map[string]any
}

func newRedmineQueueFake(t *testing.T, cfg config, issues ...map[string]any) *redmineQueueFake {
	return &redmineQueueFake{t: t, base: cfg.Redmine.BaseURL, issues: issues, journals: map[string][]map[string]any{}, next: 899}
}

func (f *redmineQueueFake) add(issue string, author int64, notes string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	f.journals[issue] = append(f.journals[issue], map[string]any{"id": f.next, "user": map[string]any{"id": author, "name": "fixture user"},
		"notes": notes, "created_on": "2026-01-03T00:00:00Z", "private_notes": false, "details": []any{}})
	return f.next
}

func (f *redmineQueueFake) roundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("X-Redmine-API-Key") != "synthetic-watch-key" || r.URL.Query().Has("key") {
		f.t.Error("upstream did not receive the configured synthetic identity in its header")
	}
	path, found := strings.CutPrefix(r.URL.Path, strings.TrimPrefix(f.base, "https://redmine-tracker.example"))
	if !found {
		f.t.Errorf("a request outside the configured address: %s", r.URL)
		return catalogReply(r, 404, ""), nil
	}
	switch {
	case r.Method == "GET" && path == "/users/current.json":
		return selectionReply(r, 200, map[string]any{"user": map[string]any{"id": 99, "login": "engine-bot"}}), nil
	case r.Method == "GET" && path == "/issues.json":
		f.reads++
		q := r.URL.Query()
		if q.Get("project_id") != "17" || q.Get("subproject_id") != "!*" || q.Get("status_id") != "open" || q.Get("offset") != "0" {
			f.t.Errorf("discovery was not scoped to the project's own open issues: %s", r.URL)
		}
		rows := f.issues
		if f.listed != nil {
			rows = f.listed(q)
		}
		return selectionReply(r, 200, map[string]any{"issues": rows, "total_count": len(rows), "offset": 0, "limit": 100}), nil
	case strings.HasPrefix(path, "/issues/") && strings.HasSuffix(path, ".json"):
		key := strings.TrimSuffix(strings.TrimPrefix(path, "/issues/"), ".json")
		var issue map[string]any
		for _, row := range f.issues {
			if fmt.Sprint(row["id"]) == key {
				issue = row
			}
		}
		if issue == nil {
			return catalogReply(r, 404, ""), nil
		}
		if r.Method == "PUT" {
			var body struct{ Issue map[string]any }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				return nil, err
			}
			for name, value := range body.Issue {
				f.puts = append(f.puts, fmt.Sprintf("%s %s=%v", key, name, value))
				switch name {
				case "notes":
					f.next++
					f.journals[key] = append(f.journals[key], map[string]any{"id": f.next, "user": map[string]any{"id": 99, "name": "Engine"},
						"notes": value, "created_on": "2026-01-03T00:00:00Z", "private_notes": false, "details": []any{}})
				case "status_id":
					issue["status"] = map[string]any{"id": value}
				case "assigned_to_id":
					issue["assigned_to"] = map[string]any{"id": value}
				}
			}
			return catalogReply(r, 204, ""), nil
		}
		record := map[string]any{}
		for name, value := range issue {
			record[name] = value
		}
		if r.URL.Query().Get("include") == "journals" {
			record["journals"] = append([]map[string]any{}, f.journals[key]...)
		}
		return selectionReply(r, 200, map[string]any{"issue": record}), nil
	}
	f.t.Errorf("unexpected Redmine request: %s %s", r.Method, path)
	return catalogReply(r, 500, "unexpected operation"), nil
}

const redmineQuestion = "どちらにしますか？ (a) この範囲 (b) 別の範囲\r\n"

// This is a real child with the ordinary role-facing client. The upstream API
// and the router are local fixtures, not a claim about a real service/model.
func TestRedmineQuestionWorker(t *testing.T) {
	if os.Getenv("REDMINE_QUESTION_HELPER") != "1" {
		return
	}
	if os.Getenv("WATCH_TEST_KEY") != "" {
		t.Fatal("controller credential reached the role")
	}
	if os.Getenv("TASK_ISSUE") != "11" || os.Getenv("TASK_TRACKER_ISSUE") != "11" {
		t.Fatal("wrong assigned issue")
	}
	client, err := tracker.CertificateClient(os.Getenv("TASK_TRACKER_CERT"))
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	source := tracker.Backlog{BaseURL: os.Getenv("TASK_TRACKER_URL"), KeyEnv: "TASK_TRACKER_KEY", Client: client}
	ctx := context.Background()
	if original, err := source.Request(ctx, "11"); err != nil || !strings.Contains(original, "Original conditions 日本語") {
		t.Fatalf("%q %v", original, err)
	}
	receipt, err := source.AddComment(ctx, "11", redmineQuestion)
	if err != nil {
		t.Fatal(err)
	}
	var posted struct{ ID int64 }
	if err := json.Unmarshal(receipt, &posted); err != nil || posted.ID <= 0 {
		t.Fatalf("unreadable receipt: %s %v", receipt, err)
	}
	readback, err := source.Comment(ctx, "11", posted.ID)
	if err != nil || !bytes.Contains(readback, []byte(`"content"`)) {
		t.Fatalf("%s %v", readback, err)
	}
	fmt.Println("Posted the question and read it back.")
}

func TestRedmineWatchPassesOnlyMarkedIssuesThroughQuestionAnswerAndWork(t *testing.T) {
	cfg := redmineConfiguration(t)
	cfg.Intake.CategoryIDs = []int64{4}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Intake.QuestionRole = "ask"
	cfg.Roles = append(cfg.Roles, chain.Role{Name: "ask", Processes: []chain.Process{{Name: "questioner", TrackerAccess: "comment",
		Command: []string{binary, "-test.run=^TestRedmineQuestionWorker$"}, Env: map[string]string{"REDMINE_QUESTION_HELPER": "1"}}}})
	// Use the exact config decoder used on disk.
	cfg, err = readConfig(configurationJSON(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	old := redmineIssueRow(14, 4)
	old["created_on"] = "2026-01-01T00:00:00Z"
	fake := newRedmineQueueFake(t, cfg, redmineIssueRow(11, 4), redmineIssueRow(12, 0), old)
	closed := false
	fake.listed = func(q map[string][]string) []map[string]any {
		if fmt.Sprint(q["category_id"]) != "[4]" || fmt.Sprint(q["created_on"]) != "[>=2026-01-02T00:00:00Z]" {
			t.Errorf("discovery did not ask for the category and the start: %v", q)
		}
		if closed {
			return []map[string]any{}
		}
		// Redmine would narrow the list itself; the engine checks again.
		return fake.issues
	}
	root := t.TempDir()
	routes := 0
	transport := http.DefaultTransport
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Host {
		case "redmine-tracker.example":
			return fake.roundTrip(r)
		case "watch-model.example":
		default:
			return transport.RoundTrip(r)
		}
		var input struct{ State chain.State }
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			return nil, err
		}
		fake.mu.Lock()
		routes++
		fake.mu.Unlock()
		choice := "ask"
		for _, result := range input.State.History {
			if result.Speaker == "requester" {
				choice = "implement"
			}
			if result.Role == "implement" {
				choice = "done"
			}
		}
		return selectionReply(r, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": choice}}}), nil
	})
	finish := startStopQueue(t, cfg, root, 30*time.Millisecond, io.Discard)
	waitFor(t, func() bool {
		state, err := loadWatchState(root, 11)
		data, readErr := os.ReadFile(filepath.Join(root, "jobs", "11", "question.json"))
		var boundary questionBoundary
		return err == nil && state.Waiting && readErr == nil && json.Unmarshal(data, &boundary) == nil && boundary.After != nil && *boundary.After == 900
	})
	fake.mu.Lock()
	closed = true // Closing an issue is not an instruction to stop its work.
	reads := fake.reads
	fake.mu.Unlock()
	fake.add("11", 88, "another person's answer")
	waitFor(t, func() bool { fake.mu.Lock(); defer fake.mu.Unlock(); return fake.reads >= reads+2 })
	if state, err := loadWatchState(root, 11); err != nil || !state.Waiting || state.Done {
		t.Fatalf("unauthorized reply or close changed work: %+v %v", state, err)
	}
	fake.add("11", 55, requesterAnswer)
	waitFor(t, func() bool { state, err := loadWatchState(root, 11); return err == nil && state.Done })
	finish()
	state, err := loadWatchState(root, 11)
	if err != nil || len(state.History) != 3 || state.History[1].Output != requesterAnswer || state.History[1].Speaker != "requester" {
		t.Fatalf("lost reply: %+v %v", state, err)
	}
	for _, id := range []string{"12", "14"} {
		if _, err := os.Stat(filepath.Join(root, "jobs", id)); !os.IsNotExist(err) {
			t.Fatalf("out-of-scope issue %s admitted", id)
		}
	}
	bound, err := os.ReadFile(filepath.Join(root, "jobs", "11", "workspace", "actual-context.txt"))
	if err != nil || !strings.HasSuffix(string(bound), "\n11\n") {
		t.Fatalf("worker assignment: %q %v", bound, err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if !reflect.DeepEqual(fake.puts, []string{"11 notes=" + redmineQuestion}) || routes != 3 {
		t.Fatalf("puts=%q routes=%d", fake.puts, routes)
	}
}

func TestRedmineAppliesTheSameStopAndAnswerRules(t *testing.T) {
	cfg := redmineConfiguration(t)
	issue := sourceIssue{ID: 51, Key: "51", Creator: tracker.Account{ID: 55}}
	for _, test := range []struct {
		user         int64
		text         string
		stop, answer bool
	}{
		{55, "停止", true, false}, {77, "\r\n 停止 \r\nreason", true, false},
		{88, "停止", false, false}, {0, "停止", false, false},
		{55, "> 停止", false, true}, {55, "```\n停止\n```", false, true},
		{55, "Example:\n停止", false, true}, {55, "停止しないでください", false, true},
		{55, requesterAnswer, false, true}, {77, requesterAnswer, false, true},
		{88, requesterAnswer, false, false}, {55, " ", false, false}, {55, "", false, false},
	} {
		rows := []json.RawMessage{redmineJournalRow(51, 701, test.user, test.text)}
		stopped, err := stopInstruction(cfg.source(), rows, issue, []int64{77})
		if err != nil || (stopped != nil) != test.stop {
			t.Fatalf("%+v stop=%s err=%v", test, stopped, err)
		}
		id, answer, err := answerToQuestion(cfg.source(), rows, issue, []int64{77}, 700)
		if err != nil || (id > 0) != test.answer || test.answer && answer != test.text {
			t.Fatalf("%+v answer=%q err=%v", test, answer, err)
		}
		id, _, err = answerToQuestion(cfg.source(), rows, issue, []int64{77}, 701)
		if err != nil || id != 0 {
			t.Fatal("an earlier comment became an answer")
		}
	}
	// A journal kept from another issue is nobody's instruction here.
	rows := []json.RawMessage{redmineJournalRow(52, 701, 55, "停止")}
	if _, err := stopInstruction(cfg.source(), rows, issue, nil); err == nil {
		t.Fatal("another issue's journal was read as this issue's")
	}
	directory := t.TempDir()
	if err := writeRuntimeFile(filepath.Join(directory, "stop-request.json"), redmineJournalRow(51, 701, 55, "停止")); err != nil {
		t.Fatal(err)
	}
	if stopped, err := savedStop(cfg.source(), directory, issue, nil); err != nil || !stopped {
		t.Fatalf("a kept stop was not read again: %v %v", stopped, err)
	}
}

func TestRedmineStopIsAppliedOnTheFirstReadWhileWorkIsRunning(t *testing.T) {
	cfg := redmineConfiguration(t)
	cfg.Intake.Statuses = &statusConfig{Processing: 2, Stopped: 5}
	cfg.Roles[0].Processes[0].Command = []string{"/bin/sh", "-c", `cat > received.txt; printf '%s' "$$" > child-pid; exec sleep 60`}
	fake := newRedmineQueueFake(t, cfg, redmineIssueRow(11, 0))
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "watch-model.example" {
			return selectionReply(r, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": "implement"}}}), nil
		}
		return fake.roundTrip(r)
	})
	root := t.TempDir()
	finish := startStopQueue(t, cfg, root, 30*time.Millisecond, io.Discard)
	waitTestPID(t, filepath.Join(root, "jobs", "11", "workspace", "child-pid"))
	fake.add("11", 55, "停止")
	waitFor(t, func() bool {
		_, err := os.Stat(filepath.Join(root, "jobs", "11", "stop-request.json"))
		return err == nil
	})
	waitFor(t, func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return strings.Contains(strings.Join(fake.puts, "|"), "11 status_id=5")
	})
	finish()
	state, err := loadWatchState(root, 11)
	if err != nil || state.Done {
		t.Fatalf("stop claimed delivery: %+v %v", state, err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if !reflect.DeepEqual(fake.puts, []string{"11 status_id=2", "11 status_id=5"}) {
		t.Fatalf("changes: %q", fake.puts)
	}
}

func TestRedmineTurnsSetConfiguredStatusesAndAssigneesButNeverHours(t *testing.T) {
	cfg := redmineConfiguration(t)
	cfg.Intake.Assign = true
	cfg.Intake.Statuses = &statusConfig{Processing: 2, AwaitingRequester: 4, Delivered: 3, Stopped: 5}
	cfg.runtimeUser = tracker.Account{ID: 99, Login: "engine-bot"}
	fake := newRedmineQueueFake(t, cfg, redmineIssueRow(11, 4))
	useCatalogTransport(t, fake.roundTrip)
	raw, _ := json.Marshal(redmineIssueRow(11, 4))
	issue, err := cfg.source().ReadIssue(raw)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	ctx := context.Background()
	say := func(text string) { t.Errorf("turn failed: %s", text) }
	acceptTurn(ctx, cfg, issue, directory, 0, false, say)
	for _, stage := range []string{processingStatus, awaitingStatus, deliveredStatus, stoppedStatus} {
		applyStatus(ctx, cfg, issue, directory, stage, say)
	}
	assignTurn(ctx, cfg, issue, directory, "requester", say)
	now := time.Now()
	hoursTurn(ctx, cfg, issue, directory, now.Add(-time.Hour), now, say)
	if !loadTurns(directory).Hours {
		t.Fatal("no-hours turn was not recorded; it would repeat")
	}
	want := []string{"11 assigned_to_id=99", "11 status_id=2", "11 status_id=4", "11 status_id=3", "11 status_id=5", "11 assigned_to_id=55"}
	if !reflect.DeepEqual(fake.puts, want) {
		t.Fatalf("changes=%q, want %q (no category, no hours, no closure)", fake.puts, want)
	}
	if got := fmt.Sprint(fake.issues[0]["category"]); !strings.Contains(got, "id:4") {
		t.Fatalf("the requester's category was replaced: %s", got)
	}
}

func TestRedmineNoticeDetailNamesNobody(t *testing.T) {
	cfg := redmineConfiguration(t)
	for failure, want := range map[string]string{
		"failed for @example":                 "failed for @⁠example",
		"`@example` and @@two":                "`@⁠example` and @⁠@⁠two",
		"exit status 1\nlater\nby @example":   "exit status 1: by @⁠example",
		"@example synthetic-watch-key":        "@⁠example [credential]",
		" \n\t ":                              "",
		"npm ERR! 404 @types/node not found":  "npm ERR! 404 @⁠types/node not found",
		"mail someone@example.invalid failed": "mail someone@⁠example.invalid failed",
	} {
		if got := noticeDetail(cfg, failure); got != want {
			t.Errorf("%q became %q, want %q", failure, got, want)
		}
	}
	if got := noticeDetail(watchConfiguration(t), "@example"); got != "@example" {
		t.Fatalf("Backlog's existing presentation changed: %q", got)
	}
	if got := noticeDetail(githubConfiguration(t), "@example"); got != "` @example `" {
		t.Fatalf("GitHub's existing presentation changed: %q", got)
	}
}

// A notice is posted once and kept with the id of its note, which Redmine
// does not answer with: it is found by reading the journals again.
func TestRedmineStallNoticeIsPostedOnceAndKeptWithItsNotesID(t *testing.T) {
	cfg := redmineConfiguration(t)
	start := time.Now().UTC().Add(-3 * time.Hour)
	root, directory := noticeJob(t, chain.State{History: []chain.Result{
		{Role: "implement", Speaker: "worker", Output: "previous work", StartedAt: start, FinishedAt: start.Add(time.Minute)},
		{Role: "router", Speaker: "runtime", Error: "failed for @example", StartedAt: start.Add(time.Hour), FinishedAt: start.Add(time.Hour)},
	}})
	queueRanSince(t, root, cfg, start)
	issue := redmineIssueRow(51, 0)
	fake := newRedmineQueueFake(t, cfg, issue)
	fake.add("51", 55, "an earlier note")
	useCatalogTransport(t, fake.roundTrip)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	n := requestNotices(cfg, sourceIssue{ID: 51, Key: "51"}, directory)
	for range 2 {
		if err := noteStall(ctx, cfg, n, directory, true, start); err != nil {
			t.Fatal(err)
		}
	}
	if err := n.flush(ctx); err != nil {
		t.Fatal(err)
	}
	records := readNotices(t, directory).Notices
	if len(fake.puts) != 1 || len(records) != 1 || records[0].CommentID != 901 || "51 notes="+records[0].Text != fake.puts[0] {
		t.Fatalf("notice was changed, lost or repeated: puts=%q records=%+v", fake.puts, records)
	}
	if !strings.HasSuffix(records[0].Text, "（直近の失敗: failed for @⁠example）。") {
		t.Fatalf("the failure names someone: %q", records[0].Text)
	}
}
