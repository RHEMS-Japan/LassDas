"""Hold the guide to settling a definition of done and checking it inside a role.

The guide's in-role check runs here against local stand-ins: a bare
repository for the Pod's mirror, a launcher that only enters the workspace and
runs the command, and an operator test that parses the Python sources. No
cluster, network or credential is used; the real launcher's confinement is
tested elsewhere (test_linux_role.py).
"""
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

SOURCE = Path(__file__).resolve().parents[2]
GUIDE = SOURCE / "deploy/ticket-engine/SETUP.md"

LAUNCHER = """import os, sys
arguments = sys.argv[1:]
separator = arguments.index("--")
options, command = arguments[:separator], arguments[separator + 1:]
runtimes = [options[i + 1] for i, option in enumerate(options) if option == "--runtime"]
if runtimes != ["/opt/ticket-automation/bundle", "/opt/ticket-automation/operator"] or "--network" not in options:
    sys.exit("stand-in launcher: unexpected options %r" % options)
if os.environ.get("PATH", "").split(":")[0] != "/runtime-policy/bin" or "COMMAND" in os.environ:
    sys.exit("stand-in launcher: the environment was not cleared as the verify stage clears it")
os.chdir(os.environ["TASK_WORKSPACE"])
os.execv(command[0], command)
"""


def section(text, heading, following):
    start = text.index(heading)
    return text[start:text.index(following, start)]


def flat(text):
    return " ".join(text.split())


class DefinitionOfDoneGuideTests(unittest.TestCase):
    def setUp(self):
        self.guide = GUIDE.read_text(encoding="utf-8")

    def test_the_owner_is_asked_four_questions_for_each_part_before_the_configuration(self):
        settle = self.guide.index("### What counts as done in this repository\n")
        self.assertLess(settle, self.guide.index("### The branch must pass before you start\n"))
        self.assertLess(settle, self.guide.index("## 4. Writing the operator configuration\n"))
        body = flat(section(self.guide, "### What counts as done in this repository\n", "### The branch must pass"))
        for sentence in (
                "List the parts of the repository first",
                "Ask the four questions below for each part",
                "1. What must be seen before a change to this part counts as correct?",
                "2. Which command checks that, and does it run in the engine's environment:",
                "3. How is the change's behaviour observed? If the answer is only that the build or the tests pass,"
                " ask what a person would look at to see the change work.",
                "4. What can no machine check here, and who checks it?",
                "it asks the owner; it does not answer for them",
                "the engine requires no file name and reads no format",
                "The engine is given only its location",
                "adds only and never removes or weakens an item",
                "run the check in section 7 again after either changes"):
            self.assertIn(sentence, body)

    def test_the_guidance_names_the_location_and_the_report_says_what_was_not_checked(self):
        guidance = flat(section(self.guide, "### Finish the project guidance (either tracker)\n", "# setup-guidance-edit"))
        self.assertIn("Name where the definition of done", guidance)
        self.assertIn("its location alone does not let them write to it", guidance)
        self.assertIn("`完了の定義: なし (導入先が定めていない)`", guidance)
        delivered = flat(section(self.guide, "### After it is delivered\n", "## 9. Stopping a request"))
        self.assertIn("`確かめていない` is one that nobody checked", delivered)

    def test_every_command_and_every_checked_part_is_run_before_the_intake_opens(self):
        check = flat(section(self.guide, "**What counts as done, checked inside a role.**", "**The queue survives a restart.**"))
        self.assertLess(self.guide.index("**What counts as done, checked inside a role.**"),
                        self.guide.index("## 8. Opening the intake and filing the first ticket"))
        for sentence in (
                "| Each command the verify stage runs |",
                "| empty | `inside a role exit: 0` |",
                "| Each part that the definition of done says a command checks | the command that checks it |",
                "with a status other than 0 |",
                "Make each break one that the part's own tools reject",
                "A broken part whose command still exits 0 is then not checked by that command",
                "Do not open the intake (section 8) until every run prints what the table says."):
            self.assertIn(sentence, check)
        self.assertNotIn("**Your test script inside a role.**", self.guide)


class DefinitionOfDoneCheckTests(unittest.TestCase):
    """The documented check, run with every cluster path replaced by a local one."""

    @classmethod
    def setUpClass(cls):
        guide = GUIDE.read_text(encoding="utf-8")
        _, found, rest = guide.partition("<!-- setup-done-check -->\n```sh\n")
        if not found:
            raise AssertionError("the guide no longer shows the in-role check of the definition of done")
        block = rest.partition("\n```\n")[0]
        lines = block.split("\n")
        cls.first_lines = lines[:2]
        _, found, rest = block.partition("<<'CHECK'\n")
        if not found:
            raise AssertionError("the in-role check has no script")
        cls.script, found, _ = rest.partition("\nCHECK")
        if not found:
            raise AssertionError("the in-role check's script does not end")

    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="done-definition-")
        self.addCleanup(self.temporary.cleanup)
        root = Path(self.temporary.name)
        self.scratch = root / "tmp"
        self.scratch.mkdir()
        self.mirror = root / "mirror.git"
        origin = root / "origin"
        (origin / "src").mkdir(parents=True)
        (origin / "docs").mkdir()
        (origin / "src/app.py").write_text("def greeting():\n    return 'Hello'\n")
        (origin / "docs/notes.md").write_text("# Notes\n")
        git = ["git", "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid",
               "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"]
        for command in (git + ["init", "--quiet", str(origin)],
                        git + ["-C", str(origin), "add", "."],
                        git + ["-C", str(origin), "commit", "--quiet", "-m", "fixture"],
                        git + ["init", "--quiet", "--bare", str(self.mirror)],
                        git + ["-C", str(origin), "push", "--quiet", str(self.mirror), "HEAD:refs/heads/integration"]):
            subprocess.run(command, check=True, capture_output=True, timeout=30)
        self.launcher = root / "launcher.py"
        self.launcher.write_text(LAUNCHER)
        self.test = root / "operator-test"
        self.test.write_text("#!/bin/sh\nset -eu\nfor f in src/*.py; do\n"
                             "  %s -B -c 'import ast, sys; ast.parse(open(sys.argv[1]).read())' \"$f\"\ndone\n"
                             % sys.executable)
        self.test.chmod(0o755)

    def run_check(self, breaking):
        script = self.script
        for old, new in (("/tmp/done-check.", str(self.scratch) + "/done-check."),
                         ("<integration-branch>", "integration"),
                         ("/var/lib/ticket-automation/mirror/<owner>/<repository-name>.git", str(self.mirror)),
                         ("python3 -B /opt/ticket-automation/bundle/harnesses/linux_role.py",
                          sys.executable + " -B " + str(self.launcher))):
            self.assertIn(old, script)
            script = script.replace(old, new)
        environment = dict(os.environ, COMMAND=str(self.test), BREAK=breaking)
        result = subprocess.run(["/bin/sh", "-s"], input=script, env=environment,
                                capture_output=True, text=True, timeout=60)
        self.assertEqual(list(self.scratch.iterdir()), [], "the check left its copy behind")
        source = subprocess.run(["git", "--git-dir", str(self.mirror), "show", "integration:src/app.py"],
                                capture_output=True, text=True, check=True, timeout=10)
        self.assertEqual(source.stdout, "def greeting():\n    return 'Hello'\n", "the check changed the mirror")
        return result

    def test_the_documented_command_passes_both_values_to_the_container(self):
        self.assertEqual(self.first_lines[0], "COMMAND=/opt/ticket-automation/operator/test BREAK=")
        self.assertIn('-- env COMMAND="$COMMAND" BREAK="$BREAK" /bin/sh -s', self.first_lines[1])

    def test_an_unbroken_copy_passes(self):
        result = self.run_check("")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(result.stdout.splitlines()[-1], "inside a role exit: 0")

    def test_a_broken_part_that_the_command_checks_fails(self):
        # "this is not Python" would parse: it compares two names.
        result = self.run_check("printf 'def broken(:\\n' >> src/app.py")
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        last = result.stdout.splitlines()[-1]
        self.assertTrue(last.startswith("inside a role exit: ") and last != "inside a role exit: 0", last)
        self.assertIn("SyntaxError", result.stderr)

    def test_a_broken_part_that_no_command_checks_still_passes_and_shows_it(self):
        result = self.run_check("printf 'not checked by anything\\n' >> docs/notes.md")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(result.stdout.splitlines()[-1], "inside a role exit: 0")

    def test_a_break_that_does_not_apply_or_changes_nothing_is_no_result(self):
        for breaking, said in (("printf x >> missing/directory/file", "the break did not apply; nothing was run"),
                               ("true", "the break changed nothing; nothing was run")):
            with self.subTest(breaking=breaking):
                result = self.run_check(breaking)
                self.assertEqual(result.returncode, 2, result.stdout + result.stderr)
                self.assertEqual(result.stdout.splitlines(), [said])


if __name__ == "__main__":
    unittest.main()
