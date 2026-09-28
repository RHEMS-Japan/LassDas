"""Run this suite again the way the runtime runs it: no account, no settings.

A machine whose account supplies a name and an address lets Git invent an
identity from it, so a program that relies on that invention passes here and
fails where it actually runs. The runtime has neither an account like that
nor personal configuration, and that difference is invisible until something
commits. This runs the whole suite a second time with an empty home and no
global or system configuration, which is the condition that matters.

The individual suites also forbid Git from inventing an identity, which is
the cheaper guard; this one is the wider net, because a home directory
carries more than Git's settings.
"""
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest

HARNESSES = Path(__file__).resolve().parent
ROOT = HARNESSES.parent.parent
SENTINEL = "SUITE_WITHOUT_HOST_IDENTITY"


def toolchain_cache():
    """Keep the Go build cache the outer run already has. An empty home would
    otherwise rebuild the standard library, which is not what this checks."""
    names = ("GOCACHE", "GOMODCACHE")
    carried = {name: os.environ[name] for name in names if os.environ.get(name)}
    if len(carried) == len(names) or not shutil.which("go"):
        return carried
    try:
        values = subprocess.run(["go", "env", *names], capture_output=True, text=True,
                                timeout=120, check=True).stdout.split()
    except (OSError, subprocess.SubprocessError):
        return carried
    return dict(zip(names, values)) if len(values) == len(names) else carried


@unittest.skipIf(os.environ.get(SENTINEL), "this run is already the one without a host identity")
class WithoutHostIdentityTests(unittest.TestCase):
    def test_the_suite_passes_with_an_empty_home_and_no_personal_configuration(self):
        with tempfile.TemporaryDirectory(prefix="without-host-identity-") as temporary:
            home = Path(temporary) / "home"
            home.mkdir()
            environment = dict(toolchain_cache(), PATH=os.environ["PATH"], LANG="C.UTF-8",
                               HOME=str(home), TMPDIR=temporary, PYTHONDONTWRITEBYTECODE="1",
                               GIT_CONFIG_GLOBAL=os.devnull, GIT_CONFIG_SYSTEM=os.devnull,
                               GIT_CONFIG_NOSYSTEM="1")
            environment[SENTINEL] = "1"
            result = subprocess.run(
                [sys.executable, "-B", "-m", "unittest", "discover",
                 "-s", "rewrite/harnesses", "-p", "test_*.py"],
                cwd=str(ROOT), env=environment, capture_output=True, text=True, timeout=1800)
            # unittest writes its result to stderr; keep both for the failure.
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertRegex(result.stderr, r"\nOK( \(skipped=\d+\))?\n")


if __name__ == "__main__":
    unittest.main()
