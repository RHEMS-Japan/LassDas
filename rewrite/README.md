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

This supplies fresh selection input, not automatic model selection. Initial
working/comparison model choices remain restricted to current Chinese
frontier/value candidates; the catalog alone does not prove value for a task.
Do not substitute a fixed shortlist or the cheapest price for that assessment.

### Existing native-agent connection

`harnesses/hermes.py` is the stdin bridge to an installed Hermes SDK. It uses
the SDK's existing agent/tool loop, not a new implementation of one. In the
role's isolated execution environment, configure the process argv as the
installation's Python executable followed by the absolute bridge path. Make
the installation's `run_agent` module importable, for example with a scoped
`PYTHONPATH`. This repository does not install or modify that dependency.

Provide these process settings explicitly:

- `HERMES_HOME`: a separate writable agent directory for each role/reviewer.
- `OPENROUTER_BASE_URL` and `NATIVE_MODEL`: the selected endpoint and model.
  The bridge has no model default, shortlist or catalog-selection policy.
- `OPENROUTER_API_KEY`: map a named credential source through `secrets`, not
  a literal credential in `env` or the configuration file.
- Optional `NATIVE_REASONING_EFFORT` (default `low`) and `NATIVE_MAX_TOKENS`
  (default `6000`, per native API response, not a request failure limit).

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

Multiple processes for one role run independently with the same prior history.
They do not see one another's current report. A Jev transport/context failure
can use the configured ordinary-LLM router. Role errors and timeouts become
observations for the next decision; there is no attempt-count terminal.

History is saved before dispatch and after return. A restart after an
interrupted action tells the router that the action may already have happened;
it must inspect before repeating it. A temporary result-save failure retries
the save, not the external action. Only one process owns a run directory.

## What the tests establish

### Tracker communication available to configured roles

`go build ./cmd/tracker` provides `read`, `comments`, `comment`, and `post`
actions. Configure its endpoint, assigned issue and named credential source in
the authorized role's environment. For example, inside that role's isolation:

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
The launcher must provide only the authorized capabilities. No live tracker
posting or unattended restart reconciliation has been validated yet.

The new command is tested as a real subprocess against a local TLS tracker,
including verbatim prose publication and readback. This is not live delivery.

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

- Automatic intake/claiming, isolated per-request checkout, tracker stop,
  integration of the role's final-comment posting/readback into unattended runs,
  and live delivery integration.
- Resolve the observed premature completion: reviewers must compare
  relevant original behavior and preserve objections against the actual request.
  Broader validation still needs misleading reports, repeated failures and restart.
- Automatic model selection from a freshly fetched catalog. Prototype config
  currently names experiment models explicitly. These names are not a durable
  shortlist; refresh the available model list on each selection, initially use
  current Chinese frontier/value candidates, and do not call a saved snapshot
current when retrieval fails.
- Production isolation. Restricting child environment variables does not
  sandbox filesystem/network access; the configured launcher must enforce the
  role's actual permissions. Do not run unconfined commands with broad keys.
- Startup acquisition/configuration/storage errors currently return from the
  CLI. Retry coverage inside `Chain.Run` is not startup recovery. Process output
  is buffered in memory; production resource behavior has not been exercised.

Jev routing is one comparison candidate, not a settled architecture decision.
