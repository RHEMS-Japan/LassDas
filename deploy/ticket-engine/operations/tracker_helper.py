"""Explicit Backlog reads or one issue creation; credentials stay in the Pod."""
import argparse
import contextlib
import datetime
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import sys
import urllib.parse
import urllib.request


class HelperError(Exception):
    pass


def positive(value):
    return type(value) is int and value > 0


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise HelperError("duplicate JSON member")
        result[key] = value
    return result


def decode(value):
    return json.loads(value, object_pairs_hook=unique_object)


def absolute(path):
    return isinstance(path, str) and os.path.isabs(path) and path != "/" and ".." not in Path(path).parts


@contextlib.contextmanager
def directory(path):
    if not os.path.isabs(path) or ".." in Path(path).parts:
        raise HelperError("use an absolute path without parent traversal")
    fd = os.open("/", os.O_RDONLY | os.O_DIRECTORY)
    try:
        for part in Path(path).parts[1:]:
            child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
            os.close(fd)
            fd = child
        yield fd
    finally:
        os.close(fd)


def input_text(path):
    if not absolute(path):
        raise HelperError("input must be an absolute file path")
    parent, name = os.path.split(path)
    with directory(parent) as root:
        fd = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=root)
        with os.fdopen(fd, "r", encoding="utf-8", newline="") as source:
            info = os.fstat(source.fileno())
            if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1:
                raise HelperError("input must be an ordinary file with one link")
            text = source.read()
    if any(ord(char) < 32 and char not in "\n\r\t" for char in text):
        raise HelperError("input contains unsupported control characters")
    return text


def private_file(root, name):
    return os.fdopen(os.open(name, os.O_RDWR | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
                             0o600, dir_fd=root), "w+b")


def save(root, name, value):
    with private_file(root, name) as output:
        output.write(json.dumps(value, ensure_ascii=False, allow_nan=False).encode())
        output.flush()
        os.fsync(output.fileno())


def now():
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def identity(issue, project):
    if not isinstance(issue, dict) or not positive(issue.get("id")) or issue.get("projectId") != project:
        raise HelperError("issue identity does not match the selected project")
    if not isinstance(issue.get("issueKey"), str) or not re.fullmatch(r"[A-Za-z0-9_]+-[1-9][0-9]*", issue["issueKey"]):
        raise HelperError("issue key is unreadable")
    return {key: issue[key] for key in ("id", "projectId", "issueKey")}


def metadata(issue, project):
    result = identity(issue, project)
    for key in ("status", "assignee"):
        item = issue.get(key)
        if item is not None and (not isinstance(item, dict) or not positive(item.get("id"))):
            raise HelperError("issue metadata is unreadable")
        result[key + "Id"] = item["id"] if item else None
    categories = issue.get("category", [])
    if not isinstance(categories, list) or any(not isinstance(item, dict) or not positive(item.get("id")) for item in categories):
        raise HelperError("issue categories are unreadable")
    result["categoryIds"] = sorted(item["id"] for item in categories)
    for key in ("estimatedHours", "actualHours"):
        value = issue.get(key)
        if value is not None and type(value) not in (int, float):
            raise HelperError("issue hours are unreadable")
        result[key] = value
    result["updated"] = issue.get("updated")
    if result["updated"] is not None and not isinstance(result["updated"], str):
        raise HelperError("issue update time is unreadable")
    return result


def selected_config(request):
    # A mounted configuration may use the platform's symlinks. It is read
    # inside the explicitly selected Pod and is never copied to the host.
    with open(request["config"], encoding="utf-8") as source:
        config = decode(source.read())
    if not isinstance(config, dict) or config.get("github") is not None:
        raise HelperError("this helper supports only Backlog configuration")
    backlog, intake = config.get("backlog"), config.get("intake")
    if not isinstance(backlog, dict) or not isinstance(intake, dict) or intake.get("project_id") != request["project"]:
        raise HelperError("configuration does not match the explicit project")
    base, key_env = backlog.get("base_url"), backlog.get("key_env")
    if not isinstance(base, str) or not isinstance(key_env, str) or not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", key_env):
        raise HelperError("Backlog connection is unavailable")
    parsed = urllib.parse.urlsplit(base)
    if parsed.scheme != "https" or not parsed.hostname or parsed.username is not None or parsed.password is not None or parsed.query or parsed.fragment:
        raise HelperError("Backlog requires a credential-free HTTPS base URL")
    key = os.environ.get(key_env)
    if not key:
        raise HelperError("credential environment is unavailable")
    return base.rstrip("/"), key_env, key


def emit(event, key):
    encoded = json.dumps(event, ensure_ascii=False, allow_nan=False)
    if key in encoded or json.dumps(key, ensure_ascii=False)[1:-1] in encoded:
        raise HelperError("response contains credential material")
    print(encoded, flush=True)


def cli_read(request, base, key_env, action, issue=None):
    command = [request["tracker"], "--base-url", base, "--key-env", key_env]
    command += ["--project-id", str(request["project"])] if action == "issues" else ["--issue", str(issue)]
    result = subprocess.run(command + [action], capture_output=True, timeout=request["timeout"])
    if result.returncode != 0:
        raise HelperError("tracker read failed")
    value = decode(result.stdout)
    if not isinstance(value, list):
        raise HelperError("tracker read did not return an array")
    return value


def read_remote(request, base, key_env, key):
    # The shipped CLI owns pagination. A failing CLI never yields a partial
    # successful list, even if it printed valid JSON before its failing exit.
    issues = cli_read(request, base, key_env, "issues")
    summaries, ids = [], set()
    for issue in issues:
        summary = metadata(issue, request["project"])
        if summary["id"] in ids:
            raise HelperError("duplicate issue in tracker result")
        ids.add(summary["id"])
        summaries.append(summary)
    comments = {}
    for number in request["issues"]:
        if number not in ids:
            raise HelperError("selected comment issue is outside the returned project")
        rows = cli_read(request, base, key_env, "comments", number)
        previous = 0
        for row in rows:
            if not isinstance(row, dict) or not positive(row.get("id")) or row["id"] <= previous:
                raise HelperError("comments are not a complete ordered result")
            previous = row["id"]
        comments[str(number)] = rows if request["native"] else [{"id": row["id"]} for row in rows]
    event = {"phase": "complete", "action": "read", "project": request["project"],
             "issues": sorted(summaries, key=lambda row: row["id"]), "comments": comments,
             "atomic_snapshot": False, "rehearsal_fixture": False}
    if request["native"]:
        event["native"] = issues
    emit(event, key)


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def http_error_302(self, request, response, code, message, headers):
        raise HelperError("HTTP redirects are not followed")

    http_error_301 = http_error_303 = http_error_307 = http_error_308 = http_error_302


def api(opener, base, key, path, timeout, form=None):
    body = None if form is None else urllib.parse.urlencode(form).encode()
    request = urllib.request.Request(base + path + "?" + urllib.parse.urlencode({"apiKey": key}),
                                     data=body, method="GET" if form is None else "POST")
    if body is not None:
        request.add_header("Content-Type", "application/x-www-form-urlencoded")
    with opener.open(request, timeout=timeout) as response:
        if response.status != (200 if form is None else 201):
            raise HelperError("unexpected tracker response")
        return decode(response.read())


def create_remote(request, base, key):
    opener = urllib.request.build_opener(NoRedirect())
    for path, selected in (("/projects/%d/issueTypes" % request["project"], request["type"]),
                           ("/priorities", request["priority"])):
        choices = api(opener, base, key, path, request["timeout"])
        if not isinstance(choices, list) or not any(isinstance(item, dict) and positive(item.get("id")) and item["id"] == selected for item in choices):
            raise HelperError("explicit type or priority is not available")
    form = {"projectId": request["project"], "issueTypeId": request["type"],
            "priorityId": request["priority"], "summary": request["summary"],
            "description": request["description"]}
    # Exactly one POST. No retry, redirect, or fallback on any failure.
    created = api(opener, base, key, "/issues", request["timeout"], form)
    receipt = identity(created, request["project"])
    emit({"phase": "created", "receipt": receipt}, key)
    observed = api(opener, base, key, "/issues/%d" % receipt["id"], request["timeout"])
    if identity(observed, request["project"]) != receipt:
        raise HelperError("created issue did not match its readback")
    emit({"phase": "complete", "action": "create", "project": request["project"], "receipt": receipt}, key)


def remote_main():
    try:
        request = decode(sys.stdin.buffer.read())
        base, key_env, key = selected_config(request)
        if request["action"] == "read":
            read_remote(request, base, key_env, key)
        elif request["action"] == "create":
            create_remote(request, base, key)
        else:
            raise HelperError("unsupported action")
        return 0
    except Exception:
        # Native HTTP and CLI exceptions can contain credentials and bodies.
        print("remote operation was not confirmed", file=sys.stderr)
        return 1


BOOTSTRAP = "import sys; n=int(sys.stdin.buffer.readline()); code=sys.stdin.buffer.read(n); exec(compile(code,'tracker_helper','exec'),{'__name__':'__remote__'})"


def run_remote(args, request, output):
    code = Path(__file__).read_bytes()
    payload = str(len(code)).encode() + b"\n" + code + json.dumps(request, ensure_ascii=False).encode()
    command = [args.kubectl, "--context", args.context, "--namespace", args.namespace,
               "exec", "-i", args.pod, "-c", args.container, "--", "python3", "-B", "-c", BOOTSTRAP]
    return subprocess.run(command, input=payload, stdout=output, stderr=subprocess.DEVNULL,
                          timeout=args.timeout).returncode


def complete(events, action, project):
    phases = [item.get("phase") for item in events if isinstance(item, dict)]
    if phases != (["complete"] if action == "read" else ["created", "complete"]):
        raise HelperError("remote completion evidence is missing")
    final = events[-1]
    if final.get("action") != action or final.get("project") != project:
        raise HelperError("remote completion target is inconsistent")
    if action == "create":
        if identity(final.get("receipt"), project) != identity(events[0].get("receipt"), project):
            raise HelperError("remote receipt is inconsistent")
    elif not isinstance(final.get("issues"), list) or not isinstance(final.get("comments"), dict) or final.get("atomic_snapshot") is not False or final.get("rehearsal_fixture") is not False:
        raise HelperError("remote read evidence is incomplete")
    return final


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("read", "create"))
    for name in ("context", "namespace", "pod", "container", "config", "output"):
        parser.add_argument("--" + name, required=True)
    parser.add_argument("--source", choices=("backlog",), required=True)
    parser.add_argument("--project-id", type=int, required=True)
    parser.add_argument("--kubectl", default="kubectl")
    parser.add_argument("--timeout", type=int, default=300)
    parser.add_argument("--tracker-bin")
    parser.add_argument("--issue-id", type=int, action="append", default=[])
    parser.add_argument("--native", action="store_true", help="include native bodies in private read output")
    parser.add_argument("--type-id", type=int)
    parser.add_argument("--priority-id", type=int)
    parser.add_argument("--summary-file")
    parser.add_argument("--description-file")
    args = parser.parse_args()
    for value in (args.context, args.namespace, args.pod, args.container):
        if not value or value != value.strip() or value.startswith("-") or any(ord(char) < 32 for char in value):
            raise HelperError("select explicit nonempty cluster target names")
    if not positive(args.project_id) or not positive(args.timeout) or not absolute(args.config) or not absolute(args.output):
        raise HelperError("select a positive project and timeout, absolute config and new output")
    request = {"action": args.action, "config": args.config, "project": args.project_id, "timeout": args.timeout}
    if args.action == "read":
        if not absolute(args.tracker_bin) or any(not positive(number) for number in args.issue_id) or len(set(args.issue_id)) != len(args.issue_id):
            raise HelperError("reads require an absolute tracker binary and distinct positive issue IDs")
        if any(value is not None for value in (args.type_id, args.priority_id, args.summary_file, args.description_file)):
            raise HelperError("creation arguments do not belong to a read")
        request.update(tracker=args.tracker_bin, issues=args.issue_id, native=args.native)
    else:
        if not positive(args.type_id) or not positive(args.priority_id) or args.native or args.issue_id or args.tracker_bin:
            raise HelperError("creation requires explicit type and priority, without read options")
        summary, description = input_text(args.summary_file), input_text(args.description_file)
        if not summary.strip():
            raise HelperError("summary must not be empty")
        request.update(type=args.type_id, priority=args.priority_id, summary=summary, description=description)
    parent, name = os.path.split(args.output)
    with directory(parent) as destination:
        os.mkdir(name, 0o700, dir_fd=destination)
        root = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=destination)
        try:
            save(root, "intent.json", dict(request, started=now(), target={key: getattr(args, key) for key in ("context", "namespace", "pod", "container")}))
            with private_file(root, "received.jsonl") as received:
                rc = run_remote(args, request, received)
                received.flush()
                os.fsync(received.fileno())
                received.seek(0)
                events = [decode(line) for line in received if line.strip()]
            if rc != 0:
                raise HelperError("remote command failed")
            result = complete(events, args.action, args.project_id)
            save(root, "result.json", dict(result, finished=now()))
        finally:
            os.close(root)
    if args.action == "read":
        print("read %d issues; private output saved; not an atomic snapshot or rehearsal fixture" % len(result["issues"]))
    else:
        print("one issue created and read back; private receipt saved")
    return 0


if __name__ == "__remote__":
    raise SystemExit(remote_main())
if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception:
        print("operation not confirmed; private output is retained if reserved. Creation may have occurred: do not blindly resubmit, even with a new output; inspect the selected project first.", file=sys.stderr)
        raise SystemExit(1)
