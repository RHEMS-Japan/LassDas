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
	// Waited says the acceptance comment told the requester to wait their
	// turn, so the start of the work is worth a comment of its own.
	Waited bool `json:"waited,omitempty"`
	// Refused counts, per turn, the tracker's refusals; after turnAttempts
	// of them the turn is left alone, so a field the project does not have
	// is not asked for on every tick for as long as the queue lives.
	Refused map[string]int `json:"refused,omitempty"`
}

const turnAttempts = 5

func (r *turnRecord) refuse(turn string) {
	if r.Refused == nil {
		r.Refused = map[string]int{}
	}
	r.Refused[turn]++
}

func (r turnRecord) givenUp(turn string) bool { return r.Refused[turn] >= turnAttempts }

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
		if !notice.has(acceptedNotice) {
			record := loadTurns(directory)
			record.Waited = ahead > 0
			if err := saveTurns(directory, record); err != nil {
				observe("place in line not recorded: " + err.Error())
			}
		}
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
		if record.givenUp("category") {
			return
		}
		if err := cfg.Backlog.SetCategories(ctx, issue.Key, append(ids, id)); err != nil {
			observe("category not set: " + err.Error())
			record.refuse("category")
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

// startDeservesNotice says whether the start of the work is worth a comment
// of its own: it is when the requester was told to wait their turn, or when
// the acceptance was never announced. A requester told the work starts at
// once hears nothing more until a stage the operator chose to announce begins.
func startDeservesNotice(cfg config, issue sourceIssue, directory string) bool {
	if !requestNotices(cfg, issue, directory).has(acceptedNotice) {
		return true
	}
	return loadTurns(directory).Waited
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
	if record.givenUp("assignee") {
		return
	}
	if err := cfg.Backlog.SetAssignee(ctx, issue.Key, user); err != nil {
		observe("assignee not handed to the " + who + ": " + err.Error())
		record.refuse("assignee")
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
	if record.givenUp("hours") {
		return
	}
	if err := cfg.Backlog.SetActualHours(ctx, issue.Key, hours); err != nil {
		observe("hours not recorded on the issue: " + err.Error())
		record.refuse("hours")
		saveTurns(directory, record)
		return
	}
	record.Hours = true
	if err := saveTurns(directory, record); err != nil {
		observe("hours recorded on the issue but not here: " + err.Error())
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
		begun := false
		for _, name := range live {
			if strings.HasPrefix(name, stage.Name+"-") {
				begun = true
			}
		}
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
		if err := requestNotices(cfg, issue, directory).post(ctx, stagePrefix+stage.Name, stage.Announce); err != nil {
			observe("stage " + stage.Name + " not announced: " + err.Error())
		}
	}
}
