"""Shared pieces of the fixed delivery processes; never a model tool.

These programs run configured operations only. Authority comes from the
operator's environment, not from a role's answer, and the credential never
reaches a command line, a remote URL or the printed report: Git asks for it
through the credential helper below, which answers one configured host.
"""
import codecs
import datetime
import http.client
import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request

SUPPORT = Path(__file__).resolve()
RECEIPT = Path(".git/ticket-engine/delivery.json")

# Text Git gives that is not UTF-8 (a file in Shift_JIS, a name in its bytes)
# is kept as surrogate escapes, so that a name goes back to Git as the bytes
# it was. Unstructured Git diagnostics replace each invalid byte below;
# individual path labels use readable() to retain the distinct byte values.
codecs.register_error("replacement-character",
                      lambda error: ("\N{REPLACEMENT CHARACTER}".encode() * (error.end - error.start), error.end))


def readable(text):
    """A path for display only; byte names must not collapse to the same text.

    Normal Unicode names stay readable. Invalid UTF-8 is shown as Python's
    quoted bytes (b'...\\x82...'), not replacement characters. Quote literal
    escapes and controls too, so a real backslash is not mistaken for a byte.
    None of these displayed strings is passed back to Git as a path.
    """
    if re.search("[\udc80-\udcff]", text):
        return repr(text.encode("utf-8", "surrogateescape"))
    if any(not character.isprintable() or character in "\\\"'" for character in text):
        return repr(text)
    return text


class DeliveryError(RuntimeError):
    """A refusal or a failed operation. The process exits non-zero."""


class DescriptionSettingError(DeliveryError):
    """Only the operator can supply or correct this description setting."""


def description_size(text):
    """One operator limit shared by review and publication, without truncation."""
    try:
        limit = int(setting("PR_DESCRIPTION_MAX_BYTES", "60000"))
        if limit <= 0:
            raise ValueError()
    except ValueError as error:
        raise DescriptionSettingError("PR_DESCRIPTION_MAX_BYTES must be a positive integer") from error
    if len(text.encode("utf-8")) > limit:
        raise DeliveryError("The pull request description exceeds the configured %d-byte limit; nothing was shortened" % limit)


def description_report():
    """The chosen role's latest reports, from this run's existing checkpoint."""
    role = os.environ.get("PR_DESCRIPTION_ROLE", "")
    if not role:
        return "", ""
    path = os.environ.get("TASK_HISTORY")
    if not path:
        raise DescriptionSettingError("TASK_HISTORY is unset; use a runtime that supplies it to review and delivery")
    description_size("")
    try:
        with Path(path).open("rb") as saved:
            raw = saved.read(64 * 1024 * 1024 + 1)
        if len(raw) > 64 * 1024 * 1024:
            raise ValueError("checkpoint exceeds the local 64 MiB read limit")
        history = json.loads(raw.decode("utf-8"))["history"]
        if not isinstance(history, list):
            raise ValueError("checkpoint history is not a list")
        selected = []
        for record in reversed(history):
            name, speaker = record["role"], record["speaker"]
            if not isinstance(name, str) or not isinstance(speaker, str):
                raise ValueError("checkpoint has a malformed report identity")
            if speaker in {"requester", "runtime"}:
                continue
            if name == role:
                text = record.get("output", "")
                if not isinstance(text, str):
                    raise ValueError("checkpoint has a malformed report body")
                selected.append(text)
            elif selected:
                break
        text = "\n\n".join(reversed(selected))
        text.encode("utf-8")
    except (OSError, UnicodeError, ValueError, KeyError, TypeError, AttributeError, RecursionError) as error:
        reason = error.strerror if isinstance(error, OSError) else str(error)
        return "", "Pull request explanation omitted: saved history could not be read (%s). The change itself still requires review." % reason
    if not text.strip():
        raise DeliveryError("The selected role has no latest report to publish; return to the worker for an explanation")
    description_size(text)
    return text, ""


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
# and the -c settings a Git passes to the Gits it starts.
GIT_SETTINGS = re.compile(r"GIT_CONFIG_(COUNT|PARAMETERS|KEY_\d+|VALUE_\d+)")
# The delivery's Git on the workspace reads it as the review's Git does: it
# drops those settings and the variables that pick Git's attributes or its diff
# program, and reads no default exclude or attributes file. In one stage's
# environment only, any of them would make the two disagree on whether anything
# changed. adversarial_review.py keeps the same lists; a test keeps them the
# same. deliver_git.py turns this on for its own process. A Git that talks to
# the service (given a url) keeps the environment as it is, which may carry what
# the operator's network needs; so does everything the check after delivery and
# the mirror run.
WORKSPACE_DROPS = ("GIT_ATTR_SOURCE", "GIT_EXTERNAL_DIFF")
WORKSPACE_FILES = ["-c", "core.excludesFile=" + os.devnull, "-c", "core.attributesFile=" + os.devnull]
workspace_as_reviewed = False


def git_environment(*, with_credential=True, service=False):
    """Git's own configuration only: no personal, repository or system file.
    In the delivery, a Git that does not talk to the service also drops what
    the review's Git drops."""
    environment = {name: value for name, value in os.environ.items()
                   if service or not workspace_as_reviewed
                   or not (GIT_SETTINGS.fullmatch(name) or name in WORKSPACE_DROPS)}
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
            # Git includes an explicit port in the credential's host field.
            # Keep that authority exactly, excluding any URL user information;
            # stripping the port both breaks this target and permits another.
            os.environ["DELIVERY_CREDENTIAL_HOST"] = host.netloc.rsplit("@", 1)[-1]
            command += ["-c", "credential.helper=", "-c", "credential.helper=" + helper]
    elif workspace_as_reviewed:
        command += WORKSPACE_FILES
    return command + list(arguments)


def run(command, *, cwd=None, check=True, timeout=None, environment=None,
        stdin=subprocess.DEVNULL, redact=True):
    """Run a configured command and keep its scrubbed output for the report.

    Pass redact=False only for text that is examined and never printed: the
    credential check below has to see what is actually there.
    """
    result = subprocess.run(command, cwd=cwd, env=environment or git_environment(), text=True,
                            errors="surrogateescape", stdin=stdin, capture_output=True, timeout=timeout)
    output = result.stdout if not redact else scrub(result.stdout)
    diagnostics = scrub(result.stderr)
    if check and result.returncode != 0:
        raise DeliveryError("Command failed (exit %d): %s\n%s%s"
                            % (result.returncode, shlex.join(command), output, diagnostics))
    return result.returncode, output, diagnostics


# The most of one line output_lines holds at once; a longer line comes in parts.
PART = 1 << 22


def character_start(buffer, position):
    """The position, or else the start of the UTF-8 character it falls in."""
    for _ in range(3):
        if 0x80 <= buffer[position] < 0xC0:
            position -= 1
    return position


def output_lines(command, *, environment=None, overlap=0, check=True):
    """A command's output a line at a time, as it comes: for output that is
    looked at rather than kept, and may be too large to hold whole. Nothing
    in it is scrubbed: a caller that prints any of it scrubs that. Lines end
    at LF only, and every other byte is kept as given; a text read would
    have turned a CR into a line end. Each line comes as (bytes, True). Of a
    line whose end has not come yet, no more than PART is held besides the
    last read: such a line comes in parts, each after the first as (bytes,
    False) and beginning with at least the last `overlap` bytes of the one
    before, so that nothing that long is cut in two; no part is cut inside
    a UTF-8 character. A failure is raised once the output has been read,
    unless check is False."""
    part = max(PART, 2 * overlap)
    with tempfile.TemporaryFile() as diagnostics:
        with subprocess.Popen(command, env=environment or git_environment(), stdin=subprocess.DEVNULL,
                              stdout=subprocess.PIPE, stderr=diagnostics) as process:
            pending, first = bytearray(), True
            for chunk in iter(lambda: process.stdout.read(1 << 20), b""):
                pending += chunk
                end = pending.rfind(b"\n")
                if end >= 0:
                    for line in bytes(pending[:end]).split(b"\n"):
                        yield line, first
                        first = True
                    del pending[:end + 1]
                while len(pending) > part:
                    cut = character_start(pending, part)
                    yield bytes(pending[:cut]), first
                    first = False
                    del pending[:character_start(pending, cut - overlap)]
            if pending:
                yield bytes(pending), first
        if check and process.returncode != 0:
            diagnostics.seek(0)
            raise DeliveryError("Command failed (exit %d): %s\n%s" % (
                process.returncode, shlex.join(command),
                scrub(diagnostics.read().decode("utf-8", "replace"))))


def output_head(command, size, *, environment=None):
    """The first size bytes of a command's output, or all of it when it is
    shorter. No more is read: once size bytes have come, the command is
    stopped. A failure before then is raised."""
    with tempfile.TemporaryFile() as diagnostics:
        with subprocess.Popen(command, env=environment or git_environment(), stdin=subprocess.DEVNULL,
                              stdout=subprocess.PIPE, stderr=diagnostics, bufsize=0) as process:
            head = b""
            while len(head) < size:
                chunk = process.stdout.read(size - len(head))
                if not chunk:
                    break
                head += chunk
            stopped = len(head) == size and process.poll() is None
            if stopped:
                process.kill()
        if not stopped and process.returncode != 0:
            diagnostics.seek(0)
            raise DeliveryError("Command failed (exit %d): %s\n%s" % (
                process.returncode, shlex.join(command),
                scrub(diagnostics.read().decode("utf-8", "replace"))))
        return head


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
            status, output, diagnostics = run(command, check=False, timeout=timeout, cwd=cwd,
                                              environment=git_environment(service=True))
        except subprocess.TimeoutExpired:
            raise TransientError("it did not finish within the configured time")
        if status != 0:
            text = (diagnostics + "\n" + output).strip()
            if transient_git(text):
                raise TransientError(text.splitlines()[0] if text else "no message")
            raise DeliveryError("Git could not %s: %s" % (describe, text[:800] or "no message"))
        return output
    return with_retry(attempt, describe) if retry else attempt()


def some_paths(paths, count=None):
    """At most twenty paths for a person to read, with how many there are in
    all when that is more."""
    count = len(paths) if count is None else count
    named = ", ".join(paths[:20])
    return named if count <= 20 else "%d paths, the first 20 of them %s" % (count, named)


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
    for stream in (sys.stdout, sys.stderr):
        stream.reconfigure(errors="replacement-character")
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
