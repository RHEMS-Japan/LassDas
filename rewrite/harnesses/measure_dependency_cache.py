"""Local, credential-free npm/Go cache experiment; includes installed files."""
import argparse
import functools
import http.server
import io
import json
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import threading
import zipfile

from linux_role import dependency_environment


def size(root):
    return sum(p.stat().st_size for p in root.rglob("*") if p.is_file())


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--pnpm", help="optional pnpm executable for the same offline check")
    args = parser.parse_args()
    with tempfile.TemporaryDirectory(prefix="request-cache-fixture-") as temporary:
        root = Path(temporary)
        served = root / "served"
        served.mkdir()
        for name in ("user.npmrc", "global.npmrc"):
            (root / name).write_text("")
        with tarfile.open(served / "fixture.tgz", "w:gz") as archive:
            for name, data in {"package/package.json": b'{"name":"cache-fixture","version":"1.0.0","main":"index.js"}',
                               "package/index.js": b"module.exports = () => 42;\n"}.items():
                info = tarfile.TarInfo(name)
                info.size = len(data)
                archive.addfile(info, io.BytesIO(data))
        version = served / "example.invalid" / "fixture" / "@v"
        version.mkdir(parents=True)
        (version / "v1.0.0.info").write_text('{"Version":"v1.0.0","Time":"2026-01-01T00:00:00Z"}')
        (version / "v1.0.0.mod").write_text("module example.invalid/fixture\n\ngo 1.23\n")
        (version / "list").write_text("v1.0.0\n")
        with zipfile.ZipFile(version / "v1.0.0.zip", "w") as archive:
            archive.writestr("example.invalid/fixture@v1.0.0/go.mod", (version / "v1.0.0.mod").read_bytes())
            archive.writestr("example.invalid/fixture@v1.0.0/answer.go", "package fixture\nfunc Answer() int { return 42 }\n")
        requests = []
        class Handler(http.server.SimpleHTTPRequestHandler):
            def do_GET(self):
                requests.append(self.path)
                super().do_GET()
            def log_message(self, *args):
                pass
        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), functools.partial(Handler, directory=str(served)))
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        origin = "http://127.0.0.1:%d" % server.server_port
        results = {}
        try:
            for shared in (False, True):
                request = root / ("shared" if shared else "separate")
                for role in range(2):
                    home = request / ("role-%d" % role)
                    project = home / "project"
                    project.mkdir(parents=True)
                    (project / "package.json").write_text('{"name":"fixture-project","version":"1.0.0","private":true}')
                    (project / "go.mod").write_text("module example.invalid/project\n\ngo 1.23\n\nrequire example.invalid/fixture v1.0.0\n")
                    (project / "answer_test.go").write_text('package project\nimport ("testing"; f "example.invalid/fixture")\nfunc TestAnswer(t *testing.T) { if f.Answer()!=42 { t.Fatal("wrong answer") } }\n')
                    cache = request / "dependency-cache" if shared else home / "cache"
                    env = {"PATH": os.environ["PATH"], "HOME": str(home), "TMPDIR": str(root),
                           "GOMAXPROCS": "2", "GOENV": "off", "GOTOOLCHAIN": "local", "GOSUMDB": "off",
                           "GOPROXY": "off" if shared and role else origin,
                           "GOCACHE": str(cache / "go-build"), "GOMODCACHE": str(cache / "go-mod"),
                           "npm_config_cache": str(cache / "npm"), "npm_config_userconfig": str(root / "user.npmrc"),
                           "npm_config_globalconfig": str(root / "global.npmrc"), "npm_config_update_notifier": "false",
                           "npm_config_registry": origin, "CI": "true"}
                    env.update(dependency_environment(cache))
                    before = len(requests)
                    commands = [["npm", "install", "--ignore-scripts", "--no-audit", "--no-fund",
                                 *(["--offline"] if shared and role else []), origin + "/fixture.tgz"],
                                ["go", "test", "-mod=mod", "-p", "1", "./..."]]
                    if args.pnpm:
                        commands.append([args.pnpm, "install", "--ignore-scripts",
                                         *(["--offline"] if shared and role else [])])
                    for command in commands:
                        proc = subprocess.run(command, cwd=project, env=env, capture_output=True, text=True, timeout=120)
                        if proc.returncode:
                            raise RuntimeError(proc.stdout + proc.stderr)
                    if args.pnpm:
                        for command, expected in ((["store", "path"], cache / "pnpm"), (["root", "-g"], home)):
                            proc = subprocess.run([args.pnpm, *command], cwd=project, env=env, capture_output=True, text=True, timeout=30)
                            if proc.returncode or not Path(proc.stdout.strip()).is_relative_to(expected):
                                raise AssertionError("pnpm scope differs: " + proc.stdout + proc.stderr)
                    if shared and role and len(requests) != before:
                        raise AssertionError("the second role contacted the fixture server")
                results["shared" if shared else "separate"] = {"bytes": size(request), "homes_bytes": sum(size(request / ("role-%d" % i)) for i in range(2)),
                                                              "shared_cache_bytes": size(request / "dependency-cache") if shared else 0}
            reduction = 1 - results["shared"]["bytes"] / results["separate"]["bytes"]
            results["reduction_percent"] = round(100 * reduction, 2)
            results["second_shared_role_offline"] = True
            results["pnpm_checked"] = bool(args.pnpm)
            print(json.dumps(results, indent=2))
            if reduction < 0.30:
                return 3
        finally:
            server.shutdown()
            server.server_close()
            thread.join()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
