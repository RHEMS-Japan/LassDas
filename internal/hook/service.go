package hook

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"
)

type Service struct {
	config     Config
	backlog    BacklogClient
	store      QueueStore
	logger     *slog.Logger
	dispatcher WorkDispatcher
	answerTick QuestionTickProcessor
	board      BoardProjector
	now        func() time.Time
}

// UseBoard mirrors accepted tickets onto the board humans watch. The board is
// a projection: leaving this unset changes nothing but visibility.
func (s *Service) UseBoard(board BoardProjector) { s.board = board }

func NewService(config Config, backlog BacklogClient, store QueueStore, logger *slog.Logger) (*Service, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if backlog == nil || store == nil || logger == nil {
		return nil, errors.New("service dependencies must not be nil")
	}
	return &Service{
		config:  config,
		backlog: backlog,
		store:   store,
		logger:  logger,
		now:     time.Now,
	}, nil
}

func (s *Service) Process(ctx context.Context, hint WebhookHint) Result {
	if err := hint.ValidateShape(); err != nil {
		return s.result(DecisionInvalid, "invalid_webhook_shape", hint, "", "")
	}

	// A comment is a wake-up signal for a waiting question, not ticket input.
	// It short-circuits before any Backlog lookup: the tick reads sealed state
	// and the authoritative comment list itself.
	if hint.ActivityType == commentActivityType && s.answerTick != nil {
		return s.processAnswerSignal(ctx, hint)
	}

	activity, err := s.backlog.GetActivity(ctx, hint.ActivityID)
	if err != nil {
		return s.externalResult("activity_lookup", err, hint)
	}
	if !sameActivity(hint, activity) {
		return s.result(DecisionIgnored, "activity_mismatch", hint, "", "")
	}
	if !s.allowedActivity(activity) {
		return s.result(DecisionIgnored, "activity_not_allowed", hint, "", "")
	}

	issue, err := s.backlog.GetIssue(ctx, activity.IssueID)
	if err != nil {
		// An issue deleted after its creation activity answers 404 forever.
		// That is a settled fact, not a dependency outage: there is nothing
		// left to process, and treating it as retryable parked the lost-ingest
		// sweep on a deleted test ticket for three days (measured 2026-08-13
		// on a live activity). Every other rejection - auth failures above
		// all - still stalls, so an outage can never skip real tickets.
		if class, kind := FailureDetails(err); class == FailureRejected && kind == "not_found" {
			return s.result(DecisionIgnored, "issue_gone", hint, "", "")
		}
		return s.externalResult("issue_lookup", err, hint)
	}
	if !s.allowedIssue(activity, issue) {
		return s.result(DecisionIgnored, "issue_not_allowed", hint, "", "")
	}
	if !s.markedForAutomation(issue) {
		return s.result(DecisionIgnored, "category_not_allowed", hint, issue.IssueKey, "")
	}

	// The run is the ticket. Asking the requester to copy a fixed string into
	// the body was both a format requirement and a limit of one ticket for the
	// lifetime of the deployment: the record key is built from this value, so a
	// second ticket carrying the same string collided and was dropped without a
	// word. The ticket already has a name, so that is what identifies the run.
	runID := issue.IssueKey
	if !runIDPattern.MatchString(runID) {
		return s.result(DecisionIgnored, "run_id_not_allowed", hint, issue.IssueKey, "")
	}

	snapshot, err := s.snapshot(activity, issue, runID)
	if err != nil {
		return s.result(DecisionInternal, "snapshot_failed", hint, issue.IssueKey, "")
	}
	envelope, err := SealSnapshot(snapshot)
	if err != nil {
		return s.result(DecisionInternal, "snapshot_failed", hint, issue.IssueKey, "")
	}
	deliveryID := envelope.DeliveryID
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return s.result(DecisionInternal, "envelope_encode_failed", hint, issue.IssueKey, deliveryID)
	}
	if len(encoded) > s.config.MaxEnvelopeBytes {
		return s.result(DecisionIgnored, "envelope_too_large", hint, issue.IssueKey, deliveryID)
	}

	disposition, err := s.store.Enqueue(ctx, QueueRequest{Envelope: envelope, QueuedAt: s.now().UTC()})
	if err != nil {
		return s.result(DecisionRetryRequested, "queue_failed", hint, issue.IssueKey, deliveryID)
	}
	switch disposition {
	case QueueCreated:
		s.dispatchWork(ctx, deliveryID)
		projectBoard(ctx, s.board, s.logger, envelope.Snapshot.IssueID, BoardRunning)
		return s.result(DecisionAccepted, "queue_created", hint, issue.IssueKey, deliveryID)
	case QueueDuplicate:
		return s.result(DecisionAccepted, "duplicate_queued", hint, issue.IssueKey, deliveryID)
	case QueueClaimed:
		return s.result(DecisionAccepted, "duplicate_claimed", hint, issue.IssueKey, deliveryID)
	case QueueConflict:
		return s.result(DecisionIgnored, "queue_conflict", hint, issue.IssueKey, deliveryID)
	default:
		return s.result(DecisionInternal, "queue_state_invalid", hint, issue.IssueKey, deliveryID)
	}
}

func (s *Service) allowedActivity(activity CanonicalActivity) bool {
	return activity.ID > 0 &&
		activity.Type == s.config.AllowedActivityType &&
		activity.ProjectID == s.config.ProjectID &&
		activity.ProjectKey == s.config.ProjectKey &&
		activity.CreatorID == s.config.AllowedCreatorID &&
		activity.IssueID > 0 &&
		activity.IssueKeyID > 0 &&
		!activity.CreatedAt.IsZero()
}

func (s *Service) allowedIssue(activity CanonicalActivity, issue CanonicalIssue) bool {
	expectedKey := s.config.ProjectKey + "-" + strconv.FormatInt(activity.IssueKeyID, 10)
	return issue.ID == activity.IssueID &&
		issue.ProjectID == s.config.ProjectID &&
		issue.CreatorID == s.config.AllowedCreatorID &&
		issue.KeyID == activity.IssueKeyID &&
		issue.IssueKey == expectedKey &&
		!issue.CreatedAt.IsZero()
}

// markedForAutomation keeps the automation opt-in: without it every issue the
// allowed creator files is claimed and worked on, including notes never meant
// for delivery (measured 2026-08-10 right after cutover).
func (s *Service) markedForAutomation(issue CanonicalIssue) bool {
	if s.config.RequiredCategoryID == 0 {
		return true
	}
	for _, id := range issue.CategoryIDs {
		if id == s.config.RequiredCategoryID {
			return true
		}
	}
	return false
}

func sameActivity(hint WebhookHint, activity CanonicalActivity) bool {
	return hint.ActivityID == activity.ID &&
		hint.ActivityType == activity.Type &&
		hint.ProjectID == activity.ProjectID &&
		hint.ProjectKey == activity.ProjectKey &&
		hint.CreatorID == activity.CreatorID &&
		hint.IssueID == activity.IssueID &&
		hint.IssueKeyID == activity.IssueKeyID
}

func (s *Service) snapshot(activity CanonicalActivity, issue CanonicalIssue, runID string) (TicketSnapshot, error) {
	snapshot := TicketSnapshot{
		SchemaVersion: SnapshotSchemaVersion,
		SpaceKey:      s.config.SpaceKey,
		ActivityID:    activity.ID,
		ActivityType:  activity.Type,
		ProjectID:     activity.ProjectID,
		ProjectKey:    activity.ProjectKey,
		IssueID:       activity.IssueID,
		IssueKey:      issue.IssueKey,
		IssueKeyID:    activity.IssueKeyID,
		CreatorID:     activity.CreatorID,
		RunID:         runID,
		CreatedAt:     activity.CreatedAt.UTC(),
		Target:        s.config.Target,
		Untrusted: UntrustedTicketData{
			Summary:     activity.Summary,
			Description: activity.Description,
		},
	}
	return snapshot, nil
}

func snapshotDigest(snapshot TicketSnapshot) (string, error) {
	snapshot.DeliveryID = ""
	snapshot.InputSHA256 = ""
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func makeDeliveryID(snapshot TicketSnapshot) string {
	value := fmt.Sprintf("%s\x00%d\x00%d\x00%s\x00%s", snapshot.SpaceKey, snapshot.ProjectID, snapshot.ActivityID, snapshot.RunID, snapshot.InputSHA256)
	digest := sha256.Sum256([]byte(value))
	return "delivery_" + hex.EncodeToString(digest[:16])
}

func ValidateEnvelope(envelope DispatchEnvelope) error {
	if !validDeliveryID(envelope.DeliveryID) || envelope.Snapshot.DeliveryID != envelope.DeliveryID {
		return errors.New("delivery id is invalid")
	}
	if envelope.Snapshot.SchemaVersion != SnapshotSchemaVersion || !validDigest(envelope.Snapshot.InputSHA256) {
		return errors.New("snapshot metadata is invalid")
	}
	if !componentPattern.MatchString(envelope.Snapshot.SpaceKey) ||
		envelope.Snapshot.ActivityID <= 0 || envelope.Snapshot.ActivityType <= 0 ||
		envelope.Snapshot.ProjectID <= 0 || !componentPattern.MatchString(envelope.Snapshot.ProjectKey) ||
		envelope.Snapshot.IssueID <= 0 || envelope.Snapshot.IssueKeyID <= 0 ||
		envelope.Snapshot.IssueKey != envelope.Snapshot.ProjectKey+"-"+strconv.FormatInt(envelope.Snapshot.IssueKeyID, 10) ||
		envelope.Snapshot.CreatorID <= 0 || !runIDPattern.MatchString(envelope.Snapshot.RunID) ||
		envelope.Snapshot.CreatedAt.IsZero() {
		return errors.New("snapshot identity is invalid")
	}
	if err := envelope.Snapshot.Target.Validate(); err != nil {
		return errors.New("snapshot target is invalid")
	}
	digest, err := snapshotDigest(envelope.Snapshot)
	if err != nil || digest != envelope.Snapshot.InputSHA256 {
		return errors.New("snapshot digest is invalid")
	}
	if makeDeliveryID(envelope.Snapshot) != envelope.DeliveryID {
		return errors.New("delivery binding is invalid")
	}
	if envelope.ClarificationJSON != "" {
		record, err := DecodeClarificationRecord([]byte(envelope.ClarificationJSON))
		if err != nil || record.DeliveryID != envelope.DeliveryID ||
			record.InputSHA256 != envelope.Snapshot.InputSHA256 ||
			record.AutomationRunID != envelope.Snapshot.RunID {
			return errors.New("clarification binding is invalid")
		}
	}
	return nil
}

func (s *Service) externalResult(operation string, err error, hint WebhookHint) Result {
	class, _ := FailureDetails(err)
	switch class {
	case FailureRejected:
		return s.result(DecisionDependencyFailed, operation+"_rejected", hint, "", "")
	case FailureRetryable, FailureUnknown:
		return s.result(DecisionRetryRequested, operation+"_failed", hint, "", "")
	default:
		return s.result(DecisionInternal, operation+"_internal", hint, "", "")
	}
}

func (s *Service) result(decision Decision, code string, hint WebhookHint, issueKey, deliveryID string) Result {
	attributes := []any{
		"decision", decision,
		"code", code,
		"activity_id", hint.ActivityID,
	}
	if issueKey != "" {
		attributes = append(attributes, "issue_key", issueKey)
	}
	if deliveryID != "" {
		attributes = append(attributes, "delivery_id", deliveryID)
	}
	s.logger.Info("hook decision", attributes...)
	return Result{Decision: decision, Code: code, DeliveryID: deliveryID}
}
