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
  stayed reachable. `NET_ADMIN` never left those setup containers. A
  `NetworkPolicy` object was **not** enough on this cluster: its network
  plugin has policy enforcement disabled, and connections still went through.
- With the controller as the container's main process, a crash after a
  comment had been stored led to one automatic restart and a read-back
  instead of a second post.
- The image is built for arm64 only.

Measured in one consumer's live instance, run from these templates since
2026-09-29 on one cluster:

- The StatefulSet in this directory, status page and operator scripts
  included, with the gateway's key in place of `MODEL_API_KEY`, on arm64
  nodes: a 20Gi ReadWriteOnce volume from the cluster's block storage class
  together with `hostUsers: false`, and the mirror as a sidecar
  (`restartPolicy: Always` on an init container), were accepted and worked.
- Live services: a Backlog project as the tracker; pull requests opened and
  merged in a GitHub repository by the shipped delivery and checked by the
  shipped merged check; models invoked through an OpenAI-compatible gateway,
  chosen at first by OpenRouter's decision model and later by the gateway's
  chat model.
- Replacing the Pod (for a configuration change or a new image) kept the
  queue. A running request resumed with the restart notice on its issue, and
  its interrupted stage started over. One image that introduced a new kind of
  comment posted it on every request already delivered; the queue now records
  when each kind began (SETUP.md, section 10).
- The fixed notices, each on a live issue: the budget pause and its recovery
  (`min_model_credit` set above and then below the key's remaining limit), the
  no-progress notice, a stop and its report, a question and the resumption
  after the answer.
- The status page behind a Service and an Ingress with basic authentication.
  The load balancer answered 504 until the egress rule for replies from port
  9200 was added.
- A rule requiring an approving review on the integration branch made every
  delivery fail with HTTP 405; one request went round 19 times until the rule
  was removed.
- Requests running side by side: before the delivery merged the integration
  branch into the ticket branch first, a conflicting request could not be
  delivered; since then a ticket branch has received that merge commit and
  its pull request merged.

Proposed, or simply not measured. Check each one before trusting it:

- Another cluster, node architecture or storage class, and this volume's
  behaviour when the Pod moves to another node.
- `resources`. The requests and limits are a starting point from spare node
  capacity, not a measured working set. A model-driven build can exceed them.
- The shipped ordered configuration as it stands, with OpenRouter invoked
  directly: the live instance invoked its models through a gateway. Another
  tracker or Git host.
- The delivery credential's real scope, and protection rules other than the
  one met. Neither can be read from inside this repository, and the delivery
  token could not read the protection settings itself.
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

## Before it accepts work, in order

Each step is a check with an observable answer. A step that cannot be
answered is a blocker, not something to note and pass. SETUP.md, sections 6
and 7, gives the commands.

1. **Build the bundle from the commit you intend to run** and record the
   `sha256` of `bin/ticket-engine` and `bin/ticket-tracker`, or take the
   published image whose `engine_sha` in `docs/DISTRIBUTION.json` is that
   commit.
2. **Build and publish the image from that same commit.** The workflow on the
   default branch does this; publishing is an approval point.
3. **Create the Secrets and the three ConfigMaps** (egress rules, operator
   configuration, operator scripts). Keep the intake scope to the one new
   ticket and an acceptance start time, so nothing older is picked up. Then
   run the cluster's server-side dry run on the StatefulSet and confirm that
   admission did not change `hostUsers` or `automountServiceAccountToken`,
   did not add an `envFrom`, and accepted the sidecar entry.
4. **Apply the StatefulSet** (an approval point). Read back `hostUsers`,
   `automountServiceAccountToken`, the container security contexts, a restart
   count of zero, and init container logs with no error. Confirm three things
   that only bite on a replacement node: the init containers name the network
   tool image by a reference every node can pull, with
   `imagePullPolicy: IfNotPresent` and never `Never`; the engine image can be
   pulled without a node's cache, or an imagePullSecret is named here; and
   the temporary volume carries no `sizeLimit`.
5. **Run the launcher check** from `rewrite/RUNTIME.md` ("Check the target
   runtime before accepting work") inside the running Pod: no credential, no
   task data, exit status 0. Then check a role's actual policy: seccomp mode
   2, no capabilities, a refused user namespace, read-only work paths.
6. **Check egress both ways from the Pod**: the tracker, the model provider
   and the delivery service reachable; the metadata address and the cluster's
   own Service and Pod ranges refused. An accepted manifest is not evidence.
7. **Confirm the credentials by name only** — that the variables are set, not
   what they contain.
8. **Connect with no work to do**: the model catalogue, the tracker's issue
   list for the configured project, and a listing of the mirror.
9. **Delete the Pod once.** The same volume must come back with the queue's
   records intact. Record the volume's usage at the same time, so a later
   reading means something.
10. **Run delivery and verification in check mode** (`--dry-run` on both).
    They end non-zero on purpose. A real push, pull request and merge is an
    approval point of its own. Run the operator's `test` once inside the role
    launcher too, against a checkout of the integration branch.
11. **If the project already has another intake**, confirm it has nothing
    running and that the new ticket cannot be picked up by it.
12. **Go live**: the requester opens the ticket, its id goes into the intake
    scope, and the Pod restarts to read it.

## What these templates do not do

They do not create a namespace, a tracker project, a repository, a token or a
secret value (the Secret template carries keys only); they do not expose the
status page unless the operator applies the Ingress; they do not change a
shared network plugin or a node; they do not install a model SDK; and they do
not decide that the syscall profile in the image is the right policy for the
work being admitted. Deleting the volume does not undo anything already
delivered outside the cluster: the queue is the record of what has already
happened.
