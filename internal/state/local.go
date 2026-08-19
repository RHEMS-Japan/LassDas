// Package state's local store: the same ledger the DynamoDB store keeps,
// held in a single SQLite file on the host that runs everything.
//
// The semantics are a deliberate, line-for-line sibling of DynamoStore.
// Every operation there is "read the bound rows consistently, compare every
// sealed attribute, then write the transition under a condition that
// re-states all of them". SQLite under BEGIN IMMEDIATE gives the same
// guarantee more directly: the transaction holds the single writer lock, so
// a read-check-write inside it IS the conditional transaction — no
// condition-expression language needed, the checks are plain Go against the
// row read inside the same transaction. What must stay identical is WHAT is
// checked and written, and that is transcribed operation by operation from
// dynamodb.go and its siblings; divergence is a bug.
//
// Rows keep the DynamoDB shape: one table, `pk` primary key, all other
// attributes in one JSON object using the exact attribute names the Dynamo
// items use. That keeps every binding comparison readable against the
// original and lets an operator diff a ledger row against the docs.
package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	_ "modernc.org/sqlite"
)

// LocalStore is the SQLite implementation of the same store interfaces the
// hook package consumes. One writer at a time by construction.
type LocalStore struct {
	db *sql.DB
}

// NewLocalStore opens (creating if needed) the ledger at path.
func NewLocalStore(path string) (*LocalStore, error) {
	if path == "" {
		return nil, errors.New("local ledger path must not be empty")
	}
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(FULL)")
	if err != nil {
		return nil, fmt.Errorf("local ledger could not open: %w", err)
	}
	// A single connection makes BEGIN IMMEDIATE the whole concurrency story.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(
		"CREATE TABLE IF NOT EXISTS ledger (pk TEXT PRIMARY KEY, attrs TEXT NOT NULL)",
	); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("local ledger could not initialize: %w", err)
	}
	return &LocalStore{db: db}, nil
}

// Close releases the underlying database handle.
func (s *LocalStore) Close() error { return s.db.Close() }

// item is one ledger row's attributes. Values are strings and int64s only —
// the exact value kinds the Dynamo items use (S and N).
type item map[string]any

func (m item) str(name string) (string, bool) {
	value, ok := m[name].(string)
	return value, ok
}

func (m item) int64At(name string) (int64, bool) {
	switch v := m[name].(type) {
	case int64:
		return v, true
	case float64:
		// json.Unmarshal's default numeric type; the encoder below never
		// produces fractions, so this is an integer in float clothing.
		return int64(v), true
	}
	return 0, false
}

func (m item) strEquals(name, want string) bool {
	value, ok := m.str(name)
	return ok && value == want
}

func (m item) int64Equals(name string, want int64) bool {
	value, ok := m.int64At(name)
	return ok && value == want
}

func (m item) has(name string) bool {
	_, ok := m[name]
	return ok
}

// tx wraps one BEGIN IMMEDIATE transaction over the ledger.
type tx struct {
	tx *sql.Tx
}

func (s *LocalStore) begin(ctx context.Context) (*tx, error) {
	raw, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	// database/sql has no IMMEDIATE knob; taking the write lock up front is
	// what makes read-check-write atomic against other writers. A no-op
	// write acquires it.
	if _, err := raw.Exec(
		"INSERT INTO ledger(pk, attrs) VALUES ('#lock', '{}') ON CONFLICT(pk) DO UPDATE SET attrs = attrs",
	); err != nil {
		_ = raw.Rollback()
		return nil, err
	}
	return &tx{tx: raw}, nil
}

// beginRead opens a transaction without taking the write lock. Only for
// operations that never write: under WAL they read a consistent snapshot
// without serializing against the single writer (the attendant's per-tick
// loads were measurably queueing behind a working runner's stages).
func (s *LocalStore) beginRead(ctx context.Context) (*tx, error) {
	raw, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	return &tx{tx: raw}, nil
}

func (t *tx) rollback() { _ = t.tx.Rollback() }

func (t *tx) commit() error { return t.tx.Commit() }

// getItem reads one row inside the transaction. A missing row is (nil, nil).
func (t *tx) getItem(pk string) (item, error) {
	var encoded string
	err := t.tx.QueryRow("SELECT attrs FROM ledger WHERE pk = ?", pk).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	decoded := item{}
	if err := json.Unmarshal([]byte(encoded), &decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}

// putNew inserts a row that must not exist (attribute_not_exists(pk)).
// Returns false when the row already exists.
func (t *tx) putNew(pk string, attrs item) (bool, error) {
	encoded, err := json.Marshal(attrs)
	if err != nil {
		return false, err
	}
	result, err := t.tx.Exec(
		"INSERT INTO ledger(pk, attrs) VALUES (?, ?) ON CONFLICT(pk) DO NOTHING",
		pk, string(encoded),
	)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

// setItem overwrites a row's attributes (the row's existence and every
// precondition were checked by the caller inside this same transaction).
func (t *tx) setItem(pk string, attrs item) error {
	encoded, err := json.Marshal(attrs)
	if err != nil {
		return err
	}
	_, err = t.tx.Exec(
		"INSERT INTO ledger(pk, attrs) VALUES (?, ?) "+
			"ON CONFLICT(pk) DO UPDATE SET attrs = excluded.attrs",
		pk, string(encoded),
	)
	return err
}

// deleteItem removes a row (the caller checked its precondition).
func (t *tx) deleteItem(pk string) error {
	_, err := t.tx.Exec("DELETE FROM ledger WHERE pk = ?", pk)
	return err
}

// ---------------------------------------------------------------------------
// Shared item comparisons (map-shaped siblings of the AttributeValue ones)
// ---------------------------------------------------------------------------

func localEventItemMatches(row item, envelope hook.DispatchEnvelope, runKey string) bool {
	return len(row) > 0 &&
		row.strEquals("record_type", "event") &&
		row.int64Equals("activity_id", envelope.Snapshot.ActivityID) &&
		row.strEquals("run_key", runKey) &&
		row.strEquals("delivery_id", envelope.DeliveryID) &&
		row.strEquals("input_sha256", envelope.Snapshot.InputSHA256)
}

func localRunItemMatches(row item, envelope hook.DispatchEnvelope, eventKey, runKey string) bool {
	return len(row) > 0 &&
		row.strEquals("pk", runKey) &&
		row.strEquals("record_type", "run") &&
		row.strEquals("event_key", eventKey) &&
		row.int64Equals("activity_id", envelope.Snapshot.ActivityID) &&
		row.strEquals("run_id", envelope.Snapshot.RunID) &&
		row.strEquals("delivery_id", envelope.DeliveryID) &&
		row.strEquals("input_sha256", envelope.Snapshot.InputSHA256)
}

// ---------------------------------------------------------------------------
// Queue: Enqueue / Pull (sibling of dynamodb.go)
// ---------------------------------------------------------------------------

func localFailure(class hook.FailureClass, code string) error {
	// The service name must satisfy the failure code charset (no hyphen), or
	// NewExternalFailure collapses every code to unexpected_failure and the
	// real cause is masked in logs and dispositions.
	return hook.NewExternalFailure("local_ledger", class, code)
}

func (s *LocalStore) Enqueue(ctx context.Context, request hook.QueueRequest) (hook.QueueDisposition, error) {
	if request.QueuedAt.IsZero() || request.Envelope.ClarificationJSON != "" ||
		hook.ValidateEnvelope(request.Envelope) != nil {
		return "", localFailure(hook.FailureRejected, "invalid_queue_request")
	}
	encoded, err := json.Marshal(request.Envelope)
	if err != nil || len(encoded) == 0 || len(encoded) > 65535 {
		return "", localFailure(hook.FailureRejected, "invalid_queue_envelope")
	}
	snapshot := request.Envelope.Snapshot
	eventKey := makeKey("event", snapshot.SpaceKey, strconv.FormatInt(snapshot.ProjectID, 10), strconv.FormatInt(snapshot.ActivityID, 10))
	runKey := makeKey("run", snapshot.SpaceKey, strconv.FormatInt(snapshot.ProjectID, 10), snapshot.RunID)
	pendingKey := makeKey("pending", snapshot.SpaceKey, strconv.FormatInt(snapshot.ProjectID, 10))
	queuedAt := request.QueuedAt.UTC().UnixMilli()

	txn, err := s.begin(ctx)
	if err != nil {
		return "", localFailure(hook.FailureRetryable, "queue_write_failed")
	}
	defer txn.rollback()
	eventRow, err1 := txn.getItem(eventKey)
	runRow, err2 := txn.getItem(runKey)
	pendingRow, err3 := txn.getItem(pendingKey)
	if err1 != nil || err2 != nil || err3 != nil {
		return "", localFailure(hook.FailureRetryable, "queue_read_failed")
	}
	if eventRow == nil && runRow == nil && pendingRow == nil {
		eventItem := item{
			"pk": eventKey, "record_type": "event", "activity_id": snapshot.ActivityID,
			"run_key": runKey, "delivery_id": request.Envelope.DeliveryID, "input_sha256": snapshot.InputSHA256,
		}
		runItem := item{
			"pk": runKey, "record_type": "run", "event_key": eventKey, "activity_id": snapshot.ActivityID,
			"run_id": snapshot.RunID, "delivery_id": request.Envelope.DeliveryID, "input_sha256": snapshot.InputSHA256,
			"envelope_json": string(encoded), "queued_at": queuedAt, "state": stateQueued,
		}
		pendingItem := item{
			"pk": pendingKey, "record_type": "pending", "run_id": snapshot.RunID,
			"run_key": runKey, "queued_at": queuedAt,
		}
		for pk, attrs := range map[string]item{eventKey: eventItem, runKey: runItem, pendingKey: pendingItem} {
			created, putErr := txn.putNew(pk, attrs)
			if putErr != nil || !created {
				return "", localFailure(hook.FailureRetryable, "queue_write_failed")
			}
		}
		if err := txn.commit(); err != nil {
			return "", localFailure(hook.FailureRetryable, "queue_write_failed")
		}
		return hook.QueueCreated, nil
	}
	// Some row already exists: resolve exactly the way the Dynamo store
	// resolves a cancelled transaction, from the same rows.
	if eventRow == nil && runRow == nil {
		return "", localFailure(hook.FailureRetryable, "queue_conflict_unresolved")
	}
	stateValue, _ := runRow.str("state")
	if !(localEventItemMatches(eventRow, request.Envelope, runKey) &&
		localRunItemMatches(runRow, request.Envelope, eventKey, runKey) &&
		runRow.strEquals("envelope_json", string(encoded))) {
		return hook.QueueConflict, nil
	}
	switch stateValue {
	case stateQueued:
		return hook.QueueDuplicate, nil
	case stateClaimed, stateQuestionPending, stateAwaitingAnswer, stateReportPending, stateTerminal:
		return hook.QueueClaimed, nil
	default:
		return "", localFailure(hook.FailureRejected, "invalid_queue_state")
	}
}

func (s *LocalStore) Pull(ctx context.Context, request hook.PullClaimRequest) (hook.DispatchEnvelope, hook.PullDisposition, error) {
	if !validPullRequest(request) {
		return hook.DispatchEnvelope{}, "", localFailure(hook.FailureRejected, "invalid_pull_request")
	}
	txn, err := s.begin(ctx)
	if err != nil {
		return hook.DispatchEnvelope{}, "", localFailure(hook.FailureRetryable, "pull_read_failed")
	}
	defer txn.rollback()

	runID := request.RunID
	if runID == "" {
		pendingRow, pendingErr := txn.getItem(makeKey("pending", request.SpaceKey, strconv.FormatInt(request.ProjectID, 10)))
		if pendingErr != nil {
			return hook.DispatchEnvelope{}, "", localFailure(hook.FailureRetryable, "pull_read_failed")
		}
		if pendingRow == nil {
			return hook.DispatchEnvelope{}, hook.PullEmpty, nil
		}
		value, ok := pendingRow.str("run_id")
		if !ok || !hook.ValidRunID(value) {
			return hook.DispatchEnvelope{}, "", localFailure(hook.FailureRejected, "pull_binding_invalid")
		}
		runID = value
	}
	runKey := makeKey("run", request.SpaceKey, strconv.FormatInt(request.ProjectID, 10), runID)
	runRow, err := txn.getItem(runKey)
	if err != nil {
		return hook.DispatchEnvelope{}, "", localFailure(hook.FailureRetryable, "pull_read_failed")
	}
	if runRow == nil {
		return hook.DispatchEnvelope{}, hook.PullEmpty, nil
	}
	stateValue, stateOK := runRow.str("state")
	if !stateOK {
		return hook.DispatchEnvelope{}, "", localFailure(hook.FailureRejected, "pull_binding_invalid")
	}
	if stateValue != stateQueued && stateValue != stateClaimed && stateValue != stateQuestionPending &&
		stateValue != stateAwaitingAnswer && stateValue != stateReportPending && stateValue != stateTerminal {
		return hook.DispatchEnvelope{}, "", localFailure(hook.FailureRejected, "pull_state_invalid")
	}
	envelopeJSON, ok := runRow.str("envelope_json")
	if !ok || len(envelopeJSON) == 0 || len(envelopeJSON) > 65535 {
		return hook.DispatchEnvelope{}, "", localFailure(hook.FailureRejected, "pull_envelope_invalid")
	}
	envelope, err := decodeEnvelope([]byte(envelopeJSON))
	if err != nil || !snapshotAllowed(envelope.Snapshot, request) {
		return hook.DispatchEnvelope{}, "", localFailure(hook.FailureRejected, "pull_envelope_invalid")
	}
	snapshot := envelope.Snapshot
	eventKey := makeKey("event", snapshot.SpaceKey, strconv.FormatInt(snapshot.ProjectID, 10), strconv.FormatInt(snapshot.ActivityID, 10))
	if !localRunItemMatches(runRow, envelope, eventKey, runKey) {
		return hook.DispatchEnvelope{}, "", localFailure(hook.FailureRejected, "pull_binding_invalid")
	}
	queuedAt, ok := runRow.int64At("queued_at")
	if !ok || request.IssuedAt.UTC().UnixMilli() < queuedAt {
		return hook.DispatchEnvelope{}, hook.PullConflict, nil
	}
	eventRow, err := txn.getItem(eventKey)
	if err != nil {
		return hook.DispatchEnvelope{}, "", localFailure(hook.FailureRetryable, "pull_read_failed")
	}
	if !localEventItemMatches(eventRow, envelope, runKey) {
		return hook.DispatchEnvelope{}, "", localFailure(hook.FailureRejected, "pull_binding_invalid")
	}
	if stateValue == stateQuestionPending || stateValue == stateAwaitingAnswer ||
		stateValue == stateReportPending || stateValue == stateTerminal {
		return hook.DispatchEnvelope{}, hook.PullClaimed, nil
	}
	if stateValue == stateClaimed {
		claim, valid := localDecodeStoredPullClaim(runRow, request, queuedAt)
		if !valid {
			return hook.DispatchEnvelope{}, "", localFailure(hook.FailureRejected, "pull_binding_invalid")
		}
		if claim.owner == request.Owner {
			return localAttachClarification(runRow, envelope)
		}
		return hook.DispatchEnvelope{}, hook.PullClaimed, nil
	}
	// stateQueued: claim it. Every condition the Dynamo update re-states was
	// just read and compared inside this same transaction.
	runRow["state"] = stateClaimed
	runRow["claimed_at"] = request.ClaimedAt.UTC().UnixMilli()
	runRow["repository_id"] = request.Owner.RepositoryID
	runRow["repository_sha256"] = request.Owner.RepositorySHA256
	runRow["workflow_ref_sha256"] = request.Owner.WorkflowRefSHA256
	runRow["workflow_sha"] = request.Owner.WorkflowSHA
	runRow["workflow_run_id"] = request.Owner.WorkflowRunID
	runRow["run_attempt"] = int64(request.Owner.RunAttempt)
	if err := txn.setItem(runKey, runRow); err != nil {
		return hook.DispatchEnvelope{}, "", localFailure(hook.FailureRetryable, "pull_write_failed")
	}
	if err := txn.commit(); err != nil {
		return hook.DispatchEnvelope{}, "", localFailure(hook.FailureRetryable, "pull_write_failed")
	}
	return localAttachClarification(runRow, envelope)
}

func localDecodeStoredPullClaim(row item, request hook.PullClaimRequest, queuedAt int64) (storedPullClaim, bool) {
	claimedAt, claimedOK := row.int64At("claimed_at")
	repositoryID, repositoryOK := row.int64At("repository_id")
	repositoryDigest, repositoryDigestOK := row.str("repository_sha256")
	workflowRefDigest, workflowRefOK := row.str("workflow_ref_sha256")
	workflowSHA, workflowSHAOK := row.str("workflow_sha")
	workflowRunID, workflowRunOK := row.int64At("workflow_run_id")
	runAttempt, runAttemptOK := row.int64At("run_attempt")
	claim := storedPullClaim{owner: hook.PullOwner{
		RepositoryID: repositoryID, RepositorySHA256: repositoryDigest, WorkflowRefSHA256: workflowRefDigest,
		WorkflowSHA: workflowSHA, WorkflowRunID: workflowRunID, RunAttempt: int(runAttempt),
	}, claimedAt: claimedAt}
	return claim, claimedOK && repositoryOK && repositoryDigestOK && workflowRefOK && workflowSHAOK && workflowRunOK && runAttemptOK &&
		claimedAt > 0 && (queuedAt == 0 || claimedAt >= queuedAt) &&
		repositoryID == request.Target.RepositoryID && repositoryDigest == request.Owner.RepositorySHA256 &&
		workflowRefDigest == request.Target.WorkflowRefSHA256 && commitPattern.MatchString(workflowSHA) && workflowRunID > 0 && runAttempt > 0 &&
		localTerminalStateShapeValid(row)
}

func localAttachClarification(row item, envelope hook.DispatchEnvelope) (hook.DispatchEnvelope, hook.PullDisposition, error) {
	digest, _, ok := localClarificationStateConsistent(row)
	if !ok {
		return hook.DispatchEnvelope{}, "", localFailure(hook.FailureRejected, "pull_binding_invalid")
	}
	if digest == "" {
		return envelope, hook.PullAcquired, nil
	}
	recordJSON, _ := row.str("clarification_json")
	envelope.ClarificationJSON = recordJSON
	if hook.ValidateEnvelope(envelope) != nil {
		return hook.DispatchEnvelope{}, "", localFailure(hook.FailureRejected, "pull_binding_invalid")
	}
	return envelope, hook.PullAcquired, nil
}

// ---------------------------------------------------------------------------
// Row-shape validation (sibling of terminalStateShapeValid and friends)
// ---------------------------------------------------------------------------

func localTerminalReportMatches(row item, digest string, code hook.TerminalCode) bool {
	return row.strEquals("terminal_report_sha256", digest) && row.strEquals("terminal_code", string(code))
}

func localValidStoredCommentID(row item) bool {
	value, ok := row.int64At("terminal_comment_id")
	return ok && value > 0
}

func localClarificationStateConsistent(row item) (string, int, bool) {
	digest, digestExists := row.str("clarification_sha256")
	recordJSON, jsonExists := row.str("clarification_json")
	revision, revisionExists := row.int64At("input_revision")
	if !digestExists && !jsonExists && !revisionExists {
		if row.has("clarification_sha256") || row.has("clarification_json") || row.has("input_revision") {
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

func localTerminalStateShapeValid(row item) bool {
	stateValue, ok := row.str("state")
	if !ok {
		return false
	}
	clarificationDigest, clarificationRounds, clarificationOK := localClarificationStateConsistent(row)
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
		for _, name := range append(append([]string{"claimed_at"}, terminalFields...), questionFields...) {
			if row.has(name) {
				return false
			}
		}
		return true
	}
	claimedAt, claimedOK := row.int64At("claimed_at")
	if !claimedOK || claimedAt <= 0 {
		return false
	}
	switch stateValue {
	case stateClaimed:
		for _, name := range append(append([]string{}, terminalFields...), questionFields...) {
			if row.has(name) {
				return false
			}
		}
		return true
	case stateQuestionPending:
		for _, name := range terminalFields {
			if row.has(name) {
				return false
			}
		}
		recordDigest, digestOK := row.str("question_record_sha256")
		recordJSON, jsonOK := row.str("question_record_json")
		startedAt, startedOK := row.int64At("question_started_at")
		leaseUntil, leaseOK := row.int64At("question_lease_until")
		leaseToken, tokenOK := row.str("question_lease_token")
		record, recordOK := questionRecordJSONValid(recordJSON, recordDigest)
		return digestOK && digestPattern.MatchString(recordDigest) &&
			jsonOK && recordOK && questionRevisionConsistent(record, clarificationDigest, clarificationRounds) &&
			startedOK && startedAt > 0 && leaseOK && leaseUntil >= startedAt &&
			tokenOK && leasePattern.MatchString(leaseToken) &&
			!row.has("question_comment_id") && !row.has("question_posted_at")
	case stateAwaitingAnswer:
		for _, name := range terminalFields {
			if row.has(name) {
				return false
			}
		}
		recordDigest, digestOK := row.str("question_record_sha256")
		recordJSON, jsonOK := row.str("question_record_json")
		startedAt, startedOK := row.int64At("question_started_at")
		commentID, commentOK := row.int64At("question_comment_id")
		postedAt, postedOK := row.int64At("question_posted_at")
		record, recordOK := questionRecordJSONValid(recordJSON, recordDigest)
		return digestOK && digestPattern.MatchString(recordDigest) &&
			jsonOK && recordOK && questionRevisionConsistent(record, clarificationDigest, clarificationRounds) &&
			startedOK && startedAt > 0 && commentOK && commentID > 0 &&
			postedOK && postedAt >= startedAt &&
			!row.has("question_lease_until") && !row.has("question_lease_token")
	case stateReportPending:
		reportDigest, digestOK := row.str("terminal_report_sha256")
		code, codeOK := row.str("terminal_code")
		startedAt, startedOK := row.int64At("terminal_started_at")
		leaseUntil, leaseOK := row.int64At("terminal_lease_until")
		leaseToken, tokenOK := row.str("terminal_lease_token")
		return digestOK && digestPattern.MatchString(reportDigest) &&
			codeOK && validTerminalCode(code) && startedOK && startedAt > 0 && leaseOK && leaseUntil >= startedAt &&
			tokenOK && leasePattern.MatchString(leaseToken) &&
			!row.has("terminal_comment_id") && !row.has("terminal_completed_at")
	case stateTerminal:
		reportDigest, digestOK := row.str("terminal_report_sha256")
		code, codeOK := row.str("terminal_code")
		startedAt, startedOK := row.int64At("terminal_started_at")
		commentID, commentOK := row.int64At("terminal_comment_id")
		completedAt, completedOK := row.int64At("terminal_completed_at")
		return digestOK && digestPattern.MatchString(reportDigest) &&
			codeOK && validTerminalCode(code) && startedOK && startedAt > 0 && commentOK && commentID > 0 &&
			completedOK && completedAt >= startedAt &&
			!row.has("terminal_lease_until") && !row.has("terminal_lease_token")
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// Terminal report: BeginTerminal / CompleteTerminal (sibling of dynamodb.go)
// ---------------------------------------------------------------------------

type localTerminalBinding struct {
	runRow       item
	eventRow     item
	envelope     hook.DispatchEnvelope
	envelopeJSON string
	runKey       string
	eventKey     string
}

func (t *tx) loadTerminalBinding(runID string, route hook.ReportRouteConfig) (localTerminalBinding, error) {
	runKey := makeKey("run", route.SpaceKey, strconv.FormatInt(route.ProjectID, 10), runID)
	runRow, err := t.getItem(runKey)
	if err != nil {
		return localTerminalBinding{}, localFailure(hook.FailureRetryable, "terminal_read_failed")
	}
	if runRow == nil {
		return localTerminalBinding{}, localFailure(hook.FailureRejected, "terminal_binding_missing")
	}
	envelopeJSON, ok := runRow.str("envelope_json")
	if !ok || len(envelopeJSON) == 0 || len(envelopeJSON) > 65535 {
		return localTerminalBinding{}, localFailure(hook.FailureRejected, "terminal_binding_invalid")
	}
	envelope, err := decodeEnvelope([]byte(envelopeJSON))
	if err != nil {
		return localTerminalBinding{}, localFailure(hook.FailureRejected, "terminal_binding_invalid")
	}
	eventKey := makeKey("event", envelope.Snapshot.SpaceKey, strconv.FormatInt(envelope.Snapshot.ProjectID, 10), strconv.FormatInt(envelope.Snapshot.ActivityID, 10))
	eventRow, err := t.getItem(eventKey)
	if err != nil {
		return localTerminalBinding{}, localFailure(hook.FailureRetryable, "terminal_read_failed")
	}
	return localTerminalBinding{
		runRow: runRow, eventRow: eventRow, envelope: envelope, envelopeJSON: envelopeJSON,
		runKey: runKey, eventKey: eventKey,
	}, nil
}

func localTerminalBindingMatches(binding localTerminalBinding, report hook.TerminalReportRequest, route hook.ReportRouteConfig) bool {
	snapshot := binding.envelope.Snapshot
	return localRunItemMatches(binding.runRow, binding.envelope, binding.eventKey, binding.runKey) &&
		localEventItemMatches(binding.eventRow, binding.envelope, binding.runKey) &&
		localTerminalStateShapeValid(binding.runRow) &&
		snapshot.SpaceKey == route.SpaceKey && snapshot.ProjectID == route.ProjectID && snapshot.ProjectKey == route.ProjectKey &&
		snapshot.CreatorID == route.AllowedCreatorID && snapshot.ActivityType == route.AllowedActivityType &&
		snapshot.RunID == route.ExpectedRunID && snapshot.Target == route.Target &&
		report.DeliveryID == binding.envelope.DeliveryID && report.InputSHA256 == snapshot.InputSHA256 &&
		report.AutomationRunID == snapshot.RunID && report.RepositoryID == route.RepositoryID &&
		binding.runRow.int64Equals("repository_id", report.RepositoryID) &&
		binding.runRow.strEquals("repository_sha256", report.RepositorySHA256) &&
		binding.runRow.strEquals("workflow_ref_sha256", report.WorkflowRefSHA256) &&
		binding.runRow.strEquals("workflow_sha", report.WorkflowSHA) &&
		binding.runRow.int64Equals("workflow_run_id", report.WorkflowRunID) &&
		binding.runRow.int64Equals("run_attempt", int64(report.RunAttempt))
}

// ResolveRunRoute is the local sibling of the Dynamo store's route rebinding
// for routeful-but-recordless callers (the question tick).
func (s *LocalStore) resolveRunRoute(ctx context.Context, route hook.ReportRouteConfig) (hook.ReportRouteConfig, error) {
	txn, err := s.beginRead(ctx)
	if err != nil {
		return route, localFailure(hook.FailureRetryable, "run_route_read_failed")
	}
	defer txn.rollback()
	directKey := makeKey("run", route.SpaceKey, strconv.FormatInt(route.ProjectID, 10), route.ExpectedRunID)
	direct, err := txn.getItem(directKey)
	if err != nil {
		return route, localFailure(hook.FailureRetryable, "run_route_read_failed")
	}
	if direct != nil {
		return route, nil
	}
	pendingRow, err := txn.getItem(makeKey("pending", route.SpaceKey, strconv.FormatInt(route.ProjectID, 10)))
	if err != nil {
		return route, localFailure(hook.FailureRetryable, "run_route_read_failed")
	}
	runID, ok := pendingRow.str("run_id")
	if !ok || !strings.HasPrefix(runID, route.ProjectKey+"-") || !issueRunIDPattern.MatchString(runID) {
		return route, nil
	}
	rebound := route
	rebound.ExpectedRunID = runID
	return rebound, nil
}

func (s *LocalStore) BeginTerminal(ctx context.Context, request hook.TerminalBeginRequest) (hook.TerminalBinding, hook.TerminalBeginDisposition, error) {
	if !validTerminalBeginRequest(request) {
		return hook.TerminalBinding{}, "", localFailure(hook.FailureRejected, "invalid_terminal_begin")
	}
	txn, err := s.begin(ctx)
	if err != nil {
		return hook.TerminalBinding{}, "", localFailure(hook.FailureRetryable, "terminal_read_failed")
	}
	defer txn.rollback()
	binding, err := txn.loadTerminalBinding(request.Report.AutomationRunID, request.Route)
	if err != nil {
		return hook.TerminalBinding{}, "", err
	}
	if !localTerminalBindingMatches(binding, request.Report, request.Route) {
		return hook.TerminalBinding{}, hook.TerminalBeginConflict, nil
	}
	result := hook.TerminalBinding{IssueID: binding.envelope.Snapshot.IssueID, IssueKey: binding.envelope.Snapshot.IssueKey}
	stateValue, _ := binding.runRow.str("state")
	switch stateValue {
	case stateTerminal:
		if localTerminalReportMatches(binding.runRow, request.ReportSHA256, request.Report.Code) && localValidStoredCommentID(binding.runRow) {
			return result, hook.TerminalBeginComplete, nil
		}
		return result, hook.TerminalBeginConflict, nil
	case stateReportPending:
		if !localTerminalReportMatches(binding.runRow, request.ReportSHA256, request.Report.Code) {
			return result, hook.TerminalBeginConflict, nil
		}
		leaseUntil, ok := binding.runRow.int64At("terminal_lease_until")
		if !ok {
			return hook.TerminalBinding{}, "", localFailure(hook.FailureRejected, "terminal_binding_invalid")
		}
		if leaseUntil >= request.StartedAt.UnixMilli() {
			return result, hook.TerminalBeginBusy, nil
		}
		// Re-acquire the expired lease.
		binding.runRow["terminal_lease_until"] = request.LeaseUntil.UnixMilli()
		binding.runRow["terminal_lease_token"] = request.LeaseToken
		if err := txn.setItem(binding.runKey, binding.runRow); err != nil {
			return hook.TerminalBinding{}, "", localFailure(hook.FailureRetryable, "terminal_begin_write_failed")
		}
		if err := txn.commit(); err != nil {
			return hook.TerminalBinding{}, "", localFailure(hook.FailureRetryable, "terminal_begin_write_failed")
		}
		return result, hook.TerminalBeginAcquired, nil
	case stateClaimed:
		if !terminalCodeAllowedFromClaimed(request.Report.Code) {
			return result, hook.TerminalBeginConflict, nil
		}
		// startTerminal: claimed -> terminal_report_pending. The Dynamo
		// condition additionally requires that no terminal or question
		// evidence exists; the shape check above guarantees it for claimed.
		binding.runRow["state"] = stateReportPending
		binding.runRow["terminal_report_sha256"] = request.ReportSHA256
		binding.runRow["terminal_code"] = string(request.Report.Code)
		binding.runRow["terminal_started_at"] = request.StartedAt.UnixMilli()
		binding.runRow["terminal_lease_until"] = request.LeaseUntil.UnixMilli()
		binding.runRow["terminal_lease_token"] = request.LeaseToken
		if err := txn.setItem(binding.runKey, binding.runRow); err != nil {
			return hook.TerminalBinding{}, "", localFailure(hook.FailureRetryable, "terminal_begin_write_failed")
		}
		if err := txn.commit(); err != nil {
			return hook.TerminalBinding{}, "", localFailure(hook.FailureRetryable, "terminal_begin_write_failed")
		}
		return result, hook.TerminalBeginAcquired, nil
	case stateAwaitingAnswer:
		if !terminalCodeAllowedFromAwaiting(request.Report.Code) {
			return result, hook.TerminalBeginConflict, nil
		}
		// startTerminalFromAwaiting: the question evidence is kept; the
		// shape check for awaiting_answer guarantees the sealed question
		// and its comment are present and consistent.
		if _, ok := binding.runRow.str("question_record_sha256"); !ok {
			return hook.TerminalBinding{}, "", localFailure(hook.FailureRejected, "terminal_binding_invalid")
		}
		binding.runRow["state"] = stateReportPending
		binding.runRow["terminal_report_sha256"] = request.ReportSHA256
		binding.runRow["terminal_code"] = string(request.Report.Code)
		binding.runRow["terminal_started_at"] = request.StartedAt.UnixMilli()
		binding.runRow["terminal_lease_until"] = request.LeaseUntil.UnixMilli()
		binding.runRow["terminal_lease_token"] = request.LeaseToken
		if err := txn.setItem(binding.runKey, binding.runRow); err != nil {
			return hook.TerminalBinding{}, "", localFailure(hook.FailureRetryable, "terminal_begin_write_failed")
		}
		if err := txn.commit(); err != nil {
			return hook.TerminalBinding{}, "", localFailure(hook.FailureRetryable, "terminal_begin_write_failed")
		}
		return result, hook.TerminalBeginAcquired, nil
	default:
		return result, hook.TerminalBeginConflict, nil
	}
}

func (s *LocalStore) CompleteTerminal(ctx context.Context, request hook.TerminalCompleteRequest) (hook.TerminalCompleteDisposition, error) {
	if !validTerminalCompleteRequest(request) {
		return "", localFailure(hook.FailureRejected, "invalid_terminal_complete")
	}
	txn, err := s.begin(ctx)
	if err != nil {
		return "", localFailure(hook.FailureRetryable, "terminal_read_failed")
	}
	defer txn.rollback()
	binding, err := txn.loadTerminalBinding(request.Report.AutomationRunID, request.Route)
	if err != nil {
		return "", err
	}
	if !localTerminalBindingMatches(binding, request.Report, request.Route) {
		return hook.TerminalCompleteConflict, nil
	}
	stateValue, _ := binding.runRow.str("state")
	if stateValue == stateTerminal {
		if localTerminalReportMatches(binding.runRow, request.ReportSHA256, request.Report.Code) &&
			binding.runRow.int64Equals("terminal_comment_id", request.CommentID) {
			return hook.TerminalAlreadyComplete, nil
		}
		return hook.TerminalCompleteConflict, nil
	}
	if stateValue != stateReportPending || !localTerminalReportMatches(binding.runRow, request.ReportSHA256, request.Report.Code) ||
		!binding.runRow.strEquals("terminal_lease_token", request.LeaseToken) {
		return hook.TerminalCompleteConflict, nil
	}
	startedAt, ok := binding.runRow.int64At("terminal_started_at")
	if !ok || request.CompletedAt.UnixMilli() < startedAt {
		return "", localFailure(hook.FailureRejected, "invalid_terminal_complete")
	}
	// Release the project's single pending slot; tolerate an already-absent
	// slot, refuse to release one naming a different run.
	pendingKey := makeKey("pending", binding.envelope.Snapshot.SpaceKey, strconv.FormatInt(binding.envelope.Snapshot.ProjectID, 10))
	pendingRow, err := txn.getItem(pendingKey)
	if err != nil {
		return "", localFailure(hook.FailureRetryable, "terminal_complete_write_failed")
	}
	if pendingRow != nil {
		if !pendingRow.strEquals("run_id", request.Report.AutomationRunID) {
			return hook.TerminalCompleteConflict, nil
		}
		if err := txn.deleteItem(pendingKey); err != nil {
			return "", localFailure(hook.FailureRetryable, "terminal_complete_write_failed")
		}
	}
	binding.runRow["state"] = stateTerminal
	binding.runRow["terminal_comment_id"] = request.CommentID
	binding.runRow["terminal_completed_at"] = request.CompletedAt.UnixMilli()
	delete(binding.runRow, "terminal_lease_token")
	delete(binding.runRow, "terminal_lease_until")
	if err := txn.setItem(binding.runKey, binding.runRow); err != nil {
		return "", localFailure(hook.FailureRetryable, "terminal_complete_write_failed")
	}
	if err := txn.commit(); err != nil {
		return "", localFailure(hook.FailureRetryable, "terminal_complete_write_failed")
	}
	return hook.TerminalCompleted, nil
}

// ---------------------------------------------------------------------------
// Question: BeginQuestion / CompleteQuestion / LoadQuestionWait
// (sibling of question.go)
// ---------------------------------------------------------------------------

func localQuestionBindingMatches(binding localTerminalBinding, record hook.QuestionRecord, route hook.ReportRouteConfig) bool {
	snapshot := binding.envelope.Snapshot
	return localRunItemMatches(binding.runRow, binding.envelope, binding.eventKey, binding.runKey) &&
		localEventItemMatches(binding.eventRow, binding.envelope, binding.runKey) &&
		localTerminalStateShapeValid(binding.runRow) &&
		snapshot.SpaceKey == route.SpaceKey && snapshot.ProjectID == route.ProjectID && snapshot.ProjectKey == route.ProjectKey &&
		snapshot.CreatorID == route.AllowedCreatorID && snapshot.ActivityType == route.AllowedActivityType &&
		snapshot.RunID == route.ExpectedRunID && snapshot.Target == route.Target &&
		record.DeliveryID == binding.envelope.DeliveryID && record.InputSHA256 == snapshot.InputSHA256 &&
		record.AutomationRunID == snapshot.RunID && record.RepositoryID == route.RepositoryID &&
		binding.runRow.int64Equals("repository_id", record.RepositoryID) &&
		binding.runRow.strEquals("repository_sha256", record.RepositorySHA256) &&
		binding.runRow.strEquals("workflow_ref_sha256", record.WorkflowRefSHA256) &&
		binding.runRow.strEquals("workflow_sha", record.WorkflowSHA) &&
		binding.runRow.int64Equals("workflow_run_id", record.WorkflowRunID) &&
		binding.runRow.int64Equals("run_attempt", int64(record.RunAttempt))
}

func (s *LocalStore) BeginQuestion(ctx context.Context, request hook.QuestionBeginRequest) (hook.TerminalBinding, hook.QuestionBeginDisposition, error) {
	if !validQuestionBeginRequest(request) {
		return hook.TerminalBinding{}, "", localFailure(hook.FailureRejected, "invalid_question_begin")
	}
	txn, err := s.begin(ctx)
	if err != nil {
		return hook.TerminalBinding{}, "", localFailure(hook.FailureRetryable, "terminal_read_failed")
	}
	defer txn.rollback()
	binding, err := txn.loadTerminalBinding(request.Record.AutomationRunID, request.Route)
	if err != nil {
		return hook.TerminalBinding{}, "", err
	}
	if !localQuestionBindingMatches(binding, request.Record, request.Route) {
		return hook.TerminalBinding{}, hook.QuestionBeginConflict, nil
	}
	result := hook.TerminalBinding{IssueID: binding.envelope.Snapshot.IssueID, IssueKey: binding.envelope.Snapshot.IssueKey}
	stateValue, _ := binding.runRow.str("state")
	switch stateValue {
	case stateAwaitingAnswer:
		if binding.runRow.strEquals("question_record_sha256", request.RecordSHA256) {
			if commentID, ok := binding.runRow.int64At("question_comment_id"); ok && commentID > 0 {
				return result, hook.QuestionBeginComplete, nil
			}
		}
		return result, hook.QuestionBeginConflict, nil
	case stateQuestionPending:
		if !binding.runRow.strEquals("question_record_sha256", request.RecordSHA256) {
			return result, hook.QuestionBeginConflict, nil
		}
		leaseUntil, ok := binding.runRow.int64At("question_lease_until")
		if !ok {
			return hook.TerminalBinding{}, "", localFailure(hook.FailureRejected, "question_binding_invalid")
		}
		if leaseUntil >= request.StartedAt.UnixMilli() {
			return result, hook.QuestionBeginBusy, nil
		}
		// reacquireQuestion: refresh the lease; started_at stays.
		binding.runRow["question_lease_until"] = request.LeaseUntil.UnixMilli()
		binding.runRow["question_lease_token"] = request.LeaseToken
		if err := txn.setItem(binding.runKey, binding.runRow); err != nil {
			return hook.TerminalBinding{}, "", localFailure(hook.FailureRetryable, "question_begin_write_failed")
		}
		if err := txn.commit(); err != nil {
			return hook.TerminalBinding{}, "", localFailure(hook.FailureRetryable, "question_begin_write_failed")
		}
		return result, hook.QuestionBeginAcquired, nil
	case stateClaimed:
		clarificationDigest, clarificationRounds, ok := localClarificationStateConsistent(binding.runRow)
		if !ok || !questionRevisionConsistent(request.Record, clarificationDigest, clarificationRounds) {
			return result, hook.QuestionBeginConflict, nil
		}
		// startQuestion: claimed -> question_report_pending. The claimed
		// shape guarantees no question or terminal evidence exists.
		binding.runRow["state"] = stateQuestionPending
		binding.runRow["question_record_sha256"] = request.RecordSHA256
		binding.runRow["question_record_json"] = request.RecordJSON
		binding.runRow["question_started_at"] = request.StartedAt.UnixMilli()
		binding.runRow["question_lease_until"] = request.LeaseUntil.UnixMilli()
		binding.runRow["question_lease_token"] = request.LeaseToken
		if err := txn.setItem(binding.runKey, binding.runRow); err != nil {
			return hook.TerminalBinding{}, "", localFailure(hook.FailureRetryable, "question_begin_write_failed")
		}
		if err := txn.commit(); err != nil {
			return hook.TerminalBinding{}, "", localFailure(hook.FailureRetryable, "question_begin_write_failed")
		}
		return result, hook.QuestionBeginAcquired, nil
	default:
		return result, hook.QuestionBeginConflict, nil
	}
}

func (s *LocalStore) CompleteQuestion(ctx context.Context, request hook.QuestionCompleteRequest) (hook.QuestionCompleteDisposition, error) {
	if !validQuestionCompleteRequest(request) {
		return "", localFailure(hook.FailureRejected, "invalid_question_complete")
	}
	txn, err := s.begin(ctx)
	if err != nil {
		return "", localFailure(hook.FailureRetryable, "terminal_read_failed")
	}
	defer txn.rollback()
	binding, err := txn.loadTerminalBinding(request.Record.AutomationRunID, request.Route)
	if err != nil {
		return "", err
	}
	if !localQuestionBindingMatches(binding, request.Record, request.Route) {
		return hook.QuestionCompleteConflict, nil
	}
	stateValue, _ := binding.runRow.str("state")
	if stateValue == stateAwaitingAnswer {
		if binding.runRow.strEquals("question_record_sha256", request.RecordSHA256) &&
			binding.runRow.int64Equals("question_comment_id", request.CommentID) {
			return hook.QuestionAlreadyComplete, nil
		}
		return hook.QuestionCompleteConflict, nil
	}
	if stateValue != stateQuestionPending || !binding.runRow.strEquals("question_record_sha256", request.RecordSHA256) ||
		!binding.runRow.strEquals("question_lease_token", request.LeaseToken) {
		return hook.QuestionCompleteConflict, nil
	}
	startedAt, ok := binding.runRow.int64At("question_started_at")
	if !ok || request.PostedAt.UnixMilli() < startedAt {
		return "", localFailure(hook.FailureRejected, "invalid_question_complete")
	}
	binding.runRow["state"] = stateAwaitingAnswer
	binding.runRow["question_comment_id"] = request.CommentID
	binding.runRow["question_posted_at"] = request.PostedAt.UnixMilli()
	delete(binding.runRow, "question_lease_token")
	delete(binding.runRow, "question_lease_until")
	if err := txn.setItem(binding.runKey, binding.runRow); err != nil {
		return "", localFailure(hook.FailureRetryable, "question_complete_write_failed")
	}
	if err := txn.commit(); err != nil {
		return "", localFailure(hook.FailureRetryable, "question_complete_write_failed")
	}
	return hook.QuestionCompleted, nil
}

func (s *LocalStore) LoadQuestionWait(ctx context.Context, route hook.ReportRouteConfig) (hook.QuestionWaitSnapshot, bool, error) {
	if route.Validate() != nil {
		return hook.QuestionWaitSnapshot{}, false, localFailure(hook.FailureRejected, "invalid_question_wait_route")
	}
	resolved, err := s.resolveRunRoute(ctx, route)
	if err != nil {
		return hook.QuestionWaitSnapshot{}, false, err
	}
	route = resolved
	txn, err := s.beginRead(ctx)
	if err != nil {
		return hook.QuestionWaitSnapshot{}, false, localFailure(hook.FailureRetryable, "terminal_read_failed")
	}
	defer txn.rollback()
	binding, err := txn.loadTerminalBinding(route.ExpectedRunID, route)
	if err != nil {
		if _, code := hook.FailureDetails(err); code == "terminal_binding_missing" {
			return hook.QuestionWaitSnapshot{}, false, nil
		}
		return hook.QuestionWaitSnapshot{}, false, err
	}
	stateValue, _ := binding.runRow.str("state")
	if stateValue != stateAwaitingAnswer && stateValue != stateQuestionPending {
		return hook.QuestionWaitSnapshot{}, false, nil
	}
	posting := stateValue == stateQuestionPending
	snapshot := binding.envelope.Snapshot
	if !localRunItemMatches(binding.runRow, binding.envelope, binding.eventKey, binding.runKey) ||
		!localEventItemMatches(binding.eventRow, binding.envelope, binding.runKey) ||
		!localTerminalStateShapeValid(binding.runRow) ||
		snapshot.SpaceKey != route.SpaceKey || snapshot.ProjectID != route.ProjectID || snapshot.ProjectKey != route.ProjectKey ||
		snapshot.CreatorID != route.AllowedCreatorID || snapshot.ActivityType != route.AllowedActivityType ||
		snapshot.RunID != route.ExpectedRunID || snapshot.Target != route.Target {
		return hook.QuestionWaitSnapshot{}, false, localFailure(hook.FailureRejected, "question_wait_binding_invalid")
	}
	recordJSON, _ := binding.runRow.str("question_record_json")
	recordDigest, _ := binding.runRow.str("question_record_sha256")
	record, recordOK := questionRecordJSONValid(recordJSON, recordDigest)
	if !recordOK || record.ValidateRoute(route) != nil {
		return hook.QuestionWaitSnapshot{}, false, localFailure(hook.FailureRejected, "question_wait_binding_invalid")
	}
	if _, _, ok := localClarificationStateConsistent(binding.runRow); !ok {
		return hook.QuestionWaitSnapshot{}, false, localFailure(hook.FailureRejected, "question_wait_binding_invalid")
	}
	commentID, commentOK := binding.runRow.int64At("question_comment_id")
	if !posting && (!commentOK || commentID <= 0) {
		return hook.QuestionWaitSnapshot{}, false, localFailure(hook.FailureRejected, "question_wait_binding_invalid")
	}
	if posting {
		commentID = 0
	}
	clarificationJSON, _ := binding.runRow.str("clarification_json")
	clarificationDigest, _ := binding.runRow.str("clarification_sha256")
	return hook.QuestionWaitSnapshot{
		Record: record, RecordJSON: recordJSON, RecordSHA256: recordDigest,
		QuestionCommentID: commentID, IssueID: snapshot.IssueID,
		ClarificationJSON: clarificationJSON, ClarificationSHA256: clarificationDigest,
		Posting: posting,
	}, true, nil
}

// ---------------------------------------------------------------------------
// Resume: ResumeWithAnswer (sibling of resume.go)
// ---------------------------------------------------------------------------

func localResumeBindingMatches(binding localTerminalBinding, record hook.ClarificationRecord, route hook.ReportRouteConfig) bool {
	snapshot := binding.envelope.Snapshot
	return localRunItemMatches(binding.runRow, binding.envelope, binding.eventKey, binding.runKey) &&
		localEventItemMatches(binding.eventRow, binding.envelope, binding.runKey) &&
		localTerminalStateShapeValid(binding.runRow) &&
		snapshot.SpaceKey == route.SpaceKey && snapshot.ProjectID == route.ProjectID && snapshot.ProjectKey == route.ProjectKey &&
		snapshot.CreatorID == route.AllowedCreatorID && snapshot.ActivityType == route.AllowedActivityType &&
		snapshot.RunID == route.ExpectedRunID && snapshot.Target == route.Target &&
		record.DeliveryID == binding.envelope.DeliveryID && record.InputSHA256 == snapshot.InputSHA256 &&
		record.AutomationRunID == snapshot.RunID && record.RepositoryID == route.RepositoryID
}

func (s *LocalStore) ResumeWithAnswer(ctx context.Context, request hook.ResumeRequest) (hook.ResumeDisposition, error) {
	request.Route.ExpectedRunID = request.Record.AutomationRunID
	if !validResumeRequest(request) {
		return "", localFailure(hook.FailureRejected, "invalid_resume_request")
	}
	txn, err := s.begin(ctx)
	if err != nil {
		return "", localFailure(hook.FailureRetryable, "terminal_read_failed")
	}
	defer txn.rollback()
	binding, err := txn.loadTerminalBinding(request.Record.AutomationRunID, request.Route)
	if err != nil {
		return "", err
	}
	if !localResumeBindingMatches(binding, request.Record, request.Route) {
		return hook.ResumeConflict, nil
	}
	if binding.runRow.strEquals("clarification_sha256", request.RecordSHA256) {
		return hook.ResumeAlreadyComplete, nil
	}
	stateValue, _ := binding.runRow.str("state")
	if stateValue != stateAwaitingAnswer || !localResumeSourceMatchesForRequest(binding.runRow, request) {
		return hook.ResumeConflict, nil
	}
	// startResume: archive the sealed record, return the run to queued.
	archiveKey := clarificationArchiveKey(binding.runKey, request.Record.InputRevision)
	archive := item{
		"pk": archiveKey, "record_type": "clarification_revision", "run_key": binding.runKey,
		"input_revision": request.Record.InputRevision, "record_sha256": request.RecordSHA256,
		"record_json": request.RecordJSON, "resumed_at": request.ResumedAt.UnixMilli(),
	}
	if startedAt, ok := binding.runRow.int64At("question_started_at"); ok {
		archive["question_started_at"] = startedAt
	}
	if postedAt, ok := binding.runRow.int64At("question_posted_at"); ok {
		archive["question_posted_at"] = postedAt
	}
	created, err := txn.putNew(archiveKey, archive)
	if err != nil || !created {
		return "", localFailure(hook.FailureRetryable, "resume_write_failed")
	}
	binding.runRow["state"] = stateQueued
	binding.runRow["queued_at"] = request.ResumedAt.UnixMilli()
	binding.runRow["clarification_sha256"] = request.RecordSHA256
	binding.runRow["clarification_json"] = request.RecordJSON
	binding.runRow["input_revision"] = request.Record.InputRevision
	binding.runRow["resumed_at"] = request.ResumedAt.UnixMilli()
	for _, name := range []string{
		"question_record_sha256", "question_record_json", "question_started_at",
		"question_comment_id", "question_posted_at", "claimed_at",
		"repository_id", "repository_sha256", "workflow_ref_sha256",
		"workflow_sha", "workflow_run_id", "run_attempt",
	} {
		delete(binding.runRow, name)
	}
	if err := txn.setItem(binding.runKey, binding.runRow); err != nil {
		return "", localFailure(hook.FailureRetryable, "resume_write_failed")
	}
	if err := txn.commit(); err != nil {
		return "", localFailure(hook.FailureRetryable, "resume_write_failed")
	}
	return hook.ResumeCompleted, nil
}

// localResumeSourceMatchesForRequest mirrors resumeSourceMatches: the run row
// must still carry the exact question of the record's final round, and the
// prior clarification chain must line up with the request's previous record.
func localResumeSourceMatchesForRequest(row item, request hook.ResumeRequest) bool {
	record := request.Record
	lastRound := record.Rounds[len(record.Rounds)-1]
	question, err := hook.DecodeQuestionRecord([]byte(lastRound.QuestionRecordJSON))
	if err != nil {
		return false
	}
	if !(row.strEquals("question_record_sha256", lastRound.QuestionRecordSHA256) &&
		row.int64Equals("question_comment_id", lastRound.QuestionCommentID) &&
		row.int64Equals("repository_id", record.RepositoryID) &&
		row.strEquals("repository_sha256", record.RepositorySHA256) &&
		row.strEquals("workflow_ref_sha256", record.WorkflowRefSHA256) &&
		row.strEquals("workflow_sha", question.WorkflowSHA) &&
		row.int64Equals("workflow_run_id", question.WorkflowRunID) &&
		row.int64Equals("run_attempt", int64(question.RunAttempt))) {
		return false
	}
	if record.InputRevision == 2 {
		_, exists := row.str("clarification_sha256")
		return !exists
	}
	return row.strEquals("clarification_sha256", request.PreviousRecordSHA256)
}

// ---------------------------------------------------------------------------
// Reply / Notify / RunComment markers + ingest cursor
// (siblings of reply.go, notify.go, runcomment.go)
// ---------------------------------------------------------------------------

func localNotifyRunStillWaiting(row item, recordDigest string) bool {
	commentID, ok := row.int64At("question_comment_id")
	return row.strEquals("state", stateAwaitingAnswer) &&
		row.strEquals("question_record_sha256", recordDigest) &&
		ok && commentID > 0
}

func localReplyMarkerMatches(marker item, runKey, recordDigest string, kind hook.ReplyKind) bool {
	return marker.strEquals("record_type", "question_reply") &&
		marker.strEquals("run_key", runKey) &&
		marker.strEquals("question_record_sha256", recordDigest) &&
		marker.strEquals("reply_kind", string(kind))
}

func (s *LocalStore) BeginReply(ctx context.Context, request hook.ReplyBeginRequest) (hook.TerminalBinding, hook.ReplyBeginDisposition, error) {
	if !validReplyBeginRequest(request) {
		return hook.TerminalBinding{}, "", localFailure(hook.FailureRejected, "invalid_reply_begin")
	}
	txn, err := s.begin(ctx)
	if err != nil {
		return hook.TerminalBinding{}, "", localFailure(hook.FailureRetryable, "terminal_read_failed")
	}
	defer txn.rollback()
	binding, err := txn.loadTerminalBinding(request.Record.AutomationRunID, request.Route)
	if err != nil {
		return hook.TerminalBinding{}, "", err
	}
	if !localQuestionBindingMatches(binding, request.Record, request.Route) {
		return hook.TerminalBinding{}, hook.ReplyBeginConflict, nil
	}
	result := hook.TerminalBinding{IssueID: binding.envelope.Snapshot.IssueID, IssueKey: binding.envelope.Snapshot.IssueKey}
	markerKey := replyMarkerKey(binding.runKey, request.Record.QuestionRevision, request.Kind, request.TriggerCommentID)
	marker, err := txn.getItem(markerKey)
	if err != nil {
		return hook.TerminalBinding{}, "", localFailure(hook.FailureRetryable, "notify_read_failed")
	}
	if marker != nil {
		if !localReplyMarkerMatches(marker, binding.runKey, request.RecordSHA256, request.Kind) {
			return result, hook.ReplyBeginConflict, nil
		}
		if commentID, ok := marker.int64At("reply_comment_id"); ok && commentID > 0 {
			return result, hook.ReplyBeginComplete, nil
		}
		if !localNotifyRunStillWaiting(binding.runRow, request.RecordSHA256) {
			return result, hook.ReplyBeginConflict, nil
		}
		leaseUntil, ok := marker.int64At("reply_lease_until")
		if !ok {
			return hook.TerminalBinding{}, "", localFailure(hook.FailureRejected, "reply_marker_invalid")
		}
		if leaseUntil >= request.StartedAt.UnixMilli() {
			return result, hook.ReplyBeginBusy, nil
		}
		// reacquireReply: refresh lease, overwrite content and trigger.
		marker["reply_lease_until"] = request.LeaseUntil.UnixMilli()
		marker["reply_lease_token"] = request.LeaseToken
		marker["content_sha256"] = request.ContentSHA256
		marker["trigger_comment_id"] = request.TriggerCommentID
		if err := txn.setItem(markerKey, marker); err != nil {
			return hook.TerminalBinding{}, "", localFailure(hook.FailureRetryable, "reply_begin_write_failed")
		}
		if err := txn.commit(); err != nil {
			return hook.TerminalBinding{}, "", localFailure(hook.FailureRetryable, "reply_begin_write_failed")
		}
		return result, hook.ReplyBeginAcquired, nil
	}
	if !localNotifyRunStillWaiting(binding.runRow, request.RecordSHA256) {
		return result, hook.ReplyBeginConflict, nil
	}
	created, err := txn.putNew(markerKey, item{
		"pk": markerKey, "record_type": "question_reply", "run_key": binding.runKey,
		"question_record_sha256": request.RecordSHA256, "reply_kind": string(request.Kind),
		"trigger_comment_id": request.TriggerCommentID, "content_sha256": request.ContentSHA256,
		"reply_started_at": request.StartedAt.UnixMilli(),
		"reply_lease_until": request.LeaseUntil.UnixMilli(), "reply_lease_token": request.LeaseToken,
	})
	if err != nil || !created {
		return hook.TerminalBinding{}, "", localFailure(hook.FailureRetryable, "reply_begin_write_failed")
	}
	if err := txn.commit(); err != nil {
		return hook.TerminalBinding{}, "", localFailure(hook.FailureRetryable, "reply_begin_write_failed")
	}
	return result, hook.ReplyBeginAcquired, nil
}

func (s *LocalStore) CompleteReply(ctx context.Context, request hook.ReplyCompleteRequest) (hook.ReplyCompleteDisposition, error) {
	if !validReplyCompleteRequest(request) {
		return "", localFailure(hook.FailureRejected, "invalid_reply_complete")
	}
	txn, err := s.begin(ctx)
	if err != nil {
		return "", localFailure(hook.FailureRetryable, "notify_read_failed")
	}
	defer txn.rollback()
	runKey := makeKey("run", request.Route.SpaceKey, strconv.FormatInt(request.Route.ProjectID, 10), request.Record.AutomationRunID)
	markerKey := replyMarkerKey(runKey, request.Record.QuestionRevision, request.Kind, request.TriggerCommentID)
	marker, err := txn.getItem(markerKey)
	if err != nil {
		return "", localFailure(hook.FailureRetryable, "notify_read_failed")
	}
	if marker == nil || !localReplyMarkerMatches(marker, runKey, request.RecordSHA256, request.Kind) {
		return hook.ReplyCompleteConflict, nil
	}
	if commentID, ok := marker.int64At("reply_comment_id"); ok {
		if commentID == request.CommentID {
			return hook.ReplyAlreadyComplete, nil
		}
		return hook.ReplyCompleteConflict, nil
	}
	if !marker.strEquals("reply_lease_token", request.LeaseToken) || !marker.strEquals("content_sha256", request.ContentSHA256) {
		return hook.ReplyCompleteConflict, nil
	}
	startedAt, ok := marker.int64At("reply_started_at")
	if !ok || request.PostedAt.UnixMilli() < startedAt {
		return "", localFailure(hook.FailureRejected, "invalid_reply_complete")
	}
	marker["reply_comment_id"] = request.CommentID
	marker["reply_posted_at"] = request.PostedAt.UnixMilli()
	delete(marker, "reply_lease_until")
	delete(marker, "reply_lease_token")
	if err := txn.setItem(markerKey, marker); err != nil {
		return "", localFailure(hook.FailureRetryable, "reply_complete_write_failed")
	}
	if err := txn.commit(); err != nil {
		return "", localFailure(hook.FailureRetryable, "reply_complete_write_failed")
	}
	return hook.ReplyCompleted, nil
}

func (s *LocalStore) ReplyState(ctx context.Context, route hook.ReportRouteConfig, record hook.QuestionRecord, kind hook.ReplyKind, triggerCommentID int64) (bool, error) {
	if route.Validate() != nil || record.ValidateShape() != nil || !validReplyKind(kind) {
		return false, localFailure(hook.FailureRejected, "invalid_reply_state")
	}
	txn, err := s.beginRead(ctx)
	if err != nil {
		return false, localFailure(hook.FailureRetryable, "notify_read_failed")
	}
	defer txn.rollback()
	runKey := makeKey("run", route.SpaceKey, strconv.FormatInt(route.ProjectID, 10), record.AutomationRunID)
	marker, err := txn.getItem(replyMarkerKey(runKey, record.QuestionRevision, kind, triggerCommentID))
	if err != nil {
		return false, localFailure(hook.FailureRetryable, "notify_read_failed")
	}
	commentID, ok := marker.int64At("reply_comment_id")
	return ok && commentID > 0, nil
}

func localNotifyMarkerMatches(marker item, runKey, recordDigest string, index int) bool {
	return marker.strEquals("record_type", "question_notify") &&
		marker.strEquals("run_key", runKey) &&
		marker.strEquals("question_record_sha256", recordDigest) &&
		marker.int64Equals("notify_index", int64(index))
}

func (s *LocalStore) BeginNotify(ctx context.Context, request hook.NotifyBeginRequest) (hook.TerminalBinding, hook.NotifyBeginDisposition, error) {
	if !validNotifyBeginRequest(request) {
		return hook.TerminalBinding{}, "", localFailure(hook.FailureRejected, "invalid_notify_begin")
	}
	txn, err := s.begin(ctx)
	if err != nil {
		return hook.TerminalBinding{}, "", localFailure(hook.FailureRetryable, "terminal_read_failed")
	}
	defer txn.rollback()
	binding, err := txn.loadTerminalBinding(request.Record.AutomationRunID, request.Route)
	if err != nil {
		return hook.TerminalBinding{}, "", err
	}
	if !localQuestionBindingMatches(binding, request.Record, request.Route) {
		return hook.TerminalBinding{}, hook.NotifyBeginConflict, nil
	}
	result := hook.TerminalBinding{IssueID: binding.envelope.Snapshot.IssueID, IssueKey: binding.envelope.Snapshot.IssueKey}
	markerKey := notifyMarkerKey(binding.runKey, request.Record.QuestionRevision, request.Index)
	marker, err := txn.getItem(markerKey)
	if err != nil {
		return hook.TerminalBinding{}, "", localFailure(hook.FailureRetryable, "notify_read_failed")
	}
	if marker != nil {
		if !localNotifyMarkerMatches(marker, binding.runKey, request.RecordSHA256, request.Index) {
			return result, hook.NotifyBeginConflict, nil
		}
		if commentID, ok := marker.int64At("notify_comment_id"); ok && commentID > 0 {
			return result, hook.NotifyBeginComplete, nil
		}
		if !localNotifyRunStillWaiting(binding.runRow, request.RecordSHA256) {
			return result, hook.NotifyBeginConflict, nil
		}
		leaseUntil, ok := marker.int64At("notify_lease_until")
		if !ok {
			return hook.TerminalBinding{}, "", localFailure(hook.FailureRejected, "notify_marker_invalid")
		}
		if leaseUntil >= request.StartedAt.UnixMilli() {
			return result, hook.NotifyBeginBusy, nil
		}
		marker["notify_lease_until"] = request.LeaseUntil.UnixMilli()
		marker["notify_lease_token"] = request.LeaseToken
		if err := txn.setItem(markerKey, marker); err != nil {
			return hook.TerminalBinding{}, "", localFailure(hook.FailureRetryable, "notify_begin_write_failed")
		}
		if err := txn.commit(); err != nil {
			return hook.TerminalBinding{}, "", localFailure(hook.FailureRetryable, "notify_begin_write_failed")
		}
		return result, hook.NotifyBeginAcquired, nil
	}
	if !localNotifyRunStillWaiting(binding.runRow, request.RecordSHA256) {
		return result, hook.NotifyBeginConflict, nil
	}
	created, err := txn.putNew(markerKey, item{
		"pk": markerKey, "record_type": "question_notify", "run_key": binding.runKey,
		"question_record_sha256": request.RecordSHA256,
		"question_revision":      request.Record.QuestionRevision,
		"notify_index":           request.Index,
		"notify_at":              request.Record.NotifyAt[request.Index-1],
		"notify_started_at":      request.StartedAt.UnixMilli(),
		"notify_lease_until":     request.LeaseUntil.UnixMilli(),
		"notify_lease_token":     request.LeaseToken,
	})
	if err != nil || !created {
		return hook.TerminalBinding{}, "", localFailure(hook.FailureRetryable, "notify_begin_write_failed")
	}
	if err := txn.commit(); err != nil {
		return hook.TerminalBinding{}, "", localFailure(hook.FailureRetryable, "notify_begin_write_failed")
	}
	return result, hook.NotifyBeginAcquired, nil
}

func (s *LocalStore) CompleteNotify(ctx context.Context, request hook.NotifyCompleteRequest) (hook.NotifyCompleteDisposition, error) {
	if !validNotifyCompleteRequest(request) {
		return "", localFailure(hook.FailureRejected, "invalid_notify_complete")
	}
	txn, err := s.begin(ctx)
	if err != nil {
		return "", localFailure(hook.FailureRetryable, "notify_read_failed")
	}
	defer txn.rollback()
	runKey := makeKey("run", request.Route.SpaceKey, strconv.FormatInt(request.Route.ProjectID, 10), request.Record.AutomationRunID)
	markerKey := notifyMarkerKey(runKey, request.Record.QuestionRevision, request.Index)
	marker, err := txn.getItem(markerKey)
	if err != nil {
		return "", localFailure(hook.FailureRetryable, "notify_read_failed")
	}
	if marker == nil || !localNotifyMarkerMatches(marker, runKey, request.RecordSHA256, request.Index) {
		return hook.NotifyCompleteConflict, nil
	}
	if commentID, ok := marker.int64At("notify_comment_id"); ok {
		if commentID == request.CommentID {
			return hook.NotifyAlreadyComplete, nil
		}
		return hook.NotifyCompleteConflict, nil
	}
	if !marker.strEquals("notify_lease_token", request.LeaseToken) {
		return hook.NotifyCompleteConflict, nil
	}
	startedAt, ok := marker.int64At("notify_started_at")
	if !ok || request.PostedAt.UnixMilli() < startedAt {
		return "", localFailure(hook.FailureRejected, "invalid_notify_complete")
	}
	marker["notify_comment_id"] = request.CommentID
	marker["notify_posted_at"] = request.PostedAt.UnixMilli()
	delete(marker, "notify_lease_until")
	delete(marker, "notify_lease_token")
	if err := txn.setItem(markerKey, marker); err != nil {
		return "", localFailure(hook.FailureRetryable, "notify_complete_write_failed")
	}
	if err := txn.commit(); err != nil {
		return "", localFailure(hook.FailureRetryable, "notify_complete_write_failed")
	}
	return hook.NotifyCompleted, nil
}

func localRunCommentBindingMatches(binding localTerminalBinding, route hook.ReportRouteConfig) bool {
	snapshot := binding.envelope.Snapshot
	return localRunItemMatches(binding.runRow, binding.envelope, binding.eventKey, binding.runKey) &&
		localEventItemMatches(binding.eventRow, binding.envelope, binding.runKey) &&
		localTerminalStateShapeValid(binding.runRow) &&
		snapshot.SpaceKey == route.SpaceKey && snapshot.ProjectID == route.ProjectID && snapshot.ProjectKey == route.ProjectKey &&
		snapshot.CreatorID == route.AllowedCreatorID && snapshot.ActivityType == route.AllowedActivityType &&
		snapshot.RunID == route.ExpectedRunID && snapshot.Target == route.Target
}

func localRunCommentMarkerMatches(marker item, runKey string, kind hook.RunCommentKind) bool {
	return marker.strEquals("record_type", "run_comment") &&
		marker.strEquals("run_key", runKey) &&
		marker.strEquals("reply_kind", string(kind))
}

func (s *LocalStore) BeginRunComment(ctx context.Context, request hook.RunCommentBeginRequest) (hook.TerminalBinding, hook.ReplyBeginDisposition, error) {
	if !validRunCommentBeginRequest(request) {
		return hook.TerminalBinding{}, "", localFailure(hook.FailureRejected, "invalid_run_comment_begin")
	}
	route, err := s.resolveRunRoute(ctx, request.Route)
	if err != nil {
		return hook.TerminalBinding{}, "", err
	}
	request.Route = route
	txn, err := s.begin(ctx)
	if err != nil {
		return hook.TerminalBinding{}, "", localFailure(hook.FailureRetryable, "terminal_read_failed")
	}
	defer txn.rollback()
	binding, err := txn.loadTerminalBinding(request.Route.ExpectedRunID, request.Route)
	if err != nil {
		return hook.TerminalBinding{}, "", err
	}
	if !localRunCommentBindingMatches(binding, request.Route) {
		return hook.TerminalBinding{}, hook.ReplyBeginConflict, nil
	}
	result := hook.TerminalBinding{IssueID: binding.envelope.Snapshot.IssueID, IssueKey: binding.envelope.Snapshot.IssueKey}
	markerKey := runCommentMarkerKey(binding.runKey, request.Kind, request.Qualifier)
	marker, err := txn.getItem(markerKey)
	if err != nil {
		return hook.TerminalBinding{}, "", localFailure(hook.FailureRetryable, "notify_read_failed")
	}
	if marker != nil {
		if !localRunCommentMarkerMatches(marker, binding.runKey, request.Kind) {
			return result, hook.ReplyBeginConflict, nil
		}
		if commentID, ok := marker.int64At("reply_comment_id"); ok && commentID > 0 {
			return result, hook.ReplyBeginComplete, nil
		}
		leaseUntil, ok := marker.int64At("reply_lease_until")
		if !ok {
			return hook.TerminalBinding{}, "", localFailure(hook.FailureRejected, "run_comment_marker_invalid")
		}
		if leaseUntil >= request.StartedAt.UnixMilli() {
			return result, hook.ReplyBeginBusy, nil
		}
		marker["reply_lease_until"] = request.LeaseUntil.UnixMilli()
		marker["reply_lease_token"] = request.LeaseToken
		marker["content_sha256"] = request.ContentSHA256
		if err := txn.setItem(markerKey, marker); err != nil {
			return hook.TerminalBinding{}, "", localFailure(hook.FailureRetryable, "run_comment_write_failed")
		}
		if err := txn.commit(); err != nil {
			return hook.TerminalBinding{}, "", localFailure(hook.FailureRetryable, "run_comment_write_failed")
		}
		return result, hook.ReplyBeginAcquired, nil
	}
	attrs := item{
		"pk": markerKey, "record_type": "run_comment", "run_key": binding.runKey,
		"reply_kind": string(request.Kind), "content_sha256": request.ContentSHA256,
		"reply_started_at":  request.StartedAt.UnixMilli(),
		"reply_lease_until": request.LeaseUntil.UnixMilli(), "reply_lease_token": request.LeaseToken,
	}
	if request.Qualifier != "" {
		attrs["qualifier"] = request.Qualifier
	}
	created, err := txn.putNew(markerKey, attrs)
	if err != nil || !created {
		return hook.TerminalBinding{}, "", localFailure(hook.FailureRetryable, "run_comment_write_failed")
	}
	if err := txn.commit(); err != nil {
		return hook.TerminalBinding{}, "", localFailure(hook.FailureRetryable, "run_comment_write_failed")
	}
	return result, hook.ReplyBeginAcquired, nil
}

func (s *LocalStore) CompleteRunComment(ctx context.Context, request hook.RunCommentCompleteRequest) (hook.ReplyCompleteDisposition, error) {
	if !validRunCommentCompleteRequest(request) {
		return "", localFailure(hook.FailureRejected, "invalid_run_comment_complete")
	}
	route, err := s.resolveRunRoute(ctx, request.Route)
	if err != nil {
		return "", err
	}
	request.Route = route
	txn, err := s.begin(ctx)
	if err != nil {
		return "", localFailure(hook.FailureRetryable, "notify_read_failed")
	}
	defer txn.rollback()
	runKey := makeKey("run", request.Route.SpaceKey, strconv.FormatInt(request.Route.ProjectID, 10), request.Route.ExpectedRunID)
	markerKey := runCommentMarkerKey(runKey, request.Kind, request.Qualifier)
	marker, err := txn.getItem(markerKey)
	if err != nil {
		return "", localFailure(hook.FailureRetryable, "notify_read_failed")
	}
	if marker == nil || !localRunCommentMarkerMatches(marker, runKey, request.Kind) {
		return hook.ReplyCompleteConflict, nil
	}
	if commentID, ok := marker.int64At("reply_comment_id"); ok {
		if commentID == request.CommentID {
			return hook.ReplyAlreadyComplete, nil
		}
		return hook.ReplyCompleteConflict, nil
	}
	if !marker.strEquals("reply_lease_token", request.LeaseToken) || !marker.strEquals("content_sha256", request.ContentSHA256) {
		return hook.ReplyCompleteConflict, nil
	}
	startedAt, ok := marker.int64At("reply_started_at")
	if !ok || request.PostedAt.UnixMilli() < startedAt {
		return "", localFailure(hook.FailureRejected, "invalid_run_comment_complete")
	}
	marker["reply_comment_id"] = request.CommentID
	marker["reply_posted_at"] = request.PostedAt.UnixMilli()
	delete(marker, "reply_lease_until")
	delete(marker, "reply_lease_token")
	if err := txn.setItem(markerKey, marker); err != nil {
		return "", localFailure(hook.FailureRetryable, "run_comment_write_failed")
	}
	if err := txn.commit(); err != nil {
		return "", localFailure(hook.FailureRetryable, "run_comment_write_failed")
	}
	return hook.ReplyCompleted, nil
}

func (s *LocalStore) RunCommentState(ctx context.Context, route hook.ReportRouteConfig, kind hook.RunCommentKind, qualifier string) (bool, error) {
	if route.Validate() != nil || !validRunCommentKind(kind) {
		return false, localFailure(hook.FailureRejected, "invalid_run_comment_state")
	}
	resolved, err := s.resolveRunRoute(ctx, route)
	if err != nil {
		return false, err
	}
	route = resolved
	txn, err := s.beginRead(ctx)
	if err != nil {
		return false, localFailure(hook.FailureRetryable, "notify_read_failed")
	}
	defer txn.rollback()
	runKey := makeKey("run", route.SpaceKey, strconv.FormatInt(route.ProjectID, 10), route.ExpectedRunID)
	marker, err := txn.getItem(runCommentMarkerKey(runKey, kind, qualifier))
	if err != nil {
		return false, localFailure(hook.FailureRetryable, "notify_read_failed")
	}
	commentID, ok := marker.int64At("reply_comment_id")
	return ok && commentID > 0, nil
}

func (s *LocalStore) LoadRunNotice(ctx context.Context, route hook.ReportRouteConfig) (hook.RunNoticeSnapshot, error) {
	if route.Validate() != nil {
		return hook.RunNoticeSnapshot{}, localFailure(hook.FailureRejected, "invalid_run_notice_route")
	}
	resolved, err := s.resolveRunRoute(ctx, route)
	if err != nil {
		return hook.RunNoticeSnapshot{}, err
	}
	route = resolved
	txn, err := s.beginRead(ctx)
	if err != nil {
		return hook.RunNoticeSnapshot{}, localFailure(hook.FailureRetryable, "terminal_read_failed")
	}
	defer txn.rollback()
	binding, err := txn.loadTerminalBinding(route.ExpectedRunID, route)
	if err != nil {
		if _, code := hook.FailureDetails(err); code == "terminal_binding_missing" {
			return hook.RunNoticeSnapshot{}, nil
		}
		return hook.RunNoticeSnapshot{}, err
	}
	if !localRunCommentBindingMatches(binding, route) {
		return hook.RunNoticeSnapshot{}, localFailure(hook.FailureRejected, "run_notice_binding_invalid")
	}
	if _, _, ok := localClarificationStateConsistent(binding.runRow); !ok {
		return hook.RunNoticeSnapshot{}, localFailure(hook.FailureRejected, "run_notice_binding_invalid")
	}
	stateValue, _ := binding.runRow.str("state")
	clarificationJSON, _ := binding.runRow.str("clarification_json")
	return hook.RunNoticeSnapshot{
		Exists:            true,
		Terminal:          stateValue == stateTerminal || stateValue == stateReportPending,
		IssueID:           binding.envelope.Snapshot.IssueID,
		Snapshot:          binding.envelope.Snapshot,
		ClarificationJSON: clarificationJSON,
	}, nil
}

func (s *LocalStore) LoadIngestCursor(ctx context.Context, route hook.ReportRouteConfig) (int64, error) {
	if route.Validate() != nil {
		return 0, localFailure(hook.FailureRejected, "invalid_ingest_cursor")
	}
	txn, err := s.beginRead(ctx)
	if err != nil {
		return 0, localFailure(hook.FailureRetryable, "notify_read_failed")
	}
	defer txn.rollback()
	row, err := txn.getItem(ingestCursorKey(route))
	if err != nil {
		return 0, localFailure(hook.FailureRetryable, "notify_read_failed")
	}
	if row == nil {
		return 0, nil
	}
	value, ok := row.int64At("last_activity_id")
	if !ok || value < 0 {
		return 0, localFailure(hook.FailureRejected, "ingest_cursor_invalid")
	}
	return value, nil
}

func (s *LocalStore) StoreIngestCursor(ctx context.Context, route hook.ReportRouteConfig, activityID int64) error {
	if route.Validate() != nil || activityID <= 0 {
		return localFailure(hook.FailureRejected, "invalid_ingest_cursor")
	}
	txn, err := s.begin(ctx)
	if err != nil {
		return localFailure(hook.FailureRetryable, "ingest_cursor_write_failed")
	}
	defer txn.rollback()
	key := ingestCursorKey(route)
	row, err := txn.getItem(key)
	if err != nil {
		return localFailure(hook.FailureRetryable, "ingest_cursor_write_failed")
	}
	if row != nil {
		if current, ok := row.int64At("last_activity_id"); ok && current >= activityID {
			// A concurrent tick already advanced past this point — success,
			// exactly like the Dynamo store's conditional-cancel tolerance.
			return nil
		}
	}
	if err := txn.setItem(key, item{
		"pk": key, "record_type": "ingest_cursor", "last_activity_id": activityID,
	}); err != nil {
		return localFailure(hook.FailureRetryable, "ingest_cursor_write_failed")
	}
	if err := txn.commit(); err != nil {
		return localFailure(hook.FailureRetryable, "ingest_cursor_write_failed")
	}
	return nil
}

// Compile-time proof that the local store satisfies every ledger interface
// the hook package consumes — the exact same set DynamoStore satisfies.
var (
	_ hook.QueueStore          = (*LocalStore)(nil)
	_ hook.TerminalReportStore = (*LocalStore)(nil)
	_ hook.QuestionStore       = (*LocalStore)(nil)
	_ hook.QuestionWaitStore   = (*LocalStore)(nil)
	_ hook.NotifyStore         = (*LocalStore)(nil)
	_ hook.ReplyStore          = (*LocalStore)(nil)
	_ hook.RunCommentStore     = (*LocalStore)(nil)
	_ hook.ResumeStore         = (*LocalStore)(nil)
	_ hook.RunNoticeStore      = (*LocalStore)(nil)
	_ hook.IngestCursorStore   = (*LocalStore)(nil)
	_ hook.QuestionTickStore   = (*LocalStore)(nil)
)

// RunOverview is one live run row, read for the attendant's card sync. A
// read-only convenience over the same rows the sealed operations manage.
type RunOverview struct {
	Key        string // the ledger row key, for administrative transitions
	RunID      string
	DeliveryID string
	State      string
	ClaimedAt  int64 // ms; 0 unless the run holds (or held) a claim
	// QuestionSealed marks a run carrying sealed question evidence — its
	// report_pending flavor belongs to the tick's expiry pass, never to
	// claim recovery.
	QuestionSealed bool
	IssueID    int64
	IssueKey   string
	Summary    string
}

// ScanRuns lists every run row in the ledger. Read-only; the attendant uses
// it to keep Hermes cards aligned with ledger states.
func (s *LocalStore) ScanRuns(ctx context.Context) ([]RunOverview, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT pk, attrs FROM ledger WHERE pk LIKE 'run#%'")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	overview := []RunOverview{}
	for rows.Next() {
		var pk, encoded string
		if err := rows.Scan(&pk, &encoded); err != nil {
			return nil, err
		}
		if strings.Contains(pk, "#clarification#") || strings.Contains(pk, "#reply#") ||
			strings.Contains(pk, "#notify#") || strings.Contains(pk, "#comment#") {
			continue
		}
		row := item{}
		if err := json.Unmarshal([]byte(encoded), &row); err != nil {
			continue
		}
		if !row.strEquals("record_type", "run") {
			continue
		}
		entry := RunOverview{Key: pk}
		entry.RunID, _ = row.str("run_id")
		entry.DeliveryID, _ = row.str("delivery_id")
		entry.State, _ = row.str("state")
		entry.ClaimedAt, _ = row.int64At("claimed_at")
		entry.QuestionSealed = row.has("question_record_sha256")
		if envelopeJSON, ok := row.str("envelope_json"); ok {
			if envelope, err := decodeEnvelope([]byte(envelopeJSON)); err == nil {
				entry.IssueID = envelope.Snapshot.IssueID
				entry.IssueKey = envelope.Snapshot.IssueKey
				entry.Summary = envelope.Snapshot.Untrusted.Summary
			}
		}
		overview = append(overview, entry)
	}
	return overview, rows.Err()
}

// RecoverLostClaim returns a claimed run whose worker provably died to the
// queue so a fresh dispatch can claim it. Neither store ever expired a
// claim on its own — the workflow constitution recovered crashed claims by
// operator surgery (measured live 2026-08-19 on the retiring instance) —
// and this administrative transition is the pod constitution's structural
// replacement: the attendant calls it only after the kanban shows no
// living worker for the card. The expected claim timestamp binds the
// transition to the observed dead claim, so a run freshly re-claimed in
// the meantime cannot be stomped. Everything sealed at enqueue (envelope,
// event binding, pending slot) is untouched; the stale owner block stays
// and is overwritten whole by the next claim.
func (s *LocalStore) RecoverLostClaim(ctx context.Context, key string, expectClaimedAt int64, issuedAt time.Time) error {
	transaction, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer transaction.rollback()
	row, err := transaction.getItem(key)
	if err != nil {
		return err
	}
	if row == nil || !row.strEquals("record_type", "run") {
		return errors.New("recover: no such run")
	}
	switch {
	case row.strEquals("state", "queued"):
		// Already recovered (idempotent replay).
	case row.strEquals("state", "claimed") && row.int64Equals("claimed_at", expectClaimedAt):
	case row.strEquals("state", stateReportPending) && row.int64Equals("claimed_at", expectClaimedAt) &&
		!row.has("question_record_sha256"):
		// report_pending reached from claimed is a runner that died between
		// the two phases of its own terminal report — the report content
		// lived only in that process, so the run can only be re-executed.
		// (report_pending reached from awaiting_answer keeps the sealed
		// question evidence and is re-driven by the tick's own expiry pass,
		// which regenerates the identical report; it is refused here.)
		if lease, ok := row.int64At("terminal_lease_until"); !ok || lease >= issuedAt.UTC().UnixMilli() {
			return errors.New("recover: terminal lease still live")
		}
		delete(row, "terminal_report_sha256")
		delete(row, "terminal_code")
		delete(row, "terminal_started_at")
		delete(row, "terminal_lease_until")
		delete(row, "terminal_lease_token")
	default:
		return errors.New("recover: run is not the observed dead claim")
	}
	if !row.strEquals("state", "queued") {
		row["state"] = "queued"
		row["queued_at"] = issuedAt.UTC().UnixMilli()
		delete(row, "claimed_at")
		if err := transaction.setItem(key, row); err != nil {
			return err
		}
	}
	return transaction.commit()
}
