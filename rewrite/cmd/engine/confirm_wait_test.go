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
	"strings"
	"sync"
	"testing"
	"time"

	"ticket-runner/internal/chain"
	"ticket-runner/internal/tracker"
)

// waitTracker is the assigned issue as the controller reaches it while a
// request waits: it reads the comments and posts its own fixed notices.
type waitTracker struct {
	mu       sync.Mutex
	comments []json.RawMessage
	posts    []string
	next     int64
}

func (w *waitTracker) use(t *testing.T) {
	t.Helper()
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		w.mu.Lock()
		defer w.mu.Unlock()
		if r.URL.Host == "watch-tracker.example" && r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues") {
			return selectionReply(r, 200, []any{}), nil // Nothing new to accept.
		}
		if r.URL.Host != "watch-tracker.example" || !strings.HasSuffix(r.URL.Path, "/comments") {
			return nil, fmt.Errorf("unexpected fixture call %s %s", r.Method, r.URL)
		}
		if r.Method == http.MethodPost {
			if err := r.ParseForm(); err != nil {
				return nil, err
			}
			w.next++
			row := issueComment(w.next, 900, r.Form.Get("content"))
			w.posts = append(w.posts, r.Form.Get("content"))
			w.comments = append(w.comments, row)
			return selectionReply(r, 201, row), nil
		}
		return selectionReply(r, 200, append([]json.RawMessage{}, w.comments...)), nil
	})
}

func (w *waitTracker) add(id, user int64, body string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.comments = append(w.comments, issueComment(id, user, body))
	if id >= w.next {
		w.next = id
	}
}

func (w *waitTracker) posted() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.posts...)
}

const waitingRequestText = "Show the item count on the list screen."

// confirmingConfiguration is the question configuration of an ordered run
// that confirms the change before delivery.
func confirmingConfiguration(t *testing.T) config {
	t.Helper()
	cfg := questionConfiguration(t)
	cfg.Workflow = &chain.Workflow{Stages: []chain.Stage{{Name: "elicit", Kind: chain.ModelStage},
		{Name: "confirm_change", Kind: chain.ModelStage, Confirm: true}, {Name: "deliver", Kind: chain.CommandStage, OnFailure: "elicit"}}}
	return cfg
}

// waitingRequest lays out one accepted request that waits for the requester,
// as the queue keeps it: its history, the recorded question and the queue's
// record of when each kind of notice began.
func waitingRequest(t *testing.T, cfg config, unseen bool, since, kindsBegan time.Time) (string, chain.State) {
	t.Helper()
	return waitingRequestFor(t, cfg, waitingRequestText, unseen, since, kindsBegan)
}

func waitingRequestFor(t *testing.T, cfg config, request string, unseen bool, since, kindsBegan time.Time) (string, chain.State) {
	t.Helper()
	root := t.TempDir()
	directory := filepath.Join(root, "jobs", "51")
	store, err := chain.Open(filepath.Join(directory, "run"), request)
	if err != nil {
		t.Fatal(err)
	}
	state := chain.State{Request: request, Step: "ask_requester", Waiting: true, WaitingWithoutQuestion: unseen,
		History: []chain.Result{
			// The request was settled long before it reached the question.
			{Role: "elicit", Speaker: "requirements", Output: "Settled requirements.", FinishedAt: since.Add(-20 * time.Hour)},
			{Role: "confirm_change", Speaker: "reader", Output: "The list screen gains a count line.", FinishedAt: since.Add(-time.Minute)},
			{Role: "ask_requester", Speaker: "questioner", Output: "Posted the question.", FinishedAt: since},
		}}
	if unseen {
		state.History[2].Error = "exit status 1"
		state.History = append(state.History, chain.Result{Role: "router", Speaker: "runtime", Output: "the question was not seen posted", FinishedAt: since})
	}
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
	store.Close()
	if err := writeRuntimeFile(filepath.Join(directory, "question.json"), []byte(`{"after":700}`)); err != nil {
		t.Fatal(err)
	}
	if err := startNoticeKinds(root, cfg, kindsBegan, func(string) {}); err != nil {
		t.Fatal(err)
	}
	return directory, state
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// The question chosen before delivery was not seen posted, so the request
// waits for a comment that nobody asked for. The controller says so once, in
// its own fixed words; the notice is not an answer, even where the
// controller's own account filed the issue, and only the requester's words
// end the wait.
func TestAWaitWithoutAQuestionIsSaidOnceAndOnlyTheRequesterEndsIt(t *testing.T) {
	for _, creator := range []int64{55, 900} {
		t.Run(fmt.Sprintf("filed-by-%d", creator), func(t *testing.T) {
			cfg := confirmingConfiguration(t)
			cfg.Intake.StatusPage = "https://status.example.invalid/requests"
			issue := sourceIssue{ID: 51, Key: "EXAMPLE-51", Creator: tracker.Account{ID: creator}}
			now := time.Now().UTC()
			directory, state := waitingRequest(t, cfg, true, now.Add(-time.Minute), now.Add(-time.Hour))
			fixture := &waitTracker{}
			fixture.add(700, 55, "起票のときの補足。")
			fixture.use(t)
			boundary := readFile(t, filepath.Join(directory, "question.json"))
			var said []string
			for tick := 0; tick < 3; tick++ {
				noticeWaitingRequest(context.Background(), cfg, issue, directory, state, func(message string) { said = append(said, message) })
				resume, answered, err := resumeWaitingRequest(context.Background(), cfg, issue, directory, waitingRequestText, state, time.Second)
				if err != nil || resume || answered {
					t.Fatalf("tick %d: resume=%v answered=%v err=%v", tick, resume, answered, err)
				}
			}
			want := unseenQuestionText + "\n変更の内容はこちらで見られます: https://status.example.invalid/requests/51"
			if posts := fixture.posted(); len(posts) != 1 || posts[0] != want {
				t.Fatalf("posted %q, want the notice once: %q (log %v)", posts, want, said)
			}
			if !bytes.Equal(readFile(t, filepath.Join(directory, "question.json")), boundary) {
				t.Fatal("the notice moved the recorded question boundary")
			}
			// The requester's own words, unchanged, are the answer.
			fixture.add(fixture.next+1, creator, "このままで納品してください。")
			resume, answered, err := resumeWaitingRequest(context.Background(), cfg, issue, directory, waitingRequestText, state, time.Second)
			if err != nil || !resume || !answered {
				t.Fatalf("the requester's comment did not end the wait: resume=%v answered=%v err=%v", resume, answered, err)
			}
			saved, err := savedHistory(directory)
			if err != nil {
				t.Fatal(err)
			}
			last := saved.History[len(saved.History)-1]
			if saved.Waiting || saved.WaitingWithoutQuestion || last.Speaker != "requester" || last.Output != "このままで納品してください。" {
				t.Fatalf("the answer was not carried in unchanged, or the wait was not cleared: waiting=%v unseen=%v last=%+v",
					saved.Waiting, saved.WaitingWithoutQuestion, last)
			}
		})
	}
}

// The queue says it on its own ticks: a request held waiting without a seen
// question gets the notice once, however many ticks pass, and is not resumed
// by it.
func TestTheQueueSaysAWaitWithoutAQuestionOnce(t *testing.T) {
	cfg := confirmingConfiguration(t)
	raw, err := json.Marshal(watchedIssue(51, waitingRequestText, "2026-01-03T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	request, err := cfg.source().RequestText(raw)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	directory, _ := waitingRequestFor(t, cfg, request, true, now.Add(-time.Minute), now.Add(-time.Hour))
	if err := writeRuntimeFile(filepath.Join(directory, "issue.json"), raw); err != nil {
		t.Fatal(err)
	}
	fixture := &waitTracker{}
	fixture.add(700, 55, "起票のときの補足。")
	fixture.use(t)
	finish := startStopQueue(t, cfg, filepath.Dir(filepath.Dir(directory)), 20*time.Millisecond, io.Discard)
	waitFor(t, func() bool { return len(fixture.posted()) > 0 })
	time.Sleep(300 * time.Millisecond) // More than a dozen further ticks.
	finish()
	if posts := fixture.posted(); len(posts) != 1 || posts[0] != unseenQuestionText {
		t.Fatalf("the queue posted %q", posts)
	}
	if state, err := savedHistory(directory); err != nil || !state.Waiting || !state.WaitingWithoutQuestion {
		t.Fatalf("the notice resumed the request: %+v %v", state, err)
	}
}

// A request that keeps waiting is reminded once per configured interval. Ticks
// and restarts within an interval repeat nothing, the reminder is never read
// as an answer, the recorded boundary does not move, and passing an interval
// neither approves nor ends anything: a stop still stops.
func TestAWaitingQuestionIsSaidAgainOncePerInterval(t *testing.T) {
	original := reminderClock
	t.Cleanup(func() { reminderClock = original })
	for _, creator := range []int64{55, 900} {
		t.Run(fmt.Sprintf("filed-by-%d", creator), func(t *testing.T) {
			cfg := questionConfiguration(t)
			cfg.Intake.QuestionReminderMinutes = 1440
			issue := sourceIssue{ID: 51, Key: "EXAMPLE-51", Creator: tracker.Account{ID: creator}}
			began := time.Now().UTC().Truncate(time.Second)
			directory, state := waitingRequest(t, cfg, false, began, began.Add(-time.Hour))
			fixture := &waitTracker{}
			fixture.add(700, 900, "質問: 一覧画面に件数の行を足してよいですか。")
			fixture.use(t)
			boundary := readFile(t, filepath.Join(directory, "question.json"))
			history := readFile(t, filepath.Join(directory, "run", "history.json"))
			tick := func(at time.Duration) {
				t.Helper()
				reminderClock = func() time.Time { return began.Add(at) }
				noticeWaitingRequest(context.Background(), cfg, issue, directory, state, func(message string) { t.Log(message) })
				resume, answered, err := resumeWaitingRequest(context.Background(), cfg, issue, directory, waitingRequestText, state, time.Second)
				if err != nil || resume || answered {
					t.Fatalf("at %v: resume=%v answered=%v err=%v", at, resume, answered, err)
				}
			}
			tick(23 * time.Hour)
			if posts := fixture.posted(); len(posts) != 0 {
				t.Fatalf("reminded before the interval: %q", posts)
			}
			tick(24*time.Hour + time.Minute)
			tick(30 * time.Hour)
			tick(47 * time.Hour)
			tick(48*time.Hour + time.Minute)
			want := []string{reminderText(24 * time.Hour), reminderText(48 * time.Hour)}
			if posts := fixture.posted(); len(posts) != 2 || posts[0] != want[0] || posts[1] != want[1] {
				t.Fatalf("posted %q, want %q", posts, want)
			}
			if !strings.Contains(want[0], "（待ち始めてから 24 時間）") || !strings.Contains(want[1], "（待ち始めてから 48 時間）") {
				t.Fatalf("the reminders do not say how long the request has waited: %q", want)
			}
			if !bytes.Equal(readFile(t, filepath.Join(directory, "question.json")), boundary) ||
				!bytes.Equal(readFile(t, filepath.Join(directory, "run", "history.json")), history) {
				t.Fatal("a reminder changed the recorded question boundary or the history")
			}
			// A stop is still a stop, never an answer.
			fixture.add(fixture.next+1, creator, "停止")
			resume, answered, err := resumeWaitingRequest(context.Background(), cfg, issue, directory, waitingRequestText, state, time.Second)
			if err != nil || !resume || answered {
				t.Fatalf("the stop was not handed on as a stop: resume=%v answered=%v err=%v", resume, answered, err)
			}
			if saved, err := savedHistory(directory); err != nil || !saved.Waiting || saved.Done {
				t.Fatalf("a stop answered the question or ended the request: %+v %v", saved, err)
			}
		})
	}
}

// Turning reminders on does not send one to every request already waiting:
// an interval that ended before the queue's engines began posting reminders
// is not said, and the next one is.
func TestARemindersFirstIntervalAfterItBeganIsTheFirstSaid(t *testing.T) {
	original := reminderClock
	t.Cleanup(func() { reminderClock = original })
	cfg := questionConfiguration(t)
	cfg.Intake.QuestionReminderMinutes = 1440
	issue := sourceIssue{ID: 51, Key: "EXAMPLE-51", Creator: tracker.Account{ID: 55}}
	began := time.Now().UTC().Add(-30 * time.Hour).Truncate(time.Second)
	directory, state := waitingRequest(t, cfg, false, began, began.Add(30*time.Hour))
	fixture := &waitTracker{}
	fixture.add(700, 900, "質問")
	fixture.use(t)
	for _, at := range []time.Duration{30*time.Hour + time.Minute, 47 * time.Hour, 48*time.Hour + time.Minute} {
		reminderClock = func() time.Time { return began.Add(at) }
		noticeWaitingRequest(context.Background(), cfg, issue, directory, state, func(message string) { t.Log(message) })
		if at < 48*time.Hour && len(fixture.posted()) != 0 {
			t.Fatalf("an interval that ended before reminders began was said at %v: %q", at, fixture.posted())
		}
	}
	if posts := fixture.posted(); len(posts) != 1 || posts[0] != reminderText(48*time.Hour) {
		t.Fatalf("posted %q", posts)
	}
}

func TestReminderSettingsAreChecked(t *testing.T) {
	cfg := questionConfiguration(t)
	for _, tc := range []struct {
		name     string
		minutes  int
		question string
		ok       bool
	}{
		{"absent", 0, "ask_requester", true},
		{"set", 1440, "ask_requester", true},
		{"negative", -1, "ask_requester", false},
		{"too-long", 1 << 62, "ask_requester", false},
		{"without-a-question-role", 1440, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			check := cfg
			intake := *cfg.Intake
			intake.QuestionReminderMinutes, intake.QuestionRole = tc.minutes, tc.question
			check.Intake = &intake
			if err := validateNotices(check); (err == nil) != tc.ok {
				t.Fatalf("validateNotices = %v", err)
			}
		})
	}
	kinds := func(check config) map[string]bool {
		named := map[string]bool{}
		for _, kind := range postableKinds(check) {
			named[kind] = true
		}
		return named
	}
	reminding := cfg
	intake := *cfg.Intake
	intake.QuestionReminderMinutes = 1440
	reminding.Intake = &intake
	if named := kinds(reminding); !named[reminderNotice] || named[unseenQuestionNotice] {
		t.Fatalf("an engine that reminds, with no stage that confirms, posts %v", named)
	}
	// Without a stage that confirms the change, nothing about the queue's
	// record of notice kinds changes.
	if named := kinds(cfg); named[reminderNotice] || named[unseenQuestionNotice] {
		t.Fatalf("kinds without reminders or a confirmation: %v", named)
	}
	if named := kinds(confirmingConfiguration(t)); !named[unseenQuestionNotice] {
		t.Fatalf("an engine whose run confirms the change does not post its notice: %v", named)
	}
}
