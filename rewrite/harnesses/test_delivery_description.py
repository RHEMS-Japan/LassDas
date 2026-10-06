"""Full explanation transport through real Git and the local service fixture."""
import json
import unittest

import test_adversarial_review as review_fixture
import test_deliver_git as delivery_fixture


class DescriptionTests(unittest.TestCase):
    def setUp(self):
        self.fixture = delivery_fixture.DeliveryTests()
        self.fixture.setUp()
        self.addCleanup(self.fixture.doCleanups)
        self.fixture.change("main.go", "package main\n// requested change\n")
        self.history = self.fixture.home / "history.json"

    def deliver(self, **extra):
        settings = dict(PR_DESCRIPTION_ROLE="work", TASK_HISTORY=str(self.history),
                        DELIVERY_ALLOWED_PATHS="main.go:notes.md", DELIVERY_MERGE_METHOD="none")
        settings.update(extra)
        return self.fixture.deliver(**settings)

    def explain(self, text):
        self.history.write_text(json.dumps({"request": "Make the requested change", "history": [
            {"role": "work", "speaker": "writer", "output": text},
        ]}), encoding="utf-8")

    def test_long_explanation_settings_and_last_finding_reach_the_pull_request(self):
        text = "実装と調査の結果\n" + "\n".join("所見 %d: " % n + "確認した根拠。" * 35 for n in range(1, 21))
        text += '\n設定例: {"enabled":true}\n確認: curl -I https://service.example.invalid/\n'
        self.explain(text)
        self.fixture.advance_integration_branch("notes.md", "Another request's project notes\n")
        result = self.deliver()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertTrue(self.fixture.state["pulls"][0]["body"].endswith(text))
        self.assertEqual(self.fixture.git(self.fixture.remote, "show", "refs/heads/ticket/TICKET-41:notes.md").stdout,
                         "Another request's project notes\n")
        self.assertNotIn("pull-request-description.md", self.fixture.git(
            self.fixture.remote, "ls-tree", "-r", "--name-only", "refs/heads/ticket/TICKET-41").stdout)
        receipt = self.fixture.receipt()
        saved = self.fixture.home / "pull-request-description.md"
        self.assertEqual(saved.read_text(), self.fixture.state["pulls"][0]["body"])
        self.assertEqual(receipt["description_path"], str(saved))
        self.assertEqual(receipt["description_commit"], receipt["head"])
        again = self.deliver()
        self.assertEqual(again.returncode, 0, again.stderr)
        self.assertEqual(len(self.fixture.state["pulls"]), 1)
        self.assertFalse(any(method == "PATCH" for method, _ in self.fixture.state["requests"]))

    def test_next_round_updates_the_same_pull_request_without_shortening(self):
        self.explain("First explanation\n")
        self.assertEqual(self.deliver().returncode, 0)
        text = "Second round: " + "日本語の確認結果\n" * 300 + "Last required verification example\n"
        self.fixture.state["pulls"][0]["body"] = self.fixture.state["pulls"][0]["body"].replace("\n", "\r\n")
        self.fixture.state["description_line_endings"] = True
        self.explain(text)
        result = self.deliver()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(len(self.fixture.state["pulls"]), 1)
        self.assertTrue(self.fixture.state["pulls"][0]["body"].replace("\r\n", "\n").endswith(text))
        self.assertEqual(self.fixture.receipt()["pull_request_body"], self.fixture.state["pulls"][0]["body"].replace("\r\n", "\n"))
        self.assertNotIn("description_retained", self.fixture.receipt())
        self.assertEqual(sum(method == "PATCH" for method, _ in self.fixture.state["requests"]), 1)

    def test_observations_tests_and_live_limits_reach_the_pull_request_unchanged(self):
        for live in ("なし (導入先に検証の手段が無い)",
                     "架空の検証先: command output = Hello; 本番ではありません"):
            with self.subTest(live=live):
                text = ("観察できる結果\n保存値の読み戻し: Hello\n"
                        "単体テスト\nfixture check: exit 0\nライブ確認\n" + live + "\n")
                self.explain(text)
                result = self.deliver()
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertTrue(self.fixture.state["pulls"][0]["body"].endswith(text))
                self.assertEqual((self.fixture.home / "pull-request-description.md").read_text(),
                                 self.fixture.state["pulls"][0]["body"])

    def test_uncertain_update_and_another_work_round_keep_the_full_explanation(self):
        self.explain("First explanation\n")
        self.assertEqual(self.deliver().returncode, 0)
        self.explain("Second explanation\n")
        self.fixture.state["description_uncertain"] = True
        failed = self.deliver(DELIVERY_RETRY_ATTEMPTS="1")
        self.assertNotEqual(failed.returncode, 0)
        self.assertIn("have not been undone", failed.stdout)
        self.assertNotIn("Nothing was delivered", failed.stdout)
        self.assertIn("pending_pull_request_body", self.fixture.receipt())
        self.assertTrue(self.fixture.receipt()["pending_pull_request_body"].endswith("Second explanation\n"))
        self.explain("Third explanation after the uncertain reply\n")
        retried = self.deliver()
        self.assertEqual(retried.returncode, 0, retried.stdout + retried.stderr)
        self.assertEqual(len(self.fixture.state["pulls"]), 1)
        self.assertTrue(self.fixture.state["pulls"][0]["body"].endswith("Third explanation after the uncertain reply\n"))
        self.assertNotIn("pending_pull_request_body", self.fixture.receipt())

    def test_a_persons_different_description_is_kept_while_new_work_is_pushed_and_merged(self):
        self.explain("First explanation\n")
        self.assertEqual(self.deliver().returncode, 0)
        self.fixture.state["pulls"][0]["body"] = "A person's explanation; do not replace it."
        self.explain("Another explanation\n")
        self.fixture.change("main.go", "package main\n// next reviewed change\n")
        result = self.deliver(DELIVERY_MERGE_METHOD="merge")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("was retained without another replacement", result.stdout)
        self.assertEqual(self.fixture.state["pulls"][0]["body"], "A person's explanation; do not replace it.")
        self.assertFalse(any(method == "PATCH" for method, _ in self.fixture.state["requests"]))
        self.assertTrue(self.fixture.state["pulls"][0]["merged"])
        self.assertIn("next reviewed change", self.fixture.git(self.fixture.remote, "show", "master:main.go").stdout)
        receipt = self.fixture.receipt()
        self.assertIn(receipt["description_commit"], result.stdout)
        self.assertIn(receipt["description_path"], result.stdout)
        self.assertTrue((self.fixture.home / "pull-request-description.md").read_text().endswith("Another explanation\n"))

    def test_an_uncertain_first_creation_is_reused_after_another_work_round(self):
        self.explain("First explanation\n")
        self.fixture.state["description_post_uncertain"] = True
        first = self.deliver(DELIVERY_RETRY_ATTEMPTS="1")
        self.assertNotEqual(first.returncode, 0)
        self.assertNotIn("Nothing was delivered", first.stdout)
        self.assertEqual(len(self.fixture.state["pulls"]), 1)
        self.assertIn("pending_pull_request_body", self.fixture.receipt())
        self.assertTrue(self.fixture.receipt()["pending_pull_request_body"].endswith("First explanation\n"))
        self.explain("Second explanation after an uncertain first creation\n")
        second = self.deliver()
        self.assertEqual(second.returncode, 0, second.stdout + second.stderr)
        self.assertEqual(len(self.fixture.state["pulls"]), 1)
        self.assertTrue(self.fixture.state["pulls"][0]["body"].endswith("Second explanation after an uncertain first creation\n"))

    def test_each_actual_creation_attempt_keeps_its_own_uncertain_text(self):
        self.explain("First attempted explanation\n")
        self.fixture.state["post_failures"] = 1
        first = self.deliver(DELIVERY_RETRY_ATTEMPTS="1")
        self.assertNotEqual(first.returncode, 0)
        self.assertEqual(self.fixture.state["pulls"], [])
        self.explain("Second attempted explanation\n")
        self.fixture.state["description_post_uncertain"] = True
        second = self.deliver(DELIVERY_RETRY_ATTEMPTS="1")
        self.assertNotEqual(second.returncode, 0)
        self.assertEqual(len(self.fixture.state["pulls"]), 1)
        self.assertTrue(self.fixture.state["pulls"][0]["body"].endswith("Second attempted explanation\n"))
        self.explain("Third explanation after the lost reply\n")
        third = self.deliver()
        self.assertEqual(third.returncode, 0, third.stdout + third.stderr)
        self.assertEqual(len(self.fixture.state["pulls"]), 1)
        self.assertTrue(self.fixture.state["pulls"][0]["body"].endswith("Third explanation after the lost reply\n"))

    def test_uncertain_creation_after_a_persons_merge_keeps_the_new_round(self):
        self.explain("Earlier round explanation\n")
        self.assertEqual(self.deliver().returncode, 0)
        self.fixture.merge_right_after_the_next_read(1)
        self.fixture.change("main.go", "package main\n// second delivery round\n")
        self.explain("Next round explanation\n")
        self.fixture.state["description_post_uncertain"] = True
        uncertain = self.deliver(DELIVERY_RETRY_ATTEMPTS="1")
        self.assertNotEqual(uncertain.returncode, 0)
        self.assertNotIn("Nothing was delivered", uncertain.stdout)
        self.assertNotIn("Nothing was merged", uncertain.stdout)
        self.assertEqual(len(self.fixture.state["pulls"]), 2)
        self.assertTrue(self.fixture.state["pulls"][0]["merged"])
        self.assertIn("pending_pull_request_body", self.fixture.receipt())
        self.assertTrue(self.fixture.receipt()["pending_pull_request_body"].endswith("Next round explanation\n"))
        self.explain("New explanation after the lost second creation\n")
        retried = self.deliver()
        self.assertEqual(retried.returncode, 0, retried.stdout + retried.stderr)
        self.assertEqual(len(self.fixture.state["pulls"]), 2)
        self.assertEqual(self.fixture.receipt()["previous"][0]["pull_request"], 1)
        self.assertEqual(self.fixture.receipt()["pull_request"], 2)
        self.assertTrue(self.fixture.state["pulls"][1]["body"].endswith("New explanation after the lost second creation\n"))

    def test_a_refused_update_stays_failed_but_service_reformatted_text_does_not_block_merge(self):
        self.explain("First explanation\n")
        self.assertEqual(self.deliver().returncode, 0)
        for failure in ("description_refusal", "description_readback_mismatch"):
            with self.subTest(failure=failure):
                self.fixture.state[failure] = True
                self.explain("Next explanation for " + failure + "\n")
                result = self.deliver(DELIVERY_MERGE_METHOD="merge")
                if failure == "description_refusal":
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("have not been undone", result.stdout)
                    self.assertFalse(self.fixture.state["pulls"][0]["merged"])
                else:
                    self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                    self.assertIn("was retained without another replacement", result.stdout)
                    self.assertTrue(self.fixture.state["pulls"][0]["merged"])
                    self.assertFalse(self.fixture.state["pulls"][0]["body"].endswith("Next explanation for " + failure + "\n"))
                self.fixture.state[failure] = False

    def test_a_full_explanation_is_present_before_automatic_merge(self):
        text = "Explanation with verification at the end.\n" * 200
        self.explain(text)
        result = self.deliver(DELIVERY_MERGE_METHOD="merge")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertTrue(self.fixture.state["pulls"][0]["merged"])
        self.assertTrue(self.fixture.state["pulls"][0]["body"].endswith(text))

    def test_bad_description_is_not_shortened_or_pushed(self):
        for shape, text in [
            ("large", "x" * 60_001),
            ("report fits but complete body does not", "x" * 59_900),
            ("large Unicode", "界" * 30_000),
        ]:
            with self.subTest(shape=shape):
                self.explain(text)
                result = self.deliver()
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("60000-byte", result.stderr)
                self.assertEqual(self.fixture.state["pulls"], [])
                self.assertNotIn("refs/heads/ticket/TICKET-41", self.fixture.remote_branches())

        result = self.deliver(PR_DESCRIPTION_MAX_BYTES="100000")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertTrue(self.fixture.state["pulls"][0]["body"].endswith(text))

    def test_empty_or_failed_report_goes_back_without_publishing(self):
        for history in (b'{"history":[]}',
                        json.dumps({"history": [{"role": "work", "speaker": "writer",
                                                "output": "", "error": "process failed"}]}).encode()):
            with self.subTest(history=history):
                self.history.write_bytes(history)
                result = self.deliver()
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(self.fixture.state["pulls"], [])
                self.assertNotIn("refs/heads/ticket/TICKET-41", self.fixture.remote_branches())

    def test_review_uses_the_delivered_bodys_byte_limit(self):
        self.explain("変更と検証の説明。")
        delivered = self.deliver()
        self.assertEqual(delivered.returncode, 0, delivered.stderr)
        size = len(self.fixture.state["pulls"][0]["body"].encode("utf-8"))
        review = review_fixture.AdversarialReviewTests()
        review.setUp()
        self.addCleanup(review.doCleanups)
        service = review_fixture.ModelStandIn([{"verdict": (False, "checked")}])
        self.addCleanup(service.close)
        for limit, status, calls in ((size - 1, 1, 0), (size, 0, 1)):
            with self.subTest(limit=limit):
                result = review.run_review(service, TASK_HISTORY=str(self.history), PR_DESCRIPTION_ROLE="work",
                                           DELIVERY_MERGE_METHOD="none", PR_DESCRIPTION_MAX_BYTES=str(limit))
                self.assertEqual(result.returncode, status, result.stdout + result.stderr)
                self.assertEqual(len(service.requests), calls)
                print("published_body_bytes=%d review_limit=%d exit=%d model_requests=%d"
                      % (size, limit, result.returncode, len(service.requests)))

    def test_unreadable_history_delivers_with_an_explicit_omission(self):
        for content in (b"not JSON", b'{"history":[]}\xff'):
            with self.subTest(content=content):
                self.history.write_bytes(content)
                result = self.deliver()
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn("Pull request explanation omitted", result.stdout)
                self.assertIn("Pull request explanation omitted", self.fixture.state["pulls"][0]["body"])
                self.assertNotIn(str(self.history), self.fixture.state["pulls"][0]["body"])

    def test_saved_report_is_still_checked_for_forbidden_text(self):
        self.explain("approved document with a forbidden-label\n")
        first = self.deliver()
        self.assertEqual(first.returncode, 0, first.stderr)
        result = self.deliver(DELIVERY_FORBIDDEN_TEXT="forbidden-label")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("configured forbidden text", result.stderr)
        self.assertIn("pull request description", result.stderr)
        self.assertNotIn("staged change", result.stderr)
        self.assertNotIn("forbidden-label", result.stderr)
        self.assertNotIn("Nothing was delivered", result.stdout)

    def test_saved_report_does_not_publish_the_delivery_credential(self):
        self.explain("Ordinary explanation\n")
        self.assertEqual(self.deliver().returncode, 0)
        result = self.deliver(GITHUB_TOKEN="Ordinary explanation")
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("Ordinary explanation", result.stdout + result.stderr)
        self.assertEqual(sum(method == "PATCH" for method, _ in self.fixture.state["requests"]), 0)


if __name__ == "__main__":
    unittest.main()
