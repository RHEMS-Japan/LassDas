"""Adversarial review of one change, as the operator's own command. Fixed process.

This is not a model tool and it grades no model's prose. It hands a reviewing
model, chosen by the operator and normally from a different publisher than
the worker, the runtime's own text for this stage (from stdin: where the
stage sits, the original request, the settled requirements, the previous
reports), the diff of the change and the output of the operator's test
commands, and asks for one structured verdict: blocking or not, and the
findings. The exit status is the only thing the runtime reads: 1 sends the
work back to the stage the operator named, 0 lets it through. The findings
are printed, so they join the history for the worker and the report writer.
The send-back counter and a log stay in the process's own directory
(TASK_HOME) and only inform: a verdict stands whether or not they were saved.

No verdict, no pass. The command exits 0 only on a verdict that does not
object and 1 only on one that does; without a verdict it does neither.
Every call of the verdict tool in a reply is read, and in each every field
named blocking in any letter case, taken as true or false when its meaning
is plain: true or false, a number equal to 1 or 0, or "true", "yes", "1",
"false", "no" or "0" in any case. One that reads as true sends the work
back; with none true, one that reads as false lets it through; anything else
is no verdict, and what the reviewer wrote with it is shown in the live view
and kept in the printed result and the log. Trouble with
the model service (a connection that fails or times out, an HTTP error, a
reply without a verdict), and anything unexpected, is waited out: the models
are asked in the operator's order, round after round, the wait between
rounds growing from REVIEW_RETRY_SECONDS to REVIEW_RETRY_CAP_SECONDS, and the
printed result names the model that gave the verdict. What asking again
cannot get past (a setting that is missing or mistyped, an endpoint that is
not HTTPS, a credential that is not set, test commands that cannot be read)
holds the review: the reason is said once, then a short line every
REVIEW_HOLD_SECONDS. A workspace or TASK_HOME that cannot be used, or a
change that cannot be read, is looked at again at each of those, and the
review goes on once it can. What the review is doing is said on stderr when
it changes, for the live view. The runtime's own notice tells the requester
when a stage runs long; an operator who fixes a setting restarts the engine,
which launches the stage afresh.

REVIEW_UNAVAILABLE=pass is the operator's opt-in for the old behaviour, and
it delivers unreviewed work when no verdict can be obtained: after
REVIEW_ATTEMPTS requests, or at once where the review would hold, it prints
NOT REVIEWED with the reason and exits 0. One exception stands even then:
when Git lists no changed path at all and no earlier delivery round
committed one, work let through can end with nothing delivered, so it is let
through only on a verdict, and without one this exits 1.

When no file was changed at all, the reviewer is told so in plain words and
asked whether the request is met by the repository exactly as it is: a
request whose answer is that nothing needs to change reaches review this way,
and so does work that was never done. When REVIEW_DIFF_PATHS matches none of
what changed, the whole change is shown instead of none of it.

Environment (all from the operator, never from a role):
  TASK_WORKSPACE          the checkout holding the change
  REVIEW_MODEL_URL        chat-completions endpoint (HTTPS, or loopback for tests)
  REVIEW_MODELS           models to ask, newline- or comma-separated, in order of preference
  REVIEW_MODEL            the one model to ask when REVIEW_MODELS is not set
  REVIEW_KEY_ENV          name of the variable holding the credential (default REVIEW_API_KEY)
  REVIEW_TEST_COMMANDS    newline-separated commands run without a shell; their output is shown
  REVIEW_DIFF_PATHS       optional space-separated paths to diff (default: the whole tree)
  TASK_HOME               the process's own directory, where the send-back counter and the log live
  REVIEW_UNAVAILABLE      pass: deliver unreviewed work when no verdict can be obtained (default: hold)
  REVIEW_TIMEOUT_SECONDS  the limit of one request (default 300)
  REVIEW_RETRY_SECONDS, REVIEW_RETRY_CAP_SECONDS: the first and the longest wait between rounds (default 5, 300)
  REVIEW_HOLD_SECONDS     the wait between looks while the review holds (default 900)
  REVIEW_ATTEMPTS         with REVIEW_UNAVAILABLE=pass, the requests made before giving up (default 3)

The credential is sent only to REVIEW_MODEL_URL, in the Authorization header,
and is scrubbed from anything this command prints or writes.
"""
import json
import os
from pathlib import Path
import shlex
import ssl
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

LIMIT = 20000       # characters of diff shown; a longer diff is cut with a visible marker
NEW_FILE_LIMIT = 6000
HEAD, TAIL = 12000, 6000  # the runtime's text: its start (assignment, request, settled requirements) and its end (latest reports)

TOOL = {"type": "function", "function": {
    "name": "verdict",
    "description": "Your review verdict on the change as it stands.",
    "parameters": {"type": "object", "properties": {
        "blocking": {"type": "boolean",
                     "description": "true only if a defect in the change must be fixed before it can be delivered"},
        "findings": {"type": "string",
                     "description": "each defect, where it is and why it matters; empty when there is none"}},
        "required": ["blocking", "findings"]}}}

SYSTEM = ("You are an adversarial reviewer of one code change. Your job is to find defects in that change"
          " that would make the delivered result wrong, unsafe or not what was asked: behaviour that"
          " contradicts the request or the settled requirements, broken or missing tests for the requested"
          " behaviour, changes outside the requested scope, data loss, crashes on ordinary input. Ignore"
          " taste, naming and style. The build of the release artifact, its verification after delivery"
          " and the report to the requester are later stages of this run, done by the runtime's own"
          " commands after your verdict: their absence now is not a defect and must not block. Decide"
          " blocking only when a defect in the change itself must be fixed before delivery; say exactly"
          " where each defect is and why it matters, so the implementer can act on it. The work has"
          " been sent back %d times so far; an objection already raised and addressed is not raised"
          " again. Answer with the verdict tool.")

# Handed in place of the diff when there is none at all.
NO_CHANGE = ("No file was changed. Judge whether the request and the settled requirements are satisfied with the"
             " repository exactly as it is; if a change is needed and none was made, that is a blocking defect.")

# The shipped delivery process's receipt. A round of it that committed put
# the change in HEAD, so a diff against HEAD no longer shows that change.
DELIVERY_RECEIPT = Path(".git", "ticket-engine", "delivery.json")


class ReviewError(Exception):
    """Something that keeps a verdict from being obtained and that asking the
    model service again cannot get past. A setting has to be fixed by the
    operator; a workspace or a TASK_HOME that cannot be used (recheck) is
    looked at again while the review holds."""

    def __init__(self, message, recheck=False):
        super().__init__(message)
        self.recheck = recheck


def setting(name, default=None):
    value = os.environ.get(name, "")
    if value == "" and default is None:
        raise ReviewError("%s is not set" % name)
    return value if value != "" else default


def number(name, default, least=0):
    value = setting(name, default)
    try:
        parsed = int(value)
    except ValueError:
        raise ReviewError("%s must be a whole number, not %r" % (name, value))
    if parsed < least:
        raise ReviewError("%s must be at least %d, not %d" % (name, least, parsed))
    return parsed


def seconds(name, default):
    value = setting(name, default)
    try:
        parsed = float(value)
    except ValueError:
        raise ReviewError("%s must be a number of seconds, not %r" % (name, value))
    if parsed <= 0:
        raise ReviewError("%s must be more than 0 seconds, not %r" % (name, value))
    return parsed


def models():
    """The reviewing models in the operator's order of preference:
    REVIEW_MODELS, newline- or comma-separated, or REVIEW_MODEL alone."""
    listed = [name.strip() for name in os.environ.get("REVIEW_MODELS", "").replace(",", "\n").splitlines()
              if name.strip()]
    return listed or [setting("REVIEW_MODEL")]


def named_models():
    """The models as they can be named before the settings are read."""
    try:
        return ", ".join(models())
    except ReviewError:
        return "(no model named)"


def credential():
    return os.environ.get(os.environ.get("REVIEW_KEY_ENV", "") or "REVIEW_API_KEY", "")


def scrub(text, secret):
    if not secret or len(secret) < 8:
        return text
    return text.replace(secret, "[credential]").replace(urllib.parse.quote(secret, safe=""), "[credential]")


def say(text):
    """What the command is doing, for the live view: on stderr, flushed, so
    it shows while the review waits and never mixes with the verdict."""
    print(scrub(text, credential()), file=sys.stderr, flush=True)


def run(command, cwd, timeout):
    """One operator command: what it printed, its exit status, or why it
    could not start. None of it is a verdict."""
    try:
        finished = subprocess.run(command, cwd=cwd, capture_output=True, text=True, timeout=timeout)
    except subprocess.TimeoutExpired:
        return "$ %s\n(timed out after %d seconds)" % (" ".join(command), timeout)
    except OSError as error:
        return "$ %s\n(could not start: %s)" % (" ".join(command), error)
    output = (finished.stdout + finished.stderr).strip()
    if len(output) > 4000:
        output = "[test output cut here: the last 4000 of %d characters shown]\n" % len(output) + output[-4000:]
    return "$ %s -> exit %d\n%s" % (" ".join(command), finished.returncode, output)


def cut(text, limit, what):
    if len(text) <= limit:
        return text
    return text[:limit] + "\n[%s cut here: %d of %d characters shown]\n" % (what, limit, len(text))


def git(workspace, *arguments, names=False):
    """Git's output, read without the user's or the system's Git settings, as
    the delivery reads the checkout: with one of them (an exclude file, say),
    the two could disagree on whether anything changed. Names come as they
    are, not as octal escapes, so a diff header in Japanese is read as it was
    written; a name that is not UTF-8 is kept usable as a path when names is
    set, and otherwise shows a replacement character."""
    environment = dict(os.environ, GIT_CONFIG_GLOBAL=os.devnull, GIT_CONFIG_SYSTEM=os.devnull,
                       GIT_CONFIG_NOSYSTEM="1")
    try:
        finished = subprocess.run(["git", "-c", "core.quotePath=false", "-C", str(workspace), *arguments],
                                  capture_output=True, timeout=60, env=environment)
    except (OSError, subprocess.TimeoutExpired) as error:
        raise ReviewError("the change could not be read: git %s: %s" % (arguments[0], error), recheck=True)
    if finished.returncode != 0:
        raise ReviewError("the change could not be read: git %s exited %d: %s"
                          % (arguments[0], finished.returncode,
                             finished.stderr.decode("utf-8", "replace").strip()[:200]), recheck=True)
    return finished.stdout.decode("utf-8", "surrogateescape" if names else "replace")


def shown(name):
    """A name as it can be printed and sent: bytes that are not UTF-8 become
    a replacement character."""
    return name.encode("utf-8", "surrogateescape").decode("utf-8", "replace")


def changed_entries(workspace, *scope):
    """Every path Git lists as changed, staged or untracked, as "XY path".
    Read separated by NUL, where Git quotes and escapes no name: read from the
    ordinary listing, a name in Japanese or with a quote in it named no file."""
    output = git(workspace, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames", *scope,
                 names=True)
    return [entry for entry in output.split("\0") if len(entry) > 3]


def gather(workspace, paths, test_commands, timeout):
    """The diff, whole, cut only at LIMIT with a visible marker; then the
    operator's test commands, each with its exit status and output."""
    scope = ["--", *paths] if paths else []
    tracked = git(workspace, "diff", "HEAD", *scope)
    entries = changed_entries(workspace, *scope)
    note = ""
    if paths and not tracked.strip() and not entries and changed_entries(workspace):
        # The paths matched none of what changed: the reviewer is shown the
        # whole change rather than nothing, since that would be no review.
        note = ("REVIEW_DIFF_PATHS (%s) matched no change, so the whole change is shown.\n\n" % " ".join(paths))
        tracked, entries = git(workspace, "diff", "HEAD"), changed_entries(workspace)
    # New files come first, so their names survive a cut of a long diff: new
    # code is where untested code most often is. A new path that is not a
    # file the reviewer can read is still named, never left out silently.
    new_files = ""
    for entry in entries:
        if not entry.startswith("?? "):
            continue
        path = workspace / entry[3:]
        name = shown(entry[3:])
        if path.is_symlink():
            new_files += "--- new symbolic link %s -> %s ---\n" % (name, shown(os.readlink(path)))
        elif path.is_file():
            try:
                content = path.read_text(encoding="utf-8", errors="replace")
            except OSError as error:
                new_files += "--- new file %s, which could not be read: %s ---\n" % (name, shown(str(error)))
                continue
            new_files += "--- new file %s ---\n%s\n" % (name, cut(content, NEW_FILE_LIMIT, "new file"))
        else:
            new_files += "--- new path %s, which is not a regular file ---\n" % name
    tests = "\n\n".join(run(shlex.split(command), workspace, timeout) for command in test_commands if command.strip())
    return note + cut(new_files + tracked, LIMIT, "diff"), cut(tests, LIMIT, "test output")


def committed_earlier(workspace):
    """Whether an earlier delivery round of this request committed its change
    in this checkout. An empty diff then means only that nothing changed
    since that round, so it is not called "no file was changed": told that,
    the reviewer would send back for good a change the worker cannot make
    again, such as one whose merge waits on the service's own checks."""
    try:
        receipt = json.loads((workspace / DELIVERY_RECEIPT).read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return False
    return isinstance(receipt, dict) and bool(receipt.get("head"))


def nothing_changed(workspace):
    """Whether the checkout holds no change at all: Git lists no changed,
    staged or untracked path, as the delivery reads it too, and no earlier
    delivery round committed one. Work let through from here can end with
    nothing delivered. False whenever that cannot be told."""
    if not workspace:
        return False
    workspace = Path(workspace)
    try:
        return workspace.is_dir() and not changed_entries(workspace) and not committed_earlier(workspace)
    except (ReviewError, OSError):
        return False


def prepare():
    """The settings, read before anything is asked, and the workspace and
    TASK_HOME, which a held review looks at again."""
    workspace = Path(setting("TASK_WORKSPACE"))
    url = setting("REVIEW_MODEL_URL")
    parsed = urllib.parse.urlsplit(url)
    if parsed.scheme != "https" and parsed.hostname not in ("127.0.0.1", "localhost"):
        raise ReviewError("REVIEW_MODEL_URL must be https")
    names = models()
    key_env = setting("REVIEW_KEY_ENV", "REVIEW_API_KEY")
    key = os.environ.get(key_env, "")
    if not key:
        raise ReviewError("the review credential is not set (%s, named by REVIEW_KEY_ENV)" % key_env)
    found = {"workspace": workspace, "url": url, "secure": parsed.scheme == "https", "models": names, "key": key,
             "timeout": number("REVIEW_TIMEOUT_SECONDS", "300", 1), "attempts": number("REVIEW_ATTEMPTS", "3", 1),
             "retry": seconds("REVIEW_RETRY_SECONDS", "5"), "cap": seconds("REVIEW_RETRY_CAP_SECONDS", "300"),
             "paths": setting("REVIEW_DIFF_PATHS", "").split()}
    try:
        found["tests"] = [line for line in setting("REVIEW_TEST_COMMANDS", "").splitlines() if line.strip()]
        for line in found["tests"]:
            shlex.split(line)
    except ValueError as error:
        raise ReviewError("REVIEW_TEST_COMMANDS could not be read: %s" % error)
    # The process's own directory (TASK_HOME) keeps the send-back counter and
    # the log. The workspace is never written; the findings reach the worker
    # through the history because this command prints them.
    home = os.environ.get("TASK_HOME", "")
    if not home:
        raise ReviewError("TASK_HOME is not set; this command keeps its state there and writes nothing into the workspace")
    found["state"] = Path(home)
    if not workspace.is_dir():
        raise ReviewError("TASK_WORKSPACE is not a directory", recheck=True)
    try:
        found["state"].mkdir(parents=True, exist_ok=True)
    except OSError as error:
        raise ReviewError("TASK_HOME cannot be used: %s" % error, recheck=True)
    return found


def truth(value):
    """A blocking value read as true or false when its meaning is plain: true
    or false; a number equal to 1 or 0; "true", "yes" or "1", or "false", "no"
    or "0", in any letter case and without spaces around it. None when it
    cannot be read."""
    if isinstance(value, bool):
        return value
    if isinstance(value, (int, float)):
        return True if value == 1 else False if value == 0 else None
    if isinstance(value, str):
        word = value.strip().lower()
        return True if word in ("true", "yes", "1") else False if word in ("false", "no", "0") else None
    return None


def read_verdict(calls):
    """(blocking, findings, why) from every call of the verdict tool in one
    reply. Every field named blocking, in any letter case, is read in every
    call. One that reads as true sends the work back, so a finding is never
    let through because the reply also said false somewhere; with none true,
    one that reads as false lets the work through; with neither there is no
    verdict, and why says so. The findings of every call are kept whichever
    way it goes, so the worker and the report writer can read them."""
    values, findings, unread = [], [], []
    for call in calls:
        try:
            arguments = json.loads(call["function"]["arguments"])
        except (KeyError, TypeError, ValueError) as error:
            unread.append(type(error).__name__)
            continue
        if not isinstance(arguments, dict):
            unread.append("not a set of named fields")
            continue
        for name, value in arguments.items():
            if str(name).lower() == "blocking":
                values.append(value)
            elif str(name).lower() == "findings" and value not in (None, ""):
                findings.append(value if isinstance(value, str) else json.dumps(value, ensure_ascii=False))
    read = [truth(value) for value in values]
    text = "\n".join(findings)
    if True in read:
        return True, text, None
    if False in read:
        return False, text, None
    if values:
        return None, text, ("the reviewer's verdict gave no blocking that reads as true or false (it gave %s)"
                            % ", ".join(json.dumps(value, ensure_ascii=False)[:40] for value in values))
    if unread:
        return None, text, "the reviewer's verdict could not be read (%s)" % ", ".join(unread)
    return None, text, "the reviewer's verdict gave no blocking"


def ask_once(found, model, prompt, diff, tests, rounds):
    """One request to one model: (blocking, findings, None) on a verdict,
    (None, what the reviewer wrote, why) otherwise."""
    request = {"model": model, "temperature": 0.2, "tools": [TOOL],
               "tool_choice": {"type": "function", "function": {"name": "verdict"}},
               "messages": [
                   {"role": "system", "content": SYSTEM % rounds},
                   {"role": "user", "content": (
                       "Where this stage sits, the original request and the settled requirements (from the runtime):\n%s"
                       "\n\n[...]\n\nThe most recent reports:\n%s\n\nDiff of the change:\n%s\n\nTest output:\n%s"
                       % (prompt[:HEAD], prompt[-TAIL:] if len(prompt) > HEAD else "",
                          diff or "(no change)", tests or "(no test command configured)"))}]}
    body = json.dumps(request, ensure_ascii=False).encode()
    headers = {"Authorization": "Bearer " + found["key"], "Content-Type": "application/json"}
    context = ssl.create_default_context(cafile=os.environ.get("SSL_CERT_FILE") or None) if found["secure"] else None
    try:
        with urllib.request.urlopen(urllib.request.Request(found["url"], body, headers), timeout=found["timeout"],
                                    context=context) as response:
            reply = json.loads(response.read().decode("utf-8", errors="replace"))
        calls = ((reply.get("choices") or [{}])[0].get("message") or {}).get("tool_calls") or []
        if not calls:
            return None, "", "the reviewer returned no verdict"
        return read_verdict(calls)
    except urllib.error.HTTPError as error:
        return None, "", "HTTP %d from the model service" % error.code
    except Exception as error:  # a model service hiccup is not a defect in the change
        return None, "", type(error).__name__ + ": " + scrub(str(error), found["key"])[:200]


def keep_unclear(model, findings, unclear):
    """What a model wrote without a plain verdict, once per text, for the
    record the review ends with. Whether it is new."""
    entry = "%s: %s" % (model, findings)
    if not findings or entry in unclear:
        return False
    unclear.append(entry)
    return True


def verdict_until_given(found, prompt, diff, tests, rounds):
    """Ask until a model gives a verdict. Each round asks the operator's
    models in order; when none answers, the wait before the next round grows
    from REVIEW_RETRY_SECONDS to REVIEW_RETRY_CAP_SECONDS. The live view is
    told when what goes wrong changes, not on every request, and is shown
    what a model wrote without a plain verdict, since this may go on a while.
    Returns (model, blocking, findings, what was written without a verdict)."""
    wait, told, asked, unclear = 0, None, 0, []
    while True:
        failures = []
        for model in found["models"]:
            asked += 1
            blocking, findings, why = ask_once(found, model, prompt, diff, tests, rounds)
            if why is None:
                if told is not None:
                    say("Review: %s gave a verdict, after %d request%s that got none."
                        % (model, asked - 1, "" if asked == 2 else "s"))
                return model, blocking, findings, unclear
            if keep_unclear(model, findings, unclear):
                say("Review: %s wrote, without a plain verdict: %s" % (model, findings[:2000]))
            failures.append("%s: %s" % (model, why))
        wait = min(max(wait * 2, found["retry"]), found["cap"])
        if failures != told:
            say("Review: no verdict yet (%s). Asking again in %gs, then at waits growing to %gs; until a model"
                " gives a verdict the work is neither let through nor sent back." % ("; ".join(failures), wait, found["cap"]))
            told = failures
        time.sleep(wait)


def verdict_or_none(found, prompt, diff, tests, rounds):
    """With REVIEW_UNAVAILABLE=pass: REVIEW_ATTEMPTS requests, the models in
    turn, then (None, None, why, ...) when none gave a verdict. The last item
    is what was written without a verdict, as for verdict_until_given."""
    last, unclear = "", []
    for attempt in range(found["attempts"]):
        model = found["models"][attempt % len(found["models"])]
        blocking, findings, why = ask_once(found, model, prompt, diff, tests, rounds)
        if why is None:
            return model, blocking, findings, unclear
        keep_unclear(model, findings, unclear)
        last = why if len(found["models"]) == 1 else "%s: %s" % (model, why)
        if attempt + 1 < found["attempts"]:
            time.sleep(min(5 * (attempt + 1), 20))
    return None, None, last, unclear


def without_verdict(model, reason, unchanged, goes_on):
    """With REVIEW_UNAVAILABLE=pass, no verdict was obtained. With nothing
    changed, letting the work through could end it with nothing delivered and
    no one's judgement, so it goes back; otherwise it goes on unreviewed, as
    goes_on says."""
    if unchanged:
        print("Review by %s: NOT REVIEWED. %s. No file was changed, and an ending with nothing delivered needs a"
              " verdict, so the work goes back this time." % (model, reason))
        return 1
    print("Review by %s: NOT REVIEWED. %s. %s" % (model, reason, goes_on))
    return 0


def reviewed(stdin_text, unchanged, passing):
    """One review: the verdict's exit status, or, with REVIEW_UNAVAILABLE=pass
    and no verdict, the old pass-through."""
    found = prepare()
    key, state = found["key"], found["state"]
    counter = state / "review-send-backs"
    try:
        sent_back = int(counter.read_text().strip() or "0") if counter.is_file() else 0
    except (OSError, ValueError) as error:
        # The count only informs the reviewer; it decides nothing.
        say("Review: the send-back counter at %s could not be read (%s); counting from 0." % (counter, error))
        sent_back = 0
    diff, test_output = gather(found["workspace"], found["paths"], found["tests"], found["timeout"])
    if unchanged:
        diff = NO_CHANGE
    ask = verdict_or_none if passing else verdict_until_given
    model, blocking, findings, unclear = ask(found, stdin_text, diff, test_output, sent_back)
    outcome, status = "PASSED", 0
    if model is None:
        model, outcome = ", ".join(found["models"]), "NOT REVIEWED"
        if unchanged:
            findings = ("no verdict could be obtained (%s); no file was changed, and an ending with nothing delivered"
                        " needs a verdict, so the work goes back this time" % findings)
            status = 1
        else:
            findings = "no verdict could be obtained (%s); the work goes on unreviewed this time" % findings
        blocking = False
    elif blocking:
        outcome, status = "SENT BACK to the worker", 1
        sent_back += 1
    if unclear:
        # What the reviewer wrote without a verdict stays in the record.
        findings = (findings + "\n\n" if findings else "") + "Written without a plain verdict:\n" + "\n".join(unclear)
    findings = scrub(findings, key)
    print("Review by %s: %s. Send-backs so far: %d.\n%s"
          % (model, outcome, sent_back, (findings or "(no findings)")[:6000]))
    try:
        if blocking:
            counter.write_text(str(sent_back))
        with (state / "review.md").open("a", encoding="utf-8") as log:
            log.write("## Review by %s (send-backs so far: %d): %s\n\n%s\n\n" % (
                model, sent_back, outcome, findings or "(no findings)"))
    except OSError as error:
        # The count and the log only inform; the outcome above stands.
        print("Review by %s: the send-back state was not saved (%s); the outcome above stands."
              % (model, scrub(str(error), key)))
    return status


def waits(name, default):
    """A wait the operator set; its default when the setting itself is what
    cannot be read, which is then the reason the review holds."""
    try:
        return seconds(name, default)
    except ReviewError:
        return float(default)


def main():
    passing = os.environ.get("REVIEW_UNAVAILABLE", "") == "pass"
    try:
        # Read as bytes and decode leniently: a stray byte in the runtime's
        # text must not become a traceback.
        stdin_text = sys.stdin.buffer.read().decode("utf-8", errors="replace") if not sys.stdin.isatty() else ""
    except (OSError, ValueError):
        stdin_text = ""
    unchanged, held, told, wait = False, None, None, 0
    while True:
        try:
            # Read before any setting: a setting that keeps the review from
            # running must not let an unchanged checkout through either.
            unchanged = nothing_changed(os.environ.get("TASK_WORKSPACE", ""))
            return reviewed(stdin_text, unchanged, passing)
        except ReviewError as error:
            reason = scrub(str(error), credential())
            if passing:
                return without_verdict(named_models(), reason, unchanged,
                                       "The work goes on unreviewed this time; nothing here is a verdict on the change.")
            # No verdict can be obtained, and none is pretended: the review
            # holds where it is. A setting only the operator can fix; the
            # workspace and TASK_HOME are looked at again at each interval.
            interval = waits("REVIEW_HOLD_SECONDS", "900")
            if reason != held:
                say("Review by %s: held, with no verdict: %s. The work is neither let through nor sent back; %s"
                    % (named_models(), reason, "this is looked at again every %gs." % interval if error.recheck else
                       "fix the setting and restart the engine, which launches this stage afresh."))
                held = reason
            else:
                say("Review still held at %s; the reason is above." % time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()))
            time.sleep(interval)
        except Exception as error:
            reason = "Unexpected %s: %s" % (type(error).__name__, scrub(str(error), credential())[:300])
            if passing:
                return without_verdict(named_models(), reason, unchanged, "The work goes on unreviewed this time.")
            # Never a pass: an unexpected error is waited out like trouble
            # with the model service, and the review starts again.
            wait = min(max(wait * 2, waits("REVIEW_RETRY_SECONDS", "5")), waits("REVIEW_RETRY_CAP_SECONDS", "300"))
            if reason != told:
                say("Review by %s: %s. Starting the review again in %gs; until a model gives a verdict the work is"
                    " neither let through nor sent back." % (named_models(), reason, wait))
                told = reason
            time.sleep(wait)


if __name__ == "__main__":
    sys.exit(main())
