#!/usr/bin/env python3
"""Look for the shapes of credentials in the tracked files or in commit messages.

A key that reaches a commit is published with it, and a public history keeps
it after the file is fixed. This looks for the shapes keys take, so it needs
no list of values and runs the same in every repository, on a pull request
from another repository too.

Usage:
  scan-credential-shapes.py tree
      Reads every file Git tracks, as the working tree has it, whole and byte
      for byte: binary files too.
  scan-credential-shapes.py messages [REVISION ...]
      Reads the messages of the commits `git rev-list REVISION ...` lists.
      Without revisions it reads the pushed range: the commits that
      scan-commit-messages.sh reads, chosen from RANGE_BASE, RANGE_HEAD,
      DEFAULT_BRANCH and GITHUB_REF by the same rules.

A match is named by its place and its shape ("path:line: shape" or "commit
SHA line N: shape"), never by its text: this output is public wherever the
repository is, and printing a key would publish it once more.

Exit status: 0 when nothing matches; 1 when something does; 2 when the files
or the history cannot be read as described.
"""
import os
import re
import subprocess
import sys

# Letter case is part of every shape. None of these patterns matches its own
# text, and the tests assemble their examples when they run, so the scan reads
# every tracked file, this one and its tests included, with no exceptions.
SHAPES = tuple((name, re.compile(pattern)) for name, pattern in (
    ("GitHub token", rb"gh[pousr]_[A-Za-z0-9]{36,}"),
    ("GitHub fine-grained token", rb"github_pat_[A-Za-z0-9]{22}_[A-Za-z0-9]{59}"),
    ("OpenRouter key", rb"sk-or-v1-[0-9a-fA-F]{64}"),
    ("Anthropic key", rb"sk-ant-[A-Za-z0-9_-]{20,}"),
    ("AWS access key ID", rb"AKIA[0-9A-Z]{16}"),
    ("private key block", rb"-{5}BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-{5}"),
    ("Slack token", rb"xox[bpa]-[0-9A-Za-z-]{10,}"),
    # 32 to 64 letters and digits given to apiKey or api_key: a query
    # parameter, a YAML or JSON field, an assignment.
    ("apiKey or api_key value",
     rb"(?:apiKey|api_key)[\"'`]?[ \t]*(?::=|=>|[:=])[ \t]*[\"'`]?[A-Za-z0-9]{32,64}"),
))
REMEDY = ("remove the value, rewrite the commits that carry it, and replace the key: "
          "a key that has been pushed counts as exposed")
USAGE = "usage: scan-credential-shapes.py tree | messages [REVISION ...]"


def stop(reason):
    print(reason, file=sys.stderr)
    sys.exit(2)


def git(*arguments, data=None):
    """Git's output. A Git command that fails ends the scan with status 2."""
    done = subprocess.run(["git", *arguments], input=data, capture_output=True)
    if done.returncode != 0:
        stop("git failed: " + done.stderr.decode("utf-8", "replace").strip())
    return done.stdout


def succeeds(*arguments):
    return subprocess.run(["git", *arguments], capture_output=True).returncode == 0


def counted(number, noun):
    return "%d %s%s" % (number, noun, "" if number == 1 else "s")


def report(text, place):
    """Prints place:line: shape for every shape on every line of text; returns
    how many it printed."""
    found = 0
    for number, line in enumerate(text.split(b"\n"), 1):
        for name, pattern in SHAPES:
            if pattern.search(line):
                print("%s%d: %s" % (place, number, name))
                found += 1
    return found


def scan_tree():
    root = git("rev-parse", "--show-toplevel").rstrip(b"\n")
    names = [name for name in git("-C", root, "ls-files", "-z").split(b"\0") if name]
    if not names:
        stop("git ls-files listed no tracked files; there would be nothing to read")
    found = read = unreadable = 0
    for name in names:
        shown = name.decode("utf-8", "backslashreplace")
        place = os.path.join(root, name)
        try:
            if os.path.islink(place):
                text = os.readlink(place)  # Git keeps a link as the path it names.
            elif os.path.isdir(place):
                print("%s: a submodule, read in its own repository" % shown)
                continue
            else:
                with open(place, "rb") as handle:
                    text = handle.read()
        except OSError as error:
            print("%s: cannot be read (%s)" % (shown, error.strerror))
            unreadable += 1
            continue
        read += 1
        found += report(text, shown + ":")
    if unreadable:
        print("%s could not be read" % counted(unreadable, "tracked file"))
    if found:
        print("found %s reading %s - %s" % (counted(found, "credential shape"),
                                             counted(read, "tracked file"), REMEDY))
        return 1
    if unreadable:
        return 2
    print("read %s; none holds a credential shape" % counted(read, "tracked file"))
    return 0


def pushed_range():
    """The revisions scan-commit-messages.sh reads, chosen by its rules: a
    change to one is a change to both."""
    base = os.environ.get("RANGE_BASE", "")
    head = os.environ.get("RANGE_HEAD", "")
    default = os.environ.get("DEFAULT_BRANCH", "")
    if not head or not default:
        stop("without revisions the pushed range is read from RANGE_HEAD and DEFAULT_BRANCH, "
             "and one of them is not set")
    on_default = os.environ.get("GITHUB_REF", "") == "refs/heads/" + default
    remote = "refs/remotes/origin/" + default
    remote_known = succeeds("rev-parse", "--verify", "--quiet", remote + "^{commit}")
    if base and succeeds("cat-file", "-e", base):
        # Anywhere but on the default branch itself, what the default branch
        # already holds is not read: no change on a branch can reword it.
        if not on_default and remote_known:
            print("reading the messages of %s..%s that %s does not hold" % (base, head, default))
            return [base + ".." + head, "--not", remote]
        print("reading the messages of %s..%s" % (base, head))
        return [base + ".." + head]
    if on_default:
        print("no usable base on %s itself; reading every reachable message" % default)
        return [head]
    if not remote_known:
        stop("there is no usable base and %s is not in the checkout, "
             "so the new commits cannot be told apart" % remote)
    print("no usable base; reading the messages of the commits %s does not hold" % default)
    return [head, "--not", remote]


def scan_messages(revisions):
    commits = git("rev-list", *(revisions or pushed_range()), "--").split()
    # Each object is cut out by the size Git states for it, so no byte inside
    # a message can end it early or start the next one.
    batch = git("cat-file", "--batch", data=b"".join(c + b"\n" for c in commits)) if commits else b""
    found = position = 0
    for commit in commits:
        end = batch.find(b"\n", position)
        header = batch[position:end].split() if end >= 0 else []
        if len(header) != 3 or header[0] != commit or header[1] != b"commit":
            stop("git cat-file did not return commit %s" % commit.decode())
        size = int(header[2])
        message = batch[end + 1:end + 1 + size].partition(b"\n\n")[2]
        position = end + 1 + size + 1
        found += report(message, "commit %s line " % commit.decode())
    read = "the messages of " + counted(len(commits), "commit")
    if found:
        print("found %s reading %s - %s" % (counted(found, "credential shape"), read, REMEDY))
        return 1
    print("read %s; none holds a credential shape" % read)
    return 0


def main(arguments):
    if arguments == ["tree"]:
        return scan_tree()
    if arguments[:1] == ["messages"]:
        return scan_messages(arguments[1:])
    print(USAGE, file=sys.stderr)
    return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
