# Local runtime bundle — not a production deployment

From the standalone module, build into a **new** directory:

```sh
GOOS=linux GOARCH=arm64 sh package.sh /path/to/new-bundle
```

Choose `GOARCH` for the target host; omit `GOOS`/`GOARCH` for the build host.
The script builds `bin/ticket-engine` and `bin/ticket-tracker`, copies the three
configured-command bridges into `harnesses/`, and includes this document.
It refuses an existing output path and retains partial output on build failure.
It does not install an SDK, fetch an image, start a daemon, activate intake,
set up credentials, change an existing service, or select a production policy.
Go, its ordinary build cache and this module's source are needed at build time.
The Go commands are static by default; Python/SDK dependencies are not bundled.

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
