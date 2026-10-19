"""Section selection is independent of Git and service fixtures."""
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import delivery_support as support
import deliver_git


class SectionTests(unittest.TestCase):
    def test_heading_variants_fences_and_setext_boundaries(self):
        for heading in ("## Pull request summary", "   ## pull REQUEST summary: ##", "Pull request summary\n---"):
            for fence in ("```", "~~~~"):
                with self.subTest(heading=heading, fence=fence):
                    text = heading + "\nKeep this\n" + fence + "\n# shell comment\n" + fence + "\nMore steps\nNext section\n---\nPrivate text"
                    self.assertEqual(support.markdown_section(text, " Pull request summary "),
                                     "Keep this\n" + fence + "\n# shell comment\n" + fence + "\nMore steps")
        self.assertIsNone(support.markdown_section("```\n## Pull request summary\nexample\n```", "Pull request summary"))
        self.assertEqual(support.markdown_section("## Pull request summary\n## Next\nprivate", "Pull request summary"), "")

    def test_latest_nonfailed_record_is_never_joined_to_an_earlier_report(self):
        with tempfile.TemporaryDirectory() as directory, patch.dict(os.environ, {
                "TASK_HISTORY": str(Path(directory) / "history.json"), "TASK_ISSUE": "TICKET-7",
                "PR_DESCRIPTION_ROLE": "confirm_change", "PR_DESCRIPTION_SECTION": "Pull request summary",
                "PR_DESCRIPTION_MAX_BYTES": "1000", "DELIVERY_MERGE_METHOD": "none", "GITHUB_TOKEN": "fixture-only-credential"}, clear=True):
            for latest in ("private preamble\n## Pull request summary\nNew instructions", "No summary in this attempt", ""):
                with self.subTest(latest=latest):
                    history = [{"role": "confirm_change", "speaker": "reader", "output": "## Pull request summary\nOld instructions"},
                               {"role": "confirm_change", "speaker": "runtime", "output": "launch ended"},
                               {"role": "confirm_change", "speaker": "reader", "output": latest},
                               {"role": "confirm_change", "speaker": "reader", "error": "failed", "output": "## Pull request summary\nFailed attempt"}]
                    Path(os.environ["TASK_HISTORY"]).write_text(json.dumps({"history": history}))
                    body, note, absent = deliver_git.pull_request_body("TICKET-7", "none")
                    self.assertNotIn("Old instructions", body)
                    self.assertNotIn("private preamble", body)
                    self.assertNotIn("Failed attempt", body)
                    self.assertEqual(absent, "New instructions" not in latest)
                    self.assertEqual(note, "")
                    if not absent:
                        self.assertTrue(body.endswith("## Pull request summary\n\nNew instructions"))

    def test_only_the_section_is_sized_and_scanned(self):
        with tempfile.TemporaryDirectory() as directory, patch.dict(os.environ, {
                "TASK_HISTORY": str(Path(directory) / "history.json"), "TASK_ISSUE": "TICKET-7",
                "PR_DESCRIPTION_ROLE": "confirm_change", "PR_DESCRIPTION_SECTION": "Pull request summary",
                "PR_DESCRIPTION_MAX_BYTES": "1000", "DELIVERY_FORBIDDEN_TEXT": "private-label", "GITHUB_TOKEN": "fixture-only-credential"}, clear=True):
            text = "private-label" * 2000 + "\n## Pull request summary\nSafe instructions\n## Private\nprivate-label"
            Path(os.environ["TASK_HISTORY"]).write_text(json.dumps({"history": [
                {"role": "confirm_change", "speaker": "reader", "output": text}]}))
            body, _, absent = deliver_git.pull_request_body("TICKET-7", "none")
            self.assertFalse(absent)
            self.assertNotIn("private-label", body)
            self.assertIn("Safe instructions", body)

    def test_section_without_role_is_an_operator_error(self):
        with patch.dict(os.environ, {"PR_DESCRIPTION_SECTION": "Pull request summary"}, clear=True):
            with self.assertRaises(support.DescriptionSettingError):
                deliver_git.pull_request_body("TICKET-7", "none")
