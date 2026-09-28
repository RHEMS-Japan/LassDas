"""Check the operator's integration branch after delivery. Fixed process.

This is not a model tool. It reads the delivery receipt, fetches the actual
integration branch from the delivery service, checks that the recorded merge
commit is really contained in it, and runs the operator's own verification
commands against that fetched source. Its report is ordinary text; the engine
parses nothing from it. A passing run is evidence about these commands on this
branch, not a judgment that the original request is fulfilled.

Environment (all from the operator, never from a role):
  TASK_WORKSPACE             checkout holding the delivery receipt
  TASK_HOME                  private directory the branch is fetched into
  GITHUB_TOKEN               delivery credential, through the credential helper
  DELIVERY_REPOSITORY        owner/name of the delivery target
  DELIVERY_BASE_BRANCH       integration branch to check
  VERIFY_COMMANDS            newline-separated commands, each run without a shell
  DELIVERY_REMOTE_URL        optional Git URL override (default: github.com)
  VERIFY_TIMEOUT_SECONDS, VERIFY_CLONE_TIMEOUT_SECONDS: optional

With --dry-run it skips the receipt and the merge check, fetches the branch
and runs the configured commands against it, and always ends non-zero: an
operator check must never be mistaken for a verified delivery.
"""
import os
from pathlib import Path
import re
import shlex
import subprocess
import sys
import tempfile

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import delivery_support as support
from delivery_support import DeliveryError

COMMIT = re.compile(r"\A[0-9a-f]{40}\Z")
LIMIT = 4000


def receipt_fields(workspace):
    receipt = support.read_receipt(support.receipt_path(workspace))
    if not receipt:
        raise DeliveryError("No delivery receipt is present; nothing was delivered to verify")
    merge = str(receipt.get("merge_sha", ""))
    if not COMMIT.match(merge):
        raise DeliveryError("The delivery receipt records no completed merge commit")
    return receipt, merge


def fetch_branch(home, url, base):
    """A fresh copy of the actual branch, never the role's own workspace."""
    Path(home).mkdir(parents=True, exist_ok=True)
    clone = tempfile.mkdtemp(prefix="verify-", dir=home)
    target = Path(clone) / "source"
    support.run_git(support.git("clone", "--no-tags", "--branch", base, "--", url, str(target), url=url),
                    describe="fetch the integration branch",
                    timeout=support.number("VERIFY_CLONE_TIMEOUT_SECONDS", 900))
    return target


def contains_merge(source, merge):
    status, _, _ = support.run(support.git("-C", str(source), "merge-base", "--is-ancestor", merge, "HEAD"),
                               check=False)
    return status == 0


def verify_commands():
    commands = []
    for line in support.setting("VERIFY_COMMANDS").splitlines():
        if not line.strip():
            continue
        parts = shlex.split(line)
        if not parts:
            continue
        commands.append(parts)
    if not commands:
        raise DeliveryError("VERIFY_COMMANDS lists no command; nothing was verified")
    return commands


def run_verification(source, commands, report):
    """Run every configured command; a failure does not stop the report."""
    environment = support.git_environment(with_credential=False)
    temporary = Path(source).parent / "tmp"
    temporary.mkdir(exist_ok=True)
    environment["TMPDIR"] = str(temporary)
    failures = 0
    for command in commands:
        try:
            status, output, diagnostics = support.run(
                command, cwd=str(source), check=False, environment=environment,
                timeout=support.number("VERIFY_TIMEOUT_SECONDS", 1200))
        except FileNotFoundError:
            status, output, diagnostics = 127, "", "The configured verification command is not installed"
        except subprocess.TimeoutExpired:
            status, output, diagnostics = 124, "", "The configured verification command ran out of time"
        text = (output + diagnostics).strip()
        if len(text) > LIMIT:
            text = "… " + text[-LIMIT:]
        report.append("Command: %s\nExit status: %d\n%s" % (shlex.join(command), status, text or "(no output)"))
        if status != 0:
            failures += 1
    return failures


def verify(arguments):
    dry = arguments == ["--dry-run"]
    if arguments and not dry:
        raise DeliveryError("This verification process takes no arguments except --dry-run")
    support.discard_prompt()
    workspace = os.environ.get("TASK_WORKSPACE") or os.getcwd()
    home = os.environ.get("TASK_HOME") or tempfile.gettempdir()
    receipt, merge = ({}, "") if dry else receipt_fields(workspace)
    owner, name = support.repository()
    base = support.setting("DELIVERY_BASE_BRANCH")
    url = support.remote_url(owner, name)
    commands = verify_commands()
    try:
        source = fetch_branch(home, url, base)
    except DeliveryError as error:
        # The next role needs this in the report, not only in diagnostics.
        print("\n\n".join([
            "Could not read %s of %s/%s, so nothing was verified." % (base, owner, name),
            "What stopped it: %s" % error,
            "This says nothing about whether the delivery is correct; it says the branch could not "
            "be read from here."]), flush=True)
        raise
    _, tip, _ = support.run(support.git("-C", str(source), "rev-parse", "HEAD"))
    report = ["Fetched %s of %s/%s at commit %s." % (base, owner, name, tip.strip())]
    if dry:
        report.append("This was a check of the configured commands against the branch as it is, "
                      "with no delivery to verify, so it ends non-zero on purpose.")
    else:
        report.append("The receipt records pull request %s merged as commit %s."
                      % (receipt.get("pull_request", "(none recorded)"), merge))
    contained = dry or contains_merge(source, merge)
    if not dry:
        report.append("The merge commit is %scontained in %s." % ("" if contained else "NOT ", base))
    failures = 0
    if contained:
        failures = run_verification(source, commands, report)
        report.append("%d of %d configured verification commands failed." % (failures, len(commands)))
    else:
        report.append("The configured verification commands were not run: the delivered merge is "
                      "not part of the integration branch, so there is nothing verified to check.")
    print("\n\n".join(report))
    if dry:
        return 3
    return 1 if failures or not contained else 0


if __name__ == "__main__":
    sys.exit(support.main(verify))
