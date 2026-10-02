package tracker

import (
	"context"
	"encoding/json"
	"net/url"
	"time"
)

// The turns of the work at which the engine may have the tracker set
// something on an issue. The names are kept in each request's own records,
// so they stay as they are.
const (
	Accepted          = "accepted"
	Processing        = "processing"
	AwaitingRequester = "awaiting_requester"
	Delivered         = "delivered"
	Stopped           = "stopped"
)

// Account is someone on the tracker: the person who filed an issue, an
// operator, or the account the engine's own credential belongs to.
type Account struct {
	ID int64
	// Login is the account's name where the tracker addresses accounts by
	// name; empty where it does not.
	Login string
}

// Issue is one issue as the engine reads it from the tracker's own record.
type Issue struct {
	// ID orders the queue and names the request's directory.
	ID int64
	// Key is what the tracker's API, the roles and the log call the issue.
	Key string
	// Created is when the issue was filed, zero when the record does not say.
	Created time.Time
	// Creator filed the issue: the requester.
	Creator Account
	// Raw is the record exactly as the tracker gave it.
	Raw json.RawMessage
}

// Comment is one comment on an issue.
type Comment struct {
	ID     int64
	Body   string
	Author Account
	// OnIssue says the record names the issue it was read for as its own.
	// A comment it places elsewhere is nobody's instruction or answer.
	OnIssue bool
}

// Tracker is what the engine asks of the tracker that holds its requests.
// The tracker's records, their field names and its API stay behind it; the
// engine keeps the records as they came and decides what they mean.
type Tracker interface {
	// Identity names the tracker and the part of it that a queue belongs to.
	Identity() string
	// CredentialEnv names the environment variable holding the account's
	// credential, so the engine can keep that value out of what it writes.
	CredentialEnv() string
	// Issues lists the issues in the configured scope, oldest first, each as
	// the tracker's own record. A list that cannot be read whole is not
	// returned at all.
	Issues(ctx context.Context) ([]json.RawMessage, error)
	// ReadIssue reads one issue record. A record without an id or a key, or
	// one from outside the configured scope, is refused.
	ReadIssue(raw json.RawMessage) (Issue, error)
	// Marked says whether the issue carries one of the marks the operator
	// chose for the engine to take issues up by. With none chosen, every
	// issue does.
	Marked(issue Issue) bool
	// Request reads one issue by its key and renders it as the request text.
	Request(ctx context.Context, key string) (string, error)
	// RequestText renders an issue record as the request text the engine
	// works from. The text is part of each request's history, so it is the
	// same for the same record every time.
	RequestText(raw json.RawMessage) (string, error)
	// Comments reads every comment on the issue, oldest first, each as the
	// tracker's own record.
	Comments(ctx context.Context, issue Issue) ([]json.RawMessage, error)
	// ReadComment reads one comment record. A record without an id is refused.
	ReadComment(raw json.RawMessage, issue Issue) (Comment, error)
	// CommentText reads only a comment record's id and words, which is all
	// the engine needs to find a comment of its own. A record without an id
	// is refused; nothing else in it is looked at.
	CommentText(raw json.RawMessage) (int64, string, error)
	// AddComment posts the text once and returns the new comment's id. A
	// failure may still have posted it: read the comments before trying again.
	AddComment(ctx context.Context, issue Issue, text string) (int64, error)
	// Myself is the account the credential belongs to.
	Myself(ctx context.Context) (Account, error)
	// Target is what the issue is set to at a turn of the work, written the
	// way the request's records keep it, or nil when the operator set nothing
	// for that turn. The engine compares it with what it recorded last; it
	// does not read it.
	Target(turn string) json.RawMessage
	// Move sets the issue to the turn's target.
	Move(ctx context.Context, issue Issue, turn string) error
	// Assign hands the issue to the account.
	Assign(ctx context.Context, issue Issue, to Account) error
	// RecordHours records on the issue how long the work took.
	RecordHours(ctx context.Context, issue Issue, hours float64) error
	// Forward sends one request that an issue scope has already checked,
	// with the account's own credential, and returns the tracker's answer.
	Forward(ctx context.Context, method, path string, query, form url.Values, expected int) ([]byte, error)
}
