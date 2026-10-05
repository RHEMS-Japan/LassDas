# Delivering to an agreed environment

When the request includes checking a change in an environment, opening or
merging its pull request is not the end. Add operator-owned commands to the
existing ordered run to check the actual integration, verify the exact artifact,
apply or observe it in the agreed environment, and read back the result before
reporting. No new engine delivery mode or model answer format is needed.

This is a composition guide, not an installed deployment adapter. Use the
project's existing CI/CD commands and records. The shipped examples do not
deploy to an environment, and this document does not grant that permission.
If the agreed endpoint is a branch or a pull request, do not add these stages.
A push or merge can already trigger CI or deployment: check those triggers
before describing either operation as only storing source.

## Agree on the endpoint and permissions

Identify the request, repository, integration branch, environment, permitted
operations, verification target and acceptance checks. Distinguish permission
to merge from permission to verify a person's merge, and both from permission
to change an environment. Keep any existing approval requirements in the
operator's own procedure; an earlier command returning zero is not approval.

The release of several changes to another environment is a separate task unless
it was explicitly included. Finishing an evaluation environment must neither
silently deploy to another environment nor wait for that separate release.

If an agreed change needs no environment deployment, use the project's existing
applicability rule and report its evidence. A missing job for a change that does
need deployment is not a successful skip. No universal path filter is supplied.

## Insert commands before reporting

Keep the existing requirements, work, verification, review and Git delivery
stages. Between Git delivery and report, configure the project's fixed commands.
For example, this is a **fragment**, not a runnable operator configuration:

```json
[
  {"name": "inspect_integration", "kind": "command", "on_failure": "elicit"},
  {"name": "verify_artifact", "kind": "command", "on_failure": "elicit"},
  {"name": "apply_environment", "kind": "command", "on_failure": "elicit"},
  {"name": "observe_environment", "kind": "command", "on_failure": "elicit"},
  {"name": "report", "kind": "model"},
  {"name": "confirm_report", "kind": "command", "on_failure": "report"}
]
```

These names are illustrative. Each must name a configured role; command roles
use fixed processes without `model_env`. `elicit` is the existing model stage
that receives failures and reconsiders the work. Running it again also reruns
the later stages. Preserve the existing question path for decisions outside the
agreed scope. The final command's observed success, not a model's claim of
completion, ends the run.

| Command | What it must establish using the project's existing records |
| --- | --- |
| Inspect integration | The actual PR and remote belong to this request and repository/base, the approved change was integrated, and the selected verification commit is known. |
| Verify/build artifact | The exact checked-out commit passed the agreed checks; the artifact bytes or immutable digest correspond to that check. |
| Apply or observe existing deployment | The same verified artifact is authorized for the named environment; read its current state before requesting an operation. |
| Observe environment | The named environment actually runs that artifact and satisfies the agreed checks, not merely a healthy response from an older version. |
| Report and confirm | Report the observed target, commit/artifact, checks, omissions and remaining work in ordinary prose; read back the matching report on the assigned issue. |

The engine records exit statuses and configured receipt text. It does not
interpret artifact identities, provider responses or approval semantics. A
missing `receipt` file is only recorded as missing by the runtime: commands
must themselves fail when a record needed for their operation is unavailable.

## Reuse verification without overstating it

`harnesses/verify_merged.py` fetches the real delivery remote and runs
`VERIFY_COMMANDS` against a separate checkout. After a recorded merge it checks
that the merge commit is contained in the integration branch and tests the
branch tip fetched at that time. That tip need not equal the merge commit.
Record the actual verified commit and build the artifact from that checkout.
If the operator selected a specific commit, the configured check must compare
the checkout's `HEAD` with it and fail on a mismatch. Do not verify one checkout
and then deploy an unverified artifact selected by a moving `latest` alias.

An existing CI record may already bind commit, checks and artifact. Read that
record instead of introducing a duplicate manifest. If the branch moves after
verification, keep the verified artifact or reverify the newly selected target;
do not silently substitute the newer artifact.

`DELIVERY_MERGE_METHOD=none` ends Git delivery at the PR. Its verifier checks
the recorded PR head; it does **not** wait for or discover a later human merge.
The verifier also returns zero without running checks for receipts describing
a PR closed unmerged or changed by a person. Its “Not checked” result is not
permission to change an environment. Inspect the actual PR and remote in the
operator command even when the local receipt or a preceding command says zero.

To continue through a person's merge, connect the existing PR service's
observation command and verify the resulting integration target. This guide
adds no human-merge watcher or approval service. Polling inside a command uses
an execution slot and counts as active work; it does not free the slot while
waiting. Do not reopen a Done run just to simulate such a watcher.

## Retry by observing, not by assuming failure

Before applying, recheck the request, target environment and verified artifact.
If existing CI has already deployed that artifact, observe it rather than send
a second deployment. If a request applied successfully but its response was
lost, the next launch must read the actual environment or provider operation
record and recognize the completed operation. Use the provider's existing
idempotency mechanism where appropriate. If its result cannot be determined,
fail without claiming success or blindly repeating a non-idempotent operation.

Application acceptance, job completion, health and artifact identity are
different observations. Check identity and the agreed application behavior
from the destination. A failed readback returns to the configured `on_failure`
stage; it must not reach report/Done. Do not add automatic rollback, broader
permissions, another target or environment recreation as incidental recovery.
For an out-of-scope repair, use the existing question path to present concrete
options and their effects.

The report stage should read existing comments before retrying a publication.
The shipped read-only `confirm-report` in the operator scripts example checks
that some stored comment equals `report/result.md`; it does not require the
report to be the newest comment. It proves storage, not report completeness or
the truth of its claims. The environment checks must precede that report.

## Keep means separate

Place fixed operator commands in a read-only operator-owned location, not
somewhere a model can replace them. Use the existing filesystem grants for only
the files each process needs. Each role's `TASK_HOME` is private; do not assume
another role can read it. Share necessary results through existing CI/provider
records or a narrowly granted workspace path.

Pass deployment credentials only to the fixed apply command. Do not give them
to models, source builds/tests or reporting. The process runner does not inherit
the parent's entire environment, but an explicitly configured secret is still
given to that process. The existing verifier removes `GITHUB_TOKEN` from its
verification children; this is not a promise to remove arbitrary other secret
variables supplied to it. Keep deployment credentials out of that role entirely.
Queue-idle/read-only operational checks can help schedule maintenance; they do
not prove artifact identity or deployment success. `--dry-run` verification is
an intentionally nonzero operator check, not a completed delivery.

## What the composition tests establish

`cmd/engine/environment_delivery_test.go` runs the existing config loader,
ordered chain, process launcher and file history with actual child processes.
It uses real local Git repositories, the existing verifier, the unmodified
shipped confirmation script and the tracker CLI through its scoped endpoint.
The Git delivery itself is prearranged in the fixture; no real PR is opened.

The test-only commands and local HTTP service emulate an operator's CI and
environment. Their records are fixture-private, not a required provider or
model schema. The models are explicit stand-ins. Tests count attempted
environment writes, not only accepted writes: wrong requests, open PRs, old
artifacts and wrong environments cause no write request. Response loss causes
one application followed by observation, and report confirmation failure must
return through report before Done. The checks also distinguish the remote
verified source from a different local workspace and later branch updates.

Mutating these test-only commands tests the boundaries of this composition,
not a new universal environment guarantee in the engine. Real CI/provider
authentication, approvals, idempotency, artifact provenance/signatures, Linux
credential isolation, browser behavior and model judgment are not established
by these tests. Validate the chosen operator commands and permissions against
the actual project's agreed environment before treating its setup as complete.
