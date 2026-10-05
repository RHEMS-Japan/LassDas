"""Observe explicit connections and read an installation; never deploy or repair."""
import argparse
import errno
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import urllib.error
import urllib.parse
import urllib.request

if __name__ != "__remote__":
    import tracker_helper as files


def endpoint(value, https=False):
    if not isinstance(value, str) or any(ord(c) <= 32 for c in value):
        raise ValueError("use an explicit URL")
    url = urllib.parse.urlsplit(value)
    if url.scheme not in (("https",) if https else ("http", "https")) or not url.hostname or \
            url.username is not None or url.password is not None or url.query or url.fragment:
        raise ValueError("use a credential-free endpoint without query or fragment")
    port = url.port if url.port is not None else (443 if url.scheme == "https" else 80)
    if port == 0:
        raise ValueError("use a nonzero target port")
    return url.hostname, port


def probe_targets(targets, timeout):
    rows = []
    for target in targets:
        host, port = endpoint(target["url"])
        observed = []
        try:
            addresses = socket.getaddrinfo(host, port, type=socket.SOCK_STREAM)
        except OSError:
            addresses = []
        seen = set()
        for family, kind, protocol, _, address in addresses:
            if (family, address) in seen:
                continue
            seen.add((family, address))
            try:
                with socket.socket(family, kind, protocol) as connection:
                    connection.settimeout(timeout)
                    connection.connect(address)
                outcome = "connected"
            except TimeoutError:
                outcome = "timeout"
            except OSError as error:
                outcome = "refused" if error.errno == errno.ECONNREFUSED else "error"
            observed.append({"address": list(address), "outcome": outcome})
        outcomes = {item["outcome"] for item in observed}
        outcome = ("dns-error" if not outcomes else "connected" if "connected" in outcomes else
                   next(iter(outcomes)) if len(outcomes) == 1 else "mixed")
        rows.append(dict(target, outcome=outcome, addresses=observed))
    return {"ok": all(row["outcome"] == row["expect"] for row in rows), "targets": rows}


def network_summary(result):
    if not isinstance(result, dict) or result.get("ok") is not True or not result.get("targets"):
        raise ValueError("network observation was not successful")
    return sorted((row["name"], row["url"], row["expect"], row["outcome"]) for row in result["targets"])


def issue_summary(result, project):
    value = files.complete([result], "read", project)
    return sorted(value["issues"], key=lambda row: row["id"])


def arguments(argv):
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="action", required=True)
    for action in ("egress", "after"):
        child = commands.add_parser(action)
        for name in ("context", "namespace", "pod", "container", "targets", "output"):
            child.add_argument("--" + name, required=True)
        child.add_argument("--kubectl", default="kubectl")
        child.add_argument("--timeout", type=int, default=60)
        if action == "after":
            for name in ("config", "engine-bin", "tracker-bin", "queue", "egress-baseline", "issues-baseline"):
                child.add_argument("--" + name, required=True)
            child.add_argument("--project-id", type=int, required=True)
            child.add_argument("--status-url", nargs=2, action="append", required=True, metavar=("HTTPS_URL", "EXPECTED_CODE"))
    args = parser.parse_args(argv)
    for name in ("context", "namespace", "pod", "container"):
        value = getattr(args, name)
        if not value or value != value.strip() or value.startswith("-") or any(ord(c) < 32 for c in value):
            raise ValueError("select explicit cluster target names")
    if args.timeout <= 0 or not files.absolute(args.output):
        raise ValueError("use a positive timeout and a new absolute output directory")
    if args.action == "after":
        if args.project_id <= 0 or not all(files.absolute(getattr(args, name)) for name in ("config", "engine_bin", "tracker_bin", "queue")):
            raise ValueError("select an explicit project and absolute installed paths")
        for url, code in args.status_url:
            endpoint(url, https=True)
            if not 100 <= int(code) <= 599 or 300 <= int(code) <= 399:
                raise ValueError("redirects cannot be a successful status observation")
    return args


def main(argv=None):
    args = arguments(argv)
    targets = files.decode(files.input_text(args.targets))
    if not isinstance(targets, list) or not targets:
        raise ValueError("supply explicit targets")
    names = set()
    for target in targets:
        endpoint(target["url"])
        if not isinstance(target["name"], str) or not target["name"] or target["name"] in names or target["expect"] not in ("connected", "refused"):
            raise ValueError("targets need distinct names and explicit expectations")
        names.add(target["name"])
    if {target["expect"] for target in targets} != {"connected", "refused"}:
        raise ValueError("include both connected and refused expectations")

    common = ["--context", args.context, "--namespace", args.namespace,
              "--pod", args.pod, "--container", args.container, "--kubectl", args.kubectl,
              "--timeout", str(args.timeout)]
    remote = [args.kubectl, "--context", args.context, "--namespace", args.namespace,
              "exec", "-i", args.pod, "-c", args.container, "--"]
    operations = Path(__file__).resolve().parent
    checks, exits = {}, {}

    def run(name, command, payload=None):
        try:
            result = subprocess.run(command, input=payload, stdout=subprocess.PIPE,
                                    stderr=subprocess.DEVNULL, timeout=args.timeout)
            exits[name] = result.returncode
            checks[name] = result.returncode == 0
            return result.stdout if checks[name] else None
        except subprocess.TimeoutExpired:
            exits[name], checks[name] = "timeout", False
        except OSError:
            exits[name], checks[name] = "could-not-start", False

    parent, name = os.path.split(args.output)
    with files.directory(parent) as destination:
        os.mkdir(name, 0o700, dir_fd=destination)
        root = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=destination)
        try:
            files.save(root, "selection.json", {key: getattr(args, key) for key in ("context", "namespace", "pod", "container")})
            code = Path(__file__).read_bytes()
            payload = str(len(code)).encode() + b"\n" + code + json.dumps({"targets": targets, "timeout": min(3, args.timeout)}).encode()
            bootstrap = "import sys; n=int(sys.stdin.buffer.readline()); code=sys.stdin.buffer.read(n); exec(compile(code,'network_probe','exec'),{'__name__':'__remote__'})"
            raw = run("network", remote + ["python3", "-B", "-c", bootstrap], payload)
            network = {"ok": False, "targets": []}
            try:
                if raw is not None:
                    observed = files.decode(raw)
                    if not isinstance(observed, dict):
                        raise ValueError("network observation is unreadable")
                    network = observed
                    checks["network"] = network_summary(network) == sorted((t["name"], t["url"], t["expect"], t["expect"]) for t in targets)
            except (ValueError, KeyError, TypeError):
                checks["network"] = False
            network["ok"] = checks["network"]
            files.save(root, "network.json" if args.action == "after" else "result.json", network)

            if args.action == "after":
                try:
                    checks["network_baseline"] = network_summary(network) == network_summary(files.decode(files.input_text(args.egress_baseline)))
                except (OSError, ValueError, KeyError, TypeError, files.HelperError):
                    checks["network_baseline"] = False
                run("config", remote + [args.engine_bin, "--config", args.config, "--check"])
                statuses = []
                opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), files.NoRedirect())
                for url, expected in args.status_url:
                    try:
                        try:
                            with opener.open(url, timeout=args.timeout) as response:
                                code = response.status
                        except urllib.error.HTTPError as error:
                            code = error.code
                            error.close()
                    except Exception:
                        code = "unavailable"
                    statuses.append(code)
                exits["status"] = statuses
                checks["status"] = statuses == [int(code) for _, code in args.status_url]
                issue_output = str(Path(args.output) / "issues")
                run("issues", ["sh", str(operations / "read-issues.sh"), *common, "--source", "backlog",
                               "--config", args.config, "--tracker-bin", args.tracker_bin,
                               "--project-id", str(args.project_id), "--output", issue_output])
                try:
                    checks["issues_baseline"] = checks["issues"] and issue_summary(files.decode(files.input_text(str(Path(issue_output) / "result.json"))), args.project_id) == \
                        issue_summary(files.decode(files.input_text(args.issues_baseline)), args.project_id)
                except (OSError, ValueError, KeyError, TypeError, files.HelperError):
                    checks["issues_baseline"] = False
                raw = run("idle", ["sh", str(operations / "idle-check.sh"), *common, "--queue", args.queue])
                try:
                    checks["idle"] = checks["idle"] and files.decode(raw).get("idle") is True
                except (ValueError, AttributeError, TypeError):
                    checks["idle"] = False
                files.save(root, "result.json", {"ok": all(checks.values()), "checks": checks, "exits": exits})
        finally:
            os.close(root)
    ok = all(checks.values())
    print("checks passed" if ok else "checks failed; inspect the saved results")
    return 0 if ok else 1


if __name__ == "__remote__":
    request = json.load(sys.stdin.buffer)
    print(json.dumps(probe_targets(request["targets"], request["timeout"])))
elif __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception:
        print("checks not confirmed; any reserved output is retained", file=sys.stderr)
        raise SystemExit(1)
