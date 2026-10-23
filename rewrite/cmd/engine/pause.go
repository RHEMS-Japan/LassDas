package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"ticket-runner/internal/chain"
	"ticket-runner/internal/tracker"
)

const workLimitFile = "work-limit.json"

// These are reasons recorded by the controller, never classifications of a
// model's output. The clock that creates these pauses is a separate caller.
const (
	activeLimitPause    = "active-limit"
	hardExitPause       = "hard-exit-limit"
	stageAttemptsPause  = "stage-attempt-limit"
	environmentPause    = "environment-short"
	workPauseNotice     = "work-paused"
	workResumeNotice    = "work-resume-accepted"
	workRecoveredNotice = "work-auto-resumed"
)

const workResumeText = "再開の指示を受け取り、一時停止の解除を記録しました。作業の開始や完了を意味するものではありません。回答待ちの質問があれば別のコメントで回答してください。実行枠と利用枠が使えるようになってから、保存した条件で続けます。"

// workLimitRecord belongs to one accepted request. Keeping earlier episodes
// also keeps their consumed control comments out of ordinary question answers.
// A request with a time limit keeps its Clock; one without keeps StageExits
// from its first watched launch. Before either and any pause, it is absent.
type workLimitRecord struct {
	Version    int            `json:"version"`
	Pauses     []pauseEpisode `json:"pauses,omitempty"`
	Clock      *workClock     `json:"clock,omitempty"`
	StageExits *stageExits    `json:"stage_exits,omitempty"`
}

type pauseEpisode struct {
	Reason   string        `json:"reason"`
	At       time.Time     `json:"at"`
	NoticeID int64         `json:"notice_id,omitempty"`
	Resume   *pauseResume  `json:"resume,omitempty"`
	Elapsed  time.Duration `json:"elapsed_ns,omitempty"`
}

// The native instruction and the history position are saved before its words
// join the history. A restart can finish that local write without consuming the
// comment twice or treating an unrelated history change as its own write.
type pauseResume struct {
	Comment         json.RawMessage `json:"comment"`
	HistoryIndex    int             `json:"history_index"`
	RecordedAt      time.Time       `json:"recorded_at"`
	ApprovalChecked bool            `json:"approval_checked,omitempty"`
	Applied         bool            `json:"applied,omitempty"`
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
	if json.Unmarshal(raw, &record) != nil || record.Version != 1 || !record.Clock.valid() || !record.StageExits.valid() ||
		(record.Clock != nil && record.StageExits != nil) {
		return workLimitRecord{}, errPauseRecord
	}
	for i, pause := range record.Pauses {
		if !knownPauseReason(pause.Reason) || pause.At.IsZero() || pause.NoticeID < 0 || pause.Elapsed < 0 {
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

// A pause is not a native stop. Its caller owns cancellation and waits for the
// child before recording its measured end or recovering an unconfirmed interval.
func recordWorkPause(directory, reason string, at time.Time) error {
	if !knownPauseReason(reason) || at.IsZero() {
		return errors.New("a work pause needs its controller reason and time")
	}
	record, err := readWorkLimit(directory)
	if err != nil || record.held() {
		return err
	}
	record.addPause(reason, at)
	return saveWorkLimit(directory, record)
}

// knownPauseReason reports a reason this controller records itself.
func knownPauseReason(reason string) bool {
	return chain.ApprovalPauseText(reason) != "" || reason == activeLimitPause || reason == hardExitPause || reason == stageAttemptsPause || reason == environmentPause
}

func pauseReason(reason string) string {
	if text := chain.ApprovalPauseText(reason); text != "" {
		return text + "。"
	}
	switch reason {
	case activeLimitPause:
		return "今回任された区間の実稼働時間が、設定された上限に達したためです。"
	case stageAttemptsPause:
		return "同じ工程で正常に終わらなかった起動が、設定の回数上限に達したためです。"
	case environmentPause:
		return "要件確定の後の判断で、合意した検証がこの実行環境に収まらないと分かったためです。実装はまだ始めていません。"
	}
	return "強制終了が設定の回数上限に達したためです。"
}

func pausedWorkText(pause pauseEpisode, clock *workClock, exits *stageExits) string {
	reason := pauseReason(pause.Reason)
	if pause.Reason == hardExitPause && clock != nil {
		reason = fmt.Sprintf("強制終了が上限の %d 回に達したためです。", clock.MaxHardExits)
	} else if pause.Reason == hardExitPause && exits != nil {
		reason = stageExitsReason(exits)
	}
	return pausedText(reason, pause, clock)
}

// pausedText is the pause notice around its reason.
func pausedText(reason string, pause pauseEpisode, clock *workClock) string {
	if pause.Reason == "approval_expired" {
		reason += "作業を続ける場合は、再開以外のコメントを書かないでください。遅れた GO も除外の対象となり、承認できなくなります。"
	}
	prefix := "この依頼の自動処理を一時停止しています。"
	if chain.ApprovalPauseText(pause.Reason) != "" {
		prefix = "確認の途中で止まりました。理由: "
		if pause.Reason != "approval_expired" {
			return prefix + reason + "依頼は未完了です。"
		}
	}
	return prefix + reason + workPauseMeasured(clock, pause.Elapsed) +
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
	recoveryErr := pauseApproval(directory, time.Now().UTC())
	if recoveryErr == nil {
		recoveryErr = recoverWorkClock(directory)
	}
	if recoveryErr == nil {
		recoveryErr = pauseStageAttempts(cfg, directory, time.Now().UTC())
	}
	if recoveryErr == nil {
		recoveryErr = pauseEnvironment(directory, time.Now().UTC())
	}
	record, recordErr := readWorkLimit(directory)
	if recoveryErr != nil {
		recordErr = recoveryErr
	}
	if recordErr == nil && len(record.Pauses) == 0 && (record.Clock == nil || record.Clock.RecoveryNoticeAt == nil) {
		return false, nil
	}
	readCtx, release := context.WithTimeout(ctx, interval)
	source := cfg.source()
	rows, stop, err := readStopComments(readCtx, source, issue, cfg.Intake.StopUserIDs)
	release()
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
	if clock := record.Clock; clock != nil && clock.RecoveryNoticeAt != nil && !record.held() {
		words := fmt.Sprintf("強制終了から自動で再開しました（%d 回目、上限 %d 回）。計測できなかった区間は時間に加えず、保存済みの残り時間で続けます。外部の操作は取り消しておらず、依頼は未完了です。実行枠と利用枠が使えるようになってから作業を続けます。", clock.HardExits, clock.MaxHardExits)
		if _, err := n.sayPauseEvent(ctx, workRecoveredNotice, clock.RecoveryNoticeAt.Format(time.RFC3339Nano), words); err != nil {
			return true, err
		}
		clock.RecoveryNoticeAt = nil
		if err := saveWorkLimit(directory, record); err != nil {
			return true, err
		}
	}
	if len(record.Pauses) == 0 {
		return false, nil
	}
	event := strconv.Itoa(len(record.Pauses))
	pause := &record.Pauses[len(record.Pauses)-1]
	if record.held() {
		applyStatus(ctx, cfg, issue, directory, awaitingStatus, observe)
		if state, err := savedHistory(directory); err == nil && (hasApproval(state.Workflow) || hasApproval(cfg.Workflow)) {
			assignApproval(ctx, cfg, issue, directory, state, observe)
		} else {
			assignTurn(ctx, cfg, issue, directory, "requester", observe)
		}
		words := pausedWorkText(*pause, record.Clock, record.StageExits)
		if pause.Reason == stageAttemptsPause || pause.Reason == environmentPause {
			// Nothing has run since the pause, so the history still says what
			// reached the limit, or what the decision read.
			if state, err := savedHistory(directory); err == nil {
				switch pause.Reason {
				case stageAttemptsPause:
					words = pausedText(stageAttemptsReason(state, cfg.Intake.stageAttempts()), *pause, record.Clock)
				case environmentPause:
					words = pausedText(environmentReason(cfg, state), *pause, record.Clock)
				}
			}
		}
		id, err := n.sayPauseEvent(ctx, workPauseNotice, event, words)
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
				excluded = append(excluded, posted.ids()...)
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
	resumeText := workResumeText
	blockedApproval := chain.ApprovalPauseText(pause.Reason) != "" && pause.Reason != "approval_expired"
	if blockedApproval {
		resumeText = approvalResumeBlockedText
	}
	if _, err := n.sayPauseEvent(ctx, workResumeNotice, event, resumeText); err != nil {
		return true, err
	}
	if blockedApproval && !pause.Resume.ApprovalChecked {
		ready, err := recheckApprovalResume(ctx, cfg, issue, directory, request)
		if err != nil || !ready {
			return true, err
		}
		pause.Resume.ApprovalChecked = true
		if err := saveWorkLimit(directory, record); err != nil {
			return true, err
		}
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
	if record.Clock != nil {
		record.Clock.Elapsed = 0
		record.Clock.Active = nil
		record.Clock.HardExits = 0
		record.Clock.RecoveryNoticeAt = nil
	}
	if exits := record.StageExits; exits != nil {
		exits.Active, exits.Stage, exits.Count, exits.Mark = nil, "", 0, 0
	}
	return saveWorkLimit(directory, *record)
}

// defaultStageAttempts is the limit selected when intake.max_stage_attempts
// is omitted or zero: above the default forced-exit count, so forced exits
// alone pause in that count's own words, and four more launches after the
// first failure, each choosing its model again.
const defaultStageAttempts = 5

// stageAttempts is the configured intake.max_stage_attempts, or its default.
func (c *intakeConfig) stageAttempts() int {
	if c == nil || c.MaxStageAttempts <= 0 {
		return defaultStageAttempts
	}
	return c.MaxStageAttempts
}

// pauseStageAttempts pauses a request whose stage has not ended cleanly as
// many launches in a row as intake.max_stage_attempts allows: the run that
// reached it stopped before choosing the stage again, or a restart cut off
// the launch that reached it. It runs while the request has no child, so no
// child's end can overwrite the pause. The count is read from the history
// every time, not kept; the requester's 再開 joins the history and starts it
// over. A request already held keeps its own pause.
func pauseStageAttempts(cfg config, directory string, now time.Time) error {
	record, err := readWorkLimit(directory)
	if err != nil || record.held() {
		return err
	}
	state, err := savedHistory(directory)
	if err != nil {
		return err
	}
	if _, count := state.FailedInARow(); count < cfg.Intake.stageAttempts() {
		return nil
	}
	if record.Clock != nil {
		// It did not resume by itself, so no recovery is said.
		record.Clock.RecoveryNoticeAt = nil
	}
	record.addPause(stageAttemptsPause, now)
	return saveWorkLimit(directory, record)
}

// stageAttemptsReason names the stage, the count and how the launches ended,
// with the words and the count of the notice of a stage that runs again. A
// launch the runtime stopped itself is not counted, so it is not named either.
func stageAttemptsReason(state chain.State, limit int) string {
	ending, count := state.FailedInARow()
	if count == 0 {
		return pauseReason(stageAttemptsPause)
	}
	kinds := countedKinds(ending.Kinds)
	where := "「" + japaneseStage(ending.Role) + "」の工程で、"
	if count == 1 {
		return fmt.Sprintf("%s正常に終わらなかった起動があり%s、上限の %d 回に達したためです。", where, kinds, limit)
	}
	return fmt.Sprintf("%s正常に終わらなかった起動が %d 回続き%s、上限の %d 回に達したためです。", where, count, kinds, limit)
}

// pauseEnvironment pauses a request whose latest record is the runtime's note
// that the decision after the first stage paused it because the environment
// falls short. The run that chose it stopped before launching anything, and
// this runs while the request has no child. The words of a 再開 follow that
// note in the history, so the same note never pauses the request twice.
func pauseEnvironment(directory string, now time.Time) error {
	record, err := readWorkLimit(directory)
	if err != nil || record.held() {
		return err
	}
	state, err := savedHistory(directory)
	if err != nil || !state.EnvironmentPaused() {
		return err
	}
	if record.Clock != nil {
		// It did not resume by itself, so no recovery is said.
		record.Clock.RecoveryNoticeAt = nil
	}
	record.addPause(environmentPause, now)
	return saveWorkLimit(directory, record)
}

// environmentReason quotes the latest launch's requirements report, where a
// run without a question role is told to name what is missing and what the
// operator would have to prepare, and says how many such pauses the request
// has had and may have. The quote is cut and scrubbed the way a failure line
// is in other notices; reportFromShortfall only chooses where it starts.
func environmentReason(cfg config, state chain.State) string {
	if hasApproval(cfg.Workflow) || hasApproval(state.Workflow) {
		return pauseReason(environmentPause)
	}
	report, label, limit := "", "要件確定の報告の冒頭: ", 0
	if flow := state.Workflow; flow != nil && len(flow.Stages) > 0 {
		limit = flow.EnvironmentPauseLimit
		first, ended := flow.Stages[0].Name, false
		for i := len(state.History) - 1; i >= 0 && report == ""; i-- {
			record := state.History[i]
			if record.Role != first {
				continue
			}
			if record.Speaker == "runtime" {
				// The runtime's record ends each launch: a second one belongs
				// to an earlier launch, whose report the decision did not read.
				if ended {
					break
				}
				ended = true
				continue
			}
			text, later := reportFromShortfall(record.Output)
			if later {
				label = "要件確定の報告のうち、足りないことを書いた段落から: "
			}
			report = noticeDetail(cfg, strings.Join(strings.Fields(text), " "))
		}
	}
	if report == "" {
		report = "（報告は空でした）"
	}
	count := state.EnvironmentPauses()
	text := pauseReason(environmentPause) + label + report + "\n" +
		"運用者が環境を広げるか用意してから「再開」と書くと、同じ依頼を要件確定の後の判断から続けます。" +
		fmt.Sprintf("環境が足りないことによる一時停止はこれが %d 回目で、上限は %d 回です。", count, limit)
	if count >= limit {
		text += "上限に達したため、次の「再開」の後は一時停止せず、検証の最も重い部分を先に 1 回試してから進みます。"
	}
	return text
}

// shortfallWords mark a paragraph of a requirements report that names what
// falls short. They choose only where the notice's quote starts, so that a
// report that opens by restating the requester's goal, as the shipped
// instructions ask, is quoted from what is missing; they decide nothing, and a
// report without any of them is quoted from its opening.
var shortfallWords = []string{"不足", "足りな", "足りず", "収まらな", "falls short", "fall short", "insufficient", "not enough", "too small", "missing"}

// reportFromShortfall is the report from its first paragraph that names a
// shortfall on, and whether that paragraph comes after the opening one. A
// paragraph ends at a blank line.
func reportFromShortfall(report string) (string, bool) {
	var paragraphs []string
	var current []string
	for _, line := range append(strings.Split(report, "\n"), "") {
		if strings.TrimSpace(line) != "" {
			current = append(current, line)
			continue
		}
		if len(current) > 0 {
			paragraphs = append(paragraphs, strings.Join(current, "\n"))
			current = nil
		}
	}
	for i, paragraph := range paragraphs {
		lower := strings.ToLower(paragraph)
		for _, word := range shortfallWords {
			if strings.Contains(lower, word) {
				return strings.Join(paragraphs[i:], "\n\n"), i > 0
			}
		}
	}
	return report, false
}
