"""Run a configured role in bubblewrap, without interpreting its answer.

The controller prepares Git before this command. Operator-specified paths are
permissions, not fields requested from a model. Network inheritance is explicit
and requires the outer runtime's egress policy; this is not a network firewall.
"""
import argparse
from collections import namedtuple
import ctypes
import os
from pathlib import Path, PurePosixPath
import shutil
import signal
import stat
import sys
import time


# Debian's alternatives: /usr/bin/cc, awk and others are symlinks into this
# directory, which point back into /usr. A sandbox that shows /usr without it
# leaves those programs dangling (rustc found no `cc` in a role).
ALTERNATIVES = Path("/etc/alternatives")

# The container's own memory accounting: cgroup v2, mounted here, the
# process's own cgroup named in /proc/self/cgroup ("0::/" with a cgroup
# namespace of its own, the full path without one).
CGROUP = Path("/sys/fs/cgroup")
PROC = Path("/proc")
MIB = 1 << 20
# What the kernel takes back before it stops any process: file cache and the
# reclaimable kernel caches (directory and inode entries). It stays out of the
# memory the guard counts as in use.
RECLAIMABLE = ("active_file", "inactive_file", "slab_reclaimable")
# The fastest one process filled memory in a measurement (22 to 33 GiB/s,
# transparent huge pages "always"). A tenth of a second between two looks can
# miss that much growth, so the guard looks a hundred times a second once the
# use is within it of the limit (always, in a container of 3.2 GiB or less).
FILL_RATE = 32 << 30  # bytes per second
# Signals the controller sends to stop a role. The launcher passes them on to
# bubblewrap and waits for it, so it is not left for the controller to collect.
PASSED_ON = (signal.SIGTERM, signal.SIGINT, signal.SIGHUP)


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


def own_cgroup(cgroup=None, proc=None):
    """This process's cgroup v2 directory and None, or None and the reason
    there is none to read."""
    cgroup = CGROUP if cgroup is None else cgroup
    proc = PROC if proc is None else proc
    try:
        lines = (proc / "self" / "cgroup").read_text().splitlines()
    except OSError as error:
        return None, "%s could not be read (%s)" % (proc / "self" / "cgroup", error.strerror)
    for line in lines:
        if line.startswith("0::"):
            return cgroup / line[3:].lstrip("/"), None
    return None, "this process is in no cgroup v2 hierarchy (%s has no 0:: line)" % (proc / "self" / "cgroup")


def memory_reading(directory):
    """The cgroup's memory in use, less what the kernel reclaims before it
    stops a process, and the anonymous memory in it. Anonymous and shared
    memory (a role's /tmp is shared memory), kernel stacks, page tables and
    unreclaimable kernel memory stay in."""
    current = int((directory / "memory.current").read_text())
    stat = dict(line.split(None, 1) for line in (directory / "memory.stat").read_text().splitlines() if line.strip())
    return current - sum(int(stat.get(name, 0)) for name in RECLAIMABLE), int(stat["anon"])


def memory_in_use(directory):
    return memory_reading(directory)[0]


Limits = namedtuple("Limits", "directory limit threshold")


def memory_guard(directory, headroom_mib):
    """Limits when the guard can run; otherwise None and the reason it cannot,
    which is None when there is no limit to keep under."""
    try:
        text = (directory / "memory.max").read_text().strip()
        if text == "max":
            return None, None
        limit = int(text)
        memory_in_use(directory)
    except (OSError, ValueError, KeyError) as error:
        return None, "the container's memory use could not be read in %s (%s)" % (
            directory, getattr(error, "strerror", None) or error)
    headroom = max(512 * MIB, limit // 8) if headroom_mib is None else headroom_mib * MIB
    if not 0 < headroom < limit:
        return None, "a headroom of %d MiB does not fit under the container's memory limit of %d MiB" % (
            headroom // MIB, limit // MIB)
    return Limits(directory, limit, limit - headroom), None


def kills_the_whole_container(directory):
    """Whether the kernel stops every process of this cgroup together once one
    is chosen (the kubelet sets this on cgroup v2)."""
    try:
        return directory is not None and (directory / "memory.oom.group").read_text().strip() == "1"
    except OSError:
        return False


# start: the process's start time, which tells a process from a later one
# given the same id. leaving: it is ending (exiting or a zombie), so its
# memory is already on its way back and it is not stopped again.
Process = namedtuple("Process", "parent name anon uid nested start leaving", defaults=(0, False))
EXITING = 0x4  # PF_EXITING in the flags of /proc/<pid>/stat


def processes(proc=None):
    """Every visible process: its parent, name, resident anonymous bytes, real
    uid, whether it runs in a PID namespace below this one (every role's
    processes do; the controller's and the launchers' do not), its start time
    and whether it is ending."""
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
            after = line[line.rindex(")") + 1:].split()
            anon = int(status.get("RssAnon", "0 kB").split()[0]) * 1024
            table[int(entry.name)] = Process(int(after[1]), name, anon, int(status["Uid"].split()[0]),
                                             len(status.get("NSpid", "").split()) > depth, int(after[19]),
                                             after[0] in ("Z", "X", "x") or bool(int(after[6]) & EXITING))
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


def victim(table, root, uid, leaving=()):
    """The largest role process in the container that is not already ending,
    if it belongs to the role launched as root; None otherwise. Every launcher
    applies the same rule, so concurrent roles agree on one process, and only
    its own launcher stops it and says so in that role's record."""
    roles = [pid for pid, process in table.items()
             if process.nested and process.uid == uid and not process.leaving and pid not in leaving]
    if not roles:
        return None
    largest = max(roles, key=lambda pid: (table[pid].anon, pid))
    return largest if largest in descendants(table, root) else None


def printable(text):
    return "".join(character if character.isprintable() else "?" for character in text)


def tell(pid, text, proc=None):
    """Write text to a process's standard error before it is stopped, when that
    is a pipe or a terminal: the tool that started it (a build tool, a model's
    terminal) then shows why it ended. Never a file: the role's own files are
    not written to. Never waits: a full pipe or one without a reader is skipped.
    Never the launcher's own standard output or error: the first is the role's
    answer, and the second gets the line anyway. A socket (a Node.js program's
    child has one) cannot be opened this way and is skipped too."""
    proc = PROC if proc is None else proc
    path = os.path.join(proc, str(pid), "fd", "2")
    try:
        target = os.stat(path)
        if not (stat.S_ISFIFO(target.st_mode) or stat.S_ISCHR(target.st_mode)):
            return
        for own in (1, 2):
            try:
                mine = os.fstat(own)
            except OSError:
                continue
            if (mine.st_dev, mine.st_ino) == (target.st_dev, target.st_ino):
                return
        descriptor = os.open(path, os.O_WRONLY | os.O_NONBLOCK | os.O_NOCTTY | os.O_CLOEXEC)
    except OSError:
        return
    try:
        os.write(descriptor, text.encode("utf-8", "replace"))
    except OSError:
        pass
    finally:
        os.close(descriptor)


def guard(root, limits, report, alive, *, pause=time.sleep, proc=None, interval=0.1, near=0.01):
    """Stop the role's largest process while the container's use is at or
    over the threshold, before the kernel's out-of-memory handling can stop
    the whole container (the kubelet asks for that on cgroup v2) and the
    controller with it. A stopped process is told why on its standard error,
    and the same line goes to report (the role's record).

    It looks every interval while the use is further from the limit than one
    process can fill in that time (FILL_RATE), and every near once it is
    nearer, so one process taking a large block at once is seen within near;
    over the threshold it looks again after near whether it stopped a process
    or not, so a second process growing beside the stopped one is seen too.
    While a stopped process is still ending, its memory counts as already
    given back."""
    uid = os.getuid()
    last_resort = limits.limit - (limits.limit - limits.threshold) // 2
    near_from = limits.limit - int(FILL_RATE * interval)
    stopped = set()  # (pid, start) of the processes this launcher stopped
    said = False
    current = os.open(limits.directory / "memory.current", os.O_RDONLY | os.O_CLOEXEC)
    try:
        while alive():
            # memory.current bounds the use from above; the full reading is needed only near the threshold.
            reading = int(os.pread(current, 64, 0))
            if reading >= limits.threshold:
                used, anon = memory_reading(limits.directory)
                if used >= limits.threshold:
                    table = processes(proc)
                    stopped = {key for key in stopped if key[0] in table and table[key[0]].start == key[1]}
                    ending = {pid for pid, _ in stopped} | {pid for pid, process in table.items() if process.leaving}
                    used -= sum(table[pid].anon for pid in ending)
                    roles = sum(process.anon for pid, process in table.items()
                                if process.nested and process.uid == uid and pid not in ending)
                    outside = anon - roles - sum(table[pid].anon for pid in ending)
                    if used >= limits.threshold and outside >= limits.threshold and used < last_resort:
                        # The controller, or another process outside every role, holds that much
                        # by itself: stopping role processes would not bring the use back under
                        # the threshold. Only nearer the limit is a role process stopped anyway.
                        if not said:
                            report.write("(launcher) This container's memory in use reached %d of %d MiB, %d MiB of "
                                         "it anonymous memory outside every role's processes; no role process is "
                                         "stopped for it until the use reaches %d MiB.\n"
                                         % (used // MIB, limits.limit // MIB, outside // MIB, last_resort // MIB))
                            report.flush()
                            said = True
                    elif used >= limits.threshold:
                        pid = victim(table, root, uid, ending)
                        if pid is not None:
                            own = sum(table[other].anon for other in descendants(table, root)
                                      if table[other].nested and other not in ending)
                            line = ("(launcher) Stopped %s (pid %d, %d MiB resident; this role's processes %d MiB in "
                                    "all) because this container's memory in use, file and reclaimable kernel caches "
                                    "left out, reached %d of %d MiB. The launcher stops a role's largest process "
                                    "before the kernel would stop the whole container; the role's other processes "
                                    "continue.\n"
                                    % (printable(table[pid].name), pid, table[pid].anon // MIB, own // MIB,
                                       used // MIB, limits.limit // MIB))
                            tell(pid, line, proc)
                            try:
                                os.kill(pid, signal.SIGKILL)
                            except ProcessLookupError:
                                pass
                            else:
                                stopped.add((pid, table[pid].start))
                                report.write(line)
                                report.flush()
            pause(near if reading >= near_from else interval)
    finally:
        os.close(current)


def collect_children(seconds):
    """Collect the children left once bubblewrap has ended (the sandbox's
    first process, when bubblewrap ended before it), for up to seconds."""
    deadline = time.monotonic() + seconds
    while True:
        try:
            pid, _ = os.waitpid(-1, os.WNOHANG)
        except ChildProcessError:
            return
        if pid == 0:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                return
            signal.sigtimedwait({signal.SIGCHLD}, min(remaining, 0.1))


def supervise(argv, environment, descriptors, limits, kill_together):
    """Run bubblewrap as this process's child and guard the role while it runs.

    The launcher ends as bubblewrap ended, with its exit status or its signal.
    A stop signal sent to the launcher is passed on to bubblewrap, and the
    launcher waits for it, so neither is left behind for the controller to
    collect; bubblewrap also ends with this process (--die-with-parent)."""
    held = {signal.SIGCHLD, *PASSED_ON}
    signal.pthread_sigmask(signal.SIG_BLOCK, held)
    try:
        ctypes.CDLL(None, use_errno=True).prctl(36, 1, 0, 0, 0)  # PR_SET_CHILD_SUBREAPER
    except (OSError, AttributeError):
        pass
    child = os.fork()
    if child == 0:
        try:
            signal.pthread_sigmask(signal.SIG_UNBLOCK, held)
            contain(kill_together)
            for descriptor in descriptors:
                os.set_inheritable(descriptor, True)
            os.execve(argv[0], argv, environment)
        except BaseException as error:
            print("(launcher) bubblewrap could not be started: %s" % error, file=sys.stderr, flush=True)
        finally:
            os._exit(127)

    def pass_on(number, _frame):
        try:
            os.kill(child, number)
        except ProcessLookupError:
            pass

    for number in PASSED_ON:
        signal.signal(number, pass_on)
    signal.pthread_sigmask(signal.SIG_UNBLOCK, set(PASSED_ON))
    ended = []

    def alive():
        if not ended:
            pid, status = os.waitpid(child, os.WNOHANG)
            if pid:
                ended.append(status)
        return not ended

    try:
        guard(child, limits, sys.stderr, alive, pause=lambda seconds: signal.sigtimedwait({signal.SIGCHLD}, seconds))
    except Exception as error:
        # A fault of the guard is not a reason to end the role: it runs on unguarded.
        print("(launcher) The memory guard stopped: %s" % error, file=sys.stderr, flush=True)
    while not ended:
        try:
            ended.append(os.waitpid(child, 0)[1])
        except InterruptedError:
            continue
    collect_children(2)
    if os.WIFSIGNALED(ended[0]):
        number = os.WTERMSIG(ended[0])
        signal.pthread_sigmask(signal.SIG_BLOCK, set(PASSED_ON))
        try:
            signal.signal(number, signal.SIG_DFL)
        except (OSError, ValueError):
            pass
        signal.pthread_sigmask(signal.SIG_UNBLOCK, {number})
        os.kill(os.getpid(), number)
        return 128 + number
    return os.WEXITSTATUS(ended[0])


def contain(kill_together):
    """Make the role's processes the first the kernel stops for memory, where it
    stops one process at a time. Where it stops a whole container together
    (memory.oom.group), that would only make this container the node's first
    choice when the node runs short, so the role keeps the controller's score."""
    if kill_together:
        return
    try:
        with open("/proc/self/oom_score_adj", "w") as handle:
            handle.write("1000")
    except OSError as error:
        print("(launcher) The role's processes could not be made the first the kernel stops for memory: %s"
              % error.strerror, file=sys.stderr)


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
        directory, reason = own_cgroup()
        limits = None
        if directory is not None:
            limits, reason = memory_guard(directory, args.memory_headroom)
        if reason is not None:
            print("(launcher) The memory guard is off for this role: %s." % reason, file=sys.stderr, flush=True)
        kill_together = kills_the_whole_container(directory)
        if limits is not None:
            sys.exit(supervise(argv, environment, descriptors, limits, kill_together))
        contain(kill_together)
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
