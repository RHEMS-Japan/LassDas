"""Real Git/process tests; no model judgment or live service is simulated as success."""
import fcntl
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import unittest

LAUNCHER = Path(__file__).with_name("git_workspace.py").resolve()
CHILD = "import json,os,sys;from pathlib import Path;print(json.dumps(dict(prompt=sys.stdin.read(),cwd=os.getcwd(),body=Path('entry.txt').read_text(),model=os.environ.get('NATIVE_MODEL'))))"


@unittest.skipUnless(shutil.which("git"), "requires Git")
class WorkspaceTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name).resolve()
        self.source = self.root / "source with spaces"
        self.source.mkdir()
        self.env = {"PATH": os.environ["PATH"], "LANG": "C", "LC_ALL": "C",
                    "PYTHONDONTWRITEBYTECODE": "1", "GIT_CONFIG_GLOBAL": os.devnull,
                    "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_SYSTEM": os.devnull,
                    "GIT_TERMINAL_PROMPT": "0"}
        self.git(self.source, "init", "--initial-branch=main")
        (self.source / "entry.txt").write_text("original\n")
        self.commit("initial")
        self.original_head = self.git(self.source, "rev-parse", "HEAD").stdout.strip()
        self.git(self.source, "tag", "first-version")

    def git(self, directory, *arguments):
        return subprocess.run(["git", "-C", str(directory), "-c", "user.name=Fixture",
                               "-c", "user.email=fixture@example.invalid", *arguments],
                              env=self.env, check=True, capture_output=True, text=True)

    def commit(self, name):
        self.git(self.source, "add", "entry.txt")
        self.git(self.source, "commit", "-m", "Codex: " + name)

    def workspace(self, name):
        path = self.root / name / "workspace"
        path.mkdir(parents=True)
        return path

    def start(self, workspace, extra=None, child_code=CHILD):
        env = dict(self.env, TASK_WORKSPACE=str(workspace), TASK_REPOSITORY=str(self.source),
                   NATIVE_MODEL="example/current-selection")
        env.update(extra or {})
        child = subprocess.Popen([sys.executable, str(LAUNCHER), "--", sys.executable, "-c", child_code],
                                 cwd=workspace, env=env, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                 stderr=subprocess.PIPE, text=True, start_new_session=True)
        self.addCleanup(self.stop, child)
        return child

    @staticmethod
    def stop(child):
        if child.poll() is None:
            os.killpg(child.pid, signal.SIGKILL)
        child.communicate()

    def run_launcher(self, workspace, extra=None):
        child = self.start(workspace, extra)
        out, err = child.communicate("原文\n$(literal)\n", timeout=15)
        return child.returncode, out, err

    def wait_file(self, path):
        deadline = time.monotonic() + 5
        while not path.exists() and time.monotonic() < deadline:
            time.sleep(0.01)
        self.assertTrue(path.exists(), "test checkout did not reach the controlled interruption point")

    def paused_git(self):
        tools = self.root / "tools"
        tools.mkdir()
        executable = tools / "git"
        # The actual clone runs normally. Pause only just before the real
        # checkout, when the source is still private and unpublished.
        executable.write_text("#!" + sys.executable + "\n" + """import os,sys,time
from pathlib import Path
if 'checkout' in sys.argv:
 Path(os.environ['TEST_CHECKOUT_REACHED']).write_text(str(os.getpid()))
 while not Path(os.environ['TEST_CHECKOUT_RELEASE']).exists(): time.sleep(0.01)
os.execv(os.environ['TEST_REAL_GIT'], [os.environ['TEST_REAL_GIT'], *sys.argv[1:]])
""")
        executable.chmod(0o700)
        return {"PATH": str(tools) + os.pathsep + self.env["PATH"],
                "TEST_REAL_GIT": shutil.which("git"),
                "TEST_CHECKOUT_REACHED": str(self.root / "checkout-reached"),
                "TEST_CHECKOUT_RELEASE": str(self.root / "checkout-release")}

    def test_empty_workspace_is_ready_before_child_and_prose_is_unchanged(self):
        workspace = self.workspace("one")
        code, out, err = self.run_launcher(workspace)
        self.assertEqual(code, 0, err)
        self.assertEqual(json.loads(out), {"prompt": "原文\n$(literal)\n", "cwd": str(workspace),
                                          "body": "original\n", "model": "example/current-selection"})
        self.assertEqual(self.git(workspace, "rev-parse", "HEAD").stdout.strip(), self.original_head)
        self.assertEqual(self.git(workspace, "branch", "--show-current").stdout, "")
        self.assertIn("Cloning into", err)
        self.assertFalse(list(workspace.parent.glob(".source-*")))

    def test_restart_preserves_dirty_work_local_commit_and_untracked_files(self):
        workspace = self.workspace("resume")
        self.assertEqual(self.run_launcher(workspace)[0], 0)
        (workspace / "entry.txt").write_text("local commit\n")
        self.git(workspace, "add", "entry.txt")
        self.git(workspace, "commit", "-m", "Codex: task work")
        task_head = self.git(workspace, "rev-parse", "HEAD").stdout
        (workspace / "entry.txt").write_text("unfinished edit\n")
        (workspace / "notes.txt").write_text("untracked progress\n")
        (self.source / "entry.txt").write_text("upstream changed\n")
        self.commit("upstream")
        code, out, err = self.run_launcher(workspace, {"TASK_REPOSITORY": str(self.root / "unavailable")})
        self.assertEqual(code, 0, err)
        self.assertEqual(json.loads(out)["body"], "unfinished edit\n")
        self.assertEqual(self.git(workspace, "rev-parse", "HEAD").stdout, task_head)
        self.assertEqual((workspace / "notes.txt").read_text(), "untracked progress\n")
        self.assertNotIn("Cloning into", err)

    def test_empty_repository_reaches_the_first_implementation(self):
        source = self.root / "empty-source"
        source.mkdir()
        self.git(source, "init", "--initial-branch=main")
        workspace = self.workspace("new-project")
        child = self.start(workspace, {"TASK_REPOSITORY": str(source)},
                           "import sys;from pathlib import Path;assert Path('.git').is_dir();Path('entry.txt').write_text('first implementation');print(sys.stdin.read())")
        out, err = child.communicate("Create the initial project.", timeout=15)
        self.assertEqual(child.returncode, 0, err)
        self.assertEqual(out, "Create the initial project.\n")
        self.assertEqual((workspace / "entry.txt").read_text(), "first implementation")

    def test_separate_requests_get_independent_work_and_new_source_tip(self):
        first, second = self.workspace("first"), self.workspace("second")
        self.assertEqual(self.run_launcher(first)[0], 0)
        (first / "entry.txt").write_text("first private edit\n")
        (self.source / "entry.txt").write_text("new upstream\n")
        self.commit("new tip")
        code, out, err = self.run_launcher(second)
        self.assertEqual(code, 0, err)
        self.assertEqual(json.loads(out)["body"], "new upstream\n")
        self.assertEqual((first / "entry.txt").read_text(), "first private edit\n")
        self.assertNotEqual(self.git(first, "rev-parse", "HEAD").stdout,
                            self.git(second, "rev-parse", "HEAD").stdout)
        self.assertFalse((second / ".git/objects/info/alternates").exists())
        for original in (self.source / ".git/objects").rglob("*"):
            if original.is_file():
                copied = first / ".git/objects" / original.relative_to(self.source / ".git/objects")
                if copied.is_file():
                    a, b = original.stat(), copied.stat()
                    self.assertNotEqual((a.st_dev, a.st_ino), (b.st_dev, b.st_ino), "shared writable Git object")

    def test_two_concurrent_launchers_clone_once_and_enter_published_directory(self):
        workspace = self.workspace("parallel")
        pause = self.paused_git()
        first = self.start(workspace, pause)
        self.wait_file(Path(pause["TEST_CHECKOUT_REACHED"]))
        # Observe the actual OS lock while checkout is paused. Merely starting
        # a second process then releasing the first can miss the overlap and
        # allow a lock-free mutant to pass because of scheduler timing.
        with (workspace.parent / ".workspace.prepare.lock").open("r+") as observer:
            with self.assertRaises(BlockingIOError):
                fcntl.flock(observer, fcntl.LOCK_EX | fcntl.LOCK_NB)
        second = self.start(workspace)
        Path(pause["TEST_CHECKOUT_RELEASE"]).touch()
        one, err1 = first.communicate("first", timeout=15)
        two, err2 = second.communicate("second", timeout=15)
        self.assertEqual((first.returncode, second.returncode), (0, 0), err1 + err2)
        self.assertEqual(json.loads(one)["cwd"], str(workspace))
        self.assertEqual(json.loads(two)["body"], "original\n")
        self.assertEqual((err1 + err2).count("Cloning into"), 1)

    def test_interrupted_checkout_is_not_published_and_restart_recovers(self):
        workspace = self.workspace("interrupted")
        pause = self.paused_git()
        first = self.start(workspace, pause)
        self.wait_file(Path(pause["TEST_CHECKOUT_REACHED"]))
        self.assertEqual(list(workspace.iterdir()), [])
        os.killpg(first.pid, signal.SIGTERM)
        first.communicate(timeout=5)
        self.assertLess(first.returncode, 0)
        code, out, err = self.run_launcher(workspace)
        self.assertEqual(code, 0, err)
        self.assertEqual(json.loads(out)["body"], "original\n")
        self.assertEqual(self.git(workspace, "rev-parse", "HEAD").stdout.strip(), self.original_head)

    def test_failed_clone_does_not_launch_role_and_retry_can_recover(self):
        workspace = self.workspace("failed")
        code, out, err = self.run_launcher(workspace, {"TASK_REPOSITORY": str(self.root / "missing-source")})
        self.assertNotEqual(code, 0)
        self.assertEqual(out, "")
        self.assertIn("does not exist", err)
        self.assertEqual(list(workspace.iterdir()), [])
        self.assertFalse(list(workspace.parent.glob(".source-*")))
        self.assertEqual(self.run_launcher(workspace)[0], 0)

    def test_work_appearing_during_clone_is_not_overwritten(self):
        workspace = self.workspace("external-writer")
        pause = self.paused_git()
        child = self.start(workspace, pause)
        self.wait_file(Path(pause["TEST_CHECKOUT_REACHED"]))
        (workspace / "entry.txt").write_text("work from another launcher\n")
        Path(pause["TEST_CHECKOUT_RELEASE"]).touch()
        out, err = child.communicate("request", timeout=15)
        self.assertNotEqual(child.returncode, 0)
        self.assertEqual(out, "")
        self.assertIn("not replaced", err)
        self.assertEqual((workspace / "entry.txt").read_text(), "work from another launcher\n")

    def test_explicit_tag_is_used_only_for_initial_preparation(self):
        (self.source / "entry.txt").write_text("new upstream\n")
        self.commit("next version")
        workspace = self.workspace("tag")
        code, out, err = self.run_launcher(workspace, {"TASK_BRANCH": "first-version"})
        self.assertEqual(code, 0, err)
        self.assertEqual(json.loads(out)["body"], "original\n")

    def test_nonempty_work_and_symlinks_are_not_replaced(self):
        workspace = self.workspace("existing")
        (workspace / "entry.txt").write_text("existing nongit work\n")
        self.assertEqual(json.loads(self.run_launcher(workspace)[1])["body"], "existing nongit work\n")
        self.assertFalse((workspace / ".git").exists())
        alias = self.root / "alias"
        alias.symlink_to(workspace, target_is_directory=True)
        code, out, err = self.run_launcher(alias)
        self.assertNotEqual(code, 0)
        self.assertEqual(out, "")
        self.assertIn("real job directory", err)
        empty = self.workspace("lock-symlink")
        sentinel = self.root / "sentinel"
        sentinel.write_text("preserve me")
        (empty.parent / ".workspace.prepare.lock").symlink_to(sentinel)
        self.assertNotEqual(self.run_launcher(empty)[0], 0)
        self.assertEqual(sentinel.read_text(), "preserve me")


if __name__ == "__main__":
    unittest.main()
