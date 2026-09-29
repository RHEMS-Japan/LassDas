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
the round counter and a log stay in the process's own directory (TASK_HOME).

Two things keep this from ending or stalling a request. A verdict that
cannot be obtained (the model service down, no verdict returned) lets the
work through with a note, because a review that is unavailable is not a
defect in the change. And the operator's round cap: once the review has sent
the work back that many times, the next blocking verdict lets the work
through with the objections recorded as unresolved.

Environment (all from the operator, never from a role):
  TASK_WORKSPACE          the checkout holding the change
  REVIEW_MODEL_URL        chat-completions endpoint (HTTPS, or loopback for tests)
  REVIEW_MODEL            model id as the endpoint names it
  REVIEW_KEY_ENV          name of the variable holding the credential (default REVIEW_API_KEY)
  REVIEW_TEST_COMMANDS    newline-separated commands run without a shell; their output is shown
  REVIEW_DIFF_PATHS       optional space-separated paths to diff (default: the whole tree)
  REVIEW_ROUNDS           send-backs allowed before objections are recorded and the work goes on (default 2)
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

LIMIT = 20000
HEAD, TAIL = 7000, 7000

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
          " where each defect is and why it matters, so the implementer can act on it. This is review"
          " round %d of at most %d; an objection already raised and addressed is not raised again."
          " Answer with the verdict tool.")


class ReviewError(Exception):
    pass


def setting(name, default=None):
    value = os.environ.get(name, "")
    if value == "" and default is None:
        raise ReviewError("%s is not set" % name)
    return value if value != "" else default


def scrub(text, secret):
    if not secret:
        return text
    return text.replace(secret, "[credential]").replace(urllib.parse.quote(secret, safe=""), "[credential]")


def run(command, cwd, timeout):
    try:
        finished = subprocess.run(command, cwd=cwd, capture_output=True, text=True, timeout=timeout)
    except subprocess.TimeoutExpired:
        return "$ %s\n(timed out after %d seconds)" % (" ".join(command), timeout)
    output = (finished.stdout + finished.stderr).strip()
    return "$ %s -> exit %d\n%s" % (" ".join(command), finished.returncode, output[-4000:])


def gather(workspace, paths, test_commands, timeout):
    diff = run(["git", "-C", str(workspace), "diff", "HEAD", "--", *paths] if paths else
               ["git", "-C", str(workspace), "diff", "HEAD"], workspace, 60)
    status = run(["git", "-C", str(workspace), "status", "--short", "--untracked-files=all", "--", *paths] if paths else
                 ["git", "-C", str(workspace), "status", "--short", "--untracked-files=all"], workspace, 60)
    for line in status.splitlines():
        if line.startswith("?? "):
            path = workspace / line[3:].strip()
            if path.is_file():
                try:
                    diff += "\n--- new file %s ---\n%s" % (line[3:].strip(), path.read_text(encoding="utf-8", errors="replace")[:6000])
                except OSError:
                    pass
    tests = "\n\n".join(run(shlex.split(command), workspace, timeout) for command in test_commands if command.strip())
    return diff[:LIMIT], tests[:LIMIT]


def ask(url, model, key, prompt, diff, tests, rounds, limit, timeout, attempts):
    """One structured verdict, or None when none could be obtained."""
    request = {"model": model, "temperature": 0.2, "tools": [TOOL],
               "tool_choice": {"type": "function", "function": {"name": "verdict"}},
               "messages": [
                   {"role": "system", "content": SYSTEM % (rounds + 1, limit)},
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
            if calls:
                verdict = json.loads(calls[0]["function"]["arguments"])
                return bool(verdict.get("blocking")), str(verdict.get("findings") or "")
            last = "the reviewer returned no verdict"
        except urllib.error.HTTPError as error:
            last = "HTTP %d from the model service" % error.code
        except Exception as error:  # a model service hiccup is not a defect in the change
            last = type(error).__name__ + ": " + scrub(str(error), key)[:200]
        time.sleep(min(5 * (attempt + 1), 20))
    return None, last


def review(stdin_text):
    workspace = Path(setting("TASK_WORKSPACE"))
    if not workspace.is_dir():
        raise ReviewError("TASK_WORKSPACE is not a directory")
    url = setting("REVIEW_MODEL_URL")
    model = setting("REVIEW_MODEL")
    key = os.environ.get(setting("REVIEW_KEY_ENV", "REVIEW_API_KEY"), "")
    if not key:
        raise ReviewError("the review credential is not set")
    limit = int(setting("REVIEW_ROUNDS", "2"))
    timeout = int(setting("REVIEW_TIMEOUT_SECONDS", "300"))
    attempts = int(setting("REVIEW_ATTEMPTS", "3"))
    paths = setting("REVIEW_DIFF_PATHS", "").split()
    tests = [line for line in setting("REVIEW_TEST_COMMANDS", "").splitlines() if line.strip()]

    # The process's own directory keeps the round counter and the log; the
    # workspace is not written, the findings reach the worker through the
    # history because this command prints them.
    report = Path(os.environ.get("TASK_HOME") or (workspace / "report"))
    report.mkdir(parents=True, exist_ok=True)
    rounds_file = report / "review-rounds"
    rounds = int(rounds_file.read_text()) if rounds_file.is_file() else 0

    diff, test_output = gather(workspace, paths, tests, timeout)
    blocking, findings = ask(url, model, key, stdin_text, diff, test_output, rounds, limit, timeout, attempts)
    if blocking is None:
        findings = "no verdict could be obtained (%s); the work goes on unreviewed this round" % findings
        blocking = False
    if blocking and rounds >= limit:
        findings = ("UNRESOLVED after %d review rounds, the operator's limit; the work goes on with these"
                    " objections recorded:\n%s" % (rounds, findings))
        blocking = False
    findings = scrub(findings, key)
    rounds_file.write_text(str(rounds + 1))
    with (report / "review.md").open("a", encoding="utf-8") as log:
        log.write("## Review round %d (%s): %s\n\n%s\n\n" % (
            rounds + 1, model, "SEND BACK" if blocking else "PASS", findings or "(no findings)"))
    print(("Review round %d by %s: SENT BACK to the worker.\n" if blocking else "Review round %d by %s: PASSED.\n")
          % (rounds + 1, model) + (findings or "(no findings)")[:6000])
    return 1 if blocking else 0


def main():
    stdin_text = sys.stdin.read() if not sys.stdin.isatty() else ""
    try:
        return review(stdin_text)
    except ReviewError as error:
        print("adversarial review: " + str(error), file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
