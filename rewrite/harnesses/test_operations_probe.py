"""Operator-visible results, with no live cluster or network fallback."""
import contextlib
import errno
import importlib.util
import io
import json
from pathlib import Path
import socket
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch


OPS = Path(__file__).resolve().parents[2] / "deploy/ticket-engine/operations"
TARGETS = [
    {"name": "model", "url": "https://allowed.example:8443", "expect": "connected"},
    {"name": "restricted", "url": "https://denied.example", "expect": "refused"},
]


class ProbeTests(unittest.TestCase):
    def setUp(self):
        self.assertTrue((OPS / "network_probe.py").is_file(),
                        "the documented network and after-deployment checks are not implemented")
        sys.path.insert(0, str(OPS))
        self.addCleanup(sys.path.remove, str(OPS))
        spec = importlib.util.spec_from_file_location("operator_probe", OPS / "network_probe.py")
        self.probe = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(self.probe)
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        self.targets = self.root / "targets.json"
        self.targets.write_text(json.dumps(TARGETS))

    def test_tcp_results_keep_ports_and_do_not_call_dns_or_timeout_refusals(self):
        seen = []

        def addresses(host, port, **kwargs):
            seen.append((host, port))
            if host == "missing.example":
                raise socket.gaierror("synthetic private diagnostic")
            return [(socket.AF_INET6, socket.SOCK_STREAM, 0, "", (host + "-first", port, 0, 0)),
                    (socket.AF_INET6, socket.SOCK_STREAM, 0, "", (host + "-second", port, 0, 0))]

        class Connection:
            def __enter__(self): return self
            def __exit__(self, *unused): pass
            def settimeout(self, timeout): pass
            def connect(self, address):
                if address[0].startswith("timeout"):
                    raise TimeoutError("synthetic private diagnostic")
                if address[0].startswith("denied") or address[0].endswith("first"):
                    raise OSError(errno.ECONNREFUSED, "synthetic private diagnostic")

        with patch.object(socket, "getaddrinfo", addresses), patch.object(socket, "socket", lambda *a: Connection()):
            result = self.probe.probe_targets(TARGETS, 1)
            self.assertTrue(result["ok"])
            self.assertIn(("allowed.example", 8443), seen)
            self.assertEqual(result["targets"][0]["outcome"], "connected")
            for host, outcome in (("missing.example", "dns-error"), ("timeout.example", "timeout")):
                target = [{"name": "unavailable", "url": "https://" + host, "expect": "refused"}]
                failed = self.probe.probe_targets(target, 1)
                self.assertFalse(failed["ok"])
                self.assertEqual(failed["targets"][0]["outcome"], outcome)
                self.assertNotIn("synthetic private diagnostic", json.dumps(failed))

    def fixture(self):
        network = {"ok": True, "targets": [dict(item, outcome=item["expect"], addresses=[]) for item in TARGETS]}
        issues = {"phase": "complete", "action": "read", "project": 17, "issues": [{"id": 41, "statusId": 1}],
                  "comments": {}, "atomic_snapshot": False, "rehearsal_fixture": False}
        for name, data in (("network.json", network), ("issues.json", issues)):
            (self.root / name).write_text(json.dumps(data))
        return network, issues

    def test_unsafe_targets_are_rejected_before_remote_execution(self):
        for index, url in enumerate(("https://" + "name:pass" + chr(64) + "denied.example", "https://allowed.example:0")):
            with self.subTest(url=url):
                self.targets.write_text(json.dumps([dict(TARGETS[0], url=url), TARGETS[1]]))
                args = ["egress", "--context", "fixture", "--namespace", "fixture", "--pod", "fixture",
                        "--container", "engine", "--targets", str(self.targets),
                        "--output", str(self.root / ("unsafe-" + str(index)))]
                with patch.object(self.probe.subprocess, "run", side_effect=AssertionError("unsafe target reached remote invocation")):
                    with self.assertRaises(ValueError):
                        self.probe.main(args)

    def arguments(self, output):
        return ["after", "--context", "fixture-context", "--namespace", "fixture-namespace",
                "--pod", "fixture-pod", "--container", "engine", "--kubectl", "fake-kubectl",
                "--targets", str(self.targets), "--output", str(output), "--config", "/fixture/operator.json",
                "--engine-bin", "/fixture/engine", "--tracker-bin", "/fixture/tracker", "--queue", "/fixture/queue",
                "--project-id", "17", "--egress-baseline", str(self.root / "network.json"),
                "--issues-baseline", str(self.root / "issues.json"), "--status-url", "https://status.example/healthz", "200"]

    def run_after(self, failure=None, baseline_change=False):
        network, issues = self.fixture()
        if baseline_change:
            (self.root / "issues.json").write_text(json.dumps(dict(issues, issues=[{"id": 41, "statusId": 2}])))
        output = self.root / (failure or "success")
        calls = []

        def run(command, **kwargs):
            calls.append(command)
            self.assertNotIn("--watch", command)
            self.assertNotIn("file-ticket.sh", " ".join(command))
            if "--check" in command:
                self.assertEqual(command[-4:], ["/fixture/engine", "--config", "/fixture/operator.json", "--check"])
                return subprocess.CompletedProcess(command, int(failure == "config"), b"private config text", b"private diagnostic")
            if any(str(part).endswith("read-issues.sh") for part in command):
                destination = Path(command[command.index("--output") + 1])
                destination.mkdir(mode=0o700)
                (destination / "result.json").write_text(json.dumps(issues))
                return subprocess.CompletedProcess(command, int(failure == "issues"), b"", b"private diagnostic")
            if any(str(part).endswith("idle-check.sh") for part in command):
                return subprocess.CompletedProcess(command, int(failure == "idle"), b'{"idle":true}', b"private diagnostic")
            self.assertEqual(command[0], "fake-kubectl")
            self.assertIn("-i", command)
            if failure == "network-malformed":
                return subprocess.CompletedProcess(command, 0, b"null", b"")
            if failure == "network":
                return subprocess.CompletedProcess(command, 1, json.dumps(network).encode(), b"private diagnostic")
            return subprocess.CompletedProcess(command, 0, json.dumps(network).encode(), b"")

        class Response:
            status = 503 if failure == "status" else 200
            def __enter__(self): return self
            def __exit__(self, *args): pass
            def read(self, *args): raise AssertionError("status response body must not be read")

        class Opener:
            def open(self, request, timeout):
                return Response()

        console = io.StringIO()
        with patch.object(self.probe.subprocess, "run", run), \
                patch.object(self.probe.urllib.request, "build_opener", return_value=Opener()), \
                contextlib.redirect_stdout(console), contextlib.redirect_stderr(console):
            result = self.probe.main(self.arguments(output))
        self.assertNotIn("private", console.getvalue())
        self.assertTrue(any("--check" in command for command in calls))
        self.assertTrue(any(any(str(part).endswith("idle-check.sh") for part in command) for command in calls))
        self.assertTrue(any(any(str(part).endswith("read-issues.sh") for part in command) for command in calls))
        saved = json.loads((output / "result.json").read_text())
        self.assertNotIn("private diagnostic", json.dumps(saved))
        return result, saved, output

    def test_success_checks_the_selected_installation_without_changing_it(self):
        rc, saved, output = self.run_after()
        self.assertEqual(rc, 0)
        self.assertTrue(saved["ok"])
        self.assertEqual(output.stat().st_mode & 0o777, 0o700)
        with self.assertRaises(FileExistsError):
            self.probe.main(self.arguments(output))

    def test_a_failed_check_survives_later_successful_checks(self):
        for failure in ("network", "network-malformed", "config", "status", "issues", "idle"):
            with self.subTest(failure=failure):
                rc, saved, _ = self.run_after(failure=failure)
                self.assertNotEqual(rc, 0)
                self.assertFalse(saved["ok"])
                self.assertFalse(saved["checks"]["network" if failure == "network-malformed" else failure])

    def test_remote_code_runs_without_local_helpers_or_live_dns(self):
        code = (OPS / "network_probe.py").read_bytes()
        request = {"targets": TARGETS, "timeout": 1}
        payload = str(len(code)).encode() + b"\n" + code + json.dumps(request).encode()
        bootstrap = ("import socket,sys; socket.getaddrinfo=lambda *a,**k: []; "
                     "n=int(sys.stdin.buffer.readline()); code=sys.stdin.buffer.read(n); "
                     "exec(compile(code,'network_probe','exec'),{'__name__':'__remote__'})")
        result = subprocess.run([sys.executable, "-B", "-I", "-c", bootstrap], input=payload,
                                capture_output=True, timeout=5, cwd=self.root, env={})
        self.assertEqual(result.returncode, 0, result.stderr)
        observed = json.loads(result.stdout)
        self.assertFalse(observed["ok"])
        self.assertEqual([row["outcome"] for row in observed["targets"]], ["dns-error", "dns-error"])

    def test_changed_or_unsuccessful_baselines_do_not_confirm_deployment(self):
        rc, saved, _ = self.run_after(baseline_change=True)
        self.assertNotEqual(rc, 0)
        self.assertFalse(saved["checks"]["issues_baseline"])
        network, _ = self.fixture()
        changed_dns = json.loads(json.dumps(network))
        changed_dns["targets"][0]["addresses"] = [{"address": "another resolved address", "outcome": "connected"}]
        self.assertEqual(self.probe.network_summary(network), self.probe.network_summary(changed_dns))
        with self.assertRaises(ValueError):
            self.probe.network_summary(dict(network, ok=False))


if __name__ == "__main__":
    unittest.main()
