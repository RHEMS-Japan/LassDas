"""Deliver the reviewed workspace to the operator's repository. Fixed process.

This is not a model tool and reads nothing from a role's answer: the target,
the branch, the paths that may be delivered and the merge method are operator
settings. It stages only the allowed paths, refuses any other change, pushes
the ticket branch, opens or reuses one pull request, merges it (or, under the
none method, leaves the merge to a person) and records a receipt. A rerun
continues the same delivery instead of repeating it; it is not a general
exactly-once guarantee for an interrupted remote operation.

Before publishing, it brings the ticket branch up to date with the
integration branch, since another request may have been merged there since
this checkout was made. A clean merge becomes a merge commit on the ticket
branch. A conflicting one is left in the working tree between Git's markers
and the delivery is refused naming the paths, so the next implementation
launch resolves them in place; the commit of the following delivery then
completes that merge. This needs the working tree writable as well as .git,
and happens under the merge method and under none only.

With DELIVERY_MERGE_METHOD=none the delivery ends at the open pull request
and leaves the merge to a person: it commits, catches up, pushes and opens or
reuses the pull request as above, records it and ends 0 without merging.
While a round's merge is left to a person (after a switch to a merge method,
until this process's own merge request succeeds), each later run reads what
became of that pull request before its push and again after it, and what the
person did decides:
- still open: reported again once its branch holds the commit on record
  (otherwise that commit is pushed), or new reviewed work goes to the same
  branch and pull request;
- its branch pushed to or rewritten by a person: nothing is pushed over it,
  and the delivery ends 0 saying so, naming a commit of this delivery that is
  not on that branch and the changes the workspace holds uncommitted, and
  records it;
- merged: recorded as merged by someone else, with the commit they made, and
  work after it is a further round with a pull request of its own; a merge
  made before this round's commit reached the pull request is such an earlier
  round, and the commit gets a new pull request;
- closed without a merge: the request ends, 0, with nothing of this round
  delivered and nothing reopened in its place; an earlier round that was
  merged is named.
After a changed branch or a closed pull request, a later run reads neither
the pull request nor its branch again: it says what was read then, and when,
and after a changed branch it names the changes the workspace still holds
uncommitted.
A merge found in a round left to a person is never reported as "merged with
method". Under a merge method alone that is said of the merge this process
asks for and also of one it finds already made, since that cannot be told
apart from the merge of an interrupted earlier attempt.

A request whose right outcome is that nothing changes, because what it asks
for already exists, leaves nothing to commit, and by default that is refused
like any other empty delivery. With DELIVERY_ALLOW_UNCHANGED=1 it ends here
instead, when the workspace has no changed path, no merge in progress and no
receipt of an earlier round, and the commit the work started from is part of
the integration branch as fetched at that moment: nothing is committed,
pushed or opened, the process ends 0 and its receipt says that nothing was
delivered and which commit of the integration branch the request stands on.
Whether that satisfies the request is not judged here: the shipped review
before this stage lets a checkout with no change through only on a reviewer's
verdict that does not object.

Environment (all from the operator, never from a role):
  TASK_ISSUE                 assigned ticket; names the branch and the message
  TASK_WORKSPACE             prepared checkout (default: the process directory)
  GITHUB_TOKEN               delivery credential, through the credential helper
  DELIVERY_REPOSITORY        owner/name of the delivery target
  DELIVERY_BASE_BRANCH       integration branch the pull request targets
  DELIVERY_ALLOWED_PATHS     colon-separated paths that may change; '.' is the whole tree
  DELIVERY_FORBIDDEN_TEXT    optional newline-separated text refused in a diff
  DELIVERY_MERGE_METHOD      merge (default), squash, rebase, or none to leave the merge to a person
  DELIVERY_ALLOW_UNCHANGED   1 lets a request that changed no file end without a delivery (default: refused)
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
METHODS = ("merge", "squash", "rebase", "none")
# The most of one name the refusal for conflict markers quotes. No path is
# longer, and a line it quotes is cut there.
QUOTED = 4096


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


def allow_unchanged():
    """Whether the operator lets a request that changed no file end here: 1
    does, 0 or unset does not. Anything else is refused rather than read as
    off: read as off, a mistyped setting would leave such a request refused
    round after round with nothing in the record saying why."""
    value = os.environ.get("DELIVERY_ALLOW_UNCHANGED", "")
    if value not in ("", "0", "1"):
        raise DeliveryError("DELIVERY_ALLOW_UNCHANGED must be 1, 0 or unset")
    return value == "1"


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


def looked_for_bytes():
    """The most bytes a text the check looks for can take in a change: four
    for each character of the longest forbidden entry or of the credential.
    No character takes more in UTF-8, and text in UTF-16 takes two for each
    ASCII character, the character and the NUL beside it."""
    texts = [entry.strip().lower() for entry in os.environ.get("DELIVERY_FORBIDDEN_TEXT", "").splitlines()]
    return 4 * max(len(text) for text in texts + [os.environ.get("GITHUB_TOKEN", "")])


def added_lines(workspace, against, blobs):
    """The lines the staged change adds. The diff marks them with ">", since
    a line whose own text begins with "+" would otherwise look like a file
    header. A file Git takes as binary, by its own judgement or by an
    attribute, is shown as text, and no diff program or text conversion
    stands in for it. A line ends only where Git ends it, at LF: a CR, a
    form feed or a line separator inside it is part of it. A long line comes
    in parts, each repeating enough of the one before it that no text the
    check looks for is cut in two. Each comes with the place of its file in
    the diff, which tells the lines of one file from those of another, and
    blobs is given the staged version of each file as the diff names it."""
    adding, place = False, -1
    for part, first in support.output_lines(support.git(
            "-C", str(workspace), "diff", "--cached", "--no-color", "--text", "--no-ext-diff", "--no-textconv",
            "--output-indicator-new=>", *against), overlap=looked_for_bytes()):
        if first:
            if part.startswith(b"diff --git "):
                place += 1
            elif part.startswith(b"index "):
                # "index <before>..<staged>", maybe with a mode: the object
                # the file's staged version is, as the diff abbreviates it.
                blobs.setdefault(place, part.split(b" ")[1].rpartition(b"..")[2].decode("ascii", "replace"))
            adding, part = part.startswith(b">"), part[1:]
        if adding:
            yield place, part.decode("utf-8", "surrogateescape")


def staged_head_holds_nul(workspace, blob):
    """Whether the staged version of a file holds a NUL byte in its first 8000
    bytes, which is what makes Git take content as binary. No more of it is
    read."""
    return bool(blob) and b"\0" in support.output_head(
        support.git("-C", str(workspace), "cat-file", "blob", blob), 8000)


def staged_texts(workspace, against, blobs):
    """The staged change's text: the lines it adds, then the names of the
    paths it touches, which belong to no file's content."""
    yield from added_lines(workspace, against, blobs)
    _, named, _ = support.run(support.git("-C", str(workspace), "diff", "--cached", "--name-only", "-z",
                                          "--no-renames", *against))
    yield from ((None, name) for name in sorted(names(named)))


def refuse_forbidden_text(texts, binary=lambda place: False):
    """Configured text must not leave the workspace, whoever wrote it. Only
    the lines this change adds, and the names of the paths it touches, are
    its text: context and removed lines are what was already there. They
    are looked at one by one, a long line in parts, so a large change is
    never held whole. Text in UTF-16 has a NUL beside each ASCII character,
    so each is also looked at with its NULs taken out. texts gives each with
    the place of its file in the diff, or None for a name; binary(place),
    asked only where it decides a refusal, tells whether Git takes that
    file's staged version as binary for its content."""
    entries = [entry.strip() for entry in os.environ.get("DELIVERY_FORBIDDEN_TEXT", "").splitlines()]
    wanted = {entry.lower() for entry in entries if entry}
    token = os.environ.get("GITHUB_TOKEN", "")
    matched, carried, not_utf8 = set(), False, set()
    for place, text in texts:
        seen = (text, text.replace("\0", "")) if "\0" in text else (text,)
        lowered = [each.lower() for each in seen]
        matched.update(entry for entry in wanted if any(entry in each for each in lowered))
        carried = carried or bool(token) and any(token in each for each in seen)
        if re.search("[\udc80-\udcff]", text):
            not_utf8.add(place)
    found = [entry for entry in entries if entry and entry.lower() in matched]
    if found:
        raise DeliveryError("Refused: the staged change contains configured forbidden text (%d entr%s)"
                            % (len(found), "y" if len(found) == 1 else "ies"))
    # A process given no credential is refused at this point, as before.
    support.credential()
    if carried:
        raise DeliveryError("Refused: the staged change contains the delivery credential")
    # Bytes that are not UTF-8 are read as surrogate escapes. ASCII is found
    # in them as written; other text is not, so it cannot pass unlooked-for.
    # A file whose staged version Git takes as binary for its content, for a
    # NUL in its first 8000 bytes as in an image, is the exception: other
    # text is found in it where written in UTF-8. It is asked file by file,
    # and only here. Attributes and the version a change replaces are not
    # asked, since either makes Git take text as binary as well, and a name
    # is never such a file.
    if not_utf8 and not all(entry.isascii() for entry in entries) and (
            None in not_utf8 or not all(binary(place) for place in sorted(not_utf8))):
        raise DeliveryError("Refused: the staged change carries text that is not UTF-8, in which configured "
                            "forbidden text that is not ASCII cannot be looked for")


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
    # Git's whitespace check is read for its conflict markers only, a line at
    # a time: it also prints each line that has a whitespace error, so what
    # the refusal names from it is scrubbed first. A long line comes in parts
    # that overlap by at least the credential's length, so a credential a cut
    # goes through is whole in the next part, and the piece the cut leaves at
    # the end of a part lies far past what is quoted of it.
    marked = set()
    reach = max(len("conflict marker"), len(os.environ.get("GITHUB_TOKEN", "").encode()))
    for part, _ in support.output_lines(support.git("-C", str(workspace), "diff", "--cached", "--check"),
                                        overlap=reach, check=False):
        line = part.decode("utf-8", "surrogateescape")
        if "conflict marker" in line:
            marked.add(support.scrub(line.rsplit(":", 2)[0])[:QUOTED])
    if marked:
        raise DeliveryError("Refused: the change still carries Git conflict markers in: "
                            + ", ".join(sorted(marked)[:20]))
    # Whether anything is staged, from Git's exit status alone: the change is
    # not read here, so a large one is not held in memory.
    staged, _, _ = support.run(support.git("-C", str(workspace), "diff", "--cached", "--quiet", "--no-ext-diff",
                                           "--no-textconv"), check=False)
    if staged not in (0, 1):
        raise DeliveryError("Git could not read the staged change")
    if staged == 0 and not merge_in_progress(workspace):
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
    blobs = {}
    refuse_forbidden_text(staged_texts(workspace, against, blobs),
                          lambda place: staged_head_holds_nul(workspace, blobs.get(place)))
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


def integration_tip(workspace, url, base):
    """The integration branch's commit as the delivery service holds it now."""
    support.run_git(support.git("-C", str(workspace), "fetch", "--no-tags", url, "refs/heads/" + base, url=url),
                    describe="read the integration branch",
                    timeout=support.number("DELIVERY_GIT_TIMEOUT_SECONDS", 600))
    _, target, _ = support.run(support.git("-C", str(workspace), "rev-parse", "FETCH_HEAD"))
    return target.strip()


def catch_up(workspace, url, base, issue, method):
    """Bring the ticket branch up to date with the integration branch. Returns
    whether a merge commit was made. A conflict is left in the working tree,
    between Git's markers where Git can write them, for the next
    implementation launch to resolve. Only under the merge method, and under
    none, where the pull request a person merges should not carry a conflict
    the work stage could have resolved: a service that squashes or rebases
    rewrites the delivered history, so the branch cannot tell its own earlier
    rounds from the integration branch's. Under none a person may squash or
    rebase all the same; a further round then conflicts with the rewritten
    copy of its own earlier change, and that goes to the work stage like any
    other conflict."""
    if method not in ("merge", "none"):
        return False
    target = integration_tip(workspace, url, base)
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


def open_pull_request(owner, name, branch, base, issue, method):
    existing = find_pull_request(owner, name, branch, base)
    if existing:
        return existing
    status, payload = support.api_retried("POST", "/repos/%s/%s/pulls" % (owner, name),
                                          describe="opening the pull request", payload={
        "title": "Deliver " + issue, "head": branch, "base": base,
        "body": "Prepared by the configured ticket engine for %s. Only the operator's allowed "
                "paths are included. Review the change itself; this description is not a result.%s"
                % (issue, " Merging it is left to a person." if method == "none" else "")})
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
    """Merge once, then confirm from the service that it is actually merged.
    Also returns whether this process asked for the merge: one found merged
    already was merged before this run asked, by an earlier attempt or by
    someone else."""
    current = read_pull_request(owner, name, number)
    asked = not current.get("merged")
    if asked:
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
            return current, asked
        if time.monotonic() >= deadline:
            raise DeliveryError("Pull request %d is not reported as merged; it may still be in progress" % number)
        time.sleep(interval)


class MergedNotSettled(DeliveryError):
    """A person merged the pull request, but the service does not report the
    merge commit yet: nothing is wrong, and the next delivery reads it again."""


def head_of(pull):
    """The commit the service names as the pull request's head, if it says."""
    head = pull.get("head")
    return head.get("sha") if isinstance(head, dict) else None


def ticket_tip(workspace, url, branch):
    """The ticket branch as the service holds it now, fetched so that its
    history can be compared with this checkout's; None when there is none."""
    listing = support.run_git(support.git("-C", str(workspace), "ls-remote", "--heads", url,
                                          "refs/heads/" + branch, url=url),
                              describe="read the delivery branch",
                              timeout=support.number("DELIVERY_GIT_TIMEOUT_SECONDS", 600))
    if not listing.strip():
        return None
    support.run_git(support.git("-C", str(workspace), "fetch", "--no-tags", url, "refs/heads/" + branch, url=url),
                    describe="read the delivery branch",
                    timeout=support.number("DELIVERY_GIT_TIMEOUT_SECONDS", 600))
    return listing.split("\t")[0].strip()


def changed_by_person(workspace, tip, commit):
    """Whether the published ticket branch holds commits this checkout does
    not: a person pushed to the pull request's branch, or rewrote it."""
    if not tip or tip == commit:
        return False
    code, _, _ = support.run(support.git("-C", str(workspace), "merge-base", "--is-ancestor", tip, commit),
                             check=False)
    return code != 0


def taken_by_person(receipt, pull):
    """What a person did with a pull request left to them: their merge, as the
    receipt of a round with the commit the service reports for it and the head
    they merged (the recorded head when the service names none), or None when
    they did not merge it."""
    if not pull.get("merged"):
        return None
    if not pull.get("merge_commit_sha"):
        raise MergedNotSettled("Pull request %d was merged by someone else, but the service does not report its "
                               "merge commit yet" % int(pull["number"]))
    return dict(receipt, merge_sha=pull["merge_commit_sha"], merged_at=pull.get("merged_at") or support.timestamp(),
                merge_left_to_person=True, head=head_of(pull) or receipt.get("head"))


def summary(receipt, pushed, committed):
    if receipt.get("merge_left_to_person"):
        merged = ("Pull request %d against %s was merged by someone else as commit %s; this process merged nothing."
                  % (receipt["pull_request"], receipt["base_branch"], receipt["merge_sha"]))
    else:
        merged = ("Pull request %d against %s was merged with method %s as commit %s."
                  % (receipt["pull_request"], receipt["base_branch"], receipt["merge_method"], receipt["merge_sha"]))
    lines = ["Delivered %s to %s." % (receipt["issue"], receipt["repository"]),
             "Branch %s carried commit %s." % (receipt["branch"], receipt["head"]),
             merged,
             "This round %s a commit and %s the branch; the receipt is at %s."
             % ("created" if committed else "reused", "pushed" if pushed else "did not need to push", support.RECEIPT)]
    if receipt.get("previous"):
        lines.append("Earlier merged rounds for this ticket: %d." % len(receipt["previous"]))
    lines.append("Merging is not by itself a check that the result works; the verification process reports that.")
    return "\n".join(lines)


def open_summary(receipt, pushed, committed):
    lines = ["Pull request %d against %s is open for %s: %s."
             % (receipt["pull_request"], receipt["base_branch"], receipt["issue"],
                receipt.get("pull_request_url") or "(the service gave no address)"),
             "Merging is left to a person; nothing was merged.",
             "Branch %s carries commit %s." % (receipt["branch"], receipt["head"]),
             "This round %s a commit and %s the branch; the receipt is at %s."
             % ("created" if committed else "reused", "pushed" if pushed else "did not need to push", support.RECEIPT)]
    if receipt.get("previous"):
        lines.append("Earlier merged rounds for this ticket: %d." % len(receipt["previous"]))
    return "\n".join(lines)


def earlier_merges(receipt):
    """One sentence for each earlier round of this request that was merged."""
    return ["An earlier round of this request was merged as commit %s through pull request %s."
            % (round["merge_sha"], round.get("pull_request")) for round in receipt.get("previous") or []
            if round.get("merge_sha")]


def read_then(receipt):
    """When a later run, which reads nothing again, says the ending was read."""
    return " at " + receipt["ended_at"] if receipt.get("ended_at") else ""


def closed_summary(receipt, again=False):
    """again: a later run, which does not read the pull request again and
    says only what was read then."""
    address = receipt.get("pull_request_url") or "(the service gave no address)"
    earlier = earlier_merges(receipt)
    nothing = "nothing %sdelivered" % ("of this round " if earlier else "")
    if again:
        lines = ["When a delivery read it%s, pull request %d against %s for %s had been closed by a person without "
                 "being merged: %s. This delivery did not read it again."
                 % (read_then(receipt), receipt["pull_request"], receipt["base_branch"], receipt["issue"], address)]
        ending = ("This request ended then with %s. This process opens no other pull request in its place; "
                  "continuing needs a new request." % nothing)
    else:
        lines = ["Pull request %d against %s for %s was closed by a person without being merged: %s."
                 % (receipt["pull_request"], receipt["base_branch"], receipt["issue"], address)]
        ending = ("This request ends with %s. The pull request is not reopened and no other is opened in its place; "
                  "continuing needs a new request." % nothing)
    return "\n".join(lines + earlier + [ending, "The receipt at %s records this." % support.RECEIPT])


def changed_summary(receipt, again=False):
    """again: a later run, which reads neither the pull request nor its branch
    again; only the work still not committed is looked at anew."""
    address = receipt.get("pull_request_url") or "(the service gave no address)"
    if again:
        lines = ["When a delivery read them%s, pull request %d against %s for %s was open (%s) and a person had "
                 "changed its branch %s, which was at %s. This delivery did not read them again and does nothing "
                 "more with that pull request."
                 % (read_then(receipt), receipt["pull_request"], receipt["base_branch"], receipt["issue"], address,
                    receipt["branch"], receipt["branch_head"])]
    else:
        lines = ["Pull request %d against %s is open for %s: %s."
                 % (receipt["pull_request"], receipt["base_branch"], receipt["issue"], address),
                 "A person changed its branch %s, which is at %s now, so this process does nothing more with that "
                 "pull request." % (receipt["branch"], receipt["branch_head"])]
    if receipt.get("not_pushed"):
        verb = "was" if again else "is"
        lines.append("This delivery's commit %s %s not on that branch, so it %s not in the pull request; nothing "
                     "was pushed over the person's commits." % (receipt["not_pushed"], verb, verb))
    if receipt.get("not_committed"):
        lines.append("The workspace still holds changes that are not committed (%s). This process did not put them "
                     "in the pull request."
                     % support.some_paths(receipt["not_committed"], receipt.get("not_committed_count")))
    lines.append("Merging is left to a person; nothing was merged by this process.")
    return "\n".join(lines)


def unchanged_summary(receipt):
    return "\n".join([
        "Nothing was delivered for %s: no file was changed." % receipt["issue"],
        "The request stands on %s of %s as it is at %s; the commit the work started from, %s, is part of it."
        % (receipt["base_branch"], receipt["repository"], receipt["base_sha"], receipt["workspace_head"]),
        "Nothing was committed, pushed, opened or merged, because DELIVERY_ALLOW_UNCHANGED is set; "
        "the receipt at %s records this." % support.RECEIPT,
        "Whether that satisfies the request is what the review before this stage judged; this process "
        "does not judge it."])


def check_only(workspace, owner, name, base, branch, method, url, allowed, unchanged):
    """An operator check: local settings plus read-only calls to the target."""
    paths = changed_paths(workspace)
    exempt = integration_paths(workspace)
    refuse_paths_outside_grant(paths, allowed, exempt)
    lines = ["Checked the delivery settings; nothing was committed, pushed, opened or merged.",
             "Target %s/%s, integration branch %s, ticket branch %s." % (owner, name, base, branch),
             "A delivery ends at the open pull request and leaves the merge to a person (DELIVERY_MERGE_METHOD "
             "is none)." if method == "none" else "A delivery merges its pull request with method %s." % method,
             "Changed paths inside the operator's grant: %s."
             % (", ".join(sorted(path for path in paths if path not in exempt)) or "none"),
             "A request that changes no file %s."
             % ("ends without a delivery when the commit it started from is part of the integration branch "
                "(DELIVERY_ALLOW_UNCHANGED is 1)" if unchanged else
                "is refused (DELIVERY_ALLOW_UNCHANGED is not 1)")]
    if exempt:
        lines.append("Paths the integration branch changed, which need no grant: %s." % ", ".join(sorted(exempt)))
    status, payload = support.api("GET", "/repos/%s/%s" % (owner, name))  # no retry: this is a check
    lines.append("Reading the repository answered status %d%s." %
                 (status, "" if status != 200 else "; its default branch is %s" % payload.get("default_branch", "?")))
    status, _ = support.api("GET", "/repos/%s/%s/branches/%s" % (owner, name, base))
    lines.append("Reading the integration branch answered status %d." % status)
    code, listing, diagnostics = support.run(
        support.git("ls-remote", "--heads", url, "refs/heads/" + base, url=url),
        check=False, timeout=support.number("DELIVERY_GIT_TIMEOUT_SECONDS", 600),
        environment=support.git_environment(service=True))
    lines.append("Listing the branch over Git exited %d%s." %
                 (code, (" with " + listing.split("\t")[0][:12]) if code == 0 and listing.strip() else ""))
    if code != 0:
        lines.append(diagnostics.strip()[:500])
    lines.append("This was a check, so it ends non-zero on purpose.")
    print("\n".join(lines))
    return 3


def refusal(issue, owner, name, base, branch, error):
    if isinstance(error, MergedNotSettled):
        return "\n".join([
            "%s for %s." % (error, issue),
            "Target %s/%s, integration branch %s, ticket branch %s." % (owner, name, base, branch),
            "Nothing more was done this time; the next delivery reads the pull request again."])
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
    # Read and write the workspace as the review reads it (delivery_support).
    support.workspace_as_reviewed = True
    workspace = os.environ.get("TASK_WORKSPACE") or os.getcwd()
    issue = issue_name()
    branch = "ticket/" + issue
    owner, name = support.repository()
    base = support.setting("DELIVERY_BASE_BRANCH")
    method = os.environ.get("DELIVERY_MERGE_METHOD", "") or "merge"
    if method not in METHODS:
        raise DeliveryError("DELIVERY_MERGE_METHOD must be one of: " + ", ".join(METHODS))
    allowed = allowed_paths()
    unchanged = allow_unchanged()
    url = support.remote_url(owner, name)
    if dry:
        return check_only(workspace, owner, name, base, branch, method, url, allowed, unchanged)
    try:
        return carry_out(workspace, issue, owner, name, base, branch, method, url, allowed, unchanged)
    except DeliveryError as error:
        print(refusal(issue, owner, name, base, branch, error), flush=True)
        raise


def finish_unchanged(workspace, issue, owner, name, base, url, path):
    """End without a delivery: nothing changed, and the commit the work
    started from is already part of the integration branch, so the request
    stands on that branch as it is now. A rerun checks again from the start
    and records where the branch is then, which is what the verification
    after delivery checks out."""
    code, current, _ = support.run(support.git("-C", str(workspace), "rev-parse", "-q", "--verify", "HEAD^{commit}"),
                                   check=False)
    if code != 0:
        raise DeliveryError("No change under the allowed paths is ready to deliver, and the checkout has no "
                            "commit for the request to stand on")
    current = current.strip()
    tip = integration_tip(workspace, url, base)
    code, _, _ = support.run(support.git("-C", str(workspace), "merge-base", "--is-ancestor", current, tip),
                             check=False)
    if code != 0:
        # The work did not start from this branch, or the branch was rewritten
        # since: the request does not stand on it, so this is no ending.
        raise DeliveryError("No change under the allowed paths is ready to deliver, and the commit the work "
                            "started from, %s, is not part of %s as it is now" % (current, base))
    receipt = {"unchanged": True, "issue": issue, "repository": owner + "/" + name, "base_branch": base,
               "base_sha": tip, "workspace_head": current}
    support.write_receipt(path, receipt)
    print(unchanged_summary(receipt))
    return 0


def is_ancestor(workspace, ancestor, commit):
    code, _, _ = support.run(support.git("-C", str(workspace), "merge-base", "--is-ancestor", ancestor, commit),
                             check=False)
    return code == 0


def end_closed(path, receipt, previous):
    """A person closed the pull request without merging it: the request ends
    with nothing of this round delivered, and the pull request is left as they
    left it."""
    receipt.update(closed_unmerged=True, ended_at=support.timestamp())
    receipt["previous"] = previous
    support.write_receipt(path, receipt)
    print(closed_summary(receipt))
    return 0


def uncommitted(workspace):
    """The changes the workspace holds uncommitted, whoever made them, as the
    receipt keeps them: at most twenty paths, in readable text, and how many
    there are."""
    paths = [support.readable(path) for path in sorted(changed_paths(workspace))]
    return {"not_committed": paths[:20], "not_committed_count": len(paths)}


def end_changed(workspace, path, receipt, previous, tip):
    """A person pushed to the pull request's branch, or rewrote it: nothing
    more is pushed there, and what is not in the pull request is named: a
    commit of this delivery, and changes the workspace holds uncommitted."""
    recorded = receipt.get("head")
    unpushed = recorded if recorded and not is_ancestor(workspace, recorded, tip) else None
    receipt.update(changed_by_person=True, branch_head=tip, not_pushed=unpushed, ended_at=support.timestamp(),
                   **uncommitted(workspace))
    receipt["previous"] = previous
    support.write_receipt(path, receipt)
    print(changed_summary(receipt))
    return 0


def carry_out(workspace, issue, owner, name, base, branch, method, url, allowed, unchanged):
    path = support.receipt_path(workspace)
    receipt = support.read_receipt(path)
    if receipt.get("unchanged"):
        # A round that ended without a delivery committed, pushed and opened
        # nothing, so there is nothing of it to continue: the workspace as it
        # is now decides again, and work changed since is a first round.
        receipt = {}
    if unchanged and not receipt and not changed_paths(workspace) and not merge_in_progress(workspace):
        return finish_unchanged(workspace, issue, owner, name, base, url, path)
    previous = receipt.pop("previous", [])
    if receipt.get("closed_unmerged") or receipt.get("changed_by_person"):
        # A person closed the pull request or changed its branch. Nothing more
        # is done with it, and it is not read again: a later delivery says
        # what was read then, and names the changes still not committed.
        receipt["previous"] = previous
        if receipt.get("changed_by_person"):
            receipt.update(uncommitted(workspace))
            support.write_receipt(path, receipt)
        print(closed_summary(receipt, again=True) if receipt.get("closed_unmerged")
              else changed_summary(receipt, again=True))
        return 0
    if receipt.get("pull_request") and not receipt.get("merge_sha") and (
            method == "none" or receipt.get("merge_left_to_person")):
        # The merge was left to a person, so what became of the pull request
        # is read before anything is pushed.
        receipt["merge_left_to_person"] = True
        if method == "none":
            # A round begun under another method is left to a person from
            # now on, which the receipt has to say.
            receipt["merge_method"] = method
        pull = read_pull_request(owner, name, int(receipt["pull_request"]))
        merged = taken_by_person(receipt, pull)
        if merged and merged["head"] == receipt.get("head"):
            # Their merge carried this round's commit: a finished round.
            receipt = merged
            support.write_receipt(path, dict(receipt, previous=previous))
        elif merged:
            # Their merge came before this round's commit reached the pull
            # request: it is an earlier round, and the commit gets a pull
            # request of its own.
            previous = previous + [merged]
            receipt = {"head": receipt.get("head")}
            support.write_receipt(path, dict(receipt, previous=previous))
        elif pull.get("state") == "closed":
            return end_closed(path, receipt, previous)
        else:
            tip = ticket_tip(workspace, url, branch)
            if changed_by_person(workspace, tip, receipt.get("head")):
                return end_changed(workspace, path, receipt, previous, tip)
            support.write_receipt(path, dict(receipt, previous=previous))
            if (method == "none" and tip == receipt.get("head") == head(workspace)
                    and not changed_paths(workspace)):
                # Open, nothing new, and the branch holds the commit on record.
                receipt["previous"] = previous
                print(open_summary(receipt, pushed=False, committed=False))
                return 0
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
    # Who merges is the current setting's: a round begun under another method
    # is left, or merged, as the operator now says. A round left to a person
    # stays theirs until this process's own merge request succeeds, so it is
    # read again after the push, where they may have changed, merged or closed
    # the pull request since the read above.
    left = method == "none" or bool(receipt.get("merge_left_to_person"))
    # A merge found after the push may end the round on record before this
    # one's commit; that round keeps its own times, which this one overwrites.
    times = {field: receipt[field] for field in ("committed_at", "pushed_at") if field in receipt}
    receipt.update(issue=issue, repository=owner + "/" + name, base_branch=base,
                   branch=branch, merge_method=method)
    if left:
        receipt["merge_left_to_person"] = True
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
    if left and receipt.get("pull_request"):
        # A person who changed the pull request's branch meanwhile keeps it.
        tip = ticket_tip(workspace, url, branch)
        if changed_by_person(workspace, tip, commit):
            return end_changed(workspace, path, receipt, previous, tip)
    pushed = push_branch(workspace, url, branch, commit)
    if pushed:
        receipt["pushed_at"] = support.timestamp()
        support.write_receipt(path, dict(receipt, previous=previous))
    # A recorded pull request is read directly: an interrupted run may have
    # merged it already, and a merged one is no longer in the open list.
    pull = (read_pull_request(owner, name, int(receipt["pull_request"])) if receipt.get("pull_request")
            else open_pull_request(owner, name, branch, base, issue, method))
    receipt["pull_request"] = int(pull["number"])
    receipt["pull_request_url"] = pull.get("html_url", "")
    receipt.setdefault("opened_at", support.timestamp())
    support.write_receipt(path, dict(receipt, previous=previous))
    if left:
        merged = taken_by_person(receipt, pull)
        if merged and merged["head"] != commit:
            # They merged it before this round's commit reached it: that merge
            # is an earlier round, and the pushed commit gets a new pull request.
            for field in ("committed_at", "pushed_at"):
                merged.pop(field, None)
            previous = previous + [dict(merged, **times)]
            for field in ("pull_request", "pull_request_url", "opened_at"):
                receipt.pop(field, None)
            pull = open_pull_request(owner, name, branch, base, issue, method)
            receipt.update(pull_request=int(pull["number"]), pull_request_url=pull.get("html_url", ""),
                           opened_at=support.timestamp())
            support.write_receipt(path, dict(receipt, previous=previous))
        elif merged:
            receipt = dict(merged, previous=previous)
            support.write_receipt(path, receipt)
            print(summary(receipt, pushed, committed))
            return 0
        elif pull.get("state") == "closed":
            return end_closed(path, receipt, previous)
    if method == "none":
        # The delivery ends here: the pull request is open for a person to merge.
        receipt["previous"] = previous
        support.write_receipt(path, receipt)
        print(open_summary(receipt, pushed, committed))
        return 0
    merged, asked = merge_pull_request(owner, name, receipt["pull_request"], method, issue)
    if asked:
        # This process merged it. One found merged already stays a person's in
        # a round left to one, and is otherwise taken for an earlier attempt's.
        receipt.pop("merge_left_to_person", None)
    receipt["merge_sha"] = merged["merge_commit_sha"]
    receipt["merged_at"] = merged.get("merged_at") or support.timestamp()
    receipt["previous"] = previous
    support.write_receipt(path, receipt)
    print(summary(receipt, pushed, committed))
    return 0


if __name__ == "__main__":
    sys.exit(support.main(deliver))
