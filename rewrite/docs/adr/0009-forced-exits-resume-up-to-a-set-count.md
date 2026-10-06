# 0009. After a forced exit, a time-limited request resumes by itself up to a set count

## Decision

For a request with an active-time limit (`intake.max_active_minutes`), a forced
exit, or a failed final save, that leaves an interval with an unknown end is
recovered automatically. The interval is closed without adding its unknown time
or the downtime, one forced exit is counted, one automatic-recovery notice is
posted, and the work continues with the saved remaining allowance. At
`intake.max_hard_exits` forced exits (default 3; omission and zero also select
3) the request pauses until its creator or an authorized operator posts `再開`,
which resets the count. The bound is saved with the request when it is
accepted.

## Alternatives compared

Pausing after every forced exit until a person resumes the request; an operator
can still choose this by setting `intake.max_hard_exits` to 1. Resuming every
time without counting forced exits: because the unknown interval is not added to
the time limit, a request that keeps being cut off would then meet no bound. The
README states that zero does not mean unlimited restarts.

## Reason

A failure does not end a request
([0002](0002-failure-never-ends-a-request.md)), and after a restart the engine
carries interrupted work on by itself. The time limit cannot count an interval
whose end is unknown, so the count of forced exits is the only bound on
a request that keeps being cut off. That count stays finite and set by the
operator, and reaching it hands the request to a person without marking it
complete.

## Status

Accepted.

## Source

Line numbers refer to commit 579970b.

- [`rewrite/README.md`](../../README.md), "Active-work limits and recovery":
  "Select the limit", lines 1406-1421; "At the limit", lines 1441-1444;
  "Restart and operator action", lines 1456-1462.
- [`rewrite/README.md`](../../README.md), "What the requester is told at
  night", lines 1492-1494.
