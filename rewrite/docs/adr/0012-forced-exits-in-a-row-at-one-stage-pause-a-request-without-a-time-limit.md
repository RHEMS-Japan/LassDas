# 0012. Without a time limit, forced exits in a row at one stage pause a request

## Decision

A watched request without an active-time limit (`intake.max_active_minutes`)
writes the same start marker at each launch as a time-limited request, and an
ordinary return or shutdown clears it. Recovery from a marker left behind
counts one forced exit at the stage the history shows running: the pending
action, or else the stage that ran last. The count goes on while forced exits
come one after another at that stage. A forced exit at another stage, a process
of the counted stage that ended by itself, successfully or not, and the
requester's words start it again from one. At `intake.max_hard_exits` (default
3; omission and zero also select 3), saved at the request's first watched
launch, the request pauses with one notice that names the stage and the count,
until the requester or an authorized operator posts `再開`, which sets the
count to zero. A time-limited request keeps the count of
[0009](0009-forced-exits-resume-up-to-a-set-count.md) unchanged.

## Alternatives compared

Counting every forced exit of the request, as a time-limited request does.
Without a time limit only a resume would reset that count, so a long request cut
off now and then by unrelated restarts, at stages that each got through, would
still pause. The case to bound is a stage cut off at the same point every time.

Counting the runtime's notes for interrupted actions in the history instead of a
start marker. An ordinary shutdown during a launch keeps the pending action as
well, so that a later resume still warns about uncertain external effects, and
on the next start it becomes the same note. A deployment would then count as a
forced exit.

Starting the count again whenever another stage runs in between. After a
restart the decision service is told to have a role inspect what happened
before repeating the interrupted action, so another stage may run between every
two forced exits and the count would never grow.

Leaving requests without a time limit unbounded, as before. A stage whose build
exhausted the container's memory was cut off, relaunched after the restart and
cut off again for hours, with comments on the issue each time.

## Reason

A failure does not end a request
([0002](0002-failure-never-ends-a-request.md)), and after a restart the engine
carries interrupted work on by itself. Without a time limit nothing else bounded
a request that is cut off at the same point every time. Counting forced exits in
a row at one stage bounds that case without pausing work that keeps getting
through, and reaching the count hands the request to a person with the same
pause, notice and resume as
[0009](0009-forced-exits-resume-up-to-a-set-count.md), without marking it
complete.

## Status

Accepted. Extends [0009](0009-forced-exits-resume-up-to-a-set-count.md) to
requests without a time limit.

## Source

Line numbers refer to commit 98f7740.

- [`rewrite/README.md`](../../README.md), "Active-work limits and recovery":
  "Select the limit", lines 1725-1738; "Restart and operator action", lines
  1778-1794.
- [`rewrite/README.md`](../../README.md), "What the requester is told at
  night", lines 1850-1856.
