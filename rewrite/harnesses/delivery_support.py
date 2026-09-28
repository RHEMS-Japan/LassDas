"""Shared pieces of the fixed delivery processes; never a model tool.

These programs run configured operations only. Every value comes from the
operator's environment, not from a role's answer, and the credential never
reaches a command line, a remote URL or the printed report: Git asks for it
through the credential helper below, which answers one configured host.
"""
import datetime
import json
import os
from pathlib import Path
import shlex
import subprocess
import sys
import urllib.error
import urllib.parse
import urllib.request

SUPPORT = Path(__file__).resolve()
RECEIPT = Path(".git/ticket-engine/delivery.json")


class DeliveryError(RuntimeError):
    """A refusal or a failed operation. The process exits non-zero."""


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


def git_environment(*, with_credential=True):
    """Git's own configuration only: no personal, repository or system file."""
    environment = dict(os.environ)
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
        raise DeliveryError("Delivery service unreachable: " + scrub(str(error.reason)))
    text = scrub(text).strip()
    if not text:
        return status, {}
    try:
        return status, json.loads(text)
    except ValueError:
        # An unparsed body is still evidence of what the service answered.
        return status, {"message": text[:500]}


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
