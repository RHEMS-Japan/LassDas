# Local runtime bundle — not a production deployment

From the standalone module, build into a **new** directory:

```sh
GOOS=linux GOARCH=arm64 sh package.sh /path/to/new-bundle
```

Choose `GOARCH` for the target host; omit `GOOS`/`GOARCH` for the build host.
The script builds `bin/ticket-engine`, `bin/ticket-tracker` and `bin/ticket-status`,
copies the configured-command harnesses, operator examples and third-party notices,
and includes `START.md`, this document and `README.md`. Begin with `START.md`; the
example leaves the intake scope unset and must be adapted before accepting work.
It refuses an existing output path and retains partial output on build failure.
It does not install an SDK, fetch an image, start a daemon, activate intake,
set up credentials, change an existing service, or select a production policy.
Go, its ordinary build cache and this module's source are needed at build time.
The Go commands are static by default; Python/SDK dependencies are not bundled.

## HTTPS with a private certificate authority

Keep TLS verification enabled. Go 1.27 supports `SSL_CERT_FILE` on macOS;
older builds may ignore it and use the system keychain. A bundle built with
Go 1.27.1 was exercised with `SSL_CERT_FILE` pointing to public roots plus the
test service's public certificate, and `GODEBUG=x509sslcertoverrideplatform=1`
because this module retains an older Go compatibility baseline. The actual
tracker command read the configured service, rejected it without that added
certificate, and the engine fetched the live model catalog. No OS trust store
or SDK installation was changed. This is connection evidence, not fulfillment.
See the [Go certificate-pool documentation](https://pkg.go.dev/crypto/x509#SystemCertPool).
The environment must be configured for the controller and, separately, any
working SDK that needs it; role environments do not inherit controller values.

## Linux role launch

`harnesses/linux_role.py` runs a configured command using the installed
[bubblewrap](https://github.com/containers/bubblewrap/blob/main/bwrap.xml).
It does not parse the command's answer. Stdin, stdout, stderr and exit status
pass through. Setup failure is a process failure visible to the existing chain,
not permission to fall back to unconfined execution or declare the request done.

The target needs Python 3.9+, a maintained bubblewrap with `--bind-fd`,
`--ro-bind-fd`, `--as-pid-1` and `--disable-userns`, and permission to create user,
mount and PID namespaces and mount a private `/proc`. The installed native SDK
has its own Python/version requirements. The offline observation used Linux
arm64, bubblewrap 0.8.0 and an already-installed SDK, not a newly built runtime.

Keep the bundle, SDK, controller, queue parent and operator configuration outside
model-writable directories. Watch mode supplies absolute `TASK_WORKSPACE` and
per-process `TASK_HOME`; standalone invocation must supply them explicitly.
They must be separate real paths, not symlinks or ancestors of one another.
The launcher creates the private home if needed, not missing project paths.
The native bridge seeds the SDK's existing `TERMINAL_CWD` from `TASK_WORKSPACE`
before SDK startup, unless explicitly configured already. Process cwd alone
did not keep the tested SDK's terminal in the workspace. Explicit native config
still controls its tools; do not configure an unrelated host or workspace there.

Example **process** entry when an operator installs the bundle at `/opt/engine`
and the SDK at the illustrated paths:

```json
{
  "name": "implementation",
  "command": [
    "python3", "/opt/engine/harnesses/linux_role.py",
    "--runtime", "/opt/engine/harnesses",
    "--runtime", "/opt/hermes",
    "--runtime", "/opt/hermes-src",
    "--write", ".",
    "--network", "inherit", "--",
    "/opt/hermes/bin/python", "/opt/engine/harnesses/hermes.py"
  ],
  "env": {"PYTHONPATH": "/opt/hermes-src", "TERMINAL_ENV": "local"},
  "model_env": "NATIVE_MODEL",
  "secrets": {"OPENROUTER_API_KEY": "WORKER_MODEL_KEY"}
}
```

This fragment is not a complete engine configuration: configure the model
selection, endpoint and workflow as described in the module README. Do not put
credentials in literal environment settings. The paths and writable directories
are operator choices, not hardcoded project conventions. Which files a change
needs is not known before the work, so the examples grant the implementation
`--write .`: every existing or future workspace file. The checkout's own `.git`
stays read-only under a grant that covers it unless `.git` is named, because
hooks and configuration there run as whatever process opens the repository
next, and the delivery process opens it holding the credential no role may
have; only the delivery process names `.git`. Narrower grants must already
exist. Review processes can omit `--write`; delivery/reporting processes get
only their required outputs.
Optional `git_workspace.py -- ...` goes before this launcher, in the trusted
controller context, to prepare an empty checkout. Do not run it as a model tool.

The role starts in an empty filesystem namespace, with system tools (`/usr` and
the system's `/etc/alternatives` links into it), certificate configuration and
explicit runtime paths read-only. Only its workspace, chosen
writable paths, private home, `/proc`, `/dev` and temporary directory are added.
Path components are opened without following symlinks and passed as descriptors
to bubblewrap. The role is PID 1 of its namespace; it cannot create further user
namespaces. Its detached local children die when that role exits. Remote jobs
and prior external effects are not undone. Long-lived delivery services belong
in the authorized delivery runtime, not the agent's temporary process tree.

### Memory

A role's processes share the controller's container, and so its memory limit.
On cgroup v2 the kubelet (1.28 and later, unless `singleProcessOOMKill` is set)
asks the kernel to stop every process of a container once it runs out of
memory, so a role's build that outgrows the limit used to stop the controller
too, and the restarted controller ran the same stage again.

When the launcher finds a memory limit for its own cgroup (the `0::` line of
`/proc/self/cgroup` under `/sys/fs/cgroup`, with or without a cgroup namespace
of its own), it stays as bubblewrap's parent and guards the role:

- It reads the container's memory in use, leaving out what the kernel takes
  back before it stops any process: file cache
  (`active_file`, `inactive_file`) and reclaimable kernel caches
  (`slab_reclaimable`, the directory and inode entries a checkout or build
  leaves behind). Anonymous and shared memory, kernel stacks, page tables and
  unreclaimable kernel memory stay in. It looks again before the use could
  reach the threshold growing at 32 GiB/s (the fastest one process filled
  memory in a measurement, below), but at least every tenth and at most every
  hundredth of a second: far from the threshold ten times a second, close to
  it a hundred. File cache does not make it look more often.
- Once less than the headroom is left, it stops (SIGKILL) the largest role
  process in the container if that process is its role's. Concurrent roles'
  launchers agree on the same process. The role's other processes continue:
  a build tool sees its compiler stopped, a model's terminal sees a killed
  command, and the role decides what to do next. It looks again a hundredth
  of a second later, counting a stopped process that is still ending as
  already gone, so a second compiler growing beside the first is stopped in
  turn if the use is still over the threshold.
- It writes one line naming the process, its size, the role's total and the
  memory in use: on the launcher's standard error, which is the role's record,
  and, when the stopped process's own standard error is a pipe or a terminal
  other than that and other than the role's own standard output (its
  answer), there too, so the tool that started it shows why it ended (a file
  is never written to, and a full pipe is skipped). A tool started by a
  Node.js program has a socket there, which cannot be opened this way: that
  tool sees only the stop, and the record has the line. The next role's
  prompt carries only the last 4,000 characters of a record's diagnostics, so
  a role that writes much after the stop can push the line out of that prompt;
  the record keeps it.
- When the anonymous memory outside every role's processes (the controller's,
  for example) is over the threshold by itself, stopping role processes would
  not bring the use back under the threshold: it says so once and stops none
  until the use reaches the limit less half the headroom (5,760 MiB of 6 GiB),
  where it stops the largest role process anyway. In that state it reads the
  process table once a second, not at every look.
- A stop signal sent to the launcher is passed on to bubblewrap, and the
  launcher waits for bubblewrap and the sandbox's first process before it ends
  by the same signal, so a cancelled role leaves no process for the controller
  to collect. Only a SIGKILL to the launcher alone, which it cannot handle,
  leaves bubblewrap and that process behind.

The guard's own CPU, per running role, as measured (one CPU, 51 processes in
the container, the role idle for 10 seconds):

| Container state | 6 GiB | 2 GiB |
|---|---|---|
| Nothing else in it | 0.21% | 0.53% |
| File cache read (2.5 GiB in 6 GiB; little in 2 GiB) | 0.42% | — |
| File cache written up to over the threshold (5.6 of 6 GiB, 1.7 of 2 GiB) | 0.32% | 0.53% |
| Memory in use 3 GiB | 0.42% | — |
| Memory in use 5 GiB, 350 MiB under the threshold | 1.89% | — |
| Memory outside every role over the threshold by itself | 1.68% | 1.47% |

The headroom is `--memory-headroom MIB`; unset, it is an eighth of the limit
and at least 512 MiB (768 MiB of 6 GiB). A role can use at most the limit less
the headroom, while the controller's own memory counts against the same limit.

When the guard cannot run, the launcher says so on the role's standard error,
`(launcher) The memory guard is off for this role: <reason>.`, and starts the
role as before: no cgroup v2 line, a cgroup whose memory files cannot be read,
or a limit too small for the headroom. A container without a limit (`max`)
has nothing to guard and says nothing.

Where the kernel stops one process at a time (Docker, cgroup v1, a kubelet
with `singleProcessOOMKill`), every role process gets `oom_score_adj` 1000, so
the kernel stops a role's process first. Where the container's cgroup has
`memory.oom.group` set, the role keeps the controller's score: a higher one
would only make the whole container the node's first choice when the node
runs short of memory.

The guard looks and stops; it reserves nothing and is not a limit per process
or per role. Growth faster than the headroom per look can still reach the
limit before the guard sees it:

- one process that takes a very large block at once and touches it at
  memory speed. With transparent huge pages set to `always` (the kernel's
  `/sys/kernel/mm/transparent_hugepage/enabled`), a single process filled
  memory at 22 to 33 GiB/s in a measurement, the 768 MiB headroom of a 6 GiB
  container in about 30 ms. The guard stopped a single 6,000 MiB block in a
  6 GiB container 9 times out of 9 (at 5,389 to 5,599 MiB) and 1,800 MiB in
  2 GiB 3 times out of 3; faster filling (several threads, faster memory) can
  still pass it. With `madvise` or `never`
  (common defaults) pages are filled more slowly;
- several processes that together grow by more than the headroom between two
  looks;
- memory no process holds (a role's `/tmp` is in memory), which goes only
  when the role ends; each look then stops the next largest role process, at
  worst the role's own command.

A larger headroom (`--memory-headroom`) widens the margin for all three, at
the cost of memory a role can use.

No per-process limit (`RLIMIT_DATA` or `RLIMIT_AS`) is set. Both count address
space a program reserves without using: under `RLIMIT_DATA` at the limit less
the headroom, programs built with AddressSanitizer and ThreadSanitizer failed
to start (they reserve their shadow memory that way), and under `RLIMIT_AS`
even of 8 GiB, so did Node.js with WebAssembly and Chromium. How many
compilers a build runs at once is the role's own environment setting (for
example `CARGO_BUILD_JOBS` for Cargo), not something the launcher sets.

## Check the target runtime before accepting work

An installed `bwrap` and an enabled user-namespace sysctl do not establish that
the outer container permits a private `/proc`. An actual prepared Kubernetes
runtime rejected even `/bin/true` with `Can't mount proc ... Operation not
permitted`. Copying the executable into that runtime would not fix the problem.

Run the **shipped launcher** in the intended execution environment, without
credentials or task data. Adjust the bundle path to its installed location:

```sh
(
  runtime_probe_dir=$(mktemp -d /tmp/ticket-runtime-check.XXXXXXXX) || exit
  mkdir "$runtime_probe_dir/work" "$runtime_probe_dir/home" || exit
  runtime_probe_status=0
  env -i PATH=/usr/bin:/bin \
    TASK_WORKSPACE="$runtime_probe_dir/work" TASK_HOME="$runtime_probe_dir/home" \
    python3 -B /opt/ticket-automation/bundle/harnesses/linux_role.py \
      --network none -- /bin/true || runtime_probe_status=$?
  rmdir "$runtime_probe_dir/home" "$runtime_probe_dir/work" "$runtime_probe_dir"
  exit "$runtime_probe_status"
)
```

This checks namespace/path launch only, not SDK compatibility, model access,
network policy or delivery. A failure must be resolved in the runtime before
enabling intake; do not remove PID isolation or bind the controller's `/proc`
as an implicit fallback.

For Kubernetes nested runtimes, [unmasked proc mounts](https://kubernetes.io/docs/tasks/configure-pod-container/security-context/#managing-proc-mounts)
require a [Pod user namespace](https://kubernetes.io/docs/concepts/workloads/pods/user-namespaces/)
(`hostUsers: false`). The node/runtime and volume filesystems must support it.
In a short-lived, credential-free diagnostic Pod, `procMount: Unmasked` with
the default seccomp profile still rejected nested namespace creation; the same
no-op launch succeeded with diagnostic `seccompProfile: Unconfined`, while
retaining non-root execution, dropped capabilities, no-new-privileges, a
read-only root and the Pod's user namespace. Both probe Pods were removed.
This identifies a runtime requirement, **not** a production security-profile
recommendation. Select and validate the deployment's allowed syscall/network
policy explicitly; do not enable untrusted work by copying diagnostic relaxations.

## Network and supervision are operator prerequisites

Default `--network none` cannot reach the controller's loopback or the provider.
Explicit `--network inherit` shares the **entire** parent network namespace;
it is **not** a provider allowlist or a per-role network boundary. It also exposes
reachable loopback services. The caller still owns credential minimization and
an appropriate outer egress/proxy policy. Do not enable live untrusted work with
broad controller credentials or unrestricted reachable management services.

An ordinary Docker default profile did not allow this nested launcher in the
observed runtime. The offline test required both `seccomp=unconfined` and
`systempaths=unconfined`, while retaining a non-root UID, dropped capabilities,
no-new-privileges, read-only root and a completely disabled outer network.
Those diagnostic relaxations are **not** a recommended production profile and
are not applied by the bundle. See Docker's
[seccomp documentation](https://docs.docker.com/engine/security/seccomp/) and
BuildKit's [nested rootless runtime notes](https://github.com/moby/buildkit/blob/master/docs/rootless.md).
Supported host security policy and live network isolation remain deployment work.

Creating a Kubernetes `NetworkPolicy` object does not prove that the network
plugin enforces it: whether it does depends on the cluster's network plugin and
its configuration, and a policy that is accepted but not enforced lets every
connection through. Before live credentials or untrusted work are admitted,
test both required allowed traffic and forbidden traffic from the actual
workload; do not infer isolation from accepted YAML, Pod readiness, or an
otherwise successful workflow. Changes to a shared network plugin have their
own deployment scope and must not be silently applied by this bundle.

An own-workload-only alternative does not depend on the network plugin, and it
held in a measured trial: an init container set IPv4/IPv6 firewall rules in its
Pod's network namespace, with `NET_ADMIN` confined to that setup container. No
host network or node mount was used. Required DNS and public connectivity stayed
available; link-local metadata connections were refused before and after the
full workflow and restart. Workers had no capabilities. This does not provide a
public-domain allowlist or permission to access arbitrary public services.

The same nested runtime also ran the actual SDK workflow with a native-ABI,
zero-capability adaptation of the pinned
[Moby seccomp policy](https://github.com/moby/profiles/blob/85e237f1fe229a0c61c9c7d8e743fa780d3b97ca/seccomp/default.json)
loaded through [bubblewrap's existing `--seccomp` option](https://github.com/containers/bubblewrap/blob/main/bwrap.xml).
A separate kernel-level probe observed worker seccomp mode 2, no capabilities,
no-new-privileges, refusal of another user namespace and read-only work paths.
The outer trusted controller still needed the diagnostic unconfined profile;
this is not a claim that Kubernetes `RuntimeDefault` worked unchanged. These
runtime-policy adaptations are not installed or generated by this bundle.

Run the controller under a supervisor that owns its workers' lifetime and retains
the run directory. A separate experiment demonstrated controller-as-container-
PID-1 crash containment and automatic runtime restart; this bundle does not
install that supervisor. A surviving unrelated init can leave old work running.
Retained unfinished histories are needed for restart; deleting them can repeat
external actions. Neither history nor a namespace provides exactly-once remote
delivery. Machine/daemon loss and live overnight delivery remain unverified.

## Observed controller command and crash recovery

For this tested containment layout, make the controller the runtime's actual
main process. Do not assume an unrelated surviving init will reap its work.
For a prepared runtime, the controller command is:

```sh
exec /opt/engine/bin/ticket-engine \
  --config /etc/ticket-automation/operator.json \
  --watch --run-dir /var/lib/ticket-automation/queue
```

These are example installation paths. Keep the queue durable and private from
roles, give the controller only configured credentials, and use the authorized
runtime's restart policy. This command alone does not provide the SDK, security
profile, networking or a deployment. Do not add a second application restart
loop. A one-request command ending is different from the continuous collector.

An offline test used the built executable as container PID 1, actual Git
preparation, native SDK/tools, two parallel review seats and the bundled tracker
helper. Fixture APIs supplied a ticket, routing/model responses and delivery
service. After the final comment was stored but before its receipt returned,
SIGQUIT crashed the actual controller with exit 2. The existing runtime's
`on-failure` policy restarted it without a test-issued start/restart command.
The old detached tool stopped, accepted source text survived a later remote edit,
the service's delivered bytes remained unchanged, and reporting read back the
one stored comment without another POST. Five completed native tool invocations
were checked for actual successful results; the interrupted posting invocation
had no returned result. This demonstrates one crash/reconciliation path in the
packaged watch wiring, not independent model reasoning or general exactly-once
delivery. No live tracker, provider, delivery target or daemon restart was used.
The outer sandbox limitations above still apply; `on-failure` does not by itself
restart a container after daemon restart.

A separate credential-free Kubernetes Job ran the packaged ten-action operator
example using the actual engine, installed native SDK and fixture model/tracker/
delivery APIs. The controller was PID 1; a crash after comment storage caused one
automatic container restart, followed by readback rather than a second post.
All eleven completed native tool calls succeeded, the accepted original request
survived, and one delivery/one comment remained. The Job, its Pod and the
`NetworkPolicy` object made for this run were removed afterward. This confirms
that bounded integration/recovery path, not live model judgment, production
delivery, or effective egress isolation. It still used the diagnostic syscall
profile described above, not an approved production profile.
