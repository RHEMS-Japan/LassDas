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

// BeginNotify acquires the exclusive right to post renotification Index of
// the sealed question. Each notification has its own marker item keyed by run,
// question revision and index, so a notification can never be sent twice. The
// begin transaction gates on the run still waiting for the answer: after an
// adopted answer, a cancellation, an expiry or any terminal outcome the gate
// fails and no further reminder goes out.
func (s *DynamoStore) BeginNotify(ctx context.Context, request hook.NotifyBeginRequest) (hook.TerminalBinding, hook.NotifyBeginDisposition, error) {
	if !validNotifyBeginRequest(request) {
		return hook.TerminalBinding{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "invalid_notify_begin")
	}
	binding, err := s.loadTerminalBinding(ctx, request.Record.AutomationRunID, request.Route)
	if err != nil {
		return hook.TerminalBinding{}, "", err
	}
	if !questionBindingMatches(binding, request.Record, request.Route) {
		return hook.TerminalBinding{}, hook.NotifyBeginConflict, nil
	}
	result := hook.TerminalBinding{IssueID: binding.envelope.Snapshot.IssueID, IssueKey: binding.envelope.Snapshot.IssueKey}
	markerKey := notifyMarkerKey(binding.runKey, request.Record.QuestionRevision, request.Index)
	marker, err := s.loadNotifyMarker(ctx, markerKey)
	if err != nil {
		return hook.TerminalBinding{}, "", err
	}
	if len(marker) > 0 {
		if !notifyMarkerMatches(marker, binding.runKey, request.RecordSHA256, request.Index) {
			return result, hook.NotifyBeginConflict, nil
		}
		if commentID, ok := attributeInt64(marker, "notify_comment_id"); ok && commentID > 0 {
			return result, hook.NotifyBeginComplete, nil
		}
		// A dead notification — the run left the wait while the marker was
		// leased — must resolve to conflict (stop), not busy (retry later).
		if !notifyRunStillWaiting(binding.runItem, request.RecordSHA256) {
			return result, hook.NotifyBeginConflict, nil
		}
		leaseUntil, ok := attributeInt64(marker, "notify_lease_until")
		if !ok {
			return hook.TerminalBinding{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "notify_marker_invalid")
		}
		if leaseUntil >= request.StartedAt.UnixMilli() {
			return result, hook.NotifyBeginBusy, nil
		}
		return s.reacquireNotify(ctx, binding, request, result, markerKey, leaseUntil)
	}
	if !notifyRunStillWaiting(binding.runItem, request.RecordSHA256) {
		return result, hook.NotifyBeginConflict, nil
	}
	return s.startNotify(ctx, binding, request, result, markerKey)
}

// CompleteNotify binds the observed Backlog comment to the notification
// marker. It pins the marker alone: the strongly gated begin is the only path
// that creates markers, and a run that moved on while the comment was in
// flight must not orphan the evidence of a comment that was actually posted.
func (s *DynamoStore) CompleteNotify(ctx context.Context, request hook.NotifyCompleteRequest) (hook.NotifyCompleteDisposition, error) {
	if !validNotifyCompleteRequest(request) {
		return "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "invalid_notify_complete")
	}
	runKey := makeKey("run", request.Route.SpaceKey, strconv.FormatInt(request.Route.ProjectID, 10), request.Record.AutomationRunID)
	markerKey := notifyMarkerKey(runKey, request.Record.QuestionRevision, request.Index)
	marker, err := s.loadNotifyMarker(ctx, markerKey)
	if err != nil {
		return "", err
	}
	if len(marker) == 0 || !notifyMarkerMatches(marker, runKey, request.RecordSHA256, request.Index) {
		return hook.NotifyCompleteConflict, nil
	}
	if commentID, ok := attributeInt64(marker, "notify_comment_id"); ok {
		if commentID == request.CommentID {
			return hook.NotifyAlreadyComplete, nil
		}
		return hook.NotifyCompleteConflict, nil
	}
	if !attributeStringEquals(marker, "notify_lease_token", request.LeaseToken) {
		return hook.NotifyCompleteConflict, nil
	}
	startedAt, ok := attributeInt64(marker, "notify_started_at")
	if !ok || request.PostedAt.UnixMilli() < startedAt {
		return "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "invalid_notify_complete")
	}
	_, err = s.api.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
		{Update: &types.Update{
			TableName: aws.String(s.table), Key: map[string]types.AttributeValue{"pk": stringValue(markerKey)},
			UpdateExpression:    aws.String("SET #notify_comment_id = :notify_comment_id, #notify_posted_at = :notify_posted_at REMOVE #notify_lease_until, #notify_lease_token"),
			ConditionExpression: aws.String("#record_type = :notify_type AND #run_key = :marker_run_key AND #question_record_digest = :question_record_digest AND #notify_index = :notify_index AND #notify_started_at = :notify_started_at AND #notify_lease_token = :notify_lease_token AND attribute_not_exists(#notify_comment_id) AND attribute_not_exists(#notify_posted_at)"),
			ExpressionAttributeNames: map[string]string{
				"#record_type": "record_type", "#run_key": "run_key",
				"#question_record_digest": "question_record_sha256", "#notify_index": "notify_index",
				"#notify_started_at": "notify_started_at", "#notify_lease_token": "notify_lease_token",
				"#notify_comment_id": "notify_comment_id", "#notify_posted_at": "notify_posted_at",
				"#notify_lease_until": "notify_lease_until",
			},
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":notify_type": stringValue("question_notify"), ":marker_run_key": stringValue(runKey),
				":question_record_digest": stringValue(request.RecordSHA256), ":notify_index": numberValue(int64(request.Index)),
				":notify_started_at": numberValue(startedAt), ":notify_lease_token": stringValue(request.LeaseToken),
				":notify_comment_id": numberValue(request.CommentID), ":notify_posted_at": numberValue(request.PostedAt.UnixMilli()),
			},
		}},
	}})
	if err == nil {
		return hook.NotifyCompleted, nil
	}
	var canceled *types.TransactionCanceledException
	if !errors.As(err, &canceled) || !onlyConditionalCancellation(canceled) {
		if hasCancellationReason(canceled, "ValidationError") {
			return "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "notify_complete_write_rejected")
		}
		return "", hook.NewExternalFailure("dynamodb", hook.FailureRetryable, "notify_complete_write_failed")
	}
	latest, loadErr := s.loadNotifyMarker(ctx, markerKey)
	if loadErr != nil {
		return "", loadErr
	}
	if notifyMarkerMatches(latest, runKey, request.RecordSHA256, request.Index) &&
		attributeInt64Equals(latest, "notify_comment_id", request.CommentID) {
		return hook.NotifyAlreadyComplete, nil
	}
	if notifyMarkerMatches(latest, runKey, request.RecordSHA256, request.Index) &&
		attributeStringEquals(latest, "notify_lease_token", request.LeaseToken) {
		return "", hook.NewExternalFailure("dynamodb", hook.FailureRetryable, "notify_complete_write_failed")
	}
	return hook.NotifyCompleteConflict, nil
}

func (s *DynamoStore) startNotify(ctx context.Context, binding terminalStoredBinding, request hook.NotifyBeginRequest, result hook.TerminalBinding, markerKey string) (hook.TerminalBinding, hook.NotifyBeginDisposition, error) {
	markerItem := map[string]types.AttributeValue{
		"pk":                     stringValue(markerKey),
		"record_type":            stringValue("question_notify"),
		"run_key":                stringValue(binding.runKey),
		"question_record_sha256": stringValue(request.RecordSHA256),
		"question_revision":      numberValue(int64(request.Record.QuestionRevision)),
		"notify_index":           numberValue(int64(request.Index)),
		"notify_at":              numberValue(request.Record.NotifyAt[request.Index-1]),
		"notify_started_at":      numberValue(request.StartedAt.UnixMilli()),
		"notify_lease_until":     numberValue(request.LeaseUntil.UnixMilli()),
		"notify_lease_token":     stringValue(request.LeaseToken),
	}
	_, err := s.api.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
		terminalEventCondition(s.table, binding),
		notifyRunGateCondition(s.table, binding.runKey, request.RecordSHA256),
		{Put: &types.Put{
			TableName: aws.String(s.table), Item: markerItem,
			ConditionExpression:      aws.String("attribute_not_exists(#pk)"),
			ExpressionAttributeNames: map[string]string{"#pk": "pk"},
		}},
	}})
	if err == nil {
		return result, hook.NotifyBeginAcquired, nil
	}
	return s.notifyBeginAfterWriteFailure(ctx, request, result, err)
}

func (s *DynamoStore) reacquireNotify(ctx context.Context, binding terminalStoredBinding, request hook.NotifyBeginRequest, result hook.TerminalBinding, markerKey string, previousLease int64) (hook.TerminalBinding, hook.NotifyBeginDisposition, error) {
	_, err := s.api.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
		terminalEventCondition(s.table, binding),
		notifyRunGateCondition(s.table, binding.runKey, request.RecordSHA256),
		{Update: &types.Update{
			TableName: aws.String(s.table), Key: map[string]types.AttributeValue{"pk": stringValue(markerKey)},
			UpdateExpression:    aws.String("SET #notify_lease_until = :notify_lease_until, #notify_lease_token = :notify_lease_token"),
			ConditionExpression: aws.String("#record_type = :notify_type AND #run_key = :marker_run_key AND #question_record_digest = :question_record_digest AND #notify_index = :notify_index AND #notify_lease_until = :previous_notify_lease AND attribute_not_exists(#notify_comment_id) AND attribute_not_exists(#notify_posted_at)"),
			ExpressionAttributeNames: map[string]string{
				"#record_type": "record_type", "#run_key": "run_key",
				"#question_record_digest": "question_record_sha256", "#notify_index": "notify_index",
				"#notify_lease_until": "notify_lease_until", "#notify_lease_token": "notify_lease_token",
				"#notify_comment_id": "notify_comment_id", "#notify_posted_at": "notify_posted_at",
			},
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":notify_type": stringValue("question_notify"), ":marker_run_key": stringValue(binding.runKey),
				":question_record_digest": stringValue(request.RecordSHA256), ":notify_index": numberValue(int64(request.Index)),
				":notify_lease_until":    numberValue(request.LeaseUntil.UnixMilli()),
				":notify_lease_token":    stringValue(request.LeaseToken),
				":previous_notify_lease": numberValue(previousLease),
			},
		}},
	}})
	if err == nil {
		return result, hook.NotifyBeginAcquired, nil
	}
	return s.notifyBeginAfterWriteFailure(ctx, request, result, err)
}

func (s *DynamoStore) notifyBeginAfterWriteFailure(ctx context.Context, request hook.NotifyBeginRequest, result hook.TerminalBinding, writeErr error) (hook.TerminalBinding, hook.NotifyBeginDisposition, error) {
	var canceled *types.TransactionCanceledException
	if !errors.As(writeErr, &canceled) {
		return hook.TerminalBinding{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRetryable, "notify_begin_write_failed")
	}
	if !onlyConditionalCancellation(canceled) {
		if hasCancellationReason(canceled, "ValidationError") {
			return hook.TerminalBinding{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "notify_begin_write_rejected")
		}
		return hook.TerminalBinding{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRetryable, "notify_begin_write_failed")
	}
	binding, err := s.loadTerminalBinding(ctx, request.Record.AutomationRunID, request.Route)
	if err != nil {
		return hook.TerminalBinding{}, "", err
	}
	if !questionBindingMatches(binding, request.Record, request.Route) {
		return result, hook.NotifyBeginConflict, nil
	}
	markerKey := notifyMarkerKey(binding.runKey, request.Record.QuestionRevision, request.Index)
	marker, err := s.loadNotifyMarker(ctx, markerKey)
	if err != nil {
		return hook.TerminalBinding{}, "", err
	}
	if len(marker) > 0 && notifyMarkerMatches(marker, binding.runKey, request.RecordSHA256, request.Index) {
		if commentID, ok := attributeInt64(marker, "notify_comment_id"); ok && commentID > 0 {
			return result, hook.NotifyBeginComplete, nil
		}
		// Busy only while the run is genuinely still waiting; a leased marker
		// on a run that moved on is a dead notification, not contention.
		if notifyRunStillWaiting(binding.runItem, request.RecordSHA256) {
			return result, hook.NotifyBeginBusy, nil
		}
		return result, hook.NotifyBeginConflict, nil
	}
	// An unchanged waiting run with no marker means the write lost to
	// something transient: retry instead of declaring a permanent conflict.
	if len(marker) == 0 && notifyRunStillWaiting(binding.runItem, request.RecordSHA256) {
		return hook.TerminalBinding{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRetryable, "notify_begin_write_failed")
	}
	return result, hook.NotifyBeginConflict, nil
}

// notifyRunStillWaiting reports whether the run is still waiting on exactly
// the sealed question a notification belongs to.
func notifyRunStillWaiting(item map[string]types.AttributeValue, recordDigest string) bool {
	stateValue, _ := attributeString(item, "state")
	return stateValue == stateAwaitingAnswer && questionRecordMatches(item, recordDigest) &&
		validStoredQuestionCommentID(item)
}

func (s *DynamoStore) loadNotifyMarker(ctx context.Context, markerKey string) (map[string]types.AttributeValue, error) {
	output, err := s.api.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table), Key: map[string]types.AttributeValue{"pk": stringValue(markerKey)}, ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return nil, hook.NewExternalFailure("dynamodb", hook.FailureRetryable, "notify_read_failed")
	}
	return output.Item, nil
}

// notifyRunGateCondition requires the run to still be waiting on exactly the
// sealed question while a notification is acquired.
func notifyRunGateCondition(table, runKey, recordDigest string) types.TransactWriteItem {
	return types.TransactWriteItem{ConditionCheck: &types.ConditionCheck{
		TableName: aws.String(table), Key: map[string]types.AttributeValue{"pk": stringValue(runKey)},
		ConditionExpression: aws.String("#state = :awaiting_gate AND #question_record_digest = :question_record_digest AND attribute_exists(#question_comment_id)"),
		ExpressionAttributeNames: map[string]string{
			"#state": "state", "#question_record_digest": "question_record_sha256", "#question_comment_id": "question_comment_id",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":awaiting_gate":          stringValue(stateAwaitingAnswer),
			":question_record_digest": stringValue(recordDigest),
		},
	}}
}

func notifyMarkerMatches(marker map[string]types.AttributeValue, runKey, recordDigest string, index int) bool {
	return attributeStringEquals(marker, "record_type", "question_notify") &&
		attributeStringEquals(marker, "run_key", runKey) &&
		attributeStringEquals(marker, "question_record_sha256", recordDigest) &&
		attributeInt64Equals(marker, "notify_index", int64(index))
}

func validNotifyBeginRequest(request hook.NotifyBeginRequest) bool {
	encoded, err := hook.MarshalQuestionRecord(request.Record)
	return err == nil && request.Record.ValidateRoute(request.Route) == nil && string(encoded) == request.RecordJSON &&
		digestPattern.MatchString(request.RecordSHA256) && hook.TerminalReportDigest(encoded) == request.RecordSHA256 &&
		request.Index >= 1 && request.Index <= hook.QuestionNotifyCount &&
		// A pure input-to-input comparison: acquiring notification n before
		// its sealed slot (or with an index the tick decision would not have
		// chosen yet) is rejected without consulting any wall clock.
		request.StartedAt.UnixMilli() >= request.Record.NotifyAt[request.Index-1] &&
		leasePattern.MatchString(request.LeaseToken) && !request.StartedAt.IsZero() && request.StartedAt.Equal(request.StartedAt.UTC()) &&
		request.LeaseUntil.Equal(request.LeaseUntil.UTC()) && request.LeaseUntil.Equal(request.StartedAt.Add(request.Route.LeaseDuration))
}

func validNotifyCompleteRequest(request hook.NotifyCompleteRequest) bool {
	encoded, err := hook.MarshalQuestionRecord(request.Record)
	return err == nil && request.Record.ValidateRoute(request.Route) == nil && string(encoded) == request.RecordJSON &&
		digestPattern.MatchString(request.RecordSHA256) && hook.TerminalReportDigest(encoded) == request.RecordSHA256 &&
		request.Index >= 1 && request.Index <= hook.QuestionNotifyCount &&
		leasePattern.MatchString(request.LeaseToken) && request.CommentID > 0 &&
		!request.PostedAt.IsZero() && request.PostedAt.Equal(request.PostedAt.UTC())
}

func notifyMarkerKey(runKey string, revision, index int) string {
	return runKey + "#notify#" + strconv.Itoa(revision) + "#" + strconv.Itoa(index)
}
