"""Real Git history and real commands; nothing about the result is simulated."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest

SCRIPT = Path(__file__).with_name("verify_merged.py").resolve()
TOKEN = "fixture-delivery-credential-7c4d2b"

IDENTITY = ("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid")


def git_environment():
    """No personal configuration, and no guessed identity either: a runtime
    with neither is exactly where these programs have to work."""
    return {"PATH": os.environ["PATH"], "LANG": "C.UTF-8", "GIT_CONFIG_GLOBAL": os.devnull,
            "GIT_CONFIG_SYSTEM": os.devnull, "GIT_CONFIG_NOSYSTEM": "1", "GIT_TERMINAL_PROMPT": "0",
            "GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "user.useConfigOnly",
            "GIT_CONFIG_VALUE_0": "true"}


@unittest.skipUnless(shutil.which("git"), "requires Git")
class VerificationTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="verify-test-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()
        self.remote = self.root / "target.git"
        self.workspace = self.root / "job" / "workspace"
        self.home = self.root / "job" / "home"
        (self.workspace / ".git" / "ticket-engine").mkdir(parents=True)
        self.home.mkdir(parents=True)
        source = self.root / "source"
        source.mkdir()
        self.git(source, "init", "--initial-branch=master")
        (source / "main.go").write_text("package main\n")
        self.git(source, "add", "-A")
        self.git(source, "commit", "-m", "Codex: initial")
        self.git(source, "checkout", "-b", "ticket/TICKET-41")
        (source / "main.go").write_text("package main // delivered\n")
        self.git(source, "add", "-A")
        self.git(source, "commit", "-m", "Codex: deliver")
        self.delivered = self.git(source, "rev-parse", "HEAD").stdout.strip()
        self.git(source, "checkout", "master")
        self.git(source, "merge", "--no-ff", "-m", "Merge the delivery", "ticket/TICKET-41")
        self.merge = self.git(source, "rev-parse", "HEAD").stdout.strip()
        self.unmerged = self.git(source, "commit-tree", "HEAD^{tree}", "-p", "HEAD",
                                 "-m", "Codex: never merged").stdout.strip()
        self.git(source, "clone", "--bare", str(source), str(self.remote))

    def git(self, directory, *arguments):
        return subprocess.run(["git", "-C", str(directory), *IDENTITY, *arguments],
                              check=True, capture_output=True, text=True, env=git_environment())

    def receipt(self, **fields):
        record = {"issue": "TICKET-41", "repository": "owner/project", "base_branch": "master",
                  "branch": "ticket/TICKET-41", "head": self.delivered, "pull_request": 1,
                  "merge_sha": self.merge}
        record.update(fields)
        (self.workspace / ".git/ticket-engine/delivery.json").write_text(json.dumps(record))

    def verify(self, commands, *arguments, **extra):
        environment = dict(git_environment(), PYTHONDONTWRITEBYTECODE="1")
        environment.update({
                       "HOME": str(self.home), "TASK_WORKSPACE": str(self.workspace),
                       "TASK_HOME": str(self.home), "GITHUB_TOKEN": TOKEN,
                       "DELIVERY_REPOSITORY": "owner/project", "DELIVERY_BASE_BRANCH": "master",
                       "DELIVERY_REMOTE_URL": str(self.remote), "VERIFY_COMMANDS": commands})
        environment.update(extra)
        return subprocess.run([sys.executable, "-B", str(SCRIPT), *arguments], input="the role prompt",
                              cwd=str(self.workspace), env=environment, capture_output=True,
                              text=True, timeout=120)

    def test_reports_a_passing_verification_of_the_merged_branch(self):
        self.receipt()
        result = self.verify("/bin/sh -c 'grep -q delivered main.go'\n/bin/sh -c 'echo checked'")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("The merge commit is contained in master.", result.stdout)
        self.assertIn("0 of 2 configured verification commands failed.", result.stdout)
        self.assertIn("checked", result.stdout)

    def test_reports_a_failing_verification_command(self):
        self.receipt()
        result = self.verify("/bin/sh -c 'echo first'\n/bin/sh -c 'echo broken >&2; exit 2'")
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("Exit status: 2", result.stdout)
        self.assertIn("broken", result.stdout)
        self.assertIn("1 of 2 configured verification commands failed.", result.stdout)

    def test_a_delivery_missing_from_the_branch_stops_the_check(self):
        self.receipt(merge_sha=self.unmerged)
        result = self.verify("/bin/sh -c 'touch %s/ran-anyway'" % self.home)
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("The merge commit is NOT contained in master.", result.stdout)
        self.assertFalse((self.home / "ran-anyway").exists())

    def test_refuses_without_a_recorded_merge(self):
        result = self.verify("/bin/sh -c 'echo unreachable'")
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("No delivery receipt", result.stderr)
        self.receipt(merge_sha="")
        again = self.verify("/bin/sh -c 'echo unreachable'")
        self.assertEqual(again.returncode, 1, again.stdout + again.stderr)
        self.assertIn("no completed merge commit", again.stderr)

    def test_the_credential_is_not_handed_to_the_configured_commands(self):
        self.receipt()
        result = self.verify("/bin/sh -c 'printf credential=%s \"${GITHUB_TOKEN-unset}\"'")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("credential=unset", result.stdout)
        self.assertNotIn(TOKEN, result.stdout + result.stderr)

    def test_a_branch_that_cannot_be_reached_is_waited_out_then_reported(self):
        self.receipt()
        result = self.verify("/bin/sh -c 'echo unreachable'",
                             DELIVERY_REMOTE_URL="https://127.0.0.1:1/absent.git",
                             DELIVERY_RETRY_ATTEMPTS="2", DELIVERY_RETRY_SECONDS="0.05")
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("may pass", result.stdout)
        self.assertIn("did not go through in 2 attempts", result.stdout)
        self.assertIn("so nothing was verified", result.stdout)
        self.assertNotIn("unreachable", result.stdout)

    def test_check_mode_runs_the_commands_without_a_delivery_and_ends_non_zero(self):
        result = self.verify("/bin/sh -c 'echo checked'", "--dry-run")
        self.assertEqual(result.returncode, 3, result.stdout + result.stderr)
        self.assertIn("ends non-zero on purpose", result.stdout)
        self.assertIn("0 of 1 configured verification commands failed.", result.stdout)


if __name__ == "__main__":
    unittest.main()
