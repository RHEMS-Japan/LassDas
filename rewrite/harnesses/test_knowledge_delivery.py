"""Knowledge and code use the actual review and delivery commands.

The model's verdicts are scripted: this checks transport and Git, not judgment.
"""
import json
from pathlib import Path
import subprocess
import sys
import unittest

import test_adversarial_review as review_fixture
import test_deliver_git as delivery_fixture


class KnowledgeDeliveryTests(unittest.TestCase):
    def setUp(self):
        self.delivery = delivery_fixture.DeliveryTests()
        self.addCleanup(self.delivery.doCleanups)
        self.delivery.setUp()

    def test_reviewed_answer_and_code_reach_one_pull_request(self):
        delivery = self.delivery
        path = "knowledge/decisions.md"
        question = "May the result contain at most three entries or five?"
        answer = "Use at most three entries."
        note = "# Existing knowledge\nDo not discard this.\n\n## Entry limit\n" + question + "\n" + answer + "\n"
        delivery.change("main.go", "package main\n\nconst EntryLimit = 3\n")
        delivery.change(path, note.replace(answer, "Use five entries."))
        config = json.loads((Path(__file__).resolve().parents[1] / "examples/operator-stages.json").read_text())
        prompt = config["instructions"] + "\nOriginal request: implement the chosen entry limit.\n"
        prompt += "Question: " + question + "\nRequester: " + answer
        model = review_fixture.ModelStandIn([
            {"verdict": (True, "The note contradicts the requester: three, not five.")},
            {"verdict": (False, "The note and code use the answered limit.")},
        ])
        self.addCleanup(model.close)
        environment = delivery_fixture.git_environment()
        environment.update(TASK_WORKSPACE=str(delivery.workspace), TASK_HOME=str(delivery.home),
                           REVIEW_MODEL_URL=model.url, REVIEW_MODEL="fixture/reviewer",
                           REVIEW_API_KEY=review_fixture.KEY, REVIEW_TEST_COMMANDS="")

        def review():
            return subprocess.run([sys.executable, "-B", str(review_fixture.SCRIPT)], input=prompt,
                                  env=environment, text=True, capture_output=True, timeout=30)

        refused = review()
        self.assertEqual(refused.returncode, 1, refused.stdout + refused.stderr)
        self.assertIn("three, not five", refused.stdout)
        self.assertEqual(delivery.state["pulls"], [])
        delivery.change(path, note)
        reviewed = review()
        self.assertEqual(reviewed.returncode, 0, reviewed.stdout + reviewed.stderr)
        self.assertEqual(len(model.requests), 2)
        for number, request in enumerate(model.requests):
            carried = request["body"]["messages"][1]["content"]
            for expected in (question, answer, "same reviewed pull request", path, "EntryLimit = 3"):
                self.assertIn(expected, carried)
            self.assertIn("Use five entries." if number == 0 else answer, carried)
        done = delivery.deliver(DELIVERY_MERGE_METHOD="none", DELIVERY_ALLOWED_PATHS="main.go:knowledge/")
        self.assertEqual(done.returncode, 0, done.stdout + done.stderr)
        self.assertEqual(len(delivery.state["pulls"]), 1)
        pull = delivery.state["pulls"][0]
        self.assertEqual(pull["state"], "open")
        head = pull["head"]["sha"]
        self.assertEqual(delivery.git(delivery.remote, "show", head + ":" + path).stdout, note)
        self.assertIn("EntryLimit = 3", delivery.git(delivery.remote, "show", head + ":main.go").stdout)
        self.assertEqual(delivery.git(delivery.remote, "show", "master:notes.md").stdout, "original\n")
        again = delivery.deliver(DELIVERY_MERGE_METHOD="none", DELIVERY_ALLOWED_PATHS="main.go:knowledge/")
        self.assertEqual(again.returncode, 0, again.stdout + again.stderr)
        self.assertEqual(len(delivery.state["pulls"]), 1)
        self.assertEqual(delivery.git(delivery.remote, "show", head + ":" + path).stdout.count(answer), 1)

    def test_knowledge_does_not_expand_the_configured_delivery_permission(self):
        delivery = self.delivery
        delivery.change("main.go", "package main\n\nconst EntryLimit = 3\n")
        delivery.change("knowledge/decisions.md", "The requester chose three entries.\n")
        refused = delivery.deliver(DELIVERY_MERGE_METHOD="none")
        self.assertEqual(refused.returncode, 1, refused.stdout + refused.stderr)
        self.assertIn("knowledge/decisions.md", refused.stdout + refused.stderr)
        self.assertEqual(delivery.state["pulls"], [])
        self.assertEqual(delivery.remote_branches(), ["refs/heads/master"])


if __name__ == "__main__":
    unittest.main()
