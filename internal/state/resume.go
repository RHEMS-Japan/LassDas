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

// ResumeWithAnswer adopts the latest round's answer and returns the waiting
// run to the queue as one conditional transaction. The run record, its key and
// every fixed identity check stay unchanged: only the state moves back to
// queued and the sealed cumulative clarification record is bound as the new
// input revision. The original envelope is never rewritten, and the question
// evidence leaving the run item is archived in the same transaction.
func (s *DynamoStore) ResumeWithAnswer(ctx context.Context, request hook.ResumeRequest) (hook.ResumeDisposition, error) {
	// The sealed record names the run it belongs to; the configured id is
	// only the tick's authentication and no longer keys run rows. Rebinding
	// must precede validation, whose route check compares this very field.
	request.Route.ExpectedRunID = request.Record.AutomationRunID
	if !validResumeRequest(request) {
		return "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "invalid_resume_request")
	}

	binding, err := s.loadTerminalBinding(ctx, request.Record.AutomationRunID, request.Route)
	if err != nil {
		return "", err
	}
	if !resumeBindingMatches(binding, request.Record, request.Route) {
		return hook.ResumeConflict, nil
	}
	stateValue, _ := attributeString(binding.runItem, "state")
	if attributeStringEquals(binding.runItem, "clarification_sha256", request.RecordSHA256) {
		return hook.ResumeAlreadyComplete, nil
	}
	if stateValue != stateAwaitingAnswer || !resumeSourceMatches(binding.runItem, request) {
		return hook.ResumeConflict, nil
	}
	return s.startResume(ctx, binding, request)
}

// resumeSourceMatches confirms the waiting run is exactly the one the request
// claims to resume: the sealed question and its posted comment are the latest
// round of the record, the stored claim owner is the workflow that posted that
// question, and the stored clarification revision is the one the record
// extends.
func resumeSourceMatches(item map[string]types.AttributeValue, request hook.ResumeRequest) bool {
	lastRound := request.Record.Rounds[len(request.Record.Rounds)-1]
	question, err := hook.DecodeQuestionRecord([]byte(lastRound.QuestionRecordJSON))
	if err != nil {
		return false
	}
	if !attributeStringEquals(item, "question_record_sha256", lastRound.QuestionRecordSHA256) ||
		!attributeInt64Equals(item, "question_comment_id", lastRound.QuestionCommentID) ||
		!attributeInt64Equals(item, "repository_id", request.Record.RepositoryID) ||
		!attributeStringEquals(item, "repository_sha256", request.Record.RepositorySHA256) ||
		!attributeStringEquals(item, "workflow_ref_sha256", request.Record.WorkflowRefSHA256) ||
		!attributeStringEquals(item, "workflow_sha", question.WorkflowSHA) ||
		!attributeInt64Equals(item, "workflow_run_id", question.WorkflowRunID) ||
		!attributeInt64Equals(item, "run_attempt", int64(question.RunAttempt)) {
		return false
	}
	digest, digestExists := attributeString(item, "clarification_sha256")
	if request.Record.InputRevision == 2 {
		return !digestExists
	}
	return digestExists && digest == request.PreviousRecordSHA256
}

func (s *DynamoStore) startResume(ctx context.Context, binding terminalStoredBinding, request hook.ResumeRequest) (hook.ResumeDisposition, error) {
	lastRound := request.Record.Rounds[len(request.Record.Rounds)-1]
	question, err := hook.DecodeQuestionRecord([]byte(lastRound.QuestionRecordJSON))
	if err != nil {
		return "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "invalid_resume_request")
	}
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
	names["#claimed_at"] = "claimed_at"
	names["#queued_at"] = "queued_at"
	names["#clarification_digest"] = "clarification_sha256"
	names["#clarification_json"] = "clarification_json"
	names["#input_revision"] = "input_revision"
	names["#resumed_at"] = "resumed_at"
	values := map[string]types.AttributeValue{
		":resume_awaiting": stringValue(stateAwaitingAnswer), ":resumed_state": stringValue(stateQueued),
		":run_type":    stringValue("run"),
		":activity_id": numberValue(binding.envelope.Snapshot.ActivityID), ":event_key": stringValue(binding.eventKey),
		":delivery": stringValue(request.Record.DeliveryID), ":envelope": stringValue(binding.envelopeJSON),
		":input_digest": stringValue(request.Record.InputSHA256), ":run_id": stringValue(request.Record.AutomationRunID),
		":repository_id": numberValue(request.Record.RepositoryID), ":repository_digest": stringValue(request.Record.RepositorySHA256),
		":workflow_ref_digest": stringValue(request.Record.WorkflowRefSHA256), ":workflow_sha": stringValue(question.WorkflowSHA),
		":workflow_run_id": numberValue(question.WorkflowRunID), ":run_attempt": numberValue(int64(question.RunAttempt)),
		":question_record_digest": stringValue(lastRound.QuestionRecordSHA256),
		":question_comment_id":    numberValue(lastRound.QuestionCommentID),
		":resumed_queued_at":      numberValue(request.ResumedAt.UnixMilli()),
		":clarification_digest":   stringValue(request.RecordSHA256),
		":clarification_json":     stringValue(request.RecordJSON),
		":input_revision":         numberValue(int64(request.Record.InputRevision)),
		":resumed_at":             numberValue(request.ResumedAt.UnixMilli()),
	}
	condition := "#state = :resume_awaiting AND #record_type = :run_type AND #activity_id = :activity_id AND #run_id = :run_id AND #event_key = :event_key AND #delivery = :delivery AND #digest = :input_digest AND #envelope = :envelope AND #repository_id = :repository_id AND #repository_digest = :repository_digest AND #workflow_ref_digest = :workflow_ref_digest AND #workflow_sha = :workflow_sha AND #workflow_run_id = :workflow_run_id AND #run_attempt = :run_attempt AND #question_record_digest = :question_record_digest AND #question_comment_id = :question_comment_id AND attribute_not_exists(#question_lease_until) AND attribute_not_exists(#question_lease_token) AND attribute_not_exists(#report_digest) AND attribute_not_exists(#terminal_code) AND attribute_not_exists(#terminal_started_at) AND attribute_not_exists(#terminal_lease_until) AND attribute_not_exists(#terminal_lease_token) AND attribute_not_exists(#terminal_comment_id) AND attribute_not_exists(#terminal_completed_at)"
	if request.Record.InputRevision == 2 {
		condition += " AND attribute_not_exists(#clarification_digest) AND attribute_not_exists(#clarification_json) AND attribute_not_exists(#input_revision)"
	} else {
		condition += " AND #clarification_digest = :previous_clarification AND #input_revision = :previous_input_revision"
		values[":previous_clarification"] = stringValue(request.PreviousRecordSHA256)
		values[":previous_input_revision"] = numberValue(int64(request.Record.InputRevision - 1))
	}
	archiveItem := map[string]types.AttributeValue{
		"pk":             stringValue(clarificationArchiveKey(binding.runKey, request.Record.InputRevision)),
		"record_type":    stringValue("clarification_revision"),
		"run_key":        stringValue(binding.runKey),
		"input_revision": numberValue(int64(request.Record.InputRevision)),
		"record_sha256":  stringValue(request.RecordSHA256),
		"record_json":    stringValue(request.RecordJSON),
		"resumed_at":     numberValue(request.ResumedAt.UnixMilli()),
	}
	if startedAt, ok := attributeInt64(binding.runItem, "question_started_at"); ok {
		archiveItem["question_started_at"] = numberValue(startedAt)
	}
	if postedAt, ok := attributeInt64(binding.runItem, "question_posted_at"); ok {
		archiveItem["question_posted_at"] = numberValue(postedAt)
	}
	_, err = s.api.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
		terminalEventCondition(s.table, binding),
		{Put: &types.Put{
			TableName: aws.String(s.table), Item: archiveItem,
			ConditionExpression:      aws.String("attribute_not_exists(#pk)"),
			ExpressionAttributeNames: map[string]string{"#pk": "pk"},
		}},
		{Update: &types.Update{
			TableName: aws.String(s.table), Key: map[string]types.AttributeValue{"pk": stringValue(binding.runKey)},
			UpdateExpression:          aws.String("SET #state = :resumed_state, #queued_at = :resumed_queued_at, #clarification_digest = :clarification_digest, #clarification_json = :clarification_json, #input_revision = :input_revision, #resumed_at = :resumed_at REMOVE #question_record_digest, #question_record_json, #question_started_at, #question_comment_id, #question_posted_at, #claimed_at, #repository_id, #repository_digest, #workflow_ref_digest, #workflow_sha, #workflow_run_id, #run_attempt"),
			ConditionExpression:       aws.String(condition),
			ExpressionAttributeNames:  names,
			ExpressionAttributeValues: values,
		}},
	}})
	if err == nil {
		return hook.ResumeCompleted, nil
	}
	return s.resumeAfterWriteFailure(ctx, request, err)
}

func (s *DynamoStore) resumeAfterWriteFailure(ctx context.Context, request hook.ResumeRequest, writeErr error) (hook.ResumeDisposition, error) {
	var canceled *types.TransactionCanceledException
	if !errors.As(writeErr, &canceled) {
		return "", hook.NewExternalFailure("dynamodb", hook.FailureRetryable, "resume_write_failed")
	}
	if !onlyConditionalCancellation(canceled) {
		if hasCancellationReason(canceled, "ValidationError") {
			return "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "resume_write_rejected")
		}
		return "", hook.NewExternalFailure("dynamodb", hook.FailureRetryable, "resume_write_failed")
	}
	latest, err := s.loadTerminalBinding(ctx, request.Record.AutomationRunID, request.Route)
	if err != nil {
		return "", err
	}
	if !resumeBindingMatches(latest, request.Record, request.Route) {
		return hook.ResumeConflict, nil
	}
	if attributeStringEquals(latest.runItem, "clarification_sha256", request.RecordSHA256) {
		return hook.ResumeAlreadyComplete, nil
	}
	latestState, _ := attributeString(latest.runItem, "state")
	// An unchanged waiting source means the conditional write lost to
	// something transient, not to a competing outcome: retry instead of
	// declaring a permanent conflict.
	if latestState == stateAwaitingAnswer && resumeSourceMatches(latest.runItem, request) {
		return "", hook.NewExternalFailure("dynamodb", hook.FailureRetryable, "resume_write_failed")
	}
	return hook.ResumeConflict, nil
}

func resumeBindingMatches(binding terminalStoredBinding, record hook.ClarificationRecord, route hook.ReportRouteConfig) bool {
	snapshot := binding.envelope.Snapshot
	return runItemMatches(binding.runItem, binding.envelope, binding.eventKey, binding.runKey) &&
		eventItemMatches(binding.eventItem, binding.envelope, binding.runKey) &&
		terminalStateShapeValid(binding.runItem) &&
		snapshot.SpaceKey == route.SpaceKey && snapshot.ProjectID == route.ProjectID && snapshot.ProjectKey == route.ProjectKey &&
		snapshot.CreatorID == route.AllowedCreatorID && snapshot.ActivityType == route.AllowedActivityType &&
		snapshot.RunID == route.ExpectedRunID && snapshot.Target == route.Target &&
		record.DeliveryID == binding.envelope.DeliveryID && record.InputSHA256 == snapshot.InputSHA256 &&
		record.AutomationRunID == snapshot.RunID && record.RepositoryID == route.RepositoryID
}

func validResumeRequest(request hook.ResumeRequest) bool {
	encoded, err := hook.MarshalClarificationRecord(request.Record)
	if err != nil || request.Record.ValidateRoute(request.Route) != nil || string(encoded) != request.RecordJSON ||
		!digestPattern.MatchString(request.RecordSHA256) || hook.TerminalReportDigest(encoded) != request.RecordSHA256 ||
		request.ResumedAt.IsZero() || !request.ResumedAt.Equal(request.ResumedAt.UTC()) {
		return false
	}
	lastRound := request.Record.Rounds[len(request.Record.Rounds)-1]
	lastQuestion, err := hook.DecodeQuestionRecord([]byte(lastRound.QuestionRecordJSON))
	if err != nil {
		return false
	}
	if request.Record.InputRevision == 2 {
		return request.PreviousRecordJSON == "" && request.PreviousRecordSHA256 == "" &&
			lastQuestion.ClarificationSHA256 == ""
	}
	// A second resume must extend the sealed previous record verbatim, and its
	// round-2 question must have been chained to that record when it was asked.
	previous, err := hook.DecodeClarificationRecord([]byte(request.PreviousRecordJSON))
	if err != nil || !digestPattern.MatchString(request.PreviousRecordSHA256) ||
		hook.TerminalReportDigest([]byte(request.PreviousRecordJSON)) != request.PreviousRecordSHA256 ||
		request.PreviousRecordSHA256 == request.RecordSHA256 ||
		previous.InputRevision != request.Record.InputRevision-1 ||
		previous.Protocol != request.Record.Protocol || previous.DeliveryID != request.Record.DeliveryID ||
		previous.InputSHA256 != request.Record.InputSHA256 || previous.RepositoryID != request.Record.RepositoryID ||
		previous.RepositorySHA256 != request.Record.RepositorySHA256 ||
		previous.WorkflowRefSHA256 != request.Record.WorkflowRefSHA256 ||
		previous.AutomationRunID != request.Record.AutomationRunID ||
		len(previous.Rounds) != len(request.Record.Rounds)-1 ||
		lastQuestion.ClarificationSHA256 != request.PreviousRecordSHA256 {
		return false
	}
	for index, round := range previous.Rounds {
		if request.Record.Rounds[index] != round {
			return false
		}
	}
	return true
}

func clarificationArchiveKey(runKey string, revision int) string {
	return runKey + "#clarification#" + strconv.Itoa(revision)
}
