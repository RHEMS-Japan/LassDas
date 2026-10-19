"""Section selection is independent of Git and service fixtures."""
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import delivery_support as support
import deliver_git
import adversarial_review


class SectionTests(unittest.TestCase):
    def test_heading_variants_fences_and_setext_boundaries(self):
        for heading in ("## Pull request summary", "   ## pull REQUEST summary: ##", "Pull request summary\n---"):
            for fence in ("```", "~~~~"):
                with self.subTest(heading=heading, fence=fence):
                    text = heading + "\nKeep this\n" + fence + "\n# shell comment\n" + fence + "\nMore steps\n\nNext section\n---\nPrivate text"
                    self.assertEqual(support.markdown_section(text, " Pull request summary "),
                                     "Keep this\n" + fence + "\n# shell comment\n" + fence + "\nMore steps")
        self.assertIsNone(support.markdown_section("```\n## Pull request summary\nexample\n```", "Pull request summary"))
        self.assertEqual(support.markdown_section("## Pull request summary\n## Next\nprivate", "Pull request summary"), "")

    def test_uncertain_fence_boundaries_do_not_publish_private_notes(self):
        for fence in ("\x60\x60\x60\ncode", "\x60\x60\x60\x60\ncode\n\x60\x60\x60",
                      "\x60\x60\x60\ncode\n    \x60\x60\x60", "\x60\x60\x60\ncode\n~~~"):
            with self.subTest(fence=fence):
                text = "## Summary\nVerified\n" + fence + "\n## Private\nprivate notes"
                self.assertIsNone(support.markdown_section(text, "Summary"))
        text = "## Summary\n\x60\x60\x60inline\x60\x60\x60 text\n## Private\nprivate notes"
        self.assertEqual(support.markdown_section(text, "Summary"), "\x60\x60\x60inline\x60\x60\x60 text")

    def test_section_levels_and_paragraph_boundaries_preserve_limits(self):
        for line in ("- Unverified: live display", "* Unverified: live display",
                     "+ Unverified: live display", "1. Unverified: live display",
                     "2) Unverified: live display", "> Unverified: live display",
                     "| Unverified | live display |", "<div>Unverified</div>"):
            with self.subTest(line=line):
                text = "## Summary\n" + line + "\n---\n\n## Private\nprivate notes"
                self.assertEqual(support.markdown_section(text, "Summary"), line + "\n---")
        self.assertEqual(support.markdown_section(
            "## Summary\nVisible\n\nTwo-line private\nheading\n---\nprivate notes", "Summary"), "Visible")
        self.assertEqual(support.markdown_section(
            "## Summary\nVisible\n### Limits\nUnverified\n# Private\nprivate notes", "Summary"),
            "Visible\n### Limits\nUnverified")
        self.assertEqual(support.markdown_section(
            "## Summary\n    indented code\n## Private\nprivate notes", "Summary"), "    indented code")
        self.assertEqual(support.markdown_section(
            "# Overview\nprivate preamble\n## Summary\nVisible\n# Private\nprivate notes", "Summary"), "Visible")

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

    def test_failed_latest_attempt_does_not_reach_back_across_other_roles(self):
        with tempfile.TemporaryDirectory() as directory, patch.dict(os.environ, {
                "TASK_HISTORY": str(Path(directory) / "history.json"), "TASK_ISSUE": "TICKET-7",
                "PR_DESCRIPTION_ROLE": "confirm", "PR_DESCRIPTION_SECTION": "Summary"}, clear=True):
            history = [{"role": "confirm", "speaker": "reader", "output": "## Summary\nOld instructions"},
                       {"role": "deliver", "speaker": "publisher", "error": "failed"},
                       {"role": "work", "speaker": "writer", "output": "New work"},
                       {"role": "review", "speaker": "reviewer", "output": "New review"},
                       {"role": "confirm", "speaker": "reader", "error": "failed", "output": "## Summary\nFailed attempt"},
                       {"role": "confirm", "speaker": "runtime", "output": "attempt ended"},
                       {"role": "request", "speaker": "requester", "output": "Continue"}]
            Path(os.environ["TASK_HISTORY"]).write_text(json.dumps({"history": history}))
            self.assertEqual(support.description_report(), ("", "", True))

    def test_forbidden_text_names_its_actual_source(self):
        with tempfile.TemporaryDirectory() as directory, patch.dict(os.environ, {
                "TASK_HISTORY": str(Path(directory) / "history.json"), "TASK_ISSUE": "TICKET-7",
                "PR_DESCRIPTION_ROLE": "confirm", "PR_DESCRIPTION_SECTION": " Summary ",
                "GITHUB_TOKEN": "fixture-only-credential"}, clear=True):
            for report, forbidden, label in (("## Summary\nprivate-label", "private-label", "section 'Summary' of role confirm"),
                                               ("No section", "Prepared by", "introduction"),
                                               ("## Summary\nSafe text", "Prepared by", "introduction")):
                with self.subTest(report=report, label=label):
                    Path(os.environ["TASK_HISTORY"]).write_text(json.dumps({"history": [
                        {"role": "confirm", "speaker": "reader", "output": report}]}))
                    os.environ["DELIVERY_FORBIDDEN_TEXT"] = forbidden
                    with self.assertRaisesRegex(support.DeliveryError, label) as failure:
                        deliver_git.pull_request_body("TICKET-7", "none")
                    if label == "introduction":
                        self.assertNotIn("section", str(failure.exception))
            os.environ.pop("DELIVERY_FORBIDDEN_TEXT")
            os.environ["GITHUB_TOKEN"] = "not a result.\n\n## Summary"
            with self.assertRaisesRegex(support.DeliveryError, "delivery credential"):
                deliver_git.pull_request_body("TICKET-7", "merge")

    def test_review_holds_instead_of_silently_discarding_section_absence(self):
        with patch.dict(os.environ, {"PR_DESCRIPTION_SECTION": "Summary"}, clear=True):
            with self.assertRaisesRegex(support.DescriptionSettingError, "delivery-only"):
                adversarial_review.prepare()

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
        for settings in ({"PR_DESCRIPTION_SECTION": "Pull request summary"},
                         {"PR_DESCRIPTION_ROLE": "work", "PR_DESCRIPTION_SECTION": "  "}):
            with self.subTest(settings=settings), patch.dict(os.environ, settings, clear=True):
                with self.assertRaises(support.DescriptionSettingError):
                    deliver_git.pull_request_body("TICKET-7", "none")
