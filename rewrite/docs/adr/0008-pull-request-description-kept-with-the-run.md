# 0008. The pull request description is kept with the run, not committed to the consumer repository

## Decision

The explanation in a pull request comes from a configured role's report in the
run's history (`PR_DESCRIPTION_ROLE`, read through the read-only `TASK_HISTORY`
reference). The review receives it with the diff and must check it, and the
delivery checks the whole description for `DELIVERY_FORBIDDEN_TEXT` and its
credential before any publication. The generated text is kept in the delivery
process's `TASK_HOME`, outside the checkout, and is never committed to the
consumer repository. The earlier option that took the description from a
document in the repository, `DELIVERY_PR_BODY_FILE`, was removed with no
compatibility path.

## Alternatives compared

A document in the consumer repository, committed with the change and used as
the description. That was the removed option.

## Reason

The review and the delivery check the explanation without it being committed,
so committing it adds no check. A committed document would remain in the
consumer's history after the merge, and because requests run side by side and
merge into the same integration branch, a document at one path would be changed
by each of them. The engine keeps its other records outside the workspace in the
same way, as the review command does with its log and send-back count.

## Status

Accepted.

## Source

Line numbers refer to commit 579970b.

- [`rewrite/README.md`](../../README.md), "A pull request description from the
  run's reports", lines 1103-1113 and 1131-1137.
- [`rewrite/README.md`](../../README.md), "Stages instead of roles", lines
  587-590 and 857-859.
