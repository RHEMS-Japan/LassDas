"""Run the guide's Go example locally; no cluster, credentials or dependencies."""
import os
from pathlib import Path
import re
import shlex
import subprocess
import tempfile
import unittest


SOURCE = Path(__file__).resolve().parents[2]
TEMPLATE = SOURCE / "deploy/ticket-engine/operator-scripts-configmap.yaml.example"


class SetupBuildPreflightTests(unittest.TestCase):
    def test_documented_vendor_command_uses_an_old_modules_prepared_private_dependency(self):
        guide = (TEMPLATE.parent / "SETUP.md").read_text()
        match = re.search(r"`(GOTOOLCHAIN=local GOMAXPROCS=2 GOFLAGS='[^']+' GOPROXY=off go build [^`]+)`", guide)
        self.assertIsNotNone(match, "the offline vendor build example disappeared")
        with tempfile.TemporaryDirectory(prefix="setup-vendor-example-") as temporary:
            checkout = Path(temporary)
            (checkout / "go.mod").write_text("module example.invalid/readcheck\n\ngo 1.13\n\nrequire example.invalid/shared v0.0.0\n")
            (checkout / "main.go").write_text('package main\nimport "example.invalid/shared"\nfunc main() { _ = shared.Value }\n')
            dependency = checkout / "vendor/example.invalid/shared/shared.go"
            dependency.parent.mkdir(parents=True)
            dependency.write_text("package shared\nconst Value = 1\n")
            (checkout / "vendor/modules.txt").write_text("# example.invalid/shared v0.0.0\nexample.invalid/shared\n")
            before = {p.relative_to(checkout): p.read_bytes() for p in checkout.rglob("*") if p.is_file()}
            environment = dict(os.environ, GOMAXPROCS="2", GOSUMDB="off", GOWORK="off")
            command = ["env", *shlex.split(match.group(1))]
            result = subprocess.run(command, cwd=checkout, env=environment,
                                    capture_output=True, text=True, timeout=60)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(before, {p.relative_to(checkout): p.read_bytes() for p in checkout.rglob("*") if p.is_file()})
            dependency.unlink()
            missing = subprocess.run(command, cwd=checkout, env=environment,
                                     capture_output=True, text=True, timeout=20)
            self.assertNotEqual(missing.returncode, 0, "missing vendored dependency was not checked")

    def test_documented_build_handles_one_main_package_without_writing_the_checkout(self):
        match = re.search(r"For a Go module the body can be as short as: (.+)", TEMPLATE.read_text())
        self.assertIsNotNone(match, "the documented build example disappeared")
        command = shlex.split(match.group(1))
        with tempfile.TemporaryDirectory(prefix="setup-build-example-") as temporary:
            root = Path(temporary)
            checkout = root / "checkout"
            checkout.mkdir()
            (checkout / "go.mod").write_text("module example.invalid/readcheck\n\ngo 1.18\n")
            source = checkout / "main.go"
            source.write_text("package main\nfunc main() {}\n")
            before = {p.name: p.read_bytes() for p in checkout.iterdir()}
            environment = dict(os.environ, GOMAXPROCS="2", GOFLAGS="-p=1 -mod=readonly",
                               GOTOOLCHAIN="local", GOPROXY="off", GOSUMDB="off", GOWORK="off")
            for path in checkout.iterdir():
                path.chmod(0o444)
            checkout.chmod(0o555)
            try:
                result = subprocess.run(command, cwd=checkout, env=environment,
                                        capture_output=True, text=True, timeout=60)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(before, {p.name: p.read_bytes() for p in checkout.iterdir()})
            finally:
                checkout.chmod(0o755)
                for path in checkout.iterdir():
                    path.chmod(0o644)
            # It must still compile and reject invalid source, not merely exit 0.
            source.write_text("this is not Go source\n")
            invalid = subprocess.run(command, cwd=checkout, env=environment,
                                     capture_output=True, text=True, timeout=20)
            self.assertNotEqual(invalid.returncode, 0, "the guide's command did not build anything")


if __name__ == "__main__":
    unittest.main()
