"""Run the shipped report-confirmation script, not a rewritten equivalent."""
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import textwrap
import unittest

TEMPLATE = Path(__file__).resolve().parents[2] / "deploy/ticket-engine/operator-scripts-configmap.yaml.example"


class ConfirmReportTests(unittest.TestCase):
    def run_check(self, comments, report="The requested change is available.\n", status=0, body=None):
        with tempfile.TemporaryDirectory(prefix="confirm-report-") as temporary:
            root = Path(temporary)
            block = re.search(r"(?m)^  confirm-report: \|\n((?:    [^\n]*(?:\n|$)|\n)+)", TEMPLATE.read_text())
            self.assertIsNotNone(block, "the shipped report check is missing")
            script = root / "confirm-report.py"
            script.write_text(textwrap.dedent(block.group(1)))
            workspace = root / "workspace"
            workspace.mkdir()
            if report is not None:
                (workspace / "report").mkdir()
                (workspace / "report/result.md").write_text(report)
            fixture = root / "comments.json"
            fixture.write_text(json.dumps(comments) if body is None else body)
            calls = root / "calls.json"
            tool = root / "tracker"
            tool.write_text("#!" + sys.executable + "\n" + textwrap.dedent("""\
                import json, os, sys
                from pathlib import Path
                Path(os.environ['FIXTURE_CALLS']).write_text(json.dumps(sys.argv[1:]))
                sys.stdout.write(Path(os.environ['FIXTURE_BODY']).read_text())
                if os.environ['FIXTURE_STATUS'] != '0':
                    sys.stderr.write('fixture read failed')
                sys.exit(int(os.environ['FIXTURE_STATUS']))
                """))
            tool.chmod(0o755)
            environment = {"PATH": os.environ["PATH"], "PYTHONDONTWRITEBYTECODE": "1",
                           "TASK_WORKSPACE": str(workspace), "TRACKER_TOOL": str(tool),
                           "TASK_TRACKER_URL": "https://scope.example/v2", "TASK_TRACKER_ISSUE": "TICKET-7",
                           "FIXTURE_CALLS": str(calls), "FIXTURE_BODY": str(fixture), "FIXTURE_STATUS": str(status)}
            result = subprocess.run([sys.executable, "-B", str(script)], env=environment,
                                    capture_output=True, text=True, timeout=10)
            arguments = json.loads(calls.read_text()) if calls.exists() else None
            if arguments is not None:
                self.assertEqual(arguments, ["--base-url", "https://scope.example/v2", "--key-env", "TASK_TRACKER_KEY",
                                             "--cert-env", "TASK_TRACKER_CERT", "--issue", "TICKET-7", "comments"])
            return result, arguments

    def test_a_matching_report_can_precede_model_stage_or_restart_notices(self):
        report = "The requested change is available."
        notices = [{"content": "A model was selected."}, {"content": "The confirmation stage began."},
                   {"content": "The controller restarted."}]
        for after in range(len(notices) + 1):
            with self.subTest(after=after):
                comments = [{"content": "An earlier question."}, {"content": report}] + notices[:after]
                result, _ = self.run_check(comments)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn("%d after it" % after, result.stdout)

    def test_latest_matching_copy_is_found_and_outer_whitespace_is_ignored(self):
        result, _ = self.run_check([{"content": "The requested change is available."},
                                   {"content": "A later event."},
                                   {"content": "\nThe requested change is available.\n"},
                                   {"content": "Another event."}])
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("4 comments on the issue, 1 after it", result.stdout)

    def test_missing_or_different_report_never_passes(self):
        for comments in ([], [{"content": "A different report."}], [{"content": None}],
                         [{"content": "A prefix: The requested change is available."}]):
            with self.subTest(comments=comments):
                result, _ = self.run_check(comments)
                self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        result, calls = self.run_check([{"content": "irrelevant"}], report=None)
        self.assertEqual(result.returncode, 1)
        self.assertIn("no report/result.md", result.stdout)
        self.assertIsNone(calls)

    def test_failed_read_or_unreadable_reply_is_not_a_confirmation(self):
        for status, body in ((1, json.dumps([{"content": "The requested change is available."}])),
                             (0, "not JSON"), (0, "")):
            with self.subTest(status=status, body=body):
                result, _ = self.run_check([], status=status, body=body)
                self.assertEqual(result.returncode, 1, result.stdout + result.stderr)

    def test_empty_report_is_not_confirmed_by_an_empty_or_contentless_comment(self):
        for report in ("", " \n\t", "\u3000\n"):
            for content in (None, "", " \n\t"):
                with self.subTest(report=report, content=content):
                    result, calls = self.run_check([{"content": content}], report=report)
                    self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
                    self.assertIn("report/result.md is empty", result.stdout)
                    self.assertIsNone(calls, "an empty local report needs no tracker request")


if __name__ == "__main__":
    unittest.main()
