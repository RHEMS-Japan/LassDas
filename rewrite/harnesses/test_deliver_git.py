"""Real Git, a real local delivery target and a stub service; no model call.

The service is a local HTTP stub of the delivery API: it records what the
process asked for, so a refusal is checked by what was NOT sent, and a rerun
is checked by the absence of a second push, pull request or merge.
"""
import http.server
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import threading
import unittest
import urllib.parse

SCRIPT = Path(__file__).with_name("deliver_git.py").resolve()
SUPPORT = Path(__file__).with_name("delivery_support.py").resolve()
TOKEN = "fixture-delivery-credential-2f1a9c"


def service_handler(state):
    class Handler(http.server.BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, *arguments):
            pass

        def bare(self, *arguments):
            return subprocess.run(["git", "-C", state["repository"], *arguments],
                                  capture_output=True, text=True, check=True).stdout.strip()

        def answer(self, status, payload):
            body = json.dumps(payload).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def record(self):
            state["requests"].append((self.command, self.path))
            state["authorization"].add(self.headers.get("Authorization", ""))

        def do_GET(self):
            self.record()
            parts = urllib.parse.urlsplit(self.path)
            if parts.path == "/repos/owner/project":
                return self.answer(200, {"default_branch": "master"})
            if parts.path.startswith("/repos/owner/project/branches/"):
                return self.answer(200, {"name": parts.path.rsplit("/", 1)[1]})
            if parts.path == "/repos/owner/project/pulls":
                query = urllib.parse.parse_qs(parts.query)
                head = query.get("head", [""])[0].partition(":")[2]
                return self.answer(200, [pull for pull in state["pulls"]
                                         if pull["head"] == head and not pull["merged"]])
            if parts.path.startswith("/repos/owner/project/pulls/"):
                number = int(parts.path.rsplit("/", 1)[1])
                return self.answer(200, state["pulls"][number - 1])
            self.answer(404, {"message": "no such path"})

        def do_POST(self):
            self.record()
            length = int(self.headers.get("Content-Length", "0"))
            payload = json.loads(self.rfile.read(length) or b"{}")
            if state["post_failures"] > 0:
                state["post_failures"] -= 1
                return self.answer(503, {"message": "the service is busy"})
            number = len(state["pulls"]) + 1
            pull = {"number": number, "html_url": "http://service.invalid/pulls/%d" % number,
                    "head": payload["head"], "base": payload["base"], "merged": False,
                    "merge_commit_sha": None}
            state["pulls"].append(pull)
            self.answer(201, pull)

        def do_PUT(self):
            self.record()
            length = int(self.headers.get("Content-Length", "0"))
            json.loads(self.rfile.read(length) or b"{}")
            if state["merge_refusal"]:
                status, message = state["merge_refusal"]
                return self.answer(status, {"message": message})
            pull = state["pulls"][int(self.path.split("/")[-2]) - 1]
            # An actual merge commit in the actual target, so a later check of
            # "is this delivery contained in the branch" has something to read.
            base = self.bare("rev-parse", "refs/heads/" + pull["base"])
            head = self.bare("rev-parse", "refs/heads/" + pull["head"])
            tree = self.bare("rev-parse", "refs/heads/%s^{tree}" % pull["head"])
            merge = self.bare("commit-tree", tree, "-p", base, "-p", head, "-m", "Merge the delivery")
            self.bare("update-ref", "refs/heads/" + pull["base"], merge)
            pull.update(merged=True, merge_commit_sha=merge, merged_at="2026-01-01T00:00:00Z")
            self.answer(200, {"sha": merge, "merged": True})

    return Handler


@unittest.skipUnless(shutil.which("git"), "requires Git")
class DeliveryTests(unittest.TestCase):
    def setUp(self):
        self.root = Path(os.environ.get("TEST_ROOT") or self.enterTempDirectory())
        self.remote = self.root / "target.git"
        self.workspace = self.root / "job" / "workspace"
        self.home = self.root / "job" / "home"
        self.workspace.parent.mkdir(parents=True)
        self.home.mkdir(parents=True)
        self.git_log = self.root / "git-invocations.log"
        source = self.root / "source"
        source.mkdir()
        self.git(source, "init", "--initial-branch=master")
        (source / "main.go").write_text("package main\n")
        (source / "notes.md").write_text("original\n")
        (source / "library").mkdir()
        (source / "library" / "run.go").write_text("package library\n")
        self.git(source, "add", "-A")
        self.git(source, "commit", "-m", "Codex: initial")
        self.git(source, "clone", "--bare", str(source), str(self.remote))
        self.git(self.root, "clone", "--no-local", str(self.remote), str(self.workspace))
        self.git(self.workspace, "checkout", "--detach", "HEAD")
        self.state = {"repository": str(self.remote), "pulls": [], "requests": [], "authorization": set(),
                      "post_failures": 0, "merge_refusal": None}
        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), service_handler(self.state))
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.path = self.recording_git()

    def enterTempDirectory(self):
        import tempfile
        temporary = tempfile.TemporaryDirectory(prefix="delivery-test-")
        self.addCleanup(temporary.cleanup)
        return Path(temporary.name).resolve()

    def recording_git(self):
        """A Git on PATH that records its arguments, then runs the real one."""
        directory = self.root / "bin"
        directory.mkdir()
        shim = directory / "git"
        shim.write_text('#!/bin/sh\nprintf "%%s\\n" "$*" >> "%s"\nexec "%s" "$@"\n'
                        % (self.git_log, shutil.which("git")))
        shim.chmod(0o755)
        return str(directory) + os.pathsep + os.environ["PATH"]

    def git(self, directory, *arguments):
        return subprocess.run(["git", "-C", str(directory), "-c", "user.name=Fixture",
                               "-c", "user.email=fixture@example.invalid", *arguments],
                              check=True, capture_output=True, text=True,
                              env={"PATH": os.environ["PATH"], "GIT_CONFIG_GLOBAL": os.devnull,
                                   "GIT_CONFIG_NOSYSTEM": "1", "GIT_TERMINAL_PROMPT": "0"})

    def environment(self, **extra):
        address = "http://127.0.0.1:%d" % self.server.server_address[1]
        environment = {"PATH": self.path, "LANG": "C.UTF-8", "PYTHONDONTWRITEBYTECODE": "1",
                       "HOME": str(self.home), "TASK_ISSUE": "TICKET-41",
                       "TASK_WORKSPACE": str(self.workspace), "TASK_HOME": str(self.home),
                       "GITHUB_TOKEN": TOKEN, "DELIVERY_REPOSITORY": "owner/project",
                       "DELIVERY_BASE_BRANCH": "master", "DELIVERY_REMOTE_URL": str(self.remote),
                       "DELIVERY_API_BASE": address, "DELIVERY_POLL_SECONDS": "0.1",
                       "DELIVERY_MERGE_TIMEOUT_SECONDS": "20", "DELIVERY_RETRY_SECONDS": "0.05",
                       "DELIVERY_RETRY_CAP_SECONDS": "0.1",
                       "DELIVERY_ALLOWED_PATHS": "main.go:go.mod:library/"}
        environment.update(extra)
        return environment

    def deliver(self, *arguments, **extra):
        return subprocess.run([sys.executable, "-B", str(SCRIPT), *arguments], input="the role prompt",
                              cwd=str(self.workspace), env=self.environment(**extra),
                              capture_output=True, text=True, timeout=120)

    def change(self, relative, text):
        path = self.workspace / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text)

    def remote_branches(self):
        listing = self.git(self.remote, "for-each-ref", "--format=%(refname)", "refs/heads")
        return listing.stdout.split()

    def receipt(self):
        path = self.workspace / ".git" / "ticket-engine" / "delivery.json"
        return json.loads(path.read_text()) if path.exists() else {}

    def methods(self):
        return [method for method, _ in self.state["requests"]]

    def test_refuses_a_change_outside_the_operator_grant(self):
        self.change("main.go", "package main // changed\n")
        self.change("notes.md", "edited outside the grant\n")
        result = self.deliver()
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("notes.md", result.stderr)
        self.assertEqual(self.remote_branches(), ["refs/heads/master"])
        self.assertEqual(self.receipt(), {})
        self.assertNotIn("POST", self.methods())

    def test_refuses_configured_forbidden_text(self):
        self.change("main.go", "package main // internal-project-codename\n")
        result = self.deliver(DELIVERY_FORBIDDEN_TEXT="Internal-Project-Codename\n")
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("forbidden text", result.stderr)
        self.assertEqual(self.remote_branches(), ["refs/heads/master"])
        self.assertNotIn("POST", self.methods())

    def test_refuses_a_change_carrying_the_delivery_credential(self):
        self.change("main.go", "package main // %s\n" % TOKEN)
        result = self.deliver()
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("contains the delivery credential", result.stderr)
        self.assertNotIn(TOKEN, result.stdout + result.stderr)
        self.assertNotIn("POST", self.methods())

    def test_delivers_once_and_a_rerun_neither_pushes_nor_merges_again(self):
        self.change("main.go", "package main // delivered\n")
        self.change("library/run.go", "package library // delivered\n")
        first = self.deliver()
        self.assertEqual(first.returncode, 0, first.stdout + first.stderr)
        receipt = self.receipt()
        self.assertEqual(receipt["pull_request"], 1)
        self.assertEqual(receipt["branch"], "ticket/TICKET-41")
        self.assertRegex(receipt["merge_sha"], r"\A[0-9a-f]{40}\Z")
        self.assertIn("refs/heads/ticket/TICKET-41", self.remote_branches())
        self.assertEqual(self.git(self.remote, "rev-parse", "refs/heads/ticket/TICKET-41").stdout.strip(),
                         receipt["head"])
        self.assertEqual(self.methods().count("POST"), 1)
        self.assertEqual(self.methods().count("PUT"), 1)
        pushes = [line for line in self.git_log.read_text().splitlines() if " push " in line]
        self.assertEqual(len(pushes), 1, pushes)

        second = self.deliver()
        self.assertEqual(second.returncode, 0, second.stdout + second.stderr)
        self.assertEqual(self.receipt(), receipt)
        self.assertEqual(self.methods().count("POST"), 1)
        self.assertEqual(self.methods().count("PUT"), 1)
        self.assertEqual([line for line in self.git_log.read_text().splitlines() if " push " in line], pushes)
        self.assertIn("Delivered TICKET-41", second.stdout)

    def test_a_run_interrupted_after_the_merge_records_it_without_merging_again(self):
        self.change("main.go", "package main // delivered\n")
        self.assertEqual(self.deliver().returncode, 0)
        complete = self.receipt()
        # What an interrupted run leaves behind: the pull request is recorded
        # and already merged at the service, but its result never got written.
        interrupted = {key: value for key, value in complete.items() if key not in ("merge_sha", "merged_at")}
        (self.workspace / ".git/ticket-engine/delivery.json").write_text(json.dumps(interrupted))
        resumed = self.deliver()
        self.assertEqual(resumed.returncode, 0, resumed.stdout + resumed.stderr)
        self.assertEqual(self.receipt()["merge_sha"], complete["merge_sha"])
        self.assertEqual(self.methods().count("POST"), 1)
        self.assertEqual(self.methods().count("PUT"), 1)

    def test_the_credential_never_reaches_output_a_command_line_or_the_receipt(self):
        self.change("main.go", "package main // delivered\n")
        result = self.deliver()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertNotIn(TOKEN, result.stdout)
        self.assertNotIn(TOKEN, result.stderr)
        self.assertNotIn(TOKEN, self.git_log.read_text())
        self.assertNotIn(TOKEN, (self.workspace / ".git/ticket-engine/delivery.json").read_text())
        self.assertEqual(self.state["authorization"], {"Bearer " + TOKEN})

    def test_waits_out_a_service_answer_that_may_pass(self):
        self.state["post_failures"] = 2
        self.change("main.go", "package main // delivered\n")
        result = self.deliver()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("may pass", result.stdout)
        self.assertEqual(self.methods().count("POST"), 3)
        self.assertEqual(len(self.state["pulls"]), 1)
        self.assertRegex(self.receipt()["merge_sha"], r"\A[0-9a-f]{40}\Z")

    def test_a_refusal_ends_at_once_and_carries_the_service_message(self):
        self.state["merge_refusal"] = (405, 'Required status check "build" is expected.')
        self.change("main.go", "package main // delivered\n")
        result = self.deliver()
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("Required status check", result.stdout)
        self.assertIn("Nothing was delivered for TICKET-41.", result.stdout)
        self.assertEqual(self.methods().count("PUT"), 1)
        self.assertNotIn("merge_sha", self.receipt())
        self.assertIn("refs/heads/ticket/TICKET-41", self.remote_branches())

    def test_check_mode_changes_nothing_and_ends_non_zero(self):
        self.change("main.go", "package main // proposed\n")
        result = self.deliver("--dry-run")
        self.assertEqual(result.returncode, 3, result.stdout + result.stderr)
        self.assertIn("nothing was committed, pushed, opened or merged", result.stdout)
        self.assertEqual(self.remote_branches(), ["refs/heads/master"])
        self.assertEqual(self.receipt(), {})
        self.assertNotIn("POST", self.methods())
        self.assertNotIn("PUT", self.methods())

    def test_the_credential_helper_answers_only_the_configured_host(self):
        def ask(request, host):
            return subprocess.run([sys.executable, "-B", str(SUPPORT), "--credential-helper", "get"],
                                  input=request, capture_output=True, text=True, timeout=30,
                                  env=dict(self.environment(), DELIVERY_CREDENTIAL_HOST=host)).stdout
        self.assertIn("password=" + TOKEN,
                      ask("protocol=https\nhost=service.invalid\n\n", "service.invalid"))
        self.assertEqual(ask("protocol=https\nhost=elsewhere.invalid\n\n", "service.invalid"), "")
        self.assertEqual(ask("protocol=http\nhost=service.invalid\n\n", "service.invalid"), "")
        self.assertEqual(ask("protocol=https\nhost=service.invalid\n\n", ""), "")


if __name__ == "__main__":
    unittest.main()
