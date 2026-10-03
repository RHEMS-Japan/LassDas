"""CI entrypoints exercised with synthetic settings and Dockerfiles only."""
import os
from pathlib import Path
import subprocess
import tempfile
import textwrap
import unittest

ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / ".github/scripts/image-go-version.sh"
WORKFLOW = ROOT / ".github/workflows/ci.yml"


@unittest.skipUnless(SCRIPT.is_file() and WORKFLOW.is_file(), "requires the repository's CI scripts")
class CIDiagnosticsTests(unittest.TestCase):
    def read_version(self, text):
        with tempfile.TemporaryDirectory(prefix="ci-version-") as directory:
            dockerfile = Path(directory) / "Dockerfile"
            dockerfile.write_text(text)
            return subprocess.run(["bash", str(SCRIPT), str(dockerfile)], capture_output=True, text=True,
                                  env={"PATH": os.environ["PATH"]}, timeout=10)

    def test_literal_markers_do_not_start_a_heredoc(self):
        for instruction in (
            "RUN echo '<<WORD'", 'RUN echo "<<WORD"', r"RUN echo \<\<WORD",
            "RUN echo 'example <<WORD inside text'", 'RUN ["printf", "<<WORD"]',
            'COPY ["literal-<<WORD", "/file"]',
        ):
            with self.subTest(instruction=instruction):
                result = self.read_version("FROM golang:1.26.8\n" + instruction + "\nFROM golang:1.26.8 AS later\n")
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout, "1.26.8\n")

    def test_a_literal_marker_cannot_hide_a_real_stage(self):
        result = self.read_version("FROM golang:1.26.8\nRUN echo '<<WORD'\nFROM golang:1.25.0\n"
                                   "RUN <<WORD\necho a real heredoc\nWORD\n")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("different Go versions", result.stderr)

    def test_real_heredocs_skip_only_their_own_bodies(self):
        for word, terminator in (("<<WORD", "WORD"), ("<<'WORD'", "WORD"), ('<<"WORD"', "WORD"),
                                 ("3<<-WORD", "\tWORD")):
            with self.subTest(word=word):
                result = self.read_version("FROM golang:1.26.8\nRUN " + word + "\nFROM golang:0.0.0\n"
                                           + terminator + "\nFROM golang:1.26.8\n")
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout, "1.26.8\n")
        result = self.read_version("FROM golang:1.26.8\nCOPY <<'FIRST' <<-SECOND /out/\n"
                                   "FROM golang:0.0.0\nFIRST\nFROM golang:0.0.1\n\tSECOND\n")
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_unknown_or_unfinished_syntax_is_not_silently_skipped(self):
        for instruction, reason in (("RUN <<'WORD", "unfinished quoting"),
                                    ("RUN <<WORD.txt", "unsupported heredoc delimiter"),
                                    ('RUN <<W"OR"D', "unsupported heredoc delimiter"),
                                    ("RUN <<", "unsupported heredoc delimiter"),
                                    ("RUN <<WORD", "never ends")):
            with self.subTest(instruction=instruction):
                result = self.read_version("FROM golang:1.26.8\n" + instruction + "\n")
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(reason, result.stderr)

    def test_from_variants_and_mismatches_still_fail_or_read_explicitly(self):
        for dockerfile, ok in (
            ("from --platform=linux/arm64 registry.example/library/golang:1.26.8-bookworm AS build\n", True),
            ("FROM \\\n  golang:1.26.8\n", True),
            ("FROM golang:latest\n", False),
            ("FROM ${IMAGE}\n", False),
            ("# escape=`\nFROM golang:1.26.8\n", False),
            ("FROM golang:1.26.8\nFROM golang:1.25.0\n", False),
        ):
            with self.subTest(dockerfile=dockerfile):
                result = self.read_version(dockerfile)
                self.assertEqual(result.returncode == 0, ok, (result.stdout, result.stderr))

    def test_ci_says_the_actual_nonrequired_value_without_interpreting_it(self):
        section = WORKFLOW.read_text().split("- name: State whether the purity scan can run\n", 1)[1]
        section = section.split("\n      - name:", 1)[0]
        script = textwrap.dedent(section.split("        run: |\n", 1)[1])
        for value, shown in (("", "''"), ("false", "false"), ("FALSE", "FALSE"), ("typo", "typo"),
                             ("value\n::warning::injected", "$'value\\n::warning::injected'")):
            with self.subTest(value=value):
                result = subprocess.run(["bash", "-euo", "pipefail", "-c", script], capture_output=True, text=True,
                                        env={"PATH": os.environ["PATH"], "REQUIRED": value,
                                             "GITHUB_EVENT_NAME": "push", "GITHUB_REPOSITORY": "example/project",
                                             "HEAD_REPOSITORY": "", "ENGINE_PURITY_TOKENS": ""}, timeout=10)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn("actual value: " + shown, result.stdout)
                self.assertNotIn("\n::warning::injected", result.stdout)
