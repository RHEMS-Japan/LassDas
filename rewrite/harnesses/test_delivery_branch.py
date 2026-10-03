"""Branch-only delivery against actual local Git, with every REST call recorded."""
import json
from pathlib import Path
import shlex
import shutil
import subprocess
import sys
import unittest

import test_deliver_git as delivery_fixture

BRANCH = "refs/heads/ticket/TICKET-41"
VERIFIER = Path(__file__).with_name("verify_merged.py").resolve()


class BranchDeliveryTests(unittest.TestCase):
    def setUp(self):
        self.f = delivery_fixture.DeliveryTests()
        self.f.setUp()
        self.addCleanup(self.f.doCleanups)
        self.base = self.ref("refs/heads/master")
        self.f.change("main.go", "package main // published work\n")

    def ref(self, name=BRANCH):
        return self.f.git(self.f.remote, "rev-parse", name).stdout.strip()

    def deliver(self, *arguments, **settings):
        return self.f.deliver("--branch-only", *arguments, **settings)

    def publish(self):
        result = self.deliver()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(self.f.state["requests"], [])
        return self.f.receipt()

    def save(self, receipt):
        path = self.f.workspace / ".git/ticket-engine/delivery.json"
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(receipt))

    def pushes(self):
        return [line for line in self.f.git_log.read_text().splitlines() if " push " in line]

    def verify(self, script=None, **settings):
        script = script or "print('configured verification ran')"
        command = shlex.join([sys.executable, "-c", script])
        return subprocess.run([sys.executable, "-B", str(VERIFIER)], input="unused prompt",
                              env=self.f.environment(VERIFY_COMMANDS=command, **settings),
                              cwd=self.f.workspace, capture_output=True, text=True, timeout=30)

    def other_commit(self, parent):
        return self.f.git(self.f.remote, "commit-tree", parent + "^{tree}", "-p", parent,
                          "-m", "Codex: another writer").stdout.strip()

    def shim(self, body):
        path = self.f.root / "bin/git"
        real = shlex.quote(shutil.which("git"))
        path.write_text("#!/bin/sh\n" + body.replace("REAL_GIT", real) + '\nexec ' + real + ' "$@"\n')
        path.chmod(0o755)

    def test_publish_reads_back_the_branch_without_changing_base_or_calling_rest(self):
        receipt = self.publish()
        self.assertIs(receipt["branch_only"], True)
        self.assertEqual(receipt["head"], self.ref())
        self.assertEqual(receipt["published_head"], self.ref())
        self.assertTrue(receipt["branch_confirmed_at"])
        self.assertEqual(self.ref("refs/heads/master"), self.base)
        self.assertFalse(any(receipt.get(key) for key in ("pull_request", "merge_sha", "merge_left_to_person")))
        self.assertNotIn("publishing_head", receipt)
        self.assertEqual(self.f.state["pulls"], [])
        result = self.deliver()
        self.assertIn("No pull request was opened", result.stdout)
        self.assertIn("Environment deployment was not checked", result.stdout)

    def test_rerun_uses_the_same_commit_and_new_work_continues_the_same_branch(self):
        first = self.publish()
        pushes = self.pushes()
        self.publish()
        self.assertEqual(self.pushes(), pushes)
        self.assertEqual(self.f.receipt()["head"], first["head"])
        self.f.change("main.go", "package main // next reviewed work\n")
        second = self.publish()
        self.assertNotEqual(second["head"], first["head"])
        self.f.git(self.f.remote, "merge-base", "--is-ancestor", first["head"], second["head"])
        self.assertEqual(len(self.pushes()), 2)
        self.assertEqual(self.ref("refs/heads/master"), self.base)

    def test_dry_run_and_incompatible_operator_settings_cannot_publish(self):
        for arguments, settings in [(("--dry-run",), {}), ((), {"DELIVERY_MERGE_METHOD": "none"}),
                                    ((), {"DELIVERY_PR_BODY_FILE": "notes.md"}), (("--branch-only",), {})]:
            with self.subTest(arguments=arguments, settings=settings):
                result = self.deliver(*arguments, **settings)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(self.f.remote_branches(), ["refs/heads/master"])
                self.assertEqual(self.f.state["requests"], [])
                self.assertEqual(self.f.receipt(), {})
                self.assertEqual(self.f.git(self.f.workspace, "rev-parse", "HEAD").stdout.strip(), self.base)

    def test_unchanged_permission_never_invents_a_branch_publication(self):
        self.f.change("main.go", "package main\n")
        refused = self.deliver()
        self.assertNotEqual(refused.returncode, 0)
        allowed = self.deliver(DELIVERY_ALLOW_UNCHANGED="1")
        self.assertEqual(allowed.returncode, 0, allowed.stdout + allowed.stderr)
        receipt = self.f.receipt()
        self.assertTrue(receipt["unchanged"])
        self.assertNotIn("branch_only", receipt)
        self.assertEqual(self.f.remote_branches(), ["refs/heads/master"])
        self.assertEqual(self.f.state["requests"], [])
        self.assertIn("Nothing was delivered", allowed.stdout)

    def test_modes_and_request_identity_cannot_reuse_each_others_receipts(self):
        original = self.publish()
        prior_pushes = self.pushes()
        for changes in ({"branch_only": False, "pull_request": 1}, {"repository": "owner/elsewhere"},
                        {"base_branch": "elsewhere"}, {"branch": "ticket/elsewhere"}, {"issue": "TICKET-42"},
                        {"head": "broken"}, {"publishing_head": self.base}, {"unchanged": True},
                        {"changed_by_person": True}):
            with self.subTest(changes=changes):
                self.save(dict(original, **changes))
                result = self.deliver()
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assertEqual(self.pushes(), prior_pushes)
                self.assertEqual(self.f.state["requests"], [])
        self.save(original)
        other_mode = self.f.deliver(DELIVERY_MERGE_METHOD="none")
        self.assertNotEqual(other_mode.returncode, 0)
        self.assertEqual(self.f.state["requests"], [])
        self.assertEqual(self.pushes(), prior_pushes)

    def test_foreign_branch_cannot_be_adopted_on_first_publication(self):
        self.f.git(self.f.remote, "update-ref", BRANCH, self.base)
        result = self.deliver()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("not overwritten or recreated", result.stdout)
        self.assertEqual(self.ref(), self.base)
        self.assertEqual(self.f.receipt(), {})

    def test_human_push_rewrite_and_deletion_are_not_reversed(self):
        receipt = self.publish()
        pushed = receipt["head"]
        for altered in (self.other_commit(pushed), self.base, None):
            with self.subTest(altered=altered):
                if altered:
                    self.f.git(self.f.remote, "update-ref", BRANCH, altered)
                else:
                    self.f.git(self.f.remote, "update-ref", "-d", BRANCH)
                prior = self.pushes()
                result = self.deliver()
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(self.pushes(), prior)
                self.assertNotIn("Published branch", result.stdout)
                if altered:
                    self.assertEqual(self.ref(), altered)
                else:
                    self.assertNotIn(BRANCH, self.f.remote_branches())

    def test_a_concurrent_writer_between_read_and_push_is_preserved(self):
        receipt = self.publish()
        self.f.change("main.go", "package main // next attempt\n")
        other = self.other_commit(receipt["head"])
        marker = self.f.root / "raced"
        self.shim('case " $* " in *" push "*)\n'
                  '  if [ ! -e %s ]; then\n'
                  '    REAL_GIT -C %s update-ref %s %s\n'
                  '    touch %s\n  fi;;\nesac'
                  % (shlex.quote(str(marker)), shlex.quote(str(self.f.remote)), BRANCH, other, shlex.quote(str(marker))))
        result = self.deliver(DELIVERY_RETRY_ATTEMPTS="1")
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertTrue(marker.exists())
        self.assertEqual(self.ref(), other)
        self.assertEqual(self.f.receipt()["published_head"], receipt["head"])
        self.assertIn("publishing_head", self.f.receipt())
        self.assertEqual(self.f.state["requests"], [])

    def test_an_uncertain_push_is_resolved_by_remote_readback(self):
        self.shim('case " $* " in *" push "*)\n  REAL_GIT "$@" || exit $?\n'
                  '  echo "response lost after saving branch" >&2\n  exit 1;;\nesac')
        result = self.deliver(DELIVERY_RETRY_ATTEMPTS="1")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(self.f.receipt()["published_head"], self.ref())
        self.assertNotIn("publishing_head", self.f.receipt())
        self.assertEqual(self.f.state["requests"], [])

    def test_saved_push_intent_recovers_after_local_confirmation_was_lost(self):
        receipt = self.publish()
        intent = dict(receipt, publishing_head=receipt["head"])
        intent.pop("published_head")
        intent.pop("branch_confirmed_at")
        self.save(intent)
        pushes = self.pushes()
        self.publish()
        self.assertEqual(self.pushes(), pushes)
        self.assertEqual(self.f.receipt()["published_head"], receipt["head"])

    def test_an_unconfirmed_push_does_not_report_success_or_skip_its_retry(self):
        self.shim('case " $* " in *" push "*) echo "push refused by fixture" >&2; exit 1;;\nesac')
        result = self.deliver(DELIVERY_RETRY_ATTEMPTS="1")
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("Published branch", result.stdout)
        self.assertNotIn("published_head", self.f.receipt())
        self.assertIn("publishing_head", self.f.receipt())
        self.assertEqual(self.f.remote_branches(), ["refs/heads/master"])
        saved = self.f.receipt()["head"]
        self.shim("")
        self.publish()
        self.assertEqual(self.ref(), saved)

    def test_confirmation_read_failure_does_not_invent_success_and_later_run_recovers(self):
        marker = self.f.root / "pushed-once"
        self.shim('case " $* " in\n'
                  ' *" push "*) REAL_GIT "$@" || exit $?; touch %s; exit 0;;\n'
                  ' *" ls-remote "*) if [ -e %s ]; then echo "read refused" >&2; exit 1; fi;;\nesac'
                  % (shlex.quote(str(marker)), shlex.quote(str(marker))))
        failed = self.deliver(DELIVERY_RETRY_ATTEMPTS="1")
        self.assertNotEqual(failed.returncode, 0)
        self.assertNotIn("Published branch", failed.stdout)
        self.assertNotIn("published_head", self.f.receipt())
        published = self.ref()
        self.assertEqual(self.f.receipt()["publishing_head"], published)
        self.shim('case " $* " in *" push "*) echo "unexpected duplicate push" >&2; exit 1;;\nesac')
        self.publish()
        self.assertEqual(self.f.receipt()["published_head"], published)

    def test_failed_push_then_new_work_and_failed_fetch_can_still_recover(self):
        self.shim('case " $* " in *" push "*) echo "push refused" >&2; exit 1;;\nesac')
        first = self.deliver(DELIVERY_RETRY_ATTEMPTS="1")
        self.assertNotEqual(first.returncode, 0)
        old = self.f.receipt()["publishing_head"]
        self.f.change("main.go", "package main // revised after failed publication\n")
        self.shim('case " $* " in *" fetch "*) echo "temporary read failure" >&2; exit 1;;\nesac')
        second = self.deliver(DELIVERY_RETRY_ATTEMPTS="1")
        self.assertNotEqual(second.returncode, 0)
        saved = self.f.receipt()["head"]
        self.assertNotEqual(saved, old)
        self.assertNotIn("publishing_head", self.f.receipt())
        self.shim("")
        self.publish()
        self.assertEqual(self.ref(), saved)
        self.assertEqual(self.f.state["requests"], [])

    def test_local_history_rewrite_cannot_replace_the_published_branch(self):
        receipt = self.publish()
        self.f.git(self.f.workspace, "checkout", "--detach", self.base)
        self.f.change("main.go", "package main // rewritten attempt\n")
        result = self.deliver()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("history was not rewritten", result.stdout)
        self.assertEqual(self.ref(), receipt["published_head"])

    def test_catch_up_keeps_base_unchanged_and_conflicts_never_reach_the_remote_branch(self):
        other = self.f.root / "other-checkout"
        self.f.git(self.f.root, "clone", str(self.f.remote), str(other))
        (other / "library/run.go").write_text("package library // independent base change\n")
        self.f.git(other, "add", "-A")
        self.f.git(other, "commit", "-m", "Codex: independent base work")
        self.f.git(other, "push", str(self.f.remote), "HEAD:refs/heads/master")
        updated = self.ref("refs/heads/master")
        receipt = self.publish()
        self.assertEqual(self.ref("refs/heads/master"), updated)
        self.f.git(self.f.remote, "merge-base", "--is-ancestor", updated, receipt["head"])
        (other / "main.go").write_text("package main // conflicting base change\n")
        self.f.git(other, "add", "-A")
        self.f.git(other, "commit", "-m", "Codex: competing base work")
        self.f.git(other, "push", str(self.f.remote), "HEAD:refs/heads/master")
        self.f.change("main.go", "package main // another local change\n")
        conflict = self.deliver()
        self.assertNotEqual(conflict.returncode, 0)
        self.assertIn("conflict", conflict.stdout)
        self.assertEqual(self.ref(), receipt["head"])
        self.assertIn("<<<<<<<", (self.f.workspace / "main.go").read_text())
        self.assertEqual(self.f.state["requests"], [])

    def test_existing_scope_and_forbidden_text_checks_apply_before_any_push(self):
        for path, content, settings in [("notes.md", "outside grant", {}),
                                        ("main.go", "package main // forbidden-marker\n",
                                         {"DELIVERY_FORBIDDEN_TEXT": "forbidden-marker"}),
                                        ("main.go", "package main // " + delivery_fixture.TOKEN + "\n", {})]:
            with self.subTest(path=path):
                self.f.change(path, content)
                result = self.deliver(**settings)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(self.f.remote_branches(), ["refs/heads/master"])
                self.assertEqual(self.f.state["requests"], [])
                self.assertNotIn(delivery_fixture.TOKEN, result.stdout + result.stderr)
                self.f.change("notes.md", "original\n")

    def test_verifier_uses_the_published_commit_not_the_modified_workspace(self):
        receipt = self.publish()
        self.f.change("main.go", "unreviewed later edit\n")
        result = self.verify("from pathlib import Path; import os; "
                             "assert Path('main.go').read_text() == 'package main // published work\\n'; "
                             "assert 'GITHUB_TOKEN' not in os.environ; print('exact source checked')")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("exact source checked", result.stdout)
        self.assertIn("No pull request or merge is recorded", result.stdout)
        self.assertIn(receipt["head"], result.stdout)
        self.assertEqual(self.f.state["requests"], [])

    def test_verifier_refuses_moved_deleted_mismatched_and_incomplete_records(self):
        receipt = self.publish()
        for changes in ({"repository": "owner/another"}, {"base_branch": "other"}, {"branch_only": "true"},
                        {"published_head": "bad"}, {"branch_confirmed_at": ""}, {"head": self.base},
                        {"publishing_head": receipt["head"]}, {"pull_request": 1}, {"changed_by_person": True}):
            with self.subTest(changes=changes):
                self.save(dict(receipt, **changes))
                result = self.verify()
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assertNotIn("configured verification ran", result.stdout)
        self.save(receipt)
        newer = self.other_commit(receipt["head"])
        self.f.git(self.f.remote, "update-ref", BRANCH, newer)
        moved = self.verify()
        self.assertNotEqual(moved.returncode, 0)
        self.assertNotIn("configured verification ran", moved.stdout)
        self.f.git(self.f.remote, "update-ref", "-d", BRANCH)
        deleted = self.verify(DELIVERY_RETRY_ATTEMPTS="1")
        self.assertNotEqual(deleted.returncode, 0)
        self.assertNotIn("configured verification ran", deleted.stdout)

    def test_verifier_does_not_check_another_requests_source(self):
        receipt = self.publish()
        self.f.git(self.f.remote, "update-ref", "refs/heads/ticket/TICKET-42", self.base)
        for changed in (dict(issue="TICKET-42", branch="ticket/TICKET-42"), dict(branch="ticket/TICKET-42")):
            with self.subTest(changed=changed):
                self.save(dict(receipt, head=self.base, published_head=self.base, **changed))
                result = self.verify()
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assertIn("different request", result.stderr)
                self.assertNotIn("configured verification ran", result.stdout)
        self.save(receipt)
        missing = self.verify(TASK_ISSUE="")
        self.assertNotEqual(missing.returncode, 0)
        self.assertNotIn("configured verification ran", missing.stdout)

    def test_failing_verification_is_not_a_verified_delivery(self):
        self.publish()
        result = self.verify("raise SystemExit(7)")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Exit status: 7", result.stdout)
        self.assertIn("1 of 1 configured verification commands failed", result.stdout)
