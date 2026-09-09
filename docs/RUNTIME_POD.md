# The single-pod runtime (Hermes kanban constitution)

This engine has two deployment constitutions. The original one runs the
stage pipeline as a GitHub Actions workflow with an AWS Lambda + DynamoDB
reception. This document specifies the second: everything — reception,
ledger, dispatch, stages — inside one host, with the
[Hermes](https://github.com/NousResearch/hermes-agent) kanban as the
dispatcher. Code comments across `internal/runtime`, `internal/runner`,
`internal/state` and the two commands reference this file.

## ローカルでの CLI 向け起動

`lassdas init` は、この本体を Apple Silicon の Docker Desktop で動かす。手順は [LOCAL_INIT.md](LOCAL_INIT.md)、契約と受入条件は [INIT.md](INIT.md) にある。

固定 digest の同じイメージ、`orchestration: cards`、既存の起動プログラムと起動時検査を使う。台帳・kanban・作業場所は専用の named volume に置き、非秘密の `config/` だけを `/etc/lassdas/config` に読み取り専用で mount する。ホストの HOME・納品先 repo・Docker socket は本体に渡さない。

`HERMES_KANBAN_BOARD` は runtime の板名と一致させる。ローカルの納品用鍵、板の鍵、route key も既存の守るファイル一覧に含める。板はホストの loopback に公開し、匿名アクセスの拒否と認証成功を起動時に確かめる。

`lassdas run stop` は対象のコンテナだけを停止し、台帳と作業記録を保持する。ローカル起動のために Pod の release、共有 workflow、レジストリや権限を変更する必要はない。

## Processes

| Process | Source | Role |
| --- | --- | --- |
| attendant | `cmd/attendant` | Resident. Every interval it runs one question tick (the whole reception protocol: tracker ingest, answer adoption, renotify, shortfall, expiry, half-posted recovery, board projection) and then aligns kanban cards with ledger states (`SyncCards`). It fully replaces the Lambda; no webhook endpoint exists — the attendant reads the tracker, the tracker never calls in. |
| runner | `cmd/runner` | Per-card. The Hermes profile's `worker.command` points at it. It claims the queued run from the ledger, drives the stage pipeline by shelling out to the unchanged `cmd/worker` / `cmd/controller` binaries, and closes the run in process through the same report/question services the Lambda wired. |

Both read one `runtime.json` (`internal/runtime.Config`), which is what
keeps them on the same ledger, routes and identities.

## The ledger

`internal/state.LocalStore`: one SQLite file, WAL, a single-writer
transaction per operation. The semantics are a line-for-line sibling of
the DynamoDB store — same rows, same attribute names, same conditional
transitions — verified by an equivalence test that drives both stores
through the same scenario and diffs the traces. Pure reads use snapshot
transactions without the write lock.

## The supervisor contract (fork worker.command)

The Hermes fork's profile-level `worker.command` support is what
dispatches the runner. The contract the code relies on:

- The dispatcher spawns the supervisor with `HERMES_KANBAN_TASK`,
  `HERMES_KANBAN_WORKSPACE`, `HERMES_KANBAN_RUN_ID` set; the workspace
  persists across re-dispatches of the same card (the runner clears it in
  `Prepare`).
- Exit code 0 → the supervisor completes the card; **the complete
  translation is a no-op when the card is not `running`** — that is what
  lets the runner block its own card (`awaiting-answer:<delivery>`) and
  then exit 0 with the block keeping its word.
- Non-zero exit → the supervisor blocks the card with the failure reason.
- The card is the liveness signal: a card stays `running` exactly while a
  worker process lives; the supervisor's translation happens within
  seconds of exit.

### Card ↔ run states

The delivery→card mapping is derived from the kanban itself on every
sync pass (`list --json --archived`, matching on the idempotency key) —
there is no attendant-side mapping file. A cached map that could be lost
or trail reality made "no card known" indistinguishable from "no card
exists", which is exactly the evidence the claim recovery needs. This
also makes concurrent attendants safe-if-pointless: every ledger
transition is CAS-guarded and every card verb idempotent.

| Ledger state | Card | Owner of the transition |
| --- | --- | --- |
| queued | created (idempotency key = delivery id) or unblocked (`--resolve`) | attendant |
| claimed | running | dispatcher/supervisor |
| question pending → awaiting answer | blocked (`needs_input`, by the runner; attendant re-blocks escapes) | runner |
| queued again (answer adopted) | unblocked | attendant |
| terminal via runner report | completed (rc 0) / blocked (rc ≠ 0) | supervisor |
| terminal via attendant (expiry, cancel) | completed by `SyncCards` | attendant |

### Crash recovery

A run whose worker died holds `claimed` (or `terminal_report_pending`
without sealed question evidence — a runner that died between the two
phases of its own report). The attendant recovers it: card not
`running`/`review`, claim older than the grace (10 min) →
`RecoverLostClaim` returns it to `queued` bound to the observed dead
claim's timestamp, so a live re-claim can never be stomped. Neither store
expires claims on its own — under the workflow constitution a dead claim
required operator surgery (measured live 2026-08-19); this transition is
the structural replacement. `terminal_report_pending` **with** sealed
question evidence belongs to the tick's expiry pass, which regenerates
the identical report and reacquires its lease itself; half-posted
questions (`question_pending`) are likewise tick-recovered.

### The triage coupling

The kanban's unblock-loop breaker routes a card to `triage` after
`BLOCK_RECURRENCE_LIMIT` (2) same-kind re-blocks; a bare unblock
deliberately preserves the counter, so the SECOND protocol-legal
clarification round of one run would already trip it. The attendant
therefore unblocks with `--resolve` (a fork addition): it acts strictly
on new information — a human answered — which is the case the breaker was
never meant to punish. The breaker still protects every other unblocker.
A card that lands in `triage` anyway (operator action, other tooling) is
a forced human decision and the attendant honors it: the run waits until
a person releases the card, exactly as with `scheduled`.

### Cards the attendant leaves alone

- `scheduled` (operator time-wait) and `triage` (the kanban's forced
  human decision) are human lanes; the attendant never automates through
  either — the run waits with the card.
- A runner-reported failure leaves the supervisor's `blocked` card as the
  visible record of that failure. The attendant retires cards only for
  its OWN terminations (`clarification_expired`, `cancelled`) — Complete
  first, Archive as the fallback for states Complete refuses.
- A `done` card under a queued run cannot be re-dispatched and still
  holds the delivery's idempotency key (only archiving releases it), so
  it is archived and a fresh card created.

## Identity and run references

The ledger seals an owner into every claim. Under GitHub Actions that was
the workflow run; here it is the pod engine: fixed repository/workflow-ref
digests from `runtime.json` plus the numeric `HERMES_KANBAN_RUN_ID` as
the per-attempt run id. Run references use the `local-run://` scheme —
`local-run://<owner>/<name>/<run id>/attempts/<n>` — sealing the same
three identities the GitHub URL carried. Each deployment accepts exactly
one scheme (`ReportRouteConfig.RunReferenceScheme`): a workflow
deployment cannot seal an unclickable local URI, a pod cannot seal a
fabricated workflow link.

## Deliberate drops and divergences from the workflow

- **model-preflight** (operator smoke probe of model endpoints): not
  carried over; probing is an operator action against the pod.
- **Intake gaps** end as an honest `clarification_required` terminal, as
  the workflow's report step decided; they are never posted as a question
  (their shape is not the readiness question format).
- **integration / production deliveries** are refused at consumer
  resolution: their success evidence needs the browser steps this runtime
  does not carry yet. `pull_request` is the shipped stopping point.
- **Toolchain provisioning** (pinned Node/pnpm, agent CLIs, bubblewrap):
  the image ships it; `resolveConsumer` asserts the consumer's toolchain
  binaries exist on PATH instead of installing them per run. Note the
  reviewing agent's bubblewrap sandbox needs unprivileged user namespaces
  — a pod security context decision, made at deployment.
- **Tool identity**: the workflow measured its checkout and binaries
  every run; the pod verifies the stage binaries against optional sha256
  pins in `runtime.json` (`worker_sha256` / `controller_sha256` /
  `browsercheck_sha256`) at runner start. The pins are never copied by
  hand: `deploy/pod/release.sh` reads them from the image's own
  `/etc/lassdas/tool-pins.txt` and writes ConfigMap and image together
  (the one time they were copied by hand, they were not — a live ticket died
  on "worker binary does not match its configured sha256 pin").
- **Review leftovers**: a reviewing agent that runs the repository's own
  tests leaves byproducts behind (a build cache, a config timestamp file,
  a hidden lint cache). They are not tampering — the published change is
  built from the sealed candidate, never from the tree — so the review
  check compares tracked changes and candidate content only, the
  reviewer's run is not scanned for changed files at all, and every
  review ends by deleting the leftovers so the next round starts from the
  candidate alone. Calling them tampering killed a live run after its
  review had passed.
- **Operator confirmation**: a report that asks a person to look
  (`deploy_failed` / `merge_unverified`, staging or production) is not
  the end of the story — the operator answers on the ticket with a
  comment whose first line is exactly 「確認済み」, posted after that
  report, by the requester or a user listed in `tracker.operator_user_ids`.
  The attendant acknowledges it once (marker `resolved`), seals
  `deliver-resolution.json` in the run directory, and the board shows the
  delivery as done ("運用担当者が確認済み"). Before this existed the board
  held such a delivery open for ever with nothing a person could press
  (live, 2026-09-02). While it waits, the snapshot carries the stage the
  run stopped at (`stage`) so the rail lights that node instead of going
  dark. The tracker is read for the confirmation for 60 days after the
  report (the board keeps showing the state afterwards), and a release
  report whose outcome seal was lost falls back to the production report
  file, so a lost seal cannot recreate the dead end.
- **Model roles**: the three agent roles read their model and credential
  from the pod environment — `LASSDAS_IMPLEMENTER_MODEL` /
  `LASSDAS_IMPLEMENTER_KEY`, `LASSDAS_REVIEW_A_MODEL` / `LASSDAS_REVIEW_A_KEY`,
  `LASSDAS_REVIEW_B_MODEL` / `LASSDAS_REVIEW_B_KEY` — and `entrypoint.sh`
  rewrites the Hermes profiles from them on every boot. The direct model
  calls of the reception (`models.*` in the consumer configuration) name
  their own credential variables, so the implementer's key can change
  without moving the reception. `LASSDAS_IMPLEMENTER_MAX_TURNS` (default
  200) caps the implementer's tool-calling iterations; the reviewers are
  capped at 40 in their profiles.
- **Budget hold**: right before the reception starts, the attendant asks
  the gateway for one token under every role's key (the reception's
  direct calls from the consumer configuration, the three agent roles
  from the environment). A refusal that names the budget holds the run:
  `budget-hold.json` in the run directory, one ticket comment (marker
  `budget-hold`), the board at intake as "予算不足で開始できません", and a
  retry every 10 minutes that resumes by itself once the cap is raised.
  Nothing else holds — a rate limit or an outage lets the run proceed.
  The gateway's two money refusals both count: a key's budget cap and the
  account's credit balance (`insufficient_quota`). The agent roles are
  probed under the models `entrypoint.sh` exports, so a role the attendant
  cannot see is logged, never silently skipped.
  Before this a run on an exhausted key spent its allowance on refusals
  and died as an unexplained model failure (live, 2026-09-02).
- **Failure streak hold**: when the same failure ended the last N
  deliveries (`chain.failure_streak_limit`, default 3; a success, a stop,
  an expired or required clarification and a refused or unresolved
  readiness end a streak — they say nothing about the automation), the
  attendant stops taking new
  deliveries, says so once on the newest failed ticket (marker
  `streak-hold`) and shows the reason as the board's banner. The
  operator's 「確認済み」 on that ticket lifts it (acknowledged once,
  `failure-streak-resolution.json` recorded in that run's directory).
  In-flight runs are not touched; the held ticket is read at most every
  two minutes.
- **Credentials**: the destination token reaches only the clone (via a
  one-shot GIT_ASKPASS) and the controller (explicit env), never the
  model-stage children — the runner strips it from its own environment
  first. The agents run as a separate user (below), so the runner's exec
  image and the state volume's owner-only files are closed to them; what
  remains readable to an agent is the workspace it was lent and the
  world-readable parts of the image.
- The **model workspace** is shaped exactly as the workflow's sealed-tar
  rebuild: synthetic single-commit git history, no remote, no credential
  (the clone token travels through a one-shot `GIT_ASKPASS` helper and is
  never stored), and a read-only base copy no agent is pointed at bounds
  what a change started from.

- **Adopted answers are preserved by the runner, not by a job of the
  instance repository.** The workflow rendered a resumed run's adopted
  answers (`worker preserve-answers`) and committed the record to the
  instance repository's knowledge tree. The pod's knowledge tree is the
  operator's copy of that tree on the state volume (`knowledge_root`,
  `/data/instance` in the example), and it has to be writable for this: a
  tree served from the read-only ConfigMap mount can only be read, and the
  engine logs `adopted answers not preserved` and moves on. The operator
  seeds that copy once from the instance repository (nothing in the image
  or the entrypoint does it) and refreshes it by hand when the repository's
  tree changes; the records the engine writes exist only on the volume, so
  a refresh must not replace the answers directory. Once the
  terminal report has sealed, `Terminal.Report` renders the record from
  the sealed envelope the run was claimed with (the snapshot re-sealed as
  the raw ticket, plus the cumulative clarification record — not from the
  workspace, which a run that died before read-ticket never filled) and
  writes it under `knowledge_root/<answer_knowledge.to>/<ticket key>.md`,
  replacing an older revision of the same ticket through a rename and
  refusing symlinks on the way. The reception reads that directory on the
  next ticket (`worker.LoadPreservedAnswers`), so a point the requester
  settled is not asked again. The question tick's own terminals — a run
  cancelled or expired while it waited for an answer — do not pass through
  `Terminal.Report`, so a run whose later round expired keeps no record of
  its earlier answers. Failures are logged for the operator and never fail
  the run.

## The observation session

The consoles show a login page to a browser with no session, so the
observation browser carries a jar of cookies. Two files hold it:

- the seed, `LASSDAS_E2E_SESSION_FILE` — a secret mount, Playwright
  storageState JSON, made once by a person who logs in;
- the renewed copy, `LASSDAS_E2E_SESSION_STATE_FILE`
  (`$STATE/e2e-session/session.json`, owner-only) — written by the
  attendant after every sign-in that lands.

The renewed copy remembers the digest of the seed it grew from, and it is
the jar in use for as long as that seed is the one mounted; a seed the
operator replaced wins over the copy. File times play no part (a secret
mount is rewritten on every pod start). Cookies whose own expiry has
passed are never installed. The copy keeps only cookies for the domains
the seed had, the login host and the landing — never for a site the round
trip merely passed through.

A consumer whose console follows the browser's language names the
language the browser asks for (`observation_language`, a BCP 47 tag):
without it a fresh headless profile asks in English and a promise about
Japanese wording can never be seen. A consumer whose console needs a
login names its entry
(`staging_login_url`, `production_login_url`). Before every observation
the browser opens the entry with the jar and counts itself signed in once
it rests on the environment's own origin, two seconds after the document
completed: an identity provider that needs a person keeps the browser on
its own page, and a console that rejects the jar sends the browser away to
its portal. Measured live (2026-09-03): a console session that lasts a day
was re-minted, with nobody at the keyboard, from the identity provider's
session — which itself rolled forward a fortnight on each use.

Every login that lands — the attendant's renewal and each observation's
own sign-in — rewrites the renewed copy with the jar it left behind. The
identity provider was seen to replace its session cookie on the first
login made from a person's seed (the seed's values were dead an hour
later), so the jar a login leaves behind is the only one known to work
next time. For the same reason the seed must never be used from anywhere
else once the pod has it: a login made elsewhere from the same seed kills
the pod's copy.

This is an operator's decision, not only a mechanism: a session the
engine keeps signing in with never expires on its own, so the person who
made the seed is not asked to log in again for as long as tickets keep
coming (a fortnight without one, and the identity provider's session
lapses by itself). The kept jar is a console session on the pod's state
volume, owner-only, readable by the engine's user alone: the agents run
as another user (below) and never see the jar paths in their environment.

Before the reception, right after the budget probe, the attendant signs
in through every destination's staging entry, once per destination per
few minutes rather than once per queued ticket. A login that lands
rewrites the renewed copy. A login the destination REFUSES — the browser
came to rest where a person is being asked to log in: a login page (an
admin console keeps its own on the landing host), a callback carrying an
error, an identity provider, a portal — holds the run: `session-hold.json`, one ticket comment
(marker `session-hold`), the board at intake in attention, a retry every
ten minutes, automatic resumption once an operator has logged in again
and replaced the seed. A login that merely could not be reached (an
outage, a browser-internal error page, a slow round trip) is logged and
the run proceeds — its observation says what it sees. The same division
the budget probe draws between "no money" and "no answer". An error page
served at the login entry's own URL reads as a refusal; the hold it
causes clears by itself once the entry answers again.

An observation the browser could not make is reported as
`observe_blocked`, never as a failed screen check: `sign_in` when the
login did not land or the target sent the browser off the destination's
own origin (a portal — the operator's to fix), `redirect` when the target
sent the browser to another page of the same destination (the requester
picks a page that does not redirect). A trailing slash or a fragment
added on the way is not a redirect. Both blocks are attention states an
operator closes with 「確認済み」; the URLs written to logs and tickets
carry neither query nor fragment (a login round trip parks on callback
URLs with authorization codes). The sealed observation itself stays as
strict as before — it refuses anything short of the target page at its
own URL — and the courtesy observation after a refusal is where the
reason gets told, with a screenshot of wherever the browser ended up.

A deployment workflow's completion is not the moment the new build is
served: the new pod is ready while the old one still answers at the edge
for a while. A sealed observation the page refused right after a deploy
is repeated a minute apart, up to five times (about four minutes; an edge
that takes longer to switch is outside this budget), before the refusal
counts; the evidence is the one observation that passed. Only the page's
refusal is waited out — the tool tells a refused login (exit 4) and every
other failure (exit 1) apart from it (exit 3), and those return at once.

## The investigating designer's identities

The investigating designer (docs/INVESTIGATING_DESIGNER.md) is not a
resident and not an agent: the runner's `investigate` card drives a model
call whose only tool is `probe`, and the kernel executes each probe with
three read-only identities — a namespaced Kubernetes ServiceAccount, an
AWS role with an explicit Deny list, and a PostgreSQL login that can
`SELECT` from content-free views and call no function outside
`pg_catalog` (EXECUTE revoked from PUBLIC in every schema the role can
use). Their shapes live in `deploy/examples/investigating-designer/`; the
consumer applies them and records the eleven stage-0 refusals listed
there before the role is enabled. The kernel process alone holds the
kubeconfig context, the AWS profile and the DSN; until the agents run
under their own UID (#23) the exposure is bounded by what the identities
allow, which is nothing writable and nothing secret.

The image ships the two clients the exec probes run — `kubectl` and the
AWS CLI, pinned by version and checksum in the Dockerfile (about 310 MB
together) — and nothing that holds a credential of its own. An exec probe
inherits only `PATH`, `HOME` and the identity pointers (`KUBECONFIG`,
`AWS_PROFILE`, `AWS_CONFIG_FILE`, `AWS_SHARED_CREDENTIALS_FILE`,
`AWS_REGION`, `AWS_DEFAULT_REGION`, `AWS_ROLE_ARN`,
`AWS_WEB_IDENTITY_TOKEN_FILE`; `internal/probe` `ExecEnvironmentNames`).
The consumer points `KUBECONFIG` at a kubeconfig whose token is the
ServiceAccount's projected token file. For AWS there are two shapes, and
they do not mix: either the cluster's pod-identity webhook sets
`AWS_ROLE_ARN` and `AWS_WEB_IDENTITY_TOKEN_FILE` from the ServiceAccount's
role annotation and the catalogue's `aws` argv names **no** `--profile`
(an explicit `--profile` in the argv makes the AWS CLI ignore those two
variables; the `AWS_PROFILE` variable alone does not), or
the catalogue names `--profile <readonly>` and that profile itself carries
`role_arn` and `web_identity_token_file` in the file `AWS_CONFIG_FILE`
points at. Neither variable carries a secret value; each names a file the
kernel's user can read.

### The design judges' own endpoints and launches

A design (or an investigation report) is judged by the reviewer of the
same letter as the candidate review by default. A consumer that wants the
design judged by a heavier model or another vendor without moving the
candidate reviews (design doc §11, decision 3) names the judges apart, in
two places that must come together: `models.design_reviewers[]` gives
each reviewer id the judge's endpoint (model, vendor, key variable; no
`lens` needed, a `design_lens` allowed), and `agents.design_reviewer_agents[]` its launch
definition (a profile of its own, its own credential source). Both are
all or none and name the reviewer ids of `models.reviewers`; the judges
must come from two vendors, must not share a (base URL, model) pair, and
at most one of them may run the designer's own model. `agent-design-review`
launches the judge's definition and seals the `DesignReview` with the
judge's model; the decision gate compares the reviews against the judges.
The lens is chosen by letter in the pod (`--lens A` for the evidence,
`--lens B` for the approach); a `design_lens` on an endpoint applies only
when a caller passes no letter.

On the pod the judge profiles (`lassdas-design-review-a/b`) are written
every boot from `LASSDAS_DESIGN_REVIEW_A_MODEL` / `_B_MODEL` (default: the
candidate reviewers' models) and `LASSDAS_DESIGN_REVIEW_A_KEY_VAR` /
`_B_KEY_VAR` (the variable the profile reads its key from; default: the
candidate reviewer's key variable), so a heavier judge is one variable
away and the candidate reviews stay as they are. The budget hold probes
the designer's key and every judge's key under their own labels (a judge
sharing the reviewer's key folds into that probe), and the spend report
lists the designer and the judges as their own seats.

## Agents under their own user

The implementer, the candidate reviewers, the design judges and the
applier are programs that read repository content; a prompt-injected one
must not be able to read the operator's console session. The image has
the engine's user, `lassdas` (uid 1000), which runs the attendant, the
runner and the worker, and a pool of agent users (uid 2000 to 2063), one
of which runs each agent launch. The engine is
unprivileged; the one program installed with file capabilities
(`cap_setuid`, `cap_setgid`, `cap_chown`, `cap_kill`) is `/usr/local/bin/agentexec`,
executable by the engine's user alone (`0750 root:lassdas`). The worker
starts each agent through it (`LASSDAS_AGENT_LAUNCHER`, set by the
entrypoint): the launcher lends the workspace to the agent user (chown,
deepest first, without following symlinks), starts the agent under that
user with the environment the worker composed — the launch definition's
own variables, the path, a locale and the agent's home — and returns the
workspace when the agent exits; the worker asks for it back once more
afterwards, for an agent it killed together with the launcher, and takes
it back before lending it, for a launch that died with the pod. The home
is made for each launch (`agent-home/<id>-…` in the run directory) and
seeded from the engine's home with what the agent's program reads there —
its Hermes profile (`config.yaml`, written every boot), the knowledge
rules the worker placed, a reviewer program's configuration — so nothing
an agent wrote into an earlier home reaches the next launch and two
agents running at once never share one; the launcher lends the whole
directory (Hermes keeps its state beside its profile), and the worker
takes it back when the run ends, so the engine can read what was left. An
agent therefore starts every launch from a fresh home: nothing it kept in
an earlier launch — a Hermes session, a memory, a skill — carries over,
by design; the run record holds the transcript. At boot the entrypoint
takes back whatever a pod that died mid-run left to the agent user under
the runs directory.

In the cards orchestration every card that runs an agent is a direct
command: the implement and apply cards run `runner chain-stage --stage
implement|apply`, and the runner hands the rendered instruction to `worker
run-instruction`, which starts the agent as above (the consumer's
`agents.implementer` and `agents.applier` name the launches). No card
runs an agent as the kanban's native worker under the engine's user any
more. The consumer's launch definitions are what runs: on the pod,
`agents.implementer` (and `agents.applier` for the design mode) is
`hermes --profile <the profile> -z` with `secret_env` mapping the
profile's `api_key_env` to the pod's key variable, and its
`timeout_seconds` now applies inside the card's wall (the implement card
allows 90 minutes; a shorter agent timeout is the tighter of the two).

Each launch runs as its own user: the image carries a pool of agent users
(`agent1` … `agent63`, uid 2001 to 2063, one group; `agent`, uid 2000, is
the boot check's probe user and runs no launch), the worker takes the
first free one for the launch's life (a lock file under the
state directory — the pool refuses to live anywhere else — freed when the
worker lets go or dies; the launcher accepts those users and their group
alone, and stops whatever a previous holder of the user left running
before it lends anything; the kanban
dispatches at most eight cards at once by default, far below the pool,
and a launch that finds every user taken fails closed), sets `USER` and
`LOGNAME` to that user's own name, and the top of a
lent workspace and home is closed to everyone but that user (0700); the
agent's temporary files go to a `tmp` inside its home (`TMPDIR`), not to
the `/tmp` every user shares. Two
agents running at once are therefore different users: neither reads the
other's workspace, home or process environment (its keys). What stays
visible across users is what the kernel shows everyone — a process's
command line, which for these agents carries the prompt — and a finished
run's home is taken back and removed, its workspace taken back and kept
closed. The launcher lends and returns trees under the runs directory
alone (`LASSDAS_AGENT_TREE_ROOT`, set by the entrypoint from the runtime
configuration's `chain.runs_root`).

Stopping an agent: a signal from the engine's user does not reach the
agent user's processes, so the worker stops a run by sending the launcher
`SIGTERM`; the launcher holds `cap_kill` for this, kills the agent's own
process group, then every process still running as that launch's user (a
tool that left the group with `setsid` included — the user is this
launch's alone), and only then returns the workspace; the same sweep
precedes every return. A workspace is lent to one launch at a time: the
launcher holds a lock beside it from the lend to the return, and a launch
that finds it held says so and waits up to two minutes, so a timed-out
card's re-dispatch does not lend a tree its earlier launch is still
returning. The agent also dies with the launcher whatever killed it — the
kernel sends it the parent-death signal with the launcher's capabilities
— and a launcher whose engine died without a word (a card's wall kills
the engine's process group, which the launcher is not in) notices within
seconds that it was orphaned and stops its agent — so a card's wall and
the worker's timeout end the agent, not only the launcher. The engine's own
home is closed by the entrypoint (0700), and records of runs made before
this engine closed its records are closed once per boot (0711 run
directories, 0600 files and 0700 directories inside; the lent trees, the
agents' homes and `agent-mcp.json` aside).

Rolling this out: the boot refuses a readable seed, so the StatefulSet's
seed volume (`defaultMode: 0440`) — with the pod's `fsGroup`, which is
what makes 0440 readable to the engine and what makes the kubelet write a
projected token 0640 rather than 0600 — must be in the live spec before
this image; the release script sets the image alone. A refused boot shows
as `CrashLoopBackOff`, `kubectl logs --previous` on the pod carries the
`REFUSING TO START` line naming the mode to set, and the release script's
rollout wait ends after 300 seconds with the ConfigMap already at the new
pins, so the way back is the previous image and the previous ConfigMap
set by hand.

What stays closed to the agent user by mode: the kept jar
(`$STATE/e2e-session`, 0700/0600), the engine's secrets (`$STATE/secrets`,
0600 to the engine's user), the run directories' own records (the sealed
records, the holds and resolutions, the instruction and the investigation
rounds: 0600, in run directories the agent user can enter but not list,
0711) and the engine's own home. The identities the probes use stay
closed the same way: the kubeconfig and the token or key file it names,
the AWS web-identity token and any credentials file, and a mounted
service-account token — the kubelet writes a projected token 0640 to the
pod's `fsGroup`; a Secret volume needs `defaultMode: 0440` (readable
through the `fsGroup`), because its default 0644 is readable by every
user. The entrypoint checks all of them on every boot with `agentexec
--check`, after tightening to 0600 any of them the engine's user owns
(the kubeconfig on the state volume, for one); an operator lists further
files in `LASSDAS_GUARDED_FILES` (colon-separated). A file the agent user
can read refuses the boot, as does a launcher without its capabilities (a
container with `allowPrivilegeEscalation: false` drops file capabilities
at exec): a pod that does not start is the fail-closed answer, and the
message names the mode to set. Two kernel-side facts the lending relies
on, stated so an operator does not remove them: a hard link from the
workspace to a file outside it cannot widen the chown, because
`fs.protected_hardlinks` (on by default) refuses linking a file one
cannot read or write, and the launcher holds no `cap_fsetid`, so the
chown back clears any set-user-id bit an agent left behind. The launcher
is not among the pinned stage binaries; it runs nothing of its own
choosing and only the engine's user can start it.
Verify a built image before it ships, from a container run as the
engine's user: `agentexec --check /etc/shadow` exits 0 (closed),
`agentexec --check /etc/passwd` exits 3 (readable), and
`agentexec --workspace "$(mktemp -d)" --home "$(mktemp -d)" -- id` prints
the agent user; the check's probe is the system shell run as that user,
because the launcher itself is not executable by it. The release script
asserts the first in the image and the second in the pod after the
rollout.

## Release discipline: the regression set

A release while a run's step is executing is refused by
`deploy/pod/release.sh`: it reads the board in the running pod, before the
build and again before the apply, and refuses when a run is outside done /
failed / stopped / question / confirm / intake / attention (waiting for an
answer, a Go, a first card or a person — a card in a human lane, no budget,
an expired sign-in — does not block) — or when the board cannot be read at all.
`RELEASE_ALLOW_INFLIGHT=1` overrides both for the release that fixes a
stuck run. Independently of that guard, a run is ended under the identity
it was claimed with: the terminal report and the question record take
their owner — repository, workflow and engine revision — from the ledger's
run row (`ClaimOwner`), not from the engine that happens to be running, so
a run that outlives a release still reaches `terminal`. A row that cannot
be read defers the report to the next tick rather than reporting under the
current identity; only a run with no claim row reports under it.

Every engine change goes out through `deploy/pod/release.sh`, and the
script refuses to build until `go test ./...` passes. The test suite is
the regression set: each live failure the pod has had is pinned by a test
that reproduces the condition, so a fixed hole cannot reopen, and a new
hole of a known class shows up before a ticket does. Adding a scenario
means adding a row here and the test it names.

| Scenario the live pod died on | Pinned by | Live case |
| --- | --- | --- |
| A design judge configured with a model the record does not name; a designer or judge key outside the spend report | `internal/worker` `TestDesignReviewRecordsTheJudgeThatRan`, `TestSpendListsTheDesignerAndTheDesignJudges`; `internal/attendant` `TestRoleProbesNameTheDesignerAndTheDesignJudges`, `TestRoleProbesNameTheDesignJudgesPodIdentities` | found by review, 2026-09-05 |
| An agent that could read the operator's session jar or seed, or the identities the probes use (same user as the engine; the jar paths in every card's environment; run records written world-readable; the implement and apply cards run natively by the kanban under the engine's user; a profile directory the agent user could not write; a launcher check that ran a probe the agent user could not execute; a closed directory lent before its contents; an agent the engine could not stop, a signal from one user not reaching another's; two agents of one user reading each other's keys; a boot reclaim whose find could end the boot) | `internal/worker` `TestRunAgentProcessGoesThroughTheLauncher`, `TestAgentEnvironmentNeverCarriesTheSessionJar`; `cmd/worker` `TestRunInstructionRunsTheAgentOnTheRenderedInstruction`; `internal/runner` `TestChainImplementRunsTheInstructionThroughTheWorker`; `cmd/agentexec` `TestParseInsistsOnOneModeAndASeparateUser`, `TestRunWithoutCapabilitiesFailsClosed`, `TestLendingOrdersADirectoryAfterItsContents`, `TestParseKeepsToTheTreeRoot`; `internal/worker` `TestAgentUsersAreDistinctWhileHeld`; `internal/attendant` `TestRunRecordsStayClosedToOtherUsers`; the entrypoint's boot check and the container verification in the release script | review of the sign-in change, 2026-09-03; reviews of the launcher and a container run, 2026-09-07 |
| A gateway answer of 502/503/504 — or a 429 that names its Retry-After — on one of an investigation's dozens of calls ending the run outright (the call is now posted again after a pause, up to three times, within the turn's own deadline; a 429 without Retry-After and every other status fail closed at once) | `internal/worker` `TestGatewayClientAsksAgainAfterAGatewayTimeout`, `TestGatewayClientRetriesA429OnlyWithRetryAfter` | live, 2026-09-08 |
| The reception presenting measured values and measurement record numbers it never obtained as choices, and the chosen one preserved as the requester's answer (a fresh assessment whose questions, choices or assumptions cite a record the ticket itself does not carry is refused as invented; the assessor is told it measures nothing; the checker's schema and prompt carry the defect code) | `internal/worker` `TestReadinessRefusesFabricatedMeasurements`, `TestCheckerPromptCodesAreInItsSchema` | live, 2026-09-08 |
| The approach-lens design reviewer asking, round after round, for a verification the design record cannot express (one wording or one measurement; the wording is not run by the kernel after apply), so a documentation-only design never converges (live: three rounds, `investigation_incomplete`) | `cmd/worker` `TestDesignReviewPromptStatesTheVerificationVocabulary` | live, 2026-09-08 |
| A design that only creates files carrying an `absent_text`, refused by the baseline wording rule with no way out named (the role spent its attempts guessing) | `internal/worker/investigate` `TestDesignOnNewFilesOnlyNeedsNoAbsentText`, `TestDesignValidation` (`absent text with only new files`) | live, 2026-09-08 |
| An investigation round that ended on answers the contract refused reported to the requester as the budget running out, with the advice to narrow the request (which changes nothing) | `internal/hook` `TestIncompleteCommentNamesRefusedAnswers`, `internal/attendant` `TestIncompleteRunPostsTheRefusalThroughBothPaths` (the chain failure and a pending resubmission both post the refusal), `TestIncompleteEvidenceReadsTheRoundRecord`, `internal/runner` `TestBuildReportCarriesTheIncompleteEvidence` | live, 2026-09-08 |
| A design reviewer calling a value unmeasured because it lay past the 2 KiB excerpt of a record the design cites (the role saw the whole record; the reviewer never opened the file) | `cmd/worker` `TestDesignReviewPromptCarriesCitedMeasurementsWhole`, `TestDesignReviewPromptWithdrawsUncitedExcerptsBeforeCitedRecords`, `TestDesignReviewPromptKeepsTheDesignsOwnRecordsWholeUnderALongReport`, `TestDesignReviewCitationsCoverEveryDesignField` | live, 2026-09-08 |
| The design reviewer reading the verification vocabulary as making `absent_text` mandatory, and refusing a design that rightly left it empty for a new file | `cmd/worker` `TestDesignReviewPromptStatesTheVerificationVocabulary` (`absent_text は任意です`) | live, 2026-09-08 |
| The role answering an "unmeasured" finding by swapping the cited id for another record of the same probe, round after round | `internal/worker` `TestRevisePromptStatesHowAPreviousFindingIsAnswered` | live, 2026-09-08 |
| A turn the provider ended with its own error inside a 200 (`finish_reason=error`) ending a design round's investigation on its first call, while the gateway retry covered only 5xx and 429 (the turn is now asked again on the gateway's pauses, up to their count) | `internal/worker` `TestInvestigateAsksAgainAfterAProviderError` | live, 2026-09-09 |
| A ticket that makes no screen promise (empty verification path) | `internal/runner` `TestReferenceStagingReportPassesWithAnHonestHold` | live, 2026-09-01 |
| A ticket arriving while another run is active | `internal/state` `TestQuestionFlowIngestsNewTicketsWhileARunIsActive` | live, 2026-09-01 |
| A reviewer that leaves tooling byproducts (files, directories, hidden caches) | `cmd/worker` `TestAgentReviewToleratesAndCleansUpToolingByproducts`; `internal/worker` `TestConfirmTreeMatchesCandidateToleratesReviewerToolingByproducts`, `TestCleanReviewByproductsRemovesOnlyWhatTheReviewerLeft` | live, 2026-09-01 |
| A reviewer that edits, reverts, or commits what it was asked to judge | `cmd/worker` `TestAgentReviewRejectsAReviewerThatEditsTheTree`, `TestAgentReviewRejectsAReviewerThatCommits`; `internal/worker` `TestConfirmTreeMatchesCandidateRejectsAReviewerThatEdits`, `TestConfirmTreeMatchesCandidateChecksASubmittedNewFile`, `TestRepositoryHeadMovesWhenTheReviewerCommits` | — |
| A model answer the contract refuses — unreadable (prose, a code fence, an unknown field) or readable but wrong in meaning (a pass verdict that lists reasons, a question id outside Q1–Q3) — on any of the eight JSON-answering calls | `internal/worker` `TestConverseJSONAsksAgainWhenTheAnswerIsUnreadable`, `TestConverseJSONGivesUpAfterThreeUnreadableAnswers`, `TestConverseJSONDoesNotRetryATransportFailure`, `TestAssessReadinessSurvivesOneUnreadableAnswer`, `TestCheckReadinessSurvivesOneContractViolation` | live, 2026-09-02 |
| A consumer whose staging deploy pushes no digest commit (its `staging_digest_commit` is unset), at the staging gate and again at the promotion gate | `cmd/controller` `TestStagingDeploymentValidationFollowsTheConsumerDigestPolicy`; `internal/githubapi` `TestCreatePromotionPullRequestAcceptsAConsumerWithoutADigestPolicy`, `TestPromotionProofFollowsTheConsumerDigestPolicy`; `internal/releaseproof` `TestStagingDeploymentAcceptsAConsumerWithoutADigestPolicy` | live, 2026-09-02 (the first gateway delivery to reach staging died at the staging gate; the promotion gate was found by review before a Go reached it) |
| An attention state nobody could clear (a report told an operator to look; no way to record that they had), and a rail that went dark for it | `internal/attendant` `TestResolveAttentionPostsOnceAndSeals`, `TestResolveAttentionStaysSilentWithoutAValidConfirmation`, `TestOperatorConfirmationFollowsTheStopRules`, `TestAttentionCarriesTheStageItStoppedAtAndAnOperatorCanClearIt`; `internal/hook` `TestDeliverResolvedContentCarriesTheMarkerAndNamesProduction`; `cmd/statusboard` `TestBoardPageLightsTheStageAnAttentionStateStoppedAt` | live, 2026-09-02 (a delivery held open 4.5 hours after its staging deploy had succeeded) |
| An implementer that stops making progress and spends the whole iteration budget on it | not a test: `LASSDAS_IMPLEMENTER_MAX_TURNS` bounds the iterations per run (`entrypoint.sh`) | live, 2026-09-02 (458 of 500 iterations, 425 of them the same search, no file changed) |
| A key out of budget spending a whole run on refusals | `internal/attendant` `TestCheckBudgetsHoldsOnceThrottlesAndClears`, `TestProbeBudgetOnlyABudgetRefusalCounts`, `TestRoleProbesCollapseDuplicatesAndReadTheEnvironment`, `TestBudgetHoldShowsAtIntakeAsAttention` | live, 2026-09-02 (the shared key ran dry; every ticket after it died as model_failed) |
| An observation browser that never launched (a Chrome flag given as a number, which the allocator refuses before the exec), indistinguishable from a wrong page | `internal/visiblecheck` `TestBrowserOptionsAreAcceptedByTheAllocator` | live, 2026-09-01 → 09-03 (35 runs, not one screenshot; found by a container run of the real code against the real staging console) |
| A sealed observation refused for its own screenshot: the capture was taken at quality 90 (a JPEG) and the evidence rules decode PNG only | `internal/visiblecheck` `TestScreenshotsAreTakenAsPNG` | live, 2026-09-03 (five refusals in a row on a page that showed the promised wording; found by re-running the tool on the run's artifacts with the reason unmasked) |
| A green staging deploy whose new build was not yet served at the edge (the old pod still answered 30 seconds later), so the sealed observation judged the previous build and a correct change read as a failed check | `internal/runner` `TestObserveUntilSettledRepeatsARefusedObservation`, `TestObserveUntilSettledGivesUpAfterTheBudget`, `TestObserveUntilSettledReturnsEveryOtherOutcomeAtOnce`, `TestObserveUntilSettledStopsWhenCancelled`; `cmd/browsercheck` `TestExitCodesTellTheRefusalsApart` | live, 2026-09-03 (the first unattended delivery after the observer was fixed: the screenshot was byte-identical to one taken before the deploy) |
| A screen check the browser could not make (an expired session jar sent it to the portal) reported as a failed check of a correct change, and a jar nobody renewed | `internal/visiblecheck` `TestLoadSessionCookiesFollowsTheSeedDigestNotFileTimes`, `TestInstallableCookiesDropOnlyTheExpired`, `TestLandedAcceptsTheOriginAndItsPathsOnly`, `TestStillSigningInKeepsTheLoginPageAndErrorsOffTheLanding`, `TestJarRejectedReadsWhereTheBrowserCameToRest`, `TestSameDocumentAndSameOrigin`, `TestSafeURLDropsQueryAndFragment`, `TestRelevantDomainsAndKeepCookie`, `TestWriteSessionFileRoundTripsOwnerOnly`; `internal/runner` `TestCourtesyVerdictNamesTheBlock`, `TestConsumerObservationCarriesTheLoginEntry`, `TestLoadE2ESessionCookies`; `internal/hook` `TestObserveBlockedReportsNameWhoActs`, `TestSessionHoldContentCarriesTheMarkerAndNamesTheDestination`; `internal/attendant` `TestCheckSessionsHoldsOnRefusalOnlyThrottlesAndClears`, `TestCheckSessionsClearsAStaleHoldWhenTheConfigIsUnreadable`, `TestSessionHoldShowsAtIntakeAsAttention`, `TestObserveBlockedIsAnAttentionState`; `internal/worker` `TestValidLoginURL`, `TestConsumerLoginURLsAreValidatedAndResolvedPerEnvironment`; `cmd/browsercheck` `TestSignInForPicksTheEnvironmentEntry` | live, 2026-09-03 (the first delivery to run every stage unattended reached staging and was refused at the screen check by a session 28 hours dead) |
| The same failure ending three deliveries in a row with nobody told | `internal/attendant` `TestDetectFailureStreakCountsOnlyTheNewestRunOfIdenticalFailures`, `TestHoldForStreakPostsOnceAndLiftsOnConfirmation`; `internal/hook` `TestHoldMessagesCarryTheirMarkersAndSpeakToTheRequester`; `cmd/statusboard` `TestBoardPageRendersTheIntakeHoldNotice` | live, 2026-09-02 (three tickets died identically on one implementer setting) |
| A stop comment competing with a deadline and a Go | `internal/attendant` `TestStopRequestedFailsClosed`, `TestContainsStopComment` | — |
| The proposer says "no design" and the checker disagrees, and the design is skipped anyway | `internal/worker` `TestNeedsDesignFallsToSafeSide` | — (design #18 §6, issue #30) |
| A destination with no trigger vocabulary configured letting a change skip its design | `internal/worker` `TestEmptyTriggerWordsNeverSkipDesign` | — (design #18 §6, issue #30) |
| An intake that cannot name the repository (gaps) | `cmd/worker/intake_cli_test.go` gap cases; the run ends as an honest `clarification_required` | live, 2026-09-01 |
| A probe request outside the declared shape (an unknown id, a slot value with whitespace or `;`, an http path outside its pattern, a link-local address) executed anyway | `internal/probe` `TestCatalogRefusesOutOfShapeRequests`, `TestHTTPProbeRefusesPrivateResolution` | design review, 2026-09-04 |
| A sql probe that lets a SELECT-only grant be bypassed (two statements, `EXPLAIN ANALYZE` of a write, `set_config`, advisory locks, `dblink`, `SELECT … INTO`, `FOR UPDATE`) | `internal/probe` `TestSQLProbeSendsOneReadStatement` | design review, 2026-09-04 |
| Key-shaped output stored or attached | `internal/probe` `TestSecretShapedOutputIsRefused`; `internal/attendant` re-scan in `uploadMeasurements` | design review, 2026-09-04 |
| A later round's appends breaking an earlier report's measurement fingerprint | `internal/probe` `TestMeasurementChainVerifiesPrefixes`; `internal/worker/investigate` `TestInvestigationRequiresMeasuredEvidence` | design review, 2026-09-04 |
| A "measured" finding citing no measurement, a refused one, or one outside the sealed prefix; a design whose cause cites no measured finding or whose files leave the allowed prefixes | `internal/worker/investigate` `TestInvestigationRequiresMeasuredEvidence`, `TestDesignValidation` | design review, 2026-09-04 |
| `DESIGN.md` drifting from `design.json` | `internal/worker/investigate` `TestDesignRenderingIsDeterministic` | design review, 2026-09-04 |
| A model answer outside the one-tool contract, a spent probe budget, or the wall ending the round without a sealed record read as success | `internal/worker` `TestInvestigateObjectsToUnsupportedReportsAndBudgetOverruns`, `TestInvestigationBudgetEndsHonestly` | design review, 2026-09-04 |
| A design round's cards colliding with the previous round's keys, or a design stage keyed in an implementation round | `internal/runtime` `TestChainCardKeysDistinguishDesignRounds`, `TestEnsureChainForDesignShapeKeysDesignAndImplementRoundsApart` | design review, 2026-09-04 |
| Design profiles half-configured | `internal/runtime` `TestDesignProfilesAreSetTogether` | design review, 2026-09-04 |
| An applier changing a file the design does not name, or its objection ignored and a candidate sealed anyway | `cmd/worker` `TestSealRefusesFilesOutsideDesign`, `TestSealTurnsObjectionIntoDesignRound`; `internal/worker` `TestPublishGateRequiresDesignSubset` | design review, 2026-09-04 |
| An investigation-only delivery with no ending, or its ending counted as a failure | `internal/hook` `TestInvestigatedIsATerminalCode`; `internal/attendant` streak exemption | design review, 2026-09-04 |
| A read-only identity that is read-only in name only (a `get` that returns a Secret, a `SELECT` that calls a writer function, a session that switches `transaction_read_only` off) | not a test: the eleven stage-0 refusals in `deploy/examples/investigating-designer/README.md`, recorded per consumer before the role is enabled; rows 7, 8 and 11 become `internal/probe` tests with the probe package | design review, 2026-09-04 |
| Tool pins that do not match the image's binaries | not a test: `release.sh` reads the pins from the image | live, 2026-09-01 |
| A run claimed under one engine revision is ended by the next: the terminal report is refused as `terminal_report_conflict` for ever when the owner comes from the running engine | `internal/state` `TestClaimOwnerIsTheIdentityTheRunWasClaimedUnder`; `Terminal.owner` reads the claim owner from the run row | live, 2026-09-05 (an investigation-only run claimed under 55ed29c, reported under 896efa8) |
| A toolchain missing from the image (`go`, `node`) | not a test: `release.sh` runs them inside the built image | first live run |

The script's own steps, in order: clean committed tree → the commit's own
CI run is green (the purity gate lives only there; a green local test says
nothing about it) → `go test ./...`
→ build (arm64, no cache) → push → pins and toolchain read from the image
→ `runtime.json` rewritten with the new engine sha and pins → dry run
stops here; `--apply` patches the ConfigMap, sets the image by digest,
waits for the rollout, and prints the pod identity check. The rollout is
not done until the attendant log shows no pin failure.
