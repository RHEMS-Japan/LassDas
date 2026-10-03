# Publish only a branch

Use this when the agreed destination is a Git branch, not an open pull
request, an integration branch or a running environment. A branch push can
still trigger your existing CI or deployment automation: check those triggers
and permissions before choosing this option.

In the existing delivery process configuration, append `--branch-only` to
the `deliver_git.py` command. Keep the operator's repository, base branch,
allowed paths, forbidden text and credential references. Remove explicit
`DELIVERY_MERGE_METHOD` and `DELIVERY_PR_BODY_FILE` settings; either conflicts
with branch-only delivery, including merge method `none`.

For a sandboxed process, keep `linux_role.py`, every existing mount and
permission argument, the interpreter and the read-only harness path. Append
the single final argument `--branch-only`; do not replace the whole command
with an unwrapped Python invocation. Do not change the other processes or
give model tools delivery keys. No engine-level depth option or model
response format is required.

The ordinary invocation still opens and merges a pull request.
`DELIVERY_MERGE_METHOD=none` still opens a pull request and leaves its merge
to a person. Neither is a substitute for branch-only publication.

## What it does and records

The fixed command applies the existing changed-path, conflict and forbidden
text/credential checks, commits the permitted work, and catches up from the
configured base when necessary. It pushes `ticket/<issue>` and reads that
branch back at the exact committed head. It does not update the base branch
or call the pull-request REST API. The existing delivery receipt records
the branch, published commit and confirmation time, with `branch_only: true`;
it contains no invented PR or merge. This is the fixed command's own receipt,
not a format requested from any model.

A rerun without new work reads the remote but does not recommit or repush.
New reviewed work continues the same branch. A saved push attempt is read
back after a lost reply or interrupted confirmation. If it cannot be
confirmed, the command returns nonzero and preserves what it knows; earlier
commits or remote changes are not undone. The configured workflow handles
that failure through its existing retry or `on_failure` destination.

An already-existing branch without this request's publication record is not
adopted. Another writer's push, rewrite or deletion is not overwritten or
recreated. The command requires both ancestry from its previously published
head and an explicit Git lease for that head, including an absent-branch
lease on first creation. The lease closes the read-to-push race; it is not
permission to rewrite history. Recovery does not automatically restore a
missing local receipt from a branch name alone.

Do not switch a request with a PR receipt to branch-only, or a recorded
branch-only publication to PR mode. Use a separately agreed new request for
such a change of destination. No existing PR is closed or merged by changing
this setting.

An empty workspace is refused by default. The separate, explicit
`DELIVERY_ALLOW_UNCHANGED=1` policy can still end with an unchanged receipt
after reading the actual base. That reports no branch publication and does
not establish that an unfulfilled request was satisfied.

## Verification and the final report

Keep the existing `verify_merged.py` process after delivery. Despite its
historical filename, it recognizes a branch-only receipt, fetches that
branch, requires the exact published head and runs the operator's commands
on that fetched commit with delivery credentials removed. A changed or
deleted branch, incomplete record, different repository/base, unreadable
remote or failing verification returns nonzero. It does not verify the
model's modified workspace, a PR merge or a deployed environment.

The final report should name the branch and commit and say that no PR was
opened, nothing was merged and no environment deployment was checked. The
existing board's generic Delivered column means the configured destination;
the detail view retains the receipt and verification report. It is not a
claim that a production environment changed. Model-written reporting still
requires the configured independent review; byte-transport tests do not
prove that every model will describe the result correctly.

`--branch-only --dry-run` checks the configuration, paths and Git reachability
without committing, pushing or contacting the PR API. Like the existing dry
run it deliberately returns nonzero, never evidence of a completed delivery.

Tests use real local Git repositories and a local API fixture that must
receive zero requests. Real hosting permissions, branch protection, triggered
CI, live credentials and deployment behavior remain installation checks.
