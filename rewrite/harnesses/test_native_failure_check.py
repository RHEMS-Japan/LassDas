"""The image check must reject a silent loss of the SDK failure observation."""
import importlib.util
from pathlib import Path
from subprocess import CompletedProcess
import unittest

SCRIPT = Path(__file__).resolve().parents[2] / ".github/scripts/check-native-repeated-failures.py"
spec = importlib.util.spec_from_file_location("native_failure_check", SCRIPT)
check = importlib.util.module_from_spec(spec)
spec.loader.exec_module(check)


class NativeFailureCheckTests(unittest.TestCase):
    def test_both_image_workflows_check_the_sdk_before_publication(self):
        root = SCRIPT.parents[2]
        invocation = "bash .github/scripts/check-native-repeated-failures.sh"
        for name in ("image-check.yml", "image.yml"):
            with self.subTest(workflow=name):
                workflow = (root / ".github/workflows" / name).read_text()
                self.assertEqual(workflow.count(invocation), 1)
                if name == "image.yml":
                    self.assertLess(workflow.index(invocation), workflow.index("docker push"))

    def test_an_ineffective_or_off_by_one_stop_and_an_unrelated_error_fail_ci(self):
        stop = "Stopped: the same failure repeated 5 times in a row: process"
        for code, calls, output in ((0, 13, "Script finished."), (1, 0, "import failed"),
                                    (1, 6, stop), (1, 5, "authentication failed")):
            with self.subTest(code=code, calls=calls, output=output):
                with self.assertRaises(AssertionError):
                    check.verify_run(CompletedProcess([], code, output, output), calls, 12, True)
        check.verify_run(CompletedProcess([], 1, stop, stop), 5, 12, True)

    def test_controls_must_reach_the_scripted_end_without_a_false_stop(self):
        for failures, enabled in ((4, True), (6, False)):
            with self.subTest(failures=failures, enabled=enabled):
                check.verify_run(CompletedProcess([], 0, "Script finished.", ""), failures + 1, failures, enabled)
                with self.assertRaises(AssertionError):
                    check.verify_run(CompletedProcess([], 1, "stopped early", ""), failures, failures, enabled)


if __name__ == "__main__":
    unittest.main()
