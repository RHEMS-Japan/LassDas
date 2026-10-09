# Setting up the ticket engine for your repository

This guide takes you from nothing to one running instance: a Pod in your
Kubernetes cluster that watches one Backlog project or labelled GitHub Issues
in one repository, takes each matching new issue as a request, works on a
checkout of one delivery repository, merges the
result into that repository's integration branch (or opens the pull request
and leaves the merge to a person, [section 4](#leaving-the-merge-to-a-person)),
and reports back on the issue. Use the source checkout for the commit the
published image was built from (`engine_sha` in `docs/DISTRIBUTION.json`).
The guide uses this directory and `rewrite/`, and refers to
`deploy/pod/Dockerfile` for image contents and the Go version. Using the
published image does not require rebuilding it. Building your own image
requires the whole source checkout and the build inputs named by that
Dockerfile, not just these template files.

Read [README.md](README.md) in this directory first. It separates what has
been measured from what is only proposed. The engine itself is described in
[rewrite/README.md](../../rewrite/README.md) and its runtime requirements in
[rewrite/RUNTIME.md](../../rewrite/RUNTIME.md). Where this guide says "one
installation", it means one consumer's instance that has run from these
templates since September 2026. What happened there is told without its
names; it cannot be checked from this repository.

**What it does not promise.** The configured `delivered` tracker status is
the final workflow turn, not proof of a merge or production deployment. A
normal changed delivery is checked on the merged branch, or on the pull
request's head when merging is left to a person. There are also endings
without a change, after a person closes a pull request, or after they change
its branch. Read the final report and delivery record to distinguish them;
the status alone does not say what arrived. Even a checked change is not
proof that the request was understood correctly.

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

## Choose the setup path before copying values

Use these choices with the person who owns the installation. Record the chosen
letters and actual names in your private setup notes; do not guess missing
permissions or open intake to discover whether the choice works.

| Decision | (a) | (b) | (c) |
| --- | --- | --- | --- |
| Tracker | Dedicated Backlog project/account, section 2 | GitHub Issues in one repository, section 2 | Shared Backlog account: verify the project identity as described below before opening intake |
| Namespace | New dedicated namespace, section 6 | Existing dedicated namespace, with its owner's approval | Shared namespace: arrange a suitable dedicated one; do not relax admission for unrelated workloads |
| Delivery | Leave merge to a person, section 4 | Automatic merge to an integration branch, after checking its rules and CI | Another completion point: define and test that project workflow first; neither PR nor merge proves production delivery |
| Build dependencies | Public dependencies with approved network access | Dependencies already in the checkout or pre-provisioned without credentials | Private dependencies: choose an approved preparation/proxy path below before accepting tickets |

Model endpoint choices are in [the gateway section](#with-a-gateway-in-front-of-the-models).
These are guide branches, not a new configuration language or an automatic
grant of the permissions each path requires.

## How a request moves

The shipped ordered configurations, `rewrite/examples/operator-stages.json`
for Backlog and `rewrite/examples/operator-github-stages.json` for GitHub Issues,
run every request through the same stages. A stage that runs a command is
finished only when the command exits 0; a stage that runs a model is finished
when its model process returns without an error. Three decisions are a
model's: after requirements, whether to ask the requester (initially or after a
failure); in the review, whether the change needs rework; and after
`confirm_change`, whether the requester must see the change before it is
delivered. Beyond those three, nothing a model writes moves the request:
writing "done" or "delivered" changes nothing.

| Stage | Kind | What happens |
| --- | --- | --- |
| `elicit` | model | Settles what the request asks for, from the request, the checkout and your instructions. Points only the requester can decide are listed with choices. |
| (`ask_requester`) | model | Only when such points are left: posts one comment with them, and the request waits for the requester's reply. After `confirm_change` it shows the requester the change instead. |
| `work` | model | Investigates, changes the checkout and runs the project's checks. |
| `verify` | command | Your build and test commands, against the changed checkout. |
| `review` | command | A second model reviews the diff and the test output; a blocking verdict sends the work back to `elicit`, then repair or a requester-only question. Without a verdict it neither lets the work through nor sends it back: it waits and asks again ([section 4](#when-the-review-gets-no-verdict)). |
| `confirm_change` | model | Reads the change in the checkout. When it alters how a person operates the product, a screen or the public API, or when that cannot be told, the decision asks the requester through `ask_requester`, and nothing is delivered before a comment from them; otherwise the report says `依頼者の確認: なし` and the work goes on to `deliver`. |
| `deliver` | command | Commits, brings the ticket branch up to date with the integration branch, pushes `ticket/<ISSUE-KEY>`, opens or reuses one pull request and merges it, or leaves the merge to a person ([section 4](#leaving-the-merge-to-a-person)). |
| `verify_merged` | command | Fetches the integration branch after the merge, or the pull request's branch when the merge is left to a person, and runs your build and tests on it. |
| `report` | model | Writes the report and posts it on the issue. |
| `confirm_report` | command | Passes when one comment on the issue is exactly the report. |

A command stage that fails sends the work back to the model stage named in its
`on_failure`, and every stage after that one runs again. There is no counter
and no failure ending: a command that can never pass keeps the request going
round, with a model launch each time, until someone fixes the cause or the
requester posts a stop ([section 9](#9-stopping-a-request)).

In this example, verification, review and delivery failures return to `elicit`.
It reads the actual failure: an ordinary repair or an unknown cause goes to
`work` to investigate and check. After handoff, only a newly required expansion
of authority that the requester alone can approve goes through `ask_requester`
with concrete alternatives; the one other question after handoff is the one
about the change, after `confirm_change` (rewrite/README.md, "Confirming the
change before delivery"). The entrance rule to ask when requirements remain
uncertain does not apply to recovery. A reply never changes filesystem, delivery
or credential permissions.
If a chosen alternative needs wider access, the operator must update that setting;
otherwise the roles must use an agreed alternative inside the existing scope.
Set positive `intake.max_active_minutes` and `intake.max_hard_exits` to bound
attempts while operator changes are pending. `intake.max_hard_exits` (default 3)
counts every forced exit of a time-limited request, at any stage. Without a time
limit it counts only forced exits in a row at one stage, so a request cut off at
the same stage after every restart still pauses. Another stage running in
between does not break the row; the count starts again from one in any of three
cases: a process of that stage ends by itself (successfully or not), the
requester's words arrive, or a forced exit happens at another stage. Ordinary
tool or verification failures are not forced exits, so neither count bounds
those retries, and without a positive active-work limit, total active time has
no bound. The limit notice reports a pause, not completion; read the required
permission change in the question and run record, fix the configuration, then
have an authorized user post `再開` on its own first line. This sets the count to
zero and, for a time-limited request, grants another interval with the same
saved cap. The cap and the forced-exit limit are saved at acceptance for a
time-limited request; a request without a time limit saves its forced-exit
limit at its first launch. A configuration change after that does not alter
them. The status page does not show the count in a row at one stage; without a
time limit, the pause notice names the stage and the count.
Each failure adds a requirements launch and a routing decision, including ordinary
repairs. A model process that exits with an error still retries its own stage.
Existing installations need to update their four `on_failure` values and shared
instructions to adopt this behavior. `confirm_report` continues to return to `report`.

## 1. What you need before starting

### A Kubernetes cluster

- **arm64 nodes.** The published image is built for arm64 only.
- **Three Kubernetes features, turned on**: Pod user namespaces
  (`UserNamespacesSupport`: `hostUsers: false`, here together with a
  persistent volume), the `procMount` field (`ProcMountType`:
  `procMount: Unmasked`), and sidecar containers (`SidecarContainers`:
  `restartPolicy: Always` on an init container). The engine container also
  runs with `seccompProfile: Unconfined`. These are requirements found by
  running the role launcher (README.md), not a recommended profile. By the
  Kubernetes feature-gate reference
  ([kubernetes.io](https://kubernetes.io/docs/reference/command-line-tools-reference/feature-gates/)),
  all three are on by default from Kubernetes 1.33: user namespaces and the
  `procMount` field were off by default before 1.33 and are stable from 1.36,
  and sidecar containers have been on by default since 1.29 and stable since
  1.33. On an older release, or where a cluster turned one off, the Pod is
  refused or never runs. The nodes' operating system and container runtime
  must support user namespaces as well. One installation was confirmed to run
  this StatefulSet on Kubernetes 1.36, containerd 2.2 and Linux 6.18 on arm64
  nodes, with a 20Gi ReadWriteOnce volume from its cluster's block storage
  class. The server-side dry run in section 6 shows what admission does to the
  StatefulSet; whether the Pod itself is admitted, and whether a node can run
  it, shows only in the StatefulSet's and the Pod's events
  ([section 11](#the-pod-does-not-become-ready)).
- **A namespace that admits this Pod.** Pod Security admission at `baseline`
  or `restricted` refuses it: two init containers add `NET_ADMIN` and the
  engine runs without the runtime's default seccomp profile, and `baseline`
  forbids both. Use a namespace whose enforced level is `privileged`
  (section 6 shows the label), and keep other workloads out of it.
- **An image built from commit f71872f (2026-10-02) or a later one.** The two
  network init containers write the egress rules with the image's own
  iptables (`/usr/sbin/xtables-nft-multi`), which older images do not carry.
  The image `docs/DISTRIBUTION.json` names is such an image. On 2026-10-02 the
  installation named above ran both from the image built from commit 2ccff5e,
  with this directory's rules (its own DNS address in place of the
  placeholder): both exited 0 with no restart and no log line, the egress
  check of section 7 gave the same results both ways as with its network
  plugin's image before, and the confinement, launcher and configuration
  checks passed. For the IPv6 rules that shows only that they were written,
  and no other cluster or network plugin was tried.
- **A storage class** that provides a 20Gi ReadWriteOnce volume.
- **Room for the Pod.** The engine requests 1 CPU and 3Gi of memory (limit
  6Gi); the other containers are small. These figures are a starting point,
  not a measured working set.
- **Pulls from ghcr.io**, or a registry mirror of the published image.

### An issue tracker: Backlog or GitHub Issues

The engine's tracker client speaks Backlog's API v2. You need a space, a
project for the requests, and a separate account for the engine itself
([section 2](#2-preparing-the-tracker)). A dedicated project is simplest.
Alternatively, use GitHub Issues in one repository with a dedicated intake
label and a separate account's PAT ([GitHub setup](#github-issues)). This
repository may differ from the repository that receives the work. Selecting
GitHub Issues does not change the models, workflow or delivery permissions.

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

With an unfiltered Backlog project, anyone who can create an issue can make
the engine read the repository, change it and merge the change. With GitHub,
anyone who can add the intake label can hand an open issue over, whoever filed
it, if it meets the creation-time boundary. Its roles can reach public
addresses. So:

1. Deliver to an integration branch that nothing deploys to production by
   itself.
2. Limit tracker membership and the ability to set the intake category or
   label to the people who may ask for changes.
3. Limit the engine's tracker account or PAT to the intake project/repository,
   so a mistyped setting cannot point it at another one.
4. Give the delivery token no permission over the repository's workflows.

A shared Backlog account weakens the protection in item 3. If the owner chooses
one, compare the project key and numeric id from section 2 with both the local
`--check` intake line and the running Pod's `--check` intake line before opening
intake. Confirm that the chosen reply/stop operators belong to that project.
Do not reuse another instance's queue; the identity checks are not a substitute
for limiting the account's access where possible.

Item 4 protects GitHub Actions workflow changes only. Other CI systems may run
configuration taken from a pushed branch with their own credentials. Inspect
all CI triggers and which branches receive secrets, including ticket branches,
before granting push access. For example, CircleCI's configuration is not
protected by a GitHub token lacking Workflows permission. Have that CI's owner
isolate its credentials from ticket-branch builds and approve the intended
access. An instruction to a model not to edit a path is not a permission boundary.

With the shipped configuration, everything goes to OpenRouter and the models
it serves: the working models, chosen for each launch among the publishers
in `model_selection.authors` (deepseek, minimax, qwen and z-ai), read the
checkout and receive the request and the run's records; the review model
(`REVIEW_MODEL`, `moonshotai/kimi-k3`, from a publisher outside that list),
and each model you add in `REVIEW_MODELS`
([section 4](#when-the-review-gets-no-verdict)), receives the request, the
diff and the test output; the decision model that picks each launch's model
(`typesafe/jev-1.13`) receives the request; and the chat model that decides at
the entrance receives the request and the earlier reports.
`model_selection.authors` narrows the publishers, and `model_selection.fixed`
names one model for every launch (rewrite/README.md, "Naming one model instead
of selecting").

### On your workstation

`kubectl` for the cluster, `python3`, and `sed`. Go 1.25 or later to check
your configuration before it goes in (section 4) and to try each new image
(section 10); without Go, the configuration check runs inside the Pod.

With an installed Go 1.21 or later and `GOTOOLCHAIN=auto`, Go can select and
download the version required by `go.mod` or `go.work`. That needs the download
to be permitted; it is not proof that the installed toolchain already meets
the requirement. `GOTOOLCHAIN=local` prevents that download and refuses a newer
requirement. Check `go version` in the relevant module and use a provisioned
toolchain for an offline runtime instead of silently weakening its requirement.

## 2. Preparing the tracker

Choose one tracker. The Backlog instructions below are not additional steps
for GitHub; for GitHub, use [GitHub Issues](#github-issues) instead.

### Backlog: the engine's own account

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
| `delivered` | the workflow ends after the report is confirmed; its actual delivery may be a merge, an open PR, or an ending without a new delivery ([section 4](#leaving-the-merge-to-a-person)) |
| `stopped` | the requester posted a stop |

Backlog's built-in Open and Resolved statuses have the ids 1 and 3
(rewrite/README.md). For the turns that need their own name, add custom
statuses to the project if your space allows them, for example "Automation
running" and "Waiting for the requester".

If delivery stops at a PR, map `delivered` to a status such as "PR ready for
review", or omit that turn. Do not copy Resolved merely because its id is 3.
No status name substitutes for the final report when the request ended
without a delivery. GitHub stage labels have the same limitation.

`intake.category_on_accept` adds one category to every accepted issue, so the
board can filter them. Create the category in the project first.

### A category that hands an issue over (in a shared project)

In a project people also use for their own tickets, `intake.category_ids`
makes the engine take only the new issues that carry one of the listed
categories: the requester sets it when filing, or later, and the issue is
taken on the next poll after it carries the category. Create a category for
this purpose only. The engine compares the category's number and nothing
else, so a category already in use hands over every issue that carries it and
was created at or after the start time, all at once. It does not look at who
set the category either: anyone who may edit an issue's categories can hand
it over, while the requester stays the account that filed the issue. This is
a different setting from `intake.category_on_accept`, which marks what was
accepted and narrows nothing (rewrite/README.md, "Automatic intake
experiment").

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
| the category that marks what was accepted | `intake.category_on_accept` |
| the category that hands an issue over, in a shared project | `intake.category_ids` ([section 8](#open-the-intake)) |
| members who may stop or answer any request besides its requester | `intake.stop_user_ids` |
| one issue, for the first ticket only | `intake.issue_ids` ([section 8](#8-opening-the-intake-and-filing-the-first-ticket)) |

### GitHub Issues

1. Enable Issues on the intake repository. Use a dedicated user account for
   the engine and a PAT restricted to that repository, supplied through the
   existing secret-management path as `TRACKER_API_KEY`. `github.key_env` is
   that variable's name, not the PAT. Keep delivery's `DELIVERY_GITHUB_TOKEN`
   separate, even if both uses concern one repository.
2. For a fine-grained PAT, give the selected repository **Issues: read and
   write**. Listing needs [Issues read](https://docs.github.com/en/rest/issues/issues#list-repository-issues);
   posting and removing its comments need [Issues write](https://docs.github.com/en/rest/issues/comments#create-an-issue-comment).
   The controller also reads its own account with
   [GET /user](https://docs.github.com/en/rest/users/users#get-the-authenticated-user),
   which adds no fine-grained permission. Organization approval, account
   access and the chosen server's policies still apply. These are documented
   permissions, not evidence that your token was tested. Do not grant code or
   workflow write access merely to use issue intake.
3. Create a dedicated intake label, plus the stage labels you configure.
   Check the repository's current labels and issues before choosing
   `automation`: if it already means something else, choose another name.
   Reusing a label can immediately hand over every open issue carrying it
   within the date window. No model assesses who added it. The requester
   remains the person who filed the issue.
4. Create optional stage labels such as `automation-accepted`,
   `automation-working`, `automation-awaiting-requester`,
   `automation-delivered` and `automation-stopped`. A stage label must differ
   from the intake label. Omit a stage setting to leave that turn unchanged.
   The engine changes these labels, not issue open/closed state; it does not
   record actual hours. `intake.assign` still controls assignee changes.
5. For github.com, **omit `github.api_url`** (default `https://api.github.com`).
   For Enterprise Server set its HTTPS API base, for example
   `https://tracker.example/api/v3`; see the
   [Enterprise API guide](https://docs.github.com/en/enterprise-server@3.21/rest/using-the-rest-api/getting-started-with-the-rest-api).
   Do not change only the hostname of the Enterprise example: github.com
   does not use `/api/v3`. Authentication, trust and reachability of your
   server must be checked separately. The shipped egress policy refuses
   private addresses; check that boundary first if an internal Enterprise
   server fails the read check.

Only open issues with the intake label and created at or after
`intake.created_since` are discovered. PRs are excluded. `intake.issue_ids`
means issue numbers here, not database IDs. A label added later does not
override the creation-time boundary. A new tracker or repository needs a
new queue; normal restarts of that same instance reuse its queue.

Use the human requester's account to file requests. Their account ID, or one
in `intake.stop_user_ids`, authorizes answers and the first-nonempty-line
`停止` comment. Closing the issue or removing its label does not stop an
already accepted request. GitHub rate-limit waits suppress requests until
the recorded wait ends (at most an hour). Three consecutive unsuccessful
stop reads pause the role process; when reads succeed again, the same queued
request is launched again. This does not preserve a running model call or
guarantee exactly-once external writes.

## 3. Preparing the delivery repository

### The token

The token is `DELIVERY_GITHUB_TOKEN`. Limit it to the one repository, and give
it no permission over the repository's workflows. What uses it, from the
scripts in `rewrite/harnesses/`:

- the mirror (`mirror_loop.py`) clones and fetches the repository;
- the delivery (`deliver_git.py`) pushes `ticket/<ISSUE-KEY>`, lists, opens
  and reads pull requests, merges one (not when the merge is left to a
  person), and reads the repository and the integration branch in its check
  mode;
- the merged check (`verify_merged.py`) clones the integration branch, or,
  with the merge left to a person, the pull request's branch.

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
that moment makes GitHub refuse the merge, the work goes back to `elicit`, and
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

With the merge left to a person ([section 4](#leaving-the-merge-to-a-person)),
the delivery never merges, so none of these refuse it: they apply to the
person who merges, and you can keep them as they are. The token needs the
same access all the same, to push the ticket branch and open the pull request,
and a rule that also covers the `ticket/...` branches applies to that push.

### What counts as done in this repository

Settle this with the owner before writing the configuration in section 4. If
an AI assistant is doing this setup for them, it asks the owner; it does not
answer for them. The engine delivers a change once the configured commands
exit 0 and the review lets it through, so what those commands check is all it
can know about whether a change is done. A definition that only says the
build passes lets through a change to a part that the build does not cover,
such as code in a language whose tools the image does not carry.

**List the parts of the repository first**, from the checkout: each language
or toolchain, each module or package, documentation, configuration, and
anything built or published from it. Ask the four questions below for each
part, and keep a part that has no answer in the list as one without an
answer: that is where a change goes through unchecked.

1. What must be seen before a change to this part counts as correct?
2. Which command checks that, and does it run in the engine's environment:
   inside a role, with the checkout read-only, with only the tools the image
   carries (`deploy/pod/Dockerfile`) and only the network the roles already
   have ([Dependencies and read-only builds](#dependencies-and-read-only-builds))?
3. How is the change's behaviour observed? If the answer is only that the
   build or the tests pass, ask what a person would look at to see the change
   work. If a program can do that, it is a live check
   ([section 4](#a-live-check-of-the-running-change-optional)); if not, it
   belongs to the next question.
4. What can no machine check here, and who checks it? The requester sees a
   change before delivery only when it alters how a person operates the
   product, what a screen shows or does, or the public API (`confirm_change`
   in [How a request moves](#how-a-request-moves)). A person can check a
   change before merging it when the merge is left to them
   ([section 4](#leaving-the-merge-to-a-person)). Otherwise nobody checks it,
   and the report says so.

**Signs of a definition too weak to rely on**: it names only the build; a
part has no answer; a command was run on a workstation but not inside a role;
a part that a command is said to check can be broken without that command
failing. Section 7 ([Before it accepts work](#before-it-accepts-work), "What
counts as done, checked inside a role") runs every command inside a role and
breaks each such part once.

**Write it down in the repository.** The owner chooses the file and its form;
the engine requires no file name and reads no format. Put it in through the
repository's ordinary pull request, so that later changes to it are reviewed
the same way, and merge that pull request into the integration branch before
the intake opens (section 8): the roles read the definition from a checkout of
that branch, so a location whose file is not there yet gives them nothing to
read. The engine is given only its location, in the project guidance
([section 4](#finish-the-project-guidance-either-tracker)). Its commands
become the operator's `build` and `test` (section 6) and, if there is one, the
live check's process. Change a command and the definition together, and run
the check in section 7 again after either changes.

**Decide whether the engine may add to it.** A request can show that the
definition lacks an item: a part it does not cover, or a check that cannot
run here. If the owner allows additions, the working role adds such an item
in the same delivery pull request, adds only and never removes or weakens an
item, and the review reads the addition with the rest of the change. If not,
the report proposes the addition. Either way, the project guidance says which.

The engine itself does not read the definition. The shipped roles read it at
the location the guidance names: requirements list the items that apply to
the request and how each is checked, and the report marks each of them
`確かめた`, with the record that shows it, or `確かめていない`, with the
reason. A check left to a person that is none of the three kinds of change
above is not asked for before delivery: the report says `確かめていない` for
it, unless the merge is left to a person who checks it.

### The branch must pass before you start

The merged check runs your build and tests against the integration branch
after every merge (with the merge left to a person, against the pull
request's head, which carries the integration branch). A branch that fails
them already will fail them for every request. The check mode in section 7
runs them once against the branch as it is; resolve any failure before the
first ticket.

### Dependencies and read-only builds

Check this before configuring the instance, not at its first failed ticket.
The shipped verification roles do not receive the delivery token or your
workstation's private package credentials. Their private HOME starts empty;
a successful build using your account's module cache is not the same test.
The post-merge checker also removes the delivery token before running your
commands. A private Go module or npm package will not become accessible just
because the repository itself can be cloned.

Choose one preparation with the owner:

- **Dependencies in the checkout.** For Go, prepare and review `vendor/` in
  the delivery repository through its normal PR process. In each module use
  `GOTOOLCHAIN=local GOMAXPROCS=2 GOFLAGS='-mod=vendor -p=1' GOPROXY=off go build -o /dev/null ./...`
  and the same environment with `go test ./...`. Explicit `-mod=vendor` is
  needed when that module's Go version does not select vendor automatically.
  A multi-module repository needs this preparation and command in each module;
  do not assume one root command covers it.
- **Pre-provisioned packages or an approved proxy.** Follow the project's
  existing mechanism, with cache/output paths writable outside the checkout.
  A private proxy still needs an explicit access arrangement; it is not
  automatically covered by the model or delivery credential.
- **New credential access is necessary.** Stop this setup choice and ask the
  owner to approve the precise access and isolation. Do not pass the delivery
  token to a model, remove dependency checks, or rewrite dependency versions
  just to make this test pass.

For a single Go `main` package, plain `go build ./...` writes a binary into the
current directory. The template's `go build -o /dev/null ./...` compiles without
that output and works in a read-only checkout. Other tools may still write
lockfiles, generated source or installed packages: place outputs under HOME
or `/tmp`, or explicitly grant only the required output paths. Use the exact
commands in verification, review and post-merge checking, and run section 7's
check inside a role before opening intake. A local build alone is insufficient.

Public dependency downloads need their own approved network destinations
(for Go these may include proxy.golang.org and sum.golang.org, or your chosen
proxy). The model/tracker egress check does not test those downloads. Inspect
the actual commands and DNS resolution from the runtime. A gateway that
resolves to a private address there will be refused by the shipped public-only
egress rules even if it resolves publicly on your workstation. Have the network
owner resolve the intended access; do not disable the rules as a test shortcut.

### Requests running side by side

When a reply resumes an ordered request still at its entrance, its next role
uses the latest configured integration branch if the prepared checkout has no
edits, local commits or untracked files and no later stage has run. Ignored
files count as work too: build output or test caches left in the checkout keep
it as it is. A successful update records the previous and new HEAD in the
role's history. An unavailable source leaves the original checkout usable and
records the reason. The replacement is prepared away from the original, so a
failed checkout leaves the original files as they were. Swapping it in needs
the job directory's filesystem to support an atomic directory exchange (Linux
`renameat2` with `RENAME_EXCHANGE`, macOS `renamex_np` with `RENAME_SWAP`);
where it does not, the old checkout stays in use and the reason is recorded.
Once work has started, the wrapper never updates that request's checkout;
later upstream changes are handled at delivery as described below. See the
README's
[workspace preparation](../../rewrite/README.md#optional-git-workspace-preparation)
for the conditions and limits.

With `intake.max_running` above 1, two requests can start from the same
integration branch, and the second to deliver finds the branch moved. Under
the merge method, and with the merge left to a person, the delivery first
merges the integration branch into the ticket branch: a clean merge becomes a
merge commit, and a conflict is left in the checkout between Git's conflict
markers and the delivery is refused naming the paths, so the next `work`
launch resolves them in place. With `squash` or `rebase` there is no such
catch-up; keep `max_running` at 1 with those. With the merge left to a
person, a new request starts from the integration branch without the pull
requests that still wait for a person's merge, so two requests that change
the same lines can conflict when a person merges the second.

## 4. Writing the operator configuration

`rewrite/examples/operator-stages.json` (Backlog) and
`rewrite/examples/operator-github-stages.json` (GitHub Issues) are complete for
the runtime of this directory: their delivery and merged check are the image's
fixed processes under `/opt/ticket-automation/scripts`; their build, tests and report check are
the operator scripts of section 6, and every checkout comes from the Pod's
mirror once you name it in place of the example's placeholder URL. Every value
you must change is one distinct string in it.

Use one of the two copy procedures below. Both examples keep identical ordered
stages, roles, permissions, question handling and knowledge-writing instructions;
only the tracker settings differ. Neither unedited example can start intake. The
separate `rewrite/examples/operator-github.json` ships the model-routed
workflow, so do not substitute that whole file for the ordered configuration.

### Backlog: make your copy with one command

Run this from the repository's root, with your values in place of the
`<...>` parts (none of them may contain `#` or `&`). `CONFIG` is where your
copy lives: an absolute path outside any repository, used again below.
Do not run it until the project id is known. If it is not known yet, keep the
example unchanged and finish section 2 first; an unquoted `<project-id>` is
not a JSON number and makes an invalid file, not a draft the engine can use.

```sh
CONFIG='<absolute path outside the repository>/operator.json'
sed -e 's#https://tracker.example.invalid/api/v2#https://<space>.backlog.com/api/v2#' \
    -e 's#"project_id": 0,#"project_id": <project-id>,#' \
    -e 's#REPLACE_WITH_RFC3339_ACCEPTANCE_START#2100-01-01T00:00:00Z#' \
    -e 's#https://repository.example.invalid/example-owner/example-repository.git#/var/lib/ticket-automation/mirror/<owner>/<repository-name>.git#g' \
    -e 's#example-owner/example-repository#<owner>/<repository-name>#g' \
    -e 's#example-integration-branch#<integration-branch>#g' \
    rewrite/examples/operator-stages.json > "$CONFIG"
```

The order of the lines matters: the fourth replaces the whole placeholder URL
before the fifth replaces what is left of `example-owner/example-repository`.

| What the command sets | Where it comes from |
| --- | --- |
| `backlog.base_url` | your space's API address (`.backlog.jp` for a space there) |
| `intake.project_id` | section 2 |
| `intake.created_since` | `2100-01-01T00:00:00Z` on purpose: the engine accepts nothing until section 8 opens the intake. The engine starts with this value; issues are taken only when they were created at or after it. |
| `<owner>/<repository-name>` | the mirror's path in every `TASK_REPOSITORY` (`/var/lib/ticket-automation/mirror/<owner>/<repository-name>.git`, the same as `MIRROR_PATH` in the StatefulSet), in place of the example's placeholder URL under `example.invalid`; and the delivery repository (`DELIVERY_REPOSITORY`) |
| `<integration-branch>` | the branch every checkout starts from (`TASK_BRANCH`) and the delivery merges into (`DELIVERY_BASE_BRANCH`) |

### GitHub: make the ordered copy

Run from the repository root, replacing the arguments below. The first repo
receives requests; the second receives delivered code. This changes the
tracker, source path and delivery values but preserves the stages and roles.
The intake remains closed. `CONFIG` must be a new file outside any repository.
The repository's setup test executes this conversion and the configuration
check together; it does not test access to a live server.

```sh
CONFIG='<absolute path outside the repository>/operator.json'
python3 - "$CONFIG" '<intake-owner>/<intake-repository>' '<delivery-owner>/<delivery-repository>' '<integration-branch>' <<'PY'
import json, sys
from pathlib import Path
target, intake_repo, delivery_repo, branch = sys.argv[1:]
cfg = json.loads(Path('rewrite/examples/operator-github-stages.json').read_text())
replacements = [
    ('https://repository.example.invalid/example-owner/example-repository.git',
     '/var/lib/ticket-automation/mirror/' + delivery_repo + '.git'),
    ('example-owner/example-repository', delivery_repo),
    ('example-integration-branch', branch),
]
def replace(value):
    if isinstance(value, str):
        for old, new in replacements:
            value = value.replace(old, new)
        return value
    if isinstance(value, list):
        return [replace(item) for item in value]
    if isinstance(value, dict):
        return {key: replace(item) for key, item in value.items()}
    return value
cfg = replace(cfg)
cfg['github']['repository'] = intake_repo
cfg['github'].pop('api_url')  # github.com uses the default API base.
cfg['intake']['created_since'] = '2100-01-01T00:00:00Z'
with Path(target).open('x') as output:
    json.dump(cfg, output, ensure_ascii=False, indent=2)
    output.write('\n')
PY
```

Change `github.intake_label` and `github.labels` if section 2 selected other
names. For Enterprise, add `github.api_url` as described there. Do not retain
`backlog`, `intake.project_id`, `category_ids`, `category_on_accept` or
`statuses`, even as zero, null or empty; they are refused with GitHub.
`intake.issue_ids`, if used, contains GitHub issue numbers. The mirror's
`MIRROR_PATH` must use the delivery repo, not the intake repo.

### Finish the project guidance (either tracker)

Then open your copy and replace the first sentence of `instructions`
("Operator setup is incomplete: ...") with your project's guidance: where the
project's own knowledge is written (a path in the repository such as
`CONTRIBUTING.md`), what the delivery target is, and anything the work must or
must not touch. Keep the rest of the paragraph; it tells every role how
progress is decided.

Also state where reusable answers may be **written**, for example "Read
project guidance in CONTRIBUTING.md; record newly answered project questions
in docs/decisions.md." Those are examples, not required filenames. A reading
location alone does not authorize edits. Include the chosen path in both
the work role's writable scope and `DELIVERY_ALLOWED_PATHS`; do not broaden
either permission just to fit this example.

When an answer is needed but no write destination is supplied, the shipped
roles offer concrete repository-appropriate file choices once in the entrance
question, alongside the substantive question. They retain that answer across
retries. No question is needed just to create notes for work that needed no
answer. The work role integrates reusable answers, without secrets or
invented facts; review compares the note with the actual exchange. Knowledge
and code use the same reviewed delivery PR. This is work assigned to the
existing models, not a controller that appends arbitrary comment text.

Name where the definition of done of
[section 3](#what-counts-as-done-in-this-repository) is written, and whether
roles may add to it, for example "What counts as done is written in
docs/done.md; roles may add a missing item to it in the delivery pull request
and never remove or weaken one." Those are examples too. The shipped roles
read the definition there; as with knowledge, its location alone does not let
them write to it. With no location named, the report says
`完了の定義: なし (導入先が定めていない)`.

For long guidance, edit a UTF-8 text file rather than escaping quotes and
newlines inside JSON by hand. This command replaces the example's first
sentence and preserves its shared instructions, writing a **new** file:

```sh
python3 - "$CONFIG" '<new configuration path>' '<project guidance text file>' <<'PY'
# setup-guidance-edit
import json, sys
from pathlib import Path
source, target, guidance_file = map(Path, sys.argv[1:])
config = json.loads(source.read_text(encoding="utf-8"))
guidance = guidance_file.read_text(encoding="utf-8").strip()
first, separator, shared = config["instructions"].partition(". ")
if not guidance:
    sys.exit("project guidance is empty; no configuration written")
if not first.startswith("Operator setup is incomplete:") or not separator or not shared:
    sys.exit("instructions are already customized or the example changed; edit the existing guidance deliberately")
config["instructions"] = guidance + "\n\n" + shared
with target.open("x", encoding="utf-8") as output:
    json.dump(config, output, ensure_ascii=False, indent=2)
    output.write("\n")
PY
```

Read the result, set `CONFIG` to that new path, and run both checks below.
The source file is untouched, an existing target is never overwritten, and
the command deliberately refuses to replace already customized guidance.
Neither file should contain credential values.

### Check that no example value is left

First check JSON syntax. This prints the line and column of malformed JSON
without starting the engine (for example an unquoted numeric placeholder, a
missing comma or a literal newline inside a quoted string):

```sh
python3 -m json.tool "$CONFIG" > /dev/null
```

Exit 0 only means readable JSON; it does not check project ids, endpoints,
permissions, duplicate keys or the meaning of the workflow. Then use the
engine check below for its own configuration rules.

The engine reads the configuration strictly and, before it does anything,
refuses what it can tell is wrong, saying where: a key it does not know, in
the wrong letter case or written twice; a stage whose kind does not match its
role; a missing project or start time; the example's paragraph still in
`instructions`; a URL whose host is still under `example.invalid`; a
`REPLACE_WITH_` component of `github.repository`; a token prefix
(`ghp_`, `gho_`, `ghu_`, `ghs_`, `ghr_` or `github_pat_`) in
`github.key_env`, which must name an environment variable, not its value. `--check`
runs those same checks without starting anything, contacting anything or
creating any file. Run it from the repository's root, on a checkout of the
commit your image was built from (`engine_sha` in `docs/DISTRIBUTION.json`),
with Go 1.25 or later; without Go, the same check runs inside the Pod at the
start of section 7:

```sh
go -C rewrite run ./cmd/engine --config "$CONFIG" --check
```

It must end with `the configuration is accepted; nothing was started`, after
the line that says which issues the engine would take up. With the intake
still closed that line reads
`intake: project <project-id>, issues created at or after 2100-01-01T00:00:00Z; every such issue is accepted`.
For GitHub it names the repository and intake label instead, says that PRs
are excluded, and lists any configured issue numbers. Check all of these.
A refusal names the place instead, for example
`roles[0].processes[0].env.TASK_REPOSITORY still holds the example's placeholder host under example.invalid; a watch needs your own value there`.

Two kinds of values the engine cannot tell from yours: the delivery
repository and its branch, which are not URLs, and an unreplaced `<...>`.
Find them with:

```sh
python3 -m json.tool "$CONFIG" > /dev/null && echo "valid JSON"
grep -n -E 'REPLACE_WITH_|example[.]invalid|example-owner|example-repository|example-integration-branch|<[a-z-]+>' "$CONFIG" \
  && echo "the lines above still carry example values" || echo "no example value left"
```

It must print `valid JSON` and `no example value left`. Neither check finds a value that is
well formed but wrong: a model id that does not exist, an operator command
that is missing or fails, a repository you cannot push to. The request that
meets one fails at that stage and goes round without end, with a model
chosen, a paid call, on every launch (section 11).

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
- `REVIEW_MODEL`: the model that reviews, as the endpoint names it; the
  review's other settings are [below](#when-the-review-gets-no-verdict).
- `DELIVERY_ALLOWED_PATHS` (`.`, the whole tree: which files a change needs is
  not known before the work; the checkout's `.git` stays out of reach of every
  model role either way) and `DELIVERY_MERGE_METHOD` (`merge`; `none` leaves
  the merge to a person, [below](#leaving-the-merge-to-a-person)).
- `NATIVE_MAX_TOKENS`: unset, the bridge allows 32000 output tokens per model
  answer ([section 11](#work-ends-without-a-change-or-a-model-stage-keeps-failing-with-ended-without-a-report)).

Optional in the delivery process's `env`: `DELIVERY_FORBIDDEN_TEXT`
(newline-separated text the delivery refuses to carry, such as internal
names) and `DELIVERY_AUTHOR_NAME` / `DELIVERY_AUTHOR_EMAIL` for its commits,
which are otherwise authored as "ticket engine". The pull request is titled
`Deliver <ISSUE-KEY>`, comes from the branch `ticket/<ISSUE-KEY>`, and is
merged as `Deliver <ISSUE-KEY> (#<number>)` when the delivery merges it.
While `DELIVERY_FORBIDDEN_TEXT` lists text that is not ASCII, such as a name
in Japanese, a change with a text file that is not UTF-8 (one kept in
Shift_JIS, say) is refused, since such text cannot be looked for in it, and
an attribute such as `binary` or `-diff` on the file does not change that. A
file with a NUL byte in its first 8000 bytes, which Git takes as binary, such
as an image, is delivered, and the text is found in it only where written in
UTF-8, even where the rest of the file is text in Shift_JIS.

Git settings (`GIT_CONFIG_COUNT` with `GIT_CONFIG_KEY_n` and
`GIT_CONFIG_VALUE_n`, or `GIT_CONFIG_PARAMETERS`) put in a process's `env`, or
for the mirror in the `env` of the StatefulSet's `mirror` container, reach the
Git that talks to the service in the delivery, in the merged check (whose
commands see them too) and in the mirror, so a proxy or a certificate can be
given that way. They do not reach the delivery's and the review's Git on the
workspace, which read it without them so that the two agree on whether
anything changed; a setting that Git would need there, such as
`safe.directory`, cannot be passed.

Do not remove `model_env` from a model stage's process, and do not put a
model id into its `env` instead: the engine refuses to start
([section 11](#the-engine-container-restarts-right-after-it-starts)).

### Settings the requester will notice

None of these is required. Each changes what appears on the issue
([section 8](#what-the-requester-sees-comment-by-comment) shows the result):

The following block is for Backlog. With GitHub, leave out `statuses` and
`category_on_accept`; use `github.labels` from section 2 instead. The other
settings are common, and user IDs mean numeric GitHub account IDs.

```json
"intake": {
  "announce": true,
  "declare_models": true,
  "status_page": "https://<status-host>/jobs/",
  "statuses": {"processing": <processing-status-id>, "awaiting_requester": <awaiting-status-id>, "delivered": <completion-status-id>, "stopped": <stopped-status-id>},
  "category_on_accept": <accepted-category-id>,
  "assign": true,
  "stop_user_ids": [<operator-user-id>]
}
```

(merged into the existing `intake` object, with your own ids from section 2
in place of each `<...>`; omit any status turn you do not want to change.
A `<...>` left in place is found by the check at the end of
[Check that no example value is left](#check-that-no-example-value-is-left).)

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
  works, and, on Backlog only, records hours from acceptance to the report.
- `category_ids` (Backlog only): the engine takes only issues carrying one of these
  categories, for a project people also use for their own tickets (section 2).
  The requester then sets that category on the issue. It is set when the
  intake opens (section 8), and the intake line names it:
  `...; only issues carrying one of the categories [<id>]`.
- `stall_notice_minutes` (default 90; 0 turns it off): rewrite/README.md,
  "What the requester is told at night".
- `question_reminder_minutes` (1440 in the example; absent or 0 says nothing):
  a question that has waited that long is said again, once per interval, in
  the engine's fixed words. Neither the reminder nor the time ends the wait or
  delivers anything: rewrite/README.md, "What the requester is told at night".
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
  {"name": "verify", "kind": "command", "on_failure": "elicit"},
  {"name": "review", "kind": "command", "on_failure": "elicit"},
  {"name": "confirm_change", "kind": "model", "confirm": true},
  {"name": "deliver", "kind": "command", "on_failure": "elicit", "announce": "納品先へのマージを始めました。マージ後の検証と報告を続けます。"},
  {"name": "verify_merged", "kind": "command", "on_failure": "elicit"},
  {"name": "report", "kind": "model"},
  {"name": "confirm_report", "kind": "command", "on_failure": "report"}
]
```

Write what is true when the stage begins: the `deliver` sentence is posted
before anything is merged ([section 11](#the-issue-says-merged-while-the-delivery-is-refused)).

### Leaving the merge to a person

By default the delivery merges the pull request it opens. With
`DELIVERY_MERGE_METHOD` set to `none`, it commits, brings the ticket branch up
to date with the integration branch, pushes it, opens the pull request (or
reuses the one it opened before) and stops there: a person merges. To choose
this, run the following on your copy after
[Make your copy with one command](#make-your-copy-with-one-command):

```sh
sed -e 's#"DELIVERY_MERGE_METHOD": "merge"#"DELIVERY_MERGE_METHOD": "none"#' "$CONFIG" > "$CONFIG.none" \
  && mv "$CONFIG.none" "$CONFIG"
grep -n '"DELIVERY_MERGE_METHOD"' "$CONFIG"
```

The `grep` must print one line, ending in `"DELIVERY_MERGE_METHOD": "none"`.
This applies only to the copy section 4 makes from
`rewrite/examples/operator-stages.json` or `operator-github-stages.json`:
the connected examples (`operator.json`, `operator-gateway.json` and
`operator-github.json`) have no such setting, and for a copy of one of them the
`grep` prints nothing.
The descriptions of the `deliver` and `verify_merged` roles in your copy state
both settings, so they need no edit. In this ordered configuration no model is
given them; they show only on the status page, as what those two stages were
handed.
Everything else is done as the rest of this guide says, with these
differences:

- **The token and the branch rules**: see
  [section 3](#3-preparing-the-delivery-repository); the delivery never merges,
  so rules on the integration branch apply to the person who merges.
- **The `verify_merged` stage** stays. With nothing merged, it runs your build
  and tests with the commit the delivery pushed checked out, on the pull
  request's branch, and says that this delivery merged nothing. That commit
  includes the catch-up merge, which the `verify` stage never saw.
- **The issue** gets the comments of section 8 up to the report. The report,
  written from the run's records, is where the requester learns the pull
  request's address and that merging it is left to a person: the delivery's
  output says `Pull request <number> against <integration-branch> is open for
  <ISSUE-KEY>: <address>.` and `Merging is left to a person; nothing was
  merged.` The request then ends like a merged one: the `delivered` status and
  the hand-back to the requester are applied where you configured them,
  although nothing has reached the integration branch. Give `delivered` a
  status whose name says that, or leave it out (section 2). A `deliver`
  sentence (above) must say what happens here, for example
  `"announce": "納品のプルリクエストを用意しています。マージは担当者が行います。"`.
- **The status page** shows the request as `done; the pull request is open,
  its merge left to a person` and counts it under `Done with the pull request
  open` (`PR を開いて完了`), not under `Delivered`.
- **The pull request** is `Deliver <ISSUE-KEY>` from `ticket/<ISSUE-KEY>`,
  and its description says that merging it is left to a person.
- **Once the request has ended**, the engine runs nothing more for it. A
  merge, a close or a push to the branch changes neither the issue nor the
  status page, which keeps showing the pull request open. While the request is
  still running (a later check sent the work back), the next delivery reads
  the pull request first: a person's merge is recorded as theirs and later
  work goes into a new pull request; a close ends the request with nothing
  delivered, shown as `Done, the pull request closed unmerged`; a push to the
  branch is left alone, nothing is pushed over it, and the delivery names
  what it did not put in. Those two endings, too, get the `delivered` status
  and the hand-back to the requester, although nothing of that round was
  delivered.

The cases this does not cover, and leaving out the `verify_merged` stage, are
in rewrite/README.md ("Stages instead of roles", from the paragraph that
begins "`DELIVERY_MERGE_METHOD=none`").

### When the review gets no verdict

The `review` stage runs `adversarial_review.py` with the settings in its
process's `env`. The example sets the first four:

| Setting | What it does |
| --- | --- |
| `REVIEW_MODEL_URL` | the chat-completions endpoint the review asks, over HTTPS |
| `REVIEW_MODEL` | the model asked, as that endpoint names it (`moonshotai/kimi-k3`) |
| `REVIEW_KEY_ENV` | the variable that holds the endpoint's key (`REVIEW_API_KEY`, which the process's `secrets` fill from `MODEL_API_KEY`) |
| `REVIEW_TEST_COMMANDS` | your test commands, one per line, run without a shell; the reviewer reads their output |
| `REVIEW_MODELS` | several models in place of `REVIEW_MODEL`, one per line or separated by commas, asked in that order; whichever answers first reviews, so each should come from a publisher outside `model_selection.authors` |
| `REVIEW_DIFF_PATHS` | the paths whose changes the reviewer is shown, separated by spaces; unset, the whole change |
| `REVIEW_RETRY_SECONDS`, `REVIEW_RETRY_CAP_SECONDS` | the first and the longest wait between two rounds of asking: 5 and 300 seconds unless set |
| `REVIEW_HOLD_SECONDS` | how often a waiting review says again that it waits: 900 seconds unless set |
| `REVIEW_TIMEOUT_SECONDS` | the limit of one request to a model and of each test command: 300 seconds unless set. A test command cut there reaches the reviewer as `(timed out after 300 seconds)` instead of its output; the `verify` stage has no such limit |
| `REVIEW_UNAVAILABLE` | `pass` lets the work through unreviewed when no verdict comes (below); unset, the review waits, and any other value is a mistyped setting |
| `REVIEW_ATTEMPTS` | with `REVIEW_UNAVAILABLE=pass` only, the requests made before the work is let through unreviewed: 3 unless set |

A blocking verdict sends the work back to `elicit`, and a verdict that does not
object lets it through to the delivery. Without a verdict the review does
neither; it waits:

- Trouble with the model service (no connection, a time-out, an HTTP error,
  a reply without a plain verdict) is waited out: the models are asked again
  in their order, round after round, with the wait between rounds growing
  from `REVIEW_RETRY_SECONDS` to `REVIEW_RETRY_CAP_SECONDS` for transient
  failures, including HTTP 402: restored credit lets the same review continue
  without restarting. Repeated unrecognized 400 responses, 401, 403, 404 and
  422 stop further attempts to that model. Other configured models are still
  tried; if none remains, fix the credential, model id or request setting and
  restart. Waiting alone does not
  make the same refused request run again.
- HTTP 413 and context-length 400 reduce the supplied material and retry.
  An unrecognized 400 also gets one reduced retry per model; another such 400
  then stops that model's attempts rather than repeating indefinitely.
  The reason and omitted ranges reach the model, live diagnostics and final
  review result. The combined runtime/history/diff/test body budget is
  halved below `REVIEW_MEMORY_CHARACTERS`; headings and omission notices are
  additional. The proposed pull request explanation remains complete. If
  minimal material is still refused by every model, use a model or gateway
  capable of accepting the required instructions and explanation and restart.
- A setting that cannot work (one that is missing or mistyped, an endpoint
  that is not HTTPS, a key that is not set, test commands that cannot be
  read) holds the review until you fix it.

While the review waits, the request stays in the `review` stage and keeps its
run slot: with `intake.max_running` at 1, every later request waits its turn
behind it. On the status page, the `review` record's live output says why:
`Review: no verdict yet (<model>: <reason>). Asking again in ...` while it
asks, repeated every `REVIEW_HOLD_SECONDS` as
`Review still without a verdict at <time>, <n> requests so far: <reason>.`;
or `Review by <model>: held, with no verdict: <reason>. ...` while a setting
holds it, followed every `REVIEW_HOLD_SECONDS` by
`Review still held at <time>; the reason is above.` The requester hears only
the engine's notice that no stage has completed, after
`intake.stall_notice_minutes` (90) and then at most every six hours
(section 8). The wait ends in one of three ways:

1. The model service answers again. The review goes on, and its result names
   the model that gave the verdict.
2. The requester stops the request (`停止`, [section 9](#9-stopping-a-request)).
3. You fix the cause (the configuration's ConfigMap or the Secret, section 6)
   and restart the Pod. The runtime records the stopped review as an interruption
   and goes on at the review's `on_failure` stage, which is `elicit` here: the
   requirements are reconsidered, then the work and review run again. The requester is told that the
   request carries on after the restart.

Each request to a model carries the change and the test output. Once the
waits reach `REVIEW_RETRY_CAP_SECONDS`, each still-available model is asked at most once every
300 seconds: up to about 100 rounds in eight hours, fewer when requests run
into `REVIEW_TIMEOUT_SECONDS`, since each of those waits out that limit first.
Whether to name more models in
`REVIEW_MODELS`, so that one model being down does not hold every request, is
your decision: each one is another company the change and the test output go
to (section 1).

`REVIEW_UNAVAILABLE=pass` makes the review let the work through unreviewed
instead: after `REVIEW_ATTEMPTS` requests (3) without a verdict, or at once
where it would hold, its result says `NOT REVIEWED` with the reason, and the
work goes on to the delivery. This guide leaves it unset, because the review is
what justifies delivering without a person. Reduced retries after a context
refusal also count against `REVIEW_ATTEMPTS`; a smaller input does not grant
extra requests. Its details, and the one case it
still sends back (a checkout with no change), are in rewrite/README.md
("Stages instead of roles", from the paragraph that begins "No verdict, no
pass").

### A live check of the running change (optional)

The report's Live verification section (`ライブ確認`) can name an observation
only when your installation supplies a way to start the changed project and
use it. Without one the report says `なし (導入先に検証の手段が無い)`, and unit
tests are not offered in its place. This section adds such a check as one
more process of the `verify` stage. It needs no new setting and no change to
the engine: the process's exit status counts like the build's and the
tests', and what it prints joins the history, where the report stage reads it.

**Prepare three things first.** Note which of them you have. A check that
cannot have all three is left out, not made to pass.

| What | Ready when |
| --- | --- |
| Start (起動方法) | The project's service starts from the checkout inside the `verify` process and listens on `127.0.0.1`, without production data, accounts or hosts. Whatever it has to build first, it builds itself: the `verify` stage starts its processes at the same time, so the check cannot wait for `project-build`. |
| Operate (操作する手段) | A program in the image (an HTTP client, a script) can reach the service and use one feature, inside the network the `verify` process already has. |
| Test user and data (テスト用のユーザーとデータ) | A user and data the owner agreed the check may create and remove. Never a real user's data. A password or key for that user is a credential: approve it on its own and give it to this process's `secrets` only, never the delivery token or a production account. |

Record which one stopped you, in your private setup notes and in the project
guidance at the start of `instructions`, in this form:

```text
Live verification method: none is supplied.
- Start: ready | missing: <what is missing>
- Operate: ready | missing: <what is missing>
- Test user and data: ready | missing: <what is missing>
```

When all three are ready, write instead what the check does, for example:
`Live verification method: the verify stage's live-check process starts the
service from the checkout on 127.0.0.1, uses the greeting as the test user
live-check-user following docs/FEATURES.md, and prints the request, the
response and the result. It checks the change before merge, not production.`
The report stage reads this sentence and the check's output. It is how the
report tells a check that was supplied and failed from one that was never
supplied.

**The command.** One program with a `run` subcommand that does the other five
in order. The names are yours; the engine looks for none of them, and each
can be run by hand.

| Subcommand | Does | Exits |
| --- | --- | --- |
| start | Starts the service from the checkout and records, in this launch's own directory, what it started: the process and its address. | 0 once the service answers; otherwise non-zero with the reason. It stops nothing it did not start. |
| diagnose | Checks the three things above for this launch. | 0 when the feature can be used; otherwise non-zero, naming which of the three is missing. |
| act | Uses the feature with the agreed test user and data, recording what it will create before creating it, and saves the actual request and response. | 0 only when what it observed is what the project's Feature Map says shows the feature working. Not running is not a pass. |
| evidence | Prints what was requested and observed: the command, the target, the expectation, the observation, the result and where the files are. | Prints what there is also after a failure. No credentials and no personal data beyond what the observation needs. |
| stop | Removes only the processes and test data this launch recorded, and keeps every evidence file. | 0 when nothing of them is left, also when nothing was started or all was removed before; otherwise non-zero, naming what is left. |

`run` exits 0 only when all five did. A cleanup that failed is a failed check,
also after a passing observation. Three facts of the engine decide where the
command writes and prints:

- Write under `$HOME/logs/` (the process's own `TASK_HOME`). Once a request has
  finished, the engine removes everything in a role's home directory except
  `logs` and the files directly in it, and whatever is written in the checkout
  is what the delivery commits.
- Print the observation as the **last** lines of standard output, and keep
  standard error short. The history keeps the end of a long output, and the
  merged check keeps only the last 4000 characters of each command's output.
- Send the service's own output to a file. The engine waits for the check's
  output to close: a service still holding it after the check has ended adds
  five seconds, after which the engine records the check as failed with
  `exec: WaitDelay expired before I/O complete`, even when it exited 0.

**When the check is stopped.** When the engine stops during the check (a
restart, an upgrade), it sends SIGTERM to the check's process group and
SIGKILL three seconds later; finish the cleanup within that time. In the
shipped launcher the check runs inside bubblewrap, which starts it in a
session of its own and kills it when bubblewrap ends, so the SIGTERM probably
reaches only bubblewrap and the check ends without cleaning up. This has not
been tried on a cluster. So let each `run` first look for the directories of
earlier launches that have no record of a finished `stop`, and run `stop` on
them. Processes inside the sandbox end with it, since Linux ends every
process of the sandbox's PID namespace when its first process ends; test data
in a database or on another host does not, and only the next `run` of the same
request removes it. Each request has a home of its own, so what a request
recorded stays when it ends or is stopped before its `verify` stage runs
again.

Know the service you started by the time it began, recorded when you start
it, not by its arguments. On Linux a process that is ending, as the service
is right after the group's SIGTERM, has no arguments left to read, so a check
that looks for its own arguments takes its own service for another process
and fails although nothing is left. When another process holds the recorded
PID now, yours has ended: leave that process alone.

**Add it to your copy.** Append this process to the `verify` role's
`processes`, with your own values in place of the example's:

<!-- setup-live-check-process -->
```json
{
  "name": "live-check",
  "command": [
    "/usr/bin/python3", "-B", "/opt/ticket-automation/bundle/harnesses/git_workspace.py", "--",
    "/usr/bin/python3", "-B", "/opt/ticket-automation/bundle/harnesses/linux_role.py",
    "--runtime", "/opt/ticket-automation/bundle", "--runtime", "/opt/ticket-automation/operator",
    "--network", "inherit", "--",
    "/opt/ticket-automation/operator/live-check", "run", "--map", "docs/FEATURES.md"
  ],
  "instructions": "Run the operator's live check of the running change. It starts the project's service from this checkout, checks that the service can be used, uses one feature as the agreed test user, prints what it requested and observed, and removes what it started. Its exit status is the observation; it returns no report and no approval.",
  "env": {
    "TASK_REPOSITORY": "https://repository.example.invalid/example-owner/example-repository.git",
    "TASK_BRANCH": "example-integration-branch",
    "LIVE_CHECK_TEST_USER": "<agreed-test-user>"
  },
  "timeout_minutes": 10
}
```

`TASK_REPOSITORY` and `TASK_BRANCH` take the same values as in the other
`verify` processes, and the checks in "Check that no example value is left"
find them and `<agreed-test-user>` while unreplaced. `LIVE_CHECK_TEST_USER` and
`--map` are this example's own; the engine passes `env` and arguments on and
reads neither. Keep `--network` as the build and tests have it, and add no
`--write` or `--create`: the checkout stays read-only. `timeout_minutes` ends a
check whose service stops answering, and the engine records that as a failure.
Put the program beside the other operator scripts (section 6).

**What adding it approves.** Adding the process approves a command, not a new
destination. It runs with the `--network` and the Pod's egress rules that the
build and tests already have. If using the service needs a destination those
rules do not allow (a staging host, an outside API), that is a separate
decision by the network's owner. Until it is made, record `Operate: missing:
<destination> is not allowed` and leave the check out. Do not change the
egress rules as part of adding the check.

**Optional or required.** Decide when you configure it. Configured, it is
required: a check that does not exit 0 sends the work back to `elicit` like
any failing command, and the request does not reach its report until a later
check exits 0. No setting lets a failing check through. If it keeps failing
for a missing preparation or permission, do not let the work role add
production access or a credential to make it pass: fix the preparation, or
remove the process and record which of the three is missing. The failures
recorded before that stay in the history.

**After the merge.** This checks the change before it is merged. To check the
integration branch after the merge as well, add the same command as one more
line of `VERIFY_COMMANDS` in `verify_merged` (each line runs without a shell).
That is a separate run against a different state, and its output is the
merged check's, not the `verify` stage's. The shipped instructions ask the
report to name the checked revision and environment under Observable results
only; to have the report say which state a live check saw, say so in the
project guidance.

**A working example.** `rewrite/examples/live-check/` checks a fictional
project on `127.0.0.1`: `verify_feature.py` has `run` and the five
subcommands, `server.py` is the project's service it starts, and
`FEATURES.md` is the project's Feature Map, one table whose columns are the
feature, how to reach it, how to use it, what shows it working and the
pitfalls. The check finds its row by the first column and reads nothing else
of the table, so a map needs no fixed form. The example writes in Japanese,
since what it prints is quoted in the report. Its tests
(`rewrite/harnesses/test_live_check_example.py`) run it through a pass, a
feature that does not work, SIGTERM while it waits for an answer, a killed
launch whose leftovers the next launch removes, a cleanup that fails, a
process it did not start, a missing preparation, and a run through the
merged check's command runner. A made-up `/proc` gives it what Linux shows of
a service that is ending and of a PID another process holds now.

### With a gateway in front of the models

`rewrite/examples/operator-gateway.json` shows the gateway settings for the
routed form, and rewrite/README.md ("Invoking through a gateway") explains
them. This ordered configuration needs them and the review's as well:

- `model_selection.gateway` with the gateway's `models_url`, `key_env` and
  `prefix`;
- **every model** process's `env.OPENROUTER_BASE_URL` and
  `secrets.OPENROUTER_API_KEY`, `stop_report` included, and `router.llm`'s
  `url` and `key_env`;
- the review's `REVIEW_MODEL_URL` (the gateway's chat completions URL),
  `REVIEW_MODEL`, or each model in `REVIEW_MODELS` (with the gateway's
  prefix), and its `secrets` mapping;
- if the gateway does not serve OpenRouter's decisions API (the one measured
  for rewrite/README.md answered 405), remove `model_selection.judge` (and
  `router.decision`, if you added it), name the gateway's chat endpoint and a
  model it serves, prefix included (`openrouter/publisher/model`), in
  `model_selection.fallback`, and remove `intake.min_model_credit`. Every
  choice is then made by that chat model.

Choose the selector route before filling keys:

| Choice | Selection and entrance routing | Credentials still needed |
| --- | --- | --- |
| a. Direct provider | Keep the shipped decisions and chat endpoints | `MODEL_API_KEY` |
| b. Gateway work, direct decisions | Keep `model_selection.judge` and any `router.decision` at the provider; send working models, review and `router.llm` through the gateway | Both provider and gateway keys |
| c. Gateway only | Remove `model_selection.judge` and `router.decision`; set `model_selection.fallback` to the gateway chat URL, a full prefixed model id, and `GATEWAY_API_KEY`; put `router.llm` there too | Only `GATEWAY_API_KEY` for models |

The public catalogue remains a credential-free provider request in all three
choices. A model appearing in a catalogue does **not** prove the gateway
implements the decisions API: ask its operator or read its API specification.
A 404/405 response alone does not distinguish an unsupported API from the
wrong path or method. If support is unknown, choose c, not a guessed decisions
URL. A trial inference can incur charges; agree that trial separately.

In the shipped ordered example, the model processes are in `elicit`,
`ask_requester`, `work`, `report`, and `stop_report` (the processes with
`model_env`). The separate `review` command uses `REVIEW_MODEL_URL` and
`REVIEW_API_KEY`, not `OPENROUTER_BASE_URL`. Build, test, delivery,
verification and comment-confirmation commands need neither model variable.
Do not add model credentials to those commands. Adapt this list if you add
your own model processes. A base such as `https://gateway.example.invalid/v1`
is for Hermes; the router, selector fallback and reviewer need the full
`https://gateway.example.invalid/v1/chat/completions` endpoint. Use the
gateway's actual API prefix; `/v1` is an example, not a suffix to append twice.

For choice c also remove an unused `intake.model_credit_url` together with
`min_model_credit`, and remove the StatefulSet's `MODEL_API_KEY` reference.
Do not remove either key for choice b. The credential check in section 7
reads names from the final configuration instead of requiring an unused key.

### Check the gateway catalogue without starting work

First finish the settings and run the engine's `--check` (section 4). Then
the following read-only GET checks can be run in the prepared runtime. They
make no inference or tracker writes; the gateway may have its own policy for
catalogue access. They compare current tool/text models from configured
publishers with the gateway's **exact prefixed ids**, and compare configured
fallback and reviewer ids with that same current served list. They do not prove that a
model can actually answer, that the account has credit, or that enough
independent publishers are available for all your simultaneous roles.

```sh
kubectl -n "$NS" exec -i "$POD" -c engine -- python3 -B - /etc/ticket-automation/operator.json <<'PY'
# setup-gateway-catalogue
import json, os, sys, urllib.error, urllib.parse, urllib.request
with open(sys.argv[1]) as source:
    config = json.load(source)
selection = config["model_selection"]
gateway = selection["gateway"]
class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None
client = urllib.request.build_opener(NoRedirect)
def get(address, key=""):
    url = urllib.parse.urlsplit(address)
    if url.scheme != "https" or not url.netloc or url.username is not None:
        sys.exit("catalogue URL must be HTTPS without embedded credentials")
    headers = {"Accept": "application/json", "Cache-Control": "no-cache"}
    if key:
        headers["Authorization"] = "Bearer " + key
    try:
        with client.open(urllib.request.Request(address, headers=headers), timeout=30) as response:
            body = response.read((16 << 20) + 1)
        if len(body) > 16 << 20:
            sys.exit("catalogue is too large; no partial result accepted")
        rows = json.loads(body)["data"]
        if not isinstance(rows, list) or not rows or any(not isinstance(row.get("id"), str) or not row["id"] for row in rows):
            raise ValueError("invalid catalogue")
        return rows
    except urllib.error.HTTPError as error:
        sys.exit("catalogue GET returned HTTP %d; no response body printed" % error.code)
    except (urllib.error.URLError, ValueError, KeyError, TypeError, AttributeError):
        sys.exit("catalogue check failed; inspect the configured endpoint and connectivity")
key = os.environ.get(gateway["key_env"], "")
if not key or "\r" in key or "\n" in key:
    sys.exit("gateway credential is unavailable")
served = {row["id"] for row in get(gateway["models_url"], key)}
public = get("https://openrouter.ai/api/v1/models?output_modalities=all")
prefix = gateway["prefix"]
matches = sorted(prefix + row["id"] for row in public
                 if row["id"].split("/", 1)[0] in selection["authors"]
                 and "tools" in row.get("supported_parameters", [])
                 and "text" in row.get("architecture", {}).get("output_modalities", [])
                 and prefix + row["id"] in served)
print(json.dumps({"served": len(served), "tool_text_matches": len(matches), "first_20_matches": matches[:20]}))
if not matches:
    sys.exit("no current matching models; check the prefix and eligible publishers")
configured = []
fallback = selection.get("fallback", {}).get("model")
if fallback:
    configured.append(fallback)
for role in config.get("roles", []):
    for process in role.get("processes", []):
        env = process.get("env", {})
        reviewers = [name.strip() for name in env.get("REVIEW_MODELS", "").replace(",", "\n").splitlines() if name.strip()]
        configured.extend(reviewers or ([env["REVIEW_MODEL"]] if env.get("REVIEW_MODEL") else []))
missing = sorted(set(configured) - served)
if missing:
    sys.exit("configured fallback or review models are not served: " + ", ".join(missing))
PY
```

Select the fallback chat model and the independent review model from the
gateway's current list, with their exact prefixed ids. The reviewer must be
outside `model_selection.authors`; it will therefore **not** be in the
matching working-model sample above. The check nevertheless requires the
configured fallback and reviewer ids to exist in the served list. Finally, run an explicitly authorized
small request before unattended intake; a successful catalogue check is not
an end-to-end model test.

When preparing sections 5 and 6, put `GATEWAY_API_KEY` in the Secret and
uncomment it in the StatefulSet. Run the checks above only after that runtime
is prepared, and use the matching checks in section 7. This ordered
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
| | `TRACKER_API_KEY` | the engine account's Backlog API key or GitHub intake PAT (section 2); not the delivery token |
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

### Keep object names and references together

Choose one `<consumer>` prefix and one `<namespace>` for the whole set. The
suffixes below are relationships, not independent names to guess:

| Object | References that must agree |
| --- | --- |
| StatefulSet `<consumer>-ticket-engine` | its Pod template `app` label, its `matchLabels`, and the status Service's selector; its first Pod is `<consumer>-ticket-engine-0` |
| ConfigMap `<consumer>-ticket-engine-operator` | the StatefulSet's `config` volume; keep the data key `operator.json` and its mount path unchanged |
| ConfigMap `<consumer>-ticket-engine-operator-scripts` | the StatefulSet's `operator-scripts` volume; keep the script data keys the commands call |
| ConfigMap `<consumer>-ticket-engine-egress` | the StatefulSet's `network-policy` volume |
| Secret `<consumer>-ticket-engine` | engine and mirror `secretKeyRef` names; each referenced key must exist |
| Secret `<consumer>-ticket-engine-status` | status container `secretKeyRef` names; this is not the delivery credential |
| Service `<consumer>-ticket-engine-status` | optional Ingress backend name, and selector pointing at the Pod's `app` label |

Set `metadata.namespace` consistently on all namespaced objects. A Secret
and a Service can share a name because they have different kinds. The
StatefulSet's `serviceName` is not the status Service: the template currently
names a governing Service it does not create (see the status Service's
comment); this guide does not rely on per-Pod DNS names. Do not point the
Ingress at that absent name. If any chosen object already belongs to another
installation, choose a different prefix; do not overwrite it to make these
references agree.

```sh
NS=<namespace>
POD=<consumer>-ticket-engine-0
```

1. **The namespace**, admitting this Pod (section 1):

   The commands below are for a **new dedicated namespace**. For an existing
   one, first read its labels and check with its owner which workloads use it;
   do not run the label overwrite on a shared namespace. Missing labels do
   not mean that admission permits this Pod: cluster defaults still apply.
   Keep an already suitable dedicated namespace as it is. Otherwise have its
   owner approve the change or allocate a dedicated namespace, then use the
   server-side dry run and Pod events to check actual admission.

   ```sh
   kubectl create namespace "$NS"
   kubectl label namespace "$NS" pod-security.kubernetes.io/enforce=privileged --overwrite
   ```

2. **The Secrets** (section 5), from your filled copy, which you then delete;
   or with the secret path you already use:

   ```sh
   kubectl -n "$NS" create -f secrets.yaml
   rm secrets.yaml
   ```

   `create`, not `apply`: a client-side `apply` keeps a copy of the whole
   object, values included, in the annotation
   `kubectl.kubernetes.io/last-applied-configuration`. To change a value
   later, fill a copy again, run `kubectl -n "$NS" replace -f secrets.yaml`,
   delete the copy, and restart the Pod: the containers read the values only
   when they start. `replace` makes both Secrets exactly what the copy holds,
   so write every value of both again; a value left empty becomes empty. With
   a gateway, uncomment `GATEWAY_API_KEY` in the new copy too: a key the copy
   lacks is removed from the Secret.

3. **The egress rules.** In your copy of `egress-configmap.yaml.example`,
   `<dns-cluster-ip>` is the address the Pods' `/etc/resolv.conf` names (the
   file's comment shows how to read it). A Service GET is not evidence of
   what the Pod received: node-local DNS can differ. Reading a Pod's file
   needs permission to execute a command in that specific Pod, not only
   read-only API access. Use a Pod its owner has authorized for this check,
   or ask the runtime operator to supply the observed nameserver address;
   do not inspect an unrelated workload or guess it. Then:

   ```sh
   kubectl -n "$NS" apply -f egress-configmap.yaml
   ```

4. **The operator configuration**, from your copy (`$CONFIG`, section 4), with
   the intake still closed (`created_since` in 2100). The key must stay
   `operator.json`, the name the engine reads:

   ```sh
   kubectl -n "$NS" create configmap <consumer>-ticket-engine-operator \
     --from-file=operator.json="$CONFIG" --dry-run=client -o yaml \
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

### Inspecting the queue without starting work

From a checkout of this repository, use the source-shipped helper below.
It needs Python 3 on the workstation and in the selected container, and
kubectl on the workstation. Keep `idle-check.sh`, `copy-queue.sh` and
`queue_helper.py` in `operations/` together;
they are not part of the host bundle. Set `CONTEXT`, `NS`, `POD`, `CONTAINER`
and the absolute `QUEUE` path to the intended installation first. No context
or workload is selected implicitly.

```sh
sh deploy/ticket-engine/operations/idle-check.sh \
  --context "$CONTEXT" --namespace "$NS" --pod "$POD" \
  --container "$CONTAINER" --queue "$QUEUE"
```

The one JSON line contains counts only: `jobs`, `running`, `waiting`,
`unfinished`, `stopped`, `stop_report_pending`, `unknown`, and `idle`.
Exit 0 means no pending work was observed in those local records; exit 1
means recorded work or uncertainty remains; exit 2 means the check could
not be performed. A nonempty live directory counts as running, even if it
might be stale. Waiting requests are not idle for this conservative check.
Unreadable, missing or conflicting histories and linked paths are unknown,
not completed work; a missing or unreadable accepted issue record is unknown
too. An unfinished stop report is not idle either. Without
a stop-report history, a recorded stop is unknown: this helper does not read
configuration to assume the reporter was disabled. Inspect that case
separately if no reporter is configured.
Use the status page's individual job history to investigate nonzero counts;
this helper does not dump that history.

This is an observation of local bookkeeping, not a process supervisor or
the engine's completion/stop-authorization decision. It does not contact the
tracker, prove delivery, stop a request, or authorize deployment. Records can
change immediately after the check. It sends no credential value as an
argument and does not print request text or remote command diagnostics.
Use `--timeout SECONDS` to bound the remote read (default 300).

### Reading Backlog issues without exposing credentials

The source-shipped `operations/read-issues.sh` and `file-ticket.sh` require
their adjacent `tracker_helper.py`, Python 3 on both sides and local kubectl.
They are not in the host bundle. These commands support **Backlog only**;
they reject GitHub or mixed configurations. They read the selected Pod's
configuration and refer to its existing credential environment by name.
Neither the credential value nor the issue text is put in a command argument
or printed to the workstation's terminal. Do not enable shell tracing.

Set the explicit cluster target variables above, `CONFIG` to the absolute
Pod configuration path, `PROJECT_ID` to its intake project, `TRACKER_BIN` to
the installed absolute tracker CLI path (normally
`/opt/ticket-automation/bundle/bin/ticket-tracker`), and `READ_OUTPUT` to a
new absolute workstation directory whose parent exists:

```sh
sh deploy/ticket-engine/operations/read-issues.sh \
  --context "$CONTEXT" --namespace "$NS" --pod "$POD" \
  --container "$CONTAINER" --source backlog --config "$CONFIG" \
  --project-id "$PROJECT_ID" --tracker-bin "$TRACKER_BIN" \
  --output "$READ_OUTPUT"
```

The installed CLI reads all issue pages; the helper preserves the complete
successful list and refuses duplicate or out-of-project identities. Add
`--issue-id NUMBER` for each issue whose complete comment list is needed;
each must belong to the returned project. No issue or comment is posted by
this command. A missing CLI, failed later page, invalid response or nonzero
remote exit is failure, not a successful partial list.

New directories are private (0700), files 0600. `intent.json` records the
selection and start time; `received.jsonl` retains received evidence;
`result.json` exists only after successful completion. Default issue data
contains IDs, keys, status/category/assignee IDs, hours and update times;
comments contain IDs only. Add `--native` only when native issue/comment
bodies are needed in these private files. Console output contains counts
only. Keep all output outside a public checkout and do not paste it into
logs or PRs. Even full pagination is not an atomic snapshot of a changing
tracker, and this aggregate list is **not** the URL-per-page offline GET
fixture required by section 10.

### Explicitly creating one Backlog issue

Use `file-ticket.sh` only after deciding to create a new request. It does not
run automatically after a read or a failed request. Supply the intended
`TYPE_ID` and `PRIORITY_ID`; the helper verifies they are present in the
project's issue-type list and priority list and never selects the first
entry. `PROJECT_ID` must equal the selected configuration's intake project.
The first version accepts summary/description only: projects with mandatory
custom fields are not supported, and no field value is guessed.

This helper creates the issue using the engine's configured tracker account.
It is not a test of normal requester intake: section 2 requires requester
issues to come from a different account. If such an issue meets the intake
settings, it can run with the engine recorded as its requester; the person
who asked is not its creator, so their answers and stop comments are not
authorized unless they are a configured operator. Engine comments can then
be mistaken for requester answers. To check intake through delivery, create
the request from the intended requester account using the tracker's normal
UI or that account's approved tooling. Do not open intake for a helper-created
issue as a substitute for this check.

Prepare ordinary single-link UTF-8 `SUMMARY_FILE` and `DESCRIPTION_FILE` on
the workstation. The summary must be nonempty; an empty description is
allowed. The file contents travel as JSON data over stdin, not shell code,
environment assignments or command arguments. Set `CREATE_OUTPUT` to a new
absolute private-output directory under an existing real parent:

```sh
sh deploy/ticket-engine/operations/file-ticket.sh \
  --context "$CONTEXT" --namespace "$NS" --pod "$POD" \
  --container "$CONTAINER" --source backlog --config "$CONFIG" \
  --project-id "$PROJECT_ID" --type-id "$TYPE_ID" --priority-id "$PRIORITY_ID" \
  --summary-file "$SUMMARY_FILE" --description-file "$DESCRIPTION_FILE" \
  --output "$CREATE_OUTPUT"
```

There is one POST at most, no automatic retry and no HTTP redirect following.
After a valid creation receipt, its minimal identity is streamed into the
private evidence before one readback GET. Success means that identity was
created and read back; it does not mean the engine accepted or delivered the
request. The command does not change intake configuration or deployment.

Both commands reserve their output before remote execution and retain it
on failure. Existing outputs, linked local inputs/ancestors, hardlinks and
special input files are refused; nothing is deleted or overwritten. Even
if no remote callback arrives, the same output cannot be reused. A nonzero
create result **does not prove that no issue was created**: timeout, broken
receipt, failed readback or local disk failure can follow a successful POST.
Check the selected project and retained evidence before deciding what to do;
using a fresh output is not proof that resubmission is safe. There is no
automatic transaction recovery or duplicate repair. The remote mounted
configuration may use platform symlinks; it is only read inside the Pod.
`--timeout SECONDS` bounds the local wait (default 300); it does not guarantee cancellation of remote processing.

### The configuration, as the engine reads it

First run the check of section 4 inside the Pod, on the configuration the
engine itself reads:

```sh
kubectl -n "$NS" exec "$POD" -c engine -- \
  /opt/ticket-automation/bundle/bin/ticket-engine --config /etc/ticket-automation/operator.json --check
```

It must print the intended intake scope and
`the configuration is accepted; nothing was started`. With Backlog the scope
names the project and any category filter. With GitHub it names the repository,
intake label, date and any issue numbers, and excludes PRs. In either case the
date must still be `2100-01-01T00:00:00Z`. If the engine container keeps
restarting instead, it refused the configuration when it started
([section 11](#the-engine-container-restarts-right-after-it-starts)).

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

The configuration's checkout source must be that mirror. This reads every
`TASK_REPOSITORY` from the configuration the engine reads and says whether the
Pod has it:

```sh
kubectl -n "$NS" exec "$POD" -c engine -- python3 -B -c '
import json, os
config = json.load(open("/etc/ticket-automation/operator.json"))
envs = [p.get("env") or {} for r in config["roles"] for p in r["processes"]]
sources = {e["TASK_REPOSITORY"] or "" for e in envs if "TASK_REPOSITORY" in e}
for source in sorted(sources):
    print(source or "(empty)", "found" if source and os.path.isdir(source) else "NOT FOUND")'
```

It must print one line,
`/var/lib/ticket-automation/mirror/<owner>/<repository-name>.git found`. Another
path, or `NOT FOUND`, means the configuration and the StatefulSet's
`MIRROR_PATH` disagree, and every request would fail at its first checkout;
`(empty) NOT FOUND` means a `TASK_REPOSITORY` written empty or `null`, which
fails the same way.

### The status page

```sh
LOCAL_PORT=9201
kubectl -n "$NS" port-forward "pod/$POD" "$LOCAL_PORT":9200
```

Choose an unused local port; if 9201 is occupied, choose another and use that
same number in the browser and health check. Do not stop an unrelated local
service. Only the left/local port changes; the Pod still listens on 9200.
Keep the port-forward terminal open and the default local-only binding.

With the example above, open <http://localhost:9201/>. It asks for the user and password
from the status Secret; without them it answers 401. Signed in, an empty queue
shows nine counters at 0 (`Queued`, `Running`, `Awaiting answer`,
`Needs attention`, `Delivered`, `Done without a change`,
`Done with the pull request open`, `Done, the pull request closed unmerged`,
`Stopped`), "No request has been accepted into this queue yet.",
and one column per configured stage. <http://localhost:9201/healthz> answers
`ok` without signing in. `/config` shows the configuration as the page read
it, and `/log` the engine's log. The labels switch to Japanese from the link
at the top of the page.

### The engine's log

```sh
kubectl -n "$NS" logs "$POD" -c engine
```

A healthy engine prints the intake line once when it starts, the same line as
the check above, and nothing more while it has nothing to do. These lines
mean something is wrong:

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

### Observing connections from the selected Pod

Use the source-shipped `operations/egress-check.sh` with its adjacent
`network_probe.py` and `tracker_helper.py`. Prepare a private JSON file of
the endpoints your installation should reach and refuse, for example:

```json
[
  {"name": "model", "url": "https://allowed.example:8443", "expect": "connected"},
  {"name": "restricted", "url": "https://denied.example", "expect": "refused"},
  {"name": "metadata", "url": "http://169.254.169.254:80", "expect": "refused"},
  {"name": "cluster-api", "url": "https://CLUSTER_API_IP:443", "expect": "refused"}
]
```

`metadata` is the cloud metadata service. `cluster-api` is the cluster's own
API: put the cluster IP of the `kubernetes` Service in the `default`
namespace in place of `CLUSTER_API_IP` (read it with
`kubectl -n default get service kubernetes -o jsonpath='{.spec.clusterIP}'`),
not the public API endpoint, which the shipped policy does not refuse.

Replace these example endpoints. Include the tracker, model/gateway and delivery
endpoints actually used, and destinations your network operator expects to be
refused. Both expectations must be present; URLs must contain no credentials,
query or fragment. URL paths are not tested. No target is inferred from prose
inside the configuration. Explicit ports and IPv6 URLs are supported.

Run this observation from the **engine container**, not just from a model
role's sandbox. Include the installation's cloud metadata endpoint and
Kubernetes API endpoint as explicit `refused` targets in the private targets
file. Use the actual endpoints for that installation; no credentials are
needed for this TCP observation. A role sandbox's refusal does not prove
that the engine itself cannot connect. Do not open intake until both engine
connections are refused, alongside the required allowed connections.

```sh
sh deploy/ticket-engine/operations/egress-check.sh \
  --context "$CONTEXT" --namespace "$NS" --pod "$POD" --container "$CONTAINER" \
  --targets "$TARGETS" --output "$BEFORE_NETWORK"
```

`TARGETS` and the new output directory are absolute local paths. The helper
resolves and tests each returned address inside that container. It saves
addresses and results privately in `result.json`; console output contains only
the overall result. Exit 0 requires every target to match its expectation.
`connected` requires at least one successful address; `refused` requires refusal
at every resolved address. DNS failures, timeouts and incomplete observations
are never treated as refusals.
A refusal proves neither which component refused it nor that a firewall rule
caused it. A connection proves neither TLS, authentication nor application health.
The observation is limited to the selected endpoints at that time.

### Read checks after an independently performed deployment

Before an authorized deployment, retain a successful network result as above
and a successful metadata-only `read-issues.sh` result from section 7. After
the deployment, use `operations/after-deploy.sh` (Backlog only):

```sh
sh deploy/ticket-engine/operations/after-deploy.sh \
  --context "$CONTEXT" --namespace "$NS" --pod "$POD" --container "$CONTAINER" \
  --targets "$TARGETS" --output "$AFTER_CHECKS" \
  --config "$CONFIG" --engine-bin "$ENGINE_BIN" --tracker-bin "$TRACKER_BIN" \
  --queue "$QUEUE" --project-id "$PROJECT_ID" \
  --egress-baseline "$BEFORE_NETWORK/result.json" \
  --issues-baseline "$BEFORE_ISSUES/result.json" \
  --status-url "$STATUS_HEALTH_URL" 200 --status-url "$STATUS_ROOT_URL" 401
```

Set the absolute installed binary/configuration/queue paths explicitly.
Status URLs are credential-free HTTPS URLs observed from the workstation;
TCP observations are from the Pod. Supply the HTTP codes expected for your
configuration (an expected 401 does not prove authenticated page health).
The helper does not follow redirects, supply credentials or read response bodies.

It runs the network check, the engine's existing `--check`, the status reads,
the existing issue-read helper and the existing conservative idle check. It
also compares network targets/outcomes and issue metadata with the two explicit
successful baselines. DNS address rotation alone is not a mismatch. Baseline
equality does not prove the same account, immutable source or atomic snapshot;
use baselines from the intended installation. Changed issue metadata may be
normal activity and is not automatically blamed on deployment.

Every required check must pass for exit 0. An earlier failure is retained even
if later checks pass. The new private output keeps `result.json`, `network.json`
and successful issue-read evidence; no existing output or baseline is overwritten.
Missing or failed baselines are not replaced with the current observation.
Use `--timeout SECONDS` (default 60) per child command/HTTP read; this bounds
local waits, not the lifetime of a remote process after disconnection.

Neither helper deploys, repairs, changes intake, posts a ticket nor authorizes a
deployment. An idle check can become stale immediately. Configuration acceptance
and baseline equality do not establish end-to-end delivery; still run the
separately authorized acceptance request described below.

**The credentials, by name only.** Read the names the final configuration
actually references, plus the mirror's delivery credential. For gateway-only
settings this does not require `MODEL_API_KEY` after all its references have
been removed. This checks presence, not validity or permissions:

```sh
kubectl -n "$NS" exec -i "$POD" -c engine -- python3 -B - /etc/ticket-automation/operator.json <<'PY'
# setup-credential-names
import json, os, re, sys
with open(sys.argv[1]) as source:
    config = json.load(source)
names = {"DELIVERY_GITHUB_TOKEN"}
def visit(value):
    if isinstance(value, dict):
        if value.get("key_env"):
            names.add(value["key_env"])
        names.update(value.get("secrets", {}).values())
        for child in value.values():
            visit(child)
    elif isinstance(value, list):
        for child in value:
            visit(child)
visit(config)
if any(not isinstance(name, str) or not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", name) for name in names):
    sys.exit("a credential reference is not an environment variable name; run --check")
result = {name: "set" if os.environ.get(name) else "unset" for name in sorted(names)}
print(json.dumps(result))
sys.exit(0 if all(state == "set" for state in result.values()) else 1)
PY
```

All `set`. Never `printenv`, `echo` a variable or print a Secret.

**Connections, with no work to do:**

The `ticket-tracker ... --project-id ... issues` command below is Backlog-only.
For GitHub, skip that command and use the read-only check following this block.
The model catalogue and mirror checks are common to both trackers.

```sh
kubectl -n "$NS" exec "$POD" -c engine -- /opt/ticket-automation/bundle/bin/ticket-engine --list-models \
  | python3 -c 'import json, sys; c = json.load(sys.stdin); print(c["fetched_at"], len(c["data"]), "models")'
kubectl -n "$NS" exec "$POD" -c engine -- /opt/ticket-automation/bundle/bin/ticket-tracker \
  --base-url https://<space>.backlog.com/api/v2 --key-env TRACKER_API_KEY --project-id <project-id> issues \
  | python3 -c 'import json, sys; rows = json.load(sys.stdin); print(len(rows), "issues"); [print(r["id"], r["issueKey"], r["created"]) for r in rows[-3:]]'
kubectl -n "$NS" exec "$POD" -c engine -- \
  git --git-dir=/var/lib/ticket-automation/mirror/<owner>/<repository-name>.git log -1 --format='%H %ci' <integration-branch>
```

For GitHub, this reads the configured account and the first page of matching
issues, without posting, changing a label or starting a role. It prints no
credential or response body on failure. The page count is not a total; it
checks read access, not write permissions or the configured date boundary.

```sh
kubectl -n "$NS" exec "$POD" -c engine -- python3 -B -c '
import json, os, sys, urllib.error, urllib.parse, urllib.request
g = json.load(open("/etc/ticket-automation/operator.json"))["github"]
base = g.get("api_url") or "https://api.github.com"
token = os.environ.get(g["key_env"], "")
if not token:
    sys.exit("tracker credential is unavailable")
class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None
client = urllib.request.build_opener(NoRedirect)
def get(path):
    req = urllib.request.Request(base.rstrip("/") + path, headers={
        "Authorization": "Bearer " + token, "Accept": "application/vnd.github+json",
        "X-GitHub-Api-Version": "2022-11-28"})
    try:
        with client.open(req, timeout=30) as response:
            return json.load(response)
    except urllib.error.HTTPError as error:
        sys.exit("GitHub read check returned HTTP %d" % error.code)
    except (urllib.error.URLError, ValueError):
        sys.exit("GitHub read check failed; inspect connectivity and configuration")
me = get("/user")
query = urllib.parse.urlencode({"state":"open", "labels":g["intake_label"], "per_page":100})
rows = get("/repos/" + g["repository"] + "/issues?" + query)
print("account", me["id"], me["login"])
print("matching issues on this page", sum("pull_request" not in row for row in rows))'
```

The account must be the engine's dedicated account. This procedure has been
checked with local fixtures, not your live token. Confirm permission to post
comments, change the configured labels and assign the intended accounts before
opening intake; the read check alone cannot establish those permissions.

**The delivery and the merged check, in check mode.** Both end with exit
status 3 on purpose: `--dry-run` commits, pushes, opens and merges nothing.
Each reads its standard input to the end, so each is given `/dev/null`
instead of the rest of this script.

```sh
kubectl -n "$NS" exec -i "$POD" -c engine -- /bin/sh -s <<'CHECK'
export GITHUB_TOKEN="$DELIVERY_GITHUB_TOKEN"
export DELIVERY_REPOSITORY=<owner>/<repository-name> DELIVERY_BASE_BRANCH=<integration-branch>
export DELIVERY_MERGE_METHOD="$(python3 -B -c 'import json; c = json.load(open("/etc/ticket-automation/operator.json")); print(*sorted({(p.get("env") or {}).get("DELIVERY_MERGE_METHOD") or "merge" for r in c["roles"] for p in r["processes"] if any(str(a).endswith("/deliver_git.py") for a in p["command"])}))')"
d=$(mktemp -d /tmp/delivery-check.XXXXXXXX)
export TASK_WORKSPACE="$d/workspace" TASK_HOME="$d/home"
git -c core.hooksPath=/dev/null clone --quiet --no-local --branch <integration-branch> \
  /var/lib/ticket-automation/mirror/<owner>/<repository-name>.git "$TASK_WORKSPACE"
cd "$TASK_WORKSPACE" || exit
TASK_ISSUE=CHECK-0 DELIVERY_ALLOWED_PATHS=. \
  python3 -B /opt/ticket-automation/scripts/deliver_git.py --dry-run </dev/null
echo "deliver check exit: $?"
VERIFY_COMMANDS='/opt/ticket-automation/operator/build
/opt/ticket-automation/operator/test' \
  python3 -B /opt/ticket-automation/scripts/verify_merged.py --dry-run </dev/null
echo "verify check exit: $?"
cd / && rm -rf "$d"
CHECK
```

The delivery check reports that reading the repository and the integration
branch answered status 200 and that listing the branch over Git exited 0, and
names the merge method your configuration gives the delivery:
`A delivery merges its pull request with method merge.`, or, with the merge
left to a person,
`A delivery ends at the open pull request and leaves the merge to a person (DELIVERY_MERGE_METHOD is none).`
The merged check lists each command with its exit status and ends with
`0 of 2 configured verification commands failed.` Anything else there means
the integration branch fails your own checks today
([section 3](#the-branch-must-pass-before-you-start)).

**What counts as done, checked inside a role.** The verify stage runs the
operator's commands confined and with the checkout read-only, which is not how
they ran in the check above. This runs one of them that way, on a copy of the
integration branch. `COMMAND` is the command as the verify stage runs it, with
its arguments. `BREAK` is empty, or one shell command that breaks one part of
the copy, run in the copy's root. The launcher passes its standard input on to
the command, so the command is given `/dev/null`:

<!-- setup-done-check -->
```sh
COMMAND=/opt/ticket-automation/operator/test BREAK=
kubectl -n "$NS" exec -i "$POD" -c engine -- env COMMAND="$COMMAND" BREAK="$BREAK" /bin/sh -s <<'CHECK'
d=$(mktemp -d /tmp/done-check.XXXXXXXX) || exit 2
mkdir "$d/home" || exit 2
git -c core.hooksPath=/dev/null clone --quiet --no-local --branch <integration-branch> \
  /var/lib/ticket-automation/mirror/<owner>/<repository-name>.git "$d/work" || { rm -rf "$d"; exit 2; }
if [ -n "$BREAK" ]; then
  (cd "$d/work" && sh -c "$BREAK") || { echo "the break did not apply; nothing was run"; rm -rf "$d"; exit 2; }
  if [ -z "$(git -C "$d/work" status --porcelain)" ]; then
    echo "the break changed nothing; nothing was run"; rm -rf "$d"; exit 2
  fi
fi
env -i PATH=/runtime-policy/bin:/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin \
  TASK_WORKSPACE="$d/work" TASK_HOME="$d/home" \
  python3 -B /opt/ticket-automation/bundle/harnesses/linux_role.py \
  --runtime /opt/ticket-automation/bundle --runtime /opt/ticket-automation/operator \
  --network inherit -- $COMMAND </dev/null
s=$?
echo "inside a role exit: $s"
rm -rf "$d"
exit "$s"
CHECK
```

Run it for each of these, and keep each printed line in your private setup
notes:

| Run | `COMMAND` | `BREAK` | Must print |
| --- | --- | --- | --- |
| Each command the verify stage runs | `/opt/ticket-automation/operator/build`, then `/opt/ticket-automation/operator/test`, then the live check's command line if you added one | empty | `inside a role exit: 0` |
| Each part that the definition of done says a command checks | the verify stage's command that checks it (`build`, `test` or the live check), not a command that only the definition names | one change that breaks that part, for example `printf 'not source\n' >> crates/parser/src/lib.rs` for a crate the definition says the tests compile | `inside a role exit:` with a status other than 0 |

A command that does not exit 0 here fails every request at `verify`, and the
request goes round without end
([section 11](#a-request-goes-round-without-end)): a test that only passes
with a writable checkout, or a tool the image lacks, does that. Make each
break one that the part's own tools reject: a line that happens to be valid in
that language breaks nothing. A command that the verify stage does not run
proves nothing here, even when the definition names it. A broken part whose
command still exits 0 is then not checked by that command: fix the command or
the image, or change the definition of done to say that no machine checks
that part, so that the report says `確かめていない` for it. A line that says
the break did not apply or changed nothing is no result; correct `BREAK` and
run it again. Do not open the intake (section 8) until every run prints what
the table says.

A live check that needs values from its process's `env` gets them on the
`env -i` line inside the script, as `NAME=value` with the value its
configuration has. A value that its process's `secrets` fills is a credential:
write it there as `NAME="$SOURCE"`, where `SOURCE` is the engine container's
variable that `secrets` names for `NAME`, so that the shell inside the
container fills it in. Never write the value itself, there or on the command
line. For a process with
`"secrets": {"LIVE_CHECK_PASSWORD": "LIVE_CHECK_USER_PASSWORD"}`, the start of
that line becomes:

<!-- setup-done-check-secret -->
```sh
env -i PATH=/runtime-policy/bin:/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin \
  LIVE_CHECK_TEST_USER='<agreed-test-user>' LIVE_CHECK_PASSWORD="$LIVE_CHECK_USER_PASSWORD" \
  TASK_WORKSPACE="$d/work" TASK_HOME="$d/home" \
```

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

In your copy (`$CONFIG`), in the same edit:

1. Narrow what is taken. With GitHub, use the dedicated intake label from
   section 2, confirm it does not already mark unintended open issues, and
   optionally set `intake.issue_ids` to the first test issue's **number**.
   Do not use `category_ids` or the Backlog command below. Its creation time
   and author are visible through GitHub's issue page/API; use the original
   creation time, not the time the label was added.
   With Backlog in a shared project, set `intake.category_ids` to the category
   of section 2. For a single first ticket you can instead file it first,
   look up its id and creation time, and set `intake.issue_ids` to
   `[<that id>]`. The Backlog `ticket-tracker ... issues` command in section 7 prints
   both for the three newest issues; this prints them for one issue key,
   however many issues came after it:

   ```sh
   kubectl -n "$NS" exec "$POD" -c engine -- /opt/ticket-automation/bundle/bin/ticket-tracker \
     --base-url https://<space>.backlog.com/api/v2 --key-env TRACKER_API_KEY --project-id <project-id> issues \
     | python3 -c 'import json, sys; [print(r["id"], r["issueKey"], r["created"]) for r in json.load(sys.stdin) if r["issueKey"] == sys.argv[1]]' <issue-key>
   ```

   It prints nothing for a key the project does not have, a mistyped one
   included. An `issue_ids` that lists nothing real accepts nothing, and an
   empty one accepts every new issue in the project; remove it once you are
   ready for all of them.
2. Set `intake.created_since`, in UTC (the moment itself counts): use the
   moment you open the intake, for example `2026-10-05T09:00:00Z`, or, for a
   ticket you filed in advance and listed in `issue_ids`, its own creation
   time or any moment before it. An issue created before `created_since` is
   never taken, whatever the issue allowlist, category or GitHub label says: the engine looks
   at the creation time first and skips such an issue without a line in its
   log.
3. Run `--check` on the copy (section 4) and read the intake line. It must say
   what you meant, for example
   `intake: project <project-id>, issues created at or after 2026-10-05T09:00:00Z; only issues carrying one of the categories [<id>]`.
   For GitHub, check the named repository, intake label, creation boundary and
   optional issue numbers instead; PRs must be excluded.
   Without Go, read the same line as the first line of the engine's log after
   the restart below.

Then apply the ConfigMap (section 6, step 4) and restart the Pod; the
engine's log opens with the same intake line:

```sh
kubectl -n "$NS" delete pod "$POD"
kubectl -n "$NS" wait --for=condition=Ready "pod/$POD" --timeout=10m
kubectl -n "$NS" logs "$POD" -c engine | head -n 1
```

If anything else already takes issues from this project or repository, make sure it cannot
take the same ticket.

### What the requester sees, comment by comment

File the ticket as a person, not as the engine's account, in ordinary words.
No format is required. With the settings from section 4 (`announce`,
`declare_models`, `statuses`, `category_on_accept`, `assign`, and the
sentences for `work` and `deliver`), the issue shows the following. Status,
category and assignee changes appear in Backlog as change entries without
text. Model ids are examples.

For GitHub, substitute the configured stage labels for statuses and omit
categories and actual hours. The same question/answer comments and assignee
changes apply, but delivery does not close the issue. If the model allowance
is already low, acceptance and answer acknowledgements say they are waiting
instead of saying work began; recovery is announced when execution starts.

| # | On the issue | When |
| --- | --- | --- |
| 1 | status `processing`; `受け付けました。すぐに自動処理を始めます。` with, when `status_page` is set, `進み具合はこちらで見られます: https://<status-host>/jobs/<issue id>`; the category; assignee: the engine's account | within one poll interval (30 seconds) of filing. With requests ahead of it: `受け付けました。前に 2 件あり、順番が来しだい自動処理を始めます。`, and later `自動処理を開始しました。` when its turn comes |
| 2 | `要件確定を始めます。（モデル: maker/model-a）` | within seconds of the first stage's model being chosen |
| 3 | if something is left for the requester: `依頼者への質問を始めます。（モデル: maker/model-b）`, then **the question**: one comment listing each open point with two to four choices; status `awaiting_requester`; assignee: the requester | the request now waits, without limit, for the reply |
| 4 | the requester's reply (below); then `返答を受け取りました。自動処理を再開しました。`, status `processing`, assignee: the engine's account | within one poll interval of the reply |
| 5 | `要件確定をやり直します。（モデル: maker/model-c）` | the run returns to its first stage with the answer in hand. If the stage runs on the same model as before, nothing is said |
| 6 | `自動実装を開始しました。（モデル: maker/model-d）` | the `work` stage's own sentence, carrying the model it chose; without a sentence, `作業を始めます。（モデル: ...）` |
| 7 | `作業をやり直します。（モデル: maker/model-e）` | only if a check or the review sent the work back and it now runs on a different model. The run goes back through 要件確定 first, said, when its model changed, as `検証が通りませんでした。要件確定をやり直します。（モデル: ...）` (or `レビューが通りませんでした。...`). A stage launched again because its own launch did not end cleanly is said every time (except right after a restart whose notice already named it), with how that launch ended and the attempt, for example `前の回は作業の途中で処理が強制終了しました（メモリ不足の可能性があります）。作業をやり直します（2 回目）。強制終了があと 2 回続いたら一時停止して相談します。（モデル: maker/model-e）`; while it keeps failing, each one rewrites the comment of the one before |
| 8 | `納品先へのマージを始めました。マージ後の検証と報告を続けます。` | the `deliver` stage begins; the merge follows |
| 9 | `報告を始めます。（モデル: maker/model-f）`, then **the report** | after the merged check passed |
| 10 | status `delivered`; assignee: the requester; the actual hours | once the report is confirmed on the issue |
| 11 | `使ったモデル (工程ごと、起動順):` and one line per stage, for example `- 要件確定: maker/model-a (27 秒) — 再実行: maker/model-c (31 秒)` | right after |

Nothing is said for `verify`, `review` and `verify_merged` unless you give
them a sentence. The status page shows every stage as it runs, with the
instruction each role was given and its output as it arrives.

The engine also posts three notices of its own when needed: after a restart
that interrupted the request
(`本体が再起動しました（1 回目）。作業の途中で強制終了したため、作業をやり直します。強制終了があと 2 回続いたら一時停止して相談します。`,
at most once in 30 minutes),
when no stage has completed for `stall_notice_minutes`, and when the model
budget falls below `min_model_credit` and recovers. rewrite/README.md ("What
the requester is told at night") has their exact words.

### Check that the review actually reviewed

On the status page, open the first request's `review` record. Its result
begins `Review by <model>: PASSED.` or `Review by <model>: SENT BACK to the
worker.`, naming the model that gave the verdict. A review whose live output
says `Review: no verdict yet (...)` or `Review by <model>: held, with no
verdict: ...` is waiting for one
([section 4](#when-the-review-gets-no-verdict)); fix what it names before you
rely on the review. A result that begins `Review by <model>: NOT REVIEWED.`
comes only with `REVIEW_UNAVAILABLE=pass`: the review got no verdict, and the
work went on without one.

### Answering a question

Reply on the issue with an ordinary comment: the number or the words of a
choice, or anything else that answers. Only the issue's creator and the users
in `intake.stop_user_ids` can answer, only the first comment with words after
the question counts, and a status change is not an answer. The reply goes into
the run as the requester's own words; nothing checks that it answers the
question. A reply whose first non-blank line is `停止` is a stop, not an
answer.

### After it is delivered

Look at the pull request on GitHub (`Deliver <ISSUE-KEY>`, merged, or open
for a person to merge), at the integration branch, and at the report. The
report is written by a model from the run's records; the status page has the
records themselves.

The report lists each item of the definition of done that applies to the
request ([section 3](#what-counts-as-done-in-this-repository)). An item
marked `確かめていない` is one that nobody checked, with the reason: it is
left for a person, and the `delivered` status does not cover it.

## 9. Stopping a request

The read-only Backlog helper in section 7 can retain issue/comment evidence
before investigating a stop. It does not stop or resume work. The separate
creation helper is an explicit new request, never an automatic retry of the
stopped one; follow the authorization and new-request procedure below.

The issue's creator, or a user in `intake.stop_user_ids`, posts a comment
whose first non-blank line is exactly `停止`. A reason can follow on later
lines. Within one poll interval:

- the running process is stopped, and nothing more of the request runs;
- the `stop_report` role posts an account of what had happened and what is
  uncertain;
- once that report is done, the status becomes `stopped` and the issue is
  handed to the requester (with those settings).

A stop written while a question waits for its answer is a stop, not the
answer: no `返答を受け取りました。…` is posted, and the status goes from
`awaiting_requester` straight to `stopped`. A stop does not wait for a model
budget pause either: recording it launches no model, and the stop report runs
at once, below the floor too (rewrite/README.md, "Requester stop in watch
mode" and "What the requester is told at night").

A stop does not undo anything already done outside the cluster: a pushed
branch, an open pull request or a merge stays. It is permanent: deleting the
comment does not resume the request, and there is no resume command. To try
again, file a new issue. Quoted text, mentions and comments by anyone else
are not stops.

The [queue inspection helper](#inspecting-the-queue-without-starting-work)
also distinguishes unfinished stop reporting from a stopped original run.
An `idle` result does not verify the stop comment's authority or its report's
content; retain the original stop and both histories for inspection.

The read checks in section 7 can collect observations after a separately
authorized repair or deployment. They neither resume stopped work nor prove
that its report or delivery is correct.

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
   staging copies a cut checkout preparation or refresh leaves as `.source-*`
   and `.refresh-*`), agent homes, live output, the log and the lock. Copy
   every `history.json` whatever its size: a request whose history is missing
   from the copy looks unstarted, and the engine starts it.

   ```sh
   sh deploy/ticket-engine/operations/copy-queue.sh \
     --context "$CONTEXT" --namespace "$NS" --pod "$POD" \
     --container "$CONTAINER" --queue "$QUEUE" \
     --output /absolute/path/to/new-queue-copy
   kubectl -n "$NS" get configmap <consumer>-ticket-engine-operator \
     -o jsonpath='{.data.operator\.json}' > operator-current.json
   ```

   Run from a source checkout as in [section 7](#inspecting-the-queue-without-starting-work).
   The output directory must not exist, and its parent must already be a real
   directory. Source and destination paths must be absolute and must not pass
   through symbolic links. The helper rejects links, special files, archive
   traversal and duplicate archive paths; it does not remove or overwrite an
   existing destination. It keeps every regular record regardless of size and
   preserves file modification times. Excluded working directories are not a
   backup of undelivered work.

   A remote failure produces no output directory. Local extraction starts only
   after the remote copy and archive validation succeed. A local write failure
   returns nonzero and leaves any new partial output for inspection; do not
   rehearse against it. Success is exit 0, not merely a directory existing.
   The records are private (directories 0700, files 0600) and can contain task
   text or configuration. Keep them out of public logs and commits.

   This is not an atomic snapshot or a backup: the controller may change
   different files during the read. Use your agreed quiescent-copy or storage
   snapshot procedure when consistency is required. The helper never pauses
   the controller or changes cluster resources. Use the chosen output path
   instead of `queue-copy` in the commands below.

2. Build the engine for your workstation from the new commit (Go 1.25 or
   later; the output directory must not exist yet):

   ```sh
   git -C <checkout of this repository> checkout <new engine_sha>
   (cd <checkout of this repository>/rewrite && sh package.sh /absolute/path/to/new-bundle)
   ```

3. Check your configuration with the new engine first: it reads the
   configuration strictly, so a key that a newer version no longer knows
   stops it at start.

   ```sh
   /absolute/path/to/new-bundle/bin/ticket-engine --config operator-current.json --check
   ```

   It must end with `the configuration is accepted; nothing was started`.
   The source-shipped offline helper observes the candidate without executing
   configured role commands, including queues with waiting or unfinished work:

   ```sh
   python3 -B deploy/ticket-engine/operations/rehearse.py \
     --source /absolute/path/to/clean-candidate-checkout \
     --commit FULL_COMMIT_ID --queue /absolute/path/to/queue-copy \
     --config /absolute/path/to/operator-current.json \
     --reads /absolute/path/to/offline-reads.json \
     --output /absolute/path/to/new-rehearsal-output --seconds 2
   ```

   The source must already be a clean checkout of that full commit, including
   no untracked or ignored files, and ship its own `rehearsal_test.go` support. The helper
   does not fetch, switch commits, create/remove worktrees or inject a test
   from a different version. Keep it beside `queue_helper.py`. It needs Python
   3, Git and an already installed Go toolchain meeting the candidate's module
   requirement. Network and toolchain downloads are disabled. For a candidate
   with external Go modules, also pass `--modules /absolute/path/to/module-cache`
   naming a pre-populated Go module cache. Its `cache/download` directory must
   contain the required module archives and metadata; extracted source alone
   is not enough. Go reads that directory through a local file proxy and
   copies only the needed modules into the new private output's module cache.
   The supplied cache is not changed, and there is no network fallback.
   Without it, a candidate needing external modules cannot be built offline;
   the helper reports a failed rehearsal, not a successful observation.
   The output must be new, outside the checkout, input queue and supplied
   module cache, with a real existing parent.
   Input paths must be absolute without symbolic links.

   Supply the GET responses you intend to assume in `offline-reads.json`.
   This helper does not collect them, contact the tracker or treat the old
   accepted `issue.json` as the tracker's current state. For example, this
   synthetic Backlog entry assumes no newly discoverable issues in one project:

   ```json
   {
     "reads": [
       {
         "url": "https://tracker.example/api/v2/issues?count=100&offset=0&order=asc&projectId%5B%5D=7&sort=created",
         "body": [],
         "min_reads": 2
       }
     ]
   }
   ```

   Use the actual configured endpoint and explicit query values for your
   offline assumptions, not the example project or endpoint. Each entry is
   an HTTPS GET URL, a native JSON response body and a positive `min_reads`.
   All entries are required to be observed at least that often. Query ordering
   is normalized; omit `apiKey` and every credential value. An optional `link`
   contains the pagination Link header; supply every requested page separately.
   Replies are HTTP 200 only. Account, issue, comment, model-budget or other
   reads needed by the configuration must also be supplied. Unknown GETs,
   incomplete coverage and unreadable replies fail, rather than receiving
   invented empty responses. This applies to the configured tracker using its
   native response shapes; it does not translate a Backlog snapshot to GitHub.

   Normally readable done, waiting and unfinished records are admitted, as are
   authorized saved stops. Missing/conflicting histories and unreadable saved
   stop reports fail. Empty queues are not rehearsal coverage. The helper
   copies the input again into a private temporary directory, retains file
   times and leaves the input unchanged. Links, hardlinks and special files
   are rejected. Every configured role command is independently removed from
   the in-memory configuration and checked before observation. Every non-GET
   attempt is blocked and fails the rehearsal, including comments, status and
   assignment changes. A required read count, the requested observation time,
   normal cancellation and unchanged terminal records are separate checks.
   Unfinished work can record failed or interrupted attempts because its role
   commands are deliberately absent; this does not test the roles' execution.
   A stopped request with a required but unfinished report is observed too.
   Every HTTP write attempt still fails the rehearsal, even if it would be an
   expected next action of unfinished work; inspect the details before deciding
   whether it is a regression. A copied live record is not a live process here.

   Exit 0 means no write was observed under the supplied offline assumptions;
   a failure is nonzero. The JSON summary contains counts and elapsed time,
   not request text, hosts or diagnostics. `full_tick_coverage` is always false:
   neither elapsed time nor read counts prove every job completed every tick.
   This is not deployment approval, a check of live tracker consistency, role
   results, or a sandbox for arbitrary candidate source, package initialization
   or toolchain code. Execute only a candidate/toolchain you trust, or use your
   separately enforced sandbox. No operator credentials are inherited by the
   Go check; configuration-named API credentials use synthetic values.

   The new output retains private configuration/read copies, a result on
   success, a log and toolchain scratch data. After observation, including a
   failed one, `observations.json` names every blocked request's method and URL
   without credentials, distinguishing writes from missing GET responses and
   listing unmet read counts. `collector.log` retains the controller's own
   diagnostics. An unconfirmed notification is retried every 25 ms tick here,
   so one restart notice can appear as dozens of POST attempts; the log
   distinguishes its initial "restart notice" from retries of an "earlier notice".
   Request bodies and headers are not recorded. These private
   files identify the affected issue; do not paste them into public logs.
   Keep all output out of public logs and
   commits. The observation's temporary queue is removed by the test; the
   original input and any new partial output are never deleted by the helper.
   A failed run must not be treated as a successful queue-copy or deployment
   check merely because its output directory exists. These helpers have been
   checked with synthetic fixtures, not a real installation.

### Rolling it out

1. If you can, wait until the status page shows nothing running.
2. Copy your `statefulset.yaml` as it is, then put the new `image` reference
   into all six places in it (`network-v4`, `network-v6`, `policy`, `mirror`,
   `engine`, `status`) and apply it. The Pod is replaced.
3. Check as in section 7: the configuration check inside the Pod, the Pod
   ready with no restarts, the status page, the engine's log (it opens with
   the intake line), and the issues of any request that was running.

If the new Pod never becomes Ready, applying your previous `statefulset.yaml`
again is not enough, because the StatefulSet keeps waiting for that Pod:
delete the Pod as well, and it is created again from the restored definition
([Forced rollback](https://kubernetes.io/docs/concepts/workloads/controllers/statefulset/#forced-rollback)
in the Kubernetes documentation).

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
- **An init container failing.** `network-v4` or `network-v6`: the image was
  built before commit f71872f and has no `/usr/sbin/xtables-nft-multi` (use
  the image `docs/DISTRIBUTION.json` names), or a rule does not parse (its
  log). `policy`: its log.
- **Containers that cannot be created**, with events about user namespaces or
  mounts: the node or its container runtime does not support `hostUsers:
  false` (section 1).
- **`CreateContainerConfigError`.** A Secret or a key that the StatefulSet
  names is missing (section 5).
- **The engine never starts while `mirror` runs.** The mirror's startup check
  waits until the copy holds a branch: a large repository is still copying, a
  repository without any commit never passes, and a refused token ends the
  container (below). The check gives up after 10 minutes (every 5 seconds, 120
  times: `periodSeconds` and `failureThreshold` of the `mirror` entry), and the
  container is then restarted in the middle of its first copy. For a
  repository whose first copy takes longer, raise `failureThreshold`.

After changing the StatefulSet for any of these, delete the Pod as well: the
StatefulSet does not replace a Pod that is not Ready
([section 10](#rolling-it-out)).

### The engine container restarts right after it starts

The engine checks its configuration before it does anything and exits with
one line naming what it refused and where; the container then restarts, and
goes on restarting. Read the line with
`kubectl -n "$NS" logs "$POD" -c engine --previous`; with `--log-file` it is
also in that file, which the status page shows at `/log`. `--check` on the
same file prints the same line without starting anything (section 4). Some
of them:

- `reading the configuration: unknown key "category_id" in intake`: a key the
  engine does not know, here a misspelling of `category_ids`, which would
  otherwise have narrowed nothing. A key in the wrong letter case is refused
  the same way (`unknown key "Project_ID" in intake`).
- `reading the configuration: the key "category_ids" is written twice in intake; the later one would win without a word`.
- `instructions still holds the example's paragraph ("Operator setup is incomplete"); a watch needs the project's own guidance there`,
  and `roles[0].processes[0].env.TASK_REPOSITORY still holds the example's placeholder host under example.invalid; a watch needs your own value there`:
  a value left from the example (section 4).
- `model stage "work" launches no model`: a model stage's process lost its
  `model_env`. Met, when one stage was meant to run on one fixed model and
  `model_env` was replaced by a model id in `env`. A model stage must keep a
  process with `model_env`; to run on one model, set `model_selection.fixed`
  instead.
- `watch requires intake.created_since as an explicit RFC3339 timestamp`: the
  time needs a time of day and a zone, `2026-10-05T00:00:00Z`.
- `intake.min_model_credit needs router.decision.key_env to name the model credential`:
  see [section 4](#settings-the-requester-will-notice).

Each of these lines was produced by the engine from the same commit, given
the mistake on purpose. A value that is well formed but wrong does not stop
it ([section 4](#check-that-no-example-value-is-left)).

### A request goes round without end

Every failing command stage sends the work back without a limit. Read the
stage that keeps failing on the status page; its output says why. The usual
causes: a value still from the example (section 4), a missing credential
(`A configured role credential is unavailable: <name>` in the record), your
`build` or `test` failing in the read-only checkout (section 7), a branch rule
or a conflict (below). Stop the request (`停止`) while you fix the cause.

### A request stays in the `review` stage

The review has no verdict and waits
([section 4](#when-the-review-gets-no-verdict)); the `review` record's live
output on the status page says why. A permanent HTTP refusal (an unrecognized
400 repeated after reduction, 401, 403, 404 or 422) is not polled: fix the setting
or the Secret and restart the Pod. A transient service failure is retried,
including HTTP 402; restoring credit does not require a restart.
HTTP 413 and context-length 400 automatically reduce runtime text, history,
diff and test output, stating the omission, but keep the complete proposed
pull request explanation. If even minimal material is refused by all
configured models, use a model or gateway that can accept it and restart. Meanwhile
later requests wait their turn behind this one, and the requester gets the
notice that no stage has completed.

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
`merge` or `none`, and the delivery process must keep both `--write .` and
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
requester.

The delivery has a setting, `DELIVERY_ALLOW_UNCHANGED=1` in the delivery
process's `env`, with which such a request ends as done without a change:
once the review has given a verdict that does not object to the checkout as
it is, the delivery commits, pushes and opens nothing, the status page counts
the request under `Done without a change`, and the `delivered` status and the
hand-back are applied all the same, so the report is what tells the requester
that nothing was delivered. It is off unless set, and the shipped
configuration leaves it off. In the build this guide is for, the review and
the delivery read the checkout the same way, so a Git setting given in only
one of their environments does not make them disagree on whether anything
changed. Whether to turn it on, what it depends on and where it stops are in
rewrite/README.md ("Stages instead of roles", from the paragraph that begins
"A request whose right outcome is that nothing changes").

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
