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
# Have an edited configuration read and checked; nothing is started:
go run ./cmd/engine --config operator.json --check
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

Routing and model-selection inference share a request timeout of sixty minutes
(`timeout_minutes` on the service names another; zero means that default, unlike
a process's `timeout_minutes`, where zero means none), a guard against a
connection that never answers rather than a limit on the work,
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

Optional `intake.category_ids` narrows new discovery to issues that carry at
least one of the listed tracker categories. This is how a project shared with
people's own tickets hands the runtime only what was marked for it: the
category is set when the issue is filed, or added later, and the issue is
accepted on the next scan after it carries the category. An issue without one
is left alone and looked at again on every scan. Like the allowlist, this is
operator scope, not an input format: nothing about the wording of a request is
inspected, and removing a category from the setting or from an issue does not
abandon work that was already accepted. Omitted or empty means no narrowing by
category. With both settings an issue must be on the allowlist and carry a
category.

Use a category that exists only for this purpose. The runtime compares the
category's number and nothing else, so a category people already use for
something else hands over every issue that carries it and was created at or
after the starting time, all at once on the first scan. It also does not look
at who set the category: anyone who may edit the issue's categories hands it
over, while the requester remains the account that filed the issue. That
account, and the ones in `intake.stop_user_ids`, are the only ones whose stop
or answer the runtime follows. `intake.category_on_accept`, described below, is
a different setting: it marks what the runtime accepted and narrows nothing.

The runtime refuses a configuration it cannot take as written, instead of
running on something else. A key it does not know is named with its place,
`unknown key "category_id" in intake`: `category_id` for `category_ids` would
otherwise be no filter at all. A key's letter case must be the documented one.
A key written twice in one object is refused too, since the later one would
win without a word. A runtime that refuses its configuration does not start,
and says why on standard error and, with `--log-file`, in that file, which is
what the status page shows.

`--check` has the configuration read and checked without starting anything:

```sh
go run ./cmd/engine --config operator.json --check
```

It runs the checks of a `--watch` start, so a configuration the watch would
refuse is refused here in the same words, and prints which new issues the
watch would take up, for example `intake: project 17, issues created at or after
2026-01-02T00:00:00Z; only issues carrying one of the categories [77]` or
`...; every such issue is accepted`, then `the configuration is accepted;
nothing was started`, and exits. It creates no queue and no log file and makes
no request to any service; it takes `--config` and nothing else. Run it on an
edited configuration before the runtime is restarted with it. The watch prints
the same line once at start, and its first scan follows at once: when a filter
is new, set `intake.created_since` to the moment of the change and read the
line from `--check` first, so that issues people filed earlier are not taken up
before anyone has read it.

A watch, and so the check, also refuses a configuration that still holds one
of the shipped examples' placeholders: a URL whose host is under
`example.invalid`, which cannot exist, or the paragraph the examples'
`instructions` open with. It names the first one by its place, for example
`roles[0].processes[0].env.TASK_REPOSITORY still holds the example's
placeholder host under example.invalid; a watch needs your own value there`.
Only the host of a URL is looked at, so an author's address under that name or
a sentence that mentions it is yours to write. A runtime started on a
configuration with such a host would take up requests and fail each of them
over and over, launching models every time. The commands an example expects
the operator to supply are not checked here: a stage whose command is missing
fails on every round.

Each scan uses fresh tracker pages. The first accepted native issue record is
saved unchanged in `queue/jobs/<id>/issue.json`; later remote edits do not replace
the original request. Its title and complete description go directly to the
existing single-request engine, without a model-generated reception contract.
The per-issue workspace, agent homes and runtime history remain in that job
directory. The collector schedules unfinished histories and does not rerun
histories whose router chose done. This is not an independent claim that the
router's completion judgment was correct. Accepted work remains available for
restart even if discovery is unavailable or the issue disappears remotely.

Execution slots (`intake.max_running`) go to accepted requests in the order they
were filed: a request runs only when no earlier request is waiting for a slot.
Filed, not handed over: an older issue that gains its category today goes ahead
of newer requests already waiting, and those are not told again how many are
ahead of them.
Once a request has finished, the caches its roles' agents built in their home
directories under `queue/jobs/<id>/homes/` are removed, since they are most of a
request's footprint and nobody reads them; the record, the request, the
notices, the workspace and each home's logs and files stay.

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

### A cap on how often a role runs

A reviewer that keeps sending the work back and a router that keeps following
it would run all night. `workflow.launch_limit` names roles and how many times
each may be launched for one request (a reply from the requester starts the
count over):

```json
"launch_limit": {"draft_report": 2}
```

At its cap a role is left out of the choices offered at the next decision, and
the runtime writes one note into the history saying so in its own words. When
every role connected after a step is at its cap they all stay offered: the cap
changes what is offered and never ends a request. Nothing here reads what a
role wrote; it counts launches: a run of consecutive records for one role is
one launch, and a launch that ended in an error and recovered into the same
role counts again.

Cap a role only where the decision that sends work back to it also offers a
way forward that is not the delivery itself. The shipped example caps
`draft_report`, because after `review_report` the run can still go on to
`post_report`. It does not cap `implement`: after `review` the only other
connection is `deliver`, and a cap there would force a delivery over the
reviewer's unmet objection. A cap on a role no connection leads to is refused.
A request that was already running keeps the workflow saved with it, so a cap
added later applies to requests accepted afterwards. An ordered run
(`router.mode: "stages"`) takes no cap, because its progress is decided by
observed results. A live run without a cap, routed by a chat model, sent the
report back and forth between its writer and its reviewer until the harness's
limit; the cap is what bounds that.

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

What counts as the requester's point is said in the same words everywhere: a
point is theirs only when the request, the repository and the operator
instructions do not settle it and it changes what the delivered result does,
where it goes or what the work may touch (a behaviour the request leaves open
without saying you may choose, a target that cannot be told apart, access or a
credential that was not given, instructions that contradict each other, an
action that cannot be undone). Wording, naming, language, level of detail and
style are never questions: the entrance takes the reading closest to the
request and to what the repository already does, writes the choice down with
its reason, and leaves it to the review of the delivered result; a point once
decided is settled and is not listed again as a question. A first live run
without this distinction asked three such preference questions on a request
that was complete, and lost the night to them.

The choice at the entrance is deliberately one-sided. Proceeding with such an
open point costs a night's work while asking costs one reply, so `investigate`
is expected only when every point of that kind is absent or already answered
and the settled requirements state the completion condition to be held to;
when the entrance cannot tell whether a point is of that kind the expected
choice is `ask_requester`, and `investigate` is never a way to find out what
was wanted. That is wording and connections only. Nothing measures a
confidence level, counts open points or inspects what the role wrote, so a
model that ignores the standard still proceeds and no test here can tell you
it will not.

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
`intake.stop_user_ids`, posts a comment with words after that point. A status
or field change, which the tracker records as a comment without words, is not
an answer; the collector makes such changes itself while it waits. That
comment's text
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

What this does not do: beyond having words, the text of an answer is never
checked, so a reply that
does not actually answer the question simply reaches the next decision like any
other report. Only the first comment after the recorded point becomes the
answer; further comments are not appended, and a later question moves the point
past them. While a request waits, each poll reads that issue's comments inside
the collector loop, so a slow tracker delays the loop by up to one interval for
every waiting request, and for every request held for the model budget. Nothing notifies the requester beyond the posted comment
itself, and an unanswered question waits indefinitely unless someone stops it.

### Stages instead of roles

`router.mode: "stages"` replaces the connected graph with an ordered list. A
stage is a configured role plus the kind of fact that satisfies it, and the
runtime, not a model, decides what runs next.

```json
"router": { "mode": "stages" },
"workflow": {
  "stages": [
    { "name": "elicit", "kind": "model" },
    { "name": "work", "kind": "model" },
    { "name": "verify", "kind": "command", "on_failure": "work" },
    { "name": "review", "kind": "command", "on_failure": "work" },
    { "name": "deliver", "kind": "command", "on_failure": "work" },
    { "name": "verify_merged", "kind": "command", "on_failure": "work" },
    { "name": "report", "kind": "model" },
    { "name": "confirm_report", "kind": "command", "on_failure": "report" }
  ]
}
```

The next assignment is the first stage that is not yet satisfied.

- A **command stage** is a role whose processes launch no model. It is
  satisfied when all of its processes exit 0, and its output joins the history
  exactly as any other result does. When it does not exit 0, the run assigns
  the stage named by `on_failure` with that output already in the record, and
  then runs every stage after it again in order, since what those stages
  proved was the earlier state of the work. A command that exited 0 can
  therefore be launched again later in the same run, so an operator's
  command, a delivery above all, has to be safe to run twice (the shipped
  delivery process reuses its commit, branch and pull request). There is
  no counter and no ending: a command that keeps failing keeps cycling.
- A **model stage** is satisfied when its processes ran without a process
  error. What the model wrote is never read, decoded or compared, so writing
  "done", "verified" or "delivered" advances nothing. The command stage that
  follows is what proves the work.
- The run is `done` when the last stage is satisfied. The last stage must be a
  command, so an observed exit status and not a model's words finishes it.
- `on_failure` must name a model stage, and a model stage takes none: a process
  error or an interrupted launch simply runs that stage again, with the
  runtime's usual note about the interruption in the record.

The shipped delivery process (`harnesses/deliver_git.py`) brings the ticket
branch up to date with the integration branch before publishing it, because
requests running side by side merge into that branch while this one is still
being worked on. A clean merge becomes a merge commit on the ticket branch. A
conflicting one is left in the working tree between Git's markers and the
delivery is refused naming the paths, so the role behind the stage's
`on_failure` resolves them in place; the commit of the next delivery completes
the merge, and a change that still carries markers is refused. Paths the
integration branch changed and the worker left as that branch has them need
no grant; what the branch already carried is not scanned for forbidden text.
This happens under the merge method and under `none` (below), not under squash
or rebase, since a service that squashes or rebases rewrites the delivered
history. Under `none` the person who merges may squash or rebase all the same;
a further round after that conflicts with the squashed copy of its own earlier
change, and the conflict is handled like any other (below). For this the
delivery process's sandbox grant must cover the working tree as well as
`.git`.

The delivery refuses a change that carries the delivery credential, or text
listed in `DELIVERY_FORBIDDEN_TEXT` (in any letter case, each entry without
the blanks around it), in a line it adds or in the name of a path it touches.
Every added line is looked at whole: a line ends where Git ends it, at LF, so
a CR, a form feed or a line separator inside it hides nothing after it. A file
Git takes as binary, for a NUL byte or for an attribute, is looked at
as text all the same, no diff program or text conversion stands in for the
lines, and text in UTF-16, which has a NUL beside each ASCII character, is
also looked at with its NULs taken out. Lines are read one at a time, and a
long line in parts of about 4 MiB that overlap by more than any text looked
for can take written in UTF-8, UTF-16 or UTF-32. Git's other reads of the
change before the commit, whether anything is staged and its check for
conflict markers, hold no more of it either, so neither a large change nor a
long line is held in memory whole.

A change that is not UTF-8, such as a file kept in Shift_JIS or a name Git
gives in such bytes, is delivered byte for byte. Its names are held to the
operator's grant like any other, and forbidden text written in ASCII is found
in it as written. Forbidden text that is not ASCII cannot be looked for in
such bytes, so while `DELIVERY_FORBIDDEN_TEXT` lists any, a change carrying
them is refused, saying so, rather than delivered unchecked. A file whose
staged version Git takes as binary for its content, for a NUL byte in its
first 8000 bytes as an image or text in UTF-16 has, is not refused for its
bytes: in it, an entry that is not ASCII is found only where it is written in
UTF-8, while ASCII entries and the credential are found as in any other file.
That holds for a file in Shift_JIS with a NUL byte that early as well.
Neither an attribute nor the version a change replaces decides it, though
either makes Git take a file as binary: text in Shift_JIS is refused under a
`binary` or `-diff` attribute and in place of a file that held a NUL. Those
first 8000 bytes are read only for a file the change would otherwise be
refused for. Where such a name
or text is printed or recorded, each byte that is not UTF-8 shows as a
replacement character (U+FFFD). Two limits follow from looking at bytes. An
ASCII entry can be found where none was written: the second byte of a
Shift_JIS character can be an ASCII letter, so `ツode` (bytes 83 63 6F 64 65)
holds `code`, and such a change is refused although it carries no forbidden
text. And Japanese in a 7-bit encoding such as ISO-2022-JP is all ASCII bytes,
so the rule for entries that are not ASCII does not apply to it, and an entry
such as `社外秘` is not found in it.

A request whose right outcome is that nothing changes, because what it asks
for already exists, leaves the delivery nothing to commit. By default the
shipped delivery refuses it ("No change under the allowed paths is ready to
deliver"), so an ordered run sends it back to the work stage for as long as it
runs. `DELIVERY_ALLOW_UNCHANGED=1` in the delivery process's environment lets
such a request end instead. When the workspace has no changed path, no merge in
progress and no receipt of an earlier delivery round, and the commit the work
started from is part of the integration branch as fetched at that moment, the
delivery commits, pushes and opens nothing and ends 0. It prints that nothing
was delivered and which commit of the integration branch the request stands
on, and its receipt records the same (`"unchanged": true` with that commit as
`base_sha`). A rerun checks again and ends the same way; work changed after
such an ending is delivered as usual. The post-delivery check
(`harnesses/verify_merged.py`) then has no merge to look for: it requires the
recorded commit to be part of the integration branch, runs the configured
commands on that branch as it is now, as after a merge, and says that no merge
was made. The
run then ends like any finished run: where they are configured, the
`delivered` status of `intake.statuses` is applied and `intake.assign` hands
the request back to the requester although nothing was merged, so the report
is what tells them that nothing was delivered; the status page shows such a
request as done without a change, not as delivered. The setting is off by
default and the shipped example leaves it off. An ending with nothing
delivered needs a reviewer's verdict: when Git lists no changed path and no
earlier delivery round committed one, the shipped review command lets the work
through only on a verdict that does not object. When none can be obtained it
keeps asking or holds, as it does for any change (below), and even with its
opt-in for passing work through unreviewed it ends 1 here, so the work goes
back to the work stage. The delivery itself does not look at the review, so turn the
setting on only where that review runs before it. The review command tells its
model in plain words when no file was changed at all, and that a change that
was needed but not made is a blocking defect.

`DELIVERY_MERGE_METHOD=none` makes the shipped delivery end at the open pull
request and leave the merge to a person. It commits, catches up with the
integration branch (a conflict still goes back to the work stage), pushes the
ticket branch, opens the pull request or reuses the one it opened before, and
ends 0 without merging. It prints `Pull request N against <base> is open for
<issue>: <url>. Merging is left to a person; nothing was merged.`, and the
receipt records the pull request's number and address, `"merge_method":
"none"` and `"merge_left_to_person": true`, with no `merge_sha`. A pull request
this delivery opens under `none` says in its description that merging it is
left to a person; one it reuses keeps its description, and the sentence stays
if the operator later switches to merging. Once the pull request is in a
person's hands, what they do with it decides; this process never undoes it.
While the merge of a round is left to a person (under `none`, and after a
switch to merging until the delivery's own merge request succeeds), every later
delivery reads what became of that pull request before its push and again
after it:

- open, its branch holding the commit on record, nothing new: the same
  statement again, and nothing is pushed. A commit on record that never
  reached the branch (a push that was refused or did not go through, a
  catch-up that failed) is pushed by the next delivery.
- open, with new reviewed work: the work is pushed to the same branch, so the
  same pull request carries it.
- open, with a branch a person pushed to or rewrote: the branch is theirs.
  Nothing is pushed over it; the delivery ends 0 saying so and names what is
  not in the pull request, whether the person's push came before or after this
  round's commit: a commit of this delivery that is not on the branch, and the
  changes the workspace still holds uncommitted, whoever made them (the work,
  a pending catch-up, the report). Twenty paths or fewer are all named; above
  twenty it names the first twenty and says how many there are. It records
  `changed_by_person` with the branch's head, `not_pushed`, and
  `not_committed` (at most twenty paths) with `not_committed_count`, the
  number of them in all, whatever it is. A later delivery does not read the
  pull request or its branch again: it says what was read and when, and names
  the changes the workspace holds uncommitted then.
- merged by a person: reported as merged by someone else, with the commit the
  service reports for their merge (recorded as `merge_sha`: the merge commit,
  the squashed commit, or for a rebase the commit the integration branch was
  moved to); work after that is a further round with a pull request of its
  own, left to a person again. If they squashed or
  rebased, that round's catch-up conflicts with its own earlier change; the
  conflict goes to the work stage like any other, and once it is resolved the
  next delivery opens the new pull request. If the person's merge deleted the
  branch, that round's push creates it again.
- merged by a person before this round's commit reached the pull request (they
  merged between the delivery's read and its push): that merge is recorded as
  an earlier round, and the pushed commit gets a new pull request. The merged
  pull request's own head is what tells the two apart. If a round stopped
  after its commit and before its push, the earlier round's record carries the
  stopped round's commit time as `committed_at`.
- not handled: a person pushes to the pull request's branch and merges it
  before any delivery has seen their push, and the next delivery has no new
  work. That delivery takes their merge for an earlier round and pushes its
  own older commit again; while their branch exists the push is refused, and
  the delivery ends 1 saying that nothing was merged, which is not so, so the
  work goes round again.
- merged by a person, but the service does not report the merge commit yet: the
  delivery ends 1 saying so, and the next one reads it again.
- closed without a merge: the request ends. The delivery ends 0 with `Pull
  request N ... was closed by a person without being merged ... This request
  ends with nothing delivered`, records `closed_unmerged`, and never reopens
  the pull request or opens another in its place; continuing needs a new
  request. When an earlier round of the request was merged, it names that
  round's merge commit and pull request and says that nothing of this round
  was delivered. A later delivery does not read the pull request again: it
  says what was read and when, and that the request ended then. A delivery
  interrupted after opening a pull request but before recording it does not
  know that one, and opens another if a person closed it meanwhile.

Switching to a merge method takes a round from the person it was left to only
once the delivery's own merge request succeeds, and only that merge is
reported as `merged with method ...`. A merge found before then is reported as
merged by someone else, and work after it is a further round, which the
delivery merges. Under a merge method alone, a pull request the delivery finds
already merged when it comes to merge it is reported as merged with the
method, since that cannot be told apart from the merge of an interrupted
earlier attempt.

With an open pull request there is no merged state to verify, so the
post-delivery check (`harnesses/verify_merged.py`) checks the pull request's
head instead. It fetches the ticket branch, requires the commit the delivery
pushed to be on it, runs the configured commands with that commit checked out,
and says that this delivery merged nothing, without looking at whether a
person merged since; it ends 0 only when that commit is there and every
command passed. That is what a person is asked to merge, including the
catch-up merge, which the verify stage before the review never saw, so keep
the stage. Once a delivery has recorded a person's merge, the check verifies
the integration branch as usual. If the person's merge deletes the branch
before a delivery has recorded it, the check cannot read the branch and the
run goes round once more, after which the next delivery records the merge;
anything the work changed in that extra round goes in a further round's pull
request. After the two endings a person causes, a closed pull request and a
branch a person changed, the check runs nothing and ends 0, saying what it did
and did not look at: nothing of this delivery is left to verify, and failing
would only send the work round again. An earlier round of the request that was
merged is named there and not checked. Where the target's own checks on pull
requests are what the person merging relies on, the stage can be left out:

```json
"workflow": {
  "stages": [
    { "name": "elicit", "kind": "model" },
    { "name": "work", "kind": "model" },
    { "name": "verify", "kind": "command", "on_failure": "work" },
    { "name": "review", "kind": "command", "on_failure": "work" },
    { "name": "deliver", "kind": "command", "on_failure": "work" },
    { "name": "report", "kind": "model" },
    { "name": "confirm_report", "kind": "command", "on_failure": "report" }
  ]
}
```

The requester is told what the report stage posts from the record: the pull
request's address and that merging it is left to a person. The run then ends
like any finished run, with the `delivered` status and the hand-back where
they are configured, although nothing reaches the integration branch until a
person merges. The operator sees the open pull request at the service, the
receipt on the status page, and, with `--dry-run`, a line saying that a
delivery ends at the open pull request. The status page shows such a request
as done with the pull request open, not as delivered, and so it does for one
whose branch a person changed. The page takes this from the receipt alone, and
the runtime runs no stage of a request that has ended, so a person's merge
after the run ended does not change it; only a delivery while the run still
goes on records a person's merge. A request whose pull request a person closed
is shown as done with the pull request closed unmerged; where an earlier round
of it was merged, its status says so instead of saying that nothing was
delivered. That too is the receipt's reading: a pull request a person reopens
and merges after the run ended is still shown closed unmerged, and the check
after delivery says what the delivery last read and when.

The shipped example's `review` stage is an adversarial review run as the
operator's own command, `harnesses/adversarial_review.py`. A model the operator
names, normally from a different publisher than the worker, is handed the
runtime's text for the stage (where it sits, the original request, the settled
requirements, the previous reports), the diff of the change and the output of
the operator's test commands, and returns one structured verdict: blocking or
not, with its findings. Every call of the verdict tool in the reply is read,
and in each every field named blocking in any letter case, taken as true or
false when its meaning is plain: true or false, a number equal to 1 or 0, or
`"true"`, `"yes"`, `"1"`, `"false"`, `"no"` or `"0"` in any case. One that
reads as true makes the verdict blocking, so a finding is never let through
because the reply also said false; with none true, one that reads as false
does not block; anything else is no verdict (below). A call's arguments are
read as JSON text or as an object, and one object further in, as
`{"verdict": {...}}` is: a true found there always counts, so an objection
written there is never let through for a false beside it, while anything else
there counts only in a call that names no blocking itself. What is kept
whichever way it goes is the findings of each call at those two levels,
arguments that cannot be read, and words given instead of a call, cut where
findings are; findings further in, or inside a list, are not read. The command
exits 1 on a blocking verdict, which sends the work back to the `work` stage,
and 0 on a verdict that does not object; without a verdict it does neither
(below). The findings are printed, so they join the history as an
observation the worker and the report writer read, and the command writes
nothing into the workspace (its send-back counter and log live in the
process's own directory, `TASK_HOME`). The runtime reads the exit status and
nothing else. There is no cap on send-backs: the review sends the work back
for as long as it finds a blocking defect, the count so far is printed with
each verdict, and a run that will not converge is ended by the requester's
stop comment, not by a limit.

No verdict, no pass. The adversarial review is what justifies delivering
without a person, so by default the command exits 0 only on a verdict that
does not object and 1 on one that does. An empty or oversized report selected
for the pull request description also returns 1 for the worker to repair,
explicitly before model review; it is not described as a model's verdict.
Otherwise, without a verdict it does neither. Trouble with the model service (a connection that fails or times
out, an HTTP error, a reply without a verdict or with one whose `blocking` is
not plain), and any unexpected error, is waited out: the models named in
`REVIEW_MODELS` (newline- or comma-separated, in order of preference; a model
named twice is asked once a round) are asked in turn, round after round, the
wait between rounds growing from `REVIEW_RETRY_SECONDS` (5) to
`REVIEW_RETRY_CAP_SECONDS` (300), and the printed result names the model that
gave the verdict. When `REVIEW_MODELS` is set, `REVIEW_MODEL` is not used;
without it, `REVIEW_MODEL` is the only model. Whichever model answers first
reviews, so every model in the list should come from a different publisher
than the worker, as the first one does. An HTTP error is said with what the
service answered, the credential scrubbed and cut to about 200 characters;
400, 401, 403, 404, 413 and 422 are named as the operator's to fix (the
credential, the model id, a request the service refuses), and asking goes
on, the other models included. What asking again cannot get past, a setting
that is missing or mistyped (among them `REVIEW_UNAVAILABLE` with any value
but `pass`, read in any letter case), an endpoint that is not HTTPS, a
credential that is not set, test commands that cannot be read, holds the
review: the reason is printed once and then a short line every
`REVIEW_HOLD_SECONDS` (900), and the stage waits there. A workspace or a
`TASK_HOME` that cannot be used, or a change that cannot be read, is looked at
again at each of those intervals, and the review goes on once it can. An
unexpected error starts the review again at the same growing waits as asking
again, the reason said again every `REVIEW_HOLD_SECONDS`; the operator's test
commands run once a review, not at every start. While it waits, the command
says on stderr what is happening whenever that changes (which model, why it
failed, when it asks again, what a model wrote without a plain verdict), and
again every `REVIEW_HOLD_SECONDS` while nothing changes, so the status page's
live view shows it without a line per request. A reply without a plain verdict
is also written to `review.md` as it comes, and kept in the result the review
ends with. The send-back counter and the log only inform: a verdict stands
whether or not they could be saved. When `REVIEW_DIFF_PATHS` matches none of
what changed, the reviewer is shown the whole change instead of none of it,
cut like any diff, as the note above it says; when the paths match part of
the change, a note says that the rest is not shown.

While the review waits, its request keeps its run slot: a slot is given back
only when the run ends, so with `max_running` 1, as in the examples, every
later request waits its turn behind it. The operator sees why in the status
page's live view, in the lines above; the requester hears only the runtime's
notice that a stage is running long, after `intake.stall_notice_minutes` (90)
and then every six hours. There are three ways out: fix what keeps the verdict
from coming (a setting, the credential, a model id) and restart the engine; a
stop comment from the requester, which ends the run; or the model service
coming back. A restart does not launch the review afresh: the runtime records
the stopped review as a failure and goes on at the review's `on_failure` stage
(`work` in the examples: the worker is launched again, with a record that the
last step may have stopped midway), then the stages after it, the review among
them, run again. The requester is told that the request carries on after a
restart. Each request to a model
carries the change and the test output: once the waits reach
`REVIEW_RETRY_CAP_SECONDS`, every model in `REVIEW_MODELS` is asked once
every 300 seconds: about 100 rounds in eight hours, so up to about 100
requests with one model and 300 with three. A longer
`REVIEW_RETRY_CAP_SECONDS` asks less often.

`REVIEW_UNAVAILABLE=pass` is the operator's opt-in for the old behaviour, and
it delivers unreviewed work when no verdict can be obtained: after
`REVIEW_ATTEMPTS` requests (3), the models in turn, or at once where the review
would otherwise hold, the command prints `NOT REVIEWED` with the reason and
exits 0. One rule comes before it: a checkout in which Git lists no changed
path and no earlier delivery round committed one can end with nothing
delivered if it is let through, so it is let through only on a verdict, and
without one the command ends 1 with `NOT REVIEWED` and the reason. The same
holds when whether anything changed cannot be told (no checkout, or Git
cannot read it, or not within the review's 60 seconds): the delivery reads
the checkout on its own and waits longer, so it may still find no change.
Once the change has been read, what Git listed while reading it decides,
whatever that first look found. With the opt-in, not being able to tell is not
limited to requests with no change: while Git cannot read the checkout in
the review's environment, a request that did change files also goes back to
the work stage, round after round, with the reason written in the record
each time, instead of going on unreviewed; by default the review holds there
instead, as for any change that cannot be read.
New files are read from Git's own list, so a name in Japanese or a new
symbolic link reaches the reviewer as it is, and a name that is not UTF-8 with
a replacement character. Git runs without the user's or the system's Git
settings, without its default exclude and attributes files (`git/ignore`
and `git/attributes` under `XDG_CONFIG_HOME`, or else under `.config/git` in
`HOME`), without settings handed to it through the environment
(`GIT_CONFIG_COUNT` with `GIT_CONFIG_KEY_n` and `GIT_CONFIG_VALUE_n`, and
`GIT_CONFIG_PARAMETERS`), and without the variables that point it at another
repository, index or work tree (`GIT_DIR`, `GIT_WORK_TREE`,
`GIT_INDEX_FILE`, `GIT_COMMON_DIR`, `GIT_ALTERNATE_OBJECT_DIRECTORIES`) or
pick its attributes or its diff program (`GIT_ATTR_SOURCE`,
`GIT_EXTERNAL_DIFF`). The delivery's Git on the workspace runs the same way,
so none of these, set for one of the two stages, makes them disagree on
whether anything changed. Settings handed to Git through the environment do
reach the delivery's Git that fetches and pushes, the post-delivery check
and the mirror. A diff or test
output longer than its limit is cut with a visible
marker, never silently. The credential named by `REVIEW_KEY_ENV` is sent only
to `REVIEW_MODEL_URL`, over HTTPS, and is scrubbed from everything the command
prints or writes.

After every stage the engine appends its own record of what it observed: how
each process ended, and the content of the file named by that process's
`receipt` setting when there is one. It is plain text for the next stage to
read, not a shape anything has to answer in, and it is also where one launch
ends, so a repaired stage is never read as part of the launch that failed.
One launch of a process has no time limit unless its `timeout_minutes` names
one; a launch that reaches that limit is stopped and recorded like any failure.

**What no model decides here**: which stage runs next, whether a stage is
satisfied, whether a failure is recoverable, and when the request is complete.
The only judgment left is at the entrance. If `intake.question_role` is set,
then after the first stage the configured decision service (the decision API
when `router.decision` names a model, the chat API otherwise) is consulted once
with exactly two choices: the question role, or the next stage. It is never
offered `done`, so the entrance still cannot end a request at a person, and the
question role cannot be a stage, so it satisfies nothing. A question holds the
request and the reply resumes it exactly as described above; the reply returns
the run to its first stage. After that no routing decision exists at all.

`examples/operator-stages.json` is the same chain as `operator.json` written
this way, for the runtime image of `deploy/ticket-engine`. Its delivery and its
check of the delivered branch are that image's fixed processes,
`deliver_git.py` and `verify_merged.py` under `/opt/ticket-automation/scripts`
(copied there from `harnesses/`; they are not part of this bundle). Every
checkout comes from `TASK_REPOSITORY`, which holds a placeholder URL under
`example.invalid` until the operator names the image's mirror of the delivery
repository there, so a watch refuses the example until then. Its other
command stages run operator-supplied programs under
`/opt/ticket-automation/operator`: a build, a test run, and a script that reads
the stored comments back and passes when one of them, looked through newest
first, is exactly the reported text. It must not require the report to be the
last comment: the runtime's own notices, a declaration of the model a launch
chose, a stage's sentence or a restart notice, can follow it, and a check that
needs the report to be last then fails on every launch, sending the work back
for good. Those programs are yours to write (`deploy/ticket-engine` carries
examples of all three); the engine only observes what they return. Every
value an operator must replace is one distinct string, so one substitution
sets each everywhere: the placeholder URL for the checkouts' source,
`example-owner/example-repository` for the delivery repository and
`example-integration-branch` for its branch. Those two are not URLs, and the
watch does not recognise them as the example's. The stopped-report role is not
part of the run, and a stop from the requester still wins over everything
here.

**Known limit**: one worker may carry several stages, because a stage's process
may be the same launcher as another's, and each launch receives the goal and
the whole record. Nothing shares a session between launches and nothing trims
the record, so the input grows with every stage and every repair cycle, and a
long repair loop will eventually exceed a model's context. No session sharing
or summarizing is implemented here on purpose: measure it first. Nothing in
this mode has run with a live model or a real tracker.

### A pull request description from the run's reports

An operator can give the fixed review and delivery commands the same
`PR_DESCRIPTION_ROLE`, naming the role whose final report should explain the
change to a person reviewing the pull request. Tell that existing role to
write its findings, implementation explanation, settings and verification
commands in its final ordinary report. No special headings or model answer
schema are required. With no role selected, the fixed delivery description
is unchanged.

This uses the runtime's read-only `TASK_HISTORY` reference, not a document in
the consumer's repository. Configure this option only with a runtime that
supplies that reference to both commands. The latest contiguous set of reports
from the selected role is used, with each process's original words. Requester
and runtime control messages are not substitutes for that role's report. The
review receives exactly this selected text alongside the diff. It must check
the explanation too; its presence is not proof that the change is correct.

The delivery adds the selected text to its ordinary introduction and checks
the whole description for `DELIVERY_FORBIDDEN_TEXT` and its credential before
any publication. `PR_DESCRIPTION_MAX_BYTES` is a positive UTF-8 byte limit
(default 60000) for that complete body. An oversized or empty selected report
sends the work back with its reason (exit 1), rather than holding the review
without asking a model. When the checkpoint is missing, unreadable, malformed
or over its local read limit, review continues without the explanation, and
both the model's input and the delivery result state what could not be read.
The pull request body states that omission too; the remaining diff and test
output are still reviewed. An unset `TASK_HISTORY` or invalid size setting
asks the operator to fix the configuration and restart. This limit is not a
claim about a hosting service's exact limit. The old repository-document option
`DELIVERY_PR_BODY_FILE` is removed; there is no compatibility path or migration.

The generated body is retained in the delivery process's
`TASK_HOME/pull-request-description.md`, outside the checkout. The delivery
record identifies that location and the commit the description accompanies.
The explanation itself is never committed into the consumer repository.
Ordinary run retention still applies to this file; it is not permanent storage.

While an open pull request still contains a body previously submitted by the
automation, later reviewed work can update it. If the service returns a
different body, keep it as the person's version and continue the configured
push and merge. The result says it was retained and where the latest generated
description is kept. CRLF/CR and LF are compared as the same line endings,
so line-ending normalization alone is not reported as a person's edit.
Communication failures remain distinct from different text;
an unknown write result is not a confirmed update.

There is no atomic comparison-and-update. A person's edit between the last
read and an update can race with that update. Coordinate direct edits with
automation rather than treating its saved body as a lock. This option does
not change ticket comments or establish production delivery. The report role
must still describe what actually reached the requested destination.

### A read-only status page

`bin/ticket-status` serves what the queue directory holds, as it is, over
HTTP: every accepted request with its state (done, waiting for the requester,
running a named stage, recovering after a restart), its position in the
configured stages and its elapsed time; and for each request the original
issue and request text, every record of the run with the instruction handed to
the role, the model requested, the full output, the diagnostics and the error,
the answers consumed from the requester, the notices the runtime posted, the
findings the review command kept, the report written in the workspace, and
what the checkout contains right now (`git status`, the diff of tracked files
and the content of new files, read with `--no-optional-locks` so nothing is
written). While a process runs, the engine copies its output as it arrives
into `queue/jobs/<id>/live/`, with every configured credential replaced before
it reaches the disk, and the page shows that copy under "Running now"; the copy
is removed when the record is complete. The engine's own observations go to a
file as well with `--log-file`, and the page shows its tail. Raw files are
served under `/jobs/<id>/raw/` and the configuration as read under `/config`.
The page holds no credential, writes nothing under the queue and takes no
action. It can require HTTP basic authentication (`--auth-user-env` and
`--auth-password-env` name environment variables) and answers `/healthz`
without it. `deploy/ticket-engine/statefulset.yaml.example` runs it as a
second container of the same Pod with the state volume mounted read-only.

Nothing the runtime wrote is out of the page's reach: every directory under
the queue is listed and every file served whole under `/files/`, with a path
that leaves the queue refused, symbolic links included. For a review after the
fact the request page adds the time spent by stage (launches, failures, total,
first start, last finish), the gap before each record, what each role's native
agent logged in its own directory (its log, the model calls and tokens it
counted, and the whole conversation when the harness saved one), the
instruction of a pending action and the delivery receipt; while a process runs
the page reloads every ten seconds and shows, beside the live output, the
native agent's own log as it grows.

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

A tracker that fails to answer one read is treated as slow, not as a lost
control channel: the work goes on, and only three failed reads in a row pause
it, so a stop filed meanwhile is still read as soon as the tracker answers.

Optional `intake.statuses` moves the issue's status at each turn of the work,
by status id, so a requester can see whose move it is on the tracker board:
`processing` when the request is accepted and whenever the runtime works on
it, `awaiting_requester` while a question waits for the requester,
`delivered` once the run is done and the report posted: usually after the
merge, but also when the run ended at the open pull request of
`DELIVERY_MERGE_METHOD=none`, with no change, at a pull request a person
closed, or at one whose branch a person changed, so the status alone does not
say that anything was merged; `stopped` after
the requester's stop, once the stop is recorded and, where a stop report is
configured, that report is done. A stop written while a question waits moves
the issue from `awaiting_requester` to `stopped` without passing through
`processing` or the runtime's own account. An id left out leaves that turn alone; the
runtime never reads or names a status. Each change is made once and recorded beside the
request in `status.json`; a refused change is asked again for as long as the
request lives, a minute after the first refusal and up to an hour apart after
repeated ones, never given up, and the same refusal is logged once.

```json
"intake": {
  "statuses": { "processing": 1001, "awaiting_requester": 1002, "delivered": 3, "stopped": 1 },
  "category_on_accept": 2001,
  "assign": true,
  "announce": true,
  "status_page": "https://status.example/jobs/"
}
```

The ids are the operator's own project's: custom statuses and categories have
the ids the tracker gave them (1001, 1002 and 2001 above stand in for them),
while 1 and 3 are the tracker's built-in open and resolved statuses.

With `announce`, the runtime also says, in its own fixed words, when a request is accepted (with
its place in line and, when `intake.status_page` is set, a link to the request's
own page), when its work starts after waiting its turn (a request told it starts at once hears no
second comment), and when it resumes after the requester's answer (a stop written while a question
waits is not an answer and is not announced as one); a stage whose `announce` sentence the
operator wrote in `workflow.stages` is announced once when it first begins.
That sentence carries the model the launch beginning the stage chose, as
` (モデル: <catalog id>)` without any gateway prefix; a stage that launches no
model, a runtime that selects none, and a launch that could not choose one at
all, say it as the operator wrote it. Once the request is delivered, one
further comment lists every launch that used a model, one line per stage in
the order the stages first ran and each stage's own launches along it, with
the time each took and the stage named as the status page names it:

```
使ったモデル (工程ごと、起動順):
- 要件確定: maker/one (27 秒)
- 作業: maker/two (11 分 0 秒) — 再実行: maker/three (5 分 18 秒) (失敗)
- 報告: maker/four (32 秒)
```

`再実行` opens every launch after a stage's first, whether a later stage sent
the work back or the stage's own process did not exit 0, and `(失敗)` marks a
launch that did not exit 0. A stage the work returned to late keeps its own
line, so its last launch is printed above launches that ran before that
return.

`"declare_models": true` (off unless set, and independent of `announce`) says,
while a stage's work is under way, which model the selection chose for it:

```
要件確定を始めます。選定モデル: maker/one
要件確定をやり直します。選定モデル: maker/two
```

The first line is said at a stage's first launch that chose a model; a stage
whose `announce` sentence goes out says that launch in its sentence alone. The
second is said at a later launch, after a later stage sent the work back or a
launch did not exit 0, and only when its models differ from those the stage was
last declared with: the same models again say nothing. A stage that ran before
the setting was turned on is told the second line at its first launch after it,
since its history shows the earlier one. A launch whose processes choose
several models names them together, separated by `、`, once all have chosen; a
launch in which some processes' selection failed is declared when it returns,
with the models that were chosen. A command stage and a launch whose selection
failed entirely say nothing. The declarations are recorded in `notices.json`
like the notices, so a restart repeats none, and a launch is declared only
during the run of the request it belongs to: one from before a question, a
restart or a budget hold, and one from before the setting was turned on, are
not declared afterwards; the list at delivery names them.

With the setting on, the watcher of a running request also looks in the
request's own files every two seconds, between polls, and once more when the
run is delivered, fails or waits for the requester. A declaration, and a
stage's `announce` sentence, therefore goes out within seconds of the choice,
and the launch a run ends or waits on, such as the question or the report
written last, is not missed. A look with nothing to say reads nothing from the
tracker. One that posts goes through the notices, which first settle a notice
whose submission was not confirmed, with one read of the issue's comments;
otherwise such a notice is retried on the poll. Only the latest launch of each
stage is looked at, so a launch that the next launch of the same stage
replaces within those two seconds is not declared; the list at delivery names
it.

`category_on_accept` adds that category to an accepted issue; it does not
decide which issues are accepted (`intake.category_ids` does). `assign` hands
the issue to the requester while a question or the delivered result waits for
them, and back to the runtime's own account while it works, and records the
hours from acceptance to the report in the issue's actual hours.

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
history reach it as context; a launch that failed the same way over and over
is one entry there, saying how often it repeated, so a long record stays
readable. In an ordered run (`router.mode: "stages"`) the report runs that one
role until it returns without a process error, and no decision service reads
the stopped record. No new model or working-answer format is required.
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
tracker cannot turn into two identical comments. The stage sentences and the
delivered request's list of the models it used are recorded there the same way,
so a restart repeats neither.

A notice is posted only about something that happened after the queue's
engines began posting its kind: `queue/notice-kinds.json` keeps that start for
each kind from the first run of an engine that posts it (a kind switched off,
or unknown to a run in between, starts again when it returns), and anything
older, such as a request accepted or delivered, a stage begun or a stall
started before then, is not posted; a notice said once per request is recorded
in `notices.json` as `predates` instead. On the first run of an engine that
keeps this record every kind starts with that run, so a stage that began before
it and had not been announced yet, or a request delivered before it whose list
had not gone out, stays silent; a record the engine cannot read when it starts
is set aside as `notice-kinds.json.unreadable`, said once, and treated the same
way.

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
stop stops it, no work is launched, and the requester is told once:

> 自動処理を一時停止しました。モデル利用枠の残りが設定の下限を下回ったためです。枠が戻り次第、自動で再開します。

The balance keeps being read each tick. When it is back above the floor the
request is launched again and says so once:

> モデル利用枠が回復したため、自動処理を再開しました。

An authorized stop does not wait for the balance: recording it launches no
model. A request held below the floor is still read for a stop on every tick,
whether its work was running, had not started, or waits on a question, and a
request stopped while a question waits is told nothing about a pause. Where a
stop report is configured, that report runs as soon as the stop is recorded,
below the floor too, and uses the key, exactly as after the stop of a running
request.

The pause and the recovery alternate, so each episode gets one line of each. An
endpoint that cannot be read is not evidence of an empty budget: it never
pauses work and never posts, and the reason goes to the log with the
credential value removed.

**When nothing has completed for a long time.** `intake.stall_notice_minutes`
is how long a running request may go without a completed step before the
requester hears about it. Absent means 90 minutes and zero switches it off. The
window is measured from the last history entry that finished without an error,
so any successful role output inside it keeps the request quiet. Past the
window, a request whose steps keep failing is told:

> 依頼はまだ終わっていませんが、過去 <n> 分間は工程が完了していません（直近の失敗: <直近の失敗の1行目>）。

The quoted failure is the first nonblank line of the most recent error (when
that line is only an exit status or a signal, the error's last line, where a
harness puts its reason, follows it), with every configured credential value
replaced by `[credential]` and the result cut to 200 characters. A request
whose work is running, with no failure recorded, is told once nothing has been
recorded for that long. The time counts only within the current launch: from
its last record or, when there is none yet, from the moment the launch began,
so a wait before it (for its turn, for the budget, for a stopped engine) does
not count; a stop that cut a step short is recorded as a failure and counted
in the form above. No time limit ends a launch, so this is the requester's
only word about a long one:

> 依頼はまだ終わっていませんが、過去 <n> 分間は工程が完了していません。この間に工程の失敗は記録されていません。

Neither form gives a cause or says who has to act; the budget notice above
says why the work paused, and not who has to act. The engine records what its
steps did, and from there a launch that is working and one that waits for its
operator look the same: a step held for a setting only the operator can
correct has not failed and has not finished. A failure that repeats may need a
person. A balance may come back by itself or only when someone adds to it. The
failing form is also said while the engine itself holds the work for the
budget; the failure it quotes then is the engine's own cancellation of the
step it stopped, and nothing is being tried. None of the three says that no
answer is awaited, because a question to the requester may be standing right
above it. The stall notice repeats at most once per six hours per request. It
is a notice and nothing else: routing, recovery and the request's goal are
untouched by it.

All three go out through the controller's own tracker credential, the same one
the stop report uses. No role is given the means to post them.

What this does not do: these notices say what the engine recorded, not that
the work is being tried at that moment or that it will succeed. They are
posted from the collector loop, so a slow tracker delays that loop while one
is being submitted. A request recorded as waiting for the requester's answer
is not stalled and says nothing further; a question whose launch has not
returned yet is not recorded as waiting, so a notice can follow it.
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
`examples/operator-stages.json` leaves out `moonshotai`, the publisher of its
review model, so that the review comes from another publisher than the work.

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
With `judge` omitted, this chat service makes every choice by itself: nothing
is tried first and nothing is logged as a failure. A `model_selection` with no
judge, no fallback and no fixed model is refused before any request is accepted.

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

The gateway measured here answers chat completions but not the decisions API
(HTTP 405). An operator who keeps `model_selection.judge` and `router.decision`
on OpenRouter keeps that spend, and the decision service's judgement, on that
account. An operator who wants every model call on the gateway omits both and
names the gateway's chat endpoint in `model_selection.fallback` and
`router.llm`: every choice, the entrance question included, is then made by
that chat model through the gateway. Its `model` is the id the gateway
invokes, prefix included (`openrouter/...` here), since the gateway prefix is
added only to selected ids. Remove `intake.min_model_credit` as well: it reads
the decision service's key, the queue refuses it without one, and there is
then no model-balance check at all; whatever budget the gateway enforces is
the only one.

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

Once a checkout is published, a record of it (`.workspace.prepared`) is kept
beside the workspace, in the job's own directory, which no role's sandbox
mounts. A workspace found empty although that record exists was lost, by a
restore or by hand, together with everything the earlier stages did in it. Its
next launch prepares it again from the repository, prints that the workspace
was lost and ends non-zero without running its command: a command stage then
goes back to its `on_failure` stage, after which every later stage runs again,
and a model stage runs again. A stage that passed on the lost work is not
taken as passed on the fresh checkout. Only a workspace found empty is
noticed, though. One put back to how it was just after its preparation (a
checkout and clean by hand, or a restore from a copy taken then) still holds
files, so it is taken as it is; with `DELIVERY_ALLOW_UNCHANGED=1`, a request
whose work was undone that way after its review ends with nothing delivered.
A workspace prepared before this record existed is taken as it is at its next
launch and gains the record without a word, so a queue already running when
this arrives goes on as it was. If it is emptied before that launch, it is
prepared as if for the first time: its loss is not reported either, and the
same ending can follow. A workspace directory removed while the engine runs is
not covered: no process can start in it, so each launch fails at once, spaced
like any launch that cannot start, and nothing passes; restarting the engine
creates the directory again, empty, and the next launch reports the loss.

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
  (default `32000`, per native API response, not a request failure limit). An
  answer that stays cut off at that limit after the agent's continuations
  leaves no report; the bridge then exits 1, so the runtime retries the role
  instead of passing an empty result to the next one.
- Optional `NATIVE_MAX_TURNS`: the number of model calls after which the native
  agent stops. None is set unless the operator names one.
- Optional `NATIVE_LOG_PREFIX_CHARS` (default `2000`): how much of each tool
  call's arguments and result the harness prints as it happens; the lines reach
  stderr, so the runtime's live copy shows them, and only the tail of them
  travels in a later role's prompt.
- `TASK_CREDENTIAL_NAMES` is set by the runtime: the names of the variables
  that hold credentials, so the harness can keep every one of them out of the
  transcript and the native agent's logs it leaves in its directory.
  The transcript is written, and the native agent's own `logs/agent.log` and
  `logs/errors.log` are rewritten with those values replaced, after the
  conversation ended normally; while the role runs, and after a crash or a
  kill, those two files are as the SDK wrote them. The status page shows them
  as they are.

`harnesses/raven.py` is a second bridge, to Raven-Code's one-turn launcher,
behind the same contract and the same `OPENROUTER_*`, `NATIVE_*` and `TASK_*`
settings. It does not import Raven: `RAVEN_ROOT` names a Raven checkout (the
launcher is `agents/raven-code/run.py` under it) and `RAVEN_PYTHON` an
interpreter that can run it (Raven wants Python 3.12 or newer, which the
bundle's own image may not carry). Under the sandbox, `RAVEN_ROOT` must also
be given as a `--runtime` path, and it must not lie inside the queue or the
workspace. The bridge renders the product's configuration with the launch's
endpoint, model and limits, names the wire protocol (chat completions, the
one the other bridge speaks for every model) rather than leaving Raven to
infer one from the model id, switches Raven's skill-evolution pipeline and
memory backend off, so a role does tonight what it did last night, keeps
Raven's state under `TASK_HOME`, removes the launcher's rendered
configuration (which holds the key) and scrubs its log after every run, and
treats a launcher that commits no report, or reports its own failure, as a
failed role: what it said goes to stderr, not to the next role.

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

Set a process's `tracker_access` to `read`, `comment` or `every-comment` to give
it temporary access to the assigned issue without giving it the controller's
tracker account key. `read` permits the issue and its comments; `comment`
additionally permits posting ordinary comment content, and leaves one comment
per launch: when the launch is over, the comments it stored before its last
one are removed with the controller's account, so a role that posts a trial
line before its question leaves only the question. Which post is which is
never read; the one the tracker stored last stays, a removal the tracker
refuses leaves that comment in place, and a notification the tracker already
sent for a removed comment is not recalled. `every-comment` keeps every post.
The assignment comes from the operator's
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
