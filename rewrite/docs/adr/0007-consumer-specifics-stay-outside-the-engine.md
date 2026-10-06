# 0007. A consumer project's specifics stay outside the engine

## Decision

The engine contains nothing specific to one consumer project. The repository,
the tracker scope, the writable paths, the delivery target, the checks and the
project knowledge come from the operator's configuration. Knowledge already in
the consumer repository is given by its location in the shared instructions,
and knowledge that exists only outside it is given as content; no file name for
project knowledge is assumed or required. Reusable answers from the requester
reach the consumer repository only through the ordinary delivery pull request,
at a destination the operator names or the requester picks when the entrance
question offers file choices. A test fails when an identifier from an operator-provided
list of consumer values appears in any tracked file or path of this repository;
without the list it reports itself skipped.

## Alternatives compared

The project instructions record alternatives that were proposed and rejected: a
stage that works out the target repository from a request, when the ticket
states it and an unclear one can be asked about; and copying a consumer's
knowledge document into a secret, when its location is enough. The README
rejects a file-name convention and a controller-owned append operation for
knowledge.

## Reason

Every mechanism has to hold for anyone's project; what does not is either
configuration received from outside or not needed at all. A copy of text that
lives in the repository needs synchronising on every change and drifts, and a
fixed file name serves only the projects that use that name. A consumer's value
inside the engine couples every later installation to the first one and blocks
a public release.

## Status

Accepted.

## Source

Line numbers refer to commit 579970b.

- [`CLAUDE.md`](../../../CLAUDE.md) at the repository root,
  "消費側の事情をフレームワークに埋め込まない", lines 17-37 and 50-60.
- [`rewrite/README.md`](../../README.md), "Current executable", lines 103-110.
- [`rewrite/README.md`](../../README.md), "Existing native-agent connection",
  lines 1999-2012.
- [`rewrite/internal/enginepurity/purity_test.go`](../../internal/enginepurity/purity_test.go),
  lines 1-6.
