# 0014. Approval binds one launch to a verified post

## Decision

A command stage may require a human GO on a complete private bundle after an
immediately preceding command. The child copies, posts, verifies and consumes;
the parent accepts GO, combines rejections and enforces the deadline. Both
use the same current-account, comment, request, notice and approver checks.
Stop always wins. Accepted GO records bind one private digest and post to one
launch position. A failed or interrupted launch needs a new plan, preceding
command, post and GO. The runtime does not interpret the operation itself.

The five approval pauses use fixed runtime history lines, without changing
Step or Recovering. Expiry records an unsuccessful approval before pausing;
resume follows on_failure back to planning. Rejection records the failure
followed by the approver's words and is not a failed launch attempt.

Post readback mismatch, comparison mismatch, no eligible approver, and saved
configuration mismatch are exceptions to [ADR 0002](0002-failure-never-ends-a-request.md):
resume alone does not make work runnable. Before any child launch the parent
repeats the comparison, approver and saved-workflow checks. If they fail it
holds the request again, without launching work. Their notices instruct the
operator to stop and file a new request, without promising continuation.

## Alternatives compared

Treating GO as a question reply would allow the requester to release approval.
Checking only eight digest characters would not bind the full copied bytes.
Resuming ordinary work from a damaged approval would discard the evidence
that the approver saw. None meets the one-GO-per-launch requirement.

## Consequences

Approval configurations require Backlog Markdown and the isolating launcher
for every role process. Approver credentials stay outside automation. Other
configurations keep their existing behavior. Approval waits return the slot,
have no reminders, and hand the issue to the first configured approver.
Runtime notifications do not quote role output in these configurations.

## Status

Accepted.

## Source

[Approval conditions and records](../../README.md#approval-conditions) and
[environment delivery](../../ENVIRONMENT-DELIVERY.md).
