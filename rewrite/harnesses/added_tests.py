"""Detect and run request-added tests; shared by both verification processes.

Operator environment: ADDED_TEST_COMMAND, ADDED_TEST_PATTERNS,
ADDED_TEST_MAX_COMMANDS (100), VERIFY_TIMEOUT_SECONDS (1200).
TASK_BASE_COMMIT is the prepared checkout basis; ADDED_TEST_BASE is only a
standalone fallback. This module does not import a delivery entry point.
"""
import fnmatch
import os
from pathlib import Path, PurePosixPath
import re
import shlex
import stat
import subprocess
import sys

import delivery_support as support
from delivery_support import DeliveryError

INTEGRATION_REF = "refs/ticket-engine/integration"
DEFAULT_PATTERNS = "*_test.*\ntest_*\n*.test.*\n*.spec.*\n*Test.java\ntests/"
PLACEHOLDER = re.compile(r"\{([^{}]*)\}")


def git(source, *arguments, check=True):
    return support.run(support.git("-C", str(source), *arguments), check=check)[1].strip()


def empty_tree(source):
    return git(source, "hash-object", "-t", "tree", "--stdin")


def workspace_base(source):
    """The latest known integration side, not the request's preparation alone."""
    code, _, _ = support.run(support.git("-C", str(source), "rev-parse", "--verify", "HEAD"), check=False)
    if code:
        git(source, "rev-parse", "--git-dir")
        return empty_tree(source)
    branch = os.environ.get("TASK_BRANCH") or os.environ.get("DELIVERY_BASE_BRANCH")
    refs = [INTEGRATION_REF] + (["refs/remotes/origin/" + branch] if branch else [])
    latest = ""
    for ref in refs:
        code, value, _ = support.run(support.git("-C", str(source), "rev-parse", "--verify", ref), check=False)
        if code == 0:
            candidate = git(source, "merge-base", "HEAD", value.strip())
            if latest:
                code, _, _ = support.run(support.git("-C", str(source), "merge-base", "--is-ancestor", latest, candidate), check=False)
                if code:
                    code, _, _ = support.run(support.git("-C", str(source), "merge-base", "--is-ancestor", candidate, latest), check=False)
                    if code:
                        raise DeliveryError("known integration bases diverged; no single basis is known")
                    continue
            latest = candidate
    if latest:
        return latest
    base = os.environ.get("TASK_BASE_COMMIT") or os.environ.get("ADDED_TEST_BASE")
    if not base:
        raise DeliveryError("no integration or prepared-checkout basis is known")
    return git(source, "rev-parse", "--verify", "--end-of-options", base + "^{object}")


def detect(source, base, end=None):
    """Return added regular paths. None means the current tree plus untracked."""
    if not base:
        raise DeliveryError("no integration-side basis was recorded")
    base = git(source, "rev-parse", "--verify", "--end-of-options", base + "^{tree}")
    if end:
        end = git(source, "rev-parse", "--verify", "--end-of-options", end + "^{tree}")
    _, raw, _ = support.run(support.git("-C", str(source), "diff", "--raw", "-z", "--no-ext-diff",
                                       "--no-textconv", "--no-renames", "--diff-filter=A", base,
                                       *([end] if end else []), "--"))
    fields = raw.split("\0")
    paths = {fields[index + 1] for index in range(0, len(fields) - 1, 2)
             if fields[index].split()[1] in {"100644", "100755"}}
    if end is None:
        _, extra, _ = support.run(support.git("-C", str(source), "ls-files", "--others", "--exclude-standard", "-z"))
        for path in filter(None, extra.split("\0")):
            try:
                if stat.S_ISREG((Path(source) / path).lstat().st_mode):
                    paths.add(path)
            except FileNotFoundError:
                pass
    return sorted(paths)


def template_parts(template):
    if not template:
        return []
    try:
        parts = shlex.split(template)
    except ValueError as error:
        raise DeliveryError("ADDED_TEST_COMMAND cannot be split: " + str(error)) from error
    names = {name for part in parts for name in PLACEHOLDER.findall(part)}
    if not names.intersection({"file", "package", "pkg"}) or names - {"file", "package", "pkg"}:
        raise DeliveryError("ADDED_TEST_COMMAND must use {file}, {package} or {pkg}, with no unknown placeholder")
    return parts


def expand(template, paths):
    """Build distinct argv lists without passing paths through a shell."""
    parts = template_parts(template)
    commands, seen = [], set()
    for path in paths:
        values = {"file": "./" + path, "package": "./" + str(PurePosixPath(path).parent)}
        values["pkg"] = values["package"]
        command = [PLACEHOLDER.sub(lambda match: values[match[1]], part) for part in parts]
        if tuple(command) not in seen:
            seen.add(tuple(command))
            commands.append(command)
    return commands


def settings():
    template = os.environ.get("ADDED_TEST_COMMAND", "")
    template_parts(template)
    patterns = [line.strip() for line in os.environ.get("ADDED_TEST_PATTERNS", DEFAULT_PATTERNS).splitlines() if line.strip()]
    if any("/" in rule and not rule.endswith("/") or rule.startswith("/") or ".." in rule.split("/") for rule in patterns):
        raise DeliveryError("ADDED_TEST_PATTERNS requires basename globs or root-relative directory prefixes ending in /")
    try:
        cap = int(os.environ.get("ADDED_TEST_MAX_COMMANDS", "100"))
        if cap < 1:
            raise ValueError()
    except ValueError as error:
        raise DeliveryError("ADDED_TEST_MAX_COMMANDS must be a positive integer") from error
    return template, patterns, cap


def run_commands(source, commands, report):
    """Run all commands, keeping the existing post-delivery command policy."""
    environment = support.git_environment(with_credential=False)
    failures = 0
    for command in commands:
        try:
            status, output, diagnostics = support.run(command, cwd=str(source), check=False, environment=environment,
                                                      timeout=support.number("VERIFY_TIMEOUT_SECONDS", 1200))
        except FileNotFoundError:
            status, output, diagnostics = 127, "", "The configured verification command is not installed"
        except subprocess.TimeoutExpired:
            status, output, diagnostics = 124, "", "The configured verification command ran out of time"
        text = (output + diagnostics).strip()
        if len(text) > 4000:
            text = "… " + text[-4000:]
        label = " ".join(support.readable(shlex.quote(part)) for part in command)
        report.append("Command: %s\nExit status: %d\n%s" % (label, status, text or "(no output)"))
        failures += status != 0
    return failures


def run(source, paths, report, configuration):
    template, patterns, cap = configuration
    paths = [path for path in paths if any(path.startswith(rule) if rule.endswith("/")
             else fnmatch.fnmatchcase(PurePosixPath(path).name, rule) for rule in patterns)]
    report.append("%d added test files matched the configured path rules." % len(paths))
    if not paths:
        return 0
    report.append("Added tests: " + ", ".join(map(support.readable, paths)))
    if not template:
        report.append("Added tests not executed individually: ADDED_TEST_COMMAND is not configured.")
        return 0
    present = []
    for path in paths:
        target = Path(source) / path
        if not target.is_file() or target.is_symlink():
            report.append("Delivered test %s is absent at %s (or no longer a regular file); not run."
                          % (support.readable(path), git(source, "rev-parse", "--short", "HEAD", check=False) or "unborn HEAD"))
        else:
            present.append(path)
    commands = expand(template, present)
    failures = run_commands(source, commands[:cap], report)
    report.append("%d of %d added-test commands failed." % (failures, min(len(commands), cap)))
    excess = max(0, len(commands) - cap)
    if excess:
        report.append("%d additional command%s not run: ADDED_TEST_MAX_COMMANDS=%d." % (excess, "" if excess == 1 else "s", cap))
    return failures + bool(excess)


def verify(arguments):
    if arguments:
        raise DeliveryError("The added-test process takes no arguments")
    configuration = settings()
    support.discard_prompt()
    source = Path(os.environ.get("TASK_WORKSPACE") or os.getcwd())
    report = []
    try:
        paths = detect(source, workspace_base(source))
    except DeliveryError as error:
        print("Added tests not executed individually: detection unavailable (%s)." % error)
        return 0
    failures = run(source, paths, report, configuration)
    print("\n\n".join(report))
    return int(bool(failures))


if __name__ == "__main__":
    sys.exit(support.main(verify))
