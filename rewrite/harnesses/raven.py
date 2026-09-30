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
import subprocess
import sys
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
    return sorted({value for name in names if name for value in [os.environ.get(name, "")] if len(value) >= 8}, key=len, reverse=True)


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
        "maxToolIterations": int(os.environ.get("NATIVE_MAX_TURNS", "0")) or 1_000_000,
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
    argv = [python, str(launcher), "--prompt-file", str(state / "task.md"), "--workspace", str(workspace),
            "--job", job, "--config", str(config_path), "--timeout", "0"]
    started = time.time()
    process = subprocess.run(argv, cwd=str(workspace), env=environment, capture_output=True, text=True)
    elapsed = int(time.time() - started)
    for line in (process.stderr or "").splitlines():
        print(scrub(line, values), file=sys.stderr)
    answer = scrub(process.stdout or "", values)
    sys.stdout.write(answer)
    sys.stdout.flush()
    print(f"Raven bridge: launcher exited {process.returncode} after {elapsed}s (job {job})", file=sys.stderr)
    if process.returncode != 0:
        return process.returncode
    if not answer.strip():
        print("Raven bridge: the launcher committed no report", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
