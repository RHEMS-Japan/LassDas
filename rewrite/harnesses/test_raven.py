"""The Raven bridge keeps the role contract without running Raven: the launcher
is a stub script recorded by these tests."""

import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

BRIDGE = Path(__file__).with_name("raven.py")

STUB_LAUNCHER = r'''#!/usr/bin/env python3
import json, os, sys
args = sys.argv[1:]
config = json.loads(open(args[args.index("--config") + 1]).read())
task = open(args[args.index("--prompt-file") + 1]).read()
workspace = args[args.index("--workspace") + 1]
open(os.path.join(workspace, "hello.txt"), "w").write("hello\n")
record = {"args": args, "config": config, "task": task, "env": {k: os.environ.get(k) for k in ("CODE_API_KEY", "CODE_STATE_ROOT", "RAVEN_HOME", "HOME")}}
open(os.environ["STUB_RECORD"], "w").write(json.dumps(record))
mode = os.environ.get("STUB_MODE", "report")
state = os.environ["CODE_STATE_ROOT"]
os.makedirs(state, exist_ok=True)
open(os.path.join(state, ".config.rendered.4242.json"), "w").write(json.dumps({"providers": {"custom": {"apiKey": os.environ["CODE_API_KEY"]}}}))
open(os.path.join(state, "launcher.log"), "w").write("[run] key " + os.environ["CODE_API_KEY"] + " seen\n")
if mode == "report":
    sys.stdout.write("Created hello.txt. The key was " + os.environ["CODE_API_KEY"] + "\n")
    sys.stderr.write("diagnostic mentioning " + os.environ["CODE_API_KEY"] + "\n")
    sys.exit(0)
if mode == "empty":
    sys.exit(0)
sys.stdout.write("FAILED: nothing committed. See /somewhere/launcher.log\n")
sys.stderr.write("launcher failed\n")
sys.exit(3)
'''


class RavenBridgeTests(unittest.TestCase):
    def setUp(self):
        self.root = Path(tempfile.mkdtemp())
        product = self.root / "src" / "agents" / "raven-code"
        product.mkdir(parents=True)
        (product / "run.py").write_text(STUB_LAUNCHER, encoding="utf-8")
        (product / "config.json").write_text(json.dumps({
            "providers": {"custom": {"apiBase": "https://openrouter.example/api/v1", "models": ["vendor/original"]}},
            "agents": {"defaults": {"model": "vendor/original", "provider": "custom", "maxTokens": 1000, "maxToolIterations": 5}},
            "tools": {"disabledTools": ["spawn"], "exec": {"timeout": 600}},
            "skillForge": {"enabled": True, "autoEvolve": True},
            "memory": {"backend": "everos"},
        }), encoding="utf-8")
        self.workspace = self.root / "work"
        self.workspace.mkdir()
        self.home = self.root / "home"
        self.record = self.root / "record.json"

    def run_bridge(self, task="do the thing", mode="report", **extra):
        env = {k: v for k, v in os.environ.items() if not k.startswith(("NATIVE_", "OPENROUTER_", "TASK_", "RAVEN_"))}
        env.update({
            "OPENROUTER_BASE_URL": "https://gateway.example/v1/", "OPENROUTER_API_KEY": "synthetic-gateway-token-1234",
            "NATIVE_MODEL": "prefix/vendor/model", "NATIVE_MAX_TOKENS": "7000", "NATIVE_MAX_TURNS": "0",
            "TASK_WORKSPACE": str(self.workspace), "TASK_HOME": str(self.home),
            "TASK_CREDENTIAL_NAMES": "OPENROUTER_API_KEY:TASK_TRACKER_KEY", "TASK_TRACKER_KEY": "synthetic-tracker-token-5678",
            "RAVEN_ROOT": str(self.root / "src"), "RAVEN_PYTHON": sys.executable,
            "STUB_RECORD": str(self.record), "STUB_MODE": mode,
        })
        env.update(extra)
        done = subprocess.run([sys.executable, str(BRIDGE)], input=task, capture_output=True, text=True, env=env)
        return done.returncode, done.stdout, done.stderr

    def test_the_launcher_gets_the_task_the_workspace_the_endpoint_and_no_self_evolution(self):
        code, out, err = self.run_bridge()
        self.assertEqual(code, 0, err)
        record = json.loads(self.record.read_text())
        self.assertEqual(record["task"], "do the thing")
        self.assertEqual(record["args"][record["args"].index("--workspace") + 1], str(self.workspace.resolve()))
        self.assertEqual(record["args"][record["args"].index("--timeout") + 1], "0")
        self.assertIn("--verbose", record["args"])
        config = record["config"]
        self.assertEqual(config["providers"]["custom"], {"apiBase": "https://gateway.example/v1", "models": ["prefix/vendor/model"]})
        self.assertEqual(config["agents"]["defaults"]["model"], "prefix/vendor/model")
        self.assertEqual(config["agents"]["defaults"]["maxTokens"], 7000)
        self.assertEqual(config["agents"]["defaults"]["maxToolIterations"], 1_000_000)
        self.assertFalse(config["skillForge"]["enabled"])
        self.assertFalse(config["skillForge"]["autoEvolve"])
        self.assertIsNone(config["memory"]["backend"])
        self.assertIn("run_subagent_dag", config["tools"]["disabledTools"])
        self.assertIn("spawn", config["tools"]["disabledTools"])
        self.assertEqual(record["env"]["CODE_API_KEY"], "synthetic-gateway-token-1234")
        self.assertTrue(record["env"]["CODE_STATE_ROOT"].startswith(str(self.home.resolve())))
        self.assertTrue(record["env"]["RAVEN_HOME"].startswith(str(self.home.resolve())))
        self.assertEqual((self.workspace / "hello.txt").read_text(), "hello\n")

    def test_the_report_reaches_stdout_with_every_credential_removed(self):
        code, out, err = self.run_bridge()
        self.assertEqual(code, 0)
        self.assertEqual(out, "Created hello.txt. The key was [credential]\n")
        self.assertIn("diagnostic mentioning [credential]", err)
        self.assertNotIn("synthetic-gateway-token", out + err)
        self.assertIn("launcher exited 0", err)

    def test_a_turn_cap_from_the_operator_is_passed_on(self):
        self.run_bridge(NATIVE_MAX_TURNS="40")
        self.assertEqual(json.loads(self.record.read_text())["config"]["agents"]["defaults"]["maxToolIterations"], 40)
        self.run_bridge(NATIVE_MAX_TURNS="-5")
        self.assertEqual(json.loads(self.record.read_text())["config"]["agents"]["defaults"]["maxToolIterations"], 1_000_000)

    def test_the_launchers_rendered_key_and_log_do_not_outlive_the_run(self):
        self.run_bridge()
        state = self.home / "raven" / "sessions"
        self.assertEqual(list(state.rglob(".config.rendered.*.json")), [])
        self.assertEqual((state / "launcher.log").read_text(), "[run] key [credential] seen\n")
        # a rendered configuration left by an earlier, killed launcher goes at the next start
        leftover = state / ".config.rendered.99.json"
        leftover.write_text("{}")
        self.run_bridge()
        self.assertFalse(leftover.exists())

    def test_a_short_credential_is_scrubbed_too(self):
        code, out, err = self.run_bridge(OPENROUTER_API_KEY="tiny")
        self.assertEqual(out, "Created hello.txt. The key was [credential]\n")
        self.assertNotIn("tiny", err)

    def test_no_report_is_a_failure_and_a_failed_launcher_keeps_its_code(self):
        code, out, err = self.run_bridge(mode="empty")
        self.assertEqual(code, 1)
        self.assertEqual(out, "")
        self.assertIn("committed no report", err)
        code, out, err = self.run_bridge(mode="fail")
        self.assertEqual(code, 3)
        self.assertIn("launcher failed", err)
        self.assertEqual(out, "")
        self.assertIn("launcher said: FAILED: nothing committed", err)

    def test_missing_settings_are_refused_before_anything_runs(self):
        code, out, err = self.run_bridge(task="   ")
        self.assertEqual(code, 2)
        self.assertIn("no instruction", err)
        code, out, err = self.run_bridge(NATIVE_MODEL="")
        self.assertEqual(code, 2)
        self.assertIn("NATIVE_MODEL", err)
        self.assertFalse(self.record.exists())


if __name__ == "__main__":
    unittest.main()
