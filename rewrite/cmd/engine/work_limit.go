package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// A clock belongs to the accepted request, not to the current configuration
// or to any stage's success. Active is a crash marker, never a wall-clock
// timestamp from which downtime may be charged on restart.
type workClock struct {
	MaxMinutes       int           `json:"max_minutes"`
	Elapsed          time.Duration `json:"elapsed_ns"`
	Active           *time.Time    `json:"active_since,omitempty"`
	MaxHardExits     int           `json:"max_hard_exits"`
	HardExits        int           `json:"hard_exits"`
	RecoveryNoticeAt *time.Time    `json:"recovery_notice_at,omitempty"`
}

func validWorkMinutes(minutes int) bool {
	return minutes >= 0 && uint64(minutes) <= uint64(time.Duration(1<<63-1)/time.Minute)
}

func (c *workClock) valid() bool {
	return c == nil || (c.MaxMinutes > 0 && validWorkMinutes(c.MaxMinutes) && c.Elapsed >= 0 && (c.Active == nil || !c.Active.IsZero()) &&
		c.MaxHardExits > 0 && c.HardExits >= 0 && c.HardExits <= c.MaxHardExits && (c.RecoveryNoticeAt == nil || !c.RecoveryNoticeAt.IsZero()))
}

// This runs before publishing issue.json. Retain a valid earlier attempt,
// including its original cap, if intake was interrupted between the writes.
func acceptWorkLimit(directory string, minutes, hardExits int) error {
	if !validWorkMinutes(minutes) {
		return errors.New("intake.max_active_minutes must be zero or a positive duration in minutes")
	}
	if hardExits < 0 {
		return errors.New("intake.max_hard_exits must be zero or positive; zero selects the default of 3")
	}
	if _, err := os.Stat(filepath.Join(directory, workLimitFile)); err == nil {
		_, err = readWorkLimit(directory)
		return err
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if minutes == 0 {
		return nil
	}
	if hardExits == 0 {
		hardExits = 3
	}
	return saveWorkLimit(directory, workLimitRecord{Version: 1, Clock: &workClock{MaxMinutes: minutes, MaxHardExits: hardExits}})
}

// Called only when the collector owns a request with no child. An open
// interval after a crash is unknown, not proof of reaching the saved cap.
func recoverWorkClock(directory string) error {
	record, err := readWorkLimit(directory)
	if err != nil || record.Clock == nil {
		return err
	}
	if record.held() {
		return nil
	}
	now := time.Now().UTC()
	changed := false
	if record.Clock.Active != nil {
		record.Clock.Active = nil
		record.Clock.HardExits++
		changed = true
		if record.Clock.HardExits >= record.Clock.MaxHardExits {
			record.Clock.RecoveryNoticeAt = nil
			record.addPause(hardExitPause, now)
		} else {
			record.Clock.RecoveryNoticeAt = &now
		}
	}
	if record.Clock.Elapsed >= time.Duration(record.Clock.MaxMinutes)*time.Minute {
		record.addPause(activeLimitPause, now)
		record.Clock.RecoveryNoticeAt = nil
		changed = true
	}
	if changed {
		return saveWorkLimit(directory, record)
	}
	return nil
}

type workTimer interface{ Stop() bool }

// The narrow clock seam lets tests advance execution time without minute
// sleeps. Production uses monotonic time within this one process only.
type workTimeSource struct {
	now   func() time.Time
	after func(time.Duration, func()) workTimer
}

var activeWorkTime = workTimeSource{now: time.Now, after: func(d time.Duration, f func()) workTimer { return time.AfterFunc(d, f) }}

type activeWork struct {
	directory string
	record    workLimitRecord
	began     time.Time
	timer     workTimer
	save      func(string, workLimitRecord) error
}

var errWorkHeld = errors.New("the request's active-work interval is held")

func beginActiveWork(directory string, cancel context.CancelFunc, clock workTimeSource) (*activeWork, error) {
	record, err := readWorkLimit(directory)
	if err != nil {
		return nil, err
	}
	if record.held() || (record.Clock != nil && record.Clock.Active != nil) {
		return nil, errWorkHeld
	}
	if record.Clock == nil {
		return nil, nil
	}
	remaining := time.Duration(record.Clock.MaxMinutes)*time.Minute - record.Clock.Elapsed
	if remaining <= 0 {
		if err := recordWorkPause(directory, activeLimitPause, clock.now()); err != nil {
			return nil, err
		}
		return nil, errWorkHeld
	}
	began := clock.now()
	stamp := began.UTC()
	record.Clock.Active = &stamp
	if err := saveWorkLimit(directory, record); err != nil {
		return nil, err
	}
	work := &activeWork{directory: directory, record: record, began: began, save: saveWorkLimit}
	work.timer = clock.after(remaining, cancel)
	return work, nil
}

// Only the child's owner writes its actual end. A late timer can only cancel
// that same child context; it cannot change this record or another interval.
func (w *activeWork) finish(ended time.Time) error {
	if w == nil {
		return nil
	}
	w.timer.Stop()
	duration := ended.Sub(w.began)
	if duration < 0 || duration > time.Duration(1<<63-1)-w.record.Clock.Elapsed {
		return errors.New("active-work duration could not be retained; its interval remains open")
	}
	w.record.Clock.Elapsed += duration
	if w.record.Clock.Elapsed >= time.Duration(w.record.Clock.MaxMinutes)*time.Minute {
		w.record.addPause(activeLimitPause, ended)
	}
	w.record.Clock.Active = nil
	return w.save(w.directory, w.record)
}

func (r *workLimitRecord) addPause(reason string, at time.Time) {
	if r.held() {
		return
	}
	pause := pauseEpisode{Reason: reason, At: at.UTC()}
	if r.Clock != nil {
		pause.Elapsed = r.Clock.Elapsed
	}
	r.Pauses = append(r.Pauses, pause)
}

func workPauseMeasured(clock *workClock, elapsed time.Duration) string {
	if clock == nil {
		return ""
	}
	return fmt.Sprintf("保存された上限は %d 分、確定済みの実稼働時間は %s です。", clock.MaxMinutes, spentText(elapsed))
}

type watchedOutcome struct {
	err, clockErr error
}
