package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"ticket-runner/internal/chain"
)

func pausedFixture(t *testing.T, state chain.State) (config, sourceIssue, string, workLimitRecord) {
	t.Helper()
	cfg := watchConfiguration(t)
	cfg.Intake.StopUserIDs = []int64{77}
	_, directory := noticeJob(t, state)
	issue := sourceIssue{ID: 51, Key: "EXAMPLE-51"}
	issue.Creator.ID = 55
	record := workLimitRecord{Version: 1, Pauses: []pauseEpisode{{Reason: activeLimitPause,
		At: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), NoticeID: 900}}}
	if err := saveWorkLimit(directory, record); err != nil {
		t.Fatal(err)
	}
	return cfg, issue, directory, record
}

func TestPauseResumesOnlyOnAnAuthorizedLaterExplicitInstruction(t *testing.T) {
	cfg, issue, _, _ := pausedFixture(t, chain.State{})
	for _, test := range []struct {
		name       string
		id, author int64
		body       string
		want       bool
	}{
		{"requester", 901, 55, "再開\n同じ条件でお願いします。", true},
		{"operator", 901, 77, "\r\n 再開 \r\n", true},
		{"old comment", 899, 55, "再開", false},
		{"notice", 900, 55, "再開", false},
		{"other person", 901, 88, "再開", false},
		{"no author", 901, 0, "再開", false},
		{"ordinary reply", 901, 55, "調べています。まだ再開しないでください。", false},
		{"quoted", 901, 55, "> 再開", false},
		{"later mention", 901, 55, "操作の例\n再開", false},
		{"stop", 901, 55, "停止", false},
		{"empty", 901, 55, "", false},
		{"controller notice", 902, 55, "再開", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := resumeInstruction(cfg.source(), []json.RawMessage{issueComment(test.id, test.author, test.body)}, issue, cfg.Intake.StopUserIDs, 900, []int64{902})
			if err != nil || (got != nil) != test.want {
				t.Fatalf("resume=%s error=%v want=%t", got, err, test.want)
			}
		})
	}
	for _, raw := range []json.RawMessage{stopComment(52, 55, "再開"), []byte(`{"id":"broken"}`)} {
		if _, err := resumeInstruction(cfg.source(), []json.RawMessage{raw}, issue, nil, 900, nil); err == nil {
			t.Fatal("a wrong-issue or unreadable instruction was accepted")
		}
	}
	first := issueComment(901, 55, "再開\nfirst")
	got, err := resumeInstruction(cfg.source(), []json.RawMessage{issueComment(903, 55, "再開\nsecond"), first}, issue, nil, 900, nil)
	if err != nil || !reflect.DeepEqual(got, first) {
		t.Fatalf("the earliest instruction was not used: %s %v", got, err)
	}
}

func TestPauseResumeUsesNativeGitHubIdentityAndIssueBoundary(t *testing.T) {
	cfg := githubConfiguration(t)
	raw, _ := json.Marshal(githubIssueRow(cfg, 11, []string{"automation"}))
	issue, err := cfg.source().ReadIssue(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, author := range []int64{55, 77, 88} {
		resume, err := resumeInstruction(cfg.source(), []json.RawMessage{githubCommentRow(cfg, 11, 901, author, "再開")}, issue, []int64{77}, 900, nil)
		if err != nil || (resume != nil) != (author != 88) {
			t.Fatalf("native author %d: %s %v", author, resume, err)
		}
	}
	if _, err := resumeInstruction(cfg.source(), []json.RawMessage{githubCommentRow(cfg, 12, 901, 55, "再開")}, issue, nil, 900, nil); err == nil {
		t.Fatal("a different GitHub issue could release the pause")
	}
}

func TestDamagedPauseRecordsHoldWork(t *testing.T) {
	directory := t.TempDir()
	for _, data := range []string{
		`not json`, `null`, `{}`, `{"version":2}`, `{"version":1,"pauses":[{}]}`,
		`{"version":1,"pauses":[{"reason":"model says stop","at":"2026-01-02T03:04:05Z"}]}`,
		`{"version":1,"pauses":[{"reason":"active-limit","at":"2026-01-02T03:04:05Z","notice_id":-1}]}`,
		`{"version":1,"pauses":[{"reason":"active-limit","at":"2026-01-02T03:04:05Z","resume":{}}]}`,
	} {
		if err := writeRuntimeFile(filepath.Join(directory, workLimitFile), []byte(data)); err != nil {
			t.Fatal(err)
		}
		if _, err := readWorkLimit(directory); err == nil {
			t.Fatalf("damaged pause allowed work: %s", data)
		}
	}
	missing, err := readWorkLimit(t.TempDir())
	if err != nil || missing.held() {
		t.Fatalf("an older request was paused: %+v %v", missing, err)
	}
}

func TestPauseResumePreservesRecoveryAndDoesNotSatisfyAFailedStage(t *testing.T) {
	flow := &chain.Workflow{Stages: []chain.Stage{{Name: "repair", Kind: chain.ModelStage},
		{Name: "verify", Kind: chain.CommandStage, OnFailure: "repair"}}}
	before := chain.State{Workflow: flow, Step: "verify", Recovering: true, Waiting: true,
		Pending:      &chain.Assignment{Role: "verify", Instruction: "inspect existing external effects"},
		PendingSince: time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC),
		History: []chain.Result{{Role: "verify", Speaker: "check", Error: "the check failed"},
			{Role: "verify", Speaker: "runtime", Output: "The check did not exit 0."}}}
	cfg, issue, directory, record := pausedFixture(t, before)
	raw := issueComment(901, 55, "再開\n元の条件を保って続けてください。")
	if err := applyPauseResume(context.Background(), cfg, issue, directory, noticeRequest, &record, raw); err != nil {
		t.Fatal(err)
	}
	after := loadJobState(t, directory)
	if after.Step != before.Step || after.Recovering != before.Recovering || after.Waiting != before.Waiting ||
		!reflect.DeepEqual(after.Pending, before.Pending) || !after.PendingSince.Equal(before.PendingSince) || after.Done {
		t.Fatalf("a control instruction changed the work: %+v", after)
	}
	if len(after.History) != len(before.History)+1 || after.History[len(before.History)].Role != "" || after.History[len(before.History)].Speaker != "requester" {
		t.Fatalf("resume was not recorded outside the stages: %+v", after.History)
	}
	next, err := (chain.StageRouter{}).Next(context.Background(), after)
	if err != nil || next.Role != "repair" {
		t.Fatalf("the resume was taken for a passing check: next=%+v error=%v", next, err)
	}
	stored, err := readWorkLimit(directory)
	if err != nil || stored.held() {
		t.Fatalf("the authorized pause was not released: %+v %v", stored, err)
	}
	ids, err := pauseReplyIDs(cfg.source(), directory, issue, cfg.Intake.StopUserIDs, nil)
	if err != nil || !reflect.DeepEqual(ids, []int64{901}) {
		t.Fatalf("the consumed control comment was not retained: %v %v", ids, err)
	}
	if err := applyPauseResume(context.Background(), cfg, issue, directory, noticeRequest, &stored, raw); err != nil {
		t.Fatal(err)
	}
	if got := loadJobState(t, directory); !reflect.DeepEqual(got, after) {
		t.Fatal("reopening a released pause repeated its history entry")
	}
}

func TestPauseResumeFinishesBothInterruptedLocalWritesExactlyOnce(t *testing.T) {
	for _, historyWritten := range []bool{false, true} {
		t.Run(map[bool]string{false: "before history", true: "after history"}[historyWritten], func(t *testing.T) {
			cfg, issue, directory, record := pausedFixture(t, chain.State{Step: "work"})
			raw := issueComment(901, 77, "再開\nunchanged words")
			at := time.Date(2026, 1, 2, 3, 5, 0, 0, time.UTC)
			record.Pauses[0].Resume = &pauseResume{Comment: raw, RecordedAt: at, HistoryIndex: 0}
			if err := saveWorkLimit(directory, record); err != nil {
				t.Fatal(err)
			}
			if historyWritten {
				writeJobHistory(t, directory, chain.State{Step: "work", History: []chain.Result{{Speaker: "requester", Output: "再開\nunchanged words", FinishedAt: at}}})
			}
			loaded, err := readWorkLimit(directory)
			if err != nil {
				t.Fatal(err)
			}
			// The operation was accepted before the restart and before this
			// operator left. Finish its writes without accepting a new command.
			cfg.Intake.StopUserIDs = nil
			if err := applyPauseResume(context.Background(), cfg, issue, directory, noticeRequest, &loaded, nil); err != nil {
				t.Fatal(err)
			}
			if state := loadJobState(t, directory); len(state.History) != 1 || state.Step != "work" {
				t.Fatalf("interrupted resume was repeated: %+v", state)
			}
		})
	}
}

func TestPauseResumeRefusesAnUnexpectedHistoryOrTerminalRequest(t *testing.T) {
	for _, variant := range []string{"history changed", "delivered", "stopped"} {
		t.Run(variant, func(t *testing.T) {
			cfg, issue, directory, record := pausedFixture(t, chain.State{})
			raw := issueComment(901, 55, "再開")
			switch variant {
			case "history changed":
				record.Pauses[0].Resume = &pauseResume{Comment: raw, HistoryIndex: 0, RecordedAt: time.Now().UTC()}
				writeJobHistory(t, directory, chain.State{History: []chain.Result{{Speaker: "requester", Output: "unrelated"}}})
			case "delivered":
				writeJobHistory(t, directory, chain.State{Done: true})
			case "stopped":
				if err := writeRuntimeFile(filepath.Join(directory, "stop-request.json"), stopComment(51, 55, "停止")); err != nil {
					t.Fatal(err)
				}
			}
			before := loadJobState(t, directory)
			if err := applyPauseResume(context.Background(), cfg, issue, directory, noticeRequest, &record, raw); err == nil {
				t.Fatal("a request that cannot resume was released")
			}
			if !reflect.DeepEqual(before, loadJobState(t, directory)) {
				t.Fatal("a refused resume changed the history")
			}
		})
	}
}

func TestWorkPauseKeepsItsReasonAndEarlierControlReceipts(t *testing.T) {
	cfg, issue, directory, record := pausedFixture(t, chain.State{})
	if err := recordWorkPause(directory, hardExitPause, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, _ := readWorkLimit(directory)
	if len(got.Pauses) != 1 || got.Pauses[0].Reason != activeLimitPause {
		t.Fatal("an outstanding pause was replaced")
	}
	if err := applyPauseResume(context.Background(), cfg, issue, directory, noticeRequest, &record, issueComment(901, 55, "再開")); err != nil {
		t.Fatal(err)
	}
	if err := recordWorkPause(directory, hardExitPause, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := readWorkLimit(directory)
	if err != nil || len(got.Pauses) != 2 || !got.held() || got.Pauses[1].Reason != hardExitPause {
		t.Fatalf("the next episode was lost: %+v %v", got, err)
	}
	if text := pausedWorkText(got.Pauses[1], got.Clock); !strings.Contains(text, "強制終了が設定の回数上限に達した") || !strings.Contains(text, "再開") || !strings.Contains(text, "停止") {
		t.Fatalf("the pause lost its forced-exit reason or choices: %s", text)
	}
}

func pauseTransport(t *testing.T, remote *noticeTracker) {
	t.Helper()
	remote.install(t, func(r *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("unexpected call outside the fixture tracker: %s", r.URL.Host)
	})
}

func TestPauseUsesTheActualPostedNoticeAndKeepsALaterResume(t *testing.T) {
	cfg, issue, directory, record := pausedFixture(t, chain.State{Pending: &chain.Assignment{Role: "work"}})
	record.Pauses[0].NoticeID = 0
	if err := saveWorkLimit(directory, record); err != nil {
		t.Fatal(err)
	}
	// A resume predating the actual pause notice must not release it. The
	// POST lands, but its response is lost, exercising read-back recovery.
	remote := &noticeTracker{silent: true, rows: []json.RawMessage{issueComment(899, 55, "再開\nold words")}}
	pauseTransport(t, remote)
	hold := func() (bool, error) {
		return holdPausedRequest(context.Background(), cfg, issue, directory, noticeRequest, time.Second, func(string) {})
	}
	if held, err := hold(); err != nil || !held {
		t.Fatalf("pause=%t error=%v", held, err)
	}
	record, err := readWorkLimit(directory)
	if err != nil || record.Pauses[0].NoticeID != 901 || len(loadJobState(t, directory).History) != 0 {
		t.Fatalf("boundary was not the actual POST: %+v %v", record, err)
	}
	// Simulate losing the local boundary write after confirming the POST.
	// A reply arriving before that write is retried must remain eligible.
	record.Pauses[0].NoticeID = 0
	if err := saveWorkLimit(directory, record); err != nil {
		t.Fatal(err)
	}
	remote.rows = append(remote.rows, issueComment(902, 77, "再開\nnew words"), issueComment(903, 88, "unrelated later comment"))
	if held, err := hold(); err != nil || held {
		t.Fatalf("later authorized resume lost: held=%t error=%v", held, err)
	}
	cfg.Intake.StopUserIDs = nil
	if held, err := hold(); err != nil || held {
		t.Fatalf("removing an operator revoked an accepted resume: held=%t error=%v", held, err)
	}
	ids, err := pauseReplyIDs(cfg.source(), directory, issue, nil, remote.rows)
	if err != nil || !reflect.DeepEqual(ids, []int64{902}) {
		t.Fatalf("accepted resume lost its answer exclusion: ids=%v error=%v", ids, err)
	}
	if raw, err := resumeInstruction(cfg.source(), []json.RawMessage{issueComment(905, 77, "再開")}, issue, nil, 904, nil); err != nil || raw != nil {
		t.Fatalf("the removed operator could resume a new pause: %s %v", raw, err)
	}
	t.Logf("after operator removal: held=false control_comment_ids=%v new_resume=false", ids)
	state := loadJobState(t, directory)
	if len(state.History) != 1 || state.History[0].Output != "再開\nnew words" || state.Pending == nil || state.Done {
		t.Fatalf("release changed the pending action or repeated its words: %+v", state)
	}
	if remote.count(pausedWorkText(record.Pauses[0], record.Clock)) != 1 || remote.count(workResumeText) != 1 {
		t.Fatalf("the restart repeated a control notice: %v", remote.all())
	}
}

func TestPauseNoticesDistinguishOccurrencesWithoutChangingOlderDedup(t *testing.T) {
	cfg, issue, directory, _ := pausedFixture(t, chain.State{})
	remote := &noticeTracker{rows: []json.RawMessage{issueComment(900, 99, "fixed pause words")}}
	pauseTransport(t, remote)
	n := requestNotices(cfg, issue, directory)
	// The first event was sent but never locally confirmed. A new event
	// must settle that POST and then get its own receipt, not reuse it.
	if err := n.save(noticeLog{Notices: []noticeRecord{{Kind: workPauseNotice, Event: "1", Text: "fixed pause words", WrittenAt: time.Now().UTC()}}}); err != nil {
		t.Fatal(err)
	}
	id, err := n.sayPauseEvent(context.Background(), workPauseNotice, "2", "fixed pause words")
	if err != nil || id != 901 || remote.count("fixed pause words") != 1 {
		t.Fatalf("distinct pause got receipt %d error=%v posts=%v", id, err, remote.all())
	}
	again, err := n.sayPauseEvent(context.Background(), workPauseNotice, "2", "fixed pause words")
	if err != nil || again != id || remote.count("fixed pause words") != 1 {
		t.Fatal("an already posted occurrence was repeated")
	}
	for i := 0; i < 2; i++ {
		if err := n.post(context.Background(), resumeNotice, resumeNoticeText, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	if remote.count(resumeNoticeText) != 1 {
		t.Fatal("the new occurrence field changed interval-based notices")
	}
	log, err := n.load()
	if err != nil || len(log.Notices) != 3 || log.Notices[0].CommentID != 900 || log.Notices[2].Event != "" {
		t.Fatalf("old and new receipts changed: %+v %v", log, err)
	}
}

func TestFirstPauseControlNoticeSurvivesSlowReadback(t *testing.T) {
	for _, kind := range []string{workPauseNotice, workResumeNotice} {
		t.Run(kind, func(t *testing.T) {
			cfg, issue, directory, _ := pausedFixture(t, chain.State{})
			remote := &noticeTracker{rows: []json.RawMessage{issueComment(900, 99, "earlier pending notice")}}
			pauseTransport(t, remote)
			transport := http.DefaultTransport
			delayed := false
			useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodGet && !delayed {
					delayed = true
					// Ensure the pending notice read crosses a whole-second
					// boundary, without adding a clock seam to production.
					time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second)))
				}
				return transport.RoundTrip(r)
			})
			n := requestNotices(cfg, issue, directory)
			if err := n.save(noticeLog{Notices: []noticeRecord{{Kind: workPauseNotice, Event: "earlier", Text: "earlier pending notice", WrittenAt: time.Now().UTC()}}}); err != nil {
				t.Fatal(err)
			}
			id, err := n.sayPauseEvent(context.Background(), kind, "current", "current control notice")
			if err != nil || id != 901 || remote.count("current control notice") != 1 {
				t.Fatalf("first notice was deferred: id=%d error=%v posts=%v", id, err, remote.all())
			}
			t.Logf("first call: kind=%s comment=%d posts=1 after slow readback", kind, id)
		})
	}
}

func TestPauseStopWinsEvenWhenThePauseRecordIsDamaged(t *testing.T) {
	for _, damaged := range []bool{false, true} {
		t.Run(fmt.Sprint(damaged), func(t *testing.T) {
			cfg, issue, directory, _ := pausedFixture(t, chain.State{Waiting: true})
			if damaged {
				if err := writeRuntimeFile(filepath.Join(directory, workLimitFile), []byte("damaged")); err != nil {
					t.Fatal(err)
				}
			}
			stop := issueComment(902, 55, "停止\noriginal instruction")
			remote := &noticeTracker{rows: []json.RawMessage{issueComment(901, 55, "再開"), stop}}
			pauseTransport(t, remote)
			held, err := holdPausedRequest(context.Background(), cfg, issue, directory, noticeRequest, time.Second, func(string) {})
			if err != nil || !held || remote.attempts() != 0 || len(loadJobState(t, directory).History) != 0 {
				t.Fatalf("stop did not win: held=%t error=%v notices=%v", held, err, remote.all())
			}
			raw, err := os.ReadFile(filepath.Join(directory, "stop-request.json"))
			if err != nil || !reflect.DeepEqual(raw, []byte(stop)) {
				t.Fatalf("native stop not retained: %s %v", raw, err)
			}
			remote.rows = nil
			if stopped, err := savedStop(cfg.source(), directory, issue, cfg.Intake.StopUserIDs); err != nil || !stopped {
				t.Fatal("removing the remote comment could undo the recorded stop")
			}
		})
	}
}

func TestPauseCannotReleaseFromAnUnreadableOrConflictingReceipt(t *testing.T) {
	for _, variant := range []string{"damaged record", "wrong authority", "missing history", "notice not confirmed"} {
		t.Run(variant, func(t *testing.T) {
			cfg, issue, directory, record := pausedFixture(t, chain.State{})
			remote := &noticeTracker{}
			pauseTransport(t, remote)
			switch variant {
			case "damaged record":
				if err := writeRuntimeFile(filepath.Join(directory, workLimitFile), []byte("damaged")); err != nil {
					t.Fatal(err)
				}
			case "wrong authority", "missing history":
				author := int64(55)
				if variant == "wrong authority" {
					author = 88
				}
				record.Pauses[0].Resume = &pauseResume{Comment: issueComment(901, author, "再開"), Applied: true, RecordedAt: time.Now().UTC()}
				if err := saveWorkLimit(directory, record); err != nil {
					t.Fatal(err)
				}
			case "notice not confirmed":
				remote.postFail = 1
			}
			held, err := holdPausedRequest(context.Background(), cfg, issue, directory, noticeRequest, time.Second, func(string) {})
			if err == nil || !held || len(loadJobState(t, directory).History) != 0 {
				t.Fatalf("untrustworthy receipt allowed work: held=%t error=%v", held, err)
			}
		})
	}
}

func TestPauseResumeIsNotConsumedAgainAsAQuestionAnswer(t *testing.T) {
	cfg, issue, directory, record := pausedFixture(t, chain.State{Step: "ask_requester", Waiting: true})
	raw := issueComment(901, 77, "再開\nthese are control words, not the outstanding answer")
	if err := applyPauseResume(context.Background(), cfg, issue, directory, noticeRequest, &record, raw); err != nil {
		t.Fatal(err)
	}
	cfg.Intake.StopUserIDs = nil
	if err := writeRuntimeFile(filepath.Join(directory, "question.json"), []byte(`{"after":800}`)); err != nil {
		t.Fatal(err)
	}
	remote := &noticeTracker{rows: []json.RawMessage{raw}}
	pauseTransport(t, remote)
	resume, answered, err := resumeWaitingRequest(context.Background(), cfg, issue, directory, noticeRequest, loadJobState(t, directory), time.Second)
	if err != nil || resume || answered || !loadJobState(t, directory).Waiting {
		t.Fatalf("the control comment answered the question: resume=%t answered=%t err=%v", resume, answered, err)
	}
	remote.rows = append(remote.rows, issueComment(902, 55, "the actual answer"))
	resume, answered, err = resumeWaitingRequest(context.Background(), cfg, issue, directory, noticeRequest, loadJobState(t, directory), time.Second)
	state := loadJobState(t, directory)
	if err != nil || !resume || !answered || state.Waiting || len(state.History) != 2 || state.History[1].Output != "the actual answer" {
		t.Fatalf("the separate answer was not accepted: %+v %v", state, err)
	}
}

func TestPauseQueueDoesNotLaunchSavedOrDamagedPauses(t *testing.T) {
	for _, damaged := range []bool{false, true} {
		t.Run(fmt.Sprint(damaged), func(t *testing.T) {
			cfg, _, directory, record := pausedFixture(t, chain.State{Pending: &chain.Assignment{Role: "work"}, Recovering: true})
			record.Pauses[0].NoticeID = 0
			if err := saveWorkLimit(directory, record); err != nil {
				t.Fatal(err)
			}
			if damaged {
				if err := writeRuntimeFile(filepath.Join(directory, workLimitFile), []byte("damaged")); err != nil {
					t.Fatal(err)
				}
			}
			remote := &noticeTracker{}
			pauseTransport(t, remote)
			log := &lockedLog{}
			root := filepath.Dir(filepath.Dir(directory))
			finish := startStopQueue(t, cfg, root, 20*time.Millisecond, log)
			// Observe several complete queue ticks, not just the first reads
			// before a wrongly launched watcher could take its execution slot.
			waitFor(t, func() bool { return remote.readings() >= 12 })
			finish()
			state := loadJobState(t, directory)
			if log.starts() != 0 || state.Done || state.Pending == nil || !state.Recovering || len(state.History) != 0 {
				t.Fatalf("paused queue launched or changed work: starts=%d state=%+v", log.starts(), state)
			}
		})
	}
}

func TestPauseResumeReturnsThroughTheExistingInterruptedActionRecovery(t *testing.T) {
	cfg, issue, directory, record := pausedFixture(t, chain.State{
		Pending: &chain.Assignment{Role: "implement", Instruction: "check the existing external receipt before repeating work"},
		History: []chain.Result{{Role: "implement", Speaker: "worker", Output: "partial report before interruption"}},
	})
	record.Pauses[0].NoticeID = 0
	if err := saveWorkLimit(directory, record); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(directory, "workspace"), 0700); err != nil {
		t.Fatal(err)
	}
	receipt := filepath.Join(directory, "workspace", "external-receipt.json")
	if err := writeRuntimeFile(receipt, []byte(`{"observed":"already applied"}`)); err != nil {
		t.Fatal(err)
	}
	remote := &noticeTracker{}
	var mu sync.Mutex
	var routed []chain.State
	remote.install(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "watch-model.example" {
			return nil, fmt.Errorf("unexpected fixture request: %s", r.URL.Host)
		}
		var input struct{ State chain.State }
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			return nil, err
		}
		mu.Lock()
		routed = append(routed, input.State)
		mu.Unlock()
		return selectionReply(r, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": "done"}}}), nil
	})
	if held, err := holdPausedRequest(context.Background(), cfg, issue, directory, noticeRequest, time.Second, func(string) {}); err != nil || !held {
		t.Fatalf("initial pause not retained: %t %v", held, err)
	}
	remote.mu.Lock()
	remote.rows = append(remote.rows, issueComment(901, 55, "再開\nkeep the original scope"))
	remote.mu.Unlock()
	log := &lockedLog{}
	finish := startStopQueue(t, cfg, filepath.Dir(filepath.Dir(directory)), 20*time.Millisecond, log)
	waitFor(t, func() bool {
		state, err := savedHistory(directory)
		return err == nil && state.Done
	})
	finish()
	mu.Lock()
	defer mu.Unlock()
	if len(routed) != 1 || log.starts() != 1 {
		t.Fatalf("the release did not use one ordinary engine launch: routes=%d starts=%d", len(routed), log.starts())
	}
	state := routed[0]
	if !state.Recovering || state.Pending != nil || len(state.History) != 3 || state.History[0].Output != "partial report before interruption" ||
		state.History[1].Role != "" || state.History[1].Output != "再開\nkeep the original scope" ||
		!strings.Contains(state.History[2].Error, "the action may have taken effect") || !strings.Contains(state.History[2].Instruction, "existing external receipt") {
		t.Fatalf("release skipped the existing recovery warning or lost the original words: %+v", state)
	}
	if raw, err := os.ReadFile(receipt); err != nil || string(raw) != `{"observed":"already applied"}` {
		t.Fatalf("the external-effect receipt was changed: %s %v", raw, err)
	}
}

func TestPauseDuplicateControlsDoNotAnswerTheQuestion(t *testing.T) {
	for _, variant := range []string{"requester", "operator", "unauthorized", "other issue", "multiple pauses"} {
		t.Run(variant, func(t *testing.T) {
			cfg, issue, directory, record := pausedFixture(t, chain.State{Step: "ask_requester", Waiting: true})
			accepted := issueComment(901, 55, "再開\nkeep the original scope")
			if err := applyPauseResume(context.Background(), cfg, issue, directory, noticeRequest, &record, accepted); err != nil {
				t.Fatal(err)
			}
			if err := writeRuntimeFile(filepath.Join(directory, "question.json"), []byte(`{"after":800}`)); err != nil {
				t.Fatal(err)
			}
			author := int64(55)
			if variant == "operator" {
				author = 77
			} else if variant == "unauthorized" {
				author = 88
			}
			duplicate := issueComment(902, author, "再開\nresending because the response has not arrived")
			if variant == "other issue" {
				duplicate = stopComment(52, 55, "再開")
			}
			rows := []json.RawMessage{accepted, duplicate}
			wantHistory := 1
			if variant == "multiple pauses" {
				record.Pauses = append(record.Pauses, pauseEpisode{Reason: hardExitPause, At: time.Now().UTC(), NoticeID: 910})
				if err := saveWorkLimit(directory, record); err != nil {
					t.Fatal(err)
				}
				next := issueComment(911, 55, "再開\nnext interval")
				if err := applyPauseResume(context.Background(), cfg, issue, directory, noticeRequest, &record, next); err != nil {
					t.Fatal(err)
				}
				rows = append(rows, next, issueComment(912, 77, "再開\noperator resending"))
				wantHistory++
			}
			ids, controlsErr := pauseReplyIDs(cfg.source(), directory, issue, cfg.Intake.StopUserIDs, rows)
			if variant == "other issue" {
				if controlsErr == nil {
					t.Fatal("a control from a different issue was read as local")
				}
			} else {
				if controlsErr != nil {
					t.Fatal(controlsErr)
				}
				found := map[int64]bool{}
				for _, id := range ids {
					found[id] = true
				}
				if !found[901] || found[902] != (variant != "unauthorized") || (variant == "multiple pauses" && (!found[911] || !found[912])) {
					t.Errorf("authorized controls were not kept separate: %v", ids)
				}
			}
			remote := &noticeTracker{rows: rows}
			pauseTransport(t, remote)
			resume, answered, err := resumeWaitingRequest(context.Background(), cfg, issue, directory, noticeRequest, loadJobState(t, directory), time.Second)
			state := loadJobState(t, directory)
			if (err != nil) != (variant == "other issue") || resume || answered || !state.Waiting || len(state.History) != wantHistory {
				t.Fatalf("control retry answered the question: resume=%t answered=%t state=%+v err=%v", resume, answered, state, err)
			}
			if variant == "other issue" {
				return
			}
			remote.rows = append(remote.rows, issueComment(920, 55, "the actual answer to the question"))
			resume, answered, err = resumeWaitingRequest(context.Background(), cfg, issue, directory, noticeRequest, state, time.Second)
			state = loadJobState(t, directory)
			if err != nil || !resume || !answered || state.Waiting || len(state.History) != wantHistory+1 || state.History[wantHistory].Output != "the actual answer to the question" {
				t.Fatalf("the separate answer was lost: %+v %v", state, err)
			}
		})
	}
}

func TestPauseControlExclusionLeavesOlderOrdinaryAnswersAlone(t *testing.T) {
	for _, variant := range []string{"no pause record", "before first pause notice"} {
		t.Run(variant, func(t *testing.T) {
			cfg, issue, directory, record := pausedFixture(t, chain.State{Step: "ask_requester", Waiting: true})
			wantHistory := 1
			if variant == "no pause record" {
				if err := os.Remove(filepath.Join(directory, workLimitFile)); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := applyPauseResume(context.Background(), cfg, issue, directory, noticeRequest, &record, issueComment(901, 55, "再開\nthis is the selected control")); err != nil {
					t.Fatal(err)
				}
				wantHistory++
			}
			if err := writeRuntimeFile(filepath.Join(directory, "question.json"), []byte(`{"after":800}`)); err != nil {
				t.Fatal(err)
			}
			words := "再開\nthis ordinary answer was written before any pause notice"
			remote := &noticeTracker{rows: []json.RawMessage{issueComment(899, 55, words)}}
			pauseTransport(t, remote)
			resume, answered, err := resumeWaitingRequest(context.Background(), cfg, issue, directory, noticeRequest, loadJobState(t, directory), time.Second)
			state := loadJobState(t, directory)
			if err != nil || !resume || !answered || state.Waiting || len(state.History) != wantHistory || state.History[wantHistory-1].Output != words {
				t.Fatalf("the existing answer semantics changed: %+v %v", state, err)
			}
		})
	}
}
