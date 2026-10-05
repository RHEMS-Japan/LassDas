"""Build and inspect the real host bundle, without model/service calls."""
import hashlib
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


SOURCE = Path(__file__).resolve().parents[1]


class PackageTests(unittest.TestCase):
    def test_github_ordered_setup_copies_the_shipped_example_and_replaces_all_destinations(self):
        guide = (SOURCE.parent / "deploy/ticket-engine/SETUP.md").read_text()
        section = guide.split("### GitHub: make the ordered copy\n", 1)[1].split("### Finish the project guidance", 1)[0]
        script = section.split("<<'PY'\n", 1)[1].split("\nPY\n", 1)[0]
        self.assertIn("rewrite/examples/operator-github-stages.json", script)
        with tempfile.TemporaryDirectory(prefix="ordered-setup-test-") as temporary:
            target = Path(temporary) / "operator.json"
            command = ["python3", "-c", script, str(target), "sample-owner/intake", "sample-owner/delivery", "integration"]
            copied = subprocess.run(command, cwd=SOURCE.parent, capture_output=True, text=True, timeout=10)
            self.assertEqual(copied.returncode, 0, copied.stderr)
            raw = target.read_text()
            configured = json.loads(raw)
            original = json.loads((SOURCE / "examples/operator-github-stages.json").read_text())
            self.assertNotRegex(raw, r"REPLACE_WITH_|example[.]invalid|example-owner|example-repository|example-integration-branch|<[a-z-]+>")
            self.assertEqual(configured["github"]["repository"], "sample-owner/intake")
            self.assertNotIn("api_url", configured["github"])
            self.assertNotIn("backlog", configured)
            for name in ("project_id", "category_ids", "category_on_accept", "statuses"):
                self.assertNotIn(name, configured["intake"])
            self.assertEqual(configured["intake"]["created_since"], "2100-01-01T00:00:00Z")
            self.assertEqual(configured["workflow"], original["workflow"])
            self.assertEqual(configured["instructions"], original["instructions"])
            self.assertTrue(configured["instructions"].startswith("Operator setup is incomplete"))
            self.assertEqual(configured["github"]["labels"], original["github"]["labels"])
            for role in configured["roles"]:
                for process in role["processes"]:
                    env = process.get("env", {})
                    for name, value in {
                        "TASK_REPOSITORY": "/var/lib/ticket-automation/mirror/sample-owner/delivery.git",
                        "TASK_BRANCH": "integration",
                        "DELIVERY_REPOSITORY": "sample-owner/delivery",
                        "DELIVERY_BASE_BRANCH": "integration",
                    }.items():
                        if name in env:
                            self.assertEqual(env[name], value)
            before = target.read_bytes()
            refused = subprocess.run(command, cwd=SOURCE.parent, capture_output=True, text=True, timeout=10)
            self.assertNotEqual(refused.returncode, 0)
            self.assertEqual(target.read_bytes(), before)
        self.assertRegex(guide, r"grep -n -E 'REPLACE_WITH_\|example\[\.\]invalid\|example-owner\|example-repository\|example-integration-branch")

    def test_real_bundle_is_self_contained_and_never_overwrites_existing_path(self):
        with tempfile.TemporaryDirectory(prefix="role-bundle-test-") as temporary:
            root = Path(temporary)
            bundle = root / "bundle with spaces"
            result = subprocess.run(
                ["sh", str(SOURCE / "package.sh"), str(bundle)], cwd=root,
                capture_output=True, text=True, timeout=90,
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            expected = ["bin/ticket-engine", "bin/ticket-tracker", "bin/ticket-status", "START.md",
                        "RUNTIME.md", "README.md", "OPERATING.md", "examples/operator.json",
                        "examples/operator-gateway.json",
                        "examples/operator-stages.json",
                        "examples/operator-github.json",
                        "examples/operator-github-stages.json",
                        "harnesses/hermes.py", "harnesses/raven.py", "harnesses/git_workspace.py",
                        "harnesses/linux_role.py", "harnesses/adversarial_review.py",
                        "harnesses/delivery_support.py"]
            self.assertEqual(sorted(str(p.relative_to(bundle)) for p in bundle.rglob("*") if p.is_file()), sorted(expected))
            for relative in expected:
                if not relative.startswith("bin/"):
                    self.assertEqual((bundle / relative).read_bytes(), (SOURCE / relative).read_bytes())
            imported = subprocess.run([sys.executable, "-B", "-c",
                                       "import sys; sys.path.insert(0, sys.argv[1]); import adversarial_review",
                                       str(bundle / "harnesses")], cwd=root, capture_output=True, text=True, timeout=10)
            self.assertEqual(imported.returncode, 0, imported.stderr)
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
            for example in ("operator-github", "operator-github-stages"):
                with self.subTest(example=example):
                    github_queue = root / (example + "-must-not-accept")
                    github_inactive = subprocess.run(
                        [str(bundle / "bin/ticket-engine"), "--config", str(bundle / "examples" / (example + ".json")),
                         "--watch", "--run-dir", str(github_queue)], cwd=root,
                        capture_output=True, text=True, timeout=10,
                    )
                    self.assertNotEqual(github_inactive.returncode, 0)
                    self.assertIn("intake.created_since", github_inactive.stderr)
                    self.assertFalse(github_queue.exists())


if __name__ == "__main__":
    unittest.main()
