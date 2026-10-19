"""Exercise both verification entry points with real commits and commands."""
from pathlib import Path
import shlex
import subprocess
import sys
import unittest

import test_verify_merged as fixture


class AddedTests(unittest.TestCase):
    def test_later_branch_change_cannot_hide_a_delivered_test(self):
        f = fixture.VerificationTests()
        f.setUp()
        self.addCleanup(f.doCleanups)
        source = f.root / "source"
        base = f.git(source, "rev-parse", "HEAD").stdout.strip()
        target = source / "new_test.py"
        target.write_text("print('test')\n")
        f.git(source, "add", ".")
        f.git(source, "commit", "-m", "Add a test")
        delivered = f.git(source, "rev-parse", "HEAD").stdout.strip()
        f.git(source, "rm", "new_test.py")
        f.git(source, "commit", "-m", "A later change removed the test")
        f.git(source, "push", str(f.remote), "master")
        f.receipt(merge_sha=delivered)
        result = f.verify("/usr/bin/true", TASK_BASE_COMMIT=base,
                          ADDED_TEST_COMMAND=shlex.quote(sys.executable) + " {file}")
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("new_test.py", result.stdout)
        self.assertIn("Exit status: 2", result.stdout)

    def test_no_added_tests_does_not_invoke_the_individual_command(self):
        f = fixture.VerificationTests()
        f.setUp()
        self.addCleanup(f.doCleanups)
        source = f.root / "source"
        base = f.git(source, "rev-parse", "HEAD").stdout.strip()
        env = dict(TASK_BASE_COMMIT=base, ADDED_TEST_COMMAND="unavailable-test-runner {file}")
        result = subprocess.run([sys.executable, "-B", str(Path(__file__).with_name("verify_added_tests.py")),
                                 "--", "/usr/bin/true"], cwd=source,
                                env=dict(fixture.git_environment(), TASK_WORKSPACE=str(source), **env),
                                capture_output=True, text=True, timeout=20)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("No added test files", result.stdout)
        f.receipt()
        result = f.verify("/usr/bin/true", **env)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("No added test files", result.stdout)

    def test_added_test_pass_failure_and_unconfigured_command_before_and_after_delivery(self):
        for after in (False, True):
            for exit_code, configured in ((0, True), (7, True), (7, False)):
                with self.subTest(after=after, exit_code=exit_code, configured=configured):
                    f = fixture.VerificationTests()
                    f.setUp()
                    self.addCleanup(f.doCleanups)
                    source = f.root / "source"
                    base = f.git(source, "rev-parse", "HEAD").stdout.strip()
                    target = source / "tests" / "new test.py"
                    target.parent.mkdir()
                    target.write_text("print('INDIVIDUAL TEST RAN')\nraise SystemExit(%d)\n" % exit_code)
                    env = dict(TASK_BASE_COMMIT=base)
                    if configured:
                        env["ADDED_TEST_COMMAND"] = shlex.quote(sys.executable) + " {file}"
                    if after:
                        f.git(source, "add", ".")
                        f.git(source, "commit", "-m", "Add a test")
                        delivered = f.git(source, "rev-parse", "HEAD").stdout.strip()
                        f.git(source, "push", str(f.remote), "master")
                        f.receipt(merge_sha=delivered)
                        result = f.verify("/usr/bin/true", **env)
                    else:
                        # Untracked, staged and committed tests are all additions.
                        if configured:
                            f.git(source, "add", ".")
                            if exit_code == 0:
                                f.git(source, "commit", "-m", "Add a test")
                        result = subprocess.run([sys.executable, "-B", str(Path(__file__).with_name("verify_added_tests.py")),
                                                 "--", "/usr/bin/true"], cwd=source,
                                                env=dict(fixture.git_environment(), TASK_WORKSPACE=str(source), **env),
                                                capture_output=True, text=True, timeout=20)
                    self.assertEqual(result.returncode, int(configured and exit_code != 0), result.stdout + result.stderr)
                    self.assertIn("tests/new test.py", result.stdout)
                    if configured:
                        self.assertIn("INDIVIDUAL TEST RAN", result.stdout)
                    else:
                        self.assertIn("not executed individually", result.stdout)
                        self.assertNotIn("INDIVIDUAL TEST RAN", result.stdout)


if __name__ == "__main__":
    unittest.main()
