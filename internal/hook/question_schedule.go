package hook

import "time"

// questionZone is Asia/Tokyo as a fixed offset: Japan has not observed
// daylight saving time since 1951, so the fixed zone is exact and removes the
// runtime dependency on a tzdata database.
var questionZone = time.FixedZone("Asia/Tokyo", 9*60*60)

// DisplayZone is the zone every instant shown to people is rendered in —
// tickets and the board alike, so one pause never reads as two times.
func DisplayZone() *time.Location { return questionZone }

const (
	questionNotifyHour   = 10
	questionDeadlineHour = 17
	// DefaultQuestionDeadlineWeekdays is how many weekdays a requester has
	// to answer when the destination sets no number of its own.
	DefaultQuestionDeadlineWeekdays = 5
	// MinQuestionDeadlineWeekdays is the shortest window that still carries
	// the three renotifications the record's shape requires, each on its own
	// weekday and all before the deadline.
	MinQuestionDeadlineWeekdays = 3
	// MaxQuestionDeadlineWeekdays is the longest window offered. Four
	// working weeks of silence is not a longer wait, it is a different
	// outcome, and the expiry is what produces it.
	MaxQuestionDeadlineWeekdays = 20
)

// ComputeQuestionSchedule turns the question posting instant into the sealed
// absolute schedule: renotifications on the 1st, 3rd and 5th weekday after the
// posting date at 10:00 Asia/Tokyo, and the answer deadline on the 5th weekday
// at 17:00. Only Saturday and Sunday are skipped; there is no holiday
// calendar (README 再通知の暫定値). The result feeds QuestionRecord.NotifyAt
// and AnswerDeadlineAt directly, so shortened test timers are a pure input
// concern of whoever seals the record.
func ComputeQuestionSchedule(postedAt time.Time) ([3]int64, int64) {
	return ComputeQuestionScheduleWithin(postedAt, DefaultQuestionDeadlineWeekdays)
}

// ComputeQuestionScheduleWithin is ComputeQuestionSchedule over a window the
// destination chose: the deadline falls on the last weekday of the window at
// 17:00, and the three renotifications are spread across it in the
// proportions the five-weekday default uses (the 1st, 3rd and 5th of five).
// A number outside the offered range falls back to the default rather than
// producing a schedule nobody asked for — which is also what keeps the three
// reminders on three separate weekdays, since a window shorter than
// MinQuestionDeadlineWeekdays has nowhere to put them and the sealed record
// refuses a schedule that is not strictly increasing.
func ComputeQuestionScheduleWithin(postedAt time.Time, deadlineWeekdays int) ([3]int64, int64) {
	if deadlineWeekdays < MinQuestionDeadlineWeekdays || deadlineWeekdays > MaxQuestionDeadlineWeekdays {
		deadlineWeekdays = DefaultQuestionDeadlineWeekdays
	}
	local := postedAt.In(questionZone)
	year, month, day := local.Date()
	date := time.Date(year, month, day, 0, 0, 0, 0, questionZone)
	weekdays := make([]time.Time, 0, deadlineWeekdays)
	for len(weekdays) < deadlineWeekdays {
		date = date.AddDate(0, 0, 1)
		if weekday := date.Weekday(); weekday != time.Saturday && weekday != time.Sunday {
			weekdays = append(weekdays, date)
		}
	}
	at := func(day time.Time, hour int) int64 {
		return time.Date(day.Year(), day.Month(), day.Day(), hour, 0, 0, 0, questionZone).UnixMilli()
	}
	// The nth weekday of this window that the nth renotification of the
	// five-weekday default lands on, rounded to the nearest weekday. For
	// every window this package offers the three land on three different
	// weekdays with the deadline on the last, which is the shape the sealed
	// record requires; that is what MinQuestionDeadlineWeekdays is for, and
	// a test walks every offered window to hold it true.
	nth := func(defaultDay int) int {
		return (defaultDay*deadlineWeekdays + DefaultQuestionDeadlineWeekdays/2) / DefaultQuestionDeadlineWeekdays
	}
	first, second, third := nth(1), nth(3), nth(5)
	notifyAt := [3]int64{
		at(weekdays[first-1], questionNotifyHour),
		at(weekdays[second-1], questionNotifyHour),
		at(weekdays[third-1], questionNotifyHour),
	}
	return notifyAt, at(weekdays[deadlineWeekdays-1], questionDeadlineHour)
}

type QuestionTickKind string

const (
	QuestionTickNone   QuestionTickKind = "none"
	QuestionTickNotify QuestionTickKind = "notify"
	QuestionTickExpire QuestionTickKind = "expire"
)

// QuestionTickAction is what one scheduled wake-up should do for a waiting
// question. NotifyIndex is 1..3 when Kind is notify.
type QuestionTickAction struct {
	Kind        QuestionTickKind
	NotifyIndex int
}

// DecideQuestionTick decides the single action for the sealed schedule at the
// given instant (unix milliseconds). Past the deadline the only action is
// expiry — a stale renotification is never sent. Otherwise the latest due
// notification wins: when an outage skips a slot, the requester gets one
// current reminder instead of a burst of stale ones. Exactly-once delivery of
// the chosen notification is owned by the notification marker in the store,
// so repeating the same decision on later ticks is harmless. The record must
// be the sealed, shape-validated question record.
func DecideQuestionTick(record QuestionRecord, now int64) QuestionTickAction {
	if now >= record.AnswerDeadlineAt {
		return QuestionTickAction{Kind: QuestionTickExpire}
	}
	for index := len(record.NotifyAt); index >= 1; index-- {
		if now >= record.NotifyAt[index-1] {
			return QuestionTickAction{Kind: QuestionTickNotify, NotifyIndex: index}
		}
	}
	return QuestionTickAction{Kind: QuestionTickNone}
}
