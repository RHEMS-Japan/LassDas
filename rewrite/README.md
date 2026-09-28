# Role-chain experiment

Start with [the handoff and startup guide](START.md) and the complete
[editable operator example](examples/operator.json). Both ship in the local
bundle. The example's intake scope is intentionally unset; it does not activate
an existing project or provision delivery/network permissions.

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

A local build bundle and a Linux role launcher are now available. See
[runtime setup and remaining prerequisites](RUNTIME.md). These do not activate
intake or change the existing production entry point.

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

The bridge defaults the SDK's optional file-mutation footer off through
`HERMES_FILE_MUTATION_VERIFIER=0`; an explicit operator value is respected.
The installed SDK can retain a failed `patch` after a later successful terminal
write and append a false "not modified" claim to the final response. A real-SDK
local fixture reproduced that sequence. Failed tool attempts and their original
reason previews still go to stderr, identified as earlier attempts, not proof
of the current file state. The bridge does not strip footer-like text from
answers or edit the SDK installation. It forwards the SDK's `final_response`
unchanged; the SDK itself may normalize provider text before returning it.

The already-redacted process diagnostics also reach both routing APIs and the
next working role, separately from the role's answer. A successful process exit
can still contain an earlier failed tool operation. Previously those diagnostics
were saved but omitted from both handoffs, so retaining a reason on stderr did
not let the next role use it. Forwarding observations does not classify a role's
answer or turn an earlier tool failure into a verdict about the current work.
This increases routing context; it is not a solution for unbounded histories
or evidence that models will correctly resolve every reported contradiction.

A role's processes need not all be model harnesses. An operator can also
configure an existing project test or deployed-service check as an ordinary
process, leaving its `model_env` unset and granting only the permissions that
command needs. Processes in the same role run in parallel; all their actual
stdout, stderr and exit observations reach the next routing decision. For
example, a verification role can return both an agent's report and the project
command's failure, without depending on the agent to repeat that failure in
its prose. The routing model still chooses investigation, repair or other work;
the command is not a new completion gate. Checks are fallible project inputs,
not permission to narrow the request, and passing them alone does not establish
completion. Configuring such a command does not solve discovering missing tests.

Routing and model-selection inference share a five-minute request timeout,
not a task deadline or attempt limit. The former thirty-second limit discarded
valid completions taking 35–40 seconds and repeatedly selected the same work.
A local TLS response delayed 31 seconds now reaches the caller. Earlier parent
deadlines and cancellation still interrupt response-header and response-body
waits; a timeout goes to the existing recovery loop, never to a completion claim.

Unsuccessful routing calls are also retained in the ordinary runtime history,
not just printed to a log. The next routing attempt, fresh model selection and
working role can see the actual failure; a dynamically selected routing endpoint
is named in its error. If both configured routing services fail, both reasons
survive. A successful alternative still proceeds normally, and caller cancellation
does not become a model failure or authorize more work. This does not blacklist a
model or force a recovery choice: the selection model decides from a fresh catalog
and the observations. Tests demonstrate the handoff and persisted recovery, not
that a real model will always choose an effective alternative. Long-running history
growth and provider context limits remain unresolved.

The top-level `instructions` are the operator's shared workflow context and
reach every working role as well as the router, alongside each process's own
instructions and the unchanged original request. For example, a reviewer needs
to know that a separately configured reporting role can post to the assigned
issue; its own lack of that tool does not make reporting impossible. This
shares context, not permissions: commands, scoped tools, credentials and OS
isolation stay separately configured. Reports and repository text remain
observations, not authority to override the operator's workflow or the request.
The router also receives each available role's process instructions and
engine-issued tracker access from that same configuration, not only its title.
Commands, directories, environment values and credential mappings are not
included in this description. These facts help assignment; they neither grant
access nor guarantee that a model will interpret them correctly.

### Configured action connections

An operator can supply `workflow` to make required connections explicit instead
of relying on prose to prevent skipped work. Names refer to configured roles:

```json
"workflow": {
  "start": ["implement"],
  "after": {
    "implement": ["review"],
    "review": ["implement", "deliver"],
    "deliver": ["verify"],
    "verify": ["implement", "report"],
    "report": ["done"]
  },
  "recover": {
    "implement": ["implement"],
    "review": ["review", "implement"],
    "deliver": ["verify"],
    "verify": ["verify", "implement"],
    "report": ["verify"]
  }
}
```

Both routing APIs receive only currently connected choices. Dispatch also
refuses an unconnected action without ending the request. The model still
interprets reports, chooses repair or progression, and decides when the work
is actually complete; working answers have no new schema or content check.
Role names and connections are project settings, not built-in stages. To review
a report before publication, configure separate drafting, reviewing and posting
actions with appropriate permissions. One reporting action that can both rewrite
and post does not establish pre-publication review of what was actually posted.

All processes finish before the next decision. A process error, missing results,
or an interrupted pending action uses `recover`, which cannot select `done`.
An exit-zero process with empty stdout is not rejected. Recovery has no attempt
cap or invented failure terminal; it may still need resources or authority that
the system cannot supply. On interruption, the next role must inspect uncertain
external effects before repeating a write. Saved-result retries do not repeat
the executed action. This is ordinary runtime recovery, not approval of output.

The accepted run saves its connections and current position before dispatch;
restart keeps these even if the operator file supplies different connections.
Other operator configuration is not frozen. Missing/unconfigured connections
are startup errors, not failed deliveries. A new graph cannot silently attach
to an already-started free-routing history. Without `workflow`, free routing
remains available for comparison, with **no enforced ordering**.

The example only establishes connections: reviewers can still make a wrong
judgment, tools can fail, and delivery can remain incomplete. Neither a traversed
graph nor passing tests proves unattended overnight completion. Local tests
exercise skipped dispatch, error recovery, persisted interruption, unchanged
prose, both API protocols and actual child processes; these are not live-model
or production acceptance results.

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

Optional `intake.issue_ids` narrows new discovery to the listed positive native
issue IDs, still within the same project and starting time. Omitted or empty
means all new issues in that project. This operator setting permits a scoped
rollout without starting unrelated tickets; it is not an input format or an
assessment of a request. Removing an ID does not abandon already accepted work:
those queue records continue to resume and the existing stop mechanism applies.

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
The bridge also seeds the native terminal's existing working-directory setting
from `TASK_WORKSPACE` unless explicitly supplied. The tested SDK otherwise began
terminal commands in its private home despite the correct process directory;
earlier laboratory launchers supplied this setting themselves.

**These are logical working directories, not filesystem/network isolation.**
An authorized launcher must confine access to other requests, controller state,
credentials and delivery targets before live untrusted work is enabled.
Production packaging and real tracker-to-production operation are not
implemented by this collector. Its tests use fixture APIs and actual local child
processes, not live-model judgments or a production tracker.

### Settling the request at the entrance

The shipped example starts at `elicit`, which settles what a request asks for
before the work is handed over. It reads the original request, the prepared
checkout and the operator instructions, then writes in ordinary prose the
requirements as it will carry them out, the points it decided itself with their
reasons, the points only the requester can decide with two to four choices
each, and any permission or credential the work needs but was not given. Its
connected actions are `elicit`, `ask_requester` and `investigate`, so settling
further, putting the open points to the requester and proceeding are ordinary
choices among connected role names. There is no new decision mechanism and no
check of what the role wrote.

The standard it settles against, in the words the decision model and the
entrance actions are both given: can this request be carried to a delivered,
verified result by morning with nobody available to answer? Points the roles
can settle from the request, the repository or the operator instructions are
settled and written down with their reason. Whatever is left goes to the
requester straight away, before anything is investigated or built, in one
comment listing each undecided point with two to four choices so it can be
answered in a single reply. A general request for clarification is not a
question.

The choice at the entrance is deliberately one-sided. Proceeding with an open
point costs a night's work while asking costs one reply, so `investigate` is
expected only when every point that only the requester could decide is absent
or already answered and the settled requirements state the completion condition
to be held to; when in doubt the expected choice is `ask_requester`, and
`investigate` is never a way to find out what was wanted. That is wording and
connections only. Nothing measures a confidence level, counts open points or
inspects what the role wrote, so a model that ignores the standard still
proceeds and no test here can tell you it will not.

`ask_requester` posts one comment carrying the requester-only points and reads
the stored text back. It has comment access and no workspace write permission,
and neither it nor `elicit` is connected to `done`: the entrance cannot end a
request at a person.

```json
"intake": {
  "question_role": "ask_requester"
}
```

`intake.question_role` names the configured role whose successful run waits for
a person. It must name an existing role with a comment-capable process, which
is checked before any work is accepted. Without the setting nothing waits.

A successful run of that role holds the request. The run history records that
it is waiting, and the collector records in `queue/jobs/<id>/question.json` how
far that issue's comments had gone at the moment of the question. The request
is then skipped until the issue's creator, or an operator listed in
`intake.stop_user_ids`, posts a comment after that point. That comment's text
is appended to the history as the requester's own words, exactly as posted, the
hold is cleared, and the record is kept as `answer-<comment id>.json` so the
same comment cannot be read as a second answer. The next decision sees the
answer and only the actions connected after the question, which in the example
returns to `elicit`.

The stop check runs first, so a comment whose first nonblank line is `停止` is
a stop, never an answer. An unsuccessful question recovers through the
configured `recover` connections instead of waiting for a reply to a question
that was never asked. A `--issue`/`--request` run has nobody watching the issue
for an answer, so it exits non-zero saying that `--watch` resumes the request.

What this does not do: the text of an answer is never checked, so a reply that
does not actually answer the question simply reaches the next decision like any
other report. Only the first comment after the recorded point becomes the
answer; further comments are not appended, and a later question moves the point
past them. While a request waits, each poll reads that issue's comments inside
the collector loop, so a slow tracker delays the loop by up to one interval for
every waiting request. Nothing notifies the requester beyond the posted comment
itself, and an unanswered question waits indefinitely unless someone stops it.

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

### What the requester is told at night

A request filed at eleven and stopped at two by an outage used to say nothing
at all: the chain kept retrying, and the only trace was a log nobody was
reading. Three fixed notices close that silence. The controller writes every
one of them, always in the same words. No model composes them, none of them
judges a role's answer, and none of them ends a request: the work carries on or
resumes by itself in every case.

Each notice is recorded in `queue/jobs/<id>/notices.json` before it is
submitted and marked afterwards, with the same atomic write the other runtime
files use. A submission that fails is retried on later ticks. Because the
wording is fixed and the controller's own, a retry first reads the issue's
comments back and matches that exact text, so an ambiguous answer from the
tracker cannot turn into two identical comments.

**After a restart.** When the queue picks up a request whose history holds an
interrupted action or an unfinished recovery, it posts:

> 自動処理は再起動後に同じ依頼を続けています。直前の工程は途中で止まった可能性があるため、確認してから進めます。

A clean start says nothing. The same request does not say it again inside 30
minutes, even across further restarts, so a crash loop cannot fill the issue
with one sentence.

**When the shared model key runs out.** This is the one failure no role can
recover from: no investigation, redesign or handoff puts money back on a key.

```json
"intake": {
  "min_model_credit": 5,
  "model_credit_url": "https://openrouter.ai/api/v1/key"
}
```

`min_model_credit` is a US dollar floor. Absent or zero asks the provider
nothing, which is what the shipped example does until an operator chooses a
figure. When it is set, the remaining balance on the key named by
`router.decision.key_env` is read before a request is launched and on every
tick of a running one. `model_credit_url` defaults to the OpenRouter key
endpoint above and may point at a gateway instead; it must be an HTTPS URL
without credentials or a query. The answer's `limit_remaining` is read as the
balance, and a null there means the key has no limit, which never pauses
anything. Below the floor, the running role is stopped exactly as an authorized
stop stops it, nothing new is launched, and the requester is told once:

> 自動処理を一時停止しました。モデル利用枠の残りが設定の下限を下回ったためです。枠が戻り次第、自動で再開します（人の操作は不要です）。

The balance keeps being read each tick. When it is back above the floor the
request is launched again and says so once:

> モデル利用枠が回復したため、自動処理を再開しました。

The pause and the recovery alternate, so each episode gets one line of each. An
endpoint that cannot be read is not evidence of an empty budget: it never
pauses work and never posts, and the reason goes to the log with the
credential value removed.

**When nothing has completed for a long time.** `intake.stall_notice_minutes`
is how long a running request may go without a completed step before the
requester hears about it. Absent means 90 minutes and zero switches it off. The
window is measured from the last history entry that finished without an error,
so any successful role output inside it keeps the request quiet. Past the
window:

> 自動処理は続いていますが、過去 <n> 分間は工程が完了していません（直近の失敗: <直近の失敗の1行目>）。復旧を試し続けており、人の操作は不要です。

The quoted failure is the first nonblank line of the most recent error, with
every configured credential value replaced by `[credential]` and the result cut
to 200 characters. The notice repeats at most once per six hours per request.
It is a notice and nothing else: routing, recovery and the request's goal are
untouched by it.

All three go out through the controller's own tracker credential, the same one
the stop report uses. No role is given the means to post them.

What this does not do: these notices say that the machinery is still trying,
not that it will succeed. They are posted from the collector loop, so a slow
tracker delays that loop while one is being submitted. A request that is
waiting for the requester's answer is not stalled and says nothing further.
Nothing here notices a crash loop that never reaches the collector at all, a
full disk, or a provider that answers quickly and uselessly. The budget reader
has been exercised against a local fixture of the documented response shape,
not against a live provider key.

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

With `model_selection` configured, ordinary-LLM routing also selects from a
fresh catalog before every routing decision. This applies both to
`router.mode=llm` and to a configured chat alternative after the decision
service is unavailable. Set `router.llm.url` and `key_env` for that transport;
its `model` is not used as a pinned or failure fallback. Selection failure
returns to the existing recovery loop. The selected routing model is logged;
the original request, responsibilities and prose history remain unchanged.
Without `model_selection`, `router.llm.model` remains the operator's explicit
model choice. The decision model used to select an endpoint is separate from
the selected model that assigns work; neither certifies a role's answer.

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

### Naming one model instead of selecting

`model_selection.fixed` names a single endpoint id, such as
`publisher/model-name`. Every process with a `model_env` then receives that id
for every launch: no catalog is fetched, no gateway list is fetched and the
selector is never asked. Configured `authors` stay in the file and are ignored
while it is set; an empty or blank value reads as no fixed model at all, and an
id without a publisher is refused with the rest of the configuration. Routing
uses the same named model when a routing LLM is selected.

A named model cannot be excluded from its own group, so the peer separation
that gives a parallel review group two publishers does not apply: both
reviewers run that one model, and their reports are no longer independent in
that sense. This is an experiment switch for comparing one strong model against
per-launch selection among the configured publishers. It is not a
recommendation, and neither arrangement is established here as the better one.
With a gateway configured, the named id is invoked through it exactly as a
selected one is.

### Invoking through a gateway

Selection and invocation can use different accounts. The optional
`model_selection.gateway` names an OpenAI-compatible gateway that serves the
same catalog models under prefixed ids:

```json
"gateway": {
  "models_url": "https://gateway.example.invalid/v1/models",
  "key_env": "GATEWAY_API_KEY",
  "prefix": "openrouter/"
}
```

With it configured, every model chosen for a launch is reached through that
gateway, so the working roles' and the routing LLM's token spend is invoiced to
the gateway account instead of the catalog account. Point each process's
`env.OPENROUTER_BASE_URL` and `secrets.OPENROUTER_API_KEY`, and
`router.llm.url`/`key_env`, at the same gateway.
`examples/operator-gateway.json` differs from `examples/operator.json` in
exactly those places and in nothing else.

The decision service stays on OpenRouter. The gateway measured here answers
chat completions but not the decisions API (HTTP 405), so
`model_selection.judge` and `router.decision` keep their OpenRouter URL and
`MODEL_API_KEY`, and their spend stays on that account.

Selection also keeps reading the public OpenRouter catalog. The gateway list
publishes ids, dates and an owner, without prices or `supported_parameters`,
which is not enough to decide eligibility or value. That list is used only to
drop eligible ids the gateway does not serve, and it is fetched again for every
selection under the same bounds as the public catalog: one bounded request, no
redirect, no partial list, no saved snapshot. It is the one catalog request
that carries a credential, so its endpoint must be HTTPS. When the list is
unavailable, that selection attempt is unavailable and returns to the existing
recovery loop; nothing falls back to invoking the un-prefixed id, which would
bill the account the operator is moving away from.

`prefix + id` is a route to the same model, not a different model and not a
mark of quality. The judge is offered bare catalog ids, publisher separation in
a parallel review group still compares publishers rather than the gateway name,
and the runtime history records the chosen id as `model` with the route beside
it as `model_prefix`. A gateway configured without `models_url`, `key_env` or
`prefix` is refused before any request is accepted, because a half-configured
one would quietly keep invoking the account being moved away from. Without
`model_selection.gateway`, none of this applies and invocation is unchanged.

### Optional Git workspace preparation

Watch mode already binds each process to its request's workspace and agent home.
For a Git project, the configured command can use `harnesses/git_workspace.py`
before its existing isolation launcher; no extra engine stage is needed:

```json
{
  "command": ["python3", "/opt/engine/harnesses/git_workspace.py", "--", "/opt/operator/isolated-role-launcher"],
  "env": {
    "TASK_REPOSITORY": "https://git.example/project.git",
    "TASK_BRANCH": "main"
  }
}
```

The paths are operator-owned examples, not installed executables. Keep the
process `directory` empty or `.` so the wrapper starts in the watch-created
job workspace. `TASK_REPOSITORY` is an operator-approved URL or absolute local
repository path, never an address extracted from a model answer. `TASK_BRANCH`
optionally selects an existing branch or tag; otherwise the remote default is
used. Authentication, when needed, must be provided explicitly and scoped to
that source, not embedded in its URL. Personal/system Git configuration and
clone templates are not loaded, and credential prompting is disabled.
Use this wrapper on roles that need source preparation, not on the stop reporter:
reporting a cancellation must not depend on cloning an unavailable repository.

Only an empty workspace is prepared. Git clones into private sibling staging,
checks out a detached HEAD there when a commit exists, then atomically publishes
the directory. A new, empty remote can reach the first implementation with its
unborn Git HEAD; lack of an initial commit does not block that work.
The local transport optimization is disabled to avoid sharing source objects
through local hardlinks; see [Git clone options](https://git-scm.com/docs/git-clone).
A per-job OS file lock serializes preparation, not the later role work. Both
parallel launchers enter the published directory before starting their roles.
This optional launcher requires POSIX file locking and same-filesystem staging.

Any nonempty workspace is reused unchanged, even when the configured upstream
has moved or is unavailable. There is no fetch, reset, cleaning, repository
certification or inspection of agent-edited Git configuration outside the role
sandbox. Changing the configured source/ref does not replace an active job.
This preserves local commits, dirty edits and untracked progress; it does not
claim that existing work is a valid or undamaged checkout. A failed clone or
checkout never starts the role and its reason reaches the existing chain. A
retry can prepare the still-empty workspace. A killed preparation can leave
unpublished private staging for operator cleanup; it is never treated as work.
Submodule/LFS setup and remote authentication are not automatically provisioned.

The wrapper is **not an isolation boundary**. Its job parent, lock and staging
must be private to the controller. The command after `--` must establish the
actual role's filesystem/network permissions before running its agent. Do not
grant a read-only reviewer broad write access just to let it prepare a checkout.
The original stdin and selected model environment pass through without parsing
or rewriting. There is no model-output gate, new role, or engine setting here.

Real local Git/process tests exercise parallel preparation, separate tickets,
clone failure/retry, interruption before publication, non-cooperating writes,
and preservation of existing work. An actual watch-loop test starts two jobs,
cancels them mid-work, moves the upstream and makes it unavailable, then resumes
both with the same original request, base and unfinished files. Its tracker and
router are fixtures: this proves the connection, not model quality or delivery.

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

A bounded Linux-container experiment used the public chain/store/process code
with synthetic routing and real direct and detached worker processes. Killing a
controller underneath a surviving init left both workers writing. Making that
same controller the container's PID 1 instead stopped both after SIGKILL; reopening
the same request history in a fresh container retained the interruption and
finished without old workers continuing. A separate abrupt-exit experiment used
the runtime's `on-failure` restart policy: observed container events were exit 42,
automatic start, and exit 0. No application restart loop or worker-PID ledger was
added. This exercises [PID-namespace lifetime](https://man7.org/linux/man-pages/man7/pid_namespaces.7.html)
and an existing [runtime restart policy](https://docs.docker.com/engine/containers/start-containers-automatically/),
not real-model recovery or exactly-once external writes. It does not package or
activate this supervisor layout or test daemon/machine loss. The separate Linux
role launcher observation below exercises native tools, not crash recovery. In particular,
`on-failure` is not a daemon-restart policy; that operational case remains open.

The optional `harnesses/linux_role.py` has also been exercised on Linux with the
installed native SDK and its real terminal tool, using a synthetic model API.
An implementation process wrote its allowed source file, could not read the
controller-only fixture, and could not write the reporting directory. Separate
implementation/review/delivery/report launches exercised different write grants,
network-off versus explicit network inheritance, private PID namespaces, and
refusal of further user namespaces. A detached local child stopped when its
role exited. Ordinary prose with surrounding text and unknown JSON fields,
stderr and a nonzero exit passed through unchanged. These finite offline checks
do not prove arbitrary escape resistance, live model quality or delivery.
The tested outer runtime needed explicit namespace-enabling diagnostic options;
the bundle does not apply them or provide a production egress policy. Details
and configuration examples are in [RUNTIME.md](RUNTIME.md).

The locally built bundle was then run as the actual engine executable, through
the Linux launcher, installed SDK and real terminal tool. A local TLS router
fixture received the unchanged request, returned a work assignment and observed
the complete report/diagnostics before choosing done. Reopening that completed
history made no further routing or worker call. The native tool performed the
allowed write and failed the two forbidden accesses. The fixture, not an LLM,
chose these actions; this proves executable wiring, not reliable judgment,
tracker intake or production delivery.

A subsequent packaged watch test connected fixture-ticket discovery, actual
Git preparation, native tool execution in implementation/two review seats,
HTTP delivery and the issue-scoped reporting helper. The controller was the
container's actual PID 1. After a comment was stored but its receipt withheld,
SIGQUIT caused exit 2; the existing runtime automatically restarted the same
executable and queue. Original intake survived a changed discovery response,
the old detached worker stopped, and the resumed reporter read back the single
stored comment without reposting or repeating delivery. The source, delivered
bytes and all five completed tool results were observed, not inferred from done.
An earlier version of that observer missed two fixture tool errors; rechecking
the original observation with the corrected check rejects it. This test uses
scripted model/router responses, not a live-model recovery judgment. The runtime
layout and limits are documented in [RUNTIME.md](RUNTIME.md); no live service
configuration was installed or changed.

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

A controlled implementation-only comparison examined what happens *after*
the router chooses implementation. Four isolated copies had the same original
request, source, tests and two actual independent review reports. One received
the captured Jev role-only choice; three received all three previously captured
ordinary-LLM instructions, unchanged, not a selected best instruction. Earlier
history was absent in every arm for this diagnostic; that is not a production
policy to discard history. No post-run counterexample was supplied to a role.
Every launch fetched the public catalog anew and selected the same working
endpoint, with the same reasoning/token settings.

The role-only arm left the source unchanged and still dropped empty-value rows.
All three concrete-instruction arms repaired that row loss. Two also preserved
empty header names; one still lost their values. Separate sandboxed executions
confirmed these differences. All four generated test suites passed, including
the unchanged defective arm. These are four single observations of one captured
state, not a general model ranking or reliable-completion rate.

Instructions were not uniformly sound: one asked the implementer to build at
the delivery path outside its writable scope. The role reported a permission
error there and a temporary build instead, with delivery still to do.
Its native harness also appended a misleading file-mutation warning
about a different relative path, despite the actual source changing. Neither
that warning nor the role's success prose was used as an acceptance result.
The observer-built artifacts were only for checking, not delivery. This trial
did not run fresh independent reviews, delivery or final posting after repair.
It suggests useful task instructions deserve comparison alongside role choice;
it does not overturn the earlier full-chain failures or establish Jev as the
best router. No new working-answer format or content gate was introduced.

Two downstream continuations then retained the six earlier pre-delivery reports,
both captured independent reviews and each selected implementation's actual
report. One candidate still lost values under an empty header; the other matched
the previous observations. Both used fresh per-launch model selection and the
actual Jev chain, without manually choosing its next step. This was a controlled
mid-run splice, not fresh intake or a claim that the earlier implementation had
seen the restored history. Observer inputs and old delivery/report files were
not copied into either workspace.

The defective candidate reached two new reviews, delivery, artifact checks,
reporting, actual fixture-comment readback and done. A reviewer had explicitly
reproduced the lost values but called that input garbage and the difference
non-blocking. The delivered artifact still lost those values in both LF and
CRLF cases, despite all 23 generated tests passing. The published completion
claim was therefore wrong. An empty header name is not excluded by the field
grammar in [RFC 4180 section 2](https://www.rfc-editor.org/rfc/rfc4180#section-2);
that reference was checked by the observer, not supplied during the run.

The other candidate reached two review pairs, delivery, artifact verification,
reporting and exact fixture-comment readback. Its actual delivery matched all
25 separate post-run checks, and its 24 generated tests passed. It performed
one additional review pair on unchanged source before proceeding; the cause
of that extra choice is not established. Neither continuation needed manual
repair or restart after launch. These results distinguish getting work delivered
from judging it complete correctly: the same chain called both done. The positive
result does not resolve the negative case, establish a reliable-completion rate,
or prove every report assertion or real tracker-to-production operation.

An additional read-only diagnostic dispatched the existing investigation role
on each candidate immediately after its first current review pair. It retained
the original request and all eleven actual reports, without injecting an
expected counterexample or a repair instruction. Each investigator was freshly
selected from the public catalog and ran the actual native harness. Both
recommended delivery. The defective case inherited the review's dismissal of
lost values as garbage input instead of proposing a repair; separate sandboxed
checks still reproduced that loss. The other candidate continued to match the
25 observed checks. Neither source was edited and no delivery or posting ran
in this diagnostic. Thus manually adding the existing investigation role did
not resolve the observed false acceptance. It does not establish autonomous
selection of investigation, justify a mandatory extra stage, or show that all
possible investigative assignments fail. No such stage was added.

A matched routing replay used those same original requests and first eleven
reports, including the first current review pair, with no new instruction,
counterexample or edited report. Jev and the ordinary-LLM router were each
called three times per candidate. Every ordinary-LLM choice fetched the public
catalog anew. On the defective candidate both routers chose delivery all three
times. The ordinary LLM supplied concrete build instructions but did not send
the observed lost values for repair. On the comparison candidate it chose
delivery three times; Jev chose another review once and delivery twice.
These are routing observations, not six independent work completions. Replacing
choice-only routing with instruction-bearing routing did not resolve this
captured false acceptance. No router default was changed.

A subsequent fresh issue-intake trial started with an empty workspace and a
new synthetic Git repository. The configured public Git launcher performed
the initial clone before the native implementation process entered its sandbox;
later roles reused the same work. SDK/startup probes used a separate workspace.
The actual issue-fetch path, implementation, two independent reviewers,
delivery and report ran without operator repair. Five role processes fetched
five fresh model catalogs. This was one explicit `--issue` invocation against
the tracker fixture, not automatic `--watch` discovery or live production.

It reached a real local zipapp and a final comment with exact list readback
in about six minutes, but still falsely claimed completion. Post-run execution
of the delivered artifact (not a rebuild) matched 16 of 25 checks: five empty-row
cases and two empty-header cases lost values, and two previously readable
large-cell inputs now raised the CSV library's default-size error. All 13
implementation-authored tests passed. Both reviewers had accepted the changed
empty-row behavior; neither caught the large-cell regression. The final report
claimed no functional work remained. Source preparation/reuse therefore has
live-model integration evidence, not evidence of reliable semantic completion.
The original synthetic repository and the checked source/artifact/report were
unchanged by the observer. No expected counterexample was fed back to the run.

A controlled early-verification trial then invoked the existing verifier once
before implementation. It saw the original request/source, not a changed
implementation or its tests/approvals, and wrote runnable checks in a shared
directory writable only by verification. Subsequent roles could read/run them;
all later actions were chosen by the existing router. This fixed first dispatch
was an experimental intervention, not autonomous selection of a new stage.
The checks were ordinary project code, not a format or gate for LLM answers.

The verifier created 14 checks, and later roles actually reused them against
source and delivery. A separate sandboxed observer found those same checks
passed both the previously defective delivery and the comparison delivery.
The new trial also ended falsely complete: its delivered artifact passed all
14 shared checks and all 13 project tests, but still failed the same nine of
the 25 independent post-run cases. Both reviewers again waived changed empty-row
behavior. Creating checks before implementation did not make them sufficient.
The first verification report even described its throwaway reference as correct
because it passed those finite checks; that claim is not acceptance evidence.

The run recovered from one relay timeout and later investigated a misleading
native file-write warning. It confirmed the real report file and the already
posted comment without a duplicate post. Those recovery observations do not
resolve the defective artifact or its inaccurate completion claim. No mandatory
early-verification stage or new output certificate was adopted.

A subsequent controlled continuation asked the existing reviewers to build
plausible incorrect implementations that pass those same checks. They produced
runnable whitespace and ragged-row counterexamples without being supplied the
known failing inputs. During later reviews they also found empty-header data
loss and the standard library's field-size limit. The working roles repaired
these defects and published a real zipapp. Independent execution of its bytes
matched all 25 previously observed cases, plus five inputs from the reviewers;
the generated project suite had 22 passing tests. One fixture comment was posted
and its readback matched the report. The independent 14 checks stayed unchanged:
their passing result alone still does not establish adequate coverage.

This is useful evidence of discovery and repair, **not clean unattended
acceptance**. The initial test-quality review was prescribed and reused an
earlier verifier's actual checks/report. An observation deadline interrupted a
later review; an operator resumed the same work, original request, reports and
fixture state, with a longer private relay deadline. Crucially, after the last
source correction the router selected delivery without another independent
review of the edited work. The finite artifact checks do not establish review
convergence on that final version or reliable behavior on other requests.

There was also an observation artifact: two controller-generated test inputs
were left in the workspace at interruption and became visible on resumption.
They were controller artifacts, not worker changes, and contained no field-size
example; nevertheless the run had observer influence. The private
observer now executes an unchanged copy of delivered bytes outside the working
directory; both missing- and existing-delivery cases leave that directory alone.
No mandatory extra stage, new role, answer certificate or production policy was
adopted from this one controlled continuation.

The actual history immediately after that final implementation was replayed
without changing its request, reports, role responsibilities or operator
instructions. Jev selected delivery in all three decisions; an ordinary routing
LLM, selected from a fresh catalog each time, selected another independent review
in all three. The existing routing instruction already required review after
changes. A synthetic completed-history control led both to finish, but neither
that control nor one actual history establishes general routing superiority.

A subsequent fresh ordinary-LLM-led issue-intake trial started from the original
Git fixture, with no copied verifier, prescribed first assignment, operator
resume or observer input. It went through investigation, implementation, two
independent reviewers, delivery, report, verification, report and completion.
Two routing timeouts recovered automatically through fresh selection. Every
routing/working invocation used the current catalog:18 fetches for10 routing
attempts and8 working-role launches. The final report was posted and read back;
the later updated report was a second, different comment, not a duplicate post.

However, the delivered bytes passed only16/25 known observations:10/10 basic,
3/8 empty-row,2/4 empty-header and1/3 large-cell cases. Generated tests were10/10.
Both reviewers observed behavior changes and dismissed them as old bugs; the
verifier also accepted the work. The final completion claim was therefore still
wrong. The run took about18 minutes without human recovery, but **unattended
termination was not unattended fulfillment**. Choosing a different router did
not solve this review-quality failure. The earlier manually assisted25/25 run
and this fresh16/25 run must not be conflated. No production entry point was
changed or completion certificate introduced.

A subsequent matched, read-only review comparison kept that candidate, original
request and two pre-review reports unchanged. Both the existing responsibility
and an added request to demonstrate weaknesses in the unchanged tests led both
reviewers to approve the defective candidate. The added responsibility did
produce test-escaping examples: one retained example lost the final row without
a final newline; a reconstruction of another reviewer's reported change lost
leading spaces. Both passed the ten project tests, but neither new example was
a defect of the actual candidate. Finding weak tests did not establish that
these reviewers would reject the candidate's existing defects. No mandatory
review stage was adopted from this result.

After repairing delivery of shared operator instructions to every role, another
fresh ordinary-LLM trial completed investigation, implementation, two reviews,
delivery, verification and a single report with exact fixture-comment readback.
It took about nine minutes with no operator repair or restart. The request,
operator workflow and role responsibilities were unchanged; fourteen fresh
catalog fetches covered seven routing calls and seven working-role launches.
The actual delivery still passed only16/25 known observations while its thirteen
generated tests passed. Empty cells/header values were lost and ordinary large
cells raised an error. The reviewers and verifier again waived an observed
behavior change, and the final report claimed fulfillment. Correct context
wiring did not establish correct completion judgment.

A further fresh trial asked the existing verifier to develop checks before
implementation and the existing reviewers to challenge those checks. The real
router chose every assignment; no earlier solution or known counterexample was
supplied. It completed in about29 minutes, including recovery from two selection
HTTP520s, and posted one report with exact fixture readback. Its fourteen project
tests and twelve model-created checks passed, but the actual delivery again
passed only16/25 independent observations. The check notes waived preservation
of empty rows, later roles accepted that waiver, and large-cell crashes remained
undetected. Earlier checks did not establish fulfillment either; this workflow
is not adopted as a mandatory stage. The original request remains unmet.

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

The entrance described above has never run with a live model or a real
tracker. Its tests use fixture APIs, fixture role programs and an actual local
collector: they establish that the hold, the boundary, the stop precedence and
the resume behave as described, not that a model actually settles a request,
asks a useful question, or stops asking once it has an answer. Whether a real
requester's reply is enough to carry a request through unattended is unmeasured.

A separate JSON-configuration merge task reached delivery and one final fixture
comment with exact readback after a manual resume. Its first 30-minute observation
window interrupted review; the resumed run finished in about 14 minutes. This is
not an uninterrupted overnight acceptance. The actual archive passed 38 declared
independent observations and 47 generated tests. Both reviewers identified a deep
merge recursion regression and the implementation role repaired it before
delivery. Independent runs of the delivered archive confirmed the reported
995/1500/2000-depth cases now work. However, a separately observed input containing
`1e999` still produces `Infinity` with a successful exit, which is not JSON.
The original implementation also had this defect; the requested valid JSON
output remains unmet despite the final report's "no work remains" claim.
The 38-case result does not establish complete fulfillment or repair the earlier
CSV failures. This trial used the old timeout and SDK-footer behavior; the
subsequent clean trial described below used those fixes.

A fresh run of the same configuration task, with no source repair, supplied
counterexample or manual restart, reached delivery and an exactly read-back
fixture comment in about 52 minutes. Independent execution of its actual archive
passed the same 38 declared cases and its 51 generated tests. Three separate
concurrent-reader observations saw only the old and final output, not a partial
file; finite sampling is not a guarantee for every schedule. Independent reviewers
found numeric overflow, and the chain repaired it before delivery: `1e999` now
returns an error instead of emitting non-JSON `Infinity` successfully.

This is not uniform improvement or proof of the full goal. The delivered program
now rejects object depths 1500 and 2000 that the original accepts; depth 995 still
matches. Its final report discloses the depth limit. The request did not explicitly
require unlimited depth, so neither unlimited-input support nor preservation of
all original behavior has been established. Three review pairs also added work
on traceback presentation beyond the original error-reporting request. One
uninterrupted fixture delivery does not resolve the earlier CSV false completion,
prove optimal routing/model selection, or validate live overnight operation.

- The optional Git launcher prepares per-request checkouts, but the scoped
  polling prototype still needs enforced filesystem/network isolation, a visible
  stop acknowledgment/final report, production supervision and live
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
