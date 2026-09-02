# The single-pod runtime (Hermes kanban constitution)

This engine has two deployment constitutions. The original one runs the
stage pipeline as a GitHub Actions workflow with an AWS Lambda + DynamoDB
reception. This document specifies the second: everything — reception,
ledger, dispatch, stages — inside one host, with the
[Hermes](https://github.com/NousResearch/hermes-agent) kanban as the
dispatcher. Code comments across `internal/runtime`, `internal/runner`,
`internal/state` and the two commands reference this file.

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
- **Credentials**: the destination token reaches only the clone (via a
  one-shot GIT_ASKPASS) and the controller (explicit env), never the
  model-stage children — the runner strips it from its own environment
  first. Residual: the runner process's exec image remains readable via
  /proc by same-UID processes, and an agent holds a same-UID shell. The
  workflow's job isolation had no equivalent exposure. **Phase-3 gate:
  the image runs agents under a separate UID (or an equivalent boundary)
  before real deliveries.**
- The **model workspace** is shaped exactly as the workflow's sealed-tar
  rebuild: synthetic single-commit git history, no remote, no credential
  (the clone token travels through a one-shot `GIT_ASKPASS` helper and is
  never stored), and a read-only base copy no agent is pointed at bounds
  what a change started from.

## Release discipline: the regression set

Every engine change goes out through `deploy/pod/release.sh`, and the
script refuses to build until `go test ./...` passes. The test suite is
the regression set: each live failure the pod has had is pinned by a test
that reproduces the condition, so a fixed hole cannot reopen, and a new
hole of a known class shows up before a ticket does. Adding a scenario
means adding a row here and the test it names.

| Scenario the live pod died on | Pinned by | Live case |
| --- | --- | --- |
| A ticket that makes no screen promise (empty verification path) | `internal/runner` `TestReferenceStagingReportPassesWithAnHonestHold` | live, 2026-09-01 |
| A ticket arriving while another run is active | `internal/state` `TestQuestionFlowIngestsNewTicketsWhileARunIsActive` | live, 2026-09-01 |
| A reviewer that leaves tooling byproducts (files, directories, hidden caches) | `cmd/worker` `TestAgentReviewToleratesAndCleansUpToolingByproducts`; `internal/worker` `TestConfirmTreeMatchesCandidateToleratesReviewerToolingByproducts`, `TestCleanReviewByproductsRemovesOnlyWhatTheReviewerLeft` | live, 2026-09-01 |
| A reviewer that edits, reverts, or commits what it was asked to judge | `cmd/worker` `TestAgentReviewRejectsAReviewerThatEditsTheTree`, `TestAgentReviewRejectsAReviewerThatCommits`; `internal/worker` `TestConfirmTreeMatchesCandidateRejectsAReviewerThatEdits`, `TestConfirmTreeMatchesCandidateChecksASubmittedNewFile`, `TestRepositoryHeadMovesWhenTheReviewerCommits` | — |
| A model answer the contract refuses — unreadable (prose, a code fence, an unknown field) or readable but wrong in meaning (a pass verdict that lists reasons, a question id outside Q1–Q3) — on any of the eight JSON-answering calls | `internal/worker` `TestConverseJSONAsksAgainWhenTheAnswerIsUnreadable`, `TestConverseJSONGivesUpAfterThreeUnreadableAnswers`, `TestConverseJSONDoesNotRetryATransportFailure`, `TestAssessReadinessSurvivesOneUnreadableAnswer`, `TestCheckReadinessSurvivesOneContractViolation` | live, 2026-09-02 |
| A stop comment competing with a deadline and a Go | `internal/attendant` `TestStopRequestedFailsClosed`, `TestContainsStopComment` | — |
| An intake that cannot name the repository (gaps) | `cmd/worker/intake_cli_test.go` gap cases; the run ends as an honest `clarification_required` | live, 2026-09-01 |
| Tool pins that do not match the image's binaries | not a test: `release.sh` reads the pins from the image | live, 2026-09-01 |
| A toolchain missing from the image (`go`, `node`) | not a test: `release.sh` runs them inside the built image | first live run |

The script's own steps, in order: clean committed tree → `go test ./...`
→ build (arm64, no cache) → push → pins and toolchain read from the image
→ `runtime.json` rewritten with the new engine sha and pins → dry run
stops here; `--apply` patches the ConfigMap, sets the image by digest,
waits for the rollout, and prints the pod identity check. The rollout is
not done until the attendant log shows no pin failure.
