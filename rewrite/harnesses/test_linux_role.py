"""Configuration/path tests. Real namespace checks run separately on Linux."""
import argparse
import importlib.util
import io
import os
from pathlib import Path, PurePosixPath
import shutil
import signal
import subprocess
import sys
import tempfile
import threading
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("linux_role", Path(__file__).with_name("linux_role.py"))
launcher = importlib.util.module_from_spec(spec)
spec.loader.exec_module(launcher)


class ConfigurationTests(unittest.TestCase):
    def test_absolute_scope(self):
        for path in ("/", "relative", "/work/../controller", "//work/role"):
            with self.assertRaises(ValueError):
                launcher.absolute(path)
        self.assertEqual(launcher.absolute("/work space/project"), Path("/work space/project"))

    def test_unavailable_platform_has_no_fallback(self):
        with patch.object(sys, "platform", "not-linux"):
            with self.assertRaisesRegex(RuntimeError, "Linux namespaces"):
                launcher.command(None, {})

    def test_unavailable_runtime_has_no_fallback(self):
        with patch.object(sys, "platform", "linux"), patch.object(launcher.shutil, "which", return_value=None):
            with self.assertRaisesRegex(RuntimeError, "no unconfined"):
                launcher.command(None, {})

    def test_rejects_overlapping_home_and_escaping_write(self):
        args = argparse.Namespace(program=["--", "/bin/true"], write=[], create=[], runtime=[], network="none")
        with patch.object(sys, "platform", "linux"), patch.object(launcher.shutil, "which", return_value="/usr/bin/bwrap"):
            for home in ("/work", "/work/role", "/"):
                with self.assertRaises(ValueError):
                    launcher.command(args, {"TASK_WORKSPACE":"/work", "TASK_HOME":home})
            for write in ("", "../secret", "/secret", "src/../../secret"):
                args.write = [write]
                with self.assertRaises(ValueError):
                    launcher.command(args, {"TASK_WORKSPACE":"/work", "TASK_HOME":"/home/role"})

    def test_history_cannot_be_broadened_by_another_mount(self):
        args = argparse.Namespace(program=["--", "/bin/true"], write=["."], create=[], runtime=[], network="none")
        with patch.object(sys, "platform", "linux"), patch.object(launcher.shutil, "which", return_value="/usr/bin/bwrap"), \
                patch.object(launcher, "open_path", side_effect=AssertionError("overlap must be refused before opening any mount")):
            for history in ("/work/history.json", "/home/role/history.json", "/", "relative", "/work/../history"):
                with self.subTest(history=history), self.assertRaises(ValueError):
                    launcher.command(args, {"TASK_WORKSPACE": "/work", "TASK_HOME": "/home/role", "TASK_HISTORY": history})
            for runtime in ("/request", "/request/history.json", "/request/history.json/child"):
                args.runtime = [runtime]
                with self.subTest(runtime=runtime), self.assertRaisesRegex(ValueError, "separate read-only file"):
                    launcher.command(args, {"TASK_WORKSPACE": "/work", "TASK_HOME": "/home/role", "TASK_HISTORY": "/request/history.json"})

    def test_history_mount_uses_only_a_regular_file_descriptor_and_closes_on_refusal(self):
        # Portable command-construction check, not a claim about Linux mounts.
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            work, home, history = root / "work", root / "home", root / "run" / "history.json"
            for directory in (work, home, history.parent):
                directory.mkdir()
            history.write_text("saved request", encoding="utf-8")
            args = argparse.Namespace(program=["--", "/bin/true"], write=[], create=[], runtime=[], network="none")
            opened = []

            def portable_open(path, **_):
                descriptor = os.open(path, os.O_RDONLY)
                opened.append(descriptor)
                return descriptor

            with patch.object(sys, "platform", "linux"), patch.object(launcher.shutil, "which", return_value="/usr/bin/bwrap"), \
                    patch.object(launcher, "open_path", side_effect=portable_open), patch.object(os, "O_PATH", 0, create=True):
                argv, env, descriptors = launcher.command(args, {"TASK_WORKSPACE": str(work), "TASK_HOME": str(home), "TASK_HISTORY": str(history)})
                try:
                    mounts = [(argv[i], int(argv[i + 1]), argv[i + 2]) for i, item in enumerate(argv) if item in ("--bind-fd", "--ro-bind-fd")]
                    matching = [entry for entry in mounts if entry[2] == str(history)]
                    self.assertEqual(len(matching), 1)
                    self.assertEqual(matching[0][0], "--ro-bind-fd")
                    self.assertEqual(os.read(matching[0][1], 100), b"saved request")
                    self.assertEqual(env["TASK_HISTORY"], str(history))
                    self.assertNotIn(str(history.parent), [entry[2] for entry in mounts])
                finally:
                    for descriptor in descriptors:
                        os.close(descriptor)
                opened.clear()
                with self.assertRaisesRegex(ValueError, "regular file"):
                    launcher.command(args, {"TASK_WORKSPACE": str(work), "TASK_HOME": str(home), "TASK_HISTORY": str(history.parent)})
                for descriptor in opened:
                    with self.assertRaises(OSError):
                        os.fstat(descriptor)


class CreatePathTests(unittest.TestCase):
    def test_a_created_directory_never_follows_a_link_in_any_component(self):
        with tempfile.TemporaryDirectory() as root:
            base = Path(root)
            work, outside = base / "work", base / "outside"
            work.mkdir()
            outside.mkdir()
            launcher.create_path(work, PurePosixPath("report/out"))
            self.assertTrue((work / "report" / "out").is_dir())
            os.symlink(outside, work / "linked")
            with self.assertRaises(OSError):
                launcher.create_path(work, PurePosixPath("linked/out"))
            self.assertEqual(os.listdir(outside), [])
            with self.assertRaises(OSError):
                launcher.create_path(work, PurePosixPath("linked"))
            launcher.create_path(work, PurePosixPath("report/out"))  # existing directories are used as they are


@unittest.skipUnless(sys.platform == "linux", "requires Linux O_PATH directory handles")
class DescriptorTests(unittest.TestCase):
    def test_history_file_is_pinned_across_checkpoint_replacement(self):
        with tempfile.TemporaryDirectory(dir="/tmp") as temporary:
            root = Path(temporary)
            path = root / "history.json"
            path.write_text("older accepted answer", encoding="utf-8")
            descriptor = launcher.open_path(path)
            try:
                replacement = root / "replacement.json"
                replacement.write_text("next saved state", encoding="utf-8")
                replacement.replace(path)
                self.assertEqual(Path("/proc/self/fd/%d" % descriptor).read_text(), "older accepted answer")
                self.assertEqual(path.read_text(), "next saved state")
            finally:
                os.close(descriptor)

    def test_private_mounts_do_not_hide_explicit_temporary_paths(self):
        with tempfile.TemporaryDirectory(dir="/tmp") as root:
            base = Path(root)
            work, home, runtime = base / "work", base / "home", base / "sdk"
            for directory in (work, home, runtime):
                directory.mkdir()
            args = argparse.Namespace(program=["--", "/bin/true"], write=[], create=[],
                                      runtime=[str(runtime)], network="none")
            with patch.object(launcher.shutil, "which", return_value="/usr/bin/bwrap"):
                argv, _, descriptors = launcher.command(args, {
                    "TASK_WORKSPACE": str(work), "TASK_HOME": str(home),
                })
            try:
                for private_mount in ("--proc", "--dev", "--tmpfs"):
                    for explicit_path in (work, home, runtime):
                        self.assertLess(argv.index(private_mount), argv.index(str(explicit_path)),
                                        "private mounts must precede explicit task/runtime mounts")
            finally:
                for descriptor in descriptors:
                    os.close(descriptor)

    def test_the_alternatives_directory_is_shown_read_only_when_the_system_has_one(self):
        with tempfile.TemporaryDirectory(dir="/tmp") as root:
            base = Path(root)
            work, home, alternatives = base / "work", base / "home", base / "alternatives"
            for directory in (work, home, alternatives):
                directory.mkdir()
            args = argparse.Namespace(program=["--", "/bin/true"], write=[], create=[], runtime=[], network="none")
            environment = {"TASK_WORKSPACE": str(work), "TASK_HOME": str(home)}
            with patch.object(launcher.shutil, "which", return_value="/usr/bin/bwrap"), \
                    patch.object(launcher, "ALTERNATIVES", alternatives):
                argv, _, descriptors = launcher.command(args, environment)
            try:
                position = argv.index(str(alternatives))
                self.assertEqual(argv[position - 2], "--ro-bind-fd")
                self.assertLess(position, argv.index("--proc"), "system paths are mounted before the private base")
            finally:
                for descriptor in descriptors:
                    os.close(descriptor)
            with patch.object(launcher.shutil, "which", return_value="/usr/bin/bwrap"), \
                    patch.object(launcher, "ALTERNATIVES", base / "absent"):
                argv, _, descriptors = launcher.command(args, environment)
            try:
                self.assertNotIn(str(base / "absent"), argv)
            finally:
                for descriptor in descriptors:
                    os.close(descriptor)

    def test_a_whole_workspace_grant_keeps_the_checkout_metadata_read_only(self):
        with tempfile.TemporaryDirectory(dir="/tmp") as root:
            base = Path(root)
            work, home = base / "work", base / "home"
            for directory in (work, home, work / ".git", work / "src"):
                directory.mkdir()

            def mounts(write):
                args = argparse.Namespace(program=["--", "/bin/true"], write=write, create=[], runtime=[], network="none")
                with patch.object(launcher.shutil, "which", return_value="/usr/bin/bwrap"):
                    argv, _, descriptors = launcher.command(args, {"TASK_WORKSPACE": str(work), "TASK_HOME": str(home)})
                for descriptor in descriptors:
                    os.close(descriptor)
                return [(argv[i], argv[i + 2]) for i, item in enumerate(argv) if item in ("--bind-fd", "--ro-bind-fd")]

            whole = mounts(["."])
            self.assertIn(("--bind-fd", str(work)), whole)
            self.assertIn(("--ro-bind-fd", str(work / ".git")), whole)
            self.assertGreater(whole.index(("--ro-bind-fd", str(work / ".git"))), whole.index(("--bind-fd", str(work))),
                               "the read-only metadata mount must follow the writable workspace mount to take effect")
            named = mounts([".git"])
            self.assertIn(("--bind-fd", str(work / ".git")), named)
            self.assertNotIn(("--ro-bind-fd", str(work / ".git")), named)
            self.assertNotIn(("--ro-bind-fd", str(work / ".git")), mounts(["src"]))

    def test_a_created_output_directory_is_made_inside_the_workspace_and_granted(self):
        with tempfile.TemporaryDirectory(dir="/tmp") as root:
            base = Path(root)
            work, home = base / "work", base / "home"
            for directory in (work, home):
                directory.mkdir()
            args = argparse.Namespace(program=["--", "/bin/true"], write=[], create=["report"], runtime=[], network="none")
            with patch.object(launcher.shutil, "which", return_value="/usr/bin/bwrap"):
                argv, _, descriptors = launcher.command(args, {"TASK_WORKSPACE": str(work), "TASK_HOME": str(home)})
            for descriptor in descriptors:
                os.close(descriptor)
            self.assertTrue((work / "report").is_dir())
            self.assertIn(("--bind-fd", str(work / "report")), [(argv[i], argv[i + 2]) for i, item in enumerate(argv) if item == "--bind-fd"])
            with patch.object(launcher.shutil, "which", return_value="/usr/bin/bwrap"), self.assertRaises(ValueError):
                launcher.command(argparse.Namespace(program=["--", "/bin/true"], write=[], create=["../out"], runtime=[], network="none"),
                                 {"TASK_WORKSPACE": str(work), "TASK_HOME": str(home)})

    def test_intermediate_and_final_symlinks_are_not_followed(self):
        with tempfile.TemporaryDirectory() as root:
            base = Path(root)
            target = base / "target"
            target.mkdir()
            (target / "file").write_text("unchanged")
            (base / "link").symlink_to(target, target_is_directory=True)
            (base / "file-link").symlink_to(target / "file")
            for path in (base / "link/file", base / "file-link"):
                with self.assertRaises((OSError, ValueError)):
                    launcher.open_path(path)
            self.assertEqual((target / "file").read_text(), "unchanged")

    def test_descriptor_keeps_original_inode_after_path_is_replaced(self):
        with tempfile.TemporaryDirectory() as root:
            path = Path(root) / "work"
            path.mkdir()
            descriptor = launcher.open_path(path, directory=True)
            try:
                identity = os.fstat(descriptor).st_ino
                path.rename(Path(root) / "previous")
                path.symlink_to("/etc", target_is_directory=True)
                self.assertEqual(os.fstat(descriptor).st_ino, identity)
                self.assertNotEqual(os.stat(path).st_ino, identity)
            finally:
                os.close(descriptor)


@unittest.skipUnless(sys.platform == "linux" and shutil.which("bwrap"), "requires real Linux bubblewrap isolation")
class HistoryIsolationTests(unittest.TestCase):
    def test_history_is_readable_but_its_neighbors_and_writes_are_not_granted(self):
        with tempfile.TemporaryDirectory(dir="/tmp") as temporary:
            root = Path(temporary)
            work, home, history = root / "workspace", root / "home", root / "run" / "history.json"
            for directory in (work, home, history.parent, root / "other-request"):
                directory.mkdir()
            history.write_text("canonical accepted answer", encoding="utf-8")
            neighbor = root / "other-request" / "history.json"
            neighbor.write_text("not granted", encoding="utf-8")
            program = ['--', '/bin/sh', '-c',
                       'test "$(cat "$1")" = "canonical accepted answer" && '
                       'test ! -e "$2" && ! (printf changed > "$1") && ! rm "$1" && '
                       '! mv "$1" "$1.moved"', 'reader', str(history), str(neighbor)]
            args = argparse.Namespace(program=program, write=["."], create=[], runtime=[], network="none")
            argv, environment, descriptors = launcher.command(args, {
                "TASK_WORKSPACE": str(work), "TASK_HOME": str(home), "TASK_HISTORY": str(history), "PATH": os.environ["PATH"]})
            try:
                result = subprocess.run(argv, env=environment, pass_fds=descriptors, capture_output=True, text=True, timeout=10)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            finally:
                for descriptor in descriptors:
                    os.close(descriptor)
            self.assertEqual(history.read_text(), "canonical accepted answer")
            self.assertEqual(neighbor.read_text(), "not granted")

    def test_a_program_behind_the_alternatives_directory_resolves_inside_the_sandbox(self):
        # On a system with Debian's alternatives, /usr/bin/awk (or cc) is a
        # symlink into /etc/alternatives; the sandbox shows /usr, and without
        # /etc/alternatives the link is dangling inside it.
        behind = [path for path in (Path("/usr/bin/awk"), Path("/usr/bin/cc"), Path("/usr/bin/pager"))
                  if path.is_symlink() and "/etc/alternatives/" in os.path.realpath(path) + "/"
                  or path.is_symlink() and str(os.readlink(path)).startswith("/etc/alternatives/")]
        if not behind:
            self.skipTest("no program on this system is linked through /etc/alternatives")
        with tempfile.TemporaryDirectory(dir="/tmp") as temporary:
            root = Path(temporary)
            work, home = root / "workspace", root / "home"
            work.mkdir()
            home.mkdir()
            program = ["--", "/bin/sh", "-c", 'target=$(readlink -f "$1") && test -x "$target"', "resolver", str(behind[0])]
            args = argparse.Namespace(program=program, write=[], create=[], runtime=[], network="none")
            argv, environment, descriptors = launcher.command(args, {
                "TASK_WORKSPACE": str(work), "TASK_HOME": str(home), "PATH": os.environ["PATH"]})
            try:
                result = subprocess.run(argv, env=environment, pass_fds=descriptors, capture_output=True, text=True, timeout=10)
                self.assertEqual(result.returncode, 0, "%s did not resolve to an executable inside the sandbox: %s"
                                 % (behind[0], result.stdout + result.stderr))
            finally:
                for descriptor in descriptors:
                    os.close(descriptor)


def write_cgroup(directory, limit, current, active_file=0, inactive_file=0):
    directory.mkdir(exist_ok=True)
    (directory / "memory.max").write_text("%s\n" % limit)
    (directory / "memory.current").write_text("%d\n" % current)
    (directory / "memory.stat").write_text("anon %d\nfile %d\nactive_file %d\ninactive_file %d\nshmem 0\n"
                                          % (current - active_file - inactive_file, active_file + inactive_file,
                                             active_file, inactive_file))


class MemoryAccountingTests(unittest.TestCase):
    """Portable checks of what the memory guard reads and decides."""

    def test_use_leaves_out_file_cache_and_no_limit_means_no_guard(self):
        with tempfile.TemporaryDirectory() as temporary:
            cgroup = Path(temporary) / "cgroup"
            write_cgroup(cgroup, 6 << 30, 5 << 30, active_file=300 << 20, inactive_file=500 << 20)
            self.assertEqual(launcher.container_memory(cgroup), (6 << 30, (5 << 30) - (800 << 20)))
            write_cgroup(cgroup, "max", 5 << 30)
            self.assertIsNone(launcher.container_memory(cgroup))
            self.assertIsNone(launcher.memory_threshold(None, cgroup))
            self.assertIsNone(launcher.container_memory(Path(temporary) / "absent"))

    def test_the_headroom_is_an_eighth_at_least_512_mib_or_the_operators(self):
        with tempfile.TemporaryDirectory() as temporary:
            cgroup = Path(temporary) / "cgroup"
            write_cgroup(cgroup, 6 << 30, 1 << 30)
            self.assertEqual(launcher.memory_threshold(None, cgroup), (6 << 30) - (768 << 20))
            self.assertEqual(launcher.memory_threshold(1024, cgroup), 5 << 30)
            for headroom in (0, -1, 6 << 10, 7 << 10):
                with self.subTest(headroom=headroom), self.assertRaisesRegex(ValueError, "headroom"):
                    launcher.memory_threshold(headroom, cgroup)
            write_cgroup(cgroup, 2 << 30, 1 << 30)
            self.assertEqual(launcher.memory_threshold(None, cgroup), (2 << 30) - (512 << 20))

    def test_only_the_largest_role_process_in_the_container_is_chosen_and_only_by_its_own_launcher(self):
        P = launcher.Process
        table = {
            1: P(0, "ticket-engine", 5 << 30, 1000, False),     # the controller is never chosen
            10: P(1, "bwrap", 1 << 20, 1000, False),
            11: P(10, "python", 300 << 20, 1000, True),         # the role's agent
            12: P(11, "cargo", 80 << 20, 1000, True),
            13: P(12, "rustc", 3 << 30, 1000, True),
            20: P(1, "bwrap", 1 << 20, 1000, False),
            21: P(20, "python", 200 << 20, 1000, True),
            22: P(21, "rustc", 1 << 30, 1000, True),
            30: P(0, "other-user", 8 << 30, 0, True),           # not this controller's user
        }
        self.assertEqual(launcher.victim(table, 10, 1000), 13)
        self.assertIsNone(launcher.victim(table, 20, 1000), "the other role's launcher leaves the choice to the owner")
        del table[13]
        self.assertEqual(launcher.victim(table, 20, 1000), 22)
        self.assertIsNone(launcher.victim(table, 10, 1000))
        self.assertIsNone(launcher.victim({1: P(0, "ticket-engine", 5 << 30, 1000, False)}, 10, 1000))


@unittest.skipUnless(sys.platform == "linux", "requires Linux /proc")
class ProcessTableTests(unittest.TestCase):
    def test_this_process_is_read_as_a_process_of_this_namespace(self):
        table = launcher.processes()
        own = table[os.getpid()]
        self.assertEqual((own.parent, own.uid, own.nested), (os.getppid(), os.getuid(), False))
        self.assertGreater(own.anon, 0)


@unittest.skipUnless(sys.platform == "linux" and shutil.which("bwrap"), "requires real Linux bubblewrap isolation")
class MemoryGuardTests(unittest.TestCase):
    def test_the_largest_role_process_is_stopped_the_role_continues_and_the_reason_is_reported(self):
        with tempfile.TemporaryDirectory(dir="/tmp") as temporary:
            root = Path(temporary)
            work, home, cgroup = root / "workspace", root / "home", root / "cgroup"
            work.mkdir()
            home.mkdir()
            (work / "hold.py").write_text(
                "import sys, time\n"
                "held = bytearray(64 << 20)\n"
                "held[::4096] = b'\\x01' * (len(held) // 4096)\n"
                "print('holding', flush=True)\n"
                "time.sleep(60)\n")
            program = ["--", "/bin/sh", "-c", 'python3 -B hold.py; echo "the holder ended with $?"']
            args = argparse.Namespace(program=program, write=[], create=[], runtime=[], network="none")
            argv, environment, descriptors = launcher.command(args, {
                "TASK_WORKSPACE": str(work), "TASK_HOME": str(home), "PATH": os.environ["PATH"]})
            try:
                role = subprocess.Popen(argv, env=environment, pass_fds=descriptors, stdout=subprocess.PIPE,
                                        stderr=subprocess.PIPE, text=True)
            finally:
                for descriptor in descriptors:
                    os.close(descriptor)
            report = []

            class Report:
                def write(self, text):
                    report.append(text)

                def flush(self):
                    pass

            try:
                self.assertEqual(role.stdout.readline(), "holding\n", "the role did not start")
                # The container's use as the guard reads it: over the threshold.
                write_cgroup(cgroup, 2 << 30, (2 << 30) - (100 << 20))
                watcher = threading.Thread(target=launcher.guard,
                                           args=(role.pid, (2 << 30) - (512 << 20), Report(), lambda: role.poll() is None),
                                           kwargs={"cgroup": cgroup, "interval": 0.05, "settle": 0.2})
                watcher.start()
                try:
                    output, errors = role.communicate(timeout=20)
                except subprocess.TimeoutExpired:
                    output, errors = None, None
                watcher.join(timeout=10)
            finally:
                if role.poll() is None:
                    role.kill()
                    role.wait()
            self.assertIsNotNone(output, "the guard did not stop the holder within 20 s")
            self.assertEqual(role.returncode, 0, output + errors)
            self.assertIn("the holder ended with 137", output, "the holder was not the process stopped")
            self.assertEqual(len(report), 1, report)
            self.assertRegex(report[0], r"^\(launcher\) Stopped python3 \(pid \d+, \d+ MiB resident\) because this "
                                        r"container's memory in use reached 1948 of 2048 MiB\.")


# Runs the launcher's main() with the container's cgroup read from a given
# directory, under a parent that collects every process left behind (it is a
# child subreaper, as a container's main process is the parent of orphans),
# and prints how many it collected after the launcher ended.
SUPERVISED = """
import ctypes, importlib.util, os, subprocess, sys, time
from pathlib import Path
if sys.argv[1] == "launch":
    spec = importlib.util.spec_from_file_location("linux_role", sys.argv[2])
    launcher = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(launcher)
    launcher.CGROUP = Path(sys.argv[3])
    sys.argv = [sys.argv[2], *sys.argv[4:]]
    launcher.main()
ctypes.CDLL(None, use_errno=True).prctl(36, 1)  # PR_SET_CHILD_SUBREAPER
role = subprocess.Popen([sys.executable, "-B", "-c", sys.argv[2], "launch", *sys.argv[3:]], stdin=subprocess.DEVNULL)
status = role.wait()
time.sleep(0.5)
left = 0
while True:
    try:
        pid, _ = os.waitpid(-1, os.WNOHANG)
    except ChildProcessError:
        break
    if not pid:
        left += 1
        time.sleep(0.2)
        if left > 10:
            break
        continue
    left += 1
print("launcher status %d, processes left behind %d" % (status, left), file=sys.stderr, flush=True)
"""


@unittest.skipUnless(sys.platform == "linux" and shutil.which("bwrap"), "requires real Linux bubblewrap isolation")
class SupervisedLaunchTests(unittest.TestCase):
    """main() with a container memory limit: bubblewrap runs under the guard."""

    def launch(self, root, program, usage=1 << 30, over_after=None):
        work, home, cgroup = root / "workspace", root / "home", root / "cgroup"
        work.mkdir(exist_ok=True)
        home.mkdir(exist_ok=True)
        write_cgroup(cgroup, 2 << 30, usage)
        environment = {"TASK_WORKSPACE": str(work), "TASK_HOME": str(home), "PATH": os.environ["PATH"]}
        role = subprocess.Popen([sys.executable, "-B", "-c", SUPERVISED, "check", SUPERVISED,
                                 str(Path(__file__).with_name("linux_role.py")), str(cgroup), "--network", "none",
                                 "--", *program], env=environment, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            if over_after is not None:
                self.assertEqual(role.stdout.readline(), over_after, "the role did not start")
                write_cgroup(cgroup, 2 << 30, (2 << 30) - (100 << 20))
            try:
                output, errors = role.communicate(timeout=30)
            except subprocess.TimeoutExpired:
                output, errors = None, "the launcher did not end within 30 s"
        finally:
            if role.poll() is None:
                role.kill()
                role.wait()
        return output, errors

    def test_a_fault_of_the_guard_leaves_the_role_running_unguarded_to_its_own_end(self):
        with tempfile.TemporaryDirectory(dir="/tmp") as temporary:
            root = Path(temporary)
            work, home = root / "workspace", root / "home"
            work.mkdir()
            home.mkdir()
            args = argparse.Namespace(program=["--", "/bin/sh", "-c", "sleep 0.3; exit 7"], write=[], create=[],
                                      runtime=[], network="none")
            argv, environment, descriptors = launcher.command(args, {
                "TASK_WORKSPACE": str(work), "TASK_HOME": str(home), "PATH": os.environ["PATH"]})
            report = io.StringIO()
            try:
                with patch.object(launcher, "guard", side_effect=RuntimeError("a fault")), patch.object(sys, "stderr", report):
                    status = launcher.supervise(argv, environment, descriptors, 1 << 30)
            finally:
                signal.pthread_sigmask(signal.SIG_UNBLOCK, {signal.SIGCHLD})
                for descriptor in descriptors:
                    os.close(descriptor)
            self.assertEqual(status, 7)
            self.assertEqual(report.getvalue(), "(launcher) The memory guard stopped: a fault\n")

    def test_a_signal_ends_the_launcher_as_it_ended_bubblewrap_or_the_controllers_process_group(self):
        def children(parent):
            found = []
            for entry in os.listdir("/proc"):
                try:
                    with open("/proc/%s/stat" % entry) as handle:
                        line = handle.read()
                except OSError:
                    continue
                if entry.isdigit() and int(line[line.rindex(")") + 1:].split()[1]) == parent:
                    found.append((int(entry), line[line.index("(") + 1:line.rindex(")")]))
            return found

        for name, stop, expected in (("bubblewrap alone", "bwrap", -signal.SIGKILL),
                                     ("the process group", "group", -signal.SIGTERM)):
            with self.subTest(name), tempfile.TemporaryDirectory(dir="/tmp") as temporary:
                root = Path(temporary)
                work, home, cgroup = root / "workspace", root / "home", root / "cgroup"
                work.mkdir()
                home.mkdir()
                write_cgroup(cgroup, 2 << 30, 1 << 30)
                role = subprocess.Popen([sys.executable, "-B", "-c", SUPERVISED, "launch",
                                         str(Path(__file__).with_name("linux_role.py")), str(cgroup), "--network", "none",
                                         "--", "/bin/sh", "-c", "echo started; sleep 30"],
                                        env={"TASK_WORKSPACE": str(work), "TASK_HOME": str(home), "PATH": os.environ["PATH"]},
                                        stdout=subprocess.PIPE, text=True, start_new_session=True)
                try:
                    self.assertEqual(role.stdout.readline(), "started\n")
                    bubblewrap = [pid for pid, command in children(role.pid) if command == "bwrap"]
                    self.assertEqual(len(bubblewrap), 1, children(role.pid))
                    if stop == "bwrap":
                        os.kill(bubblewrap[0], signal.SIGKILL)
                    else:
                        os.killpg(role.pid, signal.SIGTERM)
                    self.assertEqual(role.wait(timeout=10), expected)
                finally:
                    if role.poll() is None:
                        os.killpg(role.pid, signal.SIGKILL)
                        role.wait()
                    role.stdout.close()

    def test_the_exit_status_passes_through_and_nothing_is_left_behind(self):
        with tempfile.TemporaryDirectory(dir="/tmp") as temporary:
            output, errors = self.launch(Path(temporary), ["/bin/sh", "-c", "sleep 0.3; exit 7"])
        self.assertEqual(errors, "launcher status 7, processes left behind 0\n", output)

    def test_role_processes_are_the_kernels_first_choice_and_none_may_pass_the_threshold_alone(self):
        with tempfile.TemporaryDirectory(dir="/tmp") as temporary:
            output, errors = self.launch(Path(temporary), ["/bin/sh", "-c",
                                                           "cat /proc/self/oom_score_adj; ulimit -d; grep SigBlk /proc/self/status"])
        # The guard waits for bubblewrap with SIGCHLD blocked; the role must not inherit that.
        self.assertEqual(output, "1000\n%d\nSigBlk:\t0000000000000000\n" % (((2 << 30) - (512 << 20)) // 1024), errors)

    def test_the_largest_role_process_is_stopped_and_the_launcher_ends_as_the_role_did(self):
        with tempfile.TemporaryDirectory(dir="/tmp") as temporary:
            root = Path(temporary)
            (root / "workspace").mkdir()
            (root / "workspace" / "hold.py").write_text(
                "import time\n"
                "held = bytearray(64 << 20)\n"
                "held[::4096] = b'\\x01' * (len(held) // 4096)\n"
                "print('holding', flush=True)\n"
                "time.sleep(60)\n")
            output, errors = self.launch(root, ["/bin/sh", "-c", 'python3 -B hold.py; echo "the holder ended with $?"'],
                                         over_after="holding\n")
        self.assertEqual(output, "the holder ended with 137\n", errors)
        lines = errors.splitlines()
        # The shell's own "Killed" may stand between the two; only one process was stopped.
        self.assertEqual(len([line for line in lines if line.startswith("(launcher)")]), 1, errors)
        self.assertRegex(lines[0], r"^\(launcher\) Stopped python3 \(pid \d+, \d+ MiB resident\) because this "
                                   r"container's memory in use reached 1948 of 2048 MiB\.")
        self.assertEqual(lines[-1], "launcher status 0, processes left behind 0")


if __name__ == "__main__":
    unittest.main()
