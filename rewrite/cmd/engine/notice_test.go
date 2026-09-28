package main

import (
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
}

func (n *noticeTracker) install(t *testing.T, extra roundTripFunc) {
	t.Helper()
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "watch-tracker.example" {
			return extra(r)
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
