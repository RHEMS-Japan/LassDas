package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTheStartIsAnnouncedOnlyToARequestThatWaitedForASlot(t *testing.T) {
	cfg := watchConfiguration(t)
	cfg.Intake.MaxRunning = 1
	cfg.Intake.Announce = true
	cfg.Intake.StopReportRole = ""
	cfg.Roles[0].Processes[0].Command = []string{"/bin/sh", "-c", `printf '%s' "$$" > child-pid; exec sleep 60`}
	root := t.TempDir()
	var stopped atomic.Int64
	var mu sync.Mutex
	var comments []string
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "watch-tracker.example" {
			return selectionReply(r, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": "implement"}}}), nil
		}
		key := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v2/issues/"), "/comments")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			if err := r.ParseForm(); err != nil {
				return nil, err
			}
			mu.Lock()
			comments = append(comments, key+": "+r.PostForm.Get("content"))
			n := len(comments)
			mu.Unlock()
			return selectionReply(r, 201, map[string]any{"id": 700 + n, "content": r.PostForm.Get("content")}), nil
		case strings.HasSuffix(r.URL.Path, "/comments"):
			if id := stopped.Load(); id > 0 && key == fmt.Sprintf("EXAMPLE-%d", id) {
				return selectionReply(r, 200, []json.RawMessage{stopComment(id, 55, "停止")}), nil
			}
			return selectionReply(r, 200, []any{}), nil
		case strings.Contains(r.URL.Path, "/comments/"):
			return selectionReply(r, 200, map[string]any{"id": 701, "content": "x"}), nil
		}
		return selectionReply(r, 200, []any{watchedIssue(51, "one", "2026-01-03T00:00:00Z"), watchedIssue(52, "two", "2026-01-03T00:00:00Z")}), nil
	})
	finish := startStopQueue(t, cfg, root, 30*time.Millisecond, io.Discard)
	started := func(id int) bool {
		_, err := os.Stat(filepath.Join(root, "jobs", fmt.Sprint(id), "workspace", "child-pid"))
		return err == nil
	}
	working := 0
	waitFor(t, func() bool {
		for _, id := range []int{51, 52} {
			if started(id) {
				working = id
				return true
			}
		}
		return false
	})
	waiting := 51 + 52 - working
	// The working request is stopped; the waiting one takes the slot.
	stopped.Store(int64(working))
	waitFor(t, func() bool { return started(waiting) })
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return strings.Contains(strings.Join(comments, "\n"), fmt.Sprintf("EXAMPLE-%d: %s", waiting, startedNoticeText))
	})
	time.Sleep(150 * time.Millisecond)
	finish()
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(comments, "\n")
	if strings.Contains(joined, fmt.Sprintf("EXAMPLE-%d: %s", working, startedNoticeText)) {
		t.Fatalf("a request that started at once heard that it started: %q", comments)
	}
	if strings.Count(joined, fmt.Sprintf("EXAMPLE-%d: %s", waiting, startedNoticeText)) != 1 || strings.Count(joined, "受け付けました。") != 2 {
		t.Fatalf("announcements: %q", comments)
	}
}
