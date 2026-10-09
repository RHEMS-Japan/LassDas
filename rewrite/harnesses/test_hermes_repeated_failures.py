"""A call that keeps failing the same way ends the role.

The stand-in implements the SDK boundary the bridge extends, as the pinned
SDK has it: the agent keeps a per-turn tool-call guardrail in
``_tool_guardrails``, resets it when the turn starts, passes every call it ran
to ``after_call`` with the unaltered result and its own failure verdict, and
ends the turn without a further model call when a returned decision halts,
with the halt sentence as the final response and the decision's metadata under
``guardrail``. Real model behaviour is not tested here.
"""
import contextlib
import dataclasses
import importlib.util
import io
import json
import os
from pathlib import Path
import sys
import types
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("hermes_bridge", Path(__file__).with_name("hermes.py"))
bridge = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bridge)

WAIT = ("process", {"action": "wait"}, json.dumps({"error": "session_id is required for wait"}), True)
LIST = ("process", {"action": "list"}, json.dumps({"processes": []}), False)
EDIT = ("patch", {"path": "src/lib.rs", "old_string": "a", "new_string": "b"}, json.dumps({"success": True}), False)


@dataclasses.dataclass(frozen=True)
class Decision:
    """The SDK's guardrail decision: a frozen dataclass whose block or halt
    action ends the turn."""
    action: str = "allow"
    code: str = "allow"
    message: str = ""
    tool_name: str = ""
    count: int = 0
    signature: object = None

    @property
    def should_halt(self):
        return self.action in {"block", "halt"}

    def to_metadata(self):
        return {"action": self.action, "code": self.code, "message": self.message,
                "tool_name": self.tool_name, "count": self.count}


class Guardrails:
    """The SDK's default guardrail: it warns about a repeated failure and,
    unless its own hard stop is configured (``halt_after``), never stops one."""

    def __init__(self, halt_after=None):
        self.halt_after = halt_after
        self.failures = 0
        self.resets = 0

    def reset_for_turn(self):
        self.resets += 1
        self.failures = 0

    def before_call(self, tool_name, args):
        return Decision(tool_name=tool_name)

    def after_call(self, tool_name, args, result, *, failed=None):
        if not failed:
            return Decision(tool_name=tool_name)
        self.failures += 1
        if self.halt_after and self.failures >= self.halt_after:
            return Decision(action="halt", code="same_tool_failure_halt", tool_name=tool_name, count=self.failures,
                            message=f"Stopped {tool_name}: it failed {self.failures} times this turn.")
        return Decision(action="warn", code="repeated_exact_failure_warning", tool_name=tool_name,
                        count=self.failures, message=f"{tool_name} has failed {self.failures} times.")


class RepeatedFailureTests(unittest.TestCase):
    def run_bridge(self, calls, guardrails=None, extra_env=None, no_guardrail=False):
        events, stdout, stderr = [], io.StringIO(), io.StringIO()
        state = {"guardrails": guardrails or Guardrails()}

        class NativeRegistry:
            def kill_all(self):
                events.append(("backgrounds_closed", len(stdout.getvalue())))

        class NativeAgent:
            def __init__(self, **kwargs):
                events.append(("configuration", kwargs))
                if not no_guardrail:
                    self._tool_guardrails = state["guardrails"]

            def run_conversation(self, user_message):
                guard = getattr(self, "_tool_guardrails", None)
                state["installed"] = guard
                if guard is not None:
                    guard.reset_for_turn()
                for name, args, result, failed in calls:
                    events.append(("tool", name, args))
                    if guard is None:
                        continue
                    decision = guard.after_call(name, args, result, failed=failed)
                    events.append(("decision", decision))
                    if decision.should_halt:
                        return {"final_response": f"I stopped retrying {name} because it hit the tool-call guardrail "
                                                  f"({decision.code}) after {decision.count} repeated non-progressing attempts.",
                                "failed": False, "turn_exit_reason": "guardrail_halt",
                                "guardrail": decision.to_metadata()}
                return {"final_response": "finished normally", "failed": False, "turn_exit_reason": "text_response"}

            def interrupt(self, *, hard_cancel=False):
                events.append(("interrupted", hard_cancel))

            def close(self):
                events.append(("closed", True))

        env = {"OPENROUTER_BASE_URL": "https://model.example/api/v1", "OPENROUTER_API_KEY": "synthetic-test-only",
               "NATIVE_MODEL": "maker/test", "HERMES_HOME": "/isolated-test/role"}
        env.update(extra_env or {})
        failure, code = None, None
        with patch.dict(sys.modules, {"run_agent": types.SimpleNamespace(AIAgent=NativeAgent),
                                     "tools.process_registry": types.SimpleNamespace(process_registry=NativeRegistry())}), \
             patch.dict(os.environ, env, clear=True), patch.object(sys, "argv", ["bridge"]), \
             patch.object(sys, "stdin", io.StringIO("request")), \
             contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            try:
                code = bridge.main()
            except BaseException as exception:
                failure = exception
        self.assertIsNone(failure)
        ran = [event[1:] for event in events if event[0] == "tool"]
        return events, ran, stdout.getvalue(), stderr.getvalue(), code, state

    def test_the_fifth_identical_failure_ends_the_role_and_says_what_failed(self):
        events, ran, out, err, code, state = self.run_bridge([WAIT] * 12)
        self.assertEqual(len(ran), 5)
        self.assertEqual(code, 1)
        stop = 'Stopped: the same failure repeated 5 times in a row: process {"action":"wait"}: session_id is required for wait'
        self.assertTrue(out.startswith("I stopped retrying process because it hit the tool-call guardrail "
                                       "(repeated_identical_failure) after 5"), out)
        self.assertTrue(out.endswith("\n\n" + stop + "\n"), out)
        self.assertIn(stop, err)
        self.assertEqual(events[-1], ("closed", True))

    def test_four_identical_failures_do_not_stop_the_role(self):
        events, ran, out, err, code, state = self.run_bridge([WAIT] * 4 + [LIST])
        self.assertEqual(len(ran), 5)
        self.assertEqual((code, out), (0, "finished normally"))
        self.assertNotIn("Stopped", err)
        # Below the limit the SDK's own decision reaches it unchanged: its
        # warning still tells the model the call is looping.
        decisions = [event[1] for event in events if event[0] == "decision"]
        self.assertEqual([d.code for d in decisions[:4]], ["repeated_exact_failure_warning"] * 4)

    def test_any_other_call_or_result_in_between_starts_the_count_again(self):
        other_wait = ("process", {"action": "wait", "timeout": 30}, WAIT[2], True)
        other_error = ("process", {"action": "wait"}, json.dumps({"error": "a different reason"}), True)
        failing_check = ("terminal", {"command": "cargo test"}, json.dumps({"output": "1 failed", "exit_code": 101, "error": None}), True)
        for between in (other_wait, other_error, LIST, EDIT):
            with self.subTest(between=between[1]):
                calls = [WAIT] * 4 + [between] + [WAIT] * 4 + [between] + [WAIT] * 4
                events, ran, out, err, code, state = self.run_bridge(calls)
                self.assertEqual(len(ran), len(calls))
                self.assertEqual((code, out), (0, "finished normally"))
        # A check that fails again after each edit is not a loop.
        calls = [failing_check, EDIT] * 8
        events, ran, out, err, code, state = self.run_bridge(calls)
        self.assertEqual((len(ran), code), (16, 0))

    def test_background_processes_are_stopped_before_the_report_is_written(self):
        events, ran, out, err, code, state = self.run_bridge([WAIT] * 5)
        closed = [event[1] for event in events if event[0] == "backgrounds_closed"]
        # First before any report byte, then again by the ordinary teardown.
        self.assertEqual(closed[0], 0)
        self.assertEqual(len(closed), 2)
        self.assertEqual(code, 1)

    def test_a_role_that_finishes_stops_its_backgrounds_only_at_teardown(self):
        events, ran, out, err, code, state = self.run_bridge([WAIT] * 2 + [LIST])
        closed = [event[1] for event in events if event[0] == "backgrounds_closed"]
        self.assertEqual(closed, [len("finished normally")])
        self.assertEqual(code, 0)

    def test_the_limit_is_the_operators_setting_and_zero_turns_it_off(self):
        events, ran, out, err, code, state = self.run_bridge([WAIT] * 12, extra_env={"NATIVE_MAX_REPEATED_FAILURES": "2"})
        self.assertEqual((len(ran), code), (2, 1))
        self.assertIn("repeated 2 times in a row", out)
        for off in ("0", "-3"):
            guardrails = Guardrails()
            events, ran, out, err, code, state = self.run_bridge([WAIT] * 12, guardrails=guardrails,
                                                                 extra_env={"NATIVE_MAX_REPEATED_FAILURES": off})
            self.assertEqual((len(ran), code, out), (12, 0, "finished normally"))
            self.assertIs(state["installed"], guardrails)

    def test_the_sdk_guardrail_is_kept_and_its_own_halt_is_a_failure(self):
        guardrails = Guardrails(halt_after=3)
        events, ran, out, err, code, state = self.run_bridge([WAIT, EDIT, WAIT, EDIT, WAIT, EDIT], guardrails=guardrails)
        self.assertIsNot(state["installed"], guardrails)
        self.assertEqual(guardrails.resets, 1)
        self.assertEqual((len(ran), code), (5, 1))
        self.assertIn("Stopped by the native agent's tool-call guardrail (same_tool_failure_halt): "
                      "Stopped process: it failed 3 times this turn.", err)
        closed = [event[1] for event in events if event[0] == "backgrounds_closed"]
        self.assertEqual(closed[0], 0)

    def test_an_agent_without_the_guardrail_says_so_and_runs_as_before(self):
        events, ran, out, err, code, state = self.run_bridge([WAIT] * 8, no_guardrail=True)
        self.assertEqual((len(ran), code, out), (8, 0, "finished normally"))
        self.assertIn("no tool-call guardrail to extend", err)

    def test_a_counting_error_never_takes_a_result_away_from_the_agent(self):
        class LooseGuardrails(Guardrails):
            def after_call(self, tool_name, args, result, *, failed=None):
                return types.SimpleNamespace(should_halt=False, code="warn")
        events, ran, out, err, code, state = self.run_bridge([WAIT] * 7, guardrails=LooseGuardrails())
        self.assertEqual((len(ran), code, out), (7, 0, "finished normally"))
        self.assertEqual(err.count("Repeated-failure count unavailable, the role continues"), 1)

    def test_the_stop_sentence_names_a_terminal_failure_and_holds_no_credential(self):
        command = {"command": "curl -H 'Authorization: synthetic-test-only' https://example.invalid"}
        result = json.dumps({"output": "curl: (6) Could not resolve host\n", "exit_code": 6, "error": None})
        events, ran, out, err, code, state = self.run_bridge([("terminal", command, result, True)] * 5)
        self.assertEqual(code, 1)
        self.assertIn("Stopped: the same failure repeated 5 times in a row: terminal "
                      "{\"command\":\"curl -H 'Authorization: [credential]' https://example.invalid\"}: "
                      "exit 6: curl: (6) Could not resolve host", out)
        self.assertNotIn("synthetic-test-only", out + err)


if __name__ == "__main__":
    unittest.main()
