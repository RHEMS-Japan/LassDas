# 0014. Approval binds one launch to a verified post

## Decision

A command stage may require a human GO on a complete private bundle after an
immediately preceding command. The child copies, posts, verifies and consumes;
the parent accepts GO, combines rejections and enforces the deadline. Both
use the same current-account, comment, request, notice and approver checks.
Stop always wins. Accepted GO records bind one private digest and post to one
launch position. A failed or interrupted launch needs a new plan, preceding
command, post and GO. The runtime does not interpret the operation itself.

The five approval pauses use fixed runtime history lines, without changing
Step or Recovering. Expiry records an unsuccessful approval before pausing;
resume follows on_failure back to planning. Rejection records the failure
followed by the approver's words and is not a failed launch attempt.

Post readback mismatch, comparison mismatch, no eligible approver, and saved
configuration mismatch are exceptions to [ADR 0002](0002-failure-never-ends-a-request.md):
resume alone does not make work runnable. Before any child launch the parent
repeats the comparison, approver and saved-workflow checks. If they fail it
holds the request again, without launching work. Their notices instruct the
operator to stop and file a new request, without promising continuation.

## Posted text

The approval comment asks the requester to review the production operation in
Japanese. It shows the optional `approval.summary` file verbatim as plain text,
the exact GO line, a deadline in `approval.timezone` (default UTC), the previous
execution outcome, and the stop conditions. From the second post onward, the
current full digest is compared with post n-1 only when both belong to the same
approval stage: `計画は前回の確認と同じです。`
or `計画は前回の確認から変わりました。`. The first post, a missing post n-1,
or a post n-1 from another stage has no comparison. A summary-only change also changes the digest.
The two headings remain
`## 試し実行の結果` and `## AI が書いた計画`. The summary is not repeated
in the file listing, including its filename. Other files retain their order
and fenced representation.

````text
本番の操作の確認をお願いします。

やること:
確認済みの設定を本番に反映します。

実行してよければ、次の 1 行だけを新しいコメントに書いてください:
GO 543f0e3e

期限: 2026-10-25 22:06 (JST) まで。過ぎると一時停止します。続けるときは「再開」と書いてください (計画を作り直して、もう一度確認します)。
前回の実行: まだ実行していません。
止めたいときは「停止」とだけ書いてください。GO を消したり書き直したりしても取り消しにはなりません。
確認待ちの間に承認者が GO・停止・再開以外のコメントを書くと、修正の依頼として扱い、計画を作り直してから改めて確認します。
最初の確認の投稿より後は、確認待ちかどうかに関わらず、承認者以外の人のコメント、確認待ちでない間の承認者のコメント、コメントの編集・削除があると、安全のため実行せずに止まり、起票し直しになります (承認者が複数いるときは、残った人で続けます)。

## 試し実行の結果

```
dry-run.txt
checked
```

## AI が書いた計画

```
plan.txt
proposed
```
````

The summary path is relative to the working copy and must be inside the bundle
(check 12), follow the bundle filename and depth rules, and not also appear in
`show`. Missing, empty after trimming, over 2,000 characters after trimming,
invalid UTF-8, triple backticks or tildes, a line starting with `#` after leading
spaces and tabs, a setext underline matching `^\s{0,3}(=+|-+)\s*$`, or a
separator consisting of three or more identical `*`, `_`, or `-` characters
after whitespace is removed produces
`summary_unreadable`. Lines over 200 characters retain `bundle_limit`.
Summary failures return to planning, with the configured path and repair
instructions in the planning role's input; they do not add a pause reason.
The existing credential checks also cover the summary.
The summary's original bytes remain part of the copied bundle and digest.
Check 11 rejects an unreadable time zone name. Embedded time zone data
avoids depending on the image. Saved deadlines remain RFC3339 UTC.

Account IDs, comment IDs, the full digest, internal stage names, and runtime
clock terminology are absent from the fixed post text. GO matching, post and
launch binding, and `sent_text_sha256` readback stay unchanged. Failure notices
start with `確認の途中で止まりました。理由: ` and retain recovery instructions.
If execution started but its result cannot be read, the notice starts with
`実行を開始しましたが、結果が分かりません。理由: `.

## Alternatives compared

Treating GO as a question reply would allow the requester to release approval.
Checking only eight digest characters would not bind the full copied bytes.
Resuming ordinary work from a damaged approval would discard the evidence
that the approver saw. None meets the one-GO-per-launch requirement.

## Consequences

Approval configurations require Backlog Markdown and the isolating launcher
for every role process. Approver credentials stay outside automation. Other
configurations keep their existing behavior. Approval waits return the slot,
have no reminders, and hand the issue to the first configured approver.
Runtime notifications do not quote role output in these configurations.

## Status

Accepted.

## Source

[Approval conditions and records](../../README.md#approval-conditions) and
[environment delivery](../../ENVIRONMENT-DELIVERY.md).
