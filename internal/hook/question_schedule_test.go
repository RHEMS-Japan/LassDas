package hook

import (
	"testing"
	"time"
)

func jstDate(t *testing.T, year int, month time.Month, day, hour int) time.Time {
	t.Helper()
	return time.Date(year, month, day, hour, 0, 0, 0, questionZone)
}

func TestComputeQuestionScheduleCountsWeekdaysInTokyo(t *testing.T) {
	for _, run := range []struct {
		name     string
		postedAt time.Time
		notify   [3]time.Time
		deadline time.Time
	}{
		{
			// 2026-08-03 is a Monday.
			name:     "monday afternoon",
			postedAt: jstDate(t, 2026, 8, 3, 15),
			notify:   [3]time.Time{jstDate(t, 2026, 8, 4, 10), jstDate(t, 2026, 8, 6, 10), jstDate(t, 2026, 8, 10, 10)},
			deadline: jstDate(t, 2026, 8, 10, 17),
		},
		{
			name:     "friday morning skips the weekend",
			postedAt: jstDate(t, 2026, 8, 7, 9),
			notify:   [3]time.Time{jstDate(t, 2026, 8, 10, 10), jstDate(t, 2026, 8, 12, 10), jstDate(t, 2026, 8, 14, 10)},
			deadline: jstDate(t, 2026, 8, 14, 17),
		},
		{
			name:     "saturday posting starts monday",
			postedAt: jstDate(t, 2026, 8, 8, 12),
			notify:   [3]time.Time{jstDate(t, 2026, 8, 10, 10), jstDate(t, 2026, 8, 12, 10), jstDate(t, 2026, 8, 14, 10)},
			deadline: jstDate(t, 2026, 8, 14, 17),
		},
		{
			name:     "sunday posting starts monday",
			postedAt: jstDate(t, 2026, 8, 9, 23),
			notify:   [3]time.Time{jstDate(t, 2026, 8, 10, 10), jstDate(t, 2026, 8, 12, 10), jstDate(t, 2026, 8, 14, 10)},
			deadline: jstDate(t, 2026, 8, 14, 17),
		},
		{
			// 20:00 UTC on Monday is already 05:00 Tuesday in Tokyo, so the
			// count must start from Wednesday.
			name:     "utc instant crossing the tokyo date line",
			postedAt: time.Date(2026, 8, 3, 20, 0, 0, 0, time.UTC),
			notify:   [3]time.Time{jstDate(t, 2026, 8, 5, 10), jstDate(t, 2026, 8, 7, 10), jstDate(t, 2026, 8, 11, 10)},
			deadline: jstDate(t, 2026, 8, 11, 17),
		},
		{
			// The year boundary has no holiday calendar: 2026-12-31 (Thursday)
			// counts Friday 1/1, Monday 1/4, ... as plain weekdays.
			name:     "year boundary stays a plain weekday",
			postedAt: jstDate(t, 2026, 12, 31, 11),
			notify:   [3]time.Time{jstDate(t, 2027, 1, 1, 10), jstDate(t, 2027, 1, 5, 10), jstDate(t, 2027, 1, 7, 10)},
			deadline: jstDate(t, 2027, 1, 7, 17),
		},
	} {
		t.Run(run.name, func(t *testing.T) {
			notifyAt, deadlineAt := ComputeQuestionSchedule(run.postedAt)
			for index := range run.notify {
				if notifyAt[index] != run.notify[index].UnixMilli() {
					t.Fatalf("notify %d = %d, want %s", index+1, notifyAt[index], run.notify[index])
				}
			}
			if deadlineAt != run.deadline.UnixMilli() {
				t.Fatalf("deadline = %d, want %s", deadlineAt, run.deadline)
			}
		})
	}
}

func TestComputedScheduleSealsIntoAQuestionRecord(t *testing.T) {
	notifyAt, deadlineAt := ComputeQuestionSchedule(jstDate(t, 2026, 8, 3, 15))
	record := questionTestRecord()
	record.NotifyAt = notifyAt
	record.AnswerDeadlineAt = deadlineAt
	if err := record.ValidateShape(); err != nil {
		t.Fatalf("computed schedule does not seal: %v", err)
	}
}

func TestDecideQuestionTickPicksExpiryOverStaleNotifications(t *testing.T) {
	record := questionTestRecord() // notify 1000/2000/3000, deadline 4000
	for _, run := range []struct {
		name string
		now  int64
		want QuestionTickAction
	}{
		{name: "before the first slot", now: 500, want: QuestionTickAction{Kind: QuestionTickNone}},
		{name: "exactly at the first slot", now: 1_000, want: QuestionTickAction{Kind: QuestionTickNotify, NotifyIndex: 1}},
		{name: "between first and second", now: 1_500, want: QuestionTickAction{Kind: QuestionTickNotify, NotifyIndex: 1}},
		{name: "an outage skipping a slot sends only the latest", now: 2_100, want: QuestionTickAction{Kind: QuestionTickNotify, NotifyIndex: 2}},
		{name: "third slot", now: 3_999, want: QuestionTickAction{Kind: QuestionTickNotify, NotifyIndex: 3}},
		{name: "exactly at the deadline", now: 4_000, want: QuestionTickAction{Kind: QuestionTickExpire}},
		{name: "long after the deadline", now: 40_000, want: QuestionTickAction{Kind: QuestionTickExpire}},
	} {
		t.Run(run.name, func(t *testing.T) {
			if action := DecideQuestionTick(record, run.now); action != run.want {
				t.Fatalf("DecideQuestionTick() = %+v, want %+v", action, run.want)
			}
		})
	}
}
