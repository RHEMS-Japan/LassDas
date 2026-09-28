"""One real fetch cycle against a real repository; no service is simulated."""
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest

SCRIPT = Path(__file__).with_name("mirror_loop.py").resolve()
TOKEN = "fixture-delivery-credential-4b8e10"


@unittest.skipUnless(shutil.which("git"), "requires Git")
class MirrorTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="mirror-test-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()
        self.source = self.root / "source"
        self.source.mkdir()
        self.mirror = self.root / "state" / "mirror" / "project.git"
        self.git(self.source, "init", "--initial-branch=master")
        (self.source / "main.go").write_text("package main\n")
        self.git(self.source, "add", "-A")
        self.git(self.source, "commit", "-m", "Codex: initial")

    def git(self, directory, *arguments):
        return subprocess.run(["git", "-C", str(directory), "-c", "user.name=Fixture",
                               "-c", "user.email=fixture@example.invalid", *arguments],
                              check=True, capture_output=True, text=True,
                              env={"PATH": os.environ["PATH"], "GIT_CONFIG_GLOBAL": os.devnull,
                                   "GIT_CONFIG_NOSYSTEM": "1", "GIT_TERMINAL_PROMPT": "0"})

    def cycle(self, **extra):
        environment = {"PATH": os.environ["PATH"], "LANG": "C.UTF-8", "PYTHONDONTWRITEBYTECODE": "1",
                       "HOME": str(self.root), "GITHUB_TOKEN": TOKEN,
                       "DELIVERY_REPOSITORY": "owner/project", "MIRROR_PATH": str(self.mirror),
                       "DELIVERY_REMOTE_URL": str(self.source)}
        environment.update(extra)
        return subprocess.run([sys.executable, "-B", str(SCRIPT), "--once"], env=environment,
                              capture_output=True, text=True, timeout=120)

    def test_creates_a_bare_mirror_and_then_carries_new_work_into_it(self):
        created = self.cycle()
        self.assertEqual(created.returncode, 0, created.stdout + created.stderr)
        self.assertTrue((self.mirror / "HEAD").is_file())
        self.assertFalse((self.mirror / ".git").exists(), "the mirror must have no working tree")
        (self.source / "main.go").write_text("package main // later\n")
        self.git(self.source, "add", "-A")
        self.git(self.source, "commit", "-m", "Codex: later")
        expected = self.git(self.source, "rev-parse", "HEAD").stdout.strip()
        refreshed = self.cycle()
        self.assertEqual(refreshed.returncode, 0, refreshed.stdout + refreshed.stderr)
        self.assertEqual(self.git(self.mirror, "rev-parse", "refs/heads/master").stdout.strip(), expected)
        # What a role's workspace preparation does: clone from the mirror only.
        workspace = self.root / "workspace"
        self.git(self.root, "clone", "--no-local", str(self.mirror), str(workspace))
        self.assertIn("later", (workspace / "main.go").read_text())

    def test_an_unreachable_source_is_reported_without_the_credential(self):
        result = self.cycle(DELIVERY_REMOTE_URL=str(self.root / "absent.git"))
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("The mirror was not updated", result.stdout)
        self.assertNotIn(TOKEN, result.stdout + result.stderr)
        self.assertFalse(self.mirror.exists())


if __name__ == "__main__":
    unittest.main()
