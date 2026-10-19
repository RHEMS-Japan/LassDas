"""Portable cache environment and mount-scope checks, not kernel confinement."""
import argparse
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

import linux_role as launcher


class DependencyCacheTests(unittest.TestCase):
    def test_cache_scope_rejects_neighbors_parents_and_overlapping_mounts(self):
        args = argparse.Namespace(program=["--", "/bin/true"], write=[], create=[], runtime=[], network="none")
        env = {"TASK_HOME": "/jobs/one/homes/0-0", "TASK_WORKSPACE": "/jobs/one/workspace"}
        with patch.object(sys, "platform", "linux"), patch.object(launcher.shutil, "which", return_value="/usr/bin/bwrap"), patch.object(launcher, "open_path", side_effect=AssertionError("must refuse before mounting")):
            for cache in ("/jobs", "/jobs/one", "/jobs/two/dependency-cache", "/jobs/one/workspace/cache"):
                with self.subTest(cache=cache), self.assertRaises(ValueError):
                    launcher.command(args, dict(env, TASK_DEPENDENCY_CACHE=cache))
            env["TASK_DEPENDENCY_CACHE"] = "/jobs/one/dependency-cache"
            for key in ("TASK_HOME", "TASK_HISTORY"):
                with self.subTest(key=key), self.assertRaises(ValueError):
                    launcher.command(args, dict(env, **{key: "/jobs/one/dependency-cache/private"}))
            args.runtime = ["/jobs/one"]
            with self.assertRaises(ValueError):
                launcher.command(args, env)

    def test_only_one_request_cache_is_mounted_and_default_environment_is_unchanged(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            work, home, cache = root / "workspace", root / "homes/0-0", root / "dependency-cache"
            for path in (work, home, cache):
                path.mkdir(parents=True)
            args = argparse.Namespace(program=["--", "/bin/true"], write=[], create=[], runtime=[], network="none")
            base = {"TASK_HOME": str(home), "TASK_WORKSPACE": str(work)}
            with patch.object(sys, "platform", "linux"), patch.object(launcher.shutil, "which", return_value="/usr/bin/bwrap"), patch.object(os, "O_PATH", 0, create=True), patch.object(launcher, "open_path", side_effect=lambda path, **kw: os.open(path, os.O_RDONLY)):
                for enabled in (False, True):
                    env = dict(base, **({"TASK_DEPENDENCY_CACHE": str(cache)} if enabled else {}))
                    argv, child, descriptors = launcher.command(args, env)
                    try:
                        targets = [argv[i+2] for i, word in enumerate(argv) if word in ("--bind-fd", "--ro-bind-fd")]
                        self.assertNotIn(str(root), targets)
                        if enabled:
                            self.assertIn(str(cache), targets)
                            self.assertEqual(child["GOMODCACHE"], str(cache / "go-mod"))
                            self.assertEqual(child["npm_config_cache"], str(cache / "npm"))
                            self.assertEqual(child["npm_config_store_dir"], str(cache / "pnpm"))
                            self.assertNotIn("PNPM_HOME", child)
                            self.assertEqual(child["GOCACHE"], str(cache / "go-build"))
                        else:
                            self.assertNotIn(str(cache), targets)
                            self.assertEqual(child, dict(base, HOME=str(home), HERMES_HOME=str(home), TMPDIR="/tmp"))
                    finally:
                        for descriptor in descriptors:
                            os.close(descriptor)


@unittest.skipUnless(sys.platform == "linux" and shutil.which("bwrap"), "requires real Linux bubblewrap isolation")
class DependencyCacheIsolationTests(unittest.TestCase):
    def test_two_roles_share_cache_but_not_homes_or_other_requests(self):
        with tempfile.TemporaryDirectory(dir="/tmp") as temporary:
            root = Path(temporary)
            work, cache = root / "one/workspace", root / "one/dependency-cache"
            homes = [root / ("one/homes/%d-0" % i) for i in range(2)]
            neighbor = root / "two/dependency-cache"
            for path in (work, cache, neighbor, *homes):
                path.mkdir(parents=True)
            (neighbor / "value").write_text("other request")
            for home in homes:
                (home / "private").write_text("private home")
            for index, home in enumerate(homes):
                program = ('printf first > "$TASK_DEPENDENCY_CACHE/shared"' if index == 0 else
                           'test "$(cat "$TASK_DEPENDENCY_CACHE/shared")" = first')
                program += ' && test ! -e "$1" && test ! -e "$2"'
                args = argparse.Namespace(program=["--", "/bin/sh", "-c", program, "cache-role",
                                                   str(neighbor / "value"), str(homes[1-index] / "private")],
                                          write=[], create=[], runtime=[], network="none")
                argv, env, descriptors = launcher.command(args, {"TASK_WORKSPACE": str(work), "TASK_HOME": str(home),
                                                                  "TASK_DEPENDENCY_CACHE": str(cache), "PATH": os.environ["PATH"]})
                try:
                    result = subprocess.run(argv, env=env, pass_fds=descriptors, capture_output=True, text=True, timeout=10)
                    self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                finally:
                    for descriptor in descriptors:
                        os.close(descriptor)
            self.assertEqual((neighbor / "value").read_text(), "other request")
