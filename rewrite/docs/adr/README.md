# Design decisions

Each file here records one decision the engine was built on: what was decided,
what it was compared with, why, and where the design documents describe it.
Read the relevant record before changing the behaviour it covers.

A decision is recorded only when all three of these hold:

1. It is hard to reverse: reversing it means changing code or saved records, or
   other parts of the engine depend on it.
2. Without its context it looks surprising, so it could be undone as a mistake.
3. Alternatives were actually compared, and the README or the project
   instructions describe the choice.

Line numbers in records 0001 to 0011 refer to commit 579970b; a later record
names its own commit. When the documents have changed since, find the passage
by the section name given beside the lines.

| No. | Decision | Main source |
| --- | --- | --- |
| [0001](0001-role-chain-without-answer-checks.md) | A role chain with no schema or content checks on answers | README, "Role-chain engine" |
| [0002](0002-failure-never-ends-a-request.md) | A failure never ends a request; limits are operator settings | README, "Configured action connections", "A cap on how often a role runs" |
| [0003](0003-ordered-runs-advance-on-exit-status.md) | In an ordered run, exit status decides progress, not a role's words | README, "Stages instead of roles" |
| [0004](0004-answers-not-held-to-a-format.md) | Answers are not held to a format, except the review's blocking field | README, "Stages instead of roles", "Existing native-agent connection" |
| [0005](0005-delivery-depends-on-another-publishers-review.md) | Delivery without a person depends on a review by another publisher's model | README, "Stages instead of roles", "Naming one model instead of selecting" |
| [0006](0006-request-as-written-questions-at-the-entrance.md) | A request is taken as written, and questions come only from the entrance | README, "Settling the request at the entrance" |
| [0007](0007-consumer-specifics-stay-outside-the-engine.md) | A consumer project's specifics stay outside the engine | CLAUDE.md, "消費側の事情をフレームワークに埋め込まない" |
| [0008](0008-pull-request-description-kept-with-the-run.md) | The pull request description is kept with the run, not committed to the consumer repository | README, "A pull request description from the run's reports" |
| [0009](0009-forced-exits-resume-up-to-a-set-count.md) | After a forced exit, a time-limited request resumes by itself up to a set count | README, "Active-work limits and recovery" |
| [0010](0010-worker-writes-the-checkout-but-not-git.md) | The worker may write the whole checkout, but not its `.git` | RUNTIME.md, "Linux role launch" |
| [0011](0011-the-requester-sees-a-change-before-delivery-only-when-it-alters-use.md) | The requester sees a change before delivery only when it alters how the product is used | README, "Confirming the change before delivery" |
| [0012](0012-forced-exits-in-a-row-at-one-stage-pause-a-request-without-a-time-limit.md) | Without a time limit, forced exits in a row at one stage pause a request | README, "Active-work limits and recovery" |

## Considered and not recorded

- Routing with Jev or with an ordinary chat model. The README calls Jev routing
  one comparison candidate, not a settled decision ("Still missing before
  production use", line 2714).
- Selecting a model for each launch, or naming one model. The README calls the
  named model an experiment switch and says neither arrangement is established
  as the better one ("Naming one model instead of selecting", lines 1751-1753).
- No backward compatibility or data migration. This is a working rule for
  development, stated in the root README's "Development" section; it fixes no
  structure and can be changed for future work at any time.
- The setup assistant writing only under `.lassdas/` in the delivery repository.
  This is a direction for the setup assistant in `docs/PRODUCT_DIRECTION.md`,
  not engine behaviour described in the README. The engine itself delivers
  changes into the consumer repository and keeps its own records out of it
  ([0008](0008-pull-request-description-kept-with-the-run.md)).
