package hook

import (
	"context"
	"time"
)

// NotifyBeginRequest asks for the exclusive right to post renotification
// number Index (1..3) of the sealed question. It mirrors the question posting
// protocol: begin acquires a lease on the per-notification marker,
// CompleteNotify binds the observed Backlog comment, and a lost response is
// resolved by re-reading the marker instead of guessing. Notifications are
// unique per run, question revision and index (README 614).
type NotifyBeginRequest struct {
	Record       QuestionRecord
	RecordJSON   string
	RecordSHA256 string
	Route        ReportRouteConfig
	Index        int
	StartedAt    time.Time
	LeaseUntil   time.Time
	LeaseToken   string
}

type NotifyBeginDisposition string

const (
	NotifyBeginAcquired NotifyBeginDisposition = "acquired"
	NotifyBeginBusy     NotifyBeginDisposition = "busy"
	NotifyBeginComplete NotifyBeginDisposition = "complete"
	NotifyBeginConflict NotifyBeginDisposition = "conflict"
)

type NotifyCompleteRequest struct {
	Record       QuestionRecord
	RecordJSON   string
	RecordSHA256 string
	Route        ReportRouteConfig
	Index        int
	LeaseToken   string
	CommentID    int64
	PostedAt     time.Time
}

type NotifyCompleteDisposition string

const (
	NotifyCompleted        NotifyCompleteDisposition = "completed"
	NotifyAlreadyComplete  NotifyCompleteDisposition = "already_complete"
	NotifyCompleteConflict NotifyCompleteDisposition = "conflict"
)

type NotifyStore interface {
	BeginNotify(context.Context, NotifyBeginRequest) (TerminalBinding, NotifyBeginDisposition, error)
	CompleteNotify(context.Context, NotifyCompleteRequest) (NotifyCompleteDisposition, error)
}
