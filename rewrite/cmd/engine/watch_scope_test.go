package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
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
// engine took up every issue of the project; written twice, the later one won
// without a word. Now the engine names the key and where it is, and starts
// nothing.
func TestConfigurationKeyTheEngineDoesNotTakeIsRefused(t *testing.T) {
	for _, tc := range []struct{ text, want string }{
		{`{"intake":{"project_id":17,"category_id":[77]}}`, `unknown key "category_id" in intake`},
		{`{"intake":{"project_id":17,"categories":[77]}}`, `unknown key "categories" in intake`},
		{`{"intake":{"project_id":17},"category_ids":[77]}`, `unknown key "category_ids" at the top level`},
		{`{"intake":{"project_id":17,"stop_user_id":[5]}}`, `unknown key "stop_user_id" in intake`},
		{`{"roles":[{"name":"a","processes":[{"name":"w"}]},{"name":"b","processes":[{"name":"w"},{"name":"x","tracker_acess":"read"}]}]}`, `unknown key "tracker_acess" in roles[1].processes[1]`},
		// The decoder matches a struct's keys without regard to letter case,
		// so this would be the same setting twice, the empty one last.
		{`{"intake":{"project_id":17,"category_ids":[77],"Category_IDs":[]}}`, `unknown key "Category_IDs" in intake`},
		{`{"intake":{"project_id":17,"category_ids":[77],"category_ids":[]}}`, `the key "category_ids" is written twice in intake; the later one would win without a word`},
		{`{"intake":{"project_id":17,"created_since":"2026-06-01T00:00:00Z","created_since":"2020-01-01T00:00:00Z"}}`, `the key "created_since" is written twice in intake`},
		{`{"intake":{"project_id":17,"category_ids":[77]},"intake":{"project_id":17}}`, `the key "intake" is written twice at the top level`},
		{`{"roles":[{"name":"a","processes":[{"name":"w","env":{"A":"1","A":"2"}}]}]}`, `the key "A" is written twice in roles[0].processes[0].env`},
		// A setting written flat, as the documents name it, is not that setting.
		{`{"intake.category_ids":[77]}`, `unknown key "intake.category_ids" at the top level`},
		{`{"intake":{"":1}}`, `unknown key "" in intake`},
		{`{"intake":{"project_id":17}} {"intake":{"project_id":18}}`, "text follows the configuration object"},
		// Deeper than a configuration is read: refused by the decoder's own
		// limit, without the walk beside it growing with the depth squared.
		{`{"roles":[{"name":"a","processes":[{"name":"w","env":{"A":` + strings.Repeat("[", 20000) + strings.Repeat("]", 20000) + `}}]}]}`, "exceeded max depth"},
		{`{"intake":{"project_id":"17"}}`, "cannot unmarshal string"},
		{`{"intake":[17]}`, "cannot unmarshal array"},
		{`{"intake":{"project_id":17}`, "unexpected EOF"},
	} {
		if _, err := readConfig([]byte(tc.text)); err == nil || !strings.HasPrefix(err.Error(), "reading the configuration: ") || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: %v", tc.text, err)
		}
	}
	// What the engine takes: its own names, each once; in a map, names that
	// differ only by letter case are different names.
	cfg, err := readConfig([]byte(`{"intake":{"project_id":17,"category_ids":[77,78],"stop_user_ids":[5]},"roles":[{"name":"a","processes":[{"name":"w","env":{"http_proxy":"x","HTTP_PROXY":"x"}}]}]}` + "\n"))
	if err != nil || len(cfg.Intake.CategoryIDs) != 2 || len(cfg.Intake.StopUserIDs) != 1 || len(cfg.Roles[0].Processes[0].Env) != 2 {
		t.Fatalf("known keys: %+v, %v", cfg.Intake, err)
	}
	// What the engine writes for each request, it reads back.
	written, err := json.Marshal(watchConfiguration(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readConfig(written); err != nil {
		t.Fatalf("the engine refused its own writing: %v", err)
	}
	directory := t.TempDir()
	path, root, logFile := filepath.Join(directory, "operator.json"), filepath.Join(directory, "must-not-exist"), filepath.Join(directory, "engine.log")
	if err := os.WriteFile(path, []byte(`{"intake":{"project_id":17,"created_since":"2026-01-02T00:00:00Z","category_id":[77]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	err = run(context.Background(), []string{"--config", path, "--watch", "--run-dir", root, "--log-file", logFile}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), `unknown key "category_id" in intake`) {
		t.Fatalf("the engine started on a misspelled filter: %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("a refused configuration wrote state")
	}
	// The status page shows the log file, so the reason is there too.
	if said, err := os.ReadFile(logFile); err != nil || string(said) != "the runtime stopped: reading the configuration: unknown key \"category_id\" in intake\n" {
		t.Fatalf("the log file says %q, %v", said, err)
	}
}

// The operator can have a configuration read and checked without starting
// anything: the same checks as a real start, the line that says which issues
// a watch would take up, and no queue, no log file and no request to any
// service. A configuration the engine would refuse at start is refused here.
func TestCheckReadsTheConfigurationAndStartsNothing(t *testing.T) {
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		t.Errorf("a check made a request: %s", r.URL)
		return nil, http.ErrNotSupported
	})
	cfg := watchConfiguration(t)
	cfg.Intake.CategoryIDs = []int64{77}
	directory := t.TempDir()
	write := func(cfg config) string {
		t.Helper()
		data, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(directory, "operator.json")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	var output strings.Builder
	if err := run(context.Background(), []string{"--config", write(cfg), "--check"}, &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	want := "intake: project 17, issues created at or after 2026-01-02T00:00:00Z; only issues carrying one of the categories [77]\nthe configuration is accepted; nothing was started\n"
	if output.String() != want {
		t.Fatalf("the check said:\n%s", output.String())
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 {
		t.Fatalf("a check wrote beside the configuration: %v, %v", entries, err)
	}
	if _, err := os.Stat("queue"); !os.IsNotExist(err) {
		t.Fatal("a check created a queue")
	}
	// The watch's own checks run too.
	cfg.Intake.CategoryIDs = []int64{0}
	output.Reset()
	if err := run(context.Background(), []string{"--config", write(cfg), "--check"}, &output, io.Discard); err == nil || err.Error() != "intake.category_ids must contain positive category ids" || output.Len() != 0 {
		t.Fatalf("an invalid watch passed the check: %v %q", err, output.String())
	}
	cfg.Intake.CategoryIDs, cfg.Intake.CreatedSince = nil, "yesterday"
	if err := run(context.Background(), []string{"--config", write(cfg), "--check"}, &output, io.Discard); err == nil || !strings.Contains(err.Error(), "intake.created_since") {
		t.Fatalf("an invalid starting time passed the check: %v", err)
	}
	// The check is of a watch's start, so a configuration the watch would
	// refuse for having no intake is refused here, not called accepted.
	cfg = watchConfiguration(t)
	cfg.Intake = nil
	output.Reset()
	if err := run(context.Background(), []string{"--config", write(cfg), "--check"}, &output, io.Discard); err == nil || err.Error() != "watch requires an explicit intake.project_id" || output.Len() != 0 {
		t.Fatalf("a configuration without intake passed the check: %v %q", err, output.String())
	}
	// It is a check, not a way to start: nothing else goes with it.
	for _, extra := range [][]string{{"--watch"}, {"--run-dir", filepath.Join(directory, "queue")}, {"--log-file", filepath.Join(directory, "engine.log")}, {"--request", "request.txt"}} {
		if err := run(context.Background(), append([]string{"--config", write(watchConfiguration(t)), "--check"}, extra...), io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "--check reads --config and starts nothing") {
			t.Fatalf("--check with %v: %v", extra, err)
		}
	}
	if entries, err := os.ReadDir(directory); err != nil || len(entries) != 1 {
		t.Fatalf("a refused check wrote beside the configuration: %v, %v", entries, err)
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

// The reader walks the configuration's text beside its types and takes a
// struct's keys by their names. The decoder also takes the keys of a struct
// embedded in another as the outer one's own; the walk does not, so an
// embedded struct would make a correct configuration be refused at start.
// There is none today; this fails when one is added, before an operator
// meets it.
func TestConfigurationTypesEmbedNoStruct(t *testing.T) {
	seen := map[reflect.Type]bool{}
	var walk func(reflect.Type, string)
	walk = func(kind reflect.Type, where string) {
		for kind.Kind() == reflect.Pointer || kind.Kind() == reflect.Slice || kind.Kind() == reflect.Array || kind.Kind() == reflect.Map {
			kind = kind.Elem()
		}
		if kind.Kind() != reflect.Struct || seen[kind] {
			return
		}
		seen[kind] = true
		for i := 0; i < kind.NumField(); i++ {
			field := kind.Field(i)
			name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
			// An embedded struct of a type that is not exported is itself not
			// exported, and the decoder still takes its keys: look for the
			// embedding before skipping what is not exported.
			if field.Anonymous && name != "-" {
				t.Errorf("%s embeds %s; give it a name of its own, or teach checkKeys to take an embedded struct's keys", where, field.Name)
			}
			if !field.IsExported() || name == "-" {
				continue
			}
			walk(field.Type, where+"."+field.Name)
		}
	}
	walk(reflect.TypeOf(config{}), "config")
	if len(seen) < 5 {
		t.Fatalf("the walk saw %d types; it no longer reaches the configuration", len(seen))
	}
}
