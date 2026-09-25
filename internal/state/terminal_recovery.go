package state

import (
	"context"
	"strconv"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
)

// CloseUnreproducibleTerminal closes a terminal report that was begun but
// whose sealed record can no longer be rebuilt, once the engine has posted
// the comment that stands in for it.
//
// Every other way out of report_pending goes through the report itself:
// CompleteTerminal will only close a row against a report whose digest is
// the one the row was begun with, which is exactly what a run with no kept
// comment and no cards cannot produce. Such a row used to stay pending for
// ever — and a pending row holds the project's one slot, so nothing else
// could be enqueued behind it either.
//
// This is the narrow way past that, and it is narrow on purpose:
//
//   - the row must be in report_pending with the digest and code the
//     caller read from it, so this can never close a different ending than
//     the one it reported;
//   - its lease must have expired, so no live writer is mid-report;
//   - a row carrying sealed question evidence is refused, because its
//     ending belongs to the reception tick, which regenerates the same
//     report rather than losing it;
//   - a comment id is required, so the row cannot be closed before the
//     requester was actually told.
//
// The digest stays on the row. What changes is that the row now also names
// the comment that reported it, which is what every reader of a closed run
// looks for.
func (s *LocalStore) CloseUnreproducibleTerminal(ctx context.Context, request hook.TerminalRecoveryCloseRequest) (hook.TerminalCompleteDisposition, error) {
	if !validTerminalRecoveryClose(request) {
		return "", localFailure(hook.FailureRejected, "invalid_terminal_recovery_close")
	}
	txn, err := s.begin(ctx)
	if err != nil {
		return "", localFailure(hook.FailureRetryable, "terminal_read_failed")
	}
	defer txn.rollback()
	binding, err := txn.loadTerminalBinding(request.RunID, request.Route)
	if err != nil {
		return "", err
	}
	// The same binding a comment about a run is held to: the run and event
	// rows agree with the sealed envelope, and the envelope is this route's
	// space, project, creator and delivery. Nothing about a report is
	// checked here, because there is no report left to check.
	if !localRunCommentBindingMatches(binding, request.Route) {
		return hook.TerminalCompleteConflict, nil
	}
	runKey, row := binding.runKey, binding.runRow
	if !localTerminalReportMatches(row, request.ReportSHA256, request.Code) {
		return hook.TerminalCompleteConflict, nil
	}
	if stateValue, _ := row.str("state"); stateValue == stateTerminal {
		// Replay: the comment is already the one this row names.
		if row.int64Equals("terminal_comment_id", request.CommentID) {
			return hook.TerminalAlreadyComplete, nil
		}
		return hook.TerminalCompleteConflict, nil
	} else if stateValue != stateReportPending {
		return hook.TerminalCompleteConflict, nil
	}
	if row.has("question_record_sha256") {
		return hook.TerminalCompleteConflict, nil
	}
	if lease, ok := row.int64At("terminal_lease_until"); !ok || lease >= request.CompletedAt.UnixMilli() {
		return hook.TerminalCompleteConflict, nil
	}
	// Release the project's single pending slot, exactly as a completed
	// report does; a slot naming another run is not this run's to release.
	pendingKey := makeKey("pending", binding.envelope.Snapshot.SpaceKey, strconv.FormatInt(binding.envelope.Snapshot.ProjectID, 10))
	pendingRow, err := txn.getItem(pendingKey)
	if err != nil {
		return "", localFailure(hook.FailureRetryable, "terminal_recovery_write_failed")
	}
	if pendingRow != nil {
		if !pendingRow.strEquals("run_id", request.RunID) {
			return hook.TerminalCompleteConflict, nil
		}
		if err := txn.deleteItem(pendingKey); err != nil {
			return "", localFailure(hook.FailureRetryable, "terminal_recovery_write_failed")
		}
	}
	row["state"] = stateTerminal
	row["terminal_comment_id"] = request.CommentID
	row["terminal_completed_at"] = request.CompletedAt.UnixMilli()
	// The run is closed by a comment that is not its report, and the row
	// says so: an operator reading the ledger later should not have to
	// infer it from a comment id whose body says something else.
	row["terminal_report_recovered"] = true
	delete(row, "terminal_lease_token")
	delete(row, "terminal_lease_until")
	if err := txn.setItem(runKey, row); err != nil {
		return "", localFailure(hook.FailureRetryable, "terminal_recovery_write_failed")
	}
	if err := txn.commit(); err != nil {
		return "", localFailure(hook.FailureRetryable, "terminal_recovery_write_failed")
	}
	return hook.TerminalCompleted, nil
}

func validTerminalRecoveryClose(request hook.TerminalRecoveryCloseRequest) bool {
	return request.Route.Validate() == nil && hook.ValidRunID(request.RunID) &&
		request.Route.ExpectedRunID == request.RunID && request.Code.Valid() &&
		digestPattern.MatchString(request.ReportSHA256) && request.CommentID > 0 &&
		!request.CompletedAt.IsZero() && request.CompletedAt.Equal(request.CompletedAt.UTC()) &&
		request.CompletedAt.Before(time.Now().UTC().Add(hook.MaxTerminalReportClockSkew))
}
