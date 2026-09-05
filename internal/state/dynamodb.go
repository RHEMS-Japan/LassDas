package state

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"

	"automation.internal/ticket-ingress/internal/hook"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	stateQueued          = "queued"
	stateClaimed         = "claimed"
	stateQuestionPending = "question_report_pending"
	stateAwaitingAnswer  = "awaiting_answer"
	stateReportPending   = "terminal_report_pending"
	stateTerminal        = "terminal"
)

var (
	tablePattern  = regexp.MustCompile(`^[A-Za-z0-9_.-]{3,255}$`)
	digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
	commitPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)
	leasePattern  = regexp.MustCompile(`^[a-f0-9]{32}$`)
)

type DynamoAPI interface {
	TransactWriteItems(context.Context, *dynamodb.TransactWriteItemsInput, ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error)
	GetItem(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
}

type DynamoStore struct {
	table string
	api   DynamoAPI
}

func NewDynamoStore(table string, api DynamoAPI) (*DynamoStore, error) {
	if !tablePattern.MatchString(table) {
		return nil, errors.New("dynamodb table name is invalid")
	}
	if api == nil {
		return nil, errors.New("dynamodb client must not be nil")
	}
	return &DynamoStore{table: table, api: api}, nil
}

func (s *DynamoStore) Enqueue(ctx context.Context, request hook.QueueRequest) (hook.QueueDisposition, error) {
	// The clarification field exists only on delivered envelopes; the stored
	// envelope of a run never carries it (resume archives the record in its
	// own attributes), so an ingress envelope with one is always foreign.
	if request.QueuedAt.IsZero() || request.Envelope.ClarificationJSON != "" ||
		hook.ValidateEnvelope(request.Envelope) != nil {
		return "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "invalid_queue_request")
	}
	encoded, err := json.Marshal(request.Envelope)
	if err != nil || len(encoded) == 0 || len(encoded) > 65535 {
		return "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "invalid_queue_envelope")
	}
	snapshot := request.Envelope.Snapshot
	eventKey := makeKey("event", snapshot.SpaceKey, strconv.FormatInt(snapshot.ProjectID, 10), strconv.FormatInt(snapshot.ActivityID, 10))
	runKey := makeKey("run", snapshot.SpaceKey, strconv.FormatInt(snapshot.ProjectID, 10), snapshot.RunID)
	queuedAt := request.QueuedAt.UTC().UnixMilli()
	eventItem := map[string]types.AttributeValue{
		"pk":           stringValue(eventKey),
		"record_type":  stringValue("event"),
		"activity_id":  numberValue(snapshot.ActivityID),
		"run_key":      stringValue(runKey),
		"delivery_id":  stringValue(request.Envelope.DeliveryID),
		"input_sha256": stringValue(snapshot.InputSHA256),
	}
	runItem := map[string]types.AttributeValue{
		"pk":            stringValue(runKey),
		"record_type":   stringValue("run"),
		"event_key":     stringValue(eventKey),
		"activity_id":   numberValue(snapshot.ActivityID),
		"run_id":        stringValue(snapshot.RunID),
		"delivery_id":   stringValue(request.Envelope.DeliveryID),
		"input_sha256":  stringValue(snapshot.InputSHA256),
		"envelope_json": stringValue(string(encoded)),
		"queued_at":     numberValue(queuedAt),
		"state":         stringValue(stateQueued),
	}
	// The puller cannot name a ticket it has not seen. Rather than build a
	// queue or scan the table, one marker per project records which run is
	// waiting, so the puller finds it with the same single point read it always
	// used. Claiming it clears the marker, which is what lets the next ticket
	// take its turn: the identifier used to be a single configured value, so
	// exactly one ticket could ever be processed.
	pendingKey := makeKey("pending", snapshot.SpaceKey, strconv.FormatInt(snapshot.ProjectID, 10))
	pendingItem := map[string]types.AttributeValue{
		"pk":          stringValue(pendingKey),
		"record_type": stringValue("pending"),
		"run_id":      stringValue(snapshot.RunID),
		"run_key":     stringValue(runKey),
		"queued_at":   numberValue(queuedAt),
	}
	_, err = s.api.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
		{Put: &types.Put{
			TableName: aws.String(s.table), Item: eventItem,
			ConditionExpression:      aws.String("attribute_not_exists(#pk)"),
			ExpressionAttributeNames: map[string]string{"#pk": "pk"},
		}},
		{Put: &types.Put{
			TableName: aws.String(s.table), Item: runItem,
			ConditionExpression:      aws.String("attribute_not_exists(#pk)"),
			ExpressionAttributeNames: map[string]string{"#pk": "pk"},
		}},
		{Put: &types.Put{
			TableName: aws.String(s.table), Item: pendingItem,
			ConditionExpression:      aws.String("attribute_not_exists(#pk)"),
			ExpressionAttributeNames: map[string]string{"#pk": "pk"},
		}},
	}})
	if err == nil {
		return hook.QueueCreated, nil
	}
	var canceled *types.TransactionCanceledException
	if !errors.As(err, &canceled) {
		return "", hook.NewExternalFailure("dynamodb", hook.FailureRetryable, "queue_write_failed")
	}
	binding, err := s.loadBinding(ctx, eventKey, runKey)
	if err != nil {
		return "", err
	}
	if !binding.matches(request.Envelope, eventKey, runKey, string(encoded)) {
		return hook.QueueConflict, nil
	}
	switch binding.state {
	case stateQueued:
		return hook.QueueDuplicate, nil
	case stateClaimed, stateQuestionPending, stateAwaitingAnswer, stateReportPending, stateTerminal:
		return hook.QueueClaimed, nil
	default:
		return "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "invalid_queue_state")
	}
}

func (s *DynamoStore) Pull(ctx context.Context, request hook.PullClaimRequest) (hook.DispatchEnvelope, hook.PullDisposition, error) {
	if !validPullRequest(request) {
		return hook.DispatchEnvelope{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "invalid_pull_request")
	}
	// The puller asks for work, not for a named ticket: which ticket is waiting
	// is the store's answer, read from the marker Enqueue placed.
	runID := request.RunID
	if runID == "" {
		pending, pendingErr := s.api.GetItem(ctx, &dynamodb.GetItemInput{
			TableName:      aws.String(s.table),
			Key:            map[string]types.AttributeValue{"pk": stringValue(makeKey("pending", request.SpaceKey, strconv.FormatInt(request.ProjectID, 10)))},
			ConsistentRead: aws.Bool(true),
		})
		if pendingErr != nil {
			return hook.DispatchEnvelope{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRetryable, "pull_read_failed")
		}
		if len(pending.Item) == 0 {
			return hook.DispatchEnvelope{}, hook.PullEmpty, nil
		}
		value, ok := attributeString(pending.Item, "run_id")
		if !ok || !hook.ValidRunID(value) {
			return hook.DispatchEnvelope{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "pull_binding_invalid")
		}
		runID = value
	}
	runKey := makeKey("run", request.SpaceKey, strconv.FormatInt(request.ProjectID, 10), runID)
	runOutput, err := s.api.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table), Key: map[string]types.AttributeValue{"pk": stringValue(runKey)}, ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return hook.DispatchEnvelope{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRetryable, "pull_read_failed")
	}
	if len(runOutput.Item) == 0 {
		return hook.DispatchEnvelope{}, hook.PullEmpty, nil
	}
	stateValue, stateOK := attributeString(runOutput.Item, "state")
	if !stateOK {
		return hook.DispatchEnvelope{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "pull_binding_invalid")
	}
	if stateValue != stateQueued && stateValue != stateClaimed && stateValue != stateQuestionPending &&
		stateValue != stateAwaitingAnswer && stateValue != stateReportPending && stateValue != stateTerminal {
		return hook.DispatchEnvelope{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "pull_state_invalid")
	}
	envelopeJSON, ok := attributeString(runOutput.Item, "envelope_json")
	if !ok || len(envelopeJSON) == 0 || len(envelopeJSON) > 65535 {
		return hook.DispatchEnvelope{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "pull_envelope_invalid")
	}
	envelope, err := decodeEnvelope([]byte(envelopeJSON))
	if err != nil || !snapshotAllowed(envelope.Snapshot, request) {
		return hook.DispatchEnvelope{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "pull_envelope_invalid")
	}
	snapshot := envelope.Snapshot
	eventKey := makeKey("event", snapshot.SpaceKey, strconv.FormatInt(snapshot.ProjectID, 10), strconv.FormatInt(snapshot.ActivityID, 10))
	if !runItemMatches(runOutput.Item, envelope, eventKey, runKey) {
		return hook.DispatchEnvelope{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "pull_binding_invalid")
	}
	queuedAt, ok := attributeInt64(runOutput.Item, "queued_at")
	if !ok || request.IssuedAt.UTC().UnixMilli() < queuedAt {
		return hook.DispatchEnvelope{}, hook.PullConflict, nil
	}
	eventOutput, err := s.api.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table), Key: map[string]types.AttributeValue{"pk": stringValue(eventKey)}, ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return hook.DispatchEnvelope{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRetryable, "pull_read_failed")
	}
	if !eventItemMatches(eventOutput.Item, envelope, runKey) {
		return hook.DispatchEnvelope{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "pull_binding_invalid")
	}
	if stateValue == stateQuestionPending || stateValue == stateAwaitingAnswer ||
		stateValue == stateReportPending || stateValue == stateTerminal {
		return hook.DispatchEnvelope{}, hook.PullClaimed, nil
	}
	if stateValue == stateClaimed {
		claim, valid := decodeStoredPullClaim(runOutput.Item, request, queuedAt)
		if !valid {
			return hook.DispatchEnvelope{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "pull_binding_invalid")
		}
		if claim.owner == request.Owner {
			return attachClarification(runOutput.Item, envelope)
		}
		return hook.DispatchEnvelope{}, hook.PullClaimed, nil
	}
	_, err = s.api.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
		{ConditionCheck: &types.ConditionCheck{
			TableName: aws.String(s.table), Key: map[string]types.AttributeValue{"pk": stringValue(eventKey)},
			ConditionExpression: aws.String("#record_type = :event_type AND #activity_id = :activity_id AND #run_key = :run_key AND #delivery = :delivery AND #digest = :digest"),
			ExpressionAttributeNames: map[string]string{
				"#record_type": "record_type", "#activity_id": "activity_id", "#run_key": "run_key",
				"#delivery": "delivery_id", "#digest": "input_sha256",
			},
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":event_type": stringValue("event"), ":activity_id": numberValue(snapshot.ActivityID),
				":run_key": stringValue(runKey), ":delivery": stringValue(envelope.DeliveryID), ":digest": stringValue(snapshot.InputSHA256),
			},
		}},
		{Update: &types.Update{
			TableName: aws.String(s.table), Key: map[string]types.AttributeValue{"pk": stringValue(runKey)},
			UpdateExpression:    aws.String("SET #state = :claimed, #claimed_at = :claimed_at, #repository_id = :repository_id, #repository_digest = :repository_digest, #workflow_ref_digest = :workflow_ref_digest, #workflow_sha = :workflow_sha, #workflow_run_id = :workflow_run_id, #run_attempt = :run_attempt"),
			ConditionExpression: aws.String("#state = :queued AND #record_type = :run_type AND #activity_id = :activity_id AND #run_id = :run_id AND #event_key = :event_key AND #delivery = :delivery AND #digest = :digest AND #envelope = :envelope AND #queued_at = :queued_at"),
			ExpressionAttributeNames: map[string]string{
				"#state": "state", "#claimed_at": "claimed_at", "#repository_id": "repository_id",
				"#repository_digest": "repository_sha256", "#workflow_ref_digest": "workflow_ref_sha256",
				"#workflow_sha": "workflow_sha", "#workflow_run_id": "workflow_run_id", "#run_attempt": "run_attempt",
				"#record_type": "record_type", "#activity_id": "activity_id", "#run_id": "run_id",
				"#event_key": "event_key", "#delivery": "delivery_id", "#digest": "input_sha256",
				"#envelope": "envelope_json", "#queued_at": "queued_at",
			},
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":claimed": stringValue(stateClaimed), ":queued": stringValue(stateQueued),
				":run_type": stringValue("run"), ":activity_id": numberValue(snapshot.ActivityID), ":run_id": stringValue(snapshot.RunID),
				":claimed_at":          numberValue(request.ClaimedAt.UTC().UnixMilli()),
				":repository_id":       numberValue(request.Owner.RepositoryID),
				":repository_digest":   stringValue(request.Owner.RepositorySHA256),
				":workflow_ref_digest": stringValue(request.Owner.WorkflowRefSHA256),
				":workflow_sha":        stringValue(request.Owner.WorkflowSHA),
				":workflow_run_id":     numberValue(request.Owner.WorkflowRunID),
				":run_attempt":         numberValue(int64(request.Owner.RunAttempt)),
				":event_key":           stringValue(eventKey), ":delivery": stringValue(envelope.DeliveryID),
				":digest": stringValue(snapshot.InputSHA256), ":envelope": stringValue(envelopeJSON),
				":queued_at": numberValue(queuedAt),
			},
		}},
	}})
	if err == nil {
		return attachClarification(runOutput.Item, envelope)
	}
	return s.resolvePullWrite(ctx, request, envelope, eventKey, runKey)
}

// attachClarification copies the sealed clarification of a resumed run onto
// the returned envelope so the worker receives the adopted answers with the
// ticket. Partial or corrupted clarification state fails the pull closed
// instead of silently dispatching a revision-1 view of a resumed run.
func attachClarification(item map[string]types.AttributeValue, envelope hook.DispatchEnvelope) (hook.DispatchEnvelope, hook.PullDisposition, error) {
	digest, _, ok := clarificationStateConsistent(item)
	if !ok {
		return hook.DispatchEnvelope{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "pull_binding_invalid")
	}
	if digest == "" {
		return envelope, hook.PullAcquired, nil
	}
	recordJSON, _ := attributeString(item, "clarification_json")
	envelope.ClarificationJSON = recordJSON
	if hook.ValidateEnvelope(envelope) != nil {
		return hook.DispatchEnvelope{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "pull_binding_invalid")
	}
	return envelope, hook.PullAcquired, nil
}

type storedPullClaim struct {
	owner     hook.PullOwner
	claimedAt int64
}

func (s *DynamoStore) resolvePullWrite(
	ctx context.Context,
	request hook.PullClaimRequest,
	envelope hook.DispatchEnvelope,
	eventKey, runKey string,
) (hook.DispatchEnvelope, hook.PullDisposition, error) {
	latest, err := s.api.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table), Key: map[string]types.AttributeValue{"pk": stringValue(runKey)}, ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return hook.DispatchEnvelope{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRetryable, "pull_read_failed")
	}
	latestState, ok := attributeString(latest.Item, "state")
	if !ok || !runItemMatches(latest.Item, envelope, eventKey, runKey) {
		return hook.DispatchEnvelope{}, hook.PullConflict, nil
	}
	switch latestState {
	case stateQueued:
		return hook.DispatchEnvelope{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRetryable, "pull_write_failed")
	case stateQuestionPending, stateAwaitingAnswer, stateReportPending, stateTerminal:
		return hook.DispatchEnvelope{}, hook.PullClaimed, nil
	case stateClaimed:
		claim, valid := decodeStoredPullClaim(latest.Item, request, 0)
		if !valid {
			return hook.DispatchEnvelope{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "pull_binding_invalid")
		}
		if claim.owner == request.Owner {
			return attachClarification(latest.Item, envelope)
		}
		return hook.DispatchEnvelope{}, hook.PullClaimed, nil
	default:
		return hook.DispatchEnvelope{}, hook.PullConflict, nil
	}
}

func decodeStoredPullClaim(item map[string]types.AttributeValue, request hook.PullClaimRequest, queuedAt int64) (storedPullClaim, bool) {
	claimedAt, claimedOK := attributeInt64(item, "claimed_at")
	repositoryID, repositoryOK := attributeInt64(item, "repository_id")
	repositoryDigest, repositoryDigestOK := attributeString(item, "repository_sha256")
	workflowRefDigest, workflowRefOK := attributeString(item, "workflow_ref_sha256")
	workflowSHA, workflowSHAOK := attributeString(item, "workflow_sha")
	workflowRunID, workflowRunOK := attributeInt64(item, "workflow_run_id")
	runAttempt, runAttemptOK := attributeInt64(item, "run_attempt")
	claim := storedPullClaim{owner: hook.PullOwner{
		RepositoryID: repositoryID, RepositorySHA256: repositoryDigest, WorkflowRefSHA256: workflowRefDigest,
		WorkflowSHA: workflowSHA, WorkflowRunID: workflowRunID, RunAttempt: int(runAttempt),
	}, claimedAt: claimedAt}
	return claim, claimedOK && repositoryOK && repositoryDigestOK && workflowRefOK && workflowSHAOK && workflowRunOK && runAttemptOK &&
		claimedAt > 0 && (queuedAt == 0 || claimedAt >= queuedAt) &&
		repositoryID == request.Target.RepositoryID && repositoryDigest == request.Owner.RepositorySHA256 &&
		workflowRefDigest == request.Target.WorkflowRefSHA256 && commitPattern.MatchString(workflowSHA) && workflowRunID > 0 && runAttempt > 0 &&
		terminalStateShapeValid(item)
}

func (s *DynamoStore) BeginTerminal(ctx context.Context, request hook.TerminalBeginRequest) (hook.TerminalBinding, hook.TerminalBeginDisposition, error) {
	if !validTerminalBeginRequest(request) {
		return hook.TerminalBinding{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "invalid_terminal_begin")
	}
	binding, err := s.loadTerminalBinding(ctx, request.Report.AutomationRunID, request.Route)
	if err != nil {
		return hook.TerminalBinding{}, "", err
	}
	if !terminalBindingMatches(binding, request.Report, request.Route) {
		return hook.TerminalBinding{}, hook.TerminalBeginConflict, nil
	}
	result := hook.TerminalBinding{IssueID: binding.envelope.Snapshot.IssueID, IssueKey: binding.envelope.Snapshot.IssueKey}
	stateValue, _ := attributeString(binding.runItem, "state")
	switch stateValue {
	case stateTerminal:
		if terminalReportMatches(binding.runItem, request.ReportSHA256, request.Report.Code) && validStoredCommentID(binding.runItem) {
			return result, hook.TerminalBeginComplete, nil
		}
		return result, hook.TerminalBeginConflict, nil
	case stateReportPending:
		if !terminalReportMatches(binding.runItem, request.ReportSHA256, request.Report.Code) {
			return result, hook.TerminalBeginConflict, nil
		}
		leaseUntil, ok := attributeInt64(binding.runItem, "terminal_lease_until")
		if !ok {
			return hook.TerminalBinding{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "terminal_binding_invalid")
		}
		if leaseUntil >= request.StartedAt.UnixMilli() {
			return result, hook.TerminalBeginBusy, nil
		}
		return s.reacquireTerminal(ctx, binding, request, result, leaseUntil)
	case stateClaimed:
		if !terminalCodeAllowedFromClaimed(request.Report.Code) {
			return result, hook.TerminalBeginConflict, nil
		}
		return s.startTerminal(ctx, binding, request, result)
	case stateAwaitingAnswer:
		if !terminalCodeAllowedFromAwaiting(request.Report.Code) {
			return result, hook.TerminalBeginConflict, nil
		}
		return s.startTerminalFromAwaiting(ctx, binding, request, result)
	default:
		return result, hook.TerminalBeginConflict, nil
	}
}

// terminalCodeAllowedFromClaimed rejects codes that only make sense for a run
// that is waiting for an answer. Everything else terminates from claimed as
// before.
func terminalCodeAllowedFromClaimed(code hook.TerminalCode) bool {
	return code != hook.TerminalClarificationExpired
}

// terminalCodeAllowedFromAwaiting allows only the two finite endings of the
// answer wait: the deadline passed, or the requester cancelled. Any other
// terminal claim against a waiting run is a conflict.
func terminalCodeAllowedFromAwaiting(code hook.TerminalCode) bool {
	return code == hook.TerminalClarificationExpired || code == hook.TerminalCancelled
}

func (s *DynamoStore) CompleteTerminal(ctx context.Context, request hook.TerminalCompleteRequest) (hook.TerminalCompleteDisposition, error) {
	if !validTerminalCompleteRequest(request) {
		return "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "invalid_terminal_complete")
	}
	binding, err := s.loadTerminalBinding(ctx, request.Report.AutomationRunID, request.Route)
	if err != nil {
		return "", err
	}
	if !terminalBindingMatches(binding, request.Report, request.Route) {
		return hook.TerminalCompleteConflict, nil
	}
	stateValue, _ := attributeString(binding.runItem, "state")
	if stateValue == stateTerminal {
		if terminalReportMatches(binding.runItem, request.ReportSHA256, request.Report.Code) &&
			attributeInt64Equals(binding.runItem, "terminal_comment_id", request.CommentID) {
			return hook.TerminalAlreadyComplete, nil
		}
		return hook.TerminalCompleteConflict, nil
	}
	if stateValue != stateReportPending || !terminalReportMatches(binding.runItem, request.ReportSHA256, request.Report.Code) ||
		!attributeStringEquals(binding.runItem, "terminal_lease_token", request.LeaseToken) {
		return hook.TerminalCompleteConflict, nil
	}
	startedAt, ok := attributeInt64(binding.runItem, "terminal_started_at")
	if !ok || request.CompletedAt.UnixMilli() < startedAt {
		return "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "invalid_terminal_complete")
	}

	// The project's single pending slot is what admits the next ticket, and
	// enqueueing requires it to be absent. A finished run must hand it back,
	// or every completed ticket blocks the next one forever (measured
	// 2026-08-07: the second live ticket could not enqueue behind the first
	// one's leftover slot). The delete tolerates an already-absent slot and
	// refuses to release a slot that names a different run.
	pendingKey := makeKey("pending", binding.envelope.Snapshot.SpaceKey, strconv.FormatInt(binding.envelope.Snapshot.ProjectID, 10))
	_, err = s.api.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
		terminalEventCondition(s.table, binding),
		{Delete: &types.Delete{
			TableName:           aws.String(s.table),
			Key:                 map[string]types.AttributeValue{"pk": stringValue(pendingKey)},
			ConditionExpression: aws.String("attribute_not_exists(pk) OR run_id = :released_run"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":released_run": stringValue(request.Report.AutomationRunID),
			},
		}},
		{Update: &types.Update{
			TableName: aws.String(s.table), Key: map[string]types.AttributeValue{"pk": stringValue(binding.runKey)},
			UpdateExpression:    aws.String("SET #state = :terminal, #comment_id = :comment_id, #completed_at = :completed_at REMOVE #lease_token, #lease_until"),
			ConditionExpression: aws.String("#state = :pending AND #record_type = :run_type AND #activity_id = :activity_id AND #run_id = :run_id AND #event_key = :event_key AND #delivery = :delivery AND #digest = :input_digest AND #envelope = :envelope AND #repository_id = :repository_id AND #repository_digest = :repository_digest AND #workflow_ref_digest = :workflow_ref_digest AND #workflow_sha = :workflow_sha AND #workflow_run_id = :workflow_run_id AND #run_attempt = :run_attempt AND #report_digest = :report_digest AND #terminal_code = :terminal_code AND #started_at = :started_at AND #lease_token = :lease_token AND attribute_not_exists(#comment_id) AND attribute_not_exists(#completed_at)"),
			ExpressionAttributeNames: map[string]string{
				"#state": "state", "#comment_id": "terminal_comment_id", "#completed_at": "terminal_completed_at",
				"#lease_token": "terminal_lease_token", "#lease_until": "terminal_lease_until", "#record_type": "record_type", "#activity_id": "activity_id", "#run_id": "run_id",
				"#event_key": "event_key", "#delivery": "delivery_id", "#digest": "input_sha256",
				"#envelope": "envelope_json", "#terminal_code": "terminal_code", "#started_at": "terminal_started_at",
				"#repository_id": "repository_id", "#repository_digest": "repository_sha256",
				"#workflow_ref_digest": "workflow_ref_sha256", "#workflow_sha": "workflow_sha",
				"#workflow_run_id": "workflow_run_id", "#run_attempt": "run_attempt",
				"#report_digest": "terminal_report_sha256",
			},
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":terminal": stringValue(stateTerminal), ":pending": stringValue(stateReportPending), ":run_type": stringValue("run"),
				":comment_id": numberValue(request.CommentID), ":completed_at": numberValue(request.CompletedAt.UnixMilli()),
				":activity_id": numberValue(binding.envelope.Snapshot.ActivityID), ":run_id": stringValue(request.Report.AutomationRunID),
				":event_key": stringValue(binding.eventKey), ":delivery": stringValue(request.Report.DeliveryID),
				":input_digest": stringValue(request.Report.InputSHA256), ":envelope": stringValue(binding.envelopeJSON),
				":repository_id":     numberValue(request.Report.RepositoryID),
				":repository_digest": stringValue(request.Report.RepositorySHA256), ":workflow_ref_digest": stringValue(request.Report.WorkflowRefSHA256),
				":workflow_sha": stringValue(request.Report.WorkflowSHA), ":workflow_run_id": numberValue(request.Report.WorkflowRunID),
				":run_attempt": numberValue(int64(request.Report.RunAttempt)), ":report_digest": stringValue(request.ReportSHA256),
				":terminal_code": stringValue(string(request.Report.Code)),
				":started_at":    numberValue(startedAt),
				":lease_token":   stringValue(request.LeaseToken),
			},
		}},
	}})
	if err == nil {
		return hook.TerminalCompleted, nil
	}
	var canceled *types.TransactionCanceledException
	if !errors.As(err, &canceled) {
		return "", hook.NewExternalFailure("dynamodb", hook.FailureRetryable, "terminal_complete_write_failed")
	}
	latest, loadErr := s.loadTerminalBinding(ctx, request.Report.AutomationRunID, request.Route)
	if loadErr != nil {
		return "", loadErr
	}
	if !terminalBindingMatches(latest, request.Report, request.Route) {
		return hook.TerminalCompleteConflict, nil
	}
	latestState, _ := attributeString(latest.runItem, "state")
	if latestState == stateTerminal && terminalReportMatches(latest.runItem, request.ReportSHA256, request.Report.Code) &&
		attributeInt64Equals(latest.runItem, "terminal_comment_id", request.CommentID) {
		return hook.TerminalAlreadyComplete, nil
	}
	if latestState == stateReportPending && terminalReportMatches(latest.runItem, request.ReportSHA256, request.Report.Code) &&
		attributeStringEquals(latest.runItem, "terminal_lease_token", request.LeaseToken) {
		return "", hook.NewExternalFailure("dynamodb", hook.FailureRetryable, "terminal_complete_write_failed")
	}
	return hook.TerminalCompleteConflict, nil
}

func (s *DynamoStore) startTerminal(ctx context.Context, binding terminalStoredBinding, request hook.TerminalBeginRequest, result hook.TerminalBinding) (hook.TerminalBinding, hook.TerminalBeginDisposition, error) {
	names := terminalStartNames()
	names["#question_record_digest"] = "question_record_sha256"
	names["#question_comment_id"] = "question_comment_id"
	_, err := s.api.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
		terminalEventCondition(s.table, binding),
		{Update: &types.Update{
			TableName: aws.String(s.table), Key: map[string]types.AttributeValue{"pk": stringValue(binding.runKey)},
			UpdateExpression:          aws.String("SET #state = :pending, #report_digest = :report_digest, #terminal_code = :terminal_code, #started_at = :started_at, #lease_until = :lease_until, #lease_token = :lease_token"),
			ConditionExpression:       aws.String("#state = :claimed AND #record_type = :run_type AND #activity_id = :activity_id AND #event_key = :event_key AND #delivery = :delivery AND #digest = :input_digest AND #envelope = :envelope AND #run_id = :run_id AND #repository_id = :repository_id AND #repository_digest = :repository_digest AND #workflow_ref_digest = :workflow_ref_digest AND #workflow_sha = :workflow_sha AND #workflow_run_id = :workflow_run_id AND #run_attempt = :run_attempt AND attribute_not_exists(#report_digest) AND attribute_not_exists(#terminal_code) AND attribute_not_exists(#started_at) AND attribute_not_exists(#lease_until) AND attribute_not_exists(#lease_token) AND attribute_not_exists(#comment_id) AND attribute_not_exists(#completed_at) AND attribute_not_exists(#question_record_digest) AND attribute_not_exists(#question_comment_id)"),
			ExpressionAttributeNames:  names,
			ExpressionAttributeValues: terminalStartValues(binding, request),
		}},
	}})
	if err == nil {
		return result, hook.TerminalBeginAcquired, nil
	}
	return s.terminalBeginAfterWriteFailure(ctx, request, result, err)
}

// startTerminalFromAwaiting seals the terminal outcome of a run that was
// waiting for an answer. The question evidence (record, comment id) is kept on
// the item so the finished record still explains which question was posted and
// where. The condition pins the exact sealed question so a concurrent answer
// adoption and an expiry can never both win.
func (s *DynamoStore) startTerminalFromAwaiting(ctx context.Context, binding terminalStoredBinding, request hook.TerminalBeginRequest, result hook.TerminalBinding) (hook.TerminalBinding, hook.TerminalBeginDisposition, error) {
	questionDigest, ok := attributeString(binding.runItem, "question_record_sha256")
	if !ok {
		return hook.TerminalBinding{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "terminal_binding_invalid")
	}
	names := terminalStartNames()
	names["#question_record_digest"] = "question_record_sha256"
	names["#question_comment_id"] = "question_comment_id"
	values := terminalStartValues(binding, request)
	delete(values, ":claimed")
	values[":awaiting_source"] = stringValue(stateAwaitingAnswer)
	values[":question_record_digest"] = stringValue(questionDigest)
	_, err := s.api.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
		terminalEventCondition(s.table, binding),
		{Update: &types.Update{
			TableName: aws.String(s.table), Key: map[string]types.AttributeValue{"pk": stringValue(binding.runKey)},
			UpdateExpression:          aws.String("SET #state = :pending, #report_digest = :report_digest, #terminal_code = :terminal_code, #started_at = :started_at, #lease_until = :lease_until, #lease_token = :lease_token"),
			ConditionExpression:       aws.String("#state = :awaiting_source AND #record_type = :run_type AND #activity_id = :activity_id AND #event_key = :event_key AND #delivery = :delivery AND #digest = :input_digest AND #envelope = :envelope AND #run_id = :run_id AND #repository_id = :repository_id AND #repository_digest = :repository_digest AND #workflow_ref_digest = :workflow_ref_digest AND #workflow_sha = :workflow_sha AND #workflow_run_id = :workflow_run_id AND #run_attempt = :run_attempt AND #question_record_digest = :question_record_digest AND attribute_exists(#question_comment_id) AND attribute_not_exists(#report_digest) AND attribute_not_exists(#terminal_code) AND attribute_not_exists(#started_at) AND attribute_not_exists(#lease_until) AND attribute_not_exists(#lease_token) AND attribute_not_exists(#comment_id) AND attribute_not_exists(#completed_at)"),
			ExpressionAttributeNames:  names,
			ExpressionAttributeValues: values,
		}},
	}})
	if err == nil {
		return result, hook.TerminalBeginAcquired, nil
	}
	return s.terminalBeginAfterWriteFailure(ctx, request, result, err)
}

func (s *DynamoStore) reacquireTerminal(ctx context.Context, binding terminalStoredBinding, request hook.TerminalBeginRequest, result hook.TerminalBinding, previousLease int64) (hook.TerminalBinding, hook.TerminalBeginDisposition, error) {
	_, err := s.api.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
		terminalEventCondition(s.table, binding),
		{Update: &types.Update{
			TableName: aws.String(s.table), Key: map[string]types.AttributeValue{"pk": stringValue(binding.runKey)},
			UpdateExpression:    aws.String("SET #lease_until = :lease_until, #lease_token = :lease_token"),
			ConditionExpression: aws.String("#state = :pending AND #record_type = :run_type AND #activity_id = :activity_id AND #event_key = :event_key AND #delivery = :delivery AND #digest = :input_digest AND #envelope = :envelope AND #run_id = :run_id AND #repository_id = :repository_id AND #repository_digest = :repository_digest AND #workflow_ref_digest = :workflow_ref_digest AND #workflow_sha = :workflow_sha AND #workflow_run_id = :workflow_run_id AND #run_attempt = :run_attempt AND #report_digest = :report_digest AND #terminal_code = :terminal_code AND #lease_until = :previous_lease AND attribute_not_exists(#comment_id) AND attribute_not_exists(#completed_at)"),
			ExpressionAttributeNames: func() map[string]string {
				names := terminalStartNames()
				delete(names, "#started_at")
				return names
			}(),
			ExpressionAttributeValues: func() map[string]types.AttributeValue {
				values := terminalStartValues(binding, request)
				delete(values, ":claimed")
				delete(values, ":started_at")
				values[":previous_lease"] = numberValue(previousLease)
				return values
			}(),
		}},
	}})
	if err == nil {
		return result, hook.TerminalBeginAcquired, nil
	}
	return s.terminalBeginAfterWriteFailure(ctx, request, result, err)
}

func (s *DynamoStore) terminalBeginAfterWriteFailure(ctx context.Context, request hook.TerminalBeginRequest, result hook.TerminalBinding, writeErr error) (hook.TerminalBinding, hook.TerminalBeginDisposition, error) {
	var canceled *types.TransactionCanceledException
	if !errors.As(writeErr, &canceled) {
		return hook.TerminalBinding{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRetryable, "terminal_begin_write_failed")
	}
	if !onlyConditionalCancellation(canceled) {
		if hasCancellationReason(canceled, "ValidationError") {
			return hook.TerminalBinding{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRejected, "terminal_begin_write_rejected")
		}
		return hook.TerminalBinding{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRetryable, "terminal_begin_write_failed")
	}
	latest, err := s.loadTerminalBinding(ctx, request.Report.AutomationRunID, request.Route)
	if err != nil {
		return hook.TerminalBinding{}, "", err
	}
	if !terminalBindingMatches(latest, request.Report, request.Route) {
		return result, hook.TerminalBeginConflict, nil
	}
	stateValue, _ := attributeString(latest.runItem, "state")
	if stateValue == stateTerminal && terminalReportMatches(latest.runItem, request.ReportSHA256, request.Report.Code) && validStoredCommentID(latest.runItem) {
		return result, hook.TerminalBeginComplete, nil
	}
	if stateValue == stateReportPending && terminalReportMatches(latest.runItem, request.ReportSHA256, request.Report.Code) {
		return result, hook.TerminalBeginBusy, nil
	}
	// A source state that did not move (claimed stayed claimed, awaiting
	// stayed awaiting) means the conditional write lost to something
	// transient, not to a competing outcome: retry instead of declaring a
	// permanent conflict. A retry against a state the code is not allowed to
	// terminate from resolves to conflict in the BeginTerminal switch.
	if stateValue == stateClaimed || stateValue == stateAwaitingAnswer {
		return hook.TerminalBinding{}, "", hook.NewExternalFailure("dynamodb", hook.FailureRetryable, "terminal_begin_write_failed")
	}
	return result, hook.TerminalBeginConflict, nil
}

func onlyConditionalCancellation(canceled *types.TransactionCanceledException) bool {
	if canceled == nil || len(canceled.CancellationReasons) == 0 {
		return false
	}
	found := false
	for _, reason := range canceled.CancellationReasons {
		switch aws.ToString(reason.Code) {
		case "", "None":
		case "ConditionalCheckFailed":
			found = true
		default:
			return false
		}
	}
	return found
}

func hasCancellationReason(canceled *types.TransactionCanceledException, expected string) bool {
	if canceled == nil {
		return false
	}
	for _, reason := range canceled.CancellationReasons {
		if aws.ToString(reason.Code) == expected {
			return true
		}
	}
	return false
}

type terminalStoredBinding struct {
	runItem      map[string]types.AttributeValue
	eventItem    map[string]types.AttributeValue
	envelope     hook.DispatchEnvelope
	envelopeJSON string
	runKey       string
	eventKey     string
}

// resolveRunRoute rebinds a routeful-but-recordless caller (the tick and the
// run notices it posts) to the run the project's pending slot holds. A tick
// carries no issue: it authenticates with the fixed automation run id, but
// run rows are keyed by issue, so looking up the fixed id finds nothing and
// every question wait looks idle — observed live on the first asked question
// (observed on the first question round in production): the posted answer sat unadopted through every tick. The
// pending slot is the one durable pointer from "this project" to "the run in
// flight". A route whose id already names an existing run row is kept as is,
// so a caller bound to a specific run can never be redirected to whichever
// run holds the slot later.
func (s *DynamoStore) resolveRunRoute(ctx context.Context, route hook.ReportRouteConfig) (hook.ReportRouteConfig, error) {
	directKey := makeKey("run", route.SpaceKey, strconv.FormatInt(route.ProjectID, 10), route.ExpectedRunID)
	direct, err := s.api.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table), Key: map[string]types.AttributeValue{"pk": stringValue(directKey)}, ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return route, hook.NewExternalFailure("dynamodb", hook.FailureRetryable, "run_route_read_failed")
	}
	if len(direct.Item) != 0 {
		return route, nil
	}
	pendingKey := makeKey("pending", route.SpaceKey, strconv.FormatInt(route.ProjectID, 10))
	output, err := s.api.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table), Key: map[string]types.AttributeValue{"pk": stringValue(pendingKey)}, ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return route, hook.NewExternalFailure("dynamodb", hook.FailureRetryable, "run_route_read_failed")
	}
	runID, ok := attributeString(output.Item, "run_id")
	if !ok || !strings.HasPrefix(runID, route.ProjectKey+"-") || !issueRunIDPattern.MatchString(runID) {
		return route, nil
	}
	rebound := route
	rebound.ExpectedRunID = runID
	return rebound, nil
}

var issueRunIDPattern = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,15}-[1-9][0-9]{0,8}$`)

func (s *DynamoStore) loadTerminalBinding(ctx context.Context, runID string, route hook.ReportRouteConfig) (terminalStoredBinding, error) {
	runKey := makeKey("run", route.SpaceKey, strconv.FormatInt(route.ProjectID, 10), runID)
	runOutput, err := s.api.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table), Key: map[string]types.AttributeValue{"pk": stringValue(runKey)}, ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return terminalStoredBinding{}, hook.NewExternalFailure("dynamodb", hook.FailureRetryable, "terminal_read_failed")
	}
	if len(runOutput.Item) == 0 {
		return terminalStoredBinding{}, hook.NewExternalFailure("dynamodb", hook.FailureRejected, "terminal_binding_missing")
	}
	envelopeJSON, ok := attributeString(runOutput.Item, "envelope_json")
	if !ok || len(envelopeJSON) == 0 || len(envelopeJSON) > 65535 {
		return terminalStoredBinding{}, hook.NewExternalFailure("dynamodb", hook.FailureRejected, "terminal_binding_invalid")
	}
	envelope, err := decodeEnvelope([]byte(envelopeJSON))
	if err != nil {
		return terminalStoredBinding{}, hook.NewExternalFailure("dynamodb", hook.FailureRejected, "terminal_binding_invalid")
	}
	eventKey := makeKey("event", envelope.Snapshot.SpaceKey, strconv.FormatInt(envelope.Snapshot.ProjectID, 10), strconv.FormatInt(envelope.Snapshot.ActivityID, 10))
	eventOutput, err := s.api.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table), Key: map[string]types.AttributeValue{"pk": stringValue(eventKey)}, ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return terminalStoredBinding{}, hook.NewExternalFailure("dynamodb", hook.FailureRetryable, "terminal_read_failed")
	}
	return terminalStoredBinding{
		runItem: runOutput.Item, eventItem: eventOutput.Item, envelope: envelope, envelopeJSON: envelopeJSON,
		runKey: runKey, eventKey: eventKey,
	}, nil
}

func terminalBindingMatches(binding terminalStoredBinding, report hook.TerminalReportRequest, route hook.ReportRouteConfig) bool {
	snapshot := binding.envelope.Snapshot
	return runItemMatches(binding.runItem, binding.envelope, binding.eventKey, binding.runKey) &&
		eventItemMatches(binding.eventItem, binding.envelope, binding.runKey) &&
		terminalStateShapeValid(binding.runItem) &&
		snapshot.SpaceKey == route.SpaceKey && snapshot.ProjectID == route.ProjectID && snapshot.ProjectKey == route.ProjectKey &&
		snapshot.CreatorID == route.AllowedCreatorID && snapshot.ActivityType == route.AllowedActivityType &&
		snapshot.RunID == route.ExpectedRunID && snapshot.Target == route.Target &&
		report.DeliveryID == binding.envelope.DeliveryID && report.InputSHA256 == snapshot.InputSHA256 &&
		report.AutomationRunID == snapshot.RunID && report.RepositoryID == route.RepositoryID &&
		attributeInt64Equals(binding.runItem, "repository_id", report.RepositoryID) &&
		attributeStringEquals(binding.runItem, "repository_sha256", report.RepositorySHA256) &&
		attributeStringEquals(binding.runItem, "workflow_ref_sha256", report.WorkflowRefSHA256) &&
		attributeStringEquals(binding.runItem, "workflow_sha", report.WorkflowSHA) &&
		attributeInt64Equals(binding.runItem, "workflow_run_id", report.WorkflowRunID) &&
		attributeInt64Equals(binding.runItem, "run_attempt", int64(report.RunAttempt))
}

func terminalStateShapeValid(item map[string]types.AttributeValue) bool {
	stateValue, ok := attributeString(item, "state")
	if !ok {
		return false
	}
	clarificationDigest, clarificationRounds, clarificationOK := clarificationStateConsistent(item)
	if !clarificationOK {
		return false
	}
	terminalFields := []string{
		"terminal_report_sha256", "terminal_code", "terminal_started_at",
		"terminal_lease_until", "terminal_lease_token", "terminal_comment_id", "terminal_completed_at",
	}
	questionFields := []string{
		"question_record_sha256", "question_record_json", "question_started_at",
		"question_lease_until", "question_lease_token", "question_comment_id", "question_posted_at",
	}
	if stateValue == stateQueued {
		// A queued item is either freshly enqueued or resumed after an
		// adopted answer; in both shapes no claim, question or terminal
		// evidence may remain on it.
		for _, name := range append(append([]string{"claimed_at"}, terminalFields...), questionFields...) {
			if _, exists := item[name]; exists {
				return false
			}
		}
		return true
	}
	claimedAt, claimedOK := attributeInt64(item, "claimed_at")
	if !claimedOK || claimedAt <= 0 {
		return false
	}
	switch stateValue {
	case stateClaimed:
		for _, name := range append(append([]string{}, terminalFields...), questionFields...) {
			if _, exists := item[name]; exists {
				return false
			}
		}
		return true
	case stateQuestionPending:
		for _, name := range terminalFields {
			if _, exists := item[name]; exists {
				return false
			}
		}
		recordDigest, digestOK := attributeString(item, "question_record_sha256")
		recordJSON, jsonOK := attributeString(item, "question_record_json")
		startedAt, startedOK := attributeInt64(item, "question_started_at")
		leaseUntil, leaseOK := attributeInt64(item, "question_lease_until")
		leaseToken, tokenOK := attributeString(item, "question_lease_token")
		_, commentExists := item["question_comment_id"]
		_, postedExists := item["question_posted_at"]
		record, recordOK := questionRecordJSONValid(recordJSON, recordDigest)
		return digestOK && digestPattern.MatchString(recordDigest) &&
			jsonOK && recordOK && questionRevisionConsistent(record, clarificationDigest, clarificationRounds) &&
			startedOK && startedAt > 0 && leaseOK && leaseUntil >= startedAt &&
			tokenOK && leasePattern.MatchString(leaseToken) && !commentExists && !postedExists
	case stateAwaitingAnswer:
		for _, name := range terminalFields {
			if _, exists := item[name]; exists {
				return false
			}
		}
		recordDigest, digestOK := attributeString(item, "question_record_sha256")
		recordJSON, jsonOK := attributeString(item, "question_record_json")
		startedAt, startedOK := attributeInt64(item, "question_started_at")
		commentID, commentOK := attributeInt64(item, "question_comment_id")
		postedAt, postedOK := attributeInt64(item, "question_posted_at")
		_, leaseExists := item["question_lease_until"]
		_, tokenExists := item["question_lease_token"]
		record, recordOK := questionRecordJSONValid(recordJSON, recordDigest)
		return digestOK && digestPattern.MatchString(recordDigest) &&
			jsonOK && recordOK && questionRevisionConsistent(record, clarificationDigest, clarificationRounds) &&
			startedOK && startedAt > 0 && commentOK && commentID > 0 &&
			postedOK && postedAt >= startedAt && !leaseExists && !tokenExists
	case stateReportPending:
		reportDigest, digestOK := attributeString(item, "terminal_report_sha256")
		code, codeOK := attributeString(item, "terminal_code")
		startedAt, startedOK := attributeInt64(item, "terminal_started_at")
		leaseUntil, leaseOK := attributeInt64(item, "terminal_lease_until")
		leaseToken, tokenOK := attributeString(item, "terminal_lease_token")
		_, commentExists := item["terminal_comment_id"]
		_, completeExists := item["terminal_completed_at"]
		return digestOK && digestPattern.MatchString(reportDigest) &&
			codeOK && validTerminalCode(code) && startedOK && startedAt > 0 && leaseOK && leaseUntil >= startedAt &&
			tokenOK && leasePattern.MatchString(leaseToken) && !commentExists && !completeExists
	case stateTerminal:
		reportDigest, digestOK := attributeString(item, "terminal_report_sha256")
		code, codeOK := attributeString(item, "terminal_code")
		startedAt, startedOK := attributeInt64(item, "terminal_started_at")
		commentID, commentOK := attributeInt64(item, "terminal_comment_id")
		completedAt, completedOK := attributeInt64(item, "terminal_completed_at")
		_, leaseExists := item["terminal_lease_until"]
		_, tokenExists := item["terminal_lease_token"]
		return digestOK && digestPattern.MatchString(reportDigest) &&
			codeOK && validTerminalCode(code) && startedOK && startedAt > 0 && commentOK && commentID > 0 &&
			completedOK && completedAt >= startedAt && !leaseExists && !tokenExists
	default:
		return false
	}
}

func validTerminalCode(code string) bool {
	// One list, kept in the hook: a terminal code the hook accepts must be
	// one the ledger records, or a run whose comment was posted stays
	// "report pending" forever.
	return hook.TerminalCode(code).Valid()
}

// questionRecordJSONValid re-checks that the stored sealed question record is
// canonical and matches its stored digest. It keeps a corrupted or truncated
// record from being treated as a valid wait.
func questionRecordJSONValid(recordJSON, recordDigest string) (hook.QuestionRecord, bool) {
	if len(recordJSON) == 0 || len(recordJSON) > hook.MaxQuestionRecordBytes {
		return hook.QuestionRecord{}, false
	}
	if hook.TerminalReportDigest([]byte(recordJSON)) != recordDigest {
		return hook.QuestionRecord{}, false
	}
	record, err := hook.DecodeQuestionRecord([]byte(recordJSON))
	if err != nil {
		return hook.QuestionRecord{}, false
	}
	return record, true
}

// questionRevisionConsistent ties the active question to the clarification
// history on the same item: the question revision must be exactly one past the
// adopted rounds, and a round-2 question must chain to the sealed record that
// adopted the round-1 answer.
func questionRevisionConsistent(record hook.QuestionRecord, clarificationDigest string, clarificationRounds int) bool {
	if record.QuestionRevision != clarificationRounds+1 {
		return false
	}
	if clarificationRounds == 0 {
		return record.ClarificationSHA256 == ""
	}
	return record.ClarificationSHA256 == clarificationDigest
}

// clarificationStateConsistent validates the resumed-input fields as a unit:
// either none are present (revision 1 input) or all are present and the sealed
// cumulative record is canonical, matches its digest and matches the stored
// revision counter. Partial or corrupted clarification state fails closed.
func clarificationStateConsistent(item map[string]types.AttributeValue) (string, int, bool) {
	digest, digestExists := attributeString(item, "clarification_sha256")
	recordJSON, jsonExists := attributeString(item, "clarification_json")
	revision, revisionExists := attributeInt64(item, "input_revision")
	if !digestExists && !jsonExists && !revisionExists {
		if _, exists := item["clarification_sha256"]; exists {
			return "", 0, false
		}
		if _, exists := item["clarification_json"]; exists {
			return "", 0, false
		}
		if _, exists := item["input_revision"]; exists {
			return "", 0, false
		}
		return "", 0, true
	}
	if !digestExists || !jsonExists || !revisionExists {
		return "", 0, false
	}
	if !digestPattern.MatchString(digest) || hook.TerminalReportDigest([]byte(recordJSON)) != digest {
		return "", 0, false
	}
	record, err := hook.DecodeClarificationRecord([]byte(recordJSON))
	if err != nil || int64(record.InputRevision) != revision {
		return "", 0, false
	}
	return digest, len(record.Rounds), true
}

func terminalEventCondition(table string, binding terminalStoredBinding) types.TransactWriteItem {
	snapshot := binding.envelope.Snapshot
	return types.TransactWriteItem{ConditionCheck: &types.ConditionCheck{
		TableName: aws.String(table), Key: map[string]types.AttributeValue{"pk": stringValue(binding.eventKey)},
		ConditionExpression: aws.String("#record_type = :event_type AND #activity_id = :activity_id AND #run_key = :run_key AND #delivery = :delivery AND #digest = :digest"),
		ExpressionAttributeNames: map[string]string{
			"#record_type": "record_type", "#activity_id": "activity_id", "#run_key": "run_key",
			"#delivery": "delivery_id", "#digest": "input_sha256",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":event_type": stringValue("event"), ":activity_id": numberValue(snapshot.ActivityID),
			":run_key": stringValue(binding.runKey), ":delivery": stringValue(binding.envelope.DeliveryID),
			":digest": stringValue(snapshot.InputSHA256),
		},
	}}
}

func terminalStartNames() map[string]string {
	return map[string]string{
		"#state": "state", "#report_digest": "terminal_report_sha256",
		"#terminal_code": "terminal_code", "#started_at": "terminal_started_at", "#lease_until": "terminal_lease_until",
		"#lease_token": "terminal_lease_token", "#record_type": "record_type", "#activity_id": "activity_id", "#event_key": "event_key",
		"#comment_id": "terminal_comment_id", "#completed_at": "terminal_completed_at",
		"#delivery": "delivery_id", "#digest": "input_sha256", "#run_id": "run_id", "#repository_id": "repository_id",
		"#envelope":          "envelope_json",
		"#repository_digest": "repository_sha256", "#workflow_ref_digest": "workflow_ref_sha256",
		"#workflow_sha": "workflow_sha", "#workflow_run_id": "workflow_run_id", "#run_attempt": "run_attempt",
	}
}

func terminalStartValues(binding terminalStoredBinding, request hook.TerminalBeginRequest) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		":pending": stringValue(stateReportPending), ":claimed": stringValue(stateClaimed), ":run_type": stringValue("run"),
		":report_digest": stringValue(request.ReportSHA256),
		":terminal_code": stringValue(string(request.Report.Code)), ":started_at": numberValue(request.StartedAt.UnixMilli()),
		":lease_until": numberValue(request.LeaseUntil.UnixMilli()), ":lease_token": stringValue(request.LeaseToken),
		":activity_id": numberValue(binding.envelope.Snapshot.ActivityID), ":event_key": stringValue(binding.eventKey),
		":delivery": stringValue(request.Report.DeliveryID), ":envelope": stringValue(binding.envelopeJSON),
		":input_digest": stringValue(request.Report.InputSHA256), ":run_id": stringValue(request.Report.AutomationRunID),
		":repository_id": numberValue(request.Report.RepositoryID), ":repository_digest": stringValue(request.Report.RepositorySHA256),
		":workflow_ref_digest": stringValue(request.Report.WorkflowRefSHA256), ":workflow_sha": stringValue(request.Report.WorkflowSHA),
		":workflow_run_id": numberValue(request.Report.WorkflowRunID), ":run_attempt": numberValue(int64(request.Report.RunAttempt)),
	}
}

func terminalReportMatches(item map[string]types.AttributeValue, digest string, code hook.TerminalCode) bool {
	return attributeStringEquals(item, "terminal_report_sha256", digest) && attributeStringEquals(item, "terminal_code", string(code))
}

func validStoredCommentID(item map[string]types.AttributeValue) bool {
	value, ok := attributeInt64(item, "terminal_comment_id")
	return ok && value > 0
}

func validTerminalBeginRequest(request hook.TerminalBeginRequest) bool {
	encoded, err := hook.MarshalTerminalReportRecord(request.Report)
	return err == nil && request.Report.ValidateRoute(request.Route) == nil && string(encoded) == request.ReportJSON &&
		digestPattern.MatchString(request.ReportSHA256) && hook.TerminalReportDigest(encoded) == request.ReportSHA256 &&
		leasePattern.MatchString(request.LeaseToken) && !request.StartedAt.IsZero() && request.StartedAt.Equal(request.StartedAt.UTC()) &&
		request.LeaseUntil.Equal(request.LeaseUntil.UTC()) && request.LeaseUntil.Equal(request.StartedAt.Add(request.Route.LeaseDuration)) &&
		!request.Report.IssuedAt.Before(request.StartedAt.Add(-request.Route.ClockSkew)) &&
		!request.Report.IssuedAt.After(request.StartedAt.Add(request.Route.ClockSkew))
}

func validTerminalCompleteRequest(request hook.TerminalCompleteRequest) bool {
	encoded, err := hook.MarshalTerminalReportRecord(request.Report)
	return err == nil && request.Report.ValidateRoute(request.Route) == nil && string(encoded) == request.ReportJSON &&
		digestPattern.MatchString(request.ReportSHA256) && hook.TerminalReportDigest(encoded) == request.ReportSHA256 &&
		leasePattern.MatchString(request.LeaseToken) && request.CommentID > 0 && !request.CompletedAt.IsZero() && request.CompletedAt.Equal(request.CompletedAt.UTC())
}

type storedBinding struct {
	eventItem map[string]types.AttributeValue
	runItem   map[string]types.AttributeValue
	state     string
}

func (s *DynamoStore) loadBinding(ctx context.Context, eventKey, runKey string) (storedBinding, error) {
	eventOutput, err := s.api.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table), Key: map[string]types.AttributeValue{"pk": stringValue(eventKey)}, ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return storedBinding{}, hook.NewExternalFailure("dynamodb", hook.FailureRetryable, "queue_read_failed")
	}
	runOutput, err := s.api.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.table), Key: map[string]types.AttributeValue{"pk": stringValue(runKey)}, ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return storedBinding{}, hook.NewExternalFailure("dynamodb", hook.FailureRetryable, "queue_read_failed")
	}
	stateValue, _ := attributeString(runOutput.Item, "state")
	if len(eventOutput.Item) == 0 && len(runOutput.Item) == 0 {
		return storedBinding{}, hook.NewExternalFailure("dynamodb", hook.FailureRetryable, "queue_conflict_unresolved")
	}
	return storedBinding{eventItem: eventOutput.Item, runItem: runOutput.Item, state: stateValue}, nil
}

func (b storedBinding) matches(envelope hook.DispatchEnvelope, eventKey, runKey, envelopeJSON string) bool {
	return eventItemMatches(b.eventItem, envelope, runKey) && runItemMatches(b.runItem, envelope, eventKey, runKey) &&
		attributeStringEquals(b.runItem, "envelope_json", envelopeJSON)
}

func eventItemMatches(item map[string]types.AttributeValue, envelope hook.DispatchEnvelope, runKey string) bool {
	return len(item) > 0 &&
		attributeStringEquals(item, "record_type", "event") &&
		attributeInt64Equals(item, "activity_id", envelope.Snapshot.ActivityID) &&
		attributeStringEquals(item, "run_key", runKey) &&
		attributeStringEquals(item, "delivery_id", envelope.DeliveryID) &&
		attributeStringEquals(item, "input_sha256", envelope.Snapshot.InputSHA256)
}

func runItemMatches(item map[string]types.AttributeValue, envelope hook.DispatchEnvelope, eventKey, runKey string) bool {
	return len(item) > 0 &&
		attributeStringEquals(item, "pk", runKey) &&
		attributeStringEquals(item, "record_type", "run") &&
		attributeStringEquals(item, "event_key", eventKey) &&
		attributeInt64Equals(item, "activity_id", envelope.Snapshot.ActivityID) &&
		attributeStringEquals(item, "run_id", envelope.Snapshot.RunID) &&
		attributeStringEquals(item, "delivery_id", envelope.DeliveryID) &&
		attributeStringEquals(item, "input_sha256", envelope.Snapshot.InputSHA256)
}

func snapshotAllowed(snapshot hook.TicketSnapshot, request hook.PullClaimRequest) bool {
	return snapshot.SpaceKey == request.SpaceKey && snapshot.ProjectID == request.ProjectID &&
		snapshot.ProjectKey == request.ProjectKey && snapshot.CreatorID == request.AllowedCreatorID &&
		snapshot.ActivityType == request.AllowedActivityType && snapshot.Target == request.Target &&
		(request.RunID == "" || snapshot.RunID == request.RunID)
}

func decodeEnvelope(encoded []byte) (hook.DispatchEnvelope, error) {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var envelope hook.DispatchEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return hook.DispatchEnvelope{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return hook.DispatchEnvelope{}, errors.New("multiple envelope values")
	}
	canonical, err := json.Marshal(envelope)
	if err != nil || !bytes.Equal(canonical, encoded) || hook.ValidateEnvelope(envelope) != nil {
		return hook.DispatchEnvelope{}, errors.New("envelope is invalid")
	}
	return envelope, nil
}

func validPullRequest(request hook.PullClaimRequest) bool {
	return request.SpaceKey != "" && request.ProjectID > 0 && request.ProjectKey != "" &&
		request.AllowedCreatorID > 0 && request.AllowedActivityType > 0 &&
		request.Target.Validate() == nil && request.Owner.RepositoryID == request.Target.RepositoryID &&
		digestPattern.MatchString(request.Owner.RepositorySHA256) &&
		request.Owner.WorkflowRefSHA256 == request.Target.WorkflowRefSHA256 &&
		commitPattern.MatchString(request.Owner.WorkflowSHA) && request.Owner.WorkflowRunID > 0 && request.Owner.RunAttempt > 0 &&
		!request.IssuedAt.IsZero() && !request.ClaimedAt.IsZero() && request.ClockSkew > 0 && request.ClockSkew <= hook.MaxPullClockSkew &&
		!request.IssuedAt.After(request.ClaimedAt.Add(request.ClockSkew))
}

func makeKey(kind string, values ...string) string {
	digest := sha256.New()
	for _, value := range values {
		_, _ = digest.Write([]byte(value))
		_, _ = digest.Write([]byte{0})
	}
	return kind + "#" + hex.EncodeToString(digest.Sum(nil))
}

func stringValue(value string) types.AttributeValue {
	return &types.AttributeValueMemberS{Value: value}
}

func numberValue(value int64) types.AttributeValue {
	return &types.AttributeValueMemberN{Value: strconv.FormatInt(value, 10)}
}

func attributeString(item map[string]types.AttributeValue, name string) (string, bool) {
	value, ok := item[name].(*types.AttributeValueMemberS)
	if !ok {
		return "", false
	}
	return value.Value, true
}

func attributeInt64(item map[string]types.AttributeValue, name string) (int64, bool) {
	value, ok := item[name].(*types.AttributeValueMemberN)
	if !ok {
		return 0, false
	}
	parsed, err := strconv.ParseInt(value.Value, 10, 64)
	return parsed, err == nil
}

func attributeStringEquals(item map[string]types.AttributeValue, name, expected string) bool {
	value, ok := attributeString(item, name)
	return ok && value == expected
}

func attributeInt64Equals(item map[string]types.AttributeValue, name string, expected int64) bool {
	value, ok := attributeInt64(item, name)
	return ok && value == expected
}
