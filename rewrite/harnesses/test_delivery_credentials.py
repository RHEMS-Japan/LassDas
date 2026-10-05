"""Ask the real Git credential machinery without contacting any server."""
import importlib.util
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

SUPPORT = Path(__file__).with_name("delivery_support.py").resolve()
SPEC = importlib.util.spec_from_file_location("credential_support", SUPPORT)
support = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(support)
TOKEN = "synthetic-fixture-key"


@unittest.skipUnless(shutil.which("git"), "requires Git")
class CredentialPortTests(unittest.TestCase):
    def ask(self, configured, requested):
        # Git credential fill invokes the configured helper but no transport.
        # It runs outside a repository and without any personal config/key.
        environment = {"PATH": os.environ["PATH"], "GITHUB_TOKEN": TOKEN,
                       "GIT_TERMINAL_PROMPT": "0"}
        with patch.dict(os.environ, environment, clear=True), tempfile.TemporaryDirectory() as directory:
            command = support.git("credential", "fill", url=configured)
            self.assertNotIn(TOKEN, " ".join(command))
            return subprocess.run(command, input="url=" + requested + "\n\n", text=True,
                                  capture_output=True, env=support.git_environment(),
                                  cwd=directory, timeout=5)

    def test_git_can_get_the_key_for_the_configured_authority(self):
        for url in ("https://git.example/owner/project.git",
                    "https://git.example:8443/owner/project.git",
                    "https://git.example:443/owner/project.git",
                    "https://Git.Example:8443/owner/project.git"):
            with self.subTest(url=url):
                result = self.ask(url, url)
                self.assertEqual(result.returncode, 0, result.stderr)
                fields = dict(line.split("=", 1) for line in result.stdout.splitlines() if "=" in line)
                self.assertEqual(fields.get("password"), TOKEN)
                self.assertNotIn(TOKEN, result.stderr)

    def test_a_different_port_host_or_protocol_gets_no_key(self):
        configured = "https://git.example:8443/owner/project.git"
        for requested in ("https://git.example/owner/project.git",
                          "https://git.example:443/owner/project.git",
                          "https://git.example:9443/owner/project.git",
                          "https://other.example:8443/owner/project.git",
                          "http://git.example:8443/owner/project.git"):
            with self.subTest(requested=requested):
                result = self.ask(configured, requested)
                self.assertNotEqual(result.returncode, 0)
                self.assertNotIn(TOKEN, result.stdout + result.stderr)
                self.assertNotIn("password=", result.stdout)

    def test_store_and_erase_do_not_emit_the_key(self):
        for operation in ("store", "erase"):
            result = subprocess.run([sys.executable, "-B", str(SUPPORT), "--credential-helper", operation],
                                    input="protocol=https\nhost=git.example:8443\n\n",
                                    text=True, capture_output=True, timeout=5,
                                    env={"PATH": os.environ["PATH"], "GITHUB_TOKEN": TOKEN,
                                         "DELIVERY_CREDENTIAL_HOST": "git.example:8443"})
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(result.stdout, "")
            self.assertNotIn(TOKEN, result.stderr)


if __name__ == "__main__":
    unittest.main()
