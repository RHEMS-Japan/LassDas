# 0004. Answers are not held to a format, except the review's blocking field

## Decision

No model answer has to match a schema. A working role's prose reaches the next
step unchanged, and an empty answer is not rejected for its form. Where a
decision depends on a structured answer, the review verdict, the reader takes
what is plainly meant. It reads every call of the verdict tool and, in each,
every field named blocking in any letter case, taking true or false, 1 or 0, and
"true", "yes", "1", "false", "no" or "0" in any case; arguments may be JSON text
or an object, and one object further in is read as well. Doubt about the
blocking field does not let work through: any value read as true sends the
work back, a value that cannot be read is no verdict, and by default nothing is
delivered without a verdict ([0005](0005-delivery-depends-on-another-publishers-review.md)).

## Alternatives compared

A fixed answer format that refuses an answer whose field names or values differ
from the expected ones. Reading for plain meaning everywhere, including letting
work through when no verdict can be read: that is the old behaviour, now kept
only as the operator's explicit opt-in `REVIEW_UNAVAILABLE=pass`.

## Reason

The engine's evidence is review and observed results, not the form of an
answer ([0001](0001-role-chain-without-answer-checks.md)), so a form check would
add a way to stop work without adding evidence about the work. The one field
whose misreading could deliver a defect without a person is the blocking field,
so doubt there sends the work back or holds it.

## Status

Accepted.

## Source

Line numbers refer to commit 579970b.

- [`rewrite/README.md`](../../README.md), "Role-chain engine", lines 15-16.
- [`rewrite/README.md`](../../README.md), "Configured action connections",
  line 153.
- [`rewrite/README.md`](../../README.md), "Stages instead of roles", lines
  839-854, 910-912 and 971-972.
- [`rewrite/README.md`](../../README.md), "Existing native-agent connection",
  lines 2033 and 2043-2044.
