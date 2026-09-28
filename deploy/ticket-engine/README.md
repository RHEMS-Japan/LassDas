# The permanent runtime for one consumer's role chain

These templates run the bundle from `rewrite/` as a Kubernetes workload that
stays up: the controller is the container's main process, its queue is on a
volume that survives a restart, roles clone from a local mirror instead of
holding a delivery credential, and delivery and verification are fixed
programs rather than something a model may improvise.

| File | What it is |
| --- | --- |
| `statefulset.yaml.example` | the workload: init containers, the mirror, the engine |
| `egress-configmap.yaml.example` | this Pod's own firewall rules |
| operator configuration | `rewrite/examples/operator.json`, adapted per consumer, mounted from a ConfigMap at `/etc/ticket-automation/operator.json` |

Copy the two examples, replace every `<placeholder>`, and keep the result
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

Proposed, or simply not measured. Check each one before trusting it:

- This StatefulSet as a whole. The measurements used short-lived Pods with an
  `emptyDir`. A persistent volume combined with `hostUsers: false` is not
  measured here, and neither is this volume's behaviour when the Pod moves.
- The mirror as a sidecar. `restartPolicy: Always` on an init container was
  not exercised; step 3 below is where the API server tells you.
- `resources`. The requests and limits are a starting point from spare node
  capacity, not a measured working set. A model-driven build can exceed them.
- Every external service. The measured runs used fixture model, tracker and
  delivery services. Nothing here is evidence about a live provider, a live
  tracker, a real pull request or a real merge.
- Protection rules on the delivery target and the real scope of the delivery
  credential. Neither can be read from inside this repository.
- Cost. The engine has no spending limit of its own; the provider account's
  limits are the only bound, and they are outside this manifest.
- Progress reporting. There is none while work runs. The tracker sees the
  final comment, a stop report and any question. A role's launch is bounded
  at one hour.

## Before it accepts work, in order

Each step is a check with an observable answer. A step that cannot be
answered is a blocker, not something to note and pass.

1. **Build the bundle from the commit you intend to run** and record the
   `sha256` of `bin/ticket-engine` and `bin/ticket-tracker`.
2. **Build and publish the image from that same commit.** The workflow on the
   default branch does this; publishing is an approval point.
3. **Create the two ConfigMaps.** Keep the intake scope to the one new ticket
   and an acceptance start time, so nothing older is picked up. Then run the
   cluster's server-side dry run on the StatefulSet and confirm that
   admission did not change `hostUsers` or `automountServiceAccountToken`,
   did not add an `envFrom`, and accepted the sidecar entry.
4. **Apply the StatefulSet** (an approval point). Read back `hostUsers`,
   `automountServiceAccountToken`, the container security contexts, a restart
   count of zero, and init container logs with no error.
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
   records intact.
10. **Run delivery and verification in check mode** (`--dry-run` on both).
    They end non-zero on purpose. A real push, pull request and merge is an
    approval point of its own.
11. **Confirm the consumer's existing intake has nothing running**, and that
    the new ticket cannot be picked up by it.
12. **Go live**: the requester opens the ticket, its id goes into the intake
    scope, and the Pod restarts to read it.

## What these templates do not do

They do not create a namespace, a Service, a board, a secret or a tracker
project; they do not change a shared network plugin or a node; they do not
install a model SDK; and they do not decide that the syscall profile in the
image is the right policy for the work being admitted. Deleting the volume
does not undo anything already delivered outside the cluster: the queue is
the record of what has already happened.
