# Glossary

This glossary gives one word for each thing the engine handles, so that the
people and models who write about this repository call the same thing by the
same word. Each entry gives the English word and, where the documents have
one, the Japanese word; a short definition; places where the documents use
the word; and the words not to use for the same thing.

[harnesses/glossary_scan.py](harnesses/glossary_scan.py) reads this file and
looks for every word to avoid in README.md, START.md and RUNTIME.md, the
repository's top-level CLAUDE.md, and the `instructions` texts of the
examples. It fails when one of them appears, unless the word is listed under
"Waiting for rewording" at the end. It also fails when a word of an entry is
not used in those documents, or when a file listed after "Used in" no longer
uses it. The line numbers after "Used in" are where the word was when the
entry was written. When one has moved, the scan names the nearest line that
uses the word, which may be a different sentence, and does not fail.

The scan matches words this way:

- An English word matches as a whole word in any letter case, and also with s
  or es added. A space in it matches any run of blanks, line ends included.
- A Japanese word matches wherever it appears.
- A word is not counted inside the longer words named after "except in".

## People and places

### engine / 本体

The program `ticket-engine`. It takes requests in from the tracker, launches
roles stage by stage, keeps each request's history in the queue and posts its
own notices.

- Used in: README.md:8, START.md:65, ../CLAUDE.md:96
- Not the same as: controller, which is the engine while it runs as a process.
- Other uses: README.md also says the runtime for the engine. RUNTIME.md uses
  runtime for the environment the engine runs in.
- Avoid: `エンジン`

### controller

The engine's own process while it runs. It holds the configured credentials
and the queue, starts each launch's processes as its children, and stops them
when a request is stopped. It is meant to run under a supervisor outside the
engine that restarts it after a crash; the bundle does not install one.
START.md writes controller in Japanese text as well.

- Used in: RUNTIME.md:190, START.md:72
- Not the same as: a role's process, which receives only the credentials
  named for it.
- Avoid: none.

### operator / 運用者

The person who sets up an installation: its configuration, credentials,
commands, permissions, network and delivery target. A requester's answer does
not change any of them.

- Used in: README.md:1111, START.md:105
- Avoid: `管理者`

### requester / 依頼者

The account that filed the issue. Only the requester, and the accounts listed
in `intake.stop_user_ids`, can answer a question, stop the request or resume
a pause.

- Used in: README.md:280, START.md:106
- Avoid: `creator`; `起票者`

### installation / 導入先

One place where the engine is set up and runs, with its own configuration,
credentials, tracker scope, queue and delivery target.

- Used in: README.md:10, ../CLAUDE.md:96
- Not the same as: consumer, the project that receives the work.
- Avoid: none.

### consumer / 消費側

The project that uses the engine and receives its work. Changes and knowledge
changes are delivered to its repository, and its existing knowledge stays
there.

- Used in: README.md:1189, ../CLAUDE.md:17
- Not the same as: installation, the place where the engine runs.
- Avoid: `対象プロジェクト`

## Requests and where they are kept

### tracker / 課題管理

The issue tracking service the engine takes requests from and posts comments
to: Backlog or GitHub Issues, one of them per configuration.

- Used in: README.md:175, START.md:55
- Avoid: `トラッカー`

### issue / チケット

One item in the tracker. Its title and description, as the engine first
accepted them, are the request. Its comments carry the questions, answers,
stops, notices and the report.

- Used in: README.md:250, ../CLAUDE.md:9
- Not the same as: request, which is what the issue asks for.
- Avoid: `ticket` except in `ticket-`, `ticket/`, `ticket branch`; `課題` except in `課題管理`

### request / 依頼

What the requester asked for: the issue's title and description as the engine
first accepted them. The engine keeps them unchanged in the queue, and later
edits to the issue do not replace them.

- Used in: README.md:13, START.md:138, ../CLAUDE.md:81
- Not the same as: issue, the tracker item that carries the request.
- Avoid: none.

### intake / 受付

The engine taking new issues from the tracker within the scope the operator
configured, such as the project or repository, the label, the categories and
`intake.created_since`, and saving each as a request in the queue. Intake does
not look at the wording of a request.

- Used in: README.md:237, START.md:58
- Avoid: none.

### queue

The directory the engine keeps for its accepted requests, given as
`--run-dir`. Each request has its own directory under `queue/jobs/<id>/`,
with the request as accepted, its history, its workspace and its notices.
Requests wait there for an execution slot in the order they were filed, and a
restart with the same queue continues the same requests. START.md writes queue
in Japanese text as well.

- Used in: README.md:225, START.md:119
- Avoid: none.

### history / 履歴

The saved record of one request's run, in `queue/jobs/<id>/run/history.json`:
each assignment, what each launch returned, the requester's answers and the
engine's own notes, one record each. Roles can read it through
`TASK_HISTORY`, and a restart continues from it.

- Used in: README.md:92, START.md:139
- Avoid: `checkpoint`

### status page / 画面

The read-only page that `ticket-status` serves from the queue: each request's
state and stage, its history, the notices, the delivery receipt and the
workspace's current changes. Its overview, the board, shows the requests in
columns by stage. The page holds no credential and changes nothing.

- Used in: README.md:1240, START.md:131
- Other uses: START.md also says 画面 for the screens of the product a change
  alters, in 操作の流れ・画面・公開 API, which decides whether the requester
  sees a change before delivery.
- Avoid: none.

## Roles and how they run

### role / 役

A named part of the configuration, in `roles[]`, with a purpose and one or
more processes, each with its own instructions. A stage or a routing choice
launches a role, and its processes run in parallel.

- Used in: README.md:41, START.md:16
- Not the same as: process, a command that a role runs.
- Avoid: `役割`; `担当` except in `担当チケット`, `担当者`

### working role / 作業役

A role whose processes run a model agent on the request: investigating,
changing the checkout, reviewing or writing the report. A command stage is not
a working role, even one that asks a model, such as the adversarial review.

- Used in: README.md:1857, START.md:78
- Other uses: README.md also says the worker, mostly for the working role that
  changes the checkout. RUNTIME.md says workers for the processes that roles
  run.
- Avoid: none.

### process / プロセス

One command configured in a role, in `roles[].processes[]`, with its
arguments, environment and named credentials. Each launch runs it as a child
process of the controller. What it writes and its exit status are the
launch's output.

- Used in: README.md:42, START.md:117
- Not the same as: role.
- Avoid: none.

### launch / 起動

One start of a role's processes for one assignment, ending when all of them
have exited. `workflow.launch_limit` counts launches, and a launch has no
time limit unless `timeout_minutes` sets one.

- Used in: README.md:1081, START.md:108
- Avoid: none.

### rerun / やり直し

A later launch of a stage that has already run for this request, because a
later stage sent the work back or because the stage's own launch did not
exit 0. With `declare_models`, the engine declares a rerun only when its
model differs from the one last declared for that stage, in words such as
要件確定をやり直します.

- Used in: README.md:690, README.md:1355
- Not the same as: retry.
- Avoid: `再実行`

### retry / 再試行

Repeating one operation that failed, such as posting a comment, reading the
tracker, saving the history or asking a model service, without launching a
role again.

- Used in: README.md:1584, START.md:128
- Not the same as: rerun.
- Avoid: none.

### stage / 工程

One position in a request's workflow, filled by launching a role. In an
ordered run the stages are the configured list: a model stage is satisfied
when its processes end without a process error, a command stage when every
process exits 0, and the last stage must be a command stage.

- Used in: README.md:597, START.md:104
- Avoid: `step`; `ステージ`; `ステップ`; `段` except in `段階`, `手段`, `段落`

### workflow

The configured order of roles, in `workflow`: either connections, from which a
model chooses the next role, or the list of stages of an ordered run. Without
a workflow, a model may choose any role. START.md writes workflow in Japanese
text as well.

- Used in: README.md:119, START.md:105
- Avoid: none.

### connections / 接続

The `workflow.start`, `after` and `recover` lists: the roles that may come
first, the roles that may follow each role, and the roles that may follow it
after a process error. Routing is offered only the connected roles, and
`recover` cannot choose done.

- Used in: README.md:117, START.md:55
- Avoid: `graph`

### ordered run / 順次工程

A workflow given as a list of stages, with `router.mode` set to `stages`. The
engine, not a model, decides what runs next: the first stage that is not yet
satisfied. A failed command stage sends the work to its `on_failure` stage,
and every stage after that one runs again.

- Used in: README.md:415, START.md:18
- Avoid: none.

### routing / ルーティング

Choosing the next role, made by the decision service or by a chat model; with
connections, only among the connected roles. In an ordered run with a question
role, such a choice comes each time the first stage finishes, and each time a
stage marked `confirm` finishes: the first stage, the question role or the
next stage, never done. `workflow.entrance_rework_limit` bounds how often the
first stage can be chosen again straight away, twice unless it is set, and
`workflow.confirmation_rework_limit` how often it can be chosen after a stage
marked `confirm`, also twice; a new answer from the requester starts both
counts over.

- Used in: README.md:82, README.md:1087, START.md:78
- Avoid: none.

### decision service / 判断サービス

The model service the engine asks for a choice through a decisions API, such
as Jev: the next role, with `router.decision`, and the working model, with
`model_selection.judge`.

- Used in: README.md:1088, START.md:79
- Avoid: none.

### model selection / 選定

Before each launch of a process that has `model_env`, the engine fetches the
current model catalog and has a model choose a working model from the
publishers the operator approved, unless `model_selection.fixed` names one
model. With `declare_models`, the engine posts which model it chose.

- Used in: README.md:1731, README.md:1354
- Avoid: `選定役`

### gateway / ゲートウェイ

A service compatible with the OpenAI API that calls the selected models for
the engine, set in `model_selection.gateway`, so that their use is billed to
the gateway's account. A prefixed model id is a route to the same model. A
gateway is a way to connect, not a check.

- Used in: README.md:1842, START.md:76
- Avoid: none.

## From request to delivery

### elicitation / 要件確定

The first stage, `elicit`. It reads the request, the checkout and the
operator's instructions, settles what they answer and writes down why, and
leaves only the points that only the requester can decide, each with two to
four choices. Its standard is whether the request could be delivered and
verified by morning with nobody available to answer. In the ordered examples,
a failed verification, review or delivery returns here.

- Used in: examples/operator-stages.json:34, README.md:1339
- Avoid: `entrance`; `要件詰め`; `要件確認`

### requirements / 要件

What the request is to be carried out as, settled at elicitation: the target,
the behaviour, the delivery and the condition that counts as finished. A
requester's answer settles requirements; it does not widen permissions.

- Used in: README.md:426, START.md:110
- Avoid: none.

### question / 質問

A comment that the question role, `intake.question_role`, posts to the issue.
It lists every point that only the requester can decide and that can be
answered now, numbered, each with a recommended answer and concrete choices; a
point that depends on one not yet answered comes in a later comment, and a
reply of `推奨で` accepts every recommendation in that comment. A general
request for clarification is not a question. After a stage marked `confirm`,
the question instead shows the requester what the change does, with the
choices to deliver it as it is, to name what to change, or not to deliver it.
The request waits until it is answered or stopped.

- Used in: README.md:539, START.md:122
- Avoid: `打ち返`

### answer / 返答

The first comment with words that the requester, or an account in
`intake.stop_user_ids`, posts after a question. The engine adds it to the
history unchanged, as the requester's own words, and the request resumes.
Beyond having words, its text is not checked, and a stop comment is never an
answer.

- Used in: README.md:556, START.md:124
- Other uses: README.md also says reply for an answer, and answer for what a
  role returns at the end of a launch.
- Avoid: `回答`; `返事`; `返信`

### instructions / 指示

The operator's text in the configuration: the top-level `instructions`, given
to every role and to routing together with the request, and each process's
own. They say where project knowledge is, what the delivery target and the
allowed delivery commands are, how to verify, and what counts as finished.
They grant no permission.

- Used in: README.md:103, START.md:105
- Avoid: none.

### knowledge location / 在り処

Where the consumer's existing project knowledge is: a path inside its
repository, or, when it exists only outside the repository, the text itself in
the instructions. Passing knowledge in (渡す、取り込み) means giving this
location, so the engine keeps no copy of what the repository holds.

- Used in: README.md:2085, START.md:62, ../CLAUDE.md:34
- Avoid: none.

### knowledge change / 知識の追記

A requester's answer that is worth keeping, written into the consumer's
repository at the write destination agreed during elicitation, by the working
role together with the requested change, and delivered in the same reviewed
pull request. This is how knowledge accumulates (溜める、蓄積). The ordered
examples ask for it.

- Used in: README.md:2096, ../CLAUDE.md:66
- Avoid: none.

### workspace / 作業場所

The request's own working directory, `TASK_WORKSPACE`, under
`queue/jobs/<id>/`. For a Git project it holds the checkout of the source
repository; working roles change files there and the delivery commits from
it. Its `.git` stays read-only to roles unless it is named.

- Used in: README.md:333, START.md:63
- Avoid: none.

### review / レビュー

Examining the change against the request before delivery. In the connected
examples it is a role whose two reviewers inspect the source and tests
independently; in the ordered examples it is the adversarial review.

- Used in: README.md:866, START.md:104
- Avoid: none.

### reviewer

A process that reviews, and the model it runs. The reviewers of one review
role run in parallel from the same history and do not see each other's output
from the same launch. The Japanese documents have no word for it; START.md
says 2者レビュー for a review by two of them.

- Used in: README.md:105, README.md:1835
- Avoid: `review seat`

### adversarial review / 敵対レビュー

The `review` stage of the ordered examples. The command
`harnesses/adversarial_review.py` hands the request, the settled requirements,
the diff and the test output to a model the operator names, normally from a
different publisher than the working role's; the command does not check the
publisher. It exits 1 on a blocking verdict and 0 on a verdict that does not
object. With `PR_DESCRIPTION_ROLE`, an empty or oversized report chosen for
the pull request description also exits 1, before any model is asked. Without
a verdict it keeps asking or waits; it lets work through unreviewed only when
the operator sets `REVIEW_UNAVAILABLE=pass`.

- Used in: README.md:866, README.md:942, START.md:110, ../CLAUDE.md:86
- Avoid: none.

### send-back / 差し戻し

Sending the work back to an earlier stage to be repaired. In an ordered run, a
blocking verdict or a failed command stage sends it to the stage named by
`on_failure`, and every stage after that one runs again. An ordered run has no
limit on send-backs; a request that does not converge is ended by the
requester's stop. With connections, `workflow.launch_limit` can cap how often
a role is launched for one request: a role at its cap is no longer offered,
unless every role connected at that point is at its cap, and the cap never
ends a request.

- Used in: README.md:892, START.md:108
- Avoid: none.

### verification / 検証

Running the operator's approved checks against the work or against the
delivered result. In the ordered examples it is a command stage: `verify` runs
the build and tests, and `verify_merged` checks the delivered branch or pull
request after delivery. There the exit status is the observation, and the
stage returns no report and no approval.

- Used in: README.md:1098, START.md:104
- Avoid: none.

### delivery / 納品

Putting the reviewed change into the delivery target. In the ordered examples
the image's fixed delivery process does it: it commits, catches up with the
integration branch and pushes the ticket branch, and then, as configured, it
merges a pull request, leaves an open pull request for a person to merge, or
publishes the branch alone. Its receipt records what happened, and the engine
reads the receipt back.

- Used in: README.md:619, START.md:104
- Avoid: none.

### delivery target / 納品先

Where delivery puts the work, as the operator configured it and the request
agreed: for example a repository's integration branch, a pull request left to
a person, or a branch alone.

- Used in: README.md:10, ../CLAUDE.md:9
- Avoid: `delivery destination`; `納品地点`

### report / 報告

The text for the requester at the end of a request: written to
`report/result.md` from the records, posted as one comment and read back. A
later stage compares the stored comment with the file.

- Used in: README.md:842, START.md:104
- Other uses: README.md also says report for what a role returns at the end of
  a launch; this glossary calls that the output.
- Avoid: none.

### output / 出力

What a launch returns: what each process wrote on standard output and standard
error, and its exit status. The engine adds it to the history, where later
roles and routing read it. In an ordered run no stage is satisfied by what a
role wrote; the only choices that read it are the routing after the first stage
and after a stage marked `confirm`.

- Used in: README.md:600, START.md:133
- Avoid: none.

### notice / 通知

A comment in fixed words that the engine posts itself, through its own
tracker account: on acceptance, start and resume when `intake.announce` is
on, when a stage with an `announce` sentence begins, after a restart, when
work pauses and resumes, when no stage has completed for a long time (the
stall notice, 停滞通知), and the declarations and the list of the models
used. While a request waits for its requester, the engine also says when the
question about a change was not seen posted, and again after each interval set
by `intake.question_reminder_minutes`. No model writes them, and none of them
ends a request.

- Used in: README.md:1581, START.md:133
- Avoid: `announcement`

### stop / 停止

A comment whose first nonblank line is exactly `停止`, from the requester or
an account in `intake.stop_user_ids`. When the engine reads it, it stops the
running role and keeps the request stopped across restarts; when
`intake.stop_report_role` is set, that role reports what happened. A stop does
not undo anything that already reached the outside.

- Used in: README.md:1277, START.md:121
- Not the same as: pause.
- Avoid: none.

### pause / 一時停止

The engine holding a request by itself: when the model credit falls below
`intake.min_model_credit`; when a request with an active-work limit,
`intake.max_active_minutes`, reaches that limit or its number of forced exits,
`intake.max_hard_exits`; or after three failed reads of the tracker in a row.
Work continues by itself when the credit returns or the tracker can be read
again. After either of the two limits, an authorized `再開` comment continues
it.

- Used in: README.md:1434, README.md:1546, README.md:1633
- Not the same as: stop.
- Avoid: none.

## Figurative words

These words describe one thing as another. Write what happens instead: what
is read, what is compared, what decides, and what reached where. Searching for
`印` also finds `目印`, and `ゲートウェイ` is a real way to connect, not this
kind of word.

- Avoid: `gate`; `ゲート` except in `ゲートウェイ`; `印`; `片肺`; `completion mark`; `completion stamp`; `mark of quality`; `certificate check`; `answer certificate`; `output certificate`; `completion certificate`

## Waiting for rewording

These words to avoid still appear in the documents. A separate change rewords
them and removes each word from this list together with its last use. Until
then the scan reports where they are without failing, and it fails when a word
listed here no longer appears. `再実行` in README.md quotes the engine's own
notice, cmd/engine/turns.go:249, so it leaves this list only together with a
change to that text.

- `creator`
- `起票者`
- `対象プロジェクト`
- `トラッカー`
- `ticket`
- `課題`
- `checkpoint`
- `役割`
- `担当`
- `再実行`
- `step`
- `段`
- `graph`
- `選定役`
- `entrance`
- `要件詰め`
- `要件確認`
- `打ち返`
- `回答`
- `返事`
- `返信`
- `review seat`
- `delivery destination`
- `納品地点`
- `announcement`
- `gate`
- `印`
- `completion mark`
- `completion stamp`
- `mark of quality`
- `certificate check`
- `answer certificate`
- `output certificate`
- `completion certificate`
