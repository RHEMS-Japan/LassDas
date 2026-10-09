"""Stdin bridge to an installed Hermes SDK, not a second agent loop.

Run this inside the role's filesystem/network isolation. Make the installed
``run_agent`` module importable in that process; no personal installation path
or model shortlist belongs here.
"""
from collections.abc import Mapping
import contextlib
import dataclasses
import hashlib
import json
import os
from pathlib import Path
import signal
import sys
import threading


class WithoutKeyLines:
    """The SDK's own display prints a masked form of the key it was given
    ("sk-…" with the ends visible). Even the ends are a credential's, so the
    lines that name a key are dropped before anything reaches stderr; every
    other line passes through untouched."""

    def __init__(self, stream):
        self.stream = stream
        self.pending = ""

    def write(self, text):
        self.pending += text
        while "\n" in self.pending:
            line, self.pending = self.pending.split("\n", 1)
            if "API key" not in line and "api_key" not in line:
                self.stream.write(line + "\n")
        return len(text)

    def flush(self):
        if self.pending and "API key" not in self.pending and "api_key" not in self.pending:
            self.stream.write(self.pending)
            self.pending = ""
        self.stream.flush()

    def __getattr__(self, name):
        return getattr(self.stream, name)


def credentials():
    """The values this process must never leave on disk: every credential the
    runtime handed it (named in TASK_CREDENTIAL_NAMES) and the two this bridge
    knows by itself."""
    names = set(os.environ.get("TASK_CREDENTIAL_NAMES", "").split(":")) | {"OPENROUTER_API_KEY", "TASK_TRACKER_KEY"}
    return [os.environ[name] for name in sorted(names) if name and os.environ.get(name)]


def scrub(text, values):
    for value in values:
        text = text.replace(value, "[credential]")
    return text


class RepeatedFailureStop:
    """Ends the role when one tool call fails the same way a number of times
    in a row.

    The SDK warns the model about a call that keeps failing unchanged, but it
    does not stop one unless its own hard stop is configured, and that one
    counts every earlier failure of the call however much else happened in
    between, so a check that fails again after an edit counts towards it too.
    A role once repeated a call that could not succeed for as long as it ran,
    while a build it had started in the background used up the memory.

    This keeps the SDK's per-turn tool-call guardrail and adds one rule to it.
    The guardrail sees every call the SDK ran, with its unaltered result and
    the SDK's own verdict on whether it failed. The same tool with the same
    arguments failing with the same result `limit` times in a row ends the
    turn through the SDK's existing guardrail halt: no further model call is
    made. Any other call in between, or a success, starts the count again.
    """

    def __init__(self, guardrails, limit):
        self.guardrails = guardrails
        self.limit = limit
        self.last = None
        self.count = 0
        self.stopped = None
        self.broken = False

    def __getattr__(self, name):
        # Anything else the SDK reads is the guardrail's own.
        if name == "guardrails":
            raise AttributeError(name)
        return getattr(self.guardrails, name)

    def reset_for_turn(self):
        self.guardrails.reset_for_turn()
        self.last, self.count = None, 0

    def before_call(self, tool_name, args):
        return self.guardrails.before_call(tool_name, args)

    def after_call(self, tool_name, args, result, *, failed=None):
        decision = self.guardrails.after_call(tool_name, args, result, failed=failed)
        try:
            return self.observe(tool_name, args, result, failed, decision)
        except Exception as error:
            # Counting must never take a tool result away from the SDK.
            if not self.broken:
                self.broken = True
                print(f"\nRepeated-failure count unavailable, the role continues: {error}", file=sys.stderr)
            return decision

    def observe(self, tool_name, args, result, failed, decision):
        # The SDK always passes its verdict; without one, nothing is counted.
        if not failed:
            self.last, self.count = None, 0
            return decision
        arguments = json.dumps(args if isinstance(args, Mapping) else {}, ensure_ascii=False,
                               sort_keys=True, separators=(",", ":"), default=str)
        text = result if isinstance(result, str) else json.dumps(result, ensure_ascii=False, sort_keys=True, default=str)
        call = (tool_name, arguments, hashlib.sha256(text.encode("utf-8", "surrogatepass")).hexdigest())
        self.count = self.count + 1 if call == self.last else 1
        self.last = call
        if self.count < self.limit or getattr(decision, "should_halt", False):
            return decision
        halt = dataclasses.replace(
            decision, action="halt", code="repeated_identical_failure", count=self.count,
            message=(f"{tool_name} failed {self.count} times in a row with the same arguments and "
                     "the same result. The role ends here."))
        # Recorded only once the SDK has a halt to act on. Credentials come out
        # before anything is cut: a cut one would leave its start behind.
        values = credentials()
        self.stopped = scrub(f"Stopped: the same failure repeated {self.count} times in a row: "
                             f"{tool_name} {clip(scrub(arguments, values))}: {failure_summary(scrub(text, values))}", values)
        return halt


def clip(text, limit=300):
    text = " ".join(text.split())
    return text if len(text) <= limit else text[:limit] + "…"


def failure_summary(text):
    """The error a tool result names, or the start of the result."""
    try:
        data = json.loads(text)
    except ValueError:
        data = None
    if isinstance(data, dict):
        if data.get("error"):
            return clip(str(data["error"]))
        if data.get("exit_code") is not None:
            return clip(f"exit {data['exit_code']}: {data.get('output', '')}")
    return clip(text)


# What the activity record keeps of one command, and how many background
# processes it names. The record is rewritten at every command, so it stays a
# few hundred bytes.
ACTIVITY_CHARACTERS = 200
ACTIVITY_BACKGROUND = 5
# The arguments that say what a tool was asked to do, in the order they are
# looked for; a tool without any of them is named alone.
ACTIVITY_ARGUMENTS = ("command", "path", "file_path", "pattern", "query", "url", "action", "session_id")


def command_summary(name, args):
    """One line naming the tool and what it was asked to do, never the whole
    argument record: a file tool's content stays out of it."""
    summary = str(name or "unknown tool")
    if isinstance(args, dict):
        details = [str(args[key]) for key in ACTIVITY_ARGUMENTS if args.get(key) not in (None, "")]
        if details:
            summary += ": " + " ".join(details)
        if args.get("background"):
            summary += " (in the background)"
    return activity_text(summary)


def activity_text(text):
    """A command as the record keeps it: credentials taken out first, then cut,
    so a credential the cut falls inside leaves nothing of itself."""
    return clip(scrub(text, credentials()), ACTIVITY_CHARACTERS - 1)


class Activity:
    """Keeps TASK_ACTIVITY, the small file the runtime reads after a launch
    that did not finish: the command the native agent started last, whether it
    had returned, and the background processes it had running. A forced exit
    of the whole runtime leaves no other trace of what the role was doing, so
    the file is rewritten at every command start and end. Credentials are
    taken out before it is written; a failure to write never stops the role."""

    def __init__(self, path, registry):
        self.path = Path(path) if path else None
        self.registry = registry
        self.lock = threading.Lock()
        self.last, self.call, self.returned = "", None, True
        self.halt = None
        self.said = False

    def started(self, call_id, name, args, *_):
        with self.lock:
            self.last, self.call, self.returned = command_summary(name, args), call_id, False
            self.write()

    def completed(self, call_id, name, args, *_):
        with self.lock:
            if call_id == self.call:
                self.returned = True
            self.write()

    def halted(self, code, count):
        """The tool-call guardrail ended the role: which rule, and for a call
        that kept failing the same way, how many times in a row."""
        with self.lock:
            self.halt = {"code": str(code or "unknown"), "count": count if isinstance(count, int) else 0}
            self.write()

    def background(self):
        try:
            sessions = self.registry.list_sessions()
        except Exception:
            return []
        running = [activity_text(str(session.get("command", ""))) for session in sessions
                   if isinstance(session, dict) and session.get("status") == "running" and session.get("command")]
        return running[:ACTIVITY_BACKGROUND]

    def write(self):
        if self.path is None:
            return
        record = {"last": self.last, "returned": self.returned, "background": self.background()}
        if self.halt:
            record["halted"] = self.halt
        text = scrub(json.dumps(record, ensure_ascii=False), credentials())
        try:
            draft = self.path.with_name(self.path.name + ".tmp")
            draft.write_text(text, encoding="utf-8")
            os.replace(draft, self.path)
        except Exception as error:
            # An earlier command left in place would read as the last one, so
            # the record goes (removing needs no space); the runtime then says
            # the last command is unknown, and a later write that succeeds
            # puts it back. Said once: the role still works.
            for path in (self.path, self.path.with_name(self.path.name + ".tmp")):
                with contextlib.suppress(Exception):
                    if not path.is_dir():
                        path.unlink(missing_ok=True)
            if not self.said:
                self.said = True
                print(f"Activity record not written: {error}", file=sys.stderr)


def save_transcript(result):
    """Keep the whole conversation the native agent had, for reading afterwards,
    and take the credentials out of what the native agent logged by itself.

    The report has already been published; nothing here can take it back.
    """
    values = credentials()
    home = Path(os.environ["HERMES_HOME"])
    messages = result.get("messages") if isinstance(result, dict) else None
    if isinstance(messages, list):
        try:
            text = scrub(json.dumps(messages, ensure_ascii=False, indent=1, default=str), values)
            (home / "transcript.json").write_text(text, encoding="utf-8")
        except Exception as error:
            print(f"Transcript not saved: {error}", file=sys.stderr)
    for name in ("agent.log", "errors.log"):
        path = home / "logs" / name
        try:
            if path.is_file():
                text = path.read_text(encoding="utf-8", errors="replace")
                if any(value in text for value in values):
                    path.write_text(scrub(text, values), encoding="utf-8")
        except Exception as error:
            print(f"{name} not scrubbed: {error}", file=sys.stderr)


def main():
    # Never discover the operator's personal agent home or dotenv credentials.
    # A separate home per role also keeps parallel reviewers' sessions apart.
    if os.environ.get("TASK_HOME"):
        # Watch mode supplies a different home per request and process. Never
        # reuse a global native home merely because the operator had one set.
        os.environ["HERMES_HOME"] = os.environ["TASK_HOME"]
        Path(os.environ["TASK_HOME"]).mkdir(parents=True, exist_ok=True, mode=0o700)
    if not os.environ.get("HERMES_HOME"):
        raise RuntimeError("Set HERMES_HOME to this role's isolated agent directory")
    if os.environ.get("TASK_WORKSPACE"):
        # The native terminal may default to its home, not the process cwd.
        # Seed its existing setting before SDK imports/configuration. An
        # explicit native directory remains the operator's choice.
        os.environ.setdefault("TERMINAL_CWD", os.environ["TASK_WORKSPACE"])
    os.environ["PYTHON_DOTENV_DISABLED"] = "1"
    # SDK bookkeeping only tracks patch/write_file, not later terminal writes.
    # Its optional footer can therefore falsely claim a recovered file was not
    # changed. Use its existing switch, not a filter on the model's prose.
    # An explicit operator override remains authoritative.
    os.environ.setdefault("HERMES_FILE_MUTATION_VERIFIER", "0")
    repeated_failure_limit = max(0, int(os.environ.get("NATIVE_MAX_REPEATED_FAILURES", "5")))
    with contextlib.redirect_stdout(sys.stderr):
        from run_agent import AIAgent
        from tools.process_registry import process_registry
    if sys.argv[1:] == ["--check-import"]:
        print("native agent import available")
        return 0

    prompt = sys.stdin.read()
    reasoning = {"effort": os.environ.get("NATIVE_REASONING_EFFORT", "low")}
    activity = Activity(os.environ.get("TASK_ACTIVITY"), process_registry)
    sys.stderr = WithoutKeyLines(sys.stderr)
    with contextlib.redirect_stdout(sys.stderr):
        agent = AIAgent(
            base_url=os.environ["OPENROUTER_BASE_URL"],
            api_key=os.environ["OPENROUTER_API_KEY"],
            provider="openrouter", model=os.environ["NATIVE_MODEL"],
            enabled_toolsets=["terminal", "file"],
            # Each tool call and result preview is printed as it happens; with
            # stdout redirected they reach stderr, where the runtime's live
            # copy shows them while the role works. The report alone goes to
            # stdout. The preview length is the operator's setting.
            quiet_mode=False, tool_progress_mode="all",
            log_prefix_chars=int(os.environ.get("NATIVE_LOG_PREFIX_CHARS", "2000")),
            reasoning_config=reasoning,
            # This bridge targets OpenRouter, including an explicitly supplied
            # relay. Native URL heuristics can omit reasoning for a relay host;
            # use the SDK's request override so the chosen setting reaches it.
            request_overrides={"extra_body": {"reasoning": reasoning}},
            max_tokens=int(os.environ.get("NATIVE_MAX_TOKENS", "32000")),
            # The native agent stops after a number of model calls; the
            # operator may set one, and none is set otherwise.
            max_iterations=max(0, int(os.environ.get("NATIVE_MAX_TURNS", "0"))) or 1_000_000_000,
            skip_context_files=True, skip_memory=True, skip_background_review=True,
            # The SDK's own hooks for a tool call's start and end; they only
            # record what is running, for the runtime to read after a crash.
            tool_start_callback=activity.started, tool_complete_callback=activity.completed,
        )
    repeated_failures = None
    if repeated_failure_limit:
        guardrails = getattr(agent, "_tool_guardrails", None)
        if callable(getattr(guardrails, "after_call", None)):
            repeated_failures = RepeatedFailureStop(guardrails, repeated_failure_limit)
            agent._tool_guardrails = repeated_failures
        else:
            print("\nThe native agent has no tool-call guardrail to extend: "
                  "a call that keeps failing the same way will not stop this role", file=sys.stderr)

    # Native terminal commands can have their own process groups. Give the
    # installed agent its existing hard-interrupt path before closing it.
    # The signal handler only records intent: calling SDK locks from a Python
    # signal handler can deadlock if that same thread already holds a lock.
    stop_signal = 0
    finished = threading.Event()

    def request_stop(number, _frame):
        nonlocal stop_signal
        stop_signal = number

    previous = {number: signal.signal(number, request_stop)
                for number in (signal.SIGTERM, signal.SIGINT)}

    def interrupt_tools():
        while not finished.wait(0.02):
            if stop_signal:
                try:
                    agent.interrupt(hard_cancel=True)
                except Exception as error:
                    print(f"Native interruption failed: {error}", file=sys.stderr)
                return

    interrupter = threading.Thread(target=interrupt_tools, daemon=True)
    interrupter.start()
    try:
        with contextlib.redirect_stdout(sys.stderr):
            result = agent.run_conversation(user_message=prompt)
        # The conversation is over, so no tool is left to interrupt; retire the
        # interrupter before the report is written, so nothing it might print
        # can follow the report on stdout.
        finished.set()
        interrupter.join(timeout=1)
        # A turn the SDK's tool-call guardrail ended did not finish the role's
        # work, whichever rule stopped it. What the role started in the
        # background is stopped first: it may be what was using up the machine.
        halted, rule = None, None
        guardrail = result.get("guardrail")
        if repeated_failures is not None and repeated_failures.stopped:
            halted = repeated_failures.stopped
            rule = ("repeated_identical_failure", repeated_failures.count)
        elif guardrail or result.get("turn_exit_reason") == "guardrail_halt":
            details = guardrail if isinstance(guardrail, dict) else {}
            halted = scrub(f"Stopped by the native agent's tool-call guardrail ({details.get('code', 'unknown')}): "
                           f"{details.get('message', '')}", credentials())
            rule = (details.get("code"), details.get("count"))
        if halted:
            try:
                with contextlib.redirect_stdout(sys.stderr):
                    process_registry.kill_all()
            except Exception as error:
                print(f"\nBackground processes not stopped: {error}", file=sys.stderr)
            # Recorded once the background is stopped, so the record does
            # not name processes that are no longer running.
            activity.halted(*rule)
        # No stripping, clipping, classification, JSON parsing or approval test.
        # Preserve partial work even when the native run failed, and publish the
        # report before cleanup so a cleanup error cannot erase it.
        response = result.get("final_response")
        if response is not None:
            sys.stdout.write(response)
        if halted:
            sys.stdout.write(("\n\n" if (response or "").strip() else "") + halted + "\n")
        sys.stdout.flush()
        if result.get("error"):
            print(result["error"], file=sys.stderr)
        if halted:
            print("\n" + halted, file=sys.stderr)
        # A run that ends without a report has not done the role's work: the
        # native agent gives up, for one, on an answer that stays cut off at
        # NATIVE_MAX_TOKENS after its continuations. Say so and fail, so the
        # runtime retries the role instead of passing an empty result on. The
        # sentence starts on a line of its own: the credential filter drops a
        # whole line, and the SDK's last write may not have ended one.
        signalled = stop_signal
        empty = not signalled and not halted and not result.get("failed") and not (response or "").strip()
        if empty:
            print("\nNative agent ended without a report (an answer cut off at NATIVE_MAX_TOKENS is one cause)", file=sys.stderr)
        elif not signalled and result.get("failed") and not result.get("error"):
            print("\nNative agent reported failure without a reason", file=sys.stderr)
        save_transcript(result)
        return 128 + signalled if signalled else (1 if result.get("failed") or empty or halted else 0)
    finally:
        finished.set()
        interrupter.join(timeout=1)
        # Keep the original failed-attempt reasons, including on cancellation
        # or an SDK exception. These are not observations of the final file.
        # This optional SDK metadata must never prevent native tool teardown.
        try:
            failed = getattr(agent, "_turn_failed_file_mutations", None)
            if isinstance(failed, dict):
                for path, attempt in failed.items():
                    if isinstance(attempt, dict):
                        print("Native file tool attempt failed (earlier attempt; current file state not checked): "
                              f"[{attempt.get('tool', 'unknown')}] {path!r}: {attempt.get('error_preview', '')}",
                              file=sys.stderr)
        except Exception as error:
            print(f"Native file-attempt diagnostics unavailable: {error}", file=sys.stderr)
        try:
            with contextlib.redirect_stdout(sys.stderr):
                try:
                    # This one-shot process owns one agent and its registry.
                    # Native local tools may be registered under a shared
                    # environment id, not the session id used by close().
                    # Use native teardown, not a second process-tree walker.
                    process_registry.kill_all()
                finally:
                    agent.close()
        finally:
            for number, handler in previous.items():
                signal.signal(number, handler)


if __name__ == "__main__":
    raise SystemExit(main())
