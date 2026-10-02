"""A real Git checkout, real test commands, and a local service standing in for
the model: what the review command sends, what it writes, and how it ends."""
import http.server
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import threading
import time
import unittest

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
                elif reply.get("verdict") is None:
                    payload = json.dumps({"choices": [{"message": {"role": "assistant", "content": "I have looked."}}]}).encode()
                    self.send_response(200)
                else:
                    blocking, findings = reply["verdict"]
                    payload = json.dumps({"choices": [{"message": {"role": "assistant", "tool_calls": [
                        {"id": "call-1", "type": "function", "function": {"name": "verdict", "arguments": json.dumps(
                            {"blocking": blocking, "findings": findings})}}]}}]}).encode()
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
            self.assertIn("maker-a/first: HTTP 503 from the model service; maker-b/second: the reviewer returned no verdict",
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
        self.assertIn("this is looked at again every", stderr)
        self.assertEqual(service.requests, [])

    def test_with_the_opt_in_a_workspace_that_is_not_a_checkout_lets_the_work_through_with_a_note(self):
        shutil.rmtree(self.workspace / ".git")
        service = ModelStandIn([{"verdict": (True, "x")}])
        self.addCleanup(service.close)
        finished = self.run_review(service, **self.PASS)
        self.assertEqual(finished.returncode, 0, finished.stderr)
        self.assertNotIn("Traceback", finished.stderr)
        self.assertIn("could not be read", finished.stdout)
        self.assertIn("NOT REVIEWED", finished.stdout)
        self.assertEqual(service.requests, [])


if __name__ == "__main__":
    unittest.main()
