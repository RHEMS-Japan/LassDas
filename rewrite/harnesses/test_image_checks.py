"""Exercise image-check shell logic without Docker; not a Linux sandbox test."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]


class ImageCheckTests(unittest.TestCase):
    def test_link_walk_rejects_relative_etc_hops_and_outside_targets(self):
        script = (ROOT / ".github/scripts/check-rust-toolchain.sh").read_text()
        loop = script[script.index("    for program in "):script.index("    cc --version")]
        loop = loop.replace("cc gcc cargo rustc python3", "fixture")
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            for path in ("usr/local/bin", "usr/bin", "etc/alternatives", "opt"):
                (root / path).mkdir(parents=True, exist_ok=True)
            for path in ("usr/bin/tool", "opt/tool"):
                (root / path).write_text("#!/bin/sh\nexit 0\n")
                (root / path).chmod(0o700)
            (root / "etc/alternatives/tool").symlink_to(root / "usr/bin/tool")
            # Relocate only the root paths; the actual loop and real links run.
            loop = loop.replace("/etc/*", str(root / "etc") + "/*")
            loop = loop.replace("/usr/*", str(root / "usr") + "/*")
            for target, code, reason in (("../../../etc/alternatives/tool", 1, "resolves through"),
                                         ("../../../opt/tool", 1, "outside /usr"),
                                         ("../../bin/tool", 0, "fixture ->")):
                with self.subTest(target=target):
                    link = root / "usr/local/bin/fixture"
                    link.symlink_to(target)
                    try:
                        result = subprocess.run(["bash", "-euo", "pipefail", "-c", loop],
                                                env={"PATH": str(link.parent) + os.pathsep + os.environ["PATH"]},
                                                capture_output=True, text=True, timeout=10)
                        self.assertEqual(result.returncode, code, result.stdout + result.stderr)
                        self.assertIn(reason, result.stdout)
                        print("link case:", target, "rc=", result.returncode, result.stdout.strip())
                    finally:
                        link.unlink()

    def test_role_command_runs_alternatives_tools_and_retains_no_new_privileges(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "work").mkdir()
            # The stand-in records Docker arguments and runs only the shell
            # command locally. It does NOT simulate filesystem confinement.
            docker = root / "docker"
            docker.write_text("#!" + sys.executable + "\n" + """import json,os,subprocess,sys
from pathlib import Path
Path(os.environ['CALLS']).write_text(json.dumps(sys.argv[1:]))
body=sys.argv[-1].replace('cd /work;', 'cd '+os.environ['FIXTURE_WORK']+';')
sys.exit(subprocess.call(['bash','-c',body]))
""")
            # The launcher stand-in only runs the role command after "--",
            # with no mounts: it shows what the role is asked to run.
            launcher = root / "python3"
            launcher.write_text("#!" + sys.executable + "\n" + """import os,sys
i=sys.argv.index('--')+1
os.execvp(sys.argv[i],sys.argv[i:])
""")
            for name in ("cc", "readlink", "cargo", "awk", "which"):
                path = root / name
                path.write_text("#!/bin/sh\n" +
                                'if [ "$MISSING_TOOL" = "' + name + '" ]; then exit 127; fi\n' +
                                "printf 'called: " + name + "\\n'\n" +
                                ("printf 'test tests::adds ... ok\\ntest src/lib.rs - add (line 6) ... ok\\n'\n" if name == "cargo" else ""))
                path.chmod(0o700)
            docker.chmod(0o700)
            launcher.chmod(0o700)
            env = {"PATH": str(root) + os.pathsep + os.environ["PATH"], "CALLS": str(root / "calls"),
                   "FIXTURE_WORK": str(root / "work"), "TASK_HOME": str(root / "home"), "HOME": str(root / "home")}
            for missing in ("", "awk", "which"):
                with self.subTest(missing=missing):
                    result = subprocess.run(["bash", str(ROOT / ".github/scripts/check-role-sandbox.sh"), "fixture-image"],
                                            env=dict(env, MISSING_TOOL=missing), capture_output=True, text=True, timeout=10)
                    self.assertEqual(result.returncode, 1 if missing else 0, result.stdout + result.stderr)
                    args = json.loads((root / "calls").read_text())
                    self.assertIn("no-new-privileges", args)
                    if not missing:
                        self.assertIn("called: awk", result.stdout)
                        self.assertIn("called: which", result.stdout)
                    print("role command:", missing or "available", "rc=", result.returncode)


if __name__ == "__main__":
    unittest.main()
