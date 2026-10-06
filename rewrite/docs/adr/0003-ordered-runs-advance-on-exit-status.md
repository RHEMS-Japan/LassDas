# 0003. In an ordered run, exit status decides progress, not a role's words

## Decision

With `router.mode: "stages"`, the runtime, not a model, decides what runs next.
A command stage is satisfied only when all of its processes exit 0. A model
stage is satisfied when its processes ran without a process error; nothing it
wrote is read, so writing "done", "verified" or "delivered" advances nothing.
The last stage must be a command, so an observed exit status finishes the run.
Outside the entrance, no model decides which stage runs next, whether a stage is
satisfied, whether a failure is recoverable or when the request is complete.
The one routing decision left to a model, when a question role is configured,
comes after the first stage: another requirements pass, a question to the
requester, or the next stage. That choice is bounded by an operator setting and
is never completion ([0006](0006-request-as-written-questions-at-the-entrance.md)).

## Alternatives compared

A routing model that reads the reports and decides repair, progression and
completion, as in connected workflows and in free routing. Both remain
available for comparison.

## Reason

In the live runs recorded in the README, routing models chose completion while
the delivered work still failed the request, and choosing a different router
did not change that. A report is what a model wrote about the work; an exit
status is what a configured check observed. Progress therefore follows the
exit status. This does not make the checks sufficient: the same records show
defective work passing the checks it was given, so an ordered run proves only
what the operator's commands test.

## Status

Accepted for ordered runs, which the shipped Kubernetes example uses. Connected
workflows and free routing are kept for comparison.

## Source

Line numbers refer to commit 579970b.

- [`rewrite/README.md`](../../README.md), "Stages instead of roles", lines
  545-547, 565-582, 1020-1021 and 1058-1059.
- [`rewrite/README.md`](../../README.md), "Configured action connections",
  lines 142-145 and 163-164.
- [`rewrite/README.md`](../../README.md), "Isolated live observations", lines
  2528-2532 and 2590-2596.
