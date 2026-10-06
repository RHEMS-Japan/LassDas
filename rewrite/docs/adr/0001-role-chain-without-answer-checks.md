# 0001. A role chain with no schema or content checks on answers

## Decision

The original request and each role's ordinary prose report go to a routing
step, which invokes the next configured role. The role runs an existing agent
harness and returns its own words. No working role's answer is checked against
a schema, a signature, a confidence threshold or its content. A routing choice
names the next configured action; it is not a certificate that the previous
answer passed. The previous runner, its installer and its reusable workflow
were removed from this source tree. Their history stays in Git, and running
installations and saved requests were not migrated.

## Alternatives compared

Keeping the previous intake, in which a model generated a reception contract
for each request before the work began. The current intake passes the original
title and description on directly, and does not ask a model to reject a request
for its prose format. Adding checks on answers after live runs ended with false
completion claims: the README records that a new answer-format or
content-certification gate was not the remedy pursued, and that none was
introduced.

## Reason

A check on an answer judges what a model wrote, not what was delivered. In the
live runs recorded in the README, runs reached their reports, passed their
generated tests and posted final comments while the delivered work still failed
the request. Judgment therefore stays with the routing model and the roles,
which have the run's history. Where proof is required, it comes from results the
runtime observes, such as a review verdict or a configured command's exit
status ([0003](0003-ordered-runs-advance-on-exit-status.md),
[0005](0005-delivery-depends-on-another-publishers-review.md)).

## Status

Accepted.

## Source

Line numbers refer to commit 579970b.

- [`rewrite/README.md`](../../README.md), "Role-chain engine", lines 13-18.
- [`rewrite/README.md`](../../README.md), "Automatic intake experiment", lines
  330-332, and "Direct operator-configured tracker tool", lines 2291-2292.
- [`rewrite/README.md`](../../README.md), "Isolated live observations", lines
  2337-2340, 2443 and 2590-2596.
- [`README.md`](../../../README.md) at the repository root, lines 8-11.
