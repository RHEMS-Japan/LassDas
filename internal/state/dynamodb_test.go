package state

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	testProjectID    int64 = 909057
	testRepositoryID int64 = 901234567
)

var (
	testCreatedAt         = time.Date(2026, 8, 2, 1, 2, 3, 0, time.UTC)
	testQueuedAt          = time.Date(2026, 8, 2, 3, 4, 5, 0, time.UTC)
	testWorkflowRefDigest = strings.Repeat("b", 64)
)

type memoryDynamo struct {
	mu              sync.Mutex
	items           map[string]map[string]types.AttributeValue
	transactionErr  error
	afterApplyErr   error
	getErr          error
	lastTransaction *dynamodb.TransactWriteItemsInput
}

func newMemoryDynamo() *memoryDynamo {
	return &memoryDynamo{items: make(map[string]map[string]types.AttributeValue)}
}

func (m *memoryDynamo) TransactWriteItems(_ context.Context, input *dynamodb.TransactWriteItemsInput, _ ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastTransaction = input
	if m.transactionErr != nil {
		return nil, m.transactionErr
	}

	staged := cloneItems(m.items)
	for _, operation := range input.TransactItems {
		switch {
		case operation.Put != nil:
			key, ok := attributeString(operation.Put.Item, "pk")
			if !ok || aws.ToString(operation.Put.ConditionExpression) != "attribute_not_exists(#pk)" {
				return nil, errors.New("unsupported put")
			}
			if _, exists := staged[key]; exists {
				return nil, transactionCanceled()
			}
			staged[key] = cloneItem(operation.Put.Item)
		case operation.ConditionCheck != nil:
			key, ok := attributeString(operation.ConditionCheck.Key, "pk")
			if !ok || !conditionCheckMatches(staged[key], operation.ConditionCheck.ExpressionAttributeValues) {
				return nil, transactionCanceled()
			}
		case operation.Delete != nil:
			key, ok := attributeString(operation.Delete.Key, "pk")
			if !ok {
				return nil, errors.New("unsupported delete")
			}
			condition := aws.ToString(operation.Delete.ConditionExpression)
			if condition != "attribute_not_exists(pk) OR run_id = :released_run" {
				return nil, errors.New("unsupported delete condition")
			}
			if item, exists := staged[key]; exists {
				runID, _ := attributeString(item, "run_id")
				released, _ := attributeString(operation.Delete.ExpressionAttributeValues, ":released_run")
				if runID != released {
					return nil, transactionCanceled()
				}
				delete(staged, key)
			}
		case operation.Update != nil:
			key, ok := attributeString(operation.Update.Key, "pk")
			if !ok || !runUpdateConditionMatches(staged[key], operation.Update.ExpressionAttributeValues) {
				return nil, transactionCanceled()
			}
			if staged[key] == nil {
				// DynamoDB UpdateItem upserts; only the ingest-cursor branch
				// accepts a missing item, every other condition requires one.
				staged[key] = map[string]types.AttributeValue{"pk": stringValue(key)}
			}
			applyRunUpdate(staged[key], operation.Update.ExpressionAttributeValues)
		default:
			return nil, errors.New("unsupported transaction operation")
		}
	}
	m.items = staged
	if m.afterApplyErr != nil {
		err := m.afterApplyErr
		m.afterApplyErr = nil
		return nil, err
	}
	return &dynamodb.TransactWriteItemsOutput{}, nil
}

func (m *memoryDynamo) GetItem(_ context.Context, input *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getErr != nil {
		return nil, m.getErr
	}
	if !aws.ToBool(input.ConsistentRead) {
		return nil, errors.New("eventual read is not supported by the test double")
	}
	key, _ := attributeString(input.Key, "pk")
	return &dynamodb.GetItemOutput{Item: cloneItem(m.items[key])}, nil
}

func conditionCheckMatches(item, values map[string]types.AttributeValue) bool {
	if _, ok := values[":awaiting_gate"]; ok {
		_, questionCommentExists := item["question_comment_id"]
		return len(item) > 0 && questionCommentExists &&
			attributeEquals(item, "state", values, ":awaiting_gate") &&
			attributeEquals(item, "question_record_sha256", values, ":question_record_digest")
	}
	return eventConditionMatches(item, values)
}

func eventConditionMatches(item, values map[string]types.AttributeValue) bool {
	return len(item) > 0 &&
		attributeEquals(item, "record_type", values, ":event_type") &&
		attributeEquals(item, "activity_id", values, ":activity_id") &&
		attributeEquals(item, "run_key", values, ":run_key") &&
		attributeEquals(item, "delivery_id", values, ":delivery") &&
		attributeEquals(item, "input_sha256", values, ":digest")
}

func runUpdateConditionMatches(item, values map[string]types.AttributeValue) bool {
	if _, ok := values[":queued"]; ok {
		return len(item) > 0 &&
			attributeEquals(item, "state", values, ":queued") &&
			attributeEquals(item, "record_type", values, ":run_type") &&
			attributeEquals(item, "activity_id", values, ":activity_id") &&
			attributeEquals(item, "run_id", values, ":run_id") &&
			attributeEquals(item, "event_key", values, ":event_key") &&
			attributeEquals(item, "delivery_id", values, ":delivery") &&
			attributeEquals(item, "input_sha256", values, ":digest") &&
			attributeEquals(item, "envelope_json", values, ":envelope") &&
			attributeEquals(item, "queued_at", values, ":queued_at")
	}
	if _, ok := values[":question_record_json"]; ok {
		for _, name := range []string{
			"question_record_sha256", "question_record_json", "question_started_at",
			"question_lease_until", "question_lease_token", "question_comment_id", "question_posted_at",
			"terminal_report_sha256", "terminal_code", "terminal_started_at", "terminal_lease_until",
			"terminal_lease_token", "terminal_comment_id", "terminal_completed_at",
		} {
			if _, exists := item[name]; exists {
				return false
			}
		}
		if _, pinned := values[":question_clarification"]; pinned {
			if !attributeEquals(item, "clarification_sha256", values, ":question_clarification") ||
				!attributeEquals(item, "input_revision", values, ":question_input_revision") {
				return false
			}
		} else {
			for _, name := range []string{"clarification_sha256", "clarification_json", "input_revision"} {
				if _, exists := item[name]; exists {
					return false
				}
			}
		}
		return terminalCommonConditionMatches(item, values) && attributeEquals(item, "state", values, ":claimed")
	}
	if _, ok := values[":resume_awaiting"]; ok {
		for _, name := range []string{
			"terminal_report_sha256", "terminal_code", "terminal_started_at", "terminal_lease_until",
			"terminal_lease_token", "terminal_comment_id", "terminal_completed_at",
			"question_lease_until", "question_lease_token",
		} {
			if _, exists := item[name]; exists {
				return false
			}
		}
		if _, pinned := values[":previous_clarification"]; pinned {
			if !attributeEquals(item, "clarification_sha256", values, ":previous_clarification") ||
				!attributeEquals(item, "input_revision", values, ":previous_input_revision") {
				return false
			}
		} else {
			for _, name := range []string{"clarification_sha256", "clarification_json", "input_revision"} {
				if _, exists := item[name]; exists {
					return false
				}
			}
		}
		return terminalCommonConditionMatches(item, values) &&
			attributeEquals(item, "state", values, ":resume_awaiting") &&
			attributeEquals(item, "question_record_sha256", values, ":question_record_digest") &&
			attributeEquals(item, "question_comment_id", values, ":question_comment_id")
	}
	if _, ok := values[":question_comment_id"]; ok {
		_, commentExists := item["question_comment_id"]
		_, postedExists := item["question_posted_at"]
		return !commentExists && !postedExists && terminalCommonConditionMatches(item, values) &&
			attributeEquals(item, "state", values, ":question_pending") &&
			attributeEquals(item, "question_record_sha256", values, ":question_record_digest") &&
			attributeEquals(item, "question_started_at", values, ":question_started_at") &&
			attributeEquals(item, "question_lease_token", values, ":question_lease_token")
	}
	if _, ok := values[":previous_question_lease"]; ok {
		_, commentExists := item["question_comment_id"]
		_, postedExists := item["question_posted_at"]
		return !commentExists && !postedExists && terminalCommonConditionMatches(item, values) &&
			attributeEquals(item, "state", values, ":question_pending") &&
			attributeEquals(item, "question_record_sha256", values, ":question_record_digest") &&
			attributeEquals(item, "question_lease_until", values, ":previous_question_lease")
	}
	if _, ok := values[":awaiting_source"]; ok {
		for _, name := range []string{
			"terminal_report_sha256", "terminal_code", "terminal_started_at",
			"terminal_lease_until", "terminal_lease_token", "terminal_comment_id", "terminal_completed_at",
		} {
			if _, exists := item[name]; exists {
				return false
			}
		}
		_, questionCommentExists := item["question_comment_id"]
		return questionCommentExists && terminalCommonConditionMatches(item, values) &&
			attributeEquals(item, "state", values, ":awaiting_source") &&
			attributeEquals(item, "question_record_sha256", values, ":question_record_digest")
	}
	if _, ok := values[":previous_notify_lease"]; ok {
		_, commentExists := item["notify_comment_id"]
		_, postedExists := item["notify_posted_at"]
		return !commentExists && !postedExists &&
			attributeEquals(item, "record_type", values, ":notify_type") &&
			attributeEquals(item, "run_key", values, ":marker_run_key") &&
			attributeEquals(item, "question_record_sha256", values, ":question_record_digest") &&
			attributeEquals(item, "notify_index", values, ":notify_index") &&
			attributeEquals(item, "notify_lease_until", values, ":previous_notify_lease")
	}
	if _, ok := values[":notify_comment_id"]; ok {
		_, commentExists := item["notify_comment_id"]
		_, postedExists := item["notify_posted_at"]
		return !commentExists && !postedExists &&
			attributeEquals(item, "record_type", values, ":notify_type") &&
			attributeEquals(item, "run_key", values, ":marker_run_key") &&
			attributeEquals(item, "question_record_sha256", values, ":question_record_digest") &&
			attributeEquals(item, "notify_index", values, ":notify_index") &&
			attributeEquals(item, "notify_started_at", values, ":notify_started_at") &&
			attributeEquals(item, "notify_lease_token", values, ":notify_lease_token")
	}
	if _, ok := values[":ingest_activity"]; ok {
		if len(item) == 0 {
			return true
		}
		last, exists := attributeInt64(item, "last_activity_id")
		next, _ := attributeInt64(values, ":ingest_activity")
		return !exists || last < next
	}
	if _, ok := values[":run_comment_type"]; ok {
		_, commentExists := item["reply_comment_id"]
		_, postedExists := item["reply_posted_at"]
		base := !commentExists && !postedExists &&
			attributeEquals(item, "record_type", values, ":run_comment_type") &&
			attributeEquals(item, "run_key", values, ":marker_run_key") &&
			attributeEquals(item, "reply_kind", values, ":reply_kind")
		if _, pinned := values[":previous_reply_lease"]; pinned {
			return base && attributeEquals(item, "reply_lease_until", values, ":previous_reply_lease")
		}
		return base &&
			attributeEquals(item, "content_sha256", values, ":content_digest") &&
			attributeEquals(item, "reply_started_at", values, ":reply_started_at") &&
			attributeEquals(item, "reply_lease_token", values, ":reply_lease_token")
	}
	if _, ok := values[":previous_reply_lease"]; ok {
		_, commentExists := item["reply_comment_id"]
		_, postedExists := item["reply_posted_at"]
		return !commentExists && !postedExists &&
			attributeEquals(item, "record_type", values, ":reply_type") &&
			attributeEquals(item, "run_key", values, ":marker_run_key") &&
			attributeEquals(item, "question_record_sha256", values, ":question_record_digest") &&
			attributeEquals(item, "reply_kind", values, ":reply_kind") &&
			attributeEquals(item, "reply_lease_until", values, ":previous_reply_lease")
	}
	if _, ok := values[":reply_comment_id"]; ok {
		_, commentExists := item["reply_comment_id"]
		_, postedExists := item["reply_posted_at"]
		return !commentExists && !postedExists &&
			attributeEquals(item, "record_type", values, ":reply_type") &&
			attributeEquals(item, "run_key", values, ":marker_run_key") &&
			attributeEquals(item, "question_record_sha256", values, ":question_record_digest") &&
			attributeEquals(item, "reply_kind", values, ":reply_kind") &&
			attributeEquals(item, "content_sha256", values, ":content_digest") &&
			attributeEquals(item, "reply_started_at", values, ":reply_started_at") &&
			attributeEquals(item, "reply_lease_token", values, ":reply_lease_token")
	}
	if _, ok := values[":terminal"]; ok {
		_, commentExists := item["terminal_comment_id"]
		_, completedExists := item["terminal_completed_at"]
		return !commentExists && !completedExists && terminalCommonConditionMatches(item, values) &&
			attributeEquals(item, "state", values, ":pending") &&
			attributeEquals(item, "terminal_report_sha256", values, ":report_digest") &&
			attributeEquals(item, "terminal_code", values, ":terminal_code") &&
			attributeEquals(item, "terminal_started_at", values, ":started_at") &&
			attributeEquals(item, "terminal_lease_token", values, ":lease_token")
	}
	if _, ok := values[":previous_lease"]; ok {
		_, commentExists := item["terminal_comment_id"]
		_, completedExists := item["terminal_completed_at"]
		return !commentExists && !completedExists && terminalCommonConditionMatches(item, values) &&
			attributeEquals(item, "state", values, ":pending") &&
			attributeEquals(item, "terminal_report_sha256", values, ":report_digest") &&
			attributeEquals(item, "terminal_code", values, ":terminal_code") &&
			attributeEquals(item, "terminal_lease_until", values, ":previous_lease")
	}
	if _, ok := values[":report_digest"]; ok {
		for _, name := range []string{
			"terminal_report_sha256", "terminal_code", "terminal_started_at", "terminal_lease_until",
			"terminal_lease_token", "terminal_comment_id", "terminal_completed_at",
			"question_record_sha256", "question_comment_id",
		} {
			if _, exists := item[name]; exists {
				return false
			}
		}
		return terminalCommonConditionMatches(item, values) && attributeEquals(item, "state", values, ":claimed")
	}
	return false
}

func terminalCommonConditionMatches(item, values map[string]types.AttributeValue) bool {
	return len(item) > 0 && attributeEquals(item, "record_type", values, ":run_type") &&
		attributeEquals(item, "activity_id", values, ":activity_id") &&
		attributeEquals(item, "run_id", values, ":run_id") &&
		attributeEquals(item, "event_key", values, ":event_key") &&
		attributeEquals(item, "delivery_id", values, ":delivery") &&
		attributeEquals(item, "input_sha256", values, ":input_digest") &&
		attributeEquals(item, "envelope_json", values, ":envelope") &&
		attributeEquals(item, "repository_id", values, ":repository_id") &&
		attributeEquals(item, "repository_sha256", values, ":repository_digest") &&
		attributeEquals(item, "workflow_ref_sha256", values, ":workflow_ref_digest") &&
		attributeEquals(item, "workflow_sha", values, ":workflow_sha") &&
		attributeEquals(item, "workflow_run_id", values, ":workflow_run_id") &&
		attributeEquals(item, "run_attempt", values, ":run_attempt")
}

func attributeEquals(item map[string]types.AttributeValue, itemName string, values map[string]types.AttributeValue, valueName string) bool {
	itemString, itemIsString := item[itemName].(*types.AttributeValueMemberS)
	valueString, valueIsString := values[valueName].(*types.AttributeValueMemberS)
	if itemIsString || valueIsString {
		return itemIsString && valueIsString && itemString.Value == valueString.Value
	}
	itemNumber, itemIsNumber := item[itemName].(*types.AttributeValueMemberN)
	valueNumber, valueIsNumber := values[valueName].(*types.AttributeValueMemberN)
	return itemIsNumber && valueIsNumber && itemNumber.Value == valueNumber.Value
}

func applyClaimUpdate(item, values map[string]types.AttributeValue) {
	item["state"] = values[":claimed"]
	item["claimed_at"] = values[":claimed_at"]
	item["repository_id"] = values[":repository_id"]
	item["repository_sha256"] = values[":repository_digest"]
	item["workflow_ref_sha256"] = values[":workflow_ref_digest"]
	item["workflow_sha"] = values[":workflow_sha"]
	item["workflow_run_id"] = values[":workflow_run_id"]
	item["run_attempt"] = values[":run_attempt"]
}

func applyRunUpdate(item, values map[string]types.AttributeValue) {
	if _, ok := values[":queued"]; ok {
		applyClaimUpdate(item, values)
		return
	}
	if _, ok := values[":question_record_json"]; ok {
		item["state"] = values[":question_pending"]
		item["question_record_sha256"] = values[":question_record_digest"]
		item["question_record_json"] = values[":question_record_json"]
		item["question_started_at"] = values[":question_started_at"]
		item["question_lease_until"] = values[":question_lease_until"]
		item["question_lease_token"] = values[":question_lease_token"]
		return
	}
	if _, ok := values[":resume_awaiting"]; ok {
		item["state"] = values[":resumed_state"]
		item["queued_at"] = values[":resumed_queued_at"]
		item["clarification_sha256"] = values[":clarification_digest"]
		item["clarification_json"] = values[":clarification_json"]
		item["input_revision"] = values[":input_revision"]
		item["resumed_at"] = values[":resumed_at"]
		for _, name := range []string{
			"question_record_sha256", "question_record_json", "question_started_at",
			"question_comment_id", "question_posted_at", "claimed_at",
			"repository_id", "repository_sha256", "workflow_ref_sha256",
			"workflow_sha", "workflow_run_id", "run_attempt",
		} {
			delete(item, name)
		}
		return
	}
	if _, ok := values[":question_comment_id"]; ok {
		item["state"] = values[":awaiting"]
		item["question_comment_id"] = values[":question_comment_id"]
		item["question_posted_at"] = values[":question_posted_at"]
		delete(item, "question_lease_token")
		delete(item, "question_lease_until")
		return
	}
	if _, ok := values[":previous_question_lease"]; ok {
		item["question_lease_until"] = values[":question_lease_until"]
		item["question_lease_token"] = values[":question_lease_token"]
		return
	}
	if _, ok := values[":previous_notify_lease"]; ok {
		item["notify_lease_until"] = values[":notify_lease_until"]
		item["notify_lease_token"] = values[":notify_lease_token"]
		return
	}
	if _, ok := values[":ingest_activity"]; ok {
		item["record_type"] = values[":ingest_type"]
		item["last_activity_id"] = values[":ingest_activity"]
		return
	}
	if _, ok := values[":run_comment_type"]; ok {
		if _, pinned := values[":previous_reply_lease"]; pinned {
			item["reply_lease_until"] = values[":reply_lease_until"]
			item["reply_lease_token"] = values[":reply_lease_token"]
			item["content_sha256"] = values[":content_digest"]
			return
		}
		item["reply_comment_id"] = values[":reply_comment_id"]
		item["reply_posted_at"] = values[":reply_posted_at"]
		delete(item, "reply_lease_until")
		delete(item, "reply_lease_token")
		return
	}
	if _, ok := values[":previous_reply_lease"]; ok {
		item["reply_lease_until"] = values[":reply_lease_until"]
		item["reply_lease_token"] = values[":reply_lease_token"]
		item["content_sha256"] = values[":content_digest"]
		item["trigger_comment_id"] = values[":trigger_comment_id"]
		return
	}
	if _, ok := values[":reply_comment_id"]; ok {
		item["reply_comment_id"] = values[":reply_comment_id"]
		item["reply_posted_at"] = values[":reply_posted_at"]
		delete(item, "reply_lease_until")
		delete(item, "reply_lease_token")
		return
	}
	if _, ok := values[":notify_comment_id"]; ok {
		item["notify_comment_id"] = values[":notify_comment_id"]
		item["notify_posted_at"] = values[":notify_posted_at"]
		delete(item, "notify_lease_until")
		delete(item, "notify_lease_token")
		return
	}
	if _, ok := values[":terminal"]; ok {
		item["state"] = values[":terminal"]
		item["terminal_comment_id"] = values[":comment_id"]
		item["terminal_completed_at"] = values[":completed_at"]
		delete(item, "terminal_lease_token")
		delete(item, "terminal_lease_until")
		return
	}
	item["state"] = values[":pending"]
	item["terminal_lease_until"] = values[":lease_until"]
	item["terminal_lease_token"] = values[":lease_token"]
	if _, ok := values[":previous_lease"]; !ok {
		item["terminal_report_sha256"] = values[":report_digest"]
		item["terminal_code"] = values[":terminal_code"]
		item["terminal_started_at"] = values[":started_at"]
	}
}

func transactionCanceled() error {
	return transactionCanceledWithReason("ConditionalCheckFailed")
}

func transactionCanceledWithReason(code string) error {
	return &types.TransactionCanceledException{
		Message: aws.String("transaction canceled"),
		CancellationReasons: []types.CancellationReason{
			{Code: aws.String("None")},
			{Code: aws.String(code)},
		},
	}
}

func cloneItems(input map[string]map[string]types.AttributeValue) map[string]map[string]types.AttributeValue {
	output := make(map[string]map[string]types.AttributeValue, len(input))
	for key, item := range input {
		output[key] = cloneItem(item)
	}
	return output
}

func cloneItem(input map[string]types.AttributeValue) map[string]types.AttributeValue {
	if input == nil {
		return nil
	}
	output := make(map[string]types.AttributeValue, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func testEnvelope(t *testing.T) hook.DispatchEnvelope {
	t.Helper()
	envelope, err := hook.SealSnapshot(hook.TicketSnapshot{
		SchemaVersion: hook.SnapshotSchemaVersion,
		SpaceKey:      "example",
		ActivityID:    9001,
		ActivityType:  1,
		ProjectID:     testProjectID,
		ProjectKey:    "TICKET",
		IssueID:       8001,
		IssueKey:      "TICKET-501",
		IssueKeyID:    501,
		CreatorID:     9903853,
		RunID:         "run_20260802_alpha",
		CreatedAt:     testCreatedAt,
		Target: hook.DeliveryTarget{
			RepositoryID:      testRepositoryID,
			WorkflowRefSHA256: testWorkflowRefDigest,
		},
		Untrusted: hook.UntrustedTicketData{
			Summary:     "untrusted summary",
			Description: "$(never-execute-this)",
		},
	})
	if err != nil {
		t.Fatalf("SealSnapshot() error = %v", err)
	}
	return envelope
}

func testQueueRequest(t *testing.T) hook.QueueRequest {
	t.Helper()
	return hook.QueueRequest{Envelope: testEnvelope(t), QueuedAt: testQueuedAt}
}

func testPullRequest(t *testing.T) hook.PullClaimRequest {
	t.Helper()
	envelope := testEnvelope(t)
	return hook.PullClaimRequest{
		SpaceKey:            envelope.Snapshot.SpaceKey,
		ProjectID:           envelope.Snapshot.ProjectID,
		ProjectKey:          envelope.Snapshot.ProjectKey,
		AllowedCreatorID:    envelope.Snapshot.CreatorID,
		AllowedActivityType: envelope.Snapshot.ActivityType,
		RunID:               envelope.Snapshot.RunID,
		Target:              envelope.Snapshot.Target,
		Owner: hook.PullOwner{
			RepositoryID:      testRepositoryID,
			RepositorySHA256:  hook.HashIdentity("example/automation-receiver"),
			WorkflowRefSHA256: testWorkflowRefDigest,
			WorkflowSHA:       strings.Repeat("d", 40),
			// Above 2^53 on purpose: the chain claim identity is a 63-bit
			// hash, and a float64 detour in either store corrupts it (the
			// RFDEV-618 run had every terminal report refused over 314 lost
			// units). Keeping the fixture pathological makes every scenario
			// a precision regression test.
			WorkflowRunID:     7663335643410923834,
			RunAttempt:        1,
		},
		IssuedAt:  testQueuedAt.Add(time.Second),
		ClaimedAt: testQueuedAt.Add(2 * time.Second),
		ClockSkew: 2 * time.Minute,
	}
}

func testStore(t *testing.T, api *memoryDynamo) *DynamoStore {
	t.Helper()
	store, err := NewDynamoStore("ticket-automation-state", api)
	if err != nil {
		t.Fatalf("NewDynamoStore() error = %v", err)
	}
	return store
}

func enqueueForTest(t *testing.T, store *DynamoStore) hook.DispatchEnvelope {
	t.Helper()
	request := testQueueRequest(t)
	disposition, err := store.Enqueue(context.Background(), request)
	if err != nil || disposition != hook.QueueCreated {
		t.Fatalf("Enqueue() disposition = %s, err = %v", disposition, err)
	}
	return request.Envelope
}

func itemKeys(envelope hook.DispatchEnvelope) (string, string) {
	snapshot := envelope.Snapshot
	projectID := strconv.FormatInt(snapshot.ProjectID, 10)
	eventKey := makeKey("event", snapshot.SpaceKey, projectID, strconv.FormatInt(snapshot.ActivityID, 10))
	runKey := makeKey("run", snapshot.SpaceKey, projectID, snapshot.RunID)
	return eventKey, runKey
}

func TestEnqueueCreatesAtomicBindingAndPersistsEnvelope(t *testing.T) {
	api := newMemoryDynamo()
	store := testStore(t, api)
	request := testQueueRequest(t)

	disposition, err := store.Enqueue(context.Background(), request)
	if err != nil || disposition != hook.QueueCreated {
		t.Fatalf("disposition = %s, err = %v", disposition, err)
	}
	if api.lastTransaction == nil || len(api.lastTransaction.TransactItems) != 3 || len(api.items) != 3 {
		t.Fatalf("transaction = %+v, item count = %d", api.lastTransaction, len(api.items))
	}
	for _, operation := range api.lastTransaction.TransactItems {
		if operation.Put == nil || aws.ToString(operation.Put.ConditionExpression) != "attribute_not_exists(#pk)" {
			t.Fatalf("put = %+v", operation.Put)
		}
	}

	eventKey, runKey := itemKeys(request.Envelope)
	eventItem, runItem := api.items[eventKey], api.items[runKey]
	if !eventItemMatches(eventItem, request.Envelope, runKey) || !runItemMatches(runItem, request.Envelope, eventKey, runKey) {
		t.Fatal("stored event/run binding does not match the envelope")
	}
	encoded, err := json.Marshal(request.Envelope)
	if err != nil {
		t.Fatal(err)
	}
	if !attributeStringEquals(runItem, "envelope_json", string(encoded)) ||
		!attributeStringEquals(runItem, "state", stateQueued) ||
		!attributeInt64Equals(runItem, "queued_at", testQueuedAt.UnixMilli()) {
		t.Fatalf("run item = %+v", runItem)
	}
}

func TestEnqueueDuplicateAndClaimedAreIdempotent(t *testing.T) {
	api := newMemoryDynamo()
	store := testStore(t, api)
	request := testQueueRequest(t)
	if disposition, err := store.Enqueue(context.Background(), request); err != nil || disposition != hook.QueueCreated {
		t.Fatalf("initial disposition = %s, err = %v", disposition, err)
	}
	if disposition, err := store.Enqueue(context.Background(), request); err != nil || disposition != hook.QueueDuplicate {
		t.Fatalf("duplicate disposition = %s, err = %v", disposition, err)
	}
	if _, disposition, err := store.Pull(context.Background(), testPullRequest(t)); err != nil || disposition != hook.PullAcquired {
		t.Fatalf("pull disposition = %s, err = %v", disposition, err)
	}
	if disposition, err := store.Enqueue(context.Background(), request); err != nil || disposition != hook.QueueClaimed {
		t.Fatalf("claimed disposition = %s, err = %v", disposition, err)
	}
	if len(api.items) != 3 {
		t.Fatalf("item count = %d", len(api.items))
	}
}

func TestEnqueueConflictsNeverLeaveAPartialBinding(t *testing.T) {
	tests := map[string]func(*hook.DispatchEnvelope, *testing.T){
		"same event different run": func(envelope *hook.DispatchEnvelope, t *testing.T) {
			envelope.Snapshot.RunID = "run_20260802_other"
			*envelope = reseal(t, envelope.Snapshot)
		},
		"different event same run": func(envelope *hook.DispatchEnvelope, t *testing.T) {
			envelope.Snapshot.ActivityID++
			*envelope = reseal(t, envelope.Snapshot)
		},
		"same keys different envelope": func(envelope *hook.DispatchEnvelope, t *testing.T) {
			envelope.Snapshot.Untrusted.Description = "different immutable activity"
			*envelope = reseal(t, envelope.Snapshot)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			api := newMemoryDynamo()
			store := testStore(t, api)
			original := enqueueForTest(t, store)
			candidate := original
			mutate(&candidate, t)

			disposition, err := store.Enqueue(context.Background(), hook.QueueRequest{Envelope: candidate, QueuedAt: testQueuedAt})
			if err != nil || disposition != hook.QueueConflict {
				t.Fatalf("disposition = %s, err = %v", disposition, err)
			}
			if len(api.items) != 3 {
				t.Fatalf("conditional transaction left %d items", len(api.items))
			}
			eventKey, runKey := itemKeys(original)
			if !eventItemMatches(api.items[eventKey], original, runKey) || !runItemMatches(api.items[runKey], original, eventKey, runKey) {
				t.Fatal("conflict changed the original binding")
			}
		})
	}
}

func reseal(t *testing.T, snapshot hook.TicketSnapshot) hook.DispatchEnvelope {
	t.Helper()
	envelope, err := hook.SealSnapshot(snapshot)
	if err != nil {
		t.Fatalf("SealSnapshot() error = %v", err)
	}
	return envelope
}

func TestPullEmptyReturnsNoEnvelope(t *testing.T) {
	envelope, disposition, err := testStore(t, newMemoryDynamo()).Pull(context.Background(), testPullRequest(t))
	if err != nil || disposition != hook.PullEmpty || envelope != (hook.DispatchEnvelope{}) {
		t.Fatalf("envelope = %+v, disposition = %s, err = %v", envelope, disposition, err)
	}
}

func TestPullAtomicallyClaimsAndPersistsOwnerBinding(t *testing.T) {
	api := newMemoryDynamo()
	store := testStore(t, api)
	want := enqueueForTest(t, store)
	request := testPullRequest(t)

	got, disposition, err := store.Pull(context.Background(), request)
	if err != nil || disposition != hook.PullAcquired || got != want {
		t.Fatalf("envelope = %+v, disposition = %s, err = %v", got, disposition, err)
	}
	if api.lastTransaction == nil || len(api.lastTransaction.TransactItems) != 2 ||
		api.lastTransaction.TransactItems[0].ConditionCheck == nil || api.lastTransaction.TransactItems[1].Update == nil {
		t.Fatalf("claim transaction = %+v", api.lastTransaction)
	}
	eventCheck := api.lastTransaction.TransactItems[0].ConditionCheck
	runUpdate := api.lastTransaction.TransactItems[1].Update
	if aws.ToString(eventCheck.ConditionExpression) != "#record_type = :event_type AND #activity_id = :activity_id AND #run_key = :run_key AND #delivery = :delivery AND #digest = :digest" {
		t.Fatalf("event condition = %q", aws.ToString(eventCheck.ConditionExpression))
	}
	if aws.ToString(runUpdate.ConditionExpression) != "#state = :queued AND #record_type = :run_type AND #activity_id = :activity_id AND #run_id = :run_id AND #event_key = :event_key AND #delivery = :delivery AND #digest = :digest AND #envelope = :envelope AND #queued_at = :queued_at" {
		t.Fatalf("run condition = %q", aws.ToString(runUpdate.ConditionExpression))
	}
	if aws.ToString(runUpdate.UpdateExpression) != "SET #state = :claimed, #claimed_at = :claimed_at, #repository_id = :repository_id, #repository_digest = :repository_digest, #workflow_ref_digest = :workflow_ref_digest, #workflow_sha = :workflow_sha, #workflow_run_id = :workflow_run_id, #run_attempt = :run_attempt" {
		t.Fatalf("run update = %q", aws.ToString(runUpdate.UpdateExpression))
	}
	_, runKey := itemKeys(want)
	runItem := api.items[runKey]
	checks := map[string]int64{
		"claimed_at":      request.ClaimedAt.UnixMilli(),
		"repository_id":   request.Owner.RepositoryID,
		"workflow_run_id": request.Owner.WorkflowRunID,
		"run_attempt":     int64(request.Owner.RunAttempt),
	}
	for name, value := range checks {
		if !attributeInt64Equals(runItem, name, value) {
			t.Fatalf("%s = %+v", name, runItem[name])
		}
	}
	stringChecks := map[string]string{
		"state":               stateClaimed,
		"repository_sha256":   request.Owner.RepositorySHA256,
		"workflow_ref_sha256": request.Owner.WorkflowRefSHA256,
		"workflow_sha":        request.Owner.WorkflowSHA,
	}
	for name, value := range stringChecks {
		if !attributeStringEquals(runItem, name, value) {
			t.Fatalf("%s = %+v", name, runItem[name])
		}
	}
}

func TestConcurrentPullHasOneEnvelopeWinner(t *testing.T) {
	api := newMemoryDynamo()
	store := testStore(t, api)
	want := enqueueForTest(t, store)
	const workers = 100
	start := make(chan struct{})
	var acquired atomic.Int64
	var alreadyClaimed atomic.Int64
	var failed atomic.Int64
	var wait sync.WaitGroup
	for index := 0; index < workers; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			request := testPullRequest(t)
			request.Owner.WorkflowRunID += int64(index)
			envelope, disposition, err := store.Pull(context.Background(), request)
			if err != nil {
				failed.Add(1)
				return
			}
			switch disposition {
			case hook.PullAcquired:
				if envelope != want {
					failed.Add(1)
					return
				}
				acquired.Add(1)
			case hook.PullClaimed:
				if envelope != (hook.DispatchEnvelope{}) {
					failed.Add(1)
					return
				}
				alreadyClaimed.Add(1)
			default:
				failed.Add(1)
			}
		}(index)
	}
	close(start)
	wait.Wait()
	if acquired.Load() != 1 || alreadyClaimed.Load() != workers-1 || failed.Load() != 0 {
		t.Fatalf("acquired = %d, claimed = %d, failed = %d", acquired.Load(), alreadyClaimed.Load(), failed.Load())
	}
}

func TestPullRedeliversOnlyExactOwnerWithoutMutatingClaim(t *testing.T) {
	api := newMemoryDynamo()
	store := testStore(t, api)
	want := enqueueForTest(t, store)
	request := testPullRequest(t)
	if _, disposition, err := store.Pull(context.Background(), request); err != nil || disposition != hook.PullAcquired {
		t.Fatalf("initial claim disposition=%s err=%v", disposition, err)
	}
	claimTransaction := api.lastTransaction
	retry := request
	retry.IssuedAt = request.IssuedAt.Add(time.Minute)
	retry.ClaimedAt = request.ClaimedAt.Add(time.Minute)
	got, disposition, err := store.Pull(context.Background(), retry)
	if err != nil || disposition != hook.PullAcquired || got != want {
		t.Fatalf("exact retry envelope=%+v disposition=%s err=%v", got, disposition, err)
	}
	_, runKey := itemKeys(want)
	if api.lastTransaction != claimTransaction ||
		!attributeInt64Equals(api.items[runKey], "claimed_at", request.ClaimedAt.UnixMilli()) ||
		!attributeInt64Equals(api.items[runKey], "workflow_run_id", request.Owner.WorkflowRunID) ||
		!attributeInt64Equals(api.items[runKey], "run_attempt", int64(request.Owner.RunAttempt)) {
		t.Fatalf("exact retry mutated claim: %+v", api.items[runKey])
	}
}

func TestPullNeverTransfersClaimToDifferentOwner(t *testing.T) {
	tests := map[string]func(*hook.PullClaimRequest){
		"later workflow run": func(request *hook.PullClaimRequest) { request.Owner.WorkflowRunID++ },
		"later run attempt":  func(request *hook.PullClaimRequest) { request.Owner.RunAttempt++ },
		"workflow revision":  func(request *hook.PullClaimRequest) { request.Owner.WorkflowSHA = strings.Repeat("e", 40) },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			api := newMemoryDynamo()
			store := testStore(t, api)
			envelope := enqueueForTest(t, store)
			first := testPullRequest(t)
			if _, disposition, err := store.Pull(context.Background(), first); err != nil || disposition != hook.PullAcquired {
				t.Fatalf("initial claim disposition=%s err=%v", disposition, err)
			}
			claimTransaction := api.lastTransaction
			later := first
			mutate(&later)
			later.IssuedAt = first.IssuedAt.Add(24 * time.Hour)
			later.ClaimedAt = first.ClaimedAt.Add(24 * time.Hour)
			got, disposition, err := store.Pull(context.Background(), later)
			if err != nil || disposition != hook.PullClaimed || got != (hook.DispatchEnvelope{}) {
				t.Fatalf("different owner envelope=%+v disposition=%s err=%v", got, disposition, err)
			}
			_, runKey := itemKeys(envelope)
			if api.lastTransaction != claimTransaction ||
				!attributeInt64Equals(api.items[runKey], "claimed_at", first.ClaimedAt.UnixMilli()) ||
				!attributeStringEquals(api.items[runKey], "workflow_sha", first.Owner.WorkflowSHA) ||
				!attributeInt64Equals(api.items[runKey], "workflow_run_id", first.Owner.WorkflowRunID) ||
				!attributeInt64Equals(api.items[runKey], "run_attempt", int64(first.Owner.RunAttempt)) {
				t.Fatalf("different owner mutated claim=%+v", api.items[runKey])
			}
		})
	}
}

func TestPullReconcilesAppliedInitialClaimAfterResponseLoss(t *testing.T) {
	api := newMemoryDynamo()
	store := testStore(t, api)
	want := enqueueForTest(t, store)
	request := testPullRequest(t)
	api.afterApplyErr = errors.New("response lost after commit")
	got, disposition, err := store.Pull(context.Background(), request)
	if err != nil || disposition != hook.PullAcquired || got != want {
		t.Fatalf("reconciled envelope=%+v disposition=%s err=%v", got, disposition, err)
	}
	_, runKey := itemKeys(want)
	if !attributeInt64Equals(api.items[runKey], "claimed_at", request.ClaimedAt.UnixMilli()) ||
		!attributeInt64Equals(api.items[runKey], "workflow_run_id", request.Owner.WorkflowRunID) ||
		!attributeInt64Equals(api.items[runKey], "run_attempt", int64(request.Owner.RunAttempt)) {
		t.Fatalf("reconciled claim=%+v", api.items[runKey])
	}
}

func TestPullEnforcesFixedSelectorsAndSnapshotAllowlist(t *testing.T) {
	selectorTests := map[string]func(*hook.PullClaimRequest){
		"space":      func(request *hook.PullClaimRequest) { request.SpaceKey = "other-space" },
		"project id": func(request *hook.PullClaimRequest) { request.ProjectID++ },
		"run id":     func(request *hook.PullClaimRequest) { request.RunID = "run_20260802_other" },
	}
	for name, mutate := range selectorTests {
		t.Run("selector "+name, func(t *testing.T) {
			api := newMemoryDynamo()
			store := testStore(t, api)
			enqueueForTest(t, store)
			request := testPullRequest(t)
			mutate(&request)
			envelope, disposition, err := store.Pull(context.Background(), request)
			if err != nil || disposition != hook.PullEmpty || envelope != (hook.DispatchEnvelope{}) {
				t.Fatalf("envelope = %+v, disposition = %s, err = %v", envelope, disposition, err)
			}
		})
	}

	allowlistTests := map[string]func(*hook.PullClaimRequest){
		"project key":   func(request *hook.PullClaimRequest) { request.ProjectKey = "OTHER" },
		"creator":       func(request *hook.PullClaimRequest) { request.AllowedCreatorID++ },
		"activity type": func(request *hook.PullClaimRequest) { request.AllowedActivityType++ },
		"repository id": func(request *hook.PullClaimRequest) {
			request.Target.RepositoryID++
			request.Owner.RepositoryID = request.Target.RepositoryID
		},
		"workflow ref": func(request *hook.PullClaimRequest) {
			request.Target.WorkflowRefSHA256 = strings.Repeat("e", 64)
			request.Owner.WorkflowRefSHA256 = request.Target.WorkflowRefSHA256
		},
	}
	for name, mutate := range allowlistTests {
		t.Run("allowlist "+name, func(t *testing.T) {
			api := newMemoryDynamo()
			store := testStore(t, api)
			enqueueForTest(t, store)
			request := testPullRequest(t)
			mutate(&request)
			envelope, disposition, err := store.Pull(context.Background(), request)
			assertFailure(t, envelope, disposition, err, hook.FailureRejected, "pull_envelope_invalid", "")
		})
	}

	shapeTests := map[string]func(*hook.PullClaimRequest){
		"owner repository differs from target": func(request *hook.PullClaimRequest) { request.Owner.RepositoryID++ },
		"owner workflow differs from target":   func(request *hook.PullClaimRequest) { request.Owner.WorkflowRefSHA256 = strings.Repeat("e", 64) },
		"repository digest":                    func(request *hook.PullClaimRequest) { request.Owner.RepositorySHA256 = "not-a-digest" },
		"workflow sha":                         func(request *hook.PullClaimRequest) { request.Owner.WorkflowSHA = "not-a-commit" },
		"workflow run":                         func(request *hook.PullClaimRequest) { request.Owner.WorkflowRunID = 0 },
		"run attempt":                          func(request *hook.PullClaimRequest) { request.Owner.RunAttempt = 0 },
		"clock skew missing":                   func(request *hook.PullClaimRequest) { request.ClockSkew = 0 },
		"clock skew too large":                 func(request *hook.PullClaimRequest) { request.ClockSkew = hook.MaxPullClockSkew + time.Nanosecond },
		"claimed too far before issued": func(request *hook.PullClaimRequest) {
			request.ClaimedAt = request.IssuedAt.Add(-request.ClockSkew - time.Nanosecond)
		},
	}
	for name, mutate := range shapeTests {
		t.Run("shape "+name, func(t *testing.T) {
			api := newMemoryDynamo()
			store := testStore(t, api)
			enqueueForTest(t, store)
			request := testPullRequest(t)
			mutate(&request)
			envelope, disposition, err := store.Pull(context.Background(), request)
			assertFailure(t, envelope, disposition, err, hook.FailureRejected, "invalid_pull_request", "")
		})
	}
}

func TestPullRejectsRequestIssuedBeforeQueueAndLeavesItemClaimable(t *testing.T) {
	api := newMemoryDynamo()
	store := testStore(t, api)
	enqueueForTest(t, store)
	request := testPullRequest(t)
	request.IssuedAt = testQueuedAt.Add(-time.Millisecond)
	request.ClaimedAt = testQueuedAt.Add(time.Second)

	envelope, disposition, err := store.Pull(context.Background(), request)
	if err != nil || disposition != hook.PullConflict || envelope != (hook.DispatchEnvelope{}) {
		t.Fatalf("envelope = %+v, disposition = %s, err = %v", envelope, disposition, err)
	}
	want := testEnvelope(t)
	_, runKey := itemKeys(want)
	if !attributeStringEquals(api.items[runKey], "state", stateQueued) {
		t.Fatalf("state = %+v", api.items[runKey]["state"])
	}
	got, disposition, err := store.Pull(context.Background(), testPullRequest(t))
	if err != nil || disposition != hook.PullAcquired || got != want {
		t.Fatalf("later envelope = %+v, disposition = %s, err = %v", got, disposition, err)
	}
}

func TestPullAcceptsIssuedAtAtFutureClockSkewBoundary(t *testing.T) {
	api := newMemoryDynamo()
	store := testStore(t, api)
	want := enqueueForTest(t, store)
	request := testPullRequest(t)
	request.IssuedAt = request.ClaimedAt.Add(request.ClockSkew)

	got, disposition, err := store.Pull(context.Background(), request)
	if err != nil || disposition != hook.PullAcquired || got != want {
		t.Fatalf("envelope = %+v, disposition = %s, err = %v", got, disposition, err)
	}
}

func TestPullCorruptEnvelopeFailsClosed(t *testing.T) {
	api := newMemoryDynamo()
	store := testStore(t, api)
	envelope := enqueueForTest(t, store)
	_, runKey := itemKeys(envelope)
	encoded, _ := attributeString(api.items[runKey], "envelope_json")
	api.items[runKey]["envelope_json"] = stringValue(encoded + " ")

	got, disposition, err := store.Pull(context.Background(), testPullRequest(t))
	assertFailure(t, got, disposition, err, hook.FailureRejected, "pull_envelope_invalid", "")
	if !attributeStringEquals(api.items[runKey], "state", stateQueued) {
		t.Fatalf("state = %+v", api.items[runKey]["state"])
	}
}

func TestPullCorruptBindingFailsClosed(t *testing.T) {
	tests := map[string]struct {
		mutate func(map[string]map[string]types.AttributeValue, string, string)
		code   string
	}{
		"run digest": {
			mutate: func(items map[string]map[string]types.AttributeValue, _, runKey string) {
				items[runKey]["input_sha256"] = stringValue(strings.Repeat("e", 64))
			},
			code: "pull_binding_invalid",
		},
		"run event key": {
			mutate: func(items map[string]map[string]types.AttributeValue, _, runKey string) {
				items[runKey]["event_key"] = stringValue("event#wrong")
			},
			code: "pull_binding_invalid",
		},
		"event delivery": {
			mutate: func(items map[string]map[string]types.AttributeValue, eventKey, _ string) {
				items[eventKey]["delivery_id"] = stringValue("delivery_00000000000000000000000000000000")
			},
			code: "pull_binding_invalid",
		},
		"missing event": {
			mutate: func(items map[string]map[string]types.AttributeValue, eventKey, _ string) {
				delete(items, eventKey)
			},
			code: "pull_binding_invalid",
		},
		"unknown state": {
			mutate: func(items map[string]map[string]types.AttributeValue, _, runKey string) {
				items[runKey]["state"] = stringValue("unknown")
			},
			code: "pull_state_invalid",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			api := newMemoryDynamo()
			store := testStore(t, api)
			envelope := enqueueForTest(t, store)
			eventKey, runKey := itemKeys(envelope)
			test.mutate(api.items, eventKey, runKey)

			got, disposition, err := store.Pull(context.Background(), testPullRequest(t))
			assertFailure(t, got, disposition, err, hook.FailureRejected, test.code, "")
		})
	}
}

func TestDependencyErrorsExposeOnlySafeCodes(t *testing.T) {
	const secret = "DYNAMO-SECRET-SENTINEL"
	tests := map[string]struct {
		prepare func(*testing.T, *memoryDynamo, *DynamoStore)
		run     func(*testing.T, *DynamoStore) error
		class   hook.FailureClass
		code    string
	}{
		"enqueue write": {
			prepare: func(_ *testing.T, api *memoryDynamo, _ *DynamoStore) { api.transactionErr = errors.New(secret) },
			run: func(t *testing.T, store *DynamoStore) error {
				_, err := store.Enqueue(context.Background(), testQueueRequest(t))
				return err
			},
			class: hook.FailureRetryable, code: "queue_write_failed",
		},
		"enqueue conflict read": {
			prepare: func(_ *testing.T, api *memoryDynamo, _ *DynamoStore) {
				api.transactionErr = transactionCanceled()
				api.getErr = errors.New(secret)
			},
			run: func(t *testing.T, store *DynamoStore) error {
				_, err := store.Enqueue(context.Background(), testQueueRequest(t))
				return err
			},
			class: hook.FailureRetryable, code: "queue_read_failed",
		},
		"enqueue canceled without binding": {
			prepare: func(_ *testing.T, api *memoryDynamo, _ *DynamoStore) { api.transactionErr = transactionCanceled() },
			run: func(t *testing.T, store *DynamoStore) error {
				_, err := store.Enqueue(context.Background(), testQueueRequest(t))
				return err
			},
			class: hook.FailureRetryable, code: "queue_conflict_unresolved",
		},
		"pull read": {
			prepare: func(t *testing.T, api *memoryDynamo, store *DynamoStore) {
				enqueueForTest(t, store)
				api.getErr = errors.New(secret)
			},
			run: func(t *testing.T, store *DynamoStore) error {
				_, _, err := store.Pull(context.Background(), testPullRequest(t))
				return err
			},
			class: hook.FailureRetryable, code: "pull_read_failed",
		},
		"pull write": {
			prepare: func(t *testing.T, api *memoryDynamo, store *DynamoStore) {
				enqueueForTest(t, store)
				api.transactionErr = errors.New(secret)
			},
			run: func(t *testing.T, store *DynamoStore) error {
				_, _, err := store.Pull(context.Background(), testPullRequest(t))
				return err
			},
			class: hook.FailureRetryable, code: "pull_write_failed",
		},
		"pull conditional cancellation while queued": {
			prepare: func(t *testing.T, api *memoryDynamo, store *DynamoStore) {
				enqueueForTest(t, store)
				api.transactionErr = transactionCanceled()
			},
			run: func(t *testing.T, store *DynamoStore) error {
				_, _, err := store.Pull(context.Background(), testPullRequest(t))
				return err
			},
			class: hook.FailureRetryable, code: "pull_write_failed",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			api := newMemoryDynamo()
			store := testStore(t, api)
			test.prepare(t, api, store)
			err := test.run(t, store)
			class, code := hook.FailureDetails(err)
			if class != test.class || code != test.code || err == nil || strings.Contains(err.Error(), secret) {
				t.Fatalf("class = %s, code = %s, err = %v", class, code, err)
			}
		})
	}
}

func TestConfigAndQueueRequestFailClosed(t *testing.T) {
	if _, err := NewDynamoStore("", newMemoryDynamo()); err == nil {
		t.Fatal("NewDynamoStore accepted an empty table")
	}
	if _, err := NewDynamoStore("valid-table", nil); err == nil {
		t.Fatal("NewDynamoStore accepted a nil client")
	}

	tests := map[string]func(*hook.QueueRequest){
		"zero queue time":  func(request *hook.QueueRequest) { request.QueuedAt = time.Time{} },
		"invalid envelope": func(request *hook.QueueRequest) { request.Envelope.Snapshot.InputSHA256 = "" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			request := testQueueRequest(t)
			mutate(&request)
			disposition, err := testStore(t, newMemoryDynamo()).Enqueue(context.Background(), request)
			class, code := hook.FailureDetails(err)
			if disposition != "" || class != hook.FailureRejected || code != "invalid_queue_request" {
				t.Fatalf("disposition = %s, class = %s, code = %s, err = %v", disposition, class, code, err)
			}
		})
	}
}

func testTerminalRoute(t *testing.T) hook.ReportRouteConfig {
	t.Helper()
	pull := testPullRequest(t)
	return hook.ReportRouteConfig{
		HMACKey:           []byte("0123456789abcdef0123456789abcdef"),
		RepositoryID:      pull.Owner.RepositoryID,
		RepositorySHA256:  pull.Owner.RepositorySHA256,
		WorkflowRefSHA256: pull.Owner.WorkflowRefSHA256,
		ExpectedRunID:     pull.RunID,
		Destinations: []hook.ReportDestination{{
			Repository: "example/target", Delivery: hook.DeliverProduction,
			StagingOrigin: "https://staging.example.com", ProductionOrigin: "https://www.example.com",
		}},
		ClockSkew:           2 * time.Minute,
		LeaseDuration:       2 * time.Minute,
		SpaceKey:            pull.SpaceKey,
		ProjectID:           pull.ProjectID,
		ProjectKey:          pull.ProjectKey,
		AllowedCreatorID:    pull.AllowedCreatorID,
		AllowedActivityType: pull.AllowedActivityType,
		Target:              pull.Target,
	}
}

func testTerminalReport(t *testing.T, envelope hook.DispatchEnvelope, code hook.TerminalCode) hook.TerminalReportRequest {
	t.Helper()
	pull := testPullRequest(t)
	return hook.TerminalReportRequest{
		Protocol:          hook.TerminalReportProtocolVersion,
		DeliveryID:        envelope.DeliveryID,
		InputSHA256:       envelope.Snapshot.InputSHA256,
		RepositoryID:      pull.Owner.RepositoryID,
		RepositorySHA256:  pull.Owner.RepositorySHA256,
		WorkflowRefSHA256: pull.Owner.WorkflowRefSHA256,
		WorkflowSHA:       pull.Owner.WorkflowSHA,
		WorkflowRunID:     pull.Owner.WorkflowRunID,
		RunAttempt:        pull.Owner.RunAttempt,
		AutomationRunID:   pull.RunID,
		Code:              code,
		Repository:        "example/target",
		RunURL: "https://github.com/example/automation-receiver/actions/runs/" +
			strconv.FormatInt(pull.Owner.WorkflowRunID, 10) + "/attempts/" + strconv.Itoa(pull.Owner.RunAttempt),
		IssuedAt:          testQueuedAt.Add(3 * time.Second),
	}
}

func testTerminalBegin(t *testing.T, envelope hook.DispatchEnvelope, code hook.TerminalCode, startedAt time.Time, token string) hook.TerminalBeginRequest {
	t.Helper()
	report := testTerminalReport(t, envelope, code)
	body, err := hook.MarshalTerminalReportRecord(report)
	if err != nil {
		t.Fatalf("MarshalTerminalReportRecord() error = %v", err)
	}
	route := testTerminalRoute(t)
	return hook.TerminalBeginRequest{
		Report: report, ReportJSON: string(body), ReportSHA256: hook.TerminalReportDigest(body), Route: route,
		StartedAt: startedAt, LeaseUntil: startedAt.Add(route.LeaseDuration), LeaseToken: token,
	}
}

func claimForTerminal(t *testing.T, store *DynamoStore) hook.DispatchEnvelope {
	t.Helper()
	envelope := enqueueForTest(t, store)
	got, disposition, err := store.Pull(context.Background(), testPullRequest(t))
	if err != nil || disposition != hook.PullAcquired || got != envelope {
		t.Fatalf("claim: envelope=%+v disposition=%s err=%v", got, disposition, err)
	}
	return envelope
}

func TestBeginTerminalConditionallyBindsClaimedEnvelopeOwnerAndReport(t *testing.T) {
	api := newMemoryDynamo()
	store := testStore(t, api)
	envelope := claimForTerminal(t, store)
	request := testTerminalBegin(t, envelope, hook.TerminalValidationFailed, testQueuedAt.Add(4*time.Second), strings.Repeat("a", 32))
	binding, disposition, err := store.BeginTerminal(context.Background(), request)
	if err != nil || disposition != hook.TerminalBeginAcquired || binding.IssueID != envelope.Snapshot.IssueID || binding.IssueKey != envelope.Snapshot.IssueKey {
		t.Fatalf("binding=%+v disposition=%s err=%v", binding, disposition, err)
	}
	assertNoUnusedExpressionBindings(t, api.lastTransaction.TransactItems[1].Update)
	_, runKey := itemKeys(envelope)
	runItem := api.items[runKey]
	if !attributeStringEquals(runItem, "state", stateReportPending) ||
		!attributeStringEquals(runItem, "terminal_report_sha256", request.ReportSHA256) ||
		!attributeStringEquals(runItem, "terminal_code", string(hook.TerminalValidationFailed)) ||
		!attributeStringEquals(runItem, "terminal_lease_token", request.LeaseToken) ||
		!attributeInt64Equals(runItem, "terminal_lease_until", request.LeaseUntil.UnixMilli()) {
		t.Fatalf("terminal pending item = %+v", runItem)
	}
	if _, exists := runItem["terminal_report_json"]; exists {
		t.Fatal("terminal state persisted report URLs instead of only their digest")
	}
	if got, disposition, err := store.Pull(context.Background(), testPullRequest(t)); err != nil || disposition != hook.PullClaimed || got != (hook.DispatchEnvelope{}) {
		t.Fatalf("pull after terminal begin: envelope=%+v disposition=%s err=%v", got, disposition, err)
	}
	if disposition, err := store.Enqueue(context.Background(), testQueueRequest(t)); err != nil || disposition != hook.QueueClaimed {
		t.Fatalf("enqueue after terminal begin: disposition=%s err=%v", disposition, err)
	}
}

func TestBeginTerminalClassifiesTransactionCancellationReasons(t *testing.T) {
	tests := map[string]struct {
		err       error
		wantClass hook.FailureClass
		wantCode  string
	}{
		"conditional with unchanged binding": {
			err: transactionCanceled(), wantClass: hook.FailureRetryable, wantCode: "terminal_begin_write_failed",
		},
		"transaction conflict": {
			err: transactionCanceledWithReason("TransactionConflict"), wantClass: hook.FailureRetryable, wantCode: "terminal_begin_write_failed",
		},
		"throttling": {
			err: transactionCanceledWithReason("ThrottlingError"), wantClass: hook.FailureRetryable, wantCode: "terminal_begin_write_failed",
		},
		"missing reasons": {
			err:       &types.TransactionCanceledException{Message: aws.String("missing reasons")},
			wantClass: hook.FailureRetryable, wantCode: "terminal_begin_write_failed",
		},
		"validation": {
			err: transactionCanceledWithReason("ValidationError"), wantClass: hook.FailureRejected, wantCode: "terminal_begin_write_rejected",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			api := newMemoryDynamo()
			store := testStore(t, api)
			envelope := claimForTerminal(t, store)
			request := testTerminalBegin(t, envelope, hook.TerminalInternalFailed, testQueuedAt.Add(4*time.Second), strings.Repeat("a", 32))
			api.transactionErr = test.err
			binding, disposition, err := store.BeginTerminal(context.Background(), request)
			class, code := hook.FailureDetails(err)
			if binding != (hook.TerminalBinding{}) || disposition != "" || err == nil || class != test.wantClass || code != test.wantCode {
				t.Fatalf("binding=%+v disposition=%s class=%s code=%s err=%v", binding, disposition, class, code, err)
			}
			_, runKey := itemKeys(envelope)
			if !attributeStringEquals(api.items[runKey], "state", stateClaimed) {
				t.Fatal("canceled transaction changed terminal state")
			}
		})
	}
}

func TestBeginTerminalRetryUsesLeaseAndNeverOverwritesReport(t *testing.T) {
	api := newMemoryDynamo()
	store := testStore(t, api)
	envelope := claimForTerminal(t, store)
	started := testQueuedAt.Add(4 * time.Second)
	first := testTerminalBegin(t, envelope, hook.TerminalNonconverged, started, strings.Repeat("a", 32))
	if _, disposition, err := store.BeginTerminal(context.Background(), first); err != nil || disposition != hook.TerminalBeginAcquired {
		t.Fatalf("first begin disposition=%s err=%v", disposition, err)
	}
	busy := first
	busy.StartedAt = first.LeaseUntil.Add(-time.Nanosecond)
	busy.LeaseUntil = busy.StartedAt.Add(busy.Route.LeaseDuration)
	busy.LeaseToken = strings.Repeat("b", 32)
	busy.Report.IssuedAt = busy.StartedAt
	if _, disposition, err := store.BeginTerminal(context.Background(), busy); err != nil || disposition != hook.TerminalBeginBusy {
		t.Fatalf("busy retry disposition=%s err=%v", disposition, err)
	}
	retry := first
	retry.StartedAt = first.LeaseUntil.Add(time.Millisecond)
	retry.LeaseUntil = retry.StartedAt.Add(retry.Route.LeaseDuration)
	retry.LeaseToken = strings.Repeat("c", 32)
	retry.Report.IssuedAt = retry.StartedAt
	if _, disposition, err := store.BeginTerminal(context.Background(), retry); err != nil || disposition != hook.TerminalBeginAcquired {
		t.Fatalf("expired retry disposition=%s err=%v", disposition, err)
	}
	assertNoUnusedExpressionBindings(t, api.lastTransaction.TransactItems[1].Update)
	_, runKey := itemKeys(envelope)
	if !attributeStringEquals(api.items[runKey], "terminal_lease_token", retry.LeaseToken) ||
		!attributeStringEquals(api.items[runKey], "terminal_report_sha256", first.ReportSHA256) {
		t.Fatalf("retry overwrote report or missed lease update: %+v", api.items[runKey])
	}
	competing := retry
	competing.Report.Code = hook.TerminalInternalFailed
	body, _ := hook.MarshalTerminalReportRecord(competing.Report)
	competing.ReportJSON = string(body)
	competing.ReportSHA256 = hook.TerminalReportDigest(body)
	competing.StartedAt = retry.LeaseUntil.Add(time.Millisecond)
	competing.LeaseUntil = competing.StartedAt.Add(competing.Route.LeaseDuration)
	competing.LeaseToken = strings.Repeat("d", 32)
	competing.Report.IssuedAt = competing.StartedAt
	if _, disposition, err := store.BeginTerminal(context.Background(), competing); err != nil || disposition != hook.TerminalBeginConflict {
		t.Fatalf("competing report disposition=%s err=%v", disposition, err)
	}
	if !attributeStringEquals(api.items[runKey], "terminal_report_sha256", first.ReportSHA256) {
		t.Fatal("competing report overwrote the first terminal result")
	}
}

func TestCompleteTerminalIsConditionalIdempotentAndNonOverwritable(t *testing.T) {
	api := newMemoryDynamo()
	store := testStore(t, api)
	envelope := claimForTerminal(t, store)
	begin := testTerminalBegin(t, envelope, hook.TerminalReleaseFailed, testQueuedAt.Add(4*time.Second), strings.Repeat("a", 32))
	if _, disposition, err := store.BeginTerminal(context.Background(), begin); err != nil || disposition != hook.TerminalBeginAcquired {
		t.Fatalf("begin disposition=%s err=%v", disposition, err)
	}
	complete := hook.TerminalCompleteRequest{
		Report: begin.Report, ReportJSON: begin.ReportJSON, ReportSHA256: begin.ReportSHA256, Route: begin.Route,
		LeaseToken: begin.LeaseToken, CommentID: 808, CompletedAt: testQueuedAt.Add(5 * time.Second),
	}
	if disposition, err := store.CompleteTerminal(context.Background(), complete); err != nil || disposition != hook.TerminalCompleted {
		t.Fatalf("complete disposition=%s err=%v", disposition, err)
	}
	assertNoUnusedExpressionBindings(t, api.lastTransaction.TransactItems[2].Update)
	if disposition, err := store.CompleteTerminal(context.Background(), complete); err != nil || disposition != hook.TerminalAlreadyComplete {
		t.Fatalf("idempotent complete disposition=%s err=%v", disposition, err)
	}
	if _, disposition, err := store.BeginTerminal(context.Background(), begin); err != nil || disposition != hook.TerminalBeginComplete {
		t.Fatalf("begin after complete disposition=%s err=%v", disposition, err)
	}
	competing := complete
	competing.CommentID++
	if disposition, err := store.CompleteTerminal(context.Background(), competing); err != nil || disposition != hook.TerminalCompleteConflict {
		t.Fatalf("competing complete disposition=%s err=%v", disposition, err)
	}
	_, runKey := itemKeys(envelope)
	runItem := api.items[runKey]
	if !attributeStringEquals(runItem, "state", stateTerminal) || !attributeInt64Equals(runItem, "terminal_comment_id", 808) {
		t.Fatalf("terminal item = %+v", runItem)
	}
	if _, ok := runItem["terminal_lease_token"]; ok {
		t.Fatal("terminal state retained lease token")
	}
	// Completion hands the project's single pending slot back, so the next
	// pull finds nothing waiting — and the next ticket can enqueue at all
	// (measured 2026-08-07: the leftover slot blocked the second live ticket).
	pendingKey := makeKey("pending", envelope.Snapshot.SpaceKey, strconv.FormatInt(envelope.Snapshot.ProjectID, 10))
	if _, exists := api.items[pendingKey]; exists {
		t.Fatal("completion did not release the pending slot")
	}
	if got, disposition, err := store.Pull(context.Background(), testPullRequest(t)); err != nil || disposition != hook.PullClaimed || got != (hook.DispatchEnvelope{}) {
		t.Fatalf("pull after complete: envelope=%+v disposition=%s err=%v", got, disposition, err)
	}
}

func TestTerminalBindingRejectsStoredOwnerEnvelopeAndEventTampering(t *testing.T) {
	tests := map[string]func(map[string]map[string]types.AttributeValue, string, string){
		"owner repository": func(items map[string]map[string]types.AttributeValue, _, runKey string) {
			items[runKey]["repository_id"] = numberValue(testRepositoryID + 1)
		},
		"workflow run": func(items map[string]map[string]types.AttributeValue, _, runKey string) {
			items[runKey]["workflow_run_id"] = numberValue(1)
		},
		"input digest": func(items map[string]map[string]types.AttributeValue, _, runKey string) {
			items[runKey]["input_sha256"] = stringValue(strings.Repeat("e", 64))
		},
		"event delivery": func(items map[string]map[string]types.AttributeValue, eventKey, _ string) {
			items[eventKey]["delivery_id"] = stringValue("delivery_00000000000000000000000000000000")
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			api := newMemoryDynamo()
			store := testStore(t, api)
			envelope := claimForTerminal(t, store)
			eventKey, runKey := itemKeys(envelope)
			mutate(api.items, eventKey, runKey)
			request := testTerminalBegin(t, envelope, hook.TerminalInternalFailed, testQueuedAt.Add(4*time.Second), strings.Repeat("a", 32))
			if _, disposition, err := store.BeginTerminal(context.Background(), request); err != nil || disposition != hook.TerminalBeginConflict {
				t.Fatalf("disposition=%s err=%v", disposition, err)
			}
			if !attributeStringEquals(api.items[runKey], "state", stateClaimed) {
				t.Fatal("tampered binding was mutated")
			}
		})
	}
}

func TestConcurrentTerminalBeginHasOneLeaseOwner(t *testing.T) {
	api := newMemoryDynamo()
	store := testStore(t, api)
	envelope := claimForTerminal(t, store)
	const workers = 50
	start := make(chan struct{})
	var acquired atomic.Int64
	var busy atomic.Int64
	var failed atomic.Int64
	var wait sync.WaitGroup
	for index := 0; index < workers; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			request := testTerminalBegin(t, envelope, hook.TerminalModelFailed, testQueuedAt.Add(4*time.Second), strings.Repeat(strconv.FormatInt(int64(index%10), 10), 32))
			_, disposition, err := store.BeginTerminal(context.Background(), request)
			if err != nil {
				failed.Add(1)
				return
			}
			switch disposition {
			case hook.TerminalBeginAcquired:
				acquired.Add(1)
			case hook.TerminalBeginBusy:
				busy.Add(1)
			default:
				failed.Add(1)
			}
		}(index)
	}
	close(start)
	wait.Wait()
	if acquired.Load() != 1 || busy.Load() != workers-1 || failed.Load() != 0 {
		t.Fatalf("acquired=%d busy=%d failed=%d", acquired.Load(), busy.Load(), failed.Load())
	}
}

func assertFailure(t *testing.T, envelope hook.DispatchEnvelope, disposition hook.PullDisposition, err error, wantClass hook.FailureClass, wantCode, forbidden string) {
	t.Helper()
	class, code := hook.FailureDetails(err)
	if envelope != (hook.DispatchEnvelope{}) || disposition != "" || err == nil || class != wantClass || code != wantCode ||
		(forbidden != "" && strings.Contains(err.Error(), forbidden)) {
		t.Fatalf("envelope = %+v, disposition = %s, class = %s, code = %s, err = %v", envelope, disposition, class, code, err)
	}
}

func assertNoUnusedExpressionBindings(t *testing.T, update *types.Update) {
	t.Helper()
	if update == nil {
		t.Fatal("missing update expression")
	}
	expression := aws.ToString(update.UpdateExpression) + " " + aws.ToString(update.ConditionExpression)
	for placeholder := range update.ExpressionAttributeNames {
		if !strings.Contains(expression, placeholder) {
			t.Fatalf("unused expression attribute name %q in %q", placeholder, expression)
		}
	}
	for placeholder := range update.ExpressionAttributeValues {
		if !strings.Contains(expression, placeholder) {
			t.Fatalf("unused expression attribute value %q in %q", placeholder, expression)
		}
	}
}

// TestPullFindsWhicheverTicketIsWaiting fixes the removal of the single
// configured identifier. The puller asks for work without naming a ticket, and
// the store answers with the one that was queued. Before this, both sides had
// to agree on one value, and because the record key was built from it, exactly
// one ticket could ever be processed: a second one collided and vanished.
func TestPullFindsWhicheverTicketIsWaiting(t *testing.T) {
	api := newMemoryDynamo()
	store := testStore(t, api)
	request := hook.QueueRequest{Envelope: testEnvelope(t), QueuedAt: testQueuedAt}
	if _, err := store.Enqueue(context.Background(), request); err != nil {
		t.Fatal(err)
	}

	claim := testPullRequest(t)
	claim.RunID = ""
	envelope, disposition, err := store.Pull(context.Background(), claim)
	if err != nil || disposition != hook.PullAcquired {
		t.Fatalf("a caller that names no ticket must still receive the queued one: %s, %v", disposition, err)
	}
	if envelope.Snapshot.RunID != request.Envelope.Snapshot.RunID {
		t.Fatalf("run = %q, want %q", envelope.Snapshot.RunID, request.Envelope.Snapshot.RunID)
	}
}

// TestPullReportsNoWorkWhenNothingIsQueued keeps the empty case distinct from a
// failure: an idle schedule must not look like a broken one.
func TestPullReportsNoWorkWhenNothingIsQueued(t *testing.T) {
	store := testStore(t, newMemoryDynamo())
	claim := testPullRequest(t)
	claim.RunID = ""
	if _, disposition, err := store.Pull(context.Background(), claim); err != nil || disposition != hook.PullEmpty {
		t.Fatalf("disposition = %s, err = %v", disposition, err)
	}
}
