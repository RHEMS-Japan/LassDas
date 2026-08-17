package hook

import (
	"context"
	"time"
)

// RunCommentKind is a run-level one-shot notification: the acknowledgement
// posted when a run is accepted, and the receipt posted when an adopted
// answer resumed it. Each (kind, qualifier) pair owns one marker item, so the
// comment is posted exactly once no matter how often the tick repeats.
type RunCommentKind string

const (
	RunCommentAck     RunCommentKind = "ack"
	RunCommentReceipt RunCommentKind = "answer-receipt"
)

type RunCommentBeginRequest struct {
	Route         ReportRouteConfig
	Kind          RunCommentKind
	Qualifier     string
	ContentSHA256 string
	StartedAt     time.Time
	LeaseUntil    time.Time
	LeaseToken    string
}

type RunCommentCompleteRequest struct {
	Route         ReportRouteConfig
	Kind          RunCommentKind
	Qualifier     string
	ContentSHA256 string
	LeaseToken    string
	CommentID     int64
	PostedAt      time.Time
}

type RunCommentStore interface {
	BeginRunComment(context.Context, RunCommentBeginRequest) (TerminalBinding, ReplyBeginDisposition, error)
	CompleteRunComment(context.Context, RunCommentCompleteRequest) (ReplyCompleteDisposition, error)
	// RunCommentState reports whether the marker already binds a posted
	// comment.
	RunCommentState(ctx context.Context, route ReportRouteConfig, kind RunCommentKind, qualifier string) (bool, error)
}
