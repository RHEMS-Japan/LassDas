"""Keep a local mirror of the delivery repository fresh. Fixed process.

Roles clone their workspace from this mirror, so the model's process never
receives a credential that can reach the delivery service. This loop is the
only holder of that credential here, and it never writes into a role's
workspace. It is a copy for reading, not a delivery target and not a backup.

Environment (all from the operator, never from a role):
  MIRROR_PATH                bare mirror directory on durable storage
  DELIVERY_REPOSITORY        owner/name of the repository to mirror
  GITHUB_TOKEN               read credential, through the credential helper
  MIRROR_INTERVAL_SECONDS    seconds between fetches (default 60)
  MIRROR_FAILURE_LIMIT       consecutive failures before exiting (default 10)
  DELIVERY_REMOTE_URL        optional Git URL override (default: github.com)
  MIRROR_FETCH_TIMEOUT_SECONDS: optional
"""
import os
from pathlib import Path
import signal
import sys
import threading

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import delivery_support as support
from delivery_support import DeliveryError

stopping = threading.Event()


def report(message):
    print("%s %s" % (support.timestamp(), support.scrub(message)), flush=True)


def create(path, url):
    """First run only: a bare mirror, with no working tree to write into."""
    Path(path).parent.mkdir(parents=True, exist_ok=True)
    support.run(support.git("clone", "--mirror", "--", url, str(path), url=url),
                timeout=support.number("MIRROR_FETCH_TIMEOUT_SECONDS", 900))
    report("Created the mirror at " + str(path))


def refresh(path, url):
    support.run(support.git("--git-dir", str(path), "fetch", "--prune", "--quiet", url=url),
                timeout=support.number("MIRROR_FETCH_TIMEOUT_SECONDS", 900))


def cycle(path, url, state):
    try:
        if not Path(path).is_dir():
            create(path, url)
        else:
            refresh(path, url)
    except DeliveryError as error:
        state["failures"] += 1
        report("The mirror was not updated (%d in a row): %s" % (state["failures"], error))
        return False
    state["failures"] = 0
    return True


def mirror(arguments):
    once = arguments == ["--once"]
    if arguments and not once:
        raise DeliveryError("This mirror process takes no arguments except --once")
    path = support.setting("MIRROR_PATH")
    owner, name = support.repository()
    url = support.remote_url(owner, name)
    interval = support.number("MIRROR_INTERVAL_SECONDS", 60)
    limit = int(support.number("MIRROR_FAILURE_LIMIT", 10))
    state = {"failures": 0}
    report("Mirroring %s/%s into %s every %g seconds" % (owner, name, path, interval))
    while not stopping.is_set():
        fresh = cycle(path, url, state)
        if once:
            return 0 if fresh else 1
        if state["failures"] >= limit:
            # Exiting hands the problem to the runtime's restart policy instead
            # of letting roles clone an increasingly stale source unnoticed.
            report("Exiting after %d consecutive failures; the mirror is stale" % state["failures"])
            return 1
        stopping.wait(interval)
    report("Stopped on request; the mirror was left as it is")
    return 0


if __name__ == "__main__":
    for received in (signal.SIGTERM, signal.SIGINT):
        signal.signal(received, lambda number, frame: stopping.set())
    sys.exit(support.main(mirror))
