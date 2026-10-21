package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"ticket-runner/internal/chain"
)

// A notice the tracker could not tell apart from another new note by the
// engine's account is recorded with that note (Also), and the note is held
// for the engine's own on every path that reads people's words: it is not the
// question a role posted, not the requester's answer, and not a resume, even
// when it reads like one. Each case runs the engine's own path, not a helper
// of it. (Each fails when the notices' ids leave Also out.)
func TestANoteHeldWithANoticeIsNobodysQuestionAnswerOrResume(t *testing.T) {
	now := time.Now().UTC()
	held := func(t *testing.T, cfg config, issue sourceIssue, directory string, extra ...noticeRecord) {
		t.Helper()
		log := noticeLog{Notices: append(extra, noticeRecord{Kind: stallNotice, Text: "依頼はまだ終わっていません。",
			WrittenAt: now, PostedAt: &now, CommentID: 902, Also: []int64{901}})}
		if err := requestNotices(cfg, issue, directory).save(log); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("the question a role posted", func(t *testing.T) {
		cfg, issue, directory, _ := pausedFixture(t, chain.State{Step: "ask_requester"})
		held(t, cfg, issue, directory)
		path := filepath.Join(directory, "run", "question-post.json")
		if err := writeRuntimeFile(path, []byte(`{"after":null,"before":700}`)); err != nil {
			t.Fatal(err)
		}
		remote := &noticeTracker{rows: []json.RawMessage{issueComment(901, 99, "a note of the engine's account"), issueComment(902, 99, "依頼はまだ終わっていません。")}}
		pauseTransport(t, remote)
		q := questionProcesses{cfg: cfg, key: "EXAMPLE-51", path: path}
		observation, err := q.posted(context.Background())
		if err != nil || observation.Posted {
			t.Fatalf("a note held with a notice was taken for the role's question: %+v %v", observation, err)
		}
	})

	t.Run("the requester's answer", func(t *testing.T) {
		cfg, issue, directory, _ := pausedFixture(t, chain.State{Step: "ask_requester", Waiting: true})
		cfg.Intake.StopUserIDs = nil
		held(t, cfg, issue, directory)
		remote := &noticeTracker{rows: []json.RawMessage{issueComment(901, 55, "(a) でお願いします。"), issueComment(902, 99, "依頼はまだ終わっていません。")}}
		pauseTransport(t, remote)
		q := questionProcesses{cfg: cfg, key: "EXAMPLE-51", path: filepath.Join(directory, "run", "question-post.json")}
		if answer, err := q.reply(context.Background(), 700); err != nil || answer != "" {
			t.Fatalf("a note held with a notice was taken for the answer: %q %v", answer, err)
		}
		if err := writeRuntimeFile(filepath.Join(directory, "question.json"), []byte(`{"after":700}`)); err != nil {
			t.Fatal(err)
		}
		resume, answered, err := resumeWaitingRequest(context.Background(), cfg, issue, directory, noticeRequest, loadJobState(t, directory), time.Second)
		if err != nil || resume || answered {
			t.Fatalf("a note held with a notice ended the wait: resume=%t answered=%t err=%v", resume, answered, err)
		}
		remote.mu.Lock()
		remote.rows = append(remote.rows, issueComment(903, 55, "the actual answer"))
		remote.mu.Unlock()
		resume, answered, err = resumeWaitingRequest(context.Background(), cfg, issue, directory, noticeRequest, loadJobState(t, directory), time.Second)
		state := loadJobState(t, directory)
		if err != nil || !resume || !answered || state.History[len(state.History)-1].Output != "the actual answer" {
			t.Fatalf("the requester's own answer was not taken: resume=%t answered=%t err=%v", resume, answered, err)
		}
	})

	t.Run("a resume", func(t *testing.T) {
		cfg, issue, directory, _ := pausedFixture(t, chain.State{Pending: &chain.Assignment{Role: "work"}})
		held(t, cfg, issue, directory, noticeRecord{Kind: workPauseNotice, Event: "1", Text: "paused", WrittenAt: now, PostedAt: &now, CommentID: 900})
		remote := &noticeTracker{rows: []json.RawMessage{issueComment(900, 99, "paused"), issueComment(901, 55, "再開"),
			issueComment(902, 99, "依頼はまだ終わっていません。")}}
		pauseTransport(t, remote)
		hold := func() (bool, error) {
			return holdPausedRequest(context.Background(), cfg, issue, directory, noticeRequest, time.Second, func(string) {})
		}
		if paused, err := hold(); err != nil || !paused {
			t.Fatalf("a note held with a notice resumed the request: paused=%t err=%v", paused, err)
		}
		remote.mu.Lock()
		remote.rows = append(remote.rows, issueComment(903, 55, "再開"))
		remote.mu.Unlock()
		if paused, err := hold(); err != nil || paused {
			t.Fatalf("the requester's own resume was not taken: paused=%t err=%v", paused, err)
		}
		if _, err := os.Stat(filepath.Join(directory, workLimitFile)); err != nil {
			t.Fatal(err)
		}
	})
}
