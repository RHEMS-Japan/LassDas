package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"ticket-runner/internal/chain"
	"ticket-runner/internal/tracker"
)

// A request that keeps retrying through the night tells the requester nothing.
// These notices are fixed controller text: no model writes them, none of them
// judges a role's answer, and none of them ends a request. Each condition says
// its piece at most once, and the work carries on or resumes by itself.
const (
	resumeNoticeText   = "自動処理は再起動後に同じ依頼を続けています。直前の工程は途中で止まった可能性があるため、確認してから進めます。"
	pausedNoticeText   = "自動処理を一時停止しました。モデル利用枠の残りが設定の下限を下回ったためです。枠が戻り次第、自動で再開します（この通知への返信は不要です）。"
	restoredNoticeText = "モデル利用枠が回復したため、自動処理を再開しました。"
)

const (
	resumeNotice   = "resume"
	pausedNotice   = "budget-paused"
	restoredNotice = "budget-restored"
	stallNotice    = "stall"
)

// A crash loop must not turn one condition into a column of comments, so the
// same words wait out an interval even across process restarts.
const (
	resumeNoticeInterval = 30 * time.Minute
	stallNoticeInterval  = 6 * time.Hour
	defaultStallMinutes  = 90
)

// The engine knows what it recorded, not what will end the wait. A launch that
// runs long may be working or may be waiting for its operator to correct a
// setting, a failure that repeats may need a person, and a spent budget may
// need someone to add to it. So a notice says what was recorded and promises
// one thing only: it does not wait for an answer.
func stallNoticeText(minutes int, detail string) string {
	if detail == "" {
		return fmt.Sprintf("自動処理は続いていますが、過去 %d 分間は工程が完了していません（工程の失敗は記録されていません。進行中の工程が長引いているか、運用者の対応を待っています）。この通知への返信は不要です。", minutes)
	}
	return fmt.Sprintf("自動処理は続いていますが、過去 %d 分間は工程が完了していません（直近の失敗: %s）。復旧を試し続けています。この通知への返信は不要です。", minutes, detail)
}

// quietFor is how long the request has gone without any record being
// written: since its last record, or since it was accepted when there is
// none. A launch has no time limit, so this is the only word the requester
// gets about one that runs long without failing.
func quietFor(state chain.State, accepted, now time.Time) time.Duration {
	last := accepted
	for _, record := range state.History {
		for _, at := range []time.Time{record.StartedAt, record.FinishedAt} {
			if at.After(last) {
				last = at
			}
		}
	}
	if last.IsZero() {
		return 0
	}
	return now.Sub(last)
}

// noticeRecord is written before the comment is submitted and marked after it
// is confirmed, so an interrupted post is retried instead of repeated. One
// marked Predates is never submitted: what it would report happened before the
// queue's engines posted its kind, and the record settles the kind for this
// request as a posted one would. A declaration keeps the models it named, so
// the next launch of its stage is compared with them.
type noticeRecord struct {
	Kind      string     `json:"kind"`
	Text      string     `json:"text"`
	WrittenAt time.Time  `json:"written_at"`
	PostedAt  *time.Time `json:"posted_at,omitempty"`
	Predates  bool       `json:"predates,omitempty"`
	Models    string     `json:"models,omitempty"`
	// CommentID is the tracker's id of the comment the notice was confirmed
	// as, so a later notice of the same kind in the same words is not taken
	// for this one.
	CommentID int64 `json:"comment_id,omitempty"`
}

type noticeLog struct {
	Notices []noticeRecord `json:"notices"`
}

// notices posts the controller's own fixed comments for one accepted request.
// The controller holds the tracker credential; no role posts these.
type notices struct {
	source    tracker.Tracker
	issue     sourceIssue
	directory string
	// queue keeps when each kind of notice began; a request's directory is
	// the queue's jobs/<id>.
	queue string
}

func requestNotices(cfg config, issue sourceIssue, directory string) notices {
	return notices{source: cfg.source(), issue: issue, directory: directory, queue: filepath.Dir(filepath.Dir(directory))}
}

func (n notices) path() string { return filepath.Join(n.directory, "notices.json") }

func (n notices) load() (noticeLog, error) {
	var log noticeLog
	raw, err := os.ReadFile(n.path())
	if errors.Is(err, os.ErrNotExist) {
		return log, nil
	}
	if err != nil {
		return log, err
	}
	if err := json.Unmarshal(raw, &log); err != nil {
		return noticeLog{}, errors.New("the recorded notices are unreadable; no comment is posted from a damaged record")
	}
	return log, nil
}

func (n notices) save(log noticeLog) error {
	data, err := json.Marshal(log)
	if err != nil {
		return err
	}
	return writeRuntimeFile(n.path(), data)
}

func pendingNotice(log noticeLog) int {
	for i := range log.Notices {
		if log.Notices[i].PostedAt == nil && !log.Notices[i].Predates {
			return i
		}
	}
	return -1
}

// onceNotice reports a kind said at most once per request, so any record of
// it, posted or predating, settles it.
func onceNotice(kind string) bool {
	return kind == acceptedNotice || kind == startedNotice || kind == modelsNotice || strings.HasPrefix(kind, stagePrefix)
}

// noticeDue decides whether a condition may speak again. The budget pause and
// its recovery alternate, so each episode says its own line once; the others
// wait out their interval.
func noticeDue(log noticeLog, kind string, now time.Time) bool {
	if kind == pausedNotice || kind == restoredNotice {
		for i := len(log.Notices) - 1; i >= 0; i-- {
			switch log.Notices[i].Kind {
			case pausedNotice:
				return kind == restoredNotice
			case restoredNotice:
				return kind == pausedNotice
			}
		}
		return kind == pausedNotice
	}
	interval := resumeNoticeInterval
	if kind == stallNotice {
		interval = stallNoticeInterval
	}
	once := onceNotice(kind)
	for i := len(log.Notices) - 1; i >= 0; i-- {
		if log.Notices[i].Kind == kind {
			return !once && now.Sub(log.Notices[i].WrittenAt) >= interval
		}
	}
	return true
}

// A notice is news only from the time the queue's engines post its kind. A
// request older than the kind has no record of it, so an engine that learned
// a new kind used to post it on every request already delivered, a burst of
// comments about the past. The queue therefore keeps, for each kind, when its
// engines began posting it, and every notice carries the time of the event it
// reports: an event before its kind began is not posted.
const noticeKindsFile = "notice-kinds.json"

type noticeKinds struct {
	Since map[string]time.Time `json:"since"`
}

// noticeKindsLock serializes changes to the queue's record: the collector and
// the watcher of each running request may each add a kind.
var noticeKindsLock sync.Mutex

var errUnreadableKinds = errors.New("the queue's record of notice kinds is unreadable; no notice is posted without it")

func loadNoticeKinds(queue string) (noticeKinds, error) {
	var kinds noticeKinds
	raw, err := os.ReadFile(filepath.Join(queue, noticeKindsFile))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return kinds, err
	}
	if err == nil && json.Unmarshal(raw, &kinds) != nil {
		return noticeKinds{}, errUnreadableKinds
	}
	if kinds.Since == nil {
		kinds.Since = map[string]time.Time{}
	}
	return kinds, nil
}

func saveNoticeKinds(queue string, kinds noticeKinds) error {
	data, err := json.Marshal(kinds)
	if err != nil {
		return err
	}
	return writeRuntimeFile(filepath.Join(queue, noticeKindsFile), data)
}

// kindStart is the start written for a kind, in whole seconds: an acceptance
// and a running stage are read from a file's time, and on a filesystem that
// keeps only seconds a file written just after the start must not read as
// before it.
func kindStart(now time.Time) time.Time {
	return now.UTC().Truncate(time.Second)
}

// postableKinds are the kinds of notice an engine with this configuration
// posts.
func postableKinds(cfg config) []string {
	kinds := []string{resumeNotice}
	if cfg.Intake == nil {
		return kinds
	}
	if cfg.Intake.MinModelCredit > 0 {
		kinds = append(kinds, pausedNotice, restoredNotice)
	}
	if stallWindow(cfg) > 0 {
		kinds = append(kinds, stallNotice)
	}
	if cfg.Intake.Announce {
		kinds = append(kinds, acceptedNotice, startedNotice, resumedNotice, modelsNotice)
		if cfg.Workflow != nil {
			for _, stage := range cfg.Workflow.Stages {
				if stage.Announce != "" {
					kinds = append(kinds, stagePrefix+stage.Name)
				}
			}
		}
	}
	if cfg.Intake.DeclareModels {
		for _, role := range cfg.Roles {
			if awaitsModel(cfg, role.Name) {
				kinds = append(kinds, declarePrefix+role.Name)
			}
		}
	}
	return kinds
}

// startNoticeKinds brings the queue's record in line with the engine about to
// run on it, before anything is accepted or said. A kind this engine posts and
// the record lacks starts now. A kind it does not post is dropped, so a kind
// switched off, or unknown to an engine that ran in between, starts again when
// it is posted again instead of reaching back over the time nothing posted it.
func startNoticeKinds(queue string, cfg config, now time.Time, observe func(string)) error {
	noticeKindsLock.Lock()
	defer noticeKindsLock.Unlock()
	kinds, err := loadNoticeKinds(queue)
	if errors.Is(err, errUnreadableKinds) {
		// A record that cannot be read says when no kind began, and left as
		// it is it would hold every notice and be logged for every request on
		// every tick. It is set aside for a person to look at, and the queue
		// starts again as one without a record: nothing from before is said.
		aside := filepath.Join(queue, noticeKindsFile+".unreadable")
		if err := os.Rename(filepath.Join(queue, noticeKindsFile), aside); err != nil {
			return err
		}
		observe("the queue's record of notice kinds was unreadable; it was set aside as " + aside + " and every kind starts again now")
		kinds, err = noticeKinds{Since: map[string]time.Time{}}, nil
	}
	if err != nil {
		return err
	}
	postable := map[string]bool{}
	changed := false
	for _, kind := range postableKinds(cfg) {
		postable[kind] = true
		if kinds.Since[kind].IsZero() {
			kinds.Since[kind] = kindStart(now)
			changed = true
		}
	}
	for kind := range kinds.Since {
		if !postable[kind] {
			delete(kinds.Since, kind)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return saveNoticeKinds(queue, kinds)
}

// kindSince is when the queue's engines began posting a kind. A kind the record
// lacks starts now; nothing says it began earlier.
func kindSince(queue, kind string, now time.Time) (time.Time, error) {
	noticeKindsLock.Lock()
	defer noticeKindsLock.Unlock()
	kinds, err := loadNoticeKinds(queue)
	if err != nil {
		return time.Time{}, err
	}
	if since := kinds.Since[kind]; !since.IsZero() {
		return since, nil
	}
	kinds.Since[kind] = kindStart(now)
	if err := saveNoticeKinds(queue, kinds); err != nil {
		return time.Time{}, err
	}
	return kinds.Since[kind], nil
}

// post records the notice first and submits it after. An earlier notice whose
// submission was never confirmed is settled before a new condition speaks. The
// time given is when the event the notice reports happened; an event from
// before the queue's engines posted this kind is not posted at all.
func (n notices) post(ctx context.Context, kind, text string, at time.Time) error {
	return n.say(ctx, kind, text, "", at, func(log noticeLog, now time.Time) bool {
		return noticeDue(log, kind, now)
	})
}

// declare says which models a launch of a role chose. It is due by its own
// rule rather than once or after an interval, and its record keeps the
// models, which is what that rule compares the next launch with.
func (n notices) declare(ctx context.Context, role, text, models string, at time.Time, due func(noticeLog) bool) error {
	return n.say(ctx, declarePrefix+role, text, models, at, func(log noticeLog, _ time.Time) bool {
		return due(log)
	})
}

// say is the work of post and declare: due decides, on the record as it
// stands once an earlier unconfirmed notice is settled, whether this one
// speaks at all.
func (n notices) say(ctx context.Context, kind, text, models string, at time.Time, due func(noticeLog, time.Time) bool) error {
	log, err := n.load()
	if err != nil {
		return err
	}
	if i := pendingNotice(log); i >= 0 {
		if err := n.settle(ctx, &log, i, false); err != nil {
			return err
		}
		if log.Notices[i].Kind == kind {
			return nil
		}
	}
	now := time.Now().UTC()
	if !due(log, now) {
		return nil
	}
	since, err := kindSince(n.queue, kind, now)
	if err != nil {
		return err
	}
	if at.Before(since) {
		// A kind said once is settled by a record that it predates, so no
		// later tick or restart weighs it again, and so is a declaration,
		// whose record says which models its stage was last declared with.
		// A kind that speaks again keeps no record: one would hold back its
		// next, current occasion for a whole interval, and this occasion's
		// time does not move, so every later tick finds it before the kind's
		// start again.
		if !onceNotice(kind) && models == "" {
			return nil
		}
		log.Notices = append(log.Notices, noticeRecord{Kind: kind, WrittenAt: now, Predates: true, Models: models})
		return n.save(log)
	}
	log.Notices = append(log.Notices, noticeRecord{Kind: kind, Text: text, WrittenAt: now, Models: models})
	if err := n.save(log); err != nil {
		return err
	}
	return n.settle(ctx, &log, len(log.Notices)-1, true)
}

// flush retries a notice that was recorded but never confirmed, so a failed
// submission is carried by later ticks instead of being lost or duplicated.
func (n notices) flush(ctx context.Context) error {
	log, err := n.load()
	if err != nil {
		return err
	}
	i := pendingNotice(log)
	if i < 0 {
		return nil
	}
	return n.settle(ctx, &log, i, false)
}

// settle submits one recorded notice. A record left by an earlier attempt may
// already be visible at the issue, and an ambiguous submission may have landed,
// so both paths read the issue back and match the exact fixed text.
func (n notices) settle(ctx context.Context, log *noticeLog, i int, fresh bool) error {
	record := log.Notices[i]
	if record.PostedAt != nil {
		return nil
	}
	// A kind can say the same words again, a declaration of models said
	// before or a restart noted again, so only a comment newer than the one
	// the kind was last confirmed as can be this notice.
	after := int64(0)
	for _, earlier := range log.Notices[:i] {
		if earlier.Kind == record.Kind && earlier.CommentID > after {
			after = earlier.CommentID
		}
	}
	if !fresh {
		id, err := n.postedAfter(ctx, record.Text, after)
		if err != nil {
			return err
		}
		if id > 0 {
			return n.confirm(log, i, id)
		}
	}
	stored, err := n.source.AddComment(ctx, n.issue, record.Text)
	if err != nil {
		id, readErr := n.postedAfter(ctx, record.Text, after)
		if readErr != nil || id == 0 {
			return errors.Join(err, readErr)
		}
		return n.confirm(log, i, id)
	}
	return n.confirm(log, i, stored)
}

func (n notices) confirm(log *noticeLog, i int, id int64) error {
	at := time.Now().UTC()
	log.Notices[i].PostedAt, log.Notices[i].CommentID = &at, id
	return n.save(*log)
}

// postedAfter is the id of a comment newer than after in exactly this fixed
// text, or zero when there is none. The words are the controller's own, so
// an exact match identifies the notice without decoding anyone's prose.
func (n notices) postedAfter(ctx context.Context, text string, after int64) (int64, error) {
	rows, err := n.source.Comments(ctx, n.issue)
	if err != nil {
		return 0, err
	}
	found := int64(0)
	for _, raw := range rows {
		// Only the id and the words are read: who wrote a comment, and where
		// its record places it, do not decide whether it is this notice.
		id, words, err := n.source.CommentText(raw)
		if err != nil {
			return 0, errors.New("issue comments could not be read before repeating a notice")
		}
		if id > after && words == text {
			found = id
		}
	}
	return found, nil
}

// The shared model key running out is the one failure no role can recover: no
// investigation, redesign or handoff restores the budget. Read what the key has
// left and hold the work until it returns.
const defaultModelCreditURL = "https://openrouter.ai/api/v1/key"

func modelCreditURL(cfg config) string {
	if cfg.Intake != nil && cfg.Intake.ModelCreditURL != "" {
		return cfg.Intake.ModelCreditURL
	}
	return defaultModelCreditURL
}

// modelCreditRemaining returns the budget left on the configured model key. A
// nil amount means the key carries no limit, which never pauses work.
func modelCreditRemaining(ctx context.Context, cfg config) (*float64, error) {
	address := modelCreditURL(cfg)
	if err := validateEndpoint(address); err != nil {
		return nil, err
	}
	key := os.Getenv(cfg.Router.Decision.KeyEnv)
	if key == "" || strings.ContainsAny(key, "\r\n") {
		return nil, errors.New("model credential is unavailable")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, errors.New("cannot construct model budget request")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+key)
	client := http.Client{Timeout: 30 * time.Second}
	if cfg.Intake != nil && cfg.Intake.Client != nil {
		client = *cfg.Intake.Client
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	redact := func(text string) string { return strings.ReplaceAll(text, key, "[credential]") }
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New(redact(err.Error()))
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return nil, errors.New(redact(err.Error()))
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("model budget endpoint returned HTTP %d: %s", response.StatusCode, limitRunes(redact(string(body)), 200))
	}
	var payload struct {
		Data *struct {
			LimitRemaining *float64 `json:"limit_remaining"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.Data == nil {
		return nil, errors.New("model budget response could not be read")
	}
	// A null remainder is a key without a limit. It is not a zero balance and
	// must never be read as one.
	return payload.Data.LimitRemaining, nil
}

// modelCreditHold reports whether the configured minimum is not met, and
// whether the answer is known at all. An unreadable endpoint never pauses work
// and never posts; it is only logged.
func modelCreditHold(ctx context.Context, cfg config, observe func(string)) (low, known bool) {
	if cfg.Intake == nil || cfg.Intake.MinModelCredit <= 0 {
		return false, false
	}
	remaining, err := modelCreditRemaining(ctx, cfg)
	if err != nil {
		observe("model budget could not be read; work is not paused for it: " + err.Error())
		return false, false
	}
	return remaining != nil && *remaining < cfg.Intake.MinModelCredit, true
}

// applyBudgetNotice says once that the work is held for the model budget, and
// once that it carried on. Neither line is a verdict on any role's answer.
func applyBudgetNotice(ctx context.Context, n notices, low bool) error {
	// The reading is the event: the budget is short, or back, now.
	now := time.Now().UTC()
	if low {
		return n.post(ctx, pausedNotice, pausedNoticeText, now)
	}
	return n.post(ctx, restoredNotice, restoredNoticeText, now)
}

// savedHistory reads the request's history without taking the runtime lock,
// which the running engine holds. Saves are atomic renames, so this observes a
// whole earlier state rather than a half-written one.
func savedHistory(directory string) (chain.State, error) {
	var state chain.State
	raw, err := os.ReadFile(filepath.Join(directory, "run", "history.json"))
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return chain.State{}, err
	}
	return state, nil
}

// stalledFor measures how long the request has gone without a completed step.
// It walks the record launch by launch from the newest, and the trailing run
// of failed launches is the stall, measured from the last launch that
// completed. In an ordered run a launch is one role's process records and the
// runtime's own note after them, and it completed only when none of them
// failed; the note is what tells one launch of a role from the next. A run
// without those notes has no such boundary, so there each process record is
// read as a launch of its own. A runtime note without an error and without
// processes, a step taken up again, is neither, and is read through.
func stalledFor(state chain.State, now time.Time) (time.Duration, string, bool) {
	history := state.History
	delimited := state.Workflow != nil && len(state.Workflow.Stages) > 0
	failure := ""
	i := len(history)
	for i > 0 {
		end := i
		launchFailed := false
		for i > 0 && history[i-1].Speaker == "runtime" {
			if history[i-1].Error != "" {
				launchFailed = true
				if failure == "" {
					failure = history[i-1].Error
				}
			}
			i--
		}
		processes := 0
		for role := ""; i > 0 && history[i-1].Speaker != "runtime" && (processes == 0 || (delimited && history[i-1].Role == role)); processes++ {
			role = history[i-1].Role
			if history[i-1].Error != "" {
				launchFailed = true
				if failure == "" {
					failure = history[i-1].Error
				}
			}
			i--
		}
		if processes > 0 && !launchFailed {
			if failure == "" {
				return 0, "", false
			}
			since := history[end-1].FinishedAt
			if since.IsZero() {
				return 0, "", false
			}
			return now.Sub(since), failure, true
		}
	}
	if failure == "" || len(history) == 0 {
		return 0, "", false
	}
	since := history[0].StartedAt
	if since.IsZero() {
		since = history[0].FinishedAt
	}
	if since.IsZero() {
		return 0, "", false
	}
	return now.Sub(since), failure, true
}

func stallWindow(cfg config) time.Duration {
	minutes := defaultStallMinutes
	if cfg.Intake != nil && cfg.Intake.StallNoticeMinutes != nil {
		minutes = *cfg.Intake.StallNoticeMinutes
	}
	return time.Duration(minutes) * time.Minute
}

// noteStall says once that nothing has completed for a while. It changes no
// routing and ends nothing: the request keeps trying to recover.
func noteStall(ctx context.Context, cfg config, n notices, directory string, running bool) error {
	window := stallWindow(cfg)
	if window <= 0 {
		return nil
	}
	state, err := savedHistory(directory)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	// Each silence began where it is measured from: a stall at the last
	// completed step, a quiet launch at the last record or the acceptance.
	elapsed, failure, stalled := stalledFor(state, now)
	if stalled && elapsed > window {
		return n.post(ctx, stallNotice, stallNoticeText(int(elapsed.Minutes()), noticeDetail(cfg, failure)), now.Add(-elapsed))
	}
	// A long silence is worth a word only while the work runs: a request
	// waiting its turn, or held for the model budget, is quiet for other
	// reasons, and those have their own notices.
	if !running {
		return nil
	}
	var accepted time.Time
	if info, err := os.Stat(filepath.Join(directory, "issue.json")); err == nil {
		accepted = info.ModTime().UTC()
	}
	if quiet := quietFor(state, accepted, now); quiet > window {
		return n.post(ctx, stallNotice, stallNoticeText(int(quiet.Minutes()), ""), now.Add(-quiet))
	}
	return nil
}

// noticeDetail renders one failure line for a comment a person will read: the
// first nonblank line, with every configured credential value removed, cut to
// 200 characters. Scrubbing happens before the cut so no partial key survives.
// A process that died says only "exit status N" first; its last line, where a
// harness puts its reason, is added so the comment says why.
func noticeDetail(cfg config, text string) string {
	line := firstInstructionLine(text)
	if strings.HasPrefix(line, "exit status ") || strings.HasPrefix(line, "signal: ") {
		if last := lastInstructionLine(text); last != "" && last != line {
			line += ": " + last
		}
	}
	for _, value := range credentialValues(cfg) {
		line = strings.ReplaceAll(line, value, "[credential]")
		line = strings.ReplaceAll(line, url.QueryEscape(value), "[credential]")
	}
	return limitRunes(line, 200)
}

// lastInstructionLine is the last nonblank line of a text.
func lastInstructionLine(content string) string {
	lines := strings.Split(content, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}

// credentialValues collects every credential value this controller can name,
// longest first so a shorter value inside a longer one cannot leave a remnant.
func credentialValues(cfg config) []string {
	names := map[string]bool{cfg.source().CredentialEnv(): true, cfg.Router.Decision.KeyEnv: true, cfg.Router.LLM.KeyEnv: true}
	if cfg.ModelSelection != nil {
		names[cfg.ModelSelection.Judge.KeyEnv] = true
		if cfg.ModelSelection.Fallback != nil {
			names[cfg.ModelSelection.Fallback.KeyEnv] = true
		}
		if cfg.ModelSelection.Gateway != nil {
			names[cfg.ModelSelection.Gateway.KeyEnv] = true
		}
	}
	for _, role := range cfg.Roles {
		for _, process := range role.Processes {
			for _, source := range process.Secrets {
				names[source] = true
			}
		}
	}
	var values []string
	for name := range names {
		if name == "" {
			continue
		}
		if value := os.Getenv(name); value != "" {
			values = append(values, value)
		}
	}
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	return values
}

func limitRunes(text string, limit int) string {
	text = strings.TrimSpace(text)
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	count := 0
	for index := range text {
		if count == limit {
			return text[:index]
		}
		count++
	}
	return text
}

func validateEndpoint(address string) error {
	parsed, err := url.Parse(address)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("intake.model_credit_url must be an HTTPS URL without credentials or query parameters")
	}
	return nil
}

// validateNotices refuses unusable notice settings before any work is accepted,
// so a wrong value is found at startup and not at two in the morning.
func validateNotices(cfg config) error {
	if cfg.Intake == nil {
		return nil
	}
	if cfg.Intake.MinModelCredit < 0 {
		return errors.New("intake.min_model_credit must not be negative")
	}
	if err := cfg.Intake.Statuses.validate(); err != nil {
		return err
	}
	if cfg.Intake.CategoryOnAccept < 0 {
		return errors.New("intake.category_on_accept must not be negative")
	}
	if cfg.Intake.StatusPage != "" {
		if err := validateEndpoint(cfg.Intake.StatusPage); err != nil {
			return errors.New("intake.status_page must be an HTTPS URL without credentials or query parameters")
		}
	}
	if cfg.Intake.StallNoticeMinutes != nil && *cfg.Intake.StallNoticeMinutes < 0 {
		return errors.New("intake.stall_notice_minutes must not be negative")
	}
	if err := validateEndpoint(modelCreditURL(cfg)); err != nil {
		return err
	}
	if cfg.Intake.MinModelCredit > 0 && cfg.Router.Decision.KeyEnv == "" {
		return errors.New("intake.min_model_credit needs router.decision.key_env to name the model credential")
	}
	return nil
}
