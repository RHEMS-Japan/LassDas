"""Prepare a watch job's empty workspace, then exec its configured launcher.

This runs BEFORE the role's sandbox, never as a model tool. The command after
``--`` must establish the actual role permissions. It is not a source/answer
approval step: work is never reset. Only an untouched entrance checkout may
advance after a requester reply, before any later stage has run.

Once a workspace has been prepared, a record of it stays beside the workspace,
in the job's own directory, which no role's sandbox mounts. A workspace found
empty although that record exists was lost, by a restore or by hand: it is
prepared again, the launch says so and ends non-zero without running its
command. What earlier stages did there is gone, and a stage that passed on
that work must not be taken as passed on the fresh checkout. A failed command
stage goes back to its repair stage, after which every later stage runs again;
a model stage simply runs again.
"""
import ctypes
import datetime
import fcntl
import json
import os
from pathlib import Path
import shutil
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


def git_environment():
    environment = dict(os.environ)
    environment.update(GIT_CONFIG_GLOBAL=os.devnull, GIT_CONFIG_SYSTEM=os.devnull,
                       GIT_CONFIG_NOSYSTEM="1", GIT_TERMINAL_PROMPT="0")
    for name in ("GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR"):
        environment.pop(name, None)
    return environment


def entrance_reply():
    """Use the controller's observed launches, never words in a role's report."""
    path = os.environ.get("TASK_HISTORY")
    if not path:
        return 0
    if Path(path).stat().st_size > 64 * 1024 * 1024:
        raise ValueError("request history exceeds the 64 MiB local read limit")
    with open(path) as handle:
        state = json.load(handle)
    workflow = state.get("workflow") or {}
    stages, question = workflow.get("stages"), workflow.get("question")
    if not stages or not question or state.get("waiting"):
        return 0
    entrance = stages[0]["name"]
    reply = 0
    for index, result in enumerate(state.get("history") or []):
        if result.get("speaker") == "runtime" and result.get("role") == "router":
            continue
        if result.get("role") not in (entrance, question):
            return 0
        if result.get("speaker") == "requester" and result.get("role") == question:
            reply = index + 1
    return reply


def finish_refresh(workspace, recovered=True):
    """Resolve the record after an atomic exchange, before any role starts."""
    with record_path(workspace).open() as handle:
        record = json.load(handle)
    target = record.get("refresh_head")
    if not target:
        return
    head_path = workspace / ".git/HEAD"
    if head_path.is_symlink():
        raise RuntimeError("Workspace refresh HEAD must not be a symlink")
    head = head_path.read_text().strip()
    if head not in (record["head"], target):
        raise RuntimeError("Workspace refresh found an unexpected HEAD; the role was not launched")
    record["head"] = head
    del record["refresh_head"]
    write_record(workspace, **record)
    if recovered:
        print("Workspace refresh recovered at " + head, file=sys.stderr, flush=True)


def exchange_directories(source, target):
    """Use the OS atomic exchange; never emulate it with two directory moves."""
    libc = ctypes.CDLL(None, use_errno=True)
    path, number = ctypes.c_char_p, ctypes.c_int
    if sys.platform == "darwin" and hasattr(libc, "renamex_np"):
        operation = libc.renamex_np
        operation.argtypes = [path, path, ctypes.c_uint]
        arguments = (os.fsencode(source), os.fsencode(target), 2)  # RENAME_SWAP
    elif sys.platform.startswith("linux") and hasattr(libc, "renameat2"):
        operation = libc.renameat2
        operation.argtypes = [number, path, number, path, ctypes.c_uint]
        arguments = (-100, os.fsencode(source), -100, os.fsencode(target), 2)  # AT_FDCWD, RENAME_EXCHANGE
    else:
        raise OSError("atomic workspace exchange is not available on this host")
    operation.restype = number
    if operation(*arguments) != 0:
        error = ctypes.get_errno()
        raise OSError(error, os.strerror(error))


def refresh(workspace, repository, branch):
    """Called under the preparation lock. Failure never discards local work."""
    try:
        with record_path(workspace).open() as handle:
            record = json.load(handle)
        original = record.get("head")
        if not original or record.get("repository") != repository or record.get("branch") != branch:
            return
        reply = entrance_reply()
        if not reply or reply <= record.get("refresh_reply", 0):
            return
        metadata = workspace / ".git"
        # The controller created a detached checkout. Never open model-edited
        # configuration, an external gitdir, or a linked worktree in host Git.
        if metadata.is_symlink() or not metadata.is_dir() or (metadata / "commondir").exists():
            return
        for name, expected in (("HEAD", original + "\n"), ("config", record.get("git_config"))):
            path = metadata / name
            if path.is_symlink() or not path.is_file() or path.read_text() != expected:
                return
        environment = git_environment()

        def git(*args, directory=workspace):
            return subprocess.run(["git", "-c", "core.hooksPath=" + os.devnull,
                                   "-c", "core.fsmonitor=false", "-c", "submodule.recurse=false",
                                   "-C", str(directory), *args], env=environment,
                                  capture_output=True, text=True)

        clean = git("status", "--porcelain", "--untracked-files=all", "--ignored", "--ignore-submodules=none")
        if clean.returncode:
            print("Workspace refresh could not inspect the checkout; continuing without a reset: " +
                  clean.stderr.strip(), file=sys.stderr, flush=True)
            return
        if clean.stdout:
            return
        # A reply is checked once, including when the source is unavailable.
        # Parallel processes and retries must not keep moving the starting point.
        record["refresh_reply"] = reply
        write_record(workspace, **record)
        fetched = git("fetch", "--no-tags", "--", repository, branch or "HEAD")
        if fetched.returncode:
            print("Workspace refresh could not fetch; continuing with " + original + ": " +
                  fetched.stderr.strip(), file=sys.stderr, flush=True)
            return
        target = git("rev-parse", "--verify", "FETCH_HEAD")
        target.check_returncode()
        latest = target.stdout.strip()
        if latest == original:
            return
        with tempfile.TemporaryDirectory(prefix=".refresh-", dir=workspace.parent) as temporary:
            staged = Path(temporary) / "repository"
            shutil.copytree(workspace, staged, symlinks=True)
            updated = git("checkout", "--detach", latest, directory=staged)
            checked = git("status", "--porcelain", "--untracked-files=all", "--ignored", directory=staged)
            actual = git("rev-parse", "HEAD", directory=staged)
            if updated.returncode or checked.returncode or checked.stdout or actual.returncode or actual.stdout.strip() != latest:
                print("Workspace refresh could not prepare a complete replacement; continuing with the original checkout: " +
                      (updated.stderr + checked.stderr + checked.stdout + actual.stderr).strip(),
                      file=sys.stderr, flush=True)
                return
            # An external writer need not honor the launcher lock. Do not
            # replace work that appeared while the private checkout was built.
            unchanged = git("status", "--porcelain", "--untracked-files=all", "--ignored")
            if unchanged.returncode or unchanged.stdout or (metadata / "HEAD").read_text() != original + "\n":
                print("Workspace refresh kept the original checkout because it changed during preparation",
                      file=sys.stderr, flush=True)
                return
            record["refresh_head"] = latest
            write_record(workspace, **record)
            try:
                exchange_directories(staged, workspace)
            except OSError:
                finish_refresh(workspace)
                raise
            finish_refresh(workspace, recovered=False)
        print("Workspace updated from " + original + " to " + latest, file=sys.stderr, flush=True)
    except (OSError, ValueError, TypeError, KeyError, AttributeError, IndexError, subprocess.SubprocessError) as error:
        print("Workspace refresh could not be checked; continuing without a reset: " + str(error),
              file=sys.stderr, flush=True)


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
        if prepared_before:
            finish_refresh(workspace)
        if any(workspace.iterdir()):
            if not prepared_before:
                write_record(workspace, found_in_place=True)
            else:
                refresh(workspace, repository, branch)
            return True
        environment = git_environment()
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
                                  cwd=workspace.parent, env=environment, capture_output=True, text=True)
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
        write_record(workspace, prepared_again=prepared_before, repository=repository, branch=branch,
                     head=head.stdout.strip(), git_config=(workspace / ".git/config").read_text())
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
