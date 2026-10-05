package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"ticket-runner/internal/chain"
	"ticket-runner/internal/stagename"
	"ticket-runner/internal/tracker"
)

// The requester lives on the tracker. At each turn of the work the runtime
// says there, in its own fixed words, what happened and whose move it is:
// a comment when the request is accepted and when the work starts, the
// operator's sentence when a stage they chose to announce begins, the
// issue's category and status, the assignee, and the hours it took. Each
// change is made once and recorded beside the request, so a restart repeats
// nothing and a refused call is asked again on the next tick.

const (
	acceptedNotice = "accepted"
	startedNotice  = "started"
	modelsNotice   = "models"
	stagePrefix    = "stage:"
	declarePrefix  = "declare:"
)

func acceptedNoticeText(ahead int, page string) string {
	text := "受け付けました。すぐに自動処理を始めます。"
	if ahead > 0 {
		text = fmt.Sprintf("受け付けました。前に %d 件あり、順番が来しだい自動処理を始めます。", ahead)
	}
	if page != "" {
		text += "\n進み具合はこちらで見られます: " + page
	}
	return text
}

const (
	startedNoticeText = "自動処理を開始しました。"
	resumedNotice     = "resumed"
	resumedNoticeText = "返答を受け取りました。自動処理を再開しました。"
	budgetWaitingText = "モデル利用枠の残りが設定の下限を下回っているため、待機中です。枠が戻り次第、自動で処理を始めます。"
)

// turnRecord is what the runtime has already done on the tracker for this
// request beyond its status and notices.
type turnRecord struct {
	Category bool   `json:"category,omitempty"`
	Assignee string `json:"assignee,omitempty"`
	Hours    bool   `json:"hours,omitempty"`
	// Refused counts, per turn, the tracker's refusals, and Reasons keeps
	// the last one: a refused turn is asked again on every tick for as long
	// as the request lives, and the same refusal is logged once.
	Refused map[string]int    `json:"refused,omitempty"`
	Reasons map[string]string `json:"reasons,omitempty"`
	// Next is when each refused turn is asked again: the spacing grows with
	// the refusals, a minute at first and an hour at most, so a turn the
	// tracker keeps refusing is not asked on every tick, and never given up.
	Next map[string]time.Time `json:"next,omitempty"`
}

// turnClock is the time source for retry spacing; tests replace it.
var turnClock = time.Now

// retryDelay is how long a turn waits after its nth refusal.
func retryDelay(refused int) time.Duration {
	delay := time.Minute
	for i := 1; i < refused && delay < time.Hour; i++ {
		delay *= 2
	}
	if delay > time.Hour {
		delay = time.Hour
	}
	return delay
}

// refuse records one refusal, sets when the turn is asked again, and says
// whether its reason is new.
func (r *turnRecord) refuse(turn, reason string) bool {
	if r.Refused == nil {
		r.Refused = map[string]int{}
	}
	if r.Reasons == nil {
		r.Reasons = map[string]string{}
	}
	if r.Next == nil {
		r.Next = map[string]time.Time{}
	}
	r.Refused[turn]++
	r.Next[turn] = turnClock().Add(retryDelay(r.Refused[turn]))
	fresh := r.Reasons[turn] != reason
	r.Reasons[turn] = reason
	return fresh
}

// due says whether a turn may be asked now.
func (r turnRecord) due(turn string) bool {
	next, refused := r.Next[turn]
	return !refused || !turnClock().Before(next)
}

func loadTurns(directory string) turnRecord {
	var record turnRecord
	if raw, err := os.ReadFile(filepath.Join(directory, "turns.json")); err == nil {
		json.Unmarshal(raw, &record)
	}
	return record
}

func saveTurns(directory string, record turnRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return writeRuntimeFile(filepath.Join(directory, "turns.json"), data)
}

// requestPage is the request's own page on the status page, when the
// operator named where that is.
func requestPage(cfg config, issue sourceIssue) string {
	if cfg.Intake == nil || cfg.Intake.StatusPage == "" {
		return ""
	}
	return strings.TrimRight(cfg.Intake.StatusPage, "/") + "/" + strconv.FormatInt(issue.ID, 10)
}

// acceptTurn is the runtime's first word on a newly accepted request: the
// comment, the category, and the runtime as assignee while it works.
func acceptTurn(ctx context.Context, cfg config, issue sourceIssue, directory string, ahead int, creditLow bool, observe func(string)) {
	if cfg.Intake == nil {
		return
	}
	if cfg.Intake.Announce {
		notice := requestNotices(cfg, issue, directory)
		text := acceptedNoticeText(ahead, requestPage(cfg, issue))
		if creditLow {
			text = "受け付けました。" + budgetWaitingText
			if ahead > 0 {
				text += fmt.Sprintf("\n前に %d 件あり、利用枠が戻ってから順番に処理します。", ahead)
			}
			if page := requestPage(cfg, issue); page != "" {
				text += "\n進み具合はこちらで見られます: " + page
			}
		}
		// The request was accepted when the collector wrote its issue here.
		if info, err := os.Stat(filepath.Join(directory, "issue.json")); err != nil {
			observe("acceptance not announced: " + err.Error())
		} else if err := notice.post(ctx, acceptedNotice, text, info.ModTime()); err != nil {
			observe("acceptance not announced: " + err.Error())
		}
	}
	record := loadTurns(directory)
	if source := cfg.source(); source.Target(tracker.Accepted) != nil && !record.Category {
		if !record.due("category") {
			return
		}
		if err := source.Move(ctx, issue, tracker.Accepted); err != nil {
			if record.refuse("category", err.Error()) {
				observe("category not set, asked again later: " + err.Error())
			}
			saveTurns(directory, record)
		} else {
			record.Category = true
			if err := saveTurns(directory, record); err != nil {
				observe("category set but not recorded: " + err.Error())
			}
		}
	}
	assignTurn(ctx, cfg, issue, directory, "runtime", observe)
}

// assignTurn hands the issue to the requester or back to the runtime.
func assignTurn(ctx context.Context, cfg config, issue sourceIssue, directory, who string, observe func(string)) {
	if cfg.Intake == nil || !cfg.Intake.Assign {
		return
	}
	record := loadTurns(directory)
	if record.Assignee == who {
		return
	}
	user := cfg.runtimeUser
	if who == "requester" {
		user = issue.Creator
	}
	if user.ID <= 0 {
		return
	}
	if !record.due("assignee") {
		return
	}
	if err := cfg.source().Assign(ctx, issue, user); err != nil {
		if record.refuse("assignee", err.Error()) {
			observe("assignee not handed to the " + who + ", asked again later: " + err.Error())
		}
		saveTurns(directory, record)
		return
	}
	record.Assignee = who
	if err := saveTurns(directory, record); err != nil {
		observe("assignee handed over but not recorded: " + err.Error())
	}
}

// hoursTurn records, once, how long the request took from acceptance to its
// report, so the tracker's own field shows it.
func hoursTurn(ctx context.Context, cfg config, issue sourceIssue, directory string, accepted, finished time.Time, observe func(string)) {
	if cfg.Intake == nil || !cfg.Intake.Assign || accepted.IsZero() || finished.IsZero() || finished.Before(accepted) {
		return
	}
	record := loadTurns(directory)
	if record.Hours {
		return
	}
	hours := float64(finished.Sub(accepted).Round(time.Minute)) / float64(time.Hour)
	if !record.due("hours") {
		return
	}
	if err := cfg.source().RecordHours(ctx, issue, hours); err != nil {
		if record.refuse("hours", err.Error()) {
			observe("hours not recorded on the issue, asked again later: " + err.Error())
		}
		saveTurns(directory, record)
		return
	}
	record.Hours = true
	if err := saveTurns(directory, record); err != nil {
		observe("hours recorded on the issue but not here: " + err.Error())
	}
}

// The requester reads one comment per stage saying which model began it. At
// the end they get the whole list, every launch of every model stage in the
// order they ran, so a request that was sent back twice shows what each
// attempt was worked on and for how long. The runtime writes it; no model
// composes it, and nothing in it grades what any of them wrote.
const modelsUsedHeading = "使ったモデル (工程ごと、起動順):"

// relaunched opens each launch after the first of the same stage. The work
// comes back to a stage either because a later stage sent it back or because
// the stage's own process did not exit 0, so the word covers both.
const relaunched = " — 再実行:"

// launchFailed marks a launch whose process did not exit 0. It is listed like
// any other: the time was spent on that model either way.
const launchFailed = " (失敗)"

// modelsUsedText is that list, or empty when no launch of this request used a
// model at all. One line per stage, in the order the stages first ran, and on
// it that stage's launches in the order they ran: a stage the work came back
// to late therefore sits on its own line above launches that ran before that
// return, which is what the heading says. A stage of one of the shipped runs
// is named in Japanese, as the status page names it; one an operator named
// otherwise keeps its name.
func modelsUsedText(state chain.State) string {
	order, launches := modelLaunches(state)
	if len(order) == 0 {
		return ""
	}
	var text strings.Builder
	text.WriteString(modelsUsedHeading)
	for _, role := range order {
		name, known := stagename.Japanese(role)
		if !known {
			name = role
		}
		fmt.Fprintf(&text, "\n- %s:", name)
		for i, launch := range launches[role] {
			if i > 0 {
				text.WriteString(relaunched)
			}
			fmt.Fprintf(&text, " %s (%s)", launch.model, launch.duration)
			if launch.failed {
				text.WriteString(launchFailed)
			}
		}
	}
	return text.String()
}

// spentText is how long one launch ran, in whole seconds, with the minutes
// and hours only when there were any.
func spentText(spent time.Duration) string {
	if spent < 0 {
		spent = 0
	}
	spent = spent.Round(time.Second)
	hours, minutes, seconds := int(spent/time.Hour), int(spent/time.Minute)%60, int(spent/time.Second)%60
	switch {
	case hours > 0:
		return fmt.Sprintf("%d 時間 %d 分 %d 秒", hours, minutes, seconds)
	case minutes > 0:
		return fmt.Sprintf("%d 分 %d 秒", minutes, seconds)
	}
	return fmt.Sprintf("%d 秒", seconds)
}

// modelsTurn posts that list once, when the request is delivered, beside the
// other turns the runtime takes there. It is recorded like every other notice,
// so a restart does not post it twice, and a tracker that refuses is asked
// again on the next tick; it never ends or holds the request. The queue walks
// every delivered request on every tick, so one delivered before the queue's
// engines posted the list is settled without one.
func modelsTurn(ctx context.Context, cfg config, issue sourceIssue, directory string, state chain.State, observe func(string)) {
	if cfg.Intake == nil || !cfg.Intake.Announce {
		return
	}
	text := modelsUsedText(state)
	if text == "" {
		return
	}
	// The list reports the delivery, which the last record's end marks.
	delivered := state.History[len(state.History)-1].FinishedAt
	if err := requestNotices(cfg, issue, directory).post(ctx, modelsNotice, text, delivered); err != nil {
		observe("the models used were not announced: " + err.Error())
	}
}

// resumeTurn says that the requester's answer was read and the work goes on.
func resumeTurn(ctx context.Context, cfg config, issue sourceIssue, directory string, answerIndex int, creditLow bool, observe func(string)) {
	if cfg.Intake == nil || !cfg.Intake.Announce {
		return
	}
	// The answer was taken on this tick.
	text := resumedNoticeText
	if creditLow {
		text = "返答を受け取りました。" + budgetWaitingText
	}
	// Each appended answer has its own history position. Another question's
	// answer is new information even within the restart-notice interval.
	event := strconv.Itoa(answerIndex)
	if err := requestNotices(cfg, issue, directory).say(ctx, resumedNotice, text, "", time.Now().UTC(), func(log noticeLog, _ time.Time) bool {
		for _, record := range log.Notices {
			if record.Kind == resumedNotice && record.Event == event {
				return false
			}
		}
		return true
	}, event); err != nil {
		observe("resumption not announced: " + err.Error())
	}
}

// announceStages posts, once each, the operator's sentence for a stage that
// has begun: a stage has begun when its live copy exists or its record does.
// The sentence carries the model the launch that began the stage is working
// with, so the requester reads what is on their request and not only that
// something is. A stage that launches no model, and a runtime that chooses
// none, say the sentence as the operator wrote it. A stage that began before
// the queue's engines announced it is settled without its sentence.
func announceStages(ctx context.Context, cfg config, issue sourceIssue, directory string, observe func(string)) {
	if cfg.Workflow == nil || cfg.Intake == nil || !cfg.Intake.Announce {
		return
	}
	// A stage said, settled or on its way belongs to the record, and only a
	// stage without one goes further, so a watcher that looks every few
	// seconds reads its own files and nothing else until there is news. A
	// submission left unconfirmed is retried on the tracker's own ticks, or
	// first thing when news is posted.
	notice := requestNotices(cfg, issue, directory)
	log, err := notice.load()
	if err != nil {
		observe("stages not announced: " + err.Error())
		return
	}
	var stages []chain.Stage
	for _, stage := range cfg.Workflow.Stages {
		if stage.Announce != "" && !slices.ContainsFunc(log.Notices, func(said noticeRecord) bool {
			return said.Kind == stagePrefix+stage.Name
		}) {
			stages = append(stages, stage)
		}
	}
	if len(stages) == 0 {
		return
	}
	var live []os.DirEntry
	if entries, err := os.ReadDir(filepath.Join(directory, "live")); err == nil {
		live = entries
	}
	// first is when each role's earliest recorded launch began. A record
	// without a start cannot be placed after anything, so it reads as the
	// earliest of all: when it ended is not when the stage began, as the note
	// written after a restart for a launch the restart cut shows, which ends
	// at the restart and began before it.
	var first map[string]time.Time
	for _, stage := range stages {
		// A live copy means a process of this stage is running now, so a
		// model it is still choosing can arrive on a later tick; a stage
		// known only from the record has returned, and what it did not name
		// it never will.
		running := false
		var began time.Time
		for _, entry := range live {
			if !strings.HasPrefix(entry.Name(), stage.Name+"-") {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				// The launch ended since the listing; its record, once
				// written, says when it began.
				continue
			}
			running = true
			if began.IsZero() || info.ModTime().Before(began) {
				began = info.ModTime()
			}
		}
		if first == nil {
			first = map[string]time.Time{}
			if state, err := savedHistory(directory); err == nil {
				for _, result := range state.History {
					if earliest, seen := first[result.Role]; !seen || result.StartedAt.Before(earliest) {
						first[result.Role] = result.StartedAt
					}
				}
			}
		}
		// The stage began with its first launch: the earliest the record
		// keeps, or, before any has returned, the one running now. A stage
		// running again after a restart began before it, with the launch
		// the record keeps.
		if earliest, returned := first[stage.Name]; returned {
			began = earliest
		} else if !running {
			continue
		}
		text := stage.Announce
		// Ask the configuration first: a stage that launches no model has no
		// record to read, on this tick or any other.
		if awaitsModel(cfg, stage.Name) {
			model := firstModel(directory, stage.Name)
			if model == "" && running {
				// A launch writes its choice down before starting its child,
				// so for a stage whose processes are running the choice is at
				// most a tick away. Wait for it rather than spend this
				// stage's one sentence saying nothing about the work.
				continue
			}
			if model != "" {
				text += " (モデル: " + model + ")"
			}
			// A launch that could not choose a model at all named none, and
			// nothing later will name it for this stage. The operator's
			// sentence still goes out: that the stage began is the news.
		}
		if err := notice.post(ctx, stagePrefix+stage.Name, text, began); err != nil {
			observe("stage " + stage.Name + " not announced: " + err.Error())
		}
	}
}

// The requester is told which models the selection chose for a stage while
// its work is under way: at the stage's first launch that chose one, in the
// operator's sentence when the stage announces itself and in the runtime's
// own line when it does not, and at a later launch, after the work came back
// or a launch did not exit 0, only when its models differ from the ones last
// said. The same models again are no news.
const (
	declaredBegins = "を始めます。選定モデル: "
	declaredAgain  = "をやり直します。選定モデル: "
)

// declareModels says, on a tick of a running request, the models of each
// role's latest launch that the requester has not been told. A launch from an
// earlier run of the request, before a question, a restart or a hold, is in
// the history and in the list at delivery, and said now it would be news
// about the past, so only launches since this run began are declared. Only
// the latest launch of each role is looked at: one the next launch of the
// same role replaced before the next look is not declared, and the list at
// delivery names it.
func declareModels(ctx context.Context, cfg config, issue sourceIssue, directory string, began time.Time, observe func(string)) {
	if cfg.Intake == nil || !cfg.Intake.DeclareModels {
		return
	}
	record, err := loadChosen(filepath.Join(directory, "run"))
	if err != nil {
		observe("the models chosen were not declared: " + err.Error())
		return
	}
	if len(record.Launches) == 0 {
		return
	}
	notice := requestNotices(cfg, issue, directory)
	log, err := notice.load()
	if err != nil {
		observe("the models chosen were not declared: " + err.Error())
		return
	}
	// The saved history, read once and only when needed.
	var history []chain.Result
	read := false
	saved := func() []chain.Result {
		if !read {
			read = true
			if state, err := savedHistory(directory); err == nil {
				history = state.History
			}
		}
		return history
	}
	for _, role := range cfg.Roles {
		launches := record.Launches[role.Name]
		if len(launches) == 0 || launches[len(launches)-1].At.Before(began) {
			continue
		}
		latest := launches[len(launches)-1]
		// The processes of a launch choose in turn, so its models are said
		// together once all have chosen, or once it has returned, as a launch
		// whose selection failed for one of them does with fewer.
		if len(latest.Models) < modelProcesses(cfg, role.Name) && len(saved()) <= latest.Launch {
			continue
		}
		sentence := stageSentence(cfg, role.Name)
		// A stage that announces itself says its first launch in that
		// sentence, which goes out first.
		if cfg.Intake.Announce && sentence != "" && !slices.ContainsFunc(log.Notices, func(said noticeRecord) bool {
			return said.Kind == stagePrefix+role.Name
		}) {
			continue
		}
		models := declaredModels(latest.Models)
		last, told := declared(log, role.Name, sentence, record)
		if told && last == models {
			continue
		}
		name, known := stagename.Japanese(role.Name)
		if !known {
			name = role.Name
		}
		// The stage has run before when an earlier launch was kept, or, for
		// one that ran before launches were kept, when the history holds a
		// record of it from before this launch began.
		again := told || len(launches) > 1
		if !again {
			before := saved()
			if latest.Launch < len(before) {
				before = before[:latest.Launch]
			}
			again = slices.ContainsFunc(before, func(result chain.Result) bool { return result.Role == role.Name })
		}
		text := name + declaredBegins + models
		if again {
			text = name + declaredAgain + models
		}
		due := func(log noticeLog) bool {
			last, told := declared(log, role.Name, sentence, record)
			return !told || last != models
		}
		if err := notice.declare(ctx, role.Name, text, models, latest.At, due); err != nil {
			observe("the models chosen for " + role.Name + " were not declared: " + err.Error())
		}
	}
}

// declared reports which models the requester was last told a launch of the
// role chose, and whether they were told any. The role's last declaration
// says. Before there is one, the operator's sentence for the stage says,
// naming the model of the launch it went out about, the stage's first that
// chose one, unless it went out as written, without a model.
func declared(log noticeLog, role, sentence string, record chosenRecord) (string, bool) {
	var stage *noticeRecord
	for i := len(log.Notices) - 1; i >= 0; i-- {
		switch log.Notices[i].Kind {
		case declarePrefix + role:
			return log.Notices[i].Models, true
		case stagePrefix + role:
			stage = &log.Notices[i]
		}
	}
	if stage == nil || stage.Text == sentence {
		return "", false
	}
	// The sentence is about the launch that had begun when it was written.
	// A recorded launch that began after it is not what it named, which
	// happens when launches were not kept yet; the stage's first model is.
	if launches := record.Launches[role]; len(launches) > 0 && !launches[0].At.After(stage.WrittenAt) {
		return declaredModels(launches[0].Models), true
	}
	return record.First[role], true
}

// declaredModels names a launch's models in the order they were chosen, each
// once: under a fixed model every process of a group runs the same one.
func declaredModels(models []string) string {
	var named []string
	for _, model := range models {
		if !slices.Contains(named, model) {
			named = append(named, model)
		}
	}
	return strings.Join(named, "、")
}

// stageSentence is the operator's sentence for a stage, when one is written.
func stageSentence(cfg config, role string) string {
	if cfg.Workflow == nil {
		return ""
	}
	for _, stage := range cfg.Workflow.Stages {
		if stage.Name == role {
			return stage.Announce
		}
	}
	return ""
}
