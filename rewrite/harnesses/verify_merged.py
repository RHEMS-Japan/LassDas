"""Check the operator's integration branch after delivery. Fixed process.

This is not a model tool. It reads the delivery receipt, fetches the actual
integration branch from the delivery service, checks that the recorded merge
commit is really contained in it, and runs the operator's own verification
commands against that fetched source. Its report is ordinary text; the engine
parses nothing from it. A passing run is evidence about these commands on this
branch, not a judgment that the original request is fulfilled.

A receipt saying that nothing was changed (the delivery's
DELIVERY_ALLOW_UNCHANGED) has no merge commit to look for. Then the commit the
request stands on, as the delivery recorded it, has to be contained in the
fetched branch, and the commands run on that branch as it is now, as they do
after a merge: a request that needs no change is held to the target as the
requester will find it. The report says that no merge was made.

A receipt whose merge is left to a person (the delivery's
DELIVERY_MERGE_METHOD=none) and that records no merge yet has no merged state
to verify either. What it checks instead is the pull request's head as the
delivery pushed it: it fetches the ticket branch, requires the recorded commit
to be contained in it, runs the commands with that commit checked out, and
says that this delivery merged nothing; whether a person merged since is not
looked at here. Once the delivery has recorded that a person merged the pull
request, the integration branch is verified as usual.

Two endings a person caused end this check at 0 without running anything,
since there is nothing of this delivery left to verify and failing would only
send the work round again: a pull request closed without a merge (nothing of
that round was delivered; an earlier round of the request that was merged is
named, not checked), and a pull request whose branch a person changed (what
would be merged is theirs). The report says what was looked at and what was
not.

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
    if receipt.get("unchanged"):
        # No merge was made: what is verified is the integration branch's own
        # commit that the request stands on, as the delivery recorded it.
        stands = str(receipt.get("base_sha", ""))
        if not COMMIT.match(stands):
            raise DeliveryError("The delivery receipt says nothing was changed but records no commit "
                                "the request stands on")
        return receipt, stands
    if receipt.get("closed_unmerged") or receipt.get("changed_by_person"):
        return receipt, ""
    if receipt.get("merge_left_to_person") and not receipt.get("merge_sha"):
        # Nothing is merged yet: what is verified is the commit the delivery
        # pushed for a person to merge, on the branch it pushed it to.
        pushed = str(receipt.get("head", ""))
        if not COMMIT.match(pushed) or not str(receipt.get("branch", "")).startswith("ticket/"):
            raise DeliveryError("The delivery receipt leaves the merge to a person but records no pushed "
                                "commit and ticket branch to verify")
        return receipt, pushed
    merge = str(receipt.get("merge_sha", ""))
    if not COMMIT.match(merge):
        raise DeliveryError("The delivery receipt records no completed merge commit")
    return receipt, merge


def fetch_branch(home, url, base, describe="fetch the integration branch"):
    """A fresh copy of the actual branch, never the role's own workspace."""
    Path(home).mkdir(parents=True, exist_ok=True)
    clone = tempfile.mkdtemp(prefix="verify-", dir=home)
    target = Path(clone) / "source"
    support.run_git(support.git("clone", "--no-tags", "--branch", base, "--", url, str(target), url=url),
                    describe=describe, timeout=support.number("VERIFY_CLONE_TIMEOUT_SECONDS", 900))
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
    named = "%s (%s)" % (receipt.get("pull_request", "(none recorded)"),
                         receipt.get("pull_request_url") or "no address recorded")
    if receipt.get("closed_unmerged"):
        # An earlier round of the request may have been merged all the same.
        earlier = ["An earlier round of this request was merged as commit %s through pull request %s; this check "
                   "does not look at it." % (round["merge_sha"], round.get("pull_request"))
                   for round in receipt.get("previous") or [] if round.get("merge_sha")]
        print(" ".join(["%s: pull request %s was closed by a person without being merged, so nothing of this %s was "
                        "delivered." % ("Not checked" if earlier else "Nothing to check", named,
                                        "round" if earlier else "request")]
                       + earlier + ["The configured verification commands were not run."]))
        return 0
    if receipt.get("changed_by_person"):
        where = ("The delivery's commit %s is not on that branch, so it is not in the pull request."
                 % receipt["not_pushed"] if receipt.get("not_pushed")
                 else "The delivery's last commit %s is on that branch." % receipt.get("head"))
        if receipt.get("not_committed"):
            where += (" This round's changes to %s were never committed, so the delivery did not put them in the "
                      "pull request." % ", ".join(receipt["not_committed"]))
        print("Not checked: a person changed branch %s of pull request %s; it was at %s when the delivery last "
              "looked. %s What a person merges from there is theirs, so the configured verification commands "
              "were not run, and the branch was not read again here."
              % (receipt.get("branch"), named, receipt.get("branch_head"), where))
        return 0
    unchanged = bool(receipt.get("unchanged"))
    pending = bool(receipt.get("merge_left_to_person")) and not receipt.get("merge_sha")
    owner, name = support.repository()
    base = support.setting("DELIVERY_BASE_BRANCH")
    url = support.remote_url(owner, name)
    commands = verify_commands()
    # An unmerged pull request is checked where it is: on its ticket branch.
    fetched = receipt["branch"] if pending else base
    try:
        source = (fetch_branch(home, url, fetched, describe="fetch the pull request's branch " + fetched)
                  if pending else fetch_branch(home, url, fetched))
    except DeliveryError as error:
        # The next role needs this in the report, not only in diagnostics.
        print("\n\n".join([
            "Could not read %s of %s/%s, so nothing was verified." % (fetched, owner, name),
            "What stopped it: %s" % error,
            "This says nothing about whether the delivery is correct; it says the branch could not "
            "be read from here."]), flush=True)
        raise
    _, tip, _ = support.run(support.git("-C", str(source), "rev-parse", "HEAD"))
    report = ["Fetched %s of %s/%s at commit %s." % (fetched, owner, name, tip.strip())]
    if dry:
        report.append("This was a check of the configured commands against the branch as it is, "
                      "with no delivery to verify, so it ends non-zero on purpose.")
    elif unchanged:
        report.append("No merge was made: the delivery receipt records that no file was changed, so there "
                      "is no merge commit to look for. The request stands on %s as it was at commit %s."
                      % (base, merge))
    elif pending:
        report.append("This delivery did not merge pull request %s (%s); the merge was left to a person, and "
                      "whether they merged it since is not looked at here. What is verified is that pull "
                      "request's head as the delivery pushed it, commit %s on %s, not %s after a merge."
                      % (receipt.get("pull_request", "(none recorded)"), receipt.get("pull_request_url") or
                         "no address recorded", merge, fetched, base))
    else:
        report.append("The receipt records pull request %s merged as commit %s%s."
                      % (receipt.get("pull_request", "(none recorded)"), merge,
                         " by someone else, not by the delivery" if receipt.get("merge_left_to_person") else ""))
    if unchanged:
        what = "commit the request stands on"
        absent = "the commit the request stands on is not part of the integration branch"
    elif pending:
        what = "commit the delivery pushed"
        absent = "the commit the delivery pushed is not part of %s" % fetched
    else:
        what = "merge commit"
        absent = "the delivered merge is not part of the integration branch"
    contained = dry or contains_merge(source, merge)
    if not dry:
        report.append("The %s is %scontained in %s." % (what, "" if contained else "NOT ", fetched))
    failures = 0
    if contained:
        if unchanged:
            report.append("The configured commands ran on %s as it is now, at commit %s, as after a merge."
                          % (base, tip.strip()))
        elif pending:
            # The commit the delivery pushed, not whatever the branch holds by
            # now: that is what a person was asked to merge.
            support.run(support.git("-C", str(source), "checkout", "-q", "--detach", merge))
            report.append("The configured commands ran with commit %s checked out%s."
                          % (merge, "" if merge == tip.strip() else
                             "; %s has moved on to %s since" % (fetched, tip.strip())))
        failures = run_verification(source, commands, report)
        report.append("%d of %d configured verification commands failed." % (failures, len(commands)))
    else:
        report.append("The configured verification commands were not run: %s, so there is nothing verified "
                      "to check." % absent)
    print("\n\n".join(report))
    if dry:
        return 3
    return 1 if failures or not contained else 0


if __name__ == "__main__":
    sys.exit(support.main(verify))
