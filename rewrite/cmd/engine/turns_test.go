package main

import (
	"context"
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

	"ticket-runner/internal/chain"
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
	// Give the other request a few ticks to ask for the slot and be refused;
	// stopped too early, it would find the slot free at its first attempt
	// and, rightly, have nothing to announce.
	time.Sleep(300 * time.Millisecond)
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

func TestARefusedTurnIsAskedAgainWithGrowingSpacingAndNeverGivenUp(t *testing.T) {
	now := time.Now()
	turnClock = func() time.Time { return now }
	defer func() { turnClock = time.Now }()
	var record turnRecord
	if !record.due("category") {
		t.Fatal("a turn never refused was not due")
	}
	if !record.refuse("category", "not now") || record.refuse("category", "not now") || !record.refuse("category", "still not") {
		t.Fatal("a reason's novelty was misjudged")
	}
	if record.Refused["category"] != 3 || record.due("category") {
		t.Fatalf("after three refusals: %+v due=%v", record, record.due("category"))
	}
	for refused, want := range map[int]time.Duration{1: time.Minute, 2: 2 * time.Minute, 4: 8 * time.Minute, 7: time.Hour, 40: time.Hour} {
		if got := retryDelay(refused); got != want {
			t.Fatalf("delay after %d refusals: %v", refused, got)
		}
	}
	now = now.Add(4 * time.Minute)
	if !record.due("category") {
		t.Fatal("the third refusal's four minutes had passed and the turn was still not due")
	}
	for i := 0; i < 50; i++ {
		record.refuse("category", "never")
		now = now.Add(2 * time.Hour)
	}
	if !record.due("category") || record.Refused["category"] != 53 {
		t.Fatalf("a turn refused many times was given up or lost count: %+v", record.Refused)
	}
}

// announcingStagesConfig is an ordered run whose two stages both announce
// themselves: the first launches a model, the second is a command.
func announcingStagesConfig(t *testing.T) config {
	t.Helper()
	cfg := watchConfiguration(t)
	cfg.Intake.Announce = true
	cfg.Router.Mode = "stages"
	cfg.ModelSelection = &selectionConfig{Fixed: "maker/configured"}
	cfg.Roles = []chain.Role{
		{Name: "work", Purpose: "do the work", Processes: []chain.Process{{Name: "worker", ModelEnv: "MODEL", Command: []string{"/bin/true"}}}},
		{Name: "verify", Purpose: "run the checks", Processes: []chain.Process{{Name: "build", Command: []string{"/bin/true"}}}},
	}
	cfg.Workflow = &chain.Workflow{Stages: []chain.Stage{
		{Name: "work", Kind: chain.ModelStage, Announce: "作業を始めます。"},
		{Name: "verify", Kind: chain.CommandStage, OnFailure: "work", Announce: "検証を始めます。"},
	}}
	return cfg
}

// beginStage leaves behind the live copy a running process writes, which is
// how the queue sees that a stage has begun.
func beginStage(t *testing.T, directory, name string) {
	t.Helper()
	live := filepath.Join(directory, "live")
	if err := os.MkdirAll(live, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(live, name+".stdout"), nil, 0600); err != nil {
		t.Fatal(err)
	}
}

func announcedIssue() sourceIssue {
	return sourceIssue{ID: 51, ProjectID: 17, Key: "EXAMPLE-51"}
}

// A stage's one sentence says which model took the work on. The work can come
// back to that stage on another model; the sentence is still said once, and it
// names the model the stage began on.
func TestAStageSaysWhichModelBeganItAndSaysItOnce(t *testing.T) {
	cfg := announcingStagesConfig(t)
	fixture := &noticeTracker{}
	fixture.install(t, alwaysChoose("done"))
	_, directory := noticeJob(t, chain.State{})
	run := filepath.Join(directory, "run")
	observe := func(string) {}
	announceStages(context.Background(), cfg, announcedIssue(), directory, observe)
	if got := fixture.all(); len(got) != 0 {
		t.Fatalf("a stage that had not begun was announced: %v", got)
	}
	// The launch writes down its model before starting its child, and the
	// live copy appears once that child runs.
	if err := recordChosen(run, "work", "maker/first"); err != nil {
		t.Fatal(err)
	}
	beginStage(t, directory, "work-worker")
	announceStages(context.Background(), cfg, announcedIssue(), directory, observe)
	const said = "作業を始めます。 (モデル: maker/first)"
	if got := fixture.all(); len(got) != 1 || got[0] != said {
		t.Fatalf("the stage was announced as %v", got)
	}
	// The work comes back to the same stage on another model.
	if err := recordChosen(run, "work", "maker/second"); err != nil {
		t.Fatal(err)
	}
	writeJobHistory(t, directory, chain.State{History: []chain.Result{
		{Role: "work", Speaker: "worker", Model: "maker/first"},
		{Role: "work", Speaker: "worker", Model: "maker/second"},
	}})
	announceStages(context.Background(), cfg, announcedIssue(), directory, observe)
	announceStages(context.Background(), cfg, announcedIssue(), directory, observe)
	if got := fixture.all(); len(got) != 1 || got[0] != said {
		t.Fatalf("the relaunched stage was announced again or renamed: %v", got)
	}
	if model := firstModel(directory, "work"); model != "maker/first" {
		t.Fatalf("the stage's first model was overwritten by a later launch: %q", model)
	}
}

// A stage that launches no model has no model to name, and neither has a
// runtime that chooses none. Both say the operator's sentence as written.
func TestAStageWithoutAChosenModelIsAnnouncedAsTheOperatorWroteIt(t *testing.T) {
	cfg := announcingStagesConfig(t)
	fixture := &noticeTracker{}
	fixture.install(t, alwaysChoose("done"))
	_, directory := noticeJob(t, chain.State{})
	observe := func(string) {}
	beginStage(t, directory, "verify-build")
	announceStages(context.Background(), cfg, announcedIssue(), directory, observe)
	if got := fixture.all(); len(got) != 1 || got[0] != "検証を始めます。" {
		t.Fatalf("the command stage was announced as %v", got)
	}
	// The model stage has begun too, and this runtime selects no model at all.
	cfg.ModelSelection = nil
	beginStage(t, directory, "work-worker")
	announceStages(context.Background(), cfg, announcedIssue(), directory, observe)
	if got := fixture.all(); len(got) != 2 || got[1] != "作業を始めます。" {
		t.Fatalf("a stage with no model to name was announced as %v", got)
	}
}

// At the end the requester reads every launch that used a model, in the order
// they ran: the stage, the model and how long it took, with a stage the work
// came back to on one line and a launch that failed marked as one.
func TestTheDeliveredRequestListsEveryLaunchThatUsedAModel(t *testing.T) {
	base := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	spent := func(from, to time.Duration) (time.Time, time.Time) { return base.Add(from), base.Add(to) }
	launch := func(role, speaker, model, failure string, from, to time.Duration) chain.Result {
		started, finished := spent(from, to)
		return chain.Result{Role: role, Speaker: speaker, Model: model, Error: failure, StartedAt: started, FinishedAt: finished}
	}
	state := chain.State{History: []chain.Result{
		launch("elicit", "requirements", "maker/one", "", 0, 27*time.Second),
		{Role: "elicit", Speaker: "runtime", Output: "Runtime record for stage elicit, written by the engine from what it observed.\nProcess requirements exited 0.\n"},
		launch("work", "worker", "maker/two", "", time.Minute, 12*time.Minute),
		{Role: "work", Speaker: "runtime", Output: "Runtime record for stage work, written by the engine from what it observed.\nProcess worker exited 0.\n"},
		// A command stage launches no model and is not in the list.
		launch("verify", "project-tests", "", "exit status 1", 13*time.Minute, 14*time.Minute),
		{Role: "verify", Speaker: "runtime", Output: "Runtime record for stage verify, written by the engine from what it observed.\nProcess project-tests did not exit 0: exit status 1\n"},
		launch("work", "worker", "maker/three", "exit status 2", 15*time.Minute, 20*time.Minute+18*time.Second),
		{Role: "work", Speaker: "runtime", Output: "Runtime record for stage work, written by the engine from what it observed.\nProcess worker did not exit 0: exit status 2\n"},
		launch("work", "worker", "maker/four", "", 21*time.Minute, 81*time.Minute+5*time.Second),
		launch("shout", "crier", "maker/five", "", 82*time.Minute, 82*time.Minute),
	}}
	const want = "使ったモデル (起動順):\n" +
		"- 要件確定: maker/one (27 秒)\n" +
		"- 作業: maker/two (11 分 0 秒) — 差し戻し後: maker/three (5 分 18 秒) (失敗) — 差し戻し後: maker/four (1 時間 0 分 5 秒)\n" +
		"- shout: maker/five (0 秒)"
	if got := modelsUsedText(state); got != want {
		t.Fatalf("the list reads:\n%s\nwanted:\n%s", got, want)
	}
	if got := modelsUsedText(chain.State{History: []chain.Result{launch("verify", "project-tests", "", "", 0, time.Second)}}); got != "" {
		t.Fatalf("a request that launched no model still got a list: %q", got)
	}
}

// The list is posted when the request is delivered, once, and a restart that
// meets the same delivered request does not post it again.
func TestTheModelsUsedArePostedOnceAcrossARestart(t *testing.T) {
	cfg := watchConfiguration(t)
	cfg.Intake.Announce = true
	cfg.Intake.Statuses = &statusConfig{Delivered: 3}
	fixture := &noticeTracker{}
	fixture.install(t, alwaysChoose("done"))
	base := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	root, directory := noticeJob(t, chain.State{Done: true, History: []chain.Result{
		{Role: "implement", Speaker: "worker", Model: "maker/one", StartedAt: base, FinishedAt: base.Add(32 * time.Second)},
	}})
	since, _ := time.Parse(time.RFC3339, cfg.Intake.CreatedSince)
	const want = "使ったモデル (起動順):\n- 実装: maker/one (32 秒)"
	status := filepath.Join(directory, "status.json")
	tick := func() {
		ctx, cancel := context.WithCancel(context.Background())
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			_ = pollRequests(ctx, cfg, filepath.Join(root, "jobs"), since, 10*time.Millisecond, 1, &serialLog{writer: io.Discard})
		}()
		// The delivered turns run together; the status is the first of them.
		waitFor(t, func() bool { _, err := os.Stat(status); return err == nil })
		time.Sleep(80 * time.Millisecond)
		cancel()
		<-finished
	}
	tick()
	if got := fixture.count(want); got != 1 {
		t.Fatalf("the delivered request said which models it used %d times: %v", got, fixture.all())
	}
	if err := os.Remove(status); err != nil {
		t.Fatal(err)
	}
	tick()
	if got := fixture.count(want); got != 1 {
		t.Fatalf("a restart posted the list again: %v", fixture.all())
	}
	// The record is written before a comment is submitted, so one record is
	// also one attempt.
	if log := readNotices(t, directory).Notices; len(log) != 1 || log[0].Kind != modelsNotice || log[0].PostedAt == nil {
		t.Fatalf("the record does not show one confirmed list: %+v", log)
	}
	// Not even hours later. The list is said once for the request, like its
	// acceptance, and not once per window like the conditions that pass.
	posted := time.Now().UTC().Add(-7 * time.Hour)
	aged := noticeLog{Notices: []noticeRecord{{Kind: modelsNotice, Text: want, WrittenAt: posted, PostedAt: &posted}}}
	if noticeDue(aged, modelsNotice, time.Now().UTC()) {
		t.Fatal("the list fell due again hours after it was posted")
	}
}

// A runtime that announces nothing says none of this either.
func TestNothingIsSaidAboutModelsWhenTheRuntimeDoesNotAnnounce(t *testing.T) {
	cfg := announcingStagesConfig(t)
	cfg.Intake.Announce = false
	fixture := &noticeTracker{}
	fixture.install(t, alwaysChoose("done"))
	base := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	state := chain.State{Done: true, History: []chain.Result{
		{Role: "work", Speaker: "worker", Model: "maker/one", StartedAt: base, FinishedAt: base.Add(time.Minute)},
	}}
	_, directory := noticeJob(t, state)
	observe := func(string) {}
	if err := recordChosen(filepath.Join(directory, "run"), "work", "maker/one"); err != nil {
		t.Fatal(err)
	}
	beginStage(t, directory, "work-worker")
	announceStages(context.Background(), cfg, announcedIssue(), directory, observe)
	modelsTurn(context.Background(), cfg, announcedIssue(), directory, state, observe)
	if got := fixture.all(); len(got) != 0 {
		t.Fatalf("a runtime that announces nothing posted %v", got)
	}
	if notices := readNotices(t, directory).Notices; len(notices) != 0 {
		t.Fatalf("a runtime that announces nothing recorded %+v", notices)
	}
}
