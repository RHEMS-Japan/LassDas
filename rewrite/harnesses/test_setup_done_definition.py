"""Hold the guide to settling a definition of done and checking it inside a role.

The guide's in-role check runs here against local stand-ins: a bare
repository for the Pod's mirror, a launcher that records what it was given,
enters the workspace and runs the command, and an operator test that parses
the Python sources. The launcher's arguments are compared with the shipped
verify processes'. No cluster, network or credential is used; the real
launcher's confinement is tested elsewhere (test_linux_role.py).
"""
import json
import os
from pathlib import Path
import shlex
import subprocess
import sys
import tempfile
import unittest

SOURCE = Path(__file__).resolve().parents[2]
GUIDE = SOURCE / "deploy/ticket-engine/SETUP.md"
EXAMPLE = SOURCE / "rewrite/examples/operator-stages.json"
ROLE_PATH = "/runtime-policy/bin:/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin"

LAUNCHER = """import json, os, sys
arguments = sys.argv[1:]
separator = arguments.index("--")
with open(%r, "w") as record:
    json.dump({"options": arguments[:separator], "command": arguments[separator + 1:],
               "environment": dict(os.environ)}, record)
os.chdir(os.environ["TASK_WORKSPACE"])
os.execv(arguments[separator + 1], arguments[separator + 1:])
"""


def section(text, heading, following):
    start = text.index(heading)
    return text[start:text.index(following, start)]


def flat(text):
    return " ".join(text.split())


def code_after(text, marker):
    _, found, rest = text.partition(marker + "\n```sh\n")
    if not found:
        raise AssertionError("the guide no longer shows " + marker)
    return rest.partition("\n```\n")[0]


def verify_launcher_options():
    """The options the shipped verify processes give the role launcher."""
    roles = {role["name"]: role for role in json.loads(EXAMPLE.read_text(encoding="utf-8"))["roles"]}
    found = []
    for process in roles["verify"]["processes"]:
        command = process["command"]
        start = next(i for i, part in enumerate(command) if part.endswith("/linux_role.py")) + 1
        found.append(command[start:command.index("--", start)])
    if not found or any(options != found[0] for options in found):
        raise AssertionError("the shipped verify processes launch their commands differently: %r" % found)
    return found[0]


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
                "merge that pull request into the integration branch before the intake opens (section 8)",
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
                "| Each part that the definition of done says a command checks | the verify stage's command that"
                " checks it (`build`, `test` or the live check), not a command that only the definition names |",
                "with a status other than 0 |",
                "Make each break one that the part's own tools reject",
                "A command that the verify stage does not run proves nothing here, even when the definition names it.",
                "A broken part whose command still exits 0 is then not checked by that command",
                "Do not open the intake (section 8) until every run prints what the table says.",
                "A value that its process's `secrets` fills is a credential: write it there as `NAME=\"$SOURCE\"`",
                "Never write the value itself, there or on the command line."):
            self.assertIn(sentence, check)
        self.assertNotIn("**Your test script inside a role.**", self.guide)


class DefinitionOfDoneCheckTests(unittest.TestCase):
    """The documented check, run with every cluster path replaced by a local one."""

    @classmethod
    def setUpClass(cls):
        guide = GUIDE.read_text(encoding="utf-8")
        block = code_after(guide, "<!-- setup-done-check -->")
        cls.first_lines = block.split("\n")[:2]
        _, found, rest = block.partition("<<'CHECK'\n")
        if not found:
            raise AssertionError("the in-role check has no script")
        cls.script, found, _ = rest.partition("\nCHECK")
        if not found:
            raise AssertionError("the in-role check's script does not end")
        cls.secret_lines = code_after(guide, "<!-- setup-done-check-secret -->").split("\n")
        cls.options = verify_launcher_options()

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
        self.record = root / "launch.json"
        self.launcher = root / "launcher.py"
        self.launcher.write_text(LAUNCHER % str(self.record))
        self.test = root / "operator-test"
        self.test.write_text("#!/bin/sh\nset -eu\nfor f in \"${1:-src}\"/*.py; do\n"
                             "  %s -B -c 'import ast, sys; ast.parse(open(sys.argv[1]).read())' \"$f\"\ndone\n"
                             % sys.executable)
        self.test.chmod(0o755)

    def run_check(self, breaking, command=None, script=None, environment=None):
        command = command or str(self.test)
        script = script or self.script
        for old, new in (("/tmp/done-check.", str(self.scratch) + "/done-check."),
                         ("<integration-branch>", "integration"),
                         ("/var/lib/ticket-automation/mirror/<owner>/<repository-name>.git", str(self.mirror)),
                         ("python3 -B /opt/ticket-automation/bundle/harnesses/linux_role.py",
                          sys.executable + " -B " + str(self.launcher))):
            self.assertIn(old, script)
            script = script.replace(old, new)
        if self.record.exists():
            self.record.unlink()
        result = subprocess.run(["/bin/sh", "-s"], input=script,
                                env=dict(os.environ, **(environment or {}), COMMAND=command, BREAK=breaking),
                                capture_output=True, text=True, timeout=60)
        self.assertEqual(list(self.scratch.iterdir()), [], "the check left its copy behind")
        source = subprocess.run(["git", "--git-dir", str(self.mirror), "show", "integration:src/app.py"],
                                capture_output=True, text=True, check=True, timeout=10)
        self.assertEqual(source.stdout, "def greeting():\n    return 'Hello'\n", "the check changed the mirror")
        launch = json.loads(self.record.read_text()) if self.record.exists() else None
        if launch is not None:
            # The launcher is started as the verify stage starts it, and the
            # command gets its arguments as separate words.
            self.assertEqual(launch["options"], self.options)
            self.assertEqual(launch["command"], shlex.split(command))
            self.assertEqual(launch["environment"]["PATH"], ROLE_PATH)
            self.assertNotIn("COMMAND", launch["environment"])
            self.assertNotIn("BREAK", launch["environment"])
        return result, launch

    def test_the_documented_command_passes_both_values_to_the_container(self):
        self.assertEqual(self.first_lines[0], "COMMAND=/opt/ticket-automation/operator/test BREAK=")
        self.assertIn('-- env COMMAND="$COMMAND" BREAK="$BREAK" /bin/sh -s', self.first_lines[1])

    def test_an_unbroken_copy_passes(self):
        result, launch = self.run_check("")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(result.stdout.splitlines()[-1], "inside a role exit: 0")
        self.assertIsNotNone(launch)

    def test_a_command_with_arguments_gets_them_as_separate_words(self):
        result, launch = self.run_check("", command=str(self.test) + " src")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(launch["command"], [str(self.test), "src"])

    def test_a_broken_part_that_the_command_checks_fails(self):
        # "this is not Python" would parse: it compares two names.
        result, _ = self.run_check("printf 'def broken(:\\n' >> src/app.py")
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        last = result.stdout.splitlines()[-1]
        self.assertTrue(last.startswith("inside a role exit: ") and last != "inside a role exit: 0", last)
        self.assertIn("SyntaxError", result.stderr)

    def test_a_broken_part_that_no_command_checks_still_passes_and_shows_it(self):
        result, _ = self.run_check("printf 'not checked by anything\\n' >> docs/notes.md")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(result.stdout.splitlines()[-1], "inside a role exit: 0")

    def test_a_break_that_does_not_apply_or_changes_nothing_is_no_result(self):
        for breaking, said in (("printf x >> missing/directory/file", "the break did not apply; nothing was run"),
                               ("true", "the break changed nothing; nothing was run")):
            with self.subTest(breaking=breaking):
                result, launch = self.run_check(breaking)
                self.assertEqual(result.returncode, 2, result.stdout + result.stderr)
                self.assertEqual(result.stdout.splitlines(), [said])
                self.assertIsNone(launch, "a command ran although the break was no result")

    def test_a_credential_reaches_the_command_from_the_container_and_never_from_the_command_line(self):
        first, credential, last = self.secret_lines
        launch_line = next(line for line in self.script.split("\n") if line.startswith("env -i PATH="))
        self.assertEqual(first, launch_line)
        self.assertIn(last, self.script)
        self.assertIn('LIVE_CHECK_PASSWORD="$LIVE_CHECK_USER_PASSWORD"', credential)
        value = "fixture-only value, not a credential"
        script = self.script.replace(launch_line + "\n", launch_line + "\n" + credential + "\n", 1)
        self.assertNotIn(value, script)
        self.assertNotIn(value, "\n".join(self.first_lines))
        result, launch = self.run_check("", script=script, environment={"LIVE_CHECK_USER_PASSWORD": value})
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(launch["environment"]["LIVE_CHECK_PASSWORD"], value)
        self.assertEqual(launch["environment"]["LIVE_CHECK_TEST_USER"], "<agreed-test-user>")
        self.assertNotIn(value, result.stdout + result.stderr)


if __name__ == "__main__":
    unittest.main()
