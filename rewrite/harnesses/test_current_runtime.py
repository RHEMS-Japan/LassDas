"""Check the shipped source/image entrypoints, without a live service."""
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import textwrap
import unittest
from urllib.parse import unquote, urlsplit

ROOT = Path(__file__).resolve().parents[2]


class CurrentRuntimeTests(unittest.TestCase):
    def test_source_and_builds_do_not_depend_on_the_removed_runtime(self):
        retired = ("cmd", "internal", "config", "knowledge", "templates",
                   "scripts", "setup.sh", "go.mod", "go.sum",
                   ".github/workflows/m1-worker.yml",
                   ".github/workflows/agent-rehearsal.yml",
                   "deploy/pod/entrypoint.sh", "deploy/pod/release.sh")
        remaining = []
        for name in retired:
            path = ROOT / name
            if path.is_file() or (path.is_dir() and any(p.is_file() for p in path.rglob("*"))):
                remaining.append(name)
        self.assertEqual(remaining, [])
        dockerfile = (ROOT / "deploy/pod/Dockerfile").read_text()
        entry = re.findall(r"^ENTRYPOINT (.+)$", dockerfile, re.M)
        self.assertEqual([json.loads(value) for value in entry],
                         [["/opt/ticket-automation/bundle/bin/ticket-engine"]])
        self.assertIn("COPY --from=bundle /usr/local/go /usr/local/go", dockerfile)
        self.assertNotIn("AS engine", dockerfile)
        self.assertNotIn("agentexec", dockerfile)
        self.assertNotIn("useradd --create-home --uid 2000", dockerfile)
        for name in ("ci.yml", "image.yml"):
            workflow = (ROOT / ".github/workflows" / name).read_text()
            self.assertNotIn("go-version-file: go.mod", workflow)
            self.assertNotIn("./internal/runtime", workflow)
            self.assertNotIn("./cmd/lassdas", workflow)
        self.assertIn("bash .github/scripts/check-packaged-engine.sh", (
            ROOT / ".github/workflows/image.yml").read_text())
        published = (ROOT / ".github/workflows/image.yml").read_text()
        rust = published.index("bash .github/scripts/check-rust-toolchain.sh")
        self.assertLess(published.index("docker build --platform linux/arm64"), rust)
        self.assertLess(rust, published.index('docker push "$tag"'))
        self.assertIn("bash .github/scripts/check-rust-toolchain.sh", (
            ROOT / ".github/workflows/image-check.yml").read_text())
        sandbox = published.index("bash .github/scripts/check-role-sandbox.sh")
        self.assertLess(rust, sandbox)
        self.assertLess(sandbox, published.index('docker push "$tag"'))
        self.assertIn("bash .github/scripts/check-role-sandbox.sh", (
            ROOT / ".github/workflows/image-check.yml").read_text())

    def test_distribution_note_uses_this_build_without_the_removed_cli(self):
        workflow = (ROOT / ".github/workflows/image.yml").read_text()
        block = workflow.split("- name: Write the distributor's note\n", 1)[1]
        block = block.split("\n      - name:", 1)[0]
        command = textwrap.dedent(block.split("        run: |\n", 1)[1])
        env = {"PATH": os.environ["PATH"], "GITHUB_REPOSITORY": "example/project",
               "IMAGE_DIGEST": "registry.example/project/runtime@sha256:" + "a" * 64,
               "GITHUB_SHA": "b" * 40, "GITHUB_SERVER_URL": "https://github.com",
               "GITHUB_RUN_ID": "42"}
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "docs").mkdir()
            target = root / "docs/DISTRIBUTION.json"
            result = subprocess.run(["bash", "-euo", "pipefail", "-c", command],
                                    cwd=root, env=env, capture_output=True, text=True, timeout=10)
            self.assertEqual(result.returncode, 0, result.stderr)
            expected = {"engine_repository": "example/project", "image": env["IMAGE_DIGEST"],
                        "engine_sha": env["GITHUB_SHA"],
                        "build_record": "https://github.com/example/project/actions/runs/42"}
            self.assertEqual(target.read_text(), json.dumps(expected, indent=2) + "\n")
            before = target.read_bytes()
            del env["IMAGE_DIGEST"]
            failed = subprocess.run(["bash", "-euo", "pipefail", "-c", command],
                                    cwd=root, env=env, capture_output=True, text=True, timeout=10)
            self.assertNotEqual(failed.returncode, 0)
            self.assertEqual(target.read_bytes(), before)

    def test_image_check_exercises_the_default_entrypoint(self):
        version = subprocess.check_output(
            ["bash", str(ROOT / ".github/scripts/image-go-version.sh")], text=True).strip()
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            docker = root / "docker"
            docker.write_text("""#!/bin/sh
printf '%s\\n' "$*" >> "$CALLS"
case "$*" in
  *"--entrypoint /usr/local/go/bin/go"*) printf '/opt/ticket-automation/bundle/bin/ticket-engine: go%s\\n' "$NAMED" ;;
  *"--entrypoint"*) exit 9 ;;
  *) echo 'watch requires an explicit intake.project_id'; exit 1 ;;
esac
""")
            docker.chmod(0o700)
            calls = root / "calls"
            result = subprocess.run(
                ["bash", str(ROOT / ".github/scripts/check-packaged-engine.sh"), "fixture-image"],
                env={"PATH": str(root) + os.pathsep + os.environ["PATH"],
                     "CALLS": str(calls), "NAMED": version},
                capture_output=True, text=True, timeout=10)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            rows = calls.read_text().splitlines()
            self.assertEqual(len(rows), 2)
            self.assertNotIn("--entrypoint", rows[0])

    def test_remaining_document_links_resolve(self):
        missing = []
        for path in ROOT.rglob("*.md"):
            for target in re.findall(r"\[[^\]\n]*\]\(([^\s)]+)\)", path.read_text()):
                parsed = urlsplit(target)
                if parsed.scheme or parsed.netloc or not parsed.path:
                    continue
                if not (path.parent / unquote(parsed.path)).exists():
                    missing.append(str(path.relative_to(ROOT)) + ": " + target)
        self.assertEqual(missing, [])


if __name__ == "__main__":
    unittest.main()
