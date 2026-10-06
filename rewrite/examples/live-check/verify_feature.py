"""A live check of the running change, as an installation could write one.

This is the example of deploy/ticket-engine/SETUP.md, "A live check of the
running change (optional)". It checks one feature of the fictional project
beside it: server.py, started from the project's checkout, greets a user with
the first line of src/greeting.txt. An installation writes its own command for
its own service; the engine looks for none of the names used here.

  verify_feature.py run --map docs/FEATURES.md [--output DIRECTORY]

runs the five subcommands below in order. Each can also be run by hand on a
launch's directory (--output):

  start      starts server.py from the checkout on 127.0.0.1 and records what
             it started (state.json); 0 once /health answers 200
  diagnose   checks what using the feature needs: a way to start it, a way to
             use it (the Feature Map's row) and the agreed test user; 0 when
             nothing is missing, otherwise 1, naming what is
  act        records the test user it will create (created.json), creates it,
             asks for the greeting as that user and keeps the request and the
             answer; 0 only when the answer is what the Feature Map says shows
             the feature working
  evidence   prints what was requested and observed
  stop       stops the service this launch started and removes the test user
             it created, keeping every evidence file; 0 when nothing of them
             is left, also when nothing was started or all was removed before

run exits 0 only when every one of them did. Before it starts anything it runs
stop on the directories of earlier launches without a record of a complete
stop: a launch that was killed did not clean up. SIGTERM or SIGINT ends the
operation under way; the evidence is still printed and the cleanup still
done, and run exits 1. The observation is printed last, because the engine's
record and the merged check keep the end of a long output.

Each launch writes in a directory of its own under $TASK_HOME/logs/live-check,
which the engine keeps when it trims a finished request's home. The service's
data, where the test users are, is $TASK_HOME/live-check-store.
LIVE_CHECK_TEST_USER names the agreed test user. Nothing is started through a
shell, nothing is written in the checkout and nothing is read from standard
input.
"""
import argparse
import datetime
import json
import os
from pathlib import Path
import secrets
import signal
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

FEATURE = "挨拶を表示する"
START_SECONDS = 20
REQUEST_SECONDS = 15
STOP_SECONDS = 2
SHOWN = 200  # characters of an answer shown in the observation; response.txt has all of it

# Loopback only, whatever proxy the environment names.
OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))


class Interrupted(Exception):
    """A SIGTERM or SIGINT arrived; the operation under way is given up."""


def say(text):
    print(text, flush=True)


def now():
    return datetime.datetime.now(datetime.timezone.utc).isoformat(timespec="seconds")


def home():
    value = os.environ.get("TASK_HOME") or os.environ.get("HOME")
    if not value:
        raise SystemExit("neither TASK_HOME nor HOME is set")
    return Path(value)


def store():
    return home() / "live-check-store"


def read_json(path):
    try:
        return json.loads(Path(path).read_text(encoding="utf-8"))
    except FileNotFoundError:
        return None


def write_json(path, value):
    path = Path(path)
    written = path.with_name(path.name + ".new")
    written.write_text(json.dumps(value, ensure_ascii=False, indent=1) + "\n", encoding="utf-8")
    os.replace(written, path)


def request(method, url, body=None):
    """(status, text) of one HTTP request; status 0 when no answer came."""
    data = None if body is None else json.dumps(body, ensure_ascii=False).encode("utf-8")
    headers = {"Content-Type": "application/json"} if data is not None else {}
    try:
        with OPENER.open(urllib.request.Request(url, data=data, method=method, headers=headers),
                         timeout=REQUEST_SECONDS) as answer:
            return answer.status, answer.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as error:
        return error.code, error.read().decode("utf-8", "replace")
    except (urllib.error.URLError, OSError) as error:
        return 0, str(error)


def feature_row(map_path):
    """The cells of the Feature Map's row for FEATURE, or None. Only the first
    cell is looked for; the map needs no other form."""
    try:
        lines = Path(map_path).read_text(encoding="utf-8").splitlines()
    except (OSError, UnicodeDecodeError):
        return None
    for line in lines:
        cells = [cell.strip() for cell in line.strip().strip("|").split("|")]
        if cells and cells[0] == FEATURE:
            return cells
    return None


def process_state(pid, token):
    """'gone', 'ours' or 'other': whether the recorded process still runs, and
    whether it is the one the launch started, whose arguments carry the token.
    A process that has ended but not been collected counts as gone."""
    if Path("/proc/self").is_dir():
        try:
            status = Path("/proc/%d/stat" % pid).read_text().rpartition(")")[2].split()[0]
            arguments = Path("/proc/%d/cmdline" % pid).read_bytes().split(b"\0")
        except (FileNotFoundError, ProcessLookupError, IndexError):
            return "gone"
        if status in ("Z", "X"):
            return "gone"
        return "ours" if token.encode() in arguments else "other"
    listed = subprocess.run(["ps", "-o", "stat=", "-o", "command=", "-p", str(pid)],
                            stdin=subprocess.DEVNULL, capture_output=True, text=True)
    status, _, command = listed.stdout.strip().partition(" ")
    if listed.returncode != 0 or not status or status.startswith("Z"):
        return "gone"
    return "ours" if token in command.split() else "other"


def collect(pid):
    """Collect the exit of a service this process started itself."""
    try:
        os.waitpid(pid, os.WNOHANG)
    except ChildProcessError:
        pass


def start(out):
    state = read_json(out / "state.json")
    if state and process_state(state["pid"], state["token"]) == "ours":
        say("起動: すでに起動している (プロセス %d)" % state["pid"])
        return 0
    token = secrets.token_hex(8)
    address = out / "address"
    with open(out / "service.log", "ab") as log:
        try:
            # The service's output goes to its log, never to this command's
            # output: the engine waits for that to close.
            service = subprocess.Popen(
                [sys.executable, "-B", "server.py", "--root", ".", "--data", str(store()), "--port", "0",
                 "--address-file", str(address), "--token", token],
                stdin=subprocess.DEVNULL, stdout=log, stderr=log)
        except OSError as error:
            say("起動: 失敗: server.py を起動できない (%s)" % error)
            return 1
    write_json(out / "state.json", {"pid": service.pid, "token": token, "started_at": now(), "checkout": os.getcwd()})
    deadline = time.monotonic() + START_SECONDS
    while time.monotonic() < deadline:
        if service.poll() is not None:
            say("起動: 失敗: server.py が終了コード %s で終わった (service.log を見る)" % service.returncode)
            return 1
        try:
            port = int(address.read_text(encoding="utf-8"))
        except (FileNotFoundError, ValueError):
            time.sleep(0.05)
            continue
        url = "http://127.0.0.1:%d" % port
        if request("GET", url + "/health")[0] == 200:
            state = read_json(out / "state.json")
            state["url"] = url
            write_json(out / "state.json", state)
            say("起動: server.py をプロセス %d で起動した (%s)" % (service.pid, url))
            return 0
        time.sleep(0.05)
    say("起動: 失敗: %d 秒たっても /health が 200 を返さない" % START_SECONDS)
    return 1


def diagnose(out, map_path):
    missing = []
    state = read_json(out / "state.json")
    if not state or not state.get("url") or process_state(state["pid"], state["token"]) != "ours":
        missing.append("起動方法: server.py が起動していない")
    elif request("GET", state["url"] + "/health")[0] != 200:
        missing.append("起動方法: /health が 200 を返さない")
    if feature_row(map_path) is None:
        missing.append("操作する手段: %s に「%s」の行が無い" % (map_path, FEATURE))
    if not os.environ.get("LIVE_CHECK_TEST_USER", "").strip():
        missing.append("テスト用のユーザーとデータ: LIVE_CHECK_TEST_USER が設定されていない")
    elif not os.access(store() / "users", os.W_OK):
        missing.append("テスト用のユーザーとデータ: %s に書けない" % (store() / "users"))
    for item in missing:
        say("診断: 足りない: " + item)
    if not missing:
        say("診断: 起動方法・操作する手段・テスト用のユーザーとデータがそろっている")
    return 1 if missing else 0


def act(out, map_path):
    state = read_json(out / "state.json")
    name = os.environ.get("LIVE_CHECK_TEST_USER", "").strip()
    if not state or not state.get("url") or not name:
        say("操作: 始められない (起動の記録か LIVE_CHECK_TEST_USER が無い。diagnose を見る)")
        return 1
    user = "lc-" + state["token"][:8]
    # What will be created is on record before it exists, so a stop after an
    # interruption at any point knows what to remove.
    created = read_json(out / "created.json") or {"users": []}
    if user not in [entry["id"] for entry in created["users"]]:
        created["users"].append({"id": user, "name": name})
        write_json(out / "created.json", created)
    status, text = request("POST", state["url"] + "/users", {"id": user, "name": name})
    if status != 201:
        write_json(out / "observation.json", {"request": "POST %s/users" % state["url"], "status": status,
                                              "body": text, "expected": None, "passed": False,
                                              "observed_at": now()})
        say("操作: テスト用ユーザーを作れない (状態 %d)" % status)
        return 1
    try:
        lines = Path("src/greeting.txt").read_text(encoding="utf-8").splitlines()
    except (OSError, UnicodeDecodeError):
        lines = []
    first = lines[0] if lines else ""
    expected = first + ", " + name if first.strip() else None
    target = state["url"] + "/greeting?" + urllib.parse.urlencode({"user": user})
    (out / "request.txt").write_text("GET %s\n" % target, encoding="utf-8")
    status, text = request("GET", target)
    (out / "response.txt").write_text("status %d\n\n%s" % (status, text), encoding="utf-8")
    passed = expected is not None and status == 200 and text == expected
    write_json(out / "observation.json", {"request": "GET " + target, "status": status, "body": text,
                                          "expected": expected, "passed": passed, "observed_at": now()})
    say("操作: GET %s に状態 %d が返った" % (target, status))
    return 0 if passed else 1


def shown(text):
    text = text.replace("\n", " ")
    return text if len(text) <= SHOWN else text[:SHOWN] + "… (全文は response.txt)"


def evidence(out, map_path=None, note=""):
    """Print what was requested and observed, also after a failure. A launch
    that observed nothing gets observation.json saying why."""
    state = read_json(out / "state.json") or {}
    observation = read_json(out / "observation.json")
    if observation is None:
        sent = (out / "request.txt").read_text(encoding="utf-8").strip() if (out / "request.txt").exists() else None
        observation = {"request": sent, "status": None, "body": None, "expected": None, "passed": False,
                       "note": note or "操作していない", "observed_at": now()}
        write_json(out / "observation.json", observation)
    expected = observation.get("expected")
    say("ライブ確認 (マージ前の変更を、この checkout から起動したサービスで確かめた。本番ではない)")
    say("機能: %s (%s)" % (FEATURE, map_path or "Feature Map"))
    say("要求: " + (observation["request"] or "送っていない (%s)" % observation.get("note", "")))
    if state:
        say("対象: この checkout の server.py (プロセス %d、%s)" % (state["pid"], state.get("url", "アドレス不明")))
    else:
        say("対象: 起動していない")
    if expected is not None:
        say("期待: 状態 200、本文「%s」" % expected)
    else:
        say("期待: 状態 200、本文は src/greeting.txt の 1 行目に「, 」とテスト用ユーザーの名前が続くもの (1 行目が空なので決まらない)")
    if observation.get("status") is None:
        say("観測: 応答なし (%s)" % observation.get("note", ""))
    else:
        say("観測: 状態 %d、本文「%s」" % (observation["status"], shown(observation.get("body") or "")))
    say("合否: " + ("合格" if observation.get("passed") else "不合格"))
    files = [name for name in ("request.txt", "response.txt", "observation.json", "service.log") if (out / name).exists()]
    say("証拠: %s (%s)" % (out, "、".join(files)))
    return 0


def stop(out):
    done, problems = [], []
    state = read_json(out / "state.json")
    if state:
        pid, token = state["pid"], state["token"]
        found = process_state(pid, token)
        if found == "other":
            problems.append("プロセス %d は、この起動が渡した乱数を引数に持たない別のプロセスなので止めていない" % pid)
        elif found == "gone":
            collect(pid)
            done.append("サービス (プロセス %d) は止まっていた" % pid)
        else:
            for sent, wait in ((signal.SIGTERM, STOP_SECONDS), (signal.SIGKILL, 1)):
                try:
                    os.kill(pid, sent)
                except ProcessLookupError:
                    pass
                deadline = time.monotonic() + wait
                while time.monotonic() < deadline:
                    collect(pid)
                    if process_state(pid, token) == "gone":
                        break
                    time.sleep(0.02)
                if process_state(pid, token) == "gone":
                    break
            if process_state(pid, token) == "gone":
                done.append("サービス (プロセス %d) を止めた" % pid)
            else:
                problems.append("サービス (プロセス %d) が止まらない" % pid)
    created = read_json(out / "created.json") or {"users": []}
    for entry in created["users"]:
        try:
            (store() / "users" / (entry["id"] + ".json")).unlink()
            done.append("テスト用ユーザー %s を消した" % entry["id"])
        except FileNotFoundError:
            done.append("テスト用ユーザー %s は残っていなかった" % entry["id"])
        except OSError as error:
            problems.append("テスト用ユーザー %s を消せなかった (%s)" % (entry["id"], error.strerror or error))
    write_json(out / "stopped.json", {"stopped_at": now(), "ok": not problems, "done": done, "problems": problems})
    if not state and not created["users"]:
        say("後始末: 片付けるものは無い (サービスもテスト用データも作っていない)")
    elif problems:
        say("後始末: 失敗: %s。%s証拠は残した。" % ("。".join(problems), "済んだもの: %s。" % "、".join(done) if done else ""))
    else:
        say("後始末: %s。証拠は残した。" % "。".join(done))
    return 1 if problems else 0


def clean_earlier(out):
    """Run stop on the earlier launches beside this one that have no record of
    a complete stop."""
    problems = []
    for earlier in sorted(out.parent.iterdir()):
        if earlier == out or not earlier.is_dir():
            continue
        if not (earlier / "state.json").exists() and not (earlier / "created.json").exists():
            continue
        stopped = read_json(earlier / "stopped.json")
        if stopped and stopped.get("ok"):
            continue
        say("前の起動の後始末: %s" % earlier.name)
        if stop(earlier) != 0:
            problems.append("前の起動 %s の後始末が済んでいない" % earlier.name)
    return problems


def run(map_path, output):
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%S%fZ")
    out = Path(output) if output else home() / "logs" / "live-check" / ("%s-%d" % (stamp, os.getpid()))
    out.mkdir(parents=True, exist_ok=True)
    signals = []

    def interrupt(number, frame):
        for name in (signal.SIGTERM, signal.SIGINT):
            signal.signal(name, signal.SIG_IGN)
        signals.append(signal.Signals(number).name)
        raise Interrupted()

    signal.signal(signal.SIGTERM, interrupt)
    signal.signal(signal.SIGINT, interrupt)
    problems, note = [], ""
    try:
        problems += clean_earlier(out)
        for operation, failure in ((lambda: start(out), "起動できなかった"),
                                   (lambda: diagnose(out, map_path), "準備が足りない"),
                                   (lambda: act(out, map_path), "操作が不合格")):
            if operation() != 0:
                problems.append(failure)
                note = failure
                break
    except Interrupted:
        note = "中断された: %s" % signals[0]
        problems.append(note)
    except Exception as error:  # the evidence and the cleanup still follow
        note = "途中で止まった: %s: %s" % (type(error).__name__, error)
        problems.append(note)
    finally:
        # Nothing interrupts the evidence and the cleanup any more.
        for name in (signal.SIGTERM, signal.SIGINT):
            signal.signal(name, signal.SIG_IGN)
        try:
            evidence(out, map_path, note)
        except Exception as error:
            problems.append("証拠を出せなかった: %s" % error)
        try:
            if stop(out) != 0:
                problems.append("後始末が済んでいない")
        except Exception as error:
            problems.append("後始末が途中で止まった: %s" % error)
    if problems:
        say("結果: 1 で終わる (%s)" % "。".join(problems))
        return 1
    say("結果: 0 で終わる (起動・診断・操作・証拠・後始末がすべて済んだ)")
    return 0


def main(arguments=None):
    if hasattr(sys.stdout, "reconfigure"):
        sys.stdout.reconfigure(encoding="utf-8", errors="replace")
    parser = argparse.ArgumentParser(description="A live check of the running change (example).")
    operations = parser.add_subparsers(dest="operation", required=True)
    for name in ("run", "start", "diagnose", "act", "evidence", "stop"):
        operation = operations.add_parser(name)
        operation.add_argument("--output", required=name != "run", help="the launch's directory")
        if name != "start" and name != "stop":
            operation.add_argument("--map", required=name != "evidence", help="the project's Feature Map")
    chosen = parser.parse_args(arguments)
    if chosen.operation == "run":
        return run(chosen.map, chosen.output)
    out = Path(chosen.output)
    out.mkdir(parents=True, exist_ok=True)
    if chosen.operation == "start":
        return start(out)
    if chosen.operation == "diagnose":
        return diagnose(out, chosen.map)
    if chosen.operation == "act":
        return act(out, chosen.map)
    if chosen.operation == "evidence":
        return evidence(out, chosen.map)
    return stop(out)


if __name__ == "__main__":
    sys.exit(main())
