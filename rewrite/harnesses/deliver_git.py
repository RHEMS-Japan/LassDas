"""Deliver the reviewed workspace to the operator's repository. Fixed process.

This is not a model tool and reads nothing from a role's answer: the target,
the branch, the paths that may be delivered and the merge method are operator
settings. It stages only the allowed paths, refuses any other change, pushes
the ticket branch, opens or reuses one pull request, merges it and records a
receipt. A rerun continues the same delivery instead of repeating it; it is
not a general exactly-once guarantee for an interrupted remote operation.

Before publishing, it brings the ticket branch up to date with the
integration branch, since another request may have been merged there since
this checkout was made. A clean merge becomes a merge commit on the ticket
branch. A conflicting one is left in the working tree between Git's markers
and the delivery is refused naming the paths, so the next implementation
launch resolves them in place; the commit of the following delivery then
completes that merge. This needs the working tree writable as well as .git,
and happens under the merge method only.

Environment (all from the operator, never from a role):
  TASK_ISSUE                 assigned ticket; names the branch and the message
  TASK_WORKSPACE             prepared checkout (default: the process directory)
  GITHUB_TOKEN               delivery credential, through the credential helper
  DELIVERY_REPOSITORY        owner/name of the delivery target
  DELIVERY_BASE_BRANCH       integration branch the pull request targets
  DELIVERY_ALLOWED_PATHS     colon-separated paths that may change; '.' is the whole tree
  DELIVERY_FORBIDDEN_TEXT    optional newline-separated text refused in a diff
  DELIVERY_MERGE_METHOD      merge (default), squash or rebase
  DELIVERY_REMOTE_URL        optional Git URL override (default: github.com)
  DELIVERY_API_BASE          optional REST base (default: api.github.com)
  DELIVERY_AUTHOR_NAME/_EMAIL, DELIVERY_POLL_SECONDS,
  DELIVERY_MERGE_TIMEOUT_SECONDS, DELIVERY_GIT_TIMEOUT_SECONDS: optional

With --dry-run it checks the settings, the changed paths and the reachability
of the target, commits nothing and pushes nothing, and always ends non-zero:
an operator check must never be mistaken for a delivery that happened.
"""
import os
from pathlib import PurePosixPath
import re
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import delivery_support as support
from delivery_support import DeliveryError

ISSUE = re.compile(r"\A[A-Za-z0-9][A-Za-z0-9._-]{0,99}\Z")
METHODS = ("merge", "squash", "rebase")


def issue_name():
    issue = support.setting("TASK_ISSUE")
    if not ISSUE.match(issue) or issue.endswith(".lock") or ".." in issue:
        raise DeliveryError("TASK_ISSUE is not a usable branch or message name")
    return issue


def allowed_paths():
    paths = []
    for entry in support.setting("DELIVERY_ALLOWED_PATHS").split(":"):
        entry = entry.strip().strip("/")
        if not entry:
            continue
        candidate = PurePosixPath(entry)
        if candidate.is_absolute() or ".." in candidate.parts:
            raise DeliveryError("DELIVERY_ALLOWED_PATHS entries are workspace-relative paths")
        paths.append(candidate)
    if not paths:
        raise DeliveryError("DELIVERY_ALLOWED_PATHS grants no path; nothing may be delivered")
    return paths


def status_entries(workspace):
    _, output, _ = support.run(support.git("-C", str(workspace), "status", "--porcelain=v1", "-z",
                                           "--untracked-files=all", "--no-renames"))
    return [entry for entry in output.split("\0") if len(entry) > 3]


def changed_paths(workspace):
    """Every path Git reports as changed, staged or untracked, including both
    sides of a rename. A path outside the operator's grant stops delivery."""
    return [entry[3:] for entry in status_entries(workspace)]


def paths_to_stage(workspace):
    """The changed paths whose working tree differs from the index. A path a
    pending merge already staged, such as one the integration branch removed
    and that no longer exists, has nothing left to add."""
    return [entry[3:] for entry in status_entries(workspace) if entry[1] != " "]


def merge_in_progress(workspace):
    code, _, _ = support.run(support.git("-C", str(workspace), "rev-parse", "-q", "--verify", "MERGE_HEAD"), check=False)
    return code == 0


def names(output):
    return {entry for entry in output.split("\0") if entry}


def integration_paths(workspace):
    """While a merge of the integration branch is being completed, a path that
    branch changed and that the working tree now holds exactly as that branch
    has it is the branch's own, not something the worker wrote. A path the
    worker wrote, even one the branch also changed, needs the grant."""
    if not merge_in_progress(workspace):
        return set()
    _, theirs, _ = support.run(support.git("-C", str(workspace), "diff", "--name-only", "-z", "--no-renames",
                                           "HEAD", "MERGE_HEAD"))
    _, ours, _ = support.run(support.git("-C", str(workspace), "diff", "--name-only", "-z", "--no-renames", "MERGE_HEAD"))
    # An untracked path is never the integration branch's own, even where
    # that branch deleted a path of the same name.
    _, untracked, _ = support.run(support.git("-C", str(workspace), "ls-files", "--others", "--exclude-standard", "-z"))
    return names(theirs) - names(ours) - names(untracked)


def refuse_paths_outside_grant(paths, allowed, exempt=()):
    outside = []
    for path in paths:
        candidate = PurePosixPath(path)
        if path in exempt:
            continue
        if not any(candidate == grant or grant in candidate.parents for grant in allowed):
            outside.append(path)
    if outside:
        raise DeliveryError("Changes outside the operator's allowed paths were not delivered: "
                            + ", ".join(sorted(set(outside))[:20]))


def refuse_forbidden_text(diff, paths=()):
    """Configured text must not leave the workspace, whoever wrote it. Only
    the lines this change adds, and the names of the paths it touches, are
    its text: context and removed lines are what was already there. The
    diff is read with ">" marking added lines, since a line whose own text
    begins with "+" would otherwise look like a file header."""
    added = "\n".join([line[1:] for line in diff.splitlines() if line.startswith(">")] + list(paths))
    lowered = added.lower()
    found = [entry for entry in os.environ.get("DELIVERY_FORBIDDEN_TEXT", "").splitlines()
             if entry.strip() and entry.strip().lower() in lowered]
    if found:
        raise DeliveryError("Refused: the staged change contains configured forbidden text (%d entr%s)"
                            % (len(found), "y" if len(found) == 1 else "ies"))
    if support.credential() in added:
        raise DeliveryError("Refused: the staged change contains the delivery credential")


def head(workspace):
    _, output, _ = support.run(support.git("-C", str(workspace), "rev-parse", "HEAD"))
    return output.strip()


def stage_and_commit(workspace, issue, allowed, receipt):
    """Stage the granted paths, refuse anything else, and commit once."""
    paths = changed_paths(workspace)
    refuse_paths_outside_grant(paths, allowed, integration_paths(workspace))
    staging = paths_to_stage(workspace)
    if staging:
        support.run(support.git("-C", str(workspace), "add", "-A", "--", *staging))
    _, check, _ = support.run(support.git("-C", str(workspace), "diff", "--cached", "--check"), check=False)
    if "conflict marker" in check:
        raise DeliveryError("Refused: the change still carries Git conflict markers in: "
                            + ", ".join(sorted({line.rsplit(":", 2)[0] for line in check.splitlines()
                                                if "conflict marker" in line})[:20]))
    # The staged text is read unredacted because one of the refusals below is
    # "this change carries the delivery credential". It is never printed.
    staged, diff, _ = support.run(support.git("-C", str(workspace), "diff", "--cached", "--no-color"),
                                  check=False, redact=False)
    if staged != 0:
        raise DeliveryError("Git could not read the staged change")
    if not diff.strip() and not merge_in_progress(workspace):
        # Nothing new. A receipt for this commit means an interrupted round
        # continues below; without one there is no reviewed work to deliver.
        # A recorded commit that is now an ancestor of HEAD is the catch-up
        # merge commit of a round interrupted before it was written down.
        current = head(workspace)
        if receipt.get("head") == current:
            return current, False
        if receipt.get("head"):
            code, _, _ = support.run(support.git("-C", str(workspace), "merge-base", "--is-ancestor",
                                                 receipt["head"], current), check=False)
            if code == 0:
                return current, True
        raise DeliveryError("No change under the allowed paths is ready to deliver")
    # A pending merge whose result equals this branch's own content is still
    # concluded by a commit: that is what makes the branch mergeable.
    # What the integration branch already carried is not this change; during
    # a merge completion only what differs from that branch's tip is looked at.
    against = ["MERGE_HEAD"] if merge_in_progress(workspace) else []
    _, marked, _ = support.run(support.git("-C", str(workspace), "diff", "--cached", "--no-color",
                                           "--output-indicator-new=>", *against), redact=False)
    _, named, _ = support.run(support.git("-C", str(workspace), "diff", "--cached", "--name-only", "-z",
                                          "--no-renames", *against))
    refuse_forbidden_text(marked, sorted(names(named)))
    name = os.environ.get("DELIVERY_AUTHOR_NAME", "") or "ticket engine"
    address = os.environ.get("DELIVERY_AUTHOR_EMAIL", "") or "ticket-engine@invalid"
    # Both halves on purpose. A runtime with no Git identity cannot derive one
    # from an account, and a configuration that forbids guessing refuses the
    # commit outright; the environment settles author and committer as well.
    environment = support.git_environment()
    environment.update(GIT_AUTHOR_NAME=name, GIT_AUTHOR_EMAIL=address,
                       GIT_COMMITTER_NAME=name, GIT_COMMITTER_EMAIL=address)
    support.run(support.git("-C", str(workspace), "-c", "user.name=" + name, "-c", "user.email=" + address,
                            "commit", "--no-verify", "-m", "Deliver " + issue),
                environment=environment)
    return head(workspace), True


def catch_up(workspace, url, base, issue, method):
    """Bring the ticket branch up to date with the integration branch. Returns
    whether a merge commit was made. A conflict is left in the working tree,
    between Git's markers where Git can write them, for the next
    implementation launch to resolve. Only under the merge method: a service
    that squashes or rebases rewrites the delivered history, so the branch
    cannot tell its own earlier rounds from the integration branch's."""
    if method != "merge":
        return False
    support.run_git(support.git("-C", str(workspace), "fetch", "--no-tags", url, "refs/heads/" + base, url=url),
                    describe="read the integration branch",
                    timeout=support.number("DELIVERY_GIT_TIMEOUT_SECONDS", 600))
    _, target, _ = support.run(support.git("-C", str(workspace), "rev-parse", "FETCH_HEAD"))
    target = target.strip()
    code, _, _ = support.run(support.git("-C", str(workspace), "merge-base", "--is-ancestor", target, "HEAD"),
                             check=False)
    if code == 0:
        return False
    # Work the integration branch already contains (a merge recorded there
    # before this run was interrupted) has nothing to catch up with either.
    code, _, _ = support.run(support.git("-C", str(workspace), "merge-base", "--is-ancestor", "HEAD", target),
                             check=False)
    if code == 0:
        return False
    name = os.environ.get("DELIVERY_AUTHOR_NAME", "") or "ticket engine"
    address = os.environ.get("DELIVERY_AUTHOR_EMAIL", "") or "ticket-engine@invalid"
    environment = support.git_environment()
    environment.update(GIT_AUTHOR_NAME=name, GIT_AUTHOR_EMAIL=address,
                       GIT_COMMITTER_NAME=name, GIT_COMMITTER_EMAIL=address)
    code, _, diagnostics = support.run(
        support.git("-C", str(workspace), "-c", "user.name=" + name, "-c", "user.email=" + address,
                    "merge", "--no-ff", "--no-edit", "-m", "Merge %s into ticket/%s" % (base, issue), target),
        check=False, environment=environment)
    if code == 0:
        return True
    _, unmerged, _ = support.run(support.git("-C", str(workspace), "diff", "--name-only", "--diff-filter=U"),
                                 check=False)
    conflicted = sorted(line for line in unmerged.splitlines() if line)
    if not conflicted:
        # Not a conflict: Git refused for another reason. Leave the tree as it was.
        support.run(support.git("-C", str(workspace), "merge", "--abort"), check=False)
        raise DeliveryError("Git could not merge the integration branch %s: %s" % (base, diagnostics.strip()[:300]))
    raise DeliveryError(
        "The integration branch %s moved since this checkout and %d path%s conflict with this change: %s. "
        "Where Git could write them, the working tree holds both sides between conflict markers "
        "(<<<<<<<, =======, >>>>>>>); a path one side deleted or renamed has none and needs a decision "
        "whether it stays. Resolve them in place, keep the result building and tested, and the next "
        "delivery completes the merge."
        % (base, len(conflicted), "" if len(conflicted) == 1 else "s", ", ".join(conflicted[:20])))


def push_branch(workspace, url, branch, commit):
    """Push once. An already-published commit is not pushed again."""
    listing = support.run_git(support.git("-C", str(workspace), "ls-remote", "--heads", url,
                                          "refs/heads/" + branch, url=url),
                              describe="read the delivery branch",
                              timeout=support.number("DELIVERY_GIT_TIMEOUT_SECONDS", 600))
    published = listing.split("\t")[0].strip() if listing.strip() else ""
    if published == commit:
        return False
    support.run_git(support.git("-C", str(workspace), "push", url, commit + ":refs/heads/" + branch, url=url),
                    describe="push the delivery branch",
                    timeout=support.number("DELIVERY_GIT_TIMEOUT_SECONDS", 600))
    return True


def find_pull_request(owner, name, branch, base):
    status, payload = support.api_retried("GET", "/repos/%s/%s/pulls?head=%s&base=%s&state=open&per_page=100"
                                          % (owner, name, owner + ":" + branch, base),
                                          describe="reading existing pull requests")
    if status != 200 or not isinstance(payload, list):
        raise DeliveryError("Could not read existing pull requests (status %d): %s" % (status, payload))
    return payload[0] if payload else None


def open_pull_request(owner, name, branch, base, issue):
    existing = find_pull_request(owner, name, branch, base)
    if existing:
        return existing
    status, payload = support.api_retried("POST", "/repos/%s/%s/pulls" % (owner, name),
                                          describe="opening the pull request", payload={
        "title": "Deliver " + issue, "head": branch, "base": base,
        "body": "Prepared by the configured ticket engine for %s. Only the operator's allowed "
                "paths are included. Review the change itself; this description is not a result." % issue})
    if status == 201:
        return payload
    if status == 422:
        # The branch already has a pull request, including one opened by an
        # interrupted earlier attempt. Reuse it instead of opening a second.
        existing = find_pull_request(owner, name, branch, base)
        if existing:
            return existing
    raise DeliveryError("Could not open the pull request (status %d): %s"
                        % (status, payload.get("message", payload) if isinstance(payload, dict) else payload))


def read_pull_request(owner, name, number):
    status, payload = support.api_retried("GET", "/repos/%s/%s/pulls/%d" % (owner, name, number),
                                          describe="reading pull request %d" % number)
    if status != 200 or not isinstance(payload, dict):
        raise DeliveryError("Could not read pull request %d (status %d)" % (number, status))
    return payload


def merge_pull_request(owner, name, number, method, issue):
    """Merge once, then confirm from the service that it is actually merged."""
    current = read_pull_request(owner, name, number)
    if not current.get("merged"):
        status, payload = support.api_retried("PUT", "/repos/%s/%s/pulls/%d/merge" % (owner, name, number),
                                              describe="merging pull request %d" % number, payload={
            "merge_method": method, "commit_title": "Deliver %s (#%d)" % (issue, number)})
        if status not in (200, 405, 409):
            raise DeliveryError("Could not merge pull request %d (status %d): %s" % (number, status, payload))
        if status != 200:
            # Not mergeable, or the branch moved. Report the service's reason;
            # do not retry into a second merge of a different state.
            raise DeliveryError("The delivery service refused the merge (status %d): %s"
                                % (status, payload.get("message", payload) if isinstance(payload, dict) else payload))
    deadline = time.monotonic() + support.number("DELIVERY_MERGE_TIMEOUT_SECONDS", 900)
    interval = support.number("DELIVERY_POLL_SECONDS", 10)
    while True:
        current = read_pull_request(owner, name, number)
        if current.get("merged") and current.get("merge_commit_sha"):
            return current
        if time.monotonic() >= deadline:
            raise DeliveryError("Pull request %d is not reported as merged; it may still be in progress" % number)
        time.sleep(interval)


def summary(receipt, pushed, committed):
    lines = ["Delivered %s to %s." % (receipt["issue"], receipt["repository"]),
             "Branch %s carried commit %s." % (receipt["branch"], receipt["head"]),
             "Pull request %d against %s was merged with method %s as commit %s."
             % (receipt["pull_request"], receipt["base_branch"], receipt["merge_method"], receipt["merge_sha"]),
             "This round %s a commit and %s the branch; the receipt is at %s."
             % ("created" if committed else "reused", "pushed" if pushed else "did not need to push", support.RECEIPT)]
    if receipt.get("previous"):
        lines.append("Earlier merged rounds for this ticket: %d." % len(receipt["previous"]))
    lines.append("Merging is not by itself a check that the result works; the verification process reports that.")
    return "\n".join(lines)


def check_only(workspace, owner, name, base, branch, url, allowed):
    """An operator check: local settings plus read-only calls to the target."""
    paths = changed_paths(workspace)
    exempt = integration_paths(workspace)
    refuse_paths_outside_grant(paths, allowed, exempt)
    lines = ["Checked the delivery settings; nothing was committed, pushed, opened or merged.",
             "Target %s/%s, integration branch %s, ticket branch %s." % (owner, name, base, branch),
             "Changed paths inside the operator's grant: %s."
             % (", ".join(sorted(path for path in paths if path not in exempt)) or "none")]
    if exempt:
        lines.append("Paths the integration branch changed, which need no grant: %s." % ", ".join(sorted(exempt)))
    status, payload = support.api("GET", "/repos/%s/%s" % (owner, name))  # no retry: this is a check
    lines.append("Reading the repository answered status %d%s." %
                 (status, "" if status != 200 else "; its default branch is %s" % payload.get("default_branch", "?")))
    status, _ = support.api("GET", "/repos/%s/%s/branches/%s" % (owner, name, base))
    lines.append("Reading the integration branch answered status %d." % status)
    code, listing, diagnostics = support.run(
        support.git("ls-remote", "--heads", url, "refs/heads/" + base, url=url),
        check=False, timeout=support.number("DELIVERY_GIT_TIMEOUT_SECONDS", 600))
    lines.append("Listing the branch over Git exited %d%s." %
                 (code, (" with " + listing.split("\t")[0][:12]) if code == 0 and listing.strip() else ""))
    if code != 0:
        lines.append(diagnostics.strip()[:500])
    lines.append("This was a check, so it ends non-zero on purpose.")
    print("\n".join(lines))
    return 3


def refusal(issue, owner, name, base, branch, error):
    return "\n".join([
        "Nothing was delivered for %s." % issue,
        "Target %s/%s, integration branch %s, ticket branch %s." % (owner, name, base, branch),
        "What stopped it: %s" % error,
        "This is the delivery service's own answer or this process's own refusal, not a judgement "
        "about the work. Nothing was merged, so nothing needs undoing."])


def deliver(arguments):
    dry = arguments == ["--dry-run"]
    if arguments and not dry:
        raise DeliveryError("This delivery process takes no arguments except --dry-run")
    support.discard_prompt()
    workspace = os.environ.get("TASK_WORKSPACE") or os.getcwd()
    issue = issue_name()
    branch = "ticket/" + issue
    owner, name = support.repository()
    base = support.setting("DELIVERY_BASE_BRANCH")
    method = os.environ.get("DELIVERY_MERGE_METHOD", "") or "merge"
    if method not in METHODS:
        raise DeliveryError("DELIVERY_MERGE_METHOD must be one of: " + ", ".join(METHODS))
    allowed = allowed_paths()
    url = support.remote_url(owner, name)
    if dry:
        return check_only(workspace, owner, name, base, branch, url, allowed)
    try:
        return carry_out(workspace, issue, owner, name, base, branch, method, url, allowed)
    except DeliveryError as error:
        print(refusal(issue, owner, name, base, branch, error), flush=True)
        raise


def carry_out(workspace, issue, owner, name, base, branch, method, url, allowed):
    path = support.receipt_path(workspace)
    receipt = support.read_receipt(path)
    previous = receipt.pop("previous", [])
    if receipt.get("merge_sha") and receipt.get("head") == head(workspace) and not changed_paths(workspace):
        # Already delivered and nothing new arrived: report, do not deliver again.
        receipt["previous"] = previous
        print(summary(receipt, pushed=False, committed=False))
        return 0
    if receipt.get("merge_sha"):
        # A merged round plus new reviewed work is a further round on the same
        # branch, recorded separately so the earlier merge stays visible.
        previous = previous + [receipt]
        receipt = {}
    receipt.update(issue=issue, repository=owner + "/" + name, base_branch=base,
                   branch=branch, merge_method=method)
    commit, committed = stage_and_commit(workspace, issue, allowed, receipt)
    receipt["head"] = commit
    if committed:
        receipt["committed_at"] = support.timestamp()
    # The commit is on record before anything that can fail against the
    # service, so a refused or failed catch-up leaves a rerun something to
    # continue from instead of a committed tree it cannot name.
    support.write_receipt(path, dict(receipt, previous=previous))
    if catch_up(workspace, url, base, issue, method):
        commit, committed = head(workspace), True
        receipt["head"] = commit
        receipt["committed_at"] = support.timestamp()
        support.write_receipt(path, dict(receipt, previous=previous))
    pushed = push_branch(workspace, url, branch, commit)
    if pushed:
        receipt["pushed_at"] = support.timestamp()
        support.write_receipt(path, dict(receipt, previous=previous))
    # A recorded pull request is read directly: an interrupted run may have
    # merged it already, and a merged one is no longer in the open list.
    pull = (read_pull_request(owner, name, int(receipt["pull_request"])) if receipt.get("pull_request")
            else open_pull_request(owner, name, branch, base, issue))
    receipt["pull_request"] = int(pull["number"])
    receipt["pull_request_url"] = pull.get("html_url", "")
    receipt.setdefault("opened_at", support.timestamp())
    support.write_receipt(path, dict(receipt, previous=previous))
    merged = merge_pull_request(owner, name, receipt["pull_request"], method, issue)
    receipt["merge_sha"] = merged["merge_commit_sha"]
    receipt["merged_at"] = merged.get("merged_at") or support.timestamp()
    receipt["previous"] = previous
    support.write_receipt(path, receipt)
    print(summary(receipt, pushed, committed))
    return 0


if __name__ == "__main__":
    sys.exit(support.main(deliver))
