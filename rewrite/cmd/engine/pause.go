package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"time"

	"ticket-runner/internal/chain"
	"ticket-runner/internal/tracker"
)

const workLimitFile = "work-limit.json"

// These are reasons recorded by the controller, never classifications of a
// model's output. The clock that creates these pauses is a separate caller.
const (
	activeLimitPause = "active-limit"
	unmeasuredPause  = "unmeasured-active"
	workPauseNotice  = "work-paused"
	workResumeNotice = "work-resume-accepted"
)

const workResumeText = "再開の指示を受け取り、一時停止の解除を記録しました。作業の開始や完了を意味するものではありません。回答待ちの質問があれば別のコメントで回答してください。実行枠と利用枠が使えるようになってから、保存した条件で続けます。"

// workLimitRecord belongs to one accepted request. Keeping earlier episodes
// also keeps their consumed control comments out of ordinary question answers.
// A missing file is an older request, not an instruction to pause it.
type workLimitRecord struct {
	Version int            `json:"version"`
	Pauses  []pauseEpisode `json:"pauses,omitempty"`
}

type pauseEpisode struct {
	Reason   string       `json:"reason"`
	At       time.Time    `json:"at"`
	NoticeID int64        `json:"notice_id,omitempty"`
	Resume   *pauseResume `json:"resume,omitempty"`
}

// The native instruction and the history position are saved before its words
// join the history. A restart can finish that local write without consuming the
// comment twice or treating an unrelated history change as its own write.
type pauseResume struct {
	Comment      json.RawMessage `json:"comment"`
	HistoryIndex int             `json:"history_index"`
	RecordedAt   time.Time       `json:"recorded_at"`
	Applied      bool            `json:"applied,omitempty"`
}

var errPauseRecord = errors.New("the recorded pause is unreadable; the request remains held")

func readWorkLimit(directory string) (workLimitRecord, error) {
	raw, err := os.ReadFile(filepath.Join(directory, workLimitFile))
	if errors.Is(err, os.ErrNotExist) {
		return workLimitRecord{Version: 1}, nil
	}
	if err != nil {
		return workLimitRecord{}, err
	}
	var record workLimitRecord
	if json.Unmarshal(raw, &record) != nil || record.Version != 1 {
		return workLimitRecord{}, errPauseRecord
	}
	for i, pause := range record.Pauses {
		if (pause.Reason != activeLimitPause && pause.Reason != unmeasuredPause) || pause.At.IsZero() || pause.NoticeID < 0 {
			return workLimitRecord{}, errPauseRecord
		}
		if i+1 < len(record.Pauses) && (pause.Resume == nil || !pause.Resume.Applied) {
			return workLimitRecord{}, errPauseRecord
		}
		if resume := pause.Resume; resume != nil && (pause.NoticeID <= 0 || resume.HistoryIndex < 0 || resume.RecordedAt.IsZero() || !json.Valid(resume.Comment)) {
			return workLimitRecord{}, errPauseRecord
		}
	}
	return record, nil
}

func saveWorkLimit(directory string, record workLimitRecord) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return writeRuntimeFile(filepath.Join(directory, workLimitFile), raw)
}

func (r workLimitRecord) held() bool {
	if len(r.Pauses) == 0 {
		return false
	}
	resume := r.Pauses[len(r.Pauses)-1].Resume
	return resume == nil || !resume.Applied
}

// A pause is not a native stop. Save it before asking a running child to exit;
// its caller still owns cancellation and release of the execution slot.
func recordWorkPause(directory, reason string, at time.Time) error {
	if (reason != activeLimitPause && reason != unmeasuredPause) || at.IsZero() {
		return errors.New("a work pause needs its controller reason and time")
	}
	record, err := readWorkLimit(directory)
	if err != nil || record.held() {
		return err
	}
	record.Pauses = append(record.Pauses, pauseEpisode{Reason: reason, At: at.UTC()})
	return saveWorkLimit(directory, record)
}

func pauseReason(reason string) string {
	if reason == activeLimitPause {
		return "今回任された区間の実稼働時間が、設定された上限に達したためです。"
	}
	return "前回の実行が途中で終了し、その区間の実稼働時間を確定できないためです。上限に達したと確認したわけではありません。"
}

func pausedWorkText(pause pauseEpisode) string {
	return "この依頼の自動処理を一時停止しています。" + pauseReason(pause.Reason) +
		"依頼は未完了です。中断前の操作が既に反映されている場合があり、取り消してはいません。\n" +
		"続ける場合は、最初の空でない行を「再開」として新しいコメントを投稿してください。必要な補足は次の行に書けます。保存した条件で、外部の状態を確かめてから続けます。" +
		"対応を待つ場合は、このまま待てます。依頼を取りやめる場合は、最初の空でない行を「停止」としてください。"
}

func authorizedPauseComment(comment tracker.Comment, issue sourceIssue, operators []int64) bool {
	authorized := comment.Author.ID > 0 && comment.Author.ID == issue.Creator.ID
	for _, id := range operators {
		authorized = authorized || id > 0 && comment.Author.ID == id
	}
	return authorized
}

// resumeInstruction recognizes a controller operation, not an ordinary reply.
// Its boundary is the pause notice's actual stored comment, not the latest
// comment observed on the next poll. Stop is checked by the caller first.
func resumeInstruction(source tracker.Tracker, rows []json.RawMessage, issue sourceIssue, operators []int64, after int64, excluded []int64) (json.RawMessage, error) {
	var found json.RawMessage
	var lowest int64
	for _, raw := range rows {
		comment, err := source.ReadComment(raw, issue)
		if err != nil || !comment.OnIssue {
			return nil, errors.New("pause controls could not be read for the assigned issue")
		}
		if comment.ID <= after || firstInstructionLine(comment.Body) != "再開" || !authorizedPauseComment(comment, issue, operators) {
			continue
		}
		omitted := false
		for _, id := range excluded {
			omitted = omitted || comment.ID == id
		}
		if !omitted && (lowest == 0 || comment.ID < lowest) {
			found, lowest = raw, comment.ID
		}
	}
	return found, nil
}

func pauseReplyIDs(source tracker.Tracker, directory string, issue sourceIssue, operators []int64, rows []json.RawMessage) ([]int64, error) {
	record, err := readWorkLimit(directory)
	if err != nil {
		return nil, err
	}
	var ids []int64
	var firstNotice int64
	for _, pause := range record.Pauses {
		if pause.NoticeID > 0 && (firstNotice == 0 || pause.NoticeID < firstNotice) {
			firstNotice = pause.NoticeID
		}
		if pause.Resume == nil {
			continue
		}
		comment, err := source.ReadComment(pause.Resume.Comment, issue)
		// Authorization was checked when this control instruction was saved.
		// A later operator-list change does not undo an accepted operation.
		if err != nil || !comment.OnIssue || comment.ID <= pause.NoticeID || firstInstructionLine(comment.Body) != "再開" {
			return nil, errPauseRecord
		}
		ids = append(ids, comment.ID)
	}
	// A repeated resume is still a control operation, not an answer to the
	// original question. Older requests and words before the first actual
	// pause notice retain their ordinary answer semantics. Only the selected
	// instruction joins history; this also excludes later retries of it.
	if firstNotice == 0 {
		return ids, nil
	}
	for _, raw := range rows {
		comment, err := source.ReadComment(raw, issue)
		if err != nil || !comment.OnIssue {
			return nil, errors.New("pause controls could not be read for the assigned issue")
		}
		if comment.ID <= firstNotice || firstInstructionLine(comment.Body) != "再開" || !authorizedPauseComment(comment, issue, operators) {
			continue
		}
		known := false
		for _, id := range ids {
			known = known || id == comment.ID
		}
		if !known {
			ids = append(ids, comment.ID)
		}
	}
	return ids, nil
}

// sayPauseEvent shares the same durable post and ambiguous-response recovery
// as other notices. The occurrence has its own receipt even when an earlier
// pause used exactly the same words. This describes a condition standing now,
// not news about an old event, so the queue's kind-start seam does not hide it.
func (n notices) sayPauseEvent(ctx context.Context, kind, event, words string) (int64, error) {
	// Establish the kind before timestamping the current condition. Settling
	// an older pending notice inside say may cross a whole-second boundary.
	if _, err := kindSince(n.queue, kind, time.Now().UTC()); err != nil {
		return 0, err
	}
	if err := n.say(ctx, kind, words, "", time.Now().UTC(), func(log noticeLog, _ time.Time) bool {
		for _, record := range log.Notices {
			if record.Kind == kind && record.Event == event {
				return false
			}
		}
		return true
	}, event); err != nil {
		return 0, err
	}
	log, err := n.load()
	if err != nil {
		return 0, err
	}
	for _, record := range log.Notices {
		if record.Kind == kind && record.Event == event && record.PostedAt != nil && !record.Predates && record.CommentID > 0 {
			return record.CommentID, nil
		}
	}
	return 0, errors.New("the pause control notice has no confirmed comment receipt")
}

// holdPausedRequest runs only while this request has no child. It neither
// launches a model nor uses an execution slot. Stop is retained first, even
// when the pause record is damaged; a disappearing remote stop cannot turn
// this poll's stop-only decision into permission to run work on the next one.
func holdPausedRequest(ctx context.Context, cfg config, issue sourceIssue, directory, request string, interval time.Duration, observe func(string)) (bool, error) {
	record, recordErr := readWorkLimit(directory)
	if recordErr == nil && len(record.Pauses) == 0 {
		return false, nil
	}
	readCtx, release := context.WithTimeout(ctx, interval)
	source := cfg.source()
	rows, err := source.Comments(readCtx, issue)
	release()
	if err != nil {
		return true, err
	}
	stop, err := stopInstruction(source, rows, issue, cfg.Intake.StopUserIDs)
	if err != nil {
		return true, err
	}
	if stop != nil {
		return true, writeRuntimeFile(filepath.Join(directory, "stop-request.json"), stop)
	}
	if recordErr != nil {
		return true, recordErr
	}
	if _, err := pauseReplyIDs(source, directory, issue, cfg.Intake.StopUserIDs, nil); err != nil {
		return true, err
	}
	n := requestNotices(cfg, issue, directory)
	event := strconv.Itoa(len(record.Pauses))
	pause := &record.Pauses[len(record.Pauses)-1]
	if record.held() {
		applyStatus(ctx, cfg, issue, directory, awaitingStatus, observe)
		assignTurn(ctx, cfg, issue, directory, "requester", observe)
		id, err := n.sayPauseEvent(ctx, workPauseNotice, event, pausedWorkText(*pause))
		if err != nil {
			return true, err
		}
		if pause.NoticeID != 0 && pause.NoticeID != id {
			return true, errPauseRecord
		}
		if pause.NoticeID == 0 {
			pause.NoticeID = id
			if err := saveWorkLimit(directory, record); err != nil {
				return true, err
			}
		}
		if pause.Resume == nil {
			log, err := n.load()
			if err != nil {
				return true, err
			}
			var excluded []int64
			for _, posted := range log.Notices {
				excluded = append(excluded, posted.CommentID)
			}
			raw, err := resumeInstruction(source, rows, issue, cfg.Intake.StopUserIDs, pause.NoticeID, excluded)
			if err != nil || raw == nil {
				return true, err
			}
			if err := applyPauseResume(ctx, cfg, issue, directory, request, &record, raw); err != nil {
				return true, err
			}
		} else if err := applyPauseResume(ctx, cfg, issue, directory, request, &record, nil); err != nil {
			return true, err
		}
	}
	// An applied receipt must still point at the actual words in history.
	// A partial or conflicting local record is not permission to launch.
	state, err := savedHistory(directory)
	if err != nil {
		return true, err
	}
	log, err := n.load()
	if err != nil {
		return true, err
	}
	for i, episode := range record.Pauses {
		confirmed := false
		for _, notice := range log.Notices {
			confirmed = confirmed || (notice.Kind == workPauseNotice && notice.Event == strconv.Itoa(i+1) && notice.PostedAt != nil && !notice.Predates && notice.CommentID == episode.NoticeID)
		}
		resume := episode.Resume
		comment, err := source.ReadComment(resume.Comment, issue)
		if !confirmed || err != nil || resume.HistoryIndex >= len(state.History) ||
			!reflect.DeepEqual(state.History[resume.HistoryIndex], chain.Result{Speaker: "requester", Output: comment.Body, FinishedAt: resume.RecordedAt}) {
			return true, errPauseRecord
		}
	}
	if _, err := n.sayPauseEvent(ctx, workResumeNotice, event, workResumeText); err != nil {
		return true, err
	}
	return false, nil
}

// applyPauseResume appends words, not a successful stage result. The empty role
// is deliberately outside every workflow; Pending, Step, Recovering and an
// unanswered question keep their meanings. Only the controller's pause clears.
func applyPauseResume(ctx context.Context, cfg config, issue sourceIssue, directory, request string, record *workLimitRecord, raw json.RawMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(record.Pauses) == 0 {
		return errPauseRecord
	}
	pause := &record.Pauses[len(record.Pauses)-1]
	if pause.Resume != nil && pause.Resume.Applied {
		return nil
	}
	store, err := chain.Open(filepath.Join(directory, "run"), request)
	if err != nil {
		return err
	}
	defer store.Close()
	state, err := store.Load()
	if err != nil {
		return err
	}
	if state.Done {
		return errors.New("a delivered request cannot be resumed")
	}
	if _, err := os.Stat(filepath.Join(directory, "stop-request.json")); err == nil {
		return errors.New("a stopped request cannot be resumed")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if pause.Resume == nil {
		pause.Resume = &pauseResume{Comment: raw, HistoryIndex: len(state.History), RecordedAt: time.Now().UTC()}
		// Validate the native instruction before either of the local writes.
		comment, err := cfg.source().ReadComment(raw, issue)
		if err != nil || !comment.OnIssue || comment.ID <= pause.NoticeID || firstInstructionLine(comment.Body) != "再開" || !authorizedPauseComment(comment, issue, cfg.Intake.StopUserIDs) {
			pause.Resume = nil
			return errPauseRecord
		}
		if err := saveWorkLimit(directory, *record); err != nil {
			return err
		}
	}
	resume := pause.Resume
	comment, err := cfg.source().ReadComment(resume.Comment, issue)
	if err != nil || !comment.OnIssue || comment.ID <= pause.NoticeID || firstInstructionLine(comment.Body) != "再開" {
		return errPauseRecord
	}
	result := chain.Result{Speaker: "requester", Output: comment.Body, FinishedAt: resume.RecordedAt}
	switch {
	case len(state.History) == resume.HistoryIndex:
		state.History = append(state.History, result)
		if err := store.Save(state); err != nil {
			return err
		}
	case len(state.History) == resume.HistoryIndex+1 && reflect.DeepEqual(state.History[resume.HistoryIndex], result):
		// The previous attempt saved these exact words before it could save
		// the controller's release. No external work has run while held.
	default:
		return errors.New("the history changed before the pause could be released; work remains held")
	}
	resume.Applied = true
	return saveWorkLimit(directory, *record)
}
