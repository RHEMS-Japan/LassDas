"""Run a configured role in bubblewrap, without interpreting its answer.

The controller prepares Git before this command. Operator-specified paths are
permissions, not fields requested from a model. Network inheritance is explicit
and requires the outer runtime's egress policy; this is not a network firewall.
"""
import argparse
from collections import namedtuple
import os
from pathlib import Path, PurePosixPath
import resource
import shutil
import signal
import stat
import sys
import time


# Debian's alternatives: /usr/bin/cc, awk and others are symlinks into this
# directory, which point back into /usr. A sandbox that shows /usr without it
# leaves those programs dangling (rustc found no `cc` in a role).
ALTERNATIVES = Path("/etc/alternatives")

# The container's own memory accounting. A container on cgroup v2 with its own
# cgroup namespace sees its cgroup here; without one, or without a limit,
# there is nothing for the memory guard below to keep the role under.
CGROUP = Path("/sys/fs/cgroup")
PROC = Path("/proc")
MIB = 1 << 20


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


def container_memory(cgroup=None):
    """The container's memory limit and the part of its use the kernel cannot
    drop, in bytes, or None when there is no limit or no cgroup v2 to read.

    File cache is left out: the kernel reclaims it before it stops anything.
    Anonymous memory, shared memory (a role's private /tmp is one) and kernel
    memory stay in."""
    cgroup = CGROUP if cgroup is None else cgroup
    try:
        limit = (cgroup / "memory.max").read_text().strip()
        if limit == "max":
            return None
        current = int((cgroup / "memory.current").read_text())
        stat = dict(line.split(None, 1) for line in (cgroup / "memory.stat").read_text().splitlines() if line.strip())
        cache = int(stat.get("active_file", 0)) + int(stat.get("inactive_file", 0))
        return int(limit), current - cache
    except (OSError, ValueError):
        return None


def memory_threshold(headroom_mib, cgroup=None):
    """The use at which the guard stops a role process, or None without a limit.

    The headroom is kept for the controller and the processes that are not a
    role's. Unset, it is an eighth of the limit and at least 512 MiB."""
    usage = container_memory(cgroup)
    if usage is None:
        return None
    limit = usage[0]
    headroom = max(512 * MIB, limit // 8) if headroom_mib is None else headroom_mib * MIB
    if headroom <= 0 or headroom >= limit:
        raise ValueError("The memory headroom must be above zero and below the container's memory limit")
    return limit - headroom


Process = namedtuple("Process", "parent name anon uid nested")


def processes(proc=None):
    """Every visible process: its parent, name, resident anonymous bytes, real
    uid, and whether it runs in a PID namespace below this one (every role's
    processes do; the controller's and the launchers' do not)."""
    proc = PROC if proc is None else proc

    def fields(status):
        return dict(line.split(":", 1) for line in status.splitlines() if ":" in line)

    with open(os.path.join(proc, "self", "status")) as handle:
        depth = len(fields(handle.read()).get("NSpid", "").split())
    table = {}
    for entry in os.scandir(proc):
        if not entry.name.isdigit():
            continue
        try:
            with open(os.path.join(entry.path, "stat")) as handle:
                line = handle.read()
            with open(os.path.join(entry.path, "status")) as handle:
                status = fields(handle.read())
            name = line[line.index("(") + 1:line.rindex(")")]
            parent = int(line[line.rindex(")") + 1:].split()[1])
            anon = int(status.get("RssAnon", "0 kB").split()[0]) * 1024
            table[int(entry.name)] = Process(parent, name, anon, int(status["Uid"].split()[0]),
                                             len(status.get("NSpid", "").split()) > depth)
        except (OSError, ValueError, KeyError, IndexError):
            continue  # it ended while it was read, or it is not ours to read
    return table


def descendants(table, root):
    children = {}
    for pid, process in table.items():
        children.setdefault(process.parent, []).append(pid)
    found, pending = set(), [root]
    while pending:
        for child in children.get(pending.pop(), ()):
            if child not in found:
                found.add(child)
                pending.append(child)
    return found


def victim(table, root, uid):
    """The largest role process in the container, if it belongs to the role
    launched as root; None otherwise. Every launcher applies the same rule,
    so concurrent roles agree on one process, and only its own launcher stops
    it and says so in that role's record."""
    roles = [pid for pid, process in table.items() if process.nested and process.uid == uid]
    if not roles:
        return None
    largest = max(roles, key=lambda pid: (table[pid].anon, pid))
    return largest if largest in descendants(table, root) else None


def guard(root, threshold, report, alive, *, pause=time.sleep, cgroup=None, proc=None, interval=0.2, settle=1.0):
    """Stop the role's largest process while the container's use is at or
    over the threshold, before the kernel's out-of-memory handling can stop
    the whole container (the kubelet asks for that on cgroup v2) and the
    controller with it. A stopped process is reported on report."""
    uid = os.getuid()
    while alive():
        usage = container_memory(cgroup)
        if usage is not None and usage[1] >= threshold:
            table = processes(proc)
            pid = victim(table, root, uid)
            if pid is not None:
                try:
                    os.kill(pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                else:
                    report.write("(launcher) Stopped %s (pid %d, %d MiB resident) because this container's memory "
                                 "in use reached %d of %d MiB. The launcher stops a role's largest process before "
                                 "the kernel would stop the whole container; the role's other processes continue.\n"
                                 % (table[pid].name, pid, table[pid].anon // MIB, usage[1] // MIB, usage[0] // MIB))
                    report.flush()
                    pause(settle)
                    continue
        pause(interval)


def supervise(argv, environment, descriptors, threshold):
    """Run bubblewrap as this process's child and guard the role while it runs.

    The launcher ends as bubblewrap ended, with its exit status or its signal,
    and leaves no process behind for the controller to collect. A signal the
    controller sends the launcher's process group reaches bubblewrap as it
    did before; bubblewrap also ends with this process (--die-with-parent)."""
    signal.pthread_sigmask(signal.SIG_BLOCK, {signal.SIGCHLD})
    child = os.fork()
    if child == 0:
        try:
            signal.pthread_sigmask(signal.SIG_UNBLOCK, {signal.SIGCHLD})
            contain(threshold)
            for descriptor in descriptors:
                os.set_inheritable(descriptor, True)
            os.execve(argv[0], argv, environment)
        except BaseException as error:
            print("(launcher) bubblewrap could not be started: %s" % error, file=sys.stderr, flush=True)
        finally:
            os._exit(127)
    ended = []

    def alive():
        if not ended:
            pid, status = os.waitpid(child, os.WNOHANG)
            if pid:
                ended.append(status)
        return not ended

    try:
        guard(child, threshold, sys.stderr, alive, pause=lambda seconds: signal.sigtimedwait({signal.SIGCHLD}, seconds))
    except Exception as error:
        # A fault of the guard is not a reason to end the role: it runs on unguarded.
        print("(launcher) The memory guard stopped: %s" % error, file=sys.stderr, flush=True)
    if not ended:
        ended.append(os.waitpid(child, 0)[1])
    if os.WIFSIGNALED(ended[0]):
        number = os.WTERMSIG(ended[0])
        try:
            signal.signal(number, signal.SIG_DFL)
        except (OSError, ValueError):
            pass
        signal.pthread_sigmask(signal.SIG_UNBLOCK, {number})
        os.kill(os.getpid(), number)
        return 128 + number
    return os.WEXITSTATUS(ended[0])


def contain(threshold):
    """Make the role's processes the first the kernel stops for memory, and
    keep any single one of them under the guard's threshold. The guard sees a
    process only between its looks; this limit holds at every allocation."""
    try:
        with open("/proc/self/oom_score_adj", "w") as handle:
            handle.write("1000")
    except OSError as error:
        print("(launcher) The role's processes could not be made the first the kernel stops for memory: %s"
              % error.strerror, file=sys.stderr)
    if threshold is not None:
        _, hard = resource.getrlimit(resource.RLIMIT_DATA)
        limit = threshold if hard == resource.RLIM_INFINITY else min(threshold, hard)
        resource.setrlimit(resource.RLIMIT_DATA, (limit, limit))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--write", action="append", default=[], help="existing workspace-relative writable path; '.' grants the whole workspace")
    parser.add_argument("--create", action="append", default=[], help="workspace-relative output directory, created when missing, then writable")
    parser.add_argument("--runtime", action="append", default=[], help="additional absolute read-only tool/runtime path")
    parser.add_argument("--network", choices=("none", "inherit"), default="none")
    parser.add_argument("--memory-headroom", type=int, metavar="MIB",
                        help="memory kept free below the container's limit; the role's largest process is stopped once "
                             "less is left (default: an eighth of the limit, at least 512)")
    parser.add_argument("program", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    argv, environment, descriptors = command(args, os.environ)
    try:
        # Without a guard, a role's build that outgrows the container takes the
        # controller down with it, and the restart repeats the same stage.
        threshold = memory_threshold(args.memory_headroom)
        if threshold is not None:
            sys.exit(supervise(argv, environment, descriptors, threshold))
        contain(None)
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
