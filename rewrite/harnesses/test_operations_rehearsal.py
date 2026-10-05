"""Offline rehearsal entry-point checks; toolchain executables are fakes only."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


HELPER = Path(__file__).resolve().parents[2] / "deploy/ticket-engine/operations/rehearse.py"
FAKE = r'''
import json, os, sys
from pathlib import Path
root = Path(__file__).parent
settings = json.loads((root / "settings.json").read_text())
if Path(sys.argv[0]).name == "fake-git":
    (root / "git-called").write_text("yes")
    if "rev-parse" in sys.argv:
        print(settings.get("head", "a" * 40))
    else:
        assert "--ignored" in sys.argv
        if settings.get("dirty"):
            print("untracked fixture")
    sys.exit(settings.get("git_rc", 0))
(root / "go-called").write_text(json.dumps(dict(args=sys.argv[1:], env=dict(os.environ))))
print("private fixture toolchain diagnostic")
if settings.get("result", True):
    result = dict(records=2, reads=4, required_reads=1, observed_ms=1000,
                  normal_exit=True, full_tick_coverage=False)
    result.update(settings.get("changes", {}))
    Path(os.environ["REHEARSAL_RESULT"]).write_text(json.dumps(result))
sys.exit(settings.get("go_rc", 0))
'''


class OperationsRehearsalTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="offline-rehearsal-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()
        self.source = self.root / "source"
        support = self.source / "rewrite/cmd/engine/rehearsal_test.go"
        support.parent.mkdir(parents=True)
        support.write_text("func TestRehearsalOfflineQueue(t *testing.T) {}")
        self.queue = self.root / "queue"
        self.queue.mkdir()
        self.config = self.root / "config.json"
        self.config.write_text('{"private":"fixture configuration"}')
        self.reads = self.root / "reads.json"
        self.reads.write_text('{"reads":[]}')
        self.output = self.root / "new output"
        self.fake_git, self.fake_go = self.root / "fake-git", self.root / "fake-go"
        for path in (self.fake_git, self.fake_go):
            path.write_text("#!" + sys.executable + "\n" + FAKE)
            path.chmod(0o700)
        self.settings = self.root / "settings.json"
        self.settings.write_text("{}")

    def invoke(self, **settings):
        self.settings.write_text(json.dumps(settings))
        # Both executable paths are absolute fixtures. Missing fakes fail;
        # neither test is permitted to fall back to installed git or Go.
        command = [sys.executable, "-B", str(HELPER), "--source", str(self.source),
                   "--commit", "a" * 40, "--queue", str(self.queue), "--config", str(self.config),
                   "--reads", str(self.reads), "--output", str(self.output), "--seconds", "1",
                   "--git", str(self.fake_git), "--go", str(self.fake_go)]
        environment = dict(os.environ, FIXTURE_PRIVATE_KEY="must-not-reach-toolchain", GOFLAGS="unsafe fixture")
        return subprocess.run(command, env=environment, capture_output=True, text=True, timeout=10)

    def test_success_is_scoped_and_uses_private_isolated_output(self):
        before = self.config.read_bytes(), self.reads.read_bytes()
        result = self.invoke()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("not full job/tick coverage or deployment approval", result.stdout)
        self.assertNotIn("private fixture", result.stdout + result.stderr)
        call = json.loads((self.root / "go-called").read_text())
        self.assertEqual(call["args"][-2:], ["-run", "^TestRehearsalOfflineQueue$"])
        self.assertIn("-mod=readonly", call["args"])
        environment = call["env"]
        self.assertNotIn("FIXTURE_PRIVATE_KEY", environment)
        self.assertEqual(environment["GOFLAGS"], "-p=1")
        self.assertEqual(environment["GOTOOLCHAIN"], "local")
        self.assertEqual(environment["GOPROXY"], "off")
        self.assertEqual(environment["HOME"], str(self.output / "home"))
        self.assertEqual(self.output.stat().st_mode & 0o777, 0o700)
        for name in ("config.json", "reads.json", "run.log"):
            self.assertEqual((self.output / name).stat().st_mode & 0o777, 0o600)
        self.assertEqual((self.config.read_bytes(), self.reads.read_bytes()), before)

    def test_toolchain_failure_is_not_hidden_or_relayed(self):
        result = self.invoke(go_rc=7)
        self.assertEqual(result.returncode, 2)
        self.assertNotIn("private fixture", result.stdout + result.stderr)
        self.assertTrue((self.output / "run.log").is_file())
        self.assertNotIn("No write was observed", result.stdout)

    def test_missing_or_incomplete_success_evidence_is_rejected(self):
        cases = [dict(result=False), dict(changes={"reads": 0}), dict(changes={"required_reads": 0}),
                 dict(changes={"normal_exit": False}), dict(changes={"full_tick_coverage": True}),
                 dict(changes={"observed_ms": 1}), dict(changes={"records": 0})]
        for number, settings in enumerate(cases):
            with self.subTest(settings=settings):
                self.output = self.root / ("output-%d" % number)
                self.assertEqual(self.invoke(**settings).returncode, 2)

    def test_wrong_commit_dirty_source_and_missing_support_do_not_run_go(self):
        for settings in (dict(head="b" * 40), dict(dirty=True), dict(git_rc=4)):
            self.assertEqual(self.invoke(**settings).returncode, 2)
            self.assertFalse((self.root / "go-called").exists())
            self.assertFalse(self.output.exists())
        (self.source / "rewrite/cmd/engine/rehearsal_test.go").unlink()
        self.assertEqual(self.invoke().returncode, 2)
        self.assertFalse((self.root / "go-called").exists())

    def test_existing_output_links_and_output_inside_input_are_refused(self):
        self.output.mkdir()
        (self.output / "keep").write_text("existing work")
        self.assertEqual(self.invoke().returncode, 2)
        self.assertEqual((self.output / "keep").read_text(), "existing work")
        self.output = self.queue / "output"
        self.assertEqual(self.invoke().returncode, 2)
        self.output = self.root / "new-output"
        self.config.unlink()
        self.config.symlink_to(self.reads)
        self.assertEqual(self.invoke().returncode, 2)
        self.assertFalse(self.output.exists())
        self.assertFalse((self.root / "go-called").exists())

    def test_missing_fake_never_uses_a_real_toolchain(self):
        self.fake_go.unlink()
        self.assertEqual(self.invoke().returncode, 2)
        self.assertFalse((self.root / "go-called").exists())
        self.assertTrue(self.output.is_dir())
        self.output = self.root / "another-output"
        self.fake_git.unlink()
        self.assertEqual(self.invoke().returncode, 2)
        self.assertFalse(self.output.exists())

    def test_linked_queue_and_output_ancestors_are_refused(self):
        link = self.root / "linked-queue"
        link.symlink_to(self.queue, target_is_directory=True)
        self.queue = link
        self.assertEqual(self.invoke().returncode, 2)
        self.assertFalse((self.root / "git-called").exists())
        self.queue = self.root / "queue"
        self.output = link / "output"
        self.assertEqual(self.invoke().returncode, 2)
        self.assertFalse((self.root / "git-called").exists())


if __name__ == "__main__":
    unittest.main()
