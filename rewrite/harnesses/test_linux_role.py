"""Configuration/path tests. Real namespace checks run separately on Linux."""
import argparse
import importlib.util
import os
from pathlib import Path, PurePosixPath
import shutil
import subprocess
import sys
import tempfile
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


if __name__ == "__main__":
    unittest.main()
