"""Prepare a watch job's empty workspace, then exec its configured launcher.

This runs BEFORE the role's sandbox, never as a model tool. The command after
``--`` must establish the actual role permissions. It is not a source/answer
approval step: existing work is reused verbatim, never reset or updated.
"""
import fcntl
import os
from pathlib import Path
import subprocess
import sys
import tempfile


def prepare(workspace, repository, branch=None):
    if not workspace.is_absolute() or workspace.is_symlink() or not workspace.is_dir():
        raise ValueError("TASK_WORKSPACE must name the existing real job directory")
    # The watch owner creates this directory. Other launchers for the same job
    # serialize only preparation; reviewers may run concurrently afterwards.
    lock_path = workspace.parent / ("." + workspace.name + ".prepare.lock")
    descriptor = os.open(lock_path, os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    with os.fdopen(descriptor, "w") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        if any(workspace.iterdir()):
            # In particular, do not run Git on agent-edited local config outside
            # its sandbox. A different source/ref must not replace ongoing work.
            return
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


def main():
    if len(sys.argv) < 3 or sys.argv[1] != "--":
        raise ValueError("Pass -- followed by the configured isolated role launcher")
    workspace = Path(os.environ["TASK_WORKSPACE"])
    repository = os.environ["TASK_REPOSITORY"]
    if not repository:
        raise ValueError("TASK_REPOSITORY must name the operator-approved source")
    prepare(workspace, repository, os.environ.get("TASK_BRANCH"))
    # The old empty-directory inode may have been replaced, including while a
    # concurrent launcher started there. Enter the newly published directory.
    os.chdir(workspace)
    os.execvpe(sys.argv[2], sys.argv[2:], os.environ)


if __name__ == "__main__":
    main()
