package hook

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const SnapshotSchemaVersion = 2

var (
	componentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}$`)
	runIDPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{7,127}$`)
	digestPattern    = regexp.MustCompile(`^[a-f0-9]{64}$`)
	deliveryPattern  = regexp.MustCompile(`^delivery_[a-f0-9]{32}$`)
	safeCodePattern  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
)

type Config struct {
	SpaceKey            string
	ProjectID           int64
	ProjectKey          string
	AllowedCreatorID    int64
	AllowedActivityType int
	// RequiredCategoryID admits only issues carrying this tracker category.
	// Zero disables the gate and every issue passing the other allowlists is
	// queued - the behaviour before the gate existed.
	RequiredCategoryID int64
	RunMarker          string
	ExpectedRunID      string
	Target             DeliveryTarget
	MaxEnvelopeBytes   int
}

func (c Config) Validate() error {
	if !componentPattern.MatchString(c.SpaceKey) {
		return errors.New("space key is invalid")
	}
	if c.ProjectID <= 0 || !componentPattern.MatchString(c.ProjectKey) {
		return errors.New("project allowlist is invalid")
	}
	if c.AllowedCreatorID <= 0 || c.AllowedActivityType <= 0 {
		return errors.New("creator or activity allowlist is empty")
	}
	if c.RequiredCategoryID < 0 {
		return errors.New("required category id is invalid")
	}
	if c.AllowedActivityType == commentActivityType {
		// The answer signal turns comments into ticks, and a tick's lost-ingest
		// sweep feeds allowed activities back into Process. If both were the
		// same type the two would feed each other forever.
		return errors.New("allowed activity type must not be the comment type")
	}
	if c.RunMarker == "" || strings.TrimSpace(c.RunMarker) != c.RunMarker || strings.ContainsAny(c.RunMarker, ":\r\n") {
		return errors.New("run marker is invalid")
	}
	if !runIDPattern.MatchString(c.ExpectedRunID) {
		return errors.New("expected run id is invalid")
	}
	if err := c.Target.Validate(); err != nil {
		return err
	}
	if c.MaxEnvelopeBytes <= 0 || c.MaxEnvelopeBytes >= 64*1024 {
		return errors.New("envelope byte limit must be between 1 and 65535")
	}
	return nil
}

type DeliveryTarget struct {
	RepositoryID      int64  `json:"repository_id"`
	WorkflowRefSHA256 string `json:"workflow_ref_sha256"`
}

func (t DeliveryTarget) Validate() error {
	if t.RepositoryID <= 0 || !validIdentityDigest(t.WorkflowRefSHA256) {
		return errors.New("delivery target is invalid")
	}
	return nil
}

// WebhookHint contains untrusted routing hints from Backlog. Every field is
// compared with the activity returned by the Backlog API before it is used.
type WebhookHint struct {
	ActivityID   int64
	ActivityType int
	ProjectID    int64
	ProjectKey   string
	CreatorID    int64
	IssueID      int64
	IssueKeyID   int64
}

func (h WebhookHint) ValidateShape() error {
	if h.ActivityID <= 0 || h.ActivityType <= 0 || h.ProjectID <= 0 || h.CreatorID <= 0 || h.IssueID <= 0 || h.IssueKeyID <= 0 {
		return errors.New("webhook identifiers must be positive")
	}
	if !componentPattern.MatchString(h.ProjectKey) {
		return errors.New("webhook project key is invalid")
	}
	return nil
}

// CanonicalActivity is the immutable issue-created activity fetched from the
// Backlog API. Summary and description come from the activity, not the mutable
// current issue.
type CanonicalActivity struct {
	ID          int64
	Type        int
	ProjectID   int64
	ProjectKey  string
	CreatorID   int64
	IssueID     int64
	IssueKeyID  int64
	Summary     string
	Description string
	CreatedAt   time.Time
}

// CanonicalIssue is fetched only to confirm the current issue identity. Mutable
// issue content is deliberately excluded from the dispatch snapshot.
type CanonicalIssue struct {
	ID          int64
	ProjectID   int64
	IssueKey    string
	KeyID       int64
	CreatorID   int64
	CategoryIDs []int64
	CreatedAt   time.Time
}

type UntrustedTicketData struct {
	Summary     string `json:"summary"`
	Description string `json:"description"`
}

type TicketSnapshot struct {
	SchemaVersion int                 `json:"schema_version"`
	DeliveryID    string              `json:"delivery_id"`
	SpaceKey      string              `json:"space_key"`
	ActivityID    int64               `json:"activity_id"`
	ActivityType  int                 `json:"activity_type"`
	ProjectID     int64               `json:"project_id"`
	ProjectKey    string              `json:"project_key"`
	IssueID       int64               `json:"issue_id"`
	IssueKey      string              `json:"issue_key"`
	IssueKeyID    int64               `json:"issue_key_id"`
	CreatorID     int64               `json:"creator_id"`
	RunID         string              `json:"run_id"`
	CreatedAt     time.Time           `json:"created_at"`
	InputSHA256   string              `json:"input_sha256"`
	Target        DeliveryTarget      `json:"target"`
	Untrusted     UntrustedTicketData `json:"untrusted_ticket_data"`
}

// MaxDeliveredEnvelopeBytes bounds an envelope as delivered to the worker:
// the sealed webhook envelope (under 64KB by the ingress cap) plus the sealed
// clarification record (up to MaxClarificationRecordBytes, roughly doubled by
// JSON string escaping when embedded). Every reader on the delivery path uses
// this bound so a legitimately sealed resume can never exceed what its
// consumers accept.
const MaxDeliveredEnvelopeBytes = 64*1024 + 2*MaxClarificationRecordBytes + 4*1024

type DispatchEnvelope struct {
	DeliveryID string         `json:"delivery_id"`
	Snapshot   TicketSnapshot `json:"snapshot"`
	// ClarificationJSON carries the sealed cumulative clarification record of
	// a resumed run (canonical bytes of hook.ClarificationRecord) so the
	// worker sees the adopted answers. It lives outside Snapshot and is
	// omitted when empty: a never-resumed envelope keeps byte-identical
	// encoding, so the snapshot digest, the delivery ID derivation and every
	// stored-envelope equality pin are unaffected.
	ClarificationJSON string `json:"clarification_json,omitempty"`
}

// SealSnapshot binds an immutable snapshot digest and deterministic delivery
// ID to the envelope. Callers provide identity and untrusted ticket data but
// cannot choose either binding value.
func SealSnapshot(snapshot TicketSnapshot) (DispatchEnvelope, error) {
	snapshot.DeliveryID = ""
	snapshot.InputSHA256 = ""
	digest, err := snapshotDigest(snapshot)
	if err != nil {
		return DispatchEnvelope{}, err
	}
	snapshot.InputSHA256 = digest
	snapshot.DeliveryID = makeDeliveryID(snapshot)
	envelope := DispatchEnvelope{DeliveryID: snapshot.DeliveryID, Snapshot: snapshot}
	if err := ValidateEnvelope(envelope); err != nil {
		return DispatchEnvelope{}, err
	}
	return envelope, nil
}

type BacklogClient interface {
	GetActivity(context.Context, int64) (CanonicalActivity, error)
	GetIssue(context.Context, int64) (CanonicalIssue, error)
}

type QueueRequest struct {
	Envelope DispatchEnvelope
	QueuedAt time.Time
}

type QueueDisposition string

const (
	QueueCreated   QueueDisposition = "created"
	QueueDuplicate QueueDisposition = "duplicate"
	QueueClaimed   QueueDisposition = "claimed"
	QueueConflict  QueueDisposition = "conflict"
)

type PullOwner struct {
	RepositoryID      int64
	RepositorySHA256  string
	WorkflowRefSHA256 string
	WorkflowSHA       string
	WorkflowRunID     int64
	RunAttempt        int
}

type PullClaimRequest struct {
	SpaceKey            string
	ProjectID           int64
	ProjectKey          string
	AllowedCreatorID    int64
	AllowedActivityType int
	RunID               string
	Target              DeliveryTarget
	Owner               PullOwner
	IssuedAt            time.Time
	ClaimedAt           time.Time
	ClockSkew           time.Duration
}

type PullDisposition string

const (
	PullAcquired PullDisposition = "acquired"
	PullEmpty    PullDisposition = "empty"
	PullClaimed  PullDisposition = "claimed"
	PullConflict PullDisposition = "conflict"
)

// QueueStore binds both the immutable Backlog activity and one fixed run ID to
// one envelope. Pull moves that envelope from queued to a permanent claimed
// state. Only an exact-current-owner retransmission can receive it again; there
// is intentionally no lease, delete, reset, or cross-owner reclaim operation.
type QueueStore interface {
	Enqueue(context.Context, QueueRequest) (QueueDisposition, error)
	Pull(context.Context, PullClaimRequest) (DispatchEnvelope, PullDisposition, error)
}

type Decision string

const (
	DecisionAccepted         Decision = "accepted"
	DecisionIgnored          Decision = "ignored"
	DecisionInvalid          Decision = "invalid"
	DecisionRetryRequested   Decision = "retry_requested"
	DecisionDependencyFailed Decision = "dependency_failed"
	DecisionInternal         Decision = "internal_error"
)

type Result struct {
	Decision   Decision `json:"decision"`
	Code       string   `json:"code"`
	DeliveryID string   `json:"delivery_id,omitempty"`
}

type FailureClass string

const (
	FailureRetryable FailureClass = "retryable"
	FailureRejected  FailureClass = "rejected"
	FailureUnknown   FailureClass = "unknown"
)

type ExternalFailure struct {
	service string
	class   FailureClass
	code    string
}

func NewExternalFailure(service string, class FailureClass, code string) *ExternalFailure {
	if !safeCodePattern.MatchString(service) || !safeCodePattern.MatchString(code) {
		return &ExternalFailure{service: "external", class: FailureUnknown, code: "unexpected_failure"}
	}
	switch class {
	case FailureRetryable, FailureRejected, FailureUnknown:
	default:
		class = FailureUnknown
		code = "unexpected_failure"
	}
	return &ExternalFailure{service: service, class: class, code: code}
}

func (e *ExternalFailure) Error() string {
	return fmt.Sprintf("%s: %s", e.service, e.code)
}

func FailureDetails(err error) (FailureClass, string) {
	var failure *ExternalFailure
	if errors.As(err, &failure) {
		return failure.class, failure.code
	}
	return FailureUnknown, "unexpected_failure"
}

func validDigest(value string) bool {
	return digestPattern.MatchString(value)
}

func validIdentityDigest(value string) bool {
	return validDigest(value) && value != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
}

func validDeliveryID(value string) bool {
	return deliveryPattern.MatchString(value)
}

// ValidRunID reports whether a value is a well-formed run identifier. The run
// is named after the ticket, so this is a shape check, not an allowlist.
func ValidRunID(value string) bool { return runIDPattern.MatchString(value) }
