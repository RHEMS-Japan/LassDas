"""Check the glossary scan on the documents and on altered copies of them.

The altered copies pick their words and lines from the glossary as it is, so
these tests keep working when its line numbers are refreshed and when the
rewording empties its waiting list."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest

HARNESSES = Path(__file__).resolve().parent
ROOT = HARNESSES.parents[1]
SCRIPT = HARNESSES / "glossary_scan.py"
COPIED = ("CLAUDE.md", "rewrite/GLOSSARY.md", "rewrite/README.md", "rewrite/START.md",
          "rewrite/RUNTIME.md")
sys.path.insert(0, str(HARNESSES))
import glossary_scan  # noqa: E402


def scan(root, *files):
    return subprocess.run([sys.executable, "-B", str(SCRIPT), "--root", str(root), *map(str, files)],
                          capture_output=True, text=True, encoding="utf-8", timeout=60,
                          env=dict(os.environ, PYTHONIOENCODING="utf-8"))


def line_holding(path, text):
    for number, line in enumerate(path.read_text(encoding="utf-8").split("\n"), 1):
        if text in line:
            return number
    raise AssertionError("%s does not hold %r" % (path, text))


def waiting_words():
    _, _, waiting, _ = glossary_scan.read_glossary(ROOT / glossary_scan.GLOSSARY)
    return [word for word, _ in waiting]


class GlossaryScanTests(unittest.TestCase):
    def copy(self, temporary):
        root = Path(temporary) / "repository"
        for relative in COPIED:
            (root / relative).parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(ROOT / relative, root / relative)
        shutil.copytree(ROOT / "rewrite/examples", root / "rewrite/examples")
        return root

    def insert_after(self, path, anchor, line):
        lines = path.read_text(encoding="utf-8").split("\n")
        self.assertIn(anchor, lines)
        lines.insert(lines.index(anchor) + 1, line)
        path.write_text("\n".join(lines), encoding="utf-8")

    def assertScan(self, result, status, *lines):
        output = result.stdout + result.stderr
        self.assertEqual(result.returncode, status, output)
        for line in lines:
            self.assertIn(line, output)

    def test_the_documents_use_no_word_to_avoid_outside_the_waiting_list(self):
        result = scan(ROOT)
        self.assertScan(result, 0, "0 found outside the waiting list")
        self.assertTrue(result.stdout.endswith("result: pass\n"), result.stdout)

    def test_a_further_file_with_a_word_to_avoid_fails(self):
        with tempfile.TemporaryDirectory(prefix="glossary-scan-") as temporary:
            note = Path(temporary) / "note.md"
            note.write_text("The first line.\nこのエンジンは止まらない。\n", encoding="utf-8")
            self.assertScan(scan(ROOT, note), 1,
                            "%s:2: エンジン is a word to avoid (engine / 本体)" % note, "result: fail")
            note.write_text("The first line.\nこの本体は止まらない。\n", encoding="utf-8")
            self.assertScan(scan(ROOT, note), 0, "result: pass")

    def test_a_waiting_word_in_a_further_file_still_fails(self):
        if not waiting_words():
            self.skipTest("no word is waiting for rewording")
        word = waiting_words()[0]
        with tempfile.TemporaryDirectory(prefix="glossary-scan-") as temporary:
            note = Path(temporary) / "note.md"
            note.write_text(word + "\n", encoding="utf-8")
            self.assertScan(scan(ROOT, note), 1, "%s:1: %s is a word to avoid" % (note, word))

    def test_words_match_as_the_glossary_says(self):
        cases = (
            ("A GATE decides.", 1), ("two review seats", 1), ("the fixture tickets", 1),
            ("the ticket\nbranch is pushed", 0), ("ticket-engine and ticket/7", 0),
            ("a gateway and a gated path", 0), ("the paragraph", 0),
            ("ゲートを通す", 1), ("ゲートウェイ経由", 0), ("担当者と担当チケット", 0),
            ("次の担当", 1), ("目印を付ける", 1), ("課題管理を選ぶ", 0), ("手段と段階", 0),
        )
        with tempfile.TemporaryDirectory(prefix="glossary-scan-") as temporary:
            for number, (text, status) in enumerate(cases):
                with self.subTest(text=text):
                    note = Path(temporary) / ("case%d.md" % number)
                    note.write_text(text + "\n", encoding="utf-8")
                    self.assertScan(scan(ROOT, note), status)

    def test_an_example_is_read_for_its_instructions_only(self):
        with tempfile.TemporaryDirectory(prefix="glossary-scan-") as temporary:
            root = self.copy(temporary)
            example = root / "rewrite/examples/operator.json"
            data = json.loads(example.read_text(encoding="utf-8"))
            process = data["roles"][0]["processes"][0]
            data["roles"][0]["name"] += " 片肺"
            example.write_text(json.dumps(data, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
            self.assertScan(scan(root), 0, "result: pass")
            process["instructions"] = "片肺 " + process["instructions"]
            example.write_text(json.dumps(data, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
            line = line_holding(example, '"instructions": "片肺 ')
            self.assertScan(scan(root), 1,
                            "examples/operator.json:%d: 片肺 is a word to avoid (Figurative words)" % line)

    def test_an_example_whose_instructions_cannot_be_placed_is_refused(self):
        with tempfile.TemporaryDirectory(prefix="glossary-scan-") as temporary:
            root = self.copy(temporary)
            example = root / "rewrite/examples/operator.json"
            data = json.loads(example.read_text(encoding="utf-8"))
            example.write_text(json.dumps(data, ensure_ascii=False), encoding="utf-8")
            self.assertScan(scan(root), 2, "examples/operator.json: an instructions text")

    def test_a_new_word_to_avoid_in_a_document_fails_while_the_waiting_words_pass(self):
        with tempfile.TemporaryDirectory(prefix="glossary-scan-") as temporary:
            root = self.copy(temporary)
            start = root / "rewrite/START.md"
            added = ["このエンジンは止まらない。"] + waiting_words()[:1]
            with start.open("a", encoding="utf-8") as out:
                out.write("\n" + "\n".join(added) + "\n")
            line = line_holding(start, "このエンジンは止まらない。")
            result = scan(root)
            self.assertScan(result, 1, "START.md:%d: エンジン is a word to avoid (engine / 本体)" % line)
            for word in waiting_words()[:1]:
                self.assertNotIn("%s is a word to avoid" % word, result.stdout)
                self.assertIn("waiting for rewording: %s at " % word, result.stdout)
                self.assertIn("START.md:", result.stdout.split("waiting for rewording: %s at " % word, 1)[1].split("\n", 1)[0])

    def test_a_waiting_word_that_no_longer_appears_fails(self):
        if not waiting_words():
            self.skipTest("no word is waiting for rewording")
        word = waiting_words()[0]
        with tempfile.TemporaryDirectory(prefix="glossary-scan-") as temporary:
            root = self.copy(temporary)
            matcher = glossary_scan.pattern(word)
            for path in [root / relative for relative in COPIED if relative != "rewrite/GLOSSARY.md"] + \
                    sorted((root / "rewrite/examples").glob("*.json")):
                path.write_text(matcher.sub("X", path.read_text(encoding="utf-8")), encoding="utf-8")
            line = line_holding(root / "rewrite/GLOSSARY.md", "- `%s`" % word)
            self.assertScan(scan(root), 1,
                            "GLOSSARY.md:%d: %s no longer appears; remove it from Waiting for rewording"
                            % (line, word))

    def test_every_word_of_an_entry_is_used_where_the_entry_says(self):
        with tempfile.TemporaryDirectory(prefix="glossary-scan-") as temporary:
            root = self.copy(temporary)
            glossary = root / "rewrite/GLOSSARY.md"
            self.insert_after(glossary, "## People and places",
                              "\n### unused word / 未使用の語\n\nNothing.\n\n- Used in: README.md:1\n- Avoid: none.")
            line = line_holding(glossary, "### unused word")
            self.assertScan(scan(root), 1,
                            "GLOSSARY.md:%d: unused word is not used in the documents" % line,
                            "GLOSSARY.md:%d: 未使用の語 is not used in the documents" % line,
                            "GLOSSARY.md:%d: README.md no longer uses unused word / 未使用の語" % line)

    def test_a_moved_word_is_noted_without_failing(self):
        entries, _, _, _ = glossary_scan.read_glossary(ROOT / glossary_scan.GLOSSARY)
        engine = next(entry for entry in entries if entry.english == "engine")
        name, used = next((name, line) for name, line in engine.used if name == "README.md")
        readme = (ROOT / "rewrite/README.md").read_text(encoding="utf-8").split("\n")
        moved = next(number for number in range(used + 1, len(readme) + 1)
                     if not glossary_scan.pattern("engine").search(readme[number - 1])
                     and "本体" not in readme[number - 1])
        with tempfile.TemporaryDirectory(prefix="glossary-scan-") as temporary:
            root = self.copy(temporary)
            glossary = root / "rewrite/GLOSSARY.md"
            text = glossary.read_text(encoding="utf-8")
            old = "- Used in: README.md:%d," % used
            self.assertIn(old, text)
            glossary.write_text(text.replace(old, "- Used in: README.md:%d," % moved, 1), encoding="utf-8")
            self.assertScan(scan(root), 0, "note: README.md:%d named for engine / 本体 does not use it now; "
                            "the nearest use is README.md:" % moved, "result: pass")

    def test_the_glossary_text_is_read_for_words_to_avoid(self):
        with tempfile.TemporaryDirectory(prefix="glossary-scan-") as temporary:
            root = self.copy(temporary)
            glossary = root / "rewrite/GLOSSARY.md"
            self.insert_after(glossary, "# Glossary", "A gate opens here.")
            line = line_holding(glossary, "A gate opens here.")
            self.assertScan(scan(root), 1, "GLOSSARY.md:%d: the glossary itself uses gate" % line)

    def test_a_glossary_that_cannot_be_read_as_described_is_refused(self):
        for anchor, added, status, message in (
                ("### engine / 本体", "- Avoid: エンジン", 2, "cannot read the word to avoid"),
                ("### engine / 本体", "- Used in: README.md", 2, "is not FILE:LINE"),
                ("### engine / 本体", "- Used in: OPERATING.md:8", 2,
                 "OPERATING.md is not one of the documents the scan reads"),
                ("## Waiting for rewording", "- `not-a-word-to-avoid`", 1,
                 "not-a-word-to-avoid is waiting for rewording but is not a word to avoid"),
                ("### engine / 本体", "- Avoid: `本体`", 1,
                 "本体 is a word to avoid and part of the entry engine / 本体")):
            with self.subTest(added=added), tempfile.TemporaryDirectory(prefix="glossary-scan-") as temporary:
                root = self.copy(temporary)
                self.insert_after(root / "rewrite/GLOSSARY.md", anchor, added)
                self.assertScan(scan(root), status, message)


if __name__ == "__main__":
    unittest.main()
