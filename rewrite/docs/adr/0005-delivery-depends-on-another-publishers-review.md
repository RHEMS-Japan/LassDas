# 0005. Delivery without a person depends on a review by another publisher's model

## Decision

By default the shipped delivery merges reviewed work without a person. What
permits this is an adversarial review, run as the operator's own command, by a
model from a different publisher than the worker. The model receives the
original request, the settled requirements, the previous reports, the diff and
the output of the operator's tests, and returns a blocking verdict. A blocking
verdict sends the work back, and unless the operator opts in otherwise, nothing
is delivered without a verdict. Every fallback review model should also come
from a different publisher than the worker, the shipped ordered example leaves
the review model's publisher out of the publishers the worker is chosen from,
and the processes of one parallel group are selected from different publishers.

## Alternatives compared

A person reviewing and merging every pull request, which remains an operator
option (`DELIVERY_MERGE_METHOD=none`). Review by the worker's own model: with
one named model for every launch, both reviewers run that model and their
reports are no longer independent. Letting work through when no verdict can be
obtained, the old behaviour, now only an explicit opt-in.

## Reason

The README states that the adversarial review is what justifies delivering
without a person, and the project instructions give it more weight because no
input format is required ([0006](0006-request-as-written-questions-at-the-entrance.md)).
That justification needs a review whose report is independent of the worker's,
and the README does not count reports from one model as independent. This is
the basis for delivering without a person, not a proof of correctness: the
README also records reviewers who accepted defects.

## Status

Accepted.

## Source

Line numbers refer to commit 579970b.

- [`rewrite/README.md`](../../README.md), "Stages instead of roles", lines
  680-681, 834-840, 910-912, 923-925 and 971-972.
- [`rewrite/README.md`](../../README.md), "A pull request description from the
  run's reports", line 1114.
- [`rewrite/README.md`](../../README.md), "Experimental per-launch model
  selection", lines 1657-1658 and 1675-1676.
- [`rewrite/README.md`](../../README.md), "Naming one model instead of
  selecting", lines 1748-1751.
- [`rewrite/README.md`](../../README.md), "Isolated live observations", lines
  2590-2594.
- [`CLAUDE.md`](../../../CLAUDE.md) at the repository root, "入力について",
  line 86.
