"""Run the packaged bridge against the installed SDK and a scripted model."""
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import threading


def verify_run(result, calls, failures, enabled):
    """A stopped role is not proved by a nonzero exit alone."""
    if enabled and failures >= 5:
        assert result.returncode == 1, "the fifth identical failure did not end the role"
        assert calls == 5, f"expected exactly five model calls, got {calls}"
        reason = "same failure repeated 5 times in a row"
        assert reason in result.stdout and reason in result.stderr, "the failure-stop reason was not reported"
    else:
        assert result.returncode == 0, "the control stopped before finishing"
        assert calls == failures + 1, "the control did not execute every scripted step"
        assert "Script finished." in result.stdout, "the control lost the final model response"


def run_case(failures, enabled):
    calls = []

    class Model(BaseHTTPRequestHandler):
        def log_message(self, *_args):
            pass

        def send_json(self, body):
            data = json.dumps(body).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def do_GET(self):
            self.send_json({"data": [{"id": "maker/test", "context_length": 128000}]})

        def do_POST(self):
            body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", "0"))))
            offered = {tool.get("function", {}).get("name") for tool in body.get("tools") or []}
            if offered:
                calls.append(offered)
                number = len(calls)
                if "process" not in offered:
                    self.send_error(400, "the actual SDK did not offer its process tool")
                    return
            else:
                number = 0  # SDK auxiliary requests do not consume a tool step.
            message = {"role": "assistant", "content": "Script finished."}
            finish = "stop"
            if number and number <= failures:
                message = {"role": "assistant", "content": None, "tool_calls": [
                    {"index": 0, "id": f"call_{number}", "type": "function",
                     "function": {"name": "process", "arguments": '{"action":"wait"}'}}]}
                finish = "tool_calls"
            usage = {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
            base = {"id": f"reply_{number}", "created": 0, "model": "maker/test"}
            if not body.get("stream"):
                if "tool_calls" in message:
                    for call in message["tool_calls"]:
                        call.pop("index")
                self.send_json(dict(base, object="chat.completion", usage=usage,
                                    choices=[{"index": 0, "message": message, "finish_reason": finish}]))
                return
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.end_headers()
            for delta, reason in ((message, None), ({}, finish)):
                chunk = dict(base, object="chat.completion.chunk", usage=usage,
                             choices=[{"index": 0, "delta": delta, "finish_reason": reason}])
                self.wfile.write(b"data: " + json.dumps(chunk).encode() + b"\n\n")
            self.wfile.write(b"data: [DONE]\n\n")
            self.wfile.flush()

    with tempfile.TemporaryDirectory(prefix="native-stop-") as directory, \
            ThreadingHTTPServer(("127.0.0.1", 0), Model) as server:
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        home, workspace = Path(directory) / "role", Path(directory) / "workspace"
        home.mkdir()
        workspace.mkdir()
        # No host/model credentials are inherited. The containing image runs
        # with --network none; only its own loopback endpoint is reachable.
        environment = {"PATH": os.environ["PATH"], "PYTHONDONTWRITEBYTECODE": "1",
                       "PYTHONPATH": "/opt/hermes-src", "PYTHONNOUSERSITE": "1", "HOME": str(home),
                       "PYTHON_DOTENV_DISABLED": "1", "HERMES_HOME": str(home),
                       "TASK_HOME": str(home), "TASK_WORKSPACE": str(workspace),
                       "TERMINAL_ENV": "local", "NATIVE_MODEL": "maker/test",
                       "OPENROUTER_BASE_URL": f"http://127.0.0.1:{server.server_port}/v1",
                       "OPENROUTER_API_KEY": "fixture-only", "NATIVE_MAX_TURNS": "20"}
        if not enabled:
            environment["NATIVE_MAX_REPEATED_FAILURES"] = "0"
        try:
            result = subprocess.run([sys.executable, "/opt/ticket-automation/bundle/harnesses/hermes.py"],
                                    input="Run the supplied process calls, then report.",
                                    capture_output=True, text=True, cwd=workspace,
                                    env=environment, timeout=120)
        finally:
            server.shutdown()
            thread.join(timeout=2)
        stops = [line for line in result.stdout.splitlines() if line.startswith("Stopped")]
        print(f"failures={failures} enabled={enabled} exit={result.returncode} model_calls={len(calls)} "
              f"stop={stops}", flush=True)
        try:
            verify_run(result, len(calls), failures, enabled)
        except AssertionError as error:
            print("--- stdout ---", result.stdout, "--- stderr ---", result.stderr, sep="\n", file=sys.stderr)
            print(f"::error::{error}", flush=True)
            raise


if __name__ == "__main__":
    for failures, enabled in ((12, True), (4, True), (6, False)):
        run_case(failures, enabled)
