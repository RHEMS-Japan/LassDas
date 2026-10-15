"""Execute the guide's model checks with synthetic settings and HTTP replies."""
import contextlib
import io
import json
import os
from pathlib import Path
import re
import sys
import tempfile
import unittest
from unittest.mock import patch
import urllib.error

SOURCE = Path(__file__).resolve().parents[2]
GUIDE = SOURCE / "deploy/ticket-engine/SETUP.md"


class SetupModelPreflightTests(unittest.TestCase):
    def snippet(self, marker):
        match = re.search(r"^# " + marker + r"\n(.*?)^PY$", GUIDE.read_text(), re.M | re.S)
        self.assertIsNotNone(match, "documented check is missing")
        return match.group(1)

    def run_snippet(self, marker, config, environment, opener=None):
        with tempfile.TemporaryDirectory(prefix="setup-model-check-") as temporary:
            path = Path(temporary) / "operator.json"
            path.write_text(json.dumps(config))
            output, namespace = io.StringIO(), {}
            with patch.object(sys, "argv", ["-", str(path)]), \
                    patch.dict(os.environ, environment, clear=True), \
                    patch("urllib.request.build_opener", return_value=opener), \
                    contextlib.redirect_stdout(output):
                try:
                    exec(compile(self.snippet(marker), str(GUIDE), "exec"), namespace)
                except SystemExit as error:
                    return error.code, output.getvalue(), namespace
            return 0, output.getvalue(), namespace

    def test_credential_check_uses_only_configured_names_and_fails_when_missing(self):
        config = json.loads((SOURCE / "rewrite/examples/operator-stages.json").read_text())
        direct = {name: "synthetic-unused-value" for name in
                  ("MODEL_API_KEY", "TRACKER_API_KEY", "DELIVERY_GITHUB_TOKEN")}
        code, out, _ = self.run_snippet("setup-credential-names", config, direct)
        self.assertEqual(code, 0)
        self.assertEqual(json.loads(out), {name: "set" for name in direct})
        # Model all references after selecting gateway-only, including the
        # review process and a separate selector fallback credential.
        gateway = json.loads(json.dumps(config).replace('"MODEL_API_KEY"', '"GATEWAY_API_KEY"'))
        gateway["model_selection"].pop("judge")
        gateway["model_selection"]["fallback"] = {"key_env": "SELECTOR_KEY"}
        gateway["model_selection"]["gateway"] = {"key_env": "GATEWAY_API_KEY"}
        review = next(role for role in gateway["roles"] if role["name"] == "review")
        review["processes"][0]["secrets"]["REVIEW_API_KEY"] = "CUSTOM_REVIEW_KEY"
        environment = {name: "synthetic-private-value" for name in
                       ("GATEWAY_API_KEY", "SELECTOR_KEY", "CUSTOM_REVIEW_KEY", "TRACKER_API_KEY", "DELIVERY_GITHUB_TOKEN")}
        for missing in (None, *environment):
            with self.subTest(missing=missing):
                values = {name: value for name, value in environment.items() if name != missing}
                code, out, _ = self.run_snippet("setup-credential-names", gateway, values)
                self.assertEqual(code == 0, missing is None)
                result = json.loads(out)
                self.assertEqual(set(result), set(environment))
                self.assertNotIn("MODEL_API_KEY", result)
                if missing:
                    self.assertEqual(result[missing], "unset")
                self.assertNotIn("synthetic-private-value", out)

    def gateway(self):
        return {"roles": [{"name": "review", "processes": [{"env": {"REVIEW_MODEL": "route/another/current"}}]}],
                "model_selection": {"authors": ["maker"], "fallback": {"model": "route/maker/current"}, "gateway": {
            "models_url": "https://gateway.example/v1/models", "prefix": "route/",
            "key_env": "GATEWAY_API_KEY"}}}

    def public_model(self, model="maker/current", tools=True, text=True):
        return {"id": model, "supported_parameters": ["tools"] if tools else [],
                "architecture": {"output_modalities": ["text"] if text else []}}

    def catalogue(self, config=None, public=None, served=None, failure=None):
        requests = []
        public = public if public is not None else [self.public_model()]
        served = served if served is not None else [{"id": "route/maker/current"}, {"id": "route/another/current"}]

        class Client:
            def open(self, request, timeout):
                requests.append(request)
                if failure:
                    raise failure
                data = served if request.host == "gateway.example" else public
                return io.BytesIO(json.dumps({"data": data}).encode())

        result = self.run_snippet("setup-gateway-catalogue", config or self.gateway(),
                                  {"GATEWAY_API_KEY": "synthetic-private-value"}, Client())
        return result, requests

    def test_catalogue_matches_exact_prefixed_tool_text_models_without_inference(self):
        public = [self.public_model(), self.public_model("maker/no-tools", tools=False),
                  self.public_model("maker/no-text", text=False), self.public_model("another/current")]
        served = [{"id": "route/" + model["id"]} for model in public]
        (code, out, namespace), requests = self.catalogue(public=public, served=served)
        self.assertEqual(code, 0)
        self.assertEqual(json.loads(out), {"served": 4, "tool_text_matches": 1,
                                         "first_20_matches": ["route/maker/current"]})
        self.assertEqual(len(requests), 2)
        self.assertEqual(requests[0].get_header("Authorization"), "Bearer synthetic-private-value")
        self.assertIsNone(requests[1].get_header("Authorization"), "public catalogue received a credential")
        self.assertTrue(all(request.get_method() == "GET" and request.data is None for request in requests))
        self.assertIsNone(namespace["NoRedirect"]().redirect_request(None, None, 302, "", {}, "https://other.example"))
        self.assertNotIn("synthetic-private-value", out)

    def test_configured_fallback_and_each_selected_reviewer_must_be_served(self):
        for missing in ("fallback", "reviewer", "second-reviewer"):
            with self.subTest(missing=missing):
                config = self.gateway()
                env = config["roles"][0]["processes"][0]["env"]
                if missing == "fallback":
                    config["model_selection"]["fallback"]["model"] = "route/maker/not-served"
                elif missing == "reviewer":
                    env["REVIEW_MODEL"] = "route/another/not-served"
                else:
                    env["REVIEW_MODELS"] = "route/another/current, route/another/not-served"
                (code, out, _), requests = self.catalogue(config=config)
                self.assertNotEqual(code, 0)
                self.assertIn("not-served", str(code))
                self.assertNotIn("synthetic-private-value", str(code) + out)
                self.assertTrue(all(request.get_method() == "GET" for request in requests))
        config = self.gateway()
        env = config["roles"][0]["processes"][0]["env"]
        env.update(REVIEW_MODEL="ignored/not-served", REVIEW_MODELS="route/another/current")
        (code, _, _), _ = self.catalogue(config=config)
        self.assertEqual(code, 0, "REVIEW_MODELS must override REVIEW_MODEL as the runtime does")

    def test_wrong_prefix_or_missing_capabilities_never_report_available_models(self):
        for public, served in (([self.public_model()], [{"id": "maker/current"}]),
                               ([self.public_model(tools=False)], [{"id": "route/maker/current"}]),
                               ([self.public_model(text=False)], [{"id": "route/maker/current"}])):
            with self.subTest(public=public, served=served):
                (code, out, _), _ = self.catalogue(public=public, served=served)
                self.assertNotEqual(code, 0)
                self.assertEqual(json.loads(out)["tool_text_matches"], 0)

    def test_refused_or_invalid_catalogue_cannot_leak_response_or_send_to_plain_http(self):
        for status in (302, 401, 405, 500):
            failure = urllib.error.HTTPError("https://gateway.example/v1/models", status,
                                             "synthetic-private-value", {}, io.BytesIO(b"synthetic-private-value"))
            try:
                (code, out, _), requests = self.catalogue(failure=failure)
                self.assertNotEqual(code, 0)
                self.assertIn(str(status), str(code))
                self.assertNotIn("synthetic-private-value", str(code) + out)
                self.assertEqual(len(requests), 1)
            finally:
                failure.close()
        for address in ("http://gateway.example/v1/models", "https:" + "//user:pass" + "@" + "gateway.example/v1/models"):
            config = self.gateway()
            config["model_selection"]["gateway"]["models_url"] = address
            (code, _, _), requests = self.catalogue(config=config)
            self.assertNotEqual(code, 0)
            self.assertEqual(requests, [])
        for served in ([], [{"id": None}], ["not an entry"]):
            (code, out, _), _ = self.catalogue(served=served)
            self.assertNotEqual(code, 0)
            self.assertEqual(out, "")


if __name__ == "__main__":
    unittest.main()
