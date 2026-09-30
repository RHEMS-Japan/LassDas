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
	pausedNoticeText   = "自動処理を一時停止しました。モデル利用枠の残りが設定の下限を下回ったためです。枠が戻り次第、自動で再開します（人の操作は不要です）。"
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

func stallNoticeText(minutes int, detail string) string {
	return fmt.Sprintf("自動処理は続いていますが、過去 %d 分間は工程が完了していません（直近の失敗: %s）。復旧を試し続けており、人の操作は不要です。", minutes, detail)
}

// noticeRecord is written before the comment is submitted and marked after it
// is confirmed, so an interrupted post is retried instead of repeated.
type noticeRecord struct {
	Kind      string     `json:"kind"`
	Text      string     `json:"text"`
	WrittenAt time.Time  `json:"written_at"`
	PostedAt  *time.Time `json:"posted_at,omitempty"`
}

type noticeLog struct {
	Notices []noticeRecord `json:"notices"`
}

// notices posts the controller's own fixed comments for one accepted request.
// The controller holds the tracker credential; no role posts these.
type notices struct {
	backlog   tracker.Backlog
	issue     sourceIssue
	directory string
}

func requestNotices(cfg config, issue sourceIssue, directory string) notices {
	return notices{backlog: cfg.Backlog, issue: issue, directory: directory}
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
		if log.Notices[i].PostedAt == nil {
			return i
		}
	}
	return -1
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
	once := kind == acceptedNotice || kind == startedNotice || strings.HasPrefix(kind, stagePrefix)
	for i := len(log.Notices) - 1; i >= 0; i-- {
		if log.Notices[i].Kind == kind {
			return !once && now.Sub(log.Notices[i].WrittenAt) >= interval
		}
	}
	return true
}

// post records the notice first and submits it after. An earlier notice whose
// submission was never confirmed is settled before a new condition speaks.
func (n notices) post(ctx context.Context, kind, text string) error {
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
	if !noticeDue(log, kind, time.Now().UTC()) {
		return nil
	}
	log.Notices = append(log.Notices, noticeRecord{Kind: kind, Text: text, WrittenAt: time.Now().UTC()})
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
	if !fresh {
		posted, err := n.alreadyPosted(ctx, record.Text)
		if err != nil {
			return err
		}
		if posted {
			return n.confirm(log, i)
		}
	}
	if _, err := n.backlog.AddComment(ctx, n.issue.Key, record.Text); err != nil {
		posted, readErr := n.alreadyPosted(ctx, record.Text)
		if readErr != nil || !posted {
			return errors.Join(err, readErr)
		}
	}
	return n.confirm(log, i)
}

func (n notices) confirm(log *noticeLog, i int) error {
	at := time.Now().UTC()
	log.Notices[i].PostedAt = &at
	return n.save(*log)
}

// alreadyPosted looks for this exact fixed text among the issue's comments.
// The words are the controller's own, so an exact match identifies the notice
// without decoding anyone's prose.
func (n notices) alreadyPosted(ctx context.Context, text string) (bool, error) {
	rows, err := n.backlog.Comments(ctx, n.issue.Key, 0)
	if err != nil {
		return false, err
	}
	for _, raw := range rows {
		var comment struct {
			ID      int64
			Content string
		}
		if err := json.Unmarshal(raw, &comment); err != nil || comment.ID <= 0 {
			return false, errors.New("issue comments could not be read before repeating a notice")
		}
		if comment.Content == text {
			return true, nil
		}
	}
	return false, nil
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
	if low {
		return n.post(ctx, pausedNotice, pausedNoticeText)
	}
	return n.post(ctx, restoredNotice, restoredNoticeText)
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
func noteStall(ctx context.Context, cfg config, n notices, directory string) error {
	window := stallWindow(cfg)
	if window <= 0 {
		return nil
	}
	state, err := savedHistory(directory)
	if err != nil {
		return err
	}
	elapsed, failure, stalled := stalledFor(state, time.Now().UTC())
	if !stalled || elapsed <= window {
		return nil
	}
	return n.post(ctx, stallNotice, stallNoticeText(int(elapsed.Minutes()), noticeDetail(cfg, failure)))
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
	names := map[string]bool{cfg.Backlog.KeyEnv: true, cfg.Router.Decision.KeyEnv: true, cfg.Router.LLM.KeyEnv: true}
	if cfg.ModelSelection != nil {
		names[cfg.ModelSelection.Judge.KeyEnv] = true
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
