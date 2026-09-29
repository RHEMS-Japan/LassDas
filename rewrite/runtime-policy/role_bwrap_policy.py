#!/usr/bin/python3
"""Load the operator's compiled syscall filter, then run the real launcher.

The role launcher looks up ``bwrap`` on PATH. Installing this program there,
ahead of the real one, adds the operator's filter to every role launch through
bubblewrap's own ``--seccomp`` option, without the launcher choosing a policy.

It fails closed: a missing or unreadable filter ends the launch instead of
starting the role unconfined. Set ROLE_SECCOMP_FILTER to compile the filter
elsewhere; the default is the location the runtime's policy step writes.
"""
import os
import sys

PROGRAMS = ("/usr/bin/bwrap", "/bin/bwrap")


def main(arguments):
    path = os.environ.get("ROLE_SECCOMP_FILTER", "") or "/runtime-policy/role-filter.bpf"
    try:
        descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    except OSError as error:
        print("No role syscall filter at %s (%s); no unconfined role was started"
              % (path, error.strerror), file=sys.stderr)
        return 1
    os.set_inheritable(descriptor, True)
    for program in PROGRAMS:
        if os.path.exists(program):
            os.execv(program, ["bwrap", "--seccomp", str(descriptor), *arguments])
    print("bubblewrap is not installed; no unconfined role was started", file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
