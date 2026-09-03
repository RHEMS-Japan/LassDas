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
	// RunCommentPlan is the implementation-plan notice posted after the
	// readiness gate passes and before the first card dispatches: what the
	// automation understood and how to stop it. A notice, not a gate.
	RunCommentPlan RunCommentKind = "plan"
	// RunCommentE2E is the debug role's post-merge staging observation:
	// what the browser saw on the deployed staging page, sealed once per
	// run after the human merge and the staging deployment.
	RunCommentE2E RunCommentKind = "e2e"
	// RunCommentStagingReport is the v2 delivery's staging summary: the
	// sealed evidence plus the Go instructions (or the honest failure).
	RunCommentStagingReport RunCommentKind = "stg-report"
	// RunCommentReleaseReport is the v2 delivery's final production
	// summary, posted once after the Go-driven promotion ends either way.
	RunCommentReleaseReport RunCommentKind = "rel-report"
	// RunCommentResolved acknowledges an operator's 「確認済み」 on a report
	// that asked for attention: the automation's part of the delivery ends
	// there, recorded once on the ticket.
	RunCommentResolved RunCommentKind = "resolved"
	// RunCommentBudgetHold says a delivery cannot start because a role's
	// key is out of budget; it resumes by itself once the cap is raised.
	RunCommentBudgetHold RunCommentKind = "budget-hold"
	// RunCommentSessionHold says a delivery cannot start because the
	// observation browser cannot sign in to a destination's staging; it
	// resumes by itself once the session jar is renewed.
	RunCommentSessionHold RunCommentKind = "session-hold"
	// RunCommentStreakHold says intake is stopped because the same failure
	// ended the last N deliveries; RunCommentStreakResolved acknowledges
	// the operator's 「確認済み」 that lifts it.
	RunCommentStreakHold     RunCommentKind = "streak-hold"
	RunCommentStreakResolved RunCommentKind = "streak-resolved"
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
