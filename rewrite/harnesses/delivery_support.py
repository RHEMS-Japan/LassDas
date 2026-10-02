"""Shared pieces of the fixed delivery processes; never a model tool.

These programs run configured operations only. Every value comes from the
operator's environment, not from a role's answer, and the credential never
reaches a command line, a remote URL or the printed report: Git asks for it
through the credential helper below, which answers one configured host.
"""
import datetime
import http.client
import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

SUPPORT = Path(__file__).resolve()
RECEIPT = Path(".git/ticket-engine/delivery.json")


class DeliveryError(RuntimeError):
    """A refusal or a failed operation. The process exits non-zero."""


class TransientError(DeliveryError):
    """A failure that may pass on its own, so the operation is retried.

    Everything else is treated as a refusal and ends the process at once:
    waiting out a protected branch, a missing permission or a conflicting
    merge only delays the report that a person has to read anyway.
    """


# What the service says when the answer may differ later.
TRANSIENT_STATUS = frozenset({408, 429, 500, 502, 503, 504})
# What Git's transport says for the same kind of failure. A refusal marker
# wins over a transient one, and anything unrecognized is a refusal.
TRANSIENT_GIT = ("could not resolve host", "temporary failure in name resolution",
                 "failed to connect", "connection refused", "connection reset",
                 "connection timed out", "operation timed out", "timed out",
                 "the remote end hung up unexpectedly", "rpc failed", "early eof",
                 "ssl_read", "gnutls", "returned error: 429", "returned error: 500",
                 "returned error: 502", "returned error: 503", "returned error: 504",
                 "remote end hung up", "unexpected disconnect")
REFUSAL_GIT = ("authentication failed", "invalid username or password", "permission denied",
               "repository not found", "returned error: 401", "returned error: 403",
               "returned error: 404", "protected branch", "non-fast-forward",
               "does not exist", "could not read from remote repository")


def transient_git(text):
    lowered = text.lower()
    if any(marker in lowered for marker in REFUSAL_GIT):
        return False
    return any(marker in lowered for marker in TRANSIENT_GIT)


def with_retry(operation, describe):
    """Run an operation, waiting out failures that may pass on their own."""
    attempts = max(1, int(number("DELIVERY_RETRY_ATTEMPTS", 5)))
    delay = number("DELIVERY_RETRY_SECONDS", 30)
    cap = number("DELIVERY_RETRY_CAP_SECONDS", 240)
    for attempt in range(1, attempts + 1):
        try:
            return operation()
        except TransientError as error:
            if attempt >= attempts:
                raise DeliveryError("%s did not go through in %d attempts, and the last failure was one "
                                    "that can pass on its own: %s" % (describe, attempts, error))
            print("%s failed in a way that may pass (attempt %d of %d); waiting %gs. %s"
                  % (describe, attempt, attempts, delay, error), flush=True)
            time.sleep(delay)
            delay = min(delay * 2, cap)


def setting(name, default=None):
    value = os.environ.get(name, "")
    if value == "" and default is None:
        raise DeliveryError("Operator setting " + name + " is unset")
    return value if value != "" else default


def number(name, default):
    value = os.environ.get(name, "")
    if value == "":
        return default
    try:
        parsed = float(value)
    except ValueError:
        raise DeliveryError("Operator setting " + name + " is not a number")
    if parsed <= 0:
        raise DeliveryError("Operator setting " + name + " must be positive")
    return parsed


def credential():
    value = os.environ.get("GITHUB_TOKEN", "")
    if not value:
        raise DeliveryError("No delivery credential was provided to this process")
    return value


def scrub(text):
    """Replace the credential wherever it reached a captured stream."""
    secret = os.environ.get("GITHUB_TOKEN", "")
    if not text or not secret:
        return text or ""
    return text.replace(secret, "[credential]")


def repository():
    value = setting("DELIVERY_REPOSITORY")
    owner, separator, name = value.partition("/")
    if not owner or not separator or not name or "/" in name:
        raise DeliveryError("DELIVERY_REPOSITORY must name one owner/repository")
    return owner, name


def remote_url(owner, name):
    """The operator's target. A test or enterprise host overrides it."""
    return os.environ.get("DELIVERY_REMOTE_URL", "") or ("https://github.com/" + owner + "/" + name + ".git")


def api_base():
    return os.environ.get("DELIVERY_API_BASE", "").rstrip("/") or "https://api.github.com"


# Settings Git takes from the environment: a count of numbered keys and values,
# and the -c settings a Git passes to the Gits it starts. In one stage's
# environment only, they would make the review and the delivery disagree on
# whether anything changed. adversarial_review.py drops the same; a test keeps
# the two lists the same.
GIT_SETTINGS = re.compile(r"GIT_CONFIG_(COUNT|PARAMETERS|KEY_\d+|VALUE_\d+)")


def git_environment(*, with_credential=True):
    """Git's own configuration only: no personal, repository or system file.
    Settings handed to Git through the environment are dropped as well."""
    environment = {name: value for name, value in os.environ.items() if not GIT_SETTINGS.fullmatch(name)}
    for name in ("GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR",
                 "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_ASKPASS", "SSH_ASKPASS"):
        environment.pop(name, None)
    environment.update(GIT_CONFIG_GLOBAL=os.devnull, GIT_CONFIG_SYSTEM=os.devnull,
                       GIT_CONFIG_NOSYSTEM="1", GIT_TERMINAL_PROMPT="0", LC_ALL="C")
    if not with_credential:
        environment.pop("GITHUB_TOKEN", None)
    return environment


def git(*arguments, url=None):
    """A Git command line. The credential stays in the environment."""
    # A role may write project files; a planted repository hook must never run
    # inside this trusted process. Global and system configuration are already
    # disabled through the environment above.
    command = ["git", "-c", "core.hooksPath=" + os.devnull]
    if url is not None:
        host = urllib.parse.urlsplit(url)
        if host.scheme == "https":
            helper = "!" + " ".join(shlex.quote(part) for part in
                                    (sys.executable, "-B", str(SUPPORT), "--credential-helper"))
            os.environ["DELIVERY_CREDENTIAL_HOST"] = host.hostname or ""
            command += ["-c", "credential.helper=", "-c", "credential.helper=" + helper]
    return command + list(arguments)


def run(command, *, cwd=None, check=True, timeout=None, environment=None,
        stdin=subprocess.DEVNULL, redact=True):
    """Run a configured command and keep its scrubbed output for the report.

    Pass redact=False only for text that is examined and never printed: the
    credential check below has to see what is actually there.
    """
    result = subprocess.run(command, cwd=cwd, env=environment or git_environment(), text=True,
                            stdin=stdin, capture_output=True, timeout=timeout)
    output = result.stdout if not redact else scrub(result.stdout)
    diagnostics = scrub(result.stderr)
    if check and result.returncode != 0:
        raise DeliveryError("Command failed (exit %d): %s\n%s%s"
                            % (result.returncode, shlex.join(command), output, diagnostics))
    return result.returncode, output, diagnostics


def api(method, path, *, payload=None, timeout=None):
    """One GitHub REST call. The credential travels in a header, not a URL."""
    url = path if path.startswith("http") else api_base() + path
    body = json.dumps(payload).encode() if payload is not None else None
    request = urllib.request.Request(url, data=body, method=method)
    request.add_header("Authorization", "Bearer " + credential())
    request.add_header("Accept", "application/vnd.github+json")
    request.add_header("X-GitHub-Api-Version", "2022-11-28")
    request.add_header("User-Agent", "ticket-engine-delivery")
    if body is not None:
        request.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(request, timeout=timeout or number("DELIVERY_API_TIMEOUT_SECONDS", 60)) as response:
            text = response.read().decode("utf-8", "replace")
            status = response.status
    except urllib.error.HTTPError as error:
        text, status = error.read().decode("utf-8", "replace"), error.code
    except urllib.error.URLError as error:
        # Not reaching the service at all is the clearest transient failure.
        raise TransientError("the delivery service was unreachable: " + scrub(str(error.reason)))
    except (http.client.HTTPException, OSError) as error:
        # The connection dropped, timed out, or the answer could not be read.
        # Nothing was refused here, so this waits and asks again.
        raise TransientError("the connection to the delivery service did not complete: "
                             + scrub("%s: %s" % (type(error).__name__, error)))
    text = scrub(text).strip()
    if not text:
        return status, {}
    try:
        return status, json.loads(text)
    except ValueError:
        # An unparsed body is still evidence of what the service answered.
        return status, {"message": text[:500]}


def api_retried(method, path, *, payload=None, describe=None):
    """One REST call, waiting out an answer that may differ later."""
    def attempt():
        status, body = api(method, path, payload=payload)
        if status in TRANSIENT_STATUS:
            raise TransientError("the service answered %d" % status)
        return status, body
    return with_retry(attempt, describe or ("%s %s" % (method, path.split("?")[0])))


def run_git(command, *, describe, timeout=None, retry=True, cwd=None):
    """One Git command, separating a failure that may pass from a refusal."""
    def attempt():
        try:
            status, output, diagnostics = run(command, check=False, timeout=timeout, cwd=cwd)
        except subprocess.TimeoutExpired:
            raise TransientError("it did not finish within the configured time")
        if status != 0:
            text = (diagnostics + "\n" + output).strip()
            if transient_git(text):
                raise TransientError(text.splitlines()[0] if text else "no message")
            raise DeliveryError("Git could not %s: %s" % (describe, text[:800] or "no message"))
        return output
    return with_retry(attempt, describe) if retry else attempt()


def timestamp():
    """One machine-readable UTC instant, for the receipt only."""
    return datetime.datetime.now(datetime.timezone.utc).isoformat(timespec="seconds").replace("+00:00", "Z")


def receipt_path(workspace):
    return Path(workspace) / RECEIPT


def read_receipt(path):
    try:
        data = json.loads(Path(path).read_text())
    except FileNotFoundError:
        return {}
    except ValueError:
        raise DeliveryError("The delivery receipt exists but is not readable JSON: " + str(path))
    if not isinstance(data, dict):
        raise DeliveryError("The delivery receipt is not a record: " + str(path))
    return data


def write_receipt(path, data):
    """Replace the receipt atomically; an interrupted write leaves the old one."""
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name("." + path.name + ".new")
    temporary.write_text(json.dumps(data, indent=2, sort_keys=True) + "\n")
    os.replace(temporary, path)


def credential_helper(arguments, stdin, stdout):
    """Answer Git's credential request for the one configured host.

    Any other host, protocol or operation gets no answer: Git then fails to
    authenticate instead of sending the operator's credential elsewhere.
    """
    request = {}
    for line in stdin:
        line = line.rstrip("\n")
        if not line:
            break
        key, _, value = line.partition("=")
        request[key] = value
    if not arguments or arguments[0] != "get":
        return 0
    expected = os.environ.get("DELIVERY_CREDENTIAL_HOST", "")
    if not expected or request.get("host") != expected or request.get("protocol") != "https":
        return 0
    stdout.write("username=%s\npassword=%s\n"
                 % (os.environ.get("DELIVERY_CREDENTIAL_USERNAME", "") or "x-access-token", credential()))
    return 0


def discard_prompt():
    """The engine writes the role prompt to stdin. A fixed process has no use
    for it: read it so the engine never blocks, and decide nothing from it."""
    try:
        sys.stdin.read()
    except (OSError, ValueError):
        pass


def main(entry):
    """Shared entry point. A refusal is a process failure, never a summary."""
    try:
        return entry(sys.argv[1:])
    except DeliveryError as error:
        print(scrub(str(error)), file=sys.stderr)
        return 1


if __name__ == "__main__":
    # Git runs this module as its credential helper; it has no other command.
    if sys.argv[1:2] != ["--credential-helper"]:
        print("This module is the delivery credential helper", file=sys.stderr)
        sys.exit(2)
    sys.exit(credential_helper(sys.argv[2:], sys.stdin, sys.stdout))
