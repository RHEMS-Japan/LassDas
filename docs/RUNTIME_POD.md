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
volume, owner-only, readable by whatever runs as the engine's user.

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
AWS CLI, pinned by version and checksum in the Dockerfile (about 290 MB
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
(an explicit `--profile` makes the AWS CLI ignore those two variables), or
the catalogue names `--profile <readonly>` and that profile itself carries
`role_arn` and `web_identity_token_file` in the file `AWS_CONFIG_FILE`
points at. Neither variable carries a secret value; each names a file the
kernel's user can read.

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
| A toolchain missing from the image (`go`, `node`) | not a test: `release.sh` runs them inside the built image | first live run |

The script's own steps, in order: clean committed tree → the commit's own
CI run is green (the purity gate lives only there; a green local test says
nothing about it) → `go test ./...`
→ build (arm64, no cache) → push → pins and toolchain read from the image
→ `runtime.json` rewritten with the new engine sha and pins → dry run
stops here; `--apply` patches the ConfigMap, sets the image by digest,
waits for the rollout, and prints the pod identity check. The rollout is
not done until the attendant log shows no pin failure.
