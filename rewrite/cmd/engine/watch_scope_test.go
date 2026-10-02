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
	"strings"
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

// The allowlist and the categories are two conditions on the same issue, and
// neither lifts the starting time: an issue is accepted only when it meets
// every one the operator wrote. A category list that is missing or empty on an
// issue is no category.
func TestCategoryScopeAndAllowlistBothHold(t *testing.T) {
	cfg := watchConfiguration(t)
	cfg.Intake.IssueIDs = []int64{30, 31, 32, 33, 34}
	cfg.Intake.CategoryIDs = []int64{77}
	with := func(issue map[string]any, category any) map[string]any {
		issue["category"] = category
		return issue
	}
	marked := []any{map[string]any{"id": 77}}
	useWatchTransport(t, func(r *http.Request) (*http.Response, error) {
		return selectionReply(r, 200, []any{
			with(watchedIssue(30, "marked, but filed before the starting time", "2026-01-01T00:00:00Z"), marked),
			with(watchedIssue(31, "listed, another category", "2026-01-03T00:00:00Z"), []any{map[string]any{"id": 5}}),
			with(watchedIssue(32, "listed, category is null", "2026-01-03T00:00:00Z"), nil),
			watchedIssue(33, "listed, no category key", "2026-01-03T00:00:00Z"),
			with(watchedIssue(35, "marked, not listed", "2026-01-03T00:00:00Z"), marked),
			with(watchedIssue(34, "listed and marked", "2026-01-03T00:00:00Z"), marked),
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
		t.Fatal("the issue meeting both conditions was not collected")
	}
	cancel()
	<-done
	entries, err := os.ReadDir(jobs)
	if err != nil || len(entries) != 1 || entries[0].Name() != "34" {
		t.Fatalf("accepted: %v, %v", entries, err)
	}
}

// A setting spelled wrong is not a setting left out. Before this, a category
// filter written under a name the engine does not know was skipped, and the
// engine took up every issue of the project. Now it names the key and starts
// nothing.
func TestConfigurationKeyTheEngineDoesNotKnowIsRefused(t *testing.T) {
	for _, tc := range []struct{ text, want string }{
		{`{"intake":{"project_id":17,"category_id":[77]}}`, `unknown field "category_id"`},
		{`{"intake":{"project_id":17,"categories":[77]}}`, `unknown field "categories"`},
		{`{"intake":{"project_id":17},"category_ids":[77]}`, `unknown field "category_ids"`},
		{`{"intake":{"project_id":17,"stop_user_id":[5]}}`, `unknown field "stop_user_id"`},
		{`{"roles":[{"name":"implement","processes":[{"name":"worker","tracker_acess":"read"}]}]}`, `unknown field "tracker_acess"`},
		{`{"intake":{"project_id":17}} {"intake":{"project_id":18}}`, "text follows the configuration object"},
	} {
		if _, err := readConfig([]byte(tc.text)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: %v", tc.text, err)
		}
	}
	cfg, err := readConfig([]byte(`{"intake":{"project_id":17,"category_ids":[77,78],"stop_user_ids":[5]}}` + "\n"))
	if err != nil || len(cfg.Intake.CategoryIDs) != 2 || len(cfg.Intake.StopUserIDs) != 1 {
		t.Fatalf("known keys: %+v, %v", cfg.Intake, err)
	}
	directory := t.TempDir()
	path, root := filepath.Join(directory, "operator.json"), filepath.Join(directory, "must-not-exist")
	if err := os.WriteFile(path, []byte(`{"intake":{"project_id":17,"created_since":"2026-01-02T00:00:00Z","category_id":[77]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	err = run(context.Background(), []string{"--config", path, "--watch", "--run-dir", root}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), `unknown field "category_id"`) {
		t.Fatalf("the engine started on a misspelled filter: %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("a refused configuration wrote state")
	}
}

// The engine says once at start which new issues it takes up, so an operator
// reads whether a filter was understood before the first issue is accepted.
func TestIntakeScopeIsSaidAtStart(t *testing.T) {
	since, _ := time.Parse(time.RFC3339, "2026-01-02T00:00:00Z")
	base := "intake: project 17, issues created at or after 2026-01-02T00:00:00Z; "
	for _, tc := range []struct {
		ids, categories []int64
		want            string
	}{
		{nil, nil, base + "every such issue is accepted"},
		{nil, []int64{}, base + "every such issue is accepted"},
		{[]int64{12, 10}, nil, base + "only issue ids [12 10]"},
		{nil, []int64{77, 78}, base + "only issues carrying one of the categories [77 78]"},
		{[]int64{12}, []int64{77}, base + "only issue ids [12], and of those only the ones carrying one of the categories [77]"},
	} {
		cfg := watchConfiguration(t)
		cfg.Intake.IssueIDs, cfg.Intake.CategoryIDs = tc.ids, tc.categories
		if got := intakeScope(cfg, since); got != tc.want {
			t.Fatalf("scope:\n%s\nwant:\n%s", got, tc.want)
		}
	}
	cfg := watchConfiguration(t)
	cfg.Intake.CategoryIDs = []int64{77}
	useWatchTransport(t, func(r *http.Request) (*http.Response, error) {
		return selectionReply(r, 200, []any{}), nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := &lockedLog{}
	result := make(chan error, 1)
	go func() { result <- watchRequests(ctx, cfg, t.TempDir(), log) }()
	said := func() int {
		log.mu.Lock()
		defer log.mu.Unlock()
		return strings.Count(log.text.String(), base+"only issues carrying one of the categories [77]\n")
	}
	waitFor(t, func() bool { return said() == 1 })
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if said() != 1 {
		t.Fatalf("the scope was said %d times", said())
	}
}
