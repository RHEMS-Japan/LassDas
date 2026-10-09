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


def write_cgroup(directory, limit, current, active_file=0, inactive_file=0, slab_reclaimable=0, oom_group=0, anon=None):
    directory.mkdir(exist_ok=True)
    (directory / "memory.max").write_text("%s\n" % limit)
    (directory / "memory.current").write_text("%d\n" % current)
    (directory / "memory.oom.group").write_text("%d\n" % oom_group)
    reclaimable = active_file + inactive_file + slab_reclaimable
    anon = 0 if anon is None else anon  # unless a test says otherwise, no memory is anonymous outside the roles
    (directory / "memory.stat").write_text(
        "anon %d\nfile %d\nshmem %d\nactive_file %d\ninactive_file %d\nslab_reclaimable %d\nslab_unreclaimable 0\n"
        % (anon, active_file + inactive_file, current - reclaimable - anon, active_file, inactive_file, slab_reclaimable))


class MemoryAccountingTests(unittest.TestCase):
    """Portable checks of what the memory guard reads and decides."""

    def test_the_own_cgroup_is_found_from_the_process_cgroup_line_or_its_absence_is_said(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            cgroup, proc = root / "cgroup", root / "proc"
            (proc / "self").mkdir(parents=True)
            for line, expected in (("0::/\n", cgroup), ("0::/system.slice/engine.scope\n", cgroup / "system.slice/engine.scope")):
                (proc / "self" / "cgroup").write_text(line)
                self.assertEqual(launcher.own_cgroup(cgroup, proc), (expected, None))
            (proc / "self" / "cgroup").write_text("12:memory:/docker/engine\n1:name=systemd:/docker/engine\n")
            directory, reason = launcher.own_cgroup(cgroup, proc)
            self.assertIsNone(directory)
            self.assertIn("no cgroup v2 hierarchy", reason)
            (proc / "self" / "cgroup").unlink()
            directory, reason = launcher.own_cgroup(cgroup, proc)
            self.assertIsNone(directory)
            self.assertIn("could not be read", reason)

    def test_use_leaves_out_file_cache_and_reclaimable_kernel_caches(self):
        with tempfile.TemporaryDirectory() as temporary:
            cgroup = Path(temporary) / "cgroup"
            write_cgroup(cgroup, 6 << 30, 5 << 30, active_file=300 << 20, inactive_file=500 << 20, slab_reclaimable=653 << 20)
            self.assertEqual(launcher.memory_in_use(cgroup), (5 << 30) - (1453 << 20))

    def test_the_guard_is_on_with_a_limit_and_off_with_a_reason_otherwise(self):
        with tempfile.TemporaryDirectory() as temporary:
            cgroup = Path(temporary) / "cgroup"
            write_cgroup(cgroup, 6 << 30, 1 << 30)
            self.assertEqual(launcher.memory_guard(cgroup, None), (launcher.Limits(cgroup, 6 << 30, (6 << 30) - (768 << 20)), None))
            self.assertEqual(launcher.memory_guard(cgroup, 1024), (launcher.Limits(cgroup, 6 << 30, 5 << 30), None))
            write_cgroup(cgroup, 2 << 30, 1 << 30)
            self.assertEqual(launcher.memory_guard(cgroup, None)[0].threshold, (2 << 30) - (512 << 20))
            # No limit: nothing to keep under, and nothing to say.
            write_cgroup(cgroup, "max", 1 << 30)
            self.assertEqual(launcher.memory_guard(cgroup, None), (None, None))
            # A limit too small for the headroom, or a headroom that does not fit: off, with the reason.
            for limit, headroom in ((512 << 20, None), (256 << 20, None), (6 << 30, 0), (6 << 30, -1), (6 << 30, 6 << 10)):
                write_cgroup(cgroup, limit, 1 << 20)
                with self.subTest(limit=limit, headroom=headroom):
                    limits, reason = launcher.memory_guard(cgroup, headroom)
                    self.assertIsNone(limits)
                    self.assertIn("does not fit under the container's memory limit", reason)
            # Nothing readable there.
            for name in ("memory.max", "memory.stat"):
                write_cgroup(cgroup, 6 << 30, 1 << 30)
                (cgroup / name).unlink()
                with self.subTest(missing=name):
                    limits, reason = launcher.memory_guard(cgroup, None)
                    self.assertIsNone(limits)
                    self.assertIn("could not be read", reason)
            limits, reason = launcher.memory_guard(Path(temporary) / "absent", None)
            self.assertIsNone(limits)
            self.assertIn("could not be read", reason)

    def test_the_whole_container_is_stopped_together_only_where_its_cgroup_says_so(self):
        with tempfile.TemporaryDirectory() as temporary:
            cgroup = Path(temporary) / "cgroup"
            write_cgroup(cgroup, 6 << 30, 1 << 30, oom_group=1)
            self.assertTrue(launcher.kills_the_whole_container(cgroup))
            write_cgroup(cgroup, 6 << 30, 1 << 30, oom_group=0)
            self.assertFalse(launcher.kills_the_whole_container(cgroup))
            self.assertFalse(launcher.kills_the_whole_container(Path(temporary) / "absent"))
            self.assertFalse(launcher.kills_the_whole_container(None))

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

    def test_reclaimable_memory_over_the_threshold_stops_nothing(self):
        # A container that has read many files: in use by memory.current, but
        # the kernel would take it back before stopping anything.
        with tempfile.TemporaryDirectory() as temporary:
            cgroup = Path(temporary) / "cgroup"
            write_cgroup(cgroup, 2 << 30, (2 << 30) - (100 << 20), slab_reclaimable=653 << 20)
            looks = iter(range(5))
            report = io.StringIO()
            with patch.object(launcher, "processes", side_effect=AssertionError("no process should be looked at")), \
                    patch.object(os, "kill", side_effect=AssertionError("nothing should be stopped")):
                launcher.guard(10, launcher.Limits(cgroup, 2 << 30, (2 << 30) - (512 << 20)), report,
                               lambda: next(looks, None) is not None, pause=lambda _: None)
            self.assertEqual(report.getvalue(), "")

    def run_guard(self, cgroup, table, looks=3, limit=2 << 30, headroom=512 << 20):
        """guard() over a fixed process table: what it stopped, what it wrote, and how long it paused each time.
        The stand-ins are never really stopped, so a stopped one stays in the table as it is."""
        remaining = iter(range(looks))
        report, killed, pauses = io.StringIO(), [], []
        with patch.object(launcher, "processes", return_value=table) as reads, patch.object(launcher, "tell"), \
                patch.object(os, "getuid", return_value=1000), \
                patch.object(os, "kill", side_effect=lambda pid, number: killed.append(pid)):
            launcher.guard(10, launcher.Limits(cgroup, limit, limit - headroom), report,
                           lambda: next(remaining, None) is not None, pause=pauses.append)
        self.table_reads = reads.call_count
        return killed, report.getvalue().splitlines(), pauses

    def test_memory_held_outside_every_role_stops_a_role_process_only_near_the_limit(self):
        P = launcher.Process
        with tempfile.TemporaryDirectory() as temporary:
            cgroup = Path(temporary) / "cgroup"
            # 2 GiB, 512 MiB headroom: the threshold is 1536 MiB, the last resort 1792 MiB.
            for role, stopped in ((18 << 20, False), (160 << 20, True)):
                controller = 1650 << 20
                write_cgroup(cgroup, 2 << 30, controller + role + (10 << 20), anon=controller + role)
                table = {1: P(0, "ticket-engine", controller, 1000, False), 10: P(1, "bwrap", 1 << 20, 1000, False),
                         11: P(10, "python3", role, 1000, True)}
                with self.subTest(role=role >> 20):
                    killed, lines, _ = self.run_guard(cgroup, table)
                    if stopped:
                        self.assertEqual(killed, [11])
                        self.assertEqual(len(lines), 2, lines)
                        self.assertTrue(lines[0].startswith("(launcher) Stopped python3 (pid 11, 160 MiB resident"), lines[0])
                        # Once the role's process counts as gone, what is left is the controller's: said once.
                        self.assertTrue(lines[1].startswith("(launcher) This container's memory in use reached 1660 of"), lines[1])
                    else:
                        self.assertEqual(killed, [])
                        self.assertEqual(len(lines), 1, lines)
                        self.assertEqual(lines[0], "(launcher) This container's memory in use reached 1678 of 2048 MiB, "
                                                   "1650 MiB of it anonymous memory outside every role's processes; no "
                                                   "role process is stopped for it until the use reaches 1792 MiB.")

    def test_a_stopped_process_counts_as_gone_and_the_next_largest_is_stopped_at_the_next_look(self):
        P = launcher.Process
        with tempfile.TemporaryDirectory() as temporary:
            cgroup = Path(temporary) / "cgroup"
            # Two compilers of one role: stopping the larger leaves 300 + 900 MiB, under the threshold of 1536 MiB;
            # stopping it as well is needed only when the other has grown to 1300 MiB.
            for second, expected in ((900 << 20, [12]), (1300 << 20, [12, 13])):
                table = {1: P(0, "ticket-engine", 300 << 20, 1000, False), 10: P(1, "bwrap", 1 << 20, 1000, False),
                         11: P(10, "cargo", 0, 1000, True, 5), 12: P(11, "rustc", 1400 << 20, 1000, True, 6),
                         13: P(11, "rustc", second, 1000, True, 7)}
                used = (300 << 20) + (1400 << 20) + second
                write_cgroup(cgroup, 4 << 30, used, anon=300 << 20)
                with self.subTest(second=second >> 20):
                    killed, lines, pauses = self.run_guard(cgroup, table, limit=4 << 30, headroom=(4 << 30) - (1536 << 20))
                    self.assertEqual(killed, expected)
                    self.assertEqual(len(lines), len(expected), lines)
                    # No pause longer than the near look after a stop: a growing neighbour is not left alone for a second.
                    self.assertEqual(pauses, [0.01, 0.01, 0.01])
            # A process that is already ending (it exited, or another launcher stopped it) is not chosen,
            # and its memory counts as given back.
            table[12] = table[12]._replace(leaving=True)
            write_cgroup(cgroup, 4 << 30, (300 << 20) + (1400 << 20) + (900 << 20), anon=300 << 20)
            killed, lines, _ = self.run_guard(cgroup, table, limit=4 << 30, headroom=(4 << 30) - (1536 << 20))
            self.assertEqual((killed, lines), ([], []))

    def test_the_guard_looks_again_before_the_use_could_reach_the_threshold(self):
        # The next look comes before the use, growing at 32 GiB/s, could reach the threshold (5,376 MiB of
        # 6 GiB here), no later than a tenth and no sooner than a hundredth of a second; the use is the one
        # with caches left out, so a container full of file cache is looked at no more often.
        with tempfile.TemporaryDirectory() as temporary:
            cgroup = Path(temporary) / "cgroup"
            for limit, current, cache, pause in ((6 << 30, 100 << 20, 0, 0.1), (6 << 30, 4000 << 20, 0, 1376 / 32768),
                                                 (6 << 30, 5300 << 20, 0, 0.01), (6 << 30, 5600 << 20, 5500 << 20, 0.1),
                                                 (6 << 30, 5600 << 20, 1600 << 20, 1376 / 32768),
                                                 (2 << 30, 100 << 20, 0, 1180 / 32768)):
                write_cgroup(cgroup, limit, current, inactive_file=cache)
                with self.subTest(limit=limit >> 20, current=current >> 20, cache=cache >> 20):
                    _, _, pauses = self.run_guard(cgroup, {}, looks=2, limit=limit, headroom=768 << 20)
                    self.assertEqual(len(pauses), 2)
                    for seconds in pauses:
                        self.assertAlmostEqual(seconds, pause, places=6)
                    self.assertEqual(self.table_reads, 0)

    def test_memory_outside_every_role_over_the_threshold_is_looked_at_without_reading_the_table_each_time(self):
        P = launcher.Process
        with tempfile.TemporaryDirectory() as temporary:
            cgroup = Path(temporary) / "cgroup"
            controller, role = 1650 << 20, 18 << 20
            write_cgroup(cgroup, 2 << 30, controller + role + (10 << 20), anon=controller + role)
            table = {1: P(0, "ticket-engine", controller, 1000, False), 10: P(1, "bwrap", 1 << 20, 1000, False),
                     11: P(10, "python3", role, 1000, True)}
            # Ten looks at once (no time passes between them here): the table is read for the first only,
            # and the pace is set by the last resort, 1,792 MiB, a few milliseconds of growth away.
            killed, lines, pauses = self.run_guard(cgroup, table, looks=10)
            self.assertEqual((killed, len(lines), self.table_reads), ([], 1, 1))
            self.assertEqual(pauses, [0.01] * 10)
            # At the last resort it is read again at once, and the role's process is stopped.
            write_cgroup(cgroup, 2 << 30, controller + (160 << 20) + (10 << 20), anon=controller + (160 << 20))
            table[11] = table[11]._replace(anon=160 << 20)
            killed, _, _ = self.run_guard(cgroup, table, looks=10)
            self.assertEqual(killed, [11])

    def test_while_waiting_the_table_is_read_again_each_second_and_a_role_that_became_the_cause_is_stopped(self):
        P = launcher.Process
        with tempfile.TemporaryDirectory() as temporary:
            cgroup = Path(temporary) / "cgroup"
            before = {1: P(0, "ticket-engine", 1650 << 20, 1000, False), 10: P(1, "bwrap", 1 << 20, 1000, False),
                      11: P(10, "python3", 18 << 20, 1000, True)}
            # The controller then gives back most of its memory and the role grows: 1,610 MiB in use, over
            # the threshold (1,536 MiB) but under the last resort (1,792 MiB), and now the role's.
            after = {**before, 1: before[1]._replace(anon=300 << 20), 11: before[11]._replace(anon=1300 << 20)}
            clock, killed, report = [100.0], [], io.StringIO()
            looks = iter(range(400))

            def pause(seconds):
                clock[0] += seconds
                if clock[0] > 100.5:
                    write_cgroup(cgroup, 2 << 30, 1610 << 20, anon=1600 << 20)

            write_cgroup(cgroup, 2 << 30, 1678 << 20, anon=1668 << 20)
            with patch.object(launcher, "processes", side_effect=lambda proc=None: before if clock[0] <= 100.5 else after), \
                    patch.object(launcher.time, "monotonic", side_effect=lambda: clock[0]), \
                    patch.object(launcher, "tell"), patch.object(os, "getuid", return_value=1000), \
                    patch.object(os, "kill", side_effect=lambda pid, number: killed.append((pid, clock[0]))):
                launcher.guard(10, launcher.Limits(cgroup, 2 << 30, 3 << 29), report,
                               lambda: next(looks, None) is not None and not killed, pause=pause)
            self.assertEqual([pid for pid, _ in killed], [11])
            # Stopped at the first reading of the table a second after the one that found the controller's memory.
            self.assertGreaterEqual(killed[0][1], 101.0)
            self.assertLess(killed[0][1], 101.1)

    def test_the_reason_reaches_a_pipe_or_terminal_but_never_a_file_and_never_waits(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            fd = root / "proc" / "123" / "fd"
            fd.mkdir(parents=True)
            fifo, regular = root / "pipe", root / "role-file.txt"
            os.mkfifo(fifo)
            regular.write_text("the role's own data\n")
            (fd / "2").symlink_to(fifo)
            # No reader: skipped, not waited for.
            launcher.tell(123, "(launcher) Stopped x\n", proc=root / "proc")
            reader = os.open(fifo, os.O_RDONLY | os.O_NONBLOCK)
            try:
                launcher.tell(123, "(launcher) Stopped x\n", proc=root / "proc")
                self.assertEqual(os.read(reader, 100), b"(launcher) Stopped x\n")
            finally:
                os.close(reader)
            # The launcher's own standard output (the role's answer) is never written to.
            real_fstat = os.fstat
            reader = os.open(fifo, os.O_RDONLY | os.O_NONBLOCK)
            try:
                with patch.object(os, "fstat", side_effect=lambda fd: os.stat(fifo) if fd == 1 else real_fstat(fd)):
                    launcher.tell(123, "(launcher) Stopped x\n", proc=root / "proc")
                self.assertEqual(os.read(reader, 100), b"")
            finally:
                os.close(reader)
            (fd / "2").unlink()
            (fd / "2").symlink_to(regular)
            launcher.tell(123, "(launcher) Stopped x\n", proc=root / "proc")
            self.assertEqual(regular.read_text(), "the role's own data\n")
            launcher.tell(456, "(launcher) Stopped x\n", proc=root / "proc")  # gone already: nothing happens

    def test_a_guard_that_cannot_run_says_so_in_the_roles_record(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            work, home, cgroup = root / "work", root / "home", root / "cgroup"
            for directory in (work, home, cgroup):
                directory.mkdir()

            def portable_open(path, **_):
                return os.open(path, os.O_RDONLY)

            class Started(Exception):
                pass

            def execve(path, argv, environment):
                raise Started(argv)

            arguments = ["linux_role.py", "--network", "none", "--", "/bin/true"]
            for state, expected in (
                    ("absent", "The memory guard is off for this role: the container's memory use could not be read in %s" % cgroup),
                    ("small", "The memory guard is off for this role: a headroom of 512 MiB does not fit under the "
                              "container's memory limit of 512 MiB."),
                    ("no limit", None)):
                if state == "small":
                    write_cgroup(cgroup, 512 << 20, 1 << 20)
                elif state == "no limit":
                    write_cgroup(cgroup, "max", 1 << 20)
                report = io.StringIO()
                with self.subTest(state), patch.object(sys, "platform", "linux"), \
                        patch.object(launcher.shutil, "which", return_value="/usr/bin/bwrap"), \
                        patch.object(launcher, "open_path", side_effect=portable_open), \
                        patch.object(os, "O_PATH", 0, create=True), \
                        patch.object(launcher, "own_cgroup", return_value=(cgroup, None)), \
                        patch.object(launcher, "contain"), patch.object(os, "execve", side_effect=execve), \
                        patch.object(sys, "argv", arguments), patch.object(sys, "stderr", report), \
                        patch.dict(os.environ, {"TASK_WORKSPACE": str(work), "TASK_HOME": str(home)}):
                    with self.assertRaises(Started):
                        launcher.main()
                    lines = report.getvalue().splitlines()
                    if expected is None:
                        self.assertEqual(lines, [])
                    else:
                        self.assertEqual(len(lines), 1, lines)
                        self.assertTrue(lines[0].startswith("(launcher) " + expected), lines[0])


@unittest.skipUnless(sys.platform == "linux", "requires Linux /proc")
class ProcessTableTests(unittest.TestCase):
    def test_this_process_is_read_as_a_process_of_this_namespace(self):
        table = launcher.processes()
        own = table[os.getpid()]
        self.assertEqual((own.parent, own.uid, own.nested), (os.getppid(), os.getuid(), False))
        self.assertGreater(own.anon, 0)


REASON = (r"\(launcher\) Stopped python3 \(pid \d+, \d+ MiB resident; this role's processes \d+ MiB in all\) because "
          r"this container's memory in use, file and reclaimable kernel caches left out, reached 1948 of 2048 MiB\.")

HOLDER = ("import time\n"
          "held = bytearray(64 << 20)\n"
          "held[::4096] = b'\\x01' * (len(held) // 4096)\n"
          "print('holding', flush=True)\n"
          "time.sleep(60)\n")


@unittest.skipUnless(sys.platform == "linux" and shutil.which("bwrap"), "requires real Linux bubblewrap isolation")
class MemoryGuardTests(unittest.TestCase):
    def test_the_largest_role_process_is_stopped_the_role_continues_and_the_reason_is_reported(self):
        with tempfile.TemporaryDirectory(dir="/tmp") as temporary:
            root = Path(temporary)
            work, home, cgroup = root / "workspace", root / "home", root / "cgroup"
            work.mkdir()
            home.mkdir()
            (work / "hold.py").write_text(HOLDER)
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
            report = io.StringIO()
            try:
                self.assertEqual(role.stdout.readline(), "holding\n", "the role did not start")
                # The container's use as the guard reads it: over the threshold.
                write_cgroup(cgroup, 2 << 30, (2 << 30) - (100 << 20))
                watcher = threading.Thread(target=launcher.guard,
                                           args=(role.pid, launcher.Limits(cgroup, 2 << 30, (2 << 30) - (512 << 20)),
                                                 report, lambda: role.poll() is None),
                                           kwargs={"interval": 0.05, "near": 0.05})
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
            lines = report.getvalue().splitlines()
            self.assertEqual(len(lines), 1, lines)
            self.assertRegex(lines[0], "^" + REASON)
            # The holder's standard error was the launcher's pipe: the reason reached it too.
            self.assertRegex(errors, REASON)


# Runs the launcher's main() with the container's cgroup read from a given
# directory, under a parent that collects every process left behind (it is a
# child subreaper, as a container's main process is the parent of orphans).
# "check" prints how many it collected after the launcher ended; "cancel"
# also stops the launcher's process group once the role printed a line, as the
# controller does when it cancels a role.
SUPERVISED = """
import ctypes, importlib.util, os, signal, subprocess, sys, time
from pathlib import Path
if sys.argv[1] in ("launch", "launch-faulty"):
    spec = importlib.util.spec_from_file_location("linux_role", sys.argv[2])
    launcher = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(launcher)
    cgroup = Path(sys.argv[3])
    launcher.own_cgroup = lambda *_: (cgroup, None)
    if sys.argv[1] == "launch-faulty":
        def guard(*_, **__):
            raise RuntimeError("a fault")
        launcher.guard = guard
    sys.argv = [sys.argv[2], *sys.argv[4:]]
    launcher.main()
ctypes.CDLL(None, use_errno=True).prctl(36, 1)  # PR_SET_CHILD_SUBREAPER
mode = sys.argv[1]
launch = "launch-faulty" if mode == "check-faulty" else "launch"
role = subprocess.Popen([sys.executable, "-B", "-c", sys.argv[2], launch, *sys.argv[3:]], stdin=subprocess.DEVNULL,
                        stdout=subprocess.PIPE if mode == "cancel" else None, start_new_session=mode == "cancel")
if mode == "cancel":
    print(role.stdout.readline().decode(), end="", flush=True)
    os.killpg(role.pid, signal.SIGTERM)
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

    def launch(self, root, program, usage=1 << 30, over_after=None, mode="check", oom_group=0):
        work, home, cgroup = root / "workspace", root / "home", root / "cgroup"
        work.mkdir(exist_ok=True)
        home.mkdir(exist_ok=True)
        write_cgroup(cgroup, 2 << 30, usage, oom_group=oom_group)
        environment = {"TASK_WORKSPACE": str(work), "TASK_HOME": str(home), "PATH": os.environ["PATH"]}
        role = subprocess.Popen([sys.executable, "-B", "-c", SUPERVISED, mode, SUPERVISED,
                                 str(Path(__file__).with_name("linux_role.py")), str(cgroup), "--network", "none",
                                 "--", *program], env=environment, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            if over_after is not None:
                self.assertEqual(role.stdout.readline(), over_after, "the role did not start")
                write_cgroup(cgroup, 2 << 30, (2 << 30) - (100 << 20), oom_group=oom_group)
            try:
                output, errors = role.communicate(timeout=30)
            except subprocess.TimeoutExpired:
                output, errors = None, "the launcher did not end within 30 s"
        finally:
            if role.poll() is None:
                role.kill()
                role.wait()
        return output, errors

    def test_the_exit_status_passes_through_and_nothing_is_left_behind(self):
        with tempfile.TemporaryDirectory(dir="/tmp") as temporary:
            output, errors = self.launch(Path(temporary), ["/bin/sh", "-c", "sleep 0.3; exit 7"])
        self.assertEqual(errors, "launcher status 7, processes left behind 0\n", output)

    def test_role_processes_come_first_unless_the_kernel_stops_the_container_together_and_no_limit_is_set(self):
        program = ["/bin/sh", "-c", "cat /proc/self/oom_score_adj; ulimit -d; grep SigBlk /proc/self/status"]
        own = Path("/proc/self/oom_score_adj").read_text()
        for oom_group, adj in ((0, "1000\n"), (1, own)):
            with self.subTest(oom_group=oom_group), tempfile.TemporaryDirectory(dir="/tmp") as temporary:
                output, errors = self.launch(Path(temporary), program, oom_group=oom_group)
                # The guard waits for bubblewrap with signals blocked; the role must not inherit that.
                self.assertEqual(output, adj + "unlimited\nSigBlk:\t0000000000000000\n", errors)

    def test_the_largest_role_process_is_stopped_its_tool_and_the_record_say_why_and_the_launcher_ends_as_the_role_did(self):
        # Through a tool: the holder's standard error is a pipe to sed, as a
        # compiler's is to its build tool. Directly: it is the launcher's own,
        # which carries the line once.
        for name, role, started in (
                ("through a tool", 'python3 -B hold.py 2>&1 | sed -u "s/^/tool| /"; echo "the holder ended with ${PIPESTATUS[0]}"',
                 "tool| holding\n"),
                ("directly", 'python3 -B hold.py; echo "the holder ended with $?"', "holding\n")):
            with self.subTest(name), tempfile.TemporaryDirectory(dir="/tmp") as temporary:
                root = Path(temporary)
                (root / "workspace").mkdir()
                (root / "workspace" / "hold.py").write_text(HOLDER)
                output, errors = self.launch(root, ["/bin/bash", "-c", role], over_after=started)
                self.assertIsNotNone(output, errors)
                if name == "through a tool":
                    self.assertRegex(output, "^tool\\| " + REASON, errors)
                else:
                    self.assertNotIn("(launcher)", output)
                self.assertTrue(output.endswith("the holder ended with 137\n"), output)
                lines = errors.splitlines()
                # The shell's own "Killed" may stand between the two; only one process was stopped.
                self.assertEqual(len([line for line in lines if "(launcher)" in line]), 1, errors)
                self.assertRegex(lines[0], "^" + REASON)
                self.assertEqual(lines[-1], "launcher status 0, processes left behind 0")

    def test_a_fault_of_the_guard_leaves_the_role_running_unguarded_to_its_own_end(self):
        with tempfile.TemporaryDirectory(dir="/tmp") as temporary:
            output, errors = self.launch(Path(temporary), ["/bin/sh", "-c", "sleep 0.3; exit 7"], mode="check-faulty")
        self.assertEqual(errors, "(launcher) The memory guard stopped: a fault\n"
                                 "launcher status 7, processes left behind 0\n", output)

    def test_a_cancel_ends_the_launcher_by_the_same_signal_and_leaves_nothing_behind(self):
        with tempfile.TemporaryDirectory(dir="/tmp") as temporary:
            output, errors = self.launch(Path(temporary), ["/bin/sh", "-c", "echo started; sleep 30"], mode="cancel")
        self.assertEqual(output, "started\n")
        self.assertEqual(errors, "launcher status %d, processes left behind 0\n" % -signal.SIGTERM)

    def test_bubblewrap_ending_by_a_signal_ends_the_launcher_by_it(self):
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

        with tempfile.TemporaryDirectory(dir="/tmp") as temporary:
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
                os.kill(bubblewrap[0], signal.SIGKILL)
                self.assertEqual(role.wait(timeout=10), -signal.SIGKILL)
            finally:
                if role.poll() is None:
                    os.killpg(role.pid, signal.SIGKILL)
                    role.wait()
                role.stdout.close()


if __name__ == "__main__":
    unittest.main()
