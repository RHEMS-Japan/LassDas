"""Stdin bridge to an installed Hermes SDK, not a second agent loop.

Run this inside the role's filesystem/network isolation. Make the installed
``run_agent`` module importable in that process; no personal installation path
or model shortlist belongs here.
"""
import contextlib
import os
import sys


def main():
    # Never discover the operator's personal agent home or dotenv credentials.
    # A separate home per role also keeps parallel reviewers' sessions apart.
    if not os.environ.get("HERMES_HOME"):
        raise RuntimeError("Set HERMES_HOME to this role's isolated agent directory")
    os.environ["PYTHON_DOTENV_DISABLED"] = "1"
    with contextlib.redirect_stdout(sys.stderr):
        from run_agent import AIAgent
    if sys.argv[1:] == ["--check-import"]:
        print("native agent import available")
        return 0

    prompt = sys.stdin.read()
    with contextlib.redirect_stdout(sys.stderr):
        agent = AIAgent(
            base_url=os.environ["OPENROUTER_BASE_URL"],
            api_key=os.environ["OPENROUTER_API_KEY"],
            provider="openrouter", model=os.environ["NATIVE_MODEL"],
            enabled_toolsets=["terminal", "file"], quiet_mode=True,
            tool_progress_mode="off",
            reasoning_config={"effort": os.environ.get("NATIVE_REASONING_EFFORT", "low")},
            max_tokens=int(os.environ.get("NATIVE_MAX_TOKENS", "6000")),
            skip_context_files=True, skip_memory=True, skip_background_review=True,
        )
    try:
        with contextlib.redirect_stdout(sys.stderr):
            result = agent.run_conversation(user_message=prompt)
        # No stripping, clipping, classification, JSON parsing or approval test.
        # Preserve partial work even when the native run failed, and publish the
        # report before cleanup so a cleanup error cannot erase it.
        sys.stdout.write(result.get("final_response", ""))
        sys.stdout.flush()
        if result.get("error"):
            print(result["error"], file=sys.stderr)
        return 1 if result.get("failed") else 0
    finally:
        with contextlib.redirect_stdout(sys.stderr):
            agent.close()


if __name__ == "__main__":
    raise SystemExit(main())
