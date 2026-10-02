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

// A project shared with people's own tickets hands the runtime only what a
// requester marked for it. An issue that gains the category later is accepted
// then; the original text is saved unchanged.
func TestCategoryScopeAcceptsOnlyMarkedIssuesIncludingOnesMarkedLater(t *testing.T) {
	cfg := watchConfiguration(t)
	cfg.Intake.CategoryIDs = []int64{77, 78}
	marked := func(id int, text string, categories ...int) map[string]any {
		issue := watchedIssue(id, text, "2026-01-03T00:00:00Z")
		list := []any{}
		for _, category := range categories {
			list = append(list, map[string]any{"id": category})
		}
		issue["category"] = list
		return issue
	}
	later := make(chan struct{})
	useWatchTransport(t, func(r *http.Request) (*http.Response, error) {
		issues := []any{
			marked(20, "a person's own ticket"),
			marked(21, "another category only", 5),
			marked(22, "marked for the runtime", 5, 78),
		}
		select {
		case <-later:
			issues[0] = marked(20, "a person's own ticket", 77)
		default:
		}
		return selectionReply(r, 200, issues), nil
	})
	jobs := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	collected := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		since, _ := time.Parse(time.RFC3339, cfg.Intake.CreatedSince)
		collectIssues(ctx, cfg, jobs, since, 20*time.Millisecond, collected, func(string) {})
	}()
	saved := func(id int) bool {
		_, err := os.Stat(filepath.Join(jobs, strconv.Itoa(id), "issue.json"))
		return err == nil
	}
	select {
	case <-collected:
	case <-time.After(4 * time.Second):
		t.Fatal("the marked issue was not collected")
	}
	time.Sleep(80 * time.Millisecond) // several more ticks with the same answer
	if !saved(22) || saved(20) || saved(21) {
		t.Fatalf("accepted: 20=%t 21=%t 22=%t", saved(20), saved(21), saved(22))
	}
	close(later)
	deadline := time.Now().Add(4 * time.Second)
	for !saved(20) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if !saved(20) || saved(21) {
		t.Fatalf("after the category was added: 20=%t 21=%t", saved(20), saved(21))
	}
	raw, err := os.ReadFile(filepath.Join(jobs, "22", "issue.json"))
	var issue map[string]any
	if err != nil || json.Unmarshal(raw, &issue) != nil || issue["description"] != "marked for the runtime" || issue["unknown"] != true {
		t.Fatalf("original changed: %s, %v", raw, err)
	}
}

func TestCategoryScopeRejectsInvalidOperatorIDsBeforeIntake(t *testing.T) {
	for _, id := range []int64{0, -3} {
		cfg := watchConfiguration(t)
		cfg.Intake.CategoryIDs = []int64{id}
		if err := watchRequests(context.Background(), cfg, t.TempDir(), io.Discard); err == nil || err.Error() != "intake.category_ids must contain positive category ids" {
			t.Fatalf("invalid operator scope: %v", err)
		}
	}
}
