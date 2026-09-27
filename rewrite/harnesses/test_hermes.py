"""Bridge tests use a native SDK stand-in, not an LLM-judgment claim."""
import contextlib
import importlib.util
import io
import os
from pathlib import Path
import signal
import sys
import threading
import types
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("hermes_bridge", Path(__file__).with_name("hermes.py"))
bridge = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bridge)


class BridgeTests(unittest.TestCase):
    def run_bridge(self, result=None, error=None, cleanup_error=None, home=True, task_home=None, stop_signal=None, interrupt_error=None, background_cleanup_error=None, reasoning_effort="high"):
        events, stdout, stderr = [], io.StringIO(), io.StringIO()
        interrupted = threading.Event()

        class NativeRegistry:
            def kill_all(self):
                events.append(("backgrounds_closed", True))
                print("native background cleanup display")
                if background_cleanup_error:
                    raise background_cleanup_error

        class NativeAgent:
            def __init__(self, **kwargs):
                events.append(("configuration", kwargs))
                print("native startup display")
                events.append(("dotenv", os.environ.get("PYTHON_DOTENV_DISABLED")))
                events.append(("home", os.environ.get("HERMES_HOME")))

            def run_conversation(self, user_message):
                events.append(("request", user_message))
                print("native tool trace")
                if stop_signal:
                    signal.getsignal(stop_signal)(stop_signal, None)
                    if not interrupted.wait(1):
                        raise RuntimeError("native interrupt did not reach the tool loop")
                if error:
                    raise error
                return result

            def interrupt(self, *, hard_cancel=False):
                events.append(("interrupted", hard_cancel))
                interrupted.set()
                if interrupt_error:
                    raise interrupt_error

            def close(self):
                events.append(("closed", True))
                print("native cleanup display")
                if cleanup_error:
                    raise cleanup_error

        env = {"OPENROUTER_BASE_URL": "https://model.example/api/v1",
               "OPENROUTER_API_KEY": "synthetic-test-only", "NATIVE_MODEL": "maker/test",
               "NATIVE_MAX_TOKENS": "7000"}
        if reasoning_effort is not None:
            env["NATIVE_REASONING_EFFORT"] = reasoning_effort
        if home:
            env["HERMES_HOME"] = "/isolated-test/role"
        if task_home:
            env["TASK_HOME"] = str(task_home)
        failure, code = None, None
        with patch.dict(sys.modules, {"run_agent": types.SimpleNamespace(AIAgent=NativeAgent),
                                     "tools.process_registry": types.SimpleNamespace(process_registry=NativeRegistry())}), \
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
        self.assertEqual(events[-2], ("backgrounds_closed", True))
        self.assertEqual(events[-1], ("closed", True))
        config = events[0][1]
        self.assertEqual(config["enabled_toolsets"], ["terminal", "file"])
        self.assertEqual(config["model"], "maker/test")
        self.assertEqual(config["max_tokens"], 7000)
        self.assertEqual(config["reasoning_config"], {"effort": "high"})
        self.assertTrue(config["skip_background_review"])

    def test_background_cleanup_failure_still_closes_agent_and_keeps_report(self):
        previous = {sig: signal.getsignal(sig) for sig in (signal.SIGTERM, signal.SIGINT)}
        events, out, err, code, failure = self.run_bridge(
            {"final_response": "Actual work remains visible"}, background_cleanup_error=RuntimeError("background cleanup failed"))
        self.assertEqual(out, "Actual work remains visible")
        self.assertEqual(str(failure), "background cleanup failed")
        self.assertEqual(events[-2:], [("backgrounds_closed", True), ("closed", True)])
        for sig, handler in previous.items():
            self.assertEqual(signal.getsignal(sig), handler)

    def test_native_failure_keeps_partial_report_and_reason(self):
        events, out, err, code, failure = self.run_bridge(
            {"final_response": "Only half completed", "failed": True, "error": "transport unavailable"})
        self.assertIsNone(failure)
        self.assertEqual(code, 1)
        self.assertEqual(out, "Only half completed")
        self.assertIn("transport unavailable", err)
        self.assertEqual(events[-1], ("closed", True))

    def test_reasoning_setting_is_an_explicit_openrouter_request_override(self):
        for setting, expected in ((None, "low"), ("low", "low"), ("high", "high")):
            events, out, err, code, failure = self.run_bridge({"final_response": "plain report"}, reasoning_effort=setting)
            self.assertIsNone(failure)
            self.assertEqual(code, 0)
            self.assertEqual(out, "plain report")
            config = events[0][1]
            self.assertEqual(config["reasoning_config"], {"effort": expected})
            self.assertEqual(config.get("request_overrides"), {"extra_body": {"reasoning": {"effort": expected}}})

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

    def test_task_home_takes_precedence_over_a_global_home(self):
        with tempfile.TemporaryDirectory() as directory:
            task_home = Path(directory) / "request-role-home"
            events, out, err, code, failure = self.run_bridge({"final_response": "ordinary result"}, task_home=task_home)
            self.assertIsNone(failure)
            self.assertTrue(task_home.is_dir())
            self.assertIn(("home", str(task_home)), events)
            self.assertEqual(out, "ordinary result")

    def test_stop_signals_use_native_hard_interrupt_and_restore_handlers(self):
        for number in (signal.SIGTERM, signal.SIGINT):
            previous = {sig: signal.getsignal(sig) for sig in (signal.SIGTERM, signal.SIGINT)}
            events, out, err, code, failure = self.run_bridge({"final_response": "Actual partial work"}, stop_signal=number)
            self.assertIsNone(failure)
            self.assertEqual(code, 128 + number)
            self.assertIn(("interrupted", True), events)
            self.assertEqual(events[-2:], [("backgrounds_closed", True), ("closed", True)])
            self.assertEqual(out, "Actual partial work")
            for sig, handler in previous.items():
                self.assertEqual(signal.getsignal(sig), handler)

    def test_native_interrupt_error_is_visible_and_never_a_success_exit(self):
        events, out, err, code, failure = self.run_bridge({"final_response": "Unfinished"}, stop_signal=signal.SIGTERM, interrupt_error=RuntimeError("native cancellation unavailable"))
        self.assertIsNone(failure)
        self.assertNotEqual(code, 0)
        self.assertEqual(out, "Unfinished")
        self.assertIn("native cancellation unavailable", err)
        self.assertEqual(events[-1], ("closed", True))

    def test_native_interrupt_with_no_final_response_is_not_a_type_error(self):
        events, out, err, code, failure = self.run_bridge({"final_response": None, "failed": True, "error": "Interrupted by caller"}, stop_signal=signal.SIGTERM)
        self.assertIsNone(failure)
        self.assertEqual(code, 143)
        self.assertEqual(out, "")
        self.assertIn("Interrupted by caller", err)
        self.assertEqual(events[-1], ("closed", True))


if __name__ == "__main__":
    unittest.main()
