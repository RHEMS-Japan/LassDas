"""The activity record a role leaves for the runtime to read after a launch
that did not finish. A native SDK stand-in drives the bridge's hooks."""
import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import sys
import tempfile
import types
import unittest
from unittest.mock import patch

import test_hermes_repeated_failures as repeated

spec = importlib.util.spec_from_file_location("hermes_activity_bridge", Path(__file__).with_name("hermes.py"))
bridge = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bridge)


class Registry:
    def __init__(self, sessions=None, error=None):
        self.sessions, self.error = sessions or [], error

    def list_sessions(self):
        if self.error:
            raise self.error
        return self.sessions

    def kill_all(self):
        pass


class ActivityRecordTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory(prefix="activity-")
        self.addCleanup(directory.cleanup)
        self.path = Path(directory.name) / "task-activity.json"

    def read(self):
        return json.loads(self.path.read_text(encoding="utf-8"))

    def test_each_command_rewrites_the_record_with_what_is_running(self):
        registry = Registry([{"command": "cargo build --release", "status": "running"},
                             {"command": "sleep 1", "status": "exited"}])
        activity = bridge.Activity(str(self.path), registry)
        activity.started("call-1", "terminal", {"command": "cargo   build\n--release", "background": True})
        self.assertEqual(self.read(), {"last": "terminal: cargo build --release (in the background)",
                                       "returned": False, "background": ["cargo build --release"]})
        activity.completed("call-1", "terminal", {"command": "cargo build --release"}, "{}")
        self.assertTrue(self.read()["returned"])
        activity.started("call-2", "write_file", {"path": "src/main.rs", "content": "fn main() {}\n" * 500})
        record = self.read()
        self.assertEqual(record["last"], "write_file: src/main.rs")
        self.assertFalse(record["returned"])
        self.assertNotIn("fn main", self.path.read_text(encoding="utf-8"))
        self.assertLess(self.path.stat().st_size, 1000)
        self.assertFalse(self.path.with_name(self.path.name + ".tmp").exists())

    def test_a_long_command_is_cut_and_credentials_never_reach_the_file(self):
        activity = bridge.Activity(str(self.path), Registry([{"command": "x" * 900, "status": "running"}] * 9))
        with patch.dict(os.environ, {"TASK_CREDENTIAL_NAMES": "DEPLOY_TOKEN", "DEPLOY_TOKEN": "synthetic-secret-value"}, clear=True):
            activity.started("call", "terminal", {"command": "curl -H 'token: synthetic-secret-value' " + "y" * 900})
        text = self.path.read_text(encoding="utf-8")
        self.assertNotIn("synthetic-secret-value", text)
        record = json.loads(text)
        self.assertLessEqual(len(record["last"]), bridge.ACTIVITY_CHARACTERS)
        self.assertTrue(record["last"].endswith("…"))
        self.assertEqual(len(record["background"]), bridge.ACTIVITY_BACKGROUND)
        self.assertLess(len(text.encode("utf-8")), 2000)

    def test_a_credential_the_cut_falls_inside_leaves_nothing_of_itself(self):
        # A start no other word in the record has, so any length of it found
        # is the credential's.
        secret = "Kq7Wv" + "Q" * 35
        env = {"TASK_CREDENTIAL_NAMES": "DEPLOY_TOKEN", "DEPLOY_TOKEN": secret}

        def longest_prefix(text):
            return max((n for n in range(1, len(secret) + 1) if secret[:n] in text), default=0)

        for name, command, replaced in (
                # Positive control: the whole credential well inside the cut.
                ("inside the cut", "curl -H 'token: " + secret + "' https://example.invalid", True),
                # Cut first, its first 17 characters (27 in the background
                # list) would stay; taken out first, the line fits whole.
                ("where the cut would fall", "x" * 170 + " " + secret + " tail", True),
                # Here the cut falls inside the replacement itself.
                ("past the cut", "x" * 190 + " " + secret + " tail", False)):
            with self.subTest(name):
                activity = bridge.Activity(str(self.path), Registry([{"command": command, "status": "running"}]))
                with patch.dict(os.environ, env, clear=True):
                    activity.started("call", "terminal", {"command": command})
                text = self.path.read_text(encoding="utf-8")
                record = json.loads(text)
                self.assertEqual(longest_prefix(text), 0, record)
                self.assertEqual("[credential]" in record["last"], replaced, record)
                self.assertLessEqual(len(record["last"]), bridge.ACTIVITY_CHARACTERS)
                self.assertLessEqual(len(record["background"][0]), bridge.ACTIVITY_CHARACTERS)
        # The same holds for the sentence a repeated failure stops the role with.
        stop = bridge.RepeatedFailureStop(repeated.Guardrails(), 2)
        args = {"command": "y" * 280 + " " + secret}
        with patch.dict(os.environ, env, clear=True):
            for _ in range(2):
                stop.after_call("terminal", args, json.dumps({"error": "z" * 290 + secret}), failed=True)
        self.assertTrue(stop.stopped)
        self.assertEqual(longest_prefix(stop.stopped), 0, stop.stopped)

    def test_a_failed_write_leaves_no_older_command_behind(self):
        activity = bridge.Activity(str(self.path), Registry())
        activity.started("c1", "terminal", {"command": "cargo build --release"})
        self.assertEqual(self.read()["last"], "terminal: cargo build --release")
        draft = self.path.with_name(self.path.name + ".tmp")
        draft.mkdir()
        errors = io.StringIO()
        with contextlib.redirect_stderr(errors):
            activity.started("c2", "terminal", {"command": "make -j64"})
            activity.started("c3", "terminal", {"command": "pytest -x"})
        self.assertFalse(self.path.exists(), "the earlier command stayed as the last one")
        self.assertEqual(errors.getvalue().count("Activity record not written"), 1)
        draft.rmdir()
        activity.started("c4", "terminal", {"command": "cargo test"})
        self.assertEqual(self.read()["last"], "terminal: cargo test")

    def test_an_unreadable_process_list_or_unwritable_file_never_stops_the_role(self):
        activity = bridge.Activity(str(self.path), Registry(error=RuntimeError("registry unavailable")))
        activity.started("call", "read_file", {"path": "README.md"})
        self.assertEqual(self.read()["background"], [])
        blocked = bridge.Activity(str(self.path / "below-a-file"), Registry())
        self.path.write_text("{}", encoding="utf-8")
        errors = io.StringIO()
        with contextlib.redirect_stderr(errors):
            blocked.started("call", "terminal", {"command": "true"})
            blocked.completed("call", "terminal", {"command": "true"}, "")
        self.assertEqual(errors.getvalue().count("Activity record not written"), 1)

    def test_without_a_named_file_nothing_is_written(self):
        activity = bridge.Activity(None, Registry())
        activity.started("call", "terminal", {"command": "true"})
        self.assertFalse(self.path.exists())

    def test_a_role_the_tool_call_guardrail_ends_says_which_rule_in_the_record(self):
        runner = repeated.RepeatedFailureTests("test_four_identical_failures_do_not_stop_the_role")
        _, _, _, _, code, _ = runner.run_bridge([repeated.WAIT] * 5, extra_env={"TASK_ACTIVITY": str(self.path)})
        self.assertEqual(code, 1)
        self.assertEqual(self.read()["halted"], {"code": "repeated_identical_failure", "count": 5})
        _, _, _, _, code, _ = runner.run_bridge([repeated.WAIT] * 3, guardrails=repeated.Guardrails(halt_after=2),
                                                extra_env={"TASK_ACTIVITY": str(self.path), "NATIVE_MAX_REPEATED_FAILURES": "0"})
        self.assertEqual(code, 1)
        self.assertEqual(self.read()["halted"], {"code": "same_tool_failure_halt", "count": 2})
        self.path.unlink()
        _, _, out, _, code, _ = runner.run_bridge([repeated.WAIT] * 4 + [repeated.LIST], extra_env={"TASK_ACTIVITY": str(self.path)})
        self.assertEqual((code, out), (0, "finished normally"))
        self.assertFalse(self.path.exists())

    def test_a_halt_is_recorded_after_the_background_is_stopped(self):
        registry = Registry([{"command": "cargo build --release", "status": "running"}])
        registry.kill_all = lambda: registry.sessions.clear()

        class NativeAgent:
            def __init__(self, **kwargs):
                pass

            def run_conversation(self, user_message):
                return {"final_response": "stopped", "failed": False, "turn_exit_reason": "guardrail_halt",
                        "guardrail": {"code": "same_tool_failure_halt", "count": 3, "message": "stop"}}

            def interrupt(self, **_):
                pass

            def close(self):
                pass

        env = {"OPENROUTER_BASE_URL": "https://model.example/api/v1", "OPENROUTER_API_KEY": "synthetic-test-only",
               "NATIVE_MODEL": "maker/test", "HERMES_HOME": str(self.path.parent / "home"),
               "TASK_ACTIVITY": str(self.path)}
        with patch.dict(sys.modules, {"run_agent": types.SimpleNamespace(AIAgent=NativeAgent),
                                     "tools.process_registry": types.SimpleNamespace(process_registry=registry)}), \
             patch.dict(os.environ, env, clear=True), patch.object(sys, "argv", ["bridge"]), \
             patch.object(sys, "stdin", io.StringIO("request")), \
             contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            code = bridge.main()
        self.assertEqual(code, 1)
        self.assertEqual(self.read()["halted"], {"code": "same_tool_failure_halt", "count": 3})
        self.assertEqual(self.read()["background"], [])

    def test_the_bridge_hands_the_hooks_to_the_native_agent(self):
        seen = {}

        class NativeAgent:
            def __init__(self, **kwargs):
                seen.update(kwargs)

            def run_conversation(self, user_message):
                seen["tool_start_callback"]("call-1", "terminal", {"command": "cargo test"})
                return {"final_response": "report"}

            def interrupt(self, **_):
                pass

            def close(self):
                pass

        env = {"OPENROUTER_BASE_URL": "https://model.example/api/v1", "OPENROUTER_API_KEY": "synthetic-test-only",
               "NATIVE_MODEL": "maker/test", "HERMES_HOME": str(self.path.parent / "home"),
               "TASK_ACTIVITY": str(self.path)}
        stdout = io.StringIO()
        with patch.dict(sys.modules, {"run_agent": types.SimpleNamespace(AIAgent=NativeAgent),
                                     "tools.process_registry": types.SimpleNamespace(process_registry=Registry())}), \
             patch.dict(os.environ, env, clear=True), patch.object(sys, "argv", ["bridge"]), \
             patch.object(sys, "stdin", io.StringIO("request")), \
             contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(io.StringIO()):
            code = bridge.main()
        self.assertEqual(code, 0)
        self.assertEqual(stdout.getvalue(), "report")
        self.assertEqual(self.read(), {"last": "terminal: cargo test", "returned": False, "background": []})


if __name__ == "__main__":
    unittest.main()
