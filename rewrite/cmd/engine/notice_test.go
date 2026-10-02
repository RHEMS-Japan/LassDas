package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"ticket-runner/internal/chain"
	"ticket-runner/internal/tracker"
)

// noticeTracker answers the tracker calls the controller makes for its own
// fixed comments. Posted text is kept exactly as submitted, so a test sees both
// the wording and any repetition.
type noticeTracker struct {
	mu       sync.Mutex
	posted   []string
	rows     []json.RawMessage
	postFail int
	// silent stores the comment and then drops the answer: the post is visible
	// at the issue but the caller was never told so.
	silent bool
	posts  int
	// reads counts every read of the tracker, comments or otherwise.
	reads int
}

func (n *noticeTracker) install(t *testing.T, extra roundTripFunc) {
	t.Helper()
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "watch-tracker.example" {
			return extra(r)
		}
		if r.Method == http.MethodGet {
			n.mu.Lock()
			n.reads++
			n.mu.Unlock()
		}
		if !strings.HasSuffix(r.URL.Path, "/comments") {
			return selectionReply(r, 200, []any{}), nil
		}
		n.mu.Lock()
		defer n.mu.Unlock()
		if r.Method == http.MethodPost {
			if err := r.ParseForm(); err != nil {
				return nil, err
			}
			n.posts++
			content := r.Form.Get("content")
			if n.postFail > 0 {
				n.postFail--
				return catalogReply(r, 500, "tracker unavailable"), nil
			}
			n.posted = append(n.posted, content)
			row := map[string]any{"id": 900 + len(n.rows), "issueId": 51, "projectId": 17,
				"content": content, "createdUser": map[string]any{"id": 99}}
			raw, _ := json.Marshal(row)
			n.rows = append(n.rows, raw)
			if n.silent {
				return catalogReply(r, 500, "tracker unavailable"), nil
			}
			return selectionReply(r, 201, row), nil
		}
		return selectionReply(r, 200, append([]json.RawMessage{}, n.rows...)), nil
	})
}

func (n *noticeTracker) count(text string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	total := 0
	for _, posted := range n.posted {
		if posted == text {
			total++
		}
	}
	return total
}

func (n *noticeTracker) withPrefix(prefix string) []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	var found []string
	for _, posted := range n.posted {
		if strings.HasPrefix(posted, prefix) {
			found = append(found, posted)
		}
	}
	return found
}

func (n *noticeTracker) all() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.posted...)
}

func (n *noticeTracker) attempts() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.posts
}

func (n *noticeTracker) readings() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.reads
}

const noticeRequest = "Original issue: EXAMPLE-51\nTitle: Original title\n\nOriginal conditions"

// noticeJob lays down one already accepted request with the history a test
// needs, and returns the queue root plus that request's directory.
func noticeJob(t *testing.T, state chain.State) (string, string) {
	t.Helper()
	root := t.TempDir()
	directory := filepath.Join(root, "jobs", "51")
	if err := os.MkdirAll(filepath.Join(directory, "run"), 0700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(watchedIssue(51, "Original conditions", "2026-01-03T00:00:00Z"))
	if err := writeRuntimeFile(filepath.Join(directory, "issue.json"), raw); err != nil {
		t.Fatal(err)
	}
	writeJobHistory(t, directory, state)
	return root, directory
}

func writeJobHistory(t *testing.T, directory string, state chain.State) {
	t.Helper()
	state.Request = noticeRequest
	data, _ := json.Marshal(state)
	if err := writeRuntimeFile(filepath.Join(directory, "run", "history.json"), data); err != nil {
		t.Fatal(err)
	}
}

func loadJobState(t *testing.T, directory string) chain.State {
	t.Helper()
	state, err := savedHistory(directory)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func readNotices(t *testing.T, directory string) noticeLog {
	t.Helper()
	var log noticeLog
	raw, err := os.ReadFile(filepath.Join(directory, "notices.json"))
	if err != nil {
		return log
	}
	if err := json.Unmarshal(raw, &log); err != nil {
		t.Fatalf("unreadable notice record: %s", raw)
	}
	return log
}

// queueRanSince leaves the queue's record of notice kinds as an engine with
// this configuration would have left it, had it run on the queue since then.
// A test that lays its events down before its queue starts is about what such
// an engine says of them, not about an engine meeting the queue for the first
// time.
func queueRanSince(t *testing.T, root string, cfg config, since time.Time) {
	t.Helper()
	if err := startNoticeKinds(root, cfg, since, func(string) {}); err != nil {
		t.Fatal(err)
	}
}

// deliveredJob lays down a request delivered at the given time whose one
// launch used a model, so the delivered turns have a list to post for it.
func deliveredJob(t *testing.T, root string, id int, delivered time.Time) string {
	t.Helper()
	directory := filepath.Join(root, "jobs", fmt.Sprint(id))
	if err := os.MkdirAll(filepath.Join(directory, "run"), 0700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(watchedIssue(id, "Original conditions", "2026-01-03T00:00:00Z"))
	if err := writeRuntimeFile(filepath.Join(directory, "issue.json"), raw); err != nil {
		t.Fatal(err)
	}
	request, err := tracker.RequestText(raw)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(chain.State{Request: request, Done: true, History: []chain.Result{
		{Role: "implement", Speaker: "worker", Model: "maker/one", StartedAt: delivered.Add(-32 * time.Second), FinishedAt: delivered},
	}})
	if err := writeRuntimeFile(filepath.Join(directory, "run", "history.json"), data); err != nil {
		t.Fatal(err)
	}
	return directory
}

// deliveredList is the list deliveredJob's request is told.
const deliveredList = "使ったモデル (工程ごと、起動順):\n- 実装: maker/one (32 秒)"

// creditServer answers the model key endpoint the way the provider does. It is
// a local fixture; no provider is called.
func creditServer(t *testing.T, body func() string) *httptest.Server {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-watch-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		text := body()
		if text == "" {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"upstream unavailable"}`))
			return
		}
		_, _ = w.Write([]byte(text))
	}))
	t.Cleanup(server.Close)
	return server
}

func holdingWorker(cfg *config) {
	cfg.Roles[0].Processes[0].Command = []string{"/bin/sh", "-c",
		`cat > received.txt; printf '%s' "$$" > child-pid; exec sleep 60`}
}

func alwaysChoose(role string) roundTripFunc {
	return func(r *http.Request) (*http.Response, error) {
		return selectionReply(r, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": role}}}), nil
	}
}

// A restart in the middle of the night leaves an interrupted action behind.
// The requester is told once that the same request is being carried on, and a
// crash loop cannot turn that into a column of identical comments.
func TestRestartNoticeIsPostedOncePerRestartAndNotForACleanStart(t *testing.T) {
	cfg := watchConfiguration(t)
	root, directory := noticeJob(t, chain.State{Pending: &chain.Assignment{Role: "implement"}, History: []chain.Result{}})
	fixture := &noticeTracker{}
	fixture.install(t, alwaysChoose("done"))
	since, _ := time.Parse(time.RFC3339, cfg.Intake.CreatedSince)
	restart := func() {
		ctx, cancel := context.WithCancel(context.Background())
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			_ = pollRequests(ctx, cfg, filepath.Join(root, "jobs"), since, 10*time.Millisecond, 1, &serialLog{writer: io.Discard})
		}()
		waitFor(t, func() bool { return loadJobState(t, directory).Done })
		cancel()
		<-finished
	}
	restart()
	if got := fixture.count(resumeNoticeText); got != 1 {
		t.Fatalf("the first restart posted the notice %d times: %v", got, fixture.all())
	}
	// A second process meets the same unfinished history minutes later.
	writeJobHistory(t, directory, chain.State{Pending: &chain.Assignment{Role: "implement"}, History: []chain.Result{}})
	restart()
	if got := fixture.count(resumeNoticeText); got != 1 {
		t.Fatalf("two restarts inside thirty minutes posted the notice %d times: %v", got, fixture.all())
	}
	log := readNotices(t, directory)
	if len(log.Notices) != 1 || log.Notices[0].Kind != resumeNotice || log.Notices[0].PostedAt == nil {
		t.Fatalf("the record does not show one confirmed restart notice: %+v", log.Notices)
	}
	// A clean history is not a restart and says nothing at all.
	cleanRoot, cleanDirectory := noticeJob(t, chain.State{History: []chain.Result{}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pollRequests(ctx, cfg, filepath.Join(cleanRoot, "jobs"), since, 10*time.Millisecond, 1, &serialLog{writer: io.Discard})
	waitFor(t, func() bool { return loadJobState(t, cleanDirectory).Done })
	if notices := readNotices(t, cleanDirectory).Notices; len(notices) != 0 {
		t.Fatalf("a clean start told the requester about a restart: %+v", notices)
	}
}

// Past its window the same condition may speak again, so a request that is
// still being restarted hours later is not left silent forever.
func TestRestartNoticeSpeaksAgainOnlyAfterItsWindow(t *testing.T) {
	now := time.Now().UTC()
	recent := now.Add(-29 * time.Minute)
	log := noticeLog{Notices: []noticeRecord{{Kind: resumeNotice, Text: resumeNoticeText, WrittenAt: recent, PostedAt: &recent}}}
	if noticeDue(log, resumeNotice, now) {
		t.Fatal("the restart notice repeated inside its window")
	}
	older := now.Add(-31 * time.Minute)
	log.Notices[0].WrittenAt, log.Notices[0].PostedAt = older, &older
	if !noticeDue(log, resumeNotice, now) {
		t.Fatal("the restart notice stayed silent past its window")
	}
}

// The one failure no role can recover from is the shared key's budget. Nothing
// new starts while it is short, the requester is told once, and the work
// resumes by itself with one more comment.
func TestBudgetBelowMinimumHoldsWorkAndResumesByItself(t *testing.T) {
	cfg := watchConfiguration(t)
	cfg.Intake.MinModelCredit = 5
	holdingWorker(&cfg)
	var mu sync.Mutex
	remaining := "3"
	server := creditServer(t, func() string {
		mu.Lock()
		defer mu.Unlock()
		return `{"data":{"limit":50,"usage":47,"limit_remaining":` + remaining + `,"limit_reset":"2026-10-01"}}`
	})
	cfg.Intake.ModelCreditURL, cfg.Intake.Client = server.URL, server.Client()
	fixture := &noticeTracker{}
	fixture.install(t, alwaysChoose("implement"))
	root, directory := noticeJob(t, chain.State{History: []chain.Result{}})
	startStopQueue(t, cfg, root, 10*time.Millisecond, io.Discard)
	waitFor(t, func() bool { return fixture.count(pausedNoticeText) > 0 })
	time.Sleep(80 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(directory, "workspace", "child-pid")); err == nil {
		t.Fatal("a role ran while the model budget was below the configured minimum")
	}
	if got := fixture.count(pausedNoticeText); got != 1 {
		t.Fatalf("the hold was announced %d times: %v", got, fixture.all())
	}
	if fixture.count(restoredNoticeText) != 0 {
		t.Fatal("the work was called resumed while the budget was still short")
	}
	mu.Lock()
	remaining = "20"
	mu.Unlock()
	waitFor(t, func() bool { return fixture.count(restoredNoticeText) > 0 })
	waitFor(t, func() bool {
		_, err := os.Stat(filepath.Join(directory, "workspace", "child-pid"))
		return err == nil
	})
	if got := fixture.all(); len(got) != 2 || got[0] != pausedNoticeText || got[1] != restoredNoticeText {
		t.Fatalf("the requester was told something else: %v", got)
	}
}

// A request held for the budget before its work ever started still follows an
// authorized stop: the stop launches no model, so it does not wait for the
// budget to return. Before this, nothing read the issue's comments while the
// queue was held, and the requester's stop stayed unread for as long as it
// took someone to raise the budget.
func TestAStopIsFollowedWhileTheBudgetHoldsTheRequest(t *testing.T) {
	cfg := watchConfiguration(t)
	cfg.Intake.MinModelCredit = 5
	holdingWorker(&cfg)
	var mu sync.Mutex
	remaining := "3"
	server := creditServer(t, func() string {
		mu.Lock()
		defer mu.Unlock()
		return `{"data":{"limit":50,"usage":47,"limit_remaining":` + remaining + `,"limit_reset":"2026-10-01"}}`
	})
	cfg.Intake.ModelCreditURL, cfg.Intake.Client = server.URL, server.Client()
	fixture := &noticeTracker{}
	fixture.install(t, alwaysChoose("implement"))
	root, directory := noticeJob(t, chain.State{History: []chain.Result{}})
	startStopQueue(t, cfg, root, 10*time.Millisecond, io.Discard)
	waitFor(t, func() bool { return fixture.count(pausedNoticeText) > 0 })
	// The person who filed the issue, account 55, writes the stop.
	fixture.mu.Lock()
	stop, _ := json.Marshal(map[string]any{"id": 900 + len(fixture.rows), "issueId": 51, "projectId": 17,
		"content": "停止", "createdUser": map[string]any{"id": 55}})
	fixture.rows = append(fixture.rows, stop)
	fixture.mu.Unlock()
	waitFor(t, func() bool {
		_, err := os.Stat(filepath.Join(directory, "stop-request.json"))
		return err == nil
	})
	time.Sleep(80 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(directory, "workspace", "child-pid")); err == nil {
		t.Fatal("a role ran for a stopped request while the budget was short")
	}
	// The budget returns: a stopped request is not told that its work resumed.
	mu.Lock()
	remaining = "20"
	mu.Unlock()
	time.Sleep(120 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(directory, "workspace", "child-pid")); err == nil {
		t.Fatal("a role ran for a stopped request once the budget returned")
	}
	if got := fixture.all(); len(got) != 1 || got[0] != pausedNoticeText {
		t.Fatalf("the requester was told something else: %v", got)
	}
}

// An interrupted request is told on restart that the same request carries on.
// One with a stop standing at its issue is launched only for the stop to be
// recorded, so it is told nothing of the kind; before this the notice was
// posted a moment before the stop took effect.
func TestAnInterruptedRequestWithAStopStandingIsNotToldItCarriesOn(t *testing.T) {
	cfg := watchConfiguration(t)
	holdingWorker(&cfg)
	root, directory := noticeJob(t, chain.State{Pending: &chain.Assignment{Role: "implement"}, History: []chain.Result{}})
	fixture := &noticeTracker{}
	fixture.install(t, alwaysChoose("implement"))
	// The person who filed the issue, account 55, wrote the stop while the
	// runtime was down.
	stop, _ := json.Marshal(map[string]any{"id": 900, "issueId": 51, "projectId": 17,
		"content": "停止", "createdUser": map[string]any{"id": 55}})
	fixture.rows = append(fixture.rows, stop)
	startStopQueue(t, cfg, root, 10*time.Millisecond, io.Discard)
	waitFor(t, func() bool {
		_, err := os.Stat(filepath.Join(directory, "stop-request.json"))
		return err == nil
	})
	time.Sleep(80 * time.Millisecond)
	if got := fixture.all(); len(got) != 0 {
		t.Fatalf("a request being stopped was told: %v", got)
	}
	if _, err := os.Stat(filepath.Join(directory, "workspace", "child-pid")); err == nil {
		t.Fatal("a role ran for a request with a stop standing")
	}
}

// A running role is stopped when the budget falls away under it.
func TestBudgetFallingAwayStopsTheRunningChild(t *testing.T) {
	cfg := watchConfiguration(t)
	cfg.Intake.MinModelCredit = 5
	holdingWorker(&cfg)
	var mu sync.Mutex
	remaining := "20"
	server := creditServer(t, func() string {
		mu.Lock()
		defer mu.Unlock()
		return `{"data":{"limit":50,"usage":30,"limit_remaining":` + remaining + `}}`
	})
	cfg.Intake.ModelCreditURL, cfg.Intake.Client = server.URL, server.Client()
	fixture := &noticeTracker{}
	fixture.install(t, alwaysChoose("implement"))
	root, directory := noticeJob(t, chain.State{History: []chain.Result{}})
	startStopQueue(t, cfg, root, 10*time.Millisecond, io.Discard)
	pid := waitTestPID(t, filepath.Join(directory, "workspace", "child-pid"))
	mu.Lock()
	remaining = "1"
	mu.Unlock()
	waitFor(t, func() bool { return fixture.count(pausedNoticeText) > 0 })
	waitFor(t, func() bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) })
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("the role kept running after the budget ran out: %v", err)
	}
}

// A key with no limit has nothing to run out of, and an endpoint that cannot
// be read is not evidence of an empty budget.
func TestUnlimitedOrUnreadableBudgetNeverPausesOrPosts(t *testing.T) {
	for _, shape := range []struct{ name, body string }{
		{"no limit on the key", `{"data":{"limit":null,"usage":12,"limit_remaining":null}}`},
		{"unreadable endpoint", ""},
	} {
		t.Run(shape.name, func(t *testing.T) {
			cfg := watchConfiguration(t)
			cfg.Intake.MinModelCredit = 5
			server := creditServer(t, func() string { return shape.body })
			cfg.Intake.ModelCreditURL, cfg.Intake.Client = server.URL, server.Client()
			fixture := &noticeTracker{}
			fixture.install(t, alwaysChoose("done"))
			root, directory := noticeJob(t, chain.State{History: []chain.Result{}})
			startStopQueue(t, cfg, root, 10*time.Millisecond, io.Discard)
			waitFor(t, func() bool { return loadJobState(t, directory).Done })
			if got := fixture.all(); len(got) != 0 {
				t.Fatalf("the requester was told the work was paused: %v", got)
			}
			if notices := readNotices(t, directory).Notices; len(notices) != 0 {
				t.Fatalf("a notice was recorded without a budget below the minimum: %+v", notices)
			}
		})
	}
}

// A long silence is not a failure and not a completion. The requester is told
// once that recovery is still running, with the last failure's first line and
// no credential value in it.
func TestNoProgressNoticeSaysTheLastFailureWithoutACredential(t *testing.T) {
	cfg := watchConfiguration(t)
	holdingWorker(&cfg)
	fixture := &noticeTracker{}
	fixture.install(t, alwaysChoose("implement"))
	start := time.Now().UTC().Add(-3 * time.Hour)
	failure := "routing unavailable: model service returned HTTP 402 for key synthetic-watch-key\nsecond line is not in the notice"
	root, directory := noticeJob(t, chain.State{Step: "implement", History: []chain.Result{
		{Role: "implement", Speaker: "worker", Output: "earlier work", StartedAt: start, FinishedAt: start.Add(time.Minute)},
		{Role: "router", Speaker: "runtime", Error: "routing unavailable: an earlier one", StartedAt: start.Add(2 * time.Hour), FinishedAt: start.Add(2 * time.Hour)},
		{Role: "router", Speaker: "runtime", Error: failure, StartedAt: start.Add(150 * time.Minute), FinishedAt: start.Add(150 * time.Minute)},
	}})
	// The queue's engines have said a stall since before this one began.
	queueRanSince(t, root, cfg, start)
	startStopQueue(t, cfg, root, 10*time.Millisecond, io.Discard)
	const opening = "自動処理は続いていますが"
	waitFor(t, func() bool { return len(fixture.withPrefix(opening)) > 0 })
	time.Sleep(120 * time.Millisecond)
	found := fixture.withPrefix(opening)
	if len(found) != 1 {
		t.Fatalf("the no-progress notice reached the requester %d times: %v", len(found), found)
	}
	stall := found[0]
	if strings.Contains(stall, "synthetic-watch-key") {
		t.Fatalf("a credential value reached the requester: %q", stall)
	}
	if !strings.Contains(stall, "[credential]") || !strings.Contains(stall, "HTTP 402") {
		t.Fatalf("the last failure did not reach the requester scrubbed: %q", stall)
	}
	if strings.Contains(stall, "second line") {
		t.Fatalf("more than the failure's first line reached the requester: %q", stall)
	}
	if !strings.Contains(stall, "過去 178 分間") && !strings.Contains(stall, "過去 179 分間") && !strings.Contains(stall, "過去 180 分間") {
		t.Fatalf("the notice did not say how long nothing completed: %q", stall)
	}
	// The notice changes nothing: the work carries on to a running role.
	pid := waitTestPID(t, filepath.Join(directory, "workspace", "child-pid"))
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("the no-progress notice stopped the work: %v", err)
	}
}

// Its window is measured from the last completed step, it does not repeat for
// six hours, and zero switches it off entirely.
func TestNoProgressNoticeWindowRepetitionAndOffSwitch(t *testing.T) {
	now := time.Now().UTC()
	recent := chain.State{History: []chain.Result{
		{Role: "implement", Output: "a completed step", FinishedAt: now.Add(-10 * time.Minute)},
		{Role: "router", Error: "routing unavailable", FinishedAt: now.Add(-time.Minute)},
	}}
	elapsed, failure, stalled := stalledFor(recent, now)
	if !stalled || failure != "routing unavailable" || elapsed < 9*time.Minute || elapsed > 11*time.Minute {
		t.Fatalf("the window is not measured from the last completed step: %v %q %t", elapsed, failure, stalled)
	}
	completed := chain.State{History: []chain.Result{
		{Role: "router", Error: "routing unavailable", FinishedAt: now.Add(-3 * time.Hour)},
		{Role: "implement", Output: "work came back", FinishedAt: now.Add(-time.Minute)},
	}}
	if _, _, stalled := stalledFor(completed, now); stalled {
		t.Fatal("a completed step inside the window was read as no progress")
	}
	// The runtime writes its own note after every launch, with no error of
	// its own; the failures behind those notes still count, and a completed
	// step behind one still ends the run.
	ordered := &chain.Workflow{Stages: []chain.Stage{{Name: "implement", Kind: "model"}, {Name: "verify", Kind: "command", OnFailure: "implement"}}}
	interleaved := chain.State{Workflow: ordered, History: []chain.Result{
		{Role: "implement", Speaker: "implement-process", Output: "a completed step", FinishedAt: now.Add(-10 * time.Minute)},
		{Role: "implement", Speaker: "runtime", Output: "Process implement-process exited 0.", FinishedAt: now.Add(-10 * time.Minute)},
		{Role: "verify", Speaker: "verify-process", Error: "fork/exec verify: no such file or directory", FinishedAt: now.Add(-2 * time.Minute)},
		{Role: "verify", Speaker: "runtime", Output: "Process verify-process did not exit 0: fork/exec verify: no such file or directory", FinishedAt: now.Add(-2 * time.Minute)},
		{Role: "verify", Speaker: "verify-process", Error: "fork/exec verify: no such file or directory (again)", FinishedAt: now.Add(-time.Minute)},
		{Role: "verify", Speaker: "runtime", Output: "Process verify-process did not exit 0: fork/exec verify: no such file or directory", FinishedAt: now.Add(-time.Minute)},
	}}
	elapsed, failure, stalled = stalledFor(interleaved, now)
	if !stalled || failure != "fork/exec verify: no such file or directory (again)" || elapsed < 9*time.Minute || elapsed > 11*time.Minute {
		t.Fatalf("failures behind the runtime's own notes were not read as no progress: %v %q %t", elapsed, failure, stalled)
	}
	settled := chain.State{Workflow: ordered, History: []chain.Result{
		{Role: "verify", Speaker: "verify-process", Error: "fork/exec verify: no such file or directory", FinishedAt: now.Add(-3 * time.Hour)},
		{Role: "verify", Speaker: "runtime", Output: "Process verify-process did not exit 0.", FinishedAt: now.Add(-3 * time.Hour)},
		{Role: "verify", Speaker: "verify-process", Output: "ok", FinishedAt: now.Add(-time.Minute)},
		{Role: "verify", Speaker: "runtime", Output: "Process verify-process exited 0.", FinishedAt: now.Add(-time.Minute)},
	}}
	if _, _, stalled := stalledFor(settled, now); stalled {
		t.Fatal("a completed step followed by the runtime's note was read as no progress")
	}
	notes := chain.State{History: []chain.Result{{Role: "elicit", Speaker: "runtime", Output: "taken up again", FinishedAt: now.Add(-time.Hour)}}}
	if _, _, stalled := stalledFor(notes, now); stalled {
		t.Fatal("the runtime's notes alone were read as failures")
	}
	// A role with two processes completes a step only when both return; one
	// succeeding at every launch does not move the last completed step.
	// Without the ordered run's notes there is no boundary between launches
	// of one role, so each process record is a launch of its own: a role that
	// completed and then failed to start twice stalls only since it completed.
	free := chain.State{History: []chain.Result{
		{Role: "implement", Speaker: "implement-process", Output: "done", StartedAt: now.Add(-3*time.Hour - 10*time.Minute), FinishedAt: now.Add(-10 * time.Minute)},
		{Role: "implement", Speaker: "implement-process", Error: "fork/exec implement: no such file", StartedAt: now.Add(-5 * time.Minute), FinishedAt: now.Add(-5 * time.Minute)},
		{Role: "implement", Speaker: "implement-process", Error: "fork/exec implement: no such file", StartedAt: now.Add(-time.Minute), FinishedAt: now.Add(-time.Minute)},
	}}
	elapsed, _, stalled = stalledFor(free, now)
	if !stalled || elapsed < 9*time.Minute || elapsed > 11*time.Minute {
		t.Fatalf("without notes, launches of one role were fused: %v %t", elapsed, stalled)
	}
	twoProcesses := chain.State{Workflow: ordered, History: []chain.Result{
		{Role: "implement", Speaker: "implement-process", Output: "done", FinishedAt: now.Add(-30 * time.Minute)},
		{Role: "implement", Speaker: "runtime", Output: "Process implement-process exited 0.", FinishedAt: now.Add(-30 * time.Minute)},
		{Role: "verify", Speaker: "project-build", Output: "ok", FinishedAt: now.Add(-20 * time.Minute)},
		{Role: "verify", Speaker: "project-tests", Error: "exit status 1", FinishedAt: now.Add(-20 * time.Minute)},
		{Role: "verify", Speaker: "runtime", Output: "Process project-tests did not exit 0.", FinishedAt: now.Add(-20 * time.Minute)},
		{Role: "verify", Speaker: "project-build", Output: "ok", FinishedAt: now.Add(-time.Minute)},
		{Role: "verify", Speaker: "project-tests", Error: "exit status 1 (again)", FinishedAt: now.Add(-time.Minute)},
		{Role: "verify", Speaker: "runtime", Output: "Process project-tests did not exit 0.", FinishedAt: now.Add(-time.Minute)},
	}}
	elapsed, failure, stalled = stalledFor(twoProcesses, now)
	if !stalled || failure != "exit status 1 (again)" || elapsed < 29*time.Minute || elapsed > 31*time.Minute {
		t.Fatalf("a launch with one process succeeding was read as a completed step: %v %q %t", elapsed, failure, stalled)
	}
	// A note without error between failed launches, such as a step taken up
	// again after a restart, does not end the run of failures.
	takenUp := chain.State{Workflow: ordered, History: []chain.Result{
		{Role: "elicit", Speaker: "elicit-process", Error: "exit status 1", StartedAt: now.Add(-40 * time.Minute), FinishedAt: now.Add(-40 * time.Minute)},
		{Role: "elicit", Speaker: "runtime", Output: "Process elicit-process did not exit 0.", FinishedAt: now.Add(-40 * time.Minute)},
		{Role: "elicit", Speaker: "runtime", Output: "taken up again", FinishedAt: now.Add(-5 * time.Minute)},
		{Role: "elicit", Speaker: "elicit-process", Error: "exit status 1", FinishedAt: now.Add(-time.Minute)},
		{Role: "elicit", Speaker: "runtime", Output: "Process elicit-process did not exit 0.", FinishedAt: now.Add(-time.Minute)},
	}}
	elapsed, _, stalled = stalledFor(takenUp, now)
	if !stalled || elapsed < 39*time.Minute || elapsed > 41*time.Minute {
		t.Fatalf("a note between failed launches ended the run: %v %t", elapsed, stalled)
	}
	posted := now.Add(-5 * time.Hour)
	log := noticeLog{Notices: []noticeRecord{{Kind: stallNotice, Text: "…", WrittenAt: posted, PostedAt: &posted}}}
	if noticeDue(log, stallNotice, now) {
		t.Fatal("the no-progress notice repeated inside six hours")
	}
	log.Notices[0].WrittenAt = now.Add(-7 * time.Hour)
	if !noticeDue(log, stallNotice, now) {
		t.Fatal("the no-progress notice never speaks again")
	}
	off := 0
	var cfg config
	cfg.Intake = &intakeConfig{StallNoticeMinutes: &off}
	if stallWindow(cfg) != 0 {
		t.Fatal("zero did not switch the no-progress notice off")
	}
	if stallWindow(config{}) != 90*time.Minute {
		t.Fatalf("the default window is %v", stallWindow(config{}))
	}
}

// A submission that is refused, and one that lands without saying so, both end
// with exactly one comment at the issue.
func TestANoticeSurvivesAFailedOrAmbiguousSubmissionWithoutRepeatingItself(t *testing.T) {
	for _, shape := range []struct {
		name     string
		setup    func(*noticeTracker)
		attempts int
	}{
		{"submission refused once", func(n *noticeTracker) { n.postFail = 1 }, 2},
		{"submission never confirmed", func(n *noticeTracker) { n.silent = true }, 1},
	} {
		t.Run(shape.name, func(t *testing.T) {
			cfg := watchConfiguration(t)
			root, directory := noticeJob(t, chain.State{Pending: &chain.Assignment{Role: "implement"}, History: []chain.Result{}})
			fixture := &noticeTracker{}
			shape.setup(fixture)
			fixture.install(t, alwaysChoose("done"))
			startStopQueue(t, cfg, root, 10*time.Millisecond, io.Discard)
			waitFor(t, func() bool {
				log := readNotices(t, directory)
				return len(log.Notices) == 1 && log.Notices[0].PostedAt != nil
			})
			time.Sleep(100 * time.Millisecond)
			if got := fixture.count(resumeNoticeText); got != 1 {
				t.Fatalf("the requester saw the same notice %d times: %v", got, fixture.all())
			}
			if got := fixture.attempts(); got != shape.attempts {
				t.Fatalf("the controller submitted %d times, expected %d", got, shape.attempts)
			}
		})
	}
}

// The shipped example carries both settings, and an unusable value is refused
// at startup rather than at two in the morning.
func TestNoticeSettingsAreCheckedBeforeAnyWorkIsAccepted(t *testing.T) {
	cfg := operatorExample(t)
	if cfg.Intake.StallNoticeMinutes == nil || *cfg.Intake.StallNoticeMinutes != 90 {
		t.Fatalf("the example lost its no-progress window: %v", cfg.Intake.StallNoticeMinutes)
	}
	if cfg.Intake.MinModelCredit != 0 {
		t.Fatalf("the unedited example already asks a provider for a balance: %v", cfg.Intake.MinModelCredit)
	}
	if err := validateNotices(cfg); err != nil {
		t.Fatal(err)
	}
	if got := modelCreditURL(cfg); got != defaultModelCreditURL {
		t.Fatalf("the example does not fall back to the provider endpoint: %s", got)
	}
	negative := -1
	for _, shape := range []struct {
		name   string
		change func(*intakeConfig)
		reason string
	}{
		{"negative minimum", func(i *intakeConfig) { i.MinModelCredit = -1 }, "min_model_credit"},
		{"negative window", func(i *intakeConfig) { i.StallNoticeMinutes = &negative }, "stall_notice_minutes"},
		{"plain http endpoint", func(i *intakeConfig) { i.ModelCreditURL = "http://budget.example/key" }, "model_credit_url"},
		{"endpoint carrying a credential", func(i *intakeConfig) {
			i.ModelCreditURL = "https://name:secret@budget.example/key"
		}, "model_credit_url"},
	} {
		t.Run(shape.name, func(t *testing.T) {
			broken := operatorExample(t)
			intake := *broken.Intake
			shape.change(&intake)
			intake.ProjectID, intake.CreatedSince = 17, "2026-01-02T00:00:00Z"
			broken.Intake = &intake
			if err := validateNotices(broken); err == nil || !strings.Contains(err.Error(), shape.reason) {
				t.Fatalf("an unusable setting was accepted: %v", err)
			}
			useCatalogTransport(t, func(*http.Request) (*http.Response, error) {
				t.Error("an unusable setting still reached the network")
				return nil, errors.New("unconfigured")
			})
			root := filepath.Join(t.TempDir(), "must-not-be-created")
			// Bounded, so an unchecked setting fails here instead of running
			// the queue for as long as the test package is allowed to live.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := watchRequests(ctx, broken, root, io.Discard); err == nil ||
				!strings.Contains(err.Error(), shape.reason) {
				t.Fatalf("intake started with an unusable setting: %v", err)
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatal("an unusable setting still created a queue")
			}
		})
	}
	missing := operatorExample(t)
	intake := *missing.Intake
	intake.MinModelCredit = 5
	missing.Intake = &intake
	missing.Router.Decision.KeyEnv = ""
	if err := validateNotices(missing); err == nil || !strings.Contains(err.Error(), "key_env") {
		t.Fatalf("a budget check without a named credential was accepted: %v", err)
	}
}

// The balance is read with the configured model credential, and that value
// never reaches a log line.
func TestBudgetReaderUsesTheConfiguredCredentialAndNeverPrintsIt(t *testing.T) {
	cfg := watchConfiguration(t)
	cfg.Intake.MinModelCredit = 5
	var authorized string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorized = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = fmt.Fprint(w, `{"error":"no balance for key synthetic-watch-key"}`)
	}))
	defer server.Close()
	cfg.Intake.ModelCreditURL, cfg.Intake.Client = server.URL, server.Client()
	var recorded []string
	low, known := modelCreditHold(context.Background(), cfg, func(message string) { recorded = append(recorded, message) })
	if low || known {
		t.Fatalf("an endpoint answering 402 was read as an empty budget: low=%t known=%t", low, known)
	}
	if authorized != "Bearer synthetic-watch-key" {
		t.Fatalf("the balance was not asked for with the configured credential: %q", authorized)
	}
	if len(recorded) != 1 || strings.Contains(recorded[0], "synthetic-watch-key") {
		t.Fatalf("the credential value reached the log: %v", recorded)
	}
	if !strings.Contains(recorded[0], "[credential]") {
		t.Fatalf("the endpoint's answer was not scrubbed: %v", recorded)
	}
}

func TestNoticeDetailKeepsOneScrubbedLineWithinTwoHundredCharacters(t *testing.T) {
	cfg := watchConfiguration(t)
	long := strings.Repeat("あ", 300)
	detail := noticeDetail(cfg, "\n  synthetic-watch-key failed: "+long+"\nsecond line")
	if strings.Contains(detail, "synthetic-watch-key") || !strings.HasPrefix(detail, "[credential] failed:") {
		t.Fatalf("the credential value survived: %q", detail)
	}
	if strings.Contains(detail, "second line") {
		t.Fatalf("more than the first line survived: %q", detail)
	}
	if count := utf8.RuneCountInString(detail); count != 200 {
		t.Fatalf("the detail is %d characters", count)
	}
	if !utf8.ValidString(detail) {
		t.Fatal("the detail was cut inside a character")
	}
}

func TestARunningLaunchThatWritesNothingForTheWindowIsSaidWithoutAFailure(t *testing.T) {
	now := time.Now().UTC()
	accepted := now.Add(-100 * time.Minute)
	if quiet := quietFor(chain.State{}, accepted, now); quiet != 100*time.Minute {
		t.Fatalf("a request with no record is measured from its acceptance: %v", quiet)
	}
	state := chain.State{History: []chain.Result{{Role: "implement", Speaker: "implement-process", StartedAt: now.Add(-90 * time.Minute), FinishedAt: now.Add(-30 * time.Minute)}}}
	if quiet := quietFor(state, accepted, now); quiet != 30*time.Minute {
		t.Fatalf("a request with a record is measured from that record: %v", quiet)
	}
	if quiet := quietFor(chain.State{}, time.Time{}, now); quiet != 0 {
		t.Fatalf("a request with nothing to measure from was measured: %v", quiet)
	}
	if text := stallNoticeText(95, ""); !strings.Contains(text, "95 分") || !strings.Contains(text, "失敗はなく") || strings.Contains(text, "直近の失敗") {
		t.Fatalf("the notice without a failure names one: %q", text)
	}
}

func TestALongQuietLaunchIsSaidOnlyWhileTheWorkRuns(t *testing.T) {
	cfg := watchConfiguration(t)
	window := 90
	cfg.Intake.StallNoticeMinutes = &window
	var mu sync.Mutex
	posted := map[string][]string{}
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "watch-tracker.example" {
			return nil, http.ErrNotSupported
		}
		key := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v2/issues/"), "/comments")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			if err := r.ParseForm(); err != nil {
				return nil, err
			}
			mu.Lock()
			posted[key] = append(posted[key], r.PostForm.Get("content"))
			mu.Unlock()
			return selectionReply(r, 201, map[string]any{"id": 700 + len(posted[key]), "content": r.PostForm.Get("content")}), nil
		case strings.HasSuffix(r.URL.Path, "/comments"):
			return selectionReply(r, 200, []any{}), nil
		}
		return selectionReply(r, 200, map[string]any{"id": 701, "content": "x"}), nil
	})
	root := t.TempDir()
	accepted := time.Now().Add(-3 * time.Hour)
	directories := map[int]string{}
	for _, id := range []int{51, 52} {
		directory := filepath.Join(root, "jobs", fmt.Sprint(id))
		if err := os.MkdirAll(filepath.Join(directory, "run"), 0700); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(watchedIssue(id, "Original conditions", "2026-01-03T00:00:00Z"))
		if err := writeRuntimeFile(filepath.Join(directory, "issue.json"), raw); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(filepath.Join(directory, "issue.json"), accepted, accepted); err != nil {
			t.Fatal(err)
		}
		writeJobHistory(t, directory, chain.State{})
		directories[id] = directory
	}
	// The queue's engines have said a stall since before these were accepted.
	queueRanSince(t, root, cfg, accepted)
	// Accepted three hours ago with nothing recorded: the one waiting its
	// turn says nothing, the one running says it is long but not failing.
	waiting := sourceIssue{ID: 52, Key: "EXAMPLE-52"}
	if err := noteStall(context.Background(), cfg, requestNotices(cfg, waiting, directories[52]), directories[52], false); err != nil {
		t.Fatal(err)
	}
	running := sourceIssue{ID: 51, Key: "EXAMPLE-51"}
	if err := noteStall(context.Background(), cfg, requestNotices(cfg, running, directories[51]), directories[51], true); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(posted["EXAMPLE-52"]) != 0 {
		t.Fatalf("a request waiting its turn was told its work is long: %q", posted["EXAMPLE-52"])
	}
	if len(posted["EXAMPLE-51"]) != 1 || !strings.Contains(posted["EXAMPLE-51"][0], "失敗はなく") || !strings.Contains(posted["EXAMPLE-51"][0], "過去 1") {
		t.Fatalf("the running request was not told, or told wrongly: %q", posted["EXAMPLE-51"])
	}
}

// statusesWritten reports that the delivered turns have run for every one of
// these requests since their status records were last removed.
func statusesWritten(directories []string) func() bool {
	return func() bool {
		for _, directory := range directories {
			if _, err := os.Stat(filepath.Join(directory, "status.json")); err != nil {
				return false
			}
		}
		return true
	}
}

// The first engine that knows the list of models used meets a queue of
// requests delivered before it. It used to post the list on every one of
// them at once. None of them hears it now: not on the first tick, not on the
// ticks after, not after a restart; each keeps one record saying the list
// predates it, so nothing weighs it again.
func TestRequestsDeliveredBeforeTheirNoticeExistedNeverHearIt(t *testing.T) {
	cfg := watchConfiguration(t)
	cfg.Intake.Announce = true
	cfg.Intake.Statuses = &statusConfig{Delivered: 3}
	fixture := &noticeTracker{}
	fixture.install(t, alwaysChoose("done"))
	root := t.TempDir()
	delivered := time.Now().UTC().Add(-26 * time.Hour)
	var directories []string
	for _, id := range []int{51, 52, 53, 54} {
		directories = append(directories, deliveredJob(t, root, id, delivered))
	}
	finish := startStopQueue(t, cfg, root, 10*time.Millisecond, io.Discard)
	waitFor(t, func() bool {
		for _, directory := range directories {
			if len(readNotices(t, directory).Notices) == 0 {
				return false
			}
		}
		return true
	})
	// The same engine's later ticks.
	time.Sleep(100 * time.Millisecond)
	finish()
	if got := fixture.attempts(); got != 0 {
		t.Fatalf("requests delivered before the list existed were told %d times: %v", got, fixture.all())
	}
	kinds, err := loadNoticeKinds(root)
	if err != nil || !kinds.Since[modelsNotice].After(delivered) {
		t.Fatalf("the queue does not record the list as begun with this engine: %+v %v", kinds, err)
	}
	// A restart meets the same requests and their records.
	for _, directory := range directories {
		if err := os.Remove(filepath.Join(directory, "status.json")); err != nil {
			t.Fatal(err)
		}
	}
	finish = startStopQueue(t, cfg, root, 10*time.Millisecond, io.Discard)
	waitFor(t, statusesWritten(directories))
	time.Sleep(100 * time.Millisecond)
	finish()
	if got := fixture.attempts(); got != 0 {
		t.Fatalf("a restart told requests delivered before the list existed %d times: %v", got, fixture.all())
	}
	for _, directory := range directories {
		log := readNotices(t, directory).Notices
		if len(log) != 1 || log[0].Kind != modelsNotice || !log[0].Predates || log[0].PostedAt != nil || log[0].Text != "" {
			t.Fatalf("the record does not say once that the list predates the request: %+v", log)
		}
	}
}

// A request delivered after the engine started is told which models it used,
// exactly once, on that engine's first run as on any other: here one that was
// waiting for its requester at the first tick and was delivered after it.
// Later ticks and a restart do not post the list again.
func TestARequestDeliveredAfterTheFirstTickHearsItsListOnce(t *testing.T) {
	cfg := watchConfiguration(t)
	cfg.Intake.Announce = true
	cfg.Intake.Statuses = &statusConfig{Delivered: 3}
	fixture := &noticeTracker{}
	fixture.install(t, alwaysChoose("done"))
	root, directory := noticeJob(t, chain.State{Waiting: true, Step: "implement", History: []chain.Result{}})
	finish := startStopQueue(t, cfg, root, 10*time.Millisecond, io.Discard)
	// The first tick met the request waiting and wrote down how far its
	// conversation had gone.
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(directory, "question.json")); return err == nil })
	delivered := time.Now().UTC()
	writeJobHistory(t, directory, chain.State{Done: true, History: []chain.Result{
		{Role: "implement", Speaker: "worker", Model: "maker/one", StartedAt: delivered.Add(-32 * time.Second), FinishedAt: delivered},
	}})
	waitFor(t, func() bool { return fixture.count(deliveredList) > 0 })
	time.Sleep(100 * time.Millisecond)
	finish()
	if err := os.Remove(filepath.Join(directory, "status.json")); err != nil {
		t.Fatal(err)
	}
	finish = startStopQueue(t, cfg, root, 10*time.Millisecond, io.Discard)
	waitFor(t, statusesWritten([]string{directory}))
	time.Sleep(100 * time.Millisecond)
	finish()
	if got := fixture.all(); len(got) != 1 || got[0] != deliveredList {
		t.Fatalf("the delivered request was told: %v", got)
	}
	if log := readNotices(t, directory).Notices; len(log) != 1 || log[0].Kind != modelsNotice || log[0].Predates || log[0].PostedAt == nil {
		t.Fatalf("the record does not show one posted list: %+v", log)
	}
}

// A kind can also be new to a queue that had it before: taken out of the
// queue's record between two runs, or not posted by a run in between, as an
// engine that did not know it or a configuration that switched it off would
// leave it. Either way it starts again with the engine that posts it again,
// and a request delivered while it was gone is not told. Kept, the same
// request is told, as it always was.
func TestAKindThatCameBackStartsWhenItCameBack(t *testing.T) {
	for _, shape := range []struct {
		name      string
		meanwhile func(t *testing.T, cfg config, root string)
		told      int
	}{
		{"the kind was kept", func(*testing.T, config, string) {}, 1},
		{"the kind was taken out of the record between two runs", func(t *testing.T, _ config, root string) {
			kinds, err := loadNoticeKinds(root)
			if err != nil {
				t.Fatal(err)
			}
			delete(kinds.Since, modelsNotice)
			if err := saveNoticeKinds(root, kinds); err != nil {
				t.Fatal(err)
			}
		}, 0},
		{"a run in between did not post the kind", func(t *testing.T, cfg config, root string) {
			silent := *cfg.Intake
			silent.Announce = false
			cfg.Intake = &silent
			finish := startStopQueue(t, cfg, root, 10*time.Millisecond, io.Discard)
			waitFor(t, func() bool {
				kinds, err := loadNoticeKinds(root)
				return err == nil && kinds.Since[modelsNotice].IsZero()
			})
			finish()
		}, 0},
	} {
		t.Run(shape.name, func(t *testing.T) {
			cfg := watchConfiguration(t)
			cfg.Intake.Announce = true
			cfg.Intake.Statuses = &statusConfig{Delivered: 3}
			fixture := &noticeTracker{}
			fixture.install(t, alwaysChoose("done"))
			root := t.TempDir()
			now := time.Now().UTC()
			// Engines that post the list have run on this queue for two hours,
			// and the request was delivered an hour ago; its list is not out yet.
			queueRanSince(t, root, cfg, now.Add(-2*time.Hour))
			directory := deliveredJob(t, root, 51, now.Add(-time.Hour))
			shape.meanwhile(t, cfg, root)
			finish := startStopQueue(t, cfg, root, 10*time.Millisecond, io.Discard)
			waitFor(t, func() bool { return len(readNotices(t, directory).Notices) > 0 })
			time.Sleep(100 * time.Millisecond)
			finish()
			if got := fixture.attempts(); got != shape.told || fixture.count(deliveredList) != shape.told {
				t.Fatalf("the request was told %d times, expected %d: %v", got, shape.told, fixture.all())
			}
			log := readNotices(t, directory).Notices
			if len(log) != 1 || log[0].Kind != modelsNotice || log[0].Predates != (shape.told == 0) || (log[0].PostedAt != nil) != (shape.told == 1) {
				t.Fatalf("the record does not say what became of the list: %+v", log)
			}
		})
	}
}

// Every kind keeps to the same rule, not only the list. The first engine that
// keeps the queue's record meets a request in flight: a stage announced before
// stays as it was, a stage that began before and was not announced yet is
// settled without its sentence though it runs again now, and the request is
// not told it was accepted two hours ago. A stall that began before is not
// said and leaves no record, so a later one is said in its own time. A stage
// that begins under this engine is announced as always.
func TestARequestInFlightIsNotToldWhatBeganBeforeTheEngine(t *testing.T) {
	cfg := announcingStagesConfig(t)
	cfg.Roles = append(cfg.Roles, chain.Role{Name: "report", Purpose: "say what was done", Processes: []chain.Process{{Name: "writer", Command: []string{"/bin/true"}}}})
	cfg.Workflow.Stages = append(cfg.Workflow.Stages, chain.Stage{Name: "report", Kind: chain.CommandStage, Announce: "報告を書きます。"})
	fixture := &noticeTracker{}
	fixture.install(t, alwaysChoose("done"))
	root, directory := noticeJob(t, chain.State{})
	// Accepted two hours ago. Its first stage began then and an engine of
	// that time announced it; its second began an hour later, failed, and was
	// not announced yet. Nothing has completed since the first stage.
	earlier := time.Now().UTC().Add(-2 * time.Hour)
	if err := os.Chtimes(filepath.Join(directory, "issue.json"), earlier, earlier); err != nil {
		t.Fatal(err)
	}
	writeJobHistory(t, directory, chain.State{History: []chain.Result{
		{Role: "work", Speaker: "worker", Model: "maker/first", StartedAt: earlier, FinishedAt: earlier.Add(time.Minute)},
		{Role: "verify", Speaker: "build", Error: "exit status 1", StartedAt: earlier.Add(time.Hour), FinishedAt: earlier.Add(time.Hour)},
	}})
	said := earlier.Add(time.Second)
	announced, _ := json.Marshal(noticeLog{Notices: []noticeRecord{{Kind: stagePrefix + "work", Text: "作業を始めます。 (モデル: maker/first)", WrittenAt: said, PostedAt: &said}}})
	if err := writeRuntimeFile(filepath.Join(directory, "notices.json"), announced); err != nil {
		t.Fatal(err)
	}
	// The engine starts as the watch loop starts it, and the second stage
	// runs again under it.
	if err := startNoticeKinds(root, cfg, time.Now(), func(string) {}); err != nil {
		t.Fatal(err)
	}
	beginStage(t, directory, "verify-build")
	observe := func(string) {}
	issue := announcedIssue()
	acceptTurn(context.Background(), cfg, issue, directory, 0, observe)
	announceStages(context.Background(), cfg, issue, directory, observe)
	if err := noteStall(context.Background(), cfg, requestNotices(cfg, issue, directory), directory, true); err != nil {
		t.Fatal(err)
	}
	if got := fixture.all(); len(got) != 0 {
		t.Fatalf("the request was told what began before the engine: %v", got)
	}
	beginStage(t, directory, "report-writer")
	announceStages(context.Background(), cfg, issue, directory, observe)
	if got := fixture.all(); len(got) != 1 || got[0] != "報告を書きます。" {
		t.Fatalf("a stage that began under this engine was announced as %v", got)
	}
	records := map[string]noticeRecord{}
	for _, record := range readNotices(t, directory).Notices {
		if _, twice := records[record.Kind]; twice {
			t.Fatalf("the request has two records of %s", record.Kind)
		}
		records[record.Kind] = record
	}
	if len(records) != 4 {
		t.Fatalf("the request's records are %+v", records)
	}
	for kind, predates := range map[string]bool{stagePrefix + "work": false, acceptedNotice: true, stagePrefix + "verify": true, stagePrefix + "report": false} {
		record, found := records[kind]
		if !found || record.Predates != predates || (record.PostedAt != nil) == predates {
			t.Fatalf("the record of %s does not say what became of it: %+v", kind, records)
		}
	}
}

// pendingRun is announcingStagesConfig with stages that run for a moment, so
// a request taken up again is seen running.
func pendingRun(t *testing.T) config {
	t.Helper()
	cfg := announcingStagesConfig(t)
	cfg.Roles[0].Processes[0].Command = []string{"/bin/sh", "-c", "sleep 0.3"}
	cfg.Roles[1].Processes[0].Command = []string{"/bin/sh", "-c", "sleep 0.3"}
	return cfg
}

// A stage that was running when the engine was replaced is taken up again
// after the restart. It began before this engine, so the first engine that
// keeps the queue's record does not announce it, though it runs again under
// it: the note the restart writes for the cut launch has no start to place
// after the record's, and its end, at the restart, is not when the stage
// began.
func TestAStageRunningWhenTheEngineWasReplacedIsNotAnnouncedAgain(t *testing.T) {
	earlier := time.Now().UTC().Add(-2 * time.Hour)
	for _, shape := range []struct {
		name    string
		pending string
		history []chain.Result
		before  []string
	}{
		{"its first stage", "work", []chain.Result{}, []string{"作業を始めます。"}},
		{"its second stage", "verify", []chain.Result{
			{Role: "work", Speaker: "worker", Model: "maker/first", StartedAt: earlier, FinishedAt: earlier.Add(time.Minute)},
			{Role: "work", Speaker: "runtime", Output: "Runtime record for stage work, written by the engine from what it observed.\nProcess worker exited 0.\n", StartedAt: earlier.Add(time.Minute), FinishedAt: earlier.Add(time.Minute)},
		}, []string{"作業を始めます。", "検証を始めます。"}},
	} {
		t.Run(shape.name, func(t *testing.T) {
			cfg := pendingRun(t)
			fixture := &noticeTracker{}
			fixture.install(t, alwaysChoose("done"))
			root, directory := noticeJob(t, chain.State{Workflow: cfg.Workflow, Step: "work", Pending: &chain.Assignment{Role: shape.pending}, History: shape.history})
			if err := os.Chtimes(filepath.Join(directory, "issue.json"), earlier, earlier); err != nil {
				t.Fatal(err)
			}
			finish := startStopQueue(t, cfg, root, 10*time.Millisecond, io.Discard)
			waitFor(t, func() bool { return loadJobState(t, directory).Done })
			time.Sleep(100 * time.Millisecond)
			finish()
			for _, posted := range fixture.all() {
				for _, sentence := range shape.before {
					if strings.HasPrefix(posted, sentence) {
						t.Fatalf("a stage that began before this engine was announced: %q", fixture.all())
					}
				}
			}
			for _, record := range readNotices(t, directory).Notices {
				if record.Kind == stagePrefix+shape.pending && !record.Predates {
					t.Fatalf("the stage taken up again is not settled as begun before: %+v", record)
				}
			}
		})
	}
}

// A stage whose first launch began under this engine and was cut by a restart
// before its sentence went out is announced when it runs again: the note the
// restart writes for the cut launch keeps when it began. Whether the sentence
// names a model depends on whether the launch taken up again has chosen one by
// the look that finds the note, as for any stage known from its record.
func TestAStageCutUnderThisEngineIsAnnouncedWhenItRunsAgain(t *testing.T) {
	cfg := pendingRun(t)
	fixture := &noticeTracker{}
	fixture.install(t, alwaysChoose("done"))
	accepted := time.Now().UTC().Add(-10 * time.Minute)
	root, directory := noticeJob(t, chain.State{Workflow: cfg.Workflow, Step: "work", Pending: &chain.Assignment{Role: "work"}, PendingSince: accepted, History: []chain.Result{}})
	queueRanSince(t, root, cfg, accepted.Add(-time.Hour))
	if err := os.Chtimes(filepath.Join(directory, "issue.json"), accepted, accepted); err != nil {
		t.Fatal(err)
	}
	finish := startStopQueue(t, cfg, root, 10*time.Millisecond, io.Discard)
	waitFor(t, func() bool { return loadJobState(t, directory).Done })
	time.Sleep(100 * time.Millisecond)
	finish()
	if got := fixture.withPrefix("作業を始めます。"); len(got) != 1 {
		t.Fatalf("the stage cut under this engine was announced %d times: %q", len(got), fixture.all())
	}
}

// A record of kinds that cannot be read is set aside when the engine starts,
// said once, and the queue starts again as one without a record: nothing from
// before is posted, and nothing is held or logged again on every tick.
func TestAnUnreadableRecordOfKindsIsSetAsideOnce(t *testing.T) {
	cfg := watchConfiguration(t)
	cfg.Intake.Announce = true
	cfg.Intake.Statuses = &statusConfig{Delivered: 3}
	fixture := &noticeTracker{}
	fixture.install(t, alwaysChoose("done"))
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "jobs"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, noticeKindsFile), []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	delivered := time.Now().UTC().Add(-time.Hour)
	var directories []string
	for _, id := range []int{51, 52, 53} {
		directories = append(directories, deliveredJob(t, root, id, delivered))
	}
	var queueLog bytes.Buffer
	finish := startStopQueue(t, cfg, root, 10*time.Millisecond, &queueLog)
	waitFor(t, func() bool {
		for _, directory := range directories {
			if len(readNotices(t, directory).Notices) == 0 {
				return false
			}
		}
		return true
	})
	time.Sleep(100 * time.Millisecond)
	finish()
	if said := strings.Count(queueLog.String(), "notice kinds"); said != 1 {
		t.Fatalf("the unreadable record was said %d times:\n%s", said, queueLog.String())
	}
	if aside, err := os.ReadFile(filepath.Join(root, noticeKindsFile+".unreadable")); err != nil || string(aside) != "{not json" {
		t.Fatalf("the unreadable record was not set aside as it was: %q %v", aside, err)
	}
	if kinds, err := loadNoticeKinds(root); err != nil || !kinds.Since[modelsNotice].After(delivered) {
		t.Fatalf("the queue did not start again with a readable record: %+v %v", kinds, err)
	}
	if got := fixture.attempts(); got != 0 {
		t.Fatalf("requests delivered before were told %d times: %v", got, fixture.all())
	}
	for _, directory := range directories {
		if log := readNotices(t, directory).Notices; len(log) != 1 || !log[0].Predates {
			t.Fatalf("a request delivered before is not settled: %+v", log)
		}
	}
}

// A notice whose submission was never confirmed is looked for among the
// issue's comments by id and words alone. A comment whose author or place its
// record writes in another shape neither hides the notice nor keeps it from
// being posted.
func TestAnUnconfirmedNoticeIsFoundByItsWordsWhateverElseTheCommentsHold(t *testing.T) {
	odd := json.RawMessage(`{"id":950,"issueId":"51","projectId":null,"createdUser":"someone","content":"unrelated words"}`)
	for name, stored := range map[string]bool{"stored before": true, "never stored": false} {
		t.Run(name, func(t *testing.T) {
			cfg := watchConfiguration(t)
			_, directory := noticeJob(t, chain.State{History: []chain.Result{}})
			written := time.Now().UTC()
			saved, _ := json.Marshal(noticeLog{Notices: []noticeRecord{{Kind: resumeNotice, Text: resumeNoticeText, WrittenAt: written}}})
			if err := writeRuntimeFile(filepath.Join(directory, "notices.json"), saved); err != nil {
				t.Fatal(err)
			}
			rows := []json.RawMessage{odd}
			if stored {
				notice, _ := json.Marshal(map[string]any{"id": 951, "issueId": 51, "projectId": 17, "createdUser": map[string]any{"id": 99}, "content": resumeNoticeText})
				rows = append(rows, notice)
			}
			posts := 0
			useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodPost {
					posts++
					return selectionReply(r, 201, map[string]any{"id": 952, "content": resumeNoticeText}), nil
				}
				return selectionReply(r, 200, rows), nil
			})
			if err := requestNotices(cfg, sourceIssue{ID: 51, Key: "EXAMPLE-51"}, directory).flush(context.Background()); err != nil {
				t.Fatalf("the notice was not settled: %v", err)
			}
			log := readNotices(t, directory).Notices
			want, wantPosts := int64(952), 1
			if stored {
				want, wantPosts = 951, 0
			}
			if len(log) != 1 || log[0].PostedAt == nil || log[0].CommentID != want || posts != wantPosts {
				t.Fatalf("settled as %+v after %d posts", log, posts)
			}
		})
	}
}
