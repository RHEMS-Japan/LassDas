"""Real Git, a real local delivery target and a stub service; no model call.

The service is a local HTTP stub of the delivery API: it records what the
process asked for, so a refusal is checked by what was NOT sent, and a rerun
is checked by the absence of a second push, pull request or merge.
"""
import http.server
import importlib.util
import json
import os
from pathlib import Path
import shutil
import struct
import subprocess
import sys
import threading
import unittest
import urllib.parse
import zlib

SCRIPT = Path(__file__).with_name("deliver_git.py").resolve()
SUPPORT = Path(__file__).with_name("delivery_support.py").resolve()
TOKEN = "fixture-delivery-credential-2f1a9c"
IDENTITY = ("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid")


def git_environment():
    """No personal configuration, and no guessed identity either: a runtime
    with neither is exactly where these programs have to work. The delivery's
    Git on the workspace drops settings handed to Git through the environment,
    so the workspace's own configuration forbids the guess there (see setUp)."""
    return {"PATH": os.environ["PATH"], "LANG": "C.UTF-8", "GIT_CONFIG_GLOBAL": os.devnull,
            "GIT_CONFIG_SYSTEM": os.devnull, "GIT_CONFIG_NOSYSTEM": "1", "GIT_TERMINAL_PROMPT": "0",
            "GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "user.useConfigOnly",
            "GIT_CONFIG_VALUE_0": "true"}


def service_handler(state):
    class Handler(http.server.BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, *arguments):
            pass

        def bare(self, *arguments):
            environment = dict(git_environment(), GIT_AUTHOR_NAME="Fixture",
                               GIT_AUTHOR_EMAIL="fixture@example.invalid",
                               GIT_COMMITTER_NAME="Fixture",
                               GIT_COMMITTER_EMAIL="fixture@example.invalid")
            return subprocess.run(["git", "-C", state["repository"], *IDENTITY, *arguments],
                                  env=environment, capture_output=True, text=True,
                                  check=True).stdout.strip()

        def handle_one_request(self):
            try:
                return super().handle_one_request()
            except subprocess.CalledProcessError as error:
                # Answer with the failure instead of closing the socket, so a
                # broken fixture shows up as a failed assertion here.
                state["fixture_errors"].append(str(error))
                try:
                    self.answer(500, {"message": "fixture: " + str(error)})
                except OSError:
                    pass
                self.close_connection = True

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

        def current(self, pull):
            """As the service answers: an open pull request's head is wherever
            its branch is now; a merged one keeps the head it was merged at."""
            if pull["state"] == "open":
                try:
                    pull["head"]["sha"] = self.bare("rev-parse", "refs/heads/" + pull["head"]["ref"])
                except subprocess.CalledProcessError:
                    pass
            return pull

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
                return self.answer(200, [self.current(pull) for pull in state["pulls"]
                                         if pull["head"]["ref"] == head and pull["state"] == "open"])
            if parts.path.startswith("/repos/owner/project/pulls/"):
                number = int(parts.path.rsplit("/", 1)[1])
                return self.answer(200, self.current(state["pulls"][number - 1]))
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
                    "head": {"ref": payload["head"], "sha": None}, "base": payload["base"], "merged": False,
                    "merge_commit_sha": None, "state": "open", "body": payload.get("body", "")}
            state["pulls"].append(pull)
            self.answer(201, self.current(pull))

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
            head = self.bare("rev-parse", "refs/heads/" + pull["head"]["ref"])
            tree = self.bare("rev-parse", "refs/heads/%s^{tree}" % pull["head"]["ref"])
            merge = self.bare("commit-tree", tree, "-p", base, "-p", head, "-m", "Merge the delivery")
            self.bare("update-ref", "refs/heads/" + pull["base"], merge)
            pull["head"]["sha"] = head
            pull.update(merged=True, state="closed", merge_commit_sha=merge, merged_at="2026-01-01T00:00:00Z")
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
        self.git(self.workspace, "config", "user.useConfigOnly", "true")
        self.state = {"repository": str(self.remote), "pulls": [], "requests": [], "authorization": set(),
                      "post_failures": 0, "merge_refusal": None, "fixture_errors": []}
        self.addCleanup(lambda: self.assertEqual(self.state["fixture_errors"], []))
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
        return subprocess.run(["git", "-C", str(directory), *IDENTITY, *arguments],
                              check=True, capture_output=True, text=True, env=git_environment())

    def environment(self, **extra):
        address = "http://127.0.0.1:%d" % self.server.server_address[1]
        environment = dict(git_environment(), PATH=self.path, PYTHONDONTWRITEBYTECODE="1")
        environment.update({
                       "HOME": str(self.home), "TASK_ISSUE": "TICKET-41",
                       "TASK_WORKSPACE": str(self.workspace), "TASK_HOME": str(self.home),
                       "GITHUB_TOKEN": TOKEN, "DELIVERY_REPOSITORY": "owner/project",
                       "DELIVERY_BASE_BRANCH": "master", "DELIVERY_REMOTE_URL": str(self.remote),
                       "DELIVERY_API_BASE": address, "DELIVERY_POLL_SECONDS": "0.1",
                       "DELIVERY_MERGE_TIMEOUT_SECONDS": "20", "DELIVERY_RETRY_SECONDS": "0.05",
                       "DELIVERY_RETRY_CAP_SECONDS": "0.1",
                       "DELIVERY_ALLOWED_PATHS": "main.go:go.mod:library/"})
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

    def test_a_dot_grant_delivers_every_changed_path(self):
        self.change("main.go", "package main // delivered\n")
        self.change("notes.md", "a new file nobody named in advance\n")
        result = self.deliver(DELIVERY_ALLOWED_PATHS=".")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        delivered = self.git(self.remote, "show", "--name-only", "--format=", self.receipt()["head"]).stdout.split()
        self.assertEqual(sorted(delivered), ["main.go", "notes.md"])

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
        pushes = [line for line in self.git_log.read_text().splitlines() if " push " in line]
        resumed = self.deliver()
        self.assertEqual(resumed.returncode, 0, resumed.stdout + resumed.stderr)
        self.assertEqual(self.receipt()["merge_sha"], complete["merge_sha"])
        # The resumed run finds its commit already published and pushes nothing.
        self.assertEqual([line for line in self.git_log.read_text().splitlines() if " push " in line], pushes)
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


    def advance_integration_branch(self, relative, text, message="Codex: another delivery"):
        """Another request's work lands on the integration branch after this
        checkout was made, the way it does when requests run side by side."""
        other = self.root / ("other-" + relative.replace("/", "-"))
        self.git(self.root, "clone", "--no-local", str(self.remote), str(other))
        (other / relative).write_text(text)
        self.git(other, "add", "-A")
        self.git(other, "commit", "-m", message)
        self.git(other, "push", "origin", "HEAD:master")

    def parents(self, directory, revision="HEAD"):
        return self.git(directory, "rev-list", "--parents", "-n", "1", revision).stdout.split()[1:]

    def test_a_moved_integration_branch_is_merged_before_publishing(self):
        self.advance_integration_branch("library/run.go", "package library\n\nfunc Other() {}\n")
        self.change("main.go", "package main\n\nfunc main() {}\n")
        done = self.deliver()
        self.assertEqual(done.returncode, 0, done.stdout + done.stderr)
        self.assertEqual(len(self.parents(self.workspace)), 2, "the ticket branch did not take the merge")
        merged = self.git(self.remote, "show", "refs/heads/master:library/run.go").stdout
        self.assertIn("Other", merged)
        self.assertIn("func main", self.git(self.remote, "show", "refs/heads/master:main.go").stdout)
        self.assertIn("created a commit", done.stdout)

    def test_a_conflict_with_the_integration_branch_waits_for_the_next_round(self):
        self.advance_integration_branch("main.go", "package main\n\n// theirs\n")
        self.change("main.go", "package main\n\n// ours\n")
        refused = self.deliver()
        self.assertNotEqual(refused.returncode, 0)
        self.assertIn("Nothing was delivered for TICKET-41.", refused.stdout)
        self.assertIn("1 path conflict with this change: main.go", refused.stdout)
        self.assertIn("conflict markers", refused.stdout)
        text = (self.workspace / "main.go").read_text()
        self.assertIn("<<<<<<<", text)
        self.assertIn("// theirs", text)
        self.assertIn("// ours", text)
        self.assertTrue((self.workspace / ".git" / "MERGE_HEAD").exists(), "the merge was not left for the next round")
        self.assertEqual(self.state["pulls"], [], "a conflicting branch was published")
        # Unresolved markers are not delivered either.
        again = self.deliver()
        self.assertNotEqual(again.returncode, 0)
        self.assertIn("conflict markers in: main.go", again.stdout)
        # The next round resolves the markers in place; delivery completes the merge.
        self.change("main.go", "package main\n\n// theirs and ours\n")
        done = self.deliver()
        self.assertEqual(done.returncode, 0, done.stdout + done.stderr)
        self.assertEqual(len(self.parents(self.workspace)), 2, "the resolving commit is not a merge")
        self.assertIn("theirs and ours", self.git(self.remote, "show", "refs/heads/master:main.go").stdout)
        self.assertFalse((self.workspace / ".git" / "MERGE_HEAD").exists())

    def test_the_integration_branch_s_own_paths_need_no_grant(self):
        # notes.md and a non-ASCII name are outside the grant; the integration
        # branch changing them is not the worker's change and must not stop
        # the delivery that completes a conflicted merge.
        self.advance_integration_branch("notes.md", "revised elsewhere\n")
        self.advance_integration_branch("メモ.md", "別の依頼の文書\n")
        self.advance_integration_branch("main.go", "package main\n\n// theirs\n")
        self.change("main.go", "package main\n\n// ours\n")
        self.assertNotEqual(self.deliver().returncode, 0)
        self.change("main.go", "package main\n\n// theirs and ours\n")
        done = self.deliver()
        self.assertEqual(done.returncode, 0, done.stdout + done.stderr)
        self.assertIn("revised elsewhere", self.git(self.remote, "show", "refs/heads/master:notes.md").stdout)
        self.assertIn("別の依頼", self.git(self.remote, "show", "refs/heads/master:メモ.md").stdout)

    def test_a_path_the_worker_wrote_needs_the_grant_even_when_the_integration_branch_changed_it(self):
        self.advance_integration_branch("notes.md", "revised elsewhere\n")
        self.advance_integration_branch("main.go", "package main\n\n// theirs\n")
        self.change("main.go", "package main\n\n// ours\n")
        self.assertNotEqual(self.deliver().returncode, 0)
        self.change("main.go", "package main\n\n// theirs and ours\n")
        self.change("notes.md", "smuggled outside the grant\n")
        refused = self.deliver()
        self.assertNotEqual(refused.returncode, 0)
        self.assertIn("outside the operator's allowed paths were not delivered: notes.md", refused.stdout)
        self.assertEqual(self.state["pulls"], [])

    def test_text_the_integration_branch_already_carries_is_not_this_change(self):
        self.advance_integration_branch("library/run.go", "package library\n\n// internal-project-codename\n")
        self.advance_integration_branch("main.go", "package main\n\n// theirs\n")
        self.change("main.go", "package main\n\n// ours\n")
        forbidden = {"DELIVERY_FORBIDDEN_TEXT": "internal-project-codename"}
        self.assertNotEqual(self.deliver(**forbidden).returncode, 0)
        self.change("main.go", "package main\n\n// theirs and ours\n")
        done = self.deliver(**forbidden)
        self.assertEqual(done.returncode, 0, done.stdout + done.stderr)
        # The worker writing it is still refused.
        self.change("main.go", "package main\n\n// internal-project-codename again\n")
        refused = self.deliver(**forbidden)
        self.assertNotEqual(refused.returncode, 0)
        self.assertIn("forbidden text", refused.stdout)

    def test_a_failed_catch_up_leaves_the_commit_on_record_for_the_rerun(self):
        self.change("main.go", "package main\n\nfunc main() {}\n")
        refused = self.deliver(DELIVERY_REMOTE_URL=str(self.root / "no-such-target.git"),
                               DELIVERY_GIT_TIMEOUT_SECONDS="5")
        self.assertNotEqual(refused.returncode, 0)
        self.assertIn("Nothing was delivered", refused.stdout)
        receipt = json.loads((self.workspace / ".git" / "ticket-engine" / "delivery.json").read_text())
        self.assertEqual(receipt.get("head"), self.git(self.workspace, "rev-parse", "HEAD").stdout.strip())
        done = self.deliver()
        self.assertEqual(done.returncode, 0, done.stdout + done.stderr)
        self.assertIn("func main", self.git(self.remote, "show", "refs/heads/master:main.go").stdout)

    def test_a_squashing_service_gets_no_catch_up(self):
        self.advance_integration_branch("library/run.go", "package library\n\nfunc Other() {}\n")
        self.change("main.go", "package main\n\nfunc main() {}\n")
        done = self.deliver(DELIVERY_MERGE_METHOD="squash")
        self.assertEqual(done.returncode, 0, done.stdout + done.stderr)
        self.assertEqual(len(self.parents(self.workspace)), 1, "the branch was caught up under the squash method")
    def remove_on_integration_branch(self, relative, message="Codex: another delivery removes a path"):
        other = self.root / ("other-remove-" + relative.replace("/", "-"))
        self.git(self.root, "clone", "--no-local", str(self.remote), str(other))
        self.git(other, "rm", "-q", relative)
        self.git(other, "commit", "-m", message)
        self.git(other, "push", "origin", "HEAD:master")

    def rename_on_integration_branch(self, relative, target, message="Codex: another delivery renames a path"):
        other = self.root / ("other-rename-" + relative.replace("/", "-"))
        self.git(self.root, "clone", "--no-local", str(self.remote), str(other))
        self.git(other, "mv", relative, target)
        self.git(other, "commit", "-m", message)
        self.git(other, "push", "origin", "HEAD:master")

    def conflict_then_resolve(self, **extra):
        """A conflicting integration branch, refused once; main.go resolved."""
        self.advance_integration_branch("main.go", "package main\n\n// theirs\n")
        self.change("main.go", "package main\n\n// ours\n")
        self.assertNotEqual(self.deliver(**extra).returncode, 0)
        self.change("main.go", "package main\n\n// theirs and ours\n")

    def test_a_path_the_integration_branch_removed_is_not_the_worker_s_to_bring_back(self):
        self.remove_on_integration_branch("notes.md")
        self.conflict_then_resolve()
        self.change("notes.md", "smuggled back outside the grant\n")
        refused = self.deliver()
        self.assertNotEqual(refused.returncode, 0)
        self.assertIn("outside the operator's allowed paths were not delivered: notes.md", refused.stdout)

    def test_a_path_the_integration_branch_renamed_needs_no_grant(self):
        self.rename_on_integration_branch("notes.md", "記録.md")
        self.conflict_then_resolve()
        done = self.deliver()
        self.assertEqual(done.returncode, 0, done.stdout + done.stderr)
        self.assertIn("original", self.git(self.remote, "show", "refs/heads/master:記録.md").stdout)

    def test_forbidden_text_next_to_the_resolution_is_not_this_change(self):
        self.advance_integration_branch("main.go", "package main\n\n// internal-project-codename stays\n// theirs\n")
        self.change("main.go", "package main\n\n// ours\n")
        forbidden = {"DELIVERY_FORBIDDEN_TEXT": "internal-project-codename"}
        self.assertNotEqual(self.deliver(**forbidden).returncode, 0)
        # Keeping the integration branch's line as context: delivered.
        self.change("main.go", "package main\n\n// internal-project-codename stays\n// theirs and ours\n")
        done = self.deliver(**forbidden)
        self.assertEqual(done.returncode, 0, done.stdout + done.stderr)

    def test_removing_a_forbidden_line_the_integration_branch_wrote_is_not_this_change(self):
        self.advance_integration_branch("main.go", "package main\n\n// internal-project-codename stays\n// theirs\n")
        self.change("main.go", "package main\n\n// ours\n")
        forbidden = {"DELIVERY_FORBIDDEN_TEXT": "internal-project-codename"}
        self.assertNotEqual(self.deliver(**forbidden).returncode, 0)
        self.change("main.go", "package main\n\n// theirs and ours\n")
        done = self.deliver(**forbidden)
        self.assertEqual(done.returncode, 0, done.stdout + done.stderr)

    def test_a_resolution_back_to_this_branch_s_own_content_still_concludes_the_merge(self):
        self.advance_integration_branch("main.go", "package main\n\n// theirs\n")
        self.change("main.go", "package main\n\n// ours\n")
        self.assertNotEqual(self.deliver().returncode, 0)
        self.change("main.go", "package main\n\n// ours\n")
        done = self.deliver()
        self.assertEqual(done.returncode, 0, done.stdout + done.stderr)
        self.assertEqual(len(self.parents(self.workspace)), 2, "the unchanged resolution did not conclude the merge")
        self.assertFalse((self.workspace / ".git" / "MERGE_HEAD").exists())
        self.assertIn("// ours", self.git(self.remote, "show", "refs/heads/master:main.go").stdout)

    def test_an_added_line_that_begins_with_plus_signs_is_still_scanned(self):
        self.change("main.go", "package main\n\n++" + TOKEN + "\n")
        refused = self.deliver()
        self.assertNotEqual(refused.returncode, 0)
        self.assertIn("delivery credential", refused.stdout)
        self.change("main.go", "package main\n\n++internal-project-codename\n")
        refused = self.deliver(DELIVERY_FORBIDDEN_TEXT="internal-project-codename")
        self.assertNotEqual(refused.returncode, 0)
        self.assertIn("forbidden text", refused.stdout)
        self.assertEqual(self.state["pulls"], [])

    def test_a_forbidden_word_in_a_path_name_is_refused(self):
        self.change("library/internal-project-codename.go", "package library\n")
        refused = self.deliver(DELIVERY_FORBIDDEN_TEXT="internal-project-codename")
        self.assertNotEqual(refused.returncode, 0)
        self.assertIn("forbidden text", refused.stdout)
        self.assertEqual(self.state["pulls"], [])

    def refused_as_written(self, relative, data, expected, **settings):
        """One file written as these bytes, and a delivery that refuses it."""
        path = self.workspace / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(data)
        refused = self.deliver(**settings)
        self.assertEqual(refused.returncode, 1, refused.stdout + refused.stderr)
        self.assertIn(expected, refused.stdout)
        self.assertNotIn(TOKEN, refused.stdout + refused.stderr)
        self.assertEqual((self.remote_branches(), self.methods()), (["refs/heads/master"], []))

    def forbidden_text_and_credential_refused_in(self, relative, encode):
        self.refused_as_written(relative, encode("package main // internal-project-codename\n"),
                                "contains configured forbidden text (1 entry)",
                                DELIVERY_FORBIDDEN_TEXT="internal-project-codename")
        self.refused_as_written(relative, encode("package main // %s\n" % TOKEN), "contains the delivery credential")

    def test_text_after_a_break_inside_a_line_is_still_looked_at(self):
        # Git ends a line at LF only. Read as text, a CR in a line became a
        # line end, and splitting the text split a line at a form feed, a
        # vertical tab or a line separator as well: what followed was not
        # taken as added, and was delivered.
        for mark in ("\r", "\x0b", "\x0c", "\N{LINE SEPARATOR}"):
            self.forbidden_text_and_credential_refused_in(
                "main.go", lambda text: text.replace("// ", "//" + mark).encode())

    def test_text_in_a_file_with_a_nul_byte_is_still_looked_at(self):
        # Git takes such a file as binary and shows no line of it.
        self.forbidden_text_and_credential_refused_in("library/data.bin", lambda text: b"\x00" + text.encode())

    def test_text_in_a_file_an_attribute_keeps_from_the_diff_is_still_looked_at(self):
        (self.workspace / "library").mkdir(exist_ok=True)
        (self.workspace / "library" / ".gitattributes").write_text("*.txt -diff\n")
        self.forbidden_text_and_credential_refused_in("library/notes.txt", str.encode)

    def test_text_in_utf16_is_still_looked_at(self):
        # Git takes it as binary for its NULs: in UTF-16 each ASCII character
        # has one beside it.
        self.forbidden_text_and_credential_refused_in("library/notes.txt",
                                                      lambda text: ("\N{BYTE ORDER MARK}" + text).encode("utf-16-le"))

    def first_cut(self):
        """Where, in a file that is one added line, the first part the check
        holds of that line ends: the diff puts one byte before the line. The
        line has to go on past the next read of Git's output for the part to
        be cut there, so these lines go on 2 MiB further."""
        spec = importlib.util.spec_from_file_location("delivery_support_parts", SUPPORT)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        return module.PART - 1

    def test_text_across_the_parts_of_a_long_line_is_still_looked_at(self):
        # A line longer than a part is looked at in parts that overlap by more
        # than any text the check looks for takes.
        cut, more = self.first_cut(), b"a" * (2 << 20)
        word = b"internal-project-codename"
        for found, expected, settings in ((word, "forbidden text (1 entry)",
                                           {"DELIVERY_FORBIDDEN_TEXT": "internal-project-codename"}),
                                          (TOKEN.encode(), "the delivery credential", {})):
            self.refused_as_written("library/long.txt", b"a" * (cut - len(found) + 1) + found + more + b"\n",
                                    expected, **settings)
        # In UTF-16, the word with a NUL beside each character, 50 bytes, 41 of
        # them before the cut: neither part holds all of it unless the overlap
        # counts the NULs as well.
        self.refused_as_written("library/long.txt", "\N{BYTE ORDER MARK}".encode("utf-16-le")
                                + ("a" * ((cut - 43) // 2) + "internal-project-codename" + "a" * len(more) + "\n")
                                .encode("utf-16-le"), "forbidden text (1 entry)",
                                DELIVERY_FORBIDDEN_TEXT="internal-project-codename")
        # A word that is not ASCII, across the cut, in characters of three bytes.
        self.refused_as_written("library/long.txt", ("x" + "あ" * ((cut - 7) // 3) + "社外秘" + "あ" * len(more) + "\n")
                                .encode(), "forbidden text (1 entry)", DELIVERY_FORBIDDEN_TEXT="社外秘")

    def test_a_long_line_without_forbidden_text_is_delivered_as_it_is(self):
        # The cut falls inside a character of three bytes; read as two halves,
        # each would be bytes that are not UTF-8, which an entry that is not
        # ASCII makes a refusal.
        cut = self.first_cut()
        content = ("x" + "あ" * (cut // 3 + (2 << 20)) + "\n").encode()
        self.assertTrue(0x80 <= content[cut] < 0xC0, "the cut is not inside a character")
        (self.workspace / "library").mkdir(exist_ok=True)
        (self.workspace / "library" / "long.txt").write_bytes(content)
        done = self.deliver(DELIVERY_FORBIDDEN_TEXT="社外秘")
        self.assertEqual(done.returncode, 0, done.stdout + done.stderr)
        self.assertEqual(self.stored("refs/heads/master:library/long.txt"), content)

    def test_a_large_file_of_random_bytes_is_delivered(self):
        # 200 MB, whose lines the check looks at one by one.
        (self.workspace / "library").mkdir(exist_ok=True)
        path = self.workspace / "library" / "random.bin"
        with open(path, "wb") as written:
            for _ in range(200):
                written.write(os.urandom(1 << 20))
        done = self.deliver(DELIVERY_FORBIDDEN_TEXT="internal-project-codename")
        self.assertEqual(done.returncode, 0, done.stdout + done.stderr)
        self.assertEqual(self.git(self.remote, "rev-parse", "refs/heads/master:library/random.bin").stdout,
                         self.git(self.workspace, "hash-object", str(path)).stdout)

    def test_a_diff_program_does_not_stand_in_for_the_lines(self):
        # One that prints its own format, named in the checkout's configuration.
        self.git(self.workspace, "config", "diff.external", "printf 'changed: %s\\n'")
        self.refused_as_written("main.go", b"package main // internal-project-codename\n",
                                "contains configured forbidden text (1 entry)",
                                DELIVERY_FORBIDDEN_TEXT="internal-project-codename")

    def test_a_text_conversion_does_not_stand_in_for_the_lines(self):
        # One that the checkout's configuration defines and an attribute names.
        self.git(self.workspace, "config", "diff.plain.textconv", "true")
        (self.workspace / "library").mkdir(exist_ok=True)
        (self.workspace / "library" / ".gitattributes").write_text("*.txt diff=plain\n")
        self.forbidden_text_and_credential_refused_in("library/notes.txt", str.encode)

    def test_a_catch_up_merge_made_before_its_receipt_was_written_is_delivered(self):
        self.advance_integration_branch("library/run.go", "package library\n\nfunc Other() {}\n")
        self.change("main.go", "package main\n\nfunc main() {}\n")
        done = self.deliver()
        self.assertEqual(done.returncode, 0, done.stdout + done.stderr)
        # The state a kill between the merge commit and the receipt write
        # leaves: the receipt names the merge's first parent.
        receipt_path = self.workspace / ".git" / "ticket-engine" / "delivery.json"
        receipt = json.loads(receipt_path.read_text())
        receipt["head"] = self.parents(self.workspace)[0]
        for key in ("pushed_at", "pull_request", "pull_request_url", "opened_at", "merge_sha", "merged_at"):
            receipt.pop(key, None)
        receipt_path.write_text(json.dumps(receipt))
        self.git(self.remote, "update-ref", "-d", "refs/heads/ticket/TICKET-41")
        again = self.deliver()
        self.assertEqual(again.returncode, 0, again.stdout + again.stderr)
        self.assertEqual(json.loads(receipt_path.read_text())["head"],
                         self.git(self.workspace, "rev-parse", "HEAD").stdout.strip())

    def pushes(self):
        return [line for line in self.git_log.read_text().splitlines() if " push " in line]

    def test_a_request_that_changed_nothing_ends_without_a_delivery_when_the_operator_allows_it(self):
        started = self.git(self.workspace, "rev-parse", "HEAD").stdout.strip()
        # Another request was merged since this checkout: the request stands on
        # the branch as it is now, which still contains where the work started.
        self.advance_integration_branch("library/run.go", "package library\n\nfunc Other() {}\n")
        tip = self.git(self.remote, "rev-parse", "refs/heads/master").stdout.strip()
        result = self.deliver(DELIVERY_ALLOW_UNCHANGED="1")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("Nothing was delivered for TICKET-41: no file was changed.", result.stdout)
        self.assertIn("The request stands on master of owner/project as it is at %s;" % tip, result.stdout)
        self.assertIn("what the review before this stage judged", result.stdout)
        self.assertEqual(self.receipt(), {"unchanged": True, "issue": "TICKET-41", "repository": "owner/project",
                                          "base_branch": "master", "base_sha": tip, "workspace_head": started})
        self.assertEqual(self.git(self.workspace, "rev-parse", "HEAD").stdout.strip(), started)
        self.assertEqual(self.git(self.workspace, "status", "--porcelain").stdout, "")
        self.assertEqual(self.remote_branches(), ["refs/heads/master"])
        self.assertEqual(self.git(self.remote, "rev-parse", "refs/heads/master").stdout.strip(), tip)
        self.assertEqual(self.pushes(), [])
        self.assertEqual(self.methods(), [], "the service was asked something although nothing was delivered")

    def test_without_the_setting_a_request_that_changed_nothing_is_refused_as_before(self):
        for setting in ({}, {"DELIVERY_ALLOW_UNCHANGED": "0"}):
            refused = self.deliver(**setting)
            self.assertEqual(refused.returncode, 1, refused.stdout + refused.stderr)
            self.assertIn("What stopped it: No change under the allowed paths is ready to deliver\n", refused.stdout)
            self.assertEqual(refused.stderr, "No change under the allowed paths is ready to deliver\n")
        self.assertEqual(self.receipt(), {})
        self.assertEqual(self.remote_branches(), ["refs/heads/master"])
        self.assertEqual(self.methods(), [])

    def test_a_mistyped_setting_is_refused_rather_than_read_as_off(self):
        self.change("main.go", "package main // delivered\n")
        refused = self.deliver(DELIVERY_ALLOW_UNCHANGED="yes")
        self.assertEqual(refused.returncode, 1, refused.stdout + refused.stderr)
        self.assertIn("DELIVERY_ALLOW_UNCHANGED must be 1, 0 or unset", refused.stderr)
        self.assertEqual(self.remote_branches(), ["refs/heads/master"])
        self.assertEqual(self.methods(), [])

    def test_a_checkout_that_is_not_part_of_the_integration_branch_is_refused_as_before(self):
        self.git(self.workspace, "commit", "--allow-empty", "-m", "Codex: a commit the integration branch lacks")
        refused = self.deliver(DELIVERY_ALLOW_UNCHANGED="1")
        self.assertEqual(refused.returncode, 1, refused.stdout + refused.stderr)
        self.assertIn("What stopped it: No change under the allowed paths is ready to deliver", refused.stdout)
        self.assertIn("is not part of master as it is now", refused.stdout)
        self.assertEqual(self.receipt(), {})
        self.assertEqual(self.remote_branches(), ["refs/heads/master"])
        self.assertEqual(self.pushes(), [])
        self.assertEqual(self.methods(), [])

    def test_a_rerun_after_ending_unchanged_says_the_same_and_pushes_nothing(self):
        first = self.deliver(DELIVERY_ALLOW_UNCHANGED="1")
        self.assertEqual(first.returncode, 0, first.stdout + first.stderr)
        receipt = self.receipt()
        again = self.deliver(DELIVERY_ALLOW_UNCHANGED="1")
        self.assertEqual(again.returncode, 0, again.stdout + again.stderr)
        self.assertEqual(again.stdout, first.stdout)
        self.assertEqual(self.receipt(), receipt)
        # The ending is no round to continue: with the setting gone, the same
        # tree is refused as it always was, and still nothing is pushed.
        refused = self.deliver()
        self.assertEqual(refused.returncode, 1, refused.stdout + refused.stderr)
        self.assertIn("No change under the allowed paths is ready to deliver", refused.stdout)
        self.assertEqual(self.remote_branches(), ["refs/heads/master"])
        self.assertEqual(self.pushes(), [])
        self.assertEqual(self.methods(), [])

    def test_work_changed_after_ending_unchanged_is_delivered_as_a_first_round(self):
        self.assertEqual(self.deliver(DELIVERY_ALLOW_UNCHANGED="1").returncode, 0)
        self.change("main.go", "package main // delivered after all\n")
        done = self.deliver(DELIVERY_ALLOW_UNCHANGED="1")
        self.assertEqual(done.returncode, 0, done.stdout + done.stderr)
        receipt = self.receipt()
        self.assertNotIn("unchanged", receipt)
        self.assertEqual(receipt["previous"], [])
        self.assertEqual(receipt["pull_request"], 1)
        self.assertRegex(receipt["merge_sha"], r"\A[0-9a-f]{40}\Z")
        self.assertIn("delivered after all", self.git(self.remote, "show", "refs/heads/master:main.go").stdout)
        self.assertEqual(self.methods().count("POST"), 1)
        self.assertEqual(self.methods().count("PUT"), 1)
        # A delivered round with nothing new is reported as delivered, as before.
        again = self.deliver(DELIVERY_ALLOW_UNCHANGED="1")
        self.assertEqual(again.returncode, 0, again.stdout + again.stderr)
        self.assertIn("Delivered TICKET-41", again.stdout)
        self.assertEqual(self.receipt(), receipt)

    def test_a_round_whose_merge_was_refused_is_continued_not_ended_unchanged(self):
        # The change is committed in the round, so the tree is clean afterwards;
        # the round on record is what the next delivery carries on.
        self.state["merge_refusal"] = (405, 'Required status check "build" is expected.')
        self.change("main.go", "package main // delivered\n")
        refused = self.deliver(DELIVERY_ALLOW_UNCHANGED="1")
        self.assertEqual(refused.returncode, 1, refused.stdout + refused.stderr)
        interrupted = self.receipt()
        self.assertNotIn("merge_sha", interrupted)
        self.assertEqual(self.git(self.workspace, "status", "--porcelain").stdout, "")
        self.state["merge_refusal"] = None
        done = self.deliver(DELIVERY_ALLOW_UNCHANGED="1")
        self.assertEqual(done.returncode, 0, done.stdout + done.stderr)
        self.assertIn("Delivered TICKET-41", done.stdout)
        self.assertNotIn("no file was changed", done.stdout)
        self.assertEqual(self.receipt()["head"], interrupted["head"])
        self.assertIn("// delivered", self.git(self.remote, "show", "refs/heads/master:main.go").stdout)
        self.assertEqual(self.methods().count("POST"), 1)

    def delivered_although_settings_hide_it(self, settings):
        """A new file, with settings handed to Git through the delivery's
        environment only that would hide it, as an exclude file does."""
        self.change("library/new.go", "package library // a new file the review saw\n")
        done = self.deliver(DELIVERY_ALLOW_UNCHANGED="1", **settings)
        self.assertEqual(done.returncode, 0, done.stdout + done.stderr)
        self.assertIn("Delivered TICKET-41", done.stdout)
        self.assertNotIn("no file was changed", done.stdout)
        self.assertIn("a new file the review saw",
                      self.git(self.remote, "show", "refs/heads/master:library/new.go").stdout)

    def hide_the_new_file(self):
        hide = self.root / "hide-new-file"
        hide.write_text("library/new.go\n")
        return hide

    # G2 of the final review of the ending with no change: the delivery took
    # the new file the review had seen for no change at all, and ended without
    # a delivery. Its Git on the workspace drops these settings now, as the
    # review's Git does.

    def test_a_count_of_settings_in_the_environment_does_not_hide_a_change(self):
        self.delivered_although_settings_hide_it({"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "core.excludesFile",
                                                  "GIT_CONFIG_VALUE_0": str(self.hide_the_new_file())})

    def test_settings_passed_as_by_git_itself_do_not_hide_a_change(self):
        self.delivered_although_settings_hide_it(
            {"GIT_CONFIG_PARAMETERS": "'core.excludesFile'='%s'" % self.hide_the_new_file()})

    # The -c the delivery gives its Git on the workspace keep an exclude file
    # out whatever the environment says; core.fileMode=false is kept out only
    # by dropping the settings.

    def delivered_with_its_mode(self, settings):
        """main.go made executable, its only change, delivered as it is."""
        (self.workspace / "main.go").chmod(0o755)
        done = self.deliver(DELIVERY_ALLOW_UNCHANGED="1", **settings)
        self.assertEqual(done.returncode, 0, done.stdout + done.stderr)
        self.assertIn("Delivered TICKET-41", done.stdout)
        listed = self.git(self.remote, "ls-tree", "refs/heads/master", "main.go").stdout
        self.assertTrue(listed.startswith("100755 "), listed)

    def test_a_count_of_settings_does_not_hide_a_change_of_mode(self):
        self.delivered_with_its_mode({"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "core.fileMode",
                                      "GIT_CONFIG_VALUE_0": "false"})

    def test_settings_passed_as_by_git_itself_do_not_hide_a_change_of_mode(self):
        self.delivered_with_its_mode({"GIT_CONFIG_PARAMETERS": "'core.fileMode'='false'"})

    # The same kind, closed in the second review of this change: Git's default
    # exclude and attributes files, and the variables that pick its attributes
    # or its diff program, in the delivery's environment only.

    def test_git_s_default_exclude_file_does_not_hide_a_change(self):
        xdg = self.root / "xdg"
        (xdg / "git").mkdir(parents=True)
        (xdg / "git" / "ignore").write_text("new.go\n")
        self.delivered_although_settings_hide_it({"XDG_CONFIG_HOME": str(xdg)})

    def delivered_with_its_line_ends(self, **settings):
        """main.go with only its line ends turned into CRLF, delivered as it is."""
        (self.workspace / "main.go").write_bytes(b"package main\r\n")
        done = self.deliver(**settings)
        self.assertEqual(done.returncode, 0, done.stdout + done.stderr)
        self.assertIn("Delivered TICKET-41", done.stdout)
        delivered = subprocess.run(["git", "-C", str(self.remote), "show", "refs/heads/master:main.go"],
                                   capture_output=True, check=True, env=git_environment()).stdout
        self.assertEqual(delivered, b"package main\r\n")

    def test_git_s_default_attributes_file_does_not_hide_a_change(self):
        (self.home / ".config" / "git").mkdir(parents=True)
        (self.home / ".config" / "git" / "attributes").write_text("*.go text\n")
        self.delivered_with_its_line_ends()

    def test_attributes_taken_from_elsewhere_do_not_hide_a_change(self):
        def git_in(*arguments, text):
            return subprocess.run(["git", "-C", str(self.workspace), *arguments], input=text, capture_output=True,
                                  text=True, check=True, env=git_environment()).stdout.strip()
        blob = git_in("hash-object", "-w", "--stdin", text="*.go text\n")
        self.delivered_with_its_line_ends(
            GIT_ATTR_SOURCE=git_in("mktree", text="100644 blob %s\t.gitattributes\n" % blob))

    def test_a_diff_program_in_the_environment_hides_no_change_and_no_forbidden_text(self):
        self.change("main.go", "package main // delivered past a diff program\n")
        done = self.deliver(GIT_EXTERNAL_DIFF="true")
        self.assertEqual(done.returncode, 0, done.stdout + done.stderr)
        self.assertIn("delivered past a diff program",
                      self.git(self.remote, "show", "refs/heads/master:main.go").stdout)
        # A diff program that prints its own format, as some do, no longer lets
        # forbidden text through.
        self.change("main.go", "package main // internal-project-codename\n")
        refused = self.deliver(GIT_EXTERNAL_DIFF="printf 'changed: %s\\n'",
                               DELIVERY_FORBIDDEN_TEXT="internal-project-codename")
        self.assertEqual(refused.returncode, 1, refused.stdout + refused.stderr)
        self.assertIn("forbidden text", refused.stdout)

    def test_the_git_that_talks_to_the_service_keeps_the_operator_s_settings(self):
        # Only the delivery's Git on the workspace drops settings handed to Git
        # through the environment: the Git that fetches, lists and pushes keeps
        # them, as an operator's network may need. Here one is the only way to
        # the target.
        settings = {"DELIVERY_REMOTE_URL": "example-target:project.git", "GIT_CONFIG_COUNT": "1",
                    "GIT_CONFIG_KEY_0": "url.%s.insteadOf" % self.remote,
                    "GIT_CONFIG_VALUE_0": "example-target:project.git"}
        self.change("main.go", "package main // delivered by way of a setting\n")
        checked = self.deliver("--dry-run", **settings)
        self.assertEqual(checked.returncode, 3, checked.stdout + checked.stderr)
        self.assertIn("Listing the branch over Git exited 0 with ", checked.stdout)
        done = self.deliver(**settings)
        self.assertEqual(done.returncode, 0, done.stdout + done.stderr)
        self.assertIn("delivered by way of a setting",
                      self.git(self.remote, "show", "refs/heads/master:main.go").stdout)

    def leave_merge(self, **extra):
        return self.deliver(DELIVERY_MERGE_METHOD="none", **extra)

    def person_merges(self, number):
        """What a person merging the pull request at the service leaves behind."""
        pull = self.state["pulls"][number - 1]
        base = self.git(self.remote, "rev-parse", "refs/heads/" + pull["base"]).stdout.strip()
        head = self.git(self.remote, "rev-parse", "refs/heads/" + pull["head"]["ref"]).stdout.strip()
        tree = self.git(self.remote, "rev-parse", "refs/heads/%s^{tree}" % pull["head"]["ref"]).stdout.strip()
        merge = self.git(self.remote, "commit-tree", tree, "-p", base, "-p", head,
                         "-m", "Merged by a person").stdout.strip()
        self.git(self.remote, "update-ref", "refs/heads/" + pull["base"], merge)
        pull["head"]["sha"] = head
        pull.update(merged=True, state="closed", merge_commit_sha=merge, merged_at="2026-01-02T00:00:00Z")
        return merge

    def test_without_a_merge_method_the_delivery_ends_at_the_open_pull_request(self):
        self.change("main.go", "package main // left for a person\n")
        result = self.leave_merge()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("Pull request 1 against master is open for TICKET-41: http://service.invalid/pulls/1.\n"
                      "Merging is left to a person; nothing was merged.\n", result.stdout)
        receipt = self.receipt()
        self.assertEqual((receipt["pull_request"], receipt["pull_request_url"], receipt["merge_method"]),
                         (1, "http://service.invalid/pulls/1", "none"))
        self.assertIs(receipt["merge_left_to_person"], True)
        self.assertNotIn("merge_sha", receipt)
        self.assertEqual(self.git(self.remote, "rev-parse", "refs/heads/ticket/TICKET-41").stdout.strip(),
                         receipt["head"])
        self.assertNotIn("left for a person", self.git(self.remote, "show", "refs/heads/master:main.go").stdout)
        self.assertNotIn("PUT", self.methods())
        self.assertEqual(self.methods().count("POST"), 1)
        self.assertIn("Merging it is left to a person.", self.state["pulls"][0]["body"])

    def test_a_rerun_with_nothing_new_reports_the_open_pull_request_again(self):
        self.change("main.go", "package main // left for a person\n")
        self.assertEqual(self.leave_merge().returncode, 0)
        receipt, pushes = self.receipt(), self.pushes()
        again = self.leave_merge()
        self.assertEqual(again.returncode, 0, again.stdout + again.stderr)
        self.assertIn("Pull request 1 against master is open for TICKET-41: http://service.invalid/pulls/1.\n"
                      "Merging is left to a person; nothing was merged.\n", again.stdout)
        self.assertIn("This round reused a commit and did not need to push the branch", again.stdout)
        self.assertEqual(self.receipt(), receipt)
        # Not even a moved integration branch adds a commit: with nothing new,
        # what the person was asked to merge stays as it is.
        self.advance_integration_branch("library/run.go", "package library\n\nfunc Other() {}\n")
        self.assertEqual(self.leave_merge().stdout, again.stdout)
        self.assertEqual(self.receipt(), receipt)
        self.assertEqual(self.pushes(), pushes)
        self.assertEqual(self.methods().count("POST"), 1)
        self.assertNotIn("PUT", self.methods())

    def test_later_work_goes_to_the_same_branch_and_pull_request(self):
        self.change("main.go", "package main // first round\n")
        self.assertEqual(self.leave_merge().returncode, 0)
        first = self.receipt()
        self.change("main.go", "package main // reviewed again\n")
        later = self.leave_merge()
        self.assertEqual(later.returncode, 0, later.stdout + later.stderr)
        receipt = self.receipt()
        self.assertEqual(receipt["pull_request"], 1)
        self.assertNotEqual(receipt["head"], first["head"])
        self.assertEqual(self.git(self.remote, "rev-parse", "refs/heads/ticket/TICKET-41").stdout.strip(),
                         receipt["head"])
        self.assertIn("This round created a commit and pushed the branch", later.stdout)
        self.assertEqual(len(self.pushes()), 2)
        self.assertEqual(self.methods().count("POST"), 1)
        self.assertNotIn("PUT", self.methods())

    def test_a_pull_request_a_person_merged_is_reported_as_merged_by_someone_else(self):
        self.change("main.go", "package main // left for a person\n")
        self.assertEqual(self.leave_merge().returncode, 0)
        merge = self.person_merges(1)
        found = self.leave_merge()
        self.assertEqual(found.returncode, 0, found.stdout + found.stderr)
        self.assertIn("Pull request 1 against master was merged by someone else as commit %s; this process "
                      "merged nothing." % merge, found.stdout)
        receipt = self.receipt()
        self.assertEqual(receipt["merge_sha"], merge)
        self.assertIs(receipt["merge_left_to_person"], True)
        self.assertNotIn("PUT", self.methods())
        # Reported again the same way, and work after it is a further round
        # with a pull request of its own, also left to a person.
        self.assertEqual(self.leave_merge().stdout, found.stdout)
        self.change("main.go", "package main // after the person's merge\n")
        further = self.leave_merge()
        self.assertEqual(further.returncode, 0, further.stdout + further.stderr)
        self.assertIn("Pull request 2 against master is open for TICKET-41", further.stdout)
        self.assertIn("Earlier merged rounds for this ticket: 1.", further.stdout)
        receipt = self.receipt()
        self.assertEqual([round["merge_sha"] for round in receipt["previous"]], [merge])
        self.assertNotIn("merge_sha", receipt)
        self.assertNotIn("PUT", self.methods())

    CLOSED = ("Pull request 1 against master for TICKET-41 was closed by a person without being merged: "
              "http://service.invalid/pulls/1.\n")
    CLOSED_THEN = ("When a delivery read it at %s, pull request 1 against master for TICKET-41 had been closed by a "
                   "person without being merged: http://service.invalid/pulls/1. This delivery did not read it again.\n")
    ENDS = ("This request ends with nothing delivered. The pull request is not reopened and no other is opened in its "
            "place; continuing needs a new request.")
    ENDED_THEN = ("This request ended then with nothing delivered. This process opens no other pull request in its "
                  "place; continuing needs a new request.")

    def test_a_pull_request_a_person_closed_unmerged_ends_the_request_without_reopening_it(self):
        # A person closing the pull request is their decision: the request ends,
        # saying so, instead of going back to work round after round.
        self.change("main.go", "package main // left for a person\n")
        self.assertEqual(self.leave_merge().returncode, 0)
        pushes = self.pushes()
        self.state["pulls"][0]["state"] = "closed"
        ended = self.leave_merge()
        self.assertEqual(ended.returncode, 0, ended.stdout + ended.stderr)
        self.assertIn(self.CLOSED + self.ENDS, ended.stdout)
        receipt = self.receipt()
        self.assertIs(receipt["closed_unmerged"], True)
        # A later delivery, here with more work, says what was read and when.
        self.change("main.go", "package main // more work after the close\n")
        again = self.leave_merge()
        self.assertEqual(again.returncode, 0, again.stdout + again.stderr)
        self.assertIn(self.CLOSED_THEN % receipt["ended_at"] + self.ENDED_THEN, again.stdout)
        self.assertEqual(self.receipt(), receipt)
        self.assertEqual(self.pushes(), pushes, "work was pushed to a pull request a person closed")
        self.assertEqual(self.methods().count("POST"), 1)
        self.assertNotIn("PUT", self.methods())
        self.assertEqual(self.state["pulls"][0]["state"], "closed")

    def test_the_catch_up_still_applies_when_the_merge_is_left_to_a_person(self):
        self.advance_integration_branch("library/run.go", "package library\n\nfunc Other() {}\n")
        self.change("main.go", "package main\n\nfunc main() {}\n")
        done = self.leave_merge()
        self.assertEqual(done.returncode, 0, done.stdout + done.stderr)
        self.assertEqual(len(self.parents(self.workspace)), 2, "the ticket branch did not take the merge")
        self.assertIn("Other", self.git(self.remote, "show", "refs/heads/ticket/TICKET-41:library/run.go").stdout)
        # A conflict is still left in the tree for the work stage, and nothing is opened.
        self.advance_integration_branch("main.go", "package main\n\n// theirs\n", message="Codex: a conflicting one")
        self.change("main.go", "package main\n\n// ours\n")
        refused = self.leave_merge()
        self.assertNotEqual(refused.returncode, 0)
        self.assertIn("1 path conflict with this change: main.go", refused.stdout)
        self.assertTrue((self.workspace / ".git" / "MERGE_HEAD").exists())
        self.assertEqual(self.methods().count("POST"), 1)

    def test_a_round_left_to_a_person_is_merged_once_the_operator_says_merge(self):
        self.change("main.go", "package main // left for a person\n")
        self.assertEqual(self.leave_merge().returncode, 0)
        merged = self.deliver()
        self.assertEqual(merged.returncode, 0, merged.stdout + merged.stderr)
        self.assertIn("was merged with method merge as commit", merged.stdout)
        receipt = self.receipt()
        self.assertNotIn("merge_left_to_person", receipt)
        self.assertEqual(receipt["pull_request"], 1)
        self.assertEqual(self.methods().count("PUT"), 1)

    def test_a_round_the_service_would_not_merge_is_left_to_a_person_once_the_operator_says_none(self):
        self.state["merge_refusal"] = (405, 'Required status check "build" is expected.')
        self.change("main.go", "package main // delivered\n")
        self.assertEqual(self.deliver().returncode, 1)
        left = self.leave_merge()
        self.assertEqual(left.returncode, 0, left.stdout + left.stderr)
        self.assertIn("Pull request 1 against master is open for TICKET-41", left.stdout)
        receipt = self.receipt()
        self.assertEqual((receipt["merge_method"], receipt["merge_left_to_person"]), ("none", True))
        self.assertNotIn("merge_sha", receipt)
        self.assertEqual(self.methods().count("PUT"), 1)

    # What a person can do with the pull request a delivery left to them.

    def person_clone(self):
        other = self.root / ("person-%d" % len(list(self.root.iterdir())))
        self.git(self.root, "clone", "--no-local", "--branch", "ticket/TICKET-41", str(self.remote), str(other))
        return other

    def person_pushes(self, relative, text):
        other = self.person_clone()
        (other / relative).write_text(text)
        self.git(other, "add", "-A")
        self.git(other, "commit", "-m", "Codex: a person's own commit on the pull request")
        self.git(other, "push", "origin", "HEAD:refs/heads/ticket/TICKET-41")
        return self.git(other, "rev-parse", "HEAD").stdout.strip()

    def person_rewrites(self):
        """What an amend and a force push, or a rebase by the service, leave."""
        other = self.person_clone()
        self.git(other, "commit", "--amend", "-m", "Codex: the same change, rewritten by a person")
        self.git(other, "push", "--force", "origin", "HEAD:refs/heads/ticket/TICKET-41")
        return self.git(other, "rev-parse", "HEAD").stdout.strip()

    def person_squash_merges(self, number):
        pull = self.state["pulls"][number - 1]
        target = self.git(self.remote, "rev-parse", "refs/heads/" + pull["base"]).stdout.strip()
        head = self.git(self.remote, "rev-parse", "refs/heads/" + pull["head"]["ref"]).stdout.strip()
        tree = self.git(self.remote, "rev-parse", head + "^{tree}").stdout.strip()
        squash = self.git(self.remote, "commit-tree", tree, "-p", target,
                          "-m", "Codex: squashed by a person").stdout.strip()
        self.git(self.remote, "update-ref", "refs/heads/" + pull["base"], squash)
        pull["head"]["sha"] = head
        pull.update(merged=True, state="closed", merge_commit_sha=squash, merged_at="2026-01-03T00:00:00Z")
        return squash

    def merge_right_after_the_next_read(self, number, reads=1):
        """A person merges the pull request just after the service answered the
        delivery's read of it (the reads-th from now): the answer says open,
        the merge comes before whatever the delivery does next."""
        state, test = self.state, self
        original = self.server.RequestHandlerClass

        class Racing(original):
            def do_GET(self):
                if urllib.parse.urlsplit(self.path).path == "/repos/owner/project/pulls/%d" % number \
                        and state.get("race_reads"):
                    state["race_reads"] -= 1
                    if state["race_reads"]:
                        return super().do_GET()
                    self.record()
                    snapshot = json.loads(json.dumps(self.current(state["pulls"][number - 1])))
                    state["race_merge"] = test.person_merges(number)
                    return self.answer(200, snapshot)
                return super().do_GET()

        self.server.RequestHandlerClass = Racing
        self.state["race_reads"] = reads

    def refuse_pushes(self):
        """The service refuses pushes while the returned marker exists."""
        marker = self.root / "refuse-pushes"
        marker.touch()
        hook = self.remote / "hooks" / "pre-receive"
        hook.write_text("#!/bin/sh\nif [ -e '%s' ]; then echo 'refused for this test' >&2; exit 1; fi\nexit 0\n"
                        % marker)
        hook.chmod(0o755)
        return marker

    def fail_integration_fetch(self):
        """Fetching the integration branch fails while the returned marker exists."""
        marker = self.root / "fail-integration-fetch"
        marker.touch()
        directory = self.root / "failing-bin"
        directory.mkdir()
        shim = directory / "git"
        shim.write_text('#!/bin/sh\ncase " $* " in *" fetch "*"refs/heads/master"*) if [ -e "%s" ]; then '
                        'echo "fatal: repository not found for this test" >&2; exit 128; fi;; esac\nexec "%s" "$@"\n'
                        % (marker, self.root / "bin" / "git"))
        shim.chmod(0o755)
        self.path = str(directory) + os.pathsep + self.path
        return marker

    def published(self):
        return self.git(self.remote, "rev-parse", "refs/heads/ticket/TICKET-41").stdout.strip()

    def test_a_round_whose_push_was_refused_is_pushed_by_the_next_delivery(self):
        # s02c: the round's commit is on record but never reached the branch.
        # The next delivery must push it, not report the pull request as
        # carrying it.
        self.change("main.go", "package main // first round\n")
        self.assertEqual(self.leave_merge().returncode, 0)
        marker = self.refuse_pushes()
        self.change("main.go", "package main // reviewed again\n")
        self.assertEqual(self.leave_merge().returncode, 1)
        unpushed = self.receipt()["head"]
        self.assertNotEqual(self.published(), unpushed)
        marker.unlink()
        again = self.leave_merge()
        self.assertEqual(again.returncode, 0, again.stdout + again.stderr)
        self.assertIn("This round reused a commit and pushed the branch", again.stdout)
        self.assertEqual(self.published(), unpushed)
        self.assertEqual(self.methods().count("POST"), 1)

    def test_a_round_whose_catch_up_failed_is_pushed_by_the_next_delivery(self):
        # s02b: the catch-up could not read the integration branch after the
        # commit was made; the next delivery pushes it.
        self.change("main.go", "package main // first round\n")
        self.assertEqual(self.leave_merge().returncode, 0)
        marker = self.fail_integration_fetch()
        self.change("main.go", "package main // reviewed again\n")
        failed = self.leave_merge()
        self.assertEqual(failed.returncode, 1, failed.stdout)
        unpushed = self.receipt()["head"]
        self.assertNotEqual(self.published(), unpushed)
        marker.unlink()
        again = self.leave_merge()
        self.assertEqual(again.returncode, 0, again.stdout + again.stderr)
        self.assertIn("pushed the branch", again.stdout)
        self.assertEqual(self.published(), unpushed)

    CHANGED = ("A person changed its branch ticket/TICKET-41, which is at %s now, so this process does nothing more "
               "with that pull request.")
    CHANGED_THEN = ("When a delivery read them at %s, pull request 1 against master for TICKET-41 was open "
                    "(http://service.invalid/pulls/1) and a person had changed its branch ticket/TICKET-41, which was "
                    "at %s. This delivery did not read them again and does nothing more with that pull request.")
    NOT_COMMITTED = ("The workspace still holds changes that are not committed (%s). This process did not put them in "
                     "the pull request.")

    def test_a_person_who_pushed_to_the_branch_keeps_it(self):
        # s02: the branch is theirs now. New work is not pushed over it; the
        # delivery says so, names that work and ends. A later delivery says
        # what was read then, and names the work still not committed.
        self.change("main.go", "package main // left for a person\n")
        self.assertEqual(self.leave_merge().returncode, 0)
        theirs = self.person_pushes("library/run.go", "package library // a person's fix\n")
        self.change("main.go", "package main // more reviewed work\n")
        ended = self.leave_merge()
        self.assertEqual(ended.returncode, 0, ended.stdout + ended.stderr)
        self.assertIn(self.CHANGED % theirs, ended.stdout)
        self.assertIn(self.NOT_COMMITTED % "main.go", ended.stdout)
        receipt = self.receipt()
        self.assertEqual((receipt["changed_by_person"], receipt["branch_head"], receipt["not_committed"]),
                         (True, theirs, ["main.go"]))
        self.change("go.mod", "module example\n")
        again = self.leave_merge()
        self.assertEqual(again.returncode, 0, again.stdout + again.stderr)
        self.assertIn(self.CHANGED_THEN % (receipt["ended_at"], theirs), again.stdout)
        self.assertIn(self.NOT_COMMITTED % "go.mod, main.go", again.stdout)
        self.assertEqual(self.receipt()["not_committed"], ["go.mod", "main.go"])
        self.assertEqual(self.published(), theirs)
        self.assertEqual(self.methods().count("POST"), 1)
        self.assertNotIn("PUT", self.methods())

    def test_a_person_who_rewrote_the_branch_keeps_it(self):
        # s03: the commit on record is no longer on the branch.
        self.change("main.go", "package main // left for a person\n")
        self.assertEqual(self.leave_merge().returncode, 0)
        pushed = self.receipt()["head"]
        theirs = self.person_rewrites()
        ended = self.leave_merge()
        self.assertEqual(ended.returncode, 0, ended.stdout + ended.stderr)
        self.assertIn(self.CHANGED % theirs, ended.stdout)
        self.assertIn("This delivery's commit %s is not on that branch, so it is not in the pull request" % pushed,
                      ended.stdout)
        self.assertEqual(self.receipt()["not_pushed"], pushed)
        self.assertEqual(self.published(), theirs)

    def test_changes_left_uncommitted_are_named_as_the_workspace_holds_them(self):
        # During a pending catch-up the integration branch's own change is
        # staged too. The delivery says what the workspace holds, not who
        # wrote it.
        self.change("main.go", "package main // left for a person\n")
        self.assertEqual(self.leave_merge().returncode, 0)
        self.advance_integration_branch("library/run.go", "package library\n\nfunc Other() {}\n")
        self.advance_integration_branch("main.go", "package main\n\n// theirs\n", message="Codex: a conflicting one")
        self.change("main.go", "package main\n\n// ours\n")
        self.assertEqual(self.leave_merge().returncode, 1)
        theirs = self.person_pushes("notes.md", "a person's note\n")
        self.change("main.go", "package main\n\n// theirs and ours\n")
        ended = self.leave_merge()
        self.assertEqual(ended.returncode, 0, ended.stdout + ended.stderr)
        self.assertIn(self.CHANGED % theirs, ended.stdout)
        self.assertIn(self.NOT_COMMITTED % "library/run.go, main.go", ended.stdout)
        self.assertNotIn("This round's", ended.stdout)

    def test_at_most_twenty_uncommitted_paths_are_named_with_how_many_there_are(self):
        self.change("main.go", "package main // left for a person\n")
        self.assertEqual(self.leave_merge().returncode, 0)
        self.person_pushes("library/run.go", "package library // a person's fix\n")
        names = ["build/out-%02d.txt" % number for number in range(25)]
        for name in names:
            self.change(name, "generated\n")
        ended = self.leave_merge()
        self.assertEqual(ended.returncode, 0, ended.stdout + ended.stderr)
        self.assertIn(self.NOT_COMMITTED % ("25 paths, the first 20 of them " + ", ".join(names[:20])), ended.stdout)
        self.assertNotIn(names[20], ended.stdout)
        receipt = self.receipt()
        self.assertEqual((receipt["not_committed"], receipt["not_committed_count"]), (names[:20], 25))

    def person_pushes_after_the_commit(self, relative, text):
        """A person's push that reaches the pull request's branch after this
        round committed: while the delivery reads the integration branch to
        catch up, before its second look at the ticket branch."""
        other = self.person_clone()
        (other / relative).write_text(text)
        self.git(other, "add", "-A")
        self.git(other, "commit", "-m", "Codex: a person's own commit on the pull request")
        theirs = self.git(other, "rev-parse", "HEAD").stdout.strip()
        self.git(other, "push", "origin", "HEAD:refs/person/pending")
        directory = self.root / "racing-bin"
        directory.mkdir()
        shim = directory / "git"
        shim.write_text('#!/bin/sh\ncase " $* " in *" fetch "*"refs/heads/master"*) "%s" -C "%s" update-ref '
                        'refs/heads/ticket/TICKET-41 %s %s 2>/dev/null;; esac\nexec "%s" "$@"\n'
                        % (shutil.which("git"), self.remote, theirs, self.published(), self.root / "bin" / "git"))
        shim.chmod(0o755)
        self.path = str(directory) + os.pathsep + self.path
        return theirs

    def test_a_persons_push_after_this_round_committed_leaves_the_commit_named_and_unpushed(self):
        # The second look before the push: the person's push came after this
        # round's commit, so the commit is named, and it is not pushed over
        # theirs.
        self.change("main.go", "package main // left for a person\n")
        self.assertEqual(self.leave_merge().returncode, 0)
        pushes = self.pushes()
        theirs = self.person_pushes_after_the_commit("library/run.go", "package library // a person's fix\n")
        self.change("main.go", "package main // more reviewed work\n")
        ended = self.leave_merge()
        self.assertEqual(ended.returncode, 0, ended.stdout + ended.stderr)
        commit = self.git(self.workspace, "rev-parse", "HEAD").stdout.strip()
        self.assertIn(self.CHANGED % theirs, ended.stdout)
        self.assertIn("This delivery's commit %s is not on that branch, so it is not in the pull request; nothing was "
                      "pushed over the person's commits." % commit, ended.stdout)
        receipt = self.receipt()
        self.assertEqual((receipt["changed_by_person"], receipt["not_pushed"], receipt["not_committed"],
                          receipt["not_committed_count"]), (True, commit, [], 0))
        self.assertEqual(self.published(), theirs)
        self.assertEqual(self.pushes(), pushes)

    def test_a_later_delivery_does_not_call_a_reopened_pull_request_closed_now(self):
        # A later delivery reads nothing again: it says when the pull request
        # was found closed, not that it is closed.
        self.change("main.go", "package main // left for a person\n")
        self.assertEqual(self.leave_merge().returncode, 0)
        self.state["pulls"][0]["state"] = "closed"
        self.assertEqual(self.leave_merge().returncode, 0)
        ended_at = self.receipt()["ended_at"]
        self.state["pulls"][0]["state"] = "open"
        calls = list(self.state["requests"])
        later = self.leave_merge()
        self.assertEqual(later.returncode, 0, later.stdout + later.stderr)
        self.assertIn(self.CLOSED_THEN % ended_at + self.ENDED_THEN, later.stdout)
        for now in ("was closed by a person", "is not reopened", "This request ends"):
            self.assertNotIn(now, later.stdout)
        self.assertEqual(self.state["requests"], calls, "the pull request was read again")

    def test_a_later_delivery_names_the_head_it_read_then_and_pushes_nothing_after_the_persons_merge(self):
        # The person pushed again and merged after the delivery ended. A later
        # delivery names the head it read and when, and pushes nothing.
        self.change("main.go", "package main // left for a person\n")
        self.assertEqual(self.leave_merge().returncode, 0)
        first = self.person_pushes("library/run.go", "package library // first push by a person\n")
        self.assertEqual(self.leave_merge().returncode, 0)
        ended_at = self.receipt()["ended_at"]
        self.person_pushes("library/run.go", "package library // second push by a person\n")
        self.person_merges(1)
        pushes, calls = self.pushes(), list(self.state["requests"])
        later = self.leave_merge()
        self.assertEqual(later.returncode, 0, later.stdout + later.stderr)
        self.assertIn(self.CHANGED_THEN % (ended_at, first), later.stdout)
        self.assertNotIn("which is at", later.stdout)
        self.assertEqual((self.pushes(), self.state["requests"]), (pushes, calls))

    def test_a_pull_request_closed_after_an_earlier_round_was_merged_names_that_merge(self):
        # Something of the request was delivered: the ending does not say that
        # nothing was.
        self.change("main.go", "package main // first round\n")
        self.assertEqual(self.leave_merge().returncode, 0)
        merge = self.person_merges(1)
        self.change("main.go", "package main // second round\n")
        self.assertEqual(self.leave_merge().returncode, 0)
        self.state["pulls"][1]["state"] = "closed"
        merged = "An earlier round of this request was merged as commit %s through pull request 1.\n" % merge
        ended = self.leave_merge()
        self.assertEqual(ended.returncode, 0, ended.stdout + ended.stderr)
        self.assertIn("Pull request 2 against master for TICKET-41 was closed by a person without being merged: "
                      "http://service.invalid/pulls/2.\n" + merged + "This request ends with nothing of this round "
                      "delivered.", ended.stdout)
        self.assertEqual([round["merge_sha"] for round in self.receipt()["previous"]], [merge])
        again = self.leave_merge()
        self.assertEqual(again.returncode, 0, again.stdout + again.stderr)
        self.assertIn("This delivery did not read it again.\n" + merged + "This request ended then with nothing of "
                      "this round delivered.", again.stdout)
        for said in (ended.stdout, again.stdout):
            self.assertNotIn("with nothing delivered", said)

    def test_a_merge_before_the_push_leaves_the_earlier_round_its_own_times(self):
        # The earlier round's record keeps when it was committed and pushed;
        # this round's times stay this round's.
        self.change("main.go", "package main // first round\n")
        self.assertEqual(self.leave_merge().returncode, 0)
        path = self.workspace / ".git" / "ticket-engine" / "delivery.json"
        earlier = {"committed_at": "2026-01-01T00:00:01Z", "pushed_at": "2026-01-01T00:00:02Z"}
        path.write_text(json.dumps(dict(json.loads(path.read_text()), **earlier)))
        self.merge_right_after_the_next_read(1)
        self.change("main.go", "package main // reviewed again\n")
        self.assertEqual(self.leave_merge().returncode, 0)
        receipt = self.receipt()
        self.assertEqual({field: receipt["previous"][0][field] for field in earlier}, earlier)
        self.assertNotIn(receipt["committed_at"], earlier.values())
        self.assertNotIn(receipt["pushed_at"], earlier.values())

    def test_a_merge_before_the_push_leaves_the_pushed_commit_a_pull_request_of_its_own(self):
        # s04: the read says open, a person merges, the delivery pushes. The
        # merge does not carry the pushed commit, so that merge is an earlier
        # round and the commit goes in a new pull request.
        self.change("main.go", "package main // first round\n")
        self.assertEqual(self.leave_merge().returncode, 0)
        first = self.receipt()["head"]
        self.merge_right_after_the_next_read(1)
        self.change("main.go", "package main // reviewed again\n")
        raced = self.leave_merge()
        self.assertEqual(raced.returncode, 0, raced.stdout + raced.stderr)
        self.assertIn("Pull request 2 against master is open for TICKET-41", raced.stdout)
        self.assertIn("Earlier merged rounds for this ticket: 1.", raced.stdout)
        receipt = self.receipt()
        self.assertEqual(receipt["pull_request"], 2)
        self.assertEqual(self.published(), receipt["head"])
        self.assertEqual([(round["pull_request"], round["merge_sha"], round["head"]) for round in receipt["previous"]],
                         [(1, self.state["race_merge"], first)])
        self.assertNotIn("PUT", self.methods())

    def assert_merged_by_them(self, result, merge):
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("Pull request 1 against master was merged by someone else as commit %s; this process "
                      "merged nothing." % merge, result.stdout)
        self.assertNotIn("with method merge", result.stdout)
        self.assertNotIn("PUT", self.methods())
        self.assertIs(self.receipt()["merge_left_to_person"], True)

    def switch_after(self, person_merge):
        self.change("main.go", "package main // left for a person\n")
        self.assertEqual(self.leave_merge().returncode, 0)
        theirs = person_merge(1)
        pushes = self.pushes()
        self.assert_merged_by_them(self.deliver(), theirs)
        self.assertEqual(self.pushes(), pushes, "a commit was made and pushed after the person's merge")

    def test_switching_to_merge_after_a_persons_merge_says_someone_else_merged_it(self):
        # s06b: "merged with method" only for a merge this process asked for.
        self.switch_after(self.person_merges)

    def test_switching_to_merge_after_a_persons_squash_commits_and_pushes_nothing(self):
        # s06: no catch-up commit is made and pushed on top of their squash.
        self.switch_after(self.person_squash_merges)

    def switch_while_a_person_merges(self, reads):
        self.change("main.go", "package main // left for a person\n")
        self.assertEqual(self.leave_merge().returncode, 0)
        self.merge_right_after_the_next_read(1, reads)
        self.assert_merged_by_them(self.deliver(), self.state["race_merge"])

    def test_a_persons_merge_after_the_switched_round_was_read_stays_theirs(self):
        # The read before the push says open; the read after it finds their merge.
        self.switch_while_a_person_merges(reads=1)

    def test_a_persons_merge_just_before_this_process_merges_stays_theirs(self):
        # The read after the push says open; the merge request then finds it
        # merged already, so this process asked for no merge.
        self.switch_while_a_person_merges(reads=2)

    def test_a_persons_merge_before_the_push_of_a_switched_round_leaves_new_work_its_own_pull_request(self):
        # s04 after a switch to merging: their merge lacks the new commit, so
        # it is an earlier round, and this process merges a new pull request.
        self.change("main.go", "package main // first round\n")
        self.assertEqual(self.leave_merge().returncode, 0)
        first = self.receipt()["head"]
        self.merge_right_after_the_next_read(1)
        self.change("main.go", "package main // reviewed again\n")
        switched = self.deliver()
        self.assertEqual(switched.returncode, 0, switched.stdout + switched.stderr)
        self.assertIn("Pull request 2 against master was merged with method merge as commit", switched.stdout)
        self.assertIn("Earlier merged rounds for this ticket: 1.", switched.stdout)
        receipt = self.receipt()
        self.assertNotIn("merge_left_to_person", receipt)
        self.assertEqual([(round["pull_request"], round["merge_sha"], round["head"]) for round in receipt["previous"]],
                         [(1, self.state["race_merge"], first)])
        self.assertIn("reviewed again", self.git(self.remote, "show", "refs/heads/master:main.go").stdout)
        self.assertEqual(self.methods().count("PUT"), 1)

    def test_work_after_a_persons_squash_goes_through_the_conflict_to_a_new_pull_request(self):
        # s05: after a squash the ticket branch's own earlier change conflicts
        # with its squashed copy. That is the ordinary conflict: left in the
        # tree for the work stage, and once resolved, a new pull request.
        self.change("main.go", "package main\n\n// first round\n")
        self.assertEqual(self.leave_merge().returncode, 0)
        self.person_squash_merges(1)
        self.change("main.go", "package main\n\n// second round\n")
        refused = self.leave_merge()
        self.assertEqual(refused.returncode, 1, refused.stdout)
        self.assertIn("1 path conflict with this change: main.go", refused.stdout)
        self.assertIn("<<<<<<<", (self.workspace / "main.go").read_text())
        self.change("main.go", "package main\n\n// second round\n")
        done = self.leave_merge()
        self.assertEqual(done.returncode, 0, done.stdout + done.stderr)
        self.assertIn("Pull request 2 against master is open for TICKET-41", done.stdout)
        self.assertEqual(len(self.receipt()["previous"]), 1)
        self.assertNotIn("PUT", self.methods())

    def test_a_persons_merge_reported_without_its_commit_is_not_called_nothing_merged(self):
        # s07
        self.change("main.go", "package main // left for a person\n")
        self.assertEqual(self.leave_merge().returncode, 0)
        self.state["pulls"][0].update(merged=True, state="closed", merge_commit_sha=None)
        waiting = self.leave_merge()
        self.assertEqual(waiting.returncode, 1, waiting.stdout)
        self.assertIn("Pull request 1 was merged by someone else, but the service does not report its merge commit "
                      "yet for TICKET-41.", waiting.stdout)
        self.assertNotIn("Nothing was merged", waiting.stdout)
        self.assertNotIn("Nothing was delivered", waiting.stdout)

    def test_check_mode_says_when_the_merge_is_left_to_a_person(self):
        left = self.deliver("--dry-run", DELIVERY_MERGE_METHOD="none")
        self.assertEqual(left.returncode, 3, left.stdout + left.stderr)
        self.assertIn("A delivery ends at the open pull request and leaves the merge to a person", left.stdout)
        merging = self.deliver("--dry-run")
        self.assertIn("A delivery merges its pull request with method merge.", merging.stdout)

    def test_check_mode_says_whether_a_request_that_changed_nothing_may_end(self):
        off = self.deliver("--dry-run")
        self.assertEqual(off.returncode, 3, off.stdout + off.stderr)
        self.assertIn("A request that changes no file is refused (DELIVERY_ALLOW_UNCHANGED is not 1).", off.stdout)
        on = self.deliver("--dry-run", DELIVERY_ALLOW_UNCHANGED="1")
        self.assertEqual(on.returncode, 3, on.stdout + on.stderr)
        self.assertIn("A request that changes no file ends without a delivery", on.stdout)
        self.assertEqual(self.receipt(), {})

    # Changes that are not UTF-8: files in Shift_JIS, and names Git gives in bytes.

    def shift_jis(self, relative, text):
        """A file written in Shift_JIS, as older Japanese projects keep them."""
        data = text.encode("shift_jis")
        path = self.workspace / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(data)
        return data

    def stored(self, revision):
        """What the delivery target holds, as bytes."""
        return subprocess.run(["git", "-C", str(self.remote), "show", revision], check=True,
                              capture_output=True, env=git_environment()).stdout

    def git_naming_in_bytes(self, real, shown):
        """A Git on PATH that does what Linux Git does for a file whose name is
        not UTF-8, which macOS will not create: the file is on disk as real,
        and every argument and every output carries the bytes shown instead."""
        directory = self.root / "bytes-bin"
        directory.mkdir()
        shim = directory / "git"
        shim.write_text("#!%s\nimport os, subprocess, sys\n"
                        "arguments = [os.fsencode(argument).replace(%r, %r) for argument in sys.argv[1:]]\n"
                        "done = subprocess.run([%r] + arguments, capture_output=True)\n"
                        "sys.stdout.buffer.write(done.stdout.replace(%r, %r))\n"
                        "sys.stderr.buffer.write(done.stderr.replace(%r, %r))\n"
                        "sys.exit(done.returncode)\n"
                        % (sys.executable, shown, real, str(self.root / "bin" / "git"), real, shown, real, shown))
        shim.chmod(0o755)
        self.path = str(directory) + os.pathsep + self.path

    def test_a_change_that_is_not_utf8_is_delivered_byte_for_byte(self):
        changed = self.shift_jis("main.go", "package main // 日本語の説明\n")
        added = self.shift_jis("library/sjis.go", "package library // 表示とソース\n")
        done = self.deliver()
        self.assertEqual(done.returncode, 0, done.stdout + done.stderr)
        self.assertIn("Delivered TICKET-41", done.stdout)
        for branch in ("refs/heads/master", "refs/heads/ticket/TICKET-41"):
            self.assertEqual((self.stored(branch + ":main.go"), self.stored(branch + ":library/sjis.go")),
                             (changed, added))
        self.assertEqual(self.methods().count("POST"), 1)

    def test_forbidden_text_is_still_found_in_a_change_that_is_not_utf8(self):
        self.shift_jis("main.go", "package main // 日本語 internal-project-codename\n")
        refused = self.deliver(DELIVERY_FORBIDDEN_TEXT="Internal-Project-Codename")
        self.assertEqual(refused.returncode, 1, refused.stdout + refused.stderr)
        self.assertIn("contains configured forbidden text (1 entry)", refused.stdout)
        self.shift_jis("main.go", "package main // 日本語 %s\n" % TOKEN)
        refused = self.deliver()
        self.assertEqual(refused.returncode, 1, refused.stdout + refused.stderr)
        self.assertIn("contains the delivery credential", refused.stdout)
        self.assertNotIn(TOKEN, refused.stdout + refused.stderr)
        # Text that is not ASCII cannot be looked for in bytes that are not
        # UTF-8: such a change is refused, saying so, rather than let through.
        self.shift_jis("main.go", "package main // 日本語の説明\n")
        refused = self.deliver(DELIVERY_FORBIDDEN_TEXT="社外秘")
        self.assertEqual(refused.returncode, 1, refused.stdout + refused.stderr)
        self.assertIn("Refused: the staged change carries text that is not UTF-8, in which configured forbidden text "
                      "that is not ASCII cannot be looked for", refused.stdout)
        self.assertEqual(self.remote_branches(), ["refs/heads/master"])
        self.assertEqual(self.methods(), [])
        # A change that is UTF-8 throughout is looked through as before.
        self.change("main.go", "package main // 日本語の説明\n")
        done = self.deliver(DELIVERY_FORBIDDEN_TEXT="社外秘")
        self.assertEqual(done.returncode, 0, done.stdout + done.stderr)

    def test_an_entry_is_told_to_be_ascii_as_it_is_looked_for(self):
        # An entry is looked for without the blanks around it, so a full-width
        # space after an ASCII entry does not make it one that cannot be looked
        # for in a change that is not UTF-8.
        self.shift_jis("main.go", "package main // 日本語の説明\n")
        done = self.deliver(DELIVERY_FORBIDDEN_TEXT="internal-project-codename\N{IDEOGRAPHIC SPACE}")
        self.assertEqual(done.returncode, 0, done.stdout + done.stderr)
        self.assertIn("Delivered TICKET-41", done.stdout)

    @staticmethod
    def image():
        """A 64x64 PNG: Git takes it as binary, and most of its bytes are not UTF-8.
        Its pixels are kept in stored zlib blocks, as a PNG may keep them."""
        def chunk(kind, data):
            return struct.pack(">I", len(data)) + kind + data + struct.pack(">I", zlib.crc32(kind + data))
        rows = b"".join(b"\0" + bytes((x * 7 + y * 13) % 256 for x in range(64 * 3)) for y in range(64))
        stored = b"\x78\x01" + b"".join(
            bytes([start + 65535 >= len(rows)]) + struct.pack("<HH", len(block), 0xFFFF - len(block)) + block
            for start in range(0, len(rows), 65535) for block in [rows[start:start + 65535]])
        return (b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", 64, 64, 8, 2, 0, 0, 0))
                + chunk(b"IDAT", stored + struct.pack(">I", zlib.adler32(rows))) + chunk(b"IEND", b""))

    def test_an_image_is_delivered_while_an_entry_that_is_not_ascii_is_configured(self):
        # The refusal of bytes that are not UTF-8 is for text, in which such an
        # entry could be written another way; a file Git takes as binary is
        # looked through for it as written in UTF-8 instead.
        (self.workspace / "library").mkdir(exist_ok=True)
        (self.workspace / "library" / "logo.png").write_bytes(self.image())
        done = self.deliver(DELIVERY_FORBIDDEN_TEXT="社外秘")
        self.assertEqual(done.returncode, 0, done.stdout + done.stderr)
        self.assertEqual(self.stored("refs/heads/master:library/logo.png"), self.image())

    def test_an_entry_that_is_not_ascii_is_found_in_an_image_where_written_in_utf8(self):
        self.refused_as_written("library/logo.png", self.image() + "社外秘".encode(), "forbidden text (1 entry)",
                                DELIVERY_FORBIDDEN_TEXT="社外秘")

    def test_text_that_is_not_utf8_beside_an_image_is_still_refused(self):
        (self.workspace / "library").mkdir(exist_ok=True)
        (self.workspace / "library" / "logo.png").write_bytes(self.image())
        self.shift_jis("library/legacy.txt", "表示とソース\n")
        refused = self.deliver(DELIVERY_FORBIDDEN_TEXT="社外秘")
        self.assertEqual(refused.returncode, 1, refused.stdout + refused.stderr)
        self.assertIn("carries text that is not UTF-8", refused.stdout)
        self.assertEqual(self.methods(), [])

    def test_a_name_that_is_not_utf8_is_delivered_and_printed_with_replacement_characters(self):
        self.change("library/plain.go", "package library // a name Git gives in bytes\n")
        self.git_naming_in_bytes(b"library/plain.go", b"library/\x82\xa0.go")
        checked = self.deliver("--dry-run")
        self.assertEqual(checked.returncode, 3, checked.stdout + checked.stderr)
        self.assertIn("Changed paths inside the operator's grant: library/��.go.", checked.stdout)
        done = self.deliver()
        self.assertEqual(done.returncode, 0, done.stdout + done.stderr)
        self.assertIn("a name Git gives in bytes", self.git(self.remote, "show", "refs/heads/master:library/plain.go").stdout)

    def test_a_name_that_is_not_utf8_is_held_to_the_grant(self):
        self.change("notes.md", "edited outside the grant\n")
        self.git_naming_in_bytes(b"notes.md", b"notes-\x82\xa0.md")
        refused = self.deliver()
        self.assertEqual(refused.returncode, 1, refused.stdout + refused.stderr)
        self.assertIn("Changes outside the operator's allowed paths were not delivered: notes-��.md",
                      refused.stdout)
        self.assertIn("notes-��.md", refused.stderr)
        self.assertEqual(self.methods(), [])

    def test_forbidden_text_is_found_in_a_name_that_is_not_utf8(self):
        self.change("library/plain.go", "package library\n")
        self.git_naming_in_bytes(b"library/plain.go", b"library/\x82\xa0internal-project-codename.go")
        refused = self.deliver(DELIVERY_FORBIDDEN_TEXT="internal-project-codename")
        self.assertEqual(refused.returncode, 1, refused.stdout + refused.stderr)
        self.assertIn("contains configured forbidden text (1 entry)", refused.stdout)
        refused = self.deliver(DELIVERY_FORBIDDEN_TEXT="社外秘")
        self.assertEqual(refused.returncode, 1, refused.stdout + refused.stderr)
        self.assertIn("carries text that is not UTF-8", refused.stdout)
        self.assertEqual(self.methods(), [])

    def test_a_name_that_is_not_utf8_is_recorded_with_replacement_characters(self):
        self.change("main.go", "package main // left for a person\n")
        self.assertEqual(self.leave_merge().returncode, 0)
        self.person_pushes("library/run.go", "package library // a person's fix\n")
        self.change("library/plain.go", "package library\n")
        self.git_naming_in_bytes(b"library/plain.go", b"library/\x82\xa0.go")
        ended = self.leave_merge()
        self.assertEqual(ended.returncode, 0, ended.stdout + ended.stderr)
        self.assertIn(self.NOT_COMMITTED % "library/��.go", ended.stdout)
        self.assertEqual(self.receipt()["not_committed"], ["library/��.go"])

    def test_what_git_says_in_bytes_that_are_not_utf8_is_printed_with_replacement_characters(self):
        hook = self.remote / "hooks" / "pre-receive"
        hook.write_text("#!/bin/sh\nprintf 'refused: \\223\\372\\214\\352\\n' >&2\nexit 1\n")
        hook.chmod(0o755)
        self.change("main.go", "package main // delivered\n")
        refused = self.deliver()
        self.assertEqual(refused.returncode, 1, refused.stdout + refused.stderr)
        self.assertIn("refused: ����", refused.stdout)
        self.assertIn("refused: ����", refused.stderr)


if __name__ == "__main__":
    unittest.main()
