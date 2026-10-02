"""Adversarial review of one change, as the operator's own command. Fixed process.

This is not a model tool and it grades no model's prose. It hands a reviewing
model, chosen by the operator and normally from a different publisher than
the worker, the runtime's own text for this stage (from stdin: where the
stage sits, the original request, the settled requirements, the previous
reports), the diff of the change and the output of the operator's test
commands, and asks for one structured verdict: blocking or not, and the
findings. The exit status is the only thing the runtime reads: 1 sends the
work back to the stage the operator named, 0 lets it through. The findings
are printed, so they join the history for the worker and the report writer;
the send-back counter and a log stay in the process's own directory (TASK_HOME);
a change that cannot be read (no Git checkout) lets the work through with a note.

Only a real blocking verdict exits 1, so a review that could not be
performed does not send the work round for ever. Everything that keeps a
verdict from being obtained (a mistyped setting, a test command that cannot
start, an endpoint that is not HTTPS, a change that cannot be read, paths
that match no change, a model service that is down or returns no verdict)
ends 0 and prints NOT REVIEWED with the reason, which joins the history for
the worker and the report writer: a review that could not be performed is not
a defect in the change.

One exception: when Git lists no changed path at all and no earlier delivery
round committed one, the work let through here can end with nothing
delivered, so it is let through only on a verdict that does not object.
Without one (no verdict, a setting that keeps the review from running, an
unexpected error), this ends 1 and the work goes back. The reviewer is told
in plain words that no file was changed and asked whether the request is met
by the repository exactly as it is: a request whose answer is that nothing
needs to change reaches review this way, and so does work that was never
done.

Environment (all from the operator, never from a role):
  TASK_WORKSPACE          the checkout holding the change
  REVIEW_MODEL_URL        chat-completions endpoint (HTTPS, or loopback for tests)
  REVIEW_MODEL            model id as the endpoint names it
  REVIEW_KEY_ENV          name of the variable holding the credential (default REVIEW_API_KEY)
  REVIEW_TEST_COMMANDS    newline-separated commands run without a shell; their output is shown
  REVIEW_DIFF_PATHS       optional space-separated paths to diff (default: the whole tree)
  TASK_HOME               the process's own directory, where the send-back counter and the log live
  REVIEW_TIMEOUT_SECONDS, REVIEW_ATTEMPTS: optional (default 300, 3)

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
    pass


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


def scrub(text, secret):
    if not secret or len(secret) < 8:
        return text
    return text.replace(secret, "[credential]").replace(urllib.parse.quote(secret, safe=""), "[credential]")


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


def git(workspace, *arguments):
    # Names as they are, not as octal escapes: a diff header in Japanese is
    # read by the reviewer as it was written.
    try:
        finished = subprocess.run(["git", "-c", "core.quotePath=false", "-C", str(workspace), *arguments],
                                  capture_output=True, text=True, timeout=60)
    except (OSError, subprocess.TimeoutExpired) as error:
        raise ReviewError("the change could not be read: git %s: %s" % (arguments[0], error))
    if finished.returncode != 0:
        raise ReviewError("the change could not be read: git %s exited %d: %s"
                          % (arguments[0], finished.returncode, finished.stderr.strip()[:200]))
    return finished.stdout


def changed_entries(workspace, *scope):
    """Every path Git lists as changed, staged or untracked, as "XY path".
    Read separated by NUL, where Git quotes and escapes no name: read from the
    ordinary listing, a name in Japanese or with a quote in it named no file."""
    output = git(workspace, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames", *scope)
    return [entry for entry in output.split("\0") if len(entry) > 3]


def gather(workspace, paths, test_commands, timeout):
    """The diff, whole, cut only at LIMIT with a visible marker; then the
    operator's test commands, each with its exit status and output."""
    scope = ["--", *paths] if paths else []
    tracked = git(workspace, "diff", "HEAD", *scope)
    entries = changed_entries(workspace, *scope)
    if paths and not tracked.strip() and not entries:
        elsewhere = changed_entries(workspace)
        if elsewhere:
            raise ReviewError("REVIEW_DIFF_PATHS (%s) matched no change, but the checkout has changes under: %s"
                              % (" ".join(paths), ", ".join(sorted({entry[3:].split("/")[0] for entry in elsewhere}))))
    # New files come first, so their names survive a cut of a long diff: new
    # code is where untested code most often is. A new path that is not a
    # file the reviewer can read is still named, never left out silently.
    new_files = ""
    for entry in entries:
        if not entry.startswith("?? "):
            continue
        name = entry[3:]
        path = workspace / name
        if path.is_symlink():
            new_files += "--- new symbolic link %s -> %s ---\n" % (name, os.readlink(path))
        elif path.is_file():
            try:
                content = path.read_text(encoding="utf-8", errors="replace")
            except OSError as error:
                new_files += "--- new file %s, which could not be read: %s ---\n" % (name, error)
                continue
            new_files += "--- new file %s ---\n%s\n" % (name, cut(content, NEW_FILE_LIMIT, "new file"))
        else:
            new_files += "--- new path %s, which is not a regular file ---\n" % name
    tests = "\n\n".join(run(shlex.split(command), workspace, timeout) for command in test_commands if command.strip())
    return cut(new_files + tracked, LIMIT, "diff"), cut(tests, LIMIT, "test output")


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


def ask(url, model, key, prompt, diff, tests, rounds, timeout, attempts):
    """One structured verdict, or None when none could be obtained."""
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
    headers = {"Authorization": "Bearer " + key, "Content-Type": "application/json"}
    parsed = urllib.parse.urlsplit(url)
    if parsed.scheme != "https" and parsed.hostname not in ("127.0.0.1", "localhost"):
        raise ReviewError("REVIEW_MODEL_URL must be https")
    context = ssl.create_default_context(cafile=os.environ.get("SSL_CERT_FILE") or None)
    last = ""
    for attempt in range(attempts):
        try:
            with urllib.request.urlopen(urllib.request.Request(url, body, headers), timeout=timeout,
                                        context=context if parsed.scheme == "https" else None) as response:
                reply = json.loads(response.read().decode("utf-8", errors="replace"))
            calls = ((reply.get("choices") or [{}])[0].get("message") or {}).get("tool_calls") or []
            if not calls:
                last = "the reviewer returned no verdict"
            else:
                verdict = json.loads(calls[0]["function"]["arguments"])
                # The one field read strictly, since it decides whether the
                # work goes on: anything but true or false is no verdict.
                if isinstance(verdict, dict) and isinstance(verdict.get("blocking"), bool):
                    return verdict["blocking"], str(verdict.get("findings") or "")
                last = "the reviewer's verdict gave neither true nor false for blocking"
        except urllib.error.HTTPError as error:
            last = "HTTP %d from the model service" % error.code
        except Exception as error:  # a model service hiccup is not a defect in the change
            last = type(error).__name__ + ": " + scrub(str(error), key)[:200]
        if attempt + 1 < attempts:
            time.sleep(min(5 * (attempt + 1), 20))
    return None, last


def without_verdict(model, reason, unchanged, goes_on):
    """No verdict was obtained. With nothing changed, letting the work through
    could end it with nothing delivered and no one's judgement, so it goes
    back; otherwise it goes on unreviewed, as goes_on says."""
    if unchanged:
        print("Review by %s: NOT REVIEWED. %s. No file was changed, and an ending with nothing delivered needs a"
              " verdict, so the work goes back this time." % (model, reason))
        return 1
    print("Review by %s: NOT REVIEWED. %s. %s" % (model, reason, goes_on))
    return 0


def review(stdin_text, model, unchanged):
    """Exit 1 on a real blocking verdict, or when nothing changed and no
    verdict lets it through; 0 otherwise."""
    try:
        return reviewed(stdin_text, model, unchanged)
    except ReviewError as error:
        key = os.environ.get(os.environ.get("REVIEW_KEY_ENV", "REVIEW_API_KEY"), "")
        return without_verdict(model, scrub(str(error), key), unchanged,
                               "The work goes on unreviewed this time; nothing here is a verdict on the change.")


def reviewed(stdin_text, model, unchanged):
    workspace = Path(setting("TASK_WORKSPACE"))
    if not workspace.is_dir():
        raise ReviewError("TASK_WORKSPACE is not a directory")
    url = setting("REVIEW_MODEL_URL")
    setting("REVIEW_MODEL")
    key_env = setting("REVIEW_KEY_ENV", "REVIEW_API_KEY")
    key = os.environ.get(key_env, "")
    if not key:
        raise ReviewError("the review credential is not set (%s, named by REVIEW_KEY_ENV)" % key_env)
    timeout = number("REVIEW_TIMEOUT_SECONDS", "300", 1)
    attempts = number("REVIEW_ATTEMPTS", "3", 1)
    paths = setting("REVIEW_DIFF_PATHS", "").split()
    try:
        tests = [line for line in setting("REVIEW_TEST_COMMANDS", "").splitlines() if line.strip()]
        for line in tests:
            shlex.split(line)
    except ValueError as error:
        raise ReviewError("REVIEW_TEST_COMMANDS could not be read: %s" % error)

    # The process's own directory (TASK_HOME) keeps the send-back counter and
    # the log. The workspace is never written; the findings reach the worker
    # through the history because this command prints them.
    home = os.environ.get("TASK_HOME", "")
    if not home:
        raise ReviewError("TASK_HOME is not set; this command keeps its state there and writes nothing into the workspace")
    state = Path(home)
    try:
        state.mkdir(parents=True, exist_ok=True)
    except OSError as error:
        raise ReviewError("TASK_HOME cannot be used: %s" % error)
    counter = state / "review-send-backs"
    try:
        sent_back = int(counter.read_text().strip() or "0") if counter.is_file() else 0
    except ValueError:
        raise ReviewError("the send-back counter at %s is not a whole number" % counter)

    diff, test_output = gather(workspace, paths, tests, timeout)
    if unchanged:
        diff = NO_CHANGE
    blocking, findings = ask(url, model, key, stdin_text, diff, test_output, sent_back, timeout, attempts)
    outcome, status = "PASSED", 0
    if blocking is None:
        outcome = "NOT REVIEWED"
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
        if unchanged:
            # Nothing was changed: only a verdict that lets the work through
            # ends it here, whether or not the state was saved.
            print("Review by %s: the send-back state could not be saved (%s). No file was changed, so the outcome"
                  " above stands all the same." % (model, scrub(str(error), key)))
            return status
        # Without its state the send-backs are not counted, so this verdict
        # does not send the work back; the findings above are in the record.
        print("Review by %s: NOT REVIEWED. The send-back state could not be saved (%s), so the verdict above"
              " does not send the work back this time." % (model, scrub(str(error), key)))
        return 0
    return status


def main():
    model = os.environ.get("REVIEW_MODEL", "") or "(no model named)"
    # Read before any setting: a setting that keeps the review from running
    # must not let an unchanged checkout through either.
    unchanged = nothing_changed(os.environ.get("TASK_WORKSPACE", ""))
    try:
        # Read as bytes and decode leniently: a stray byte in the runtime's
        # text must not become a traceback, which an ordered run would read
        # as a send-back for ever.
        stdin_text = sys.stdin.buffer.read().decode("utf-8", errors="replace") if not sys.stdin.isatty() else ""
        return review(stdin_text, model, unchanged)
    except Exception as error:  # never a traceback, and exit 1 only when nothing changed
        key = os.environ.get(os.environ.get("REVIEW_KEY_ENV", "REVIEW_API_KEY"), "")
        return without_verdict(model, "Unexpected %s: %s" % (type(error).__name__, scrub(str(error), key)[:300]),
                               unchanged, "The work goes on unreviewed this time.")


if __name__ == "__main__":
    sys.exit(main())
