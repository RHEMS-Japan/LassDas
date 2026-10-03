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
