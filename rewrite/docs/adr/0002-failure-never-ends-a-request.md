# 0002. A failure never ends a request; limits are operator settings

## Decision

The engine has no failing end state and no attempt-count terminal. A process
error, a failed command or a review that sends the work back leads to the
configured recovery or repair, and the work runs again with the failure in its record.
Only the configured end or a person's stop finishes a request. Limits exist
only as operator settings, and none of them fails a request:
`workflow.launch_limit` leaves a role out of the next choices, and the optional
active-time limit pauses the request, keeping its position, history and
workspace, until an authorized person posts `再開`.

## Alternatives compared

An attempt cap that marks a request failed after a number of tries; the README
states that recovery has no attempt cap or invented failure terminal. No bound
at all: a live run routed by a chat model sent a report back and forth between
its writer and its reviewer until the harness's own limit.

## Reason

A failed attempt is not a completed request, and the failure is the input to
the next attempt: the shipped ordered example sends a failed verification,
review or delivery back to requirements with the actual failure in view.
Abandoning a request is a person's decision, made with a stop comment, not the
result of a count. A loop can still run all night, so the operator can bound it,
but the bounds narrow the choices or pause for a person instead of discarding
the work.

## Status

Accepted.

## Source

Line numbers refer to commit 579970b.

- [`rewrite/README.md`](../../README.md), "Configured action connections",
  lines 152-157.
- [`rewrite/README.md`](../../README.md), "A cap on how often a role runs",
  lines 389-419.
- [`rewrite/README.md`](../../README.md), "Stages instead of roles", lines
  567-576 and 1031-1034.
- [`rewrite/README.md`](../../README.md), "Active-work limits and recovery",
  lines 1400-1402, and its "At the limit", lines 1436-1448.
- [`rewrite/README.md`](../../README.md), "Existing native-agent connection",
  lines 2121-2122.
