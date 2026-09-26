"""Bridge tests use a native SDK stand-in, not an LLM-judgment claim."""
import contextlib
import importlib.util
import io
import os
from pathlib import Path
import sys
import types
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("hermes_bridge", Path(__file__).with_name("hermes.py"))
bridge = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bridge)


class BridgeTests(unittest.TestCase):
    def run_bridge(self, result=None, error=None, cleanup_error=None, home=True):
        events, stdout, stderr = [], io.StringIO(), io.StringIO()

        class NativeAgent:
            def __init__(self, **kwargs):
                events.append(("configuration", kwargs))
                print("native startup display")
                events.append(("dotenv", os.environ.get("PYTHON_DOTENV_DISABLED")))

            def run_conversation(self, user_message):
                events.append(("request", user_message))
                print("native tool trace")
                if error:
                    raise error
                return result

            def close(self):
                events.append(("closed", True))
                print("native cleanup display")
                if cleanup_error:
                    raise cleanup_error

        env = {"OPENROUTER_BASE_URL": "https://model.example/api/v1",
               "OPENROUTER_API_KEY": "synthetic-test-only", "NATIVE_MODEL": "maker/test",
               "NATIVE_REASONING_EFFORT": "high", "NATIVE_MAX_TOKENS": "7000"}
        if home:
            env["HERMES_HOME"] = "/isolated-test/role"
        failure, code = None, None
        with patch.dict(sys.modules, {"run_agent": types.SimpleNamespace(AIAgent=NativeAgent)}), \
             patch.dict(os.environ, env, clear=True), patch.object(sys, "argv", ["bridge"]), \
             patch.object(sys, "stdin", io.StringIO("original\n依頼\nunchanged")), \
             contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            try:
                code = bridge.main()
            except BaseException as exception:
                failure = exception
        return events, stdout.getvalue(), stderr.getvalue(), code, failure

    def test_full_prose_and_original_request_without_ui_or_format_inspection(self):
        prose = '  Unusual introduction\n{"unknown": true}\n直していない点もそのまま。\n' + "long report " * 2000
        events, out, err, code, failure = self.run_bridge({"final_response": prose})
        self.assertIsNone(failure)
        self.assertEqual(code, 0)
        self.assertEqual(out, prose)
        for diagnostic in ("startup", "tool trace", "cleanup"):
            self.assertIn(diagnostic, err)
        self.assertIn(("request", "original\n依頼\nunchanged"), events)
        self.assertIn(("dotenv", "1"), events)
        self.assertEqual(events[-1], ("closed", True))
        config = events[0][1]
        self.assertEqual(config["enabled_toolsets"], ["terminal", "file"])
        self.assertEqual(config["model"], "maker/test")
        self.assertEqual(config["max_tokens"], 7000)
        self.assertEqual(config["reasoning_config"], {"effort": "high"})
        self.assertTrue(config["skip_background_review"])

    def test_native_failure_keeps_partial_report_and_reason(self):
        events, out, err, code, failure = self.run_bridge(
            {"final_response": "Only half completed", "failed": True, "error": "transport unavailable"})
        self.assertIsNone(failure)
        self.assertEqual(code, 1)
        self.assertEqual(out, "Only half completed")
        self.assertIn("transport unavailable", err)
        self.assertEqual(events[-1], ("closed", True))

    def test_native_exception_still_closes_agent(self):
        events, out, err, code, failure = self.run_bridge(error=RuntimeError("connection stopped"))
        self.assertIsInstance(failure, RuntimeError)
        self.assertEqual(str(failure), "connection stopped")
        self.assertEqual(out, "")
        self.assertEqual(events[-1], ("closed", True))

    def test_cleanup_failure_does_not_erase_completed_report(self):
        events, out, err, code, failure = self.run_bridge(
            {"final_response": "Real work happened."}, cleanup_error=RuntimeError("cleanup failed"))
        self.assertEqual(out, "Real work happened.")
        self.assertEqual(str(failure), "cleanup failed")

    def test_missing_isolated_home_does_not_start_native_agent(self):
        events, out, err, code, failure = self.run_bridge(home=False)
        self.assertEqual(events, [])
        self.assertIsInstance(failure, RuntimeError)
        self.assertIn("HERMES_HOME", str(failure))


if __name__ == "__main__":
    unittest.main()
