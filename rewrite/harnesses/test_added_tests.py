"""Added tests run on real request history, not concurrent deliveries."""
import json
import io
import os
from pathlib import Path
import shlex
import subprocess
import sys
import unittest
from unittest.mock import patch
from contextlib import redirect_stdout

import test_verify_merged as fixture


class AddedTests(unittest.TestCase):
    setUp = fixture.VerificationTests.setUp
    git = fixture.VerificationTests.git
    receipt = fixture.VerificationTests.receipt
    verify = fixture.VerificationTests.verify
    unchanged_receipt = fixture.VerificationTests.unchanged_receipt

    @property
    def source(self):
        return self.root / "source"

    def revision(self, name="HEAD"):
        return self.git(self.source, "rev-parse", name).stdout.strip()

    def write(self, path, code=0):
        target = self.source / path
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text("print('INDIVIDUAL TEST RAN')\nraise SystemExit(%d)\n" % code)

    def commit(self):
        self.git(self.source, "add", ".")
        self.git(self.source, "commit", "-m", "Add fixture")
        return self.revision()

    def before(self, **extra):
        environment = dict(fixture.git_environment(), TASK_WORKSPACE=str(self.source), **extra)
        return subprocess.run([sys.executable, "-B", str(Path(__file__).with_name("added_tests.py"))],
                              cwd=self.source, env=environment, input="prompt", capture_output=True,
                              text=True, timeout=20)

    def command(self):
        return shlex.quote(sys.executable) + " {file}"

    def assert_ok(self, result):
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_before_delivery_untracked_staged_committed_and_no_command(self):
        base = self.revision()
        self.write("test_first.py")
        self.write("nested/second_test.py")
        self.git(self.source, "add", "nested")
        self.assert_ok(self.before(TASK_BASE_COMMIT=base, ADDED_TEST_COMMAND=self.command()))
        self.commit()
        result = self.before(TASK_BASE_COMMIT=base, ADDED_TEST_COMMAND=self.command())
        self.assert_ok(result)
        self.assertEqual(result.stdout.count("INDIVIDUAL TEST RAN"), 2)
        self.write("test_failing.py", 7)
        result = self.before(TASK_BASE_COMMIT=base, ADDED_TEST_COMMAND=self.command())
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("1 of 3 added-test commands failed", result.stdout)
        result = self.before(TASK_BASE_COMMIT=base)
        self.assert_ok(result)
        self.assertIn("not executed individually", result.stdout)

    def test_rules_modes_package_deduplication_and_limit(self):
        base = self.revision()
        expected = ["nested/one_test.py", "nested/test_two.py", "nested/x.spec.ts", "FooTest.java", "tests/test_check.py"]
        ignored = ["docs/testing.md", "testdata/fixture.txt", "conftest.py", "pytest.ini", "latest.ts",
                   "attestation.go", "test-results/junit.xml", "tests/data.json", "tests/conftest.py",
                   "tests/__init__.py", "nested/x.spec.ts.snap", "nested/x.test.js.map",
                   "test_data.csv", "answer_test.golden"]
        for path in expected + ignored:
            self.write(path)
        (self.source / "test_symlink.py").symlink_to("nested/one_test.py")
        self.git(self.source, "add", ".")
        self.git(self.source, "update-index", "--add", "--cacheinfo", "160000," + base + ",tests/submodule")
        result = self.before(TASK_BASE_COMMIT=base, ADDED_TEST_COMMAND="echo CHECK {pkg}")
        self.assert_ok(result)
        self.assertIn("5 added test files", result.stdout)
        self.assertIn("0 of 3 added-test commands failed", result.stdout)
        for path in ignored + ["test_symlink.py", "tests/submodule"]:
            self.assertNotIn(path, result.stdout)
        limited = self.before(TASK_BASE_COMMIT=base, ADDED_TEST_COMMAND="echo CHECK {package}",
                              ADDED_TEST_MAX_COMMANDS="2")
        self.assert_ok(limited)
        self.assertIn("1 additional command not run", limited.stdout)
        self.assertIn("0 of 2 added-test commands failed, and 1 not run", limited.stdout)
        custom = self.before(TASK_BASE_COMMIT=base, ADDED_TEST_COMMAND="echo {file}",
                             ADDED_TEST_PATTERNS="*.java\ndocs/")
        self.assert_ok(custom)
        self.assertIn("2 added test files", custom.stdout)

    def test_settings_are_checked_without_additions_and_aggregate_still_runs(self):
        self.receipt()
        for command in ("echo", "echo '{file}", "echo {unknown}"):
            with self.subTest(command=command):
                result = self.verify("echo AGGREGATE", ADDED_TEST_COMMAND=command)
                self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
                self.assertIn("AGGREGATE", result.stdout)
                self.assertIn("ADDED_TEST_COMMAND", result.stdout + result.stderr)
                self.assertNotIn("Traceback", result.stderr)
                result = self.before(TASK_BASE_COMMIT=self.revision(), ADDED_TEST_COMMAND=command)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("ADDED_TEST_COMMAND", result.stdout + result.stderr)

    def test_no_additions_and_unknown_basis_keep_aggregate_environment(self):
        result = self.before(TASK_BASE_COMMIT=self.revision(), ADDED_TEST_COMMAND="unavailable-runner {file}")
        self.assert_ok(result)
        self.assertIn("0 added test files", result.stdout)
        self.receipt(merge_method="rebase", integration_base="missing-commit")
        result = self.verify("/bin/sh -c 'test -d \"$TMPDIR\" && printf \"AGGREGATE tmp=%s\\n\" \"$TMPDIR\"'",
                             TMPDIR=str(self.home), ADDED_TEST_COMMAND=self.command())
        self.assert_ok(result)
        clones = list(self.home.glob("verify-*/tmp"))
        self.assertEqual(len(clones), 1)
        self.assertIn("AGGREGATE tmp=" + str(clones[0]), result.stdout)
        self.assertIn("detection unavailable", result.stdout)
        self.assertFalse((self.home / "tmp").exists())

    def test_unborn_missing_basis_dry_run_and_prepared_base_precedence(self):
        empty = self.root / "unborn"
        empty.mkdir()
        self.git(empty, "init", "--initial-branch=main")
        (empty / "test_first.py").write_text("print('UNBORN')")
        result = subprocess.run([sys.executable, "-B", str(Path(__file__).with_name("added_tests.py"))],
                                cwd=empty, env=dict(fixture.git_environment(), ADDED_TEST_COMMAND=self.command()),
                                input="", capture_output=True, text=True, timeout=20)
        self.assert_ok(result)
        self.assertIn("UNBORN", result.stdout)
        self.write("test_request.py")
        result = self.before(ADDED_TEST_COMMAND=self.command())
        self.assert_ok(result)
        self.assertIn("detection unavailable", result.stdout)
        base = self.revision()
        self.commit()
        result = self.before(TASK_BASE_COMMIT=base, ADDED_TEST_BASE="HEAD", ADDED_TEST_COMMAND=self.command())
        self.assert_ok(result)
        self.assertIn("INDIVIDUAL TEST RAN", result.stdout)
        result = self.verify("echo AGGREGATE", "--dry-run", ADDED_TEST_COMMAND=self.command())
        self.assertEqual(result.returncode, 3)
        self.assertIn("AGGREGATE", result.stdout)

    def history(self, method):
        base = self.revision()
        self.git(self.source, "checkout", "-b", "ticket/new")
        self.write("test_ours.py")
        first = self.commit()
        self.write("test_second.py")
        head = self.commit()
        self.git(self.source, "checkout", "master")
        self.write("test_other.py", 19)
        integration = self.commit()
        if method == "merge":
            self.git(self.source, "merge", "--no-ff", "-m", "Merge request", head)
        elif method == "squash":
            self.git(self.source, "merge", "--squash", head)
            self.commit()
        elif method == "rebase":
            self.git(self.source, "cherry-pick", first, head)
        merge = self.revision()
        self.git(self.source, "push", str(self.remote), "master", "ticket/new")
        self.receipt(head=head, merge_sha=merge, merge_method=method, integration_base=base, branch="ticket/new")
        return base, head, integration, merge

    def check_delivery(self, method):
        base, head, integration, merge = self.history(method)
        result = self.verify("echo AGGREGATE", TASK_BASE_COMMIT=base, ADDED_TEST_COMMAND=self.command())
        self.assert_ok(result)
        self.assertEqual(result.stdout.count("INDIVIDUAL TEST RAN"), 2)
        self.assertNotIn("test_other.py", result.stdout)
        self.assertIn("0 of 2 added-test commands failed", result.stdout)
        self.assertLess(result.stdout.index("AGGREGATE"), result.stdout.index("INDIVIDUAL TEST RAN"))

    def test_merge_excludes_concurrent_additions(self):
        self.check_delivery("merge")

    def test_squash_excludes_concurrent_additions(self):
        self.check_delivery("squash")

    def test_rebase_checks_all_original_request_commits(self):
        self.check_delivery("rebase")

    def test_persons_rebase_uses_original_request_even_when_configured_for_merge(self):
        base, head, integration, merge = self.history("rebase")
        self.receipt(head=head, merge_sha=merge, merge_method="merge", merge_left_to_person=True,
                     integration_base=base, branch="ticket/new")
        result = self.verify("echo AGGREGATE", ADDED_TEST_COMMAND=self.command())
        self.assert_ok(result)
        self.assertEqual(result.stdout.count("INDIVIDUAL TEST RAN"), 2, result.stdout)
        self.assertNotIn("test_other.py", result.stdout)

    def test_post_delivery_cap_reports_unrun_without_retrying(self):
        self.history("merge")
        result = self.verify("echo AGGREGATE", ADDED_TEST_COMMAND=self.command(), ADDED_TEST_MAX_COMMANDS="1")
        self.assert_ok(result)
        self.assertEqual(result.stdout.count("INDIVIDUAL TEST RAN"), 1)
        self.assertIn("0 of 1 added-test commands failed, and 1 not run", result.stdout)

    def test_pending_human_merge_and_branch_only_use_recorded_request_basis(self):
        base, head, integration, merge = self.history("merge")
        for method in ("none", "merge"):
            self.receipt(head=head, branch="ticket/new", merge_sha=None, merge_method=method,
                         merge_left_to_person=True, integration_base=base)
            result = self.verify("echo AGGREGATE", TASK_BASE_COMMIT=base, ADDED_TEST_COMMAND=self.command())
            self.assert_ok(result)
            self.assertEqual(result.stdout.count("INDIVIDUAL TEST RAN"), 2, result.stdout)
        record = {"issue": "TICKET-41", "repository": "owner/project", "base_branch": "master",
                  "branch": "ticket/TICKET-41", "head": head, "published_head": head,
                  "branch_only": True, "branch_confirmed_at": "now", "integration_base": base}
        self.git(self.source, "push", str(self.remote), head + ":refs/heads/ticket/TICKET-41")
        (self.workspace / ".git/ticket-engine/delivery.json").write_text(json.dumps(record))
        result = self.verify("echo AGGREGATE", TASK_ISSUE="TICKET-41", ADDED_TEST_COMMAND=self.command())
        self.assert_ok(result)
        self.assertEqual(result.stdout.count("INDIVIDUAL TEST RAN"), 2)

    def test_removed_test_is_not_run_and_unchanged_has_no_additions(self):
        base, head, integration, merge = self.history("merge")
        self.git(self.source, "rm", "test_ours.py", "test_second.py")
        self.commit()
        self.git(self.source, "push", str(self.remote), "master")
        result = self.verify("echo AGGREGATE", ADDED_TEST_COMMAND=self.command())
        self.assert_ok(result)
        self.assertIn("absent at", result.stdout)
        self.assertIn("not run", result.stdout)
        self.assertNotIn("Exit status: 2", result.stdout)
        self.unchanged_receipt(base_sha=base)
        result = self.verify("echo AGGREGATE", TASK_BASE_COMMIT=base, ADDED_TEST_COMMAND=self.command())
        self.assert_ok(result)
        self.assertIn("Unchanged delivery: no added tests", result.stdout)

    def test_caught_up_workspace_uses_integration_side_and_quotes_paths(self):
        base, head, integration, merge = self.history("pending")
        self.git(self.source, "checkout", "ticket/new")
        import deliver_git
        from unittest.mock import patch
        with patch.dict(os.environ, fixture.git_environment(), clear=True):
            self.assertTrue(deliver_git.catch_up(self.source, str(self.remote), "master", "TICKET-41", "none"))
        self.assertEqual(self.revision("refs/ticket-engine/integration"), integration)
        self.write("test_line\nFORGED.py")
        result = self.before(TASK_BASE_COMMIT=base, ADDED_TEST_COMMAND=self.command())
        self.assert_ok(result)
        self.assertNotIn("test_other.py", result.stdout)
        self.assertNotIn("\nFORGED", result.stdout)
        self.assertIn("test_line\\nFORGED.py", result.stdout)

    def test_a_newer_origin_ref_takes_precedence_over_an_old_catch_up(self):
        base = self.revision()
        self.git(self.source, "update-ref", "refs/ticket-engine/integration", base)
        self.write("test_other.py", 9)
        integration = self.commit()
        self.git(self.source, "update-ref", "refs/remotes/origin/master", integration)
        self.write("test_ours.py")
        self.commit()
        result = self.before(TASK_BRANCH="master", TASK_BASE_COMMIT=base, ADDED_TEST_COMMAND=self.command())
        self.assert_ok(result)
        self.assertNotIn("test_other.py", result.stdout)
        self.assertEqual(result.stdout.count("INDIVIDUAL TEST RAN"), 1)

    def test_conflicting_catch_up_excludes_the_incoming_test(self):
        import deliver_git
        base, head, integration, merge = self.history("pending")
        (self.source / "main.go").write_text("integration change\n")
        integration = self.commit()
        self.git(self.source, "push", str(self.remote), "master")
        self.git(self.source, "checkout", "ticket/new")
        (self.source / "main.go").write_text("request change\n")
        self.commit()
        with patch.dict(os.environ, fixture.git_environment(), clear=True):
            with self.assertRaises(deliver_git.DeliveryError):
                deliver_git.catch_up(self.source, str(self.remote), "master", "TICKET-41", "none")
        self.assertEqual(self.revision("MERGE_HEAD"), integration)
        result = self.before(TASK_BRANCH="master", TASK_BASE_COMMIT=base, ADDED_TEST_COMMAND=self.command())
        self.assert_ok(result)
        self.assertNotIn("test_other.py", result.stdout)
        self.assertEqual(result.stdout.count("INDIVIDUAL TEST RAN"), 2)

    def test_refreshed_workspace_excludes_tests_from_before_the_reply(self):
        import git_workspace
        refreshed = self.root / "refreshed"
        refreshed.mkdir()
        with patch.dict(os.environ, fixture.git_environment(), clear=True):
            self.assertTrue(git_workspace.prepare(refreshed, str(self.remote), "master"))
        old = self.git(refreshed, "rev-parse", "refs/remotes/origin/master").stdout.strip()
        self.write("test_other.py", 19)
        latest = self.commit()
        self.git(self.source, "push", str(self.remote), "master")
        with patch.dict(os.environ, fixture.git_environment(), clear=True), patch.object(git_workspace, "entrance_reply", return_value=1):
            git_workspace.refresh(refreshed, str(self.remote), "master")
        self.assertEqual(self.git(refreshed, "rev-parse", "HEAD").stdout.strip(), latest)
        self.assertEqual(self.git(refreshed, "rev-parse", "refs/remotes/origin/master").stdout.strip(), old)
        (refreshed / "test_ours.py").write_text("print('ONLY THIS REQUEST')\n")
        result = subprocess.run([sys.executable, "-B", str(Path(__file__).with_name("git_workspace.py")), "--",
                                 sys.executable, "-B", str(Path(__file__).with_name("added_tests.py"))],
                                cwd=refreshed, env=dict(fixture.git_environment(), TASK_WORKSPACE=str(refreshed),
                                TASK_REPOSITORY=str(self.remote), TASK_BRANCH="master", ADDED_TEST_COMMAND=self.command()),
                                input="", capture_output=True, text=True, timeout=20)
        self.assert_ok(result)
        self.assertNotIn("test_other.py", result.stdout)
        self.assertIn("ONLY THIS REQUEST", result.stdout)

    def test_completed_command_report_is_flushed_before_the_next_command(self):
        import added_tests
        output = io.StringIO()

        def execute(command, **kwargs):
            if command == ["second"]:
                self.assertIn("FIRST RESULT", output.getvalue())
                raise InterruptedError("process interrupted")
            return 0, "FIRST RESULT", ""

        with redirect_stdout(output), patch.object(added_tests.support, "run", side_effect=execute):
            with self.assertRaises(InterruptedError):
                added_tests.run_commands(self.source, [["first"], ["second"]], added_tests.Report())
        self.assertEqual(output.getvalue().count("FIRST RESULT"), 1)


if __name__ == "__main__":
    unittest.main()
