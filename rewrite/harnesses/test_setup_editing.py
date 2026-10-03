"""Exercise the documented local JSON edit; never contact a cluster."""
import json
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import unittest

SOURCE = Path(__file__).resolve().parents[2]
GUIDE = SOURCE / "deploy/ticket-engine/SETUP.md"
EXAMPLE = SOURCE / "rewrite/examples/operator-stages.json"


class SetupEditingTests(unittest.TestCase):
    def guidance_script(self):
        match = re.search(r"^# setup-guidance-edit\n(.*?)^PY$", GUIDE.read_text(), re.M | re.S)
        self.assertIsNotNone(match, "the documented guidance editor is missing")
        return match.group(1)

    def fixture(self, root):
        config = json.loads(EXAMPLE.read_text())
        source, target, prose = root / "source.json", root / "new.json", root / "guidance.txt"
        source.write_text(json.dumps(config))
        prose.write_text('日本語の説明。\nUse "quoted text", a \\literal path, and more than one line.\n')
        return config, source, target, prose

    def edit(self, source, target, prose):
        return subprocess.run([sys.executable, "-B", "-c", self.guidance_script(), str(source), str(target), str(prose)],
                              capture_output=True, text=True, timeout=10)

    def test_guidance_preserves_json_and_shared_instructions_without_modifying_the_source(self):
        with tempfile.TemporaryDirectory(prefix="setup-guidance-") as temporary:
            config, source, target, prose = self.fixture(Path(temporary))
            before = source.read_bytes()
            result = self.edit(source, target, prose)
            self.assertEqual(result.returncode, 0, result.stderr)
            written = json.loads(target.read_text())
            expected = dict(config)
            expected["instructions"] = prose.read_text().strip() + "\n\n" + config["instructions"].partition(". ")[2]
            self.assertEqual(written, expected)
            self.assertEqual(source.read_bytes(), before)
            self.assertNotIn("Operator setup is incomplete:", written["instructions"])
            self.assertIn("Keep the original request", written["instructions"])

    def test_existing_target_empty_guidance_and_custom_instructions_are_not_overwritten(self):
        with tempfile.TemporaryDirectory(prefix="setup-no-overwrite-") as temporary:
            config, source, target, prose = self.fixture(Path(temporary))
            target.write_text("an existing file must stay\n")
            result = self.edit(source, target, prose)
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(target.read_text(), "an existing file must stay\n")
            target.unlink()
            prose.write_text(" \n\t")
            self.assertNotEqual(self.edit(source, target, prose).returncode, 0)
            self.assertFalse(target.exists())
            prose.write_text("new guidance")
            config["instructions"] = "Already approved project guidance. Keep it."
            source.write_text(json.dumps(config))
            before = source.read_bytes()
            self.assertNotEqual(self.edit(source, target, prose).returncode, 0)
            self.assertFalse(target.exists())
            self.assertEqual(source.read_bytes(), before)

    def test_documented_syntax_check_distinguishes_bad_json_from_unfinished_settings(self):
        self.assertIn('python3 -m json.tool "$CONFIG" > /dev/null', GUIDE.read_text())
        with tempfile.TemporaryDirectory(prefix="setup-syntax-") as temporary:
            path = Path(temporary) / "operator.json"
            for text in ('{"project_id": <project-id>}', '{"instructions": "line\ninside quotes"}'):
                path.write_text(text)
                result = subprocess.run([sys.executable, "-B", "-m", "json.tool", str(path)],
                                        capture_output=True, text=True, timeout=10)
                self.assertNotEqual(result.returncode, 0)
                self.assertRegex(result.stderr, r"line \d+ column \d+")
            path.write_text(EXAMPLE.read_text())
            unfinished = subprocess.run([sys.executable, "-B", "-m", "json.tool", str(path)],
                                        capture_output=True, text=True, timeout=10)
            self.assertEqual(unfinished.returncode, 0, "syntax checks must not be sold as completed setup")


if __name__ == "__main__":
    unittest.main()
