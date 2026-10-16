"""Exercise CI selection and its final status without GitHub or long suites."""
import os
from pathlib import Path
import re
import subprocess
import tempfile
import textwrap
import unittest

ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / ".github/scripts/test-harness-group.sh"
WORKFLOW = ROOT / ".github/workflows/ci.yml"


@unittest.skipUnless(WORKFLOW.is_file(), "requires repository CI workflow")
class CIPartitionTests(unittest.TestCase):
    def fixture(self, root):
        script = root / ".github/scripts/test-harness-group.sh"
        script.parent.mkdir(parents=True)
        script.write_bytes(SCRIPT.read_bytes())
        harnesses = root / "rewrite/harnesses"
        harnesses.mkdir(parents=True)
        for name in ("adversarial_review", "a_future", "without_host_identity", "z_future"):
            (harnesses / ("test_" + name + ".py")).write_text(textwrap.dedent(f"""\
                import os
                from pathlib import Path
                import unittest
                class Fixture(unittest.TestCase):
                    def test_selected(self):
                        with Path(os.environ['SELECTION_LOG']).open('a') as out:
                            out.write('{name}\\n')
                        self.assertNotEqual(os.environ.get('FAIL_SELECTED'), '{name}')
                """))
        return script, harnesses

    def test_every_file_runs_in_exactly_one_group_and_failures_propagate(self):
        with tempfile.TemporaryDirectory(prefix="ci-groups-") as temporary:
            root = Path(temporary)
            script, _ = self.fixture(root)
            for group, names in (("adversarial", ["adversarial_review"]),
                                 ("remaining", ["a_future", "without_host_identity", "z_future"])):
                for failure in ("", names[0]):
                    with self.subTest(group=group, failure=failure):
                        log = root / (group + (failure or "pass") + ".log")
                        env = dict(os.environ, SELECTION_LOG=str(log), FAIL_SELECTED=failure)
                        result = subprocess.run(["bash", str(script), group], cwd=root,
                                                env=env, capture_output=True, text=True, timeout=10)
                        self.assertEqual(result.returncode == 0, not failure, result.stderr)
                        self.assertEqual(sorted(log.read_text().splitlines()), names)

    def test_skipped_tests_are_counted_by_reason(self):
        with tempfile.TemporaryDirectory(prefix="ci-skips-") as temporary:
            root = Path(temporary)
            script, harnesses = self.fixture(root)
            (harnesses / "test_needs_tools.py").write_text(textwrap.dedent("""\
                import unittest
                class Needs(unittest.TestCase):
                    @unittest.skip("no frobnicator is installed")
                    def test_one(self):
                        pass
                    @unittest.skip("no frobnicator is installed")
                    def test_two(self):
                        pass
                    @unittest.skip("only on another platform")
                    def test_three(self):
                        pass
                """))
            env = dict(os.environ, SELECTION_LOG=str(root / "selection.log"), FAIL_SELECTED="")
            result = subprocess.run(["bash", str(script), "remaining"], cwd=root, env=env,
                                    capture_output=True, text=True, timeout=10)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("OK (skipped=3)", result.stderr)
            lines = result.stderr.splitlines()
            self.assertEqual([line for line in lines if line.startswith("skipped ")],
                             ["skipped 2: no frobnicator is installed", "skipped 1: only on another platform"])

    def test_a_group_where_no_test_runs_exits_5_as_unittest_does(self):
        """python3 -m unittest exits 5 when no test ran, unless some were
        skipped (Python 3.12); the group script does the same."""
        cases = (
            ("a TestCase with no test", "import unittest\nclass Nothing(unittest.TestCase):\n    pass\n", 5),
            ("no TestCase", "VALUE = 1\n", 5),
            ("every test skipped", textwrap.dedent("""\
                import unittest
                class T(unittest.TestCase):
                    @unittest.skip("no tool")
                    def test_a(self):
                        pass
                """), 0),
        )
        for label, source, status in cases:
            with self.subTest(label), tempfile.TemporaryDirectory(prefix="ci-no-tests-") as temporary:
                root = Path(temporary)
                script, harnesses = self.fixture(root)
                (harnesses / "test_adversarial_review.py").write_text(source)
                result = subprocess.run(["bash", str(script), "adversarial"], cwd=root,
                                        capture_output=True, text=True, timeout=10)
                self.assertEqual(result.returncode, status, result.stderr)

    def test_tests_import_the_files_beside_them_from_another_directory(self):
        """python3 -m unittest puts the tests' directory first on sys.path by
        its full name, so a test that changes directory still imports the
        files beside it; an empty first entry would follow the change."""
        with tempfile.TemporaryDirectory(prefix="ci-sys-path-") as temporary:
            root = Path(temporary)
            script, harnesses = self.fixture(root)
            (harnesses / "beside_helper.py").write_text("VALUE = 1\n")
            (harnesses / "test_adversarial_review.py").write_text(textwrap.dedent("""\
                import os
                import tempfile
                import unittest
                class Beside(unittest.TestCase):
                    def test_import_after_changing_directory(self):
                        here = os.getcwd()
                        with tempfile.TemporaryDirectory() as elsewhere:
                            os.chdir(elsewhere)
                            try:
                                import beside_helper
                            finally:
                                os.chdir(here)
                        self.assertEqual(beside_helper.VALUE, 1)
                """))
            result = subprocess.run(["bash", str(script), "adversarial"], cwd=root,
                                    capture_output=True, text=True, timeout=10)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("Ran 1 test", result.stderr)

    def test_empty_unknown_or_missing_groups_never_report_success(self):
        with tempfile.TemporaryDirectory(prefix="ci-empty-group-") as temporary:
            root = Path(temporary)
            script, harnesses = self.fixture(root)
            (harnesses / "test_adversarial_review.py").unlink()
            for args in ([], ["unknown"], ["remaining", "extra"], ["adversarial"]):
                with self.subTest(args=args):
                    result = subprocess.run(["bash", str(script), *args], cwd=root,
                                            capture_output=True, text=True, timeout=10)
                    self.assertNotEqual(result.returncode, 0)

    def test_required_status_depends_on_go_and_both_python_groups(self):
        workflow = WORKFLOW.read_text()
        jobs = dict(re.findall(r"^  ([\w-]+):\n(.*?)(?=^  [\w-]+:|\Z)", workflow, re.M | re.S))
        aggregate = jobs["role-chain"]
        self.assertIn("needs: [role-chain-go, role-chain-python]", aggregate)
        self.assertIn("if: ${{ always() }}", aggregate)
        python = jobs["role-chain-python"]
        self.assertIn("group: [adversarial, remaining]", python)
        self.assertIn("fail-fast: false", python)
        self.assertIn('run: bash ../.github/scripts/test-harness-group.sh "$HARNESS_GROUP"', python)
        self.assertIn("HARNESS_GROUP: ${{ matrix.group }}", python)
        self.assertNotIn("continue-on-error:", aggregate + python)
        self.assertIn("GO_RESULT: ${{ needs['role-chain-go'].result }}", aggregate)
        self.assertIn("PYTHON_RESULT: ${{ needs['role-chain-python'].result }}", aggregate)
        command = textwrap.dedent(aggregate.split("        run: |\n", 1)[1])
        for go in ("success", "failure", "cancelled", "skipped"):
            for py in ("success", "failure", "cancelled", "skipped"):
                with self.subTest(go=go, python=py):
                    result = subprocess.run(["bash", "-euo", "pipefail", "-c", command],
                                            env=dict(os.environ, GO_RESULT=go, PYTHON_RESULT=py),
                                            capture_output=True, text=True, timeout=5)
                    self.assertEqual(result.returncode == 0, go == py == "success")


if __name__ == "__main__":
    unittest.main()
