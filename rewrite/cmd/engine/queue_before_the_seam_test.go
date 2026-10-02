package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"ticket-runner/internal/chain"
)

// queueBeforeTheSeam is a queue written by the engine from before it asked
// its tracker through an interface, as that engine left it: request 61
// delivered after its requester answered a question, 62 waiting for an answer
// and 63 stopped by its requester. The files every launch writes again
// (engine.json, request.txt) and the request workspaces are left out.
const queueBeforeTheSeam = "testdata/queue-before-the-seam"

// beforeTheSeamConfiguration is the configuration that queue was written with.
func beforeTheSeamConfiguration(t *testing.T) config {
	t.Helper()
	cfg := questionConfiguration(t)
	cfg.Intake.MaxRunning = 3
	cfg.Intake.Announce = true
	cfg.Intake.Assign = true
	cfg.Intake.Statuses = &statusConfig{Processing: 1001, AwaitingRequester: 1002, Delivered: 3, Stopped: 1}
	cfg.Intake.CategoryOnAccept = 2001
	cfg.Intake.StatusPage = "https://board.example/jobs/"
	// The queue was written once, so a notice about a long silence would
	// depend on how long ago that was.
	silent := 0
	cfg.Intake.StallNoticeMinutes = &silent
	return cfg
}

// The engine that asks through the interface takes such a queue up where the
// earlier one left it: the waiting request goes on with its requester's
// answer, and the delivered and the stopped one are not touched at all, on
// the tracker or on disk.
func TestAQueueWrittenBeforeTheSeamCarriesOn(t *testing.T) {
	cfg := beforeTheSeamConfiguration(t)
	root := t.TempDir()
	if err := os.CopyFS(root, os.DirFS(queueBeforeTheSeam)); err != nil {
		t.Fatal(err)
	}
	original := map[string][]byte{}
	if err := filepath.WalkDir(queueBeforeTheSeam, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		original[strings.TrimPrefix(path, queueBeforeTheSeam+"/")] = data
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// The issue holds the comments the earlier engine posted on it, and now
	// the requester's answer to its question.
	var said noticeLog
	if err := json.Unmarshal(original["jobs/62/notices.json"], &said); err != nil {
		t.Fatal(err)
	}
	comments := []any{}
	answerID := int64(0)
	for _, notice := range said.Notices {
		comments = append(comments, map[string]any{"id": notice.CommentID, "issueId": 62, "projectId": 17, "createdUser": map[string]any{"id": 900}, "content": notice.Text})
		answerID = max(answerID, notice.CommentID+1)
	}
	comments = append(comments, map[string]any{"id": answerID, "issueId": 62, "projectId": 17, "createdUser": map[string]any{"id": 55}, "content": requesterAnswer})
	var mu sync.Mutex
	var asked []string
	posted := int64(9000)
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "watch-tracker.example" {
			var input struct{ State chain.State }
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				return nil, err
			}
			choice := "ask_requester"
			for _, result := range input.State.History {
				if result.Speaker == "requester" {
					choice = "done"
				}
			}
			return selectionReply(r, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": choice}}}), nil
		}
		mu.Lock()
		defer mu.Unlock()
		if err := r.ParseForm(); err != nil {
			return nil, err
		}
		switch {
		case r.URL.Path == "/api/v2/users/myself":
			return selectionReply(r, 200, map[string]any{"id": 900}), nil
		case r.URL.Path == "/api/v2/issues":
			return selectionReply(r, 200, []any{watchedIssue(61, "Original conditions", "2026-01-03T00:00:00Z"),
				watchedIssue(62, "Original conditions", "2026-01-03T00:00:00Z"), watchedIssue(63, "Original conditions", "2026-01-03T00:00:00Z")}), nil
		}
		asked = append(asked, strings.TrimSpace(r.Method+" "+r.URL.Path+" "+r.PostForm.Encode()))
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/issues/EXAMPLE-62/comments":
			return selectionReply(r, 200, comments), nil
		case r.Method == http.MethodPost:
			posted++
			return selectionReply(r, 201, map[string]any{"id": posted, "content": r.PostForm.Get("content")}), nil
		case r.Method == http.MethodPatch:
			reply := map[string]any{"id": 62}
			if id, err := strconv.Atoi(r.PostForm.Get("statusId")); err == nil {
				reply["status"] = map[string]any{"id": id}
			}
			if id, err := strconv.Atoi(r.PostForm.Get("assigneeId")); err == nil {
				reply["assignee"] = map[string]any{"id": id}
			}
			if hours := r.PostForm.Get("actualHours"); hours != "" {
				reply["actualHours"] = json.Number(hours)
			}
			return selectionReply(r, 200, reply), nil
		}
		return catalogReply(r, 404, "not part of this queue"), nil
	})
	log := &lockedLog{}
	t.Cleanup(func() {
		if t.Failed() {
			log.mu.Lock()
			defer log.mu.Unlock()
			t.Logf("queue log:\n%s", log.text.String())
		}
	})
	// The whole watch runs, so the queue's identity is checked as at a start.
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- watchRequests(ctx, cfg, root, log) }()
	var once sync.Once
	var ended error
	finish := func() error {
		once.Do(func() { cancel(); ended = <-result })
		return ended
	}
	t.Cleanup(func() { finish() })
	directory := filepath.Join(root, "jobs", "62")
	// The hours are the last of the turns taken on a delivered request.
	waitFor(t, func() bool {
		state, err := loadWatchState(root, 62)
		return err == nil && state.Done && loadTurns(directory).Hours
	})
	if err := finish(); !errors.Is(err, context.Canceled) {
		t.Fatalf("queue ended: %v", err)
	}
	log.mu.Lock()
	text := log.text.String()
	log.mu.Unlock()
	if strings.Contains(text, "different request") || strings.Contains(text, "could not be read") || strings.Count(text, "starting accepted request 62\n") != 1 ||
		strings.Contains(text, "starting accepted request 61") || strings.Contains(text, "starting accepted request 63") {
		t.Fatalf("the queue was not taken up as it was left:\n%s", text)
	}
	state, err := loadWatchState(root, 62)
	if err != nil || len(state.History) != 2 || state.History[1].Speaker != "requester" || state.History[1].Output != requesterAnswer {
		t.Fatalf("the answer did not reach the waiting request's history: %+v %v", state, err)
	}
	if _, err := os.Stat(filepath.Join(directory, fmt.Sprintf("answer-%d.json", answerID))); err != nil {
		t.Fatalf("the answered question was not kept: %v", err)
	}
	// Only the waiting request is spoken for: told its answer was taken,
	// moved and handed over while it works, and again once it is delivered.
	mu.Lock()
	defer mu.Unlock()
	var changes []string
	for _, request := range asked {
		if request != "GET /api/v2/issues/EXAMPLE-62/comments" {
			changes = append(changes, request)
		}
	}
	want := []string{
		"POST /api/v2/issues/EXAMPLE-62/comments " + url.Values{"content": {resumedNoticeText}}.Encode(),
		"PATCH /api/v2/issues/EXAMPLE-62 statusId=1001",
		"PATCH /api/v2/issues/EXAMPLE-62 assigneeId=900",
		"PATCH /api/v2/issues/EXAMPLE-62 statusId=3",
		"PATCH /api/v2/issues/EXAMPLE-62 assigneeId=55",
		"PATCH /api/v2/issues/EXAMPLE-62 actualHours=0.00",
	}
	if strings.Join(changes, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the tracker was asked:\n%s", strings.Join(asked, "\n"))
	}
	// Nothing of the delivered and the stopped request was written again.
	for path, data := range original {
		if strings.HasPrefix(path, "jobs/62/") {
			continue
		}
		now, err := os.ReadFile(filepath.Join(root, path))
		if err != nil || !bytes.Equal(now, data) {
			t.Errorf("%s changed: %s (%v)", path, now, err)
		}
	}
}
