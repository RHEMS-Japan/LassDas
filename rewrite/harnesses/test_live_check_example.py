"""Exercise the live check example (rewrite/examples/live-check) against its
fictional service on 127.0.0.1. Nothing here reaches another host."""
import importlib.util
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from unittest import mock

EXAMPLE = Path(__file__).resolve().parents[1] / "examples" / "live-check"
TOOL = EXAMPLE / "verify_feature.py"


def load_tool():
    specification = importlib.util.spec_from_file_location("verify_feature", TOOL)
    module = importlib.util.module_from_spec(specification)
    specification.loader.exec_module(module)
    return module


class LiveCheckExampleTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="live-check-")
        self.addCleanup(temporary.cleanup)
        self.base = Path(temporary.name)
        self.checkout = self.base / "checkout"
        (self.checkout / "src").mkdir(parents=True)
        (self.checkout / "docs").mkdir()
        shutil.copy(EXAMPLE / "server.py", self.checkout / "server.py")
        shutil.copy(EXAMPLE / "FEATURES.md", self.checkout / "docs" / "FEATURES.md")
        self.greeting("Hello 日本語\n")
        self.home = self.base / "home"
        self.environment = dict(os.environ, TASK_HOME=str(self.home), LIVE_CHECK_TEST_USER="live-check-user",
                                PYTHONDONTWRITEBYTECODE="1")
        self.environment.pop("GREETING_HOLD_FILE", None)
        self.hold = self.base / "hold"
        self.tool = load_tool()
        self.addCleanup(self.stop_what_is_left)

    def greeting(self, text):
        (self.checkout / "src" / "greeting.txt").write_text(text, encoding="utf-8")

    def command(self, *arguments):
        return [sys.executable, "-B", str(TOOL), *arguments]

    def check(self, *arguments, environment=None):
        return subprocess.run(self.command(*(arguments or ("run", "--map", "docs/FEATURES.md"))),
                              cwd=self.checkout, env=environment or self.environment,
                              stdin=subprocess.DEVNULL, capture_output=True, text=True, timeout=60)

    def held_check(self):
        """A check whose request the service holds until the hold file goes."""
        self.hold.write_text("")
        environment = dict(self.environment, GREETING_HOLD_FILE=str(self.hold))
        process = subprocess.Popen(self.command("run", "--map", "docs/FEATURES.md"), cwd=self.checkout,
                                   env=environment, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                   stderr=subprocess.PIPE, text=True, start_new_session=True)
        self.addCleanup(lambda: process.poll() is None and process.kill())
        waiting = Path(str(self.hold) + ".waiting")
        deadline = time.monotonic() + 30
        while not waiting.exists():
            self.assertIsNone(process.poll(), "the check ended before its request was held")
            self.assertLess(time.monotonic(), deadline, "the service never held the request")
            time.sleep(0.01)
        return process

    def launches(self):
        directory = self.home / "logs" / "live-check"
        return sorted(path for path in directory.iterdir() if path.is_dir()) if directory.is_dir() else []

    def users(self):
        directory = self.home / "live-check-store" / "users"
        return sorted(path.name for path in directory.iterdir()) if directory.is_dir() else []

    def record(self, launch, name):
        return json.loads((launch / name).read_text(encoding="utf-8"))

    def assertGone(self, launch):
        state = self.record(launch, "state.json")
        deadline = time.monotonic() + 5
        while self.tool.process_state(state["pid"], state["token"]) != "gone":
            self.assertLess(time.monotonic(), deadline, "the service %d is still running" % state["pid"])
            time.sleep(0.02)

    def stop_what_is_left(self):
        for launch in self.launches():
            state = json.loads((launch / "state.json").read_text()) if (launch / "state.json").exists() else None
            if state and self.tool.process_state(state["pid"], state["token"]) == "ours":
                os.kill(state["pid"], signal.SIGKILL)
        users = self.home / "live-check-store" / "users"
        if users.is_dir():
            users.chmod(0o700)

    def observation(self, output):
        """The closing lines: from the observation's heading to the result."""
        return output[output.rindex("ライブ確認 ("):].strip().splitlines()

    def test_a_passing_check_prints_the_observation_last_and_leaves_nothing_behind(self):
        result = self.check()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(result.stderr, "")
        lines = self.observation(result.stdout)
        self.assertTrue(lines[-1].startswith("結果: 0 で終わる"), lines)
        self.assertTrue(lines[-2].startswith("後始末: サービス (プロセス "), lines)
        self.assertIn("観測: 状態 200、本文「Hello 日本語, live-check-user」", lines)
        self.assertIn("合否: 合格", lines)
        request = next(line for line in lines if line.startswith("要求: GET http://127.0.0.1:"))
        [launch] = self.launches()
        self.assertGone(launch)
        self.assertEqual(self.users(), [])
        self.assertTrue(self.record(launch, "stopped.json")["ok"])
        for name in ("request.txt", "response.txt", "observation.json", "service.log"):
            self.assertTrue((launch / name).is_file(), name)
        # The request reached the service: its own log has it.
        path = request.split("127.0.0.1:", 1)[1].split("/", 1)[1]
        self.assertIn('"GET /%s HTTP/1.1" 200' % path, (launch / "service.log").read_text())
        # Nothing was written in the checkout.
        self.assertEqual(sorted(str(path.relative_to(self.checkout)) for path in self.checkout.rglob("*")),
                         ["docs", "docs/FEATURES.md", "server.py", "src", "src/greeting.txt"])

    def test_a_feature_that_does_not_work_is_cleaned_up_and_ends_one(self):
        self.greeting("")
        result = self.check()
        self.assertEqual(result.returncode, 1, result.stdout)
        lines = self.observation(result.stdout)
        self.assertIn("観測: 状態 500、本文「the first line of src/greeting.txt is empty」", lines)
        self.assertIn("合否: 不合格", lines)
        self.assertEqual(lines[-1], "結果: 1 で終わる (操作が不合格)")
        [launch] = self.launches()
        self.assertGone(launch)
        self.assertEqual(self.users(), [])
        self.assertTrue(self.record(launch, "stopped.json")["ok"])
        self.assertTrue((launch / "response.txt").is_file())

    def test_sigterm_while_the_answer_is_awaited_cleans_up_before_the_engines_sigkill(self):
        check = self.held_check()
        signalled = time.monotonic()
        os.killpg(check.pid, signal.SIGTERM)  # the engine signals the check's whole process group
        output, _ = check.communicate(timeout=10)
        # The engine sends SIGKILL three seconds after SIGTERM.
        self.assertLess(time.monotonic() - signalled, 2, output)
        self.assertEqual(check.returncode, 1, output)
        lines = self.observation(output)
        self.assertIn("観測: 応答なし (中断された: SIGTERM)", lines)
        self.assertTrue(any(line.startswith("後始末: サービス (プロセス ") for line in lines), lines)
        self.assertEqual(lines[-1], "結果: 1 で終わる (中断された: SIGTERM)")
        [launch] = self.launches()
        self.assertGone(launch)
        self.assertEqual(self.users(), [])
        self.assertTrue(self.record(launch, "stopped.json")["ok"])
        self.assertTrue((launch / "request.txt").is_file())

    def test_what_a_killed_launch_left_is_removed_by_the_next_one(self):
        check = self.held_check()
        os.kill(check.pid, signal.SIGKILL)  # the check alone: its service keeps running
        check.communicate(timeout=10)
        [killed] = self.launches()
        state = self.record(killed, "state.json")
        self.assertEqual(self.tool.process_state(state["pid"], state["token"]), "ours")
        self.assertEqual(len(self.users()), 1)
        self.assertFalse((killed / "stopped.json").exists())
        self.hold.unlink()
        result = self.check()
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertIn("前の起動の後始末: %s" % killed.name, result.stdout)
        self.assertGone(killed)
        self.assertTrue(self.record(killed, "stopped.json")["ok"])
        self.assertEqual(self.users(), [])
        self.assertEqual(len(self.launches()), 2)

    @unittest.skipIf(os.geteuid() == 0, "root removes files from a directory it may not write")
    def test_a_cleanup_that_fails_is_not_a_pass_and_stop_can_be_run_again(self):
        check = self.held_check()
        users = self.home / "live-check-store" / "users"
        users.chmod(0o500)
        self.hold.unlink()
        output, _ = check.communicate(timeout=30)
        self.assertEqual(check.returncode, 1, output)
        lines = self.observation(output)
        self.assertIn("合否: 合格", lines)
        self.assertTrue(any(line.startswith("後始末: 失敗: テスト用ユーザー lc-") for line in lines), lines)
        self.assertEqual(lines[-1], "結果: 1 で終わる (後始末が済んでいない)")
        [launch] = self.launches()
        self.assertFalse(self.record(launch, "stopped.json")["ok"])
        evidence = {name: (launch / name).read_bytes() for name in ("request.txt", "response.txt", "observation.json")}
        users.chmod(0o700)
        again = self.check("stop", "--output", str(launch))
        self.assertEqual(again.returncode, 0, again.stdout)
        self.assertEqual(self.users(), [])
        self.assertTrue(self.record(launch, "stopped.json")["ok"])
        self.assertEqual({name: (launch / name).read_bytes() for name in evidence}, evidence)

    def test_stop_runs_again_safely_and_keeps_the_evidence(self):
        self.assertEqual(self.check().returncode, 0)
        [launch] = self.launches()
        evidence = {path.name: path.read_bytes() for path in launch.iterdir()
                    if path.name in ("request.txt", "response.txt", "observation.json", "service.log")}
        again = self.check("stop", "--output", str(launch))
        self.assertEqual(again.returncode, 0, again.stdout)
        self.assertIn("は止まっていた", again.stdout)
        self.assertIn("は残っていなかった", again.stdout)
        self.assertEqual({name: (launch / name).read_bytes() for name in evidence}, evidence)
        empty = self.check("stop", "--output", str(self.base / "never-started"))
        self.assertEqual(empty.returncode, 0, empty.stdout)
        self.assertIn("片付けるものは無い", empty.stdout)

    def test_a_process_it_did_not_start_is_left_running(self):
        other = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(60)"])
        self.addCleanup(other.wait)
        self.addCleanup(other.kill)
        launch = self.base / "launch"
        launch.mkdir()
        (launch / "state.json").write_text(json.dumps({"pid": other.pid, "token": "0123456789abcdef"}))
        result = self.check("stop", "--output", str(launch))
        self.assertEqual(result.returncode, 1, result.stdout)
        self.assertIn("止めていない", result.stdout)
        self.assertIsNone(other.poll())
        self.assertFalse(self.record(launch, "stopped.json")["ok"])

    def test_what_is_missing_is_named_and_the_service_is_still_stopped(self):
        environment = dict(self.environment)
        environment.pop("LIVE_CHECK_TEST_USER")
        (self.checkout / "docs" / "FEATURES.md").write_text("| 機能 |\n| --- |\n| 別の機能 |\n", encoding="utf-8")
        result = self.check(environment=environment)
        self.assertEqual(result.returncode, 1, result.stdout)
        self.assertIn("診断: 足りない: テスト用のユーザーとデータ: LIVE_CHECK_TEST_USER が設定されていない", result.stdout)
        self.assertIn("診断: 足りない: 操作する手段: docs/FEATURES.md に「挨拶を表示する」の行が無い", result.stdout)
        self.assertIn("観測: 応答なし (準備が足りない)", result.stdout)
        self.assertTrue(result.stdout.rstrip().endswith("結果: 1 で終わる (準備が足りない)"))
        [launch] = self.launches()
        self.assertGone(launch)
        self.assertEqual(self.users(), [])

    def test_the_merged_check_can_run_it_as_one_of_its_commands(self):
        import verify_merged
        report = []
        with mock.patch.dict(os.environ, {"TASK_HOME": str(self.home), "LIVE_CHECK_TEST_USER": "live-check-user"}):
            failures = verify_merged.run_verification(str(self.checkout), [self.command("run", "--map", "docs/FEATURES.md")], report)
        self.assertEqual(failures, 0, report)
        [text] = report
        self.assertIn("Exit status: 0", text)
        self.assertIn("観測: 状態 200、本文「Hello 日本語, live-check-user」", text)
        self.assertTrue(text.rstrip().endswith("結果: 0 で終わる (起動・診断・操作・証拠・後始末がすべて済んだ)"), text[-300:])

    def test_it_starts_nothing_through_a_shell(self):
        source = TOOL.read_text(encoding="utf-8") + (EXAMPLE / "server.py").read_text(encoding="utf-8")
        for text in ("shell=True", "os.system", "os.popen", "/bin/sh"):
            self.assertNotIn(text, source)


if __name__ == "__main__":
    unittest.main()
