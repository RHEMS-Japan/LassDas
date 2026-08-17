package state

import (
	"context"
	"errors"
	"strconv"

	"automation.internal/ticket-ingress/internal/hook"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// BeginReply acquires the exclusive right to post one intake reply (format
// guidance or shortfall re-listing). It follows the notification marker
// pattern: a per-marker conditional Put gated on the run still waiting for
// the answer, a lease for the posting window, and CompleteReply binding the
// observed comment. The guidance marker key carries no trigger, so a second
// guidance for the same revision is structurally impossible.
func (s *DynamoStore) BeginReply(ctx context.Context, request hook.ReplyBeginRequest) (hook.TerminalBinding, hook.ReplyBeginDisposition, error) {
	if !validReplyBeginRequest(request) {
		return hook.TerminalBinding{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "invalid_reply_begin")
	}
	binding, err := s.loadTerminalBinding(ctx, request.Record.AutomationRunID, request.Route)
	if err != nil {
		return hook.TerminalBinding{}, "", err
	}
	if !questionBindingMatches(binding, request.Record, request.Route) {
		return hook.TerminalBinding{}, hook.ReplyBeginConflict, nil
	}
	result := hook.TerminalBinding{IssueID: binding.envelope.Snapshot.IssueID, IssueKey: binding.envelope.Snapshot.IssueKey}
	markerKey := replyMarkerKey(binding.runKey, request.Record.QuestionRevision, request.Kind, request.TriggerCommentID)
	marker, err := s.loadNotifyMarker(ctx, markerKey)
	if err != nil {
		return hook.TerminalBinding{}, "", err
	}
	if len(marker) > 0 {
		if !replyMarkerMatches(marker, binding.runKey, request.RecordSHA256, request.Kind) {
			return result, hook.ReplyBeginConflict, nil
		}
		if commentID, ok := attributeInt64(marker, "reply_comment_id"); ok && commentID > 0 {
			return result, hook.ReplyBeginComplete, nil
		}
		if !notifyRunStillWaiting(binding.runItem, request.RecordSHA256) {
			return result, hook.ReplyBeginConflict, nil
		}
		leaseUntil, ok := attributeInt64(marker, "reply_lease_until")
		if !ok {
			return hook.TerminalBinding{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "reply_marker_invalid")
		}
		if leaseUntil >= request.StartedAt.UnixMilli() {
			return result, hook.ReplyBeginBusy, nil
		}
		return s.reacquireReply(ctx, binding, request, result, markerKey, leaseUntil)
	}
	if !notifyRunStillWaiting(binding.runItem, request.RecordSHA256) {
		return result, hook.ReplyBeginConflict, nil
	}
	markerItem := map[string]types.AttributeValue{
		"pk":                     stringValue(markerKey),
		"record_type":            stringValue("question_reply"),
		"run_key":                stringValue(binding.runKey),
		"question_record_sha256": stringValue(request.RecordSHA256),
		"reply_kind":             stringValue(string(request.Kind)),
		"trigger_comment_id":     numberValue(request.TriggerCommentID),
		"content_sha256":         stringValue(request.ContentSHA256),
		"reply_started_at":       numberValue(request.StartedAt.UnixMilli()),
		"reply_lease_until":      numberValue(request.LeaseUntil.UnixMilli()),
		"reply_lease_token":      stringValue(request.LeaseToken),
	}
	_, err = s.api.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
		terminalEventCondition(s.table, binding),
		notifyRunGateCondition(s.table, binding.runKey, request.RecordSHA256),
		{Put: &types.Put{
			TableName: aws.String(s.table), Item: markerItem,
			ConditionExpression:      aws.String("attribute_not_exists(#pk)"),
			ExpressionAttributeNames: map[string]string{"#pk": "pk"},
		}},
	}})
	if err == nil {
		return result, hook.ReplyBeginAcquired, nil
	}
	return s.replyBeginAfterWriteFailure(ctx, request, result, err)
}

// CompleteReply binds the observed Backlog comment to the reply marker. Like
// the notification complete, it pins the marker alone so a posted comment is
// never orphaned when the run moves on mid-flight.
func (s *DynamoStore) CompleteReply(ctx context.Context, request hook.ReplyCompleteRequest) (hook.ReplyCompleteDisposition, error) {
	if !validReplyCompleteRequest(request) {
		return "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "invalid_reply_complete")
	}
	runKey := makeKey("run", request.Route.SpaceKey, strconv.FormatInt(request.Route.ProjectID, 10), request.Record.AutomationRunID)
	markerKey := replyMarkerKey(runKey, request.Record.QuestionRevision, request.Kind, request.TriggerCommentID)
	marker, err := s.loadNotifyMarker(ctx, markerKey)
	if err != nil {
		return "", err
	}
	if len(marker) == 0 || !replyMarkerMatches(marker, runKey, request.RecordSHA256, request.Kind) {
		return hook.ReplyCompleteConflict, nil
	}
	if commentID, ok := attributeInt64(marker, "reply_comment_id"); ok {
		if commentID == request.CommentID {
			return hook.ReplyAlreadyComplete, nil
		}
		return hook.ReplyCompleteConflict, nil
	}
	if !attributeStringEquals(marker, "reply_lease_token", request.LeaseToken) ||
		!attributeStringEquals(marker, "content_sha256", request.ContentSHA256) {
		return hook.ReplyCompleteConflict, nil
	}
	startedAt, ok := attributeInt64(marker, "reply_started_at")
	if !ok || request.PostedAt.UnixMilli() < startedAt {
		return "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "invalid_reply_complete")
	}
	_, err = s.api.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
		{Update: &types.Update{
			TableName: aws.String(s.table), Key: map[string]types.AttributeValue{"pk": stringValue(markerKey)},
			UpdateExpression:    aws.String("SET #reply_comment_id = :reply_comment_id, #reply_posted_at = :reply_posted_at REMOVE #reply_lease_until, #reply_lease_token"),
			ConditionExpression: aws.String("#record_type = :reply_type AND #run_key = :marker_run_key AND #question_record_digest = :question_record_digest AND #reply_kind = :reply_kind AND #content_digest = :content_digest AND #reply_started_at = :reply_started_at AND #reply_lease_token = :reply_lease_token AND attribute_not_exists(#reply_comment_id) AND attribute_not_exists(#reply_posted_at)"),
			ExpressionAttributeNames: map[string]string{
				"#record_type": "record_type", "#run_key": "run_key",
				"#question_record_digest": "question_record_sha256", "#reply_kind": "reply_kind",
				"#content_digest": "content_sha256", "#reply_started_at": "reply_started_at",
				"#reply_lease_token": "reply_lease_token", "#reply_comment_id": "reply_comment_id",
				"#reply_posted_at": "reply_posted_at", "#reply_lease_until": "reply_lease_until",
			},
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":reply_type": stringValue("question_reply"), ":marker_run_key": stringValue(runKey),
				":question_record_digest": stringValue(request.RecordSHA256), ":reply_kind": stringValue(string(request.Kind)),
				":content_digest": stringValue(request.ContentSHA256), ":reply_started_at": numberValue(startedAt),
				":reply_lease_token": stringValue(request.LeaseToken),
				":reply_comment_id":  numberValue(request.CommentID), ":reply_posted_at": numberValue(request.PostedAt.UnixMilli()),
			},
		}},
	}})
	if err == nil {
		return hook.ReplyCompleted, nil
	}
	var canceled *types.TransactionCanceledException
	if !errors.As(err, &canceled) || !onlyConditionalCancellation(canceled) {
		if hasCancellationReason(canceled, "ValidationError") {
			return "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "reply_complete_write_rejected")
		}
		return "", hook.NewExternalFailure("dynamodb", hook.FailureRetryable, "reply_complete_write_failed")
	}
	latest, loadErr := s.loadNotifyMarker(ctx, markerKey)
	if loadErr != nil {
		return "", loadErr
	}
	if replyMarkerMatches(latest, runKey, request.RecordSHA256, request.Kind) &&
		attributeInt64Equals(latest, "reply_comment_id", request.CommentID) {
		return hook.ReplyAlreadyComplete, nil
	}
	if replyMarkerMatches(latest, runKey, request.RecordSHA256, request.Kind) &&
		attributeStringEquals(latest, "reply_lease_token", request.LeaseToken) {
		return "", hook.NewExternalFailure("dynamodb", hook.FailureRetryable, "reply_complete_write_failed")
	}
	return hook.ReplyCompleteConflict, nil
}

// ReplyState reports whether the marker already binds a posted reply comment.
func (s *DynamoStore) ReplyState(ctx context.Context, route hook.ReportRouteConfig, record hook.QuestionRecord, kind hook.ReplyKind, triggerCommentID int64) (bool, error) {
	if route.Validate() != nil || record.ValidateShape() != nil || !validReplyKind(kind) {
		return false, hook.NewExternalFailure("dynamodb", hook.FailureRejected, "invalid_reply_state")
	}
	runKey := makeKey("run", route.SpaceKey, strconv.FormatInt(route.ProjectID, 10), record.AutomationRunID)
	marker, err := s.loadNotifyMarker(ctx, replyMarkerKey(runKey, record.QuestionRevision, kind, triggerCommentID))
	if err != nil {
		return false, err
	}
	commentID, ok := attributeInt64(marker, "reply_comment_id")
	return ok && commentID > 0, nil
}

func (s *DynamoStore) reacquireReply(ctx context.Context, binding terminalStoredBinding, request hook.ReplyBeginRequest, result hook.TerminalBinding, markerKey string, previousLease int64) (hook.TerminalBinding, hook.ReplyBeginDisposition, error) {
	_, err := s.api.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
		terminalEventCondition(s.table, binding),
		notifyRunGateCondition(s.table, binding.runKey, request.RecordSHA256),
		{Update: &types.Update{
			TableName: aws.String(s.table), Key: map[string]types.AttributeValue{"pk": stringValue(markerKey)},
			UpdateExpression:    aws.String("SET #reply_lease_until = :reply_lease_until, #reply_lease_token = :reply_lease_token, #content_digest = :content_digest, #trigger_comment_id = :trigger_comment_id"),
			ConditionExpression: aws.String("#record_type = :reply_type AND #run_key = :marker_run_key AND #question_record_digest = :question_record_digest AND #reply_kind = :reply_kind AND #reply_lease_until = :previous_reply_lease AND attribute_not_exists(#reply_comment_id) AND attribute_not_exists(#reply_posted_at)"),
			ExpressionAttributeNames: map[string]string{
				"#record_type": "record_type", "#run_key": "run_key",
				"#question_record_digest": "question_record_sha256", "#reply_kind": "reply_kind",
				"#reply_lease_until": "reply_lease_until", "#reply_lease_token": "reply_lease_token",
				"#content_digest": "content_sha256", "#trigger_comment_id": "trigger_comment_id",
				"#reply_comment_id": "reply_comment_id", "#reply_posted_at": "reply_posted_at",
			},
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":reply_type": stringValue("question_reply"), ":marker_run_key": stringValue(binding.runKey),
				":question_record_digest": stringValue(request.RecordSHA256), ":reply_kind": stringValue(string(request.Kind)),
				":reply_lease_until":    numberValue(request.LeaseUntil.UnixMilli()),
				":reply_lease_token":    stringValue(request.LeaseToken),
				":content_digest":       stringValue(request.ContentSHA256),
				":trigger_comment_id":   numberValue(request.TriggerCommentID),
				":previous_reply_lease": numberValue(previousLease),
			},
		}},
	}})
	if err == nil {
		return result, hook.ReplyBeginAcquired, nil
	}
	return s.replyBeginAfterWriteFailure(ctx, request, result, err)
}

func (s *DynamoStore) replyBeginAfterWriteFailure(ctx context.Context, request hook.ReplyBeginRequest, result hook.TerminalBinding, writeErr error) (hook.TerminalBinding, hook.ReplyBeginDisposition, error) {
	var canceled *types.TransactionCanceledException
	if !errors.As(writeErr, &canceled) {
		return hook.TerminalBinding{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRetryable, "reply_begin_write_failed")
	}
	if !onlyConditionalCancellation(canceled) {
		if hasCancellationReason(canceled, "ValidationError") {
			return hook.TerminalBinding{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "reply_begin_write_rejected")
		}
		return hook.TerminalBinding{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRetryable, "reply_begin_write_failed")
	}
	binding, err := s.loadTerminalBinding(ctx, request.Record.AutomationRunID, request.Route)
	if err != nil {
		return hook.TerminalBinding{}, "", err
	}
	if !questionBindingMatches(binding, request.Record, request.Route) {
		return result, hook.ReplyBeginConflict, nil
	}
	markerKey := replyMarkerKey(binding.runKey, request.Record.QuestionRevision, request.Kind, request.TriggerCommentID)
	marker, err := s.loadNotifyMarker(ctx, markerKey)
	if err != nil {
		return hook.TerminalBinding{}, "", err
	}
	if len(marker) > 0 && replyMarkerMatches(marker, binding.runKey, request.RecordSHA256, request.Kind) {
		if commentID, ok := attributeInt64(marker, "reply_comment_id"); ok && commentID > 0 {
			return result, hook.ReplyBeginComplete, nil
		}
		if notifyRunStillWaiting(binding.runItem, request.RecordSHA256) {
			return result, hook.ReplyBeginBusy, nil
		}
		return result, hook.ReplyBeginConflict, nil
	}
	if len(marker) == 0 && notifyRunStillWaiting(binding.runItem, request.RecordSHA256) {
		return hook.TerminalBinding{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRetryable, "reply_begin_write_failed")
	}
	return result, hook.ReplyBeginConflict, nil
}

func replyMarkerMatches(marker map[string]types.AttributeValue, runKey, recordDigest string, kind hook.ReplyKind) bool {
	return attributeStringEquals(marker, "record_type", "question_reply") &&
		attributeStringEquals(marker, "run_key", runKey) &&
		attributeStringEquals(marker, "question_record_sha256", recordDigest) &&
		attributeStringEquals(marker, "reply_kind", string(kind))
}

func validReplyKind(kind hook.ReplyKind) bool {
	return kind == hook.ReplyGuidance || kind == hook.ReplyShortfall
}

func validReplyBeginRequest(request hook.ReplyBeginRequest) bool {
	encoded, err := hook.MarshalQuestionRecord(request.Record)
	return err == nil && request.Record.ValidateRoute(request.Route) == nil && string(encoded) == request.RecordJSON &&
		digestPattern.MatchString(request.RecordSHA256) && hook.TerminalReportDigest(encoded) == request.RecordSHA256 &&
		validReplyKind(request.Kind) && request.TriggerCommentID > 0 &&
		digestPattern.MatchString(request.ContentSHA256) &&
		leasePattern.MatchString(request.LeaseToken) && !request.StartedAt.IsZero() && request.StartedAt.Equal(request.StartedAt.UTC()) &&
		request.LeaseUntil.Equal(request.LeaseUntil.UTC()) && request.LeaseUntil.Equal(request.StartedAt.Add(request.Route.LeaseDuration))
}

func validReplyCompleteRequest(request hook.ReplyCompleteRequest) bool {
	encoded, err := hook.MarshalQuestionRecord(request.Record)
	return err == nil && request.Record.ValidateRoute(request.Route) == nil && string(encoded) == request.RecordJSON &&
		digestPattern.MatchString(request.RecordSHA256) && hook.TerminalReportDigest(encoded) == request.RecordSHA256 &&
		validReplyKind(request.Kind) && request.TriggerCommentID > 0 &&
		digestPattern.MatchString(request.ContentSHA256) &&
		leasePattern.MatchString(request.LeaseToken) && request.CommentID > 0 &&
		!request.PostedAt.IsZero() && request.PostedAt.Equal(request.PostedAt.UTC())
}

// replyMarkerKey fixes the guidance marker per revision — the "once per
// revision" guidance contract is enforced by the key itself — while shortfall
// markers are one per incomplete answer comment.
func replyMarkerKey(runKey string, revision int, kind hook.ReplyKind, triggerCommentID int64) string {
	if kind == hook.ReplyGuidance {
		return runKey + "#reply#" + strconv.Itoa(revision) + "#guidance"
	}
	return runKey + "#reply#" + strconv.Itoa(revision) + "#" + strconv.FormatInt(triggerCommentID, 10)
}
