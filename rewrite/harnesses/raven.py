#!/usr/bin/env python3
"""Bridge from the runtime's role contract to Raven-Code's one-turn CLI hosting.

The contract is the one hermes.py keeps: the instruction arrives on stdin, the
role works in TASK_WORKSPACE, the report leaves on stdout and nothing else
does, diagnostics go to stderr with every credential value removed, and a run
that commits no report exits 1 so the runtime retries the role.

Raven itself is not imported: the bridge renders a configuration from the
product's own (agents/raven-code/config.json under RAVEN_ROOT) and hands it,
with the task, to the product's launcher on a Python that can run it
(RAVEN_PYTHON). The launcher owns the session, the transcript and the
working-tree footprint; the bridge owns what the runtime cares about.

Environment:
  OPENROUTER_BASE_URL, OPENROUTER_API_KEY, NATIVE_MODEL     the endpoint and model (as for hermes.py)
  NATIVE_MAX_TOKENS (32000), NATIVE_MAX_TURNS (none), NATIVE_REASONING_EFFORT (low)
  TASK_WORKSPACE (cwd), TASK_HOME (cwd/.raven)              where the work and the state live
  RAVEN_ROOT (/opt/raven/src), RAVEN_PYTHON (/opt/raven/venv/bin/python)
  TASK_CREDENTIAL_NAMES                                     names of variables never to echo

The skill-evolution pipeline and the memory backend are switched off in the
rendered configuration: a role must do the same thing tonight as it did last
night, and it must not write anywhere but its workspace and its state.
"""

import json
import os
import signal
import subprocess
import sys
import threading
import time
from pathlib import Path

DISABLED_TOOLS = [
    "spawn", "message", "hub", "deep_research", "image_generate", "text_to_speech", "video_generate",
    "understand_media", "create_playbook", "deliver_files", "load_playbook", "plugin", "run_subagent_dag",
    "cron", "browser_click", "browser_navigate", "browser_press", "browser_screenshot", "browser_scroll",
    "browser_snapshot", "browser_type", "browser_hover", "browser_select",
]


def credentials():
    """Every credential value in this process's environment: the ones the
    runtime handed it (named in TASK_CREDENTIAL_NAMES) and the bridge's own."""
    names = set(os.environ.get("TASK_CREDENTIAL_NAMES", "").split(":")) | {"OPENROUTER_API_KEY", "CODE_API_KEY"}
    return sorted({value for name in names if name for value in [os.environ.get(name, "")] if value}, key=len, reverse=True)


def scrub(text, values):
    for value in values:
        text = text.replace(value, "[credential]")
    return text


def rendered_config(root, model, base_url):
    """The product's own configuration with this launch's endpoint, model and
    limits, and with self-evolution and memory off."""
    source = root / "agents" / "raven-code" / "config.json"
    config = json.loads(source.read_text(encoding="utf-8"))
    providers = config.setdefault("providers", {})
    providers["custom"] = {"apiBase": base_url, "models": [model]}
    defaults = config.setdefault("agents", {}).setdefault("defaults", {})
    defaults.update({
        "model": model, "provider": "custom",
        "reasoningEffort": os.environ.get("NATIVE_REASONING_EFFORT", "low"),
        "maxTokens": int(os.environ.get("NATIVE_MAX_TOKENS", "32000")),
        "maxToolIterations": max(0, int(os.environ.get("NATIVE_MAX_TURNS", "0"))) or 1_000_000,
        "llmCallTimeout": 3600,
    })
    tools = config.setdefault("tools", {})
    tools["restrictToWorkspace"] = False
    tools["sandbox"] = {"backend": "none"}
    tools.setdefault("exec", {})["timeout"] = 3600
    tools["disabledTools"] = sorted(set(tools.get("disabledTools", [])) | set(DISABLED_TOOLS))
    config["skillForge"] = {**config.get("skillForge", {}), "enabled": False, "autoDetect": False, "autoEvolve": False}
    config["memory"] = {**config.get("memory", {}), "backend": None}
    return config


def sweep_renders(state, values):
    """Remove the launcher's rendered configurations, which hold the key in
    plain text: the launcher deletes its own when it ends, but a launcher
    that was killed does not, and this directory is shown to people. The
    launcher's log is scrubbed the same way."""
    for rendered in state.rglob(".config.rendered.*.json"):
        try:
            rendered.unlink()
        except OSError as error:
            print(f"Raven bridge: rendered configuration not removed: {error}", file=sys.stderr)
    for log in state.rglob("launcher.log"):
        try:
            text = log.read_text(encoding="utf-8", errors="replace")
            cleaned = scrub(text, values)
            if cleaned != text:
                log.write_text(cleaned, encoding="utf-8")
        except OSError as error:
            print(f"Raven bridge: launcher log not scrubbed: {error}", file=sys.stderr)


def main():
    task = sys.stdin.read()
    if not task.strip():
        print("Raven bridge: no instruction on stdin", file=sys.stderr)
        return 2
    root = Path(os.environ.get("RAVEN_ROOT", "/opt/raven/src"))
    python = os.environ.get("RAVEN_PYTHON", "/opt/raven/venv/bin/python")
    launcher = root / "agents" / "raven-code" / "run.py"
    for required in ("OPENROUTER_BASE_URL", "OPENROUTER_API_KEY", "NATIVE_MODEL"):
        if not os.environ.get(required):
            print(f"Raven bridge: {required} is not set", file=sys.stderr)
            return 2
    if not launcher.is_file() or not Path(python).exists():
        print(f"Raven bridge: launcher {launcher} or interpreter {python} is missing", file=sys.stderr)
        return 2
    workspace = Path(os.environ.get("TASK_WORKSPACE") or os.getcwd()).resolve()
    home = Path(os.environ.get("TASK_HOME") or workspace / ".raven").resolve()
    state = home / "raven"
    state.mkdir(parents=True, exist_ok=True)
    (state / "task.md").write_text(task, encoding="utf-8")
    config_path = state / "config.json"
    config_path.write_text(json.dumps(rendered_config(root, os.environ["NATIVE_MODEL"], os.environ["OPENROUTER_BASE_URL"].rstrip("/")), indent=2), encoding="utf-8")
    os.chmod(config_path, 0o600)
    raven_home = state / "home"
    raven_home.mkdir(exist_ok=True)
    job = f"role-{int(time.time())}-{os.getpid()}"
    environment = dict(os.environ)
    environment.update({
        "CODE_API_KEY": os.environ["OPENROUTER_API_KEY"],
        "CODE_STATE_ROOT": str(state / "sessions"),
        "RAVEN_HOME": str(raven_home),
        "HOME": str(home),
        "PYTHONDONTWRITEBYTECODE": "1",
    })
    values = credentials()
    sweep_renders(state, values)
    # --verbose mirrors the launcher's diagnostics to stderr; relay() below
    # passes them on as they arrive, scrubbed.
    argv = [python, str(launcher), "--prompt-file", str(state / "task.md"), "--workspace", str(workspace),
            "--job", job, "--config", str(config_path), "--timeout", "0", "--verbose"]
    started = time.time()
    # A stop from the runtime reaches the bridge first; hand it to the launcher
    # and wait, so the launcher's own cleanup can run before the sweep below.
    child = subprocess.Popen(argv, cwd=str(workspace), env=environment, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    def forward(number, _frame):
        try:
            child.send_signal(number)
        except OSError:
            pass
    previous = {number: signal.signal(number, forward) for number in (signal.SIGTERM, signal.SIGINT)}

    # The launcher's diagnostics are relayed line by line as they arrive, so
    # the runtime's live copy shows a working role, not a finished one.
    def relay():
        for line in child.stderr:
            print(scrub(line.rstrip("\n"), values), file=sys.stderr, flush=True)

    relaying = threading.Thread(target=relay, daemon=True)
    relaying.start()
    try:
        out = child.stdout.read()
        child.wait()
    finally:
        relaying.join(timeout=5)
        for number, handler in previous.items():
            signal.signal(number, handler)
        sweep_renders(state, values)
    elapsed = int(time.time() - started)
    answer = scrub(out or "", values)
    print(f"Raven bridge: launcher exited {child.returncode} after {elapsed}s (job {job})", file=sys.stderr)
    if child.returncode != 0:
        # What the launcher says about a failed turn is a diagnostic, not the
        # role's report: it names its log by path and commits no answer.
        for line in answer.splitlines():
            print("Raven bridge: launcher said: " + line, file=sys.stderr)
        if child.returncode < 0:
            return 128 - child.returncode
        return child.returncode
    sys.stdout.write(answer)
    sys.stdout.flush()
    if not answer.strip():
        print("Raven bridge: the launcher committed no report", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
