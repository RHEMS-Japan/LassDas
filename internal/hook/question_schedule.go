package hook

import "time"

// questionZone is Asia/Tokyo as a fixed offset: Japan has not observed
// daylight saving time since 1951, so the fixed zone is exact and removes the
// runtime dependency on a tzdata database.
var questionZone = time.FixedZone("Asia/Tokyo", 9*60*60)

const (
	questionNotifyHour   = 10
	questionDeadlineHour = 17
)

// ComputeQuestionSchedule turns the question posting instant into the sealed
// absolute schedule: renotifications on the 1st, 3rd and 5th weekday after the
// posting date at 10:00 Asia/Tokyo, and the answer deadline on the 5th weekday
// at 17:00. Only Saturday and Sunday are skipped; there is no holiday
// calendar (README 再通知の暫定値). The result feeds QuestionRecord.NotifyAt
// and AnswerDeadlineAt directly, so shortened test timers are a pure input
// concern of whoever seals the record.
func ComputeQuestionSchedule(postedAt time.Time) ([3]int64, int64) {
	local := postedAt.In(questionZone)
	year, month, day := local.Date()
	date := time.Date(year, month, day, 0, 0, 0, 0, questionZone)
	weekdays := make([]time.Time, 0, 5)
	for len(weekdays) < 5 {
		date = date.AddDate(0, 0, 1)
		if weekday := date.Weekday(); weekday != time.Saturday && weekday != time.Sunday {
			weekdays = append(weekdays, date)
		}
	}
	at := func(day time.Time, hour int) int64 {
		return time.Date(day.Year(), day.Month(), day.Day(), hour, 0, 0, 0, questionZone).UnixMilli()
	}
	notifyAt := [3]int64{
		at(weekdays[0], questionNotifyHour),
		at(weekdays[2], questionNotifyHour),
		at(weekdays[4], questionNotifyHour),
	}
	return notifyAt, at(weekdays[4], questionDeadlineHour)
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
