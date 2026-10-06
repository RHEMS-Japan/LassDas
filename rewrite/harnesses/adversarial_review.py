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
object and 1 on one that does. An empty or oversized selected PR report also
returns 1 for the worker to repair, explicitly before model review. Otherwise,
without a verdict it does neither.
Every call of the verdict tool in a reply is read, and in each every field
named blocking in any letter case, taken as true or false when its meaning
is plain: true or false, a number equal to 1 or 0, or "true", "yes", "1",
"false", "no" or "0" in any case. One that reads as true sends the work
back; with none true, one that reads as false lets it through; anything else
is no verdict, and what the reviewer wrote with it is shown in the live view
and kept in the printed result and the log. A call's arguments are read as
JSON text or as an object, and one object further in, as {"verdict": {...}}
is: a true found there always counts, anything else there only in a call
that names no blocking itself. What is kept is the findings of each
call at those two levels, arguments that cannot be read, and words given
instead of a call, cut where findings are; findings further in, or inside a
list, are not read.
Trouble with the model service (a connection that fails or times out, an
HTTP error, a reply without a verdict) is waited out: the models are asked
in the operator's order, a model named twice once a round, round after
round, the wait between rounds growing from REVIEW_RETRY_SECONDS to
REVIEW_RETRY_CAP_SECONDS, and the printed result names the model that gave
the verdict. An HTTP error is said with what the service answered, scrubbed
and cut to about 200 characters. Permanent request, authentication
and model-setting refusals remove that model from further attempts; other
configured models are still asked. HTTP 402 stays retryable when credit returns.
HTTP 413 and context-length 400 reduce
the supplied material and retry; an unrecognized 400 gets one reduced attempt
before that model is refused. The omission is stated to the model and
in the result. The proposed PR explanation remains complete. When every
model requires an operator correction, no model is called again until restart.
What asking
again cannot get past (a setting that is missing or mistyped, an endpoint
that is not HTTPS, a credential that is not set, test commands that cannot
be read, a REVIEW_UNAVAILABLE other than pass or none) holds the review: the
reason is said once, then a short line every REVIEW_HOLD_SECONDS. A
workspace or TASK_HOME that cannot be used, or a change that cannot be read,
is looked at again at each of those, and the review goes on once it can.
Anything unexpected starts the review again at the growing waits, said again
every REVIEW_HOLD_SECONDS, without running the operator's test commands
again. What the review is doing is said on stderr when it changes, and
again every REVIEW_HOLD_SECONDS while it does not, for the live view. The
runtime's own notice tells the requester when a stage runs long. An operator
who fixes a setting restarts the engine: the runtime records the stopped
review as an interruption and goes on at the review's on_failure stage
(requirements in the ordered example), and the review runs again after repair.

REVIEW_UNAVAILABLE=pass is the operator's opt-in for the old behaviour, and
it delivers unreviewed work when no verdict can be obtained: after
REVIEW_ATTEMPTS requests, or at once where the review would hold, it prints
NOT REVIEWED with the reason and exits 0. Description settings that the
worker cannot fix still hold until the operator corrects them and restarts.
Another exception stands even then:
when Git lists no changed path at all and no earlier delivery round
committed one, work let through can end with nothing delivered, so it is let
through only on a verdict, and without one this exits 1. So it does when
whether anything changed cannot be told (no checkout, or Git cannot read it,
or not in time), since the delivery reads the checkout on its own, waits
longer, and may find no change. Once the change has been read, what Git
listed while reading it decides, whatever the first look found.

When no file was changed at all, the reviewer is told so in plain words and
asked whether the request is met by the repository exactly as it is: a
request whose answer is that nothing needs to change reaches review this way,
and so does work that was never done. When REVIEW_DIFF_PATHS matches none of
what changed, the whole change is shown instead of none of it, and the note
above it says when that had to be cut; when the paths match part of the
change, a note says that the rest is not shown.

Environment (all from the operator, never from a role):
  TASK_WORKSPACE          the checkout holding the change
  REVIEW_MODEL_URL        chat-completions endpoint (HTTPS, or loopback for tests)
  REVIEW_MODELS           models to ask, newline- or comma-separated, in order of preference
  REVIEW_MODEL            the one model to ask when REVIEW_MODELS is not set
  REVIEW_KEY_ENV          name of the variable holding the credential (default REVIEW_API_KEY)
  REVIEW_TEST_COMMANDS    newline-separated commands run without a shell; their output is shown
  REVIEW_DIFF_PATHS       optional space-separated paths to diff (default: the whole tree)
  REVIEW_MEMORY_CHARACTERS positive body-character budget for saved review material (default 48000)
  TASK_HOME               the process's own directory, where the send-back counter and the log live
  TASK_HISTORY            runtime-supplied read-only checkpoint, not an operator-selected document
  REVIEW_UNAVAILABLE      pass, in any case: deliver unreviewed work when no verdict can be obtained (default: hold)
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
import re
import shlex
import ssl
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import delivery_support

LIMIT = 20000       # characters of diff shown; a longer diff is cut with a visible marker
NEW_FILE_LIMIT = 6000
FINDINGS_LIMIT = 6000  # characters of findings printed, and of a reply kept as written when it gives no verdict
HEAD, TAIL = 12000, 6000  # the runtime's text: its start (assignment, request, settled requirements) and its end (latest reports)
REVIEW_LOG_LIMIT = 12000  # characters from the existing ordinary log, explicitly a tail
HISTORY_BYTES = 64 << 20  # local checkpoint read bound, not model context size

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
          " again. Label each finding in ordinary prose as Act on (must fix before delivery), Consider,"
          " Noted or Dismissed, explaining why. Only an Act on defect justifies blocking; the other"
          " dispositions are not reasons to block. Look for design problems where the same workaround"
          " appears in unrelated places, or callers must know internal rules to use an implementation"
          " safely. Explain the concrete defect; do not demand a speculative redesign. These labels"
          " guide the reader, not an additional response schema. Answer with the verdict tool.")

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
    REVIEW_MODELS, newline- or comma-separated, or REVIEW_MODEL alone. A model
    named twice is asked once a round."""
    listed = []
    for name in os.environ.get("REVIEW_MODELS", "").replace(",", "\n").splitlines():
        if name.strip() and name.strip() not in listed:
            listed.append(name.strip())
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
        finished = subprocess.run(command, cwd=cwd, capture_output=True, timeout=timeout)
    except subprocess.TimeoutExpired:
        return "$ %s\n(timed out after %d seconds)" % (" ".join(command), timeout)
    except OSError as error:
        return "$ %s\n(could not start: %s)" % (" ".join(command), error)
    # Read as bytes: output that is not UTF-8 shows a replacement character
    # instead of ending the review in an error it would start again from.
    # CR and CRLF end a line, as when the output was read as text, so a test
    # tool that redraws its progress with CR shows each step on its own line.
    output = "".join(stream.decode("utf-8", "replace").replace("\r\n", "\n").replace("\r", "\n")
                     for stream in (finished.stdout, finished.stderr)).strip()
    if len(output) > 4000:
        output = "[test output cut here: the last 4000 of %d characters shown]\n" % len(output) + output[-4000:]
    return "$ %s -> exit %d\n%s" % (" ".join(command), finished.returncode, output)


def cut(text, limit, what):
    if len(text) <= limit:
        return text
    return text[:limit] + "\n[%s cut here: %d of %d characters shown]\n" % (what, limit, len(text))


# Settings Git takes from the environment: a count of numbered keys and values,
# and the -c settings a Git passes to the Gits it starts. The delivery's Git on
# the workspace drops the same (delivery_support); a test keeps the lists the same.
GIT_SETTINGS = re.compile(r"GIT_CONFIG_(COUNT|PARAMETERS|KEY_\d+|VALUE_\d+)")


def git_environment():
    """The environment the review runs Git in; see git()."""
    environment = {name: value for name, value in os.environ.items() if name not in (
        "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_ALTERNATE_OBJECT_DIRECTORIES",
        "GIT_ATTR_SOURCE", "GIT_EXTERNAL_DIFF") and not GIT_SETTINGS.fullmatch(name)}
    # Error excerpts below recognize Git's fatal/error prefixes. Keep Git's
    # diagnostics in that language without changing the role's test commands.
    environment.update(GIT_CONFIG_GLOBAL=os.devnull, GIT_CONFIG_SYSTEM=os.devnull, GIT_CONFIG_NOSYSTEM="1", LC_ALL="C")
    return environment


def git(workspace, *arguments, names=False):
    """Git's output, read as the delivery reads the checkout: without the
    user's or the system's Git settings, without settings handed to it through
    the environment, without Git's default exclude and attributes files, and
    without the variables that point Git at another repository, index or work
    tree, or pick its attributes or its diff program. With one of them (an
    exclude file, an index of its own, a clean checkout elsewhere) the two could
    disagree on whether anything changed. The delivery's lists are in
    delivery_support, which the review's bundle does not carry, so the same
    lists are kept here. Names come as they are, not as octal escapes, so a diff
    header in Japanese is read as it was written; a name that is not UTF-8 is
    kept usable as a path when names is set, and otherwise shows a replacement
    character."""
    try:
        finished = subprocess.run(["git", "-c", "core.quotePath=false", "-c", "core.excludesFile=" + os.devnull,
                                   "-c", "core.attributesFile=" + os.devnull, "-C", str(workspace), *arguments],
                                  capture_output=True, timeout=60, env=git_environment())
    except (OSError, subprocess.TimeoutExpired) as error:
        raise ReviewError("the change could not be read: git %s: %s" % (arguments[0], error), recheck=True)
    if finished.returncode != 0:
        # One line of Git's: the one saying fatal or error, else its first.
        # The rest, such as the usage text after a warning, would fill the
        # live view at every interval the review holds.
        lines = finished.stderr.decode("utf-8", "replace").strip().splitlines()
        why = next((line for line in lines if line.startswith(("fatal:", "error:"))), lines[0] if lines else "")
        raise ReviewError("the change could not be read: git %s exited %d: %s"
                          % (arguments[0], finished.returncode, why[:200]), recheck=True)
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


def gather(workspace, paths, test_commands, timeout, ran):
    """The diff, whole, cut only at LIMIT with a visible marker; then the
    operator's test commands, each with its exit status and output; and
    whether the checkout holds no change at all, from what Git listed here,
    so that the review that shows the change also decides that from the same
    reading, never from one that failed. The commands run once a review: ran
    keeps their output for a review that starts again after an error, so they
    are not run again every few seconds while it waits."""
    scope = ["--", *paths] if paths else []
    tracked = git(workspace, "diff", "HEAD", *scope)
    entries = changed_entries(workspace, *scope)
    listed, note = entries, ""
    if paths:
        listed = changed_entries(workspace)
        if not tracked.strip() and not entries and listed:
            # The paths matched none of what changed: the reviewer is shown
            # the whole change rather than nothing, since that would be no
            # review. Whether it then had to be cut is said below.
            note = "REVIEW_DIFF_PATHS (%s) matched no change, so the whole change is shown" % " ".join(paths)
            tracked, entries = git(workspace, "diff", "HEAD"), listed
        elif set(listed) - set(entries):
            note = "Changes outside REVIEW_DIFF_PATHS (%s) are not shown.\n\n" % " ".join(paths)
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
    change = new_files + tracked
    if note.startswith("REVIEW_DIFF_PATHS"):
        note += (", cut at %d of its %d characters where it says so.\n\n" % (LIMIT, len(change))
                 if len(change) > LIMIT else ".\n\n")
    if "tests" not in ran:
        ran["tests"] = "\n\n".join(run(shlex.split(command), workspace, timeout)
                                    for command in test_commands if command.strip())
    unchanged = not listed and not tracked.strip() and not committed_earlier(workspace)
    return note + cut(change, LIMIT, "diff"), cut(ran["tests"], LIMIT, "test output"), unchanged


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
    nothing delivered. None when that cannot be told (no checkout, or Git
    could not read it, or not in time): the delivery reads the checkout on
    its own and waits longer, so it may still find no change, and not being
    able to tell is never taken as a change."""
    if not workspace:
        return None
    workspace = Path(workspace)
    try:
        if not workspace.is_dir():
            return None
        return not changed_entries(workspace) and not committed_earlier(workspace)
    except (ReviewError, OSError):
        return None


def held_back(unchanged):
    """Why work without a verdict goes back, when it may hold no change:
    there is none, or that could not be told."""
    return "no file was changed" if unchanged else "whether any file was changed could not be told"


def prepare():
    """The settings, read before anything is asked, and the workspace and
    TASK_HOME, which a held review looks at again."""
    # A model outage may be bypassed, but cannot repair a description setting.
    description, note, description_error = "", "", ""
    try:
        description, note = delivery_support.description_report()
    except delivery_support.DescriptionSettingError:
        raise
    except delivery_support.DeliveryError as error:
        description_error = str(error)
    workspace = Path(setting("TASK_WORKSPACE"))
    url = setting("REVIEW_MODEL_URL")
    parsed = urllib.parse.urlsplit(url)
    if parsed.scheme != "https" and parsed.hostname not in ("127.0.0.1", "localhost"):
        raise ReviewError("REVIEW_MODEL_URL must be https")
    names = models()
    unavailable = os.environ.get("REVIEW_UNAVAILABLE", "")
    if unavailable.strip().lower() not in ("", "pass"):
        # Not taken quietly as the default: the operator meant something.
        raise ReviewError("REVIEW_UNAVAILABLE is %r; it may be pass, or left unset to wait for a verdict" % unavailable)
    key_env = setting("REVIEW_KEY_ENV", "REVIEW_API_KEY")
    key = os.environ.get(key_env, "")
    if not key:
        raise ReviewError("the review credential is not set (%s, named by REVIEW_KEY_ENV)" % key_env)
    found = {"workspace": workspace, "url": url, "secure": parsed.scheme == "https", "models": names, "key": key,
             "timeout": number("REVIEW_TIMEOUT_SECONDS", "300", 1), "attempts": number("REVIEW_ATTEMPTS", "3", 1),
             "retry": seconds("REVIEW_RETRY_SECONDS", "5"), "cap": seconds("REVIEW_RETRY_CAP_SECONDS", "300"),
             "hold": seconds("REVIEW_HOLD_SECONDS", "900"),
             "memory_characters": number("REVIEW_MEMORY_CHARACTERS", "48000", 1),
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
    found.update(description=scrub(description, key), description_note=scrub(note, key),
                 description_error=scrub(description_error, key))
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


def as_written(value):
    """What a reply said, as text the record can keep, cut where findings are."""
    text = value if isinstance(value, str) else json.dumps(value, ensure_ascii=False)
    return cut(text, FINDINGS_LIMIT, "reply") if text.strip() else ""


def read_verdict(calls):
    """(blocking, findings, why) from every call of the verdict tool in one
    reply. Arguments are read as a JSON text or as an object as they come.
    Every field named blocking, in any letter case, is read in every call
    and in the objects one level inside it, such as {"verdict": {...}}; one
    level inside, a value that does not read as true counts only in a call
    that names no blocking itself. Findings are read at both levels. One
    that reads as true sends the work back, so a finding is never let
    through because the reply also said false somewhere; with none true,
    one that reads as false lets the work through; with neither there is no
    verdict, and why says so. The findings of every call are kept whichever
    way it goes, and arguments that cannot be read are kept as they were
    written, so the worker and the report writer can read them."""
    values, findings, unread = [], [], []
    for call in calls:
        function = call.get("function") if isinstance(call, dict) else None
        arguments = function.get("arguments") if isinstance(function, dict) else None
        if isinstance(arguments, str):
            try:
                arguments = json.loads(arguments)
            except ValueError:
                unread.append("arguments that are not JSON")
                findings.append(as_written(arguments))
                continue
        if not isinstance(arguments, dict):
            unread.append("arguments that are not a set of named fields")
            if arguments not in (None, ""):
                findings.append(as_written(arguments))
            continue
        below = [item for value in arguments.values() if isinstance(value, dict) for item in value.items()]
        given = [value for name, value in arguments.items() if str(name).lower() == "blocking"]
        nested = [value for name, value in below if str(name).lower() == "blocking"]
        # A true one object down counts whatever the call says itself, so an
        # objection written there is never let through for a false beside it.
        # Anything else there stands in only for a call that names no blocking
        # itself, as {"verdict": {...}} does: beside a blocking the call gave
        # but that cannot be read, a false found further in decides nothing.
        values.extend(given + [value for value in nested if truth(value) is True] if given else nested)
        for name, value in list(arguments.items()) + below:
            if str(name).lower() == "findings" and value not in (None, ""):
                findings.append(value if isinstance(value, str) else json.dumps(value, ensure_ascii=False))
    findings = [text for text in findings if text]
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


def said_in(message):
    """What a reply said in words rather than through the verdict tool."""
    content = message.get("content") if isinstance(message, dict) else None
    if isinstance(content, list):
        content = "\n".join(str(part.get("text", "")) if isinstance(part, dict) else str(part) for part in content)
    return as_written(content) if isinstance(content, str) else ""


def review_memory(state_directory, key, limit):
    """Read existing sources, not a model-authored summary or resolution schema.

    Latest reports are selected by recorded role/process identity and workflow
    kind only. Their words do not establish that an objection was addressed.
    A finite tail of the old log is ordinary prose: headings inside findings
    are not trusted record delimiters.
    """
    sections, notices = [], []
    path = os.environ.get("TASK_HISTORY")
    if path:
        try:
            with Path(path).open("rb") as saved:
                raw = saved.read(HISTORY_BYTES + 1)
            if len(raw) > HISTORY_BYTES:
                raise ValueError("checkpoint exceeds the local 64 MiB read limit")
            state = json.loads(raw.decode("utf-8"))
            history = state["history"]
            current = state["pending"]["role"]
            request = state["request"]
            if not isinstance(history, list) or not isinstance(current, str) or not current or not isinstance(request, str):
                raise ValueError("checkpoint lacks the current request, history or pending role")
            stages = (state.get("workflow") or {}).get("stages") or []
            model_roles = {stage["name"] for stage in stages if stage["kind"] == "model"}
            latest, accepted = {}, None
            for index, record in enumerate(history):
                role, speaker = record["role"], record["speaker"]
                if not isinstance(role, str) or not isinstance(speaker, str) or not isinstance(record.get("output", ""), str):
                    raise ValueError("checkpoint has a malformed report")
                if speaker == "requester":
                    if role:
                        accepted = index
                    else:
                        # A runtime control supplement is not a question answer.
                        latest[role, speaker] = index
                elif speaker != "runtime" and (not stages or role in model_roles or role == current):
                    latest[role, speaker] = index
            selected = set(latest.values())
            if accepted is not None:
                selected.add(accepted)
                # The preceding question-role reports are retained alongside
                # the accepted answer, even when that role has worked again.
                role = history[accepted]["role"]
                index = accepted - 1
                while index >= 0 and history[index]["role"] == role and history[index]["speaker"] != "requester":
                    if history[index]["speaker"] != "runtime":
                        selected.add(index)
                    index -= 1
            protected = [("Canonical original request", scrub(request, key))]
            for index in sorted(selected):
                record = history[index]
                label = json.dumps({"record": index, "role": record["role"], "speaker": record["speaker"],
                                    "finished_at": record.get("finished_at", "")}, ensure_ascii=False)
                protected.append(("Saved report " + label, scrub(record.get("output", ""), key)))
            shortened = sum(len(body) for _, body in protected) > limit
            share = limit // len(protected)
            displayed = []
            for label, body in protected:
                if shortened and len(body) > share:
                    start = (share + 1) // 2
                    end = len(body) - (share - start)
                    body = (body[:start] + "\n[characters %d:%d omitted from this body]\n"
                            % (start, end) + body[end:])
                displayed.append(label + ":\n" + body)
            text = "\n\n".join(displayed)
            text.encode("utf-8")
            if shortened:
                notices.append("Saved review context exceeded the %d-character budget; body ranges were omitted." % limit)
            sections.append("Selected canonical reports, with any omitted body ranges explicitly named. "
                            "These are observations, not proof that a defect was repaired. "
                            "Older reports remain in the checkpoint but are not all included here.\n" + text)
        except (OSError, UnicodeError, ValueError, KeyError, TypeError, AttributeError, RecursionError) as error:
            notice = scrub("Saved review context could not be read: %s. "
                           "Reviewing the runtime text, available review log, current diff and test output; "
                           "the unread saved material is not represented as remembered." % error, key)
            sections.append(notice)
            notices.append(notice)
            say(notice)
    log = state_directory / "review.md"
    try:
        with log.open("rb") as written:
            written.seek(0, os.SEEK_END)
            start = max(0, written.tell() - 4 * REVIEW_LOG_LIMIT)
            written.seek(start)
            raw = written.read(4 * REVIEW_LOG_LIMIT)
        if start:
            # A seek can land inside one UTF-8 character. Discard only that
            # partial leading character of an explicitly omitted prefix.
            while raw and raw[0] & 0xC0 == 0x80:
                raw = raw[1:]
        text = raw.decode("utf-8")
        cut = bool(start) or len(text) > REVIEW_LOG_LIMIT
        if cut:
            notices.append("Previous review log exceeded its 12000-character display limit; earlier text was omitted.")
        text = text[-REVIEW_LOG_LIMIT:]
        sections.append("Previous review log (ordinary prose, not a verdict for this change; "
                        + ("only its last 12000 characters, earlier text omitted" if cut else "the complete existing log")
                        + "):\n" + text)
    except FileNotFoundError:
        pass
    except (OSError, UnicodeError) as error:
        # The log only informs, as when writing it failed in the prior run.
        notice = "Previous review log could not be read; it is not represented as remembered: " + str(error)
        sections.append(notice)
        notices.append(notice)
    return scrub("\n\n".join(sections), key), scrub(" ".join(notices), key)


def review_material(found, model, prompt, diff, tests):
    """Keep each source identifiable when a service requires less context.
    The separately reviewed PR explanation is never cut by this budget."""
    recent = prompt[-TAIL:] if len(prompt) > HEAD else ""
    if len(prompt) > HEAD + TAIL:
        recent = "[...]\n" + recent
    parts = [("Where this stage sits, the original request and the settled requirements (from the runtime)", prompt[:HEAD]),
             ("The most recent reports", recent),
             ("Saved review context", found.get("memory") or "(no additional saved context available)"),
             ("Diff of the change", diff or "(no change)"),
             ("Test output", tests or "(no test command configured)")]
    budget = found.get("context_limits", {}).get(model)
    shown = []
    for label, body in parts:
        if budget is not None and len(body) > budget // len(parts):
            share = budget // len(parts)
            start, end = (share + 1) // 2, len(body) - share // 2
            body = body[:start] + "\n[characters %d:%d omitted from this body]\n" % (start, end) + body[end:]
        shown.append(label + ":\n" + body)
    note = found.get("context_notes", {}).get(model, "")
    return ((note + "\n\n") if note else "") + "\n\n".join(shown), sum(len(body) for _, body in parts)


def ask_once(found, model, prompt, diff, tests, rounds):
    """Ask one model, reducing oversized material before trying it again.
    Return a verdict or its absence; permanent refusals raise ReviewError."""
    if found.get("attempts_left") == 0:
        return None, "", "the configured review request limit was reached"
    if "attempts_left" in found:
        found["attempts_left"] -= 1
    found["requests_made"] = found.get("requests_made", 0) + 1
    material, amount = review_material(found, model, prompt, diff, tests)
    request = {"model": model, "temperature": 0.2, "tools": [TOOL],
               "tool_choice": {"type": "function", "function": {"name": "verdict"}},
               "messages": [
                   {"role": "system", "content": SYSTEM % rounds},
                   {"role": "user", "content": material}]}
    if found.get("description_note"):
        request["messages"][1]["content"] += "\n\n" + found["description_note"]
    if found.get("description"):
        request["messages"][1]["content"] += (
            "\n\nProposed pull request explanation (review these original words with the diff; "
            "they are not evidence that delivery already happened):\n" + found["description"])
    body = json.dumps(request, ensure_ascii=False).encode()
    headers = {"Authorization": "Bearer " + found["key"], "Content-Type": "application/json"}
    context = ssl.create_default_context(cafile=os.environ.get("SSL_CERT_FILE") or None) if found["secure"] else None
    try:
        with urllib.request.urlopen(urllib.request.Request(found["url"], body, headers), timeout=found["timeout"],
                                    context=context) as response:
            reply = json.loads(response.read().decode("utf-8", errors="replace"))
        message = (reply.get("choices") or [{}])[0].get("message") or {}
        calls = message.get("tool_calls") or []
        if not calls:
            # Words instead of the verdict tool: no verdict, but what was said
            # stays in the record.
            return None, said_in(message), "the reviewer returned no verdict"
        return read_verdict(calls)
    except urllib.error.HTTPError as error:
        shortened = found.setdefault("shortened_400", set())
        why, too_large = refused(error, found["key"], model not in shortened)
        if not too_large:
            return None, "", why
        if error.code == 400:
            shortened.add(model)
        limits = found.setdefault("context_limits", {})
        budget = min(limits.get(model, amount), found["memory_characters"]) // 2
        # At least both ends of each of the five sources must fit. A model
        # that still refuses this needs a different capacity/endpoint setting.
        if budget < 10:
            raise ReviewError(why + "; minimal review material was refused; configure a model that accepts the"
                              " required instructions and complete pull request explanation")
        limits[model] = budget
        note = ("Review material reduced after %s for %s; runtime text, saved context, diff and test bodies"
                " now share at most %d characters. Omitted ranges are not represented as reviewed;"
                " the proposed pull request explanation remains complete." % (why, model, budget))
        found.setdefault("context_notes", {})[model] = note
        say(note)
        log(found, note + "\n\n")
        return ask_once(found, model, prompt, diff, tests, rounds)
    except Exception as error:  # a model service hiccup is not a defect in the change
        return None, "", type(error).__name__ + ": " + scrub(str(error), found["key"])[:200]


# What the model service says with these is about the request the operator
# set up (a credential, a model id, a request too large), not a passing fault.
OPERATORS_TO_FIX = (400, 401, 403, 404, 422)


def refused(error, key, may_shorten_400):
    """Classify the service's refusal before shortening its diagnostic.
    This is transport metadata, not a judgment of the reviewer's prose."""
    try:
        said = error.read().decode("utf-8", "replace")
    except Exception:  # what it said only informs
        said = ""
    said = " ".join(scrub(said, key).split())
    context_reason = bool(re.search(
        r"context[_ -]length[_ -]exceeded|context.{0,80}(?:exceed|too (?:long|large))"
        r"|maximum context (?:length|window)|too many (?:input |prompt )?tokens"
        r"|(?:input|prompt).{0,40}tokens.{0,30}(?:exceed|limit)", said, re.I))
    too_large = error.code == 413 or error.code == 400 and (context_reason or may_shorten_400)
    reason = "HTTP %d from the model service%s" % (
        error.code, ": " + (said[:200] + "..." if len(said) > 200 else said) if said else "",
    )
    if error.code in OPERATORS_TO_FIX and not too_large:
        raise ReviewError(reason + "; this is for the operator to fix (the credential, model id or request)")
    return reason, too_large


def keep_unclear(found, model, findings, why, unclear):
    """What a model wrote without a plain verdict, once per text: kept for
    the result the review ends with, and logged at once, so that review.md
    tells what the model last said while the review is still asking. Whether
    it is new."""
    entry = "%s: %s" % (model, findings)
    if not findings or entry in unclear:
        return False
    unclear.append(entry)
    log(found, "## Review by %s: no verdict yet (%s)\n\n%s\n\n" % (model, why, findings))
    return True


def log(found, text):
    """Add to review.md in TASK_HOME. It only informs, so a log that cannot
    be written changes nothing."""
    try:
        with (found["state"] / "review.md").open("a", encoding="utf-8") as written:
            written.write(scrub(text, found["key"]))
    except OSError:
        pass


def verdict_until_given(found, prompt, diff, tests, rounds):
    """Ask until a model gives a verdict. Each round asks the operator's
    models in order; when none answers, the wait before the next round grows
    from REVIEW_RETRY_SECONDS to REVIEW_RETRY_CAP_SECONDS. The live view is
    told when what goes wrong changes, not on every request, and is shown
    what a model wrote without a plain verdict, since this may go on a while.
    Returns (model, blocking, findings, what was written without a verdict)."""
    wait, told, unclear, said_at = 0, None, [], time.monotonic()
    unavailable = {}
    while True:
        failures = []
        for model in found["models"]:
            if model in unavailable:
                continue
            try:
                blocking, findings, why = ask_once(found, model, prompt, diff, tests, rounds)
            except ReviewError as error:
                unavailable[model] = str(error)
                say("Review: not asking %s again: %s" % (model, error))
                continue
            if why is None:
                if told is not None:
                    asked = found["requests_made"]
                    say("Review: %s gave a verdict, after %d request%s that got none."
                        % (model, asked - 1, "" if asked == 2 else "s"))
                return model, blocking, findings, unclear
            if keep_unclear(found, model, findings, why, unclear):
                say("Review: %s wrote, without a plain verdict: %s" % (model, findings[:2000]))
            failures.append("%s: %s" % (model, why))
        if len(unavailable) == len(found["models"]):
            raise ReviewError("; ".join("%s: %s" % item for item in unavailable.items()))
        wait = min(max(wait * 2, found["retry"]), found["cap"])
        if failures != told:
            say("Review: no verdict yet (%s). Asking again in %gs, then at waits growing to %gs; until a model"
                " gives a verdict the work is neither let through nor sent back." % ("; ".join(failures), wait, found["cap"]))
            told, said_at = failures, time.monotonic()
        elif time.monotonic() - said_at >= found["hold"]:
            # Not one line for hours: the live view hears again at the hold
            # interval that the review is still waiting, and why.
            say("Review still without a verdict at %s, %d requests so far: %s."
                % (time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), found["requests_made"], "; ".join(failures)))
            said_at = time.monotonic()
        time.sleep(wait)


def verdict_or_none(found, prompt, diff, tests, rounds):
    """With REVIEW_UNAVAILABLE=pass: REVIEW_ATTEMPTS requests, the models in
    turn, then (None, None, why, ...) when none gave a verdict. The last item
    is what was written without a verdict, as for verdict_until_given."""
    last, unclear, unavailable, position = "", [], {}, 0
    found["attempts_left"] = found["attempts"]
    for attempt in range(found["attempts"]):
        while found["models"][position % len(found["models"])] in unavailable:
            position += 1
        model = found["models"][position % len(found["models"])]
        position += 1
        try:
            blocking, findings, why = ask_once(found, model, prompt, diff, tests, rounds)
        except ReviewError as error:
            unavailable[model] = str(error)
            if len(unavailable) == len(found["models"]):
                return None, None, "; ".join("%s: %s" % item for item in unavailable.items()), unclear
            blocking, findings, why = None, "", str(error)
        if why is None:
            return model, blocking, findings, unclear
        keep_unclear(found, model, findings, why, unclear)
        last = why if len(found["models"]) == 1 else "%s: %s" % (model, why)
        if found["attempts_left"] == 0:
            break
        if attempt + 1 < found["attempts"]:
            time.sleep(min(5 * (attempt + 1), 20))
    return None, None, last, unclear


def without_verdict(model, reason, unchanged, goes_on):
    """With REVIEW_UNAVAILABLE=pass, no verdict was obtained. With nothing
    changed, or when that cannot be told (unchanged is None), letting the work
    through could end it with nothing delivered and no one's judgement, so it
    goes back; otherwise it goes on unreviewed, as goes_on says."""
    if unchanged is not False:
        print("Review by %s: NOT REVIEWED. %s. %s, and an ending with nothing delivered needs a verdict, so the"
              " work goes back this time." % (model, reason, held_back(unchanged).capitalize()))
        return 1
    print("Review by %s: NOT REVIEWED. %s. %s" % (model, reason, goes_on))
    return 0


def reviewed(stdin_text, unchanged, passing, ran):
    """One review: the verdict's exit status, or, with REVIEW_UNAVAILABLE=pass
    and no verdict, the old pass-through. ran keeps what the operator's test
    commands printed, for a review that starts again."""
    found = prepare()
    key, state = found["key"], found["state"]
    if found.get("description_error"):
        reason = "Review: SENT BACK to the worker before model review: " + found["description_error"]
        print(reason)
        log(found, reason + "\n\n")
        return 1
    found["memory"], memory_notice = review_memory(state, key, found["memory_characters"])
    counter = state / "review-send-backs"
    try:
        sent_back = int(counter.read_text().strip() or "0") if counter.is_file() else 0
    except (OSError, ValueError) as error:
        # The count only informs the reviewer; it decides nothing.
        say("Review: the send-back counter at %s could not be read (%s); counting from 0." % (counter, error))
        sent_back = 0
    # Once the change is read, the reading that showed it decides whether
    # anything changed: not the look before, which may have failed, nor a
    # look after, which could fail in turn.
    diff, test_output, unchanged = gather(found["workspace"], found["paths"], found["tests"], found["timeout"], ran)
    if unchanged:
        diff = NO_CHANGE
    ask = verdict_or_none if passing else verdict_until_given
    model, blocking, findings, unclear = ask(found, stdin_text, diff, test_output, sent_back)
    outcome, status = "PASSED", 0
    if model is None:
        model, outcome = ", ".join(found["models"]), "NOT REVIEWED"
        if unchanged is not False:
            findings = ("no verdict could be obtained (%s); %s, and an ending with nothing delivered needs a verdict,"
                        " so the work goes back this time" % (findings, held_back(unchanged)))
            status = 1
        else:
            findings = "no verdict could be obtained (%s); the work goes on unreviewed this time" % findings
        blocking = False
    elif blocking:
        outcome, status = "SENT BACK to the worker", 1
        sent_back += 1
    if found.get("description_note"):
        findings = found["description_note"] + "\n" + findings
    if found.get("context_notes"):
        findings = "\n".join(found["context_notes"].values()) + "\n" + findings
    findings = scrub((memory_notice + "\n" if memory_notice else "") + findings, key)
    # What the reviewer wrote without a verdict stays in the record: in the
    # result printed here, and in review.md, where it was logged as written.
    printed = findings + (("\n\n" if findings else "") + "Written without a plain verdict:\n" + "\n".join(unclear)
                          if unclear else "")
    print("Review by %s: %s. Send-backs so far: %d.\n%s"
          % (model, outcome, sent_back, scrub(printed or "(no findings)", key)[:FINDINGS_LIMIT]))
    try:
        if blocking:
            counter.write_text(str(sent_back))
        with (state / "review.md").open("a", encoding="utf-8") as written:
            written.write("## Review by %s (send-backs so far: %d): %s\n\n%s\n\n" % (
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
    # Read as the operator may have written it; any other value than pass,
    # or none, holds the review until it is fixed (see prepare).
    passing = os.environ.get("REVIEW_UNAVAILABLE", "").strip().lower() == "pass"
    try:
        # Read as bytes and decode leniently: a stray byte in the runtime's
        # text must not become a traceback.
        stdin_text = sys.stdin.buffer.read().decode("utf-8", errors="replace") if not sys.stdin.isatty() else ""
    except (OSError, ValueError):
        stdin_text = ""
    # Until it has been told, whether anything changed is not known, and an
    # error before then lets nothing through without a verdict.
    unchanged, held, told, wait, said_at, ran = None, None, None, 0, 0, {}
    while True:
        try:
            # Read before any setting: a setting that keeps the review from
            # running must not let an unchanged checkout through either.
            unchanged = nothing_changed(os.environ.get("TASK_WORKSPACE", ""))
            return reviewed(stdin_text, unchanged, passing, ran)
        except (ReviewError, delivery_support.DescriptionSettingError) as error:
            reason = scrub(str(error), credential())
            if passing and not isinstance(error, delivery_support.DescriptionSettingError):
                return without_verdict(named_models(), reason, unchanged,
                                       "The work goes on unreviewed this time; nothing here is a verdict on the change.")
            # No verdict can be obtained, and none is pretended: the review
            # holds where it is. A setting only the operator can fix; the
            # workspace and TASK_HOME are looked at again at each interval.
            interval = waits("REVIEW_HOLD_SECONDS", "900")
            if reason != held:
                say("Review by %s: held, with no verdict: %s. The work is neither let through nor sent back; %s"
                    % (named_models(), reason, "this is looked at again every %gs." % interval
                       if isinstance(error, ReviewError) and error.recheck else
                       "fix the setting and restart the engine. The restarted runtime records this review as a"
                       " failure and goes on at the review's on_failure stage, and the review runs again after it."))
                held = reason
            else:
                say("Review still held at %s; the reason is above." % time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()))
            time.sleep(interval)
            if not isinstance(error, ReviewError) or not error.recheck:
                while True:
                    say("Review still held at %s; the reason is above."
                        % time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()))
                    time.sleep(interval)
        except Exception as error:
            reason = "Unexpected %s: %s" % (type(error).__name__, scrub(str(error), credential())[:300])
            if passing:
                return without_verdict(named_models(), reason, unchanged, "The work goes on unreviewed this time.")
            # Never a pass: an unexpected error is waited out like trouble
            # with the model service, at the same growing waits, and the
            # review starts again; the test commands are not run again.
            wait = min(max(wait * 2, waits("REVIEW_RETRY_SECONDS", "5")), waits("REVIEW_RETRY_CAP_SECONDS", "300"))
            if reason != told:
                say("Review by %s: %s. Starting the review again in %gs; until a model gives a verdict the work is"
                    " neither let through nor sent back." % (named_models(), reason, wait))
                told, said_at = reason, time.monotonic()
            elif time.monotonic() - said_at >= waits("REVIEW_HOLD_SECONDS", "900"):
                say("Review still starting again at %s; the reason is above."
                    % time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()))
                said_at = time.monotonic()
            time.sleep(wait)


if __name__ == "__main__":
    sys.exit(main())
