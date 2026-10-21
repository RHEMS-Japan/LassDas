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
	"strconv"
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
	// keep, when set, changes the words of a note as Redmine keeps them.
	keep func(string) string
}

// redmineFixtureAccounts are the accounts the fake's GET /users/<id>.json
// finds; Redmine's anonymous user (here 4) is not one, as Redmine answers it
// to a key that is not an administrator's.
var redmineFixtureAccounts = map[string]string{"55": "requester", "77": "operator", "88": "someone", "99": "engine-bot"}

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
	case r.Method == "GET" && strings.HasPrefix(path, "/users/"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/users/"), ".json")
		login, found := redmineFixtureAccounts[id]
		if !found {
			return catalogReply(r, 404, ""), nil
		}
		number, _ := strconv.Atoi(id)
		return selectionReply(r, 200, map[string]any{"user": map[string]any{"id": number, "login": login}}), nil
	case r.Method == "GET" && path == "/issues.json":
		f.reads++
		q := r.URL.Query()
		if q.Get("project_id") != "17" || q.Get("subproject_id") != "!*" || q.Get("status_id") != "open" || q.Get("sort") != "id" || q.Has("issue_id") {
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
					if f.keep != nil {
						value = f.keep(value.(string))
					}
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
	// Filed without an account: Redmine's anonymous user (4 here).
	anonymous := redmineIssueRow(15, 4)
	anonymous["author"] = map[string]any{"id": 4, "name": "Anonymous"}
	fake := newRedmineQueueFake(t, cfg, redmineIssueRow(11, 4), redmineIssueRow(12, 0), old, anonymous)
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
	for _, id := range []string{"12", "14", "15"} {
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

// Words someone else edited are the editor's; a note by an author that is not
// a person's account is nobody's; a list that Redmine gives out of id order
// is read in id order. Each decides whether a note is an answer or a stop.
// (Each case fails at b997c63: it credited edits to the first author, took
// the anonymous user's notes for the requester's, and refused the list.)
func TestRedmineAnswersAndStopsComeOnlyFromTheirAuthors(t *testing.T) {
	cfg := redmineConfiguration(t)
	edited := func(user, editor int64, text string) json.RawMessage {
		raw, _ := json.Marshal(map[string]any{"issue": map[string]any{"id": 51}, "id": 701, "user": map[string]any{"id": user},
			"updated_by": map[string]any{"id": editor}, "notes": text})
		return raw
	}
	issue := sourceIssue{ID: 51, Key: "51", Creator: tracker.Account{ID: 55}}
	for _, test := range []struct {
		name         string
		row          json.RawMessage
		stop, answer bool
	}{
		{"edited by a manager", edited(55, 66, "停止"), false, false},
		{"answer edited by a manager", edited(55, 66, requesterAnswer), false, false},
		{"edited by its author", edited(55, 55, "停止"), true, false},
		{"edited by an operator", edited(55, 77, "停止"), true, false},
	} {
		rows := []json.RawMessage{test.row}
		stopped, err := stopInstruction(cfg.source(), rows, issue, []int64{77})
		if err != nil || (stopped != nil) != test.stop {
			t.Errorf("%s: stop=%s err=%v", test.name, stopped, err)
		}
		id, _, err := answerToQuestion(cfg.source(), rows, issue, []int64{77}, 700)
		if err != nil || (id > 0) != test.answer {
			t.Errorf("%s: answer=%d err=%v", test.name, id, err)
		}
	}

	// Read through the tracker: an anonymous author's 停止 on an issue whose
	// author was anonymous, and the requester's 停止 listed before an older
	// note.
	fake := newRedmineQueueFake(t, cfg, redmineIssueRow(51, 0))
	useCatalogTransport(t, fake.roundTrip)
	fake.journals["51"] = []map[string]any{{"id": 702, "user": map[string]any{"id": 4}, "notes": "停止"}}
	rows, err := cfg.source().Comments(context.Background(), issue)
	if err != nil {
		t.Fatal(err)
	}
	anonymousIssue := sourceIssue{ID: 51, Key: "51", Creator: tracker.Account{ID: 4}}
	if stopped, err := stopInstruction(cfg.source(), rows, anonymousIssue, nil); err != nil || stopped != nil {
		t.Fatalf("the anonymous user's 停止 stopped an issue it filed: %s %v", stopped, err)
	}
	fake.journals["51"] = []map[string]any{
		{"id": 704, "user": map[string]any{"id": 55}, "notes": "停止", "created_on": "2026-01-03T00:00:00Z"},
		{"id": 703, "user": map[string]any{"id": 99}, "notes": "a question", "created_on": "2026-01-03T00:01:00Z"},
	}
	rows, err = cfg.source().Comments(context.Background(), issue)
	if err != nil {
		t.Fatalf("a list out of id order was refused: %v", err)
	}
	if stopped, err := stopInstruction(cfg.source(), rows, issue, nil); err != nil || stopped == nil {
		t.Fatalf("the requester's 停止 was not read: %v", err)
	}
}

// Redmine keeps a notice in other words than sent; the notice is the only new
// note by the engine's account, and it is posted once however many ticks pass.
// (Fails at b997c63, which found no note in the sent words and posted the
// notice again on every tick.)
func TestRedmineNoticeIsPostedOnceWhenRedmineKeepsOtherWords(t *testing.T) {
	for name, keep := range map[string]func(string) string{
		// A database that cannot hold four-byte characters keeps them as ?.
		"four-byte characters as ?": func(text string) string { return strings.ReplaceAll(text, "😀", "?") },
		// A plugin or a database that drops an invisible character.
		"word joiners dropped": func(text string) string { return strings.ReplaceAll(text, "\u2060", "") },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := redmineConfiguration(t)
			start := time.Now().UTC().Add(-3 * time.Hour)
			root, directory := noticeJob(t, chain.State{History: []chain.Result{
				{Role: "implement", Speaker: "worker", Output: "previous work", StartedAt: start, FinishedAt: start.Add(time.Minute)},
				{Role: "router", Speaker: "runtime", Error: "failed 😀 for @example", StartedAt: start.Add(time.Hour), FinishedAt: start.Add(time.Hour)},
			}})
			queueRanSince(t, root, cfg, start)
			fake := newRedmineQueueFake(t, cfg, redmineIssueRow(51, 0))
			fake.keep = keep
			useCatalogTransport(t, fake.roundTrip)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			n := requestNotices(cfg, sourceIssue{ID: 51, Key: "51"}, directory)
			if err := noteStall(ctx, cfg, n, directory, true, start); err != nil {
				t.Fatal(err)
			}
			for range 4 {
				if err := n.flush(ctx); err != nil {
					t.Fatal(err)
				}
			}
			records := readNotices(t, directory).Notices
			if len(fake.puts) != 1 || len(records) != 1 || records[0].CommentID != 900 || records[0].PostedAt == nil {
				t.Fatalf("notice posted %d times: %q %+v", len(fake.puts), fake.puts, records)
			}
		})
	}
}

// The word joiners go in before the cut: the detail stays within the bound
// the other trackers keep. (Fails at b997c63, which added them after the cut.)
func TestRedmineNoticeDetailStaysWithinItsBound(t *testing.T) {
	failure := strings.Repeat("@x ", 150)
	redmine := []rune(noticeDetail(redmineConfiguration(t), failure))
	backlog := []rune(noticeDetail(watchConfiguration(t), failure))
	if len(redmine) > len(backlog) {
		t.Fatalf("the Redmine detail runs to %d code points, past the bound the others keep (%d)", len(redmine), len(backlog))
	}
	if strings.Contains(string(redmine), "@x") {
		t.Fatal("an @ before a name was left as it was")
	}
}

// A pause notice Redmine kept in other words, landing with another new note
// by the engine's account, is taken as the newer of the two, and the other is
// held for the engine's own too: the pause has a positive receipt, is said
// once, and neither note is taken for the requester's answer even where the
// engine's account filed the issue. (Fails at 3adf54a, which recorded the
// notice with id 0, so the pause never had a receipt; adapted from the third
// review's probe.)
func TestRedmineHoldsEveryNoteItCannotTellApartForItsOwnNotice(t *testing.T) {
	cfg := redmineConfiguration(t)
	start := time.Now().UTC().Add(-3 * time.Hour)
	root, directory := noticeJob(t, chain.State{History: []chain.Result{
		{Role: "implement", Speaker: "worker", Output: "previous work", StartedAt: start, FinishedAt: start.Add(time.Minute)},
	}})
	queueRanSince(t, root, cfg, start)
	fake := newRedmineQueueFake(t, cfg, redmineIssueRow(51, 0))
	fake.keep = func(text string) string { return strings.ReplaceAll(text, "😀", "?") }
	late := int64(0)
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		if r.Method == "PUT" && late == 0 {
			late = fake.add("51", 99, "a role's late note") // lands in the same window
		}
		return fake.roundTrip(r)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	n := requestNotices(cfg, sourceIssue{ID: 51, Key: "51"}, directory)
	var ids []int64
	for range 3 {
		id, err := n.sayPauseEvent(ctx, workPauseNotice, "1", "paused 😀")
		if err != nil {
			t.Fatalf("the pause has no receipt: %v", err)
		}
		ids = append(ids, id)
	}
	// The notice log as written: the notice's id and the notes held with it.
	data, err := os.ReadFile(filepath.Join(directory, "notices.json"))
	var written struct {
		Notices []struct {
			CommentID int64   `json:"comment_id"`
			Also      []int64 `json:"also_comment_ids"`
		}
	}
	if err != nil || json.Unmarshal(data, &written) != nil || len(written.Notices) != 1 {
		t.Fatalf("the notice log: %s %v", data, err)
	}
	notice := written.Notices[0]
	if len(fake.puts) != 1 || ids[0] != 901 || ids[1] != 901 || ids[2] != 901 || notice.CommentID != 901 ||
		fmt.Sprint(notice.Also) != fmt.Sprint([]int64{late}) {
		t.Fatalf("puts=%d ids=%v notice=%+v (late note %d)", len(fake.puts), ids, notice, late)
	}
	rows, err := cfg.source().Comments(ctx, sourceIssue{ID: 51, Key: "51"})
	if err != nil {
		t.Fatal(err)
	}
	byEngine := sourceIssue{ID: 51, Key: "51", Creator: tracker.Account{ID: 99}}
	held := append([]int64{notice.CommentID}, notice.Also...)
	if id, answer, err := answerToQuestion(cfg.source(), rows, byEngine, nil, 0, held...); err != nil || id != 0 {
		t.Fatalf("the engine's own note was read as an answer: %d %q %v", id, answer, err)
	}
}

// The answer to the change that posts a notice is lost. Saved in its own
// words, the notice is found when the issue is read back and not posted
// again. Saved in other words, it cannot be told from another note, since
// nothing says Redmine saved anything: it is posted again once, and that post
// is confirmed. (The first case fails at 3adf54a, which read nothing back
// after a failed change; the second case is the fourth review's rule, which
// b84f97f broke by taking the only new note after a failed change.)
func TestRedmineNoticeWhoseAnswerWasLostIsFoundOnlyByItsWords(t *testing.T) {
	for name, keep := range map[string]func(string) string{
		"its own words":  nil,
		"in other words": func(text string) string { return strings.ReplaceAll(text, "😀", "?") },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := redmineConfiguration(t)
			start := time.Now().UTC().Add(-3 * time.Hour)
			root, directory := noticeJob(t, chain.State{History: []chain.Result{
				{Role: "implement", Speaker: "worker", Output: "previous work", StartedAt: start, FinishedAt: start.Add(time.Minute)},
				{Role: "router", Speaker: "runtime", Error: "failed 😀", StartedAt: start.Add(time.Hour), FinishedAt: start.Add(time.Hour)},
			}})
			queueRanSince(t, root, cfg, start)
			fake := newRedmineQueueFake(t, cfg, redmineIssueRow(51, 0))
			fake.keep = keep
			puts := 0
			useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
				response, err := fake.roundTrip(r)
				if r.Method == "PUT" {
					if puts++; puts == 1 {
						return catalogReply(r, 502, ""), nil // saved, but the answer is lost
					}
				}
				return response, err
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			n := requestNotices(cfg, sourceIssue{ID: 51, Key: "51"}, directory)
			noteStall(ctx, cfg, n, directory, true, start)
			for range 3 {
				n.flush(ctx)
			}
			records := readNotices(t, directory).Notices
			want := map[string][2]int{"its own words": {1, 900}, "in other words": {2, 901}}[name]
			if puts != want[0] || len(fake.journals["51"]) != want[0] || len(records) != 1 || records[0].CommentID != int64(want[1]) || records[0].PostedAt == nil {
				t.Fatalf("puts=%d journals=%d records=%+v", puts, len(fake.journals["51"]), records)
			}
		})
	}
}

// A private note is nobody's answer, stop or resume, whoever wrote it, and so
// it never reaches a role through the history either. (Fails at 3adf54a,
// which kept private notes from the role's own reads only.)
func TestRedmineIgnoresPrivateNotes(t *testing.T) {
	cfg := redmineConfiguration(t)
	fake := newRedmineQueueFake(t, cfg, redmineIssueRow(51, 0))
	useCatalogTransport(t, fake.roundTrip)
	issue := sourceIssue{ID: 51, Key: "51", Creator: tracker.Account{ID: 55}}
	for _, words := range []string{"停止", requesterAnswer, "再開"} {
		fake.journals["51"] = []map[string]any{{"id": 702, "user": map[string]any{"id": 55}, "notes": words, "private_notes": true}}
		rows, err := cfg.source().Comments(context.Background(), issue)
		if err != nil {
			t.Fatal(err)
		}
		if stopped, err := stopInstruction(cfg.source(), rows, issue, nil); err != nil || stopped != nil {
			t.Fatalf("a private %q stopped the request: %s %v", words, stopped, err)
		}
		if id, _, err := answerToQuestion(cfg.source(), rows, issue, nil, 700); err != nil || id != 0 {
			t.Fatalf("a private %q was taken for the answer: %d %v", words, id, err)
		}
		if raw, err := resumeInstruction(cfg.source(), rows, issue, nil, 700, nil); err != nil || raw != nil {
			t.Fatalf("a private %q resumed the request: %s %v", words, raw, err)
		}
	}
	// The same words in a public note count.
	fake.journals["51"] = []map[string]any{{"id": 703, "user": map[string]any{"id": 55}, "notes": "停止"}}
	rows, _ := cfg.source().Comments(context.Background(), issue)
	if stopped, err := stopInstruction(cfg.source(), rows, issue, nil); err != nil || stopped == nil {
		t.Fatalf("a public 停止 was not read: %v", err)
	}
}
