# Discarding stopped workspaces

By default, stopping work keeps its source checkout and unpublished changes.
Home-cache cleanup after stop reporting is separate and already retries failures.
To reclaim stopped source checkouts automatically, an operator may set
`intake.stopped_workspace_retention_hours` to a positive integer in the existing
configuration. Zero or omission disables it. A configured stop-report role is
required. Validate the complete configuration with the existing `--check`.

**This setting authorizes irreversible loss of unpublished commits, tracked,
untracked and ignored workspace files after the grace period.** It is not an
archive or a promise that the work can be reconstructed. Arrange a separate
backup before enabling it if those files must remain recoverable. It does not
enable disposal on any installation merely by being documented here.

The grace period starts when the controller first observes an authorized saved
stop and its completed separate stop report. Enabling the setting does not
immediately discard old stopped jobs. Restart preserves the deadline; changing
the positive setting before removal starts grants a new full grace period.
Zero stops further removal, but cannot restore bytes already removed.

Only the fixed workspace prepared by the shipped preparation command qualifies.
Unrecorded or adopted workspaces, unreadable records and unexpected filesystem
arrangements are retained with a reason. Questions, paused work, failed attempts
and delivered requests do not qualify merely because time has passed. No question
expiry or new model judgment is introduced. A stop report's existing completion
decision is reused, not replaced with a new output certificate.

Before removing source data, the controller atomically saves the exact standard
delivery receipt, workspace report and configured process receipts in one
`workspace-evidence.json` file outside the workspace. If this file cannot be
saved or becomes unreadable during removal, further deletion is held.
Evidence larger than the 64 MiB saved-file limit is retained in place, not truncated.
The accepted request, both histories, stop instruction and logs stay in place.
The status page distinguishes discarded or partially removed workspaces and reads
their retained evidence. This evidence does not recreate unpublished work or
change a stopped request into a completed delivery.

The controller takes the existing work, report and preparation locks. It does not
follow workspace links or cross observed mount boundaries. Locked, unreadable or
unsupported targets wait; a failed removal retries on later ticks against the
same recorded directory. Each attempt is bounded so a large tree does not require
one unbounded deletion pass. Other accepted work can continue.

Do not edit/delete the controller's removal records or replace a workspace during
removal. Privileged concurrent host changes are outside this guarantee. A crash
can leave partial removal; the retained evidence remains, but the lost source
files are not restored. Native stop remains final for that request: use a new
request if further work is wanted.
