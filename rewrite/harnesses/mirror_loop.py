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
  DELIVERY_REMOTE_URL        optional Git URL override (default: github.com)
  MIRROR_FETCH_TIMEOUT_SECONDS: optional

A failure that may pass on its own (the network, a timeout, the service
answering 5xx) never ends this loop: it says so and tries again at the next
interval, because the roles can keep working from the copy already here. A
refusal (a credential that is not accepted, a repository that is not there)
ends the process instead, so the runtime's restart policy and its restart
count make it visible rather than leaving an ever staler copy behind.
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
    # This loop is the retry, so a single attempt is enough here.
    support.run_git(support.git("clone", "--mirror", "--", url, str(path), url=url),
                    describe="copy the repository", retry=False,
                    timeout=support.number("MIRROR_FETCH_TIMEOUT_SECONDS", 900))
    report("Created the mirror at " + str(path))


def refresh(path, url):
    support.run_git(support.git("--git-dir", str(path), "fetch", "--prune", "--quiet", url=url),
                    describe="refresh the copy", retry=False,
                    timeout=support.number("MIRROR_FETCH_TIMEOUT_SECONDS", 900))


def cycle(path, url):
    """Return True when the copy is fresh, False when it may become fresh
    later. A refusal is raised: this loop is not the place to wait it out."""
    try:
        if not Path(path).is_dir():
            create(path, url)
        else:
            refresh(path, url)
    except support.TransientError as error:
        report("The mirror was not updated, in a way that may pass on its own: %s" % error)
        return False
    return True


def mirror(arguments):
    once = arguments == ["--once"]
    if arguments and not once:
        raise DeliveryError("This mirror process takes no arguments except --once")
    path = support.setting("MIRROR_PATH")
    owner, name = support.repository()
    url = support.remote_url(owner, name)
    interval = support.number("MIRROR_INTERVAL_SECONDS", 60)
    report("Mirroring %s/%s into %s every %g seconds" % (owner, name, path, interval))
    while not stopping.is_set():
        try:
            fresh = cycle(path, url)
        except DeliveryError as error:
            # A refusal will not pass by waiting. End, so the restart count and
            # this line say so, instead of serving a copy that quietly ages.
            report("The mirror was refused and this process is ending: %s" % error)
            return 1
        if once:
            return 0 if fresh else 1
        stopping.wait(interval)
    report("Stopped on request; the mirror was left as it is")
    return 0


if __name__ == "__main__":
    for received in (signal.SIGTERM, signal.SIGINT):
        signal.signal(received, lambda number, frame: stopping.set())
    sys.exit(support.main(mirror))
