"""Read queue records through an explicitly selected Pod; never run the engine."""
import argparse
import contextlib
import json
import os
from pathlib import Path, PurePosixPath
import shutil
import stat
import subprocess
import sys
import tarfile
import tempfile


class QueueError(Exception):
    pass


@contextlib.contextmanager
def directory(path):
    """Pin each real directory; neither an ancestor nor the leaf may be a link."""
    if not os.path.isabs(path) or ".." in Path(path).parts:
        raise QueueError("use an absolute path without parent traversal")
    fd = os.open("/", os.O_RDONLY | os.O_DIRECTORY)
    try:
        for part in Path(path).parts[1:]:
            child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
            os.close(fd)
            fd = child
        yield fd
    finally:
        os.close(fd)


@contextlib.contextmanager
def child_directory(parent, name):
    fd = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=parent)
    try:
        yield fd
    finally:
        os.close(fd)


def excluded(name, is_directory):
    if is_directory:
        # Private staging a cut checkout preparation or refresh leaves behind.
        return name in {"workspace", "homes", "live"} or name.startswith((".source-", ".refresh-"))
    return name in {"engine.log", "runner.lock"}


def archive_queue(root, archive, prefix=""):
    with os.scandir(root) as entries:
        for entry in sorted(entries, key=lambda item: item.name):
            info = entry.stat(follow_symlinks=False)
            if excluded(entry.name, stat.S_ISDIR(info.st_mode)):
                continue
            name = prefix + entry.name
            item = tarfile.TarInfo(name)
            item.mtime = info.st_mtime
            if stat.S_ISDIR(info.st_mode):
                item.type, item.mode = tarfile.DIRTYPE, 0o700
                archive.addfile(item)
                with child_directory(root, entry.name) as child:
                    archive_queue(child, archive, name + "/")
            elif stat.S_ISREG(info.st_mode) and info.st_nlink == 1:
                fd = os.open(entry.name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=root)
                with os.fdopen(fd, "rb") as source:
                    before = os.fstat(source.fileno())
                    if not stat.S_ISREG(before.st_mode) or before.st_nlink != 1:
                        raise QueueError("queue contains a non-regular or linked file")
                    item.size, item.mode, item.mtime = before.st_size, 0o600, before.st_mtime
                    archive.addfile(item, source)
                    after = os.fstat(source.fileno())
                    if (before.st_size, before.st_mtime_ns) != (after.st_size, after.st_mtime_ns):
                        raise QueueError("a queue record changed while being copied")
            else:
                raise QueueError("queue contains a link or special file")


def read_json(parent, name):
    fd = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=parent)
    with os.fdopen(fd, "r", encoding="utf-8") as source:
        info = os.fstat(source.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1:
            raise QueueError("queue record is not a regular file with one link")
        result = json.load(source)
    if not isinstance(result, dict):
        raise QueueError("queue record is not an object")
    return result


def history(parent, name):
    with child_directory(parent, name) as run:
        record = read_json(run, "history.json")
    if type(record.get("done")) is not bool or not isinstance(record.get("request"), str):
        raise QueueError("history is unreadable")
    if record.get("history") is not None and not isinstance(record["history"], list):
        raise QueueError("history is unreadable")
    for flag in ("waiting", "recovering"):
        if flag in record and type(record[flag]) is not bool:
            raise QueueError("history is unreadable")
    if record.get("pending") is not None and not isinstance(record["pending"], dict):
        raise QueueError("history is unreadable")
    if record["done"] and (record.get("pending") is not None or record.get("waiting") or record.get("recovering")):
        raise QueueError("history has conflicting states")
    return record


def inspect_job(job, counts):
    if not read_json(job, "issue.json"):
        raise QueueError("accepted issue record is empty")
    try:
        with child_directory(job, "live") as live:
            if os.listdir(live):
                counts["running"] += 1
    except FileNotFoundError:
        pass
    original = history(job, "run")
    stopped = False
    try:
        stopped = bool(read_json(job, "stop-request.json"))
        if not stopped:
            raise QueueError("stop record is empty")
    except FileNotFoundError:
        pass
    try:
        report = history(job, "stop-report")
        if not report["done"]:
            counts["stop_report_pending"] += 1
    except FileNotFoundError:
        if stopped:
            # No configuration is read: an absent report does not prove that
            # its role was disabled, so an operator must inspect this case.
            counts["unknown"] += 1
    if stopped:
        counts["stopped"] += 1
    elif original.get("waiting"):
        counts["waiting"] += 1
    elif not original["done"]:
        counts["unfinished"] += 1


def inspect_queue(root):
    counts = dict(jobs=0, running=0, waiting=0, unfinished=0, stopped=0,
                  stop_report_pending=0, unknown=0)
    with child_directory(root, "jobs") as jobs:
        for name in sorted(os.listdir(jobs)):
            if name == "notice-kinds.json":
                continue
            counts["jobs"] += 1
            try:
                if not name.isascii() or not name.isdecimal() or int(name) <= 0:
                    raise QueueError("unrecognized queue entry")
                with child_directory(jobs, name) as job:
                    inspect_job(job, counts)
            except (OSError, ValueError, QueueError):
                counts["unknown"] += 1
    counts["idle"] = not any(counts[name] for name in
                             ("running", "waiting", "unfinished", "stop_report_pending", "unknown"))
    return counts


def validate_archive(archive):
    seen = {}
    for member in archive.getmembers():
        path = PurePosixPath(member.name)
        if path.is_absolute() or not path.parts or ".." in path.parts or "\\" in member.name:
            raise QueueError("copy contains an unsafe archive path")
        name = str(path)
        if name in seen or not (member.isfile() or member.isdir()):
            raise QueueError("copy contains duplicate paths, links or special files")
        seen[name] = member.isdir()
    for name in seen:
        if any(str(parent) in seen and not seen[str(parent)] for parent in PurePosixPath(name).parents):
            raise QueueError("copy contains a file used as a directory")
    if not seen.get("jobs"):
        raise QueueError("copy does not contain a jobs directory")


def extract_archive(archive, output):
    # Validate before creating the destination. No extractall: only ordinary
    # directories and exclusively created files are allowed in this copy.
    validate_archive(archive)
    parent, name = os.path.split(output)
    if not name:
        raise QueueError("output must name a new directory")
    with directory(parent) as destination:
        os.mkdir(name, 0o700, dir_fd=destination)
        with child_directory(destination, name) as root:
            for member in archive.getmembers():
                parts = PurePosixPath(member.name).parts
                with contextlib.ExitStack() as stack:
                    current = root
                    for part in parts[:-1] if member.isfile() else parts:
                        try:
                            os.mkdir(part, 0o700, dir_fd=current)
                        except FileExistsError:
                            pass
                        current = stack.enter_context(child_directory(current, part))
                    if member.isfile():
                        fd = os.open(parts[-1], os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
                                     0o600, dir_fd=current)
                        with os.fdopen(fd, "wb") as target, archive.extractfile(member) as source:
                            shutil.copyfileobj(source, target)
                        os.utime(parts[-1], (member.mtime, member.mtime), dir_fd=current, follow_symlinks=False)


def run_remote(args, output):
    command = [args.kubectl, "--context", args.context, "--namespace", args.namespace,
               "exec", "-i", args.pod, "-c", args.container, "--", "python3", "-B", "-",
               "--remote-" + args.action, args.queue]
    # The source contains no credentials. Do not relay kubectl diagnostics,
    # request records or arbitrary remote output into a terminal or CI log.
    result = subprocess.run(command, input=Path(__file__).read_bytes(), stdout=output,
                            stderr=subprocess.DEVNULL, timeout=args.timeout)
    if result.returncode != 0:
        raise QueueError("remote queue read failed; no successful copy or idle result is available")


def main():
    if len(sys.argv) == 3 and sys.argv[1] in {"--remote-copy", "--remote-idle"}:
        with directory(sys.argv[2]) as root:
            if sys.argv[1] == "--remote-copy":
                with tarfile.open(fileobj=sys.stdout.buffer, mode="w|") as archive:
                    archive_queue(root, archive)
            else:
                print(json.dumps(inspect_queue(root), sort_keys=True))
        return 0
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("copy", "idle"))
    for option in ("context", "namespace", "pod", "container", "queue"):
        parser.add_argument("--" + option, required=True)
    parser.add_argument("--kubectl", default="kubectl", help="kubectl executable, never a shell command")
    parser.add_argument("--timeout", type=int, default=300, help="remote read timeout in seconds")
    parser.add_argument("--output", help="new absolute local directory, for copy only")
    args = parser.parse_args()
    targets = (args.context, args.namespace, args.pod, args.container)
    if any(not value or value != value.strip() or value.startswith("-") or
           any(ord(char) < 32 for char in value) for value in targets):
        raise QueueError("context, namespace, pod and container must be explicit nonempty names")
    if args.timeout <= 0 or not os.path.isabs(args.queue) or args.queue == "/" or ".." in Path(args.queue).parts:
        raise QueueError("use a positive timeout and an explicit absolute queue directory")
    if args.action == "copy":
        if not args.output or not os.path.isabs(args.output) or ".." in Path(args.output).parts:
            raise QueueError("copy requires a new absolute output directory")
        parent, name = os.path.split(args.output)
        with directory(parent) as destination:
            if not name or os.path.lexists(args.output):
                raise QueueError("output already exists or does not name a new directory")
    elif args.output:
        raise QueueError("idle does not write an output directory")
    with tempfile.TemporaryFile() as received:
        run_remote(args, received)
        received.seek(0)
        if args.action == "copy":
            with tarfile.open(fileobj=received, mode="r:") as archive:
                extract_archive(archive, args.output)
            print("queue records copied; this is not an atomic snapshot")
            return 0
        result = json.load(received)
        names = {"jobs", "running", "waiting", "unfinished", "stopped", "stop_report_pending", "unknown"}
        if not isinstance(result, dict) or set(result) != names | {"idle"} or any(type(result[name]) is not int or result[name] < 0 for name in names):
            raise QueueError("remote idle result is unreadable")
        expected = not any(result[name] for name in names - {"jobs", "stopped"})
        if type(result["idle"]) is not bool or result["idle"] != expected:
            raise QueueError("remote idle result is inconsistent")
        print(json.dumps(result, sort_keys=True))
        return 0 if result["idle"] else 1


if __name__ == "__main__":
    try:
        sys.exit(main())
    except QueueError as error:
        print(str(error) + "; no successful result. Any new partial output is retained for inspection.", file=sys.stderr)
        sys.exit(2)
    except (OSError, ValueError, tarfile.TarError, subprocess.SubprocessError):
        print("queue check/copy failed; no successful result. Any new partial output is retained for inspection.", file=sys.stderr)
        sys.exit(2)
