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

// BeginRunComment acquires the exclusive right to post one run-level
// notification (acknowledgement or answer receipt). It follows the reply
// marker pattern; the gate only requires the run to exist with a matching
// binding — these comments describe the run rather than one waiting state, so
// they may complete even after the run moved on.
func (s *DynamoStore) BeginRunComment(ctx context.Context, request hook.RunCommentBeginRequest) (hook.TerminalBinding, hook.ReplyBeginDisposition, error) {
	if !validRunCommentBeginRequest(request) {
		return hook.TerminalBinding{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "invalid_run_comment_begin")
	}
	route, err := s.resolveRunRoute(ctx, request.Route)
	if err != nil {
		return hook.TerminalBinding{}, "", err
	}
	request.Route = route
	binding, err := s.loadTerminalBinding(ctx, request.Route.ExpectedRunID, request.Route)
	if err != nil {
		return hook.TerminalBinding{}, "", err
	}
	if !runCommentBindingMatches(binding, request.Route) {
		return hook.TerminalBinding{}, hook.ReplyBeginConflict, nil
	}
	result := hook.TerminalBinding{IssueID: binding.envelope.Snapshot.IssueID, IssueKey: binding.envelope.Snapshot.IssueKey}
	markerKey := runCommentMarkerKey(binding.runKey, request.Kind, request.Qualifier)
	marker, err := s.loadNotifyMarker(ctx, markerKey)
	if err != nil {
		return hook.TerminalBinding{}, "", err
	}
	if len(marker) > 0 {
		if !runCommentMarkerMatches(marker, binding.runKey, request.Kind) {
			return result, hook.ReplyBeginConflict, nil
		}
		if commentID, ok := attributeInt64(marker, "reply_comment_id"); ok && commentID > 0 {
			return result, hook.ReplyBeginComplete, nil
		}
		leaseUntil, ok := attributeInt64(marker, "reply_lease_until")
		if !ok {
			return hook.TerminalBinding{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "run_comment_marker_invalid")
		}
		if leaseUntil >= request.StartedAt.UnixMilli() {
			return result, hook.ReplyBeginBusy, nil
		}
		return s.reacquireRunComment(ctx, binding, request, result, markerKey, leaseUntil)
	}
	markerItem := map[string]types.AttributeValue{
		"pk":                stringValue(markerKey),
		"record_type":       stringValue("run_comment"),
		"run_key":           stringValue(binding.runKey),
		"reply_kind":        stringValue(string(request.Kind)),
		"content_sha256":    stringValue(request.ContentSHA256),
		"reply_started_at":  numberValue(request.StartedAt.UnixMilli()),
		"reply_lease_until": numberValue(request.LeaseUntil.UnixMilli()),
		"reply_lease_token": stringValue(request.LeaseToken),
	}
	if request.Qualifier != "" {
		markerItem["qualifier"] = stringValue(request.Qualifier)
	}
	_, err = s.api.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
		terminalEventCondition(s.table, binding),
		{Put: &types.Put{
			TableName: aws.String(s.table), Item: markerItem,
			ConditionExpression:      aws.String("attribute_not_exists(#pk)"),
			ExpressionAttributeNames: map[string]string{"#pk": "pk"},
		}},
	}})
	if err == nil {
		return result, hook.ReplyBeginAcquired, nil
	}
	return s.runCommentAfterWriteFailure(ctx, request, result, err)
}

// CompleteRunComment binds the observed comment to the marker, pinning the
// marker alone like the notification complete.
func (s *DynamoStore) CompleteRunComment(ctx context.Context, request hook.RunCommentCompleteRequest) (hook.ReplyCompleteDisposition, error) {
	if !validRunCommentCompleteRequest(request) {
		return "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "invalid_run_comment_complete")
	}
	route, err := s.resolveRunRoute(ctx, request.Route)
	if err != nil {
		return "", err
	}
	request.Route = route
	runKey := makeKey("run", request.Route.SpaceKey, strconv.FormatInt(request.Route.ProjectID, 10), request.Route.ExpectedRunID)
	markerKey := runCommentMarkerKey(runKey, request.Kind, request.Qualifier)
	marker, err := s.loadNotifyMarker(ctx, markerKey)
	if err != nil {
		return "", err
	}
	if len(marker) == 0 || !runCommentMarkerMatches(marker, runKey, request.Kind) {
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
		return "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "invalid_run_comment_complete")
	}
	_, err = s.api.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
		{Update: &types.Update{
			TableName: aws.String(s.table), Key: map[string]types.AttributeValue{"pk": stringValue(markerKey)},
			UpdateExpression:    aws.String("SET #reply_comment_id = :reply_comment_id, #reply_posted_at = :reply_posted_at REMOVE #reply_lease_until, #reply_lease_token"),
			ConditionExpression: aws.String("#record_type = :run_comment_type AND #run_key = :marker_run_key AND #reply_kind = :reply_kind AND #content_digest = :content_digest AND #reply_started_at = :reply_started_at AND #reply_lease_token = :reply_lease_token AND attribute_not_exists(#reply_comment_id) AND attribute_not_exists(#reply_posted_at)"),
			ExpressionAttributeNames: map[string]string{
				"#record_type": "record_type", "#run_key": "run_key", "#reply_kind": "reply_kind",
				"#content_digest": "content_sha256", "#reply_started_at": "reply_started_at",
				"#reply_lease_token": "reply_lease_token", "#reply_comment_id": "reply_comment_id",
				"#reply_posted_at": "reply_posted_at", "#reply_lease_until": "reply_lease_until",
			},
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":run_comment_type": stringValue("run_comment"), ":marker_run_key": stringValue(runKey),
				":reply_kind": stringValue(string(request.Kind)), ":content_digest": stringValue(request.ContentSHA256),
				":reply_started_at": numberValue(startedAt), ":reply_lease_token": stringValue(request.LeaseToken),
				":reply_comment_id": numberValue(request.CommentID), ":reply_posted_at": numberValue(request.PostedAt.UnixMilli()),
			},
		}},
	}})
	if err == nil {
		return hook.ReplyCompleted, nil
	}
	var canceled *types.TransactionCanceledException
	if !errors.As(err, &canceled) || !onlyConditionalCancellation(canceled) {
		if hasCancellationReason(canceled, "ValidationError") {
			return "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "run_comment_write_rejected")
		}
		return "", hook.NewExternalFailure("dynamodb", hook.FailureRetryable, "run_comment_write_failed")
	}
	latest, loadErr := s.loadNotifyMarker(ctx, markerKey)
	if loadErr != nil {
		return "", loadErr
	}
	if runCommentMarkerMatches(latest, runKey, request.Kind) && attributeInt64Equals(latest, "reply_comment_id", request.CommentID) {
		return hook.ReplyAlreadyComplete, nil
	}
	if runCommentMarkerMatches(latest, runKey, request.Kind) && attributeStringEquals(latest, "reply_lease_token", request.LeaseToken) {
		return "", hook.NewExternalFailure("dynamodb", hook.FailureRetryable, "run_comment_write_failed")
	}
	return hook.ReplyCompleteConflict, nil
}

// RunCommentState reports whether the marker already binds a posted comment.
func (s *DynamoStore) RunCommentState(ctx context.Context, route hook.ReportRouteConfig, kind hook.RunCommentKind, qualifier string) (bool, error) {
	if route.Validate() != nil || !validRunCommentKind(kind) {
		return false, hook.NewExternalFailure("dynamodb", hook.FailureRejected, "invalid_run_comment_state")
	}
	route, err := s.resolveRunRoute(ctx, route)
	if err != nil {
		return false, err
	}
	runKey := makeKey("run", route.SpaceKey, strconv.FormatInt(route.ProjectID, 10), route.ExpectedRunID)
	marker, err := s.loadNotifyMarker(ctx, runCommentMarkerKey(runKey, kind, qualifier))
	if err != nil {
		return false, err
	}
	commentID, ok := attributeInt64(marker, "reply_comment_id")
	return ok && commentID > 0, nil
}

func (s *DynamoStore) reacquireRunComment(ctx context.Context, binding terminalStoredBinding, request hook.RunCommentBeginRequest, result hook.TerminalBinding, markerKey string, previousLease int64) (hook.TerminalBinding, hook.ReplyBeginDisposition, error) {
	_, err := s.api.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
		terminalEventCondition(s.table, binding),
		{Update: &types.Update{
			TableName: aws.String(s.table), Key: map[string]types.AttributeValue{"pk": stringValue(markerKey)},
			UpdateExpression:    aws.String("SET #reply_lease_until = :reply_lease_until, #reply_lease_token = :reply_lease_token, #content_digest = :content_digest"),
			ConditionExpression: aws.String("#record_type = :run_comment_type AND #run_key = :marker_run_key AND #reply_kind = :reply_kind AND #reply_lease_until = :previous_reply_lease AND attribute_not_exists(#reply_comment_id) AND attribute_not_exists(#reply_posted_at)"),
			ExpressionAttributeNames: map[string]string{
				"#record_type": "record_type", "#run_key": "run_key", "#reply_kind": "reply_kind",
				"#reply_lease_until": "reply_lease_until", "#reply_lease_token": "reply_lease_token",
				"#content_digest": "content_sha256", "#reply_comment_id": "reply_comment_id",
				"#reply_posted_at": "reply_posted_at",
			},
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":run_comment_type": stringValue("run_comment"), ":marker_run_key": stringValue(binding.runKey),
				":reply_kind":           stringValue(string(request.Kind)),
				":reply_lease_until":    numberValue(request.LeaseUntil.UnixMilli()),
				":reply_lease_token":    stringValue(request.LeaseToken),
				":content_digest":       stringValue(request.ContentSHA256),
				":previous_reply_lease": numberValue(previousLease),
			},
		}},
	}})
	if err == nil {
		return result, hook.ReplyBeginAcquired, nil
	}
	return s.runCommentAfterWriteFailure(ctx, request, result, err)
}

func (s *DynamoStore) runCommentAfterWriteFailure(ctx context.Context, request hook.RunCommentBeginRequest, result hook.TerminalBinding, writeErr error) (hook.TerminalBinding, hook.ReplyBeginDisposition, error) {
	var canceled *types.TransactionCanceledException
	if !errors.As(writeErr, &canceled) {
		return hook.TerminalBinding{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRetryable, "run_comment_write_failed")
	}
	if !onlyConditionalCancellation(canceled) {
		if hasCancellationReason(canceled, "ValidationError") {
			return hook.TerminalBinding{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "run_comment_write_rejected")
		}
		return hook.TerminalBinding{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRetryable, "run_comment_write_failed")
	}
	binding, err := s.loadTerminalBinding(ctx, request.Route.ExpectedRunID, request.Route)
	if err != nil {
		return hook.TerminalBinding{}, "", err
	}
	if !runCommentBindingMatches(binding, request.Route) {
		return result, hook.ReplyBeginConflict, nil
	}
	markerKey := runCommentMarkerKey(binding.runKey, request.Kind, request.Qualifier)
	marker, err := s.loadNotifyMarker(ctx, markerKey)
	if err != nil {
		return hook.TerminalBinding{}, "", err
	}
	if len(marker) > 0 && runCommentMarkerMatches(marker, binding.runKey, request.Kind) {
		if commentID, ok := attributeInt64(marker, "reply_comment_id"); ok && commentID > 0 {
			return result, hook.ReplyBeginComplete, nil
		}
		return result, hook.ReplyBeginBusy, nil
	}
	if len(marker) == 0 {
		return hook.TerminalBinding{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRetryable, "run_comment_write_failed")
	}
	return result, hook.ReplyBeginConflict, nil
}

// runCommentBindingMatches accepts any state whose stored shape is internally
// consistent: acknowledgement and receipt describe the run, not one state.
func runCommentBindingMatches(binding terminalStoredBinding, route hook.ReportRouteConfig) bool {
	snapshot := binding.envelope.Snapshot
	return runItemMatches(binding.runItem, binding.envelope, binding.eventKey, binding.runKey) &&
		eventItemMatches(binding.eventItem, binding.envelope, binding.runKey) &&
		terminalStateShapeValid(binding.runItem) &&
		snapshot.SpaceKey == route.SpaceKey && snapshot.ProjectID == route.ProjectID && snapshot.ProjectKey == route.ProjectKey &&
		snapshot.CreatorID == route.AllowedCreatorID && snapshot.ActivityType == route.AllowedActivityType &&
		snapshot.RunID == route.ExpectedRunID && snapshot.Target == route.Target
}

func runCommentMarkerMatches(marker map[string]types.AttributeValue, runKey string, kind hook.RunCommentKind) bool {
	return attributeStringEquals(marker, "record_type", "run_comment") &&
		attributeStringEquals(marker, "run_key", runKey) &&
		attributeStringEquals(marker, "reply_kind", string(kind))
}

// validRunCommentKind accepts the kinds hook.StoreRunCommentKinds names —
// the one-shot comments whose marker lives in the store. A kind posted
// through the store but missing from that list made RunCommentState refuse
// it and a run whose report was approved wait for ever (the investigation
// report, live 2026-09-05).
func validRunCommentKind(kind hook.RunCommentKind) bool {
	for _, known := range hook.StoreRunCommentKinds() {
		if kind == known {
			return true
		}
	}
	return false
}

func validRunCommentBeginRequest(request hook.RunCommentBeginRequest) bool {
	return request.Route.Validate() == nil && validRunCommentKind(request.Kind) &&
		len(request.Qualifier) <= 64 && digestPattern.MatchString(request.ContentSHA256) &&
		leasePattern.MatchString(request.LeaseToken) && !request.StartedAt.IsZero() &&
		request.LeaseUntil.Equal(request.StartedAt.Add(request.Route.LeaseDuration))
}

func validRunCommentCompleteRequest(request hook.RunCommentCompleteRequest) bool {
	return request.Route.Validate() == nil && validRunCommentKind(request.Kind) &&
		len(request.Qualifier) <= 64 && digestPattern.MatchString(request.ContentSHA256) &&
		leasePattern.MatchString(request.LeaseToken) && request.CommentID > 0 && !request.PostedAt.IsZero()
}

func runCommentMarkerKey(runKey string, kind hook.RunCommentKind, qualifier string) string {
	key := runKey + "#comment#" + string(kind)
	if qualifier != "" {
		key += "#" + qualifier
	}
	return key
}

// LoadRunNotice reads the run-level view for the acceptance and receipt
// notices. A missing run is simply "does not exist"; a present run with a
// broken seal is an error.
func (s *DynamoStore) LoadRunNotice(ctx context.Context, route hook.ReportRouteConfig) (hook.RunNoticeSnapshot, error) {
	if route.Validate() != nil {
		return hook.RunNoticeSnapshot{}, hook.NewExternalFailure("dynamodb", hook.FailureRejected, "invalid_run_notice_route")
	}
	route, err := s.resolveRunRoute(ctx, route)
	if err != nil {
		return hook.RunNoticeSnapshot{}, err
	}
	binding, err := s.loadTerminalBinding(ctx, route.ExpectedRunID, route)
	if err != nil {
		if _, code := hook.FailureDetails(err); code == "terminal_binding_missing" {
			return hook.RunNoticeSnapshot{}, nil
		}
		return hook.RunNoticeSnapshot{}, err
	}
	if !runCommentBindingMatches(binding, route) {
		return hook.RunNoticeSnapshot{}, hook.NewExternalFailure("dynamodb", hook.FailureRejected, "run_notice_binding_invalid")
	}
	stateValue, _ := attributeString(binding.runItem, "state")
	_, _, clarificationOK := clarificationStateConsistent(binding.runItem)
	if !clarificationOK {
		return hook.RunNoticeSnapshot{}, hook.NewExternalFailure("dynamodb", hook.FailureRejected, "run_notice_binding_invalid")
	}
	clarificationJSON, _ := attributeString(binding.runItem, "clarification_json")
	return hook.RunNoticeSnapshot{
		Exists:            true,
		Terminal:          stateValue == stateTerminal || stateValue == stateReportPending,
		IssueID:           binding.envelope.Snapshot.IssueID,
		Snapshot:          binding.envelope.Snapshot,
		ClarificationJSON: clarificationJSON,
	}, nil
}

// LoadIngestCursor and StoreIngestCursor keep the last activity ID the lost
// -webhook completion has already inspected. The cursor only moves forward.
func (s *DynamoStore) LoadIngestCursor(ctx context.Context, route hook.ReportRouteConfig) (int64, error) {
	if route.Validate() != nil {
		return 0, hook.NewExternalFailure("dynamodb", hook.FailureRejected, "invalid_ingest_cursor")
	}
	item, err := s.loadNotifyMarker(ctx, ingestCursorKey(route))
	if err != nil {
		return 0, err
	}
	if len(item) == 0 {
		return 0, nil
	}
	cursor, ok := attributeInt64(item, "last_activity_id")
	if !ok || cursor < 0 {
		return 0, hook.NewExternalFailure("dynamodb", hook.FailureRejected, "ingest_cursor_invalid")
	}
	return cursor, nil
}

func (s *DynamoStore) StoreIngestCursor(ctx context.Context, route hook.ReportRouteConfig, activityID int64) error {
	if route.Validate() != nil || activityID <= 0 {
		return hook.NewExternalFailure("dynamodb", hook.FailureRejected, "invalid_ingest_cursor")
	}
	_, err := s.api.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
		{Update: &types.Update{
			TableName: aws.String(s.table), Key: map[string]types.AttributeValue{"pk": stringValue(ingestCursorKey(route))},
			UpdateExpression:    aws.String("SET #record_type = :ingest_type, #last = :ingest_activity"),
			ConditionExpression: aws.String("attribute_not_exists(#last) OR #last < :ingest_activity"),
			ExpressionAttributeNames: map[string]string{
				"#record_type": "record_type", "#last": "last_activity_id",
			},
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":ingest_type": stringValue("ingest_cursor"), ":ingest_activity": numberValue(activityID),
			},
		}},
	}})
	if err == nil {
		return nil
	}
	var canceled *types.TransactionCanceledException
	if errors.As(err, &canceled) && onlyConditionalCancellation(canceled) {
		// A concurrent tick already advanced past this point.
		return nil
	}
	return hook.NewExternalFailure("dynamodb", hook.FailureRetryable, "ingest_cursor_write_failed")
}

func ingestCursorKey(route hook.ReportRouteConfig) string {
	return "ingest#" + route.SpaceKey + "#" + strconv.FormatInt(route.ProjectID, 10)
}
