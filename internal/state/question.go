package state

import (
	"context"
	"errors"

	"automation.internal/ticket-ingress/internal/hook"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// BeginQuestion moves a claimed run into question_report_pending under a
// lease, sealing the canonical question record before any Backlog comment is
// posted. It mirrors BeginTerminal: begin acquires the exclusive right to
// post, CompleteQuestion binds the observed comment, and a lost response is
// resolved by re-reading the record instead of guessing.
func (s *DynamoStore) BeginQuestion(ctx context.Context, request hook.QuestionBeginRequest) (hook.TerminalBinding, hook.QuestionBeginDisposition, error) {
	if !validQuestionBeginRequest(request) {
		return hook.TerminalBinding{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "invalid_question_begin")
	}
	binding, err := s.loadTerminalBinding(ctx, request.Record.AutomationRunID, request.Route)
	if err != nil {
		return hook.TerminalBinding{}, "", err
	}
	if !questionBindingMatches(binding, request.Record, request.Route) {
		return hook.TerminalBinding{}, hook.QuestionBeginConflict, nil
	}
	result := hook.TerminalBinding{IssueID: binding.envelope.Snapshot.IssueID, IssueKey: binding.envelope.Snapshot.IssueKey}
	stateValue, _ := attributeString(binding.runItem, "state")
	switch stateValue {
	case stateAwaitingAnswer:
		if questionRecordMatches(binding.runItem, request.RecordSHA256) && validStoredQuestionCommentID(binding.runItem) {
			return result, hook.QuestionBeginComplete, nil
		}
		return result, hook.QuestionBeginConflict, nil
	case stateQuestionPending:
		if !questionRecordMatches(binding.runItem, request.RecordSHA256) {
			return result, hook.QuestionBeginConflict, nil
		}
		leaseUntil, ok := attributeInt64(binding.runItem, "question_lease_until")
		if !ok {
			return hook.TerminalBinding{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "question_binding_invalid")
		}
		if leaseUntil >= request.StartedAt.UnixMilli() {
			return result, hook.QuestionBeginBusy, nil
		}
		return s.reacquireQuestion(ctx, binding, request, result, leaseUntil)
	case stateClaimed:
		clarificationDigest, clarificationRounds, ok := clarificationStateConsistent(binding.runItem)
		if !ok || !questionRevisionConsistent(request.Record, clarificationDigest, clarificationRounds) {
			return result, hook.QuestionBeginConflict, nil
		}
		return s.startQuestion(ctx, binding, request, result)
	default:
		return result, hook.QuestionBeginConflict, nil
	}
}

// CompleteQuestion binds the observed Backlog comment to the sealed question
// and moves the run into awaiting_answer. Duplicate completion resolves to
// already_complete only when both the sealed record and the comment match.
func (s *DynamoStore) CompleteQuestion(ctx context.Context, request hook.QuestionCompleteRequest) (hook.QuestionCompleteDisposition, error) {
	if !validQuestionCompleteRequest(request) {
		return "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "invalid_question_complete")
	}
	binding, err := s.loadTerminalBinding(ctx, request.Record.AutomationRunID, request.Route)
	if err != nil {
		return "", err
	}
	if !questionBindingMatches(binding, request.Record, request.Route) {
		return hook.QuestionCompleteConflict, nil
	}
	stateValue, _ := attributeString(binding.runItem, "state")
	if stateValue == stateAwaitingAnswer {
		if questionRecordMatches(binding.runItem, request.RecordSHA256) &&
			attributeInt64Equals(binding.runItem, "question_comment_id", request.CommentID) {
			return hook.QuestionAlreadyComplete, nil
		}
		return hook.QuestionCompleteConflict, nil
	}
	if stateValue != stateQuestionPending || !questionRecordMatches(binding.runItem, request.RecordSHA256) ||
		!attributeStringEquals(binding.runItem, "question_lease_token", request.LeaseToken) {
		return hook.QuestionCompleteConflict, nil
	}
	startedAt, ok := attributeInt64(binding.runItem, "question_started_at")
	if !ok || request.PostedAt.UnixMilli() < startedAt {
		return "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "invalid_question_complete")
	}

	names := questionCommonNames()
	names["#question_record_digest"] = "question_record_sha256"
	names["#question_lease_token"] = "question_lease_token"
	names["#question_lease_until"] = "question_lease_until"
	names["#question_started_at"] = "question_started_at"
	names["#question_comment_id"] = "question_comment_id"
	names["#question_posted_at"] = "question_posted_at"
	values := questionCommonValues(binding, request.Record)
	values[":awaiting"] = stringValue(stateAwaitingAnswer)
	values[":question_pending"] = stringValue(stateQuestionPending)
	values[":question_record_digest"] = stringValue(request.RecordSHA256)
	values[":question_lease_token"] = stringValue(request.LeaseToken)
	values[":question_started_at"] = numberValue(startedAt)
	values[":question_comment_id"] = numberValue(request.CommentID)
	values[":question_posted_at"] = numberValue(request.PostedAt.UnixMilli())
	_, err = s.api.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
		terminalEventCondition(s.table, binding),
		{Update: &types.Update{
			TableName: aws.String(s.table), Key: map[string]types.AttributeValue{"pk": stringValue(binding.runKey)},
			UpdateExpression:          aws.String("SET #state = :awaiting, #question_comment_id = :question_comment_id, #question_posted_at = :question_posted_at REMOVE #question_lease_token, #question_lease_until"),
			ConditionExpression:       aws.String("#state = :question_pending AND #record_type = :run_type AND #activity_id = :activity_id AND #run_id = :run_id AND #event_key = :event_key AND #delivery = :delivery AND #digest = :input_digest AND #envelope = :envelope AND #repository_id = :repository_id AND #repository_digest = :repository_digest AND #workflow_ref_digest = :workflow_ref_digest AND #workflow_sha = :workflow_sha AND #workflow_run_id = :workflow_run_id AND #run_attempt = :run_attempt AND #question_record_digest = :question_record_digest AND #question_started_at = :question_started_at AND #question_lease_token = :question_lease_token AND attribute_not_exists(#question_comment_id) AND attribute_not_exists(#question_posted_at)"),
			ExpressionAttributeNames:  names,
			ExpressionAttributeValues: values,
		}},
	}})
	if err == nil {
		return hook.QuestionCompleted, nil
	}
	var canceled *types.TransactionCanceledException
	if !errors.As(err, &canceled) {
		return "", hook.NewExternalFailure("dynamodb", hook.FailureRetryable, "question_complete_write_failed")
	}
	latest, loadErr := s.loadTerminalBinding(ctx, request.Record.AutomationRunID, request.Route)
	if loadErr != nil {
		return "", loadErr
	}
	if !questionBindingMatches(latest, request.Record, request.Route) {
		return hook.QuestionCompleteConflict, nil
	}
	latestState, _ := attributeString(latest.runItem, "state")
	if latestState == stateAwaitingAnswer && questionRecordMatches(latest.runItem, request.RecordSHA256) &&
		attributeInt64Equals(latest.runItem, "question_comment_id", request.CommentID) {
		return hook.QuestionAlreadyComplete, nil
	}
	if latestState == stateQuestionPending && questionRecordMatches(latest.runItem, request.RecordSHA256) &&
		attributeStringEquals(latest.runItem, "question_lease_token", request.LeaseToken) {
		return "", hook.NewExternalFailure("dynamodb", hook.FailureRetryable, "question_complete_write_failed")
	}
	return hook.QuestionCompleteConflict, nil
}

func (s *DynamoStore) startQuestion(ctx context.Context, binding terminalStoredBinding, request hook.QuestionBeginRequest, result hook.TerminalBinding) (hook.TerminalBinding, hook.QuestionBeginDisposition, error) {
	condition := "#state = :claimed AND #record_type = :run_type AND #activity_id = :activity_id AND #event_key = :event_key AND #delivery = :delivery AND #digest = :input_digest AND #envelope = :envelope AND #run_id = :run_id AND #repository_id = :repository_id AND #repository_digest = :repository_digest AND #workflow_ref_digest = :workflow_ref_digest AND #workflow_sha = :workflow_sha AND #workflow_run_id = :workflow_run_id AND #run_attempt = :run_attempt AND attribute_not_exists(#question_record_digest) AND attribute_not_exists(#question_record_json) AND attribute_not_exists(#question_started_at) AND attribute_not_exists(#question_lease_until) AND attribute_not_exists(#question_lease_token) AND attribute_not_exists(#question_comment_id) AND attribute_not_exists(#question_posted_at) AND attribute_not_exists(#report_digest) AND attribute_not_exists(#terminal_code) AND attribute_not_exists(#terminal_started_at) AND attribute_not_exists(#terminal_lease_until) AND attribute_not_exists(#terminal_lease_token) AND attribute_not_exists(#terminal_comment_id) AND attribute_not_exists(#terminal_completed_at)"
	names := questionStartNames()
	values := questionStartValues(binding, request)
	names["#clarification_digest"] = "clarification_sha256"
	names["#input_revision"] = "input_revision"
	// A revision-1 question may only start on a never-resumed run; a
	// revision-2 question must pin the exact sealed clarification record it
	// chains to, so a stale round-1 question can never reopen a resumed run.
	if request.Record.QuestionRevision == 1 {
		names["#clarification_json"] = "clarification_json"
		condition += " AND attribute_not_exists(#clarification_digest) AND attribute_not_exists(#clarification_json) AND attribute_not_exists(#input_revision)"
	} else {
		condition += " AND #clarification_digest = :question_clarification AND #input_revision = :question_input_revision"
		values[":question_clarification"] = stringValue(request.Record.ClarificationSHA256)
		values[":question_input_revision"] = numberValue(int64(request.Record.QuestionRevision))
	}
	_, err := s.api.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
		terminalEventCondition(s.table, binding),
		{Update: &types.Update{
			TableName: aws.String(s.table), Key: map[string]types.AttributeValue{"pk": stringValue(binding.runKey)},
			UpdateExpression:          aws.String("SET #state = :question_pending, #question_record_digest = :question_record_digest, #question_record_json = :question_record_json, #question_started_at = :question_started_at, #question_lease_until = :question_lease_until, #question_lease_token = :question_lease_token"),
			ConditionExpression:       aws.String(condition),
			ExpressionAttributeNames:  names,
			ExpressionAttributeValues: values,
		}},
	}})
	if err == nil {
		return result, hook.QuestionBeginAcquired, nil
	}
	return s.questionBeginAfterWriteFailure(ctx, request, result, err)
}

func (s *DynamoStore) reacquireQuestion(ctx context.Context, binding terminalStoredBinding, request hook.QuestionBeginRequest, result hook.TerminalBinding, previousLease int64) (hook.TerminalBinding, hook.QuestionBeginDisposition, error) {
	names := questionCommonNames()
	names["#question_record_digest"] = "question_record_sha256"
	names["#question_lease_until"] = "question_lease_until"
	names["#question_lease_token"] = "question_lease_token"
	names["#question_comment_id"] = "question_comment_id"
	names["#question_posted_at"] = "question_posted_at"
	values := questionCommonValues(binding, request.Record)
	values[":question_pending"] = stringValue(stateQuestionPending)
	values[":question_record_digest"] = stringValue(request.RecordSHA256)
	values[":question_lease_until"] = numberValue(request.LeaseUntil.UnixMilli())
	values[":question_lease_token"] = stringValue(request.LeaseToken)
	values[":previous_question_lease"] = numberValue(previousLease)
	_, err := s.api.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
		terminalEventCondition(s.table, binding),
		{Update: &types.Update{
			TableName: aws.String(s.table), Key: map[string]types.AttributeValue{"pk": stringValue(binding.runKey)},
			UpdateExpression:          aws.String("SET #question_lease_until = :question_lease_until, #question_lease_token = :question_lease_token"),
			ConditionExpression:       aws.String("#state = :question_pending AND #record_type = :run_type AND #activity_id = :activity_id AND #event_key = :event_key AND #delivery = :delivery AND #digest = :input_digest AND #envelope = :envelope AND #run_id = :run_id AND #repository_id = :repository_id AND #repository_digest = :repository_digest AND #workflow_ref_digest = :workflow_ref_digest AND #workflow_sha = :workflow_sha AND #workflow_run_id = :workflow_run_id AND #run_attempt = :run_attempt AND #question_record_digest = :question_record_digest AND #question_lease_until = :previous_question_lease AND attribute_not_exists(#question_comment_id) AND attribute_not_exists(#question_posted_at)"),
			ExpressionAttributeNames:  names,
			ExpressionAttributeValues: values,
		}},
	}})
	if err == nil {
		return result, hook.QuestionBeginAcquired, nil
	}
	return s.questionBeginAfterWriteFailure(ctx, request, result, err)
}

func (s *DynamoStore) questionBeginAfterWriteFailure(ctx context.Context, request hook.QuestionBeginRequest, result hook.TerminalBinding, writeErr error) (hook.TerminalBinding, hook.QuestionBeginDisposition, error) {
	var canceled *types.TransactionCanceledException
	if !errors.As(writeErr, &canceled) {
		return hook.TerminalBinding{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRetryable, "question_begin_write_failed")
	}
	if !onlyConditionalCancellation(canceled) {
		if hasCancellationReason(canceled, "ValidationError") {
			return hook.TerminalBinding{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "question_begin_write_rejected")
		}
		return hook.TerminalBinding{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRetryable, "question_begin_write_failed")
	}
	latest, err := s.loadTerminalBinding(ctx, request.Record.AutomationRunID, request.Route)
	if err != nil {
		return hook.TerminalBinding{}, "", err
	}
	if !questionBindingMatches(latest, request.Record, request.Route) {
		return result, hook.QuestionBeginConflict, nil
	}
	stateValue, _ := attributeString(latest.runItem, "state")
	if stateValue == stateAwaitingAnswer && questionRecordMatches(latest.runItem, request.RecordSHA256) && validStoredQuestionCommentID(latest.runItem) {
		return result, hook.QuestionBeginComplete, nil
	}
	if stateValue == stateQuestionPending && questionRecordMatches(latest.runItem, request.RecordSHA256) {
		return result, hook.QuestionBeginBusy, nil
	}
	if stateValue == stateClaimed {
		return hook.TerminalBinding{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRetryable, "question_begin_write_failed")
	}
	return result, hook.QuestionBeginConflict, nil
}

// LoadQuestionWait reads the fixed run and, when it is waiting for an answer,
// returns the sealed question and clarification context the tick operates on.
// A missing run or any non-waiting state is simply "not waiting"; a waiting
// item that fails its own seal checks is an error, never a silent idle.
func (s *DynamoStore) LoadQuestionWait(ctx context.Context, route hook.ReportRouteConfig) (hook.QuestionWaitSnapshot, bool, error) {
	if route.Validate() != nil {
		return hook.QuestionWaitSnapshot{}, false, hook.NewExternalFailure("dynamodb", hook.FailureRejected, "invalid_question_wait_route")
	}
	route, err := s.resolveRunRoute(ctx, route)
	if err != nil {
		return hook.QuestionWaitSnapshot{}, false, err
	}
	binding, err := s.loadTerminalBinding(ctx, route.ExpectedRunID, route)
	if err != nil {
		if _, code := hook.FailureDetails(err); code == "terminal_binding_missing" {
			return hook.QuestionWaitSnapshot{}, false, nil
		}
		return hook.QuestionWaitSnapshot{}, false, err
	}
	stateValue, _ := attributeString(binding.runItem, "state")
	if stateValue != stateAwaitingAnswer && stateValue != stateQuestionPending {
		return hook.QuestionWaitSnapshot{}, false, nil
	}
	posting := stateValue == stateQuestionPending
	snapshot := binding.envelope.Snapshot
	if !runItemMatches(binding.runItem, binding.envelope, binding.eventKey, binding.runKey) ||
		!eventItemMatches(binding.eventItem, binding.envelope, binding.runKey) ||
		!terminalStateShapeValid(binding.runItem) ||
		snapshot.SpaceKey != route.SpaceKey || snapshot.ProjectID != route.ProjectID || snapshot.ProjectKey != route.ProjectKey ||
		snapshot.CreatorID != route.AllowedCreatorID || snapshot.ActivityType != route.AllowedActivityType ||
		snapshot.RunID != route.ExpectedRunID || snapshot.Target != route.Target {
		return hook.QuestionWaitSnapshot{}, false, hook.NewExternalFailure("dynamodb", hook.FailureRejected, "question_wait_binding_invalid")
	}
	recordJSON, _ := attributeString(binding.runItem, "question_record_json")
	recordDigest, _ := attributeString(binding.runItem, "question_record_sha256")
	record, ok := questionRecordJSONValid(recordJSON, recordDigest)
	if !ok || record.ValidateRoute(route) != nil {
		return hook.QuestionWaitSnapshot{}, false, hook.NewExternalFailure("dynamodb", hook.FailureRejected, "question_wait_binding_invalid")
	}
	commentID, commentOK := attributeInt64(binding.runItem, "question_comment_id")
	clarificationDigest, _, clarificationOK := clarificationStateConsistent(binding.runItem)
	if !clarificationOK || (!posting && (!commentOK || commentID <= 0)) {
		return hook.QuestionWaitSnapshot{}, false, hook.NewExternalFailure("dynamodb", hook.FailureRejected, "question_wait_binding_invalid")
	}
	if posting {
		commentID = 0
	}
	clarificationJSON, _ := attributeString(binding.runItem, "clarification_json")
	return hook.QuestionWaitSnapshot{
		Record: record, RecordJSON: recordJSON, RecordSHA256: recordDigest,
		QuestionCommentID: commentID, IssueID: snapshot.IssueID,
		ClarificationJSON: clarificationJSON, ClarificationSHA256: clarificationDigest,
		Posting: posting,
	}, true, nil
}

func questionBindingMatches(binding terminalStoredBinding, record hook.QuestionRecord, route hook.ReportRouteConfig) bool {
	snapshot := binding.envelope.Snapshot
	return runItemMatches(binding.runItem, binding.envelope, binding.eventKey, binding.runKey) &&
		eventItemMatches(binding.eventItem, binding.envelope, binding.runKey) &&
		terminalStateShapeValid(binding.runItem) &&
		snapshot.SpaceKey == route.SpaceKey && snapshot.ProjectID == route.ProjectID && snapshot.ProjectKey == route.ProjectKey &&
		snapshot.CreatorID == route.AllowedCreatorID && snapshot.ActivityType == route.AllowedActivityType &&
		snapshot.RunID == route.ExpectedRunID && snapshot.Target == route.Target &&
		record.DeliveryID == binding.envelope.DeliveryID && record.InputSHA256 == snapshot.InputSHA256 &&
		record.AutomationRunID == snapshot.RunID && record.RepositoryID == route.RepositoryID &&
		attributeInt64Equals(binding.runItem, "repository_id", record.RepositoryID) &&
		attributeStringEquals(binding.runItem, "repository_sha256", record.RepositorySHA256) &&
		attributeStringEquals(binding.runItem, "workflow_ref_sha256", record.WorkflowRefSHA256) &&
		attributeStringEquals(binding.runItem, "workflow_sha", record.WorkflowSHA) &&
		attributeInt64Equals(binding.runItem, "workflow_run_id", record.WorkflowRunID) &&
		attributeInt64Equals(binding.runItem, "run_attempt", int64(record.RunAttempt))
}

func questionRecordMatches(item map[string]types.AttributeValue, digest string) bool {
	return attributeStringEquals(item, "question_record_sha256", digest)
}

func validStoredQuestionCommentID(item map[string]types.AttributeValue) bool {
	value, ok := attributeInt64(item, "question_comment_id")
	return ok && value > 0
}

func validQuestionBeginRequest(request hook.QuestionBeginRequest) bool {
	encoded, err := hook.MarshalQuestionRecord(request.Record)
	return err == nil && request.Record.ValidateRoute(request.Route) == nil && string(encoded) == request.RecordJSON &&
		digestPattern.MatchString(request.RecordSHA256) && hook.TerminalReportDigest(encoded) == request.RecordSHA256 &&
		leasePattern.MatchString(request.LeaseToken) && !request.StartedAt.IsZero() && request.StartedAt.Equal(request.StartedAt.UTC()) &&
		request.LeaseUntil.Equal(request.LeaseUntil.UTC()) && request.LeaseUntil.Equal(request.StartedAt.Add(request.Route.LeaseDuration))
}

func validQuestionCompleteRequest(request hook.QuestionCompleteRequest) bool {
	encoded, err := hook.MarshalQuestionRecord(request.Record)
	return err == nil && request.Record.ValidateRoute(request.Route) == nil && string(encoded) == request.RecordJSON &&
		digestPattern.MatchString(request.RecordSHA256) && hook.TerminalReportDigest(encoded) == request.RecordSHA256 &&
		leasePattern.MatchString(request.LeaseToken) && request.CommentID > 0 && !request.PostedAt.IsZero() && request.PostedAt.Equal(request.PostedAt.UTC())
}

func questionCommonNames() map[string]string {
	return map[string]string{
		"#state": "state", "#record_type": "record_type", "#activity_id": "activity_id", "#event_key": "event_key",
		"#delivery": "delivery_id", "#digest": "input_sha256", "#envelope": "envelope_json",
		"#run_id": "run_id", "#repository_id": "repository_id",
		"#repository_digest": "repository_sha256", "#workflow_ref_digest": "workflow_ref_sha256",
		"#workflow_sha": "workflow_sha", "#workflow_run_id": "workflow_run_id", "#run_attempt": "run_attempt",
	}
}

func questionCommonValues(binding terminalStoredBinding, record hook.QuestionRecord) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		":run_type":    stringValue("run"),
		":activity_id": numberValue(binding.envelope.Snapshot.ActivityID), ":event_key": stringValue(binding.eventKey),
		":delivery": stringValue(record.DeliveryID), ":envelope": stringValue(binding.envelopeJSON),
		":input_digest": stringValue(record.InputSHA256), ":run_id": stringValue(record.AutomationRunID),
		":repository_id": numberValue(record.RepositoryID), ":repository_digest": stringValue(record.RepositorySHA256),
		":workflow_ref_digest": stringValue(record.WorkflowRefSHA256), ":workflow_sha": stringValue(record.WorkflowSHA),
		":workflow_run_id": numberValue(record.WorkflowRunID), ":run_attempt": numberValue(int64(record.RunAttempt)),
	}
}

func questionStartNames() map[string]string {
	names := questionCommonNames()
	names["#question_record_digest"] = "question_record_sha256"
	names["#question_record_json"] = "question_record_json"
	names["#question_started_at"] = "question_started_at"
	names["#question_lease_until"] = "question_lease_until"
	names["#question_lease_token"] = "question_lease_token"
	names["#question_comment_id"] = "question_comment_id"
	names["#question_posted_at"] = "question_posted_at"
	names["#report_digest"] = "terminal_report_sha256"
	names["#terminal_code"] = "terminal_code"
	names["#terminal_started_at"] = "terminal_started_at"
	names["#terminal_lease_until"] = "terminal_lease_until"
	names["#terminal_lease_token"] = "terminal_lease_token"
	names["#terminal_comment_id"] = "terminal_comment_id"
	names["#terminal_completed_at"] = "terminal_completed_at"
	return names
}

func questionStartValues(binding terminalStoredBinding, request hook.QuestionBeginRequest) map[string]types.AttributeValue {
	values := questionCommonValues(binding, request.Record)
	values[":question_pending"] = stringValue(stateQuestionPending)
	values[":claimed"] = stringValue(stateClaimed)
	values[":question_record_digest"] = stringValue(request.RecordSHA256)
	values[":question_record_json"] = stringValue(request.RecordJSON)
	values[":question_started_at"] = numberValue(request.StartedAt.UnixMilli())
	values[":question_lease_until"] = numberValue(request.LeaseUntil.UnixMilli())
	values[":question_lease_token"] = stringValue(request.LeaseToken)
	return values
}
