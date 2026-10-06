# 0011. The requester sees a change before delivery only when it alters how the product is used

## Decision

An ordered run can mark a model stage that reads the change before delivery
with `"confirm": true`. Its role reads the change actually made in the
checkout, the settled requirements and the project's knowledge, and says
whether the change alters how a person operates the product, what a screen
shows or does, or the public API. The project's knowledge defines the public
API; where it does not, the runtime's instruction takes the entry points used
from outside. The role names what it read and what it could not read, and a
change too long to read whole is not an internal one.

After that stage, the decision service that chooses after the first stage
chooses again among the first stage, the question role and the next stage,
never done. Its instructions say to ask when the change alters one of the
three or when the report cannot tell. Once the question is chosen, the request
waits for a comment with words from the requester whatever the question's
launch did, and nothing is delivered before it: a question that posted
nothing, did not exit 0 or was cut short by a restart waits as well, and the
engine posts its own notice asking for a comment. The reply goes back to the
stage that asked. A correction or a refusal leads back to requirements, after
which every later stage runs again, so a reply covers only the change it was
given about.

The limits on the choices after the first stage,
`intake.question_no_post_limit` and `workflow.entrance_rework_limit`, do not
apply after this stage. Returns to requirements from it are bounded by
`workflow.confirmation_rework_limit`, and `intake.question_reminder_minutes`
says a waiting question again on each interval. Without such a stage nothing
changes. The ordered examples ship it as `confirm_change`, between the review
and the delivery.

## Alternatives compared

A question and a wait built into the delivery command, which would hold a
second copy of the question handling the engine already has. A confirmation
after the report, when the change has already been merged. Connected routing
for every stage, which would change how unrelated stages progress for the sake
of one condition. A classifier that looks for fixed words in a role's text, and
a separate record of approvals. Asking about every change, against the
owner's decision that only these three kinds of change go to a person.
Counting a confirmation question that posted nothing toward the existing
no-post limit and going on: that would deliver without the requester's words.
Launching the question role again up to a separate limit before waiting: one
more counter, where waiting with the engine's notice is already bounded at one
launch per decision.

## Reason

The owner decided on 2026-10-05 that a person confirms only a change to how
the product is operated, to a screen or to the public API, and that an unclear
case is asked. Otherwise the engine delivers without a person
([0005](0005-delivery-depends-on-another-publishers-review.md)), questions come
at the start ([0006](0006-request-as-written-questions-at-the-entrance.md)),
and a failure never ends a request
([0002](0002-failure-never-ends-a-request.md)). A confirmation that a missing
post, a failed read or a reached limit could pass would not be one, so once
the question is chosen only the requester's words, or a stop, move the request
on. Reading the change and deciding whether to ask are model work, like
settling the requirements; the runtime holds the delivery until the requester
has spoken.

## Status

Accepted. Narrows [0006](0006-request-as-written-questions-at-the-entrance.md).

## Source

Line numbers refer to commit d1b5ac3.

- [`rewrite/README.md`](../../README.md), "Settling the request at the
  entrance", lines 472-476.
- [`rewrite/README.md`](../../README.md), "Stages instead of roles", lines
  1103-1118.
- [`rewrite/README.md`](../../README.md), "Confirming the change before
  delivery", lines 1183-1266.
- [`rewrite/README.md`](../../README.md), "What the requester is told at
  night", lines 1814-1846.
