# Role-chain experiment

This is a standalone, undeployed prototype, not the production entry point.
It does not import the previous worker or runner. It is not a claim that an
arbitrary request will finish unattended or reach a live delivery target.

The original request and ordinary prose reports go to a routing model. The
router invokes a configured role; the role runs an existing agent harness and
returns its own words. There are no candidate/review schemas, signatures,
confidence thresholds, or content-based output gates. A native API choice or
function call identifies which configured action to execute, not whether a
working role's answer passes a certificate check.

## Current executable

From this directory:

```sh
go run ./cmd/engine --config operator.json --request request.txt --run-dir run
# Or read an issue's original title and description:
go run ./cmd/engine --config operator.json --issue EXAMPLE-1 --run-dir run
# Continuously collect new issues within an explicit operator scope:
go run ./cmd/engine --config operator.json --watch --run-dir queue
# Fetch the current model catalog before selecting experiment models:
go run ./cmd/engine --list-models
```

`operator.json` configures `router.mode` (`jev` or `llm`), routing endpoints,
credential environment-variable names, and `roles`. Each role has a name,
purpose, and one or more processes. A process specifies an argv array,
directory, explicit environment, and named credential sources. Standard input
carries the original request, current assignment, and previous reports without
shell interpolation. Use the existing harness's final-response interface;
do not assume that a CLI's `--quiet` flag removes reasoning displays or other
UI output. A live experiment found that those displays inflated the next
role's context. Its thin native SDK adapter forwards the complete final prose
on stdout and diagnostics on stderr, without classifying the answer.

### Automatic intake experiment

`--watch` uses the configured tracker and requires an explicit intake scope:

```json
"intake": {
  "project_id": 17,
  "created_since": "2026-01-02T00:00:00Z",
  "poll_interval_seconds": 30,
  "max_running": 1
}
```

This is part of `operator.json`, not a format imposed on a requester. The
timestamp is inclusive. Only issues in that project created at or after it are
accepted; issue status does not narrow that scope. Interval and capacity default
to 30 seconds and one running request. The explicit starting time prevents
silently executing every historical ticket. Do not activate this on a real
project without authority for the chosen scope and the configured actions.

Each scan uses fresh tracker pages. The first accepted native issue record is
saved unchanged in `queue/jobs/<id>/issue.json`; later remote edits do not replace
the original request. Its title and complete description go directly to the
existing single-request engine, without a model-generated reception contract.
The per-issue workspace, agent homes and runtime history remain in that job
directory. The collector schedules unfinished histories and does not rerun
histories whose router chose done. This is not an independent claim that the
router's completion judgment was correct. Accepted work remains available for
restart even if discovery is unavailable or the issue disappears remotely.

One process owns a queue directory. Graceful cancellation waits for its active
children to stop before releasing ownership. Restarting the same queue reuses
original requests, pending histories and workspaces; the existing engine tells
the router that interrupted actions may already have happened. Discovery errors
and unfinished-child reasons remain in the log. Discovery now runs separately
from accepted-work scheduling and stop observation, so a stuck list request
does not block cancellation or recovery of local work. This is not distributed
ownership across separate queue roots or supervision after a hard process crash.

In watch mode a process directory must be empty or relative to its job workspace,
not a shared absolute checkout. The launcher receives `TASK_WORKSPACE`,
`TASK_HOME` (different for each role process), and `TASK_ISSUE`. These names cannot
also be credential/model-selection destinations. The Hermes bridge uses
`TASK_HOME` instead of a global `HERMES_HOME`. Commands and referenced bridge
paths must be available from the per-job directory. Repository preparation and
actual delivery permissions still have to be supplied by the configured roles.

**These are logical working directories, not filesystem/network isolation.**
An authorized launcher must confine access to other requests, controller state,
credentials and delivery targets before live untrusted work is enabled.
Production packaging and real tracker-to-production operation are not
implemented by this collector. Its tests use fixture APIs and actual local child
processes, not live-model judgments or a production tracker.

### Requester stop in watch mode

The issue's original creator can stop its queued or running work by posting a
comment whose first nonblank line is exactly `停止`. A reason can follow on later
lines. Optional `intake.stop_user_ids` lists additional authorized operator user
ids. The account identity comes from native tracker metadata, not a name claimed
in text. Quotes, later mentions, role reports and other users' comments are not
stop instructions. If the requester identity is unavailable and no operator is
configured, work waits with the reason visible instead of running without an
identified stop authority. A shared bot/requester account cannot distinguish
machine posts from human instructions; use separately scoped service identities.

Each accepted unfinished request reads its control comments before starting and
on the configured polling interval, independently of occupied execution slots,
other roles and issue discovery. It uses the native
[comment API](https://developer.nulab.com/docs/backlog/api/2/get-comment-list/).
The collector rereads the full comment history, including edits to older
comments. An authorized stop cancels the current process group/router call;
it does not wait for an LLM to agree. The native stop comment is retained as
`stop-request.json`. The work remains stopped on restart even if that remote
comment is deleted. An unreadable saved instruction holds work too. No automatic
resume command is implemented. This record is the user's instruction, not a
completion mark attached to a model answer.

If a stopped role returns reports or errors, the chain makes one final local
history-write attempt to retain them, without another model call or external
action. The interrupted assignment remains pending: on an authorized resume,
the next role sees both the reports and the warning that external effects may
already exist. A local write failure is logged and returned, not retried forever
during shutdown. This does not recover unreturned output, bypass a saved stop,
or guarantee persistence through disk failure, a hung filesystem or a hard crash.
An installed-SDK trial with a synthetic model and tracker stopped a pending
comment POST after the tracker had stored it. The returned cancellation and
diagnostics survived in history, no further routing occurred, and the recorded
native child PIDs and scoped listener were gone. A stored remote effect is not
undone by retaining its local observation.

An unreadable/unavailable control channel pauses an active engine and retains
unfinished history; once reads recover it can resume with the existing warning
that an interrupted action may already have taken effect. A control read is
bounded by the polling interval (and the transport's existing deadline). This
does not promise instantaneous stopping during a network failure. The fixture
tests confirm cancellation in the first read that returns an authorized stop,
including implementation, review, delivery, reporting, router waits, and queued
work while another request owns the only execution slot. They also exercise
HTTP failure, an unresponsive read, storage failure and restart after deletion
of the remote stop. These are not live tracker or native-model stop trials.

**Prior external effects are not rolled back by cancellation.** The control
monitor is specific to `--watch`, not the standalone `--request`/`--issue` command. Large-queue API
rate limits, per-request monitoring resource usage, distributed ownership and
crash supervision are not production-validated.

To send a stop report, set `intake.stop_report_role` to an existing configured
reporting role. An unknown role is rejected before intake. The saved stop holds
the original work first; then only that role is available to the existing
router/engine. The full accepted request, native stop comment and stopped-work
history reach it as context. No new model or working-answer format is required.
Without this operator setting, a stop is logged locally but not reported to the
tracker. Use a reporting harness restricted to observation and the assigned
issue's comments; merely naming a broadly privileged harness `report` does not
make it safe. Permissions are not narrowed by a prose instruction.

Reporting uses the same local history store in `stop-report/`. It does not
clear the stop or mark the original request delivered. A restart resumes only
unfinished reporting, even if the remote stop was removed or discovery is down.
The same role home, issue-scoped access, configured model selector and execution
capacity are reused; a model-enabled reporting retry fetches a fresh catalog.
There is no content check that turns the role's prose into a completion stamp.
Reporting completion is still the router's judgment, not independent proof that
the final comment is correct or exists. A privileged/misbehaving reporter or a
router that incorrectly chooses done can violate that expectation; real-model
acceptance remains necessary. Do not interpret the fixture tests as that proof.

Tests use real subprocesses and a synthetic tracker/router to exercise queued
and active stops, report submission stored before a 503, comment readback without
a duplicate POST, controller cancellation during submission and reporting-only
restart, original-history preservation, and denial of another role's dispatch.
They also verify fresh model selection on both reporting launches. No live issue
or production delivery was used.

A further installed-SDK run exercised watch intake → saved stop → reporting-only
engine → native terminal → tracker CLI, under the existing laboratory OS sandbox.
The source stored one report before returning 503; list/id readback found its
unchanged prose. The original run stayed unfinished with no role execution,
while reporting finished. Recorded child PIDs and scoped access disappeared.
Model answers and the tracker were synthetic; this validates wiring and the
observed access boundary, not real-model judgment or production isolation.

### Current model list

`--list-models` makes a new credential-free OpenRouter catalog request on every
invocation and prints JSON with the observation time and complete model entries
on stdout. It requests all output modalities, including decision models, with
no pagination limit. Unknown model metadata is preserved. This follows the
[catalog API](https://openrouter.ai/docs/api/api-reference/models/list-all-models-and-their-properties).

There is no saved-list fallback: unavailable, invalid, empty or oversized
responses produce an error and no partial list. The query is separate from
running a request and cannot be combined with request flags. An administrative
query error does not label a work request completed or failed.

The catalog alone does not prove value for a task. Do not substitute a fixed
version shortlist or the cheapest price for that assessment.

### Experimental per-launch model selection

The engine can select a current working model before each configured process
launch. Set that process's `model_env` to the environment variable its existing
harness reads (`NATIVE_MODEL` for the shipped bridge). Configure the shared
`model_selection.judge` with the same decision-service fields as
`router.decision` (`url`, `model`, `key_env`), and `model_selection.authors` with
the approved publisher ids. No new account or credential value is required.
For the initial Chinese-model experiments the configured publishers are
`qwen`, `z-ai`, `deepseek`, `moonshotai`, and `minimax`; these names are not
hard-coded in the selector and are not a complete nationality classifier.

Every selection fetches a new complete catalog. Text/tool-capable entries from
those publishers, with their current descriptions, dates and prices, are passed
to the decision model along with the original request and role responsibility.
Runtime failures of previously selected models also inform the choice. Versions
and price thresholds are not fixed in code. Processes in one parallel group
select different publishers, fetching the list again for each selection. The
work processes still receive the same prior reports, not a peer's new answer.

Catalog or selection errors return their reasons to the chain as role
observations. They neither complete the request nor silently launch an old
model left in the process environment. A later retry fetches again. The
selected endpoint is recorded as `model` in the runtime history; this says what
the harness was asked to use, not which upstream provider ultimately served it
or whether its work is correct. Selection does not inspect working prose or
authorize delivery. These mechanics do not establish unattended completion or
optimal model value.

An optional `model_selection.fallback` names a configured ordinary-LLM chat
service using the same `url`, `model`, and `key_env` fields. It is not inferred
silently from another role. If primary selection fails, the engine logs that
reason, fetches the catalog again, and asks this alternative to choose an
endpoint from the new eligible list. The same publisher limits, peer separation
and original request apply. This native function call cannot finish the work;
its only choices are model endpoints. If both services fail, both reasons return
to the chain without starting a stale worker. Cancellation does not start the
alternative. A long-lasting outage of every configured selector still needs
recovery; this is not a guarantee that every external failure can be resolved.

Processes without `model_env` use their explicitly configured command as before
(including non-LLM tools); the presence of `model_selection` alone does not
rewrite their environment. Production packaging and automatic setup remain
unimplemented. API-fixture tests exercise a real child process after a catalog
outage and verify fresh selection, publisher separation and reason retention;
they are not evidence of live task quality.

A live selection-only probe fetched the catalog separately for both reviewers,
called the actual decision service, and passed the selected ids to real child
processes. The first version selected an older coding model despite newer
frontier entries. The selector now supplies readable listing dates and canonical
versions, and explicitly distinguishes current availability from current
generation. Three subsequent probes selected the current-generation pair, with
two fresh catalog requests in each. These observations do not establish optimal
selection, resistance to every misleading catalog entry, or successful task
completion. The selected worker models were not themselves asked to do work in
these selection-only probes.

A separate live fault probe replaced the primary selector with an explicit
HTTP 503 fixture. The configured real chat alternative selected both reviewers
and their ids reached real child processes. Four actual catalog requests were
observed: a primary attempt and a fresh alternative attempt for each reviewer.
The primary reasons remained visible. This tests selection recovery, not a real
provider outage or completion of a working-role task during that outage.

A fresh native-agent task also used per-launch selection throughout issue
intake, implementation, two independent reviews, local artifact delivery and
reporting to a tracker fixture. Five process launches made five catalog fetches;
requested working-model ids matched the returned API model ids. Thirteen
independent post-run cases and the generated ten-test suite passed. A stored
comment followed by HTTP 503 was reconciled by readback without a second POST.
The fixture had a stale CSV title for this search-CLI task, and that title was
repeated in the final report; its description and tested artifact were the
search task. This is a limited local observation, not production delivery,
restart recovery or a resolution of the earlier CSV counterexamples.

### Existing native-agent connection

`harnesses/hermes.py` is the stdin bridge to an installed Hermes SDK. It uses
the SDK's existing agent/tool loop, not a new implementation of one. In the
role's isolated execution environment, configure the process argv as the
installation's Python executable followed by the absolute bridge path. Make
the installation's `run_agent` module importable, for example with a scoped
`PYTHONPATH`. This repository does not install or modify that dependency.

Provide these process settings explicitly:

- `HERMES_HOME`: a separate writable agent directory for each role/reviewer.
  Watch mode instead supplies and creates a per-process `TASK_HOME`.
- `OPENROUTER_BASE_URL` and `NATIVE_MODEL`: the selected endpoint and model.
  The bridge has no model default, shortlist or catalog-selection policy.
- `OPENROUTER_API_KEY`: map a named credential source through `secrets`, not
  a literal credential in `env` or the configuration file.
- Optional `NATIVE_REASONING_EFFORT` (default `low`) and `NATIVE_MAX_TOKENS`
  (default `6000`, per native API response, not a request failure limit).

The reasoning setting is also passed through the SDK's explicit OpenRouter
request override. An installed-SDK trial found that its URL-based capability
check omitted this setting when using a relay, despite receiving it in the
constructor. A local HTTP receiver reproduced the missing parameter and now
observes the default/explicit `low` and explicit `high`, along with the configured
token limit. This verifies transmission, not that a provider honors every effort
level or that more reasoning improves delivery. Choose an effort supported by
the selected endpoint's current catalog metadata. Earlier native trials that
only recorded the bridge's setting do not establish the actual wire effort.

A subsequent paired native review kept the original request, source, earlier
reports and selected two-model pair unchanged, with the same 12,000-token
response ceiling. Both reviewers accepted the known empty-value-row loss at
both `low` and `high`, treating documentation and tests as justification for
the behavior change. The actual API bodies carried the selected effort; no
completed response hit the token ceiling. This one counterexample does not
establish a general ranking, but raising effort did not repair it. The separate
intended-change controls were stopped after this negative result, not counted
as completed comparisons. No higher default or answer-content gate was adopted.

A different current two-publisher pair, chosen by the public per-launch selector
from fresh catalogs, also approved the same behavior change. Both final reports
were complete, after recovery from two private relay-observation errors. This
does not establish general model rankings; changing the pair alone did not
repair this counterexample either.

A private black-box comparison then withheld source, authored tests and previous
reports from the reviewers, exposing only the original request and the actual
original/candidate CLI input and output. One reviewer independently observed
that the candidate dropped an empty line that previously produced a record,
but treated the preservation requirement as debatable; the other found no
functional counterexample. Neither tried the known single-column case. Appending
these actual reports to the unchanged pre-delivery history still led both Jev
and a fresh-catalog-selected ordinary routing LLM to choose delivery in three
replays each. No corrective work ran in these decision-only replays. This does
not justify adopting blind review as a sufficient safeguard or either router
as a proven solution; no new response gate or default review interface was added.

A diagnostic continuation then started with those two current reviews and the
unchanged original request/source, omitting four older reports only from that
experiment's initial context. Both router types chose implementation in the
decision replay. In the actual native continuation, the implementation role
added tests but did not change the source; new reviewers again accepted the
behavior, and Jev proceeded through delivery, verification and a read-back final
comment. Independent post-run checks still found the empty-value-row loss. Six
working launches made six fresh catalog requests. Dispatching corrective work
is not evidence that the correction happened. Neither deleting history nor
adding the separately tested chronology instruction was adopted into the engine.

The adapter disables dotenv discovery, implicit memory/context-file loading
and native background review. Supply the repository knowledge locations and
actual role permissions in the configured instructions; the agent may read
them within its sandbox. It enables the native terminal/file tools. A custom
native home/config must not silently point those tools at an unrelated host.

**The bridge is not a sandbox.** The configured container/launcher must enforce
filesystem, network and credential access. Separate environment variables alone
do not make a host execution safe. The local experiments enforce these limits
outside the adapter; production packaging is still missing.

The full native final response goes to stdout and all redirected native display
output to stderr. A failed native run returns a nonzero exit and its partial
report/reason, so the chain can decide recovery. Even a cleanup failure retains
the report already obtained. No model response must conform to an answer schema.

On cancellation the runner sends SIGTERM to the harness process group, allowing
native cleanup before escalating that same group to SIGKILL after three seconds.
The bridge forwards SIGTERM/SIGINT to the installed agent's hard interrupt from
a separate thread, then invokes the native background-process registry teardown
and closes the agent. A registry cleanup is needed because native local tools
can be registered under an environment id rather than the agent session id.
This adapter owns one agent/registry per process; it is not a shared gateway.
Cancelled work keeps its partial report but never receives a successful exit.
An interrupted SDK result with no final response remains an empty report, not
a report-format error.

An offline trial used the installed SDK and its actual terminal tool against a
synthetic model endpoint. With the old immediate SIGKILL, a detached terminal
kept writing after cancellation returned. With native teardown connected, both
foreground execution and a background command during a hung model request
stopped writing and their recorded child PIDs disappeared. The isolated role
must be allowed to signal its own tool processes; the trial separately checked
that signaling the test controller outside that sandbox remained forbidden.
These are process-lifecycle observations, not real-model or live-tracker trials.
The accepted-request watch path was also exercised in all four roles
(implement/review/deliver/report), both foreground and background. Each applied
the authorized stop on its first read, retained the stop without marking the
request done, and reaped the native child. With a 40ms poll, cancellation plus
recording took 1.061–1.091 seconds, not a one-poll wall-clock guarantee.
Graceful cleanup is not containment: a hard crash, a non-cooperating harness,
untracked detached children or remote work still require an execution sandbox
or supervisor that owns their lifetime. Earlier external effects are not undone.
Native background tools belong to this invocation; a service intended to outlive
it must be handed to the configured delivery runtime, not left in its scratch
process tree.

Multiple processes for one role run independently with the same prior history.
They do not see one another's current report. A Jev transport/context failure
can use the configured ordinary-LLM router. Role errors and timeouts become
observations for the next decision; there is no attempt-count terminal.

History is saved before dispatch and after return. A restart after an
interrupted action tells the router that the action may already have happened;
it must inspect before repeating it. A temporary result-save failure retries
the save, not the external action. Only one process owns a run directory.

One fresh native-agent trial was deliberately interrupted with SIGTERM after
the tracker fixture stored its first comment, before returning the receipt.
The test harness restarted the same engine command with the unchanged pending
history. The router sent investigation and reporting roles to inspect what had
happened. They read back the existing report without another POST: one stored
comment, identical to the report file. The local artifact passed 13 independent
post-run cases and its generated 10-test suite. This observes reconciliation
after a supplied restart; it does not package a production supervisor, exercise
a machine/power failure, or establish general exactly-once delivery.

At startup, temporary request-read or history-acquisition failures are also
logged and retried every ten seconds until cancellation. No role is dispatched
without the original request and its history store. Once the original is read,
it is retained while storage recovers; a later source edit does not silently
replace it during that wait. Existing ownership and different-request checks
remain in force, and existing pending actions are not cleared by acquisition.
Command/configuration parsing errors still return before this retry loop. An
unrecoverable credential, source or storage problem is not automatically fixed
by waiting; reasons remain visible, not reported as a completed request.

## What the tests establish

### Tracker communication available to configured roles

#### Issue-scoped role access

Set a process's `tracker_access` to `read` or `comment` to give it temporary
access to the assigned issue without giving it the controller's tracker account
key. `read` permits the issue and its comments; `comment` additionally permits
posting ordinary comment content. The assignment comes from the operator's
`--issue`, the accepted watch issue, or explicit `assigned_issue` configuration
for file-based requests. It is never inferred from model prose. Watch mode
assigns each issue separately and rejects a queue-wide `assigned_issue`.

The process receives `TASK_TRACKER_URL`, `TASK_TRACKER_ISSUE`, the public
`TASK_TRACKER_CERT`, and an ephemeral `TASK_TRACKER_KEY`. It can use the existing
tracker tool from its authorized environment:

```sh
tracker --base-url "$TASK_TRACKER_URL" --key-env TASK_TRACKER_KEY --cert-env TASK_TRACKER_CERT --issue "$TASK_TRACKER_ISSUE" post < report.txt
tracker --base-url "$TASK_TRACKER_URL" --key-env TASK_TRACKER_KEY --cert-env TASK_TRACKER_CERT --issue "$TASK_TRACKER_ISSUE" --comment-id 42 comment
```

Each launch gets a separate loopback TLS endpoint, certificate and access key.
The server closes when the role returns or the request is cancelled. TLS trust
is explicit, not disabled; the scoped client does not use ambient HTTP proxies.
Runtime access values are not serialized into operator/watch configuration, and
echoed keys are scrubbed from role history. Do not also map the controller's
tracker key through any role's `secrets` when scoped access is enabled; this
configuration is rejected. Other reserved environment collisions are rejected.

Changing CLI arguments or using HTTP directly does not grant another issue,
project discovery, deletion, status changes or explicit notification recipients.
Posting can still cause the tracker's ordinary comment notifications. A report's
content is not parsed, scored, rewritten or accepted as proof of completion.
API transport limits remain, and ambiguous submissions are not replayed by this
server. Roles can inspect posted comments with their same scoped access.

These permissions **do not isolate a process from the controller, sibling
processes, other files or the network**. The configured OS/container launcher
must do that and keep account credentials/controller state inaccessible. The
loopback address requires the launcher to provide access to the same loopback
network; cross-host/container networking is not configured here. A local mock
tracker and real child-process/CLI trials cover assignment, post/readback,
read-only denial, expiry, cancellation and ambiguous writes. They are not live
service/model or production-isolation acceptance. An already accepted external
write cannot be undone by closing the local endpoint.

An additional offline watch trial used the installed SDK and its real terminal
tool under an OS sandbox. A trusted laboratory launcher added only the newly
issued loopback port to the existing profile before confinement. The actual
tracker CLI posted once, observed a simulated lost receipt, and read the same
comment back. A read-only role's POST and direct HTTP to another issue were
denied; protected controller/sibling fixture files and the direct upstream
socket were inaccessible. A stop during an outstanding POST cancelled the
upstream request on the first stop read, without undoing its already stored
comment; the native child PIDs and scoped listener were gone afterwards.
The model responses and tracker were fixtures. SDK metadata probes returned
recorded 404s, not unreported all-success API traffic. This verifies this local
connection, not live-model reasoning, a packaged production sandbox or arbitrary
escape resistance. The laboratory launcher is not installed by the product.
An SDK attempt to fetch external model metadata also failed under confinement;
this trial does not exercise the engine's live per-launch catalog selection.

#### Direct operator-configured tracker tool

`go build ./cmd/tracker` provides `issues`, `read`, `comments`, `comment`, and `post`
actions. Configure its endpoint, assigned issue and named credential source in
the authorized role's environment.

Direct use has whatever authority the supplied upstream account key grants;
`--issue` alone is not a permission boundary. Prefer the scoped role connection
above for autonomous work.

For example, inside that role's isolation:

```sh
tracker --base-url https://tracker.example/api/v2 --key-env REPORT_KEY --issue EXAMPLE-1 post < report.txt
tracker --base-url https://tracker.example/api/v2 --key-env REPORT_KEY --issue EXAMPLE-1 --comment-id 42 comment
tracker --base-url https://tracker.example/api/v2 --key-env REPORT_KEY --issue EXAMPLE-1 --after-id 0 comments
```

The reporting agent invokes this as a tool with its prepared report, not as the
role harness itself: harness stdin contains the assignment and earlier reports,
which must not accidentally be posted as the final comment.

Post stdin is sent unchanged as the API's form `content`; there is no success
template, answer schema or completion mark. The native comment receipt goes to
stdout. `comment` reads that API id back, and `comments` retrieves all pages
after the cursor, including a possible inclusive page boundary without duplicates.
This follows the [comment API](https://developer.nulab.com/docs/backlog/api/2/add-comment/).

A failed or unreadable submission response is ambiguous: the command reports
the reason and does not automatically retry the POST. The role must inspect the
actual comments before deciding what to do next. Redirects are refused and
credential values are redacted from transport errors. These commands do not
certify the report, mark the request complete, grant permission to post, or
enforce issue-level authorization beyond the configured service credential.
The launcher must provide only the authorized capabilities. No live-tracker
posting or production restart supervision has been validated yet.

The new command is tested as a real subprocess against a local TLS tracker,
including verbatim prose publication and readback. This is not live delivery.

For read-only issue discovery, provide an explicit project rather than an issue:

```sh
tracker --base-url https://tracker.example/api/v2 --key-env INTAKE_KEY --project-id 17 issues
```

This follows the [issue-list API](https://developer.nulab.com/docs/backlog/api/2/get-issue-list/):
it requests pages of 100, ordered by creation, and returns the native records
with original descriptions and unknown metadata intact. Every invocation starts
a new scan. An unreadable/failed page, repeated issue or out-of-project response
returns a reason and no partial stdout list. The service's offset pagination is
not an atomic snapshot; the polling collector rescans to pick up changes
during pagination. Empty projects return an empty array.

This reads existing issues as well as new ones. Listing is not claiming or
authorizing their execution. The opt-in `--watch` collector described above
connects explicit intake scope to durable per-issue scheduling and logical
working directories; actual permission isolation is still the launcher's job.
The command does not reuse the old reception contracts or ask a model to reject
requests based on their prose format. Tests include multi-page local TLS reads
through a real CLI child and a failed second page with no partial output.

An isolated native-agent run has now read an issue through the public intake
path, performed work and two review rounds, delivered a local artifact, and
used this command to post its prose report to a tracker fixture. The first
submission stored the comment but returned HTTP 503. The reporting agent
inspected the actual comments, confirmed the unchanged report was present,
and did not repeat the POST. One comment was stored. This establishes that
observed recovery in a mock service, not real-service permissions, restart
reconciliation, or general exactly-once delivery.

The router nevertheless chose done with an unmet preservation requirement:
the artifact still dropped empty-value rows in four independently reproduced
cases. Its generated tests and the first ten examples passed. Successful
publication and readback did not make that request complete.

### Automated checks

```sh
GOMAXPROCS=2 go test -p 1 -count=1 ./...
GOMAXPROCS=2 go test -race -p 1 -count=1 ./...
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s harnesses -p 'test_*.py'
```

Tests use API fixtures, local TLS servers, and real local child processes. They
cover original prose delivery, preservation of early objections, free-form and
failed role outputs, transient storage failures inside the chain, cancellation,
restart observations, independent reviewers, credential isolation/redaction,
redirect refusal, and routing fallback. They do not establish real model
judgment or real tracker-to-production delivery. A fixture choosing `done` is
only an executable-wiring test.

## Isolated live observations

Two fresh live-model runs used the real engine run path and an installed native
agent's tool loop to repair a small CSV CLI, obtain two independent model reviews,
build a real zipapp, execute that artifact, and write the requested report.
Neither run needed manual repair or restart after it began. Both routers chose
done, and both artifacts passed 10 separate post-run inputs. However, a further
comparison with the original exposed a regression in both artifacts: a plain
CSV cell with 131,073 characters worked before and now raises a field-limit
error. A review in the ordinary-LLM run even mentioned the regression but
dismissed it as outside scope despite the request to preserve ordinary input.

These are **not accepted completions**. Reaching the report stage and passing
the initial examples did not establish the full requested behavior. Review
context and handling of objections need further live investigation, not a
new answer-format or content-certification gate.

An adversarial-review follow-up supplied the original source read-only. A
reviewer reproduced the regression, the router sent it back for repair, and
the repaired artifact passed the boundary examples. But the router then skipped
independent review of that new implementation before delivery. The routing
instruction now explicitly distinguishes reviews of current work from reviews
before a later change. In captured-state comparisons, each routing model chose
new review in three trials with that instruction; the previous Jev instruction
chose delivery in all three. This is limited prompt evidence, not a guarantee.
A subsequent isolated replay from the repaired implementation actually invoked
both current-work reviewers, then delivered, exercised the artifact and wrote
the final report. Separate execution passed the ten original examples and
the field-limit boundary comparisons. This replay began from a captured
mid-run state; it is not a fresh end-to-end completion. It does not establish
general reliability, nor does it make the earlier premature completions valid.

A later fresh Jev run performed implementation, two reviews, repair, two new
reviews, delivery, artifact execution and reporting without manual intervention.
It still did not satisfy the request: an empty-value row in a single-column CSV
was dropped, whereas the original preserved it. An independent reviewer had
explicitly reported this data loss, but it was treated as a documented behavior
change and the router chose completion. Separate post-run execution reproduced
four such dropped-row cases. Disclosing a deviation did not fulfill the request
to preserve ordinary input. The routing/role comparison remains open.

The matched fresh ordinary-LLM run also repaired the field-limit regression,
re-reviewed the changed work and executed the delivered artifact. It added
verification both before and after the written report, yet still chose done
with the same empty-value row loss in four independently reproduced cases.
Its 28 generated tests and the ten initial post-run examples passed; neither
was enough to establish the preservation requirement. The packaged native
bridge was used for this run, with no manual source repair or restart.

In a separate comparison using the identical captured Jev-run history, Jev
chose done three times and the ordinary LLM chose another verification three
times. The latter instructions still treated disclosed deviations as acceptable;
an extra verification choice is not evidence that the defect would be repaired.
Both routers chose done on an explicitly intended behavior-change control.
The full ordinary-LLM run above demonstrates why counting verification steps
alone is insufficient. Switching routers has not resolved this observed failure.
That run also encountered relay timeouts before recovering, so its latency and
returned usage totals are not a clean model-performance or billing comparison.

Finally, replaying the ordinary-LLM failure history with the same model, prompt
and 4,000-token ceiling but `low` versus `max` reasoning still produced done in
all three trials of each setting. Both chose done on the intended-change routing
control; none ended through token exhaustion. Raising this reasoning setting alone did
not fix the observed completion judgment, so it was not adopted as a repair.

A native-review comparison also tested whether earlier role reports caused
the same error. Two models independently reviewed identical read-only code
with the original request, first with those reports and then without them.
Both models accepted the unwanted blank-row behavior in both conditions.
Removing prior reports alone did not resolve the observed failure and was
not adopted. The review copies did not include earlier post-run test inputs;
the original run and source hashes were unchanged. This is one controlled
case, not proof that review context never matters.

A further native comparison retained the original history and clarified that
reviewers cannot authorize a departure from the requester's requirements.
Both reviewers still accepted the unwanted blank-row behavior. They also
accepted a synthetic control that explicitly requested that change. The
wording did not fix the negative case and was not adopted as a demonstrated
repair. The original run and source remained unchanged.

A separate fresh line-search task exercised the same native chain: add opt-in
regular-expression search while preserving literal search, then deliver and
report. It reached two independent reviews, a local artifact and a report
without manual intervention. All 13 post-run artifact cases and the generated
9-test suite passed; packaged source matched the reviewed working source.
The report nevertheless included an inaccurate explanatory regex example.
Passing those functional cases is not proof that every report claim is correct,
and this task does not resolve the earlier CSV failure or demonstrate live
tracker-to-production delivery.

For the first two trials, Jev routing took about six minutes and ordinary-LLM
routing about fourteen. The latter selected extra investigation/verification
steps, and the working roles also took different amounts of time. This is not
evidence of a general speed advantage or an optimal routing architecture.

The run used a test binary whose only substitution was trusting a local TLS
relay certificate; it did not substitute the models, router or agent execution.
This was file-based intake, not the live tracker path.

This is one local task, not a production acceptance or proof of reliable
completion. Earlier integration attempts required manual sandbox/TLS setup
repairs, and a resumed run's report incorrectly claimed the test suite emitted
no stderr. Those attempts are not counted as clean unattended completions.
The native adapter is now included here, but the experiment launcher and role
sandbox are not packaged as a supported deployment. No existing production
entry point was replaced.

## Still missing before production use

- The scoped polling prototype needs a genuinely isolated per-request checkout,
  a visible stop acknowledgment/final report, production supervision and live
  delivery integration. Its logical
  directories are not a permission boundary. Final-comment posting/readback has
  only been exercised by native agents against a tracker fixture.
- Resolve the observed premature completion: reviewers must compare
  relevant original behavior and preserve objections against the actual request.
  Broader validation still needs misleading reports, repeated failures and restart.
- Per-launch fresh-catalog selection is connected and has limited live evidence
  above. Optimal task value, recovery from every provider/credit outage and
  automatic production setup are not established. No saved snapshot is called
  current when retrieval fails.
- Production isolation. Restricting child environment variables does not
  sandbox filesystem/network access; the configured launcher must enforce the
  role's actual permissions. Do not run unconfined commands with broad keys.
- Startup request/history acquisition now retries, but command/configuration
  parsing still returns, a changed source can conflict with an existing run,
  and an external supervisor for process death is not packaged. Process output
  is buffered in memory; production resource behavior has not been exercised.

Jev routing is one comparison candidate, not a settled architecture decision.
