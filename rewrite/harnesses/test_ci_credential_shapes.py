"""The credential-shape scan, run the way CI runs it, on repositories made here.

Every credential below is assembled while the test runs, from a prefix and a
filler, so no file holds one in a shape the scan reports: the scan reads this
file too, and it has no exceptions.
"""
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / ".github/scripts/scan-credential-shapes.py"

# A made-up value for every shape the scan reads, with the name it reports.
FOUND = (
    ("GitHub token", "ghp_" + "x" * 36),
    ("GitHub token", "gho_" + "x" * 36),
    ("GitHub token", "ghu_" + "x" * 36),
    ("GitHub token", "ghs_" + "x" * 36),
    ("GitHub token", "ghr_" + "x" * 40),
    ("GitHub fine-grained token", "github_pat_" + "x" * 22 + "_" + "x" * 59),
    ("OpenRouter key", "sk-or-v1-" + "0" * 64),
    ("Anthropic key", "sk-ant-" + "x" * 20),
    ("Anthropic key", "sk-ant-api03-" + "x" * 90),
    ("AWS access key ID", "AKIA" + "X" * 16),
    ("private key block", "-" * 5 + "BEGIN PRIVATE KEY" + "-" * 5),
    ("private key block", "-" * 5 + "BEGIN RSA PRIVATE KEY" + "-" * 5),
    ("private key block", "-" * 5 + "BEGIN OPENSSH PRIVATE KEY" + "-" * 5),
    ("private key block", "-" * 5 + "BEGIN PGP PRIVATE KEY BLOCK" + "-" * 5),
    ("Slack token", "xoxb-" + "0" * 10 + "-" + "x" * 24),
    ("Slack token", "xoxp-" + "0" * 10 + "-" + "x" * 32),
    ("Slack token", "xoxa-2-" + "x" * 30),
    ("apiKey or api_key value", "?apiKey=" + "x" * 64),
    ("apiKey or api_key value", '"api_key": "' + "x" * 32 + '"'),
    ("apiKey or api_key value", "apiKey := '" + "x" * 40 + "'"),
)

# Text next to each shape that is not one, and passes.
PASSED = (
    "ghp_" + "x" * 35,  # one character short
    "ghx_" + "x" * 36,  # a prefix GitHub does not use
    "GHP_" + "x" * 36,  # letter case is part of the shape
    "github_pat_",  # the prefix alone, as documentation names it
    "github_pat_" + "x" * 21 + "_" + "x" * 59,
    "sk-or-v1-" + "0" * 63,
    "sk-or-v1-" + "x" * 64,  # not hexadecimal
    "sk-ant-" + "x" * 19,
    "AKIA" + "X" * 15,
    "AKIA" + "x" * 16,
    "-" * 5 + "BEGIN PUBLIC KEY" + "-" * 5,
    "-" * 5 + "BEGIN CERTIFICATE" + "-" * 5,
    "xoxb-",
    "xoxb-" + "x" * 9,
    "xoxz-" + "0" * 10 + "-" + "x" * 24,
    "apiKey=" + "x" * 31,
    "apiKey" + "x" * 64,  # a longer name, not a value
    "apiKey: ${{ secrets.API_KEY }}",
)


def environment(**settings):
    """No personal configuration and no guessed identity, as the other suites
    run Git, and nothing of a CI run (GITHUB_REF, the range) unless given."""
    return dict({"PATH": os.environ["PATH"], "LANG": "C.UTF-8", "GIT_CONFIG_GLOBAL": os.devnull,
                 "GIT_CONFIG_SYSTEM": os.devnull, "GIT_CONFIG_NOSYSTEM": "1", "GIT_TERMINAL_PROMPT": "0",
                 "GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "user.useConfigOnly",
                 "GIT_CONFIG_VALUE_0": "true", "GIT_AUTHOR_NAME": "Fixture",
                 "GIT_AUTHOR_EMAIL": "fixture@example.invalid", "GIT_COMMITTER_NAME": "Fixture",
                 "GIT_COMMITTER_EMAIL": "fixture@example.invalid"}, **settings)


@unittest.skipUnless(SCRIPT.is_file() and shutil.which("git"), "requires the repository's CI scripts and Git")
class CredentialShapeScanTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="credential-shapes-")
        self.addCleanup(temporary.cleanup)
        self.repository = Path(temporary.name)
        self.git("init", "-q", "-b", "main")

    def git(self, *arguments, message=None, directory=None):
        return subprocess.run(["git", *arguments], cwd=directory or self.repository, env=environment(),
                              input=message, capture_output=True, text=True, check=True,
                              timeout=30).stdout.strip()

    def track(self, name, content):
        path = self.repository / name
        if isinstance(content, bytes):
            path.write_bytes(content)
        else:
            path.write_text(content, encoding="utf-8")
        self.git("add", name)

    def commit(self, message):
        self.git("commit", "-q", "--allow-empty", "-F", "-", message=message)
        return self.git("rev-parse", "HEAD")

    def scan(self, *arguments, directory=None, **settings):
        done = subprocess.run([sys.executable, "-B", str(SCRIPT), *arguments],
                              cwd=directory or self.repository, env=environment(**settings),
                              capture_output=True, text=True, timeout=120)
        return done.returncode, done.stdout + done.stderr

    def assertNotShown(self, value, output):
        """No ten characters in a row of the value reach the output."""
        for start in range(len(value) - 9):
            self.assertNotIn(value[start:start + 10], output)

    def test_each_shape_fails_the_scan_by_place_and_name_never_by_text(self):
        for number, (shape, value) in enumerate(FOUND, 1):
            with self.subTest(number=number, shape=shape):
                self.track("notes.txt", "a line without a key\n" + value + "\n")
                status, output = self.scan("tree")
                self.assertEqual(status, 1, output)
                self.assertIn("notes.txt:2: %s\n" % shape, output)
                self.assertEqual(output.count("notes.txt:"), 1, output)
                self.assertNotShown(value, output)

    def test_text_next_to_the_shapes_passes(self):
        self.track("notes.txt", "\n".join(PASSED) + "\n")
        status, output = self.scan("tree")
        self.assertEqual(status, 0, output)
        self.assertIn("read 1 tracked file; none holds a credential shape", output)

    def test_this_repository_passes_whole_from_a_subdirectory(self):
        tracked = self.git("ls-files", "-z", directory=ROOT).count("\0")
        status, output = self.scan("tree", directory=ROOT / "rewrite")
        self.assertEqual(status, 0, output)
        self.assertIn("read %d tracked files; none holds a credential shape" % tracked, output)

    def test_only_tracked_files_are_read_and_binary_ones_too(self):
        self.track("tracked.txt", "nothing to find\n")
        (self.repository / "untracked.txt").write_text(FOUND[0][1] + "\n", encoding="utf-8")
        status, output = self.scan("tree")
        self.assertEqual(status, 0, output)
        self.assertIn("read 1 tracked file;", output)
        self.track("blob.bin", b"\x00\xff\xfe\r\n" + FOUND[9][1].encode() + b"\x00\x01\n")
        status, output = self.scan("tree")
        self.assertEqual(status, 1, output)
        self.assertIn("blob.bin:2: AWS access key ID\n", output)
        self.assertNotIn("untracked.txt", output)

    def test_files_that_cannot_be_read_do_not_pass(self):
        self.track("gone.txt", "text\n")
        (self.repository / "gone.txt").unlink()
        status, output = self.scan("tree")
        self.assertEqual(status, 2, output)
        self.assertIn("gone.txt: cannot be read", output)
        self.git("rm", "-q", "--cached", "gone.txt")
        status, output = self.scan("tree")
        self.assertEqual(status, 2, output)
        self.assertIn("no tracked files", output)

    def test_commit_messages_are_read_line_by_line(self):
        clean = self.commit("A subject\n\nA body without a key\n")
        leaked = self.commit("A subject\n\nA body\n" + FOUND[0][1] + "\n")
        status, output = self.scan("messages", "HEAD")
        self.assertEqual(status, 1, output)
        self.assertIn("commit %s line 4: GitHub token\n" % leaked, output)
        self.assertNotIn(clean, output)
        self.assertNotShown(FOUND[0][1], output)
        status, output = self.scan("messages", clean)
        self.assertEqual(status, 0, output)
        self.assertIn("read the messages of 1 commit; none holds a credential shape", output)

    def test_the_pushed_range_is_the_one_the_identifier_scan_reads(self):
        first = self.commit("Start\n")
        published = self.commit("Published\n\n" + FOUND[9][1] + "\n")
        self.git("update-ref", "refs/remotes/origin/main", published)
        self.git("checkout", "-q", "-b", "topic")
        clean = self.commit("A clean change\n")
        leaked = self.commit("A change\n\n" + FOUND[0][1] + "\n")
        topic = {"DEFAULT_BRANCH": "main", "GITHUB_REF": "refs/heads/topic"}
        main = {"DEFAULT_BRANCH": "main", "GITHUB_REF": "refs/heads/main"}
        for case, settings, expected, commits, reported in (
            ("a push to a branch", dict(topic, RANGE_BASE=clean, RANGE_HEAD=leaked), 1, 1, [leaked]),
            ("a range reaching into main", dict(topic, RANGE_BASE=first, RANGE_HEAD=leaked), 1, 2, [leaked]),
            ("the first push of a branch", dict(topic, RANGE_BASE="0" * 40, RANGE_HEAD=leaked), 1, 2, [leaked]),
            ("a clean push", dict(topic, RANGE_BASE=published, RANGE_HEAD=clean), 0, 1, []),
            ("a push to main", dict(main, RANGE_BASE=first, RANGE_HEAD=published), 1, 1, [published]),
            ("a forced push to main", dict(main, RANGE_BASE="", RANGE_HEAD=published), 1, 2, [published]),
        ):
            with self.subTest(case):
                status, output = self.scan("messages", **settings)
                self.assertEqual(status, expected, output)
                self.assertIn("the messages of %d commit" % commits, output)
                for commit in (first, published, clean, leaked):
                    self.assertEqual("commit %s line 3: " % commit in output, commit in reported, output)

    def test_a_range_that_cannot_be_told_and_wrong_use_do_not_pass(self):
        self.commit("Start\n")
        for arguments, settings, reason in (
            (["messages"], {}, "is not set"),
            (["messages"], {"DEFAULT_BRANCH": "main", "GITHUB_REF": "refs/heads/topic", "RANGE_HEAD": "HEAD"},
             "cannot be told apart"),
            ([], {}, "usage:"),
            (["tree", "extra"], {}, "usage:"),
            (["files"], {}, "usage:"),
        ):
            with self.subTest(arguments=arguments, settings=settings):
                status, output = self.scan(*arguments, **settings)
                self.assertEqual(status, 2, output)
                self.assertIn(reason, output)


if __name__ == "__main__":
    unittest.main()
