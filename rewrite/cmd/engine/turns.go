package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"ticket-runner/internal/chain"
	"ticket-runner/internal/stagename"
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
func acceptTurn(ctx context.Context, cfg config, issue sourceIssue, directory string, ahead int, observe func(string)) {
	if cfg.Intake == nil {
		return
	}
	if cfg.Intake.Announce {
		notice := requestNotices(cfg, issue, directory)
		if err := notice.post(ctx, acceptedNotice, acceptedNoticeText(ahead, requestPage(cfg, issue))); err != nil {
			observe("acceptance not announced: " + err.Error())
		}
	}
	record := loadTurns(directory)
	if id := cfg.Intake.CategoryOnAccept; id > 0 && !record.Category {
		ids := []int64{}
		for _, c := range issue.Category {
			if c.ID > 0 && c.ID != id {
				ids = append(ids, c.ID)
			}
		}
		if !record.due("category") {
			return
		}
		if err := cfg.Backlog.SetCategories(ctx, issue.Key, append(ids, id)); err != nil {
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
		user = issue.Creator.ID
	}
	if user <= 0 {
		return
	}
	if !record.due("assignee") {
		return
	}
	if err := cfg.Backlog.SetAssignee(ctx, issue.Key, user); err != nil {
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
	if err := cfg.Backlog.SetActualHours(ctx, issue.Key, hours); err != nil {
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
// again on the next tick; it never ends or holds the request.
func modelsTurn(ctx context.Context, cfg config, issue sourceIssue, directory string, state chain.State, observe func(string)) {
	if cfg.Intake == nil || !cfg.Intake.Announce {
		return
	}
	text := modelsUsedText(state)
	if text == "" {
		return
	}
	if err := requestNotices(cfg, issue, directory).post(ctx, modelsNotice, text); err != nil {
		observe("the models used were not announced: " + err.Error())
	}
}

// resumeTurn says that the requester's answer was read and the work goes on.
func resumeTurn(ctx context.Context, cfg config, issue sourceIssue, directory string, observe func(string)) {
	if cfg.Intake == nil || !cfg.Intake.Announce {
		return
	}
	if err := requestNotices(cfg, issue, directory).post(ctx, resumedNotice, resumedNoticeText); err != nil {
		observe("resumption not announced: " + err.Error())
	}
}

// announceStages posts, once each, the operator's sentence for a stage that
// has begun: a stage has begun when its live copy exists or its record does.
// The sentence carries the model the launch that began the stage is working
// with, so the requester reads what is on their request and not only that
// something is. A stage that launches no model, and a runtime that chooses
// none, say the sentence as the operator wrote it.
func announceStages(ctx context.Context, cfg config, issue sourceIssue, directory string, observe func(string)) {
	if cfg.Workflow == nil || cfg.Intake == nil || !cfg.Intake.Announce {
		return
	}
	var live []string
	if entries, err := os.ReadDir(filepath.Join(directory, "live")); err == nil {
		for _, entry := range entries {
			live = append(live, entry.Name())
		}
	}
	var roles map[string]bool
	for _, stage := range cfg.Workflow.Stages {
		if stage.Announce == "" {
			continue
		}
		// A live copy means a process of this stage is running now, so a
		// model it is still choosing can arrive on a later tick; a stage
		// known only from the record has returned, and what it did not name
		// it never will.
		running := false
		for _, name := range live {
			if strings.HasPrefix(name, stage.Name+"-") {
				running = true
			}
		}
		begun := running
		if !begun {
			if roles == nil {
				roles = map[string]bool{}
				if state, err := savedHistory(directory); err == nil {
					for _, result := range state.History {
						roles[result.Role] = true
					}
				}
			}
			begun = roles[stage.Name]
		}
		if !begun {
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
		if err := requestNotices(cfg, issue, directory).post(ctx, stagePrefix+stage.Name, text); err != nil {
			observe("stage " + stage.Name + " not announced: " + err.Error())
		}
	}
}
