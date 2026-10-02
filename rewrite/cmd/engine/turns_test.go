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
	root, directory := noticeJob(t, chain.State{})
	queueRanSince(t, root, cfg, time.Now().Add(-time.Hour))
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
	root, directory := noticeJob(t, chain.State{})
	queueRanSince(t, root, cfg, time.Now().Add(-time.Hour))
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

// A launch that could not choose a model at all names none, and no later
// launch names one for a stage that has already had its sentence. The stage
// still began, which is the news, so the operator's sentence goes out as
// written rather than the stage passing in silence.
func TestAStageWhoseSelectionFailedIsAnnouncedWithoutAModel(t *testing.T) {
	cfg := announcingStagesConfig(t)
	fixture := &noticeTracker{}
	fixture.install(t, alwaysChoose("done"))
	root, directory := noticeJob(t, chain.State{})
	queueRanSince(t, root, cfg, time.Now().Add(-time.Hour))
	observe := func(string) {}
	// Nothing ran, so there is no live copy: the record of the launch that
	// could not choose is all there is of the stage. It carries its times,
	// as every record the engine writes does.
	failed := time.Now().UTC()
	writeJobHistory(t, directory, chain.State{History: []chain.Result{
		{Role: "work", Speaker: "worker", Error: "Selecting a current model: catalog HTTP 503: actual reason", StartedAt: failed, FinishedAt: failed},
	}})
	announceStages(context.Background(), cfg, announcedIssue(), directory, observe)
	if got := fixture.all(); len(got) != 1 || got[0] != "作業を始めます。" {
		t.Fatalf("the stage was announced as %v", got)
	}
	// A later launch of it does choose one. The stage has had its sentence.
	if err := recordChosen(filepath.Join(directory, "run"), "work", "maker/late"); err != nil {
		t.Fatal(err)
	}
	beginStage(t, directory, "work-worker")
	announceStages(context.Background(), cfg, announcedIssue(), directory, observe)
	if got := fixture.all(); len(got) != 1 || got[0] != "作業を始めます。" {
		t.Fatalf("the stage was announced a second time or renamed: %v", got)
	}
}

// While a process of the stage is running, a model it has not written down
// yet is at most a tick away, so the sentence waits for it instead of
// spending the stage's one comment on a launch it cannot name.
func TestAStageStillChoosingWaitsForItsModel(t *testing.T) {
	cfg := announcingStagesConfig(t)
	cfg.Roles[0].Processes = append(cfg.Roles[0].Processes, chain.Process{Name: "peer", Command: []string{"/bin/true"}})
	fixture := &noticeTracker{}
	fixture.install(t, alwaysChoose("done"))
	root, directory := noticeJob(t, chain.State{})
	queueRanSince(t, root, cfg, time.Now().Add(-time.Hour))
	observe := func(string) {}
	// The stage's model-less process is already running; its peer has not
	// come back from the selection yet.
	beginStage(t, directory, "work-peer")
	announceStages(context.Background(), cfg, announcedIssue(), directory, observe)
	if got := fixture.all(); len(got) != 0 {
		t.Fatalf("the stage was announced before its model was known: %v", got)
	}
	if err := recordChosen(filepath.Join(directory, "run"), "work", "maker/chosen"); err != nil {
		t.Fatal(err)
	}
	announceStages(context.Background(), cfg, announcedIssue(), directory, observe)
	if got := fixture.all(); len(got) != 1 || got[0] != "作業を始めます。 (モデル: maker/chosen)" {
		t.Fatalf("the stage was announced as %v", got)
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
	// The stage the work came back to keeps its own line, so its last launch
	// is printed above the launch of another stage that ran before that
	// return; the heading says the list is read that way.
	const want = "使ったモデル (工程ごと、起動順):\n" +
		"- 要件確定: maker/one (27 秒)\n" +
		"- 作業: maker/two (11 分 0 秒) — 再実行: maker/three (5 分 18 秒) (失敗) — 再実行: maker/four (1 時間 0 分 5 秒)\n" +
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
	// The request was delivered while the queue's engines posted the list.
	queueRanSince(t, root, cfg, base)
	since, _ := time.Parse(time.RFC3339, cfg.Intake.CreatedSince)
	const want = "使ったモデル (工程ごと、起動順):\n- 実装: maker/one (32 秒)"
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

// declaringConfig is announcingStagesConfig with the models declared, a first
// stage that has no sentence of its own, as the requirements stage of the
// shipped run has none, and a review stage whose two processes each choose a
// model.
func declaringConfig(t *testing.T) config {
	t.Helper()
	cfg := announcingStagesConfig(t)
	cfg.Intake.DeclareModels = true
	cfg.Roles = append(cfg.Roles,
		chain.Role{Name: "elicit", Purpose: "settle the requirements", Processes: []chain.Process{{Name: "requirements", ModelEnv: "MODEL", Command: []string{"/bin/true"}}}},
		chain.Role{Name: "review", Purpose: "review the work", Processes: []chain.Process{
			{Name: "reviewer-a", ModelEnv: "MODEL", Command: []string{"/bin/true"}},
			{Name: "reviewer-b", ModelEnv: "MODEL", Command: []string{"/bin/true"}},
		}})
	cfg.Workflow.Stages = append([]chain.Stage{{Name: "elicit", Kind: chain.ModelStage}}, cfg.Workflow.Stages...)
	cfg.Workflow.Stages = append(cfg.Workflow.Stages, chain.Stage{Name: "review", Kind: chain.ModelStage})
	return cfg
}

// launched writes down one launch's choices as the engine does, one per
// process that chose a model.
func launched(t *testing.T, directory, role string, launch int, models ...string) {
	t.Helper()
	for _, model := range models {
		if err := recordLaunch(filepath.Join(directory, "run"), role, model, launch); err != nil {
			t.Fatal(err)
		}
	}
}

// declaringJob is an accepted request on a queue whose engines declare the
// models of cfg, for a run that began a moment ago.
func declaringJob(t *testing.T, cfg config) (string, time.Time) {
	t.Helper()
	root, directory := noticeJob(t, chain.State{})
	queueRanSince(t, root, cfg, time.Now().Add(-time.Hour))
	return directory, time.Now().UTC().Add(-time.Second)
}

func TestTheFirstLaunchOfAStageWithoutASentenceIsDeclaredOnce(t *testing.T) {
	cfg := declaringConfig(t)
	fixture := &noticeTracker{}
	fixture.install(t, alwaysChoose("done"))
	directory, began := declaringJob(t, cfg)
	observe := func(string) {}
	declareModels(context.Background(), cfg, announcedIssue(), directory, began, observe)
	if got := fixture.all(); len(got) != 0 {
		t.Fatalf("a stage that chose nothing was declared: %v", got)
	}
	launched(t, directory, "elicit", 0, "maker/one")
	for range 3 {
		declareModels(context.Background(), cfg, announcedIssue(), directory, began, observe)
	}
	if got := fixture.all(); len(got) != 1 || got[0] != "要件確定を始めます。選定モデル: maker/one" {
		t.Fatalf("the first launch was declared as %v", got)
	}
	if log := readNotices(t, directory).Notices; len(log) != 1 || log[0].Kind != declarePrefix+"elicit" || log[0].Models != "maker/one" || log[0].PostedAt == nil {
		t.Fatalf("the record does not keep the declared model: %+v", log)
	}
}

// A stage launched again after a send-back or a failed launch is declared
// again only when its models differ from those it was last declared with.
func TestALaunchAgainIsDeclaredOnlyWhenItsModelsChange(t *testing.T) {
	cfg := declaringConfig(t)
	fixture := &noticeTracker{}
	fixture.install(t, alwaysChoose("done"))
	directory, began := declaringJob(t, cfg)
	observe := func(string) {}
	tick := func() {
		for range 2 {
			declareModels(context.Background(), cfg, announcedIssue(), directory, began, observe)
		}
	}
	launched(t, directory, "elicit", 0, "maker/one")
	tick()
	launched(t, directory, "elicit", 2, "maker/one")
	tick()
	if got := fixture.all(); len(got) != 1 {
		t.Fatalf("a launch again on the same model was declared: %v", got)
	}
	launched(t, directory, "elicit", 4, "maker/two")
	tick()
	launched(t, directory, "elicit", 6, "maker/one")
	tick()
	want := []string{
		"要件確定を始めます。選定モデル: maker/one",
		"要件確定をやり直します。選定モデル: maker/two",
		"要件確定をやり直します。選定モデル: maker/one",
	}
	if got := fixture.all(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the launches were declared as %q", got)
	}
}

// A stage that announces itself says its first launch in the operator's
// sentence alone; a later launch on other models is declared like any other.
func TestAnAnnouncedStageIsNotDeclaredTwiceForOneLaunch(t *testing.T) {
	cfg := declaringConfig(t)
	fixture := &noticeTracker{}
	fixture.install(t, alwaysChoose("done"))
	directory, began := declaringJob(t, cfg)
	observe := func(string) {}
	launched(t, directory, "work", 0, "maker/first")
	beginStage(t, directory, "work-worker")
	// The sentence goes out first, on the same tick or a later one.
	declareModels(context.Background(), cfg, announcedIssue(), directory, began, observe)
	if got := fixture.all(); len(got) != 0 {
		t.Fatalf("the launch was declared before its sentence: %v", got)
	}
	for range 2 {
		announceStages(context.Background(), cfg, announcedIssue(), directory, observe)
		declareModels(context.Background(), cfg, announcedIssue(), directory, began, observe)
	}
	launched(t, directory, "work", 2, "maker/first")
	declareModels(context.Background(), cfg, announcedIssue(), directory, began, observe)
	launched(t, directory, "work", 4, "maker/second")
	for range 2 {
		declareModels(context.Background(), cfg, announcedIssue(), directory, began, observe)
	}
	want := []string{"作業を始めます。 (モデル: maker/first)", "作業をやり直します。選定モデル: maker/second"}
	if got := fixture.all(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the announced stage was told %q", got)
	}
}

// One launch whose processes choose several models is declared once with all
// of them, once they have all chosen or once it has returned, as a launch
// does whose selection failed for one process. A fixed model running both is
// named once. A stage that chooses nothing, and a failed selection, say
// nothing.
func TestALaunchOfSeveralModelsIsDeclaredOnceWithAllOfThem(t *testing.T) {
	cfg := declaringConfig(t)
	fixture := &noticeTracker{}
	fixture.install(t, alwaysChoose("done"))
	directory, began := declaringJob(t, cfg)
	observe := func(string) {}
	tick := func() { declareModels(context.Background(), cfg, announcedIssue(), directory, began, observe) }
	launched(t, directory, "review", 0, "maker/a")
	tick()
	if got := fixture.all(); len(got) != 0 {
		t.Fatalf("a launch still choosing was declared: %v", got)
	}
	launched(t, directory, "review", 0, "maker/b")
	tick()
	tick()
	// The next launch chose for one process; the other's selection failed.
	launched(t, directory, "review", 3, "maker/c")
	tick()
	if got := fixture.all(); len(got) != 1 {
		t.Fatalf("a launch still choosing was declared: %v", got)
	}
	failed := time.Now().UTC()
	writeJobHistory(t, directory, chain.State{History: []chain.Result{
		{Role: "verify", Speaker: "build", StartedAt: failed, FinishedAt: failed},
		{Role: "review", Speaker: "reviewer-a", Model: "maker/a", StartedAt: failed, FinishedAt: failed},
		{Role: "review", Speaker: "reviewer-b", Model: "maker/b", StartedAt: failed, FinishedAt: failed},
		{Role: "review", Speaker: "reviewer-a", Model: "maker/c", StartedAt: failed, FinishedAt: failed},
		{Role: "review", Speaker: "reviewer-b", Error: "Selecting a current model: catalog HTTP 503", StartedAt: failed, FinishedAt: failed},
	}})
	tick()
	launched(t, directory, "review", 7, "maker/d", "maker/d")
	tick()
	want := []string{
		"レビューを始めます。選定モデル: maker/a、maker/b",
		"レビューをやり直します。選定モデル: maker/c",
		"レビューをやり直します。選定モデル: maker/d",
	}
	if got := fixture.all(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the review launches were declared as %q", got)
	}
}

// A launch that chose its model before declarations existed on the queue is
// settled without a word, its record keeping the model, so no later tick
// weighs it again and a later launch on the same model is no news either.
func TestALaunchFromBeforeDeclarationsExistedIsSettledWithoutAWord(t *testing.T) {
	cfg := declaringConfig(t)
	fixture := &noticeTracker{}
	fixture.install(t, alwaysChoose("done"))
	root, directory := noticeJob(t, chain.State{})
	earlier := time.Now().UTC().Add(-time.Hour)
	chosen, _ := json.Marshal(chosenRecord{First: map[string]string{"elicit": "maker/one"}, Launches: map[string][]chosenLaunch{
		"elicit": {{Launch: 0, At: earlier, Models: []string{"maker/one"}}},
	}})
	if err := writeRuntimeFile(chosenPath(filepath.Join(directory, "run")), chosen); err != nil {
		t.Fatal(err)
	}
	// Declarations begin on this queue now, with a run that began before the
	// launch it meets.
	queueRanSince(t, root, cfg, time.Now())
	observe := func(string) {}
	for range 2 {
		declareModels(context.Background(), cfg, announcedIssue(), directory, earlier.Add(-time.Minute), observe)
	}
	if got := fixture.all(); len(got) != 0 {
		t.Fatalf("a launch from before declarations existed was declared: %v", got)
	}
	if log := readNotices(t, directory).Notices; len(log) != 1 || !log[0].Predates || log[0].Models != "maker/one" || log[0].PostedAt != nil {
		t.Fatalf("the record does not settle the launch: %+v", log)
	}
	launched(t, directory, "elicit", 2, "maker/one")
	declareModels(context.Background(), cfg, announcedIssue(), directory, earlier.Add(-time.Minute), observe)
	launched(t, directory, "elicit", 4, "maker/two")
	declareModels(context.Background(), cfg, announcedIssue(), directory, earlier.Add(-time.Minute), observe)
	if got := fixture.all(); len(got) != 1 || got[0] != "要件確定をやり直します。選定モデル: maker/two" {
		t.Fatalf("the launches after it were declared as %v", got)
	}
}

// A launch of an earlier run of the request, here one that returned before any
// tick saw it and before the request waited for its requester, is not
// declared afterwards: it would be news about the past. It leaves no record,
// so the next launch, in the run under way, is the one declared, as a launch
// again since the stage has run before.
func TestALaunchFromAnEarlierRunIsNotDeclaredAfterwards(t *testing.T) {
	cfg := declaringConfig(t)
	fixture := &noticeTracker{}
	fixture.install(t, alwaysChoose("done"))
	directory, _ := declaringJob(t, cfg)
	asked := time.Now().UTC().Add(-10 * time.Minute)
	resumed := asked.Add(5 * time.Minute)
	keep := func(launches ...chosenLaunch) {
		t.Helper()
		chosen, _ := json.Marshal(chosenRecord{First: map[string]string{"elicit": "maker/one"}, Launches: map[string][]chosenLaunch{"elicit": launches}})
		if err := writeRuntimeFile(chosenPath(filepath.Join(directory, "run")), chosen); err != nil {
			t.Fatal(err)
		}
	}
	observe := func(string) {}
	first := chosenLaunch{Launch: 0, At: asked, Models: []string{"maker/one"}}
	keep(first)
	declareModels(context.Background(), cfg, announcedIssue(), directory, resumed, observe)
	if got, log := fixture.all(), readNotices(t, directory).Notices; len(got) != 0 || len(log) != 0 {
		t.Fatalf("a launch of an earlier run was declared or recorded: %v %+v", got, log)
	}
	keep(first, chosenLaunch{Launch: 4, At: resumed.Add(time.Second), Models: []string{"maker/one"}})
	declareModels(context.Background(), cfg, announcedIssue(), directory, resumed, observe)
	if got := fixture.all(); len(got) != 1 || got[0] != "要件確定をやり直します。選定モデル: maker/one" {
		t.Fatalf("the launch of the run under way was declared as %v", got)
	}
}

// Without the setting the runtime says and keeps exactly what it did before:
// the operator's sentence, no line of its own, and only the first model of
// each stage.
func TestWithoutDeclaringNothingChanges(t *testing.T) {
	cfg := declaringConfig(t)
	cfg.Intake.DeclareModels = false
	fixture := &noticeTracker{}
	fixture.install(t, alwaysChoose("done"))
	directory, began := declaringJob(t, cfg)
	run := filepath.Join(directory, "run")
	observe := func(string) {}
	for _, choice := range [][2]string{{"elicit", "maker/one"}, {"work", "maker/first"}, {"work", "maker/second"}, {"review", "maker/a"}, {"review", "maker/b"}} {
		if err := recordChosen(run, choice[0], choice[1]); err != nil {
			t.Fatal(err)
		}
		beginStage(t, directory, choice[0]+"-worker")
		announceStages(context.Background(), cfg, announcedIssue(), directory, observe)
		declareModels(context.Background(), cfg, announcedIssue(), directory, began, observe)
	}
	if got := fixture.all(); len(got) != 1 || got[0] != "作業を始めます。 (モデル: maker/first)" {
		t.Fatalf("without the setting the requester was told %q", got)
	}
	raw, err := os.ReadFile(chosenPath(run))
	if err != nil || string(raw) != `{"first":{"elicit":"maker/one","review":"maker/a","work":"maker/first"}}` {
		t.Fatalf("without the setting the choices were kept as %s (%v)", raw, err)
	}
	if raw, err := os.ReadFile(filepath.Join(directory, "notices.json")); err != nil || strings.Contains(string(raw), `"models"`) {
		t.Fatalf("without the setting the record changed: %s (%v)", raw, err)
	}
}

// Through the queue: the engine writes each choice down as it makes it, the
// watcher's tick declares it, and a restart that takes the stage up again on
// the same model says nothing more. A request delivered long ago is not told
// about a launch it never heard of. Without the setting nothing is said and
// the engine keeps only the first model, as it always did.
func TestTheQueueDeclaresEachLaunchOnceAcrossARestart(t *testing.T) {
	for _, declaring := range []bool{true, false} {
		t.Run(fmt.Sprintf("declare_models=%v", declaring), func(t *testing.T) {
			cfg := watchConfiguration(t)
			cfg.Intake.DeclareModels = declaring
			cfg.Router.Mode = "stages"
			cfg.ModelSelection = &selectionConfig{Fixed: "maker/configured"}
			cfg.Roles = []chain.Role{
				// The first launch holds until the queue is stopped under it;
				// the one after the restart returns at once.
				{Name: "work", Purpose: "do the work", Processes: []chain.Process{{Name: "worker", ModelEnv: "MODEL",
					Command: []string{"/bin/sh", "-c", `if [ -e worked ]; then exit 0; fi; touch worked; exec sleep 30`}}}},
				{Name: "verify", Purpose: "run the checks", Processes: []chain.Process{{Name: "build", Command: []string{"/bin/sh", "-c", "exit 0"}}}},
			}
			cfg.Workflow = &chain.Workflow{Stages: []chain.Stage{{Name: "work", Kind: chain.ModelStage}, {Name: "verify", Kind: chain.CommandStage, OnFailure: "work"}}}
			fixture := &noticeTracker{}
			fixture.install(t, alwaysChoose("done"))
			root, directory := noticeJob(t, chain.State{})
			old := deliveredJob(t, root, 52, time.Now().Add(-26*time.Hour))
			undeclared, _ := json.Marshal(chosenRecord{First: map[string]string{"work": "maker/old"}, Launches: map[string][]chosenLaunch{
				"work": {{Launch: 0, At: time.Now().UTC().Add(-27 * time.Hour), Models: []string{"maker/old"}}},
			}})
			if err := writeRuntimeFile(chosenPath(filepath.Join(old, "run")), undeclared); err != nil {
				t.Fatal(err)
			}
			const line = "作業を始めます。選定モデル: maker/configured"
			var queueLog bytes.Buffer
			defer func() {
				if t.Failed() {
					t.Logf("queue log:\n%s", queueLog.String())
					for i, result := range loadJobState(t, directory).History {
						if i < 8 {
							t.Logf("record %d: %s %s error=%q output=%.300q", i, result.Role, result.Speaker, result.Error, result.Output)
						}
					}
				}
			}()
			finish := startStopQueue(t, cfg, root, 10*time.Millisecond, &queueLog)
			waitFor(t, func() bool {
				_, err := os.Stat(filepath.Join(directory, "workspace", "worked"))
				return err == nil && (!declaring || fixture.count(line) > 0)
			})
			time.Sleep(100 * time.Millisecond)
			finish()
			finish = startStopQueue(t, cfg, root, 10*time.Millisecond, &queueLog)
			waitFor(t, func() bool { return loadJobState(t, directory).Done })
			time.Sleep(100 * time.Millisecond)
			finish()
			var declared []string
			for _, comment := range fixture.all() {
				if strings.Contains(comment, "選定モデル") {
					declared = append(declared, comment)
				}
			}
			if declaring && (len(declared) != 1 || declared[0] != line) {
				t.Fatalf("the launches were declared as %q", declared)
			}
			if !declaring && len(declared) != 0 {
				t.Fatalf("without the setting the requester was told %q", declared)
			}
			if !declaring {
				if raw, err := os.ReadFile(chosenPath(filepath.Join(directory, "run"))); err != nil || string(raw) != `{"first":{"work":"maker/configured"}}` {
					t.Fatalf("without the setting the choices were kept as %s (%v)", raw, err)
				}
			}
			if log := readNotices(t, old).Notices; len(log) != 0 {
				t.Fatalf("a request delivered long ago was told about its launch: %+v", log)
			}
		})
	}
}
