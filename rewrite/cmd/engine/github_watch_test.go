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
	"sync/atomic"
	"testing"
	"time"

	"ticket-runner/internal/chain"
	"ticket-runner/internal/tracker"
)

func githubIssueRow(cfg config, number int, labels []string) map[string]any {
	return map[string]any{"number": number, "title": "Original title", "body": "Original conditions 日本語\r\n",
		"created_at": "2026-01-03T00:00:00Z", "repository_url": cfg.GitHub.APIURL + "/repos/example/project",
		"labels": labels, "user": map[string]any{"id": 55, "login": "requester"}}
}

func githubCommentRow(cfg config, number int, id, author int64, body string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"id": id, "body": body, "user": map[string]any{"id": author, "login": "fixture-user"},
		"issue_url": fmt.Sprintf("%s/repos/example/project/issues/%d", cfg.GitHub.APIURL, number)})
	return raw
}

const githubQuestion = "どちらにしますか？ (a) この範囲 (b) 別の範囲\r\n"

// This is a real child with the ordinary role-facing client. The upstream API
// and the router are local fixtures, not a claim about a real service/model.
func TestGitHubQuestionWorker(t *testing.T) {
	if os.Getenv("GITHUB_QUESTION_HELPER") != "1" {
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
	receipt, err := source.AddComment(ctx, "11", githubQuestion)
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

func TestGitHubWatchPassesOnlyLabelledIssuesThroughQuestionAnswerAndWork(t *testing.T) {
	cfg := githubConfiguration(t)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Intake.QuestionRole = "ask"
	cfg.Roles = append(cfg.Roles, chain.Role{Name: "ask", Processes: []chain.Process{{Name: "questioner", TrackerAccess: "comment",
		Command: []string{binary, "-test.run=^TestGitHubQuestionWorker$"}, Env: map[string]string{"GITHUB_QUESTION_HELPER": "1"}}}})
	// Use the exact config decoder used on disk, including the omission of
	// Backlog-only settings in the job's generated configuration.
	cfg, err = readConfig(configurationJSON(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	var mu sync.Mutex
	comments := []json.RawMessage{}
	closed, closedReads := false, 0
	posts, routes := 0, 0
	transport := http.DefaultTransport
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "github-tracker.example" && r.URL.Host != "watch-model.example" {
			return transport.RoundTrip(r)
		}
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Host == "watch-model.example" {
			var input struct{ State chain.State }
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				return nil, err
			}
			routes++
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
		}
		if r.Header.Get("Authorization") != "Bearer synthetic-watch-key" {
			t.Error("upstream did not receive the configured synthetic identity")
		}
		path := strings.TrimPrefix(r.URL.String(), cfg.GitHub.APIURL)
		switch {
		case r.Method == "GET" && strings.HasPrefix(path, "/repos/example/project/issues?"):
			if r.URL.Query().Get("labels") != "automation" || r.URL.Query().Get("state") != "open" {
				t.Error("discovery was not label/open scoped")
			}
			if closed {
				closedReads++
				return selectionReply(r, 200, []any{}), nil
			}
			pr := githubIssueRow(cfg, 13, []string{"automation"})
			pr["pull_request"] = map[string]any{}
			old := githubIssueRow(cfg, 14, []string{"automation"})
			old["created_at"] = "2026-01-01T00:00:00Z"
			return selectionReply(r, 200, []any{githubIssueRow(cfg, 11, []string{"automation"}), githubIssueRow(cfg, 12, nil), pr, old}), nil
		case r.Method == "GET" && path == "/repos/example/project/issues/11":
			return selectionReply(r, 200, githubIssueRow(cfg, 11, []string{"automation"})), nil
		case r.Method == "GET" && strings.HasPrefix(path, "/repos/example/project/issues/11/comments?"):
			return selectionReply(r, 200, comments), nil
		case r.Method == "POST" && path == "/repos/example/project/issues/11/comments":
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body) != 1 || body["body"] != githubQuestion {
				t.Errorf("changed question: %v %v", body, err)
			}
			posts++
			comments = append(comments, githubCommentRow(cfg, 11, 900, 99, body["body"]))
			return selectionReply(r, 201, json.RawMessage(comments[0])), nil
		case r.Method == "GET" && path == "/repos/example/project/issues/comments/900":
			return selectionReply(r, 200, json.RawMessage(comments[0])), nil
		default:
			t.Errorf("unexpected GitHub request: %s %s", r.Method, path)
			return catalogReply(r, 500, "unexpected operation"), nil
		}
	})
	finish := startStopQueue(t, cfg, root, 30*time.Millisecond, io.Discard)
	waitFor(t, func() bool {
		state, err := loadWatchState(root, 11)
		data, readErr := os.ReadFile(filepath.Join(root, "jobs", "11", "question.json"))
		var boundary questionBoundary
		return err == nil && state.Waiting && readErr == nil && json.Unmarshal(data, &boundary) == nil && boundary.After != nil && *boundary.After == 900
	})
	mu.Lock()
	closed = true // Closing an issue is not an instruction to stop its work.
	comments = append(comments, githubCommentRow(cfg, 11, 901, 88, "another person's answer"))
	mu.Unlock()
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return closedReads >= 2 })
	if state, err := loadWatchState(root, 11); err != nil || !state.Waiting || state.Done {
		t.Fatalf("unauthorized reply or close changed work: %+v %v", state, err)
	}
	mu.Lock()
	comments = append(comments, githubCommentRow(cfg, 11, 902, 55, requesterAnswer))
	mu.Unlock()
	waitFor(t, func() bool { state, err := loadWatchState(root, 11); return err == nil && state.Done })
	finish()
	state, err := loadWatchState(root, 11)
	if err != nil || len(state.History) != 3 || state.History[1].Output != requesterAnswer || state.History[1].Speaker != "requester" {
		t.Fatalf("lost reply: %+v %v", state, err)
	}
	for _, id := range []string{"12", "13", "14"} {
		if _, err := os.Stat(filepath.Join(root, "jobs", id)); !os.IsNotExist(err) {
			t.Fatalf("out-of-scope issue %s admitted", id)
		}
	}
	bound, err := os.ReadFile(filepath.Join(root, "jobs", "11", "workspace", "actual-context.txt"))
	if err != nil || !strings.HasSuffix(string(bound), "\n11\n") {
		t.Fatalf("worker assignment: %q %v", bound, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if posts != 1 || routes != 3 {
		t.Fatalf("posts=%d routes=%d", posts, routes)
	}
}

func TestBothTrackersApplyTheSameStopAndAnswerRules(t *testing.T) {
	for _, kind := range []string{"backlog", "github"} {
		t.Run(kind, func(t *testing.T) {
			cfg := watchConfiguration(t)
			issue := sourceIssue{ID: 51, Key: "EXAMPLE-51", Creator: tracker.Account{ID: 55}}
			comment := func(user int64, text string) json.RawMessage { return issueComment(701, user, text) }
			if kind == "github" {
				cfg = githubConfiguration(t)
				issue.Key = "51"
				comment = func(user int64, text string) json.RawMessage { return githubCommentRow(cfg, 51, 701, user, text) }
			}
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
				{88, requesterAnswer, false, false}, {55, " ", false, false},
			} {
				rows := []json.RawMessage{comment(test.user, test.text)}
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
		})
	}
}

func TestGitHubStopIsAppliedOnTheFirstReadWhileWorkIsRunning(t *testing.T) {
	cfg := githubConfiguration(t)
	cfg.Roles[0].Processes[0].Command = []string{"/bin/sh", "-c", `cat > received.txt; printf '%s' "$$" > child-pid; exec sleep 60`}
	var stop atomic.Bool
	var stopReads atomic.Int64
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "watch-model.example" {
			return selectionReply(r, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": "implement"}}}), nil
		}
		if strings.HasSuffix(r.URL.Path, "/comments") {
			rows := []json.RawMessage{}
			if stop.Load() {
				stopReads.Add(1)
				rows = append(rows, githubCommentRow(cfg, 11, 900, 55, "停止"))
			}
			return selectionReply(r, 200, rows), nil
		}
		return selectionReply(r, 200, []any{githubIssueRow(cfg, 11, []string{"automation"})}), nil
	})
	root := t.TempDir()
	finish := startStopQueue(t, cfg, root, 30*time.Millisecond, io.Discard)
	waitTestPID(t, filepath.Join(root, "jobs", "11", "workspace", "child-pid"))
	stop.Store(true)
	waitFor(t, func() bool {
		_, err := os.Stat(filepath.Join(root, "jobs", "11", "stop-request.json"))
		return err == nil
	})
	finish()
	if stopReads.Load() != 1 {
		t.Fatalf("stop took %d reads", stopReads.Load())
	}
	state, err := loadWatchState(root, 11)
	if err != nil || state.Done {
		t.Fatalf("stop claimed delivery: %+v %v", state, err)
	}
}

func TestGitHubTurnsSetConfiguredLabelsAndAssigneesButNeverHoursOrClosure(t *testing.T) {
	cfg := githubConfiguration(t)
	cfg.Intake.Assign = true
	cfg.GitHub.Labels = tracker.GitHubLabels{Accepted: "accepted", Processing: "working", AwaitingRequester: "waiting", Delivered: "delivered", Stopped: "stopped"}
	cfg.runtimeUser = tracker.Account{ID: 99, Login: "worker"}
	raw, _ := json.Marshal(githubIssueRow(cfg, 11, []string{"automation"}))
	issue, err := cfg.source().ReadIssue(raw)
	if err != nil {
		t.Fatal(err)
	}
	labels, assignees := []string{}, []string{}
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/user") {
			return selectionReply(r, 200, map[string]any{"id": 99, "login": "worker"}), nil
		}
		var body map[string][]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/labels") && len(body) == 1 && len(body["labels"]) == 1 {
			labels = append(labels, body["labels"][0])
			return selectionReply(r, 200, body["labels"]), nil
		}
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/assignees") && len(body) == 1 && len(body["assignees"]) == 1 {
			assignees = append(assignees, body["assignees"][0])
			return selectionReply(r, 201, map[string]any{"assignees": []any{map[string]any{"login": body["assignees"][0]}}}), nil
		}
		t.Errorf("unexpected operation (hours or closure must never be sent): %s %s %v", r.Method, r.URL, body)
		return catalogReply(r, 500, "unexpected operation"), nil
	})
	directory := t.TempDir()
	ctx := context.Background()
	say := func(text string) { t.Errorf("turn failed: %s", text) }
	acceptTurn(ctx, cfg, issue, directory, 0, say)
	for _, stage := range []string{processingStatus, awaitingStatus, deliveredStatus, stoppedStatus} {
		applyStatus(ctx, cfg, issue, directory, stage, say)
	}
	assignTurn(ctx, cfg, issue, directory, "requester", say)
	now := time.Now()
	hoursTurn(ctx, cfg, issue, directory, now.Add(-time.Hour), now, say)
	if !loadTurns(directory).Hours {
		t.Fatal("no-hours turn was not recorded; it would repeat")
	}
	if !reflect.DeepEqual(labels, []string{"accepted", "working", "waiting", "delivered", "stopped"}) || !reflect.DeepEqual(assignees, []string{"worker", "requester"}) {
		t.Fatalf("labels=%v assignees=%v", labels, assignees)
	}
}
