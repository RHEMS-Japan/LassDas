# Setting up the ticket engine for your repository

This guide takes you from nothing to one running instance: a Pod in your
Kubernetes cluster that watches one Backlog project, takes each new issue
there as a request, works on a checkout of one GitHub repository, merges the
result into that repository's integration branch, and reports back on the
issue. You need this directory, `rewrite/` and `docs/DISTRIBUTION.json`;
nothing else.

Read [README.md](README.md) in this directory first. It separates what has
been measured from what is only proposed. The engine itself is described in
[rewrite/README.md](../../rewrite/README.md) and its runtime requirements in
[rewrite/RUNTIME.md](../../rewrite/RUNTIME.md). Where this guide says "one
installation", it means one consumer's instance that has run from these
templates since September 2026. What happened there is told without its
names; it cannot be checked from this repository.

**What it does not promise.** A request that reaches "delivered" has had its
change merged, the configured checks pass on the merged branch, and a report
posted. That is not proof that the request was understood correctly. Read the
merged change.

## Contents

1. [What you need before starting](#1-what-you-need-before-starting)
2. [Preparing the tracker](#2-preparing-the-tracker)
3. [Preparing the delivery repository](#3-preparing-the-delivery-repository)
4. [Writing the operator configuration](#4-writing-the-operator-configuration)
5. [The secrets](#5-the-secrets)
6. [Applying the manifests, in order](#6-applying-the-manifests-in-order)
7. [Checking that it is up](#7-checking-that-it-is-up)
8. [Opening the intake and filing the first ticket](#8-opening-the-intake-and-filing-the-first-ticket)
9. [Stopping a request](#9-stopping-a-request)
10. [Upgrading to a new image](#10-upgrading-to-a-new-image)
11. [Troubleshooting](#11-troubleshooting)
12. [Where things are](#12-where-things-are)

## How a request moves

The shipped ordered configuration, `rewrite/examples/operator-stages.json`,
runs every request through these stages. A stage that runs a command is
finished only when the command exits 0; a stage that runs a model is finished
when its model process returns without an error. Two decisions are a model's:
at the entrance, whether to ask the requester before the work starts, and in
the review, whether the change goes back to `work`. Beyond those two, nothing
a model writes moves the request: writing "done" or "delivered" changes
nothing.

| Stage | Kind | What happens |
| --- | --- | --- |
| `elicit` | model | Settles what the request asks for, from the request, the checkout and your instructions. Points only the requester can decide are listed with choices. |
| (`ask_requester`) | model | Only when such points are left: posts one comment with them, and the request waits for the requester's reply. |
| `work` | model | Investigates, changes the checkout and runs the project's checks. |
| `verify` | command | Your build and test commands, against the changed checkout. |
| `review` | command | A second model reviews the diff and the test output; a blocking verdict sends the work back to `work`. |
| `deliver` | command | Commits, brings the ticket branch up to date with the integration branch, pushes `ticket/<ISSUE-KEY>`, opens or reuses one pull request and merges it. |
| `verify_merged` | command | Fetches the integration branch after the merge and runs your build and tests on it. |
| `report` | model | Writes the report and posts it on the issue. |
| `confirm_report` | command | Passes when one comment on the issue is exactly the report. |

A command stage that fails sends the work back to the model stage named in its
`on_failure`, and every stage after that one runs again. There is no counter
and no failure ending: a command that can never pass keeps the request going
round, with a model launch each time, until someone fixes the cause or the
requester posts a stop ([section 9](#9-stopping-a-request)).

## 1. What you need before starting

### A Kubernetes cluster

- **arm64 nodes.** The published image is built for arm64 only.
- **Three Kubernetes features, turned on**: Pod user namespaces
  (`hostUsers: false`, here together with a persistent volume), the
  `procMount` field (`procMount: Unmasked`), and sidecar containers
  (`restartPolicy: Always` on an init container). The engine container also
  runs with `seccompProfile: Unconfined`. These are requirements found by
  running the role launcher (README.md), not a recommended profile. This
  repository names no minimum Kubernetes version: the version of the one
  cluster that ran it was not recorded. Look up the feature gates
  `UserNamespacesSupport`, `ProcMountType` and `SidecarContainers` for your
  version; the nodes' operating system and container runtime must support
  user namespaces as well. The server-side dry run in section 6 shows what
  admission does to the StatefulSet; whether the Pod itself is admitted, and
  whether a node can run it, shows only in the StatefulSet's and the Pod's
  events ([section 11](#the-pod-does-not-become-ready)). One installation
  runs this StatefulSet on arm64 nodes with a 20Gi ReadWriteOnce volume from
  its cluster's block storage class.
- **A namespace that admits this Pod.** Pod Security admission at `baseline`
  or `restricted` refuses it: two init containers add `NET_ADMIN` and the
  engine runs without the runtime's default seccomp profile, and `baseline`
  forbids both. Use a namespace whose enforced level is `privileged`
  (section 6 shows the label), and keep other workloads out of it.
- **An image with the iptables tools for the egress rules.** The two network
  init containers run `/usr/sbin/xtables-nft-multi` from an image you name.
  One installation used its network plugin's own DaemonSet image, which
  carries that program and which every node already pulls. If your network
  plugin's image does not carry it, any image that does and that every node
  can pull will serve; this repository does not ship one.
- **A storage class** that provides a 20Gi ReadWriteOnce volume.
- **Room for the Pod.** The engine requests 1 CPU and 3Gi of memory (limit
  6Gi); the other containers are small. These figures are a starting point,
  not a measured working set.
- **Pulls from ghcr.io**, or a registry mirror of the published image.

### A Backlog project

The engine's tracker client speaks Backlog's API v2. You need a space, a
project for the requests, and a separate account for the engine itself
([section 2](#2-preparing-the-tracker)). A dedicated project is simplest.

### A GitHub repository to deliver to

The repository (`<owner>/<repository-name>`) with at least one commit, its
integration branch (the one pull requests merge into), and a token for the
identity that delivers ([section 3](#3-preparing-the-delivery-repository)).
The delivery scripts also accept another Git host through
`DELIVERY_REMOTE_URL` and `DELIVERY_API_BASE`, but only GitHub has been used.

### A model endpoint and key

The engine is written for OpenRouter. It chooses each launch's model from
OpenRouter's public model catalogue, whose address is fixed in the code
(`rewrite/cmd/engine/models.go`); it names models by OpenRouter's ids
(`publisher/model`); and the bridge to the working agent talks to the endpoint
as OpenRouter (`provider="openrouter"` in `rewrite/harnesses/hermes.py`).
Two arrangements work today:

- **OpenRouter itself**, with one OpenRouter API key for everything. This is
  what the shipped configuration does: its chat completions API for the work
  and the entrance decision, its decisions API (the model
  `typesafe/jev-1.13`) for choosing a model at each launch.
- **A gateway in front of OpenRouter** that serves the same models under ids
  made of a prefix and the catalogue id (`openrouter/` + `qwen/...`), with its
  own key ([section 4](#with-a-gateway-in-front-of-the-models)). The Pod must
  still reach openrouter.ai, because the catalogue is read from there. One
  installation sends every model call through such a gateway.

Any other OpenAI-compatible endpoint cannot be used today.

The engine has no spending limit of its own. Set a limit on the key or the
account. `intake.min_model_credit` can also pause work when the key's
remaining limit drops below a floor; it needs a setting the shipped
configuration lacks ([section 4](#settings-the-requester-will-notice)).

### Who can make it act, and where the code goes

Anyone who can create an issue in the project can make the engine read the
repository, change it and merge the change: it takes every new issue in the
project, whoever filed it, and its roles can reach any public address. So:

1. Deliver to an integration branch that nothing deploys to production by
   itself.
2. Keep the tracker project's members to the people who may ask for changes.
3. Make the engine's tracker account a member of this project only, so that a
   mistyped project id cannot point it at another one.
4. Give the delivery token no permission over the repository's workflows.

With the shipped configuration, everything goes to OpenRouter and the models
it serves: the working models, chosen for each launch among the publishers
in `model_selection.authors` (deepseek, minimax, moonshotai, qwen and z-ai),
read the checkout and receive the request and the run's records; the review
model (`REVIEW_MODEL`, `moonshotai/kimi-k3`) receives the request, the diff and
the test output; the decision model that picks each launch's model
(`typesafe/jev-1.13`) receives the request; and the chat model that decides at
the entrance receives the request and the earlier reports.
`model_selection.authors` narrows the publishers, and `model_selection.fixed`
names one model for every launch (rewrite/README.md, "Naming one model instead
of selecting").

### On your workstation

`kubectl` for the cluster, `python3`, and `sed`. Go 1.25 or later to try each
new image before it goes in (section 10).

## 2. Preparing the tracker

### The engine's own account

1. Create an account for the engine and add it to the project as a member,
   and to no other project. In one installation it is an ordinary member, not
   an administrator.
2. Issue an API key for that account (Backlog: the account's personal
   settings, API). This is `TRACKER_API_KEY`. Everything the engine posts or
   changes appears under this account's name.
3. What the engine does with the account (from `rewrite/internal/tracker`):
   lists the project's issues, reads issues and their comments, posts
   comments, deletes comments it posted itself (a role with `comment` access
   leaves one comment per launch), changes an issue's status, categories,
   assignee and actual hours, and reads which account it is.

**Never file requests as the engine's account.** The person who files an
issue is the requester: only their comments (and those of the users in
`intake.stop_user_ids`) count as an answer or a stop. An issue filed with the
engine's key has the engine as its requester: the person who actually asked
can then neither answer nor stop it from their own account, and a comment
with words from the engine's account can be taken for the answer.

### Statuses and a category (optional, but the requester sees them)

`intake.statuses` moves the issue's status at four turns. Decide which status
means what; a turn you leave out is left alone.

| Turn | When |
| --- | --- |
| `processing` | the request is accepted, and while the engine works on it |
| `awaiting_requester` | a question waits for the requester's reply |
| `delivered` | the change is merged and the report is posted |
| `stopped` | the requester posted a stop |

Backlog's built-in Open and Resolved statuses have the ids 1 and 3
(rewrite/README.md). For the turns that need their own name, add custom
statuses to the project if your space allows them, for example "Automation
running" and "Waiting for the requester".

`intake.category_on_accept` adds one category to every accepted issue, so the
board can filter them. Create the category in the project first.

### Looking up the ids

The configuration takes numeric ids. This reads them through Backlog's API v2
([developer.nulab.com/docs/backlog](https://developer.nulab.com/docs/backlog/):
the project, its statuses, categories and users, and the key's own account)
with the engine account's key, which it takes from an environment variable
and never prints. Run it on your workstation; a space on backlog.jp has its
API at `https://<space>.backlog.jp/api/v2` instead:

```sh
read -rs TRACKER_API_KEY && export TRACKER_API_KEY   # paste the key; nothing is shown
python3 - 'https://<space>.backlog.com/api/v2' '<PROJECT_KEY>' <<'EOF'
import json, os, sys, urllib.error, urllib.parse, urllib.request
base, project = sys.argv[1].rstrip("/"), sys.argv[2]
def get(path):
    query = urllib.parse.urlencode({"apiKey": os.environ["TRACKER_API_KEY"]})
    try:
        with urllib.request.urlopen(base + path + "?" + query, timeout=30) as answer:
            return json.load(answer)
    except urllib.error.HTTPError as error:
        sys.exit("%s answered HTTP %d" % (path, error.code))
print("project  %10d  %s" % (get("/projects/" + project)["id"], project))
for status in get("/projects/" + project + "/statuses"):
    print("status   %10d  %s" % (status["id"], status["name"]))
for category in get("/projects/" + project + "/categories"):
    print("category %10d  %s" % (category["id"], category["name"]))
for user in get("/projects/" + project + "/users"):
    print("member   %10d  %s" % (user["id"], user["name"]))
me = get("/users/myself")
print("this key acts as %d (%s)" % (me["id"], me["name"]))
EOF
unset TRACKER_API_KEY
```

`<PROJECT_KEY>` is the prefix of the project's issue keys (`EXAMPLE` in
`EXAMPLE-1`). The last line must name the engine's account, not yours. This
script was syntax-checked but not run against Backlog for this guide.

| Id | Goes into |
| --- | --- |
| the project | `intake.project_id` |
| the statuses you chose | `intake.statuses` (`processing`, `awaiting_requester`, `delivered`, `stopped`) |
| the category | `intake.category_on_accept` |
| members who may stop or answer any request besides its requester | `intake.stop_user_ids` |
| one issue, for the first ticket only | `intake.issue_ids` ([section 8](#8-opening-the-intake-and-filing-the-first-ticket)) |

## 3. Preparing the delivery repository

### The token

The token is `DELIVERY_GITHUB_TOKEN`. Limit it to the one repository, and give
it no permission over the repository's workflows. What uses it, from the
scripts in `rewrite/harnesses/`:

- the mirror (`mirror_loop.py`) clones and fetches the repository;
- the delivery (`deliver_git.py`) pushes `ticket/<ISSUE-KEY>`, lists, opens
  and reads pull requests, merges one, and reads the repository and the
  integration branch in its check mode;
- the merged check (`verify_merged.py`) clones the integration branch.

That is read and write access to the repository's contents and to its pull
requests. On GitHub, a fine-grained personal access token restricted to this
repository with Contents and Pull requests set to read and write covers those
calls. Without the Workflows permission, a change under `.github/workflows/`
cannot be pushed, which is the point. The smallest working set has not been
measured here. The check mode in [section 7](#7-checking-that-it-is-up)
shows that the token can read the repository and list the branch; it cannot
show that a merge will be accepted. A token with an expiry date stops the
mirror on that date (the `mirror` container ends and restarts), so note the
date.

### Rules that would refuse the merge

The shipped delivery merges its own pull request through the API right after
opening it, and it does not wait. That is the only reason the integration
branch's rules matter here: anything the delivery identity cannot satisfy at
that moment makes GitHub refuse the merge, the work goes back to `work`, and
every later stage runs again, round after round, until the rule changes or
the requester posts a stop ([section 11](#a-delivery-is-refused-by-a-branch-rule)).
Check the integration branch for:

- required approving reviews;
- required status checks (the merge is attempted right after the pull
  request opens, before any check can have finished);
- required signed commits (the delivery's commits are not signed);
- required linear history, merge queues, or push restrictions that leave out
  the delivery identity;
- the repository setting that allows merge commits, when the merge method is
  `merge` (the shipped one).

If you keep such rules for people, let the delivery identity bypass them where
your plan offers that, and keep the branch one that nothing deploys from by
itself (section 1). The delivery token often cannot read the protection
settings itself, so look at them as an administrator.

### The branch must pass before you start

The merged check runs your build and tests against the integration branch
after every merge. A branch that fails them already will fail them for every
request. The check mode in section 7 runs them once against the branch as it
is; resolve any failure before the first ticket.

### Requests running side by side

With `intake.max_running` above 1, two requests can start from the same
integration branch, and the second to deliver finds the branch moved. Under
the merge method the delivery first merges the integration branch into the
ticket branch: a clean merge becomes a merge commit, and a conflict is left in
the checkout between Git's conflict markers and the delivery is refused naming
the paths, so the next `work` launch resolves them in place. With `squash` or
`rebase` there is no such catch-up; keep `max_running` at 1 with those.

## 4. Writing the operator configuration

`rewrite/examples/operator-stages.json` is complete for the runtime of this
directory: its delivery and merged check are the image's fixed processes
under `/opt/ticket-automation/scripts`, its build, tests and report check are
the operator scripts of section 6, and every checkout comes from the Pod's
mirror. Every value you must change is one distinct string in it.

### Make your copy with one command

Run this from the repository's root, with your values in place of the
`<...>` parts (none of them may contain `#` or `&`), and keep the result
outside any repository:

```sh
sed -e 's#https://tracker.example.invalid/api/v2#https://<space>.backlog.com/api/v2#' \
    -e 's#"project_id": 0,#"project_id": <project-id>,#' \
    -e 's#REPLACE_WITH_RFC3339_ACCEPTANCE_START#2100-01-01T00:00:00Z#' \
    -e 's#example-owner/example-repository#<owner>/<repository-name>#g' \
    -e 's#example-integration-branch#<integration-branch>#g' \
    rewrite/examples/operator-stages.json > operator.json
```

| What the command sets | Where it comes from |
| --- | --- |
| `backlog.base_url` | your space's API address (`.backlog.jp` for a space there) |
| `intake.project_id` | section 2 |
| `intake.created_since` | `2100-01-01T00:00:00Z` on purpose: the engine accepts nothing until section 8 opens the intake. The engine starts with this value; issues are taken only when they were created at or after it. |
| `<owner>/<repository-name>` | the delivery repository (`DELIVERY_REPOSITORY`) and the mirror's path (every `TASK_REPOSITORY`, `/var/lib/ticket-automation/mirror/<owner>/<repository-name>.git`, the same as `MIRROR_PATH` in the StatefulSet) |
| `<integration-branch>` | the branch every checkout starts from (`TASK_BRANCH`) and the delivery merges into (`DELIVERY_BASE_BRANCH`) |

Then open `operator.json` and replace the first sentence of `instructions`
("Operator setup is incomplete: ...") with your project's guidance: where the
project's own knowledge is written (a path in the repository such as
`CONTRIBUTING.md`), what the delivery target is, and anything the work must or
must not touch. Keep the rest of the paragraph; it tells every role how
progress is decided.

### Check that no example value is left

The engine checks only the configuration's structure when it starts (stage
kinds, role names, the form of each setting). Values it cannot judge, such as
the example's repository and branch, an unedited instruction or a model id
that does not exist, are accepted. A request then fails where such a value is
used (a checkout from a mirror that is not there, a push to a repository that
does not exist) and goes round without end, with a model chosen, a paid call,
on every launch; an unedited instruction misleads every role without failing
anything. Before you go on:

```sh
python3 -m json.tool operator.json > /dev/null && echo "valid JSON"
grep -n -E 'example\.invalid|example-owner|example-repository|example-integration-branch|REPLACE_WITH|Operator setup is incomplete|"project_id": 0,|<[a-z-]+>' operator.json \
  && echo "the lines above still carry example values" || echo "no example value left"
```

The first line must print `valid JSON` and the second `no example value
left`.

### Settings you may leave

- `router` (`mode: stages`, and `llm` for the entrance decision) and
  `model_selection` (OpenRouter's decision model chooses a model for each
  launch among the publishers in `authors`; section 1 says where that sends
  the code). To run every model launch on one model instead, set
  `model_selection.fixed` to its `publisher/model` id.
- `intake.poll_interval_seconds` (30), `intake.max_running` (1),
  `intake.stop_report_role`, `intake.question_role`.
- The roles' commands, their `model_env` (`NATIVE_MODEL`), their `secrets`
  mappings and their `tracker_access` values.
- `REVIEW_MODEL`: the model that reviews, as the endpoint names it.
- `DELIVERY_ALLOWED_PATHS` (`.`, the whole tree: which files a change needs is
  not known before the work; the checkout's `.git` stays out of reach of every
  model role either way) and `DELIVERY_MERGE_METHOD` (`merge`).
- `NATIVE_MAX_TOKENS`: unset, the bridge allows 32000 output tokens per model
  answer ([section 11](#work-ends-without-a-change-or-a-model-stage-keeps-failing-with-ended-without-a-report)).

Optional in the delivery process's `env`: `DELIVERY_FORBIDDEN_TEXT`
(newline-separated text the delivery refuses to carry, such as internal
names) and `DELIVERY_AUTHOR_NAME` / `DELIVERY_AUTHOR_EMAIL` for its commits,
which are otherwise authored as "ticket engine". The pull request is titled
`Deliver <ISSUE-KEY>`, comes from the branch `ticket/<ISSUE-KEY>`, and is
merged as `Deliver <ISSUE-KEY> (#<number>)`.

Do not remove `model_env` from a model stage's process, and do not put a
model id into its `env` instead: the engine refuses to start
([section 11](#the-engine-container-restarts-right-after-it-starts)).

### Settings the requester will notice

None of these is required. Each changes what appears on the issue
([section 8](#what-the-requester-sees-comment-by-comment) shows the result):

```json
"intake": {
  "announce": true,
  "declare_models": true,
  "status_page": "https://<status-host>/jobs/",
  "statuses": {"processing": 1001, "awaiting_requester": 1002, "delivered": 3, "stopped": 1},
  "category_on_accept": 2001,
  "assign": true,
  "stop_user_ids": [3001]
}
```

(merged into the existing `intake` object; 1001, 1002, 2001 and 3001 stand for
your own ids from section 2.)

- `announce`: fixed comments when a request is accepted, when it starts after
  waiting its turn, and when it resumes after an answer; each stage's own
  sentence when it first begins (below); the list of models used once the
  request is delivered. Without `announce`, no stage sentence is posted.
- `declare_models`: says which model was chosen when a model stage begins,
  and again when a later launch of it runs on a different model.
- `status_page`: only if the status page is reachable from the requester's
  browser (`status-ingress.yaml.example`); the acceptance comment then links
  to the request's own page.
- `assign`: hands the issue to the requester while a question or the
  delivered result waits for them, back to the engine's account while it
  works, and records the hours from acceptance to the report.
- `stall_notice_minutes` (default 90; 0 turns it off): rewrite/README.md,
  "What the requester is told at night".
- `min_model_credit` (default off) reads the remaining limit of the key named
  by `router.decision.key_env`, from `intake.model_credit_url` (by default
  OpenRouter's `https://openrouter.ai/api/v1/key`, asked on every poll). The
  shipped configuration has no `router.decision`, and with `min_model_credit`
  alone the engine refuses to start. Add the decision service as
  `rewrite/examples/operator.json` names it
  (`"decision": {"url": "https://openrouter.ai/api/alpha/decisions", "model": "typesafe/jev-1.13", "key_env": "MODEL_API_KEY"}`
  inside `router`); the entrance decision then goes to that service instead of
  the chat API. The engine accepted the two together.

A stage can carry a sentence of yours, posted once when it first begins if
`announce` is on. Section 8 assumes these two, in `workflow`:

```json
"stages": [
  {"name": "elicit", "kind": "model"},
  {"name": "work", "kind": "model", "announce": "自動実装を開始しました。"},
  {"name": "verify", "kind": "command", "on_failure": "work"},
  {"name": "review", "kind": "command", "on_failure": "work"},
  {"name": "deliver", "kind": "command", "on_failure": "work", "announce": "納品先へのマージを始めました。マージ後の検証と報告を続けます。"},
  {"name": "verify_merged", "kind": "command", "on_failure": "work"},
  {"name": "report", "kind": "model"},
  {"name": "confirm_report", "kind": "command", "on_failure": "report"}
]
```

Write what is true when the stage begins: the `deliver` sentence is posted
before anything is merged ([section 11](#the-issue-says-merged-while-the-delivery-is-refused)).

### With a gateway in front of the models

`rewrite/examples/operator-gateway.json` shows the gateway settings for the
routed form, and rewrite/README.md ("Invoking through a gateway") explains
them. This ordered configuration needs them and the review's as well:

- `model_selection.gateway` with the gateway's `models_url`, `key_env` and
  `prefix`;
- **every** process's `env.OPENROUTER_BASE_URL` and
  `secrets.OPENROUTER_API_KEY`, `stop_report` included, and `router.llm`'s
  `url` and `key_env`;
- the review's `REVIEW_MODEL_URL` (the gateway's chat completions URL),
  `REVIEW_MODEL` (with the gateway's prefix) and its `secrets` mapping;
- if the gateway does not serve OpenRouter's decisions API (the one measured
  for rewrite/README.md answered 405), remove `model_selection.judge` (and
  `router.decision`, if you added it), name the gateway's chat endpoint and a
  model it serves, prefix included (`openrouter/publisher/model`), in
  `model_selection.fallback`, and remove `intake.min_model_credit`. Every
  choice is then made by that chat model.

Then put `GATEWAY_API_KEY` in the Secret and uncomment it in the StatefulSet,
and use the gateway's variants of the checks in section 7. This ordered
configuration with every change above was accepted by the engine from the
same commit, which began polling. A process left pointing at the provider
directly fails at its first model call
([section 11](#the-stop-report-never-starts-behind-a-gateway)), and the
gateway's own request timeout can cut long answers
([section 11](#long-answers-fail-with-http-504-through-a-gateway)).

## 5. The secrets

[secrets.yaml.example](secrets.yaml.example) lists the keys of the two
Secrets the StatefulSet reads, each with a comment on where its value comes
from:

| Secret | Key | Value |
| --- | --- | --- |
| `<consumer>-ticket-engine` | `MODEL_API_KEY` | the model provider's key (OpenRouter with the shipped configuration) |
| | `TRACKER_API_KEY` | the engine account's Backlog API key (section 2) |
| | `DELIVERY_GITHUB_TOKEN` | the delivery token (section 3) |
| | `GATEWAY_API_KEY` | only with a gateway |
| `<consumer>-ticket-engine-status` | `STATUS_USER`, `STATUS_PASSWORD` | basic authentication for the status page; choose them yourself |

Every key the StatefulSet reads must exist in the Secret, or the Pod does not
start: to leave out `MODEL_API_KEY` with a gateway, delete its three lines in
the StatefulSet as well. Never commit a filled copy. From here on the values
are referred to by name only.

## 6. Applying the manifests, in order

Copy each `*.example` in this directory, drop the `.example`, and replace
every `<placeholder>`. Keep the copies outside this repository: they name your
repository and your cluster.

```sh
NS=<namespace>
POD=<consumer>-ticket-engine-0
```

1. **The namespace**, admitting this Pod (section 1):

   ```sh
   kubectl create namespace "$NS"
   kubectl label namespace "$NS" pod-security.kubernetes.io/enforce=privileged --overwrite
   ```

2. **The Secrets** (section 5), from your filled copy, which you then delete;
   or with the secret path you already use:

   ```sh
   kubectl -n "$NS" apply -f secrets.yaml
   rm secrets.yaml
   ```

3. **The egress rules.** In your copy of `egress-configmap.yaml.example`,
   `<dns-cluster-ip>` is the address the Pods' `/etc/resolv.conf` names (the
   file's comment shows how to read it). Then:

   ```sh
   kubectl -n "$NS" apply -f egress-configmap.yaml
   ```

4. **The operator configuration**, from your `operator.json`, with the intake
   still closed (`created_since` in 2100):

   ```sh
   kubectl -n "$NS" create configmap <consumer>-ticket-engine-operator \
     --from-file=operator.json=operator.json --dry-run=client -o yaml \
     | kubectl -n "$NS" apply -f -
   ```

   The same command updates it later. The engine reads it when it starts, so
   a change takes effect at the next restart of the Pod.

5. **The operator's programs.** In your copy of
   `operator-scripts-configmap.yaml.example`, replace `build` and `test`
   with your project's commands (they fail on purpose until you do) and keep
   `confirm-report` as it is. Then apply it.

6. **A server-side dry run of the StatefulSet**, and a read-back of what
   admission did to it:

   ```sh
   kubectl -n "$NS" create --dry-run=server -f statefulset.yaml -o json | python3 -c '
   import json, sys
   spec = json.load(sys.stdin)["spec"]["template"]["spec"]
   print(json.dumps({"hostUsers": spec.get("hostUsers"),
                     "automountServiceAccountToken": spec.get("automountServiceAccountToken"),
                     "envFrom": [c.get("envFrom") for c in spec["containers"]],
                     "restartPolicy": {c["name"]: c.get("restartPolicy") for c in spec["initContainers"]},
                     "securityContext": {c["name"]: {k: c.get("securityContext", {}).get(k) for k in ("procMount", "seccompProfile", "capabilities")}
                                         for c in spec["initContainers"] + spec["containers"]}}, indent=1))'
   ```

   Expect `hostUsers` false, `automountServiceAccountToken` false, no
   `envFrom`, `mirror` with `restartPolicy` `Always`; the engine with
   `procMount` `Unmasked` and `seccompProfile` `Unconfined`; every other
   container with `RuntimeDefault`; and `NET_ADMIN` added only for
   `network-v4` and `network-v6`. Anything else means an admission controller
   changed the StatefulSet: stop and find out why. If the API server refuses
   `restartPolicy` on an init container, move the `mirror` entry to
   `containers` (the comment on that entry says what that costs). Pod Security
   is enforced on the Pod the StatefulSet creates, not on this dry run: a
   refusal there shows in the StatefulSet's events after step 7.

7. **The StatefulSet:**

   ```sh
   kubectl -n "$NS" apply -f statefulset.yaml
   kubectl -n "$NS" wait --for=condition=Ready "pod/$POD" --timeout=10m
   ```

   If the wait ends without the Pod ready, or reports that there is no Pod,
   see [section 11](#the-pod-does-not-become-ready).

8. **The status page's Service**, and, only if you decide to expose the page,
   its Ingress (`status-ingress.yaml.example`; read its comments first). The
   page needs neither to be checked: section 7 uses a port-forward.

   ```sh
   kubectl -n "$NS" apply -f status-service.yaml
   ```

## 7. Checking that it is up

Each check has an answer you can see. One that cannot be answered is a
blocker, not something to note and pass. The intake is still closed, so
nothing here can start a request.

### The Pod and its setup containers

```sh
kubectl -n "$NS" get pod "$POD"
kubectl -n "$NS" logs "$POD" -c policy
kubectl -n "$NS" logs "$POD" -c network-v4
kubectl -n "$NS" logs "$POD" -c mirror --tail=5
```

- The Pod is `Running`, every container ready, no restarts.
- `policy` printed one JSON line with `"capabilities": []`, a
  `compiled_rules` count and `"native_architecture": "arm64"`.
- `network-v4` and `network-v6` printed nothing.
- `mirror` printed `Mirroring <owner>/<repository-name> into ... every 60 seconds`
  and, on the first start, `Created the mirror at /var/lib/ticket-automation/mirror/<owner>/<repository-name>.git`.
  Its startup check passes only once the copy holds a branch, so the engine
  starts after the first copy is complete.

### The status page

```sh
kubectl -n "$NS" port-forward "pod/$POD" 9200:9200
```

Open <http://127.0.0.1:9200/> in a browser. It asks for the user and password
from the status Secret; without them it answers 401. Signed in, an empty queue
shows the counters (Queued, Running, Awaiting answer, Needs attention,
Delivered, Stopped) at 0, "No request has been accepted into this queue yet.",
and one column per configured stage. <http://127.0.0.1:9200/healthz> answers
`ok` without signing in. `/config` shows the configuration as the page read
it, and `/log` the engine's log. The labels switch to Japanese from the link
at the top of the page.

### The engine's log

```sh
kubectl -n "$NS" logs "$POD" -c engine
```

A healthy engine with nothing to do prints nothing. These lines mean
something is wrong:

| Line | Meaning |
| --- | --- |
| `issue discovery unavailable: tracker credential is unavailable` | `TRACKER_API_KEY` is empty |
| `issue discovery unavailable: tracker returned HTTP 401: ...` | the tracker refused the key |
| `issue discovery unavailable: ... dial tcp ...` | the tracker cannot be reached (egress rules, DNS, the URL) |
| `the runtime's own tracker account is unknown; issues are not handed over either way: ...` | with `assign` on, the engine could not read its own account; it works, but hands nothing over until a restart |
| `waiting to receive or resume request: the run directory belongs to a different request` | the volume's queue belongs to another tracker URL or project ([section 11](#a-configuration-change-has-no-effect-or-the-engine-waits-on-a-different-request)) |

### Before it accepts work

None of these hands the engine a request or prints a credential.

**The role launcher starts a role** (rewrite/RUNTIME.md, "Check the target
runtime before accepting work"). Exit status 0 is the only acceptable answer:

```sh
kubectl -n "$NS" exec -i "$POD" -c engine -- /bin/sh -s <<'CHECK'
d=$(mktemp -d /tmp/ticket-runtime-check.XXXXXXXX) || exit
mkdir "$d/work" "$d/home" || exit
s=0
env -i PATH=/runtime-policy/bin:/usr/bin:/bin TASK_WORKSPACE="$d/work" TASK_HOME="$d/home" \
  python3 -B /opt/ticket-automation/bundle/harnesses/linux_role.py --network none -- /bin/true || s=$?
rm -rf "$d"
exit "$s"
CHECK
echo "launcher check exit: $?"
```

**A role runs confined.** This launches a short Python program as a role and
reports what it can do:

```sh
kubectl -n "$NS" exec -i "$POD" -c engine -- /bin/sh -s <<'CHECK'
d=$(mktemp -d /tmp/ticket-policy-check.XXXXXXXX)
mkdir "$d/work" "$d/home"
printf 'probe-original\n' > "$d/work/request.txt"
env -i PATH=/runtime-policy/bin:/usr/bin:/bin TASK_WORKSPACE="$d/work" TASK_HOME="$d/home" \
  python3 -B /opt/ticket-automation/bundle/harnesses/linux_role.py --network inherit -- \
  python3 -B -c '
import ctypes, json, pathlib, socket
r = {"status": {k: v.strip() for k, v in (line.split(":", 1) for line in open("/proc/self/status"))
                if k in ("Seccomp", "Seccomp_filters", "CapEff", "CapPrm", "NoNewPrivs")}}
for label, host, port in (("public", "1.1.1.1", 443), ("metadata", "169.254.169.254", 80)):
    try:
        with socket.create_connection((host, port), timeout=2): r[label] = "connected"
    except OSError as error: r[label] = type(error).__name__
r["request_file"] = pathlib.Path("request.txt").read_text().strip()
try:
    pathlib.Path("forbidden.txt").write_text("must not write"); r["workspace_write"] = "ALLOWED"
except OSError as error: r["workspace_write"] = error.errno
r["unshare_return"] = ctypes.CDLL(None, use_errno=True).unshare(0x10000000)
print(json.dumps(r))
'
rm -rf "$d"
CHECK
```

Expect `Seccomp` 2, `Seccomp_filters` 1, `CapEff` and `CapPrm` all zeros,
`NoNewPrivs` 1, `public` connected, `metadata` `ConnectionRefusedError`,
`request_file` `probe-original`, `workspace_write` an error number (1, 13 or
30; `ALLOWED` fails the check), and `unshare_return` -1.

**Egress, both ways.** `<kubernetes-service-ip>` is
`kubectl -n default get service kubernetes -o jsonpath='{.spec.clusterIP}'`;
with a gateway, add its host to the targets:

```sh
kubectl -n "$NS" exec "$POD" -c engine -- python3 -B -c '
import json, socket
targets = {"tracker": ("<space>.backlog.com", 443), "models": ("openrouter.ai", 443),
           "delivery": ("github.com", 443), "delivery-api": ("api.github.com", 443),
           "metadata": ("169.254.169.254", 80), "cluster-api": ("<kubernetes-service-ip>", 443)}
result = {}
for name, address in targets.items():
    try:
        with socket.create_connection(address, timeout=3): result[name] = "connected"
    except OSError as error: result[name] = type(error).__name__
print(json.dumps(result))'
```

The first four (and the gateway) must be `connected`, `metadata` and
`cluster-api` `ConnectionRefusedError`. An applied manifest is not evidence
that anything is refused; this is.

**The credentials, by name only** (with a gateway, add `GATEWAY_API_KEY`):

```sh
kubectl -n "$NS" exec "$POD" -c engine -- python3 -B -c '
import json, os
print(json.dumps({name: "set" if os.environ.get(name) else "unset"
                  for name in ("MODEL_API_KEY", "TRACKER_API_KEY", "DELIVERY_GITHUB_TOKEN")}))'
```

All `set`. Never `printenv`, `echo` a variable or print a Secret.

**Connections, with no work to do:**

```sh
kubectl -n "$NS" exec "$POD" -c engine -- /opt/ticket-automation/bundle/bin/ticket-engine --list-models \
  | python3 -c 'import json, sys; c = json.load(sys.stdin); print(c["fetched_at"], len(c["data"]), "models")'
kubectl -n "$NS" exec "$POD" -c engine -- /opt/ticket-automation/bundle/bin/ticket-tracker \
  --base-url https://<space>.backlog.com/api/v2 --key-env TRACKER_API_KEY --project-id <project-id> issues \
  | python3 -c 'import json, sys; rows = json.load(sys.stdin); print(len(rows), "issues"); [print(r["id"], r["issueKey"], r["created"]) for r in rows[-3:]]'
kubectl -n "$NS" exec "$POD" -c engine -- \
  git --git-dir=/var/lib/ticket-automation/mirror/<owner>/<repository-name>.git log -1 --format='%H %ci' <integration-branch>
```

**The delivery and the merged check, in check mode.** Both end with exit
status 3 on purpose: `--dry-run` commits, pushes, opens and merges nothing.

```sh
kubectl -n "$NS" exec -i "$POD" -c engine -- /bin/sh -s <<'CHECK'
export GITHUB_TOKEN="$DELIVERY_GITHUB_TOKEN"
export DELIVERY_REPOSITORY=<owner>/<repository-name> DELIVERY_BASE_BRANCH=<integration-branch>
d=$(mktemp -d /tmp/delivery-check.XXXXXXXX)
export TASK_WORKSPACE="$d/workspace" TASK_HOME="$d/home"
git -c core.hooksPath=/dev/null clone --quiet --no-local --branch <integration-branch> \
  /var/lib/ticket-automation/mirror/<owner>/<repository-name>.git "$TASK_WORKSPACE"
cd "$TASK_WORKSPACE" || exit
TASK_ISSUE=CHECK-0 DELIVERY_ALLOWED_PATHS=. \
  python3 -B /opt/ticket-automation/scripts/deliver_git.py --dry-run
echo "deliver check exit: $?"
VERIFY_COMMANDS='/opt/ticket-automation/operator/build
/opt/ticket-automation/operator/test' \
  python3 -B /opt/ticket-automation/scripts/verify_merged.py --dry-run
echo "verify check exit: $?"
cd / && rm -rf "$d"
CHECK
```

The delivery check reports that reading the repository and the integration
branch answered status 200 and that listing the branch over Git exited 0. The
merged check lists each command with its exit status and ends with
`0 of 2 configured verification commands failed.` Anything else there means
the integration branch fails your own checks today
([section 3](#the-branch-must-pass-before-you-start)).

**Your test script inside a role.** The verify stage runs it confined and
with the checkout read-only, which is not how it ran in the check above:

```sh
kubectl -n "$NS" exec -i "$POD" -c engine -- /bin/sh -s <<'CHECK'
d=$(mktemp -d /tmp/test-check.XXXXXXXX)
git -c core.hooksPath=/dev/null clone --quiet --no-local --branch <integration-branch> \
  /var/lib/ticket-automation/mirror/<owner>/<repository-name>.git "$d/work"
env -i PATH=/runtime-policy/bin:/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin \
  TASK_WORKSPACE="$d/work" TASK_HOME="$d/home" \
  python3 -B /opt/ticket-automation/bundle/harnesses/linux_role.py \
  --runtime /opt/ticket-automation/operator --network inherit -- /opt/ticket-automation/operator/test
echo "test inside a role exit: $?"
rm -rf "$d"
CHECK
```

Exit 0 is required; a test that only passes with a writable checkout fails
every request at `verify`.

**The queue survives a restart.** Delete the Pod once and look again:

```sh
kubectl -n "$NS" delete pod "$POD"
kubectl -n "$NS" wait --for=condition=Ready "pod/$POD" --timeout=10m
kubectl -n "$NS" exec "$POD" -c engine -- ls /var/lib/ticket-automation/queue /var/lib/ticket-automation/mirror
kubectl -n "$NS" exec "$POD" -c engine -- df -h /var/lib/ticket-automation /tmp
```

`history.json`, `jobs`, `notice-kinds.json` and `runner.lock` are still in the
queue, the mirror is still there (its log says `Mirroring ...` without
`Created the mirror`). Keep the `df` figures: nothing prunes the volume, and a
full volume stops progress without a word, because saving history is retried
rather than given up. Compare after the first real requests.

**What only fails on a replacement node.** When its node is replaced, the Pod
starts on a node that has never run it:

```sh
kubectl -n "$NS" get statefulset <consumer>-ticket-engine \
  -o jsonpath='{range .spec.template.spec.initContainers[*]}{.name}{"\t"}{.image}{"\t"}{.imagePullPolicy}{"\n"}{end}'
kubectl -n "$NS" get statefulset <consumer>-ticket-engine \
  -o jsonpath='{range .spec.template.spec.volumes[*]}{.name}{"\t"}{.emptyDir.sizeLimit}{"\n"}{end}'
```

Every image must be one any node can pull (the published image needs no
credential; a private registry needs an imagePullSecret in the StatefulSet),
no init container may say `Never`, and the `temporary` volume must show no
size limit: exceeding one evicts the Pod in the middle of a request.

## 8. Opening the intake and filing the first ticket

### Open the intake

Until now `created_since` was `2100-01-01T00:00:00Z`, so the engine has taken
nothing. It stays closed until every check in section 7 has passed because
closing it again later does not stop what was already taken: an accepted
request stays in the queue and runs.

Set `intake.created_since` to a moment just before you file the first ticket,
in UTC (for example `2026-10-05T09:00:00Z`; the moment itself counts). In a
project that already has other new issues, file the ticket first, look up its
id (the `ticket-tracker ... issues` command in section 7 prints it), and also
set `intake.issue_ids` to `[<that id>]`. Change both in the same edit, apply
the ConfigMap (section 6, step 4) and restart the Pod:

```sh
kubectl -n "$NS" delete pod "$POD"
```

An `issue_ids` that lists nothing real accepts nothing; an empty one accepts
every new issue in the project. Remove `issue_ids` once you are ready for all
of them. If anything else already takes issues from this project, make sure it
cannot take the same ticket.

### What the requester sees, comment by comment

File the ticket as a person, not as the engine's account, in ordinary words.
No format is required. With the settings from section 4 (`announce`,
`declare_models`, `statuses`, `category_on_accept`, `assign`, and the
sentences for `work` and `deliver`), the issue shows the following. Status,
category and assignee changes appear in Backlog as change entries without
text. Model ids are examples.

| # | On the issue | When |
| --- | --- | --- |
| 1 | status `processing`; `受け付けました。すぐに自動処理を始めます。` with, when `status_page` is set, `進み具合はこちらで見られます: https://<status-host>/jobs/<issue id>`; the category; assignee: the engine's account | within one poll interval (30 seconds) of filing. With requests ahead of it: `受け付けました。前に 2 件あり、順番が来しだい自動処理を始めます。`, and later `自動処理を開始しました。` when its turn comes |
| 2 | `要件確定を始めます。選定モデル: maker/model-a` | within seconds of the first stage's model being chosen |
| 3 | if something is left for the requester: `依頼者への質問を始めます。選定モデル: maker/model-b`, then **the question**: one comment listing each open point with two to four choices; status `awaiting_requester`; assignee: the requester | the request now waits, without limit, for the reply |
| 4 | the requester's reply (below); then `返答を受け取りました。自動処理を再開しました。`, status `processing`, assignee: the engine's account | within one poll interval of the reply |
| 5 | `要件確定をやり直します。選定モデル: maker/model-c` | the run returns to its first stage with the answer in hand. If the stage runs on the same model as before, nothing is said |
| 6 | `自動実装を開始しました。 (モデル: maker/model-d)` | the `work` stage's own sentence, carrying the model it chose; without a sentence, `作業を始めます。選定モデル: ...` |
| 7 | `作業をやり直します。選定モデル: maker/model-e` | only if a check or the review sent the work back and it now runs on a different model |
| 8 | `納品先へのマージを始めました。マージ後の検証と報告を続けます。` | the `deliver` stage begins; the merge follows |
| 9 | `報告を始めます。選定モデル: maker/model-f`, then **the report** | after the merged check passed |
| 10 | status `delivered`; assignee: the requester; the actual hours | once the report is confirmed on the issue |
| 11 | `使ったモデル (工程ごと、起動順):` and one line per stage, for example `- 要件確定: maker/model-a (27 秒) — 再実行: maker/model-c (31 秒)` | right after |

Nothing is said for `verify`, `review` and `verify_merged` unless you give
them a sentence. The status page shows every stage as it runs, with the
instruction each role was given and its output as it arrives.

The engine also posts three notices of its own when needed: after a restart
that interrupted the request
(`自動処理は再起動後に同じ依頼を続けています。...`, at most once in 30 minutes),
when no stage has completed for `stall_notice_minutes`, and when the model
budget falls below `min_model_credit` and recovers. rewrite/README.md ("What
the requester is told at night") has their exact words.

### Check that the review actually reviewed

On the status page, open the first request's `review` record. Its output
begins `Review by <model>: PASSED.` or `Review by <model>: SENT BACK to the
worker.`. If it begins `Review by <model>: NOT REVIEWED.`, the review could
not be performed (a wrong model id, the endpoint down, a missing key) and the
work went on without one. The line says why; fix that before you rely on the
review.

### Answering a question

Reply on the issue with an ordinary comment: the number or the words of a
choice, or anything else that answers. Only the issue's creator and the users
in `intake.stop_user_ids` can answer, only the first comment with words after
the question counts, and a status change is not an answer. The reply goes into
the run as the requester's own words; nothing checks that it answers the
question. A reply whose first non-blank line is `停止` is a stop, not an
answer.

### After it is delivered

Look at the pull request on GitHub (`Deliver <ISSUE-KEY>`, merged), at the
integration branch, and at the report. The report is written by a model from
the run's records; the status page has the records themselves.

## 9. Stopping a request

The issue's creator, or a user in `intake.stop_user_ids`, posts a comment
whose first non-blank line is exactly `停止`. A reason can follow on later
lines. Within one poll interval:

- the running process is stopped, and nothing more of the request runs;
- the status becomes `stopped` and the issue is handed to the requester (with
  those settings);
- the `stop_report` role posts an account of what had happened and what is
  uncertain.

A stop does not undo anything already done outside the cluster: a pushed
branch, an open pull request or a merge stays. It is permanent: deleting the
comment does not resume the request, and there is no resume command. To try
again, file a new issue. Quoted text, mentions and comments by anyone else
are not stops.

## 10. Upgrading to a new image

### Where a new image comes from

Each commit of the default branch is built into an image by the repository's
workflow, and `docs/DISTRIBUTION.json` records it: `image` (the reference
with its digest), `engine_sha` (the commit it was built from) and
`build_record`. Read the changes between your current `engine_sha` and the new
one before taking it:

```sh
git log --oneline <current engine_sha>..<new engine_sha> -- rewrite/
```

Look in particular for new `intake` settings, new kinds of comments, and
changed defaults.

### What the new engine does to requests already in the queue

From rewrite/README.md ("What the requester is told at night") and the
engine's code:

- **Delivered and stopped requests.** A kind of comment is posted only about
  something that happened after the queue's engines began posting that kind;
  `queue/notice-kinds.json` keeps that start for each kind. A new kind of
  comment therefore does not reach back to requests finished before the new
  image ran. Status, assignee and hours changes are not comments and have no
  such start: a turn the queue has no record of is made, so turning on
  `statuses` or `assign` later applies them to the requests already
  delivered.
- **Running requests.** A restart cuts the running stage; it starts again,
  and the issue gets the restart notice (at most once in 30 minutes).
- **Waiting requests** keep waiting for their answer.
- **The configuration.** Each request keeps the stage list it was accepted
  with. Everything else in the configuration is read again at start and
  applies to every unfinished request. Do not rename or remove a role that a
  running request's stages name.

### Try it against a copy of the queue first

A new image can do something new to old records in its first minute
([section 11](#a-new-image-posts-on-requests-that-were-already-finished)).
Run the new engine over a copy of the queue, with no credentials, and read
what it tried to do.

1. Copy the queue's records out of the Pod, without checkouts (and the
   staging copies a cut checkout preparation leaves as `.source-*`), agent
   homes, live output, the log and the lock. Copy every `history.json`
   whatever its size: a request whose history is missing from the copy looks
   unstarted, and the engine starts it.

   ```sh
   kubectl -n "$NS" exec "$POD" -c engine -- sh -c 'cd /var/lib/ticket-automation/queue &&
     find . -type f -not -path "*/workspace/*" -not -path "*/.source-*" -not -path "*/homes/*" \
       -not -path "*/live/*" -not -name engine.log -not -name runner.lock -print0 | tar --null -T - -cf -' > queue-copy.tar
   mkdir queue-copy && tar -xf queue-copy.tar -C queue-copy
   kubectl -n "$NS" get configmap <consumer>-ticket-engine-operator \
     -o jsonpath='{.data.operator\.json}' > operator-current.json
   ```

2. Build the engine for your workstation from the new commit (Go 1.25 or
   later; the output directory must not exist yet):

   ```sh
   git -C <checkout of this repository> checkout <new engine_sha>
   (cd <checkout of this repository>/rewrite && sh package.sh /absolute/path/to/new-bundle)
   ```

3. Run it over the copy with an empty environment, so it holds no credential,
   for two or three poll intervals, then stop it with Ctrl-C:

   ```sh
   env -i PATH=/usr/bin:/bin /absolute/path/to/new-bundle/bin/ticket-engine \
     --config operator-current.json --watch --run-dir queue-copy 2>&1 | tee rehearsal.log
   ```

   Without a credential every tracker call fails before it leaves your
   machine, and the engine logs each write it could not make. Lines with
   `not set`, `not announced`, `not declared`, `not recorded`, `not handed`
   or `not confirmed` name a request and a change the new image would make
   on the issue. `issue discovery unavailable`,
   `stop instructions could not be read`,
   `work paused while stop instructions are unavailable` and
   `the runtime's own tracker account is unknown` are only the missing
   credential speaking; the last one also means hand-overs of the assignee
   are not tried in this run.

This procedure was checked against a small queue made for the purpose (one
request delivered before its queue's notice kinds began, one after, never
announced): the log named a status change for both and the model list for the
second only, and the copy recorded the first one's list as predating its kind.
It has not been run as written against a real queue.

### Rolling it out

1. If you can, wait until the status page shows nothing running.
2. Put the new `image` reference into all four places in your
   `statefulset.yaml` (`policy`, `mirror`, `engine`, `status`) and apply it.
   The Pod is replaced.
3. Check as in section 7: the Pod ready with no restarts, the status page,
   the engine's log, and the issues of any request that was running.

## 11. Troubleshooting

An entry that says "Met" was met at one installation. The others follow from
the code they name, from how Kubernetes reports a Pod, or from the engine's
tests, and were not met there.

### The Pod does not become Ready

```sh
kubectl -n "$NS" get events --sort-by=.lastTimestamp | tail -n 30
kubectl -n "$NS" describe statefulset <consumer>-ticket-engine
kubectl -n "$NS" describe pod "$POD"
kubectl -n "$NS" logs "$POD" -c <container> --previous
```

- **No Pod at all.** The StatefulSet's events carry the API server's refusal;
  `violates PodSecurity` means the namespace's level (section 1).
- **`Pending`.** No node matches the node selector and tolerations, or the
  volume claim has no volume (the storage class).
- **An init container failing.** `network-v4` or `network-v6`: the image has
  no `/usr/sbin/xtables-nft-multi`, or a rule does not parse (their logs say
  which). `policy`: its log.
- **Containers that cannot be created**, with events about user namespaces or
  mounts: the node or its container runtime does not support `hostUsers:
  false` (section 1).
- **`CreateContainerConfigError`.** A Secret or a key that the StatefulSet
  names is missing (section 5).
- **The engine never starts while `mirror` runs.** The mirror's startup check
  waits until the copy holds a branch: a large repository is still copying, a
  repository without any commit never passes, and a refused token ends the
  container (below).

### The engine container restarts right after it starts

The engine checks its configuration's structure before it does anything and
exits with one line naming what it refused; the container then restarts, and
goes on restarting. Read the line with
`kubectl -n "$NS" logs "$POD" -c engine --previous`. Three of them:

- `model stage "work" launches no model`: a model stage's process lost its
  `model_env`. Met, when one stage was meant to run on one fixed model and
  `model_env` was replaced by a model id in `env`. A model stage must keep a
  process with `model_env`; to run on one model, set `model_selection.fixed`
  instead.
- `watch requires intake.created_since as an explicit RFC3339 timestamp`: the
  time needs a time of day and a zone, `2026-10-05T00:00:00Z`.
- `intake.min_model_credit needs router.decision.key_env to name the model credential`:
  see [section 4](#settings-the-requester-will-notice).

These three lines were produced by the engine from the same commit, given
each mistake on purpose. A value the engine cannot judge does not stop it
([section 4](#check-that-no-example-value-is-left)).

### A request goes round without end

Every failing command stage sends the work back without a limit. Read the
stage that keeps failing on the status page; its output says why. The usual
causes: a value still from the example (section 4), a missing credential
(`A configured role credential is unavailable: <name>` in the record), your
`build` or `test` failing in the read-only checkout (section 7), a branch rule
or a conflict (below). Stop the request (`停止`) while you fix the cause.

### Work ends without a change, or a model stage keeps failing with "ended without a report"

A model answer is cut off at the bridge's output limit, `NATIVE_MAX_TOKENS`.
Met when a role had to write a large file in one tool call under a limit lower
than today's default: the answer never fit, and the work ended with no
change. The bridge now allows 32000 by default, and an answer still cut off
after its continuations makes the role fail with
`Native agent ended without a report (an answer cut off at NATIVE_MAX_TOKENS is one cause)`,
so the stage is launched again instead of handing an empty result on. If you
set `NATIVE_MAX_TOKENS` lower, or a gateway or provider caps output lower,
expect this again.

### Long answers fail with HTTP 504 through a gateway

A gateway between the engine and the provider has its own limit on how long
one request may take. Met: one gateway's default of 300 seconds cut every
long answer, and the role received 504, whatever `NATIVE_MAX_TOKENS` allowed.
Set the gateway's upstream request timeout above the longest answer you allow
(`NATIVE_MAX_TOKENS` divided by your slowest model's output rate). The
engine's own limit on a routing or selection request is 60 minutes.

### The run keeps going back to the report stage

A report check that wants the report to be the last comment fails once the
engine posts anything after it (a relaunch's model declaration, a stage
sentence, a restart notice). The check in
`operator-scripts-configmap.yaml.example` looks through the comments newest
first; keep it. Its line on the status page says what it found.

### A delivery is refused by a branch rule

The `deliver` stage's output says `Nothing was delivered for <ISSUE-KEY>.`
and `The delivery service refused the merge (status 405): ...`, and the
request goes round `work`, `verify`, `review` and `deliver` again and again,
each round costing a model launch and a review. Met: a rule requiring an
approving review was added to an integration branch while requests were
running, and every delivery was refused ("At least 1 approving review is
required") until it was removed. Change the rule or let the delivery identity
bypass it ([section 3](#rules-that-would-refuse-the-merge)); the next round
then delivers. If the rule cannot change, stop the request (`停止`).

### A delivery is refused for a conflict with another request

Requests running side by side change the same files, and the second
delivery's branch no longer merges. Met before the delivery learned to merge
the integration branch first: the request could not be delivered, and its
`work` role could not merge anything itself (its `.git` is read-only, on
purpose), so it kept searching until the requester stopped it. Now the
delivery merges the integration branch into the ticket branch, leaves
conflicts in the checkout between markers and names the paths, and the next
`work` launch resolves them. If you still see this: the merge method must be
`merge`, and the delivery process must keep both `--write .` and
`--write .git` (the example has both). Otherwise keep `intake.max_running`
at 1.

### The stop report never starts behind a gateway

When every model goes through a gateway, the ids the selection chooses are
the gateway's (with its prefix). A process still pointed at the provider
directly receives such an id and fails at its first model call. Met: the
`stop_report` role had been left on the provider, so stopped requests never
got their report. Point every model process at the gateway, `stop_report`
included ([section 4](#with-a-gateway-in-front-of-the-models)).

### The status page answers 504 through a load balancer

The Pod's egress rules reject private destinations, and a load balancer
reaches the Pod from a private address, so the replies are dropped. Met. The
rule that accepts replies from port 9200
(`-A TICKET_EGRESS -p tcp --sport 9200 ! --syn -j ACCEPT`, already in
`egress-configmap.yaml.example`) fixes it. The rules are applied when the Pod
starts: apply the ConfigMap, then restart the Pod.

### The issue says "merged" while the delivery is refused

A stage's sentence is posted when the stage begins. Met: a `deliver` sentence
saying the change was merged stood on an issue while the delivery was being
refused. Write each sentence as what is true when its stage begins
([section 4](#settings-the-requester-will-notice)).

### A new image posts on requests that were already finished

Met: an image that introduced the list of models used posted that list on
every request already delivered in the queue, before the queue kept a start
for each kind of comment. The queue keeps it now (section 10), but status,
assignee and hours changes have no such start; try each new image against a
copy of the queue first ([section 10](#try-it-against-a-copy-of-the-queue-first)).

### A configuration change has no effect, or the engine waits on a different request

The engine reads its configuration when it starts: after applying the
operator ConfigMap, restart the Pod. A request that was already accepted
keeps its stage list.

The queue on the volume belongs to one tracker URL and one project id. Started
with another, the engine does not exit; it logs
`waiting to receive or resume request: the run directory belongs to a different request`
every 10 seconds and does nothing else (observed with the engine on a
workstation). To serve another project, give it its own StatefulSet and
volume; do not delete the queue's records to make the message go away, since
they are the only record of what has already been done outside the cluster.

### The `mirror` container keeps restarting

From the mirror's code (`mirror_loop.py`). Its log ends with
`The mirror was refused and this process is ending: ...`: the token was
refused (expired, revoked, or without access to the repository), or the
repository is not there. A network failure does not end it; it logs
`The mirror was not updated, in a way that may pass on its own` and tries
again. Replace the token in the Secret and restart the Pod.

### Answers or stops from the person who asked are ignored

The issue was filed with the engine's key, so the engine's account is its
requester ([section 2](#the-engines-own-account)): only that account and the
users in `intake.stop_user_ids` can answer or stop it, and the engine cannot
tell its own comments from an answer. Met in an earlier version, which also
took the engine's own status change for the answer and went on without
waiting; comments without words are no longer answers, but the rest holds.
File requests from a person's account.

### A request that needs no change goes round without end

From the delivery script: when the work changes nothing, the delivery refuses
with `No change under the allowed paths is ready to deliver`, which sends the
work back like any other failure. Stop such a request (`停止`) and tell the
requester; the ordered configuration has no ending for "nothing to do".

## 12. Where things are

In the Pod:

| Path | What |
| --- | --- |
| `/etc/ticket-automation/operator.json` | the operator configuration (ConfigMap) |
| `/opt/ticket-automation/operator/` | the operator's programs (ConfigMap) |
| `/opt/ticket-automation/bundle/` | the engine, the tracker tool, the status page, the harnesses |
| `/opt/ticket-automation/scripts/` | the fixed delivery, merged check and mirror programs |
| `/var/lib/ticket-automation/mirror/<owner>/<repository-name>.git` | the mirror the roles clone from |
| `/var/lib/ticket-automation/queue/engine.log` | the engine's log, also shown at `/log` on the status page |
| `/var/lib/ticket-automation/queue/jobs/<issue id>/` | one request |

In a request's directory: `issue.json` (the issue as first accepted),
`run/history.json` (every stage's record), `notices.json`, `status.json` and
`turns.json` (what was posted and changed on the issue), `question.json` and
`answer-<comment id>.json` (a question and its answer), `stop-request.json`
and `stop-report/` (a stop and its report's own record), `workspace/` (the
checkout, whose `.git/ticket-engine/delivery.json` is the delivery's
receipt), `homes/` (each role's private directory) and `live/` (a running
process's output). These can hold the requester's text and project code:
keep them private.

On the status page: `/` (every request), `/jobs/<issue id>` (one request),
`/jobs/<issue id>/workspace` (its checkout now), `/files/` (every file of the
queue), `/config`, `/log`, `/healthz`.
