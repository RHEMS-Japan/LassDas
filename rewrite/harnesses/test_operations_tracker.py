"""Only synthetic HTTP and explicit fake executables; no cluster fallback."""
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


OPERATIONS = Path(__file__).resolve().parents[2] / "deploy/ticket-engine/operations"
FAKE = r'''
import io, json, os, socket, sys, urllib.request, urllib.response
from email.message import Message
from pathlib import Path
args = sys.argv[1:]
root = Path(os.environ["FIXTURE_ROOT"])
with (root / "calls.jsonl").open("a") as output:
    output.write(json.dumps(args) + "\n")
assert args[:10] == ["--context", "fixture-context", "--namespace", "fixture-namespace", "exec", "-i", "fixture-pod", "-c", "fixture-container", "--"]
assert args[10:13] == ["python3", "-B", "-c"]
payload = sys.stdin.buffer.read()
(root / "stdin.bin").write_bytes(payload)
mode = os.environ.get("FIXTURE_MODE", "success")
def denied(*args, **kwargs):
    raise AssertionError("real network is forbidden")
socket.create_connection = denied
socket.socket.connect = denied
class HTTP(urllib.request.HTTPSHandler):
    def https_open(self, request):
        with (root / "http.jsonl").open("a") as output:
            output.write(json.dumps({"method": request.get_method(), "url": request.full_url,
                                     "body": request.data.decode() if request.data else None}) + "\n")
        path = urllib.parse.urlsplit(request.full_url).path
        code, value, headers = 200, {}, Message()
        if path.endswith("/issueTypes"):
            value = [{"id": 5}, {"id": 8}]
        elif path.endswith("/priorities"):
            value = [{"id": 2}, {"id": 3}]
        elif request.get_method() == "POST":
            if mode == "timeout":
                raise TimeoutError("private fixture diagnostic token-sentinel")
            if mode.startswith("redirect-"):
                code = int(mode.split("-")[1])
                headers["Location"] = "https://tracker.invalid/api/v2/moved"
            else:
                code = 201
                value = {"id": 41, "projectId": 7, "issueKey": "SAMPLE-41"}
                if mode == "receipt-project":
                    value["projectId"] = 9
                if mode == "receipt-id":
                    value["id"] = 0
                if mode == "receipt-private":
                    value["issueKey"] = "token-sentinel"
        elif path.endswith("/issues/41"):
            if mode == "callback-timeout":
                import time
                time.sleep(3)
            if mode == "readback-failure":
                raise OSError("private fixture diagnostic token-sentinel")
            value = {"id": 41, "projectId": 7, "issueKey": "SAMPLE-41"}
            if mode == "readback-mismatch":
                value["id"] = 42
        else:
            raise AssertionError("unexpected fixture URL")
        response = urllib.response.addinfourl(io.BytesIO(json.dumps(value).encode()), headers, request.full_url, code)
        response.msg = "fixture"
        return response
original = urllib.request.build_opener
urllib.request.build_opener = lambda *handlers: original(HTTP(), *handlers)
os.environ["FIXTURE_KEY"] = "token-sentinel"
if mode == "missing-key":
    del os.environ["FIXTURE_KEY"]
sys.stdin = io.TextIOWrapper(io.BytesIO(payload), encoding="utf-8")
if mode == "remote-failure":
    print('{"phase":"complete","action":"read","project":7,"issues":[],"comments":{},"atomic_snapshot":false,"rehearsal_fixture":false}')
    print("private fixture diagnostic token-sentinel", file=sys.stderr)
    sys.exit(29)
exec(args[13], {"__name__": "__main__"})
'''

CLI = r'''
import json, os, sys
from pathlib import Path
root = Path(os.environ["FIXTURE_ROOT"])
args = sys.argv[1:]
with (root / "cli.jsonl").open("a") as output:
    output.write(json.dumps(args) + "\n")
assert args[:4] == ["--base-url", "https://tracker.invalid/api/v2", "--key-env", "FIXTURE_KEY"]
assert os.environ["FIXTURE_KEY"] == "token-sentinel"
mode = os.environ.get("FIXTURE_MODE", "success")
if args[-1] == "issues":
    assert args[4:] == ["--project-id", "7", "issues"]
    value = [{"id": number, "projectId": 7, "issueKey": "SAMPLE-%d" % number,
              "summary": "private summary", "description": "private body", "status": {"id": 1, "name": "private status"},
              "assignee": {"id": 11, "name": "private person"}, "category": [{"id": 3, "name": "private category"}],
              "actualHours": 1.5, "estimatedHours": None, "updated": "2026-01-01T00:00:00Z"}
             for number in range(1, 102)]
    if mode == "duplicate":
        value.append(value[0])
    if mode == "outside-project":
        value[0]["projectId"] = 9
    if mode == "native-secret":
        value[0]["description"] = "token-sentinel"
else:
    assert args[4:] == ["--issue", "41", "comments"]
    value = [{"id": number, "content": "private comment"} for number in range(1, 102)]
    if mode == "comments-disorder":
        value.reverse()
if mode == "bad-json":
    print("private broken JSON")
else:
    print(json.dumps(value))
if mode == "cli-failure":
    print("private fixture diagnostic token-sentinel", file=sys.stderr)
    sys.exit(23)
'''


class OperationsTrackerTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="tracker-operations-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()
        self.fake = self.root / "fake-kubectl"
        self.cli = self.root / "fake-tracker"
        for path, code in ((self.fake, FAKE), (self.cli, CLI)):
            path.write_text("#!" + sys.executable + "\n" + code)
            path.chmod(0o700)
        self.config = self.root / "config.json"
        self.configuration = {"backlog": {"base_url": "https://tracker.invalid/api/v2", "key_env": "FIXTURE_KEY"},
                              "intake": {"project_id": 7}}
        self.config.write_text(json.dumps(self.configuration))
        self.summary, self.description = self.root / "summary.txt", self.root / "description.txt"
        self.summary.write_text("依頼の見出し & 'quoted' $value")
        self.description.write_text("private body\nsecond line; $(not-code) `still-data`\n")
        self.output = self.root / "private output"
        self.environment = dict(os.environ, FIXTURE_ROOT=str(self.root))
        self.environment.pop("FIXTURE_KEY", None)

    def run_helper(self, action="read", extra=(), mode="success"):
        command = ["/bin/sh", str(OPERATIONS / ("read-issues.sh" if action == "read" else "file-ticket.sh")),
                   "--kubectl", str(self.fake), "--context", "fixture-context", "--namespace", "fixture-namespace",
                   "--pod", "fixture-pod", "--container", "fixture-container", "--source", "backlog",
                   "--config", str(self.config), "--project-id", "7", "--output", str(self.output)]
        if action == "read":
            command += ["--tracker-bin", str(self.cli)]
        else:
            command += ["--type-id", "5", "--priority-id", "2", "--summary-file", str(self.summary),
                        "--description-file", str(self.description)]
        return subprocess.run(command + list(extra), env=dict(self.environment, FIXTURE_MODE=mode),
                              capture_output=True, text=True, timeout=30)

    def rows(self, name):
        path = self.root / name
        return [json.loads(line) for line in path.read_text().splitlines()] if path.exists() else []

    def posts(self):
        return [row for row in self.rows("http.jsonl") if row["method"] == "POST"]

    def assert_private(self, result):
        public = result.stdout + result.stderr
        for text in ("token-sentinel", "private fixture diagnostic", "private summary", "private body",
                     "private person", "private comment", self.summary.read_text()):
            self.assertNotIn(text, public)
        for args in self.rows("calls.jsonl") + self.rows("cli.jsonl"):
            rendered = json.dumps(args, ensure_ascii=False)
            self.assertNotIn("token-sentinel", rendered)
            self.assertNotIn(self.summary.read_text(), rendered)
            self.assertNotIn("private body", rendered)

    def test_read_retains_all_rows_and_comments_but_defaults_to_metadata(self):
        result = self.run_helper(extra=("--issue-id", "41"))
        self.assertEqual(result.returncode, 0, result.stderr)
        saved = json.loads((self.output / "result.json").read_text())
        self.assertEqual(len(saved["issues"]), 101)
        self.assertEqual(len(saved["comments"]["41"]), 101)
        self.assertEqual(saved["issues"][-1]["id"], 101)
        self.assertNotIn("private", (self.output / "result.json").read_text())
        self.assertIn("not an atomic snapshot or rehearsal fixture", result.stdout)
        self.assertFalse(self.posts())
        self.assertEqual(len(self.rows("cli.jsonl")), 2)
        self.assert_private(result)

    def test_native_is_explicit_and_private_and_never_returns_credential_material(self):
        result = self.run_helper(extra=("--native", "--issue-id", "41"))
        self.assertEqual(result.returncode, 0, result.stderr)
        saved = json.loads((self.output / "result.json").read_text())
        self.assertEqual(len(saved["native"]), 101)
        self.assertEqual(saved["native"][0]["description"], "private body")
        self.assertEqual(saved["comments"]["41"][0]["content"], "private comment")
        self.assertEqual(self.output.stat().st_mode & 0o777, 0o700)
        for path in self.output.iterdir():
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
        self.assert_private(result)
        self.output = self.root / "secret-rejected"
        result = self.run_helper(extra=("--native",), mode="native-secret")
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("token-sentinel", (self.output / "received.jsonl").read_text())
        self.assert_private(result)

    def test_failed_incomplete_or_out_of_project_reads_are_not_success(self):
        for mode in ("cli-failure", "remote-failure", "bad-json", "duplicate", "outside-project", "comments-disorder"):
            with self.subTest(mode=mode):
                self.output = self.root / mode
                result = self.run_helper(extra=("--issue-id", "41"), mode=mode)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse((self.output / "result.json").exists())
                self.assert_private(result)

    def test_create_once_preserves_stdin_text_and_records_receipt_before_readback(self):
        before = hashlib.sha256(self.config.read_bytes()).hexdigest()
        result = self.run_helper("create")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(self.posts()), 1)
        import urllib.parse
        form = urllib.parse.parse_qs(self.posts()[0]["body"], keep_blank_values=True)
        self.assertEqual(form["summary"], [self.summary.read_text()])
        self.assertEqual(form["description"], [self.description.read_text()])
        self.assertEqual(form["issueTypeId"], ["5"])
        self.assertEqual(form["priorityId"], ["2"])
        self.assertIn("apiKey=token-sentinel", self.posts()[0]["url"])
        events = [json.loads(line) for line in (self.output / "received.jsonl").read_text().splitlines()]
        self.assertEqual([event["phase"] for event in events], ["created", "complete"])
        self.assertEqual(events[0]["receipt"]["id"], 41)
        self.assertEqual(len(self.rows("http.jsonl")), 4)
        self.assertEqual(hashlib.sha256(self.config.read_bytes()).hexdigest(), before)
        self.assert_private(result)

    def test_post_failures_never_retry_and_readback_failures_retain_known_receipt(self):
        for mode in ("timeout", "redirect-301", "redirect-302", "redirect-303", "redirect-307", "redirect-308", "receipt-project", "receipt-id", "receipt-private", "readback-failure", "readback-mismatch"):
            with self.subTest(mode=mode):
                self.output = self.root / mode
                before = len(self.posts())
                http_before = len(self.rows("http.jsonl"))
                result = self.run_helper("create", mode=mode)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(len(self.posts()) - before, 1)
                if mode.startswith("redirect") or mode in ("receipt-project", "receipt-id", "receipt-private"):
                    self.assertEqual(len(self.rows("http.jsonl")) - http_before, 3)
                self.assertIn("do not blindly resubmit", result.stderr)
                self.assertTrue((self.output / "intent.json").is_file())
                self.assertFalse((self.output / "result.json").exists())
                evidence = (self.output / "received.jsonl").read_text()
                if mode.startswith("readback"):
                    self.assertEqual(json.loads(evidence)["receipt"]["id"], 41)
                self.assert_private(result)
                calls = len(self.rows("calls.jsonl"))
                self.assertNotEqual(self.run_helper("create").returncode, 0)
                self.assertEqual(len(self.rows("calls.jsonl")), calls)

    def test_explicit_choices_and_project_are_checked_before_post(self):
        for option, value in (("type-id", "9"), ("priority-id", "9"), ("project-id", "9"),
                              ("type-id", "0"), ("priority-id", "0")):
            with self.subTest(option=option, value=value):
                self.output = self.root / (option + value)
                result = self.run_helper("create", extra=("--" + option, value))
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(self.posts())
        self.configuration["github"] = {"owner": "fixture", "repo": "fixture"}
        self.config.write_text(json.dumps(self.configuration))
        self.output = self.root / "mixed-config"
        self.assertNotEqual(self.run_helper("create").returncode, 0)
        self.assertFalse(self.posts())

    def test_outer_timeout_keeps_the_streamed_receipt_without_retry(self):
        result = self.run_helper("create", extra=("--timeout", "1"), mode="callback-timeout")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(len(self.posts()), 1)
        evidence = (self.output / "received.jsonl").read_text()
        self.assertEqual(json.loads(evidence)["receipt"]["id"], 41)
        self.assertFalse((self.output / "result.json").exists())
        self.assert_private(result)

    def test_invalid_endpoint_or_missing_key_never_posts(self):
        from urllib.parse import urlunsplit
        credential_url = urlunsplit(("https", "user:pass" + chr(64) + "tracker.invalid", "/api/v2", "", ""))
        for index, base in enumerate(("http://tracker.invalid/api/v2", credential_url,
                                     "https://tracker.invalid/api/v2?apiKey=fixture", "https://tracker.invalid/api/v2#fragment")):
            self.configuration["backlog"]["base_url"] = base
            self.config.write_text(json.dumps(self.configuration))
            self.output = self.root / ("bad-endpoint-%d" % index)
            self.assertNotEqual(self.run_helper("create").returncode, 0)
        self.configuration["backlog"]["base_url"] = "https://tracker.invalid/api/v2"
        self.config.write_text(json.dumps(self.configuration))
        self.output = self.root / "no-key"
        self.assertNotEqual(self.run_helper("create", mode="missing-key").returncode, 0)
        self.assertFalse(self.posts())

    def test_existing_output_and_unsafe_inputs_are_refused_before_callback(self):
        self.output.mkdir()
        keep = self.output / "keep"
        keep.write_text("existing evidence")
        self.assertNotEqual(self.run_helper("create").returncode, 0)
        self.assertEqual(keep.read_text(), "existing evidence")
        self.assertFalse(self.rows("calls.jsonl"))
        for kind in ("symlink", "hardlink", "fifo"):
            with self.subTest(kind=kind):
                linked = self.root / kind
                if kind == "symlink":
                    linked.symlink_to(self.description)
                elif kind == "hardlink":
                    os.link(self.description, linked)
                else:
                    os.mkfifo(linked)
                self.output = self.root / (kind + "-output")
                result = self.run_helper("create", extra=("--description-file", str(linked)))
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(self.rows("calls.jsonl"))
                linked.unlink()
        parent = self.root / "linked-parent"
        parent.symlink_to(self.root, target_is_directory=True)
        self.output = parent / "unsafe-output"
        self.assertNotEqual(self.run_helper().returncode, 0)
        self.assertFalse(self.rows("calls.jsonl"))

    def test_missing_fake_or_tracker_does_not_fallback_and_reserved_output_stays(self):
        self.cli.unlink()
        self.assertNotEqual(self.run_helper().returncode, 0)
        self.assertTrue((self.output / "intent.json").exists())
        self.assertFalse((self.output / "result.json").exists())
        self.fake.unlink()
        self.output = self.root / "no-callback"
        self.assertNotEqual(self.run_helper("create").returncode, 0)
        self.assertTrue((self.output / "intent.json").exists())
        self.assertNotEqual(self.run_helper("create").returncode, 0)
        self.assertFalse(self.posts())

    def test_completion_evidence_and_local_receipt_save_failure_remain_nonzero(self):
        spec = importlib.util.spec_from_file_location("operation_tracker", OPERATIONS / "tracker_helper.py")
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        for events in ([], [{"phase": "created", "receipt": {"id": 41, "projectId": 7, "issueKey": "SAMPLE-41"}}],
                       [{"phase": "complete", "action": "create", "project": 7}]):
            with self.assertRaises(module.HelperError):
                module.complete(events, "create", 7)
        from unittest import mock
        original = module.save
        def failed_save(root, name, value):
            if name == "result.json":
                raise OSError("synthetic local disk failure")
            return original(root, name, value)
        args = ["tracker_helper", "create", "--kubectl", str(self.fake), "--context", "fixture-context",
                "--namespace", "fixture-namespace", "--pod", "fixture-pod", "--container", "fixture-container",
                "--source", "backlog", "--config", str(self.config), "--project-id", "7", "--output", str(self.output),
                "--type-id", "5", "--priority-id", "2", "--summary-file", str(self.summary), "--description-file", str(self.description)]
        with mock.patch.object(sys, "argv", args), mock.patch.dict(os.environ, self.environment), mock.patch.object(module, "save", failed_save):
            with self.assertRaises(OSError):
                module.main()
        self.assertEqual(len(self.posts()), 1)
        self.assertIn('"phase": "created"', (self.output / "received.jsonl").read_text())
