"""Build and inspect the real host bundle, without model/service calls."""
import hashlib
from pathlib import Path
import subprocess
import tempfile
import unittest


SOURCE = Path(__file__).resolve().parents[1]


class PackageTests(unittest.TestCase):
    def test_real_bundle_is_self_contained_and_never_overwrites_existing_path(self):
        with tempfile.TemporaryDirectory(prefix="role-bundle-test-") as temporary:
            root = Path(temporary)
            bundle = root / "bundle with spaces"
            result = subprocess.run(
                ["sh", str(SOURCE / "package.sh"), str(bundle)], cwd=root,
                capture_output=True, text=True, timeout=90,
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            expected = ["bin/ticket-engine", "bin/ticket-tracker", "START.md",
                        "RUNTIME.md", "README.md", "examples/operator.json",
                        "harnesses/hermes.py", "harnesses/git_workspace.py",
                        "harnesses/linux_role.py"]
            self.assertEqual(sorted(str(p.relative_to(bundle)) for p in bundle.rglob("*") if p.is_file()), sorted(expected))
            for relative in expected:
                if not relative.startswith("bin/"):
                    self.assertEqual((bundle / relative).read_bytes(), (SOURCE / relative).read_bytes())
            before = {p: hashlib.sha256(p.read_bytes()).hexdigest() for p in bundle.rglob("*") if p.is_file()}
            refused = subprocess.run(["sh", str(SOURCE / "package.sh"), str(bundle)],
                                     cwd=root, capture_output=True, text=True, timeout=10)
            self.assertNotEqual(refused.returncode, 0)
            self.assertEqual(before, {p: hashlib.sha256(p.read_bytes()).hexdigest() for p in before})
            link = root / "existing-link"
            link.symlink_to(bundle, target_is_directory=True)
            refused_link = subprocess.run(["sh", str(SOURCE / "package.sh"), str(link)],
                                          cwd=root, capture_output=True, text=True, timeout=10)
            self.assertNotEqual(refused_link.returncode, 0)
            self.assertEqual(before, {p: hashlib.sha256(p.read_bytes()).hexdigest() for p in before})
            queue = root / "must-not-accept"
            inactive = subprocess.run(
                [str(bundle / "bin/ticket-engine"), "--config", str(bundle / "examples/operator.json"),
                 "--watch", "--run-dir", str(queue)], cwd=root,
                capture_output=True, text=True, timeout=10,
            )
            self.assertNotEqual(inactive.returncode, 0)
            self.assertIn("explicit intake.project_id", inactive.stderr)
            self.assertFalse(queue.exists())


if __name__ == "__main__":
    unittest.main()
