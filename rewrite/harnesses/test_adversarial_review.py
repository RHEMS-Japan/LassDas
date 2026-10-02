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
    """Answers chat completions with scripted verdicts and records every request."""

    def __init__(self, replies):
        self.replies = list(replies)
        self.requests = []
        owner = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_POST(self):
                length = int(self.headers.get("Content-Length", "0"))
                body = json.loads(self.rfile.read(length).decode("utf-8"))
                owner.requests.append({"path": self.path, "authorization": self.headers.get("Authorization", ""),
                                       "body": body})
                reply = owner.replies.pop(0) if owner.replies else {"status": 200, "verdict": (False, "")}
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

    def environment(self, service, **extra):
        environment = {"PATH": os.environ["PATH"], "LANG": "C.UTF-8", "PYTHONDONTWRITEBYTECODE": "1",
                       "TASK_WORKSPACE": str(self.workspace), "TASK_HOME": str(self.home), "REVIEW_MODEL_URL": service.url,
                       "REVIEW_MODEL": "fixture/reviewer", "REVIEW_KEY_ENV": "REVIEW_API_KEY", "REVIEW_API_KEY": KEY,
                       "REVIEW_TEST_COMMANDS": sys.executable + " -c \"print('tests ran fine')\"",
                       "REVIEW_DIFF_PATHS": "src tests", "REVIEW_ATTEMPTS": "2",
                       "REVIEW_TIMEOUT_SECONDS": "30"}
        environment.update(extra)
        return environment

    def run_review(self, service, stdin_text="Current assignment:\nStage 4 of 8\n\nOriginal request:\nadd a thing\n", **extra):
        return subprocess.run([sys.executable, "-B", str(SCRIPT)], input=stdin_text, capture_output=True,
                              text=True, env=self.environment(service, **extra), timeout=120)

    def review_in_process(self, service, before_each_command, **extra):
        """The review run in this process, so that a Git that runs out of
        time can be had without waiting for it: before_each_command sees each
        command the review starts and may raise in its place. The exit status
        and what the review printed."""
        spec = importlib.util.spec_from_file_location("review_under_test", SCRIPT)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)

        def run(command, *arguments, **keywords):
            before_each_command(command)
            return subprocess.run(command, *arguments, **keywords)

        printed = io.StringIO()
        stdin = io.TextIOWrapper(io.BytesIO(b"Original request:\nadd a thing\n"))
        with mock.patch.object(module, "subprocess", types.SimpleNamespace(
                run=run, TimeoutExpired=subprocess.TimeoutExpired)), \
                mock.patch.dict(os.environ, self.environment(service, **extra), clear=True), \
                mock.patch.object(sys, "stdin", stdin), contextlib.redirect_stdout(printed):
            status = module.main()
        return status, printed.getvalue()

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

    def test_a_pass_or_a_missing_verdict_does_not_consume_the_send_back_budget(self):
        service = ModelStandIn([{"status": 500}, {"verdict": None}, {"verdict": (False, "")}, {"verdict": (True, "a real defect")}])
        self.addCleanup(service.close)
        self.assertEqual(self.run_review(service).returncode, 0)   # no verdict: through, not counted
        self.assertEqual(self.run_review(service).returncode, 0)   # a pass: not counted
        self.assertFalse((self.home / "review-send-backs").exists())
        finished = self.run_review(service)                          # the first real objection still sends back
        self.assertEqual(finished.returncode, 1, finished.stdout)
        self.assertEqual((self.home / "review-send-backs").read_text(), "1")

    def test_no_verdict_or_a_failing_service_lets_the_work_through_with_a_note(self):
        service = ModelStandIn([{"status": 500}, {"verdict": None}])
        self.addCleanup(service.close)
        finished = self.run_review(service)
        self.assertEqual(finished.returncode, 0, finished.stderr)
        self.assertIn("no verdict could be obtained", self.review_log())
        self.assertEqual(len(service.requests), 2)

    def test_the_credential_reaches_only_the_model_service_and_never_the_output(self):
        service = ModelStandIn([{"status": 502, "error": "bad key " + KEY}, {"status": 502, "error": "bad key " + KEY}])
        self.addCleanup(service.close)
        finished = self.run_review(service)
        self.assertEqual(finished.returncode, 0, finished.stderr)
        self.assertEqual(service.requests[0]["authorization"], "Bearer " + KEY)
        for text in (finished.stdout, finished.stderr, self.review_log()):
            self.assertNotIn(KEY, text)

    def test_a_setting_that_cannot_be_used_never_sends_the_work_round_and_says_so(self):
        # A mistyped setting is not a defect in the change: the command ends 0,
        # prints NOT REVIEWED with the setting's name, calls nothing and writes
        # nothing into the workspace. Exit 1 is reserved for a real send-back,
        # which in an ordered run would otherwise repeat for ever.
        service = ModelStandIn([{"verdict": (True, "x")}])
        self.addCleanup(service.close)
        for name, value in (("REVIEW_MODEL_URL", ""),
                            ("REVIEW_ATTEMPTS", "many"), ("TASK_HOME", ""), ("TASK_HOME", str(self.workspace / "src" / "tool.py" / "x")),
                            ("REVIEW_TEST_COMMANDS", "echo 'unterminated"), ("REVIEW_DIFF_PATHS", "app lib")):
            finished = self.run_review(service, **{name: value})
            self.assertEqual(finished.returncode, 0, (name, finished.stdout, finished.stderr))
            self.assertIn("NOT REVIEWED", finished.stdout, name)
            self.assertIn(name, finished.stdout, name)
            self.assertNotIn("Traceback", finished.stderr, name)
        (self.home / "review-send-backs").write_text("not a number")
        finished = self.run_review(service)
        self.assertEqual(finished.returncode, 0, finished.stderr)
        self.assertIn("NOT REVIEWED", finished.stdout)
        self.assertNotIn("Traceback", finished.stderr)
        self.assertEqual(service.requests, [])
        self.assertFalse((self.workspace / "report").exists())

    def test_paths_that_match_no_change_are_said_so_instead_of_passing_blind(self):
        service = ModelStandIn([{"verdict": (False, "")}])
        self.addCleanup(service.close)
        finished = self.run_review(service, REVIEW_DIFF_PATHS="app lib")
        self.assertEqual(finished.returncode, 0, finished.stderr)
        self.assertIn("matched no change", finished.stdout)
        self.assertIn("src", finished.stdout)
        self.assertNotIn("PASSED", finished.stdout)
        self.assertEqual(service.requests, [])

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

    def test_an_unexpected_error_and_a_stray_byte_never_end_with_a_traceback_or_status_one(self):
        service = ModelStandIn([{"verdict": (False, "")}])
        self.addCleanup(service.close)
        # An unexpected exception (urlsplit refuses this URL) is caught at the top; it makes no request.
        finished = self.run_review(service, REVIEW_MODEL_URL="https://[oops/v1/chat/completions")
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

    def test_the_credential_is_sent_only_over_https_or_loopback(self):
        service = ModelStandIn([{"verdict": (True, "x")}])
        self.addCleanup(service.close)
        finished = self.run_review(service, REVIEW_MODEL_URL="http://model.example.invalid/v1/chat/completions")
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

    def test_with_nothing_changed_no_verdict_sends_the_work_back(self):
        # Let through, an unchanged checkout can end with nothing delivered,
        # so only a verdict lets it through: a service that is down, a reply
        # without a verdict, a setting that keeps the review from running and
        # an unexpected error all send it back instead.
        self.leave_unchanged()
        for case, replies, extra in (
                ("service down", [{"status": 503}, {"status": 503}], {}),
                ("text only", [{"verdict": None}, {"verdict": None}], {}),
                ("no credential", [], {"REVIEW_API_KEY": ""}),
                ("no endpoint", [], {"REVIEW_MODEL_URL": ""}),
                ("unexpected error", [], {"REVIEW_MODEL_URL": "https://[oops/v1/chat/completions"})):
            service = ModelStandIn(replies)
            self.addCleanup(service.close)
            finished = self.run_review(service, **extra)
            self.assertEqual(finished.returncode, 1, (case, finished.stdout, finished.stderr))
            self.assertIn("NOT REVIEWED", finished.stdout, case)
            self.assertIn(self.GOES_BACK, finished.stdout, case)
            self.assertNotIn("Traceback", finished.stderr, case)
        self.assertFalse((self.home / "review-send-backs").exists(), "no verdict was counted as a send-back")

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
        self.assertIn("could not be saved", sent_back.stdout)
        passed = self.run_review(service)
        self.assertEqual(passed.returncode, 0, passed.stdout)
        self.assertIn("PASSED", passed.stdout)

    def test_with_a_change_or_a_committed_round_no_verdict_goes_on_as_before(self):
        for case in ("a change", "a committed round"):
            if case == "a committed round":
                self.leave_unchanged()
                receipt = self.workspace / ".git" / "ticket-engine" / "delivery.json"
                receipt.parent.mkdir()
                receipt.write_text(json.dumps({"head": self.git("rev-parse", "HEAD").stdout.strip()}))
            service = ModelStandIn([{"status": 503}, {"status": 503}])
            self.addCleanup(service.close)
            finished = self.run_review(service)
            self.assertEqual(finished.returncode, 0, (case, finished.stdout))
            self.assertIn("the work goes on unreviewed this time", finished.stdout, case)

    def test_blocking_is_read_when_its_meaning_is_plain_and_the_findings_always_stay(self):
        # REPRO 07 printed blocking null beside a finding that a needed change
        # is missing as PASSED; read as true or false only, "true" and 1 then
        # let a change through unreviewed with the findings gone. Every call
        # and every blocking field in any letter case is read: one plain true
        # sends back, else one plain false passes, else there is no verdict.
        # What the reviewer wrote stays in the output and the log every time.
        sent_back, passed, none = ("SENT BACK", 1, 1), ("PASSED", 0, 0), ("NOT REVIEWED", 0, 1)
        cases = (("\"true\"", [{"blocking": "true"}], sent_back),
                 ("\" TRUE \"", [{"blocking": " TRUE "}], sent_back),
                 ("1", [{"blocking": 1}], sent_back),
                 ("\"yes\"", [{"blocking": "yes"}], sent_back),
                 ("\"1\"", [{"blocking": "1"}], sent_back),
                 ("Blocking", [{"Blocking": True}], sent_back),
                 ("false and Blocking true", [{"blocking": False, "Blocking": True}], sent_back),
                 ("false, then true", [{"blocking": False}, {"blocking": True}], sent_back),
                 ("true, then false", [{"blocking": True}, {"blocking": False}], sent_back),
                 ("\"false\"", [{"blocking": "false"}], passed),
                 ("0", [{"blocking": 0}], passed),
                 ("\"no\"", [{"blocking": "no"}], passed),
                 ("\"0\"", [{"blocking": "0"}], passed),
                 ("false and Blocking maybe", [{"blocking": False, "Blocking": "maybe"}], passed),
                 ("null", [{"blocking": None}], none),
                 ("missing", [{}], none),
                 ("\"maybe\"", [{"blocking": "maybe"}], none),
                 ("2", [{"blocking": 2}], none),
                 ("1.5", [{"blocking": 1.5}], none),
                 ("is_blocking", [{"is_blocking": True}], none))
        for tree in ("a change", "no change"):
            if tree == "no change":
                self.leave_unchanged()
            for case, calls, (outcome, changed, unchanged) in cases:
                findings = ["REQUIRED_NEW_BEHAVIOR is missing (%s, %s, call %d)." % (case, tree, number)
                            for number in range(1, len(calls) + 1)]
                service = ModelStandIn([{"calls": [dict(fields, Findings=finding)
                                                   for fields, finding in zip(calls, findings)]}])
                self.addCleanup(service.close)
                finished = self.run_review(service, REVIEW_ATTEMPTS="1")
                status = changed if tree == "a change" else unchanged
                self.assertEqual(finished.returncode, status, (case, tree, finished.stdout, finished.stderr))
                self.assertIn("Review by fixture/reviewer: %s" % outcome, finished.stdout, (case, tree))
                for finding in findings:
                    self.assertIn(finding, finished.stdout, (case, tree))
                    self.assertIn(finding, self.review_log(), (case, tree))
                if outcome == "NOT REVIEWED":
                    self.assertIn("no verdict could be obtained", finished.stdout, (case, tree))
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
        finished = self.run_review(service, GIT_INDEX_FILE=str(self.home.parent / "an-index-of-its-own"))
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
        finding = "REQUIRED_NEW_BEHAVIOR is missing in src/tool.py"
        broken = '{"blocking": true, "findings": "%s' % finding
        cases = (("arguments as an object", {"raw_calls": [{"blocking": True, "findings": finding}]}, "SENT BACK", 1, 1),
                 ("a verdict one level down", {"calls": [{"verdict": {"blocking": True, "findings": finding}}]},
                  "SENT BACK", 1, 1),
                 ("arguments that are not JSON", {"raw_calls": [broken]}, "NOT REVIEWED", 0, 1),
                 ("words instead of a call", {"content": "BLOCKING: " + finding}, "NOT REVIEWED", 0, 1),
                 ("a broken call beside a false one", {"raw_calls": [broken, json.dumps({"blocking": False})]},
                  "PASSED", 0, 0))
        for tree in ("a change", "no change"):
            if tree == "no change":
                self.leave_unchanged()
            for case, reply, outcome, changed, unchanged in cases:
                service = ModelStandIn([reply])
                self.addCleanup(service.close)
                finished = self.run_review(service, REVIEW_ATTEMPTS="1")
                status = changed if tree == "a change" else unchanged
                self.assertEqual(finished.returncode, status, (case, tree, finished.stdout, finished.stderr))
                self.assertIn("Review by fixture/reviewer: %s" % outcome, finished.stdout, (case, tree))
                self.assertIn(finding, finished.stdout, (case, tree))
                self.assertIn(finding, self.review_log().split("## Review by")[-1], (case, tree))
        # Kept as written, but no more than findings are shown.
        service = ModelStandIn([{"content": "x" * 20000}])
        self.addCleanup(service.close)
        finished = self.run_review(service, REVIEW_ATTEMPTS="1")
        self.assertIn("[reply cut here: 6000 of 20000 characters shown]", self.review_log())

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
        service = ModelStandIn([{"status": 503}, {"status": 503}])
        self.addCleanup(service.close)
        finished = self.run_review(service)
        text = service.requests[0]["body"]["messages"][1]["content"]
        self.assertIn("--- new symbolic link src/link -> nowhere ---", text)
        self.assertNotIn("No file was changed", text)
        # Something did change, so a missing verdict is no reason to hold it back here.
        self.assertEqual(finished.returncode, 0, finished.stdout)

    def test_a_workspace_that_is_not_a_checkout_goes_back_with_a_note(self):
        # Without a checkout whether anything changed cannot be told, so no
        # missing verdict lets the work through. The delivery could deliver
        # nothing from here either, so the request goes back to work as before.
        shutil.rmtree(self.workspace / ".git")
        service = ModelStandIn([{"verdict": (True, "x")}])
        self.addCleanup(service.close)
        finished = self.run_review(service)
        self.assertEqual(finished.returncode, 1, finished.stderr)
        self.assertNotIn("Traceback", finished.stderr)
        self.assertIn("could not be read", finished.stdout)
        self.assertIn("NOT REVIEWED", finished.stdout)
        self.assertIn("Whether any file was changed could not be told", finished.stdout)
        missing = self.run_review(service, TASK_WORKSPACE=str(self.workspace / "no-such-directory"))
        self.assertEqual(missing.returncode, 1, missing.stdout)
        self.assertIn("Whether any file was changed could not be told", missing.stdout)
        self.assertEqual(service.requests, [])

    CANNOT_TELL = ("Whether any file was changed could not be told, and an ending with nothing delivered needs a"
                   " verdict, so the work goes back this time.")

    def test_a_review_that_cannot_tell_whether_anything_changed_lets_nothing_through_without_a_verdict(self):
        # Case B of the third review: the review's git status runs out of its
        # 60 seconds while the delivery, which waits up to 600, reads the same
        # unchanged checkout and ends with nothing delivered. Not being able
        # to tell is not taken as a change, and neither is an error before it
        # was told: without a verdict the work goes back, and a blocking
        # verdict stands even when its state cannot be saved. When Git reads
        # the change after the first look failed, it is told again, and the
        # reviewer hears that no file was changed. The control, with Git
        # answering, says that no file was changed.
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
        # reads status first to tell, then once with the change, and once more
        # to tell again after a failed first look.
        cases = (("git status always runs out of time", status_runs_out_of_time(), [], False, 1,
                  ["timed out after 60 seconds", self.CANNOT_TELL]),
                 ("an unexpected error first", unexpected_error_first(), [], False, 1,
                  ["Unexpected RuntimeError", self.CANNOT_TELL]),
                 ("the first look and the second telling run out of time, then no verdict",
                  status_runs_out_of_time(1, 3), [{"status": 503}, {"status": 503}], False, 1,
                  ["no verdict could be obtained (HTTP 503 from the model service); whether any file was changed"
                   " could not be told, and an ending with nothing delivered needs a verdict"]),
                 ("the first look and the second telling run out of time, then a blocking verdict not saved",
                  status_runs_out_of_time(1, 3), [{"verdict": (True, "the request needs a new file")}], True, 1,
                  ["SENT BACK", "Whether any file was changed could not be told, so the outcome above stands"]),
                 ("only the first look runs out of time, then no verdict", status_runs_out_of_time(1),
                  [{"status": 503}, {"status": 503}], False, 1,
                  ["no file was changed, and an ending with nothing delivered needs a verdict"]),
                 ("only the first look runs out of time, then a passing verdict", status_runs_out_of_time(1),
                  [{"verdict": (False, "")}], False, 0, ["PASSED"]))
        for case, before, replies, unsaved, expected, said in cases:
            service = ModelStandIn(replies)
            self.addCleanup(service.close)
            if unsaved:
                self.home.chmod(0o500)
            try:
                status, printed = self.review_in_process(service, before, REVIEW_DIFF_PATHS="")
            finally:
                self.home.chmod(0o700)
            self.assertEqual(status, expected, (case, printed))
            for text in said:
                self.assertIn(text, printed, case)
            if case.startswith("only the first look"):
                # Told again from what Git read: the reviewer is asked whether
                # the request is met with no file changed.
                self.assertIn(self.NO_CHANGE, service.requests[0]["body"]["messages"][1]["content"], case)
                self.assertNotIn("could not be told", printed, case)
        service = ModelStandIn([{"status": 503}, {"status": 503}])
        self.addCleanup(service.close)
        status, printed = self.review_in_process(service, lambda command: None)
        self.assertEqual(status, 1, printed)
        self.assertIn("no file was changed, and an ending with nothing delivered needs a verdict", printed)
        self.assertNotIn("could not be told", printed)


if __name__ == "__main__":
    unittest.main()
