"""Prepare a watch job's empty workspace, then exec its configured launcher.

This runs BEFORE the role's sandbox, never as a model tool. The command after
``--`` must establish the actual role permissions. It is not a source/answer
approval step: existing work is reused verbatim, never reset or updated.

Once a workspace has been prepared, a record of it stays beside the workspace,
in the job's own directory, which no role's sandbox mounts. A workspace found
empty although that record exists was lost, by a restore or by hand: it is
prepared again, the launch says so and ends non-zero without running its
command. What earlier stages did there is gone, and a stage that passed on
that work must not be taken as passed on the fresh checkout. A failed command
stage goes back to its repair stage, after which every later stage runs again;
a model stage simply runs again.
"""
import datetime
import fcntl
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile


def record_path(workspace):
    """The record that the workspace was prepared, beside the preparation lock."""
    return workspace.parent / ("." + workspace.name + ".prepared")


def write_record(workspace, **fields):
    record = record_path(workspace)
    fields["recorded_at"] = datetime.datetime.now(datetime.timezone.utc).isoformat(timespec="seconds")
    temporary = record.with_name(record.name + ".new")
    descriptor = os.open(temporary, os.O_CREAT | os.O_WRONLY | os.O_TRUNC | os.O_NOFOLLOW, 0o600)
    with os.fdopen(descriptor, "w") as handle:
        handle.write(json.dumps(fields, sort_keys=True) + "\n")
    os.replace(temporary, record)


def prepare(workspace, repository, branch=None):
    """Prepare the workspace when it is empty. Returns False when it had been
    prepared before and was found empty: whatever was done in it is gone."""
    if not workspace.is_absolute() or workspace.is_symlink() or not workspace.is_dir():
        raise ValueError("TASK_WORKSPACE must name the existing real job directory")
    # The watch owner creates this directory. Other launchers for the same job
    # serialize only preparation; reviewers may run concurrently afterwards.
    lock_path = workspace.parent / ("." + workspace.name + ".prepare.lock")
    descriptor = os.open(lock_path, os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    with os.fdopen(descriptor, "w") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        prepared_before = os.path.lexists(record_path(workspace))
        if any(workspace.iterdir()):
            # In particular, do not run Git on agent-edited local config outside
            # its sandbox. A different source/ref must not replace ongoing work.
            # A workspace prepared before the record existed is taken as it is.
            if not prepared_before:
                write_record(workspace, found_in_place=True)
            return True
        environment = dict(os.environ)
        environment.update(GIT_CONFIG_GLOBAL=os.devnull, GIT_CONFIG_SYSTEM=os.devnull,
                           GIT_CONFIG_NOSYSTEM="1", GIT_TERMINAL_PROMPT="0")
        for name in ("GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR"):
            environment.pop(name, None)
        # Clone away from the live workspace. An interrupted clone/checkout is
        # never published; a later launch can try again without touching work.
        # A hard kill can leave this private staging directory for operator GC.
        with tempfile.TemporaryDirectory(prefix=".source-", dir=workspace.parent) as temporary:
            staged = Path(temporary) / "repository"
            command = ["git", "-c", "core.hooksPath=" + os.devnull, "clone",
                       "--no-local", "--no-checkout", "--template="]
            if branch:
                command.append("--branch=" + branch)
            command += ["--", repository, str(staged)]
            subprocess.run(command, cwd=workspace.parent, env=environment,
                           stdout=sys.stderr, check=True)
            head = subprocess.run(["git", "-C", str(staged), "rev-parse", "--verify", "--quiet", "HEAD"],
                                  cwd=workspace.parent, env=environment, stdout=subprocess.DEVNULL)
            if head.returncode == 0:
                subprocess.run(["git", "-c", "core.hooksPath=" + os.devnull, "-C", str(staged),
                                "checkout", "--detach", "HEAD"], cwd=workspace.parent,
                               env=environment, stdout=sys.stderr, check=True)
            elif head.returncode != 1:
                head.check_returncode()
            # An empty remote has an unborn HEAD. It is valid starting work,
            # not a reason to keep the first implementation from ever running.
            # os.replace only replaces an empty target directory. Recheck it
            # for a launcher not honoring our lock, and never delete its work.
            if any(workspace.iterdir()):
                raise RuntimeError("Work arrived during preparation; it was not replaced")
            os.replace(staged, workspace)
        # Only a published checkout is on record: an interrupted or failed
        # preparation leaves nothing that could later read as a lost workspace.
        write_record(workspace, prepared_again=prepared_before)
        return not prepared_before


LOST = ("This request's workspace was lost: it had been prepared before and was found empty. It has been "
        "prepared again from the repository, so nothing an earlier stage did in it remains. This launch ends "
        "here without running its command, so the stages whose work was lost run again.")


def main():
    if len(sys.argv) < 3 or sys.argv[1] != "--":
        raise ValueError("Pass -- followed by the configured isolated role launcher")
    workspace = Path(os.environ["TASK_WORKSPACE"])
    repository = os.environ["TASK_REPOSITORY"]
    if not repository:
        raise ValueError("TASK_REPOSITORY must name the operator-approved source")
    if not prepare(workspace, repository, os.environ.get("TASK_BRANCH")):
        print(LOST, flush=True)
        sys.exit(1)
    # The old empty-directory inode may have been replaced, including while a
    # concurrent launcher started there. Enter the newly published directory.
    os.chdir(workspace)
    os.execvpe(sys.argv[2], sys.argv[2:], os.environ)


if __name__ == "__main__":
    main()
