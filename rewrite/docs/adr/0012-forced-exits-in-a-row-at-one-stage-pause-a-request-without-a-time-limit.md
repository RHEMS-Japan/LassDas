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

## Addendum: launches that did not end cleanly

### Decision

With or without a time limit, the launches in a row of the stage that ran last
that did not end cleanly are counted from the request's history: a role's
error, a forced exit, a process stopped at its time limit and a launch for
which no model could be selected, together and in any order. A launch the
runtime stopped itself is not counted and does not start the count over.
Another stage, a clean launch of the stage and the requester's words, an
answer or a `再開`, start it over. At `intake.max_stage_attempts` (default 5;
omission and zero also select 5) the run stops before the stage is chosen
again, and the request pauses with one notice that names the stage, the count
and how the launches ended, until the same authorized `再開`. The forced-exit
counts above and of [0009](0009-forced-exits-resume-up-to-a-set-count.md) are
unchanged and kept apart; when a forced exit reaches both, the forced-exit
pause is the one recorded.

### Alternatives compared

Counting errors into the forced-exit count above. Its rule that a process of
the stage ending by itself starts the count over is what lets a stage that
alternates errors and forced exits run all night; dropping the rule would
change what `intake.max_hard_exits` counts for every installation.

Keeping the count in `work-limit.json`. Errors happen inside a run, and the
record a run saves when it ends is the one it read at its start, so a count
written beside it would be overwritten; the history already holds how every
launch ended, including the note a restart writes for a cut launch.

Using `intake.max_hard_exits` for this count. At its default of 3, two quick
failures of a model service and one error would pause a request, and one
setting could not be tuned for forced exits and for errors separately.

Counting launches the runtime stopped itself. A deploy or a credit hold during
a long stage would then count against the stage.

Stopping only from the watch, at its next tick. By then the next launch would
have chosen a model and started.

### Reason

A failure does not end a request
([0002](0002-failure-never-ends-a-request.md)), and the ordered run launches a
model stage again after every process error, so without a time limit nothing
bounded a stage that never ended cleanly. Counting from the history bounds that
case with one source for the count, leaves a stage that a later stage sends
back uncounted, since another stage runs in between, and hands the request to
a person with the same pause, notice and resume, without marking it complete.

## Status

Accepted. Extends [0009](0009-forced-exits-resume-up-to-a-set-count.md) to
requests without a time limit. The addendum bounds launches that did not end
cleanly, with or without a time limit.

## Source

Line numbers refer to commit 98f7740.

- [`rewrite/README.md`](../../README.md), "Active-work limits and recovery":
  "Select the limit", lines 1725-1738; "Restart and operator action", lines
  1778-1794.
- [`rewrite/README.md`](../../README.md), "What the requester is told at
  night", lines 1850-1856.

The addendum's line numbers refer to commit fbc4cc8.

- [`rewrite/README.md`](../../README.md), "Active-work limits and recovery":
  "Select the limit", lines 1797-1809; "Restart and operator action", lines
  1877-1903.
