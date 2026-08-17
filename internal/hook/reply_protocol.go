package hook

import (
	"context"
	"time"
)

// ReplyKind distinguishes the two intake replies: the one-time format
// guidance and the per-comment shortfall re-listing.
type ReplyKind string

const (
	ReplyGuidance  ReplyKind = "guidance"
	ReplyShortfall ReplyKind = "shortfall"
)

// ReplyBeginRequest asks for the exclusive right to post one intake reply.
// The guidance marker key is fixed per question revision, which makes the
// "once per revision" contract structural; a shortfall marker is keyed by the
// incomplete answer comment it replies to. ContentSHA256 binds the
// deterministic reply body decided before posting.
type ReplyBeginRequest struct {
	Record           QuestionRecord
	RecordJSON       string
	RecordSHA256     string
	Route            ReportRouteConfig
	Kind             ReplyKind
	TriggerCommentID int64
	ContentSHA256    string
	StartedAt        time.Time
	LeaseUntil       time.Time
	LeaseToken       string
}

type ReplyBeginDisposition string

const (
	ReplyBeginAcquired ReplyBeginDisposition = "acquired"
	ReplyBeginBusy     ReplyBeginDisposition = "busy"
	ReplyBeginComplete ReplyBeginDisposition = "complete"
	ReplyBeginConflict ReplyBeginDisposition = "conflict"
)

type ReplyCompleteRequest struct {
	Record           QuestionRecord
	RecordJSON       string
	RecordSHA256     string
	Route            ReportRouteConfig
	Kind             ReplyKind
	TriggerCommentID int64
	ContentSHA256    string
	LeaseToken       string
	CommentID        int64
	PostedAt         time.Time
}

type ReplyCompleteDisposition string

const (
	ReplyCompleted        ReplyCompleteDisposition = "completed"
	ReplyAlreadyComplete  ReplyCompleteDisposition = "already_complete"
	ReplyCompleteConflict ReplyCompleteDisposition = "conflict"
)

type ReplyStore interface {
	BeginReply(context.Context, ReplyBeginRequest) (TerminalBinding, ReplyBeginDisposition, error)
	CompleteReply(context.Context, ReplyCompleteRequest) (ReplyCompleteDisposition, error)
	// ReplyState reports whether a bound reply comment exists for the marker:
	// the guidance marker of the revision, or the shortfall marker of one
	// trigger comment. It feeds AnswerIntakeInput.GuidanceSent and
	// HandledCommentIDs.
	ReplyState(ctx context.Context, route ReportRouteConfig, record QuestionRecord, kind ReplyKind, triggerCommentID int64) (bool, error)
}
