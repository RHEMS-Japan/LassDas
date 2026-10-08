"""Run a configured role in bubblewrap, without interpreting its answer.

The controller prepares Git before this command. Operator-specified paths are
permissions, not fields requested from a model. Network inheritance is explicit
and requires the outer runtime's egress policy; this is not a network firewall.
"""
import argparse
import os
from pathlib import Path, PurePosixPath
import shutil
import stat
import sys


# Debian's alternatives: /usr/bin/cc, awk and others are symlinks into this
# directory, which point back into /usr. A sandbox that shows /usr without it
# leaves those programs dangling (rustc found no `cc` in a role).
ALTERNATIVES = Path("/etc/alternatives")


def absolute(value):
    path = Path(value)
    if path.anchor != "/" or ".." in path.parts or path == Path("/"):
        raise ValueError("Use an absolute non-root path with one leading slash and without '..'")
    return path


def open_path(path, *, directory=False, create=False):
    """Pin each component without following a symlink, including on relaunch."""
    descriptor = os.open("/", os.O_PATH | os.O_DIRECTORY)
    try:
        for index, name in enumerate(path.parts[1:]):
            last = index == len(path.parts) - 2
            flags = os.O_PATH | os.O_NOFOLLOW
            if not last or directory:
                flags |= os.O_DIRECTORY
            if create:
                try:
                    os.mkdir(name, mode=0o700, dir_fd=descriptor)
                except FileExistsError:
                    pass
            child = os.open(name, flags, dir_fd=descriptor)
            os.close(descriptor)
            descriptor = child
            if stat.S_ISLNK(os.fstat(descriptor).st_mode):
                raise ValueError("A launch path is a symlink; no target was followed")
        return descriptor
    except BaseException:
        os.close(descriptor)
        raise


def create_path(workspace, relative):
    """Make the directory under the workspace one component at a time, never
    following a symbolic link: a link a role planted in the way refuses the
    launch instead of leading the new directory outside the workspace."""
    descriptor = os.open(workspace, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        for part in relative.parts:
            try:
                os.mkdir(part, 0o700, dir_fd=descriptor)
            except FileExistsError:
                pass
            child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=descriptor)
            os.close(descriptor)
            descriptor = child
    finally:
        os.close(descriptor)


def command(args, environment):
    if sys.platform != "linux":
        raise RuntimeError("This launcher requires Linux namespaces and bubblewrap")
    bwrap = shutil.which("bwrap")
    if bwrap is None:
        raise RuntimeError("bubblewrap is unavailable; no unconfined command was started")
    workspace = absolute(environment["TASK_WORKSPACE"])
    home = absolute(environment["TASK_HOME"])
    if home == workspace or home.is_relative_to(workspace) or workspace.is_relative_to(home):
        raise ValueError("TASK_HOME must be separate from the shared workspace")
    if not args.program:
        raise ValueError("Pass -- followed by the role's configured command")
    program = args.program[1:] if args.program[0] == "--" else args.program
    if not program:
        raise ValueError("No configured role command")
    writable = []
    for value in args.write:
        relative = PurePosixPath(value)
        if not value or relative.is_absolute() or ".." in relative.parts:
            raise ValueError("Writable paths must be relative to the workspace without '..'")
        writable.append(workspace / relative)
    for value in args.create:
        # An output directory of the runtime's own (a report, a receipt): made
        # inside the workspace when the checkout does not have it, then
        # granted like any other writable path. A path that exists already
        # is used as it is; a symbolic link there fails at the mount.
        relative = PurePosixPath(value)
        if not value or relative.is_absolute() or ".." in relative.parts:
            raise ValueError("Created paths must be relative to the workspace without '..'")
        create_path(workspace, relative)
        writable.append(workspace / relative)
    runtimes = [absolute(value) for value in args.runtime]
    history = absolute(environment["TASK_HISTORY"]) if environment.get("TASK_HISTORY") else None
    if history is not None:
        for path in [workspace, home, *runtimes]:
            if history == path or history.is_relative_to(path) or path.is_relative_to(history):
                raise ValueError("Request history must be a separate read-only file, not inside another mount")
    for path in runtimes:
        if any(path == grant or path.is_relative_to(grant) or grant.is_relative_to(path)
               for grant in (workspace, home)):
            raise ValueError("Runtime mounts must not overlap task data")

    descriptors = []

    def mount(path, read_only=True, *, directory=False, create=False):
        descriptor = open_path(path, directory=directory, create=create)
        descriptors.append(descriptor)
        return ["--ro-bind-fd" if read_only else "--bind-fd", str(descriptor), str(path)]

    try:
        result = [bwrap, "--unshare-all", "--unshare-user", "--disable-userns",
                  "--die-with-parent", "--as-pid-1", "--new-session", "--cap-drop", "ALL"]
        if args.network == "inherit":
            result += ["--share-net"]
        # An empty root, not a read-only view of the controller's filesystem.
        result += mount(Path("/usr"), directory=True)
        for path in (Path("/bin"), Path("/sbin"), Path("/lib"), Path("/lib64")):
            if path.is_symlink():
                target = path.resolve(strict=True)
                if not target.is_relative_to(Path("/usr")):
                    raise ValueError("System toolchain symlink does not point inside /usr")
                result += ["--symlink", str(target), str(path)]
            elif path.exists():
                result += mount(path, directory=True)
        for name in ("ssl/certs", "resolv.conf", "hosts", "nsswitch.conf", "passwd", "group"):
            path = Path("/etc") / name
            if path.exists():
                # System-managed symlinks (e.g. resolved's resolv.conf) are
                # readable configuration, unlike model-writable task paths.
                descriptor = os.open(path, os.O_PATH)
                descriptors.append(descriptor)
                result += ["--ro-bind-fd", str(descriptor), str(path)]
        if ALTERNATIVES.is_dir() and not ALTERNATIVES.is_symlink():
            result += mount(ALTERNATIVES, directory=True)
        # Mount the private base first: a later /tmp would hide explicitly
        # granted work, home or SDK paths beneath it (including the preflight).
        result += ["--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp"]
        for path in runtimes:
            result += mount(path)
        result += mount(workspace, directory=True)
        for path in writable:
            # Missing write targets are not silently created in the project.
            # Grant an existing parent, or prepare an output directory first.
            result += mount(path, read_only=False)
        for path in writable:
            # A checkout's own metadata stays read-only under a grant that
            # covers it: hooks and configuration inside .git run as whatever
            # process opens the repository next, and the delivery process
            # opens it holding the credential no role may have. Only a grant
            # that names .git itself opens it (the delivery process's own).
            metadata = path / ".git"
            if metadata.is_dir() and not metadata.is_symlink() and metadata not in writable:
                result += mount(metadata, directory=True)
        result += mount(home, read_only=False, directory=True, create=True)
        if history is not None:
            descriptor = open_path(history)
            descriptors.append(descriptor)
            if not stat.S_ISREG(os.fstat(descriptor).st_mode):
                raise ValueError("Request history must be a regular file")
            # Only the file is visible. Its empty destination parents do not
            # expose the controller's directory or neighboring requests.
            result += ["--ro-bind-fd", str(descriptor), str(history)]
        result += ["--chdir", str(workspace), "--", *program]
        child_environment = dict(environment)
        child_environment.update(HOME=str(home), HERMES_HOME=str(home), TMPDIR="/tmp")
        return result, child_environment, descriptors
    except BaseException:
        for descriptor in descriptors:
            os.close(descriptor)
        raise


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--write", action="append", default=[], help="existing workspace-relative writable path; '.' grants the whole workspace")
    parser.add_argument("--create", action="append", default=[], help="workspace-relative output directory, created when missing, then writable")
    parser.add_argument("--runtime", action="append", default=[], help="additional absolute read-only tool/runtime path")
    parser.add_argument("--network", choices=("none", "inherit"), default="none")
    parser.add_argument("program", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    argv, environment, descriptors = command(args, os.environ)
    try:
        for descriptor in descriptors:
            os.set_inheritable(descriptor, True)
        # Stdin, stdout and stderr pass through. No buffering, output decoding,
        # fallback to host execution or SDK/role-answer classification.
        os.execve(argv[0], argv, environment)
    finally:
        for descriptor in descriptors:
            os.close(descriptor)


if __name__ == "__main__":
    main()
