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

`HERMES_KANBAN_BOARD` は runtime の板名と一致させる。納品用鍵と route key は既存の守るファイル一覧で保護する。ローカルの板は `LASSDAS_BOARD_AUTH=local` で閲覧専用とし、ホストの loopback にだけ公開する。起動時に `/` と `/api/board` を認証なしで読めることを確かめる。板のパスワードは生成しない。Pod の既定は引き続き Basic 認証で、local モードでは操作と webhook の受信を無効にする。

`lassdas run stop` は対象のコンテナだけを停止し、台帳と作業記録を保持する。ローカル起動のために Pod の release、共有 workflow、レジストリや権限を変更する必要はない。

## Processes

| Process | Source | Role |
| --- | --- | --- |
| attendant | `cmd/attendant` | Resident. Every interval it runs one question tick (the whole reception protocol: tracker ingest, answer adoption, renotify, shortfall, expiry, half-posted recovery, board projection) and then advances every delivery's chain of stage cards (`SyncChains`): it claims the queued run, prepares it, creates the round's missing cards, reads what a finished stage sealed, and owns the question and the terminal report the ticket receives. It fully replaces the Lambda; no webhook endpoint exists — the attendant reads the tracker, the tracker never calls in. |
| runner | `cmd/runner` | Per-card. The Hermes stage profile's `worker.command` points at it, and the subcommand says which card it is running: `chain-stage` (one stage of a delivery's chain), `deliver` (the delivery continuation up to a named milestone), `e2e-check` (the post-merge staging observation). It works in the shared run directory the card names, shelling out to the unchanged `cmd/worker` / `cmd/controller` binaries, and ends with an exit code; it never touches the ledger. |

Both read one `runtime.json` (`internal/runtime.Config`), which is what
keeps them on the same ledger, routes and identities.

## The ticket page

The observer writes an initial
snapshot after reception and independently refreshes it every five seconds
by default (`attendant --observe-interval`). The board streams file changes
on a one-second watch. The running step is shown separately from the coarse
pipeline stage, including steps within reception. A missing, invalid, or
more-than-three-minute-old snapshot is displayed as unconfirmed progress,
not an empty healthy board. Connection/receive time does not prove that the
progress itself is fresh.

Observation is separate from advancing stages. `--chain-interval` checks
for stage transitions every ten seconds by default; `0` ties those checks
to the reception tick. Disabling either fast loop does not
disable the other. These are polling intervals, not upper bounds on stage
latency: measure a stage's completion to the next stage's start separately
from the age of the displayed snapshot.

Records keep the shared run-directory layout below, and the ticket page
reads a delivery's own directory under `chain.runs_root`. No board row and
no request names a directory: the delivery id is the only thing that comes
from outside, and it is reduced to a single path element. Clients cannot
select a workspace.

The status board lists the runs; each row links to `/tickets/<issue key>`, one page per ticket built from the run directory's own records, in order, with the evidence under each step:

- the reception: what the intake read, every assess/check attempt (a check that failed shows as a sent-back round with the checker's reasons; the last attempt carries the assumptions and questions the decision rests on), the sealed decision and its design line;
- each design round of a design-backed delivery: the investigation (findings, unknowns, questions, what it measured), the design (cause, approach, files, verification, blast radius, what it leaves alone), each design reviewer's verdict with its findings — or, for a reviewer that returned no verdict, the tail of its transcript — the round's decision, and the applier's objection when it could not follow the design;
- each implementation round: the changed files and the implementer's (or applier's) report;
- each review: the verdict and findings, or the tail of the transcript of a reviewer that returned no verdict;
- the sandbox validation, the pull request, the merge, the delivery verdict per phase;
- the recorded price of every priced model call (code reviews record no price, design reviews do, and the page says so);
- the ending: the failed step, the failure class in the requester's words, the tail of the trail, and — for a reception model turn that gave up — what the worker knew (`model-failure-detail.json`: the phrase, the model and effort, how many calls and why, the last answer's finish reason, token counts with the reasoning share, request id, gateway status), summarised in one sentence;
- the gateway's billing reading (`spend.json`, written by every terminal report of a run that read its ticket — the same figures the terminal comment carries, per key with the roles it serves and the key's name as the gateway reports it, never the key — so a failed run's cost shows too).

`/api/tickets/<key>` serves that view together with the board row (a key run more than once resolves to its newest claim); `/api/tickets/<key>/records/<name>` serves one raw record by page name — the fixed names, `readiness-<assessment|check>-<n>`, `stage-<n>-<file>` and `design-<n>-<file>` for the JSON records of a stage or design round — from the run directory the board row names, never from a path the client wrote. Everything shown passes the same secret scan as a probe output, and always before any cut: masked values keep their markers, a record the scan refuses whole is not shown, a raw record is scanned whole and only then cut, with a note, when longer than 2 MiB - one too large to scan whole (over 32 MiB - above every sealed record; only a measurements file grown over several design rounds can reach it) is refused rather than cut, and records are served one at a time. The run directories are read from `LASSDAS_RUNS_ROOT`, or the `runs` directory beside the status directory (the pod's `/data` layout). The page changes nothing: it has no action of its own. The pod's Basic credentials guard all three routes; under `LASSDAS_BOARD_AUTH=local` (the CLI's own machine, loopback only, read-only) they are readable without credentials like the rest of the local board — which now includes the run's transcripts and records, not only the board's own rows.

## The ledger

`internal/state.LocalStore`: one SQLite file, WAL, a single-writer
transaction per operation. The semantics are a line-for-line sibling of
the DynamoDB store — same rows, same attribute names, same conditional
transitions — verified by an equivalence test that drives both stores
through the same scenario and diffs the traces. Pure reads use snapshot
transactions without the write lock.

## The supervisor contract (fork worker.command)

The Hermes fork's profile-level `worker.command` support is what
dispatches a stage card. The contract the code relies on:

- The dispatcher spawns the supervisor with `HERMES_KANBAN_WORKSPACE`
  set: the delivery's own run directory, which every card of its chain
  shares and which persists across re-dispatches of the same card. A
  re-dispatched card removes its own half-written outputs before running
  again, and never trusts a file another agent left as proof that it
  already ran.
- Exit code 0 → the supervisor completes the card, and the attendant
  reads what the stage sealed on its next pass.
- Non-zero exit → the supervisor blocks the card with the failure reason,
  and that blocked card is the visible record of the failure.
- The card is the liveness signal: a card stays `running` exactly while a
  worker process lives; the supervisor's translation happens within
  seconds of exit.

### Card ↔ run states

The delivery→cards mapping is derived from the kanban itself on every
sync pass (`list --json --archived`, matching on the idempotency key
`<delivery>:<stage>:r<N>` or `:d<N>`) — there is no attendant-side
mapping file. A cached map that could be lost or trail reality made "no
card known" indistinguishable from "no card exists", which is exactly the
evidence the claim recovery needs. This also makes concurrent attendants
safe-if-pointless: every ledger transition is CAS-guarded and every card
verb idempotent.

| Ledger state | Cards | Owner of the transition |
| --- | --- | --- |
| queued | any stale card archived, the run prepared, the round's chain created | attendant |
| claimed | the round's next card runs; a finished one is `done` | dispatcher/supervisor |
| claimed, round to redo | the round's remaining cards archived, the next round's chain created | attendant |
| question sealed → awaiting answer | the whole chain archived; the question is posted to the ticket | attendant |
| queued again (answer adopted) | a fresh round's chain | attendant |
| terminal | the chain is left as it stands; the failure's blocked card remains | supervisor / attendant |

### Crash recovery

A run whose worker died holds `claimed` (or `terminal_report_pending`
without sealed question evidence — a stage that died between the two
phases of its own report). The attendant recovers it: with the run's
records unusable — the sealed envelope unreadable, or the engine changed
under the delivery — it archives the chain and calls `RecoverLostClaim`,
which returns the run to `queued` bound to the observed dead claim's
timestamp, so a live re-claim can never be stomped. Neither store
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
- A stage that exited non-zero leaves the supervisor's `blocked` card as
  the visible record of that failure.
- A card that cannot be re-dispatched still holds its idempotency key
  (only archiving releases it), so the attendant archives it and creates
  the round's chain afresh.

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
  認証済みの状態ボードでは、報告が投稿済みで受付期間内の依頼に
  「確認を記録して閉じる」を表示する。PR・変更ファイル・判定時刻と
  確認手順を読み、必要な対応を終えたことをチェックしてから送信する。
  既存の依頼者の資格情報で「確認済み」を投稿し、従来の受付処理が
  対応待ちを閉じる。再デプロイや本番反映は起動しない。送信済みと
  終了処理の完了は区別し、投稿失敗時は再送できる。
  ローカルの閲覧専用モードでは投稿せず、チケットでの確認方法を案内する。
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
- **The resolution ladder**: a card that fails is no longer the end of the
  delivery. The card seals what kind of thing went wrong
  (`history/<round>/<stage>-failure.json`), and the attendant plays a hand
  it has not played for that kind and dispatches the stage again — the
  failed stage and the ones after it are archived and rebuilt, the ones
  before it keep their records. A full volume sweeps the finished
  deliveries' copies of the destination, then gives back this delivery's
  own verification sandbox. When nothing is left to change, the waits
  between attempts double from `chain.retry_backoff_base_seconds`
  (default 60) up to `chain.retry_backoff_max_seconds` (default 1800),
  for as long as it takes; `chain.retry_max_attempts` (default 0, meaning
  no bound) is there for an operator who wants one. After
  `chain.retry_notice_attempts` (default 3) the ticket is told once that
  the delivery is still going (marker `ladder`), and a key that has
  reached its spending limit is told at once, because no model the engine
  could move to is reached any other way. The record of the climb is
  `retry/<stage>-r<N>.json` in the run directory, so a pod replaced
  mid-climb resumes where it was. A 「停止」 from the requester is read
  before each dispatch and ends the delivery as cancelled.
- **The failure streak hold is retired**: it stopped intake when the same
  failure ended the last N deliveries. Nothing counts now — a failed card
  is climbed away from rather than reported — so `chain.failure_streak_limit`
  is refused by name at load, the way `hermes_profile` is.
- **Intake pause**: `chain.intake_paused_since`
  (an RFC 3339 time) is the operator's explicit pause. The attendant and the
  console re-read it from the mounted config before every tick, so editing
  the ConfigMap is enough — no restart, which would interrupt the running
  deliveries the pause promises to leave alone (the edit reaches the mounted
  file within the kubelet sync period, about a minute). Queued deliveries are
  not started while it is set — each queued ticket is told once per pause
  (marker `intake-paused` with the pause instant) and the board shows
  受付停止中 with the instant in Asia/Tokyo — and claimed deliveries continue
  to their end. Removing the value resumes intake; the queued runs then start
  in order.
- **A configuration change no longer stops a published delivery**: the
  merge observation and the delivery continuation hold a finished run's
  sealed records to the configuration digest that run recorded, not to the
  mounted configuration as it stands now. Editing the configuration used to
  stop both — any field at all, because the binding is a digest of the whole
  file — and the merge observation stopped without a word, so a delivery
  whose pull request had been merged rested at マージ待ち for as long as the
  run was kept (live, 2026-09-24, more than ten hours). That lever is gone:
  a delivery whose pull request is already published now continues across a
  configuration change. A run still in flight is unchanged and is still
  refused when the configuration moves underneath it. The continuation's own
  checks still read the live configuration's values — the implementer model,
  the reviewer count, the validation commands and toolchain, the integration
  branch — so a change to one of those can still stop it, and a run whose own
  records cannot be read is now said once per run in the attendant log.
- **Waits on a person**: the Go wait (`chain.deliver.go_wait_seconds`,
  default 7 days) reminds the requester on the questions' weekday rhythm
  (1st, 3rd, 5th weekday at 10:00 Asia/Tokyo after the staging report,
  marker `go-reminder`), and a Go or an answer that arrives after its wait
  expired gets one reply (marker `late-word`): nothing resumes, file the
  request again to continue. Finished runs are read for such late words at
  most once an hour for 14 days.
- **Credentials**: the destination token reaches only the clone (via a
  one-shot GIT_ASKPASS) and the controller (explicit env), never the
  model-stage children — the entrypoint moves it out of the process
  environment into an operator-only file before any resident starts, and
  only the stages that reach the destination read it back.
  The agents run as a separate user (below), so the runner's exec
  image and the state volume's owner-only files are closed to them; what
  remains readable to an agent is the workspace it was lent and the
  world-readable parts of the image.
- The **model workspace** is shaped exactly as the workflow's sealed-tar
  rebuild: synthetic single-commit git history, no remote, no credential
  (the clone token travels through a one-shot `GIT_ASKPASS` helper and is
  never stored), and a read-only base copy no agent is pointed at bounds
  what a change started from.

- **Adopted answers are preserved by the engine, not by a job of the
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
takes it back when the run ends, so the engine can read what was left. A
design reviewer's home also carries a read-only copy (0444) of the run's
measurements: the file the engine writes is the engine's own, and the
prompt names the copy by the path this launch made. An
agent therefore starts every launch from a fresh home: nothing it kept in
an earlier launch — a Hermes session, a memory, a skill — carries over,
by design; the run record holds the transcript. At boot the entrypoint
takes back whatever a pod that died mid-run left to the agent user under
the runs directory.

Every card that runs an agent is a direct
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

`chain.runs_root` is required, and must be on persistent storage in a pod
deployment: a card left on Hermes' scratch default would put its workspace
outside the launcher's permitted tree and lose its records on replacement.
Every card of a delivery's chain receives `--workspace
dir:<runs_root>/<delivery_id>`.

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
set by hand. A `measurements.jsonl` that carries a record with `masked` kinds (written since the store-time masking change) does not verify under an image from before that change, because the older struct re-marshals the line without the field: a run whose measurements were masked stops with a broken chain after such a rollback, so let those runs finish or expire before rolling back past it.

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
| A turn the provider ended with its own error inside a 200 (`finish_reason=error`) ending a design round's investigation on its first call, while the gateway retry covered only 502/503/504 and a 429 with Retry-After (the turn is now asked again on the gateway's pauses, up to their count) | `internal/worker` `TestInvestigateAsksAgainAfterAProviderError` | live, 2026-09-09 |
| The reception asking the requester what a measurement would tell, on the belief that production cannot be reached (it can: the investigation stage measures with the catalogue), and refusing over-long texts as "invalid" with no field or number named | `internal/worker` `TestReceptionKnowsTheCatalogueAndTheTextLimits`, `TestReceptionRefusalsNameTheFieldAndTheLimit`, `TestEarlierAnswersLicenseRecordNumbers` | live, 2026-09-09 |
| A revise round's designer with no way back to the earlier round's records — previous_round carried the design and the findings only, and the read rule allowed no offset 0 — so an "unmeasured" finding could only be answered by swapping ids or measuring again (live, two rounds); a fixed marker for the launch home rewritten inside a record, so the excerpt the reviewer judges no longer matches the sealed record | `internal/worker` `TestReviseRoundListsEarlierRecordsAndMayReadFromTheStart`, `cmd/worker` `TestPreviousRoundCarriesTheInvestigation`, `TestTheHomeTokenIsPerLaunchAndLeavesRecordedOutputAlone`, `TestALaunchRefusesAStandInItDidNotDraw`, `TestTheRecordIndexKeepsTheNewestAndSaysHowManyItLeftOut` | live, 2026-09-08 |
| A design reviewer told to read the measurements file it cannot open (0600 to the engine; the reviewer runs as its own user), and judging without the ticket or the catalogue | `internal/worker` `TestCopyHomeFilesPlacesReadOnlyCopies`, `TestReviewingAgentGetsItsHomeFilesAndTheHomePath`, `cmd/worker` `TestDesignReviewPromptCarriesTheTicketCatalogueAndObjection`, `TestReadPreviousObjectionBecomesAFinding`, `TestAgentDesignReviewReadsTheRealTicketAndObjectionFiles`, `internal/runner` `TestChainDesignReviewPassesTheTicketAndTheObjection`, `TestAgentDesignReviewPointsTheReviewerAtTheCopyInItsOwnHome`, `TestTheLaunchHomePathFitsTheReserveAndAnOverlongOneLeavesNoHome`, `TestAFailedHomeCopyLeavesNoHomeBehind` | live, 2026-09-08 |
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
| The same failure ending three deliveries in a row with nobody told | — the hold that counted them is retired: a failed card is climbed away from rather than reported, so the run of identical endings cannot form (`internal/attendant` `TestTheThreeBrokenEndingsAreNoLongerProduced`) | live, 2026-09-02 (three tickets died identically on one implementer setting) |
| A card that failed ending the delivery, so a night's work reached the morning as a ticket saying nothing was done | `internal/attendant` `TestOnlyTheFailedStageAndTheOnesAfterItAreRebuilt`, `TestTheSameHandIsNeverPlayedTwice`, `TestADiskFailureSweepsTheFinishedRunsBeforeItsOwnSandbox`, `TestTheClimbSurvivesAPodBeingReplaced`, `TestAKeyAtItsLimitWaitsIsToldOnceAndResumes`, `TestAStopAskedForDuringTheClimbEndsTheDelivery` | live, 2026-09-25 (a review card went twenty-six minutes without an answer and the delivery ended) |
| A pod replaced mid-turn recorded as a model that would not answer, sending the delivery down half an hour of waits over a rolling restart | `internal/runner` `TestAStepKilledByItsContextIsSealedAsInterrupted`, `TestSealStageFailureSaysWhenTheCardWasStoppedRatherThanFailed`; `internal/attendant` `TestAnInterruptedCardIsNotCountedAsAModelFailure` | review, 2026-09-25 (a step killed by a signal comes back from Cmd.Run as an exit code with no error beside it) |
| A stop comment competing with a deadline and a Go | `internal/attendant` `TestStopRequestedFailsClosed`, `TestContainsStopComment` | — |
| The proposer says "no design" and the checker disagrees, and the design is skipped anyway | `internal/worker` `TestNeedsDesignFallsToSafeSide` | — (design #18 §6, issue #30) |
| A destination with no trigger vocabulary configured being judged by anything other than the framework's default vocabulary | `internal/worker` `TestUnsetTriggerWordsUseTheDefaultVocabulary` | — (design #18 §6, issue #30) |
| An intake that cannot name the repository (gaps) | `cmd/worker/intake_cli_test.go` gap cases; the run ends as an honest `clarification_required` | live, 2026-09-01 |
| A probe request outside the declared shape (an unknown id, a slot value with whitespace or `;`, an http path outside its pattern, a link-local address) executed anyway | `internal/probe` `TestCatalogRefusesOutOfShapeRequests`, `TestHTTPProbeRefusesPrivateResolution` | design review, 2026-09-04 |
| A sql probe that lets a SELECT-only grant be bypassed (two statements, `EXPLAIN ANALYZE` of a write, `set_config`, advisory locks, `dblink`, `SELECT … INTO`, `FOR UPDATE`) | `internal/probe` `TestSQLProbeSendsOneReadStatement` | design review, 2026-09-04 |
| Key-shaped output stored or attached | `internal/probe` `TestSecretShapedOutputIsMaskedNotDropped` / `TestMaskedOutputStaysReadable`; `internal/attendant` re-scan in `uploadMeasurements` | design review, 2026-09-04 |
| A later round's appends breaking an earlier report's measurement fingerprint | `internal/probe` `TestMeasurementChainVerifiesPrefixes`; `internal/worker/investigate` `TestInvestigationRequiresMeasuredEvidence` | design review, 2026-09-04 |
| A "measured" finding citing no measurement, a refused one, or one outside the sealed prefix; a design whose cause cites no measured finding or whose files leave the allowed prefixes | `internal/worker/investigate` `TestInvestigationRequiresMeasuredEvidence`, `TestDesignValidation` | design review, 2026-09-04 |
| `DESIGN.md` drifting from `design.json` | `internal/worker/investigate` `TestDesignRenderingIsDeterministic` | design review, 2026-09-04 |
| A model answer outside the one-tool contract, a spent probe budget, or the wall ending the round without a sealed record read as success | `internal/worker` `TestInvestigateObjectsToUnsupportedReportsAndBudgetOverruns`, `TestInvestigationBudgetEndsHonestly` | design review, 2026-09-04 |
| A design round's cards colliding with the previous round's keys, or a design stage keyed in an implementation round | `internal/runtime` `TestChainCardKeysDistinguishDesignRounds`, `TestEnsureChainForDesignShapeKeysDesignAndImplementRoundsApart` | design review, 2026-09-04 |
| Design profiles half-configured | `internal/runtime` `TestDesignProfilesAreSetTogether` | design review, 2026-09-04 |
| An applier changing a file the design does not name, or its objection ignored and a candidate sealed anyway | `cmd/worker` `TestSealRefusesFilesOutsideDesign`, `TestSealTurnsObjectionIntoDesignRound`; `internal/worker` `TestPublishGateRequiresDesignSubset` | design review, 2026-09-04 |
| A retry that erases the attempt it was meant to answer — the second launch failing before it starts, leaving no record at all, or a slow first attempt pushing a second launch into the card's wall | `cmd/worker` `TestTheRetryIsSkippedWithoutRoomOrTime`, `TestAnAgentThatChangedNothingIsAskedAgainWithTheTreeInFrontOfIt` (empty_attempts) | design review, 2026-09-09 |
| A model call that spends its whole allowance, leaving no answer, no usage and no cost, and ending the delivery where it was asked; a remedy for it that cannot fire, because the transport collapses its cause into a message; and a remedy that fires so often it spends most of a round on one unanswered question | `internal/worker` `TestASpentAllowanceJoinsTheProvidersRetryLadder`, `TestTheAllowanceRetryFiresOnTheRealTransportsError`, `TestATurnOfSpentAllowancesAsksAgainOnceNotThreeTimes`, `TestAProviderErrorKeepsItsOwnLadder`, `TestATransportsOwnTimeLimitIsAlsoASpentAllowance` | live, 2026-09-09 (the reception, after thirteen runs) |
| A model answer cut off at its allowance with nothing written - the whole completion counted as reasoning - treated as a long answer and given up on; `converseTurn` now asks the same turn again with the reasoning effort a step lower, twice at most, before the room logic, and the requester's note says the answer never began and that running the ticket again may pass (the sealed record keeps the configured effort, not the lowered one that answered) | `internal/worker` `TestConverseTurnLowersReasoningEffortWhenNoAnswerBegan`, `TestLoweredReAskFailingOtherwiseNamesTheLowering`, `TestAfterCutoffNamesWhatTheLastReAskChanged`; `internal/runner` `TestReasoningExhaustedCutoffIsToldAsSuch`, `TestLongAnswerAfterALoweringIsToldAsBoth`, `TestLoweredThenWidenedCutoffIsToldInFull` | live, 2026-09-15 (the reception's assessor, 32,768 reasoning tokens circling one thought) |
| A ticket whose delivery continued past the pull request staying 自動処理中 on the tracker for ever: the terminal report projected "running" and nothing projected the end after the staging or production report, so every finished ticket was closed by hand; `projectDeliveryEnd` now projects the end the status board sees (delivered, or needs attention) once per phase on every tick, again when an operator's 「確認済み」 changes it, only once the latest phase's report is posted (a release seal counts as the release phase even without a production report file: a stop or expiry during the Go wait, a dead promote card), and only for a ticket still in one of the automation's own statuses (a ticket a person closed, or one the tracker no longer has, is left alone) | `internal/attendant` `TestDeliveryEndPhaseFollowsTheStatusBoard`, `TestProjectDeliveryEndProjectsOncePerPhase`, `TestProjectDeliveryEndWaitsForThePostedReleaseReport`, `TestProjectDeliveryEndLeavesTicketsItDoesNotOwn`, `TestProjectDeliveryEndProjectsReleaseSealsWithoutAReportFile`, `TestProjectDeliveryEndLeavesADeletedTicketAlone`, `TestSyncChainsProjectsADeliveredEndOnce` | live, 2026-09-15 (a docs-only ticket merged to staging as not deployable) |
| A writer whose files land in its own home — the agent's tools resolve a relative path against the home, not the directory the launch lends it, so a design's repository-relative paths wrote nothing the seal could see | `internal/runner` `TestTheApplyInstructionNamesTheWorkingCopyAbsolutely`, `TestTheImplementInstructionCardCarriesTheWorkingCopyRoot`; `cmd/worker` `TestImplementInstructionRendersTheKernelPrompt` | live, 2026-09-09 (two runs, four probes) |
| An agent that reports the work as done without touching the working copy — the tenth live run described a file it never wrote, and the delivery died half an hour in | `cmd/worker` `TestAnAgentThatChangedNothingIsAskedAgainWithTheTreeInFrontOfIt` | live, 2026-09-09 |
| A request to create a file refused one step later, when the snapshot demanded a before-side for a file that does not exist yet | `internal/worker` `TestASnapshotRecordsATargetThatDoesNotExistYet` | design review, 2026-09-09 |
| A request to create a file with no answer among the paths that exist — the choice offered only existing files and forbade inventing one, so the run ended as an internal failure with the reason only in the pod log | `internal/worker` `TestARequestedNewFileIsOfferedAndAccepted`; `internal/runner` `TestTheRequesterIsToldWhenNoFileCouldBeChosen` | live, 2026-09-09 |
| Sealed reviews that cannot be read — one that will not parse, one removed after the decision was sealed, a dangling symlink, a renamed reviewer id, or a configuration naming no reviewer — counted as reviews that found nothing wrong: the delivery went back to the applier with a design a reviewer may have called wrong, and no record anywhere said the decision had been made that way | `internal/attendant` `TestSealedReviewsThatCannotBeReadAreAReasonToStop`, `TestAConfigurationWithNoReviewerIsAReasonToStop`, `TestWhereAnImplementationRoundGoesNext`, `TestAFailedRoundGoesWhereTheReviewsSay`, `TestTheAnswerDoesNotDependOnTheOrderOfTheReviewers`, `TestTheReasonAStoppedRunCarriesNamesNothingInternal` | audit, 2026-09-09 |
| A reception failure ending the ticket with the failure class and nothing else, the cause only in the pod log: the readiness gate had seven such exits and two of them said anything (the pretrip steps still say nothing, and are not covered here); a note a ticket could choose for itself, through the head of its own answer that a refused model reports; and a note telling a requester to send the ticket again when what stopped it was a limit that waiting does not lift | `internal/runner` `TestAReceptionStageThatNeverGotAnAnswerTellsTheRequesterSo`, `TestTheTransportsOwnFailureAlsoReachesTheRequester`, `TestAnUnnamedReceptionFailureStillLeavesANote`, `TestAnUnexplainedReceptionFailureInventsNoCause`, `TestATicketCannotChooseTheNoteItsRequesterIsShown`, `TestATicketCannotChooseAnyNoteThroughTheHeadOfAnAnswer`, `TestATransportFailureIsToldAsWhatActuallyHappened`, `TestAnAnswerFailureIsToldAsWhatItActuallyIs`, `TestTheNoteComesFromTheLineThatEndedTheStage`, `TestARecordTheGateCouldNotAcceptAlsoLeavesAReason`, `TestNoNoteInstructsTheRequester` | live, 2026-09-09 |
| A moment that passes ending a whole round on its first occurrence: the gateway's own 500, its 408, a provider's 529, and a connection the network dropped were each a permanent failure, while its 502, 503, 504 and a 429 with a short Retry-After were asked again. An investigation spends up to sixty probe calls across its rounds, so one of them was enough. Asking again is not free: the gateway is sent a fresh request each time, so a moment that passes after the upstream has already done its work is billed more than once — which is why a failure that waiting cannot change (a wrong address, a certificate that does not verify) is not asked again at all. And a moment that is slow to arrive spending the call's whole allowance between attempts, so that the failure reached its turn as a spent allowance — asked again in turn, for twice the time and none of the reason | `internal/worker` `TestAMomentThatPassesIsAskedAgainHoweverItArrived`, `TestADroppedConnectionIsAskedAgain`, `TestASpentAllowanceIsNotAskedAgainByTheTransport`, `TestTheFailureSaysHowManyAttemptsItTook`, `TestASlowStatusDoesNotSpendTheWholeAllowance`, `TestTheWaitBetweenAttemptsEndsWhenTheCallerGivesUp`, `TestASettingThatIsWrongIsNotAskedAgain`, `TestTheGuardStillLetsAQuickFailureBeAskedAgain`, `TestASlowUnreachableGatewayDoesNotSpendTheWholeAllowance`, `TestTheFailureSurvivesACallerWhoGivesUpDuringTheWait` | audit, 2026-09-09 |
| A machine signal manufactured by normalisation — a label saying the design is *not* wrong normalising onto design-wrong would archive the implementation round and spend a design round | `internal/worker` `TestTheCandidateReviewersLabelIsNormalisedAsItIsRead`; `internal/worker/investigate` `TestAFindingLabelIsMadeToFitInsteadOfEndingTheReview` | design review, 2026-09-09 |
| A sound review discarded over its label — one non-ASCII word in a finding code failed the whole card and ended the delivery as a model failure; on the candidate review the same label also carries design-wrong, the signal that sends the delivery back to its design | `internal/worker/investigate` `TestAFindingLabelIsMadeToFitInsteadOfEndingTheReview`; `internal/worker` `TestTheAgentsFindingLabelIsNormalisedAsItIsRead`, `TestTheCandidateReviewersLabelIsNormalisedAsItIsRead` | live, 2026-09-09 |
| A design that claims something does not exist from a listing of names, leaves a measurement it took out of the design, or promises measured values with no record id — each spent a review round in both live runs | `internal/worker` `TestTheDesignContractStatesWhatTheReviewersKeepRejecting` | live, 2026-09-09 (two consecutive runs) |
| An applier's objection written where its instruction says (the root of its working copy) ending the run as a change outside the writable scope instead of reopening the design; an objection beside other edits, or with an empty or overlong reason, accepted or left in the tree; an objection left behind by a run that died, read by the card's second attempt as this round's | `cmd/worker` `TestRunInstructionSealsTheAppliersObjectionWrittenInTheWorkingDirectory`, `TestAFailedApplierLeavesNoObjectionForTheNextAttempt`, `TestTheApplierCardClearsALeftoverObjectionBeforeItRuns`, `TestAnApplierHaltThatIsNotARegularFileIsRefused`; `internal/attendant` `TestApplyCardObjectionReopensDesignRound`; `internal/runner` `TestChainApplyCardCarriesTheDesignAndTheObjectionDestination`, `TestApplyInstructionCarriesThePreviousRoundsFindings` (rule pins) | live, 2026-09-09 (#103) |
| An investigation-only delivery with no ending | `internal/hook` `TestInvestigatedIsATerminalCode` | design review, 2026-09-04 |
| A read-only identity that is read-only in name only (a `get` that returns a Secret, a `SELECT` that calls a writer function, a session that switches `transaction_read_only` off) | not a test: the eleven stage-0 refusals in `deploy/examples/investigating-designer/README.md`, recorded per consumer before the role is enabled; rows 7, 8 and 11 become `internal/probe` tests with the probe package | design review, 2026-09-04 |
| Tool pins that do not match the image's binaries | not a test: `release.sh` reads the pins from the image | live, 2026-09-01 |
| A run claimed under one engine revision is ended by the next: the terminal report is refused as `terminal_report_conflict` for ever when the owner comes from the running engine | `internal/state` `TestClaimOwnerIsTheIdentityTheRunWasClaimedUnder`; `Terminal.owner` reads the claim owner from the run row | live, 2026-09-05 (an investigation-only run claimed under 55ed29c, reported under 896efa8) |
| A run that ends `model_failed` — 12 of the 33 runs the board holds, the most common outcome — naming no step, so the requester cannot tell a stop before anything was written from one after the change was made and reviewed, and the operator's only copy of the cause is a container log the next release erases | `internal/attendant` `TestAModelFailureTellsTheRequesterWhichStepFailed`, `TestAModelFailureOnTheImplementationSideAlsoNamesItsStep`, `TestEveryStepOfTheWorkHasARequesterFacingName`, `TestTheSentenceEveryStepProducesIsTrueOfThatStep`, `TestTheSealAndTheReviewAreNotToldAsTheSameStep`, `TestAReportPostedOnTheSecondAttemptStillNamesTheStep`, `TestAMalformedRecordCostsTheSentenceNotTheComment`, `TestThePublishCardNeverEndsARunAsAModelFailure`; `internal/runner` `TestEveryReceptionExitNamesItsOwnStep`, `TestAReceptionRecordThatCannotBeReadIsNotToldAsTheAIFailing`; `internal/hook` `TestTheModelFailureSentenceNamesTheStepWhenItHasOne`, `TestAStepNameThatIsNotOneBoundedLineIsRefused` | live, 2026-09-09 (a run ended `model_failed` at 16:02; the pod was replaced before the cause was read, and it is not recoverable) |
| A toolchain missing from the image (`go`, `node`) | not a test: `release.sh` runs them inside the built image | first live run |
| A delivery whose pull request was merged never noticed, because the merge observation was held to the configuration as it stands now and refused the ticket of every run sealed before the last change to it — a refusal indistinguishable from "not merged yet" | `internal/worker` `TestFinishedRunIsValidatedAgainstTheDigestItRecorded`; `cmd/controller` `TestReadMergedReadsAFinishedRunAfterTheConfigurationChanged`, `TestTheDeliveryContinuationReadsAFinishedRunAfterTheConfigurationChanged`, `TestInFlightVerbsDoNotAcceptARecordedDigest`; `internal/attendant` `TestAFinishedRunsMergeIsRecordedAfterTheConfigurationChanged`, `TestAFinishedRunWhoseRecordsCannotBeReadIsSaidOnce`, `TestAPublishedDeliveryWithAnUnreadableRecordIsSaidOnce`, `TestATransientRefusalIsNotSaid`, `TestAFinishedRunThatPublishedNothingIsNotSaid`; `internal/runner` `TestPostTerminalVerbsNameTheDigestTheRunRecorded`; `cmd/attendant` `TestResidentObservesHumanMerge` | live, 2026-09-24 (the board asked for a merge made ten hours earlier; a run sealed under the current configuration was recorded within four seconds) |
| A run whose failure could be read only in the pod — a reviewer that returned no verdict, a key whose allowance ran out, a design decision, the price of the calls — with the board showing one line and the ticket a paragraph; the ticket page now shows the whole run from its own records, masked the way probe outputs are, by the key the board lists and never by a path | `internal/ticketview` `TestBuildAssemblesTheRunInOrderWithCostsAndMaskedSecrets`, `TestBuildShowsEveryReadinessAttempt`, `TestBuildReadsDesignRoundsAndApplierRuns`, `TestBuildMasksBeforeTakingATail`, `TestRecordPathServesOnlyKnownNames`, `TestShownRefusesWholeSecretsAndBoundsLength`; `cmd/statusboard` `TestTicketAPIResolvesTheKeyThroughTheBoardOnly`, `TestTicketAPIPicksTheNewestRunOfAKey`, `TestTicketRecordsAreAllowListedAndMasked`, `TestTicketRecordsAreMaskedBeforeAnyCut`, `TestTicketRoutesSitBehindTheAccessGate`, `TestBoardHTMLLinksEachCardToItsTicketPage` | live, 2026-09-15 (two failed deliveries whose causes were found only through the pod's records and the gateway's database) |
| A reception that reasoned itself to the wall ending as model_failed with nothing but the step name in the run directory — the cause (finish reason, 32,768 reasoning tokens, no answer) found only in the gateway's own logs; the turn now writes what it knew on one stderr line, the runner keeps it as `model-failure-detail.json` beside the failed step, and the ticket page says what the numbers mean | `internal/worker` `TestATurnThatGivesUpLeavesItsDetailOnOneLine`, `TestAGatewayStatusReachesTheDetailAsANumber`, `TestASuccessfulTurnLeavesNoDetailLine`, `TestParseFailureDetailLineIsStrict`; `internal/runner` `TestAReceptionModelFailureKeepsTheWorkersDetailBesideTheRun`, `TestAReceptionFailureWithoutDetailLeavesNoRecord`; `internal/ticketview` `TestBuildReadsTheBilledSpendAndTheModelFailureDetail`, `TestModelFailureSummaryNamesEachCase` | live, 2026-09-15 (a readiness assessment that spent its whole allowance reasoning) |
| A run's cost read from the gateway for the terminal comment and then lost — the board and the ticket page could sum only the prices a few records carry, a fraction of the bill; the reading is now kept as `spend.json` on every report, per key with its roles, and the page shows it first | `internal/runner` `TestTheSpendReadingIsKeptBesideTheRun`; `internal/ticketview` `TestBuildReadsTheBilledSpendAndTheModelFailureDetail` | live, 2026-09-15 (a ticket billed $2.79 whose page said $0.05) |

The script's own steps, in order: clean committed tree → the commit's own
CI run is green (the purity gate lives only there; a green local test says
nothing about it) → `go test ./...`
→ build (arm64, no cache) → push → pins and toolchain read from the image
→ `runtime.json` rewritten with the new engine sha and pins → dry run
stops here; `--apply` patches the ConfigMap, sets the image by digest,
waits for the rollout, and prints the pod identity check. The rollout is
not done until the attendant log shows no pin failure.
