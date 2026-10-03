"""A real Git checkout, real test commands, and a local service standing in for
the model: what the review command sends, what it writes, and how it ends."""
import contextlib
import http.server
import importlib.util
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import threading
import time
import types
import unittest
from unittest import mock

SCRIPT = Path(__file__).with_name("adversarial_review.py").resolve()
KEY = "fixture-review-credential-9e1f3a"
IDENTITY = ("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid")


def git_environment():
    return {"PATH": os.environ["PATH"], "LANG": "C.UTF-8", "GIT_CONFIG_GLOBAL": os.devnull,
            "GIT_CONFIG_SYSTEM": os.devnull, "GIT_CONFIG_NOSYSTEM": "1", "GIT_TERMINAL_PROMPT": "0",
            "GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "user.useConfigOnly", "GIT_CONFIG_VALUE_0": "true"}


class ModelStandIn:
    """Answers chat completions with scripted verdicts and records every request.
    The replies are one list for every model, or a list per model id; a single
    reply in place of a list is given every time."""

    def __init__(self, replies):
        self.replies = replies if isinstance(replies, dict) else list(replies)
        self.requests = []
        owner = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_POST(self):
                length = int(self.headers.get("Content-Length", "0"))
                body = json.loads(self.rfile.read(length).decode("utf-8"))
                owner.requests.append({"path": self.path, "authorization": self.headers.get("Authorization", ""),
                                       "body": body})
                reply = owner.reply_for(body.get("model"))
                if reply.get("status", 200) != 200:
                    payload = json.dumps({"error": reply.get("error", "service error")}).encode()
                    self.send_response(reply["status"])
                elif reply.get("verdict") is None and not {"arguments", "calls", "raw_calls"} & set(reply):
                    payload = json.dumps({"choices": [{"message": {"role": "assistant", "content": reply.get(
                        "content", "I have looked.")}}]}).encode()
                    self.send_response(200)
                else:
                    if "raw_calls" in reply:
                        # Arguments as a service might send them: broken text, or an object.
                        calls = reply["raw_calls"]
                    elif "calls" in reply:
                        calls = [json.dumps(arguments) for arguments in reply["calls"]]
                    elif "arguments" in reply:
                        calls = [json.dumps(reply["arguments"])]
                    else:
                        blocking, findings = reply["verdict"]
                        calls = [json.dumps({"blocking": blocking, "findings": findings})]
                    payload = json.dumps({"choices": [{"message": {"role": "assistant", "tool_calls": [
                        {"id": "call-%d" % number, "type": "function", "function": {"name": "verdict", "arguments":
                            arguments}} for number, arguments in enumerate(calls, 1)]}}]}).encode()
                    self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)

            def log_message(self, *arguments):
                pass

        self.server = http.server.HTTPServer(("127.0.0.1", 0), Handler)
        self.url = "http://127.0.0.1:%d/v1/chat/completions" % self.server.server_address[1]
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    def reply_for(self, model):
        replies = self.replies.get(model, []) if isinstance(self.replies, dict) else self.replies
        if isinstance(replies, dict):
            return replies
        return replies.pop(0) if replies else {"status": 200, "verdict": (False, "")}

    def models_asked(self):
        return [request["body"]["model"] for request in self.requests]

    def close(self):
        self.server.shutdown()
        self.server.server_close()


@unittest.skipUnless(shutil.which("git"), "requires Git")
class AdversarialReviewTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="review-test-")
        self.addCleanup(temporary.cleanup)
        self.workspace = Path(temporary.name).resolve() / "workspace"
        self.workspace.mkdir()
        self.home = Path(temporary.name).resolve() / "home"
        self.home.mkdir()
        self.git("init", "--initial-branch=master")
        (self.workspace / "src").mkdir()
        (self.workspace / "src" / "tool.py").write_text("def run():\n    return 1\n")
        self.git("add", "-A")
        self.git("commit", "-m", "Codex: initial")
        # The change under review: one edit and one new test file.
        (self.workspace / "src" / "tool.py").write_text("def run():\n    return 2  # changed\n")
        (self.workspace / "tests").mkdir()
        (self.workspace / "tests" / "test_tool.py").write_text("print('new test file')\n")

    def git(self, *arguments):
        return subprocess.run(["git", "-C", str(self.workspace), *IDENTITY, *arguments],
                              check=True, capture_output=True, text=True, env=git_environment())

    STDIN = "Current assignment:\nStage 4 of 8\n\nOriginal request:\nadd a thing\n"
    HOLD = 0.2  # REVIEW_HOLD_SECONDS in these tests

    def environment(self, service, **extra):
        # Waits in fractions of a second, so that waiting out a model service
        # and holding take a test moments rather than minutes.
        environment = {"PATH": os.environ["PATH"], "LANG": "C.UTF-8", "PYTHONDONTWRITEBYTECODE": "1",
                       "TASK_WORKSPACE": str(self.workspace), "TASK_HOME": str(self.home), "REVIEW_MODEL_URL": service.url,
                       "REVIEW_MODEL": "fixture/reviewer", "REVIEW_KEY_ENV": "REVIEW_API_KEY", "REVIEW_API_KEY": KEY,
                       "REVIEW_TEST_COMMANDS": sys.executable + " -c \"print('tests ran fine')\"",
                       "REVIEW_DIFF_PATHS": "src tests", "REVIEW_ATTEMPTS": "2",
                       "REVIEW_TIMEOUT_SECONDS": "30", "REVIEW_RETRY_SECONDS": "0.05",
                       "REVIEW_RETRY_CAP_SECONDS": "0.2", "REVIEW_HOLD_SECONDS": str(self.HOLD)}
        environment.update(extra)
        return environment

    def run_review(self, service, stdin_text=STDIN, **extra):
        return subprocess.run([sys.executable, "-B", str(SCRIPT)], input=stdin_text, capture_output=True,
                              text=True, env=self.environment(service, **extra), timeout=120)

    def start_review(self, service, **extra):
        """A review running in the background: its runtime text read from a
        file, and what it prints written to files the test can look at while
        it runs."""
        directory = Path(tempfile.mkdtemp(dir=self.home.parent))
        (directory / "runtime-text.txt").write_text(self.STDIN)
        with (directory / "runtime-text.txt").open() as stdin, (directory / "stdout").open("w") as stdout, \
                (directory / "stderr").open("w") as stderr:
            process = subprocess.Popen([sys.executable, "-B", str(SCRIPT)], stdin=stdin, stdout=stdout, stderr=stderr,
                                       env=self.environment(service, **extra))
        self.addCleanup(lambda: process.poll() is None and process.kill())
        return process, directory / "stdout", directory / "stderr"

    def said(self, path, text, timeout=60):
        """Wait until the running review has printed text."""
        deadline = time.monotonic() + timeout
        while text not in path.read_text():
            if time.monotonic() > deadline:
                self.fail("the review never said %r: %s" % (text, path.read_text()))
            time.sleep(0.05)

    def held_review(self, service, intervals=6, **extra):
        """Start a review that is expected not to end, let it run for several
        hold intervals after it first says what it is doing, then stop it:
        whether it was still running, and what it printed."""
        process, stdout, stderr = self.start_review(service, **extra)
        self.said(stderr, "Review")
        time.sleep(self.HOLD * intervals)
        running = process.poll() is None
        process.kill()
        process.wait(timeout=30)
        return running, stdout.read_text(), stderr.read_text()

    def review_in_process(self, service, before_each_command, **extra):
        """The review run in this process, so that a Git that runs out of
        time can be had without waiting for it: before_each_command sees each
        command the review starts and may raise in its place. The exit status,
        what the review printed, and what it said on stderr."""
        spec = importlib.util.spec_from_file_location("review_under_test", SCRIPT)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)

        def run(command, *arguments, **keywords):
            before_each_command(command)
            return subprocess.run(command, *arguments, **keywords)

        printed, said = io.StringIO(), io.StringIO()
        stdin = io.TextIOWrapper(io.BytesIO(b"Original request:\nadd a thing\n"))
        with mock.patch.object(module, "subprocess", types.SimpleNamespace(
                run=run, TimeoutExpired=subprocess.TimeoutExpired)), \
                mock.patch.dict(os.environ, self.environment(service, **extra), clear=True), \
                mock.patch.object(sys, "stdin", stdin), contextlib.redirect_stdout(printed), \
                contextlib.redirect_stderr(said):
            if hasattr(self, "before_main"):
                self.before_main(module)
            status = module.main()
        return status, printed.getvalue(), said.getvalue()

    def review_log(self):
        path = self.home / "review.md"
        return path.read_text(encoding="utf-8") if path.is_file() else ""

    def test_a_blocking_verdict_sends_the_work_back_and_records_the_findings(self):
        service = ModelStandIn([{"verdict": (True, "src/tool.py returns 2 but the request asked for 3")}])
        self.addCleanup(service.close)
        finished = self.run_review(service)
        self.assertEqual(finished.returncode, 1, finished.stderr)
        self.assertIn("SENT BACK", self.review_log())
        self.assertIn("asked for 3", self.review_log())
        self.assertEqual((self.home / "review-send-backs").read_text(), "1")
        self.assertIn("SENT BACK", finished.stdout)
        self.assertIn("asked for 3", finished.stdout)
        self.assertFalse((self.workspace / "report").exists(), "the review must not write into the workspace")
        self.assertIn("sent back 0 times so far", service.requests[0]["body"]["messages"][0]["content"])

    def test_the_reviewer_receives_the_runtime_text_the_diff_and_the_test_output(self):
        service = ModelStandIn([{"verdict": (False, "")}])
        self.addCleanup(service.close)
        finished = self.run_review(service, stdin_text="Current assignment:\nStage 4 of 8: review\n\nOriginal request:\nreturn two\n")
        self.assertEqual(finished.returncode, 0, finished.stderr)
        sent = service.requests[0]["body"]
        text = sent["messages"][1]["content"]
        self.assertIn("return two", text)
        self.assertIn("+    return 2  # changed", text)
        self.assertIn("new file tests/test_tool.py", text)
        self.assertIn("tests ran fine", text)
        self.assertEqual(sent["tool_choice"]["function"]["name"], "verdict")
        self.assertIn("PASS", self.review_log())

    PASS = {"REVIEW_UNAVAILABLE": "pass"}  # the operator's opt-in for the old behaviour

    def test_service_trouble_is_waited_out_and_the_verdict_decides(self):
        # Three requests get nothing; the fourth gets a verdict, and the exit
        # status is that verdict's. The live view hears of the trouble once,
        # not on every request, and once more when a verdict arrives.
        for verdict, status, outcome in (((True, "src/tool.py returns 2, not 3"), 1, "SENT BACK"),
                                          ((False, ""), 0, "PASSED")):
            service = ModelStandIn([{"status": 503, "error": "busy " + KEY}] * 3 + [{"verdict": verdict}])
            self.addCleanup(service.close)
            finished = self.run_review(service)
            self.assertEqual(finished.returncode, status, (finished.stdout, finished.stderr))
            self.assertIn("Review by fixture/reviewer: %s" % outcome, finished.stdout)
            self.assertEqual(len(service.requests), 4)
            self.assertEqual(finished.stderr.count("no verdict yet"), 1, finished.stderr)
            self.assertIn("fixture/reviewer: HTTP 503 from the model service", finished.stderr)
            self.assertIn("gave a verdict, after 3 requests that got none", finished.stderr)
            self.assertNotIn(KEY, finished.stdout + finished.stderr)
            self.assertNotIn("NOT REVIEWED", finished.stdout)

    def test_a_reply_without_a_verdict_is_asked_again_too(self):
        service = ModelStandIn([{"verdict": None}, {"verdict": None}, {"verdict": (False, "")}])
        self.addCleanup(service.close)
        finished = self.run_review(service)
        self.assertEqual(finished.returncode, 0, finished.stdout)
        self.assertIn("PASSED", finished.stdout)
        self.assertIn("the reviewer returned no verdict", finished.stderr)
        self.assertEqual(len(service.requests), 3)

    def test_the_models_are_asked_in_order_and_the_one_that_answered_is_named(self):
        for listing in ("maker-a/first\nmaker-b/second", "maker-a/first, maker-b/second"):
            service = ModelStandIn({"maker-a/first": {"status": 503},
                                    "maker-b/second": [{"verdict": None}, {"verdict": (True, "a missing test")}]})
            self.addCleanup(service.close)
            finished = self.run_review(service, REVIEW_MODELS=listing)
            self.assertEqual(finished.returncode, 1, (listing, finished.stdout, finished.stderr))
            self.assertIn("Review by maker-b/second: SENT BACK", finished.stdout)
            self.assertNotIn("fixture/reviewer", finished.stdout)
            self.assertEqual(service.models_asked(), ["maker-a/first", "maker-b/second"] * 2)
            self.assertIn("maker-a/first: HTTP 503 from the model service: {\"error\": \"service error\"};"
                          " maker-b/second: the reviewer returned no verdict",
                          finished.stderr)
            self.assertIn("maker-b/second gave a verdict", finished.stderr)

    def test_a_setting_only_the_operator_can_fix_holds_the_review_without_an_end(self):
        # No verdict can be obtained and none is pretended: the command neither
        # exits 0 nor 1. It says why once, then a short line at each interval.
        for name, value, reason in (("REVIEW_MODEL_URL", "http://model.example.invalid/v1/chat/completions", "must be https"),
                                    ("REVIEW_API_KEY", "", "the review credential is not set"),
                                    ("REVIEW_ATTEMPTS", "many", "REVIEW_ATTEMPTS must be a whole number"),
                                    ("REVIEW_TEST_COMMANDS", "echo 'unterminated", "REVIEW_TEST_COMMANDS could not be read")):
            service = ModelStandIn([{"verdict": (False, "")}])
            self.addCleanup(service.close)
            running, stdout, stderr = self.held_review(service, **{name: value})
            self.assertTrue(running, (name, stdout, stderr))
            self.assertEqual(stdout, "", name)
            self.assertEqual(stderr.count(reason), 1, (name, stderr))
            self.assertIn("fix the setting and restart the engine", stderr, name)
            # A restart does not launch the review afresh: the runtime records
            # it as a failure and goes on at the stage named for that.
            self.assertIn("goes on at the review's on_failure stage", stderr, name)
            self.assertGreaterEqual(stderr.count("Review still held at"), 2, (name, stderr))
            self.assertEqual(service.requests, [], name)

    def test_a_held_review_goes_on_once_task_home_can_be_used(self):
        blocker = self.home / "in-the-way"
        blocker.write_text("a file where the review's directory should be\n")
        service = ModelStandIn([{"verdict": (True, "the request asked for 3")}])
        self.addCleanup(service.close)
        process, stdout_path, stderr_path = self.start_review(service, TASK_HOME=str(blocker / "home"))
        self.said(stderr_path, "TASK_HOME cannot be used")
        time.sleep(self.HOLD * 3)
        self.assertIsNone(process.poll(), "the review ended while TASK_HOME could not be used")
        blocker.unlink()
        process.wait(timeout=60)
        stdout, stderr = stdout_path.read_text(), stderr_path.read_text()
        self.assertEqual(process.returncode, 1, (stdout, stderr))
        self.assertIn("SENT BACK", stdout)
        self.assertEqual(stderr.count("TASK_HOME cannot be used"), 1, stderr)
        self.assertIn("this is looked at again every", stderr)

    def test_an_unexpected_error_is_waited_out_never_passed(self):
        service = ModelStandIn([{"verdict": (False, "")}])
        self.addCleanup(service.close)
        # urlsplit refuses this URL with an error the command does not expect.
        running, stdout, stderr = self.held_review(service, REVIEW_MODEL_URL="https://[oops/v1/chat/completions")
        self.assertTrue(running, (stdout, stderr))
        self.assertEqual(stdout, "")
        self.assertEqual(stderr.count("Unexpected ValueError"), 1, stderr)
        self.assertNotIn("Traceback", stderr)
        self.assertEqual(service.requests, [])

    def test_a_blocking_verdict_sends_the_work_back_even_when_its_state_cannot_be_saved(self):
        self.home.chmod(0o500)
        self.addCleanup(self.home.chmod, 0o700)
        for extra in ({}, self.PASS):
            service = ModelStandIn([{"verdict": (True, "the request asked for 3")}])
            self.addCleanup(service.close)
            finished = self.run_review(service, **extra)
            self.assertEqual(finished.returncode, 1, (extra, finished.stdout))
            self.assertIn("SENT BACK", finished.stdout)
            self.assertIn("the send-back state was not saved", finished.stdout)
            self.assertNotIn("NOT REVIEWED", finished.stdout)

    def test_a_send_back_counter_that_cannot_be_read_counts_from_zero(self):
        (self.home / "review-send-backs").write_text("not a number")
        service = ModelStandIn([{"verdict": (True, "x")}])
        self.addCleanup(service.close)
        finished = self.run_review(service)
        self.assertEqual(finished.returncode, 1, finished.stdout)
        self.assertIn("could not be read", finished.stderr)
        self.assertEqual((self.home / "review-send-backs").read_text(), "1")

    def test_paths_that_match_no_change_show_the_whole_change(self):
        service = ModelStandIn([{"verdict": (True, "src/tool.py returns 2")}])
        self.addCleanup(service.close)
        finished = self.run_review(service, REVIEW_DIFF_PATHS="app lib")
        self.assertEqual(finished.returncode, 1, finished.stdout)
        text = service.requests[0]["body"]["messages"][1]["content"]
        self.assertIn("REVIEW_DIFF_PATHS (app lib) matched no change, so the whole change is shown.", text)
        self.assertIn("+    return 2  # changed", text)
        self.assertIn("new file tests/test_tool.py", text)

    def test_the_notes_on_the_diff_paths_say_what_was_shown(self):
        # The whole change shown in place of the paths is cut like any
        # other, and the note says so; and when the paths match part of the
        # change, the rest is said not to be shown instead of left out quietly.
        (self.workspace / "src" / "tool.py").write_text("def run():\n    return 2  # changed\n" + "".join(
            "# line %05d of an edit that makes the diff long\n" % i for i in range(1000)))
        service = ModelStandIn([{"verdict": (False, "")}, {"verdict": (False, "")}])
        self.addCleanup(service.close)
        self.assertEqual(self.run_review(service, REVIEW_DIFF_PATHS="app").returncode, 0)
        text = service.requests[0]["body"]["messages"][1]["content"]
        self.assertRegex(text, r"REVIEW_DIFF_PATHS \(app\) matched no change, so the whole change is shown, cut at"
                               r" 20000 of its \d+ characters where it says so\.")
        self.assertIn("[diff cut here:", text)
        self.assertEqual(self.run_review(service, REVIEW_DIFF_PATHS="tests").returncode, 0)
        text = service.requests[1]["body"]["messages"][1]["content"]
        self.assertIn("Changes outside REVIEW_DIFF_PATHS (tests) are not shown.", text)
        self.assertNotIn("+    return 2  # changed", text)

    # With REVIEW_UNAVAILABLE=pass, today's behaviour on every path where no
    # verdict can be obtained: NOT REVIEWED, and the work goes through.

    def test_with_the_opt_in_a_pass_or_a_missing_verdict_does_not_consume_the_send_back_budget(self):
        service = ModelStandIn([{"status": 500}, {"verdict": None}, {"verdict": (False, "")}, {"verdict": (True, "a real defect")}])
        self.addCleanup(service.close)
        self.assertEqual(self.run_review(service, **self.PASS).returncode, 0)   # no verdict: through, not counted
        self.assertEqual(self.run_review(service, **self.PASS).returncode, 0)   # a pass: not counted
        self.assertFalse((self.home / "review-send-backs").exists())
        finished = self.run_review(service, **self.PASS)                          # the first real objection still sends back
        self.assertEqual(finished.returncode, 1, finished.stdout)
        self.assertEqual((self.home / "review-send-backs").read_text(), "1")

    def test_with_the_opt_in_no_verdict_lets_the_work_through_with_a_note(self):
        service = ModelStandIn([{"status": 500}, {"verdict": None}])
        self.addCleanup(service.close)
        finished = self.run_review(service, **self.PASS)
        self.assertEqual(finished.returncode, 0, finished.stderr)
        self.assertIn("NOT REVIEWED", finished.stdout)
        self.assertIn("no verdict could be obtained", self.review_log())
        self.assertEqual(len(service.requests), 2)

    def test_with_the_opt_in_the_models_are_asked_in_turn_before_giving_up(self):
        service = ModelStandIn({"maker-a/first": {"status": 503}, "maker-b/second": {"status": 502}})
        self.addCleanup(service.close)
        finished = self.run_review(service, REVIEW_MODELS="maker-a/first, maker-b/second", **self.PASS)
        self.assertEqual(finished.returncode, 0, finished.stdout)
        self.assertIn("Review by maker-a/first, maker-b/second: NOT REVIEWED", finished.stdout)
        self.assertEqual(service.models_asked(), ["maker-a/first", "maker-b/second"])

    def test_the_credential_reaches_only_the_model_service_and_never_the_output(self):
        service = ModelStandIn([{"status": 502, "error": "bad key " + KEY}, {"status": 502, "error": "bad key " + KEY}])
        self.addCleanup(service.close)
        finished = self.run_review(service, **self.PASS)
        self.assertEqual(finished.returncode, 0, finished.stderr)
        self.assertEqual(service.requests[0]["authorization"], "Bearer " + KEY)
        for text in (finished.stdout, finished.stderr, self.review_log()):
            self.assertNotIn(KEY, text)

    def test_with_the_opt_in_a_setting_that_cannot_be_used_lets_the_work_through_and_says_so(self):
        # Today's behaviour, chosen by the operator: NOT REVIEWED with the
        # setting's name, nothing called, nothing written into the workspace.
        service = ModelStandIn([{"verdict": (True, "x")}])
        self.addCleanup(service.close)
        for name, value in (("REVIEW_MODEL_URL", ""),
                            ("REVIEW_ATTEMPTS", "many"), ("TASK_HOME", ""), ("TASK_HOME", str(self.workspace / "src" / "tool.py" / "x")),
                            ("REVIEW_TEST_COMMANDS", "echo 'unterminated")):
            finished = self.run_review(service, **{name: value, **self.PASS})
            self.assertEqual(finished.returncode, 0, (name, finished.stdout, finished.stderr))
            self.assertIn("NOT REVIEWED", finished.stdout, name)
            self.assertIn(name, finished.stdout, name)
            self.assertNotIn("Traceback", finished.stderr, name)
        self.assertEqual(service.requests, [])
        self.assertFalse((self.workspace / "report").exists())

    def test_a_test_command_that_cannot_start_is_shown_and_the_review_still_happens(self):
        service = ModelStandIn([{"verdict": (False, "")}])
        self.addCleanup(service.close)
        finished = self.run_review(service, REVIEW_TEST_COMMANDS="/nonexistent/operator/test")
        self.assertEqual(finished.returncode, 0, finished.stderr)
        self.assertNotIn("Traceback", finished.stderr)
        self.assertIn("could not start", service.requests[0]["body"]["messages"][1]["content"])
        self.assertIn("PASSED", finished.stdout)

    def test_long_test_output_is_cut_with_a_visible_marker(self):
        service = ModelStandIn([{"verdict": (False, "")}])
        self.addCleanup(service.close)
        noisy = sys.executable + " -c \"print('HEAD-OF-OUTPUT'); print('x' * 9000); print('TAIL-OF-OUTPUT')\""
        finished = self.run_review(service, REVIEW_TEST_COMMANDS=noisy)
        self.assertEqual(finished.returncode, 0, finished.stderr)
        text = service.requests[0]["body"]["messages"][1]["content"]
        self.assertIn("[test output cut here:", text)
        self.assertIn("TAIL-OF-OUTPUT", text)

    def test_with_the_opt_in_an_unexpected_error_and_a_stray_byte_never_end_with_a_traceback_or_status_one(self):
        service = ModelStandIn([{"verdict": (False, "")}])
        self.addCleanup(service.close)
        # An unexpected exception (urlsplit refuses this URL) is caught at the top; it makes no request.
        finished = self.run_review(service, REVIEW_MODEL_URL="https://[oops/v1/chat/completions", **self.PASS)
        self.assertEqual(finished.returncode, 0, finished.stderr)
        self.assertIn("NOT REVIEWED", finished.stdout)
        self.assertIn("Unexpected", finished.stdout)
        self.assertNotIn("Traceback", finished.stderr)
        self.assertEqual(service.requests, [])
        # A stray byte in the runtime's text is decoded leniently under a strict locale too.
        environment = {"PATH": os.environ["PATH"], "LANG": "en_US.UTF-8", "PYTHONIOENCODING": "utf-8",
                       "PYTHONDONTWRITEBYTECODE": "1", "TASK_WORKSPACE": str(self.workspace), "TASK_HOME": str(self.home),
                       "REVIEW_MODEL_URL": service.url, "REVIEW_MODEL": "fixture/reviewer", "REVIEW_KEY_ENV": "REVIEW_API_KEY",
                       "REVIEW_API_KEY": KEY, "REVIEW_TEST_COMMANDS": "", "REVIEW_DIFF_PATHS": "src tests",
                       "REVIEW_ATTEMPTS": "1", "REVIEW_TIMEOUT_SECONDS": "30"}
        finished = subprocess.run([sys.executable, "-B", str(SCRIPT)], input=b"Original request:\nfix \xff\xfe it\n",
                                  capture_output=True, env=environment, timeout=120)
        self.assertEqual(finished.returncode, 0, finished.stderr.decode(errors="replace"))
        self.assertNotIn(b"Traceback", finished.stderr)
        self.assertIn("PASSED", finished.stdout.decode(errors="replace"))

    def test_with_the_opt_in_the_credential_is_sent_only_over_https_or_loopback(self):
        service = ModelStandIn([{"verdict": (True, "x")}])
        self.addCleanup(service.close)
        finished = self.run_review(service, REVIEW_MODEL_URL="http://model.example.invalid/v1/chat/completions", **self.PASS)
        self.assertEqual(finished.returncode, 0, finished.stderr)
        self.assertIn("must be https", finished.stdout)
        self.assertIn("NOT REVIEWED", finished.stdout)
        self.assertEqual(service.requests, [])

    def test_a_large_change_reaches_the_reviewer_whole_and_a_cut_is_visible(self):
        big = "".join("line %04d of a long file that the reviewer must see\n" % i for i in range(700))
        (self.workspace / "src" / "big.py").write_text(big)
        (self.workspace / "src" / "tool.py").write_text("def run():\n    return 3\n" + "".join("# padding %d\n" % i for i in range(300)))
        service = ModelStandIn([{"verdict": (False, "")}])
        self.addCleanup(service.close)
        finished = self.run_review(service)
        self.assertEqual(finished.returncode, 0, finished.stderr)
        text = service.requests[0]["body"]["messages"][1]["content"]
        self.assertIn("+# padding 299", text, "the whole diff of an edited file must reach the reviewer")
        self.assertIn("new file src/big.py", text)
        self.assertIn("[new file cut here:", text)
        self.assertLess(text.index("new file src/big.py"), text.index("+# padding 299"), "new files come first")

    NO_CHANGE = ("Diff of the change:\nNo file was changed. Judge whether the request and the settled requirements"
                 " are satisfied with the repository exactly as it is; if a change is needed and none was made,"
                 " that is a blocking defect.")

    def leave_unchanged(self):
        self.git("checkout", "--", ".")
        shutil.rmtree(self.workspace / "tests")

    def test_no_change_at_all_is_said_in_plain_words_and_the_verdict_is_read_as_before(self):
        self.leave_unchanged()
        service = ModelStandIn([{"verdict": (False, "")}, {"verdict": (True, "the request asks for a new option")}])
        self.addCleanup(service.close)
        passed = self.run_review(service)
        self.assertEqual(passed.returncode, 0, passed.stderr)
        self.assertIn("PASSED", passed.stdout)
        sent_back = self.run_review(service)
        self.assertEqual(sent_back.returncode, 1, sent_back.stdout)
        self.assertIn("SENT BACK", sent_back.stdout)
        for request in service.requests:
            text = request["body"]["messages"][1]["content"]
            self.assertIn(self.NO_CHANGE, text)
            self.assertNotIn("(no change)", text)
            self.assertIn("tests ran fine", text)

    def test_a_change_is_never_called_no_change(self):
        service = ModelStandIn([{"verdict": (False, "")}])
        self.addCleanup(service.close)
        finished = self.run_review(service)
        self.assertEqual(finished.returncode, 0, finished.stderr)
        text = service.requests[0]["body"]["messages"][1]["content"]
        self.assertIn("+    return 2  # changed", text)
        self.assertNotIn("No file was changed", text)

    def test_an_empty_diff_after_a_committed_delivery_round_is_shown_as_before(self):
        # The delivery committed its round, so the change is in HEAD and the
        # diff against it is empty: that is not a change nobody made.
        self.leave_unchanged()
        head = self.git("rev-parse", "HEAD").stdout.strip()
        receipt = self.workspace / ".git" / "ticket-engine" / "delivery.json"
        receipt.parent.mkdir()
        receipt.write_text(json.dumps({"issue": "TICKET-41", "head": head, "pull_request": 1}))
        service = ModelStandIn([{"verdict": (False, "")}, {"verdict": (False, "")}])
        self.addCleanup(service.close)
        self.assertEqual(self.run_review(service).returncode, 0)
        text = service.requests[0]["body"]["messages"][1]["content"]
        self.assertIn("Diff of the change:\n(no change)", text)
        self.assertNotIn("No file was changed", text)
        # An ending without a delivery committed nothing, so it changes nothing here.
        receipt.write_text(json.dumps({"unchanged": True, "base_sha": head, "workspace_head": head}))
        self.assertEqual(self.run_review(service).returncode, 0)
        self.assertIn(self.NO_CHANGE, service.requests[1]["body"]["messages"][1]["content"])

    GOES_BACK = "an ending with nothing delivered needs a verdict, so the work goes back this time"

    def test_with_nothing_changed_the_opt_in_still_lets_nothing_through_without_a_verdict(self):
        # Let through, an unchanged checkout can end with nothing delivered.
        # That rule comes before the opt-in: a service that is down, a reply
        # without a verdict, a setting that keeps the review from running and
        # an unexpected error all send the work back instead of through.
        self.leave_unchanged()
        for case, replies, extra in (
                ("service down", [{"status": 503}, {"status": 503}], {}),
                ("text only", [{"verdict": None}, {"verdict": None}], {}),
                ("no credential", [], {"REVIEW_API_KEY": ""}),
                ("no endpoint", [], {"REVIEW_MODEL_URL": ""}),
                ("unexpected error", [], {"REVIEW_MODEL_URL": "https://[oops/v1/chat/completions"})):
            service = ModelStandIn(replies)
            self.addCleanup(service.close)
            finished = self.run_review(service, **{**extra, **self.PASS})
            self.assertEqual(finished.returncode, 1, (case, finished.stdout, finished.stderr))
            self.assertIn("NOT REVIEWED", finished.stdout, case)
            self.assertIn(self.GOES_BACK, finished.stdout, case)
            self.assertNotIn("Traceback", finished.stderr, case)
        self.assertFalse((self.home / "review-send-backs").exists(), "no verdict was counted as a send-back")

    def test_with_nothing_changed_and_no_verdict_the_review_keeps_asking(self):
        self.leave_unchanged()
        service = ModelStandIn({"fixture/reviewer": {"status": 503}})
        self.addCleanup(service.close)
        running, stdout, stderr = self.held_review(service)
        self.assertTrue(running, (stdout, stderr))
        self.assertEqual(stdout, "")
        self.assertGreaterEqual(len(service.requests), 3)
        self.assertIn(self.NO_CHANGE.split("\n", 1)[1], service.requests[0]["body"]["messages"][1]["content"])

    def test_with_nothing_changed_the_verdict_decides_even_when_the_state_cannot_be_saved(self):
        self.leave_unchanged()
        self.home.chmod(0o500)
        self.addCleanup(self.home.chmod, 0o700)
        service = ModelStandIn([{"verdict": (True, "the request needs a new file and none was written")},
                                {"verdict": (False, "")}])
        self.addCleanup(service.close)
        sent_back = self.run_review(service)
        self.assertEqual(sent_back.returncode, 1, sent_back.stdout)
        self.assertIn("SENT BACK", sent_back.stdout)
        self.assertIn("was not saved", sent_back.stdout)
        passed = self.run_review(service)
        self.assertEqual(passed.returncode, 0, passed.stdout)
        self.assertIn("PASSED", passed.stdout)

    def test_with_the_opt_in_a_change_or_a_committed_round_without_a_verdict_goes_on_as_before(self):
        for case in ("a change", "a committed round"):
            if case == "a committed round":
                self.leave_unchanged()
                receipt = self.workspace / ".git" / "ticket-engine" / "delivery.json"
                receipt.parent.mkdir()
                receipt.write_text(json.dumps({"head": self.git("rev-parse", "HEAD").stdout.strip()}))
            service = ModelStandIn([{"status": 503}, {"status": 503}])
            self.addCleanup(service.close)
            finished = self.run_review(service, **self.PASS)
            self.assertEqual(finished.returncode, 0, (case, finished.stdout))
            self.assertIn("the work goes on unreviewed this time", finished.stdout, case)

    def test_blocking_is_read_when_its_meaning_is_plain_and_the_findings_always_stay(self):
        # REPRO 07 printed blocking null beside a finding that a needed change
        # is missing as PASSED; read as true or false only, "true" and 1 then
        # let a change through unreviewed with the findings gone. Every call
        # and every blocking field in any letter case is read: one plain true
        # sends back, else one plain false passes, else there is no verdict.
        # Then, by default, it is asked again until a verdict decides, and with
        # the opt-in it is NOT REVIEWED. What the reviewer wrote stays every time.
        decided = (("\"true\"", [{"blocking": "true"}], "SENT BACK", 1),
                   ("\" TRUE \"", [{"blocking": " TRUE "}], "SENT BACK", 1),
                   ("1", [{"blocking": 1}], "SENT BACK", 1),
                   ("\"yes\"", [{"blocking": "yes"}], "SENT BACK", 1),
                   ("\"1\"", [{"blocking": "1"}], "SENT BACK", 1),
                   ("Blocking", [{"Blocking": True}], "SENT BACK", 1),
                   ("false and Blocking true", [{"blocking": False, "Blocking": True}], "SENT BACK", 1),
                   ("false, then true", [{"blocking": False}, {"blocking": True}], "SENT BACK", 1),
                   ("true, then false", [{"blocking": True}, {"blocking": False}], "SENT BACK", 1),
                   ("\"false\"", [{"blocking": "false"}], "PASSED", 0),
                   ("0", [{"blocking": 0}], "PASSED", 0),
                   ("\"no\"", [{"blocking": "no"}], "PASSED", 0),
                   ("\"0\"", [{"blocking": "0"}], "PASSED", 0),
                   ("false and Blocking maybe", [{"blocking": False, "Blocking": "maybe"}], "PASSED", 0),
                   # A true one object down counts whatever the call says itself.
                   ("false beside a true one object down", [{"blocking": False, "verdict": {"blocking": True}}],
                    "SENT BACK", 1),
                   ("\"false\" beside \"yes\" one object down",
                    [{"blocking": "false", "verdict": {"blocking": "yes"}}], "SENT BACK", 1),
                   ("null beside a true one object down", [{"blocking": None, "detail": {"blocking": True}}],
                    "SENT BACK", 1),
                   ("\"maybe\" beside a true one object down", [{"blocking": "maybe", "detail": {"blocking": True}}],
                    "SENT BACK", 1))
        unclear = (("null", [{"blocking": None}]), ("missing", [{}]), ("\"maybe\"", [{"blocking": "maybe"}]),
                   ("2", [{"blocking": 2}]), ("1.5", [{"blocking": 1.5}]), ("is_blocking", [{"is_blocking": True}]))

        def reply(case, tree, calls):
            findings = ["REQUIRED_NEW_BEHAVIOR is missing (%s, %s, call %d)." % (case, tree, number)
                        for number in range(1, len(calls) + 1)]
            return {"calls": [dict(fields, Findings=finding) for fields, finding in zip(calls, findings)]}, findings

        for tree in ("a change", "no change"):
            if tree == "no change":
                self.leave_unchanged()
            for case, calls, outcome, status in decided:
                answer, findings = reply(case, tree, calls)
                service = ModelStandIn([answer])
                self.addCleanup(service.close)
                finished = self.run_review(service)
                self.assertEqual(finished.returncode, status, (case, tree, finished.stdout, finished.stderr))
                self.assertIn("Review by fixture/reviewer: %s" % outcome, finished.stdout, (case, tree))
                for finding in findings:
                    self.assertIn(finding, finished.stdout, (case, tree))
                    self.assertIn(finding, self.review_log(), (case, tree))
            for case, calls in unclear:
                answer, findings = reply(case, tree, calls)
                # By default it is asked again, and the verdict that follows decides.
                service = ModelStandIn([answer, {"verdict": (True, "the verdict that came next")}])
                self.addCleanup(service.close)
                finished = self.run_review(service)
                self.assertEqual(finished.returncode, 1, (case, tree, finished.stdout, finished.stderr))
                self.assertIn("Review by fixture/reviewer: SENT BACK", finished.stdout, (case, tree))
                self.assertIn("the verdict that came next", finished.stdout, (case, tree))
                for finding in findings:
                    self.assertIn(finding, finished.stdout, (case, tree))
                    self.assertIn(finding, self.review_log(), (case, tree))
                    self.assertIn(finding, finished.stderr, (case, tree))
                # With the opt-in it is NOT REVIEWED; the work goes back only with no change.
                service = ModelStandIn([answer])
                self.addCleanup(service.close)
                finished = self.run_review(service, REVIEW_ATTEMPTS="1", **self.PASS)
                self.assertEqual(finished.returncode, 0 if tree == "a change" else 1, (case, tree, finished.stdout))
                self.assertIn("Review by fixture/reviewer: NOT REVIEWED", finished.stdout, (case, tree))
                self.assertIn("no verdict could be obtained", finished.stdout, (case, tree))
                for finding in findings:
                    self.assertIn(finding, finished.stdout, (case, tree))
                    self.assertIn(finding, self.review_log(), (case, tree))
                if tree == "no change":
                    self.assertIn(self.GOES_BACK, finished.stdout, case)

    def test_git_reads_neither_the_users_nor_the_systems_settings(self):
        # As for the delivery: a user's own settings could change what one of
        # the two reads, and they would disagree on whether anything changed.
        # Here they name a diff program that prints nothing. (An exclude file
        # they name is kept out by git() whether they are read or not.)
        home = self.home.parent / "user-home"
        home.mkdir()
        (home / ".gitconfig").write_text("[diff]\n\texternal = true\n")
        self.reviewed_as_it_is({"HOME": str(home)}, "+    return 2  # changed")

    def test_git_is_not_pointed_at_another_index_or_checkout(self):
        # V2 and V3 of the third review: an operator's GIT_INDEX_FILE, or a
        # GIT_DIR and GIT_WORK_TREE of a clean checkout elsewhere, in the
        # review's settings only. The delivery reads the workspace without
        # them, and so does the review now: an unchanged checkout is still
        # unchanged, and a new file in the workspace is shown, not hidden.
        self.leave_unchanged()
        other = self.home.parent / "other"
        subprocess.run(["git", "clone", "-q", str(self.workspace), str(other)], check=True, env=git_environment())
        service = ModelStandIn([{"status": 503}, {"status": 503}])
        self.addCleanup(service.close)
        # Without a verdict this branch's default would wait; the opt-in ends.
        finished = self.run_review(service, GIT_INDEX_FILE=str(self.home.parent / "an-index-of-its-own"), **self.PASS)
        self.assertEqual(finished.returncode, 1, finished.stdout)
        self.assertIn(self.GOES_BACK, finished.stdout)
        (self.workspace / "src" / "farewell.txt").write_text("a new file the reviewer must see\n")
        service = ModelStandIn([{"verdict": (False, "")}])
        self.addCleanup(service.close)
        finished = self.run_review(service, GIT_DIR=str(other / ".git"), GIT_WORK_TREE=str(other))
        self.assertEqual(finished.returncode, 0, finished.stdout)
        text = service.requests[0]["body"]["messages"][1]["content"]
        self.assertIn("--- new file src/farewell.txt ---\na new file the reviewer must see", text)
        self.assertNotIn("No file was changed", text)

    def test_git_takes_no_settings_from_the_environment(self):
        # G1 of the final review: settings handed to Git through the review's
        # environment only, an exclude file that hides the one new file, made
        # the reviewer hear that no file was changed while the delivery
        # delivered it. The review drops them now, as the delivery's Git on the
        # workspace does.
        self.leave_unchanged()
        hide = self.home.parent / "hide-farewell"
        hide.write_text("src/farewell.txt\n")
        (self.workspace / "src" / "farewell.txt").write_text("a new file the reviewer must see\n")
        for settings in ({"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "core.excludesFile",
                          "GIT_CONFIG_VALUE_0": str(hide)},
                         {"GIT_CONFIG_PARAMETERS": "'core.excludesFile'='%s'" % hide}):
            service = ModelStandIn([{"verdict": (False, "")}])
            self.addCleanup(service.close)
            finished = self.run_review(service, **settings)
            self.assertEqual(finished.returncode, 0, finished.stdout)
            text = service.requests[0]["body"]["messages"][1]["content"]
            self.assertIn("--- new file src/farewell.txt ---\na new file the reviewer must see", text)
            self.assertNotIn("No file was changed", text)

    def test_settings_in_the_environment_do_not_hide_a_change_of_mode(self):
        # git() keeps an exclude file out with -c whatever the environment
        # says; core.fileMode=false is kept out only by dropping the settings.
        self.leave_unchanged()
        (self.workspace / "src" / "tool.py").chmod(0o755)
        for settings in ({"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "core.fileMode",
                          "GIT_CONFIG_VALUE_0": "false"},
                         {"GIT_CONFIG_PARAMETERS": "'core.fileMode'='false'"}):
            self.reviewed_as_it_is(settings, "old mode 100644\nnew mode 100755")

    def reviewed_as_it_is(self, settings, seen):
        """The review with something in its environment only that would hide
        the change from its Git; the reviewer is shown the change all the same."""
        service = ModelStandIn([{"verdict": (False, "")}])
        self.addCleanup(service.close)
        finished = self.run_review(service, **settings)
        self.assertEqual(finished.returncode, 0, finished.stdout)
        text = service.requests[0]["body"]["messages"][1]["content"]
        self.assertIn(seen, text)
        self.assertNotIn("No file was changed", text)

    def end_lines_only(self):
        """The one change: src/tool.py with its line ends turned into CRLF."""
        self.leave_unchanged()
        (self.workspace / "src" / "tool.py").write_bytes(b"def run():\r\n    return 1\r\n")

    def test_git_reads_no_default_exclude_file(self):
        self.leave_unchanged()
        (self.workspace / "src" / "farewell.txt").write_text("a new file the reviewer must see\n")
        xdg = self.home.parent / "xdg"
        (xdg / "git").mkdir(parents=True)
        (xdg / "git" / "ignore").write_text("farewell.txt\n")
        self.reviewed_as_it_is({"XDG_CONFIG_HOME": str(xdg)}, "--- new file src/farewell.txt ---")

    def test_git_reads_no_default_attributes_file(self):
        self.end_lines_only()
        home = self.home.parent / "user-home"
        (home / ".config" / "git").mkdir(parents=True)
        (home / ".config" / "git" / "attributes").write_text("*.py text\n")
        self.reviewed_as_it_is({"HOME": str(home)}, "+def run():\r")

    def test_git_takes_no_attributes_from_elsewhere(self):
        self.end_lines_only()
        def git_in(*arguments, text):
            return subprocess.run(["git", "-C", str(self.workspace), *arguments], input=text, capture_output=True,
                                  text=True, check=True, env=git_environment()).stdout.strip()
        blob = git_in("hash-object", "-w", "--stdin", text="*.py text\n")
        tree = git_in("mktree", text="100644 blob %s\t.gitattributes\n" % blob)
        self.reviewed_as_it_is({"GIT_ATTR_SOURCE": tree}, "+def run():\r")

    def test_git_uses_no_diff_program_from_the_environment(self):
        self.reviewed_as_it_is({"GIT_EXTERNAL_DIFF": "true"}, "+    return 2  # changed")

    def test_git_carriage_returns_in_diffs_and_names_reach_the_reviewer_unchanged(self):
        self.end_lines_only()
        name = "src/notes\rname.txt"
        (self.workspace / name).write_text("contents of the file whose name contains CR\n")
        service = ModelStandIn([{"verdict": (False, "")}])
        self.addCleanup(service.close)
        finished = self.run_review(service)
        self.assertEqual(finished.returncode, 0, (finished.stdout, finished.stderr))
        text = service.requests[0]["body"]["messages"][1]["content"]
        self.assertIn("+def run():\r\n+    return 1\r\n", text)
        self.assertIn("--- new file %s ---\ncontents of the file whose name contains CR" % name, text)
        self.assertNotIn("src/notes\nname.txt", text)

    def test_only_git_gets_the_fixed_diagnostic_locale(self):
        shim = self.home.parent / "git-locale"
        shim.mkdir()
        (shim / "git").write_text(
            "#!/bin/sh\n"
            "if [ \"$LC_ALL\" != C ]; then echo 'unexpected git locale' >&2; exit 91; fi\n"
            "exec %s \"$@\"\n" % shutil.which("git"))
        (shim / "git").chmod(0o755)
        service = ModelStandIn([{"verdict": (False, "")}])
        self.addCleanup(service.close)
        command = sys.executable + " -c \"import os; print('test locale: ' + os.environ['LC_ALL'])\""
        # The command's locale is an operator choice. Git alone needs English
        # diagnostics; there is no dependency on installed translated locales.
        finished = self.run_review(service, **self.PASS, LC_ALL="C.UTF-8",
                                   PATH=str(shim) + os.pathsep + os.environ["PATH"], REVIEW_TEST_COMMANDS=command)
        self.assertEqual(finished.returncode, 0, (finished.stdout, finished.stderr))
        text = service.requests[0]["body"]["messages"][1]["content"]
        self.assertIn("test locale: C.UTF-8", text)

    def test_the_review_drops_what_the_delivery_drops_on_the_workspace(self):
        # The review's bundle carries no delivery_support, so each keeps its
        # own list of what it drops from Git's environment. Lists that drift
        # apart would let one stage's environment split the two again.
        def load(name, path):
            spec = importlib.util.spec_from_file_location(name, path)
            module = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(module)
            return module
        review = load("review_under_test", SCRIPT)
        delivery = load("delivery_support_under_test", SCRIPT.with_name("delivery_support.py"))
        delivery.workspace_as_reviewed = True
        probe = {name: "probe" for name in (
            "GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0", "GIT_CONFIG_KEY_12", "GIT_CONFIG_VALUE_12",
            "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_NOSYSTEM", "GIT_CONFIG",
            "GIT_CONFIG_KEY_", "GIT_CONFIG_KEY_A", "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR",
            "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_ATTR_SOURCE", "GIT_EXTERNAL_DIFF", "GIT_ASKPASS", "SSH_ASKPASS",
            "PATH")}
        with mock.patch.dict(os.environ, probe, clear=True):
            dropped_by_review = sorted(set(probe) - set(review.git_environment()))
            dropped_by_delivery = sorted(set(probe) - set(delivery.git_environment()))
        # Only the delivery's Git can ask for a credential, so only it drops
        # the programs that would ask a person for one.
        self.assertEqual(dropped_by_review, [name for name in dropped_by_delivery
                                             if name not in ("GIT_ASKPASS", "SSH_ASKPASS")])
        self.assertEqual(dropped_by_review, [
            "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_ATTR_SOURCE", "GIT_COMMON_DIR", "GIT_CONFIG_COUNT",
            "GIT_CONFIG_KEY_0", "GIT_CONFIG_KEY_12", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_VALUE_0",
            "GIT_CONFIG_VALUE_12", "GIT_DIR", "GIT_EXTERNAL_DIFF", "GIT_INDEX_FILE", "GIT_WORK_TREE"])

    def test_a_reply_in_another_shape_is_read_as_far_as_it_can_be_and_kept_as_written(self):
        # The second point of the third review: arguments given as an object,
        # a verdict wrapped one level down, arguments that are not JSON, words
        # instead of a call, and a broken call beside a readable one. The rule
        # is the same; what the reviewer wrote reaches the output and the log.
        # With the opt-in, so that a reply without a verdict ends the review;
        # by default it is asked again, which the last part shows.
        finding = "REQUIRED_NEW_BEHAVIOR is missing in src/tool.py"
        broken = '{"blocking": true, "findings": "%s' % finding
        cases = (("arguments as an object", {"raw_calls": [{"blocking": True, "findings": finding}]}, "SENT BACK", 1, 1),
                 ("a verdict one level down", {"calls": [{"verdict": {"blocking": True, "findings": finding}}]},
                  "SENT BACK", 1, 1),
                 ("arguments that are not JSON", {"raw_calls": [broken]}, "NOT REVIEWED", 0, 1),
                 ("words instead of a call", {"content": "BLOCKING: " + finding}, "NOT REVIEWED", 0, 1),
                 ("a broken call beside a false one", {"raw_calls": [broken, json.dumps({"blocking": False})]},
                  "PASSED", 0, 0),
                 # A true one level down counts beside the call's own false, with
                 # the findings written next to it.
                 ("a false beside a true one level down",
                  {"calls": [{"blocking": False, "verdict": {"blocking": True, "findings": finding}}]}, "SENT BACK", 1, 1),
                 # Anything else one level down stands in only for a call that
                 # names no blocking itself.
                 ("a blocking that cannot be read beside a false one level down",
                  {"calls": [{"blocking": "Yes, because the needed file is missing", "findings": finding,
                              "issues": {"blocking": False}}]}, "NOT REVIEWED", 0, 1),
                 ("a null blocking beside a false one level down",
                  {"calls": [{"blocking": None, "findings": finding, "detail": {"blocking": False}}]},
                  "NOT REVIEWED", 0, 1),
                 ("no blocking, and a false one level down",
                  {"calls": [{"findings": finding, "notes": {"blocking": False}}]}, "PASSED", 0, 0))
        for tree in ("a change", "no change"):
            if tree == "no change":
                self.leave_unchanged()
            for case, reply, outcome, changed, unchanged in cases:
                service = ModelStandIn([reply])
                self.addCleanup(service.close)
                finished = self.run_review(service, REVIEW_ATTEMPTS="1", **self.PASS)
                status = changed if tree == "a change" else unchanged
                self.assertEqual(finished.returncode, status, (case, tree, finished.stdout, finished.stderr))
                self.assertIn("Review by fixture/reviewer: %s" % outcome, finished.stdout, (case, tree))
                self.assertIn(finding, finished.stdout, (case, tree))
                # On this branch a reply without a verdict is logged as it comes.
                self.assertIn(finding, self.review_log(), (case, tree))
        # Kept as written, but no more than findings are shown.
        service = ModelStandIn([{"content": "x" * 20000}])
        self.addCleanup(service.close)
        finished = self.run_review(service, REVIEW_ATTEMPTS="1", **self.PASS)
        self.assertIn("[reply cut here: 6000 of 20000 characters shown]", self.review_log())
        # By default, words instead of a call are asked again; they stay in the
        # live view while waiting and in the record the verdict ends with.
        service = ModelStandIn([{"content": "BLOCKING: " + finding}, {"verdict": (True, "the verdict that came next")}])
        self.addCleanup(service.close)
        finished = self.run_review(service)
        self.assertEqual(finished.returncode, 1, finished.stdout)
        self.assertIn("the verdict that came next", finished.stdout)
        for record in (finished.stdout, finished.stderr, self.review_log()):
            self.assertIn("BLOCKING: " + finding, record)

    def test_a_name_that_is_not_utf8_is_named_not_a_traceback(self):
        # macOS refuses such a name, so a Git ahead on PATH lists one as Linux
        # Git does, with the name's own bytes, and leaves the rest to Git.
        shim = self.home.parent / "git-shim"
        shim.mkdir()
        (shim / "git").write_text(
            "#!/bin/sh\n"
            "for a in \"$@\"; do if [ \"$a\" = status ]; then printf '?? src/\\202\\240.go\\0'; exit 0; fi; done\n"
            "exec %s \"$@\"\n" % shutil.which("git"))
        (shim / "git").chmod(0o755)
        service = ModelStandIn([{"verdict": (False, "")}])
        self.addCleanup(service.close)
        finished = self.run_review(service, PATH=str(shim) + os.pathsep + os.environ["PATH"])
        self.assertEqual(finished.returncode, 0, (finished.stdout, finished.stderr))
        self.assertNotIn("Traceback", finished.stderr)
        self.assertIn("PASSED", finished.stdout)
        self.assertIn("new path src/��.go", service.requests[0]["body"]["messages"][1]["content"])

    def test_a_new_file_with_a_japanese_name_reaches_the_reviewer(self):
        self.leave_unchanged()
        (self.workspace / "src" / "設定.txt").write_text("元の設定\n", encoding="utf-8")
        self.git("add", "-A")
        self.git("commit", "-m", "Codex: a tracked file with a Japanese name")
        (self.workspace / "src" / "設定.txt").write_text("変えた設定\n", encoding="utf-8")
        (self.workspace / "src" / "メモ.md").write_text("日本語の覚え書き\n", encoding="utf-8")
        service = ModelStandIn([{"verdict": (False, "")}])
        self.addCleanup(service.close)
        finished = self.run_review(service)
        self.assertEqual(finished.returncode, 0, finished.stdout)
        text = service.requests[0]["body"]["messages"][1]["content"]
        self.assertIn("--- new file src/メモ.md ---\n日本語の覚え書き", text)
        self.assertIn("diff --git a/src/設定.txt b/src/設定.txt", text)
        self.assertIn("+変えた設定", text)
        self.assertNotIn("No file was changed", text)

    def test_a_new_symbolic_link_is_named_not_called_no_change(self):
        self.leave_unchanged()
        os.symlink("nowhere", self.workspace / "src" / "link")
        service = ModelStandIn([{"verdict": (False, "")}])
        self.addCleanup(service.close)
        finished = self.run_review(service)
        self.assertEqual(finished.returncode, 0, finished.stdout)
        text = service.requests[0]["body"]["messages"][1]["content"]
        self.assertIn("--- new symbolic link src/link -> nowhere ---", text)
        self.assertNotIn("No file was changed", text)

    def test_a_workspace_that_is_not_a_checkout_holds_and_is_looked_at_again(self):
        shutil.rmtree(self.workspace / ".git")
        service = ModelStandIn([{"verdict": (True, "x")}])
        self.addCleanup(service.close)
        running, stdout, stderr = self.held_review(service)
        self.assertTrue(running, (stdout, stderr))
        self.assertEqual(stderr.count("the change could not be read"), 1, stderr)
        # One line of Git's in the reason, not the usage text Git prints
        # after it, so the live view shows the hold on one line.
        held = [line for line in stderr.splitlines() if "the change could not be read" in line]
        self.assertIn("this is looked at again every", held[0], stderr)
        self.assertNotIn("usage:", stderr)
        self.assertEqual(service.requests, [])

    def test_the_reason_from_git_is_its_fatal_line_not_a_warning_before_it(self):
        # A warning Git prints first, such as a file of its own it cannot read
        # in a sandbox, is not why the change could not be read.
        shim = self.home.parent / "git-shim"
        shim.mkdir()
        (shim / "git").write_text(
            "#!/bin/sh\n"
            "for a in \"$@\"; do if [ \"$a\" = diff ]; then\n"
            "  echo \"warning: unable to access 'attributes': Permission denied\" >&2\n"
            "  echo 'fatal: bad object HEAD' >&2; exit 128; fi; done\n"
            "exec %s \"$@\"\n" % shutil.which("git"))
        (shim / "git").chmod(0o755)
        service = ModelStandIn([{"verdict": (False, "")}])
        self.addCleanup(service.close)
        running, stdout, stderr = self.held_review(service, PATH=str(shim) + os.pathsep + os.environ["PATH"])
        self.assertTrue(running, (stdout, stderr))
        self.assertIn("git diff exited 128: fatal: bad object HEAD. The work is neither", stderr)
        self.assertNotIn("Permission denied", stderr)
        self.assertEqual(service.requests, [])

    def test_with_the_opt_in_a_workspace_that_is_not_a_checkout_goes_back_with_a_note(self):
        # Without a checkout whether anything changed cannot be told, so even
        # the opt-in lets no missing verdict through. The delivery could
        # deliver nothing from here either, so the request goes back to work.
        shutil.rmtree(self.workspace / ".git")
        service = ModelStandIn([{"verdict": (True, "x")}])
        self.addCleanup(service.close)
        finished = self.run_review(service, **self.PASS)
        self.assertEqual(finished.returncode, 1, finished.stderr)
        self.assertNotIn("Traceback", finished.stderr)
        self.assertIn("could not be read", finished.stdout)
        self.assertIn("NOT REVIEWED", finished.stdout)
        self.assertIn("Whether any file was changed could not be told", finished.stdout)
        missing = self.run_review(service, TASK_WORKSPACE=str(self.workspace / "no-such-directory"), **self.PASS)
        self.assertEqual(missing.returncode, 1, missing.stdout)
        self.assertIn("Whether any file was changed could not be told", missing.stdout)
        self.assertEqual(service.requests, [])

    CANNOT_TELL = ("Whether any file was changed could not be told, and an ending with nothing delivered needs a"
                   " verdict, so the work goes back this time.")

    def test_while_asking_again_the_log_tells_what_the_model_last_said(self):
        # A reply without a plain verdict is written to review.md as it
        # comes, so whoever looks while the review asks again can read it.
        finding = "REQUIRED_NEW_BEHAVIOR is missing; the model said so without a verdict"
        service = ModelStandIn({"fixture/reviewer": {"arguments": {"blocking": None, "findings": finding}}})
        self.addCleanup(service.close)
        running, stdout, stderr = self.held_review(service)
        self.assertTrue(running, (stdout, stderr))
        self.assertIn(finding, self.review_log())
        self.assertIn("no verdict yet", self.review_log())
        self.assertEqual(self.review_log().count(finding), 1, "logged once, not at every request")

    def test_output_that_is_not_utf8_is_read_and_the_review_happens(self):
        # A change and a test command's output in Shift_JIS reach the
        # reviewer with replacement characters, instead of an error the review
        # would start again from without ever asking.
        (self.workspace / "src" / "tool.py").write_bytes("def run():\n    return '設定'\n".encode("shift_jis"))
        service = ModelStandIn([{"verdict": (True, "the encoding of src/tool.py changed")}])
        self.addCleanup(service.close)
        command = sys.executable + " -c \"import sys; sys.stdout.buffer.write(b'\\x90\\xdd\\x92\\xe8 test ran')\""
        finished = self.run_review(service, REVIEW_TEST_COMMANDS=command)
        self.assertEqual(finished.returncode, 1, (finished.stdout, finished.stderr))
        text = service.requests[0]["body"]["messages"][1]["content"]
        self.assertIn("\ufffd", text)
        self.assertIn("test ran", text)
        self.assertNotIn("Unexpected", finished.stderr)

    def test_carriage_returns_in_the_test_output_end_lines(self):
        # A test tool that redraws its progress with CR, or ends its lines with
        # CRLF, reaches the reviewer one step a line, as when the output was
        # read as text.
        service = ModelStandIn([{"verdict": (False, "")}])
        self.addCleanup(service.close)
        command = (sys.executable + " -c \"import sys; sys.stdout.buffer.write(b'progress 1\\rprogress 2\\r\\ndone\\r\\n');"
                   " sys.stderr.buffer.write(b'a warning\\r\\n')\"")
        finished = self.run_review(service, REVIEW_TEST_COMMANDS=command)
        self.assertEqual(finished.returncode, 0, (finished.stdout, finished.stderr))
        text = service.requests[0]["body"]["messages"][1]["content"]
        self.assertIn("progress 1\nprogress 2\ndone\na warning", text)
        self.assertNotIn("\r", text)

    def test_an_unexpected_error_is_waited_out_said_again_and_the_tests_are_not_run_again(self):
        # The review starts again at the growing waits, says so again at
        # the hold interval, and does not run the operator's test commands at
        # every start.
        service = ModelStandIn([{"verdict": (False, "")}])
        self.addCleanup(service.close)
        tests_run = []

        def count_tests(command):
            if command[:1] == [sys.executable]:
                tests_run.append(command)

        def before_main(module):
            real, failures = module.verdict_until_given, []

            def failing(*arguments):
                if len(failures) < 8:
                    failures.append(1)
                    raise RuntimeError("an error the review does not expect")
                return real(*arguments)
            module.verdict_until_given = failing

        self.before_main = before_main
        status, printed, said = self.review_in_process(service, count_tests)
        self.assertEqual(status, 0, (printed, said))
        self.assertIn("PASSED", printed)
        self.assertEqual(len(tests_run), 1, tests_run)
        self.assertEqual(said.count("Unexpected RuntimeError"), 1, said)
        self.assertIn("Review still starting again at", said)

    def test_an_answer_the_operator_has_to_fix_is_said_with_the_services_words_and_again_while_waiting(self):
        # A 404 for a mistyped model id, with what the service said (the
        # credential scrubbed), named as the operator's to fix, and said again
        # at the hold interval; another model in REVIEW_MODELS is still asked.
        service = ModelStandIn({"fixture/reviewer": {"status": 404, "error": "not a valid model ID; key " + KEY}})
        self.addCleanup(service.close)
        running, stdout, stderr = self.held_review(service)
        self.assertTrue(running, (stdout, stderr))
        self.assertIn("HTTP 404 from the model service: {\"error\": \"not a valid model ID; key [credential]\"}", stderr)
        self.assertIn("this is for the operator to fix", stderr)
        self.assertIn("Review still without a verdict at", stderr)
        self.assertNotIn(KEY, stdout + stderr + self.review_log())
        service = ModelStandIn({"maker-a/first": {"status": 401, "error": "no such key"},
                                "maker-b/second": [{"verdict": (True, "src/tool.py returns 2")}]})
        self.addCleanup(service.close)
        finished = self.run_review(service, REVIEW_MODELS="maker-a/first, maker-b/second")
        self.assertEqual(finished.returncode, 1, (finished.stdout, finished.stderr))
        self.assertIn("Review by maker-b/second: SENT BACK", finished.stdout)

    def test_the_opt_in_is_read_as_written_and_another_value_holds(self):
        # "pass" in any letter case and with spaces around it is the
        # opt-in; another value is named and holds, never quietly the default.
        service = ModelStandIn([{"status": 503}, {"status": 503}])
        self.addCleanup(service.close)
        finished = self.run_review(service, REVIEW_UNAVAILABLE=" Pass ")
        self.assertEqual(finished.returncode, 0, finished.stdout)
        self.assertIn("NOT REVIEWED", finished.stdout)
        running, stdout, stderr = self.held_review(service, REVIEW_UNAVAILABLE="yes")
        self.assertTrue(running, (stdout, stderr))
        self.assertIn("REVIEW_UNAVAILABLE is 'yes'; it may be pass, or left unset", stderr)
        self.assertIn("fix the setting and restart the engine", stderr)

    def test_a_model_named_twice_is_asked_once_a_round(self):
        # A model named twice is asked once a round: a repeat in the list
        # would only send it more requests, not give the review another chance.
        service = ModelStandIn({"maker-a/first": {"status": 503},
                                "maker-b/second": [{"verdict": None}, {"verdict": (False, "")}]})
        self.addCleanup(service.close)
        finished = self.run_review(service, REVIEW_MODELS="maker-a/first, maker-b/second, maker-a/first")
        self.assertEqual(finished.returncode, 0, (finished.stdout, finished.stderr))
        self.assertEqual(service.models_asked(), ["maker-a/first", "maker-b/second"] * 2)

    def test_a_review_that_cannot_tell_whether_anything_changed_lets_nothing_through_without_a_verdict(self):
        # Case B of the third review: the review's git status runs out of its
        # 60 seconds while the delivery, which waits up to 600, reads the same
        # unchanged checkout and ends with nothing delivered. By default the
        # review holds and asks again; with the opt-in, not being able to tell
        # is not taken as a change, and neither is an error before it was
        # told: without a verdict the work goes back. Once the change is read,
        # the reading that showed it decides, so a first look that failed
        # does not leave the reviewer handed "(no change)" (R1 of the last
        # review, where a look after the reading failed in turn). The control,
        # with Git answering, says that no file was changed.
        self.leave_unchanged()

        def status_runs_out_of_time(*which):
            """The status calls that run out of time, counted from 1; all of
            them when none is named."""
            seen = []

            def before(command):
                if command[:1] == ["git"] and "status" in command:
                    seen.append(command)
                    if not which or len(seen) in which:
                        raise subprocess.TimeoutExpired(command, 60)
            return before

        def unexpected_error_first():
            failed = []

            def before(command):
                if command[:1] == ["git"] and "status" in command and not failed:
                    failed.append(command)
                    raise RuntimeError("an error the review does not expect")
            return before

        # With the whole tree as the change (no REVIEW_DIFF_PATHS), the review
        # reads status first to tell, then once as it reads the change; there
        # is no look after that, so the third call that R1 failed is never made.
        cases = (("git status always runs out of time", status_runs_out_of_time(), [], False, 1,
                  ["timed out after 60 seconds", self.CANNOT_TELL]),
                 ("an unexpected error first", unexpected_error_first(), [], False, 1,
                  ["Unexpected RuntimeError", self.CANNOT_TELL]),
                 ("the first look runs out of time, then no verdict", status_runs_out_of_time(1, 3),
                  [{"status": 503}, {"status": 503}], False, 1,
                  ["no file was changed, and an ending with nothing delivered needs a verdict"]),
                 ("the first look runs out of time, then a passing verdict", status_runs_out_of_time(1, 3),
                  [{"verdict": (False, "")}], False, 0, ["PASSED"]),
                 ("the first look runs out of time, then a blocking verdict not saved", status_runs_out_of_time(1, 3),
                  [{"verdict": (True, "the request needs a new file")}], True, 1,
                  ["SENT BACK", "the outcome above stands"]))
        for case, before, replies, unsaved, expected, said in cases:
            service = ModelStandIn(replies)
            self.addCleanup(service.close)
            if unsaved:
                self.home.chmod(0o500)
            try:
                status, printed, _ = self.review_in_process(service, before, REVIEW_DIFF_PATHS="", **self.PASS)
            finally:
                self.home.chmod(0o700)
            self.assertEqual(status, expected, (case, printed))
            for text in said:
                self.assertIn(text, printed, case)
            if case.startswith("the first look"):
                # Decided from the reading of the change: the reviewer is
                # asked whether the request is met with no file changed.
                self.assertIn(self.NO_CHANGE, service.requests[0]["body"]["messages"][1]["content"], case)
                self.assertNotIn("could not be told", printed, case)
        service = ModelStandIn([{"status": 503}, {"status": 503}])
        self.addCleanup(service.close)
        status, printed, _ = self.review_in_process(service, lambda command: None, **self.PASS)
        self.assertEqual(status, 1, printed)
        self.assertIn("no file was changed, and an ending with nothing delivered needs a verdict", printed)
        self.assertNotIn("could not be told", printed)


if __name__ == "__main__":
    unittest.main()
