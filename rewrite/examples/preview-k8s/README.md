# A preview of each request on Kubernetes (example)

An example of the two commands the engine's preview setting names: one that
runs a request's checkout where the requester can open it, and one that
removes it again. [preview.py](preview.py) is both (`preview.py up`,
`preview.py down`), and prints a preview's address for a build that has to
know it (`preview.py origin`). It needs Python 3.11 or later with its standard library,
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
| The router namespace | The password check and the router that sends `<label>.<PREVIEW_DOMAIN>` to the Service `pv-<label>`, the identities, and the cleanup that removes what is past its lifetime, set up once by a person ([What a person sets up once](#what-a-person-sets-up-once)). |
| The cluster | An admission policy that lets only the shape below into the preview namespace. |

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
   read, the time) before the Pod's last rules container (`net`) has ended
   with status 0, `up` removes the Deployment first and says so.
6. Makes the signed URL for the preview's end, and opens the public address
   as the requester would: (a) `PREVIEW_READY_PATH` without the password,
   which the router must answer with 401; (b) the same with the check user's
   password (`PREVIEW_CHECK_AUTH`) but without the signed URL's cookie, which
   the router must answer with 403; (c) the signed URL with the password,
   which must answer 302 and set the cookie `__Host-preview`; (d)
   `PREVIEW_READY_PATH` with the password and the cookie, which the
   application must answer with 200 to 399. What the router and the load
   balancer answer when they have nothing to forward to yet (404, 502, 503,
   504), and an unreachable address, are waited out for 60 seconds, in (a) as
   in (d).
7. Writes three lines to `TASK_PREVIEW`:

   ```text
   https://sample-12.preview.example.invalid/_preview/open?t=1800000000.Ml7L0aF-Nfihd1zfYSG9eFn5rnEHt8KkUXtD7fqN4bA
   この画面は 2026-10-17 18:00 (日本時間) まで開けます
   動いている変更の識別子: 3f9c0a1b2d4e
   ```

`down` removes the request's Deployment and Service by their label and reads
back that neither is left. It reads nothing of the checkout.

## What a person sets up once

Everything here is done once per cluster, by someone allowed to change it,
and is shared by every installation that uses the same preview namespace.
None of it is done by the engine or the preview commands. Steps 3 to 7 change
the cluster, and step 5 restarts the engine: do each only with the go-ahead
of whoever owns them.

The files beside this README hold `<placeholder>`s: copy each, replace every
placeholder (see [Values a person fills in](#values-a-person-fills-in)), and
keep the filled copies with your installation's private notes, not in a
public repository.

| File | What |
| --- | --- |
| [namespaces.yaml.example](namespaces.yaml.example) | The preview namespace and the router namespace. |
| [preview-namespace.yaml.example](preview-namespace.yaml.example) | The quota (five previews, 3 GiB of memory between them), the default limits and the egress rules `preview-egress`. Nothing else is ever put there by a person. |
| [admission-policy.yaml.example](admission-policy.yaml.example) | The admission policies for Deployments, Pods and Services in the preview namespace, and their bindings. |
| [router.yaml.example](router.yaml.example) | The router (nginx with the password check and the signed URL's check), its egress rules, its Service and the one Ingress for `*.<preview-domain>`; and the relay of an API, for previews whose page reads one (see [Relaying an API](#relaying-an-api)). |
| [preview-router.js](preview-router.js) | The router's check of the signed URLs, for nginx's njs module (see [The signed URL](#the-signed-url)), and the relay's key. |
| [access.yaml.example](access.yaml.example) | The ServiceAccount of the preview commands with its token and Role, and the cleanup's ServiceAccount and Role. |
| [cleanup.yaml.example](cleanup.yaml.example) and [cleanup.sh](cleanup.sh) | The CronJob that removes what is past its lifetime, never got ready, or whose dependency step restarted, every ten minutes. |
| [dry_run.py](dry_run.py) | The check of the policy, with dry runs only (see [Checking the policy](#checking-the-policy)). |

Example values used below: the domain `preview.example.invalid`, the namespaces
`preview-apps` (previews) and `preview-router` (router), five previews at
once, a lifetime of 72 hours, 3 GiB of memory in all.

1. **A certificate and a DNS name for `*.<preview-domain>`.** Every preview
   is one label under the domain (`sample-12.preview.example.invalid`), so one
   wildcard certificate and one wildcard DNS record serve them all and no
   request ever creates a DNS record. A certificate for `*.example.invalid` does
   not cover `sample-12.preview.example.invalid`: a wildcard covers one label.
   Choose a domain whose name says nothing internal (the address is posted on
   the tracker) and whose registrable part runs nothing else of yours, as
   `example.invalid` in these examples: a preview runs a branch's code in the
   requester's browser. Under a domain your other services share, its
   JavaScript could set cookies for that whole domain, would receive the
   cookies those services set for it, and would send them requests that count
   as same-site. With AWS Certificate Manager, in the load balancer's region:

   ```sh
   aws acm request-certificate --region <region> \
     --domain-name '*.<preview-domain>' --validation-method DNS
   ```

   Create the validation CNAME record ACM names in the public zone that holds
   the domain, wait until the certificate is `ISSUED` (the load balancer
   controller's documentation says validation can take up to 30 minutes), and
   keep the record: ACM renews a DNS-validated certificate with it. The
   certificate's ARN is `<certificate-arn>`.
2. **The policy first.** Apply the namespaces, then the preview namespace's
   objects and the admission policy, before anything else runs there:

   ```sh
   kubectl apply -f namespaces.yaml
   kubectl apply -f preview-namespace.yaml
   kubectl apply -f admission-policy.yaml
   ```

3. **The router.** Make the password file with two users: one for the
   requesters, whose password you hand to them outside the tracker, and one
   for the preview command's check (`PREVIEW_CHECK_AUTH`), which only the
   engine gets. Each line is `user:hash`. `openssl passwd -6` makes a SHA-512
   crypt hash, which nginx checks with the C library's crypt: glibc and musl,
   the C libraries of Linux nginx images, read it. (`openssl passwd -apr1`
   makes a weaker hash nginx reads by itself everywhere.)

   Then the signing key of the signed URLs, and the router's script. The key
   goes to the router here and to the engine in step 5; keep the file only
   until then:

   ```sh
   printf '%s:%s\n' requester "$(openssl passwd -6)" > htpasswd
   printf '%s:%s\n' preview-check "$(openssl passwd -6)" >> htpasswd
   kubectl -n <router-namespace> create secret generic preview-router-auth --from-file=htpasswd
   rm htpasswd
   openssl rand -base64 32 | tr -d '\n' > signing-key
   kubectl -n <router-namespace> create secret generic preview-router-signing --from-file=key=signing-key
   kubectl -n <router-namespace> create configmap preview-router-js --from-file=preview-router.js
   kubectl apply -f router.yaml
   ```

   If a preview's page reads an API through the router, fill in the relay
   (see [Relaying an API](#relaying-an-api)) and put its key in the Secret
   `preview-relay` before applying the router:

   ```sh
   kubectl -n <router-namespace> create secret generic preview-relay --from-file=key=relay-key
   rm relay-key
   ```

   The router starts without that Secret too (its reference is optional);
   the relay then answers 503 and sends nothing.

   Otherwise delete every block between `relay: begin` and `relay: end` in
   the copy of [router.yaml.example](router.yaml.example): what is left is
   the router without a relay.

   The nginx image needs the njs module (loaded from
   `/etc/nginx/modules/ngx_http_js_module.so`) and the auth_request module;
   the official images have both. If the image has njs built in, drop the
   `load_module` line.

   The Ingress makes one internet-facing load balancer (it costs while it
   exists). When it has an address
   (`kubectl -n <router-namespace> get ingress preview-router`), create the
   DNS record `*.<preview-domain>` as an alias (or CNAME) of that address by
   hand. The Ingress carries no external-dns annotation on purpose: a record
   external-dns made would not be removed with it.
4. **The rules image.** The router's init container `net` loads its rules
   with the same image and the same program as the previews' `net-hold`,
   `net-install` and `net`. When the router's Pod runs, the program is there
   and loads a rules file. If `net` fails
   (`kubectl -n <router-namespace> logs deploy/preview-router -c net`), the
   program is somewhere else in the image: change the command in
   [router.yaml.example](router.yaml.example), `NET_COMMANDS` in
   [preview.py](preview.py) and the two command lists of the policy together
   (see [The rules image](#the-rules-image)).
5. **The identities.** Apply [access.yaml.example](access.yaml.example). Put
   the token of `preview-operator` into the engine's Secret as
   `PREVIEW_K8S_TOKEN`, the check user's `user:password` as
   `PREVIEW_CHECK_AUTH` and the content of `signing-key` as
   `PREVIEW_SIGNING_KEY`, add the three to the StatefulSet's environment the
   way the other keys are added
   ([SETUP.md](../../../deploy/ticket-engine/SETUP.md), section 5), remove
   `signing-key`, and restart the engine: it reads its keys when it starts.
6. **The cleanup.**

   ```sh
   kubectl -n <router-namespace> create configmap preview-cleanup --from-file=cleanup.sh
   kubectl apply -f cleanup.yaml
   ```

7. **The commands.** Add `preview.py` and the cluster's CA certificate (the
   file `PREVIEW_K8S_CA_FILE` names) to the operator-scripts ConfigMap, and
   the two roles and the preview setting to the operator configuration (see
   [The role configuration](#the-role-configuration)).
8. **Check.** Run [dry_run.py](dry_run.py) with the token of
   `preview-operator` (see [Checking the policy](#checking-the-policy)). Then
   file one request that changes a page of a small project, and open the
   address the engine posts.

## Values a person fills in

| Placeholder | Where it is used | What it is |
| --- | --- | --- |
| `<preview-namespace>` | every file, `PREVIEW_NAMESPACE` | The namespace of the previews, for example `preview-apps`. |
| `<router-namespace>` | every file but the policy | The namespace of the router, the identities and the cleanup, for example `preview-router`. |
| `<preview-domain>` | router, `PREVIEW_DOMAIN` | The domain every preview gets one label under, for example `preview.example.invalid`. |
| `<certificate-arn>` | router | The ARN of the certificate for `*.<preview-domain>` (step 1). |
| `<cluster-dns-ip>` | preview namespace, router | The cluster DNS Service address: `kubectl -n kube-system get service kube-dns -o jsonpath='{.spec.clusterIP}'`. It has to be the nameserver a Pod's `/etc/resolv.conf` names. |
| `<service-cidr>` | preview namespace, router | The cluster's Service address range. |
| `<net-image>` | policy, router, `PREVIEW_NET_IMAGE` | The rules image, by digest (`name@sha256:...`). It needs `iptables-restore` and must run on the preview's nodes. |
| `<nginx-image>` | router | An nginx image that runs as a user other than root (uid 101), by digest. |
| `<kubectl-image>` | cleanup | An image with kubectl, a POSIX sh, a `date` that reads `-d @SECONDS`, `expr`, `mktemp`, `cut`, `grep` and `rm`, by digest. Without one of them the cleanup ends 1 and removes nothing. |
| `<relay-label-pattern>` | router, relay | The labels whose API is relayed, as a regular expression of the whole label, for example `sample-example-[0-9]+`. |
| `<relay-path-prefix>` | router, relay | The paths relayed, as the start of the path, for example `/api/`. |
| `<relay-allowed-path>` | router, relay | One allowed path, a regular expression of the whole path, for example `/api/items/[0-9]+`. One line for each. |
| `<relay-allowed-query>` | router, relay | A query form of an allowed path, for example `owner=demo(&page=[0-9]{1,4})?(&limit=[0-9]{1,4})?`. One line for each form; a path without one is relayed with no query only. |
| `<relay-id-query-path>` | router, relay | An allowed path that needs its query (it takes an identifier there), refused without one, for example `/api/projects/[0-9]+/items`. One line for each, or delete the line. The template keeps the query lines in their order: the line that refuses a second `?` first, then these, the line that lets an empty query through, and the forms. |
| `<relay-refused-path>` | router, relay | One refused path, a regular expression of the start of the path, for example `/api/account/keys(/\|$)`. One line for each. |
| `<relay-service>` | router, relay | The API's Service as `<name>.<namespace>`, answering on port 80, for example `api.example-dev`. |
| `<relay-header>` | router, relay | The header the API reads the key from, for example `X-Example-Key`. |
| `<relay-cleared-header>` | router, relay | A header a browser can send that the API acts on (one that changes whom a request acts as, for example `X-Act-As`); the relay sends it empty. One line for each, or delete the line. |
| `<region>` | step 1 only | The region of the load balancer. |
| `<api-server>` | `PREVIEW_K8S_SERVER` | The address of the cluster's API server. The engine's Pod has to reach it. |

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
more. When the wait ends before the Pod's last rules container (`net`) has
ended with status 0, `up` removes the Deployment before it ends 1.

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
| `PREVIEW_NAME` | `sample` | yes | This installation's short name, the first part of every preview's name and host. 1 to 20 lowercase letters, digits and hyphens, starting with a letter and not ending with a hyphen. Two installations sharing a namespace need different names: GitHub request numbers are digits only. |
| `PREVIEW_START` | `python3 -m http.server "$PORT" --bind 0.0.0.0 --directory site` | yes (up) | The command that starts the application, run with `sh -c` in the checkout (`/src`). `PORT` is set. |
| `PREVIEW_PORT` | `8080` | yes (up) | The port the application listens on. |
| `PREVIEW_READY_PATH` | `/` | yes (up) | The path that answers once the application is up. It is the readiness probe and the path of the public check. |
| `PREVIEW_LIFETIME_HOURS` | `72` | yes (up) | How long a preview stays after it was last brought up, 1 to 720. |
| `PREVIEW_IMAGE` | `python:3.12-slim` | yes (up) | The image the application runs in. It needs `sh`, `tar` and `sleep`, and `timeout` (coreutils, or BusyBox 1.30 or later) when `PREVIEW_INSTALL` is set. Name it by digest to keep it fixed. |
| `PREVIEW_MEMORY` | `512Mi` | no (`512Mi`) | The memory limit of the application and of the dependency step. At most the preview namespace's per-container maximum (2Gi in the example cluster setup), or the Pod is refused when it is created. |
| `PREVIEW_INSTALL` | `npm ci` | no | A dependency step, run with `sh -c` in the checkout before the application. Only during this step can the Pod reach public addresses. |
| `PREVIEW_INSTALL_SECONDS` | `240` | no (`240`) | The step's time limit: digits only, 1 to 480, and, when `PREVIEW_INSTALL` is set, at most `PREVIEW_WAIT_SECONDS` − 180 (see Time above). Anything else ends `up` before the cluster is called. |
| `PREVIEW_ENV_FILE` | `.env.preview` | no | A file in the checkout, relative to it, of `KEY=VALUE` lines for the application's environment. No secrets: the file is in the branch. At most 64 KiB; a line that is not `KEY=VALUE` (blank lines and `#` lines aside) ends `up`. `PORT` is always the script's. |
| `PREVIEW_PACK_DIR` | `web/dist` | no | A directory of the checkout, relative to it, that git ignores, such as the output a build of the verification stage left there. With it, `up` carries only what is inside it, with the directory as the root (`web/dist/index.html` is `/src/index.html` in the Pod), instead of the checkout's tracked and untracked files. The directory itself has to be ignored, as `dist` or `dist/` in a `.gitignore` ignores it: `dist/*` ignores only what is inside, and git does not count a directory with a tracked file in it as ignored either. One trailing `/` is allowed. `up` ends 1 before the cluster is called when the value starts with `/` or has a doubled `/` or a `.` or `..` part; when the directory, or a directory on its way, is a symbolic link or missing; when git does not ignore the directory itself; when it holds no file or link; and when it holds more than 1 GiB. A link inside is carried as a link. The same contents give the same change identifier, so the same build again only extends the lifetime. |
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

- Before anything else `up` and `down` remove every name listed in
  `TASK_CREDENTIAL_NAMES` (and these three, listed or not) from their
  environment, write the values into a pipe and start the program again
  without them, reading them from the pipe. `/proc/self/environ` shows the
  environment a program started with, so removing a value is not enough;
  after the restart the running program's environment holds none. `origin`
  reads no credential and does none of this.
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
- **Checked:** the tests run [preview-router.js](preview-router.js) with the
  njs command line where one is installed (`PREVIEW_NJS`, or `njs` on
  `PATH`) and skip it where not; a table of signatures njs made binds
  `preview.py` to the same text everywhere. They need njs 1.0's command
  line: njs 0.8.2's has no `-m`.
- **The router** checks it after the password: the signature made again
  from the request's `Host` and `<end>` has to match (compared in constant
  time), and `<end>` has to be later than now. Then it sets the cookie
  `__Host-preview` (Secure, HttpOnly, SameSite=Lax, Path=/, no Domain, until
  the end) and answers 302 to `/`, with `Referrer-Policy: no-referrer`.
  Every other path needs the password and that cookie; without them it
  answers 401 and 403. Paths under `/_preview/` belong to the router and
  never reach the application.
- **The application's cookies:** before forwarding, the router removes its
  own cookie from the `Cookie` header; in the answer, it rewrites the
  `Domain` of every `Set-Cookie` to the host and drops one of its own name or
  with a `Domain` that still names something else.

## The origin of a preview

`preview.py origin` prints, as one line, the origin `up` gives the request's
preview: `https://<label>.<PREVIEW_DOMAIN>`, the label named as `up` names
it (`PREVIEW_NAME`, a hyphen and the request number, lowercased, every run of
other characters made one hyphen, a long one cut and given part of its
hash). It reads `TASK_ISSUE`, `PREVIEW_NAME` and `PREVIEW_DOMAIN` only, no
credential, and calls nothing. A build that has to know where its page will
be served, such as a page whose API address is fixed when it is built, can
ask for it before `up` runs instead of writing the naming rule a second time:

```sh
origin="$(python3 -I /opt/ticket-automation/operator/preview.py origin)" || origin=""
```

When one of the three is missing or wrong it ends with 2, the reason on
standard error and nothing on standard output.

## Exit status and output

| Status | When |
| --- | --- |
| 0 | `up`: the preview answers on its public address and `TASK_PREVIEW` is written. `down`: neither the Deployment nor the Service of the request is left, also when there was none. `origin`: the origin is printed. |
| 1 | Anything else. The last line of the output is the reason, with the cluster's own message where it gave one (a policy refusal names the policy). `TASK_PREVIEW` is not written. |
| 2 | A wrong command line. The engine never starts it so with this configuration. Also `origin` without one of its settings, or with a wrong one. |

After a failure nothing is removed, except a Deployment whose Pod's last
rules container (`net`) had not ended with status 0 when the wait ended, for
whatever reason: until then the dependency step could still run with public addresses
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
- With `PREVIEW_PACK_DIR`, the directory and every directory on its way and
  inside it are opened the same way. The directory being a link, or a link
  on its way, ends `up`; a link inside is packed as a link.
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
- `nodeSelector` is `kubernetes.io/arch: amd64`, and the policy accepts no
  other value (nor `kubernetes.io/os` other than `linux`): a cluster whose
  general nodes are arm64 changes it in `preview.py` and in the policy
  together.
- The volumes are emptyDirs (the checkout's holds 1 GiB) and the ConfigMap
  `preview-egress`. No Secret, no projected token, no host path.
- The Service is a ClusterIP Service from port 80 to `PREVIEW_PORT`, with a
  selector and no annotations.

[admission-policy.yaml.example](admission-policy.yaml.example) accepts this
shape and refuses the ones that would let the token's holder run a privileged
Pod, reach the node, take the node's network or publish a load balancer
([Checking the policy](#checking-the-policy) lists what it refuses).

## The rules image

`net-hold`, `net-install` and `net` load the egress rules of the ConfigMap
`preview-egress` with `iptables-restore --wait 5 /policy/<file>`, which
replaces the whole filter table each time. The image is `PREVIEW_NET_IMAGE`,
named by digest; it needs `iptables-restore` on its `PATH` and must run on
the preview's nodes. These commands are fixed in `NET_COMMANDS` in
[preview.py](preview.py), and the admission policy accepts exactly them:
when the program sits elsewhere in a new image, change both together, then
the digest.

## Checking the policy

[dry_run.py](dry_run.py) sends every shape below to the cluster as a
server-side dry run (`kubectl apply --server-side --dry-run=server`): it goes
through admission, policies included, and nothing is stored. It uses only the
kubeconfig files it is given, never the default one:

```sh
python3 dry_run.py --kubeconfig preview-operator.kubeconfig --context <context> \
  --namespace <preview-namespace> --other-namespace <router-namespace> \
  --net-image '<net-image>' \
  --admin-kubeconfig admin.kubeconfig --admin-context <admin-context>
```

`--kubeconfig` holds the token of `preview-operator`, so the Deployments and
Services are sent with the preview commands' own permissions. The Pod cases
need an identity that may create Pods (`--admin-kubeconfig`), since
`preview-operator` may not; they check the Pod policy, the last line, met
when a ReplicaSet creates a Pod. `--write DIRECTORY` writes every case's
object as JSON instead, without a cluster. A policy just applied takes a
moment before it acts (0.6 seconds on the local server below): the command
first sends one shape only the policy refuses until it is refused, for at
most `--settle-seconds` (30).

Both kubeconfig files have to hold the credential itself: a `token`, or a
`tokenFile` naming a file that holds one. kubectl runs here with `PATH`,
`LANG`, a temporary `HOME` and `KUBECONFIG` only, so a credential plugin
(`exec`) finds neither its configuration under the home directory nor any
`AWS_*` variable. The AWS CLI's `aws eks get-token`, which an EKS kubeconfig
names, is such a plugin: with it the Pod cases usually fail with the
plugin's own error. (A machine that gets AWS credentials without either, such
as one with an instance profile, may get through. A stand-in plugin that
needs its file under `HOME` and `AWS_PROFILE` answered kubectl from the
caller's shell, and failed for each Pod case under this command, on the local
server below.) On EKS, write the token the plugin would print into a new
private directory outside the repository, with the same arguments as the
kubeconfig's `exec` section (role, profile), and name the file by its
absolute path as the admin user's `tokenFile`. The token is short-lived: run
the command right after, then remove the directory. Do not write the token
into the checkout, where nothing keeps it out of a commit:

```sh
dir=$(mktemp -d)   # a new directory, mode 0700, outside the checkout
aws eks get-token --region <region> --cluster-name <cluster> \
  --query status.token --output text > "$dir/admin-token"
# the admin user's tokenFile: "$dir/admin-token" (print "$dir" for the path)
# ... run dry_run.py ...
rm -rf "$dir"
```

A case passes when what has to pass passes, and what has to be refused is
refused with `ValidatingAdmissionPolicy '<its policy>'` and the rule's
message. A refusal for another reason (RBAC, the API's own validation, the
quota) did not test the policy and fails the case. The command also asks
`kubectl auth can-i` what `preview-operator` may do: Deployments, Services and
`pods/exec` in the preview namespace yes; Pods, Ingresses, Secrets,
ConfigMaps, ServiceAccounts, Roles, ephemeral containers and anything in
another namespace no. It exits 0 when every check came out as expected.

| Case | Sent as | Expected |
| --- | --- | --- |
| preview.py's Deployment without the dependency step | Deployment | passes |
| preview.py's Deployment with the dependency step | Deployment | passes |
| preview.py's Service | Service | passes |
| a privileged container | Deployment | preview-deployment-shape |
| a hostPath volume | Deployment | preview-deployment-shape |
| hostNetwork | Deployment | preview-deployment-shape |
| hostPID | Deployment | preview-deployment-shape |
| a hostPort | Deployment | preview-deployment-shape |
| nodeName | Deployment | preview-deployment-shape |
| a toleration of another taint (an engine-only node pool) | Deployment | preview-deployment-shape |
| a toleration without a key (tolerates every taint) | Deployment | preview-deployment-shape |
| affinity | Deployment | preview-deployment-shape |
| a nodeSelector of another label | Deployment | preview-deployment-shape |
| nodeSelector kubernetes.io/arch=arm64 | Deployment | preview-deployment-shape |
| nodeSelector kubernetes.io/os=windows | Deployment | preview-deployment-shape |
| NET_ADMIN in a container other than the rules containers | Deployment | preview-deployment-shape |
| net with another image | Deployment | preview-deployment-shape |
| net with another command | Deployment | preview-deployment-shape |
| net with args | Deployment | preview-deployment-shape |
| net-install with net's command | Deployment | preview-deployment-shape |
| net adding SYS_ADMIN besides NET_ADMIN | Deployment | preview-deployment-shape |
| net without dropping ALL | Deployment | preview-deployment-shape |
| the application without dropping ALL | Deployment | preview-deployment-shape |
| receive dropping only NET_RAW | Deployment | preview-deployment-shape |
| every rules container reading /policy from the checkout | Deployment | preview-deployment-shape |
| net alone reading /policy from the checkout | Deployment | preview-deployment-shape |
| the checkout mounted over /usr/sbin in net | Deployment | preview-deployment-shape |
| a checkout file mounted over /policy/run.rules in net | Deployment | preview-deployment-shape |
| an environment variable in net (XTABLES_LIBDIR) | Deployment | preview-deployment-shape |
| PATH set in net | Deployment | preview-deployment-shape |
| net mounting the rules without readOnly | Deployment | preview-deployment-shape |
| envFrom in net (a ConfigMap) | Deployment | preview-deployment-shape |
| the rules ConfigMap with install.rules as run.rules | Deployment | preview-deployment-shape |
| the application mounting the rules volume | Deployment | preview-deployment-shape |
| the init containers without net | Deployment | preview-deployment-shape |
| an init container after net | Deployment | preview-deployment-shape |
| an init container before net-hold | Deployment | preview-deployment-shape |
| an init container between receive and net | Deployment | preview-deployment-shape |
| an init container with restartPolicy Always | Deployment | preview-deployment-shape |
| install without timeout | Deployment | preview-deployment-shape |
| install without -s KILL | Deployment | preview-deployment-shape |
| install with -s TERM | Deployment | preview-deployment-shape |
| install for 0 seconds | Deployment | preview-deployment-shape |
| install for 481 seconds | Deployment | preview-deployment-shape |
| install for 30s | Deployment | preview-deployment-shape |
| install with args | Deployment | preview-deployment-shape |
| no seccompProfile on the Pod | Deployment | preview-deployment-shape |
| seccompProfile Unconfined on the Pod | Deployment | preview-deployment-shape |
| seccompProfile Unconfined on a container | Deployment | preview-deployment-shape |
| procMount Unmasked | Deployment | preview-deployment-shape |
| priorityClassName system-node-critical | Deployment | preview-deployment-shape |
| a priority of 1000000 | Deployment | preview-deployment-shape |
| no terminationGracePeriodSeconds (30 by default) | Deployment | preview-deployment-shape |
| terminationGracePeriodSeconds 30 | Deployment | preview-deployment-shape |
| sysctls | Deployment | preview-deployment-shape |
| seLinuxOptions | Deployment | preview-deployment-shape |
| AppArmor Unconfined on the Pod | Deployment | preview-deployment-shape |
| AppArmor Unconfined on a container | Deployment | preview-deployment-shape |
| resourceClaims | Deployment | preview-deployment-shape |
| a projected ServiceAccount token volume | Deployment | preview-deployment-shape |
| a Secret volume | Deployment | preview-deployment-shape |
| an environment value from a Secret | Deployment | preview-deployment-shape |
| an environment from a Secret | Deployment | preview-deployment-shape |
| no hostUsers | Deployment | preview-deployment-shape |
| automountServiceAccountToken true | Deployment | preview-deployment-shape |
| another ServiceAccount | Deployment | preview-deployment-shape |
| a runtimeClassName | Deployment | preview-deployment-shape |
| an application container named net-install | Deployment | preview-deployment-shape |
| an application running as root | Deployment | preview-deployment-shape |
| a LoadBalancer Service | Service | preview-service-shape |
| a NodePort Service | Service | preview-service-shape |
| an ExternalName Service | Service | preview-service-shape |
| a Service with externalIPs | Service | preview-service-shape |
| a Service without a selector | Service | preview-service-shape |
| a Service with an external-dns hostname | Service | preview-service-shape |
| a Service with a load balancer annotation | Service | preview-service-shape |
| preview.py's Pod (as a ReplicaSet would create it) | Pod (admin) | passes |
| a privileged Pod | Pod (admin) | preview-pod-shape |
| a Pod with an init container before the rules | Pod (admin) | preview-pod-shape |
| a Pod without seccompProfile | Pod (admin) | preview-pod-shape |

Every one of these came out as expected on a local kube-apiserver v1.36.0
with this directory's manifests applied. That is not the cluster the
previews will run on: run it there after applying.

## Relaying an API

A preview whose page reads data from an API (the development environment of
the same product, say) cannot reach it itself: the preview's Pod has no
egress while it runs, and the API needs a key the preview's code must never
hold. The router can relay that API instead. A request of the preview's page
to a path under one prefix is sent by the router to a Service of the
cluster, with a key only the router holds, and only when it reads.

- **Which previews:** those whose label (the preview's name, `-`, and the
  issue key in lowercase) matches `<relay-label-pattern>` whole. Write the
  name and the form of the key, `sample-example-[0-9]+` for the name
  `sample` and keys like `EXAMPLE-12`. A pattern that only starts with the
  name would also relay another installation whose name starts the same
  (`sample-stg-example-12`).
- **Which paths:** those under `<relay-path-prefix>`. Every other path of
  these previews goes to the preview as before.
- **Only GET and HEAD of an allowed path.** Each allowed path is one line,
  `<relay-allowed-path>`, a regular expression of the whole path
  (`/api/items/[0-9]+`). Every other method is refused, and so is every GET
  not on the list.
- **Refused first.** Each refused path is one line, `<relay-refused-path>`, a
  regular expression of the start of the path (`/api/account/keys(/|$)`).
  They are checked before the allowed ones, so a path on both lists is
  refused. Put there every path that answers with a credential and every GET
  that changes something, even when nobody would allow it: the allowed list
  is the guard, and this is the guard behind it.
- **Characters.** A path with a character other than `A-Z a-z 0-9 / . _ ~ -`
  after nginx decoded it is refused before both lists: it would have to be
  encoded again to be sent.
- **The query:** an allowed path is relayed with no query, or with one of
  the forms listed for it, each `<relay-allowed-query>` on a line of its own
  beside the path. Write each name once, in a fixed order, with values from
  a set without `%`: then a repeated name, a name or value written with `%`,
  a name not listed, and another value of a fixed one are all refused. Any
  other query is refused. The order is the one the page builds its query
  in: after writing the list, open the page once and check that none of its
  requests answers 403. A path that takes an identifier in its query is
  also listed as `<relay-id-query-path>`, and is then refused without a
  query. The lines are checked from the top, and the template keeps their
  order: first the line that refuses a query with a second `?`, then the
  paths that need their query, the line that lets an empty query through,
  and the forms.
- **The path decided on is the path sent.** nginx decodes and normalizes the
  path first (`/../`, `%2F`); the lists are checked on that path, and that
  path is what the API receives, with the query as it came. The relay keeps
  the path in `$relay_path` before it moves the request to its own location,
  because after the move `$uri` is that location's name and every allowed
  path would be refused.
- **What the API receives:** the request with `<relay-header>` set to the
  key and `Host` the Service's name. `Authorization` (the shared password),
  `Cookie` (the router's and the page's) and each `<relay-cleared-header>`
  are sent empty. Every header is set in the relay's location: nginx carries
  none of the location `/`'s over once one is set there.
- **What the page receives:** the API's answer as it is, status and body,
  without `Set-Cookie`.
- **Refusals** are 403 with `Content-Type: application/json` and a fixed
  sentence: `{"error":"確認用の画面では保存できません"}` for another method,
  `{"error":"確認用の画面では使えない操作です"}` for a path or a query not
  allowed, `{"error":"この画面は課題に投稿された URL から開いてください"}`
  without the signed URL's cookie. When the API does not answer, or the
  router has no key, 503 with `{"error":"API に届きません"}`. Never the
  router's HTML pages. A refused path can carry a sentence of its own on its
  line (a login path, say); keep sentences free of `"` and `\`.
- **The page's own requests:** every answer for these previews carries
  `Content-Security-Policy: connect-src 'self'`. The preview's code can send
  what it reads through the relay to its own origin only, by fetch, XHR or
  WebSocket.

The router's access log writes a relayed request with the path the relay
decided on, without its query, and never the key. Its error log keeps only
critical errors of the relay, so an API that does not answer leaves no
relayed address or query there; the access log still has the 503.

The values go into the relay's lines of the router's `nginx.conf` (the
blocks between `relay: begin` and `relay: end` in
[router.yaml.example](router.yaml.example)), one line for each path. The key
goes into the Secret `preview-relay` (key `key`), which the router reads as
`PREVIEW_RELAY_KEY` when it starts: it never reaches a preview's Pod, the
engine or the browser. The Service has to answer on port 80, the only port
the router's egress allows in the Service range. To relay the API of a
second name, copy every block between the markers and give the copy's
variables, location and key other names.

The example the tests run, as its lines read once filled in:

```nginx
map $preview_label $relay_label { default 0; "~^(?:sample-example-[0-9]+)$" 1; }
map "$relay_label:$relay_path" $relay_on { default 0; "~^1:/api/" 1; }
map "$request_method:$relay_path" $relay_refusal {
  "~^(GET|HEAD):.*[^A-Za-z0-9/._~-]" "確認用の画面では使えない操作です";
  "~^(GET|HEAD):/api/account/keys(/|$)" "確認用の画面では使えない操作です";
  "~^(GET|HEAD):/api/items/[0-9]+/export$" "確認用の画面では使えない操作です";
  "~^(GET|HEAD):/api/items$" "";
  "~^(GET|HEAD):/api/items/[0-9]+$" "";
  "~^(GET|HEAD):/api/projects/[0-9]+/items$" "";
  "~^(GET|HEAD):" "確認用の画面では使えない操作です";
  default "確認用の画面では保存できません";
}
map "$relay_path?$args" $relay_args_refusal {
  default "確認用の画面では使えない操作です";
  "~^[^?]*[?][^?]*[?]" "確認用の画面では使えない操作です";
  "~^/api/projects/[0-9]+/items[?]$" "確認用の画面では使えない操作です";
  "~^[^?]*\?$" "";
  "~^/api/projects/[0-9]+/items[?]owner=demo(&page=[0-9]{1,4})?(&limit=[0-9]{1,4})?$" "";
}
```

with `proxy_pass http://api.example-dev.svc.cluster.local$relay_uri;`,
`proxy_set_header X-Act-As "";` and `proxy_set_header X-Example-Key
$relay_key;` in the relay's location.

**Checked:** `harnesses/test_preview_k8s_manifests.py` reads the relay's
blocks as text everywhere, and runs the router's whole configuration with
this example filled in on an nginx with the njs and auth_request modules
(`PREVIEW_NGINX`), skipping that where none is named. It sends real requests
to stand-ins for the API and the Pod: the cases above, the labels that are
and are not relayed, the headers the API receives, and controls that show
each check sees the relay's pieces taken out. That run was nginx 1.28.0 with
njs 1.0.1 on macOS, and CI runs it with nginx 1.30.6 and njs 1.0.2 from
nginx.org's packages; the router's image and the real Service are checked on
the cluster (see [Checking a running preview](#checking-a-running-preview)).

## Checking a running preview

What only a real cluster shows, to try once the setup is applied and a
preview is up (the name below is an example):

- **No egress while the application runs.** From the application's
  container, open a TCP connection to a public address by its number, not
  its name: a name already fails at DNS, which would pass with the TCP rule
  broken.

  ```sh
  kubectl -n <preview-namespace> exec deploy/pv-sample-12 -c app -- \
    python3 -c "import socket; socket.create_connection(('1.1.1.1', 443), 5)"
  ```

  It has to fail (connection refused or timed out).
- **The password never reaches the application.** Bring up a checkout whose
  application answers with the headers it received, open it, and look for
  `Authorization` and `__Host-preview`: neither may be there.
- **The relay, when there is one.** With the password and a signed URL of a
  relayed preview, an allowed GET answers 200 with the API's JSON (the router
  reaches the Service on port 80 and the key works), a refused path and
  `POST` answer 403 with their JSON sentence, the same path through a preview
  of another name reaches that preview, and the page's answers carry
  `Content-Security-Policy: connect-src 'self'`.
- **No token in the router's log.**
  `kubectl -n <router-namespace> logs deploy/preview-router | grep -c 't='`
  has to print 0 after a signed URL was opened.

## Replacing the token or the password

The token of `preview-operator` has no expiry: the engine reads its keys when
it starts, so a token that expired would stop every preview until the engine
was restarted. Replace it when it may have leaked, when the people who could
read it change, and at the interval the owner of the installation sets:

1. Make a second token Secret for the same ServiceAccount (a copy of
   `preview-operator-token` under another name).
2. Put its token into the engine's Secret as `PREVIEW_K8S_TOKEN`.
3. Restart the engine. This is a change to the running installation: do it
   with its owner's go-ahead.
4. Delete the old token Secret. The old token stops working at once.
5. Check that the old token is refused: with it,
   `kubectl -n <preview-namespace> get deployments` has to answer
   `Unauthorized`.

In a hurry, do step 4 first. Until the engine has the new token every
preview fails, and the requesters get their questions in words; no request
stops.

The check user's password the same way: a new `user:hash` line in the
password file (step 3 of the setup), the Secret `preview-router-auth`
replaced and the router restarted, the new `user:password` into the engine's
Secret as `PREVIEW_CHECK_AUTH` and the engine restarted, then the old line
removed. The requesters' password is changed in the password file alone and
handed to them again outside the tracker.

The signing key of the signed URLs is held twice, by the router and by the
engine, and both have to change:

1. Make a new key: `openssl rand -base64 32 | tr -d '\n' > signing-key`.
2. Replace the router's Secret `preview-router-signing` with it, and restart
   the router (`kubectl -n <router-namespace> rollout restart deployment/preview-router`):
   nginx reads the key from its environment when it starts.
3. Put the same value into the engine's Secret as `PREVIEW_SIGNING_KEY`, and
   restart the engine (with its owner's go-ahead). Remove `signing-key`.
4. Every open preview's URL and cookie now answer 403; a requester asks for
   the preview again, and it is brought up with a new URL.

Between steps 2 and 3 the two keys differ: every preview brought up then
fails its check with 403 and the requester gets the question in words.

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
  attempt. That limit holds for the shape `preview.py` sends; the token's
  holder chooses the image, whose `timeout` need not be real, and can make the
  Deployment again. What stays bounded for them is the policy: no privilege,
  no private address, the quota.
- The signed URL is posted on the tracker: whoever can read the request and
  knows the shared password can open that request's preview until its end.
  It also stays in the engine's own records of the request (its history and
  its notices), in the requester's browser history, and in a load balancer
  access log if one is turned on. Changing the signing key is the
  only way to stop a URL before its end, and it stops every open preview.
- An application cannot use paths under `/_preview/`: the router keeps them.
- Cookies the application sets in its answers are bound to the preview's
  host: the router rewrites their `Domain` to the host
  (`proxy_cookie_domain`) and drops any whose `Domain` still names something
  else. Cookies its JavaScript writes or sends are not stopped: under a
  domain other services share, a preview can still set cookies for that
  domain from the browser.
- The router's access log writes the path without its query, so tokens stay
  out of it. Its error log keeps only critical errors for the signed URL's
  path, so a mistyped password or an unknown user does not write the request
  line, with its token, there either; such failures are not logged at all.
- One password file for every installation the router serves: a requester of
  one installation can open another's previews.
- The relay refuses before the password is checked: a request without it gets
  the refusal's sentence for a path not allowed and 401 for an allowed one,
  and can tell the allowed paths from that. Nothing is sent to the API.
- The relay takes each query form in its listed order only: a page that
  sends the same names in another order is refused.
- The relay acts with its key's rights. Whatever an allowed path reads with
  it, the preview's code can read too, and `connect-src 'self'` stops only
  fetch, XHR and WebSocket: an image or a navigation can still carry it
  elsewhere. Allow only what the page needs, and fix every identifier in an
  allowed path or query that the API does not check against the key.
- The API's answers are served from the preview's host as they are: an API
  that answers with HTML has it shown there.
- Nothing stops other Pods of the cluster from connecting to a preview: the
  cluster's NetworkPolicy may not be enforced, and a preview holds no
  credential to take.
- `kubectl exec` can still run anything in a preview Pod; the policy cannot
  shape it. Only the preview identity may, and the Pod holds nothing secret.
- Not yet tried on a real cluster, only on a local API server and in the
  tests: that the chosen nginx image has the njs and auth_request modules,
  that the rules image loads the rules inside a Pod with its own user
  namespace (`hostUsers: false`), that the running rules still let the
  router and the readiness probe in, that the engine's Pod reaches the API
  server and the hand-over over `kubectl exec` works from inside the role
  launcher, and the warnings the cluster writes in each policy's status when
  it type-checks the expressions (the local server had no controller
  manager to write them).
