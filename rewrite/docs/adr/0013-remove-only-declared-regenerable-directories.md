# 0013: Remove only declared regenerable directories

Reference baseline: `3a0bdd3`. Main source: README, "Opt-in removal of
regenerated install directories".

## Decision

An empty-by-default `intake.trim_install_dirs` lets an operator declare exact
relative install-output directories. Before a different role executes, only
the previous role's bound homes are cleaned, after all processes returned and
results were saved. Same-role retries and entering a requester question keep
their directories. Failed roles
that hand back to another role are included; success-only cleanup was considered
but not selected because the declared files are regenerable and failure evidence
remains in history and logs. Workspace cleanup waits for the existing finished
request path, stop report and preview lifetime. Cleanup failure is an observation,
not delivery failure. Finished workspace cleanup has one 512-entry visit budget
per tick and preserves Git-tracked work. A zero-removal pass stops and reports
the remaining tree rather than retrying immediately. No selected path remaining
means no saved-configuration read; no completion marker is needed.

Reuse the existing descriptor-relative directory/mount boundary helpers. Do not
pass arbitrary multi-component operator paths to the string-path final-cache
remover. Preserve source, Git metadata, receipts, history and logs. Do not change
the old final home cleanup or add a recovery journal. The operator's command
prepares dependencies each time, retaining its private download cache, and must
ensure children no longer use the home after return.

## Alternatives

- Shared writable package/build caches or hardlinks reduce duplication but
  allow one role's writes to affect another. Not adopted.
- One shared home changes credentials, session state and independent review
  boundaries. Not adopted.
- Automatically detect a language and reinstall dependencies: the runtime does
  not know the operator's build, lock, required credentials or generated output.
  Use the existing operator commands, not a new installer.
- Recursively remove every directory with a familiar basename risks deleting
  source or an unrelated consumer's artifacts. Only exact configured paths.
- Clean after each process exits: other processes in the same role may still
  run. Wait for the whole role and save its result first.

## Limits

Offline recreation is conditional on the actual manifest, toolchain, install
scripts and retained private cache. A successful fixture is not a general
promise. External previews may depend on workspace files; retain existing
teardown/hold ordering. A detached descendant must be contained by the launcher.
No automatic migration, shared cache, new completion condition or saved model
output format is introduced.
