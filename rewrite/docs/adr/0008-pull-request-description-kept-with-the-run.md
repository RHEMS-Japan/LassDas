# 0008. The pull request description is kept with the run, not committed to the consumer repository

## Decision

The explanation in a pull request comes from a configured role's report in the
run's history (`PR_DESCRIPTION_ROLE`, read through the read-only `TASK_HISTORY`
reference). When review and delivery select the same pre-review role, review
receives it with the diff and must check it. Alternatively delivery alone can
select a later confirmation role's optional `PR_DESCRIPTION_SECTION`; this
summary is published without the earlier code review checking it. In both setups
delivery checks the whole description for `DELIVERY_FORBIDDEN_TEXT` and its
credential before any publication. The generated text is kept in the delivery
process's `TASK_HOME`, outside the checkout, and is never committed to the
consumer repository. The earlier option that took the description from a
document in the repository, `DELIVERY_PR_BODY_FILE`, was removed with no
compatibility path.

Section selection is delivery-only. It does not reach back across other roles
to a previous attempt, and an unclosed code fence is treated as an absent
section, with a distinct receipt reason. When a PR body is needed, delivery
checks description settings before staging; a setting or fixed-text refusal only
the operator can correct holds the process instead of immediately requesting
model rework. Endings that create no PR body do not require those settings.
The operator corrects the setting and restarts; the interrupted delivery then
follows its configured `on_failure` route, which may include model work before
delivery is tried again. After finding the section, any heading-shaped line
ends it even inside a fence or list; ambiguous syntax may shorten the summary,
never extend it into private text. Deeper headings are boundaries too. Fence
and paragraph handling locate the start and detect an open fence at the cut,
not authorize text beyond that boundary. Headings inside HTML blocks do not
start a section; an unclosed block leaves those candidates absent. Only
CRLF/CR/LF split lines.

## Alternatives compared

A document in the consumer repository, committed with the change and used as
the description. That was the removed option.

## Reason

The configured explanation checks do not require committing a document;
committing it adds no review of a later confirmation summary. A document would remain in the
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
