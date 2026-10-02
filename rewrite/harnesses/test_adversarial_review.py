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
import unittest

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
                elif reply.get("verdict") is None and "arguments" not in reply:
                    payload = json.dumps({"choices": [{"message": {"role": "assistant", "content": "I have looked."}}]}).encode()
                    self.send_response(200)
                else:
                    if "arguments" in reply:
                        arguments = reply["arguments"]
                    else:
                        blocking, findings = reply["verdict"]
                        arguments = {"blocking": blocking, "findings": findings}
                    payload = json.dumps({"choices": [{"message": {"role": "assistant", "tool_calls": [
                        {"id": "call-1", "type": "function", "function": {"name": "verdict", "arguments": json.dumps(
                            arguments)}}]}}]}).encode()
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

    def run_review(self, service, stdin_text="Current assignment:\nStage 4 of 8\n\nOriginal request:\nadd a thing\n", **extra):
        environment = {"PATH": os.environ["PATH"], "LANG": "C.UTF-8", "PYTHONDONTWRITEBYTECODE": "1",
                       "TASK_WORKSPACE": str(self.workspace), "TASK_HOME": str(self.home), "REVIEW_MODEL_URL": service.url,
                       "REVIEW_MODEL": "fixture/reviewer", "REVIEW_KEY_ENV": "REVIEW_API_KEY", "REVIEW_API_KEY": KEY,
                       "REVIEW_TEST_COMMANDS": sys.executable + " -c \"print('tests ran fine')\"",
                       "REVIEW_DIFF_PATHS": "src tests", "REVIEW_ATTEMPTS": "2",
                       "REVIEW_TIMEOUT_SECONDS": "30"}
        environment.update(extra)
        return subprocess.run([sys.executable, "-B", str(SCRIPT)], input=stdin_text, capture_output=True,
                              text=True, env=environment, timeout=120)

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
        # let a change through unreviewed with the findings gone. A plain
        # value is a verdict, in any letter case of the field; anything else
        # is none, and what the reviewer wrote stays in the output and log.
        cases = (("\"true\"", {"blocking": "true"}, "SENT BACK", 1, 1),
                 ("\" TRUE \"", {"blocking": " TRUE "}, "SENT BACK", 1, 1),
                 ("1", {"blocking": 1}, "SENT BACK", 1, 1),
                 ("Blocking", {"Blocking": True}, "SENT BACK", 1, 1),
                 ("\"false\"", {"blocking": "false"}, "PASSED", 0, 0),
                 ("0", {"blocking": 0}, "PASSED", 0, 0),
                 ("null", {"blocking": None}, "NOT REVIEWED", 0, 1),
                 ("missing", {}, "NOT REVIEWED", 0, 1),
                 ("\"maybe\"", {"blocking": "maybe"}, "NOT REVIEWED", 0, 1))
        for tree in ("a change", "no change"):
            if tree == "no change":
                self.leave_unchanged()
            for case, fields, outcome, changed, unchanged in cases:
                finding = "REQUIRED_NEW_BEHAVIOR is missing (%s, %s)." % (case, tree)
                reply = {"arguments": dict(fields, Findings=finding)}
                service = ModelStandIn([reply, reply])
                self.addCleanup(service.close)
                finished = self.run_review(service)
                status = changed if tree == "a change" else unchanged
                self.assertEqual(finished.returncode, status, (case, tree, finished.stdout, finished.stderr))
                self.assertIn("Review by fixture/reviewer: %s" % outcome, finished.stdout, (case, tree))
                self.assertIn(finding, finished.stdout, (case, tree))
                self.assertIn(finding, self.review_log(), (case, tree))
                if outcome == "NOT REVIEWED":
                    self.assertIn("no verdict could be obtained", finished.stdout, (case, tree))
                    if tree == "no change":
                        self.assertIn(self.GOES_BACK, finished.stdout, case)

    def test_git_reads_neither_the_users_nor_the_systems_settings(self):
        # As for the delivery: a user's own settings could hide a path from
        # one of the two only, and they would disagree on whether anything
        # changed. Here the only new file is hidden by Git's default exclude
        # file, as the delivery sees it, while the user's settings name another.
        self.leave_unchanged()
        home = self.home.parent / "user-home"
        (home / ".config" / "git").mkdir(parents=True)
        (home / ".config" / "git" / "ignore").write_text("notes.txt\n")
        (home / "nothing-excluded").write_text("")
        (home / ".gitconfig").write_text("[core]\n\texcludesFile = %s\n" % (home / "nothing-excluded"))
        (self.workspace / "notes.txt").write_text("a note Git's default exclude file hides\n")
        service = ModelStandIn([{"status": 503}, {"status": 503}])
        self.addCleanup(service.close)
        finished = self.run_review(service, HOME=str(home))
        self.assertEqual(finished.returncode, 1, finished.stdout)
        self.assertIn(self.GOES_BACK, finished.stdout)

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

    def test_a_workspace_that_is_not_a_checkout_lets_the_work_through_with_a_note(self):
        shutil.rmtree(self.workspace / ".git")
        service = ModelStandIn([{"verdict": (True, "x")}])
        self.addCleanup(service.close)
        finished = self.run_review(service)
        self.assertEqual(finished.returncode, 0, finished.stderr)
        self.assertNotIn("Traceback", finished.stderr)
        self.assertIn("could not be read", finished.stdout)
        self.assertIn("NOT REVIEWED", finished.stdout)
        self.assertEqual(service.requests, [])


if __name__ == "__main__":
    unittest.main()
