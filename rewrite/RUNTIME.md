# Local runtime bundle — not a production deployment

From the standalone module, build into a **new** directory:

```sh
GOOS=linux GOARCH=arm64 sh package.sh /path/to/new-bundle
```

Choose `GOARCH` for the target host; omit `GOOS`/`GOARCH` for the build host.
The script builds `bin/ticket-engine` and `bin/ticket-tracker`, copies the three
configured-command bridges into `harnesses/`, and includes `START.md`, this
document, `README.md` and `examples/operator.json`. Begin with `START.md`; the
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
    "--write", "src", "--write", "tests",
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
are operator choices, not hardcoded project conventions. An empty project can
use `--write .`; this also grants its Git metadata, tests and every existing or
future workspace file. Narrower grants must already exist. Review processes can
omit `--write`; delivery/reporting processes get only their required outputs.
Optional `git_workspace.py -- ...` goes before this launcher, in the trusted
controller context, to prepare an empty checkout. Do not run it as a model tool.

The role starts in an empty filesystem namespace, with system tools, certificate
configuration and explicit runtime paths read-only. Only its workspace, chosen
writable paths, private home, `/proc`, `/dev` and temporary directory are added.
Path components are opened without following symlinks and passed as descriptors
to bubblewrap. The role is PID 1 of its namespace; it cannot create further user
namespaces. Its detached local children die when that role exits. Remote jobs
and prior external effects are not undone. Long-lived delivery services belong
in the authorized delivery runtime, not the agent's temporary process tree.

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
