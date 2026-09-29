"""Stdin bridge to an installed Hermes SDK, not a second agent loop.

Run this inside the role's filesystem/network isolation. Make the installed
``run_agent`` module importable in that process; no personal installation path
or model shortlist belongs here.
"""
import contextlib
import json
import os
from pathlib import Path
import signal
import sys
import threading


def save_transcript(result):
    """Keep the whole conversation the native agent had, for reading afterwards.

    The report has already been published; nothing here can take it back. The
    credentials this process was given are replaced before the text is written.
    """
    messages = result.get("messages") if isinstance(result, dict) else None
    if not isinstance(messages, list):
        return
    try:
        text = json.dumps(messages, ensure_ascii=False, indent=1, default=str)
        for name in ("OPENROUTER_API_KEY", "TASK_TRACKER_KEY"):
            value = os.environ.get(name)
            if value:
                text = text.replace(value, "[credential]")
        (Path(os.environ["HERMES_HOME"]) / "transcript.json").write_text(text, encoding="utf-8")
    except Exception as error:
        print(f"Transcript not saved: {error}", file=sys.stderr)


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
    with contextlib.redirect_stdout(sys.stderr):
        from run_agent import AIAgent
        from tools.process_registry import process_registry
    if sys.argv[1:] == ["--check-import"]:
        print("native agent import available")
        return 0

    prompt = sys.stdin.read()
    reasoning = {"effort": os.environ.get("NATIVE_REASONING_EFFORT", "low")}
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
            max_tokens=int(os.environ.get("NATIVE_MAX_TOKENS", "6000")),
            skip_context_files=True, skip_memory=True, skip_background_review=True,
        )

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
        # No stripping, clipping, classification, JSON parsing or approval test.
        # Preserve partial work even when the native run failed, and publish the
        # report before cleanup so a cleanup error cannot erase it.
        response = result.get("final_response")
        if response is not None:
            sys.stdout.write(response)
        sys.stdout.flush()
        if result.get("error"):
            print(result["error"], file=sys.stderr)
        save_transcript(result)
        return 128 + stop_signal if stop_signal else (1 if result.get("failed") else 0)
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
