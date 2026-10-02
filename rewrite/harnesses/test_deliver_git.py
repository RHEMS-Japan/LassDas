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
    with neither is exactly where these programs have to work."""
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
        self.assertIn(self.CLOSED_THEN % receipt["ended_at"] + self.ENDS, again.stdout)
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
    NOT_COMMITTED = ("This round's changes to %s were not committed, so this process did not put them in the pull "
                     "request; they are left in the workspace.")

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
        # round's commit, so the commit is named instead of the round's paths,
        # and it is not pushed over theirs.
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
        self.assertEqual((receipt["changed_by_person"], receipt["not_pushed"], receipt["not_committed"]),
                         (True, commit, []))
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
        self.assertIn(self.CLOSED_THEN % ended_at + self.ENDS, later.stdout)
        self.assertNotIn("was closed by a person", later.stdout)
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
        merged = ("An earlier round of this request was merged as commit %s through pull request 1.\nThis request "
                  "ends with nothing of this round delivered." % merge)
        ended = self.leave_merge()
        self.assertEqual(ended.returncode, 0, ended.stdout + ended.stderr)
        self.assertIn("Pull request 2 against master for TICKET-41 was closed by a person without being merged: "
                      "http://service.invalid/pulls/2.\n" + merged, ended.stdout)
        self.assertEqual([round["merge_sha"] for round in self.receipt()["previous"]], [merge])
        again = self.leave_merge()
        self.assertEqual(again.returncode, 0, again.stdout + again.stderr)
        self.assertIn("This delivery did not read it again.\n" + merged, again.stdout)
        for said in (ended.stdout, again.stdout):
            self.assertNotIn("ends with nothing delivered", said)

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


if __name__ == "__main__":
    unittest.main()
