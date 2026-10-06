# 0010. The worker may write the whole checkout, but not its `.git`

## Decision

The shipped examples let the work role write anywhere in its checkout
(`--write .`), every existing and future file, instead of a list of paths fixed
before the work. Under a grant that covers it, the checkout's own `.git` stays
read-only unless `.git` is named, and only the delivery process names it.

## Alternatives compared

A list of writable paths chosen in advance for each project or request. Narrower
grants remain possible, but they must name paths that already exist.

## Reason

Which files a change needs is not known before the work. `.git` is the
exception because hooks and configuration inside it run as whatever process
opens the repository next, and the delivery process opens it holding a
credential no role may have. A writable `.git` would let a role have its own
code run with that credential.

## Status

Accepted.

## Source

Line numbers refer to commit 579970b.

- [`rewrite/RUNTIME.md`](../../RUNTIME.md), "Linux role launch", lines 80-90.
- [`rewrite/START.md`](../../START.md), "2. 実行環境と設定", lines 61 and 67.
- [`rewrite/README.md`](../../README.md), "Stages instead of roles", lines
  601-603.
