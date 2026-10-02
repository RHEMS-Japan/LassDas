package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ticket-runner/internal/chain"
	"ticket-runner/internal/tracker"
)

func TestTheIssueMovesOncePerTurnAndARefusedMoveIsAskedAgain(t *testing.T) {
	cfg := watchConfiguration(t)
	cfg.Intake.Statuses = &statusConfig{Processing: 1001, AwaitingRequester: 1002, Delivered: 3, Stopped: 1}
	var mu sync.Mutex
	var set []string
	refuse := true
	refusedCalls := 0
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPatch && r.URL.Host == "watch-tracker.example" {
			mu.Lock()
			defer mu.Unlock()
			if err := r.ParseForm(); err != nil {
				return nil, err
			}
			if refuse {
				refuse = false
				refusedCalls++
				return catalogReply(r, 503, "not now"), nil
			}
			id := r.PostForm.Get("statusId")
			set = append(set, strings.TrimPrefix(r.URL.Path, "/api/v2/issues/")+"="+id)
			n, _ := strconv.Atoi(id)
			return selectionReply(r, 200, map[string]any{"id": 51, "status": map[string]any{"id": n}}), nil
		}
		return nil, http.ErrNotSupported
	})
	directory := t.TempDir()
	issue := sourceIssue{ID: 51, Key: "EXAMPLE-51"}
	var log bytes.Buffer
	observe := func(message string) { log.WriteString(message + "\n") }
	now := time.Now()
	turnClock = func() time.Time { return now }
	defer func() { turnClock = time.Now }()
	// The first move is refused and left unrecorded; the next attempt, once
	// its minute has passed, makes it and records it; the same turn again
	// asks nothing.
	applyStatus(context.Background(), cfg, issue, directory, processingStatus, observe)
	var recorded statusRecord
	if raw, err := os.ReadFile(filepath.Join(directory, "status.json")); err != nil || json.Unmarshal(raw, &recorded) != nil || recorded.Kind != processingStatus || recorded.Refused != 1 || !strings.Contains(log.String(), "status not set to processing") {
		t.Fatalf("a refused move was recorded as made, or not counted, or not logged: %+v %v", recorded, err)
	}
	now = now.Add(2 * time.Minute)
	applyStatus(context.Background(), cfg, issue, directory, processingStatus, observe)
	applyStatus(context.Background(), cfg, issue, directory, processingStatus, observe)
	applyStatus(context.Background(), cfg, issue, directory, awaitingStatus, observe)
	applyStatus(context.Background(), cfg, issue, directory, processingStatus, observe)
	applyStatus(context.Background(), cfg, issue, directory, deliveredStatus, observe)
	applyStatus(context.Background(), cfg, issue, directory, deliveredStatus, observe)
	mu.Lock()
	got := strings.Join(set, " ")
	mu.Unlock()
	if got != "EXAMPLE-51=1001 EXAMPLE-51=1002 EXAMPLE-51=1001 EXAMPLE-51=3" {
		t.Fatalf("moves made: %s", got)
	}
	// A refused turn is asked again with growing spacing and never given up;
	// the same refusal is logged once, and nothing freezes the next turn.
	logged := strings.Count(log.String(), "status not set to stopped")
	asked := 0
	for i := 0; i < 7; i++ {
		refuse = true
		mu.Lock()
		beforeRefused := refusedCalls
		mu.Unlock()
		applyStatus(context.Background(), cfg, issue, directory, stoppedStatus, observe)
		mu.Lock()
		if refusedCalls > beforeRefused {
			asked++
		}
		mu.Unlock()
		now = now.Add(2 * time.Hour)
	}
	if asked != 7 {
		t.Fatalf("a refused move was given up: asked %d of 7 times", asked)
	}
	if strings.Count(log.String(), "status not set to stopped") != logged+1 {
		t.Fatalf("the same refusal was logged more than once or not at all:\n%s", log.String())
	}
	// Within the spacing the tracker is left alone.
	refuse = true
	mu.Lock()
	beforeRefused := refusedCalls
	mu.Unlock()
	applyStatus(context.Background(), cfg, issue, directory, stoppedStatus, observe)
	now = now.Add(time.Second)
	applyStatus(context.Background(), cfg, issue, directory, stoppedStatus, observe)
	mu.Lock()
	if refusedCalls != beforeRefused+1 {
		mu.Unlock()
		t.Fatalf("a refused move was asked again before its time: %d", refusedCalls-beforeRefused)
	}
	mu.Unlock()
	now = now.Add(2 * time.Hour)
	mu.Lock()
	before := len(set)
	mu.Unlock()
	refuse = false
	applyStatus(context.Background(), cfg, issue, directory, awaitingStatus, observe)
	mu.Lock()
	if len(set) != before+1 || set[len(set)-1] != "EXAMPLE-51=1002" {
		mu.Unlock()
		t.Fatalf("a refused turn froze the next turn: %v", set)
	}
	mu.Unlock()
	// A turn without a configured id changes nothing, as does no configuration.
	cfg.Intake.Statuses.Stopped = 0
	applyStatus(context.Background(), cfg, issue, directory, stoppedStatus, observe)
	cfg.Intake.Statuses = nil
	applyStatus(context.Background(), cfg, issue, directory, processingStatus, observe)
	mu.Lock()
	defer mu.Unlock()
	if len(set) != 5 {
		t.Fatalf("an unconfigured turn moved the issue: %v", set)
	}
}

func TestTheQueueMovesDeliveredAndStoppedRequestsOnItsTicks(t *testing.T) {
	cfg := watchConfiguration(t)
	cfg.Intake.Statuses = &statusConfig{Processing: 1001, Delivered: 3, Stopped: 1}
	cfg.Intake.StopReportRole = ""
	cfg.Intake.Assign = true
	cfg.Intake.CategoryOnAccept = 2001
	cfg.Intake.StatusPage = "https://board.example/jobs/"
	cfg.Intake.Announce = true
	var mu sync.Mutex
	set := map[string]string{}
	var comments []string
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "watch-tracker.example" {
			// The accepted request's own run may start; it is not under test.
			return catalogReply(r, 503, "not under test"), nil
		}
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			return selectionReply(r, 200, []any{}), nil
		case r.URL.Path == "/api/v2/users/myself":
			return selectionReply(r, 200, map[string]any{"id": 900}), nil
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			if err := r.ParseForm(); err != nil {
				return nil, err
			}
			comments = append(comments, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v2/issues/"), "/comments")+": "+r.PostForm.Get("content"))
			return selectionReply(r, 201, map[string]any{"id": 700 + len(comments), "content": r.PostForm.Get("content")}), nil
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/comments/"):
			return selectionReply(r, 200, map[string]any{"id": 701, "content": "x"}), nil
		case r.Method == http.MethodPatch:
			if err := r.ParseForm(); err != nil {
				return nil, err
			}
			key := strings.TrimPrefix(r.URL.Path, "/api/v2/issues/")
			set[key] += r.PostForm.Encode() + ";"
			reply := map[string]any{"id": 1}
			if v := r.PostForm.Get("statusId"); v != "" {
				n, _ := strconv.Atoi(v)
				reply["status"] = map[string]any{"id": n}
			}
			if a := r.PostForm.Get("assigneeId"); a != "" {
				n, _ := strconv.Atoi(a)
				reply["assignee"] = map[string]any{"id": n}
			}
			if c := r.PostForm["categoryId[]"]; len(c) > 0 {
				var cats []map[string]any
				for _, id := range c {
					n, _ := strconv.Atoi(id)
					cats = append(cats, map[string]any{"id": n})
				}
				reply["category"] = cats
			}
			return selectionReply(r, 200, reply), nil
		case strings.HasSuffix(r.URL.Path, "/issues"):
			return selectionReply(r, 200, []any{}), nil
		}
		return nil, http.ErrNotSupported
	})
	root := t.TempDir()
	for id, state := range map[string]chain.State{"60": {Done: true, History: []chain.Result{{Role: "confirm_report", Speaker: "confirm-process", FinishedAt: time.Now().Add(time.Minute)}}}, "61": {}} {
		dir := filepath.Join(root, "jobs", id)
		if err := os.MkdirAll(filepath.Join(dir, "run"), 0700); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(watchedIssue(map[string]int{"60": 60, "61": 61}[id], "Original conditions", "2026-01-03T00:00:00Z"))
		if err := os.WriteFile(filepath.Join(dir, "issue.json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
		request, err := tracker.RequestText(raw)
		if err != nil {
			t.Fatal(err)
		}
		state.Request = request
		saved, _ := json.Marshal(state)
		if err := os.WriteFile(filepath.Join(dir, "run", "history.json"), saved, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeRuntimeFile(filepath.Join(root, "jobs", "61", "stop-request.json"), stopComment(61, 55, "停止")); err != nil {
		t.Fatal(err)
	}
	// A third request is accepted but not started: it is announced with its
	// place in line and its page, put in the category, and handed to the
	// runtime. Its own run then starts; nothing here waits for it.
	dir := filepath.Join(root, "jobs", "62")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(watchedIssue(62, "Original conditions", "2026-01-03T00:00:00Z"))
	if err := os.WriteFile(filepath.Join(dir, "issue.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	cfg.Roles[0].Processes[0].Command = []string{"/bin/sh", "-c", "exec sleep 60"}
	// The third request was accepted while the queue's engines announced
	// acceptances; this run is not their first.
	queueRanSince(t, root, cfg, time.Now().Add(-time.Hour))
	var queueLog bytes.Buffer
	finish := startStopQueue(t, cfg, root, 20*time.Millisecond, &queueLog)
	defer func() {
		if t.Failed() {
			mu.Lock()
			defer mu.Unlock()
			t.Logf("patches: %v\ncomments: %q\nlog: %s", set, comments, queueLog.String())
		}
	}()
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return strings.Contains(set["EXAMPLE-60"], "statusId=3") && strings.Contains(set["EXAMPLE-61"], "statusId=1") && len(comments) >= 1 && strings.Contains(set["EXAMPLE-62"], "categoryId")
	})
	time.Sleep(150 * time.Millisecond)
	finish()
	mu.Lock()
	defer mu.Unlock()
	if strings.Count(set["EXAMPLE-60"], "statusId=3") != 1 || !strings.Contains(set["EXAMPLE-60"], "assigneeId=55") || !strings.Contains(set["EXAMPLE-60"], "actualHours=") {
		t.Fatalf("the delivered request was not handed to the requester once with its hours: %q", set["EXAMPLE-60"])
	}
	if strings.Count(set["EXAMPLE-61"], "statusId=1") != 1 || !strings.Contains(set["EXAMPLE-61"], "assigneeId=55") {
		t.Fatalf("the stopped request was not handed to the requester once: %q", set["EXAMPLE-61"])
	}
	if strings.Count(set["EXAMPLE-62"], "categoryId%5B%5D=2001") != 1 || !strings.Contains(set["EXAMPLE-62"], "assigneeId=900") || !strings.Contains(set["EXAMPLE-62"], "statusId=1001") {
		t.Fatalf("the accepted request was not categorised, handed to the runtime and moved once: %q", set["EXAMPLE-62"])
	}
	joined := strings.Join(comments, "\n")
	if strings.Count(joined, "EXAMPLE-62: 受け付けました。すぐに自動処理を始めます。\n進み具合はこちらで見られます: https://board.example/jobs/62") != 1 || strings.Contains(joined, "EXAMPLE-60:") || strings.Contains(joined, "EXAMPLE-61:") {
		t.Fatalf("acceptance was announced wrongly: %q", comments)
	}
}

func TestTheLineAheadCountsUnfinishedEarlierRequestsByNumber(t *testing.T) {
	root := t.TempDir()
	jobs := filepath.Join(root, "jobs")
	for _, id := range []string{"9", "51", "999", "1000", "1001"} {
		if err := os.MkdirAll(filepath.Join(jobs, id, "run"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	done, _ := json.Marshal(chain.State{Done: true})
	os.WriteFile(filepath.Join(jobs, "9", "run", "history.json"), done, 0600)
	os.WriteFile(filepath.Join(jobs, "999", "stop-request.json"), []byte(`{"id":1}`), 0600)
	entries, err := os.ReadDir(jobs)
	if err != nil {
		t.Fatal(err)
	}
	// Before 1001: 9 is delivered, 999 is stopped, 51 and 1000 wait.
	if got := unfinishedBefore(jobs, entries, 1001); got != 2 {
		t.Fatalf("ahead of 1001: %d, want 2", got)
	}
	if got := unfinishedBefore(jobs, entries, 51); got != 0 {
		t.Fatalf("ahead of 51: %d, want 0", got)
	}
	if got := unfinishedBefore(jobs, entries, 1000); got != 1 {
		t.Fatalf("ahead of 1000: %d, want 1", got)
	}
}
