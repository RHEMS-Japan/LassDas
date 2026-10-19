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
    def test_list_contained_headings_do_not_start_the_public_section(self):
        for marker in ("- ", "1. ", "2) "):
            indent = " " * len(marker)
            for heading in ("## Summary", "Summary\n" + indent + "---"):
                for blank in ("", "\n", "\n" + indent + "***\n", "\n" + indent + "---\n"):
                    with self.subTest(marker=marker, heading=heading, blank=blank):
                        text = marker + "Private example\n" + blank + indent + heading + "\n" + indent + "PRIVATE\n"
                        self.assertIsNone(support.markdown_section(text, "Summary"))
                        self.assertEqual(support.markdown_section(text + "\n## Summary\nVisible", "Summary"), "Visible")
        self.assertEqual(support.markdown_section("- Example\n\nSummary\n---\nVisible", "Summary"), "Visible")

    def test_html_blocks_cannot_supply_a_false_section_start(self):
        for opening, closing in (("<script>", "</script>"), ("<!--", "-->"),
                                 ("<pre>", "</pre>"), ("<?", "?>"),
                                 ("<![CDATA[", "]]>"), ("<!DECLARATION", ">"),
                                 ("<div>", ""), ("<custom attr='<foo>'>", ""),
                                 ('<custom title="a>b">', "")):
            with self.subTest(opening=opening):
                fake = opening + "\n## Summary\nPRIVATE_INSIDE_HTML\n" + closing + "\n"
                self.assertIsNone(support.markdown_section(fake, "Summary"))
                self.assertEqual(support.markdown_section(fake + "\n## Summary\nVisible", "Summary"), "Visible")
                self.assertIsNone(support.markdown_section(opening + "\n## Summary\nPRIVATE", "Summary"))

    def test_a_list_item_ending_before_its_fence_cannot_publish_private_text(self):
        for marker in ("- ", "2. "):
            for heading in ("   ## Private", "    ## Private", "\t## Private", "Private heading\n---"):
                with self.subTest(marker=marker, heading=heading):
                    text = "## Summary\nVisible\n" + marker + "```sh\ncode\n" + heading + "\nPRIVATE\n  ```\n"
                    self.assertNotIn("PRIVATE", support.markdown_section(text, "Summary") or "")

    def test_inline_html_and_pipe_paragraph_headings_do_not_leak(self):
        for heading in ("<b>Private heading</b>", "| Private heading |"):
            for blank in ("", "Visible\n\n"):
                with self.subTest(heading=heading, blank=blank):
                    text = "## Summary\n" + blank + heading + "\n---\nPRIVATE\n"
                    self.assertNotIn("PRIVATE", support.markdown_section(text, "Summary") or "")
            self.assertEqual(support.markdown_section(heading + "\n---\nVisible", heading), "Visible")

    def test_list_fences_never_hide_private_sections(self):
        for item in ("- ", "* ", "+ ", "1. ", "2) ", "123456789. "):
            for fence in ("```", "~~~~"):
                with self.subTest(item=item, fence=fence):
                    text = ("## Summary\nVisible\n" + item + fence + "sh\n"
                            + " " * len(item) + "echo example\n" + " " * len(item) + fence
                            + "\n## Private observations\nPRIVATE\n" + fence + "\n")
                    self.assertNotIn("PRIVATE", support.markdown_section(text, "Summary") or "")
        self.assertEqual(support.markdown_section(
            "## Summary\nVisible\n- ```sh\n  echo example\n  ```\n## Private\nPRIVATE", "Summary"),
            "Visible\n- ```sh\n  echo example\n  ```")

    def test_paragraph_continuations_and_interruptions(self):
        for continuation in ("    continuation", "\tcontinuation", "2) continuation", "2. continuation",
                             "<span>inline</span>", "<https://example.invalid>", "| inline text"):
            with self.subTest(continuation=continuation):
                text = "## Summary\nVisible\n\nPrivate heading\n" + continuation + "\n---\nPRIVATE"
                self.assertEqual(support.markdown_section(text, "Summary"), "Visible")
        for rule in ("***", "___", "* * *", "- - -"):
            with self.subTest(rule=rule):
                self.assertEqual(support.markdown_section("Preamble\n" + rule + "\nSummary\n---\nVisible", "Summary"), "Visible")
        for block in ("<!-- closed -->", "<?closed?>", "<!DOCTYPE html>", "<![CDATA[closed]]>",
                      "<script>closed</script>", "<div>block</div>"):
            with self.subTest(block=block):
                self.assertEqual(support.markdown_section("Preamble\n" + block + "\n\nSummary\n---\nVisible", "Summary"), "Visible")
        self.assertEqual(support.markdown_section(
            "## Summary\nVisible\n<!-- closed -->\nPrivate heading\n---\nPRIVATE", "Summary"),
            "Visible\n<!-- closed -->")
        for marker in ("- ", "1. ", "2) "):
            text = "## Summary\n" + marker + "Unverified\n  still pending\n---\n## Private\nPRIVATE"
            self.assertEqual(support.markdown_section(text, "Summary"), marker + "Unverified")
        self.assertEqual(support.markdown_section("## Summary\n    code\nPrivate heading\n---\nPRIVATE", "Summary"), "    code")
        self.assertEqual(support.markdown_section("Before\n```\ncode\n```\nSummary\n---\nVisible", "Summary"), "Visible")
        self.assertEqual(support.markdown_section("## Summary\nVisible\n### Summary\nMore\n## Private\nPRIVATE", "Summary"), "Visible")

    def test_only_markdown_line_endings_split_lines(self):
        for separator in ("\x85", "\u2028", "\x0b", "\x0c"):
            with self.subTest(separator=repr(separator)):
                text = "## Summary\nok" + separator + "```\n## Private\nPRIVATE\n```\n"
                self.assertEqual(support.markdown_section(text, "Summary"), "ok" + separator + "```")
        for ending in ("\r\n", "\r", "\n"):
            self.assertEqual(support.markdown_section(ending.join(("## Summary", "Visible", "## Private", "PRIVATE")), "Summary"), "Visible")

    def test_heading_variants_fences_and_setext_boundaries(self):
        for heading in ("## Pull request summary", "   ## pull REQUEST summary: ##", "Pull request summary\n---"):
            for fence in ("```", "~~~~"):
                with self.subTest(heading=heading, fence=fence):
                    text = heading + "\nKeep this\n" + fence + "\necho example\n" + fence + "\nMore steps\n\nNext section\n---\nPrivate text"
                    self.assertEqual(support.markdown_section(text, " Pull request summary "),
                                     "Keep this\n" + fence + "\necho example\n" + fence + "\nMore steps")
        self.assertIsNone(support.markdown_section("## Summary\n```\n# shell comment\n```\nPrivate", "Summary"))
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
                self.assertEqual(support.markdown_section(text, "Summary"), "")
        self.assertEqual(support.markdown_section(
            "## Summary\nVisible\n\nTwo-line private\nheading\n---\nprivate notes", "Summary"), "Visible")
        self.assertEqual(support.markdown_section(
            "## Summary\nVisible\n### Limits\nUnverified\n# Private\nprivate notes", "Summary"),
            "Visible")
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
                    self.assertEqual(bool(absent), "New instructions" not in latest)
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
            self.assertEqual(support.description_report(), ("", "", "absent_or_empty"))

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
                        self.assertIsInstance(failure.exception, support.DescriptionSettingError)
                        self.assertNotIn("section", str(failure.exception))
            Path(os.environ["TASK_HISTORY"]).write_text("not JSON")
            os.environ["DELIVERY_FORBIDDEN_TEXT"] = "explanation omitted"
            with self.assertRaisesRegex(support.DescriptionSettingError, "omission notice"):
                deliver_git.pull_request_body("TICKET-7", "none")
            Path(os.environ["TASK_HISTORY"]).write_text(json.dumps({"history": [
                {"role": "confirm", "speaker": "reader", "output": "## Summary\nSafe text"}]}))
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
            settings.update(TASK_HISTORY="unused-fixture-history", TASK_ISSUE="TICKET-7")
            with self.subTest(settings=settings), patch.dict(os.environ, settings, clear=True):
                with self.assertRaisesRegex(support.DescriptionSettingError, "PR_DESCRIPTION_SECTION"):
                    deliver_git.pull_request_body("TICKET-7", "none")

    def test_blank_body_is_absent_not_a_heading_only_description(self):
        with tempfile.TemporaryDirectory() as directory, patch.dict(os.environ, {
                "TASK_HISTORY": str(Path(directory) / "history.json"), "TASK_ISSUE": "TICKET-7",
                "PR_DESCRIPTION_ROLE": "work", "PR_DESCRIPTION_SECTION": "Summary",
                "GITHUB_TOKEN": "fixture-only-credential"}, clear=True):
            Path(os.environ["TASK_HISTORY"]).write_text(json.dumps({"history": [
                {"role": "work", "speaker": "writer", "output": "## Summary\n \t\n## Private\nPRIVATE"}]}))
            body, _, absent = deliver_git.pull_request_body("TICKET-7", "none")
            self.assertTrue(absent)
            self.assertNotIn("## Summary", body)
