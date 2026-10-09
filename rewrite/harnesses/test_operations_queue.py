"""Exercise queue helpers through a fake kubectl, never a real cluster."""
import hashlib
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest


OPERATIONS = Path(__file__).resolve().parents[2] / "deploy/ticket-engine/operations"
FAKE = r'''
import json, os, subprocess, sys, tarfile
from pathlib import Path
args = sys.argv[1:]
Path(os.environ["FAKE_CALLS"]).write_text(json.dumps(args))
assert args[:7] == ["--context", "fixture-context", "--namespace", "fixture-namespace", "exec", "-i", "fixture-pod"]
assert args[7:13] == ["-c", "fixture-container", "--", "python3", "-B", "-"]
source = sys.stdin.buffer.read()
mode = os.environ.get("FAKE_MODE", "remote")
if mode == "failure":
    with tarfile.open(fileobj=sys.stdout.buffer, mode="w|") as archive:
        item = tarfile.TarInfo("jobs")
        item.type = tarfile.DIRTYPE
        archive.addfile(item)
    print("private fixture diagnostic must not be relayed", file=sys.stderr)
    sys.exit(23)
if mode == "archive":
    sys.stdout.buffer.write(Path(os.environ["FAKE_ARCHIVE"]).read_bytes())
    sys.exit(0)
if mode == "reply":
    print(os.environ["FAKE_REPLY"])
    sys.exit(0)
sys.exit(subprocess.run([sys.executable, "-B", "-", *args[13:]], input=source).returncode)
'''


class OperationsQueueTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="queue-operations-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name).resolve()
        self.queue = self.root / "queue with spaces"
        (self.queue / "jobs").mkdir(parents=True)
        self.output = self.root / "new copy"
        self.fake = self.root / "fake-kubectl"
        self.fake.write_text("#!" + sys.executable + "\n" + FAKE)
        self.fake.chmod(0o700)
        self.calls = self.root / "calls.json"
        self.env = dict(os.environ, FAKE_CALLS=str(self.calls))

    def run_helper(self, action, extra=(), **environment):
        # Always pass this exact executable. A missing fixture is an error,
        # never permission to find a real kubectl on the developer's PATH.
        command = ["/bin/sh", str(OPERATIONS / ("copy-queue.sh" if action == "copy" else "idle-check.sh")),
                   "--kubectl", str(self.fake), "--context", "fixture-context",
                   "--namespace", "fixture-namespace", "--pod", "fixture-pod",
                   "--container", "fixture-container", "--queue", str(self.queue)]
        if action == "copy":
            command += ["--output", str(self.output)]
        command += list(extra)
        return subprocess.run(command, env=dict(self.env, **environment), capture_output=True,
                              text=True, timeout=30)

    def record(self, relative, value):
        path = self.queue / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(value))
        return path

    def job(self, number=7, **state):
        self.record("jobs/%d/issue.json" % number, {"id": number})
        return self.record("jobs/%d/run/history.json" % number,
                           dict(request="fixture request", history=[], done=True, **state))

    def state(self, **changes):
        return dict(dict(request="fixture request", history=[], done=True), **changes)

    def test_copy_preserves_records_times_and_source_but_excludes_working_data(self):
        history = self.job()
        os.utime(history, (1234, 1234))
        files = ["identity.json", "notice-kinds.json", "jobs/7/issue.json", "jobs/7/stop-request.json",
                 "jobs/7/stop-report/history.json", "jobs/7/notices.json", "jobs/7/question.json",
                 "jobs/7/answer-9.json"]
        for name in files:
            self.record(name, {"fixture": name})
        omitted = ["jobs/7/workspace/source.txt", "jobs/7/homes/0-0/logs/log.txt",
                   "jobs/7/live/active.json", "jobs/7/.source-pending/source.txt", "engine.log", "runner.lock"]
        for name in omitted:
            self.record(name, {"private": "not in the copy"})
        before = {str(path.relative_to(self.queue)): path.read_bytes()
                  for path in self.queue.rglob("*") if path.is_file()}
        result = self.run_helper("copy")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("not an atomic snapshot", result.stdout)
        for name, content in before.items():
            self.assertEqual((self.queue / name).read_bytes(), content)
            if name in omitted:
                self.assertFalse((self.output / name).exists(), name)
            else:
                self.assertEqual((self.output / name).read_bytes(), content)
                self.assertEqual((self.output / name).stat().st_mode & 0o777, 0o600)
        self.assertEqual((self.output / "jobs/7/run/history.json").stat().st_mtime, 1234)
        self.assertEqual(self.output.stat().st_mode & 0o777, 0o700)
        self.assertEqual(json.loads(self.calls.read_text())[-2:], ["--remote-copy", str(self.queue)])
        self.assertNotIn("fixture request", result.stdout + result.stderr)

    def test_copy_leaves_out_a_cut_refresh_as_it_does_a_cut_preparation(self):
        # A launch killed while preparing or refreshing a checkout leaves a
        # whole checkout, links included, in private staging beside the
        # workspace. Neither is a queue record.
        self.job()
        for staging in (".source-cut", ".refresh-cut"):
            self.record("jobs/7/%s/repository/.git/config" % staging, {"private": "checkout"})
            (self.queue / "jobs/7" / staging / "repository/linked").symlink_to(".git/config")
        result = self.run_helper("copy")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(sorted(path.name for path in (self.output / "jobs/7").iterdir()), ["issue.json", "run"])
        self.assertTrue((self.output / "jobs/7/run/history.json").is_file())

    def test_copy_keeps_history_larger_than_the_old_size_cutoff(self):
        path = self.job()
        with path.open("wb") as output:
            output.write(b'{"request":"fixture request","done":true,"history":[],"detail":"')
            for _ in range(65):
                output.write(b"x" * (1024 * 1024))
            output.write(b'"}')
        result = self.run_helper("copy")
        self.assertEqual(result.returncode, 0, result.stderr)
        with path.open("rb") as original, (self.output / "jobs/7/run/history.json").open("rb") as copied:
            self.assertEqual(hashlib.file_digest(original, "sha256").digest(),
                             hashlib.file_digest(copied, "sha256").digest())

    def test_existing_output_and_linked_parents_are_rejected_before_remote_access(self):
        self.output.mkdir()
        (self.output / "keep").write_text("existing work")
        result = self.run_helper("copy")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual((self.output / "keep").read_text(), "existing work")
        self.assertFalse(self.calls.exists())
        self.output = self.root / "linked-output"
        self.output.symlink_to(self.root / "new copy", target_is_directory=True)
        self.assertNotEqual(self.run_helper("copy").returncode, 0)
        self.assertFalse(self.calls.exists())
        self.output = self.root / "linked-output" / "child"
        self.assertNotEqual(self.run_helper("copy").returncode, 0)
        self.assertFalse((self.root / "new copy" / "child").exists())

    def test_source_links_and_special_files_are_not_followed(self):
        self.job()
        outside = self.root / "outside"
        outside.write_text("must stay private")
        unsafe = self.queue / "unsafe"
        for kind in ("symlink", "hardlink", "fifo"):
            with self.subTest(kind=kind):
                if kind == "symlink":
                    unsafe.symlink_to(outside)
                elif kind == "hardlink":
                    os.link(outside, unsafe)
                else:
                    os.mkfifo(unsafe)
                result = self.run_helper("copy")
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(self.output.exists())
                self.assertNotIn("must stay private", result.stdout + result.stderr)
                unsafe.unlink()
        self.queue = self.root / "linked-source"
        self.queue.symlink_to(self.root / "queue with spaces", target_is_directory=True)
        self.assertNotEqual(self.run_helper("copy").returncode, 0)

    def test_remote_failure_and_missing_fake_never_produce_a_copy_or_idle_success(self):
        self.job()
        for action in ("copy", "idle"):
            result = self.run_helper(action, FAKE_MODE="failure")
            self.assertNotEqual(result.returncode, 0)
            self.assertNotIn("private fixture diagnostic", result.stdout + result.stderr)
            self.assertFalse(self.output.exists())
        self.fake.unlink()
        self.assertNotEqual(self.run_helper("copy").returncode, 0)
        self.assertFalse(self.output.exists())

    def test_blank_or_option_shaped_targets_never_use_an_implicit_cluster_scope(self):
        for option in ("context", "namespace", "pod", "container"):
            for value in ("", " ", "--all", "line\nbreak"):
                with self.subTest(option=option, value=value):
                    result = self.run_helper("idle", extra=("--" + option + "=" + value,))
                    self.assertEqual(result.returncode, 2, result.stderr)
                    self.assertFalse(self.calls.exists())

    def archive(self, entries):
        path = self.root / "remote.tar"
        with tarfile.open(path, "w") as archive:
            root = tarfile.TarInfo("jobs")
            root.type = tarfile.DIRTYPE
            archive.addfile(root)
            for name, kind in entries:
                item = tarfile.TarInfo(name)
                if kind == "link":
                    item.type, item.linkname = tarfile.SYMTYPE, "../outside"
                elif kind == "hardlink":
                    item.type, item.linkname = tarfile.LNKTYPE, "outside"
                elif kind == "fifo":
                    item.type = tarfile.FIFOTYPE
                else:
                    item.size = 4
                archive.addfile(item, io.BytesIO(b"data") if item.isfile() else None)
        return str(path)

    def test_unsafe_remote_archives_cannot_write_outside_or_overwrite(self):
        outside = self.root / "outside"
        outside.write_text("existing outside data")
        cases = [[("../outside", "file")], [(str(outside), "file")], [("jobs/link", "link")],
                 [("jobs/link", "hardlink")], [("jobs/fifo", "fifo")],
                 [("jobs/file", "file"), ("jobs/file", "file")],
                 [("jobs/file", "file"), ("jobs/file/child", "file")]]
        for number, entries in enumerate(cases):
            with self.subTest(entries=entries):
                self.output = self.root / ("rejected-copy-%d" % number)
                result = self.run_helper("copy", FAKE_MODE="archive", FAKE_ARCHIVE=self.archive(entries))
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(self.output.exists())
                self.assertEqual(outside.read_text(), "existing outside data")
        bad = self.root / "bad.tar"
        bad.write_text("truncated archive")
        self.output = self.root / "bad-copy"
        self.assertNotEqual(self.run_helper("copy", FAKE_MODE="archive", FAKE_ARCHIVE=str(bad)).returncode, 0)
        self.assertFalse(self.output.exists())

    def test_local_write_failure_is_not_reported_as_a_success_or_cleaned_by_deletion(self):
        archive = self.archive([("jobs/" + "x" * 300, "file")])
        result = self.run_helper("copy", FAKE_MODE="archive", FAKE_ARCHIVE=archive)
        self.assertEqual(result.returncode, 2, result.stderr)
        self.assertTrue(self.output.is_dir())
        self.assertIn("partial output is retained", result.stderr)
        self.assertNotIn("queue records copied", result.stdout)

    def test_idle_distinguishes_recorded_work_without_printing_requests(self):
        self.job()
        self.job(8)
        self.job(9)
        self.record("jobs/8/run/history.json", self.state(done=False, waiting=True))
        self.record("jobs/9/run/history.json", self.state(done=False, pending={"role": "implement"}))
        self.record("jobs/9/live/process.json", {"private": "not printed"})
        result = self.run_helper("idle")
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertEqual(json.loads(result.stdout), dict(jobs=3, running=1, waiting=1, unfinished=1,
                                                        stopped=0, stop_report_pending=0, unknown=0, idle=False))
        self.assertNotIn("fixture request", result.stdout + result.stderr)

    def test_finished_records_and_completed_stop_report_are_observationally_idle(self):
        self.job()
        self.job(8)
        self.record("jobs/8/run/history.json", self.state(done=False, pending={"role": "deliver"}))
        self.record("jobs/8/stop-request.json", {"id": 4, "content": "stop fixture"})
        self.record("jobs/8/stop-report/history.json", self.state())
        result = self.run_helper("idle")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(json.loads(result.stdout)["idle"])
        self.assertEqual(json.loads(result.stdout)["stopped"], 1)

    def test_missing_corrupt_conflicting_and_linked_history_are_not_idle(self):
        history = self.job()
        for content in ("broken json", "[]", "{}", json.dumps(self.state(done=True, waiting=True))):
            history.write_text(content)
            result = self.run_helper("idle")
            self.assertEqual(result.returncode, 1, result.stderr)
            self.assertGreater(json.loads(result.stdout)["unknown"], 0)
        history.unlink()
        self.assertEqual(self.run_helper("idle").returncode, 1)
        outside = self.root / "outside-history"
        outside.write_text(json.dumps(self.state()))
        history.symlink_to(outside)
        self.assertEqual(self.run_helper("idle").returncode, 1)

    def test_missing_or_unreadable_accepted_issue_is_not_idle(self):
        self.job()
        issue = self.queue / "jobs/7/issue.json"
        issue.unlink()
        self.assertEqual(self.run_helper("idle").returncode, 1)
        issue.write_text("unreadable")
        self.assertEqual(self.run_helper("idle").returncode, 1)

    def test_stop_report_missing_unreadable_or_unfinished_is_not_idle(self):
        self.job()
        self.record("jobs/7/stop-request.json", {"id": 4, "content": "stop fixture"})
        self.assertEqual(self.run_helper("idle").returncode, 1)
        report = self.record("jobs/7/stop-report/history.json", self.state(done=False, pending={"role": "report"}))
        result = self.run_helper("idle")
        self.assertEqual(result.returncode, 1)
        self.assertEqual(json.loads(result.stdout)["stop_report_pending"], 1)
        report.write_text("unreadable")
        result = self.run_helper("idle")
        self.assertEqual(result.returncode, 1)
        self.assertGreater(json.loads(result.stdout)["unknown"], 0)

    def test_bad_remote_idle_response_is_never_an_idle_success_or_diagnostic_dump(self):
        for reply in ("private response", "null", "[]", '[{"private":"fixture"}]', json.dumps({"idle": True}),
                      json.dumps(dict(jobs=1, running=1, waiting=0, unfinished=0, stopped=0,
                                      stop_report_pending=0, unknown=0, idle=True))):
            result = self.run_helper("idle", FAKE_MODE="reply", FAKE_REPLY=reply)
            self.assertEqual(result.returncode, 2, result.stderr)
            self.assertNotIn("private response", result.stdout + result.stderr)


if __name__ == "__main__":
    unittest.main()
