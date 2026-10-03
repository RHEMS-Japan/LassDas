# The permanent runtime for one consumer's role chain

These templates run the bundle from `rewrite/` as a Kubernetes workload that
stays up: the controller is the container's main process, its queue is on a
volume that survives a restart, roles clone from a local mirror instead of
holding a delivery credential, and delivery and verification are fixed
programs rather than something a model may improvise.

**To set one up, follow [SETUP.md](SETUP.md)** from start to finish: what you
need, the tracker, the repository, the operator configuration, the secrets,
the order of applying, the checks, the first ticket, stopping, upgrading and
the failures that were met.

| File | What it is |
| --- | --- |
| `statefulset.yaml.example` | the workload: init containers, the mirror, the engine, the status page |
| `egress-configmap.yaml.example` | this Pod's own firewall rules |
| `operator-scripts-configmap.yaml.example` | the operator's programs the shipped ordered configuration calls: `build`, `test` and the report check `confirm-report` |
| `operations/idle-check.sh`, `operations/copy-queue.sh`, `operations/queue_helper.py` | read-only queue inspection and a new private copy of its records; explicit Pod selection, no deployment or engine launch (SETUP.md, sections 7, 9 and 10) |
| `secrets.yaml.example` | the two Secrets' keys, without values, and where each value comes from |
| `status-service.yaml.example` | a cluster-internal address for the status page |
| `status-ingress.yaml.example` | optional: the status page at a host name; exposing it is the operator's choice |
| operator configuration | `rewrite/examples/operator-stages.json`, adapted per consumer (SETUP.md, section 4), mounted from a ConfigMap at `/etc/ticket-automation/operator.json` |

Copy the examples, replace every `<placeholder>`, and keep the result
outside this repository if it names a consumer, a repository or a secret.
**Reference every credential by name.** No key value belongs in a manifest, a
ConfigMap, a log line, a commit or a report.

## What was measured, and what is only proposed

Measured, by running the actual launcher and the actual engine in this kind of
runtime, without credentials or task data (`rewrite/RUNTIME.md`):

- The role launcher needs a Pod user namespace (`hostUsers: false`) and
  `procMount: Unmasked` for its private `/proc`. With the default seccomp
  profile the nested namespace was still refused; the same no-op launch
  succeeded with `seccompProfile: Unconfined` on the controller container,
  while non-root execution, dropped capabilities, no-new-privileges and a
  read-only root filesystem were kept. This is a runtime requirement that was
  found, **not** a recommended security profile.
- Roles themselves ran under a filter compiled from the pinned upstream
  profile for the native ABI with no capability-conditioned rule, loaded
  through bubblewrap's own `--seccomp`. A kernel-level probe of a role
  observed seccomp mode 2 with one filter, no effective or permitted
  capabilities, no-new-privileges, a refused second user namespace, and work
  paths it could not write.
- Egress rules written by init containers into this Pod's own network
  namespace held: the link-local metadata address was refused before and
  after a full workflow and a restart, while DNS and public destinations
  stayed reachable. `NET_ADMIN` never left those setup containers. Whether a
  `NetworkPolicy` object is enforced depends on the cluster's network plugin,
  and an accepted object is no evidence that it is. These rules close the
  Pod's egress whether or not one is, so check the refusals from the running
  Pod (SETUP.md, section 7) rather than trusting either.
- With the controller as the container's main process, a crash after a
  comment had been stored led to one automatic restart and a read-back
  instead of a second post.
- The image is built for arm64 only.

One consumer's installation has run these templates since September 2026,
with a live tracker, real pull requests merged by the shipped delivery, and
models reached through a gateway in front of OpenRouter. What it showed is in
SETUP.md where it matters: the cluster it ran on in section 1, and what went
wrong there and how it was resolved in section 11. On 2026-10-02 it ran its
two network init containers from the image's own iptables (SETUP.md,
section 1).

Proposed, or simply not measured. Check each one before trusting it:

- Another cluster, node architecture or storage class, and this volume's
  behaviour when the Pod moves to another node.
- The IPv6 egress rules as the image's own iptables writes them: the
  installation that ran them saw only that they were written.
- Leaving the merge to a person (SETUP.md, "Leaving the merge to a person"):
  checked by the repository's tests and against a stand-in for GitHub, not
  yet on an installation.
- `resources`. The requests and limits are a starting point from spare node
  capacity, not a measured working set. A model-driven build can exceed them.
- The shipped ordered configuration as it stands, with OpenRouter invoked
  directly: the installation above invoked its models through a gateway.
  Another tracker or Git host.
- The delivery credential's real scope, and branch rules other than a
  required approving review (SETUP.md, section 11). Neither can be read from
  inside this repository, and at the installation above the delivery token
  could not read the protection settings itself.
- Cost. The engine has no spending limit of its own. `intake.min_model_credit`
  pauses work when the remaining limit of the key `router.decision` names
  drops below a floor, so it needs that setting; with every model call
  through a gateway it has nothing to read, and the gateway's budget is the
  only bound.
- Disk. The volume is 20Gi and nothing prunes it. Once a request has
  finished, the caches its roles built in their private homes are removed;
  its records, checkout and logs stay, and a running request's caches grow
  without limit. A full volume stops progress quietly, because saving
  history is retried rather than abandoned. Record `df` before going live and
  look again after the first real requests.
- Time. A role's launch has no time limit unless its `timeout_minutes` sets
  one. A launch that runs long without failing is reported to the requester
  by the no-progress notice, and the status page shows its output as it
  arrives.

## Before it accepts work

SETUP.md, sections 6 to 8, gives the order, every check with the answer to
expect, and the moment the intake opens. The intake stays closed
(`created_since` in 2100) until every check has passed, because closing it
later does not stop requests already taken. A check that cannot be answered
is a blocker, not something to note and pass. Applying the StatefulSet,
opening the intake, and the first real push, pull request and merge are each
a decision of their own.

## What these templates do not do

They do not create a namespace, a tracker project, a repository, a token or a
secret value (the Secret template carries keys only); they do not expose the
status page unless the operator applies the Ingress; they do not change a
shared network plugin or a node; they do not install a model SDK; and they do
not decide that the syscall profile in the image is the right policy for the
work being admitted. Deleting the volume does not undo anything already
delivered outside the cluster: the queue is the record of what has already
happened.
