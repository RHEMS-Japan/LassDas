"""Configuration/path tests. Real namespace checks run separately on Linux."""
import argparse
import importlib.util
import os
from pathlib import Path
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
        args = argparse.Namespace(program=["--", "/bin/true"], write=[], runtime=[], network="none")
        with patch.object(sys, "platform", "linux"), patch.object(launcher.shutil, "which", return_value="/usr/bin/bwrap"):
            for home in ("/work", "/work/role", "/"):
                with self.assertRaises(ValueError):
                    launcher.command(args, {"TASK_WORKSPACE":"/work", "TASK_HOME":home})
            for write in ("", "../secret", "/secret", "src/../../secret"):
                args.write = [write]
                with self.assertRaises(ValueError):
                    launcher.command(args, {"TASK_WORKSPACE":"/work", "TASK_HOME":"/home/role"})


@unittest.skipUnless(sys.platform == "linux", "requires Linux O_PATH directory handles")
class DescriptorTests(unittest.TestCase):
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


if __name__ == "__main__":
    unittest.main()
