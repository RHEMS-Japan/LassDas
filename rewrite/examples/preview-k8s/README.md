# A preview of each request on Kubernetes (example)

An example of the two commands the engine's preview setting names: one that
runs a request's checkout where the requester can open it, and one that
removes it again. [preview.py](preview.py) is both (`preview.py up`,
`preview.py down`). It needs Python 3.11 or later with its standard library,
and kubectl; nothing else.

The engine looks for none of the names used here. Another installation can
write its own commands for another place to run a preview (a different
cluster layout, a PR preview service, a host of its own) and name those
instead.

## What the requester sees

Before the question that asks whether to deliver a change that alters what
people see, the engine posts one comment with the preview's signed URL, for
example `https://sample-12.preview.example.invalid/_preview/open?t=1800000000.Ml7L…`,
and until when it can be opened. The requester opens it in a browser, phone
included, and signs in with the shared user name and password they were
given when the installation was set up; the address bar then shows the
preview's own `/`. The URL opens this request's preview only, and only
until its end (see [The signed URL](#the-signed-url)). The requester
answers the question. When the request is delivered or stopped the preview
is removed, and the address shows a page that says the preview has ended and
how to ask for it again. When no preview could be made the question is asked
as before, in words, with one line saying so.

## What runs where

| Where | What |
| --- | --- |
| The engine's Pod | `preview.py up` and `preview.py down`, each started through the role launcher ([linux_role.py](../../harnesses/linux_role.py)) with `--network inherit`, the checkout read-only. |
| The preview namespace | One Deployment and one Service per request, named `pv-<PREVIEW_NAME>-<request number>`. Nothing else is created there by the commands. |
| The router namespace | The password check and the router that sends `<label>.<PREVIEW_DOMAIN>` to the Service `pv-<label>`, set up once by a person. |

`up` does, in order:

1. Takes the credentials out of its environment and starts itself again
   without them (see [Credentials](#credentials)).
2. Checks every setting; reads `PREVIEW_ENV_FILE`; lists the checkout with
   `git ls-files -co --exclude-standard`; packs the listed files into a tar
   archive; computes the change's identifier from the archive and the Pod
   template.
3. Writes a kubeconfig to the launch's `/tmp`. If a Pod of the same change is
   ready already, it only moves the Deployment's lifetime to the new end and
   goes to step 6. Otherwise it applies the Deployment and the Service
   (server-side apply; the same names every time).
4. Waits for the new Pod's `receive` container and streams the archive into
   it with `kubectl exec`. While it waits it reads the Pod about once a
   second: a dependency step (`install`) that ended with a status other than
   0, or was started again, ends the wait at once, so the kubelet does not run
   the step again with public addresses open. A ReplicaSet the cluster
   refused to create a Pod for (a policy, the quota) ends it at once too, with
   the cluster's reason.
5. Waits until the Pod is ready and the Service has a ready endpoint. Steps 4
   and 5 together end at `PREVIEW_WAIT_SECONDS`. When the wait ends for any
   reason (the dependency step, a rules container, the hand-over, a failed
   read, the time) with the Pod short of its last rules container (`net`),
   `up` removes the Deployment first and says so.
6. Makes the signed URL for the preview's end, and opens the public address
   as the requester would: (a) `PREVIEW_READY_PATH` without the password,
   which the router must answer with 401; (b) the same with the check user's
   password (`PREVIEW_CHECK_AUTH`) but without the signed URL's cookie, which
   the router must answer with 403; (c) the signed URL with the password,
   which must answer 302 and set the cookie `__Host-preview`; (d)
   `PREVIEW_READY_PATH` with the password and the cookie, which the
   application must answer with 200 to 399. The router's answers when it has
   nothing to forward to (404, 502, 503) and an unreachable address are
   waited out for 60 seconds.
7. Writes three lines to `TASK_PREVIEW`:

   ```text
   https://sample-12.preview.example.invalid/_preview/open?t=1800000000.Ml7L0aF-Nfihd1zfYSG9eFn5rnEHt8KkUXtD7fqN4bA
   この画面は 2026-10-17 18:00 (日本時間) まで開けます
   動いている変更の識別子: 3f9c0a1b2d4e
   ```

`down` removes the request's Deployment and Service by their label and reads
back that neither is left. It reads nothing of the checkout.

## The role configuration

The two commands are roles outside the stages, named by the engine's preview
setting. As the engine's design writes it:

```json
"workflow": {
  "stages": ["..."],
  "preview": { "up": "preview_up", "down": "preview_down" }
},
"roles": [
  { "name": "preview_up", "purpose": "Run the request's checkout as a preview the requester can open.",
    "processes": [ { "name": "preview-up",
      "command": ["/usr/bin/python3", "-B", "/opt/ticket-automation/bundle/harnesses/linux_role.py",
                  "--runtime", "/opt/ticket-automation/bundle", "--runtime", "/opt/ticket-automation/operator",
                  "--network", "inherit", "--", "/opt/ticket-automation/operator/preview.py", "up"],
      "env": { "PREVIEW_NAME": "sample", "PREVIEW_START": "python3 -m http.server \"$PORT\" --bind 0.0.0.0 --directory site",
               "PREVIEW_PORT": "8080", "PREVIEW_READY_PATH": "/", "PREVIEW_LIFETIME_HOURS": "72",
               "PREVIEW_IMAGE": "python:3.12-slim", "PREVIEW_DOMAIN": "preview.example.invalid",
               "PREVIEW_NAMESPACE": "preview-apps", "PREVIEW_K8S_SERVER": "https://<api-server>",
               "PREVIEW_K8S_CA_FILE": "/opt/ticket-automation/operator/preview-k8s-ca.crt",
               "PREVIEW_NET_IMAGE": "<net-image>" },
      "secrets": { "PREVIEW_K8S_TOKEN": "PREVIEW_K8S_TOKEN", "PREVIEW_CHECK_AUTH": "PREVIEW_CHECK_AUTH",
                   "PREVIEW_SIGNING_KEY": "PREVIEW_SIGNING_KEY" },
      "timeout_minutes": 10 } ] },
  { "name": "preview_down", "purpose": "Remove the request's preview.",
    "processes": [ { "name": "preview-down",
      "command": ["...the same launcher arguments...", "--", "/opt/ticket-automation/operator/preview.py", "down"],
      "env": { "PREVIEW_NAME": "sample", "PREVIEW_NAMESPACE": "preview-apps",
               "PREVIEW_K8S_SERVER": "https://<api-server>",
               "PREVIEW_K8S_CA_FILE": "/opt/ticket-automation/operator/preview-k8s-ca.crt" },
      "secrets": { "PREVIEW_K8S_TOKEN": "PREVIEW_K8S_TOKEN" },
      "timeout_minutes": 3 } ] }
]
```

`preview.py` and the cluster's CA certificate go into the operator-scripts
ConfigMap (see
[operator-scripts-configmap.yaml.example](../../../deploy/ticket-engine/operator-scripts-configmap.yaml.example)),
whose mode makes the script executable. The three credentials go into the
engine's Secret and the StatefulSet's environment like the other keys
([SETUP.md](../../../deploy/ticket-engine/SETUP.md), section 5).

**Time.** The wait `PREVIEW_WAIT_SECONDS` (420 seconds by default) is what
bounds steps 4 and 5: the hand-over, the dependency step and the start of the
application all happen inside it. With `PREVIEW_INSTALL`, it has to be at
least `PREVIEW_INSTALL_SECONDS` + 180, or `up` ends before it calls the
cluster: `up` has to be still watching when a late dependency step fails, to
remove its Deployment. With the defaults (240 + 180 = 420) that holds; a step
of 120 seconds leaves 300 of the 420; a step of 480 needs a wait of 660 or
more. When the wait ends with the Pod short of its last rules container
(`net`), `up` removes the Deployment before it ends 1.

Keep `timeout_minutes` × 60 at least `PREVIEW_WAIT_SECONDS` + 180: the rest
covers listing and packing the checkout, applying and the public check. If
the engine's limit comes first, it stops `up` in the middle of the dependency
step, the Deployment is not removed, and only the cleanup stops the step
(within its 10 minutes). The defaults fit 10 minutes exactly (420 + 180 =
600); a wait of 660 needs 15.

## Settings

Set in the role's `env`. The engine reads none of them.

| Setting | Example | Required | Meaning |
| --- | --- | --- | --- |
| `PREVIEW_NAME` | `sample` | yes | This installation's short name, the first part of every preview's name and host. 1 to 20 lowercase letters, digits and hyphens, starting with a letter. Two installations sharing a namespace need different names: GitHub request numbers are digits only. |
| `PREVIEW_START` | `python3 -m http.server "$PORT" --bind 0.0.0.0 --directory site` | yes (up) | The command that starts the application, run with `sh -c` in the checkout (`/src`). `PORT` is set. |
| `PREVIEW_PORT` | `8080` | yes (up) | The port the application listens on. |
| `PREVIEW_READY_PATH` | `/` | yes (up) | The path that answers once the application is up. It is the readiness probe and the path of the public check. |
| `PREVIEW_LIFETIME_HOURS` | `72` | yes (up) | How long a preview stays after it was last brought up, 1 to 720. |
| `PREVIEW_IMAGE` | `python:3.12-slim` | yes (up) | The image the application runs in. It needs `sh`, `tar` and `sleep`, and `timeout` (coreutils, or BusyBox 1.30 or later) when `PREVIEW_INSTALL` is set. Name it by digest to keep it fixed. |
| `PREVIEW_MEMORY` | `512Mi` | no (`512Mi`) | The memory limit of the application and of the dependency step. At most the preview namespace's per-container maximum (2Gi in the example cluster setup), or the Pod is refused when it is created. |
| `PREVIEW_INSTALL` | `npm ci` | no | A dependency step, run with `sh -c` in the checkout before the application. Only during this step can the Pod reach public addresses. |
| `PREVIEW_INSTALL_SECONDS` | `240` | no (`240`) | The step's time limit: digits only, 1 to 480, and, when `PREVIEW_INSTALL` is set, at most `PREVIEW_WAIT_SECONDS` − 180 (see Time above). Anything else ends `up` before the cluster is called. |
| `PREVIEW_ENV_FILE` | `.env.preview` | no | A file in the checkout, relative to it, of `KEY=VALUE` lines for the application's environment. No secrets: the file is in the branch. At most 64 KiB; a line that is not `KEY=VALUE` (blank lines and `#` lines aside) ends `up`. `PORT` is always the script's. |
| `PREVIEW_WAIT_SECONDS` | `420` | no (420 for up, 60 for down) | How long to wait for the Pod to be ready (up) or for the objects to be gone (down): 1 to 3600. With `PREVIEW_INSTALL`, at least `PREVIEW_INSTALL_SECONDS` + 180. Above 1800 the cleanup, which removes a Deployment that has had no ready Pod for 30 minutes, may remove the preview while `up` still waits. |
| `PREVIEW_DOMAIN` | `preview.example.invalid` | yes (up) | The domain under which every preview gets one label: `sample-12.preview.example.invalid`. The same for every installation using the router. |
| `PREVIEW_NAMESPACE` | `preview-apps` | yes | The preview namespace. |
| `PREVIEW_K8S_SERVER` | `https://<api-server>` | yes | The cluster's API server address. |
| `PREVIEW_K8S_CA_FILE` | `/opt/ticket-automation/operator/preview-k8s-ca.crt` | yes | The cluster's CA certificate (not a secret), by its absolute path. |
| `PREVIEW_NET_IMAGE` | `<net-image>` | yes (up) | The image of the containers that load the egress rules, by digest (see [The rules image](#the-rules-image)). |

The engine gives each command `TASK_WORKSPACE` (the checkout), `TASK_HOME`,
`TASK_ISSUE` (the request number), `TASK_PREVIEW` (where `up` writes its
three lines) and `TASK_CREDENTIAL_NAMES`; the launcher gives `TMPDIR=/tmp`,
memory of this launch only.

Names: `pv-<PREVIEW_NAME>-<request number>`, the number lowercased and every
run of other characters made one hyphen (`PROJ-81` becomes `proj-81`). A
name longer than 63 characters is cut and ends in eight characters of its
hash, so two long request numbers never share a preview.

## Credentials

| Name | What | Given to |
| --- | --- | --- |
| `PREVIEW_K8S_TOKEN` | The token of the ServiceAccount that may manage Deployments and Services in the preview namespace, and nothing outside it. | up and down |
| `PREVIEW_CHECK_AUTH` | `user:password` of the router's second user, kept for this check. The requesters' user is a different one and is never given to the engine. | up |
| `PREVIEW_SIGNING_KEY` | The key the signed URLs are signed with, the same value the router holds (at least 32 characters; `openssl rand -base64 32` makes 44). | up |

What the command does with them:

- Before anything else it removes every name listed in
  `TASK_CREDENTIAL_NAMES` (and these three, listed or not) from its
  environment, writes the values into a pipe and starts itself again
  without them, reading them from the pipe. `/proc/self/environ` shows the
  environment a program started with, so removing a value is not enough;
  after the restart the running program's environment holds none.
- git and kubectl start with `PATH`, `LANG` and a temporary `HOME` only;
  kubectl also gets `KUBECONFIG`.
- The kubeconfig is written with mode 0600 into a new directory under the
  launch's `/tmp`, after git has run, and removed at the end. The launcher's
  `/tmp` goes with the sandbox, also when the engine kills the launch at its
  time limit. It is never written under `TASK_HOME`, which outlives the
  launch.
- Every line it prints has the credential values (and the encoded check
  header) replaced by `[credential]`. The engine replaces them as well. It
  never prints the signed URL: that goes to `TASK_PREVIEW` only.
- The signing key is used in memory only, to sign; it is written nowhere.

## The signed URL

Every preview's address carries a signature, so knowing the domain (and the
shared password) is not enough to open another request's preview, and an
address stops opening at the preview's end.

- **Token:** `<end>.<signature>`. `<end>` is the preview's end in Unix
  seconds, the same moment as the lifetime the Deployment carries.
  `<signature>` is HMAC-SHA256 with `PREVIEW_SIGNING_KEY` over the host, a
  newline and `<end>` (`sample-12.preview.example.invalid\n1800000000`),
  written in base64url **without `=` padding**: 43 characters of
  `A-Z a-z 0-9 - _`. The host is lowercase and has no port, as nginx's
  `$host` is.
- **URL:** `https://<host>/_preview/open?t=<token>`, written by `up` as the
  first line of `TASK_PREVIEW`. A lifetime extended (the same change brought
  up again) gets a new URL with the new end; the old one stops at the old
  end.
- **The router** checks it after the password: the signature made again
  from the request's `Host` and `<end>` has to match (compared in constant
  time), and `<end>` has to be later than now. Then it sets the cookie
  `__Host-preview` (Secure, HttpOnly, SameSite=Lax, Path=/, no Domain, until
  the end) and answers 302 to `/`, with `Referrer-Policy: no-referrer`.
  Every other path needs the password and that cookie; without them it
  answers 401 and 403. Paths under `/_preview/` belong to the router and
  never reach the application.

## Exit status and output

| Status | When |
| --- | --- |
| 0 | `up`: the preview answers on its public address and `TASK_PREVIEW` is written. `down`: neither the Deployment nor the Service of the request is left, also when there was none. |
| 1 | Anything else. The last line of the output is the reason, with the cluster's own message where it gave one (a policy refusal names the policy). `TASK_PREVIEW` is not written. |
| 2 | A wrong command line. The engine never starts it so with this configuration. |

After a failure nothing is removed, except a Deployment whose Pod had not
reached its last rules container (`net`) when the wait ended, for whatever
reason: until then the dependency step could still run with public addresses
open. `down` and the cleanup remove the rest.

## What it never reads

A role can leave symbolic links in the checkout, `-> /proc/self/environ` for
one. Nothing of the checkout is read through one:

- Each listed path is opened one directory at a time without following
  links. A link is packed as a link (its target text only); a path under a
  linked directory is left out; anything that is neither a regular file nor
  a link is left out. Archive entries carry no owner and no time, so the
  same files give the same archive.
- `PREVIEW_ENV_FILE` is read the same way, and a link there, or a linked
  directory on its way, ends `up`: it is not packed as a link instead.
- In the Pod, a link is unpacked as a link. Following it there finds only
  the preview Pod's own files, which hold no credential.

## The shape the cluster accepts

The Deployment `up` sends, and nothing else of it varies but the settings:

- Strategy `Recreate`, one replica: a new change stops the old Pod before the
  new one starts, so a preview never takes the quota twice.
- The lifetime (`preview-expires-at`, RFC 3339 in UTC) and the request number
  on the Deployment itself; the change's identifier (`preview-change`) on the
  Pod template, so extending the lifetime does not replace the Pod and a new
  change does.
- The init containers `net-hold`, `receive`, `net`, or with `PREVIEW_INSTALL`
  `net-hold`, `receive`, `net-install`, `install`, `net`: the running rules
  first and last, the dependency step's rules only around the step. The step
  runs as `timeout -s KILL <seconds> sh -c "$PREVIEW_INSTALL"`: KILL, because
  a step that ignores TERM would keep running past the limit.
- No ServiceAccount token, no Service links, its own user namespace
  (`hostUsers: false`), seccomp `RuntimeDefault`, no priority class and no
  priority (0, so it displaces no Pod of priority 0 or more), a termination
  grace of 5 seconds, amd64 nodes. It names no `preemptionPolicy`: the
  cluster's Priority admission refuses a Pod whose `preemptionPolicy`
  differs from its class's, and without a class that is
  `PreemptLowerPriority`.
- `net-*` run as root with `NET_ADMIN` only and a read-only root. Every
  other container runs as a user other than root, without privilege
  escalation and with every capability dropped. Memory, CPU and local
  storage are limited in every container.
- The volumes are emptyDirs (the checkout's holds 1 GiB) and the ConfigMap
  `preview-egress`. No Secret, no projected token, no host path.
- The Service is a ClusterIP Service from port 80 to `PREVIEW_PORT`, with a
  selector and no annotations.

The cluster's admission policy accepts this shape and refuses the ones that
would let the token's holder run a privileged Pod, reach the node, take the
node's network or publish a load balancer.

## The rules image

`net-hold`, `net-install` and `net` load the egress rules of the ConfigMap
`preview-egress` with `iptables-restore --wait 5 /policy/<file>`, which
replaces the whole filter table each time. The image is `PREVIEW_NET_IMAGE`,
named by digest; it needs `iptables-restore` on its `PATH` and must run on
the preview's nodes. These commands are fixed in `NET_COMMANDS` in
[preview.py](preview.py), and the admission policy accepts exactly them:
when the program sits elsewhere in a new image, change both together, then
the digest.

## Limits

- The checkout lives in the Pod's emptyDir. A Pod moved to another node (the
  node replaced, a spot node reclaimed) starts empty and never gets ready;
  the cleanup removes it, and the requester asks for the preview again.
- One token for every installation sharing the preview namespace: one
  installation's token can replace or remove another's previews.
- The time in the result's second line is Japan time.
- Only IPv4 rules are loaded. A cluster that gives Pods IPv6 needs the same
  rules for ip6tables.
- What the dependency step downloads is not checked. During the step the Pod
  can reach any public address, for at most `PREVIEW_INSTALL_SECONDS` per
  attempt.
- The signed URL is posted on the tracker: whoever can read the request and
  knows the shared password can open that request's preview until its end.
  It also stays in the engine's own records of the request (its history and
  its notices), in the requester's browser history, and in a load balancer
  access log if one is turned on. Changing the signing key is the
  only way to stop a URL before its end, and it stops every open preview.
- An application cannot use paths under `/_preview/`: the router keeps them.
