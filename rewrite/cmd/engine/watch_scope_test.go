package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestIssueAllowlistNarrowsDiscoveryWithoutChangingOriginal(t *testing.T) {
	cfg := watchConfiguration(t)
	cfg.Intake.IssueIDs = []int64{12, 10}
	useWatchTransport(t, func(r *http.Request) (*http.Response, error) {
		return selectionReply(r, 200, []any{
			watchedIssue(10, "allowlist does not bypass the time boundary", "2026-01-01T00:00:00Z"),
			watchedIssue(11, "unrelated current issue", "2026-01-03T00:00:00Z"),
			watchedIssue(12, "Original ordinary prose 日本語", "2026-01-03T00:00:00Z"),
		}), nil
	})
	jobs := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	collected := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		since, _ := time.Parse(time.RFC3339, cfg.Intake.CreatedSince)
		collectIssues(ctx, cfg, jobs, since, time.Hour, collected, func(string) {})
	}()
	select {
	case <-collected:
	case <-time.After(4 * time.Second):
		t.Fatal("scoped issue was not collected")
	}
	cancel()
	<-done
	for _, id := range []int{10, 11} {
		if _, err := os.Stat(filepath.Join(jobs, strconv.Itoa(id))); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("out-of-scope issue %d was saved: %v", id, err)
		}
	}
	raw, err := os.ReadFile(filepath.Join(jobs, "12", "issue.json"))
	var issue map[string]any
	if err != nil || json.Unmarshal(raw, &issue) != nil || issue["description"] != "Original ordinary prose 日本語" || issue["unknown"] != true {
		t.Fatalf("original changed: %s, %v", raw, err)
	}
}

func TestIssueAllowlistRejectsInvalidOperatorIDsBeforeIntake(t *testing.T) {
	for _, id := range []int64{0, -1} {
		cfg := watchConfiguration(t)
		cfg.Intake.IssueIDs = []int64{id}
		if err := watchRequests(context.Background(), cfg, t.TempDir(), io.Discard); err == nil || err.Error() != "intake.issue_ids must contain positive issue ids" {
			t.Fatalf("invalid operator scope: %v", err)
		}
	}
}
