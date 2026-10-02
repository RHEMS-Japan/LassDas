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
IDENTITY = ("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid")


def git_environment():
    """No personal configuration, and no guessed identity either: a runtime
    with neither is exactly where these programs have to work. The delivery
    drops settings handed to Git through the environment, so the workspace's
    own configuration forbids the guess for its Git (see setUp)."""
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
    # a delivery. It drops these settings now, as the review does.

    def test_a_count_of_settings_in_the_environment_does_not_hide_a_change(self):
        self.delivered_although_settings_hide_it({"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "core.excludesFile",
                                                  "GIT_CONFIG_VALUE_0": str(self.hide_the_new_file())})

    def test_settings_passed_as_by_git_itself_do_not_hide_a_change(self):
        self.delivered_although_settings_hide_it(
            {"GIT_CONFIG_PARAMETERS": "'core.excludesFile'='%s'" % self.hide_the_new_file()})

    def test_check_mode_says_whether_a_request_that_changed_nothing_may_end(self):
        off = self.deliver("--dry-run")
        self.assertEqual(off.returncode, 3, off.stdout + off.stderr)
        self.assertIn("A request that changes no file is refused (DELIVERY_ALLOW_UNCHANGED is not 1).", off.stdout)
        on = self.deliver("--dry-run", DELIVERY_ALLOW_UNCHANGED="1")
        self.assertEqual(on.returncode, 3, on.stdout + on.stderr)
        self.assertIn("A request that changes no file ends without a delivery", on.stdout)
        self.assertEqual(self.receipt(), {})


if __name__ == "__main__":
    unittest.main()
