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

    def unchanged_receipt(self, **fields):
        """What the delivery records when it ended without a change."""
        record = {"unchanged": True, "issue": "TICKET-41", "repository": "owner/project",
                  "base_branch": "master", "base_sha": self.merge, "workspace_head": self.merge}
        record.update(fields)
        (self.workspace / ".git/ticket-engine/delivery.json").write_text(json.dumps(record))

    def test_an_unchanged_delivery_is_checked_on_the_branch_as_it_is_now(self):
        # The request stood on the branch before the delivery merged above.
        # What is checked is the branch as the requester finds it now; the
        # recorded commit only has to be part of it.
        earlier = self.git(self.remote, "rev-parse", "refs/heads/master^1").stdout.strip()
        self.unchanged_receipt(base_sha=earlier, workspace_head=earlier)
        result = self.verify("/bin/sh -c 'grep -q delivered main.go'")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("No merge was made: the delivery receipt records that no file was changed", result.stdout)
        self.assertIn("The commit the request stands on is contained in master.", result.stdout)
        self.assertIn("The configured commands ran on master as it is now, at commit %s, as after a merge."
                      % self.merge, result.stdout)
        self.assertIn("0 of 1 configured verification commands failed.", result.stdout)
        # Someone breaks the behaviour on the branch afterwards: the check
        # fails, as it would after a merge, instead of passing on the old
        # commit the request stood on.
        original = self.git(self.remote, "rev-parse", "refs/heads/master^1^{tree}").stdout.strip()
        broken = self.git(self.remote, "commit-tree", original, "-p", self.merge,
                          "-m", "Codex: remove the delivered behaviour").stdout.strip()
        self.git(self.remote, "update-ref", "refs/heads/master", broken)
        self.unchanged_receipt()
        failing = self.verify("/bin/sh -c 'grep -q delivered main.go'")
        self.assertEqual(failing.returncode, 1, failing.stdout + failing.stderr)
        self.assertIn("The configured commands ran on master as it is now, at commit %s" % broken, failing.stdout)
        self.assertIn("1 of 1 configured verification commands failed.", failing.stdout)

    def test_an_unchanged_delivery_whose_commit_left_the_branch_is_not_verified(self):
        self.unchanged_receipt(base_sha=self.unmerged)
        result = self.verify("/bin/sh -c 'touch %s/ran-anyway'" % self.home)
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("No merge was made", result.stdout)
        self.assertIn("The commit the request stands on is NOT contained in master.", result.stdout)
        self.assertFalse((self.home / "ran-anyway").exists())

    def test_an_unchanged_receipt_without_its_commit_is_refused(self):
        self.unchanged_receipt(base_sha="")
        result = self.verify("/bin/sh -c 'echo unreachable'")
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("records no commit the request stands on", result.stderr)
        self.assertNotIn("unreachable", result.stdout)

    def left_to_person_receipt(self, **fields):
        """What the delivery records when it opened the pull request and left
        the merge to a person (DELIVERY_MERGE_METHOD=none)."""
        record = {"merge_method": "none", "merge_left_to_person": True, "merge_sha": None,
                  "pull_request_url": "http://service.invalid/pulls/1"}
        record.update(fields)
        self.receipt(**record)

    def test_an_unmerged_pull_request_is_verified_at_the_head_the_delivery_pushed(self):
        self.left_to_person_receipt()
        result = self.verify("/bin/sh -c 'grep -q delivered main.go'")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("Fetched ticket/TICKET-41 of owner/project at commit %s." % self.delivered, result.stdout)
        self.assertIn("This delivery did not merge pull request 1 (http://service.invalid/pulls/1); the merge was "
                      "left to a person, and whether they merged it since is not looked at here.", result.stdout)
        self.assertIn("The commit the delivery pushed is contained in ticket/TICKET-41.", result.stdout)
        self.assertIn("The configured commands ran with commit %s checked out.\n" % self.delivered, result.stdout)
        self.assertIn("0 of 1 configured verification commands failed.", result.stdout)

    def test_an_unmerged_pull_request_whose_head_left_its_branch_is_not_verified(self):
        self.left_to_person_receipt(head=self.unmerged)
        result = self.verify("/bin/sh -c 'touch %s/ran-anyway'" % self.home)
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("The commit the delivery pushed is NOT contained in ticket/TICKET-41.", result.stdout)
        self.assertIn("the commit the delivery pushed is not part of ticket/TICKET-41", result.stdout)
        self.assertFalse((self.home / "ran-anyway").exists())

    def test_an_unmerged_pull_request_whose_branch_is_gone_is_not_verified(self):
        self.left_to_person_receipt(branch="ticket/TICKET-GONE")
        result = self.verify("/bin/sh -c 'echo unreachable'", DELIVERY_RETRY_ATTEMPTS="1")
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("Could not read ticket/TICKET-GONE of owner/project, so nothing was verified.", result.stdout)
        self.assertIn("fetch the pull request's branch ticket/TICKET-GONE", result.stdout)
        self.assertNotIn("unreachable", result.stdout)

    def test_a_pull_request_a_person_merged_is_verified_on_the_integration_branch(self):
        self.left_to_person_receipt(merge_sha=self.merge)
        result = self.verify("/bin/sh -c 'grep -q delivered main.go'")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("Fetched master of owner/project", result.stdout)
        self.assertIn("The receipt records pull request 1 merged as commit %s by someone else, not by the "
                      "delivery." % self.merge, result.stdout)
        self.assertIn("The merge commit is contained in master.", result.stdout)

    def test_a_receipt_left_to_a_person_without_its_pushed_commit_is_refused(self):
        self.left_to_person_receipt(head="")
        result = self.verify("/bin/sh -c 'echo unreachable'")
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("records no pushed commit and ticket branch to verify", result.stderr)

    def test_endings_a_person_caused_end_the_check_without_running_anything(self):
        # A pull request closed without a merge, or one whose branch a person
        # changed: nothing of this delivery is left to verify, and failing would
        # only send the work round again. The check says so and ends 0.
        generated = ["build/out-%02d.txt" % number for number in range(20)]
        for fields, said in (({"closed_unmerged": True, "ended_at": "2026-01-02T00:00:00Z"},
                              "Nothing to check: when the delivery last looked, at 2026-01-02T00:00:00Z, pull request "
                              "1 (http://service.invalid/pulls/1) had been closed by a person without being merged, "
                              "so nothing of this request had been delivered."),
                             ({"changed_by_person": True, "branch_head": self.unmerged, "not_pushed": self.delivered},
                              "Not checked: a person changed branch ticket/TICKET-41 of pull request 1 "
                              "(http://service.invalid/pulls/1); it was at %s when the delivery last looked. The "
                              "delivery's commit %s is not on that branch" % (self.unmerged, self.delivered)),
                             ({"changed_by_person": True, "branch_head": self.unmerged, "not_pushed": None,
                               "not_committed": ["go.mod", "main.go"], "not_committed_count": 2},
                              "The delivery's last commit %s is on that branch. The workspace then held changes that "
                              "were not committed (go.mod, main.go), which the delivery did not put in the pull "
                              "request." % self.delivered),
                             ({"changed_by_person": True, "branch_head": self.unmerged, "not_pushed": None,
                               "not_committed": generated, "not_committed_count": 25},
                              "The workspace then held changes that were not committed (25 paths, the first 20 of "
                              "them %s)" % ", ".join(generated)),
                             ({"closed_unmerged": True, "pull_request": 2,
                               "pull_request_url": "http://service.invalid/pulls/2",
                               "previous": [{"pull_request": 1, "merge_sha": self.merge}]},
                              "Not checked: when the delivery last looked, pull request 2 "
                              "(http://service.invalid/pulls/2) had been closed by a person without being merged, so "
                              "nothing of this round had been delivered. An earlier round of this request was merged "
                              "as commit %s through pull request 1; this check does not look at it." % self.merge)):
            self.left_to_person_receipt(**fields)
            result = self.verify("/bin/sh -c 'touch %s/ran-anyway'" % self.home,
                                 DELIVERY_REMOTE_URL=str(self.root / "unreachable.git"))
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertIn(said, result.stdout)
            self.assertIn("not run", result.stdout)
            self.assertFalse((self.home / "ran-anyway").exists())
            if fields.get("previous"):
                # An earlier round was merged: something was delivered.
                self.assertNotIn("Nothing to check", result.stdout)
                self.assertNotIn("nothing of this request", result.stdout)

    def test_command_output_that_is_not_utf8_is_reported_with_replacement_characters(self):
        self.receipt()
        result = self.verify("/bin/sh -c 'printf \"\\223\\372\\214\\352 checked\\n\"'")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("���� checked", result.stdout)
        self.assertIn("0 of 1 configured verification commands failed.", result.stdout)

    def test_check_mode_runs_the_commands_without_a_delivery_and_ends_non_zero(self):
        result = self.verify("/bin/sh -c 'echo checked'", "--dry-run")
        self.assertEqual(result.returncode, 3, result.stdout + result.stderr)
        self.assertIn("ends non-zero on purpose", result.stdout)
        self.assertIn("0 of 1 configured verification commands failed.", result.stdout)


if __name__ == "__main__":
    unittest.main()
