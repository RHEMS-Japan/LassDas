"""Observe copied queue records offline; not full job/tick coverage or a sandbox."""
import argparse
import json
import os
from pathlib import Path
import re
import signal
import stat
import subprocess
import sys

from queue_helper import QueueError, directory


def read_file(path):
    if not os.path.isabs(path) or ".." in Path(path).parts:
        raise QueueError("input paths must be absolute without parent traversal")
    parent, name = os.path.split(path)
    with directory(parent) as root:
        fd = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=root)
        with os.fdopen(fd, "rb") as source:
            info = os.fstat(source.fileno())
            if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1:
                raise QueueError("input must be an ordinary unlinked file")
            return source.read()


def run_tool(command, environment, **options):
    result = subprocess.run(command, env=environment, capture_output=True, timeout=30, **options)
    if result.returncode != 0:
        raise QueueError("candidate inspection failed")
    return result.stdout


def valid_result(result, duration):
    names = {"records", "reads", "required_reads", "observed_ms", "normal_exit", "full_tick_coverage"}
    return (isinstance(result, dict) and set(result) == names and
            all(type(result[name]) is int and result[name] > 0 for name in
                ("records", "reads", "required_reads", "observed_ms")) and
            result["reads"] >= result["required_reads"] and result["observed_ms"] >= duration and
            result["normal_exit"] is True and result["full_tick_coverage"] is False)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("source", "commit", "queue", "config", "reads", "output"):
        parser.add_argument("--" + name, required=True)
    parser.add_argument("--seconds", type=int, default=2, help="observation duration, 1 to 60 seconds")
    parser.add_argument("--git", default="git", help="git executable, never a shell command")
    parser.add_argument("--go", default="go", help="installed Go executable; no automatic download")
    args = parser.parse_args()
    if not re.fullmatch(r"[a-fA-F0-9]{40}|[a-fA-F0-9]{64}", args.commit) or not 1 <= args.seconds <= 60:
        raise QueueError("name an exact commit and a duration from one to sixty seconds")
    for path in (args.source, args.queue):
        with directory(path):
            pass
    if not os.path.isabs(args.output) or ".." in Path(args.output).parts:
        raise QueueError("output must be a new absolute directory")
    if any(Path(args.output).is_relative_to(Path(path)) for path in (args.source, args.queue)):
        raise QueueError("output must be outside the source checkout and input queue")
    parent, name = os.path.split(args.output)
    with directory(parent):
        if not name or os.path.lexists(args.output):
            raise QueueError("output already exists or does not name a new directory")
    config, reads = read_file(args.config), read_file(args.reads)
    support = Path(args.source) / "rewrite/cmd/engine/rehearsal_test.go"
    if b"func TestRehearsalOfflineQueue(" not in read_file(str(support)):
        raise QueueError("candidate does not ship the required offline rehearsal support")
    # Do not inherit credentials, Go flags, proxy commands or personal Git settings.
    environment = dict(PATH=os.environ.get("PATH", ""), LANG="C.UTF-8", GIT_CONFIG_NOSYSTEM="1",
                       GIT_CONFIG_GLOBAL=os.devnull, GIT_CONFIG_SYSTEM=os.devnull,
                       GIT_OPTIONAL_LOCKS="0", GIT_TERMINAL_PROMPT="0")
    command = [args.git, "-C", args.source]
    if run_tool(command + ["rev-parse", "HEAD"], environment).decode().strip().lower() != args.commit.lower():
        raise QueueError("source checkout is not at the selected commit")
    if run_tool(command + ["status", "--porcelain", "-z", "--untracked-files=all", "--ignored"], environment):
        raise QueueError("source checkout must be clean, including untracked and ignored files")
    with directory(parent) as destination:
        os.mkdir(name, 0o700, dir_fd=destination)
    output = Path(args.output)
    for child in ("home", "cache", "tmp", "modules"):
        (output / child).mkdir(mode=0o700)
    for name, data in (("config.json", config), ("reads.json", reads)):
        fd = os.open(output / name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
        with os.fdopen(fd, "wb") as target:
            target.write(data)
    environment.update(HOME=str(output / "home"), GOCACHE=str(output / "cache"), TMPDIR=str(output / "tmp"),
                       GOPATH=str(output / "home"), GOMODCACHE=str(output / "modules"), GOENV="off",
                       GOTOOLCHAIN="local", GOPROXY="off", GOSUMDB="off", GOWORK="off", CGO_ENABLED="0",
                       GOMAXPROCS="2", GOFLAGS="-p=1", REHEARSAL_QUEUE=args.queue,
                       REHEARSAL_CONFIG=str(output / "config.json"), REHEARSAL_READS=str(output / "reads.json"),
                       REHEARSAL_RESULT=str(output / "result.json"), REHEARSAL_MS=str(args.seconds * 1000))
    fd = os.open(output / "run.log", os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, "wb") as log:
        process = subprocess.Popen([args.go, "test", "-mod=readonly", "-p", "1", "-count=1",
                                    "-timeout", str(args.seconds + 30) + "s", "./cmd/engine",
                                    "-run", "^TestRehearsalOfflineQueue$"],
                                   cwd=str(Path(args.source) / "rewrite"), env=environment,
                                   stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
        try:
            code = process.wait(timeout=args.seconds + 180)
        except subprocess.TimeoutExpired:
            os.killpg(process.pid, signal.SIGTERM)
            try:
                process.wait(timeout=2)
            except subprocess.TimeoutExpired:
                os.killpg(process.pid, signal.SIGKILL)
                process.wait()
            raise QueueError("offline rehearsal timed out") from None
    if code != 0:
        raise QueueError("offline rehearsal failed; inspect the retained private output")
    result = json.loads(read_file(str(output / "result.json")))
    if not valid_result(result, args.seconds * 1000):
        raise QueueError("offline rehearsal lacks read coverage or normal-exit evidence")
    print(json.dumps(result, sort_keys=True))
    print("No write was observed under the supplied offline assumptions; not full job/tick coverage or deployment approval.")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except QueueError as error:
        print(str(error) + "; no successful rehearsal. Any new private output is retained.", file=sys.stderr)
        sys.exit(2)
    except (OSError, ValueError, subprocess.SubprocessError):
        print("rehearsal failed; no successful rehearsal. Any new private output is retained.", file=sys.stderr)
        sys.exit(2)
