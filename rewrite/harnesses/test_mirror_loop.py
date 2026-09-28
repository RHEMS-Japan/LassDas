"""One real fetch cycle against a real repository; no service is simulated."""
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import time
import unittest

SCRIPT = Path(__file__).with_name("mirror_loop.py").resolve()
TOKEN = "fixture-delivery-credential-4b8e10"

IDENTITY = ("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid")


def git_environment():
    """No personal configuration, and no guessed identity either: a runtime
    with neither is exactly where these programs have to work."""
    return {"PATH": os.environ["PATH"], "LANG": "C.UTF-8", "GIT_CONFIG_GLOBAL": os.devnull,
            "GIT_CONFIG_SYSTEM": os.devnull, "GIT_CONFIG_NOSYSTEM": "1", "GIT_TERMINAL_PROMPT": "0",
            "GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "user.useConfigOnly",
            "GIT_CONFIG_VALUE_0": "true"}


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
        return subprocess.run(["git", "-C", str(directory), *IDENTITY, *arguments],
                              check=True, capture_output=True, text=True, env=git_environment())

    def loop(self, **extra):
        """Start the real loop, not a single cycle, and let it run."""
        child = subprocess.Popen([sys.executable, "-B", str(SCRIPT)], env=self.environment(**extra),
                                 stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        self.addCleanup(self.stop, child)
        return child

    @staticmethod
    def stop(child):
        if child.poll() is None:
            child.terminate()
        try:
            child.wait(timeout=20)
        except subprocess.TimeoutExpired:
            child.kill()

    def environment(self, **extra):
        environment = dict(git_environment(), PYTHONDONTWRITEBYTECODE="1")
        environment.update({
                       "HOME": str(self.root), "GITHUB_TOKEN": TOKEN,
                       "DELIVERY_REPOSITORY": "owner/project", "MIRROR_PATH": str(self.mirror),
                       "DELIVERY_REMOTE_URL": str(self.source), "MIRROR_INTERVAL_SECONDS": "0.2"})
        environment.update(extra)
        return environment

    def cycle(self, **extra):
        return subprocess.run([sys.executable, "-B", str(SCRIPT), "--once"], env=self.environment(**extra),
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

    def test_a_refused_source_ends_the_loop_and_says_so(self):
        child = self.loop(DELIVERY_REMOTE_URL=str(self.root / "absent.git"))
        out, err = child.communicate(timeout=30)
        self.assertEqual(child.returncode, 1, out + err)
        self.assertIn("The mirror was refused and this process is ending", out)
        self.assertNotIn(TOKEN, out + err)
        self.assertFalse(self.mirror.exists())

    def test_a_source_that_cannot_be_reached_keeps_the_loop_alive(self):
        child = self.loop(DELIVERY_REMOTE_URL="https://127.0.0.1:1/absent.git")
        deadline = time.monotonic() + 25
        lines = []
        while time.monotonic() < deadline and len(lines) < 2:
            line = child.stdout.readline()
            if not line:
                break
            if "may pass on its own" in line:
                lines.append(line)
        self.assertEqual(len(lines), 2, "".join(lines))
        self.assertIsNone(child.poll(), "a failure that may pass must not end the loop")
        child.terminate()
        out, err = child.communicate(timeout=20)
        self.assertEqual(child.returncode, 0, out + err)
        self.assertIn("Stopped on request", out)
        self.assertNotIn(TOKEN, "".join(lines) + out + err)


if __name__ == "__main__":
    unittest.main()
